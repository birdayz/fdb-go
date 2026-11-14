//! Source-only driver tests. The injected opener never loads a native client.
use super::*;
use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::writer::{ExpectedValue, KeyValues, Transaction};
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage, Value};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::PermissionsExt,
    sync::atomic::{AtomicUsize, Ordering},
};

static TEMP_ID: AtomicUsize = AtomicUsize::new(0);
struct Files(PathBuf);
impl Files {
    fn new() -> Self {
        let path = std::env::temp_dir().join(format!(
            "otel-rust-driver-{}-{}",
            std::process::id(),
            TEMP_ID.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        Self(path)
    }
    fn job(&self, frames: &[Vec<u8>]) -> Job {
        let corpus = self.0.join("corpus");
        fs::write(&corpus, framed(frames)).unwrap();
        let layout = Layout {
            version: 1,
            descriptor_sha256: hex::encode(Sha256::digest(include_bytes!(
                "../testdata/descriptors.pb"
            ))),
            record_prefix: "01".into(),
            index_prefixes: fdb_otel_rust::row::INDEXES
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
        let manifest = self.0.join("layout.json");
        fs::write(&manifest, serde_json::to_vec(&layout).unwrap()).unwrap();
        Job {
            client_library: self.0.join("never-loaded-client"),
            cluster_file: self.0.join("never-opened-cluster"),
            layout_file: manifest,
            corpus_file: corpus.clone(),
            corpus_sha256: hash_file(&corpus).unwrap(),
            binary_sha256: hash_file(&std::env::current_exe().unwrap()).unwrap(),
            source_revision: "1".repeat(40),
            expected_frames: frames.len().max(1) as u64,
            row_budget: 10,
            batch_size: 1,
            workers: 1,
            record_compression: RecordCompression::None,
            timeout_seconds: 5,
            output: self.0.join("receipt.json"),
        }
    }
}
impl Drop for Files {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap();
    }
}
fn frames() -> Vec<Vec<u8>> {
    let vectors: serde_json::Value =
        serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    vectors
        .as_array()
        .unwrap()
        .iter()
        .filter(|v| v["name"].as_str().unwrap().starts_with("v2_"))
        .take(2)
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

#[derive(Clone, Copy, Default)]
enum Mode {
    #[default]
    Success,
    FailSecond,
    Deadline,
}
#[derive(Default)]
struct State {
    data: BTreeMap<Vec<u8>, Vec<u8>>,
    attempts: usize,
    committed: usize,
    mode: Mode,
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
        Ok(())
    }
    fn on_error(&mut self, _: i32, _: &Context) -> Result<()> {
        panic!("driver fake emits no retryable native errors")
    }
}
fn receipt(path: &Path) -> serde_json::Value {
    serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
}
fn rejected_before_open(job: Job, message: &str) {
    let output = job.output.clone();
    let err =
        run_with::<Fake>(job, |_, _| panic!("invalid job reached database opener")).unwrap_err();
    assert_eq!(err.kind, ErrorKind::Config, "{message}: {err}");
    assert!(
        !output.exists(),
        "{message} created output before rejection"
    );
}

#[test]
fn valid_sealed_job_records_exact_ack_population_and_exclusive_output() {
    let files = Files::new();
    let source = frames();
    assert_eq!(source.len(), 2);
    let job = files.job(&source);
    let output = job.output.clone();
    let db = Fake::new(Mode::Success);
    run_with(job.clone(), |client, cluster| {
        assert_eq!(client, files.0.join("never-loaded-client"));
        assert_eq!(cluster, files.0.join("never-opened-cluster"));
        Ok(db.clone())
    })
    .unwrap();
    let result = receipt(&output);
    assert_eq!(result["frames"], 2);
    assert_eq!(result["committed_spans"], 2);
    assert_eq!(result["committed_batches"], 2);
    assert_eq!(result["retries"], 0);
    assert!(result["error"].is_null());
    let samples = result["samples"].as_array().unwrap();
    assert_eq!(samples.len(), 2);
    assert!(
        samples
            .iter()
            .all(|s| s["received_spans"] == 1 && s["committed_spans"] == 1 && s["error"].is_null())
    );
    assert!(
        result["boundary"]
            .as_str()
            .unwrap()
            .contains("not external durable drain")
    );
    assert_eq!(
        fs::metadata(&output).unwrap().permissions().mode() & 0o777,
        0o600
    );
    assert_eq!(db.0.lock().unwrap().committed, 2);
    let before = fs::read(&output).unwrap();
    let err = run_with::<Fake>(job, |_, _| panic!("reused output reached opener")).unwrap_err();
    assert_eq!(err.kind, ErrorKind::Config);
    assert_eq!(fs::read(&output).unwrap(), before);
}

#[test]
fn job_bounds_paths_seals_and_manifest_fail_before_open_or_output() {
    let files = Files::new();
    let valid = files.job(&frames());
    let mut cases = Vec::new();
    macro_rules! invalid {
        ($field:ident, $value:expr) => {{
            let mut job = valid.clone();
            job.$field = $value;
            cases.push((stringify!($field), job));
        }};
    }
    invalid!(expected_frames, 0);
    invalid!(expected_frames, 11);
    invalid!(row_budget, 10_000_001);
    invalid!(batch_size, 0);
    invalid!(batch_size, 65);
    invalid!(workers, 0);
    invalid!(workers, 9);
    invalid!(timeout_seconds, 0);
    invalid!(timeout_seconds, 3601);
    invalid!(source_revision, "1".repeat(39));
    invalid!(source_revision, "g".repeat(40));
    invalid!(client_library, PathBuf::from("relative"));
    invalid!(cluster_file, PathBuf::from("relative"));
    invalid!(layout_file, PathBuf::from("relative"));
    invalid!(corpus_file, PathBuf::from("relative"));
    invalid!(output, PathBuf::from("relative"));
    invalid!(client_library, valid.cluster_file.clone());
    invalid!(corpus_sha256, "0".repeat(64));
    invalid!(binary_sha256, "0".repeat(64));
    for (name, job) in cases {
        rejected_before_open(job, name);
    }
    fs::write(&valid.layout_file, b"{}").unwrap();
    rejected_before_open(valid.clone(), "malformed layout");
    let job = files.job(&frames());
    let mut manifest = receipt(&job.layout_file);
    manifest["descriptor_sha256"] = serde_json::Value::String("0".repeat(64));
    fs::write(&job.layout_file, serde_json::to_vec(&manifest).unwrap()).unwrap();
    rejected_before_open(job, "unsupported layout");
}

#[test]
fn strict_json_and_compression_reject_unknown_or_missing_fields() {
    let incomplete = br#"{"expected_frames":1}"#;
    assert!(serde_json::from_slice::<Job>(incomplete).is_err());
    let unknown = br#"{"client_library":"/a","cluster_file":"/b","layout_file":"/c","corpus_file":"/d","corpus_sha256":"x","binary_sha256":"x","source_revision":"x","expected_frames":1,"row_budget":1,"batch_size":1,"workers":1,"record_compression":"none","timeout_seconds":1,"output":"/e","surprise":true}"#;
    assert!(serde_json::from_slice::<Job>(unknown).is_err());
    let unknown_mode = std::str::from_utf8(unknown)
        .unwrap()
        .replace(",\"surprise\":true", "")
        .replace("\"none\"", "\"brotli\"");
    assert!(serde_json::from_str::<Job>(&unknown_mode).is_err());
}

#[test]
fn framed_corpus_guards_reject_truncation_size_and_row_budget() {
    let files = Files::new();
    let valid = frames().remove(0);
    let mut too_many = framed(&[valid.clone(), valid.clone()]);
    let cases = [
        (vec![0], 1),
        (vec![0, 0, 0], 1),
        (vec![0, 0, 0, 0], 1),
        ((8 * 1024 * 1024 + 1u32).to_be_bytes().to_vec(), 1),
        (vec![0, 0, 0, 2, 1], 1),
        (std::mem::take(&mut too_many), 1),
    ];
    for (bytes, limit) in cases {
        let path = files.0.join("bad-frame");
        fs::write(&path, bytes).unwrap();
        let mut input = Input {
            reader: BufReader::new(File::open(path).unwrap()),
            frames: 0,
            ended: false,
        };
        assert_eq!(
            next(&mut input, 64, limit).unwrap_err().kind,
            ErrorKind::Admission
        );
    }
    let path = files.0.join("valid-frame");
    fs::write(&path, framed(std::slice::from_ref(&valid))).unwrap();
    let mut input = Input {
        reader: BufReader::new(File::open(path).unwrap()),
        frames: 0,
        ended: false,
    };
    assert_eq!(next(&mut input, 64, 1).unwrap(), vec![valid]);
    assert_eq!(input.frames, 1);
    assert!(input.ended);
    assert!(next(&mut input, 64, 1).unwrap().is_empty());
}

#[test]
fn malformed_and_many_span_frames_never_reach_transaction_commit() {
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let desc = pool
        .get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
        .unwrap();
    let mut message = DynamicMessage::decode(desc, frames()[0].as_slice()).unwrap();
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
    for wire in [vec![0xff], message.encode_to_vec()] {
        let files = Files::new();
        let job = files.job(&[wire]);
        let output = job.output.clone();
        let db = Fake::new(Mode::Success);
        run_with(job, |_, _| Ok(db.clone())).unwrap_err();
        let result = receipt(&output);
        assert_eq!(result["frames"], 1);
        assert_eq!(result["committed_spans"], 0);
        assert!(result["samples"].as_array().unwrap().is_empty());
        assert!(
            result["error"]
                .as_str()
                .unwrap()
                .contains("exactly one span")
        );
        assert_eq!(db.0.lock().unwrap().attempts, 0);
        assert_eq!(
            db.0.lock().unwrap().data,
            BTreeMap::from([(vec![0], vec![1, 2])])
        );
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
    for (name, spec) in [
        ("start zero", (trace.clone(), span.clone(), 0)),
        (
            "start int64 max",
            (trace.clone(), span.clone(), i64::MAX as u64),
        ),
        ("one nonzero byte per id", (low_trace, low_span, 1)),
    ] {
        let files = Files::new();
        let job = files.job(&[frame(&[vec![spec]])]);
        let output = job.output.clone();
        let db = Fake::new(Mode::Success);
        run_with(job, |_, _| Ok(db.clone())).unwrap();
        assert_eq!(receipt(&output)["committed_spans"], 1, "{name}");
        assert_eq!(db.0.lock().unwrap().committed, 1, "{name}");
    }
    let ok = (trace.clone(), vec![0x33; 8], 1);
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
        let files = Files::new();
        let mut job = files.job(&[frame(&[vec![ok.clone()]]), bad]);
        job.batch_size = 2;
        let output = job.output.clone();
        let db = Fake::new(Mode::Success);
        run_with(job, |_, _| Ok(db.clone())).unwrap_err();
        let result = receipt(&output);
        assert_eq!(result["committed_spans"], 0, "{name}");
        assert!(result["samples"].as_array().unwrap().is_empty(), "{name}");
        assert!(
            result["error"]
                .as_str()
                .unwrap()
                .contains("exactly one span"),
            "{name}"
        );
        assert_eq!(db.0.lock().unwrap().attempts, 0, "{name}");
        assert_eq!(
            db.0.lock().unwrap().data,
            BTreeMap::from([(vec![0], vec![1, 2])]),
            "{name}"
        );
    }
}

#[test]
fn failure_cancels_next_work_and_preserves_acknowledged_partial_receipt() {
    let files = Files::new();
    let mut source = frames();
    source.push(source[0].clone());
    let job = files.job(&source);
    let output = job.output.clone();
    let db = Fake::new(Mode::FailSecond);
    run_with(job, |_, _| Ok(db.clone())).unwrap_err();
    let result = receipt(&output);
    assert_eq!(
        result["frames"], 2,
        "third frame must not be dispatched after failure"
    );
    assert_eq!(result["committed_spans"], 1);
    assert_eq!(result["committed_batches"], 1);
    assert_eq!(result["samples"].as_array().unwrap().len(), 2);
    assert_eq!(result["samples"][0]["committed_spans"], 1);
    assert_eq!(result["samples"][1]["committed_spans"], 0);
    assert!(
        result["error"]
            .as_str()
            .unwrap()
            .contains("injected second commit")
    );
    assert_eq!(db.0.lock().unwrap().attempts, 2);
    assert_eq!(db.0.lock().unwrap().committed, 1);
}

#[test]
fn exact_population_reconciliation_rejects_empty_short_and_extra_source() {
    for (source, expected) in [(vec![], 1), (frames(), 3), (frames(), 1)] {
        let files = Files::new();
        let mut job = files.job(&source);
        job.expected_frames = expected;
        let output = job.output.clone();
        let db = Fake::new(Mode::Success);
        run_with(job, |_, _| Ok(db.clone())).unwrap_err();
        let result = receipt(&output);
        assert_eq!(result["frames"], source.len());
        assert_eq!(result["committed_spans"], source.len());
        assert!(
            result["error"]
                .as_str()
                .unwrap()
                .contains("did not reconcile")
        );
        assert_eq!(db.0.lock().unwrap().committed, source.len());
    }
}

#[test]
fn driver_deadline_records_canceled_commit_without_ack_or_followup() {
    let files = Files::new();
    let mut job = files.job(&frames());
    job.timeout_seconds = 1;
    let output = job.output.clone();
    let db = Fake::new(Mode::Deadline);
    run_with(job, |_, _| Ok(db.clone())).unwrap_err();
    let result = receipt(&output);
    assert_eq!(result["frames"], 1);
    assert_eq!(result["committed_spans"], 0);
    assert_eq!(result["committed_batches"], 0);
    assert_eq!(result["samples"].as_array().unwrap().len(), 1);
    assert!(result["error"].as_str().unwrap().contains("Canceled"));
    assert_eq!(db.0.lock().unwrap().attempts, 1);
    assert_eq!(
        db.0.lock().unwrap().data,
        BTreeMap::from([(vec![0], vec![1, 2])])
    );
}
