//! Source-only driver tests. The injected opener never loads a native client.
use super::*;
use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{
    row::INDEXES,
    writer::{ExpectedValue, KeyValues, Transaction},
};
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage, Value};
use serde_json::{Value as Json, json};
use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    os::unix::fs::PermissionsExt,
    sync::{
        OnceLock,
        atomic::{AtomicUsize, Ordering},
        mpsc,
    },
};

const FROZEN_SCHEMA_SHA256: &str =
    "c7d9ae94ed71db095f0de48999e2683a2c12f96525241f1e9a7471f836f57ec6";
/// The frozen schema's ingest-mode fields; `Job` must require exactly these.
const JOB_FIELDS: [&str; 28] = [
    "schema",
    "mode",
    "arm",
    "binary_sha256",
    "source_revision",
    "source_manifest_sha256",
    "timeout_seconds",
    "output",
    "corpus_config",
    "expected_frames",
    "row_budget",
    "corpus_file",
    "run",
    "cluster_file",
    "cluster_sha256",
    "client_backend",
    "client_library",
    "client_library_sha256",
    "corpus_sha256",
    "workload",
    "writer_batch_size",
    "writer_workers",
    "record_compression",
    "enable_message_deltas",
    "enable_message_full_text",
    "bulk_index_conflicts",
    "layout_file",
    "layout_sha256",
];
const CORPUS_FIELDS: [&str; 6] = [
    "generator",
    "seed",
    "count",
    "tenants",
    "attributes",
    "match_every",
];
const DIRECT_FIELDS: [&str; 3] = ["kind", "producers", "producer_batch_spans"];
const QUEUE_FIELDS: [&str; 14] = [
    "kind",
    "producers",
    "producer_batch_spans",
    "consumers",
    "queue_capacity_spans",
    "batch_min_spans",
    "batch_max_spans",
    "flush_ms",
    "block_on_overflow",
    "export_timeout_ms",
    "retry_enabled",
    "retry_initial_ms",
    "retry_max_ms",
    "retry_elapsed_ms",
];
/// The Stage0 point2 and paid-pilot workload, byte for byte.
const POINT2: &str = r#"{"kind":"collector_queue","producers":8,"producer_batch_spans":1,"consumers":8,"queue_capacity_spans":10000,"batch_min_spans":64,"batch_max_spans":256,"flush_ms":200,"block_on_overflow":true,"export_timeout_ms":120000,"retry_enabled":true,"retry_initial_ms":500,"retry_max_ms":10000,"retry_elapsed_ms":300000}"#;
const SPAWN_ERROR: &str = "Operation: cannot spawn producer thread";
const STAND_IN_CLIENT: &[u8] = b"never-loaded stand-in client library";

fn sha256(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}
fn exe_sha256() -> &'static str {
    static EXE: OnceLock<String> = OnceLock::new();
    EXE.get_or_init(|| hash_file(&std::env::current_exe().unwrap()).unwrap())
}

static TEMP_ID: AtomicUsize = AtomicUsize::new(0);
/// A private directory with one layout, cluster file and stand-in client library.
struct Files {
    dir: PathBuf,
    next: AtomicUsize,
    pin: String,
}
impl Files {
    fn new() -> Self {
        let dir = std::env::temp_dir().join(format!(
            "otel-rust-driver-{}-{}",
            std::process::id(),
            TEMP_ID.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&dir).unwrap();
        let layout = Layout {
            version: 1,
            descriptor_sha256: sha256(include_bytes!("../testdata/descriptors.pb")),
            record_prefix: "01".into(),
            index_prefixes: INDEXES
                .iter()
                .enumerate()
                .map(|(i, name)| ((*name).into(), format!("02{i:02x}")))
                .collect(),
            checks: vec![ExpectedValue {
                key: "00".into(),
                value: Some("0102".into()),
            }],
            empty_ranges: vec![("03".into(), "04".into())],
            messages: "none".into(),
        };
        fs::write(
            dir.join("layout.json"),
            serde_json::to_vec(&layout).unwrap(),
        )
        .unwrap();
        fs::write(dir.join("fdb.cluster"), b"test:cluster@127.0.0.1:4500").unwrap();
        fs::write(dir.join("libfdb_c.so"), STAND_IN_CLIENT).unwrap();
        Self {
            dir,
            next: AtomicUsize::new(0),
            pin: sha256(STAND_IN_CLIENT),
        }
    }
    fn path(&self, stem: &str) -> PathBuf {
        self.dir.join(format!(
            "{stem}-{}",
            self.next.fetch_add(1, Ordering::Relaxed)
        ))
    }
    /// A valid ingest job over a fresh corpus of `frames` and a fresh output path.
    fn job(&self, frames: &[Vec<u8>], workload: Json) -> Json {
        let corpus = self.path("corpus");
        fs::write(&corpus, framed(frames)).unwrap();
        let file = |name: &str| self.dir.join(name);
        let n = frames.len();
        json!({
            "schema": "otel_paired_job_v1",
            "mode": "ingest",
            "arm": "rust_native",
            "binary_sha256": exe_sha256(),
            "source_revision": "1".repeat(40),
            "source_manifest_sha256": "2".repeat(64),
            "timeout_seconds": 5,
            "output": self.path("receipt"),
            "corpus_config": {
                "generator": "otel_fixture_v2",
                "seed": u64::MAX,
                "count": n,
                "tenants": 1,
                "attributes": 8,
                "match_every": 100,
            },
            "expected_frames": n,
            "row_budget": 10,
            "corpus_file": corpus,
            "run": "driver_test_run",
            "cluster_file": file("fdb.cluster"),
            "cluster_sha256": sha256(&fs::read(file("fdb.cluster")).unwrap()),
            "client_backend": "native_c",
            "client_library": file("libfdb_c.so"),
            "client_library_sha256": self.pin,
            "corpus_sha256": sha256(&fs::read(&corpus).unwrap()),
            "workload": workload,
            "writer_batch_size": 1,
            "writer_workers": 1,
            "record_compression": "none",
            "enable_message_deltas": false,
            "enable_message_full_text": false,
            "bulk_index_conflicts": false,
            "layout_file": file("layout.json"),
            "layout_sha256": sha256(&fs::read(file("layout.json")).unwrap()),
        })
    }
    fn write(&self, bytes: &[u8]) -> PathBuf {
        let path = self.path("job");
        fs::write(&path, bytes).unwrap();
        path
    }
    fn run(&self, job: &Json, db: &Fake) -> Result<()> {
        self.run_hooked(job, db, |_| Ok(()))
    }
    fn run_hooked(
        &self,
        job: &Json,
        db: &Fake,
        hook: impl FnMut(usize) -> std::io::Result<()>,
    ) -> Result<()> {
        let path = self.write(&serde_json::to_vec(job).unwrap());
        run_spawning(
            &path,
            &self.pin,
            |client, cluster| {
                assert_eq!(client, self.dir.join("libfdb_c.so"));
                assert_eq!(cluster, self.dir.join("fdb.cluster"));
                Ok(db.clone())
            },
            hook,
        )
    }
    /// The bytes must fail as Config before any open, leaving `output` uncreated.
    fn refuse(&self, bytes: &[u8], output: &Path, case: &str) {
        let err = run_spawning(
            &self.write(bytes),
            &self.pin,
            |_: &Path, _: &Path| -> Result<Fake> {
                panic!("{case}: invalid job reached the opener")
            },
            |_| Ok(()),
        )
        .unwrap_err();
        assert_eq!(err.kind, ErrorKind::Config, "{case}: {err}");
        assert!(!output.exists(), "{case}: output was created");
    }
    fn refuse_json(&self, job: &Json, case: &str) {
        self.refuse(&serde_json::to_vec(job).unwrap(), &output(job), case);
    }
}
impl Drop for Files {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.dir).unwrap();
    }
}
fn output(job: &Json) -> PathBuf {
    PathBuf::from(job["output"].as_str().unwrap())
}
fn receipt(path: &Path) -> Json {
    serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
}
/// The job's report, which must satisfy the frozen ingest report contract.
fn checked_receipt(job: &Json) -> Json {
    let r = receipt(&output(job));
    if let Err(violation) = report_contract(&r, job) {
        panic!("report violates the ingest contract: {violation}\n{r}");
    }
    r
}
/// A plain field, or a counter marker's value.
fn value<'a>(r: &'a Json, field: &str) -> &'a Json {
    if r[field].is_object() {
        &r[field]["value"]
    } else {
        &r[field]
    }
}

