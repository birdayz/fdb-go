//! Standalone in-process ingest driver for one sealed `otel_paired_job_v1` job with
//! mode=ingest and arm=rust_native. No listener, provisioning or cluster discovery.
use fdb_otel_rust::{
    Error, ErrorKind, Result,
    collector::{self, Collector},
    native::{self, NativeDatabase},
    row::Schema,
    storage::RecordCompression,
    writer::{Context, Database, Layout, WriteStats, Writer},
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs::{File, OpenOptions},
    io::{BufReader, Read, Write},
    os::unix::fs::OpenOptionsExt,
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex},
    thread,
    time::{Duration, Instant},
};

const JOB_SCHEMA: &str = "otel_paired_job_v1";
const REPORT_SCHEMA: &str = "otel_control_report_v1";
const BOUNDARY: &str = "span-only in-process ingest of one sealed otel_paired_job_v1 job; native libfdb_c 7.3.77 API 730, multi-version client disabled, 10000ms transaction timeout, no retry-count limit; writer_direct is driver->Writer and collector_queue is driver->Collector->Writer; admission is not an ACK; top-level acked_span_operations is an availability object counting acknowledged write operations including replays, not distinct durable spans; a sample's accepted_spans counts prechecked frames submitted to Writer::write (writer_direct) or spans Collector::consume admitted (collector_queue), never ACKs; only writer_direct samples carry direct_write_acked_operations; outer_retries is structurally 0 because the Rust Writer has no outer retry loop; no OTLP receiver, logs, messages, TEXT, HA or open-loop SLO claim; wall is first corpus read through Flush/shutdown, not external durable drain or audit";
const DIRECT_LATENCY: &str =
    "decode-inclusive Writer::write call per producer batch; frame shape precheck excluded";
const QUEUE_LATENCY: &str = "decode-inclusive Collector::consume admission per producer batch; frame shape precheck excluded; not an ACK";

/// The ingest-mode fields of the frozen paired-job-v1 schema, all required.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Job {
    schema: String,
    mode: String,
    arm: String,
    binary_sha256: String,
    source_revision: String,
    source_manifest_sha256: String,
    timeout_seconds: u64,
    output: PathBuf,
    corpus_config: CorpusConfig,
    expected_frames: u64,
    row_budget: u64,
    corpus_file: PathBuf,
    run: String,
    cluster_file: PathBuf,
    cluster_sha256: String,
    client_backend: String,
    client_library: PathBuf,
    client_library_sha256: String,
    corpus_sha256: String,
    workload: Workload,
    writer_batch_size: usize,
    writer_workers: usize,
    record_compression: String,
    enable_message_deltas: bool,
    enable_message_full_text: bool,
    bulk_index_conflicts: bool,
    layout_file: PathBuf,
    layout_sha256: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CorpusConfig {
    generator: String,
    seed: u64,
    count: u64,
    tenants: u64,
    attributes: u64,
    match_every: u64,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(tag = "kind", deny_unknown_fields)]
enum Workload {
    #[serde(rename = "writer_direct")]
    WriterDirect {
        producers: usize,
        producer_batch_spans: usize,
    },
    #[serde(rename = "collector_queue")]
    CollectorQueue {
        producers: usize,
        producer_batch_spans: usize,
        consumers: usize,
        queue_capacity_spans: usize,
        batch_min_spans: usize,
        batch_max_spans: usize,
        flush_ms: u64,
        block_on_overflow: bool,
        export_timeout_ms: u64,
        retry_enabled: bool,
        retry_initial_ms: u64,
        retry_max_ms: u64,
        retry_elapsed_ms: u64,
    },
}
impl Workload {
    fn producers(&self) -> (usize, usize) {
        match *self {
            Self::WriterDirect {
                producers,
                producer_batch_spans,
            }
            | Self::CollectorQueue {
                producers,
                producer_batch_spans,
                ..
            } => (producers, producer_batch_spans),
        }
    }
}

fn failure(kind: ErrorKind, text: &str) -> Error {
    Error::new(kind, text)
}
fn invalid(field: &str) -> Error {
    Error::new(
        ErrorKind::Config,
        format!("invalid sealed job field: {field}"),
    )
}
fn lower_hex(text: &str, len: usize) -> bool {
    text.len() == len && text.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}
