//! Deterministic transaction state-machine tests, NOT live-FDB conformance.
use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{
    Error, ErrorKind, Result,
    row::Schema,
    storage::RecordCompression,
    writer::{Context, Database, ExpectedValue, KeyValues, Layout, Transaction, Writer},
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, VecDeque},
    sync::{Arc, Mutex},
};

#[derive(Default)]
struct State {
    data: BTreeMap<Vec<u8>, Vec<u8>>,
    outcomes: VecDeque<Option<i32>>,
    attempts: usize,
    retries: usize,
    transactions: usize,
    force_large: bool,
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
        Ok(if self.db.0.lock().unwrap().force_large {
            500_001
        } else {
            self.changes
                .iter()
                .map(|(k, v)| k.len() + v.as_ref().map_or(0, Vec::len))
                .sum::<usize>() as i64
        })
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
            return Err(Error {
                kind: ErrorKind::Operation,
                message: "terminal FDB error".into(),
                fdb_code: Some(code),
            });
        }
        self.db.0.lock().unwrap().retries += 1;
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
}
fn inputs() -> Vec<Vec<u8>> {
    let vectors: Vec<Vector> =
        serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    vectors
        .into_iter()
        .filter(|v| v.name.starts_with("v2_"))
        .map(|v| STANDARD.decode(v.otlp).unwrap())
        .collect()
}
fn database() -> Fake {
    let db = Fake::default();
    db.0.lock().unwrap().data.insert(vec![0], vec![1, 2]);
    db
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
    // Same identity, changed timestamp/name/string terms. Projected old entries
    // must disappear, not merely have a new set appended.
    let pool = prost_reflect::DescriptorPool::decode(
        include_bytes!("../testdata/descriptors.pb").as_slice(),
    )
    .unwrap();
    let desc = pool
        .get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
        .unwrap();
    use prost::Message;
    use prost_reflect::Value;
    let mut rs = prost_reflect::DynamicMessage::decode(desc, source[0].as_slice()).unwrap();
    let mut ss = rs
        .get_field_by_name("scope_spans")
        .unwrap()
        .as_list()
        .unwrap()[0]
        .as_message()
        .unwrap()
        .clone();
    let mut sp = ss.get_field_by_name("spans").unwrap().as_list().unwrap()[0]
        .as_message()
        .unwrap()
        .clone();
    sp.set_field_by_name("start_time_unix_nano", Value::U64(99));
    sp.set_field_by_name("attributes", Value::List(vec![]));
    ss.set_field_by_name("spans", Value::List(vec![Value::Message(sp)]));
    rs.set_field_by_name("scope_spans", Value::List(vec![Value::Message(ss)]));
    let changed = rs.encode_to_vec();
    writer
        .write(&Context::default(), std::slice::from_ref(&changed))
        .1
        .unwrap();
    let keys = Schema::pinned().unwrap().prepare(&[changed]).unwrap()[0]
        .indexes
        .clone();
    let expected_indexes = 7 * 10 + keys.values().map(Vec::len).sum::<usize>();
    assert_eq!(db.0.lock().unwrap().data.len(), 1 + 8 + expected_indexes);
    assert_ne!(db.0.lock().unwrap().data, first);
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
