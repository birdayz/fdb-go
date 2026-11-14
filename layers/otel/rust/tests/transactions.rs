//! Deterministic transaction state-machine tests, NOT live-FDB conformance.
use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{
    Error, ErrorKind, Result,
    collector::{Collector, Completion, Config, Stats},
    row::Schema,
    storage::{self, RecordCompression},
    tuple::{self, Element},
    writer::{
        Context, Database, Diagnostics, ExpectedValue, KeyValues, Layout, Transaction, Writer,
    },
};
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage, Value};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, VecDeque},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

#[derive(Default)]
struct State {
    data: BTreeMap<Vec<u8>, Vec<u8>>,
    outcomes: VecDeque<Option<i32>>,
    attempts: usize,
    retries: usize,
    transactions: usize,
    force_large: bool,
    max_records_per_transaction: Option<usize>,
    size_override: Option<i64>,
    after_retry: Option<(Vec<u8>, Option<Vec<u8>>)>,
    committed_records: Vec<usize>,
}
#[derive(Clone, Default)]
struct Fake(Arc<Mutex<State>>);
struct Tx {
    db: Fake,
    changes: BTreeMap<Vec<u8>, Option<Vec<u8>>>,
}
impl Database for Fake {
    fn transaction(&self) -> Result<Box<dyn Transaction + '_>> {
        self.0.lock().unwrap().transactions += 1;
        Ok(Box::new(Tx {
            db: self.clone(),
            changes: BTreeMap::new(),
        }))
    }
}
impl Transaction for Tx {
    fn get(&mut self, key: &[u8], ctx: &Context) -> Result<Option<Vec<u8>>> {
        ctx.check()?;
        Ok(self.db.0.lock().unwrap().data.get(key).cloned())
    }
    fn ranges(&mut self, ranges: &[(Vec<u8>, Vec<u8>)], ctx: &Context) -> Result<Vec<KeyValues>> {
        ctx.check()?;
        let state = self.db.0.lock().unwrap();
        Ok(ranges
            .iter()
            .map(|(b, e)| {
                state
                    .data
                    .range(b.clone()..e.clone())
                    .map(|(k, v)| (k.clone(), v.clone()))
                    .collect()
            })
            .collect())
    }
    fn set(&mut self, key: &[u8], value: &[u8]) {
        self.changes.insert(key.to_vec(), Some(value.to_vec()));
    }
    fn clear(&mut self, key: &[u8]) {
        self.changes.insert(key.to_vec(), None);
    }
    fn approximate_size(&mut self, _: &Context) -> Result<i64> {
        let state = self.db.0.lock().unwrap();
        let records = self.changes.keys().filter(|key| key[0] == 1).count();
        Ok(
            if state.force_large
                || state
                    .max_records_per_transaction
                    .is_some_and(|max| records > max)
            {
                500_001
            } else if let Some(size) = state.size_override {
                size
            } else {
                self.changes
                    .iter()
                    .map(|(k, v)| k.len() + v.as_ref().map_or(0, Vec::len))
                    .sum::<usize>() as i64
            },
        )
    }
    fn commit(&mut self, ctx: &Context) -> Result<()> {
        ctx.check()?;
        let mut state = self.db.0.lock().unwrap();
        state.attempts += 1;
        if let Some(Some(code)) = state.outcomes.pop_front() {
            return Err(Error {
                kind: ErrorKind::Transient,
                message: format!("injected {code}"),
                fdb_code: Some(code),
            });
        }
        state
            .committed_records
            .push(self.changes.keys().filter(|key| key[0] == 1).count());
        for (k, v) in std::mem::take(&mut self.changes) {
            if let Some(v) = v {
                state.data.insert(k, v);
            } else {
                state.data.remove(&k);
            }
        }
        Ok(())
    }
    fn on_error(&mut self, code: i32, ctx: &Context) -> Result<()> {
        ctx.check()?;
        self.changes.clear();
        if code != 1020 {
            // As native `checked`: 1000..1500 is transient (e.g. 1031), the rest terminal.
            return Err(Error {
                kind: if (1000..1500).contains(&code) {
                    ErrorKind::Transient
                } else {
                    ErrorKind::Operation
                },
                message: "terminal FDB error".into(),
                fdb_code: Some(code),
            });
        }
        let mut state = self.db.0.lock().unwrap();
        state.retries += 1;
        if let Some((key, value)) = state.after_retry.take() {
            if let Some(value) = value {
                state.data.insert(key, value);
            } else {
                state.data.remove(&key);
            }
        }
        Ok(())
    }
}
fn layout() -> Layout {
    Layout {
        version: 1,
        descriptor_sha256: hex::encode(Sha256::digest(include_bytes!(
            "../testdata/descriptors.pb"
        ))),
        record_prefix: "01".into(),
        index_prefixes: fdb_otel_rust::row::INDEXES
            .iter()
            .enumerate()
            .map(|(i, n)| ((*n).into(), format!("02{:02x}", i)))
            .collect(),
        checks: vec![ExpectedValue {
            key: "00".into(),
            value: Some("0102".into()),
        }],
        empty_ranges: vec![("03".into(), "04".into())],
        messages: "none".into(),
    }
}
#[derive(Deserialize)]
struct Vector {
    name: String,
    otlp: String,
    row: String,
    pk: String,
    keys: BTreeMap<String, Vec<String>>,
}
fn vectors() -> Vec<Vector> {
    serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap()
}
fn inputs() -> Vec<Vec<u8>> {
    vectors()
        .into_iter()
        .filter(|v| v.name.starts_with("v2_"))
        .map(|v| STANDARD.decode(v.otlp).unwrap())
        .collect()
}
fn expected_physical(layout: &Layout, fixtures: &[&Vector]) -> BTreeMap<Vec<u8>, Vec<u8>> {
    let mut data = BTreeMap::from([(vec![0], vec![1, 2])]);
    for fixture in fixtures {
        let mut key = hex::decode(&layout.record_prefix).unwrap();
        key.extend(hex::decode(&fixture.pk).unwrap());
        key.push(0x14); // FDB tuple integer zero: unsplit suffix.
        let row = STANDARD.decode(&fixture.row).unwrap();
        let mut value = vec![0x0a]; // RecordTypeUnion.SPANS_0, length-delimited.
        prost::encoding::encode_varint(row.len() as u64, &mut value);
        value.extend(row);
        assert!(
            value.len() <= 100_000,
            "this independent map expects unsplit fixtures"
        );
        assert!(
            data.insert(key, value).is_none(),
            "duplicate source identity"
        );
        for (name, entries) in &fixture.keys {
            for entry in entries {
                let mut key = hex::decode(&layout.index_prefixes[name]).unwrap();
                key.extend(hex::decode(entry).unwrap());
                data.insert(key, vec![]);
            }
        }
    }
    data
}
fn database() -> Fake {
    let db = Fake::default();
    db.0.lock().unwrap().data.insert(vec![0], vec![1, 2]);
    db
}

