use crate::{
    Error, ErrorKind, Result,
    row::{ACTUAL_TXN_BYTES, ADMISSION_BYTES, INDEXES, PreparedSpan, Schema},
    storage::{self, RecordCompression},
    tuple::{self, Element},
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Instant,
};

pub type KeyValues = Vec<(Vec<u8>, Vec<u8>)>;

#[derive(Clone, Default)]
pub struct Context {
    canceled: Arc<AtomicBool>,
    pub deadline: Option<Instant>,
}
impl Context {
    pub fn cancel(&self) {
        self.canceled.store(true, Ordering::Release);
    }
    pub fn with_deadline(mut self, deadline: Instant) -> Self {
        self.deadline = Some(deadline);
        self
    }
    pub fn check(&self) -> Result<()> {
        if self.canceled.load(Ordering::Acquire)
            || self.deadline.is_some_and(|d| Instant::now() >= d)
        {
            Err(Error::new(
                ErrorKind::Canceled,
                "caller canceled or deadline ended",
            ))
        } else {
            Ok(())
        }
    }
}

/// Implementations must issue serializable range reads, return every key or an
/// error, and abort on drop. Native OnError owns retryability and backoff.
pub trait Transaction {
    fn get(&mut self, key: &[u8], ctx: &Context) -> Result<Option<Vec<u8>>>;
    fn ranges(&mut self, ranges: &[(Vec<u8>, Vec<u8>)], ctx: &Context) -> Result<Vec<KeyValues>>;
    fn set(&mut self, key: &[u8], value: &[u8]);
    fn clear(&mut self, key: &[u8]);
    fn approximate_size(&mut self, ctx: &Context) -> Result<i64>;
    fn commit(&mut self, ctx: &Context) -> Result<()>;
    fn on_error(&mut self, code: i32, ctx: &Context) -> Result<()>;
}
pub trait Database: Send + Sync {
    fn transaction(&self) -> Result<Box<dyn Transaction + '_>>;
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ExpectedValue {
    pub key: String,
    pub value: Option<String>,
}

/// An immutable, exclusively owned tenant resolved by the Go control plane.
/// Prefixes are hex; every expected value is read in every write transaction.
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Layout {
    pub version: u32,
    pub descriptor_sha256: String,
    pub record_prefix: String,
    pub index_prefixes: BTreeMap<String, String>,
    pub checks: Vec<ExpectedValue>,
    pub empty_ranges: Vec<(String, String)>,
    pub messages: String,
}

#[derive(Clone)]
struct BoundLayout {
    records: Vec<u8>,
    indexes: BTreeMap<String, Vec<u8>>,
    checks: Vec<(Vec<u8>, Option<Vec<u8>>)>,
    empty_ranges: Vec<(Vec<u8>, Vec<u8>)>,
}

impl Layout {
    pub fn validate(&self) -> Result<()> {
        self.bind().map(|_| ())
    }

