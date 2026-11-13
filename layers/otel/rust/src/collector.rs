//! Bounded, in-process span queue. Admission is not a durable ACK; Flush is a
//! sequence barrier, and export counters only count completed sink calls.
use crate::{
    Error, ErrorKind, Result,
    row::Schema,
    writer::{Context, Database, WriteStats, Writer},
};
use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, VecDeque},
    sync::{Arc, Condvar, Mutex},
    thread,
    time::{Duration, Instant},
};

pub trait Sink: Send + Sync + 'static {
    fn export(&self, ctx: &Context, spans: &[Vec<u8>]) -> (WriteStats, Result<()>);
}
impl<D: Database + 'static> Sink for Writer<D> {
    fn export(&self, ctx: &Context, spans: &[Vec<u8>]) -> (WriteStats, Result<()>) {
        self.write(ctx, spans)
    }
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    pub consumers: usize,
    pub queue_spans: usize,
    pub batch_min: usize,
    pub batch_max: usize,
    pub flush_ms: u64,
    pub block_on_overflow: bool,
    pub export_timeout_ms: u64,
    pub retry_initial_ms: u64,
    pub retry_max_ms: u64,
    pub retry_elapsed_ms: u64,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            consumers: 4,
            queue_spans: 10_000,
            batch_min: 512,
            batch_max: 4096,
            flush_ms: 200,
            block_on_overflow: false,
            export_timeout_ms: 30_000,
            retry_initial_ms: 5000,
            retry_max_ms: 30_000,
            retry_elapsed_ms: 300_000,
        }
    }
}
#[derive(Clone, Default, Debug, Serialize)]
pub struct Stats {
    pub accepted_spans: u64,
    pub rejected_spans: u64,
    pub exported_spans: u64,
    pub stored_spans: u64,
    pub failed_spans: u64,
    pub retries: u64,
    pub queued_spans: usize,
}
struct Part {
    seq: u64,
    spans: VecDeque<Vec<u8>>,
    at: Instant,
}
struct State {
    pending: VecDeque<Part>,
    pending_spans: usize,
    outstanding: usize,
    next: u64,
    remaining: BTreeMap<u64, usize>,
    failures: Vec<Error>,
    omitted: usize,
    force: bool,
    stopping: bool,
    workers: usize,
    stats: Stats,
}
struct Shared {
    state: Mutex<State>,
    changed: Condvar,
    work: Context,
    config: Config,
}
pub struct Collector {
    shared: Arc<Shared>,
    threads: Mutex<Vec<thread::JoinHandle<()>>>,
    schema: Schema,
}

impl Collector {
    pub fn start(sink: Arc<dyn Sink>, config: Config) -> Result<Self> {
        if !(1..=32).contains(&config.consumers)
            || !(1..=10_000_000).contains(&config.queue_spans)
            || config.batch_min == 0
            || config.batch_min > config.queue_spans
            || config.batch_max < config.batch_min
            || config.flush_ms == 0
            || config.export_timeout_ms == 0
            || config.retry_max_ms < config.retry_initial_ms
        {
            return Err(Error::new(
                ErrorKind::Config,
                "invalid bounded span queue configuration",
            ));
        }
        let schema = Schema::pinned()?;
        let shared = Arc::new(Shared {
            state: Mutex::new(State {
                pending: VecDeque::new(),
                pending_spans: 0,
                outstanding: 0,
                next: 0,
                remaining: BTreeMap::new(),
                failures: Vec::new(),
                omitted: 0,
                force: false,
                stopping: false,
                workers: config.consumers,
                stats: Stats::default(),
            }),
            changed: Condvar::new(),
            work: Context::default(),
            config,
        });
        let mut threads = Vec::new();
        for i in 0..shared.config.consumers {
            let s = shared.clone();
            let sink = sink.clone();
            match thread::Builder::new()
                .name(format!("otel-export-{i}"))
                .spawn(move || worker(s, sink))
            {
                Ok(t) => threads.push(t),
                Err(_) => {
                    shared.work.cancel();
                    {
                        let mut state = shared.state.lock().unwrap();
                        state.stopping = true;
                    }
                    shared.changed.notify_all();
                    for t in threads {
                        let _ = t.join();
                    }
                    return Err(Error::new(
                        ErrorKind::Operation,
                        "cannot create exporter worker",
                    ));
                }
            }
        }
        Ok(Self {
            shared,
            threads: Mutex::new(threads),
            schema,
        })
    }