fn minimal_span(id: u8, start: u64, name_bytes: usize, padding: usize) -> Vec<u8> {
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let mut span = DynamicMessage::new(
        pool.get_message_by_name("opentelemetry.proto.trace.v1.Span")
            .unwrap(),
    );
    span.set_field_by_name("trace_id", Value::Bytes(vec![0x11; 16].into()));
    span.set_field_by_name("span_id", Value::Bytes(vec![id; 8].into()));
    span.set_field_by_name("start_time_unix_nano", Value::U64(start));
    span.set_field_by_name("name", Value::String("x".repeat(name_bytes)));
    let mut scope = DynamicMessage::new(
        pool.get_message_by_name("opentelemetry.proto.trace.v1.ScopeSpans")
            .unwrap(),
    );
    scope.set_field_by_name("spans", Value::List(vec![Value::Message(span)]));
    let mut resource = DynamicMessage::new(
        pool.get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
            .unwrap(),
    );
    resource.set_field_by_name("scope_spans", Value::List(vec![Value::Message(scope)]));
    let mut wire = resource.encode_to_vec();
    if padding != 0 {
        // Unknown wrapper payload changes only the embedded OTLP byte count.
        prost::encoding::encode_key(999, prost::encoding::WireType::LengthDelimited, &mut wire);
        prost::encoding::encode_varint(padding as u64, &mut wire);
        wire.extend(std::iter::repeat_n(0xab, padding));
    }
    wire
}

#[test]
fn exact_census_replay_replacement_and_clears() {
    let db = database();
    let layout = layout();
    let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
    let source = inputs();
    let (stats, result) = writer.write(&Context::default(), &source);
    result.unwrap();
    assert_eq!(stats.committed_spans, 8);
    assert_eq!(stats.committed_batches, 1);
    let first = db.0.lock().unwrap().data.clone();
    let fixtures = vectors();
    let original = fixtures
        .iter()
        .filter(|v| v.name.starts_with("v2_"))
        .collect::<Vec<_>>();
    assert_eq!(original.len(), 8);
    assert_eq!(first, expected_physical(&layout, &original));
    assert_eq!(
        first.len(),
        89,
        "8 records + 80 index keys + one synthetic header"
    );
    writer.write(&Context::default(), &source).1.unwrap();
    assert_eq!(
        db.0.lock().unwrap().data,
        first,
        "replay must be idempotent"
    );
    // Fidelity is an independently Go-built replacement of v2_0's identity.
    let replacement = fixtures.iter().find(|v| v.name == "fidelity").unwrap();
    assert_eq!(replacement.pk, original[0].pk);
    let changed = STANDARD.decode(&replacement.otlp).unwrap();
    let (stats, result) = writer.write(&Context::default(), &[changed]);
    result.unwrap();
    assert_eq!(stats.committed_spans, 1);
    let mut replaced = original.clone();
    replaced[0] = replacement;
    let expected = expected_physical(&layout, &replaced);
    assert_ne!(
        expected, first,
        "replacement control must change rows and keys"
    );
    assert!(
        first.keys().any(|key| !expected.contains_key(key)),
        "control must retract old keys"
    );
    assert_eq!(db.0.lock().unwrap().data, expected);
}

#[test]
fn admission_cancel_ownership_corruption_and_guard_rollback() {
    let source = inputs();
    let layout = layout();
    for case in ["admission", "canceled", "ownership", "guard"] {
        let db = database();
        let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::Zlib1).unwrap();
        let ctx = Context::default();
        let mut in_call = vec![source[0].clone()];
        let kind = match case {
            "admission" => {
                in_call.push(vec![0xff]);
                ErrorKind::Admission
            }
            "canceled" => {
                ctx.cancel();
                ErrorKind::Canceled
            }
            "ownership" => {
                db.0.lock().unwrap().data.insert(vec![0], vec![9]);
                ErrorKind::Ownership
            }
            _ => {
                db.0.lock().unwrap().force_large = true;
                ErrorKind::Operation
            }
        };
        let before = db.0.lock().unwrap().data.clone();
        let (stats, result) = writer.write(&ctx, &in_call);
        assert_eq!(result.unwrap_err().kind, kind, "{case}");
        assert_eq!(stats.committed_spans, 0, "{case}");
        assert_eq!(db.0.lock().unwrap().data, before, "{case} must roll back");
    }
    let db = database();
    let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
    writer.write(&Context::default(), &source[..1]).1.unwrap();
    let record =
        db.0.lock()
            .unwrap()
            .data
            .keys()
            .find(|k| k[0] == 1)
            .unwrap()
            .clone();
    db.0.lock().unwrap().data.insert(record, vec![10, 2, 0]);
    let before = db.0.lock().unwrap().data.clone();
    assert_eq!(
        writer
            .write(&Context::default(), &source[..1])
            .1
            .unwrap_err()
            .kind,
        ErrorKind::Corruption
    );
    assert_eq!(db.0.lock().unwrap().data, before);
}

#[test]
fn transient_retry_terminal_error_and_partial_progress() {
    let db = database();
    let layout = layout();
    let source = inputs();
    db.0.lock().unwrap().outcomes = VecDeque::from([Some(1020), None, Some(2004)]);
    let writer = Writer::new(db.clone(), &layout, 1, RecordCompression::Zlib1).unwrap();
    let (stats, result) = writer.write(&Context::default(), &source);
    assert_eq!(result.unwrap_err().fdb_code, Some(2004));
    assert_eq!(stats.committed_spans, 1);
    assert_eq!(stats.committed_batches, 1);
    assert_eq!(stats.retries, 1);
    let state = db.0.lock().unwrap();
    assert_eq!(state.attempts, 3);
    assert_eq!(state.retries, 1);
    assert_eq!(state.transactions, 2);
    assert_eq!(state.data.len(), 12);
}