/// Absolute, without empty, `.` or `..` components or a trailing slash.
fn clean_absolute(path: &Path) -> bool {
    let raw = path.as_os_str().as_encoded_bytes();
    raw.first() == Some(&b'/')
        && !raw.contains(&0)
        && path.components().count() > 1
        && path
            .components()
            .skip(1)
            .all(|c| matches!(c, Component::Normal(_)))
        && path.components().collect::<PathBuf>().as_os_str() == path.as_os_str()
}

/// Every schema and cross-field rule for ingest/rust_native; no field has a default.
fn validate(job: &Job, job_path: &Path) -> Result<RecordCompression> {
    let check = |ok: bool, field: &str| if ok { Ok(()) } else { Err(invalid(field)) };
    check(job.schema == JOB_SCHEMA, "schema")?;
    check(job.mode == "ingest", "mode (only ingest is supported)")?;
    check(
        job.arm == "rust_native",
        "arm (only rust_native is supported)",
    )?;
    check(job.client_backend == "native_c", "client_backend")?;
    for (field, value, len) in [
        ("binary_sha256", &job.binary_sha256, 64),
        ("source_revision", &job.source_revision, 40),
        ("source_manifest_sha256", &job.source_manifest_sha256, 64),
        ("cluster_sha256", &job.cluster_sha256, 64),
        ("client_library_sha256", &job.client_library_sha256, 64),
        ("corpus_sha256", &job.corpus_sha256, 64),
        ("layout_sha256", &job.layout_sha256, 64),
    ] {
        check(lower_hex(value, len), field)?;
    }
    check((1..=3600).contains(&job.timeout_seconds), "timeout_seconds")?;
    let c = &job.corpus_config;
    check(c.generator == "otel_fixture_v2", "corpus_config.generator")?;
    check((2..=1_000_000).contains(&c.count), "corpus_config.count")?;
    check(c.tenants == 1, "corpus_config.tenants")?;
    check(c.attributes == 8, "corpus_config.attributes")?;
    check(
        (1..=1_000_000).contains(&c.match_every),
        "corpus_config.match_every",
    )?;
    check(
        (1..=1_000_000).contains(&job.expected_frames) && job.expected_frames == c.count,
        "expected_frames",
    )?;
    check(
        (1..=10_000_000).contains(&job.row_budget) && job.row_budget >= job.expected_frames,
        "row_budget",
    )?;
    check(
        (8..=48).contains(&job.run.len())
            && job
                .run
                .bytes()
                .all(|b| matches!(b, b'a'..=b'z' | b'0'..=b'9' | b'_')),
        "run",
    )?;
    check(
        (1..=64).contains(&job.writer_batch_size),
        "writer_batch_size",
    )?;
    check(
        job.writer_workers == 1,
        "writer_workers (only 1 is supported)",
    )?;
    check(!job.enable_message_deltas, "enable_message_deltas")?;
    check(!job.enable_message_full_text, "enable_message_full_text")?;
    check(!job.bulk_index_conflicts, "bulk_index_conflicts")?;
    let compression = match job.record_compression.as_str() {
        "none" => RecordCompression::None,
        "zlib-1" => RecordCompression::Zlib1,
        "zlib-default" => RecordCompression::ZlibDefault,
        _ => return Err(invalid("record_compression")),
    };
    let (producers, producer_batch) = job.workload.producers();
    check((1..=32).contains(&producers), "workload.producers")?;
    check(
        (1..=64).contains(&producer_batch),
        "workload.producer_batch_spans",
    )?;
    if let Workload::CollectorQueue {
        consumers,
        queue_capacity_spans,
        batch_min_spans,
        batch_max_spans,
        flush_ms,
        export_timeout_ms,
        retry_enabled,
        retry_initial_ms,
        retry_max_ms,
        retry_elapsed_ms,
        ..
    } = job.workload
    {
        let ms = 1..=3_600_000;
        check((1..=32).contains(&consumers), "workload.consumers")?;
        check(
            (1..=10_000_000).contains(&queue_capacity_spans),
            "workload.queue_capacity_spans",
        )?;
        check(
            (1..=10_000_000).contains(&batch_min_spans) && batch_min_spans <= queue_capacity_spans,
            "workload.batch_min_spans",
        )?;
        check(
            (1..=10_000_000).contains(&batch_max_spans) && batch_max_spans >= batch_min_spans,
            "workload.batch_max_spans",
        )?;
        // A larger producer call could never be admitted.
        check(
            producer_batch <= queue_capacity_spans,
            "workload.producer_batch_spans",
        )?;
        check(ms.contains(&flush_ms), "workload.flush_ms")?;
        check(
            ms.contains(&export_timeout_ms),
            "workload.export_timeout_ms",
        )?;
        // Go's retry_on_failure validation: an enabled retry needs a positive interval.
        check(
            retry_initial_ms <= 3_600_000 && (!retry_enabled || retry_initial_ms > 0),
            "workload.retry_initial_ms",
        )?;
        check(
            ms.contains(&retry_max_ms) && retry_max_ms >= retry_initial_ms,
            "workload.retry_max_ms",
        )?;
        check(retry_elapsed_ms <= 3_600_000, "workload.retry_elapsed_ms")?;
    }
    let paths = [
        ("job path", job_path),
        ("client_library", job.client_library.as_path()),
        ("cluster_file", job.cluster_file.as_path()),
        ("layout_file", job.layout_file.as_path()),
        ("corpus_file", job.corpus_file.as_path()),
        ("output", job.output.as_path()),
    ];
    for (i, (field, path)) in paths.iter().enumerate() {
        check(clean_absolute(path), field)?;
        check(
            paths[i + 1..]
                .iter()
                .all(|(_, other)| other.as_os_str() != path.as_os_str()),
            "paths must be distinct",
        )?;
    }
    Ok(compression)
}