/// The frozen schema's required ingest report fields ($defs.ingest_ack_and_samples).
const REPORT_FIELDS: [&str; 23] = [
    "schema",
    "mode",
    "arm",
    "effective_job",
    "acked_span_operations",
    "samples",
    "status",
    "error",
    "frames",
    "received_spans",
    "accepted_spans",
    "rejected_spans",
    "failed_spans",
    "queued_spans_at_exit",
    "ingest_wall_ns",
    "admission_phase_ns",
    "drain_ns",
    "ingest_resources",
    "committed_batches",
    "native_retries",
    "outer_retries",
    "export_attempts",
    "queue_retries",
];
const MARKERS: [&str; 6] = [
    "acked_span_operations",
    "committed_batches",
    "native_retries",
    "outer_retries",
    "export_attempts",
    "queue_retries",
];
const PLAIN_COUNTS: [&str; 6] = [
    "frames",
    "received_spans",
    "accepted_spans",
    "rejected_spans",
    "failed_spans",
    "queued_spans_at_exit",
];
const TIMINGS: [&str; 3] = ["ingest_wall_ns", "admission_phase_ns", "drain_ns"];
const RESOURCE_FIELDS: [&str; 6] = [
    "scope",
    "user_cpu_seconds",
    "system_cpu_seconds",
    "cpu_seconds",
    "peak_rss_bytes",
    "measurement_source",
];
const SAMPLE_FIELDS: [&str; 4] = ["ns", "received_spans", "accepted_spans", "error"];
/// A settled Collector's optional diagnostics, each disabled in the measured path.
const QUEUE_DIAGNOSTIC_FIELDS: [&str; 6] = [
    "max_outstanding_spans",
    "blocked_requests",
    "blocked_ns",
    "export_call_spans",
    "completion_samples",
    "completion_samples_omitted",
];
fn disabled_diagnostic() -> Json {
    json!({"availability": "unavailable", "reason": DIAGNOSTICS_OFF})
}

fn nonblank(v: &Json) -> bool {
    v.as_str().is_some_and(|s| !s.trim().is_empty())
}
fn finite_nonnegative(v: &Json) -> bool {
    v.as_f64().is_some_and(|x| x.is_finite() && x >= 0.0)
}

/// The frozen report contract for rust_native ingest, plus the binary's own rules:
/// strict counter markers, plain counts and timings, measured resources on success,
/// kind-specific sample shapes, the sealed job echoed, and no census or rate field.
fn report_contract(r: &Json, job: &Json) -> std::result::Result<(), String> {
    let fail = |what: String| Err(what);
    let object = r.as_object().ok_or("report is not an object")?;
    for field in REPORT_FIELDS {
        if !object.contains_key(field) {
            return fail(format!("missing {field}"));
        }
    }
    if r["schema"] != "otel_control_report_v1" || r["mode"] != "ingest" || r["arm"] != "rust_native"
    {
        return fail("schema, mode or arm".into());
    }
    let ok = match (&r["status"], &r["error"]) {
        (s, Json::Null) if s == "ok" => true,
        (s, e) if s == "error" && nonblank(e) => false,
        _ => return fail("status/error pair".into()),
    };
    if r["effective_job"] != *job {
        return fail("effective_job is not the sealed job".into());
    }
    // Distinct spans exist only after the census: absent, not even null.
    if object.contains_key("distinct_spans") {
        return fail("ingest must not carry distinct_spans".into());
    }
    for field in [
        "spans_per_second",
        "rate_numerator",
        "cpu_ms_per_distinct_span",
        "cpu_denominator",
    ] {
        if object.get(field).is_some_and(|v| !v.is_null()) {
            return fail(format!("ingest must not report {field}"));
        }
    }
    // Timings are integers on every report, 0 when ingest never started.
    for field in TIMINGS {
        if !r[field].is_u64() {
            return fail(format!("{field} is not a plain duration"));
        }
    }
    for field in MARKERS {
        let marker = r[field]
            .as_object()
            .ok_or(format!("{field} is not a marker"))?;
        let keys: BTreeSet<&str> = marker.keys().map(String::as_str).collect();
        let valid = match marker.get("availability").and_then(Json::as_str) {
            Some("available") => {
                keys == BTreeSet::from(["availability", "value"]) && marker["value"].is_u64()
            }
            Some("unavailable") => {
                keys == BTreeSet::from(["availability", "reason"]) && nonblank(&marker["reason"])
            }
            _ => false,
        };
        if !valid {
            return fail(format!("{field} marker {}", r[field]));
        }
    }
    if r["acked_span_operations"]["availability"] != "available" {
        return fail("rust_native must report its ACK operations".into());
    }
    for field in PLAIN_COUNTS {
        if !r[field].is_u64() {
            return fail(format!("{field} is not a plain count"));
        }
    }
    let resources = &r["ingest_resources"];
    let reason = object.get("ingest_resources_unavailable_reason");
    if resources.is_null() {
        // Only a failed report may lack the ingest-window measurement, and says why.
        if ok || !reason.is_some_and(nonblank) {
            return fail("ingest_resources null without a failed status and reason".into());
        }
    } else {
        let fields = resources
            .as_object()
            .ok_or("ingest_resources is not an object")?;
        let keys: BTreeSet<&str> = fields.keys().map(String::as_str).collect();
        let (user, system, cpu) = (
            &resources["user_cpu_seconds"],
            &resources["system_cpu_seconds"],
            &resources["cpu_seconds"],
        );
        if reason.is_some()
            || keys != BTreeSet::from(RESOURCE_FIELDS)
            || resources["scope"] != "first_read_through_flush"
            || ![user, system, cpu].into_iter().all(finite_nonnegative)
            || (user.as_f64().unwrap() + system.as_f64().unwrap() - cpu.as_f64().unwrap()).abs()
                > 1e-9
            || resources["peak_rss_bytes"]
                .as_u64()
                .is_none_or(|rss| rss == 0)
            || !nonblank(&resources["measurement_source"])
        {
            return fail(format!("ingest_resources {resources}"));
        }
    }
    let samples = r["samples"].as_array().ok_or("samples is not an array")?;
    if ok && (samples.is_empty() || r["ingest_wall_ns"].as_u64().is_none_or(|ns| ns == 0)) {
        return fail("a successful ingest needs samples and wall time".into());
    }
    let direct = job["workload"]["kind"] == "writer_direct";
    for s in samples {
        let fields = s.as_object().ok_or("sample is not an object")?;
        let mut keys: BTreeSet<&str> = fields.keys().map(String::as_str).collect();
        let ack = keys.remove("direct_write_acked_operations");
        if keys != BTreeSet::from(SAMPLE_FIELDS)
            || (ack && (!direct || !s["direct_write_acked_operations"].is_u64()))
            || !s["ns"].is_u64()
            || s["received_spans"].as_u64().is_none_or(|n| n == 0)
            || !s["accepted_spans"].is_u64()
            || !(s["error"].is_null() || s["error"].is_string())
        {
            return fail(format!("sample {s}"));
        }
    }
    let sum = |pick: &dyn Fn(&Json) -> u64| samples.iter().map(pick).sum::<u64>();
    if r["received_spans"] != sum(&|s| s["received_spans"].as_u64().unwrap()) {
        return fail("received_spans is not the sample population".into());
    }
    if direct {
        let failed = sum(&|s| {
            if s["error"].is_null() {
                0
            } else {
                s["received_spans"].as_u64().unwrap()
            }
        });
        if r["accepted_spans"] != sum(&|s| s["accepted_spans"].as_u64().unwrap())
            || r["failed_spans"] != failed
            || r["rejected_spans"] != 0
            || r["queued_spans_at_exit"] != 0
            || r["export_attempts"]["availability"] != "unavailable"
            || r["queue_retries"]["availability"] != "unavailable"
        {
            return fail("writer_direct counts".into());
        }
    } else if r["export_attempts"]["availability"] != "available"
        || r["queue_retries"]["availability"] != "available"
    {
        return fail("collector_queue counters must be observed".into());
    }
    // Optional diagnostics are disabled in the measured path: each says exactly so, and
    // none is a measured empty map, array or zero, nor a claimed observation.
    if r["transaction_spans"] != disabled_diagnostic() {
        return fail(format!("transaction_spans {}", r["transaction_spans"]));
    }
    let queue = &r["queue"];
    let settled = queue.as_object().is_some_and(|q| {
        q.keys().map(String::as_str).collect::<BTreeSet<_>>()
            == BTreeSet::from(QUEUE_DIAGNOSTIC_FIELDS)
            && q.values().all(|v| *v == disabled_diagnostic())
    });
    // writer_direct has no queue; a Collector reports one once it settled, as on success.
    if (direct && !queue.is_null()) || (!direct && !settled && (ok || !queue.is_null())) {
        return fail(format!("queue diagnostics {queue}"));
    }
    Ok(())
}
fn vectors() -> Vec<Json> {
    let all: Json = serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    all.as_array()
        .unwrap()
        .iter()
        .filter(|v| v["name"].as_str().unwrap().starts_with("v2_"))
        .cloned()
        .collect()
}
/// The eight independently Go-built otel_fixture_v2 frames, v2_0..v2_7.
fn frames() -> Vec<Vec<u8>> {
    vectors()
        .iter()
        .map(|v| STANDARD.decode(v["otlp"].as_str().unwrap()).unwrap())
        .collect()
}
fn framed(frames: &[Vec<u8>]) -> Vec<u8> {
    let mut bytes = Vec::new();
    for frame in frames {
        bytes.extend_from_slice(&(frame.len() as u32).to_be_bytes());
        bytes.extend_from_slice(frame);
    }
    bytes
}
/// The fake's complete map holding exactly v2_i for each index: header, records, keys.
fn expected_physical(indices: &[usize]) -> BTreeMap<Vec<u8>, Vec<u8>> {
    let vectors = vectors();
    let mut data = BTreeMap::from([(vec![0], vec![1, 2])]);
    for v in indices.iter().map(|&i| &vectors[i]) {
        let mut key = vec![0x01];
        key.extend(hex::decode(v["pk"].as_str().unwrap()).unwrap());
        key.push(0x14); // FDB tuple integer zero: unsplit suffix.
        let row = STANDARD.decode(v["row"].as_str().unwrap()).unwrap();
        let mut value = vec![0x0a]; // RecordTypeUnion.SPANS_0, length-delimited.
        prost::encoding::encode_varint(row.len() as u64, &mut value);
        value.extend(row);
        assert!(data.insert(key, value).is_none(), "duplicate identity");
        for (name, entries) in v["keys"].as_object().unwrap() {
            let index = INDEXES.iter().position(|n| n == name).unwrap() as u8;
            for entry in entries.as_array().unwrap() {
                let mut key = vec![0x02, index];
                key.extend(hex::decode(entry.as_str().unwrap()).unwrap());
                data.insert(key, vec![]);
            }
        }
    }
    data
}
fn direct(producers: usize, batch: usize) -> Json {
    json!({"kind": "writer_direct", "producers": producers, "producer_batch_spans": batch})
}
/// A small blocking queue with fast flush and a 1ms retry, every field explicit.
fn queue(producers: usize, consumers: usize, batch_min: usize, batch_max: usize) -> Json {
    json!({
        "kind": "collector_queue",
        "producers": producers,
        "producer_batch_spans": 1,
        "consumers": consumers,
        "queue_capacity_spans": 8,
        "batch_min_spans": batch_min,
        "batch_max_spans": batch_max,
        "flush_ms": 10,
        "block_on_overflow": true,
        "export_timeout_ms": 5000,
        "retry_enabled": true,
        "retry_initial_ms": 1,
        "retry_max_ms": 1,
        "retry_elapsed_ms": 1000,
    })
}
fn unchanged(db: &Fake) -> bool {
    let state = db.0.lock().unwrap();
    state.transactions == 0 && state.data == BTreeMap::from([(vec![0], vec![1, 2])])
}

