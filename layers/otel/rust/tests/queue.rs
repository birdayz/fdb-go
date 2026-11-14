use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{
    Error, ErrorKind, Result,
    collector::{Collector, Config, Sink},
    writer::{Context, WriteStats},
};
use serde::Deserialize;
use std::{
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicUsize, Ordering},
        mpsc,
    },
    time::{Duration, Instant},
};
#[derive(Deserialize)]
struct Vector {
    name: String,
    otlp: String,
}
fn source() -> Vec<Vec<u8>> {
    let vectors: Vec<Vector> =
        serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    vectors
        .into_iter()
        .filter(|v| v.name.starts_with("v2_"))
        .map(|v| STANDARD.decode(v.otlp).unwrap())
        .collect()
}
fn bounded() -> Context {
    Context::default().with_deadline(Instant::now() + Duration::from_secs(3))
}
/// One consumer and `batch` spans per export; every parameter explicit.
fn config(queue_spans: usize, batch: usize, flush_ms: u64, retry: Option<u64>) -> Config {
    Config {
        consumers: 1,
        queue_spans,
        batch_min: batch,
        batch_max: batch,
        flush_ms,
        block_on_overflow: false,
        export_timeout_ms: 30_000,
        retry_enabled: retry.is_some(),
        retry_initial_ms: retry.unwrap_or(0),
        retry_max_ms: retry.unwrap_or(1),
        retry_elapsed_ms: 300_000,
    }
}
struct Gate {
    open: Mutex<bool>,
    changed: Condvar,
    entered: mpsc::SyncSender<()>,
}
impl Sink for Gate {
    fn export(&self, ctx: &Context, spans: &[Vec<u8>]) -> (WriteStats, Result<()>) {
        let _ = self.entered.try_send(());
        let mut open = self.open.lock().unwrap();
        while !*open {
            if let Err(err) = ctx.check() {
                return (WriteStats::default(), Err(err));
            }
            open = self
                .changed
                .wait_timeout(open, Duration::from_millis(5))
                .unwrap()
                .0;
        }
        (
            WriteStats {
                committed_spans: spans.len() as u64,
                ..WriteStats::default()
            },
            Ok(()),
        )
    }
}
#[test]
fn queue_capacity_includes_active_spans_and_shutdown_drains() {
    let (entered, receiver) = mpsc::sync_channel(8);
    let sink = Arc::new(Gate {
        open: Mutex::new(false),
        changed: Condvar::new(),
        entered,
    });
    let cfg = config(2, 1, 200, Some(5_000));
    let collector = Collector::start(sink.clone(), cfg).unwrap();
    let spans = source();
    let ctx = bounded();
    collector.consume(&ctx, &spans[..1]).unwrap();
    receiver.recv_timeout(Duration::from_secs(2)).unwrap();
    collector.consume(&ctx, &spans[1..2]).unwrap();
    assert_eq!(
        collector.consume(&ctx, &spans[2..3]).unwrap_err().kind,
        ErrorKind::Budget
    );
    assert_eq!(collector.stats().queued_spans, 2);
    *sink.open.lock().unwrap() = true;
    sink.changed.notify_all();
    collector.shutdown(&ctx).unwrap();
    let stats = collector.stats();
    assert_eq!(stats.accepted_spans, 2);
    assert_eq!(stats.rejected_spans, 1);
    assert_eq!(stats.exported_spans, 2);
    assert_eq!(stats.stored_spans, 2);
    assert_eq!(stats.failed_spans, 0);
    assert_eq!(stats.queued_spans, 0);
    assert_eq!(
        collector.consume(&ctx, &spans[..1]).unwrap_err().kind,
        ErrorKind::Operation
    );
    collector.shutdown(&ctx).unwrap();
}
struct Selective {
    bad: Vec<u8>,
    calls: AtomicUsize,
}
impl Sink for Selective {
    fn export(&self, _: &Context, spans: &[Vec<u8>]) -> (WriteStats, Result<()>) {
        self.calls.fetch_add(1, Ordering::SeqCst);
        if spans.contains(&self.bad) {
            return (
                WriteStats::default(),
                Err(Error::new(ErrorKind::Admission, "bad request")),
            );
        }
        (
            WriteStats {
                committed_spans: spans.len() as u64,
                ..WriteStats::default()
            },
            Ok(()),
        )
    }
}
#[test]
fn permanent_merged_failure_isolates_requests_and_flush_clears_errors() {
    let source = source();
    let ctx = bounded();
    let sink = Arc::new(Selective {
        bad: source[2].clone(),
        calls: AtomicUsize::new(0),
    });
    let cfg = config(8, 8, 60_000, Some(5_000));
    let collector = Collector::start(sink.clone(), cfg).unwrap();
    collector.consume(&ctx, &source[..2]).unwrap();
    collector.consume(&ctx, &source[2..3]).unwrap();
    assert_eq!(
        collector.flush(&ctx).unwrap_err().kind,
        ErrorKind::Admission
    );
    collector.flush(&ctx).unwrap();
    collector.shutdown(&ctx).unwrap();
    let stats = collector.stats();
    assert_eq!(stats.accepted_spans, 3);
    assert_eq!(stats.failed_spans, 1);
    assert_eq!(stats.exported_spans, 2);
    assert_eq!(stats.stored_spans, 2);
    assert_eq!(stats.retries, 0);
    assert_eq!(sink.calls.load(Ordering::SeqCst), 3);
}
struct Transient(AtomicUsize);
impl Sink for Transient {
    fn export(&self, _: &Context, spans: &[Vec<u8>]) -> (WriteStats, Result<()>) {
        if self.0.fetch_add(1, Ordering::SeqCst) == 0 {
            return (
                WriteStats::default(),
                Err(Error::new(ErrorKind::Transient, "retry me")),
            );
        }
        (
            WriteStats {
                committed_spans: spans.len() as u64,
                ..WriteStats::default()
            },
            Ok(()),
        )
    }
}
#[test]
fn transient_retry_and_deadline_cancellation_do_not_claim_an_ack() {
    let sink = Arc::new(Transient(AtomicUsize::new(0)));
    let cfg = config(10_000, 1, 200, Some(1));
    let collector = Collector::start(sink.clone(), cfg).unwrap();
    let source = source();
    collector.consume(&bounded(), &source[..1]).unwrap();
    collector.shutdown(&bounded()).unwrap();
    assert_eq!(sink.0.load(Ordering::SeqCst), 2);
    assert_eq!(collector.stats().retries, 1);
    assert_eq!(collector.stats().exported_spans, 1);
    let (entered, receiver) = mpsc::sync_channel(1);
    let gate = Arc::new(Gate {
        open: Mutex::new(false),
        changed: Condvar::new(),
        entered,
    });
    let cfg = config(10_000, 1, 200, None);
    let collector = Collector::start(gate, cfg).unwrap();
    collector.consume(&bounded(), &source[..1]).unwrap();
    receiver.recv_timeout(Duration::from_secs(2)).unwrap();
    let canceled = Context::default();
    canceled.cancel();
    assert_eq!(
        collector.shutdown(&canceled).unwrap_err().kind,
        ErrorKind::Canceled
    );
    assert!(collector.shutdown(&bounded()).is_err());
    assert_eq!(collector.stats().exported_spans, 0);
    assert_eq!(collector.stats().failed_spans, 1);
    assert_eq!(collector.stats().queued_spans, 0);
}
#[test]
fn retry_is_explicitly_enabled_and_an_enabled_zero_interval_is_refused() {
    let mut zero = config(10_000, 1, 200, Some(1));
    zero.retry_initial_ms = 0;
    let refused = Collector::start(Arc::new(Transient(AtomicUsize::new(0))), zero);
    assert_eq!(refused.err().unwrap().kind, ErrorKind::Config);
    // Disabled retry: a transient failure completes the request as failed after one attempt.
    let sink = Arc::new(Transient(AtomicUsize::new(0)));
    let collector = Collector::start(sink.clone(), config(10_000, 1, 200, None)).unwrap();
    collector.consume(&bounded(), &source()[..1]).unwrap();
    assert_eq!(
        collector.shutdown(&bounded()).unwrap_err().kind,
        ErrorKind::Transient
    );
    let stats = collector.stats();
    assert_eq!(sink.0.load(Ordering::SeqCst), 1);
    assert_eq!((stats.export_attempts, stats.retries), (1, 0));
    assert_eq!((stats.exported_spans, stats.failed_spans), (0, 1));
    let completions = collector.completions();
    assert_eq!(completions.len(), 1);
    assert_eq!((completions[0].seq, completions[0].spans), (0, 1));
    assert!(
        completions[0]
            .error
            .as_deref()
            .unwrap()
            .contains("retry me")
    );
}