fn hash_file(path: &Path) -> Result<String> {
    let mut file =
        File::open(path).map_err(|_| failure(ErrorKind::Config, "cannot open sealed file"))?;
    let mut hash = Sha256::new();
    let mut buf = [0; 65536];
    loop {
        let n = file
            .read(&mut buf)
            .map_err(|_| failure(ErrorKind::Config, "cannot read sealed file"))?;
        if n == 0 {
            break;
        }
        hash.update(&buf[..n]);
    }
    Ok(hex::encode(hash.finalize()))
}
/// Reads a whole sealed file once, so its hash and its parsed content agree.
fn read_sealed(path: &Path, limit: u64) -> Result<Vec<u8>> {
    let mut bytes = Vec::new();
    File::open(path)
        .and_then(|f| f.take(limit + 1).read_to_end(&mut bytes))
        .map_err(|_| failure(ErrorKind::Config, "cannot read sealed file"))?;
    if bytes.len() as u64 > limit {
        return Err(failure(
            ErrorKind::Config,
            "sealed file exceeds its size bound",
        ));
    }
    Ok(bytes)
}

struct Input {
    reader: BufReader<File>,
    frames: u64,
    ended: bool,
}
fn next(input: &mut Input, batch_size: usize, limit: u64) -> Result<Vec<Vec<u8>>> {
    let mut out = Vec::new();
    for _ in 0..batch_size {
        if input.ended {
            break;
        }
        let mut size = [0; 4];
        match input.reader.read(&mut size[..1]) {
            Ok(0) => {
                input.ended = true;
                break;
            }
            Ok(_) => {}
            Err(_) => return Err(failure(ErrorKind::Admission, "corpus read failed")),
        }
        input
            .reader
            .read_exact(&mut size[1..])
            .map_err(|_| failure(ErrorKind::Admission, "truncated corpus frame header"))?;
        let size = u32::from_be_bytes(size) as usize;
        if size == 0 || size > 8 * 1024 * 1024 || input.frames >= limit {
            return Err(failure(
                ErrorKind::Admission,
                "corpus frame size or row budget exceeded",
            ));
        }
        let mut bytes = vec![0; size];
        input
            .reader
            .read_exact(&mut bytes)
            .map_err(|_| failure(ErrorKind::Admission, "truncated corpus frame"))?;
        out.push(bytes);
        input.frames += 1;
    }
    Ok(out)
}