#[derive(Clone, Copy, Default)]
enum Mode {
    #[default]
    Success,
    FailSecond,
    Deadline,
    /// The first commit applies, signals `committed_signal`, then returns only once canceled.
    AwaitCancelAfterFirst,
}
#[derive(Default)]
struct State {
    data: BTreeMap<Vec<u8>, Vec<u8>>,
    transactions: usize,
    attempts: usize,
    committed: usize,
    mode: Mode,
    committed_signal: Option<mpsc::Sender<()>>,
    saw_cancel: bool,
}
#[derive(Clone)]
struct Fake(Arc<Mutex<State>>);
impl Fake {
    fn new(mode: Mode) -> Self {
        Self(Arc::new(Mutex::new(State {
            data: BTreeMap::from([(vec![0], vec![1, 2])]),
            mode,
            ..State::default()
        })))
    }
}
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
            .map(|(begin, end)| {
                state
                    .data
                    .range(begin.clone()..end.clone())
                    .map(|(key, value)| (key.clone(), value.clone()))
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
    fn approximate_size(&mut self, ctx: &Context) -> Result<i64> {
        ctx.check()?;
        Ok(self
            .changes
            .iter()
            .map(|(key, value)| key.len() + value.as_ref().map_or(0, Vec::len))
            .sum::<usize>() as i64)
    }
    fn commit(&mut self, ctx: &Context) -> Result<()> {
        ctx.check()?;
        let mut state = self.db.0.lock().unwrap();
        state.attempts += 1;
        match state.mode {
            Mode::FailSecond if state.attempts == 2 => {
                return Err(failure(
                    ErrorKind::Operation,
                    "injected second commit failure",
                ));
            }
            Mode::Deadline => {
                drop(state);
                loop {
                    ctx.check()?;
                    thread::sleep(Duration::from_millis(1));
                }
            }
            _ => {}
        }
        state.committed += 1;
        for (key, value) in std::mem::take(&mut self.changes) {
            match value {
                Some(value) => {
                    state.data.insert(key, value);
                }
                None => {
                    state.data.remove(&key);
                }
            }
        }
        if matches!(state.mode, Mode::AwaitCancelAfterFirst) && state.committed == 1 {
            let signal = state.committed_signal.take().expect("one gated commit");
            drop(state);
            signal.send(()).unwrap();
            // Waits on the cancellation predicate itself; no timing assumption.
            while ctx.check().is_ok() {
                thread::yield_now();
            }
            // Cancellation, not the job deadline, released it.
            self.db.0.lock().unwrap().saw_cancel = ctx.deadline.is_some_and(|d| Instant::now() < d);
        }
        Ok(())
    }
    fn on_error(&mut self, _: i32, _: &Context) -> Result<()> {
        panic!("driver fake emits no retryable native errors")
    }
}

fn key_set(v: &Json) -> BTreeSet<String> {
    v.as_object().unwrap().keys().cloned().collect()
}
fn str_set(v: &Json) -> BTreeSet<String> {
    v.as_array()
        .unwrap()
        .iter()
        .map(|s| s.as_str().unwrap().to_owned())
        .collect()
}
fn names(list: &[&str]) -> BTreeSet<String> {
    list.iter().map(|s| (*s).to_owned()).collect()
}

#[test]
fn frozen_schema_copy_pins_the_ingest_corpus_and_workload_field_sets() {
    let bytes = include_bytes!("../testdata/paired-job-v1.schema.json");
    assert_eq!(sha256(bytes), FROZEN_SCHEMA_SHA256, "schema copy drifted");
    let schema: Json = serde_json::from_slice(bytes).unwrap();
    let ingest = schema["oneOf"]
        .as_array()
        .unwrap()
        .iter()
        .find(|b| b["properties"]["mode"]["const"] == "ingest")
        .unwrap();
    assert_eq!(str_set(&ingest["required"]), names(&JOB_FIELDS));
    assert_eq!(key_set(&ingest["properties"]), names(&JOB_FIELDS));
    assert_eq!(ingest["additionalProperties"], false);
    let corpus = &schema["properties"]["corpus_config"];
    assert_eq!(str_set(&corpus["required"]), names(&CORPUS_FIELDS));
    assert_eq!(key_set(&corpus["properties"]), names(&CORPUS_FIELDS));
    for (kind, fields) in [
        ("writer_direct", &DIRECT_FIELDS[..]),
        ("collector_queue", &QUEUE_FIELDS[..]),
    ] {
        let variant = schema["properties"]["workload"]["oneOf"]
            .as_array()
            .unwrap()
            .iter()
            .find(|v| v["properties"]["kind"]["const"] == kind)
            .unwrap();
        assert_eq!(str_set(&variant["required"]), names(fields), "{kind}");
        assert_eq!(key_set(&variant["properties"]), names(fields), "{kind}");
    }
    // The rust_native ingest rule this binary also enforces.
    assert_eq!(
        schema["allOf"][1]["then"]["properties"]["writer_workers"]["const"],
        1
    );
    // The report contract's field lists are the frozen schema's own.
    let defs = &schema["$defs"];
    let report = &defs["ingest_ack_and_samples"];
    assert_eq!(str_set(&report["required"]), names(&REPORT_FIELDS));
    let mut policy = str_set(&defs["counter_policy"]["required"]);
    assert!(policy.remove("arm"));
    assert_eq!(policy, names(&MARKERS));
    for marker in MARKERS {
        assert!(report["properties"].get(marker).is_some(), "{marker}");
    }
    // Plain counts and timings are nonnegative integers in the schema, never nullable.
    for field in PLAIN_COUNTS.iter().chain(TIMINGS.iter()) {
        let property = &report["properties"][field];
        assert_eq!(
            (&property["type"], &property["minimum"]),
            (&json!("integer"), &json!(0)),
            "{field}"
        );
    }
    assert_eq!(
        str_set(&defs["ingest_resources"]["required"]),
        names(&RESOURCE_FIELDS)
    );
    for sample in ["writer_direct_sample", "collector_admission_sample"] {
        assert_eq!(str_set(&defs[sample]["required"]), names(&SAMPLE_FIELDS));
        assert_eq!(defs[sample]["additionalProperties"], false);
    }
    let mut direct_fields = names(&SAMPLE_FIELDS);
    direct_fields.insert("direct_write_acked_operations".into());
    assert_eq!(
        key_set(&defs["writer_direct_sample"]["properties"]),
        direct_fields
    );
    assert_eq!(
        key_set(&defs["collector_admission_sample"]["properties"]),
        names(&SAMPLE_FIELDS)
    );
    // Null resources only on a failed report that says why; measured ones forbid the reason.
    let branches = defs["ingest_resource_policy"]["oneOf"].as_array().unwrap();
    assert_eq!(
        branches[0]["not"]["required"],
        json!(["ingest_resources_unavailable_reason"])
    );
    assert_eq!(branches[1]["properties"]["status"]["const"], "error");
    assert!(str_set(&branches[1]["required"]).contains("ingest_resources_unavailable_reason"));
    let files = Files::new();
    let job = files.job(&frames()[..2], direct(1, 1));
    assert_eq!(key_set(&job), names(&JOB_FIELDS));
    assert_eq!(key_set(&job["corpus_config"]), names(&CORPUS_FIELDS));
    assert_eq!(key_set(&queue(1, 1, 1, 1)), names(&QUEUE_FIELDS));
    assert_eq!(key_set(&direct(1, 1)), names(&DIRECT_FIELDS));
}

#[test]
fn missing_unknown_duplicate_and_trailing_json_fail_closed_before_open() {
    let files = Files::new();
    let source = frames();
    for workload in [direct(2, 2), queue(2, 2, 2, 4)] {
        let valid = files.job(&source[..4], workload);
        let kind = valid["workload"]["kind"].as_str().unwrap().to_owned();
        let kind_fields = if kind == "writer_direct" {
            &DIRECT_FIELDS[..]
        } else {
            &QUEUE_FIELDS[..]
        };
        for field in JOB_FIELDS {
            let mut job = valid.clone();
            job.as_object_mut().unwrap().remove(field);
            files.refuse(
                &serde_json::to_vec(&job).unwrap(),
                &output(&valid),
                &format!("{kind}: missing {field}"),
            );
        }
        for field in CORPUS_FIELDS {
            let mut job = valid.clone();
            job["corpus_config"].as_object_mut().unwrap().remove(field);
            files.refuse_json(&job, &format!("{kind}: missing corpus_config.{field}"));
        }
        for field in kind_fields {
            let mut job = valid.clone();
            job["workload"].as_object_mut().unwrap().remove(*field);
            files.refuse_json(&job, &format!("{kind}: missing workload.{field}"));
        }
        for at in [None, Some("corpus_config"), Some("workload")] {
            let mut job = valid.clone();
            let object = match at {
                None => &mut job,
                Some(name) => &mut job[name],
            };
            object["surprise"] = json!(false);
            files.refuse_json(&job, &format!("{kind}: unknown field in {at:?}"));
        }
        let text = serde_json::to_string(&valid).unwrap();
        for (case, bad) in [
            (
                "duplicate top-level field",
                text.replacen("\"run\":", "\"run\":\"driver_test_run\",\"run\":", 1),
            ),
            (
                "duplicate workload kind",
                text.replacen("\"kind\":", "\"kind\":\"writer_direct\",\"kind\":", 1),
            ),
            (
                "duplicate workload field",
                text.replacen("\"producers\":", "\"producers\":2,\"producers\":", 1),
            ),
            (
                "duplicate corpus_config field",
                text.replacen("\"seed\":", "\"seed\":1,\"seed\":", 1),
            ),
            ("second JSON value", format!("{text} {{}}")),
            ("trailing bytes", format!("{text}x")),
            ("empty file", String::new()),
        ] {
            assert_ne!(bad, text, "{case} must change the text");
            files.refuse(bad.as_bytes(), &output(&valid), &format!("{kind}: {case}"));
        }
        let mut bad_utf8 = text.clone().into_bytes();
        let at = text.find("driver_test_run").unwrap();
        bad_utf8[at + 6] = 0xff;
        files.refuse(&bad_utf8, &output(&valid), "invalid UTF-8 string");
    }
}

#[test]
fn values_bounds_seals_and_cross_field_rules_fail_before_open_or_output() {
    let files = Files::new();
    let source = frames();
    let base = files.job(&source[..4], queue(2, 2, 2, 4));
    let layout = base["layout_file"].as_str().unwrap().to_owned();
    let cases: Vec<(&str, Json)> = vec![
        ("schema", json!("otel_paired_job_v2")),
        ("mode", json!("corpus")),
        ("mode", json!("setup")),
        ("mode", json!("verify")),
        ("mode", json!("Ingest")),
        ("arm", json!("go_pure")),
        ("arm", json!("go_native")),
        ("client_backend", json!("pure_go")),
        ("client_library", Json::Null),
        ("client_library_sha256", Json::Null),
        ("client_library_sha256", json!("0".repeat(64))),
        ("binary_sha256", json!("0".repeat(64))),
        ("binary_sha256", json!(exe_sha256().to_uppercase())),
        ("source_revision", json!("1".repeat(39))),
        ("source_revision", json!("A".repeat(40))),
        ("source_manifest_sha256", json!("2".repeat(63))),
        ("timeout_seconds", json!(0)),
        ("timeout_seconds", json!(3601)),
        ("timeout_seconds", json!("5")),
        ("timeout_seconds", json!(-1)),
        ("timeout_seconds", json!(1.5)),
        ("corpus_config.generator", json!("otel_fixture_v1")),
        ("corpus_config.count", json!(1)),
        ("corpus_config.tenants", json!(2)),
        ("corpus_config.attributes", json!(7)),
        ("corpus_config.match_every", json!(0)),
        ("corpus_config.seed", json!(-1)),
        ("corpus_config.seed", json!(1.5)),
        ("expected_frames", json!(3)),
        ("row_budget", json!(3)),
        ("row_budget", json!(10_000_001)),
        ("run", json!("short")),
        ("run", json!("Upper_case_run")),
        ("run", json!("dash-in-the-run")),
        ("writer_batch_size", json!(0)),
        ("writer_batch_size", json!(65)),
        ("writer_workers", json!(2)),
        ("writer_workers", json!(0)),
        ("record_compression", json!("zlib1")),
        ("record_compression", json!("brotli")),
        ("enable_message_deltas", json!(true)),
        ("enable_message_full_text", json!(true)),
        ("bulk_index_conflicts", json!(true)),
        ("bulk_index_conflicts", json!(0)),
        ("corpus_file", json!("corpus")),
        ("corpus_file", json!("/tmp/../corpus")),
        ("output", json!(format!("{}//receipt", files.dir.display()))),
        ("layout_file", json!(format!("{layout}/"))),
        ("corpus_file", base["output"].clone()),
        ("corpus_sha256", json!("0".repeat(64))),
        ("layout_sha256", json!("0".repeat(64))),
        ("cluster_sha256", json!("0".repeat(64))),
        ("workload.kind", json!("collector")),
        ("workload.producers", json!(0)),
        ("workload.producers", json!(33)),
        ("workload.producer_batch_spans", json!(0)),
        ("workload.producer_batch_spans", json!(65)),
        ("workload.producer_batch_spans", json!(9)),
        ("workload.consumers", json!(0)),
        ("workload.consumers", json!(33)),
        ("workload.queue_capacity_spans", json!(0)),
        ("workload.batch_min_spans", json!(0)),
        ("workload.batch_min_spans", json!(9)),
        ("workload.batch_max_spans", json!(1)),
        ("workload.flush_ms", json!(0)),
        ("workload.flush_ms", json!(3_600_001)),
        ("workload.export_timeout_ms", json!(0)),
        ("workload.retry_initial_ms", json!(0)),
        ("workload.retry_initial_ms", json!(2)),
        ("workload.retry_max_ms", json!(0)),
        ("workload.retry_elapsed_ms", json!(3_600_001)),
        ("workload.block_on_overflow", json!("true")),
    ];
    for (path, value) in cases {
        let mut job = base.clone();
        let mut at = &mut job;
        for part in path.split('.') {
            at = &mut at[part];
        }
        *at = value.clone();
        files.refuse_json(&job, &format!("{path} = {value}"));
    }
    // A direct job carries no queue fields.
    let mut job = files.job(&source[..2], direct(1, 1));
    job["workload"]["consumers"] = json!(1);
    files.refuse_json(&job, "queue field in writer_direct");
}

#[test]
fn writer_direct_job_reports_exact_acks_samples_seals_and_exclusive_output() {
    let files = Files::new();
    let source = frames();
    let mut job = files.job(&source, direct(2, 4));
    job["writer_batch_size"] = json!(4);
    let db = Fake::new(Mode::Success);
    files.run(&job, &db).unwrap();
    let r = checked_receipt(&job);
    assert_eq!(
        (&r["schema"], &r["mode"], &r["arm"], &r["status"]),
        (
            &json!("otel_control_report_v1"),
            &json!("ingest"),
            &json!("rust_native"),
            &json!("ok")
        )
    );
    assert!(r["error"].is_null());
    assert_eq!(r["job_sha256"], sha256(&serde_json::to_vec(&job).unwrap()));
    for seal in [
        "binary_sha256",
        "source_revision",
        "source_manifest_sha256",
        "corpus_sha256",
        "layout_sha256",
        "cluster_sha256",
        "client_library_sha256",
    ] {
        assert_eq!(r[seal], job[seal], "{seal}");
    }
    assert_eq!(r["latency_boundary"], DIRECT_LATENCY);
    // An availability object, never a bare number; ingest reports no census count.
    assert_eq!(
        r["acked_span_operations"],
        json!({"availability": "available", "value": 8})
    );
    assert!(r.get("distinct_spans").is_none());
    assert!(r.get("ingest_resources_unavailable_reason").is_none());
    assert_eq!(
        (
            &r["frames"],
            &r["acked_span_operations"]["value"],
            &r["committed_batches"]["value"],
            &r["native_retries"]["value"],
            &r["outer_retries"]["value"]
        ),
        (&json!(8), &json!(8), &json!(2), &json!(0), &json!(0))
    );
    // No queue: nothing rejected, held or drained, and its counters are not applicable.
    for (field, want) in [
        ("received_spans", 8),
        ("accepted_spans", 8),
        ("rejected_spans", 0),
        ("failed_spans", 0),
        ("queued_spans_at_exit", 0),
        ("drain_ns", 0),
    ] {
        assert_eq!(value(&r, field), &json!(want), "{field}");
    }
    for field in ["export_attempts", "queue_retries"] {
        assert_eq!(r[field]["availability"], "unavailable", "{field}");
        assert!(
            r[field]["reason"]
                .as_str()
                .unwrap()
                .contains("not applicable")
        );
    }
    assert!(r["exported_spans"].is_null() && r["queue"].is_null());
    let samples = r["samples"].as_array().unwrap();
    assert_eq!(samples.len(), 2);
    for s in samples {
        // accepted_spans counts frames submitted to Writer::write; the ACKs are separate.
        assert_eq!(
            (
                &s["received_spans"],
                &s["accepted_spans"],
                &s["direct_write_acked_operations"],
                &s["error"]
            ),
            (&json!(4), &json!(4), &json!(4), &Json::Null)
        );
        assert!(s.get("acked_span_operations").is_none());
    }
    assert_eq!(r["admission_latency"]["population"], 2);
    assert!(r["admission_latency"]["p99_ns"].as_u64().unwrap() > 0);
    // Diagnostics are disabled at the source: no transaction-size distribution.
    assert_eq!(r["transaction_spans"], disabled_diagnostic());
    assert!(r["queue"].is_null());
    assert!(r["admission_phase_ns"].as_u64().unwrap() <= r["ingest_wall_ns"].as_u64().unwrap());
    assert_eq!(r["ingest_resources"]["scope"], "first_read_through_flush");
    // The effective job is the sealed job itself, u64 seed included.
    assert_eq!(r["effective_job"], job);
    assert_eq!(r["effective_job"]["corpus_config"]["seed"], u64::MAX);
    assert_eq!(r["global_write_bound"], 2);
    assert_eq!(
        fs::metadata(output(&job)).unwrap().permissions().mode() & 0o777,
        0o600
    );
    {
        let state = db.0.lock().unwrap();
        assert_eq!((state.transactions, state.committed), (2, 2));
        assert_eq!(state.data, expected_physical(&[0, 1, 2, 3, 4, 5, 6, 7]));
    }
    // An existing report is never reused or truncated.
    let before = fs::read(output(&job)).unwrap();
    assert_eq!(files.run(&job, &db).unwrap_err().kind, ErrorKind::Config);
    assert_eq!(fs::read(output(&job)).unwrap(), before);
    assert_eq!(db.0.lock().unwrap().transactions, 2);
    let mut job = files.job(&source[..2], direct(1, 1));
    job["output"] = json!(files.dir.join("absent").join("receipt"));
    assert_eq!(files.run(&job, &db).unwrap_err().kind, ErrorKind::Config);
    assert!(!output(&job).exists());
}

/// The settled queue's diagnostics are each the disabled marker, never observations.
fn assert_queue_diagnostics_disabled(r: &Json) {
    let queue = r["queue"].as_object().unwrap();
    assert_eq!(queue.len(), QUEUE_DIAGNOSTIC_FIELDS.len());
    for field in QUEUE_DIAGNOSTIC_FIELDS {
        assert_eq!(queue[field], disabled_diagnostic(), "{field}");
    }
    assert_eq!(r["transaction_spans"], disabled_diagnostic());
}

#[test]
fn collector_queue_job_runs_the_real_writer_and_reports_queue_populations() {
    let files = Files::new();
    let source = frames();
    let mut job = files.job(&source, queue(2, 2, 2, 4));
    job["writer_batch_size"] = json!(4);
    let db = Fake::new(Mode::Success);
    files.run(&job, &db).unwrap();
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "ok", "{r}");
    assert_eq!(r["latency_boundary"], QUEUE_LATENCY);
    for (field, want) in [
        ("frames", 8),
        ("accepted_spans", 8),
        ("rejected_spans", 0),
        ("exported_spans", 8),
        ("failed_spans", 0),
        ("queued_spans_at_exit", 0),
        ("queue_retries", 0),
        ("native_retries", 0),
        ("outer_retries", 0),
    ] {
        assert_eq!(value(&r, field), &json!(want), "{field}");
    }
    assert_eq!(
        r["acked_span_operations"],
        json!({"availability": "available", "value": 8})
    );
    // Writer batches of 4 cover every export of at most 4 spans in one transaction.
    let attempts = r["export_attempts"]["value"].as_u64().unwrap();
    assert_eq!(r["committed_batches"]["value"], attempts);
    let samples = r["samples"].as_array().unwrap();
    assert_eq!(samples.len(), 8);
    for s in samples {
        assert_eq!(
            (&s["received_spans"], &s["accepted_spans"], &s["error"]),
            (&json!(1), &json!(1), &Json::Null)
        );
        // Admission is not an ACK: collector samples carry neither ACK field.
        assert!(s.get("acked_span_operations").is_none());
        assert!(s.get("direct_write_acked_operations").is_none());
    }
    assert_queue_diagnostics_disabled(&r);
    assert!(r["drain_ns"].as_u64().is_some());
    assert_eq!(r["global_write_bound"], 2);
    assert_eq!(
        db.0.lock().unwrap().data,
        expected_physical(&[0, 1, 2, 3, 4, 5, 6, 7])
    );
}