#[test]
fn constructor_rejects_each_manifest_arm_and_batch_bounds() {
    let valid = layout();
    let mut cases = Vec::new();
    macro_rules! invalid {
        ($name:literal, $change:expr) => {{
            let mut layout = valid.clone();
            ($change)(&mut layout);
            cases.push(($name, layout));
        }};
    }
    invalid!("version", |l: &mut Layout| l.version = 2);
    invalid!("descriptor", |l: &mut Layout| l.descriptor_sha256 =
        "0".repeat(64));
    invalid!("messages", |l: &mut Layout| l.messages = "decompose".into());
    invalid!("vacuous checks", |l: &mut Layout| l.checks.clear());
    invalid!("vacuous ranges", |l: &mut Layout| l.empty_ranges.clear());
    invalid!("missing index", |l: &mut Layout| {
        l.index_prefixes.remove("SPANS_TIME");
    });
    invalid!("wrong same-count indexes", |l: &mut Layout| {
        let prefix = l.index_prefixes.remove("SPANS_TIME").unwrap();
        l.index_prefixes.insert("unexpected".into(), prefix);
    });
    invalid!("extra index", |l: &mut Layout| {
        l.index_prefixes.insert("unexpected".into(), "02ff".into());
    });
    invalid!("empty record prefix", |l: &mut Layout| l
        .record_prefix
        .clear());
    invalid!("invalid record hex", |l: &mut Layout| l.record_prefix =
        "gg".into());
    invalid!("empty index prefix", |l: &mut Layout| {
        l.index_prefixes.insert("SPANS_TIME".into(), "".into());
    });
    invalid!("invalid index hex", |l: &mut Layout| {
        l.index_prefixes.insert("SPANS_TIME".into(), "gg".into());
    });
    invalid!("empty check key", |l: &mut Layout| l.checks[0].key.clear());
    invalid!("invalid check key hex", |l: &mut Layout| l.checks[0].key =
        "gg".into());
    invalid!("invalid expected hex", |l: &mut Layout| l.checks[0].value =
        Some("gg".into()));
    invalid!("empty range begin", |l: &mut Layout| l.empty_ranges[0]
        .0
        .clear());
    invalid!("invalid range hex", |l: &mut Layout| l.empty_ranges[0].1 =
        "gg".into());
    invalid!("equal range", |l: &mut Layout| l.empty_ranges[0] =
        ("03".into(), "03".into()));
    invalid!("reversed range", |l: &mut Layout| l.empty_ranges[0] =
        ("04".into(), "03".into()));
    assert_eq!(cases.len(), 19);
    for (name, layout) in cases {
        let db = database();
        let before = db.0.lock().unwrap().data.clone();
        let result = Writer::new(db.clone(), &layout, 64, RecordCompression::None);
        assert_eq!(result.err().unwrap().kind, ErrorKind::Config, "{name}");
        assert_eq!(db.0.lock().unwrap().transactions, 0, "{name}");
        assert_eq!(db.0.lock().unwrap().data, before, "{name}");
    }
    for batch in [0, 65, usize::MAX] {
        assert_eq!(
            Writer::new(database(), &valid, batch, RecordCompression::None)
                .err()
                .unwrap()
                .kind,
            ErrorKind::Config
        );
    }
    for batch in [1, 64] {
        assert!(Writer::new(database(), &valid, batch, RecordCompression::None).is_ok());
    }
}

#[test]
fn expected_absence_and_half_open_state_range_are_enforced() {
    let source = inputs();
    let mut layout = layout();
    layout.checks.push(ExpectedValue {
        key: "05".into(),
        value: None,
    });
    for (key, reject) in [
        (vec![3], true),
        (vec![3, 0], true),
        (vec![4], false),
        (vec![5], true),
    ] {
        let db = database();
        db.0.lock().unwrap().data.insert(key.clone(), vec![]);
        let before = db.0.lock().unwrap().data.clone();
        let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), &source[..1]);
        if reject {
            assert_eq!(result.unwrap_err().kind, ErrorKind::Ownership, "{key:?}");
            assert_eq!(stats.committed_spans, 0);
            assert_eq!(db.0.lock().unwrap().attempts, 0);
            assert_eq!(db.0.lock().unwrap().data, before);
        } else {
            result.unwrap();
            assert_eq!(
                stats.committed_spans, 1,
                "range end is exclusive and absent key5 is valid"
            );
        }
    }
}

#[test]
fn ownership_and_index_state_seals_are_rechecked_after_on_error() {
    let source = inputs();
    for mutation in [
        (vec![0], Some(vec![9])),
        (vec![0], None),
        (vec![3], Some(vec![])),
        (vec![5], Some(vec![])),
    ] {
        let db = database();
        let mut layout = layout();
        layout.checks.push(ExpectedValue {
            key: "05".into(),
            value: None,
        });
        {
            let mut state = db.0.lock().unwrap();
            state.outcomes.push_back(Some(1020));
            state.after_retry = Some(mutation.clone());
        }
        let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), &source[..1]);
        assert_eq!(result.unwrap_err().kind, ErrorKind::Ownership);
        assert_eq!(stats.committed_spans, 0);
        assert_eq!(stats.retries, 1);
        let state = db.0.lock().unwrap();
        assert_eq!(state.attempts, 1, "no second commit under changed seal");
        assert_eq!(state.transactions, 1, "OnError must reuse the transaction");
        assert!(state.committed_records.is_empty());
        let mut expected = BTreeMap::from([(vec![0], vec![1, 2])]);
        if let Some(value) = mutation.1 {
            expected.insert(mutation.0, value);
        } else {
            expected.remove(&mutation.0);
        }
        assert_eq!(state.data, expected);
    }
}

#[test]
fn physical_record_key_accepts_exact_limit_and_rejects_one_byte_over() {
    let source = inputs();
    let pk_len = hex::decode(&vectors().into_iter().find(|v| v.name == "v2_0").unwrap().pk)
        .unwrap()
        .len();
    for (limit, reject) in [(10_000, false), (10_001, true)] {
        let db = database();
        let before = db.0.lock().unwrap().data.clone();
        let mut layout = layout();
        layout.record_prefix = hex::encode(vec![0xaa; limit - pk_len - 1]);
        let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), &source[..1]);
        if reject {
            assert_eq!(result.unwrap_err().kind, ErrorKind::Admission);
            assert_eq!(stats.committed_spans, 0);
            assert_eq!(db.0.lock().unwrap().data, before);
            assert_eq!(db.0.lock().unwrap().attempts, 0);
        } else {
            result.unwrap();
            assert_eq!(stats.committed_spans, 1);
            assert!(
                db.0.lock()
                    .unwrap()
                    .data
                    .keys()
                    .any(|key| key.len() == 10_000)
            );
        }
    }
}

#[test]
fn actual_size_equal_guard_commits_and_combined_oversize_replans_halves() {
    let source = inputs();
    let db = database();
    db.0.lock().unwrap().size_override = Some(500_000);
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &source[..1]);
    result.unwrap();
    assert_eq!(stats.committed_spans, 1);
    assert_eq!(
        db.0.lock().unwrap().attempts,
        1,
        "500000 equality must commit"
    );

    let db = database();
    db.0.lock().unwrap().max_records_per_transaction = Some(2);
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &source[..6]);
    result.unwrap();
    assert_eq!(stats.committed_spans, 6);
    assert_eq!(stats.committed_batches, 4);
    let state = db.0.lock().unwrap();
    assert_eq!(state.transactions, 7);
    assert_eq!(state.committed_records, [1, 2, 1, 2]);
    let fixtures = vectors();
    let expected = fixtures
        .iter()
        .filter(|v| v.name.starts_with("v2_"))
        .take(6)
        .collect::<Vec<_>>();
    assert_eq!(state.data, expected_physical(&layout(), &expected));
}