#[derive(Serialize)]
struct Sample {
    ns: u64,
    received_spans: u64,
    /// writer_direct: prechecked frames submitted to Writer::write (not ACKs);
    /// collector_queue: spans `consume` admitted.
    accepted_spans: u64,
    /// writer_direct only: that call's WriteStats ACK operations; omitted otherwise.
    #[serde(skip_serializing_if = "Option::is_none")]
    direct_write_acked_operations: Option<u64>,
    error: Option<String>,
}
#[derive(Default, Serialize)]
struct Latency {
    population: u64,
    min_ns: Option<u64>,
    p50_ns: Option<u64>,
    p95_ns: Option<u64>,
    p99_ns: Option<u64>,
    max_ns: Option<u64>,
}
impl Latency {
    /// Nearest-rank percentiles over every timed call, failed calls included.
    fn of(samples: &[Sample]) -> Self {
        let mut ns: Vec<u64> = samples.iter().map(|s| s.ns).collect();
        ns.sort_unstable();
        let rank = |q: f64| {
            if ns.is_empty() {
                return None;
            }
            let i = ((q * ns.len() as f64).ceil() as usize).clamp(1, ns.len());
            ns.get(i - 1).copied()
        };
        Self {
            population: ns.len() as u64,
            min_ns: ns.first().copied(),
            p50_ns: rank(0.50),
            p95_ns: rank(0.95),
            p99_ns: rank(0.99),
            max_ns: ns.last().copied(),
        }
    }
}
#[derive(Serialize)]
struct CompletionSample {
    seq: u64,
    spans: usize,
    arrival_ns: u64,
    completed_ns: u64,
    error: Option<String>,
}
#[derive(Serialize)]
struct QueueReport {
    max_outstanding_spans: usize,
    blocked_requests: u64,
    blocked_ns: u64,
    export_call_spans: BTreeMap<u64, u64>,
    completion_samples: Vec<CompletionSample>,
    completion_samples_omitted: u64,
}
#[derive(Serialize)]
struct Resources {
    scope: &'static str,
    user_cpu_seconds: f64,
    system_cpu_seconds: f64,
    cpu_seconds: f64,
    peak_rss_bytes: u64,
    peak_rss_scope: &'static str,
    measurement_source: &'static str,
}

/// getrusage(2)'s `struct rusage` on Linux x86_64 (glibc bits/types/struct_rusage.h):
/// two timevals, then fourteen longs, 144 bytes. It is bound on no other target.
#[repr(C)]
#[derive(Default)]
struct RUsage {
    user_s: i64,
    user_us: i64,
    system_s: i64,
    system_us: i64,
    max_rss_kb: i64,
    _rest: [i64; 13],
}
const RUSAGE_BOUND: bool = cfg!(all(target_os = "linux", target_arch = "x86_64"));
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
unsafe extern "C" {
    fn getrusage(who: i32, usage: *mut RUsage) -> i32;
}
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
fn rusage() -> Option<RUsage> {
    let mut usage = RUsage::default();
    // RUSAGE_SELF (0) covers every thread, including the native network thread.
    (unsafe { getrusage(0, &mut usage) } == 0).then_some(usage)
}
/// Elsewhere the ABI is unverified, so CPU and RSS are reported unavailable.
#[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
fn rusage() -> Option<RUsage> {
    None
}
fn resources(start: &RUsage, end: &RUsage) -> Resources {
    let seconds = |s: i64, us: i64| s as f64 + us as f64 / 1e6;
    let user = seconds(end.user_s, end.user_us) - seconds(start.user_s, start.user_us);
    let system = seconds(end.system_s, end.system_us) - seconds(start.system_s, start.system_us);
    Resources {
        scope: "first_read_through_flush",
        user_cpu_seconds: user,
        system_cpu_seconds: system,
        cpu_seconds: user + system,
        peak_rss_bytes: end.max_rss_kb.max(0) as u64 * 1024,
        peak_rss_scope: "whole-process high-water mark at end of Flush/shutdown, not a phase peak",
        measurement_source: "getrusage(RUSAGE_SELF) deltas from before the first corpus read to after Flush/shutdown",
    }
}

/// A counter with explicit availability; Rust ingest always observes its ACK operations.
#[derive(Serialize)]
struct Availability {
    availability: &'static str,
    value: u64,
}
impl Availability {
    fn available(value: u64) -> Self {
        Self {
            availability: "available",
            value,
        }
    }
}

#[derive(Serialize)]
struct Report {
    schema: &'static str,
    mode: &'static str,
    arm: &'static str,
    status: &'static str,
    error: Option<String>,
    source_revision: String,
    source_manifest_sha256: String,
    binary_sha256: String,
    job_sha256: String,
    corpus_sha256: String,
    layout_sha256: String,
    cluster_sha256: String,
    client_library_sha256: String,
    boundary: &'static str,
    latency_boundary: &'static str,
    effective_job: serde_json::Value,
    artifacts: Vec<serde_json::Value>,
    frames: u64,
    accepted_spans: Option<u64>,
    rejected_spans: Option<u64>,
    /// Acknowledged write operations including replays; not distinct durable spans.
    acked_span_operations: Availability,
    committed_batches: u64,
    export_attempts: Option<u64>,
    native_retries: u64,
    outer_retries: u64,
    queue_retries: Option<u64>,
    exported_spans: Option<u64>,
    failed_spans: Option<u64>,
    queued_spans_at_exit: Option<u64>,
    ingest_wall_ns: Option<u64>,
    admission_phase_ns: Option<u64>,
    drain_ns: Option<u64>,
    samples: Vec<Sample>,
    admission_latency: Latency,
    transaction_spans: BTreeMap<u64, u64>,
    queue: Option<QueueReport>,
    ingest_resources: Option<Resources>,
    /// Why `ingest_resources` is null although ingest started.
    ingest_resources_unavailable: Option<&'static str>,
}