#[test]
fn stage0_point2_workload_text_runs_verbatim_through_the_collector() {
    let files = Files::new();
    let source = frames();
    let workload: Json = serde_json::from_str(POINT2).unwrap();
    let mut job = files.job(&source, workload.clone());
    job["writer_batch_size"] = json!(64);
    job["record_compression"] = json!("none");
    let text = serde_json::to_string(&job)
        .unwrap()
        .replace(&serde_json::to_string(&workload).unwrap(), POINT2);
    assert!(text.contains(POINT2));
    let db = Fake::new(Mode::Success);
    run_spawning(
        &files.write(text.as_bytes()),
        &files.pin,
        |_, _| Ok(db.clone()),
        |_| Ok(()),
    )
    .unwrap();
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "ok", "{r}");
    assert_eq!(r["effective_job"]["workload"], workload);
    assert_eq!(r["global_write_bound"], 8);
    assert_queue_diagnostics_disabled(&r);
    for (field, want) in [
        ("accepted_spans", 8),
        ("exported_spans", 8),
        ("failed_spans", 0),
    ] {
        assert_eq!(value(&r, field), &json!(want), "{field}");
    }
    assert_eq!(
        r["acked_span_operations"],
        json!({"availability": "available", "value": 8})
    );
    assert_eq!(r["samples"].as_array().unwrap().len(), 8);
    assert_eq!(
        db.0.lock().unwrap().data,
        expected_physical(&[0, 1, 2, 3, 4, 5, 6, 7])
    );
}