    pub fn consume(&self, ctx: &Context, inputs: &[Vec<u8>]) -> Result<()> {
        let spans = self.schema.admit(inputs)?;
        let n = spans.len();
        if n == 0 {
            return Ok(());
        }
        let s = &self.shared;
        let mut state = s.state.lock().unwrap();
        if n > s.config.queue_spans {
            state.stats.rejected_spans += n as u64;
            return Err(Error::new(
                ErrorKind::Admission,
                "request exceeds queue capacity",
            ));
        }
        loop {
            if let Err(err) = ctx.check() {
                state.stats.rejected_spans += n as u64;
                return Err(err);
            }
            if state.stopping {
                state.stats.rejected_spans += n as u64;
                return Err(Error::new(ErrorKind::Operation, "collector is stopped"));
            }
            if state.outstanding + n <= s.config.queue_spans {
                break;
            }
            if !s.config.block_on_overflow {
                state.stats.rejected_spans += n as u64;
                return Err(Error::new(ErrorKind::Budget, "sending queue is full"));
            }
            state = s
                .changed
                .wait_timeout(state, Duration::from_millis(10))
                .unwrap()
                .0;
        }
        let seq = state.next;
        state.next += 1;
        state.outstanding += n;
        state.pending_spans += n;
        state.remaining.insert(seq, n);
        state.pending.push_back(Part {
            seq,
            spans: spans.into(),
            at: Instant::now(),
        });
        state.stats.accepted_spans += n as u64;
        s.changed.notify_all();
        Ok(())
    }

    pub fn stats(&self) -> Stats {
        let state = self.shared.state.lock().unwrap();
        let mut stats = state.stats.clone();
        stats.queued_spans = state.outstanding;
        stats
    }

    pub fn flush(&self, ctx: &Context) -> Result<()> {
        let s = &self.shared;
        let mut state = s.state.lock().unwrap();
        let target = state.next;
        state.force = true;
        s.changed.notify_all();
        while state
            .remaining
            .first_key_value()
            .is_some_and(|(seq, _)| *seq < target)
        {
            ctx.check()?;
            state = s
                .changed
                .wait_timeout(state, Duration::from_millis(10))
                .unwrap()
                .0;
        }
        let failures = std::mem::take(&mut state.failures);
        let omitted = std::mem::take(&mut state.omitted);
        if failures.is_empty() && omitted == 0 {
            Ok(())
        } else {
            Err(Error::new(
                failures.first().map_or(ErrorKind::Operation, |e| e.kind),
                format!(
                    "{} export failures, {omitted} further failures omitted; first: {}",
                    failures.len(),
                    failures
                        .first()
                        .map_or("unavailable".into(), ToString::to_string)
                ),
            ))
        }
    }

    /// Deadline expiry cancels outstanding native transactions. Workers remain
    /// owned by this handle; a later shutdown (or Drop) joins them before close.
    pub fn shutdown(&self, ctx: &Context) -> Result<()> {
        let s = &self.shared;
        {
            let mut state = s.state.lock().unwrap();
            state.stopping = true;
            state.force = true;
            s.changed.notify_all();
        }
        let result = self.flush(ctx);
        if let Err(err) = ctx.check() {
            s.work.cancel();
            s.changed.notify_all();
            return Err(err);
        }
        let mut state = s.state.lock().unwrap();
        while state.workers > 0 {
            if let Err(err) = ctx.check() {
                s.work.cancel();
                s.changed.notify_all();
                return Err(err);
            }
            state = s
                .changed
                .wait_timeout(state, Duration::from_millis(10))
                .unwrap()
                .0;
        }
        drop(state);
        for t in self.threads.lock().unwrap().drain(..) {
            if t.join().is_err() {
                return Err(Error::new(ErrorKind::Operation, "export worker panicked"));
            }
        }
        result
    }
}
impl Drop for Collector {
    fn drop(&mut self) {
        self.shared.work.cancel();
        {
            let mut state = self.shared.state.lock().unwrap();
            state.stopping = true;
            state.force = true;
        }
        self.shared.changed.notify_all();
        for t in self.threads.lock().unwrap().drain(..) {
            let _ = t.join();
        }
    }
}