/// State shared by producer threads; the first error wins and stops publishing.
struct Shared<'a> {
    input: Mutex<Input>,
    samples: Mutex<Vec<Sample>>,
    direct: Mutex<WriteStats>,
    error: Mutex<Option<Error>>,
    publish: Context,
    schema: &'a Schema,
    batch: usize,
    budget: u64,
}
impl Shared<'_> {
    fn fail(&self, err: Error) {
        let mut first = self.error.lock().unwrap();
        if first.is_none() {
            *first = Some(err);
        }
        self.publish.cancel();
    }
}
/// One timed producer call: (accepted spans, writer progress, outcome).
type Call<'a> = dyn Fn(&Context, &[Vec<u8>]) -> (u64, Option<WriteStats>, Result<()>) + Sync + 'a;

/// `producers` threads read `batch` frames each, precheck them untimed, then time `call`.
fn produce(
    shared: &Shared<'_>,
    call: &Call<'_>,
    producers: usize,
    before_spawn: &mut impl FnMut(usize) -> std::io::Result<()>,
) {
    // The scope joins every started producer, including after a spawn failure.
    thread::scope(|scope| {
        for i in 0..producers {
            let worker = move || {
                loop {
                    if shared.publish.check().is_err() {
                        break;
                    }
                    let frames = match next(
                        &mut shared.input.lock().unwrap(),
                        shared.batch,
                        shared.budget,
                    ) {
                        Ok(frames) if frames.is_empty() => break,
                        Ok(frames) => frames,
                        Err(err) => {
                            shared.fail(err);
                            break;
                        }
                    };
                    // One single-span ResourceSpans per frame, by prepare's own rules.
                    if !frames.iter().all(|wire| {
                        shared
                            .schema
                            .validate(std::slice::from_ref(wire))
                            .is_ok_and(|spans| spans == 1)
                    }) {
                        shared.fail(failure(
                            ErrorKind::Admission,
                            "corpus must have exactly one span per frame",
                        ));
                        break;
                    }
                    let begin = Instant::now();
                    let (accepted, progress, result) = call(&shared.publish, &frames);
                    let ns = begin.elapsed().as_nanos() as u64;
                    shared.samples.lock().unwrap().push(Sample {
                        ns,
                        received_spans: frames.len() as u64,
                        accepted_spans: accepted,
                        direct_write_acked_operations: progress.as_ref().map(|p| p.committed_spans),
                        error: result.as_ref().err().map(ToString::to_string),
                    });
                    if let Some(progress) = progress {
                        let mut total = shared.direct.lock().unwrap();
                        total.committed_spans += progress.committed_spans;
                        total.committed_batches += progress.committed_batches;
                        total.retries += progress.retries;
                        total.logical_otlp_bytes += progress.logical_otlp_bytes;
                        for (size, count) in progress.transaction_spans {
                            *total.transaction_spans.entry(size).or_default() += count;
                        }
                    }
                    if let Err(err) = result {
                        shared.fail(err);
                        break;
                    }
                }
            };
            let spawned = before_spawn(i).and_then(|()| {
                thread::Builder::new()
                    .name(format!("otel-producer-{i}"))
                    .spawn_scoped(scope, worker)
            });
            if spawned.is_err() {
                shared.fail(failure(
                    ErrorKind::Operation,
                    "cannot spawn producer thread",
                ));
                break;
            }
        }
    });
}