#[test]
fn failures_after_reservation_write_synced_error_receipts_with_exact_progress() {
    let files = Files::new();
    let source = frames();
    // The opener fails: an empty-progress error receipt, not an empty file.
    let job = files.job(&source[..2], direct(1, 1));
    let err = run_spawning(
        &files.write(&serde_json::to_vec(&job).unwrap()),
        &files.pin,
        |_: &Path, _: &Path| -> Result<Fake> {
            Err(Error::new(ErrorKind::Operation, "injected open failure"))
        },
        |_| Ok(()),
    )
    .unwrap_err();
    assert_eq!(err.to_string(), "Operation: injected open failure");
    let r = checked_receipt(&job);
    assert_eq!(
        (&r["status"], &r["error"]),
        (&json!("error"), &json!("Operation: injected open failure"))
    );
    assert_eq!(
        (&r["frames"], &r["acked_span_operations"]["value"]),
        (&json!(0), &json!(0))
    );
    assert!(r["samples"].as_array().unwrap().is_empty());
    // Ingest never started: null resources with a reason, and exact zero timings.
    assert!(r["ingest_resources"].is_null());
    for field in TIMINGS {
        assert_eq!(r[field], 0, "{field}");
    }
    assert_eq!(
        r["ingest_resources_unavailable_reason"],
        "ingest did not start, so there is no ingest-window measurement"
    );
    assert_eq!(r["admission_latency"]["population"], 0);
    assert!(r["admission_latency"]["p50_ns"].is_null());

    // writer_direct: the second commit fails; the first stays acknowledged.
    let job = files.job(&source[..3], direct(1, 1));
    let db = Fake::new(Mode::FailSecond);
    files.run(&job, &db).unwrap_err();
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "error");
    assert!(
        r["error"]
            .as_str()
            .unwrap()
            .contains("injected second commit")
    );
    assert_eq!(
        (
            &r["frames"],
            &r["acked_span_operations"]["value"],
            &r["committed_batches"]["value"]
        ),
        (&json!(2), &json!(1), &json!(1))
    );
    let samples = r["samples"].as_array().unwrap();
    assert_eq!(samples.len(), 2);
    assert_eq!(
        (
            &samples[0]["direct_write_acked_operations"],
            &samples[1]["direct_write_acked_operations"]
        ),
        (&json!(1), &json!(0))
    );
    assert!(samples[1]["error"].is_string());
    assert_eq!(db.0.lock().unwrap().data, expected_physical(&[0]));

    // collector_queue: the second single-span export fails permanently; the third commits.
    let mut job = files.job(&source[..3], queue(1, 1, 1, 1));
    job["workload"]["retry_enabled"] = json!(false);
    job["workload"]["retry_initial_ms"] = json!(0);
    let db = Fake::new(Mode::FailSecond);
    assert_eq!(files.run(&job, &db).unwrap_err().kind, ErrorKind::Operation);
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "error");
    for (field, want) in [
        ("frames", 3),
        ("accepted_spans", 3),
        ("exported_spans", 2),
        ("failed_spans", 1),
        ("queued_spans_at_exit", 0),
        ("rejected_spans", 0),
        ("committed_batches", 2),
        ("export_attempts", 3),
        ("queue_retries", 0),
    ] {
        assert_eq!(value(&r, field), &json!(want), "{field}");
    }
    assert_eq!(
        r["acked_span_operations"],
        json!({"availability": "available", "value": 2})
    );
    assert_queue_diagnostics_disabled(&r);
    assert_eq!(db.0.lock().unwrap().data, expected_physical(&[0, 2]));
}