#[test]
fn count_limit_and_cumulative_estimate_partition_at_inclusive_boundaries() {
    let schema = Schema::pinned().unwrap();
    let source = (1..=65)
        .map(|id| minimal_span(id, 1, 0, 0))
        .collect::<Vec<_>>();
    let prepared = schema.prepare(&source).unwrap();
    assert_eq!(prepared.len(), 65, "identities must not collapse");
    assert!(prepared.iter().map(|s| s.estimate).sum::<usize>() < 300_000);
    let db = database();
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &source);
    result.unwrap();
    assert_eq!(stats.committed_spans, 65);
    assert_eq!(stats.committed_batches, 2);
    let state = db.0.lock().unwrap();
    assert_eq!(state.committed_records, [64, 1]);
    assert_eq!(state.transactions, 2);
    assert_eq!(state.attempts, 2);
    drop(state);

    // Pin the measured cost of these concrete inputs, not merely a relation
    // between two estimates. No attributes contribute extra fan-out costs.
    // estimate = 2048 + 45 (other columns) + 2 + 3 (OTLP tag, len; len is 1 byte unpadded) + 41
    //   (span envelope) + 5 (padding tag, len) + padding: 147_856 -> 150_000; unpadded -> 2_137.
    for (extra, batches) in [(0, vec![4, 1]), (1, vec![2, 3])] {
        let source = vec![
            minimal_span(1, 1, 0, 147_856),
            minimal_span(2, 1, 0, 147_856 + extra),
            minimal_span(3, 1, 0, 0),
        ];
        let prepared = schema.prepare(&source).unwrap();
        assert_eq!(prepared[0].estimate, 150_000);
        assert_eq!(prepared[1].estimate, 150_000 + extra);
        assert_eq!(prepared[2].estimate, 2_137);
        let db = database();
        let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), &source);
        result.unwrap();
        assert_eq!(stats.committed_spans, 3);
        assert_eq!(stats.committed_batches, 2);
        let state = db.0.lock().unwrap();
        assert_eq!(state.committed_records, batches, "extra={extra}");
        assert_eq!(state.transactions, 2, "no actual-size fallback expected");
        assert_eq!(state.attempts, 2);
    }
}

#[test]
fn split_unsplit_replacements_remove_every_stale_record_and_index_key() {
    let schema = Schema::pinned().unwrap();
    let db = database();
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let small = minimal_span(1, 1, 10, 0);
    let large = minimal_span(1, 2, 110_000, 0);
    let mut first_small = None;
    let mut first_large = None;
    for (source, record_count) in [(&small, 1), (&large, 3), (&small, 1), (&large, 3)] {
        let expected = schema.prepare(std::slice::from_ref(source)).unwrap();
        let (stats, result) = writer.write(&Context::default(), std::slice::from_ref(source));
        result.unwrap();
        assert_eq!(stats.committed_spans, 1);
        assert_eq!(stats.committed_batches, 1);
        let state = db.0.lock().unwrap();
        let records = state
            .data
            .iter()
            .filter(|(k, _)| k[0] == 1)
            .collect::<Vec<_>>();
        assert_eq!(records.len(), record_count);
        let mut stored = Vec::new();
        for (i, (key, value)) in records.iter().enumerate() {
            let mut expected_key = vec![1];
            expected_key.extend(&expected[0].primary_key);
            expected_key.extend(tuple::pack(&[Element::Int(if record_count == 1 {
                0
            } else {
                i as i64 + 1
            })]));
            assert_eq!(**key, expected_key);
            assert!(value.len() <= 100_000);
            stored.extend(*value);
        }
        assert_eq!(storage::decode(&stored).unwrap(), expected[0].row);
        // Two no-attribute indexes plus the header and current record chunks.
        assert_eq!(state.data.len(), record_count + 3);
        let previous = if record_count == 1 {
            &mut first_small
        } else {
            &mut first_large
        };
        if let Some(map) = previous {
            assert_eq!(
                &state.data, map,
                "returning to the same image must be exact"
            );
        } else {
            *previous = Some(state.data.clone());
        }
    }
    assert_ne!(first_small.unwrap(), first_large.unwrap());
}

#[test]
fn corrupt_prior_split_shapes_fail_without_any_ack_or_mutation() {
    let source = minimal_span(1, 1, 110_000, 0);
    let schema = Schema::pinned().unwrap();
    let prepared = schema.prepare(std::slice::from_ref(&source)).unwrap();
    let mut prefix = vec![1];
    prefix.extend(&prepared[0].primary_key);
    let key = |suffix| [prefix.clone(), tuple::pack(&[Element::Int(suffix)])].concat();
    for case in [
        "missing first",
        "missing middle",
        "mixed unsplit and split",
        "noncontiguous last",
        "oversized chunk",
        "noncanonical suffix",
        "unsupported transform",
        "malformed row",
        "primary-key disagreement",
    ] {
        let db = database();
        let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
        writer
            .write(&Context::default(), std::slice::from_ref(&source))
            .1
            .unwrap();
        let mut state = db.0.lock().unwrap();
        assert_eq!(state.data.keys().filter(|k| k[0] == 1).count(), 3);
        match case {
            "missing first" => {
                state.data.remove(&key(1));
            }
            "missing middle" => {
                state.data.remove(&key(2));
            }
            "mixed unsplit and split" => {
                state.data.insert(key(0), vec![10, 0]);
            }
            "noncontiguous last" => {
                let value = state.data.remove(&key(3)).unwrap();
                state.data.insert(key(4), value);
            }
            "oversized chunk" => {
                state.data.insert(key(1), vec![42; 100_001]);
            }
            "noncanonical suffix" => {
                let value = state.data.remove(&key(3)).unwrap();
                state
                    .data
                    .insert([prefix.clone(), vec![0x16, 0, 3]].concat(), value);
            }
            "primary-key disagreement" => {
                let other = schema.prepare(&[minimal_span(2, 1, 110_000, 0)]).unwrap();
                let stored = storage::encode(&other[0].row, RecordCompression::None).unwrap();
                for (suffix, value) in storage::split(&stored) {
                    state.data.insert(key(suffix), value);
                }
            }
            _ => {
                state.data.retain(|k, _| k[0] != 1);
                state.data.insert(
                    key(0),
                    if case == "malformed row" {
                        vec![10, 1, 0xff]
                    } else {
                        vec![4, 2, 0, 0, 0, 0]
                    },
                );
            }
        }
        let before = state.data.clone();
        let attempts = state.attempts;
        drop(state);
        let (stats, result) = writer.write(&Context::default(), std::slice::from_ref(&source));
        assert_eq!(result.unwrap_err().kind, ErrorKind::Corruption, "{case}");
        assert_eq!(stats.committed_spans, 0, "{case}");
        assert_eq!(stats.committed_batches, 0, "{case}");
        let state = db.0.lock().unwrap();
        assert_eq!(state.attempts, attempts, "{case}: must not attempt commit");
        assert_eq!(state.data, before, "{case}");
    }
}