    fn bind(&self) -> Result<BoundLayout> {
        let bad = || {
            Error::new(
                ErrorKind::Config,
                "invalid or unsupported OTEL_V1 layout manifest",
            )
        };
        let digest = hex::encode(Sha256::digest(include_bytes!("../testdata/descriptors.pb")));
        if self.version != 1
            || self.descriptor_sha256 != digest
            || self.messages != "none"
            || self.checks.is_empty()
            || self.empty_ranges.is_empty()
            || self.index_prefixes.len() != INDEXES.len()
            || !INDEXES.iter().all(|n| self.index_prefixes.contains_key(*n))
        {
            return Err(bad());
        }
        let parse = |text: &str| hex::decode(text).map_err(|_| bad());
        let records = parse(&self.record_prefix)?;
        if records.is_empty() {
            return Err(bad());
        }
        let mut indexes = BTreeMap::new();
        for (n, p) in &self.index_prefixes {
            let prefix = parse(p)?;
            if prefix.is_empty() {
                return Err(bad());
            }
            indexes.insert(n.clone(), prefix);
        }
        let checks = self
            .checks
            .iter()
            .map(|c| {
                Ok((
                    parse(&c.key)?,
                    c.value.as_ref().map(|v| parse(v)).transpose()?,
                ))
            })
            .collect::<Result<Vec<_>>>()?;
        let empty_ranges = self
            .empty_ranges
            .iter()
            .map(|(a, b)| Ok((parse(a)?, parse(b)?)))
            .collect::<Result<Vec<_>>>()?;
        if checks.iter().any(|(k, _)| k.is_empty())
            || empty_ranges.iter().any(|(a, b)| a.is_empty() || a >= b)
        {
            return Err(bad());
        }
        Ok(BoundLayout {
            records,
            indexes,
            checks,
            empty_ranges,
        })
    }
}

/// Whether a Writer or Collector collects its optional diagnostics: size maps, logical
/// OTLP bytes, request completions and capacity-wait timers. Chosen once, at
/// construction. Disabled leaves those fields uncollected (empty or zero), which is
/// not an observation; the ACK, batch, retry and queue counters stay exact either way.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Diagnostics {
    #[default]
    Enabled,
    Disabled,
}

/// Counts entries into diagnostic-only work, per instance, so unit tests can show a
/// disabled policy never starts it. Outside this crate's unit tests it is empty.
#[derive(Default)]
pub(crate) struct Probe {
    #[cfg(test)]
    entered: std::sync::atomic::AtomicUsize,
}
impl Probe {
    pub(crate) fn enter(&self) {
        #[cfg(test)]
        self.entered.fetch_add(1, Ordering::Relaxed);
    }
    #[cfg(test)]
    pub(crate) fn entered(&self) -> usize {
        self.entered.load(Ordering::Relaxed)
    }
}

#[derive(Default, Debug, Clone, Serialize)]
pub struct WriteStats {
    pub committed_spans: u64,
    /// Committed transactions keyed by their span count; uncollected when the
    /// Writer's diagnostics are disabled.
    pub transaction_spans: BTreeMap<u64, u64>,
    pub committed_batches: u64,
    /// Uncollected (0) when the Writer's diagnostics are disabled.
    pub logical_otlp_bytes: u64,
    pub retries: u64,
}

pub struct Writer<D: Database> {
    db: D,
    schema: Schema,
    layout: BoundLayout,
    batch_size: usize,
    compression: RecordCompression,
    diagnostics: Diagnostics,
    probe: Probe,
}
impl<D: Database> Writer<D> {
    pub fn new(
        db: D,
        layout: &Layout,
        batch_size: usize,
        compression: RecordCompression,
    ) -> Result<Self> {
        Self::new_with_diagnostics(db, layout, batch_size, compression, Diagnostics::Enabled)
    }

    /// `new` with an explicit diagnostics policy; a Disabled Writer never builds its
    /// transaction-size map or sums logical OTLP bytes.
    pub fn new_with_diagnostics(
        db: D,
        layout: &Layout,
        batch_size: usize,
        compression: RecordCompression,
        diagnostics: Diagnostics,
    ) -> Result<Self> {
        if !(1..=64).contains(&batch_size) {
            return Err(Error::new(ErrorKind::Config, "batch size must be 1..64"));
        }
        Ok(Self {
            db,
            schema: Schema::pinned()?,
            layout: layout.bind()?,
            batch_size,
            compression,
            diagnostics,
            probe: Probe::default(),
        })
    }

    /// Failure keeps earlier committed batches in the returned progress, never
    /// counts an uncertain/failed commit, and never replays a complete call.
    pub fn write(&self, ctx: &Context, inputs: &[Vec<u8>]) -> (WriteStats, Result<()>) {
        let mut stats = WriteStats::default();
        let result = (|| {
            ctx.check()?;
            let spans = self.schema.prepare(inputs)?;
            ctx.check()?;
            let mut start = 0;
            while start < spans.len() {
                let mut end = start;
                let mut estimate = 0;
                while end < spans.len()
                    && end - start < self.batch_size
                    && estimate + spans[end].estimate <= ADMISSION_BYTES
                {
                    estimate += spans[end].estimate;
                    end += 1;
                }
                self.save(ctx, &spans[start..end], &mut stats)?;
                start = end;
            }
            Ok(())
        })();
        (stats, result)
    }