#[test]
fn job_deadline_cancels_the_commit_without_ack_or_followup() {
    let files = Files::new();
    let mut job = files.job(&frames()[..2], direct(1, 1));
    job["timeout_seconds"] = json!(1);
    let db = Fake::new(Mode::Deadline);
    files.run(&job, &db).unwrap_err();
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "error");
    assert!(r["error"].as_str().unwrap().contains("Canceled"));
    assert_eq!(
        (
            &r["frames"],
            &r["acked_span_operations"]["value"],
            &r["committed_batches"]["value"]
        ),
        (&json!(1), &json!(0), &json!(0))
    );
    assert_eq!(r["samples"].as_array().unwrap().len(), 1);
    let state = db.0.lock().unwrap();
    assert_eq!(state.attempts, 1);
    assert_eq!(state.data, BTreeMap::from([(vec![0], vec![1, 2])]));
}

#[test]
fn source_population_must_reconcile_with_expected_frames_and_row_budget() {
    let files = Files::new();
    let source = frames();
    // (corpus frames, expected/count, row budget, acked, frames read, error text)
    for (corpus, expected, budget, acked, read, text) in [
        (2, 3, 10, 2, 2, "did not reconcile"),
        (3, 2, 10, 3, 3, "did not reconcile"),
        (3, 2, 2, 2, 2, "row budget exceeded"),
    ] {
        let mut job = files.job(&source[..corpus], direct(1, 1));
        job["expected_frames"] = json!(expected);
        job["corpus_config"]["count"] = json!(expected);
        job["row_budget"] = json!(budget);
        let db = Fake::new(Mode::Success);
        files.run(&job, &db).unwrap_err();
        let r = checked_receipt(&job);
        assert_eq!(r["status"], "error");
        assert!(r["error"].as_str().unwrap().contains(text), "{r}");
        assert_eq!(
            (&r["frames"], &r["acked_span_operations"]["value"]),
            (&json!(read), &json!(acked))
        );
        let indices: Vec<usize> = (0..acked).collect();
        assert_eq!(db.0.lock().unwrap().data, expected_physical(&indices));
    }
}

#[test]
fn malformed_and_many_span_frames_never_reach_transaction_commit() {
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let desc = pool
        .get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
        .unwrap();
    let source = frames();
    let mut message = DynamicMessage::decode(desc, source[0].as_slice()).unwrap();
    let Value::List(scopes) = message.get_field_by_name_mut("scope_spans").unwrap() else {
        panic!()
    };
    let Value::Message(scope) = &mut scopes[0] else {
        panic!()
    };
    let Value::List(spans) = scope.get_field_by_name_mut("spans").unwrap() else {
        panic!()
    };
    spans.push(spans[0].clone());
    let files = Files::new();
    for (name, bad) in [
        ("malformed", vec![0xff]),
        ("two spans", message.encode_to_vec()),
    ] {
        for workload in [direct(1, 2), queue(1, 1, 1, 1)] {
            // A valid frame first: the whole precheck batch is refused before any call.
            let mut job = files.job(&[source[1].clone(), bad.clone()], workload);
            job["workload"]["producer_batch_spans"] = json!(2);
            let db = Fake::new(Mode::Success);
            files.run(&job, &db).unwrap_err();
            let r = checked_receipt(&job);
            assert_eq!(r["status"], "error", "{name}");
            assert!(
                r["error"].as_str().unwrap().contains("exactly one span"),
                "{name}"
            );
            assert_eq!(r["frames"], 2, "{name}");
            assert_eq!(r["acked_span_operations"]["value"], 0, "{name}");
            assert!(r["samples"].as_array().unwrap().is_empty(), "{name}");
            assert!(unchanged(&db), "{name}");
        }
    }
}

/// One ResourceSpans frame with the given scopes of (trace, span, start) spans.
fn frame(scopes: &[Vec<(Vec<u8>, Vec<u8>, u64)>]) -> Vec<u8> {
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let message = |name: &str| DynamicMessage::new(pool.get_message_by_name(name).unwrap());
    let mut rs = message("opentelemetry.proto.trace.v1.ResourceSpans");
    let scopes = scopes.iter().map(|spans| {
        let mut ss = message("opentelemetry.proto.trace.v1.ScopeSpans");
        let spans = spans.iter().map(|(trace, span, start)| {
            let mut sp = message("opentelemetry.proto.trace.v1.Span");
            sp.set_field_by_name("trace_id", Value::Bytes(trace.clone().into()));
            sp.set_field_by_name("span_id", Value::Bytes(span.clone().into()));
            sp.set_field_by_name("start_time_unix_nano", Value::U64(*start));
            Value::Message(sp)
        });
        ss.set_field_by_name("spans", Value::List(spans.collect()));
        Value::Message(ss)
    });
    rs.set_field_by_name("scope_spans", Value::List(scopes.collect()));
    rs.encode_to_vec()
}

#[test]
fn frame_precheck_applies_prepare_identity_start_and_one_span_rules() {
    let (trace, span) = (vec![0x11; 16], vec![0x22; 8]);
    let mut low_trace = vec![0; 16];
    low_trace[15] = 1;
    let mut low_span = vec![0; 8];
    low_span[7] = 1;
    let ok = (trace.clone(), vec![0x33; 8], 1);
    let files = Files::new();
    for (name, spec) in [
        ("start zero", (trace.clone(), span.clone(), 0)),
        (
            "start int64 max",
            (trace.clone(), span.clone(), i64::MAX as u64),
        ),
        ("one nonzero byte per id", (low_trace, low_span, 1)),
    ] {
        let job = files.job(
            &[frame(&[vec![spec]]), frame(&[vec![ok.clone()]])],
            direct(1, 1),
        );
        let db = Fake::new(Mode::Success);
        files.run(&job, &db).unwrap();
        assert_eq!(
            checked_receipt(&job)["acked_span_operations"]["value"],
            2,
            "{name}"
        );
        assert_eq!(db.0.lock().unwrap().committed, 2, "{name}");
    }
    let rejected = [
        ("empty span id", frame(&[vec![(trace.clone(), vec![], 1)]])),
        (
            "short span id",
            frame(&[vec![(trace.clone(), vec![0x22; 7], 1)]]),
        ),
        (
            "long span id",
            frame(&[vec![(trace.clone(), vec![0x22; 9], 1)]]),
        ),
        (
            "zero span id",
            frame(&[vec![(trace.clone(), vec![0; 8], 1)]]),
        ),
        ("empty trace id", frame(&[vec![(vec![], span.clone(), 1)]])),
        (
            "short trace id",
            frame(&[vec![(vec![0x11; 15], span.clone(), 1)]]),
        ),
        (
            "long trace id",
            frame(&[vec![(vec![0x11; 17], span.clone(), 1)]]),
        ),
        (
            "zero trace id",
            frame(&[vec![(vec![0; 16], span.clone(), 1)]]),
        ),
        (
            "start past int64",
            frame(&[vec![(trace.clone(), span.clone(), i64::MAX as u64 + 1)]]),
        ),
        (
            "start at u64 max",
            frame(&[vec![(trace.clone(), span.clone(), u64::MAX)]]),
        ),
        ("no spans", frame(&[vec![]])),
        (
            "two spans in one scope",
            frame(&[vec![ok.clone(), (trace.clone(), span.clone(), 1)]]),
        ),
        (
            "two scopes",
            frame(&[vec![ok.clone()], vec![(trace.clone(), span.clone(), 1)]]),
        ),
    ];
    for (name, bad) in rejected {
        // A valid frame then the bad one in the same batch: nothing of either may commit.
        let job = files.job(&[frame(&[vec![ok.clone()]]), bad], direct(1, 2));
        let db = Fake::new(Mode::Success);
        files.run(&job, &db).unwrap_err();
        let r = checked_receipt(&job);
        assert_eq!(r["acked_span_operations"]["value"], 0, "{name}");
        assert!(r["samples"].as_array().unwrap().is_empty(), "{name}");
        assert!(
            r["error"].as_str().unwrap().contains("exactly one span"),
            "{name}"
        );
        assert!(unchanged(&db), "{name}");
    }
}

