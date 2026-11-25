package collector

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/layers/otel"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// benchSink wraps the real exporter: it sums ExportStats over successful
// calls, times every call, and credits exported spans to their conversations.
type benchSink struct {
	exp   *otel.Exporter
	track *benchTracker

	mu       sync.Mutex
	stats    otel.ExportStats
	calls    int64
	failed   int64
	spans    int64
	messages int64
	rawBytes int64
	latency  []int64 // calls completed since the last window cut
}

func (s *benchSink) ExportResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans) (otel.ExportStats, error) {
	var n, msgs, raw int64
	convs := map[int]int{}
	for _, rs := range spans {
		raw += int64(proto.Size(rs))
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if sp == nil {
					continue
				}
				n++
				msgs += benchSpanMessages(sp)
				c, ok := benchConversationOf(sp.TraceId)
				if !ok {
					c = -1
				}
				convs[c]++
			}
		}
	}
	t0 := time.Now()
	st, err := s.exp.ExportResourceSpans(ctx, spans)
	d := time.Since(t0).Nanoseconds()
	s.mu.Lock()
	s.calls++
	s.latency = append(s.latency, d)
	if err != nil {
		s.failed++
	} else {
		addExportStats(&s.stats, st)
		s.spans += n
		s.messages += msgs
		s.rawBytes += raw
	}
	s.mu.Unlock()
	if err == nil {
		s.track.spansExported(convs)
	}
	return st, err
}

func (s *benchSink) Shutdown(ctx context.Context) error { return s.exp.Shutdown(ctx) }

func (s *benchSink) snapshot() (otel.ExportStats, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats, s.failed
}

// benchSpanMessages counts the message elements of a span's content attributes.
func benchSpanMessages(sp *tracepb.Span) int64 {
	var n int64
	for _, kv := range sp.Attributes {
		switch kv.Key {
		case benchInputKey, benchOutputKey, "gen_ai.system_instructions", "gen_ai.tool.definitions":
			n += int64(len(kv.Value.GetArrayValue().GetValues()))
		}
	}
	return n
}

func addExportStats(dst *otel.ExportStats, s otel.ExportStats) {
	dst.ReceivedSpans += s.ReceivedSpans
	dst.DuplicateSpans += s.DuplicateSpans
	dst.Spans += s.Spans
	dst.Batches += s.Batches
	dst.LogicalBytes += s.LogicalBytes
	dst.NormalizedSpans += s.NormalizedSpans
	dst.UnindexedAttributes += s.UnindexedAttributes
	d, m := &dst.MessageDeltas, s.MessageDeltas
	d.TotalMessages += m.TotalMessages
	d.UniqueMessages += m.UniqueMessages
	d.ReusedMessages += m.ReusedMessages
	d.RawMessageBytes += m.RawMessageBytes
	d.StoredDeltaBytes += m.StoredDeltaBytes
	d.SavedBytes += m.SavedBytes
	dst.Messages.Add(s.Messages)
}

// addLogStats sums every counter; EventIDs are not retained.
func addLogStats(dst *otel.LogExportStats, s otel.LogExportStats) {
	dst.ReceivedLogs += s.ReceivedLogs
	dst.DuplicateLogs += s.DuplicateLogs
	dst.GeneratedIDs += s.GeneratedIDs
	dst.Logs += s.Logs
	dst.Batches += s.Batches
	dst.Replaced += s.Replaced
	dst.Correlated += s.Correlated
	dst.LogicalBytes += s.LogicalBytes
	dst.StoredBytes += s.StoredBytes
	dst.UnindexedAttributes += s.UnindexedAttributes
}

// ---------------------------------------------------------------------------
// Commit watermark
// ---------------------------------------------------------------------------

type benchCommit struct {
	spans uint8 // turns <= maxBenchTurns < 256
	log   bool
}

// benchTracker stores two bytes per conversation above the commit watermark,
// so memory follows the in-flight window rather than corpus size.
type benchTracker struct {
	turns   int
	claimed *atomic.Int64 // conversations handed to producers

	mu        sync.Mutex
	base      int // lowest uncommitted conversation; the watermark is base-1
	window    []benchCommit
	peak      int
	anomalies int64
	watermark atomic.Int64
}

func newBenchTracker(turns int, claimed *atomic.Int64) *benchTracker {
	t := &benchTracker{turns: turns, claimed: claimed}
	t.watermark.Store(-1)
	return t
}