#[test]
fn replanning_retains_first_half_ack_when_second_half_fails() {
    let db = database();
    {
        let mut state = db.0.lock().unwrap();
        state.max_records_per_transaction = Some(2);
        state.outcomes = VecDeque::from([None, Some(2004)]);
    }
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &inputs()[..4]);
    assert_eq!(result.unwrap_err().fdb_code, Some(2004));
    assert_eq!(stats.committed_spans, 2);
    assert_eq!(stats.committed_batches, 1);
    let state = db.0.lock().unwrap();
    assert_eq!(state.transactions, 3);
    assert_eq!(state.committed_records, [2]);
    let fixtures = vectors();
    let expected = fixtures
        .iter()
        .filter(|v| v.name.starts_with("v2_"))
        .take(2)
        .collect::<Vec<_>>();
    assert_eq!(state.data, expected_physical(&layout(), &expected));
}

type SpanSpec = (Vec<u8>, Vec<u8>, u64);
/// One ResourceSpans with schema URL `resource` and the given (scope name, spans).
fn wrapper(resource: &str, scopes: &[(&str, Vec<SpanSpec>)]) -> Vec<u8> {
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let message = |name: &str| DynamicMessage::new(pool.get_message_by_name(name).unwrap());
    let mut rs = message("opentelemetry.proto.trace.v1.ResourceSpans");
    rs.set_field_by_name("schema_url", Value::String(resource.into()));
    let mut list = Vec::new();
    for (name, spans) in scopes {
        let mut scope = message("opentelemetry.proto.common.v1.InstrumentationScope");
        scope.set_field_by_name("name", Value::String((*name).into()));
        let mut ss = message("opentelemetry.proto.trace.v1.ScopeSpans");
        ss.set_field_by_name("scope", Value::Message(scope));
        let spans = spans.iter().map(|(trace, span, start)| {
            let mut sp = message("opentelemetry.proto.trace.v1.Span");
            sp.set_field_by_name("trace_id", Value::Bytes(trace.clone().into()));
            sp.set_field_by_name("span_id", Value::Bytes(span.clone().into()));
            sp.set_field_by_name("start_time_unix_nano", Value::U64(*start));
            Value::Message(sp)
        });
        ss.set_field_by_name("spans", Value::List(spans.collect()));
        list.push(Value::Message(ss));
    }
    rs.set_field_by_name("scope_spans", Value::List(list));
    rs.encode_to_vec()
}
fn ok_span(id: u8) -> SpanSpec {
    (vec![0x11; 16], vec![id; 8], u64::from(id))
}

#[test]
fn repeated_resources_scopes_and_spans_split_into_their_own_wrappers() {
    let schema = Schema::pinned().unwrap();
    let inputs = vec![
        wrapper(
            "res-a",
            &[
                ("a1", vec![ok_span(1), ok_span(2)]),
                ("a2", vec![ok_span(3)]),
            ],
        ),
        wrapper("res-b", &[("b1", vec![ok_span(4), ok_span(5)])]),
    ];
    assert_eq!(schema.validate(&inputs).unwrap(), 5);
    let db = database();
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &inputs);
    result.unwrap();
    assert_eq!(stats.committed_spans, 5);
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let state = db.0.lock().unwrap();
    assert_eq!(state.data.keys().filter(|k| k[0] == 1).count(), 5);
    for (id, resource, scope) in [
        (1, "res-a", "a1"),
        (2, "res-a", "a1"),
        (3, "res-a", "a2"),
        (4, "res-b", "b1"),
        (5, "res-b", "b1"),
    ] {
        // Literal FDB tuple (0, trace, span) under record prefix 01, unsplit suffix 0x14.
        let mut key = vec![0x01, 0x14, 0x01];
        key.extend([0x11; 16]);
        key.extend([0x00, 0x01]);
        key.extend([id; 8]);
        key.extend([0x00, 0x14]);
        let value = &state.data[&key];
        assert_eq!(value[0], 0x0a, "RecordTypeUnion.SPANS_0");
        let mut row_bytes = &value[1..];
        let len = prost::encoding::decode_varint(&mut row_bytes).unwrap();
        assert_eq!(row_bytes.len() as u64, len, "span {id}");
        let row =
            DynamicMessage::decode(pool.get_message_by_name("SPANS").unwrap(), row_bytes).unwrap();
        let otlp = row
            .get_field_by_name("OTLP")
            .unwrap()
            .as_bytes()
            .unwrap()
            .clone();
        let rs = DynamicMessage::decode(
            pool.get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
                .unwrap(),
            otlp,
        )
        .unwrap();
        assert_eq!(
            rs.get_field_by_name("schema_url").unwrap().as_str(),
            Some(resource)
        );
        let scopes = rs
            .get_field_by_name("scope_spans")
            .unwrap()
            .as_list()
            .unwrap()
            .to_vec();
        assert_eq!(scopes.len(), 1, "span {id}");
        let ss = scopes[0].as_message().unwrap();
        let name = ss.get_field_by_name("scope").unwrap();
        assert_eq!(
            name.as_message()
                .unwrap()
                .get_field_by_name("name")
                .unwrap()
                .as_str(),
            Some(scope)
        );
        let spans = ss
            .get_field_by_name("spans")
            .unwrap()
            .as_list()
            .unwrap()
            .to_vec();
        assert_eq!(spans.len(), 1, "span {id}");
        let span_id = spans[0]
            .as_message()
            .unwrap()
            .get_field_by_name("span_id")
            .unwrap();
        assert_eq!(span_id.as_bytes().unwrap().as_ref(), [id; 8]);
    }
}