fn worker(s: Arc<Shared>, sink: Arc<dyn Sink>) {
    loop {
        let mut state = s.state.lock().unwrap();
        let mut spans = Vec::new();
        let mut owners = Vec::new();
        let mut parts = BTreeMap::<u64, usize>::new();
        loop {
            if state.pending_spans == 0 && state.stopping {
                state.workers -= 1;
                s.changed.notify_all();
                return;
            }
            let expired = state
                .pending
                .front()
                .is_some_and(|p| p.at.elapsed() >= Duration::from_millis(s.config.flush_ms));
            if state.pending_spans > 0
                && (state.pending_spans >= s.config.batch_min
                    || state.force
                    || state.stopping
                    || expired)
            {
                break;
            }
            state = s
                .changed
                .wait_timeout(state, Duration::from_millis(s.config.flush_ms.min(10)))
                .unwrap()
                .0;
        }
        while spans.len() < s.config.batch_max {
            let Some(part) = state.pending.front_mut() else {
                break;
            };
            while spans.len() < s.config.batch_max {
                let Some(span) = part.spans.pop_front() else {
                    break;
                };
                spans.push(span);
                owners.push(part.seq);
                *parts.entry(part.seq).or_default() += 1;
            }
            if part.spans.is_empty() {
                state.pending.pop_front();
            }
        }
        state.pending_spans -= spans.len();
        if state.pending_spans == 0 {
            state.force = false;
        }
        drop(state);
        let (stats, outcome) = export_with_retry(&s, sink.as_ref(), &spans);
        if outcome.as_ref().is_err_and(|e| e.permanent()) && parts.len() > 1 {
            for (seq, count) in parts {
                let request = spans
                    .iter()
                    .zip(&owners)
                    .filter(|(_, owner)| **owner == seq)
                    .map(|(span, _)| span.clone())
                    .collect::<Vec<_>>();
                let (stats, result) = export_with_retry(&s, sink.as_ref(), &request);
                complete(&s, BTreeMap::from([(seq, count)]), count, stats, result);
            }
        } else {
            complete(&s, parts, spans.len(), stats, outcome);
        }
    }
}

fn export_with_retry(s: &Shared, sink: &dyn Sink, spans: &[Vec<u8>]) -> (WriteStats, Result<()>) {
    let started = Instant::now();
    let mut delay = s.config.retry_initial_ms;
    loop {
        if let Err(err) = s.work.check() {
            break (WriteStats::default(), Err(err));
        }
        let mut attempt = s.work.clone();
        attempt.deadline = Some(Instant::now() + Duration::from_millis(s.config.export_timeout_ms));
        let (stats, result) = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            sink.export(&attempt, spans)
        }))
        .unwrap_or_else(|_| {
            (
                WriteStats::default(),
                Err(Error::new(ErrorKind::Operation, "sink panicked")),
            )
        });
        match result {
            Ok(()) => break (stats, Ok(())),
            Err(err) => {
                if err.permanent()
                    || s.config.retry_initial_ms == 0
                    || (s.config.retry_elapsed_ms > 0
                        && started.elapsed() + Duration::from_millis(delay)
                            > Duration::from_millis(s.config.retry_elapsed_ms))
                {
                    break (stats, Err(err));
                }
                {
                    s.state.lock().unwrap().stats.retries += 1;
                }
                let until = Instant::now() + Duration::from_millis(delay);
                while Instant::now() < until && s.work.check().is_ok() {
                    thread::sleep(Duration::from_millis(1));
                }
                delay = delay.saturating_mul(2).min(s.config.retry_max_ms);
            }
        }
    }
}

fn complete(
    s: &Shared,
    parts: BTreeMap<u64, usize>,
    count: usize,
    stats: WriteStats,
    outcome: Result<()>,
) {
    let mut state = s.state.lock().unwrap();
    state.outstanding -= count;
    state.stats.stored_spans += stats.committed_spans;
    for (seq, count) in parts {
        let remaining = state.remaining.get_mut(&seq).unwrap();
        *remaining -= count;
        if *remaining == 0 {
            state.remaining.remove(&seq);
        }
    }
    if let Err(err) = outcome {
        state.stats.failed_spans += count as u64;
        if state.failures.len() < 64 {
            state.failures.push(err);
        } else {
            state.omitted += 1;
        }
    } else {
        state.stats.exported_spans += count as u64;
    }
    s.changed.notify_all();
}