// slot returns c's entry, or nil for a conversation never claimed or already
// committed; callers hold t.mu.
func (t *benchTracker) slot(c int) *benchCommit {
	if c < t.base || int64(c) >= t.claimed.Load() {
		t.anomalies++
		return nil
	}
	for len(t.window) <= c-t.base {
		t.window = append(t.window, benchCommit{})
	}
	t.peak = max(t.peak, len(t.window))
	return &t.window[c-t.base]
}

// spansExported credits successfully exported spans per conversation.
func (t *benchTracker) spansExported(counts map[int]int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for c, n := range counts {
		s := t.slot(c)
		if s == nil {
			continue
		}
		if int(s.spans)+n > t.turns {
			t.anomalies++
			continue
		}
		s.spans += uint8(n)
	}
	t.advance()
}

// logsWritten marks conversations whose log has been written.
func (t *benchTracker) logsWritten(convs []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range convs {
		s := t.slot(c)
		if s == nil {
			continue
		}
		if s.log {
			t.anomalies++
		}
		s.log = true
	}
	t.advance()
}

func (t *benchTracker) advance() {
	i := 0
	for i < len(t.window) && int(t.window[i].spans) == t.turns && t.window[i].log {
		i++
	}
	t.window = t.window[i:]
	t.base += i
	t.watermark.Store(int64(t.base) - 1)
}

// Watermark returns the highest W with every conversation <= W committed.
func (t *benchTracker) Watermark() int64 { return t.watermark.Load() }

func (t *benchTracker) state() (tracked, peak int, anomalies int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.window), t.peak, t.anomalies
}

// benchRun is the state that lives for the whole ingest. It holds no
// per-conversation request or log: the Collector queue is the only buffer.
type benchRun struct {
	cfg     CollectorBenchConfig
	gen     *benchGen
	x       *TracesExporter
	sink    *benchSink
	track   *benchTracker
	logs    *benchLogWriter
	queries []benchQuery
	load    *benchLoad
	claimed atomic.Int64
	raw     atomic.Int64
	total   int
	start   time.Time

	seriesMu sync.Mutex
	phase    string
	prevEnd  time.Duration
	prev     benchCounters
	prevCPU  benchCPUSample
	windows  []CollectorBenchWindow
	memory   CollectorBenchMemory
}

// benchRuntime permits deterministic admission and sampling fixtures without
// changing the public workload configuration or historical receipt schema.
type benchRuntime struct {
	now             func() time.Time
	beforeAdmission func(context.Context, *benchRun, int) error
}

func (r *benchRun) now() time.Time {
	if r.cfg.runtime != nil && r.cfg.runtime.now != nil {
		return r.cfg.runtime.now()
	}
	return time.Now()
}

func (r *benchRun) nextConversation(limit int, deadline time.Time) (int, bool) {
	if !deadline.IsZero() && !r.now().Before(deadline) {
		return 0, false
	}
	c := int(r.claimed.Add(1) - 1)
	return c, c < limit
}

// ingest runs phase 1 and returns its first error.
func (r *benchRun) ingest(ctx context.Context, rep *CollectorBenchReport) error {
	cfg := r.cfg
	ictx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.prevCPU = readBenchCPU()
	r.start = r.now()
	if start, ok := ctx.Value(benchmarkIngestStartKey{}).(func()); ok {
		start()
	}
	limit := cfg.Conversations
	var deadline time.Time
	if cfg.Duration > 0 {
		limit, deadline = maxBenchConversations, r.start.Add(cfg.Duration)
	}

	sampleStop, sampleDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampleDone)
		t := time.NewTicker(cfg.ReportInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.cut(false)
			case <-sampleStop:
				return
			}
		}
	}()
	logDone := make(chan error, 1)
	go func() {
		err := r.logs.run(ictx)
		if err != nil {
			cancel(err)
		}
		logDone <- err
	}()
	queryStop := make(chan struct{})
	var queryWG sync.WaitGroup
	var next atomic.Int64
	for range cfg.QueryWorkers {
		queryWG.Go(func() { r.queryWorker(ictx, queryStop, &next) })
	}

	produceErrs := r.runProducers(ictx, cancel, limit, deadline)
	close(queryStop)
	queryWG.Wait()
	rep.UnderLoad.WallNs = r.now().Sub(r.start).Nanoseconds()
	r.total = min(int(r.claimed.Load()), limit)
	r.cut(true)
	r.seriesMu.Lock()
	r.phase = benchPhaseDrain
	r.seriesMu.Unlock()

	flushErr := r.x.Flush(ictx)
	close(r.logs.in)
	logErr := <-logDone
	rep.Ingest.IngestWallNs = r.now().Sub(r.start).Nanoseconds()
	close(sampleStop)
	<-sampleDone
	r.cut(true)

	// One failure cancels the others, so report all of them.
	var errs []error
	if len(produceErrs) > 0 {
		errs = append(errs, componentErrorf("collector bench ingest: %d rejected requests: %w", len(produceErrs), errors.Join(produceErrs[:min(len(produceErrs), 4)]...)))
	}
	if logErr != nil {
		errs = append(errs, logErr)
	}
	if flushErr != nil {
		errs = append(errs, componentErrorf("collector bench flush: %w", flushErr))
	}
	return errors.Join(append(errs, ctx.Err())...)
}