#[test]
fn identity_and_start_boundaries_through_validate_and_writer_write() {
    let schema = Schema::pinned().unwrap();
    let (trace, span) = (vec![0x11; 16], vec![0x22; 8]);
    let mut low_trace = vec![0; 16];
    low_trace[15] = 1;
    let mut low_span = vec![0; 8];
    low_span[7] = 1;
    for (name, spec) in [
        ("start zero", (trace.clone(), span.clone(), 0)),
        (
            "start int64 max",
            (trace.clone(), span.clone(), i64::MAX as u64),
        ),
        ("one nonzero byte per id", (low_trace, low_span, 1)),
    ] {
        let input = wrapper("r", &[("s", vec![spec])]);
        assert_eq!(
            schema.validate(std::slice::from_ref(&input)).unwrap(),
            1,
            "{name}"
        );
        let db = database();
        let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), &[input]);
        result.unwrap();
        assert_eq!(stats.committed_spans, 1, "{name}");
    }
    let rejected = [
        ("empty span id", (trace.clone(), vec![], 1)),
        ("short span id", (trace.clone(), vec![0x22; 7], 1)),
        ("long span id", (trace.clone(), vec![0x22; 9], 1)),
        ("zero span id", (trace.clone(), vec![0; 8], 1)),
        ("empty trace id", (vec![], span.clone(), 1)),
        ("short trace id", (vec![0x11; 15], span.clone(), 1)),
        ("long trace id", (vec![0x11; 17], span.clone(), 1)),
        ("zero trace id", (vec![0; 16], span.clone(), 1)),
        (
            "start past int64",
            (trace.clone(), span.clone(), i64::MAX as u64 + 1),
        ),
        ("start at u64 max", (trace.clone(), span.clone(), u64::MAX)),
    ];
    for (name, bad) in rejected {
        // A valid span first, then the bad one later in the same scope and in a later wrapper.
        for inputs in [
            vec![wrapper("r", &[("s", vec![ok_span(3), bad.clone()])])],
            vec![
                wrapper("r", &[("s", vec![ok_span(3)])]),
                wrapper("q", &[("t", vec![bad.clone()])]),
            ],
        ] {
            assert_eq!(
                schema.validate(&inputs).unwrap_err().kind,
                ErrorKind::Admission,
                "{name}"
            );
            let db = database();
            let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
            let (stats, result) = writer.write(&Context::default(), &inputs);
            assert_eq!(result.unwrap_err().kind, ErrorKind::Admission, "{name}");
            assert_eq!(stats.committed_spans, 0, "{name}");
            let state = db.0.lock().unwrap();
            assert_eq!(state.transactions, 0, "{name}");
            assert_eq!(
                state.data,
                BTreeMap::from([(vec![0], vec![1, 2])]),
                "{name}"
            );
        }
    }
}

#[derive(Deserialize)]
struct WireCase {
    name: String,
    input_otlp: String,
    outputs: Vec<Vector>,
}

#[derive(Deserialize)]
struct WireFixture {
    cases: Vec<WireCase>,
    malformed: Vec<WireMalformed>,
}

#[derive(Deserialize)]
struct WireMalformed {
    name: String,
    input_otlp: String,
}

#[test]
fn go_unknown_field_rows_and_keys_reach_the_physical_map() {
    let fixture: WireFixture =
        serde_json::from_slice(include_bytes!("../testdata/wire_vectors.json")).unwrap();
    assert_eq!(fixture.cases.len(), 8, "empty or changed wire fixture");
    let layout = layout();
    let otlp_bytes = |outputs: &[&Vector]| {
        outputs
            .iter()
            .map(|v| STANDARD.decode(&v.otlp).unwrap().len() as u64)
            .sum::<u64>()
    };
    let mut inputs = Vec::new();
    for case in &fixture.cases {
        let input = STANDARD.decode(&case.input_otlp).unwrap();
        let db = database();
        let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), std::slice::from_ref(&input));
        result.unwrap();
        let outputs: Vec<&Vector> = case.outputs.iter().collect();
        assert_eq!(stats.committed_spans, outputs.len() as u64, "{}", case.name);
        assert_eq!(
            stats.logical_otlp_bytes,
            otlp_bytes(&outputs),
            "{}",
            case.name
        );
        assert_eq!(
            db.0.lock().unwrap().data,
            expected_physical(&layout, &outputs),
            "{}",
            case.name
        );
        inputs.push(input);
    }
    let outputs: Vec<&Vector> = fixture.cases.iter().flat_map(|c| &c.outputs).collect();
    let db = database();
    let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &inputs);
    result.unwrap();
    assert_eq!(stats.committed_spans, 12);
    assert_eq!(stats.logical_otlp_bytes, otlp_bytes(&outputs));
    assert_eq!(
        db.0.lock().unwrap().data,
        expected_physical(&layout, &outputs)
    );
    // A later Go-rejected wrapper fails the whole call before any transaction.
    for m in &fixture.malformed {
        let mut call = inputs.clone();
        call.push(STANDARD.decode(&m.input_otlp).unwrap());
        let db = database();
        let writer = Writer::new(db.clone(), &layout, 64, RecordCompression::None).unwrap();
        let (stats, result) = writer.write(&Context::default(), &call);
        assert_eq!(result.unwrap_err().kind, ErrorKind::Admission, "{}", m.name);
        assert_eq!(stats.committed_spans, 0, "{}", m.name);
        let state = db.0.lock().unwrap();
        assert_eq!(state.transactions, 0, "{}", m.name);
        assert_eq!(
            state.data,
            BTreeMap::from([(vec![0], vec![1, 2])]),
            "{}",
            m.name
        );
    }
}

// The real Collector over the real Writer over this transactional fake. Every
// counter below is the composed result of Writer partial progress, not a supplied value.

fn bounded() -> Context {
    Context::default().with_deadline(Instant::now() + Duration::from_secs(5))
}
/// One consumer exporting exactly `batch` queued spans per call; flush forces the rest.
fn queue(batch: usize, retry_ms: u64) -> Config {
    Config {
        consumers: 1,
        queue_spans: 10_000,
        batch_min: batch,
        batch_max: batch,
        flush_ms: 60_000,
        block_on_overflow: false,
        export_timeout_ms: 30_000,
        retry_enabled: true,
        retry_initial_ms: retry_ms,
        retry_max_ms: retry_ms,
        retry_elapsed_ms: 300_000,
    }
}
fn otlp(fixtures: &[&Vector]) -> Vec<Vec<u8>> {
    fixtures
        .iter()
        .map(|v| STANDARD.decode(&v.otlp).unwrap())
        .collect()
}
fn named<'a>(fixtures: &'a [Vector], names: &[&str]) -> Vec<&'a Vector> {
    names
        .iter()
        .map(|n| fixtures.iter().find(|v| v.name == *n).unwrap())
        .collect()
}
fn sizes(pairs: &[(u64, u64)]) -> BTreeMap<u64, u64> {
    pairs.iter().copied().collect()
}