#[test]
fn first_producer_spawn_failure_writes_empty_synced_receipt_without_transactions() {
    let files = Files::new();
    for workload in [direct(2, 1), queue(2, 1, 1, 1)] {
        let job = files.job(&frames()[..2], workload);
        let db = Fake::new(Mode::Success);
        let mut calls = Vec::new();
        let err = files
            .run_hooked(&job, &db, |i| {
                calls.push(i);
                Err(std::io::Error::other("injected spawn failure"))
            })
            .unwrap_err();
        assert_eq!(err.to_string(), SPAWN_ERROR);
        assert_eq!(calls, [0], "no producer launch after the failed spawn");
        let r = checked_receipt(&job);
        assert_eq!(
            (&r["status"], &r["error"]),
            (&json!("error"), &json!(SPAWN_ERROR))
        );
        assert_eq!(
            (
                &r["frames"],
                &r["acked_span_operations"]["value"],
                &r["committed_batches"]["value"]
            ),
            (&json!(0), &json!(0), &json!(0))
        );
        assert!(r["samples"].as_array().unwrap().is_empty());
        assert_eq!(
            fs::metadata(output(&job)).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert!(unchanged(&db));
    }
}

#[test]
fn later_producer_spawn_failure_cancels_joins_and_keeps_exact_acknowledged_progress() {
    let files = Files::new();
    let job = files.job(&frames()[..2], direct(3, 1));
    let db = Fake::new(Mode::AwaitCancelAfterFirst);
    let (signal, committed) = mpsc::channel();
    db.0.lock().unwrap().committed_signal = Some(signal);
    let mut calls = Vec::new();
    let err = files
        .run_hooked(&job, &db, |i| {
            calls.push(i);
            if i == 0 {
                return Ok(());
            }
            // Fail only after producer 0's first commit has been applied.
            // Watchdog only, not a timing verdict: a missing commit fails instead of hanging.
            committed
                .recv_timeout(Duration::from_secs(5))
                .expect("producer 0 committed before the later spawn");
            Err(std::io::Error::other("injected spawn failure"))
        })
        .unwrap_err();
    assert_eq!(err.to_string(), SPAWN_ERROR);
    assert_eq!(calls, [0, 1], "no producer launch after the failed spawn");
    let r = checked_receipt(&job);
    assert_eq!(
        (
            &r["frames"],
            &r["acked_span_operations"]["value"],
            &r["committed_batches"]["value"]
        ),
        (&json!(1), &json!(1), &json!(1))
    );
    let samples = r["samples"].as_array().unwrap();
    assert_eq!(samples.len(), 1);
    assert_eq!(
        (
            &samples[0]["received_spans"],
            &samples[0]["direct_write_acked_operations"],
            &samples[0]["error"]
        ),
        (&json!(1), &json!(1), &Json::Null)
    );
    assert_eq!(r["error"], SPAWN_ERROR, "never the reconciliation error");
    // run_spawning returned, so producer 0 was joined; nothing ran after its canceled loop check.
    let state = db.0.lock().unwrap();
    assert!(
        state.saw_cancel,
        "producer 0 observed the spawn-failure cancellation"
    );
    assert_eq!(
        (state.transactions, state.attempts, state.committed),
        (1, 1, 1)
    );
    assert_eq!(state.data, expected_physical(&[0]));
}

#[test]
fn multiple_producers_spawn_and_commit_the_exact_population() {
    let files = Files::new();
    let job = files.job(&frames()[..6], direct(3, 1));
    let db = Fake::new(Mode::Success);
    let mut calls = Vec::new();
    files
        .run_hooked(&job, &db, |i| {
            calls.push(i);
            Ok(())
        })
        .unwrap();
    assert_eq!(calls, [0, 1, 2]);
    let r = checked_receipt(&job);
    assert_eq!(
        (
            &r["frames"],
            &r["acked_span_operations"]["value"],
            &r["committed_batches"]["value"]
        ),
        (&json!(6), &json!(6), &json!(6))
    );
    let samples = r["samples"].as_array().unwrap();
    assert_eq!(samples.len(), 6);
    assert!(samples.iter().all(|s| s["received_spans"] == 1
        && s["direct_write_acked_operations"] == 1
        && s["error"].is_null()));
    let state = db.0.lock().unwrap();
    assert_eq!((state.transactions, state.committed), (6, 6));
    assert_eq!(state.data, expected_physical(&[0, 1, 2, 3, 4, 5]));
}

#[test]
fn collector_deadline_settles_every_admitted_request_exactly_once() {
    let files = Files::new();
    let mut job = files.job(&frames()[..3], queue(1, 1, 1, 1));
    job["timeout_seconds"] = json!(1);
    // The real Collector -> Writer<Fake>: the first commit waits for cancellation.
    let db = Fake::new(Mode::Deadline);
    let err = files.run(&job, &db).unwrap_err();
    assert_eq!(err.kind, ErrorKind::Canceled);
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "error");
    assert!(r["error"].as_str().unwrap().contains("Canceled"));
    for (field, want) in [
        ("frames", 3),
        ("accepted_spans", 3),
        ("exported_spans", 0),
        ("failed_spans", 3),
        ("queued_spans_at_exit", 0),
        ("rejected_spans", 0),
        ("committed_batches", 0),
        ("export_attempts", 1),
    ] {
        assert_eq!(value(&r, field), &json!(want), "{field}");
    }
    assert_eq!(
        r["acked_span_operations"],
        json!({"availability": "available", "value": 0})
    );
    // Settled after the final join: no request is left queued, and no optional
    // completion history is reported.
    assert_queue_diagnostics_disabled(&r);
    let settled = {
        let state = db.0.lock().unwrap();
        assert_eq!(
            (state.transactions, state.attempts, state.committed),
            (1, 1, 0)
        );
        assert_eq!(state.data, BTreeMap::from([(vec![0], vec![1, 2])]));
        (state.transactions, state.attempts, state.data.clone())
    };
    // Every consumer was joined before run returned: nothing can commit afterwards.
    for _ in 0..100 {
        thread::yield_now();
    }
    let state = db.0.lock().unwrap();
    assert_eq!(
        (state.transactions, state.attempts, state.data.clone()),
        settled
    );
}

#[test]
fn producer_failure_stops_publishing_but_admitted_requests_still_drain() {
    let files = Files::new();
    let source = frames();
    let mut job = files.job(&source[..3], queue(1, 1, 1, 1));
    // Three valid frames, then a truncated fourth: the producer fails after three admissions.
    let mut corpus = framed(&source[..3]);
    corpus.extend_from_slice(&[0, 0, 0, 9, 1]);
    fs::write(job["corpus_file"].as_str().unwrap(), &corpus).unwrap();
    job["corpus_sha256"] = json!(sha256(&corpus));
    let db = Fake::new(Mode::Success);
    let err = files.run(&job, &db).unwrap_err();
    assert_eq!(err.kind, ErrorKind::Admission);
    let r = checked_receipt(&job);
    assert_eq!(r["status"], "error");
    assert!(
        r["error"]
            .as_str()
            .unwrap()
            .contains("truncated corpus frame")
    );
    for (field, want) in [
        ("frames", 3),
        ("accepted_spans", 3),
        ("exported_spans", 3),
        ("failed_spans", 0),
        ("queued_spans_at_exit", 0),
    ] {
        assert_eq!(value(&r, field), &json!(want), "{field}");
    }
    assert_eq!(
        r["acked_span_operations"],
        json!({"availability": "available", "value": 3})
    );
    assert_eq!(r["samples"].as_array().unwrap().len(), 3);
    assert_queue_diagnostics_disabled(&r);
    assert_eq!(db.0.lock().unwrap().data, expected_physical(&[0, 1, 2]));
}

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
#[test]
fn rusage_binding_matches_the_linux_x86_64_struct_layout() {
    use std::mem::{align_of, offset_of, size_of};
    assert_eq!((size_of::<RUsage>(), align_of::<RUsage>()), (144, 8));
    assert_eq!(
        [
            offset_of!(RUsage, user_s),
            offset_of!(RUsage, user_us),
            offset_of!(RUsage, system_s),
            offset_of!(RUsage, system_us),
            offset_of!(RUsage, max_rss_kb),
            offset_of!(RUsage, _rest),
        ],
        [0, 8, 16, 24, 32, 40]
    );
    assert!(RUSAGE_BOUND);
    assert!(rusage().expect("getrusage(RUSAGE_SELF)").max_rss_kb > 0);
}

#[test]
fn resources_report_exact_cpu_deltas_and_the_rss_high_water_scope() {
    let start = RUsage {
        user_s: 1,
        user_us: 250_000,
        system_s: 0,
        system_us: 500_000,
        max_rss_kb: 7,
        _rest: [0; 13],
    };
    let end = RUsage {
        user_s: 3,
        user_us: 0,
        system_s: 2,
        system_us: 0,
        max_rss_kb: 2048,
        _rest: [0; 13],
    };
    let r = resources(&start, &end).expect("a valid measurement");
    assert_eq!(
        (r.user_cpu_seconds, r.system_cpu_seconds, r.cpu_seconds),
        (1.75, 1.5, 3.25)
    );
    assert_eq!(r.peak_rss_bytes, 2048 * 1024);
    assert_eq!(r.scope, "first_read_through_flush");
    assert!(r.measurement_source.contains("whole-process high-water"));
    // No fallback: a missing RSS or CPU that went backwards is not a measurement.
    for (case, bad) in [
        (
            "no RSS",
            RUsage {
                max_rss_kb: 0,
                ..end
            },
        ),
        (
            "negative RSS",
            RUsage {
                max_rss_kb: -1,
                ..end
            },
        ),
        ("user CPU backwards", RUsage { user_s: 0, ..end }),
        (
            "system CPU backwards",
            RUsage {
                system_us: 0,
                system_s: 0,
                ..end
            },
        ),
    ] {
        assert!(resources(&start, &bad).is_none(), "{case}");
    }
}

/// One report mutation, typed so each case's closure is a plain function pointer.
fn mutation(f: fn(&mut Json)) -> fn(&mut Json) {
    f
}

