//! Standalone in-process writer driver. No listener, server provisioning, or
//! implicit cluster discovery. Every run requires an explicit sealed job file.
use fdb_otel_rust::{
    Error, ErrorKind, Result,
    native::NativeDatabase,
    storage::RecordCompression,
    writer::{Context, Layout, Writer},
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    fs::{File, OpenOptions},
    io::{BufReader, Read, Write},
    os::unix::fs::OpenOptionsExt,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    thread,
    time::{Duration, Instant},
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Job {
    client_library: PathBuf,
    cluster_file: PathBuf,
    layout_file: PathBuf,
    corpus_file: PathBuf,
    corpus_sha256: String,
    binary_sha256: String,
    source_revision: String,
    expected_frames: u64,
    row_budget: u64,
    batch_size: usize,
    workers: usize,
    record_compression: RecordCompression,
    timeout_seconds: u64,
    output: PathBuf,
}
#[derive(Serialize)]
struct Sample {
    ns: u64,
    received_spans: u64,
    committed_spans: u64,
    error: Option<String>,
}
#[derive(Serialize)]
struct Receipt {
    boundary: &'static str,
    source_revision: String,
    binary_sha256: String,
    corpus_sha256: String,
    frames: u64,
    committed_spans: u64,
    committed_batches: u64,
    retries: u64,
    wall_ns: u64,
    samples: Vec<Sample>,
    error: Option<String>,
}
struct Input {
    reader: BufReader<File>,
    frames: u64,
    ended: bool,
}
fn failure(kind: ErrorKind, text: &str) -> Error {
    Error::new(kind, text)
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
fn run(job: Job) -> Result<()> {
    if job.expected_frames == 0
        || job.expected_frames > job.row_budget
        || job.row_budget > 10_000_000
        || !(1..=64).contains(&job.batch_size)
        || !(1..=8).contains(&job.workers)
        || !(1..=3600).contains(&job.timeout_seconds)
        || job.source_revision.len() != 40
        || !job.source_revision.bytes().all(|b| b.is_ascii_hexdigit())
        || [
            &job.client_library,
            &job.cluster_file,
            &job.layout_file,
            &job.corpus_file,
            &job.output,
        ]
        .iter()
        .any(|p| !p.is_absolute())
    {
        return Err(failure(
            ErrorKind::Config,
            "invalid sealed writer job bounds",
        ));
    }
    if hash_file(&job.corpus_file)? != job.corpus_sha256
        || hash_file(
            &std::env::current_exe()
                .map_err(|_| failure(ErrorKind::Config, "cannot find current executable"))?,
        )? != job.binary_sha256
    {
        return Err(failure(
            ErrorKind::Config,
            "corpus or executable SHA256 mismatch",
        ));
    }
    let layout: Layout = serde_json::from_reader(
        File::open(&job.layout_file)
            .map_err(|_| failure(ErrorKind::Config, "cannot open layout manifest"))?,
    )
    .map_err(|_| failure(ErrorKind::Config, "invalid layout manifest"))?;
    // Exclusive-create output before connecting or writing; failed runs leave an
    // no prior result can be silently overwritten. Setup errors are on stderr.
    let mut output = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&job.output)
        .map_err(|_| failure(ErrorKind::Config, "output must be a new writable file"))?;
    let db = NativeDatabase::open(&job.client_library, &job.cluster_file)?;
    let writer = Arc::new(Writer::new(
        db,
        &layout,
        job.batch_size,
        job.record_compression,
    )?);
    let input = Arc::new(Mutex::new(Input {
        reader: BufReader::new(
            File::open(&job.corpus_file)
                .map_err(|_| failure(ErrorKind::Config, "cannot reopen corpus"))?,
        ),
        frames: 0,
        ended: false,
    }));
    let schema = Arc::new(fdb_otel_rust::row::Schema::pinned()?);
    let samples = Arc::new(Mutex::new(Vec::new()));
    let stats = Arc::new(Mutex::new((0u64, 0u64, 0u64)));
    let error = Arc::new(Mutex::new(None::<String>));
    let started = Instant::now();
    let ctx = Context::default().with_deadline(started + Duration::from_secs(job.timeout_seconds));
    thread::scope(|scope| {
        for _ in 0..job.workers {
            let writer = writer.clone();
            let schema = schema.clone();
            let input = input.clone();
            let samples = samples.clone();
            let stats = stats.clone();
            let error = error.clone();
            let ctx = ctx.clone();
            let job = &job;
            scope.spawn(move || {
                loop {
                    if ctx.check().is_err() {
                        break;
                    }
                    let bytes =
                        match next(&mut input.lock().unwrap(), job.batch_size, job.row_budget) {
                            Ok(bytes) if bytes.is_empty() => break,
                            Ok(bytes) => bytes,
                            Err(err) => {
                                *error.lock().unwrap() = Some(err.to_string());
                                ctx.cancel();
                                break;
                            }
                        };
                    // The corpus contract is one single-span ResourceSpans per
                    // frame. A many-span frame cannot bypass the row budget.
                    if !bytes.iter().all(|wire| {
                        schema
                            .admit(std::slice::from_ref(wire))
                            .is_ok_and(|spans| spans.len() == 1)
                    }) {
                        *error.lock().unwrap() =
                            Some("Admission: corpus must have exactly one span per frame".into());
                        ctx.cancel();
                        break;
                    }
                    let begin = Instant::now();
                    let (progress, result) = writer.write(&ctx, &bytes);
                    samples.lock().unwrap().push(Sample {
                        ns: begin.elapsed().as_nanos() as u64,
                        received_spans: bytes.len() as u64,
                        committed_spans: progress.committed_spans,
                        error: result.as_ref().err().map(ToString::to_string),
                    });
                    {
                        let mut total = stats.lock().unwrap();
                        total.0 += progress.committed_spans;
                        total.1 += progress.committed_batches;
                        total.2 += progress.retries;
                    }
                    if let Err(err) = result {
                        let mut first = error.lock().unwrap();
                        if first.is_none() {
                            *first = Some(err.to_string());
                        }
                        ctx.cancel();
                        break;
                    }
                }
            });
        }
    });
    let frames = input.lock().unwrap().frames;
    let totals = *stats.lock().unwrap();
    let mut error = error.lock().unwrap().take();
    if error.is_none()
        && (frames != job.expected_frames
            || totals.0 != job.expected_frames
            || !input.lock().unwrap().ended)
    {
        error = Some("Operation: exact source/committed population did not reconcile".into());
    }
    let failed = error.is_some();
    let receipt = Receipt {
        boundary: "span-only in-process immutable-fixture writer; native libfdb_c 7.3.77; no OTLP receiver, logs, messages, TEXT, HA or open-loop SLO claim; call latency excludes corpus decode precheck; wall includes input reads/precheck and final commit, not external durable drain or audit",
        source_revision: job.source_revision,
        binary_sha256: job.binary_sha256,
        corpus_sha256: job.corpus_sha256,
        frames,
        committed_spans: totals.0,
        committed_batches: totals.1,
        retries: totals.2,
        wall_ns: started.elapsed().as_nanos() as u64,
        samples: std::mem::take(&mut *samples.lock().unwrap()),
        error,
    };
    serde_json::to_writer(&mut output, &receipt)
        .map_err(|_| failure(ErrorKind::Operation, "cannot write run receipt"))?;
    output
        .write_all(b"\n")
        .map_err(|_| failure(ErrorKind::Operation, "cannot finish run receipt"))?;
    output
        .sync_all()
        .map_err(|_| failure(ErrorKind::Operation, "cannot sync run receipt"))?;
    if failed {
        Err(failure(
            ErrorKind::Operation,
            "writer failed; inspect preserved receipt",
        ))
    } else {
        Ok(())
    }
}
fn main() {
    let result = (|| {
        let args = std::env::args_os().skip(1).collect::<Vec<_>>();
        if args.len() != 1 {
            return Err(failure(
                ErrorKind::Config,
                "usage: fdb-otel-rust ABSOLUTE_JOB_JSON",
            ));
        }
        let job: Job = serde_json::from_reader(
            File::open(&args[0])
                .map_err(|_| failure(ErrorKind::Config, "cannot open explicit job file"))?,
        )
        .map_err(|_| failure(ErrorKind::Config, "invalid writer job JSON"))?;
        run(job)
    })();
    if let Err(err) = result {
        eprintln!("{err}");
        std::process::exit(1);
    }
}