func (r *benchRun) runProducers(ctx context.Context, cancel context.CancelCauseFunc, limit int, deadline time.Time) []error {
	cfg := r.cfg
	var producers sync.WaitGroup
	var errMu sync.Mutex
	var produceErrs []error
	for range cfg.Producers {
		producers.Go(func() {
			for ctx.Err() == nil {
				c, ok := r.nextConversation(limit, deadline)
				if !ok {
					return
				}
				if cfg.runtime != nil && cfg.runtime.beforeAdmission != nil {
					if err := cfg.runtime.beforeAdmission(ctx, r, c); err != nil {
						errMu.Lock()
						produceErrs = append(produceErrs, err)
						errMu.Unlock()
						cancel(err)
						return
					}
				}
				req := r.gen.request(c)
				r.raw.Add(int64(proto.Size(req)))
				if err := r.x.ConsumeTraces(ctx, []*tracepb.ResourceSpans{req}); err != nil {
					errMu.Lock()
					produceErrs = append(produceErrs, componentErrorf("conversation %d: %w", c, err))
					errMu.Unlock()
					cancel(err)
					return
				}
				select {
				case r.logs.in <- c:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	producers.Wait()
	return produceErrs
}

// report fills the ingest sections of the report from the run state.
func (r *benchRun) report(rep *CollectorBenchReport) {
	cfg := r.cfg
	rep.Corpus = benchCorpusStats(cfg.TurnsPerConversation, r.total, r.raw.Load())
	in := &rep.Ingest
	if secs := float64(in.IngestWallNs) / 1e9; secs > 0 {
		in.SpansPerSec = float64(rep.Corpus.Spans) / secs
		in.MessagesPerSec = float64(rep.Corpus.TotalMessageElements) / secs
		in.RawBytesPerSec = float64(rep.Corpus.RawInputBytes) / secs
	}
	in.Collector = r.x.Stats()
	in.Export, in.FailedExportCalls = r.sink.snapshot()
	r.logs.mu.Lock()
	in.Logs, in.LogWallNs, in.LogCalls, in.FailedLogCalls = r.logs.stats, r.logs.wallNs, r.logs.calls, r.logs.failed
	r.logs.mu.Unlock()
	r.seriesMu.Lock()
	rep.Series = slices.Clone(r.windows)
	rep.Memory = r.memory
	r.seriesMu.Unlock()
	rep.Rates = benchRates(rep.Series, cfg.Warmup)
	tracked, peak, anomalies := r.track.state()
	rep.Watermark = CollectorBenchWatermark{Conversations: r.total, Watermark: r.track.Watermark(), Tracked: tracked, PeakTracked: peak, Anomalies: anomalies}
	rep.UnderLoad.Queries = r.load.results(time.Duration(rep.UnderLoad.WallNs))
}

// benchCounters are cumulative ingest counters; windows are their deltas.
type benchCounters struct {
	spans, messages, rawBytes, logs, calls, failed, retries, queries int64
}

// counters reads the cumulative counters and takes the export-call latencies
// recorded since the previous call, under one sink lock so they agree.
func (r *benchRun) counters() (benchCounters, Stats, []int64) {
	st := r.x.Stats()
	var c benchCounters
	r.sink.mu.Lock()
	c.spans, c.messages, c.rawBytes, c.calls, c.failed = r.sink.spans, r.sink.messages, r.sink.rawBytes, r.sink.calls, r.sink.failed
	latency := r.sink.latency
	r.sink.latency = nil
	r.sink.mu.Unlock()
	r.logs.mu.Lock()
	c.logs = r.logs.stats.Logs
	r.logs.mu.Unlock()
	c.retries, c.queries = st.Retries, r.load.done.Load()
	return c, st, latency
}

// cut closes the current window in the run's current phase. Windows under a
// millisecond are merged into the next one, except the last of a phase.
func (r *benchRun) cut(final bool) {
	r.seriesMu.Lock()
	defer r.seriesMu.Unlock()
	now := r.now().Sub(r.start)
	if now <= r.prevEnd || !final && now-r.prevEnd < time.Millisecond {
		return
	}
	cur, st, latency := r.counters()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p := r.prev
	w := CollectorBenchWindow{
		Phase: r.phase, StartNs: r.prevEnd.Nanoseconds(), EndNs: now.Nanoseconds(),
		ExportedSpans: cur.spans - p.spans, Messages: cur.messages - p.messages, RawInputBytes: cur.rawBytes - p.rawBytes,
		Logs: cur.logs - p.logs, ExportCalls: cur.calls - p.calls, FailedCalls: cur.failed - p.failed,
		Retries: cur.retries - p.retries, Queries: cur.queries - p.queries,
		ExportLatency: benchHistogram(latency), QueuedSpans: st.QueuedSpans, Watermark: r.track.Watermark(),
		SysBytes: ms.Sys, HeapInuse: ms.HeapInuse,
	}
	w.SpansPerSec = float64(w.ExportedSpans) / (now - r.prevEnd).Seconds()
	w.CPU, r.prevCPU = benchCPUWindow(r.prevCPU, w.EndNs-w.StartNs, w.ExportedSpans)
	r.memory.PeakSysBytes = max(r.memory.PeakSysBytes, ms.Sys)
	r.memory.PeakHeapInuseBytes = max(r.memory.PeakHeapInuseBytes, ms.HeapInuse)
	r.windows = append(r.windows, w)
	r.prev, r.prevEnd = cur, now
}

// benchRates sums every window for the whole run, and load windows starting
// at or after warmup for the steady state.
func benchRates(windows []CollectorBenchWindow, warmup time.Duration) CollectorBenchRates {
	rate := func(keep func(CollectorBenchWindow) bool) CollectorBenchRate {
		var out CollectorBenchRate
		var msgs, raw, logs int64
		for _, w := range windows {
			if !keep(w) {
				continue
			}
			out.Windows++
			out.WallNs += w.EndNs - w.StartNs
			out.Spans += w.ExportedSpans
			msgs += w.Messages
			raw += w.RawInputBytes
			logs += w.Logs
			out.CPU.add(w.CPU)
		}
		out.CPU.derive(out.WallNs, out.Spans)
		if secs := float64(out.WallNs) / 1e9; secs > 0 {
			out.SpansPerSec = float64(out.Spans) / secs
			out.MessagesPerSec = float64(msgs) / secs
			out.RawBytesPerSec = float64(raw) / secs
			out.LogsPerSec = float64(logs) / secs
		}
		return out
	}
	return CollectorBenchRates{
		WholeRun: rate(func(CollectorBenchWindow) bool { return true }),
		SteadyState: rate(func(w CollectorBenchWindow) bool {
			return w.Phase == benchPhaseLoad && w.StartNs >= warmup.Nanoseconds()
		}),
	}
}

// benchLogWriter exports produced conversations' logs in batches and marks
// them written in the tracker. Its channel bounds the backlog.
type benchLogWriter struct {
	gen   *benchGen
	exp   *otel.Exporter
	track *benchTracker
	in    chan int

	mu     sync.Mutex
	stats  otel.LogExportStats
	calls  int64
	failed int64
	wallNs int64
}

// run batches until in is closed, flushing every benchLogBatch logs or
// benchLogFlushInterval.
func (w *benchLogWriter) run(ctx context.Context) error {
	tick := time.NewTicker(benchLogFlushInterval)
	defer tick.Stop()
	batch := make([]int, 0, benchLogBatch)
	for {
		select {
		case c, ok := <-w.in:
			if !ok {
				return w.flush(ctx, batch)
			}
			if batch = append(batch, c); len(batch) < benchLogBatch {
				continue
			}
		case <-tick.C:
		}
		if err := w.flush(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
	}
}

func (w *benchLogWriter) flush(ctx context.Context, batch []int) error {
	if len(batch) == 0 {
		return nil
	}
	records := make([]otel.LogRecord, len(batch))
	for i, c := range batch {
		records[i] = w.gen.log(c)
	}
	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		t0 := time.Now()
		st, err := w.exp.ExportLogs(ctx, records)
		d := time.Since(t0).Nanoseconds()
		w.mu.Lock()
		w.calls++
		w.wallNs += d
		if err != nil {
			w.failed++
		} else {
			addLogStats(&w.stats, st)
		}
		w.mu.Unlock()
		if err == nil {
			w.track.logsWritten(batch)
			return nil
		}
		if attempt == benchLogAttempts || ctx.Err() != nil {
			return componentErrorf("collector bench logs: %w", err)
		}
		t := time.NewTimer(backoff)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return componentErrorf("collector bench logs: %w", errors.Join(err, ctx.Err()))
		}
		backoff *= 2
	}
}