#[test]
fn collector_retries_a_partially_committed_writer_call_as_a_whole() {
    let fixtures = vectors();
    let four = named(&fixtures, &["v2_0", "v2_1", "v2_2", "v2_3"]);
    let db = database();
    // Writer batches of 2: the call's first transaction commits, its second gets 1031.
    db.0.lock().unwrap().outcomes = VecDeque::from([None, Some(1031)]);
    let writer = Writer::new(db.clone(), &layout(), 2, RecordCompression::None).unwrap();
    let collector = Collector::start(Arc::new(writer), queue(4, 1)).unwrap();
    collector.consume(&bounded(), &otlp(&four)).unwrap();
    collector.flush(&bounded()).unwrap();
    collector.shutdown(&bounded()).unwrap();
    let stats = collector.stats();
    assert_eq!((stats.accepted_spans, stats.rejected_spans), (4, 0));
    assert_eq!((stats.exported_spans, stats.failed_spans), (4, 0));
    // Operations: 2 from the failed attempt plus 4 from the whole-call retry.
    assert_eq!((stats.stored_spans, stats.committed_batches), (6, 3));
    assert_eq!(
        (stats.export_attempts, stats.retries, stats.native_retries),
        (2, 1, 0)
    );
    assert_eq!(stats.export_call_spans, sizes(&[(4, 2)]));
    assert_eq!(stats.transaction_spans, sizes(&[(2, 3)]));
    assert_eq!(stats.queued_spans, 0);
    let completions = collector.completions();
    assert_eq!(completions.len(), 1);
    assert_eq!((completions[0].seq, completions[0].spans), (0, 4));
    assert!(completions[0].error.is_none());
    let state = db.0.lock().unwrap();
    assert_eq!(
        (state.attempts, state.transactions, state.retries),
        (4, 4, 0)
    );
    assert_eq!(state.committed_records, [2, 2, 2]);
    assert_eq!(state.data, expected_physical(&layout(), &four));
}

#[test]
fn collector_counts_writer_dedup_and_native_on_error_retries() {
    let fixtures = vectors();
    // Fidelity replaces v2_0's identity inside the same call: two rows are written.
    let request = named(&fixtures, &["v2_0", "v2_1", "fidelity"]);
    let db = database();
    db.0.lock().unwrap().outcomes = VecDeque::from([Some(1020)]);
    let writer = Writer::new(db.clone(), &layout(), 64, RecordCompression::None).unwrap();
    let collector = Collector::start(Arc::new(writer), queue(3, 1)).unwrap();
    collector.consume(&bounded(), &otlp(&request)).unwrap();
    collector.shutdown(&bounded()).unwrap();
    let stats = collector.stats();
    assert_eq!((stats.accepted_spans, stats.exported_spans), (3, 3));
    assert_eq!((stats.stored_spans, stats.committed_batches), (2, 1));
    assert_eq!(
        (stats.export_attempts, stats.retries, stats.native_retries),
        (1, 0, 1)
    );
    assert_eq!(stats.export_call_spans, sizes(&[(3, 1)]));
    assert_eq!(stats.transaction_spans, sizes(&[(2, 1)]));
    let state = db.0.lock().unwrap();
    assert_eq!(
        (state.attempts, state.transactions, state.retries),
        (2, 1, 1)
    );
    assert_eq!(
        state.data,
        expected_physical(&layout(), &named(&fixtures, &["fidelity", "v2_1"]))
    );
}

#[test]
fn permanent_merged_failure_isolates_requests_after_partial_commits() {
    let fixtures = vectors();
    let a = named(&fixtures, &["v2_0", "v2_1"]);
    let b = named(&fixtures, &["v2_2"]);
    let db = database();
    // One-span transactions: the merged call commits A then fails on B (2004 is
    // terminal); isolation replays A (two commits) and B fails alone.
    db.0.lock().unwrap().outcomes =
        VecDeque::from([None, None, Some(2004), None, None, Some(2004)]);
    let writer = Writer::new(db.clone(), &layout(), 1, RecordCompression::None).unwrap();
    let collector = Collector::start(Arc::new(writer), queue(3, 1)).unwrap();
    collector.consume(&bounded(), &otlp(&a)).unwrap();
    collector.consume(&bounded(), &otlp(&b)).unwrap();
    let err = collector.flush(&bounded()).unwrap_err();
    assert_eq!(err.kind, ErrorKind::Operation);
    collector.shutdown(&bounded()).unwrap();
    let stats = collector.stats();
    assert_eq!(
        (
            stats.accepted_spans,
            stats.exported_spans,
            stats.failed_spans
        ),
        (3, 2, 1)
    );
    // Operations: A's two from the merged call plus A's two replayed alone.
    assert_eq!((stats.stored_spans, stats.committed_batches), (4, 4));
    assert_eq!(
        (stats.export_attempts, stats.retries, stats.native_retries),
        (3, 0, 0)
    );
    assert_eq!(stats.export_call_spans, sizes(&[(1, 1), (2, 1), (3, 1)]));
    assert_eq!(stats.transaction_spans, sizes(&[(1, 4)]));
    let completions = collector.completions();
    assert_eq!(completions.len(), 2);
    assert_eq!((completions[0].seq, completions[0].spans), (0, 2));
    assert!(completions[0].error.is_none());
    assert_eq!((completions[1].seq, completions[1].spans), (1, 1));
    assert!(completions[1].error.is_some());
    let state = db.0.lock().unwrap();
    assert_eq!((state.attempts, state.transactions), (6, 6));
    assert_eq!(state.data, expected_physical(&layout(), &a));
}

#[test]
fn canceled_whole_call_retry_keeps_partial_commits_and_a_replay_completes() {
    let fixtures = vectors();
    let four = named(&fixtures, &["v2_0", "v2_1", "v2_2", "v2_3"]);
    let db = database();
    db.0.lock().unwrap().outcomes = VecDeque::from([None, Some(1031)]);
    let writer = Writer::new(db.clone(), &layout(), 2, RecordCompression::None).unwrap();
    // A one-minute backoff: cancellation, not the retry, ends the call.
    let collector = Collector::start(Arc::new(writer), queue(4, 60_000)).unwrap();
    collector.consume(&bounded(), &otlp(&four)).unwrap();
    let limit = bounded();
    while collector.stats().retries == 0 {
        limit.check().unwrap();
        std::thread::yield_now();
    }
    let canceled = Context::default();
    canceled.cancel();
    assert_eq!(
        collector.shutdown(&canceled).unwrap_err().kind,
        ErrorKind::Canceled
    );
    assert!(collector.shutdown(&bounded()).is_err());
    let stats = collector.stats();
    assert_eq!(
        (stats.exported_spans, stats.failed_spans, stats.queued_spans),
        (0, 4, 0)
    );
    assert_eq!((stats.stored_spans, stats.committed_batches), (2, 1));
    assert_eq!((stats.export_attempts, stats.retries), (1, 1));
    assert_eq!(
        db.0.lock().unwrap().data,
        expected_physical(&layout(), &four[..2])
    );
    // Replaying the whole call later is idempotent and completes the population.
    let writer = Writer::new(db.clone(), &layout(), 2, RecordCompression::None).unwrap();
    let replay = Collector::start(Arc::new(writer), queue(4, 1)).unwrap();
    replay.consume(&bounded(), &otlp(&four)).unwrap();
    replay.shutdown(&bounded()).unwrap();
    let stats = replay.stats();
    assert_eq!(
        (
            stats.exported_spans,
            stats.stored_spans,
            stats.committed_batches
        ),
        (4, 4, 2)
    );
    assert_eq!(
        db.0.lock().unwrap().data,
        expected_physical(&layout(), &four)
    );
}