    fn save(&self, ctx: &Context, spans: &[PreparedSpan], stats: &mut WriteStats) -> Result<()> {
        let mut tx = self.db.transaction()?;
        loop {
            ctx.check()?;
            match self.attempt(tx.as_mut(), ctx, spans) {
                Ok(()) => {
                    stats.committed_spans += spans.len() as u64;
                    stats.committed_batches += 1;
                    if self.diagnostics == Diagnostics::Enabled {
                        self.probe.enter();
                        *stats
                            .transaction_spans
                            .entry(spans.len() as u64)
                            .or_default() += 1;
                        stats.logical_otlp_bytes +=
                            spans.iter().map(|s| s.otlp.len() as u64).sum::<u64>();
                    }
                    return Ok(());
                }
                Err(err) if err.kind == ErrorKind::Budget => {
                    // Retry smaller transactions without publishing the oversized
                    // transaction. A single replacement cannot fit by splitting.
                    drop(tx);
                    if spans.len() == 1 {
                        return Err(Error::new(
                            ErrorKind::Operation,
                            "span replacement exceeds actual 500KB transaction guard",
                        ));
                    }
                    let half = spans.len() / 2;
                    self.save(ctx, &spans[..half], stats)?;
                    return self.save(ctx, &spans[half..], stats);
                }
                Err(err) => {
                    if let Some(code) = err.fdb_code {
                        tx.on_error(code, ctx)?;
                        stats.retries += 1;
                    } else {
                        return Err(err);
                    }
                }
            }
        }
    }

    fn attempt(
        &self,
        tx: &mut dyn Transaction,
        ctx: &Context,
        spans: &[PreparedSpan],
    ) -> Result<()> {
        for (key, expected) in &self.layout.checks {
            if tx.get(key, ctx)? != *expected {
                return Err(Error::new(
                    ErrorKind::Ownership,
                    "tenant header/layout/catalog seal changed",
                ));
            }
        }
        if tx
            .ranges(&self.layout.empty_ranges, ctx)?
            .iter()
            .any(|r| !r.is_empty())
        {
            return Err(Error::new(
                ErrorKind::Ownership,
                "tenant index state changed",
            ));
        }
        let ranges = spans
            .iter()
            .map(|s| {
                let mut prefix = self.layout.records.clone();
                prefix.extend_from_slice(&s.primary_key);
                let mut begin = prefix.clone();
                begin.push(0);
                let mut end = prefix;
                end.push(0xff);
                (begin, end)
            })
            .collect::<Vec<_>>();
        let prior = tx.ranges(&ranges, ctx)?;
        if prior.len() != spans.len() {
            return Err(Error::new(
                ErrorKind::Operation,
                "incomplete prior-image read",
            ));
        }
        for (span, old) in spans.iter().zip(prior) {
            ctx.check()?;
            let mut record_prefix = self.layout.records.clone();
            record_prefix.extend_from_slice(&span.primary_key);
            let old_row = self.old_row(&record_prefix, &span.primary_key, &old)?;
            let new_keys = self.physical_indexes(&span.indexes);
            if let Some(row) = old_row {
                for key in self
                    .physical_indexes(&self.schema.index_entries(&row))
                    .difference(&new_keys)
                {
                    tx.clear(key);
                }
            }
            for (key, _) in &old {
                tx.clear(key);
            }
            let stored = storage::encode(&span.row, self.compression)?;
            for (suffix, value) in storage::split(&stored) {
                let mut key = record_prefix.clone();
                key.extend_from_slice(&tuple::pack(&[Element::Int(suffix)]));
                if key.len() > 10_000 {
                    return Err(Error::new(
                        ErrorKind::Admission,
                        "record key exceeds FDB limit",
                    ));
                }
                tx.set(&key, &value);
            }
            for key in new_keys {
                tx.set(&key, &[]);
            }
        }
        if tx.approximate_size(ctx)? > ACTUAL_TXN_BYTES {
            return Err(Error::new(
                ErrorKind::Budget,
                "actual transaction size exceeds 500KB",
            ));
        }
        ctx.check()?;
        tx.commit(ctx)
    }