#[test]
fn report_contract_rejects_marker_count_resource_sample_and_policy_violations() {
    let files = Files::new();
    let source = frames();
    let direct_job = files.job(&source[..2], direct(1, 1));
    files.run(&direct_job, &Fake::new(Mode::Success)).unwrap();
    let queue_job = files.job(&source[..2], queue(1, 1, 1, 1));
    files.run(&queue_job, &Fake::new(Mode::Success)).unwrap();
    // An early failure: the native client never opened, so no ingest window exists.
    let early_job = files.job(&source[..2], direct(1, 1));
    run_spawning(
        &files.write(&serde_json::to_vec(&early_job).unwrap()),
        &files.pin,
        |_: &Path, _: &Path| -> Result<Fake> {
            Err(Error::new(
                ErrorKind::Operation,
                "native client open failed",
            ))
        },
        |_| Ok(()),
    )
    .unwrap_err();
    let jobs = [&direct_job, &queue_job, &early_job];
    // checked_receipt asserts each real report satisfies the contract.
    let reports = jobs.map(checked_receipt);
    assert!(reports[0]["ingest_resources"].is_object());
    assert!(
        reports[0]
            .get("ingest_resources_unavailable_reason")
            .is_none()
    );
    assert!(reports[2]["ingest_resources"].is_null());
    for field in REPORT_FIELDS {
        let mut r = reports[0].clone();
        r.as_object_mut().unwrap().remove(field);
        assert!(report_contract(&r, jobs[0]).is_err(), "missing {field}");
    }
    // The disabled-diagnostics policy is explicit on every report.
    assert_eq!(reports[0]["transaction_spans"], disabled_diagnostic());
    assert!(reports[0]["queue"].is_null() && reports[2]["queue"].is_null());
    assert_queue_diagnostics_disabled(&reports[1]);
    let cases: Vec<(&str, usize, fn(&mut Json))> = vec![
        (
            "measured empty transaction map",
            0,
            mutation(|r| r["transaction_spans"] = json!({})),
        ),
        (
            "transaction-size distribution",
            1,
            mutation(|r| r["transaction_spans"] = json!({"1": 2})),
        ),
        (
            "available transaction diagnostic",
            0,
            mutation(|r| r["transaction_spans"] = json!({"availability": "available", "value": 2})),
        ),
        (
            "blank disabled reason",
            0,
            mutation(|r| r["transaction_spans"]["reason"] = json!(" ")),
        ),
        (
            "contradictory disabled reason",
            1,
            mutation(|r| r["queue"]["blocked_ns"]["reason"] = json!("not measured")),
        ),
        (
            "early receipt with a measured map",
            2,
            mutation(|r| r["transaction_spans"] = json!({})),
        ),
        (
            "measured empty completion array",
            1,
            mutation(|r| r["queue"]["completion_samples"] = json!([])),
        ),
        (
            "measured zero max outstanding",
            1,
            mutation(|r| r["queue"]["max_outstanding_spans"] = json!(0)),
        ),
        (
            "measured zero omitted completions",
            1,
            mutation(|r| r["queue"]["completion_samples_omitted"] = json!(0)),
        ),
        (
            "available export sizes",
            1,
            mutation(|r| {
                r["queue"]["export_call_spans"] = json!({"availability": "available", "value": 1})
            }),
        ),
        (
            "missing queue diagnostic",
            1,
            mutation(|r| {
                r["queue"]
                    .as_object_mut()
                    .unwrap()
                    .remove("blocked_requests");
            }),
        ),
        (
            "extra queue diagnostic",
            1,
            mutation(|r| r["queue"]["completion_timeline"] = disabled_diagnostic()),
        ),
        (
            "settled collector without its queue",
            1,
            mutation(|r| r["queue"] = Json::Null),
        ),
        (
            "writer_direct with a queue",
            0,
            mutation(|r| {
                r["queue"] = json!({});
                for field in QUEUE_DIAGNOSTIC_FIELDS {
                    r["queue"][field] = disabled_diagnostic();
                }
            }),
        ),
        (
            "bare marker",
            0,
            mutation(|r| r["committed_batches"] = json!(1)),
        ),
        (
            "null marker value",
            0,
            mutation(|r| r["native_retries"]["value"] = Json::Null),
        ),
        (
            "value and reason",
            0,
            mutation(|r| r["outer_retries"]["reason"] = json!("both")),
        ),
        (
            "blank reason",
            0,
            mutation(|r| r["export_attempts"]["reason"] = json!("  ")),
        ),
        (
            "unavailable without reason",
            0,
            mutation(|r| {
                r["export_attempts"]
                    .as_object_mut()
                    .unwrap()
                    .remove("reason");
            }),
        ),
        (
            "ACK unavailable",
            0,
            mutation(|r| {
                r["acked_span_operations"] = json!({"availability": "unavailable", "reason": "no"})
            }),
        ),
        (
            "unknown availability",
            0,
            mutation(|r| r["queue_retries"]["availability"] = json!("maybe")),
        ),
        ("count as string", 0, mutation(|r| r["frames"] = json!("2"))),
        (
            "negative count",
            0,
            mutation(|r| r["failed_spans"] = json!(-1)),
        ),
        (
            "fractional count",
            0,
            mutation(|r| r["accepted_spans"] = json!(2.5)),
        ),
        (
            "measured without timing",
            0,
            mutation(|r| r["drain_ns"] = Json::Null),
        ),
        (
            "zero RSS",
            0,
            mutation(|r| r["ingest_resources"]["peak_rss_bytes"] = json!(0)),
        ),
        (
            "CPU sum",
            0,
            mutation(|r| r["ingest_resources"]["cpu_seconds"] = json!(1e9)),
        ),
        (
            "negative CPU",
            0,
            mutation(|r| r["ingest_resources"]["user_cpu_seconds"] = json!(-1.0)),
        ),
        (
            "resource scope",
            0,
            mutation(|r| r["ingest_resources"]["scope"] = json!("argv_to_exit")),
        ),
        (
            "extra resource key",
            0,
            mutation(|r| r["ingest_resources"]["peak_rss_scope"] = json!("x")),
        ),
        (
            "reason with measured resources",
            0,
            mutation(|r| r["ingest_resources_unavailable_reason"] = json!("why")),
        ),
        (
            "null reason with measured resources",
            0,
            mutation(|r| r["ingest_resources_unavailable_reason"] = Json::Null),
        ),
        (
            "success without resources",
            0,
            mutation(|r| {
                r["ingest_resources"] = Json::Null;
                r["ingest_resources_unavailable_reason"] = json!("why");
            }),
        ),
        (
            "ACK name in a sample",
            0,
            mutation(|r| r["samples"][0]["acked_span_operations"] = json!(1)),
        ),
        (
            "null direct sample ACK",
            0,
            mutation(|r| r["samples"][0]["direct_write_acked_operations"] = Json::Null),
        ),
        (
            "distinct spans",
            0,
            mutation(|r| r["distinct_spans"] = json!(2)),
        ),
        (
            "null distinct spans",
            0,
            mutation(|r| r["distinct_spans"] = Json::Null),
        ),
        ("rate", 0, mutation(|r| r["spans_per_second"] = json!(1.0))),
        (
            "other effective job",
            0,
            mutation(|r| r["effective_job"]["run"] = json!("other_run_name")),
        ),
        (
            "received population",
            0,
            mutation(|r| r["received_spans"] = json!(99)),
        ),
        (
            "direct rejection",
            0,
            mutation(|r| r["rejected_spans"] = json!(1)),
        ),
        (
            "direct export attempts",
            0,
            mutation(|r| r["export_attempts"] = json!({"availability": "available", "value": 0})),
        ),
        (
            "success with an error",
            0,
            mutation(|r| r["error"] = json!("x")),
        ),
        (
            "failure without an error",
            0,
            mutation(|r| r["status"] = json!("error")),
        ),
        (
            "success without samples",
            0,
            mutation(|r| r["samples"] = json!([])),
        ),
        (
            "ACK in a collector sample",
            1,
            mutation(|r| r["samples"][0]["direct_write_acked_operations"] = json!(1)),
        ),
        (
            "collector export attempts unavailable",
            1,
            mutation(|r| {
                r["export_attempts"] = json!({"availability": "unavailable", "reason": "no"})
            }),
        ),
        (
            "early blank reason",
            2,
            mutation(|r| r["ingest_resources_unavailable_reason"] = json!(" ")),
        ),
        (
            "early without reason",
            2,
            mutation(|r| {
                r.as_object_mut()
                    .unwrap()
                    .remove("ingest_resources_unavailable_reason");
            }),
        ),
        (
            "early success",
            2,
            mutation(|r| {
                r["status"] = json!("ok");
                r["error"] = Json::Null;
            }),
        ),
        ("early null error", 2, mutation(|r| r["error"] = Json::Null)),
    ];
    for (case, base, mutate) in cases {
        let mut r = reports[base].clone();
        mutate(&mut r);
        assert!(
            report_contract(&r, jobs[base]).is_err(),
            "{case} was accepted"
        );
    }
    // Timings are integers on measured and early reports alike, never null.
    for (field, base) in TIMINGS.iter().flat_map(|f| [(*f, 0), (*f, 2)]) {
        let mut r = reports[base].clone();
        r[field] = Json::Null;
        assert!(
            report_contract(&r, jobs[base]).is_err(),
            "null {field} on report {base} was accepted"
        );
    }
}