/// Every exact Collector counter, functional or ACK-side; none of its diagnostics.
fn counters(s: &Stats) -> [u64; 10] {
    [
        s.accepted_spans,
        s.rejected_spans,
        s.exported_spans,
        s.stored_spans,
        s.committed_batches,
        s.export_attempts,
        s.native_retries,
        s.failed_spans,
        s.retries,
        s.queued_spans as u64,
    ]
}
/// The fake database's attempts, transactions, native retries and final contents.
type Outcome = (usize, usize, usize, BTreeMap<Vec<u8>, Vec<u8>>);
fn outcome(db: &Fake) -> Outcome {
    let state = db.0.lock().unwrap();
    (
        state.attempts,
        state.transactions,
        state.retries,
        state.data.clone(),
    )
}
struct Queued {
    stats: Stats,
    completions: Vec<Completion>,
    flush: Option<ErrorKind>,
    db: Outcome,
}
/// One real Writer -> Collector run on a fresh fake, both constructed under
/// `diagnostics`: admit each request, Flush, shut down, read the settled result.
fn queued(
    diagnostics: Diagnostics,
    outcomes: &[Option<i32>],
    writer_batch: usize,
    queue_batch: usize,
    requests: &[Vec<Vec<u8>>],
) -> Queued {
    let db = database();
    db.0.lock().unwrap().outcomes = outcomes.iter().copied().collect();
    let writer = Writer::new_with_diagnostics(
        db.clone(),
        &layout(),
        writer_batch,
        RecordCompression::None,
        diagnostics,
    )
    .unwrap();
    let collector =
        Collector::start_with_diagnostics(Arc::new(writer), queue(queue_batch, 1), diagnostics)
            .unwrap();
    for request in requests {
        collector.consume(&bounded(), request).unwrap();
    }
    let flush = collector.flush(&bounded()).err().map(|e| e.kind);
    collector.shutdown(&bounded()).unwrap();
    let (stats, completions) = collector.into_final();
    Queued {
        stats,
        completions,
        flush,
        db: outcome(&db),
    }
}

/// Disabled diagnostics change no outcome: the same counters, Flush results, physical
/// map and transaction history as the default, with nothing optional collected.
#[test]
fn disabled_diagnostics_keep_every_collector_outcome() {
    let fixtures = vectors();
    let pick = |names: &[&str]| otlp(&named(&fixtures, names));
    let four = pick(&["v2_0", "v2_1", "v2_2", "v2_3"]);
    for (name, outcomes, writer_batch, queue_batch, requests, flushed) in [
        // Three requests split across queue batches of 3, the last one short.
        (
            "split requests and a short final batch",
            vec![],
            2,
            3,
            vec![four.clone(), pick(&["v2_4"]), pick(&["v2_5", "v2_6"])],
            None,
        ),
        // The call's first transaction commits, its second gets 1031: a whole-call retry.
        (
            "partial success then a whole-call retry",
            vec![None, Some(1031)],
            2,
            4,
            vec![four.clone()],
            None,
        ),
        (
            "native OnError and dedup",
            vec![Some(1020)],
            64,
            3,
            vec![pick(&["v2_0", "v2_1", "fidelity"])],
            None,
        ),
        (
            "permanent merged failure and isolation",
            vec![None, None, Some(2004), None, None, Some(2004)],
            1,
            3,
            vec![pick(&["v2_0", "v2_1"]), pick(&["v2_2"])],
            Some(ErrorKind::Operation),
        ),
    ] {
        let on = queued(
            Diagnostics::Enabled,
            &outcomes,
            writer_batch,
            queue_batch,
            &requests,
        );
        let off = queued(
            Diagnostics::Disabled,
            &outcomes,
            writer_batch,
            queue_batch,
            &requests,
        );
        assert_eq!(counters(&off.stats), counters(&on.stats), "{name}");
        assert_eq!((on.flush, off.flush), (flushed, flushed), "{name}");
        assert_eq!(off.db, on.db, "{name}");
        assert!(on.db.3.len() > 1, "{name}");
        assert_eq!(on.stats.queued_spans, 0, "{name}");
        // The default collects; the disabled run has nothing optional to report.
        assert_eq!(on.completions.len(), requests.len(), "{name}");
        assert!(
            !on.stats.export_call_spans.is_empty() && !on.stats.transaction_spans.is_empty(),
            "{name}"
        );
        assert!(off.completions.is_empty(), "{name}");
        assert!(
            off.stats.export_call_spans.is_empty() && off.stats.transaction_spans.is_empty(),
            "{name}"
        );
        assert_eq!(
            (
                off.stats.max_outstanding_spans,
                off.stats.blocked_requests,
                off.stats.completions_omitted
            ),
            (0, 0, 0),
            "{name}"
        );
    }
}

/// Cancellation in a whole-call retry's backoff ends the same way with diagnostics
/// disabled: the partial acknowledgment is kept and every worker joins.
#[test]
fn disabled_diagnostics_keep_partial_progress_through_cancellation() {
    let fixtures = vectors();
    let four = named(&fixtures, &["v2_0", "v2_1", "v2_2", "v2_3"]);
    let mut runs = Vec::new();
    for diagnostics in [Diagnostics::Enabled, Diagnostics::Disabled] {
        let db = database();
        db.0.lock().unwrap().outcomes = VecDeque::from([None, Some(1031)]);
        let writer = Writer::new_with_diagnostics(
            db.clone(),
            &layout(),
            2,
            RecordCompression::None,
            diagnostics,
        )
        .unwrap();
        // A one-minute backoff: cancellation, not the retry, ends the call.
        let collector =
            Collector::start_with_diagnostics(Arc::new(writer), queue(4, 60_000), diagnostics)
                .unwrap();
        collector.consume(&bounded(), &otlp(&four)).unwrap();
        let limit = bounded();
        while collector.stats().retries == 0 {
            limit.check().unwrap();
            std::thread::yield_now();
        }
        let canceled = Context::default();
        canceled.cancel();
        assert_eq!(
            collector.shutdown(&canceled).unwrap_err().kind,
            ErrorKind::Canceled
        );
        let (stats, completions) = collector.into_final();
        // Joined: the map read now cannot change afterwards.
        let settled = outcome(&db);
        std::thread::sleep(Duration::from_millis(20));
        assert_eq!(outcome(&db), settled);
        assert_eq!(settled.3, expected_physical(&layout(), &four[..2]));
        assert_eq!(counters(&stats), [4, 0, 0, 2, 1, 1, 0, 4, 1, 0]);
        assert_eq!(
            completions.len(),
            usize::from(diagnostics == Diagnostics::Enabled)
        );
        runs.push(settled);
    }
    assert_eq!(runs[0], runs[1]);
}