    fn physical_indexes(&self, entries: &BTreeMap<String, Vec<Vec<u8>>>) -> BTreeSet<Vec<u8>> {
        let mut keys = BTreeSet::new();
        for (index, entries) in entries {
            for entry in entries {
                let mut key = self.layout.indexes[index].clone();
                key.extend_from_slice(entry);
                keys.insert(key);
            }
        }
        keys
    }

    fn old_row(
        &self,
        prefix: &[u8],
        expected_pk: &[u8],
        old: &KeyValues,
    ) -> Result<Option<crate::records::Spans>> {
        if old.is_empty() {
            return Ok(None);
        }
        let corrupt = || {
            Error::new(
                ErrorKind::Corruption,
                "invalid split record or primary-key disagreement",
            )
        };
        let mut stored = Vec::new();
        let unsplit = old.len() == 1 && old[0].0 == [prefix, &[0x14]].concat();
        for (i, (key, value)) in old.iter().enumerate() {
            let suffix = if unsplit { 0 } else { i as i64 + 1 };
            if *key != [prefix, &tuple::pack(&[Element::Int(suffix)])].concat()
                || value.len() > 100_000
            {
                return Err(corrupt());
            }
            stored.extend_from_slice(value);
        }
        let row = self.schema.decode_row(&storage::decode(&stored)?)?;
        let trace = row.trace_id.as_deref().unwrap_or_default();
        let span = row.span_id.as_deref().unwrap_or_default();
        if tuple::pack(&[Element::Int(0), Element::Bytes(trace), Element::Bytes(span)])
            != expected_pk
        {
            return Err(corrupt());
        }
        Ok(Some(row))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::{Engine, engine::general_purpose::STANDARD};
    use std::sync::Mutex;

    /// An in-memory database: `conflicts` commits fail with 1020 first, and commit
    /// number `fail_at` fails terminally.
    #[derive(Clone, Default)]
    struct Memory(Arc<Mutex<MemoryState>>);
    #[derive(Default)]
    struct MemoryState {
        data: BTreeMap<Vec<u8>, Vec<u8>>,
        conflicts: usize,
        fail_at: Option<usize>,
        commits: usize,
    }
    struct Tx {
        db: Memory,
        changes: BTreeMap<Vec<u8>, Option<Vec<u8>>>,
    }
    impl Database for Memory {
        fn transaction(&self) -> Result<Box<dyn Transaction + '_>> {
            Ok(Box::new(Tx {
                db: self.clone(),
                changes: BTreeMap::new(),
            }))
        }
    }
    impl Transaction for Tx {
        fn get(&mut self, key: &[u8], _: &Context) -> Result<Option<Vec<u8>>> {
            Ok(self.db.0.lock().unwrap().data.get(key).cloned())
        }
        fn ranges(&mut self, ranges: &[(Vec<u8>, Vec<u8>)], _: &Context) -> Result<Vec<KeyValues>> {
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
            Ok(0)
        }
        fn commit(&mut self, _: &Context) -> Result<()> {
            let mut state = self.db.0.lock().unwrap();
            if state.conflicts > 0 {
                state.conflicts -= 1;
                return Err(Error {
                    kind: ErrorKind::Transient,
                    message: "injected conflict".into(),
                    fdb_code: Some(1020),
                });
            }
            state.commits += 1;
            if state.fail_at == Some(state.commits) {
                return Err(Error {
                    kind: ErrorKind::Operation,
                    message: "injected terminal failure".into(),
                    fdb_code: Some(2000),
                });
            }
            for (k, v) in std::mem::take(&mut self.changes) {
                match v {
                    Some(v) => state.data.insert(k, v),
                    None => state.data.remove(&k),
                };
            }
            Ok(())
        }
        fn on_error(&mut self, code: i32, _: &Context) -> Result<()> {
            self.changes.clear();
            if code == 1020 {
                Ok(())
            } else {
                Err(Error::new(ErrorKind::Operation, "terminal FDB error"))
            }
        }
    }
    fn layout() -> Layout {
        Layout {
            version: 1,
            descriptor_sha256: hex::encode(Sha256::digest(include_bytes!(
                "../testdata/descriptors.pb"
            ))),
            record_prefix: "01".into(),
            index_prefixes: INDEXES
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
    fn inputs() -> Vec<Vec<u8>> {
        let vectors: serde_json::Value =
            serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
        vectors
            .as_array()
            .unwrap()
            .iter()
            .filter(|v| v["name"].as_str().unwrap().starts_with("v2_"))
            .map(|v| STANDARD.decode(v["otlp"].as_str().unwrap()).unwrap())
            .collect()
    }

    /// One write of the eight fixture spans in batches of 3 under `diagnostics`: its
    /// progress, outcome, diagnostic entries and the database afterward.
    fn write(
        diagnostics: Diagnostics,
        conflicts: usize,
        fail_at: Option<usize>,
    ) -> (WriteStats, Result<()>, usize, BTreeMap<Vec<u8>, Vec<u8>>) {
        let db = Memory::default();
        {
            let mut state = db.0.lock().unwrap();
            state.data.insert(vec![0], vec![1, 2]);
            state.conflicts = conflicts;
            state.fail_at = fail_at;
        }
        let writer = Writer::new_with_diagnostics(
            db.clone(),
            &layout(),
            3,
            RecordCompression::None,
            diagnostics,
        )
        .unwrap();
        let (stats, result) = writer.write(&Context::default(), &inputs());
        let data = db.0.lock().unwrap().data.clone();
        (stats, result, writer.probe.entered(), data)
    }

    #[test]
    fn disabled_diagnostics_are_never_entered_and_progress_stays_exact() {
        assert_eq!(inputs().len(), 8);
        // A 1020 conflict and native retry, then success; then a terminal failure on
        // the second commit after the first batch's acknowledgment.
        for (conflicts, fail_at, batches) in [(1, None, 3), (0, Some(2), 1)] {
            let (on, on_result, on_entered, on_data) =
                write(Diagnostics::Enabled, conflicts, fail_at);
            let (off, off_result, off_entered, off_data) =
                write(Diagnostics::Disabled, conflicts, fail_at);
            assert_eq!(on_result.is_ok(), fail_at.is_none());
            assert_eq!(off_result.is_ok(), fail_at.is_none());
            for stats in [&on, &off] {
                assert_eq!(
                    (
                        stats.committed_spans,
                        stats.committed_batches,
                        stats.retries
                    ),
                    (
                        if fail_at.is_none() { 8 } else { 3 },
                        batches,
                        conflicts as u64
                    )
                );
            }
            assert!(on_data.len() > 1);
            assert_eq!(on_data, off_data);
            // The default policy collects: one diagnostic entry per committed batch.
            assert_eq!(on_entered, batches as usize);
            assert_eq!(on.transaction_spans.values().sum::<u64>(), batches);
            assert!(on.logical_otlp_bytes > 0);
            // Disabled never starts that work, rather than discarding its result.
            assert_eq!(off_entered, 0);
            assert!(off.transaction_spans.is_empty());
            assert_eq!(off.logical_otlp_bytes, 0);
        }
        let default =
            Writer::new(Memory::default(), &layout(), 3, RecordCompression::None).unwrap();
        assert_eq!(default.diagnostics, Diagnostics::Enabled);
    }
}