fn effective_job(job: &Job) -> serde_json::Value {
    let (producers, _) = job.workload.producers();
    let writers = match job.workload {
        Workload::WriterDirect { .. } => producers,
        Workload::CollectorQueue { consumers, .. } => consumers,
    };
    serde_json::json!({
        "schema": job.schema,
        "mode": job.mode,
        "arm": job.arm,
        "client_backend": job.client_backend,
        "run": job.run,
        "corpus_config": {
            "generator": job.corpus_config.generator,
            "seed": job.corpus_config.seed,
            "count": job.corpus_config.count,
            "tenants": job.corpus_config.tenants,
            "attributes": job.corpus_config.attributes,
            "match_every": job.corpus_config.match_every,
        },
        "expected_frames": job.expected_frames,
        "row_budget": job.row_budget,
        "timeout_seconds": job.timeout_seconds,
        "workload": job.workload,
        "writer_batch_size": job.writer_batch_size,
        "writer_workers": job.writer_workers,
        "global_write_bound": writers * job.writer_workers,
        "record_compression": job.record_compression,
        "enable_message_deltas": job.enable_message_deltas,
        "enable_message_full_text": job.enable_message_full_text,
        "bulk_index_conflicts": job.bulk_index_conflicts,
        "native_policy": {
            "client": "libfdb_c 7.3.77",
            "api_version": 730,
            "multi_version_client": "disabled",
            "transaction_timeout_ms": 10000,
            "transaction_retry_limit": -1,
            "transaction_max_spans": 64,
            "transaction_estimate_bytes": 300000,
            "transaction_actual_bytes": 500000,
        },
    })
}

fn run(job_path: &Path) -> Result<()> {
    run_spawning(
        job_path,
        native::CLIENT_SHA256,
        NativeDatabase::open,
        |_| Ok(()),
    )
}

/// `client_pin` is the only accepted client SHA256; `before_spawn(i)` precedes producer
/// `i`'s spawn and its error takes the spawn-failure path.
fn run_spawning<D: Database + 'static>(
    job_path: &Path,
    client_pin: &str,
    open: impl FnOnce(&Path, &Path) -> Result<D>,
    mut before_spawn: impl FnMut(usize) -> std::io::Result<()>,
) -> Result<()> {
    let deadline_start = Instant::now();
    if !clean_absolute(job_path) {
        return Err(invalid("job path"));
    }
    let job_bytes = read_sealed(job_path, 1 << 20)?;
    let job_sha256 = hex::encode(Sha256::digest(&job_bytes));
    let job: Job = serde_json::from_slice(&job_bytes)
        .map_err(|_| failure(ErrorKind::Config, "invalid otel_paired_job_v1 JSON"))?;
    let compression = validate(&job, job_path)?;
    let seal = |ok: bool, what: &str| {
        if ok {
            Ok(())
        } else {
            Err(failure(ErrorKind::Config, what))
        }
    };
    let exe = std::env::current_exe()
        .map_err(|_| failure(ErrorKind::Config, "cannot find current executable"))?;
    seal(
        hash_file(&exe)? == job.binary_sha256,
        "executable SHA256 mismatch",
    )?;
    seal(
        job.client_library_sha256 == client_pin
            && hash_file(&job.client_library)? == job.client_library_sha256,
        "client library is not the pinned libfdb_c",
    )?;
    let cluster = read_sealed(&job.cluster_file, 64 * 1024)?;
    seal(
        hex::encode(Sha256::digest(&cluster)) == job.cluster_sha256,
        "cluster file SHA256 mismatch",
    )?;
    let layout_bytes = read_sealed(&job.layout_file, 64 * 1024 * 1024)?;
    seal(
        hex::encode(Sha256::digest(&layout_bytes)) == job.layout_sha256,
        "layout SHA256 mismatch",
    )?;
    let layout: Layout = serde_json::from_slice(&layout_bytes)
        .map_err(|_| failure(ErrorKind::Config, "invalid layout manifest"))?;
    layout.validate()?;
    seal(
        hash_file(&job.corpus_file)? == job.corpus_sha256,
        "corpus SHA256 mismatch",
    )?;
    // Reserve the report before any database side effect; nothing is ever overwritten.
    let mut output = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&job.output)
        .map_err(|_| failure(ErrorKind::Config, "output must be a new writable file"))?;
    let collector = matches!(job.workload, Workload::CollectorQueue { .. });
    let mut report = Report {
        schema: REPORT_SCHEMA,
        mode: "ingest",
        arm: "rust_native",
        status: "error",
        error: None,
        source_revision: job.source_revision.clone(),
        source_manifest_sha256: job.source_manifest_sha256.clone(),
        binary_sha256: job.binary_sha256.clone(),
        job_sha256,
        corpus_sha256: job.corpus_sha256.clone(),
        layout_sha256: job.layout_sha256.clone(),
        cluster_sha256: job.cluster_sha256.clone(),
        client_library_sha256: job.client_library_sha256.clone(),
        boundary: BOUNDARY,
        latency_boundary: if collector {
            QUEUE_LATENCY
        } else {
            DIRECT_LATENCY
        },
        effective_job: effective_job(&job),
        artifacts: Vec::new(),
        frames: 0,
        accepted_spans: collector.then_some(0),
        rejected_spans: collector.then_some(0),
        acked_span_operations: Availability::available(0),
        committed_batches: 0,
        export_attempts: collector.then_some(0),
        native_retries: 0,
        outer_retries: 0,
        queue_retries: collector.then_some(0),
        exported_spans: collector.then_some(0),
        failed_spans: collector.then_some(0),
        queued_spans_at_exit: collector.then_some(0),
        ingest_wall_ns: None,
        admission_phase_ns: None,
        drain_ns: None,
        samples: Vec::new(),
        admission_latency: Latency::default(),
        transaction_spans: BTreeMap::new(),
        queue: None,
        ingest_resources: None,
        ingest_resources_unavailable: None,
    };
    let deadline = deadline_start + Duration::from_secs(job.timeout_seconds);
    let outcome = ingest(
        &job,
        compression,
        &layout,
        open,
        &mut before_spawn,
        deadline,
        &mut report,
    );
    if let Err(err) = &outcome {
        report.error = Some(err.to_string());
    } else {
        report.status = "ok";
    }
    serde_json::to_writer(&mut output, &report)
        .map_err(|_| failure(ErrorKind::Operation, "cannot write run receipt"))?;
    output
        .write_all(b"\n")
        .map_err(|_| failure(ErrorKind::Operation, "cannot finish run receipt"))?;
    output
        .sync_all()
        .map_err(|_| failure(ErrorKind::Operation, "cannot sync run receipt"))?;
    outcome
}

fn nanos(d: Duration) -> u64 {
    d.as_nanos().min(u64::MAX as u128) as u64
}

/// Everything after the report is reserved; fills `report` with acknowledged progress.
fn ingest<D: Database + 'static>(
    job: &Job,
    compression: RecordCompression,
    layout: &Layout,
    open: impl FnOnce(&Path, &Path) -> Result<D>,
    before_spawn: &mut impl FnMut(usize) -> std::io::Result<()>,
    deadline: Instant,
    report: &mut Report,
) -> Result<()> {
    let db = open(&job.client_library, &job.cluster_file)?;
    let writer = Arc::new(Writer::new(db, layout, job.writer_batch_size, compression)?);
    let schema = Schema::pinned()?;
    let (producers, producer_batch) = job.workload.producers();
    let shared = Shared {
        input: Mutex::new(Input {
            reader: BufReader::new(
                File::open(&job.corpus_file)
                    .map_err(|_| failure(ErrorKind::Config, "cannot reopen corpus"))?,
            ),
            frames: 0,
            ended: false,
        }),
        samples: Mutex::new(Vec::new()),
        direct: Mutex::new(WriteStats::default()),
        error: Mutex::new(None),
        publish: Context::default().with_deadline(deadline),
        schema: &schema,
        batch: producer_batch,
        budget: job.row_budget,
    };
    // Draining is bounded by the job deadline but not canceled by a producer failure.
    let drain = Context::default().with_deadline(deadline);
    let collector = match job.workload {
        Workload::WriterDirect { .. } => None,
        Workload::CollectorQueue {
            consumers,
            queue_capacity_spans,
            batch_min_spans,
            batch_max_spans,
            flush_ms,
            block_on_overflow,
            export_timeout_ms,
            retry_enabled,
            retry_initial_ms,
            retry_max_ms,
            retry_elapsed_ms,
            ..
        } => Some(Collector::start(
            writer.clone(),
            collector::Config {
                consumers,
                queue_spans: queue_capacity_spans,
                batch_min: batch_min_spans,
                batch_max: batch_max_spans,
                flush_ms,
                block_on_overflow,
                export_timeout_ms,
                retry_enabled,
                retry_initial_ms,
                retry_max_ms,
                retry_elapsed_ms,
            },
        )?),
    };
    let start_usage = rusage();
    let t0 = Instant::now();
    match &collector {
        None => {
            let direct = |ctx: &Context, frames: &[Vec<u8>]| {
                let (progress, result) = writer.write(ctx, frames);
                (frames.len() as u64, Some(progress), result)
            };
            produce(&shared, &direct, producers, before_spawn);
        }
        Some(queue) => {
            let admit = |ctx: &Context, frames: &[Vec<u8>]| {
                let result = queue.consume(ctx, frames);
                let accepted = if result.is_ok() {
                    frames.len() as u64
                } else {
                    0
                };
                (accepted, None, result)
            };
            produce(&shared, &admit, producers, before_spawn);
        }
    }
    let admitted = Instant::now();
    report.admission_phase_ns = Some(nanos(admitted - t0));
    // Sequence-covering Flush and shutdown within the job deadline, then the final drop
    // cancels what remains and joins every consumer: only settled counters are reported.
    let settled = collector.map(|queue| {
        if let Err(err) = queue.flush(&drain) {
            shared.fail(err);
        }
        if let Err(err) = queue.shutdown(&drain) {
            shared.fail(err);
        }
        queue.into_final()
    });
    if settled.is_some() {
        report.drain_ns = Some(nanos(admitted.elapsed()));
    }
    let end_usage = rusage();
    report.ingest_wall_ns = Some(nanos(t0.elapsed()));
    report.ingest_resources = start_usage
        .zip(end_usage)
        .map(|(start, end)| resources(&start, &end));
    if report.ingest_resources.is_none() {
        report.ingest_resources_unavailable = Some(if RUSAGE_BOUND {
            "getrusage(RUSAGE_SELF) failed"
        } else {
            "getrusage is bound only for Linux x86_64"
        });
    }
    let (frames, ended) = {
        let input = shared.input.lock().unwrap();
        (input.frames, input.ended)
    };
    report.frames = frames;
    let samples = std::mem::take(&mut *shared.samples.lock().unwrap());
    report.admission_latency = Latency::of(&samples);
    report.samples = samples;
    let exact = match settled {
        None => {
            let total = shared.direct.lock().unwrap();
            report.acked_span_operations = Availability::available(total.committed_spans);
            report.committed_batches = total.committed_batches;
            report.native_retries = total.retries;
            report.transaction_spans = total.transaction_spans.clone();
            total.committed_spans == job.expected_frames
        }
        Some((s, completions)) => {
            report.accepted_spans = Some(s.accepted_spans);
            report.rejected_spans = Some(s.rejected_spans);
            report.acked_span_operations = Availability::available(s.stored_spans);
            report.committed_batches = s.committed_batches;
            report.export_attempts = Some(s.export_attempts);
            report.native_retries = s.native_retries;
            report.queue_retries = Some(s.retries);
            report.exported_spans = Some(s.exported_spans);
            report.failed_spans = Some(s.failed_spans);
            report.queued_spans_at_exit = Some(s.queued_spans as u64);
            report.transaction_spans = s.transaction_spans.clone();
            let since = |at: Instant| nanos(at.saturating_duration_since(t0));
            report.queue = Some(QueueReport {
                max_outstanding_spans: s.max_outstanding_spans,
                blocked_requests: s.blocked_requests,
                blocked_ns: s.blocked_ns,
                export_call_spans: s.export_call_spans.clone(),
                completion_samples: completions
                    .into_iter()
                    .map(|c| CompletionSample {
                        seq: c.seq,
                        spans: c.spans,
                        arrival_ns: since(c.arrival),
                        completed_ns: since(c.completed),
                        error: c.error,
                    })
                    .collect(),
                completion_samples_omitted: s.completions_omitted,
            });
            s.accepted_spans == job.expected_frames
                && s.exported_spans == job.expected_frames
                && s.rejected_spans == 0
                && s.failed_spans == 0
                && s.queued_spans == 0
        }
    };
    if let Some(err) = shared.error.lock().unwrap().take() {
        return Err(err);
    }
    if !(exact && frames == job.expected_frames && ended) {
        return Err(failure(
            ErrorKind::Operation,
            "exact source/committed population did not reconcile",
        ));
    }
    Ok(())
}

#[cfg(test)]
#[path = "driver_tests.rs"]
mod tests;

fn main() {
    let result = (|| {
        let args = std::env::args_os().skip(1).collect::<Vec<_>>();
        if args.len() != 1 {
            return Err(failure(
                ErrorKind::Config,
                "usage: otel-rust ABSOLUTE_JOB_JSON",
            ));
        }
        run(Path::new(&args[0]))
    })();
    if let Err(err) = result {
        eprintln!("{err}");
        std::process::exit(1);
    }
}
