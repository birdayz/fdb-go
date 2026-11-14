package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/collector"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func parseTestJob(t *testing.T, j map[string]any) *job {
	t.Helper()
	out, err := parsed(encode(t, j), "/job.json")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCollectorConfigIsExplicit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, c := range []struct {
		name     string
		workload string
		workers  int
		bound    int
	}{
		{"point1", point1, 1, 1},
		{"point2", point2, 1, 8},
		{"two consumers, three workers", strings.Replace(point1, `"consumers":1`, `"consumers":2`, 1), 3, 6},
	} {
		raw := f.job("ingest", "go_pure", c.workload)
		raw["writer_workers"] = c.workers
		j := parseTestJob(t, raw)
		cfg := collectorConfig(j)
		w := j.Workload
		e := cfg.Exporter
		if e.MaxInFlightTransactions != c.bound || j.globalWriteBound() != c.bound {
			t.Fatalf("%s: write slots %d, want consumers*writer_workers %d (never the default 32)", c.name, e.MaxInFlightTransactions, c.bound)
		}
		if e.BatchSize != 64 || e.Workers != c.workers || e.RecordCompression != "zlib-1" || e.BulkIndexConflicts || e.EnableMessageDeltas || e.EnableMessageFullText ||
			e.TransactionTimeout != 10*time.Second || e.Run != "control_test_run" || e.Tenant != 0 || e.ClusterFile != f.cluster {
			t.Fatalf("%s: exporter %+v", c.name, e)
		}
		q := cfg.SendingQueue
		if !q.Enabled || q.NumConsumers != w.Consumers || q.QueueSize != w.QueueCapacitySpans || q.BlockOnOverflow != w.BlockOnOverflow ||
			q.Batch.FlushTimeout != time.Duration(w.FlushMS)*time.Millisecond || q.Batch.MinSize != w.BatchMinSpans || q.Batch.MaxSize != w.BatchMaxSpans {
			t.Fatalf("%s: queue %+v", c.name, q)
		}
		rc := cfg.RetryOnFailure
		if cfg.Timeout != 120*time.Second || !rc.Enabled || rc.InitialInterval != 500*time.Millisecond || rc.MaxInterval != 10*time.Second || rc.MaxElapsedTime != 300*time.Second {
			t.Fatalf("%s: timeout %v retry %+v", c.name, cfg.Timeout, rc)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// The producer batch is the call shape, independent of writer and queue batches.
		if w.ProducerBatchSpans != 1 || e.BatchSize == w.ProducerBatchSpans || q.Batch.MaxSize == w.ProducerBatchSpans {
			t.Fatalf("%s: producer batch %d conflated", c.name, w.ProducerBatchSpans)
		}
	}
	if point2Job := parseTestJob(t, f.job("ingest", "go_native", point2)); point2Job.Workload.Producers != 8 || point2Job.Workload.Consumers != 8 || point2Job.Workload.BatchMaxSpans != 256 {
		t.Fatal("point2 is not the sealed 8/8/256 pilot workload")
	}
	// writer_direct's bound is its producers' per-call workers; it has no queue.
	raw := f.job("ingest", "go_pure", direct)
	raw["writer_workers"] = 3
	if got := parseTestJob(t, raw).globalWriteBound(); got != 6 {
		t.Fatalf("direct bound %d, want producers*writer_workers 6", got)
	}
}

// The Go markers follow the frozen counter policy for every Go arm and kind.
func TestGoCounterPolicy(t *testing.T) {
	t.Parallel()
	doc := schemaDoc(t)
	var unexposed []*regexp.Regexp
	for _, p := range jsonAt(doc, "$defs", "go_unexposed_counter", "allOf", 1, "properties", "reason", "allOf").([]any) {
		unexposed = append(unexposed, regexp.MustCompile(p.(map[string]any)["pattern"].(string)))
	}
	inapplicable := regexp.MustCompile(jsonAt(doc, "$defs", "go_inapplicable_counter", "allOf", 1, "properties", "reason", "pattern").(string))
	matchesAll := func(s string) bool {
		for _, re := range unexposed {
			if !re.MatchString(s) {
				return false
			}
		}
		return true
	}
	f := newFixture(t)
	for _, arm := range []string{"go_pure", "go_native"} {
		for _, workload := range []string{point2, direct} {
			j := parseTestJob(t, f.job("ingest", arm, workload))
			r := newIngestReport(j, reportCommon{})
			for name, m := range map[string]counter{"acked_span_operations": r.AckedSpanOperations, "committed_batches": r.CommittedBatches, "outer_retries": r.OuterRetries} {
				if m.Availability != "unavailable" || !matchesAll(m.Reason) {
					t.Fatalf("%s %s: %s = %+v", arm, workload, name, m)
				}
			}
			if arm == "go_pure" && (r.NativeRetries.Availability != "unavailable" || !inapplicable.MatchString(r.NativeRetries.Reason)) {
				t.Fatalf("go_pure native_retries %+v", r.NativeRetries)
			}
			if arm == "go_native" && (r.NativeRetries.Availability != "unavailable" || !matchesAll(r.NativeRetries.Reason)) {
				t.Fatalf("go_native native_retries %+v", r.NativeRetries)
			}
			if j.collector() != (r.ExportAttempts.Availability == "available" && r.QueueRetries.Availability == "available") {
				t.Fatalf("%s %s: queue counters %+v %+v", arm, workload, r.ExportAttempts, r.QueueRetries)
			}
			// Optional diagnostics are unavailable from the start; no queue exists and no
			// mapping is proved before ingest opens its client.
			if r.TransactionSpans != diagnosticOff() || r.Queue != nil || r.NativeMappingRule != nil {
				t.Fatalf("%s %s: diagnostics %+v %+v %v", arm, workload, r.TransactionSpans, r.Queue, r.NativeMappingRule)
			}
		}
	}
}

// scriptedSink stands in for the Exporter behind the real Collector; export decides
// each call's outcome.
type scriptedSink struct {
	mu     sync.Mutex
	calls  int
	export func(ctx context.Context, call int, spans []*tracepb.ResourceSpans) error
	// done, when set, is closed by Shutdown, which the Collector calls only after every
	// worker and active call joined.
	done     chan struct{}
	shutdown sync.Once
}

func spanCount(spans []*tracepb.ResourceSpans) int {
	n := 0
	for _, rs := range spans {
		for _, ss := range rs.ScopeSpans {
			n += len(ss.Spans)
		}
	}
	return n
}

func (s *scriptedSink) ExportResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans) (otel.ExportStats, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if err := s.export(ctx, call, spans); err != nil {
		// Partial progress the Collector drops on failure: no ACK total can be derived.
		return otel.ExportStats{Spans: int64(spanCount(spans) / 2)}, err
	}
	return otel.ExportStats{Spans: int64(spanCount(spans)), Batches: 1}, nil
}

func (s *scriptedSink) Shutdown(context.Context) error {
	if s.done != nil {
		s.shutdown.Do(func() { close(s.done) })
	}
	return nil
}

// heldExports returns a release channel for exports a test holds and a done channel for
// the sink. Its cleanup, registered before any assertion, releases them once and waits,
// bounded, until the Collector joined and shut the sink down.
func heldExports(t *testing.T, after func()) (release, done chan struct{}) {
	release, done = make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the released Collector did not join and shut its sink down within 10s")
			return
		}
		if after != nil {
			after()
		}
	})
	return release, done
}

func (s *scriptedSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// queueCase is one Collector ingest of 8 frames through a scripted sink.
type queueCase struct {
	workload  map[string]any
	deadline  time.Duration
	joinBound time.Duration
	sink      *scriptedSink
	openErr   error
	// usage, when set, counts the window's resource samples.
	usage *int
}

func (c queueCase) run(t *testing.T) (*ingestReport, error) {
	t.Helper()
	f := newFixture(t)
	raw := f.job("ingest", "go_pure", point1)
	raw["corpus_config"].(map[string]any)["count"], raw["expected_frames"], raw["row_budget"] = 8, 8, 8
	w := raw["workload"].(map[string]any)
	for k, v := range c.workload {
		w[k] = v
	}
	j := parseTestJob(t, raw)
	corpus, _ := testCorpus(t, 8)
	in, err := openCorpus(corpus, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	rec := &fakeRuntime{}
	rt := f.runtime(rec)
	rt.joinBound = c.joinBound
	if c.usage != nil {
		rt.usage = func() (syscall.Rusage, error) {
			*c.usage++
			return selfUsage()
		}
	}
	rt.sink = func(context.Context, otel.ExporterConfig) (collector.Sink, error) {
		if c.openErr != nil {
			return nil, c.openErr
		}
		return c.sink, nil
	}
	r := newIngestReport(j, newCommon(j, strings.Repeat("c", 64), encode(t, raw), ingestBoundary))
	ctx, cancel := context.WithTimeout(t.Context(), c.deadline)
	defer cancel()
	// A scripted sink opens no client, so no mapping rule precedes it.
	outcome := ingestCollector(ctx, rt, j, "", in, &layoutManifest{}, r)
	if rec.native != 0 {
		t.Fatal("a pure ingest opened the native client")
	}
	return r, outcome
}

// testCorpus writes n otel_fixture_v2 frames and returns its path and the frames.
func testCorpus(t *testing.T, n int) (string, [][]byte) {
	t.Helper()
	cfg := fixtureConfig(corpusConfig{Generator: "otel_fixture_v2", Seed: 283, Count: n, Tenants: 1, Attributes: 8, MatchEvery: 2})
	var buf bytes.Buffer
	var frames [][]byte
	for i := range n {
		_, wire, err := otel.FixtureOTLP(otel.Generate(cfg, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFrame(&buf, wire); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, wire)
	}
	p := filepath.Join(t.TempDir(), "corpus")
	writeFile(t, p, buf.Bytes())
	return p, frames
}

func reportJSON(t *testing.T, r *ingestReport) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(encode(t, r), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func value(c counter) int64 {
	if c.Value == nil {
		return -1
	}
	return *c.Value
}

// requireUnexposedACK: whatever happened, a Go report never claims ACK totals.
func requireUnexposedACK(t *testing.T, r *ingestReport) {
	t.Helper()
	if r.AckedSpanOperations.Availability != "unavailable" || r.AckedSpanOperations.Value != nil || r.CommittedBatches.Value != nil || r.OuterRetries.Value != nil {
		t.Fatalf("Go ACK-side counters %+v %+v %+v", r.AckedSpanOperations, r.CommittedBatches, r.OuterRetries)
	}
	for _, s := range r.Samples {
		if s.DirectWriteAckedOperations != nil {
			t.Fatal("a collector sample carries an ACK field")
		}
	}
}

func transient() error {
	return otel.NewError("transient", "export", "injected transient failure", nil)
}

// A transient first attempt retries to success: the queue counters are exact and the
// ACK stays unavailable even though every span was exported.
func TestCollectorRetryReportsQueueCountersNotACKs(t *testing.T) {
	t.Parallel()
	sink := &scriptedSink{export: func(_ context.Context, call int, _ []*tracepb.ResourceSpans) error {
		if call == 1 {
			return transient()
		}
		return nil
	}}
	r, err := queueCase{
		workload: map[string]any{"producers": 2, "consumers": 2, "batch_min_spans": 4, "batch_max_spans": 4, "flush_ms": 1, "retry_initial_ms": 1, "retry_max_ms": 1},
		deadline: time.Minute, joinBound: time.Minute, sink: sink,
	}.run(t)
	// Everything reconciled: only the post-ingest seal check, which needs a cluster, failed.
	if !errors.Is(err, errNoCluster) {
		t.Fatalf("ingest: %v", err)
	}
	if r.AcceptedSpans != 8 || *r.ExportedSpans != 8 || r.FailedSpans != 0 || r.QueuedSpansAtExit != 0 || r.RejectedSpans != 0 || r.Frames != 8 || r.ReceivedSpans != 8 {
		t.Fatalf("populations %+v", r)
	}
	if value(r.ExportAttempts) != int64(sink.callCount()) || value(r.QueueRetries) != 1 || !*r.CollectorJoined {
		t.Fatalf("queue counters %+v %+v (sink calls %d)", r.ExportAttempts, r.QueueRetries, sink.callCount())
	}
	// The joined Collector names each optional queue diagnostic unavailable; a scripted
	// sink opened no native client, so no mapping rule is claimed.
	if r.Queue == nil || *r.Queue != *queueDiagnosticsOff() || r.TransactionSpans != diagnosticOff() || r.NativeMappingRule != nil {
		t.Fatalf("diagnostics %+v %+v %v", r.Queue, r.TransactionSpans, r.NativeMappingRule)
	}
	requireUnexposedACK(t, r)
	if len(r.Samples) != 8 || r.IngestResources == nil || r.IngestWallNS <= 0 || r.DrainNS <= 0 {
		t.Fatalf("samples %d, resources %v, wall %d, drain %d", len(r.Samples), r.IngestResources, r.IngestWallNS, r.DrainNS)
	}
	checkIngestShape(t, reportJSON(t, r))
}

// A permanent failure of a merged batch replays its parts: only the poisoned request
// fails, the rest export, and the report still claims no ACK operations.
func TestCollectorMergedFailureIsolationKeepsACKUnavailable(t *testing.T) {
	t.Parallel()
	var poison []byte
	sink := &scriptedSink{}
	sink.export = func(_ context.Context, _ int, spans []*tracepb.ResourceSpans) error {
		for _, rs := range spans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					if bytes.Equal(sp.SpanId, poison) {
						return otel.NewError("operation", "export", "injected permanent failure", nil)
					}
				}
			}
		}
		return nil
	}
	c := queueCase{
		// Nothing dispatches before Flush: all eight requests merge into one batch.
		workload: map[string]any{"batch_min_spans": 64, "batch_max_spans": 256, "flush_ms": 3600000},
		deadline: time.Minute, joinBound: time.Minute, sink: sink,
	}
	_, frames := testCorpus(t, 8)
	rs, err := decodeFrame(frames[3])
	if err != nil {
		t.Fatal(err)
	}
	poison = rs.ScopeSpans[0].Spans[0].SpanId
	r, err := c.run(t)
	if err == nil || errors.Is(err, errNoCluster) {
		t.Fatalf("a failed export was not reported: %v", err)
	}
	if r.AcceptedSpans != 8 || *r.ExportedSpans != 7 || r.FailedSpans != 1 || r.QueuedSpansAtExit != 0 || !*r.CollectorJoined {
		t.Fatalf("populations %+v", r)
	}
	// One merged attempt, then each of the eight parts once.
	if value(r.ExportAttempts) != 9 || value(r.QueueRetries) != 0 {
		t.Fatalf("queue counters %+v %+v", r.ExportAttempts, r.QueueRetries)
	}
	requireUnexposedACK(t, r)
	checkIngestShape(t, reportJSON(t, r))
}

// A nonblocking full queue rejects the call that does not fit; what was admitted still
// drains under the job deadline.
func TestCollectorQueueFullRejectsAndDrains(t *testing.T) {
	t.Parallel()
	sink := &scriptedSink{export: func(_ context.Context, call int, _ []*tracepb.ResourceSpans) error {
		if call == 1 {
			// The first export backs off for the retry interval, holding its span queued.
			return transient()
		}
		return nil
	}}
	r, err := queueCase{
		workload: map[string]any{
			"producers": 1, "consumers": 1, "queue_capacity_spans": 2, "batch_min_spans": 1, "batch_max_spans": 1,
			"block_on_overflow": false, "flush_ms": 1, "retry_initial_ms": 1000, "retry_max_ms": 1000,
		},
		deadline: time.Minute, joinBound: time.Minute, sink: sink,
	}.run(t)
	var full *collector.QueueFullError
	if !errors.As(err, &full) {
		t.Fatalf("ingest: %v", err)
	}
	if r.RejectedSpans < 1 || r.AcceptedSpans+r.RejectedSpans != r.ReceivedSpans || *r.ExportedSpans != r.AcceptedSpans || r.FailedSpans != 0 || r.QueuedSpansAtExit != 0 || !*r.CollectorJoined {
		t.Fatalf("populations %+v", r)
	}
	requireUnexposedACK(t, r)
	checkIngestShape(t, reportJSON(t, r))
}

// At the job deadline a blocked admission fails, the expired Shutdown cancels the
// in-flight export, and the join takes no further export time.
func TestCollectorDeadlineCancelsThenJoins(t *testing.T) {
	t.Parallel()
	sink := &scriptedSink{export: func(ctx context.Context, _ int, _ []*tracepb.ResourceSpans) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	r, err := queueCase{
		workload: map[string]any{
			"producers": 1, "consumers": 1, "queue_capacity_spans": 1, "batch_min_spans": 1, "batch_max_spans": 1,
			"block_on_overflow": true, "flush_ms": 1, "export_timeout_ms": 3600000,
		},
		deadline: 500 * time.Millisecond, joinBound: time.Minute, sink: sink,
	}.run(t)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ingest: %v", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("settling after the deadline took %v: an export grace was granted", took)
	}
	if !*r.CollectorJoined || r.AcceptedSpans != 1 || r.FailedSpans != 1 || *r.ExportedSpans != 0 || r.QueuedSpansAtExit != 0 || r.RejectedSpans != r.ReceivedSpans-1 {
		t.Fatalf("populations after the join %+v", r)
	}
	if value(r.ExportAttempts) != 1 || value(r.QueueRetries) != 0 {
		t.Fatalf("queue counters %+v %+v", r.ExportAttempts, r.QueueRetries)
	}
	checkIngestShape(t, reportJSON(t, r))
}

// A worker that ignores cancellation cannot be joined within the bound: the outcome is
// unsettled, with only producer facts, and no window end or counter is read.
func TestCollectorUnjoinedIsUnsettled(t *testing.T) {
	t.Parallel()
	release, done := heldExports(t, nil)
	sink := &scriptedSink{done: done, export: func(context.Context, int, []*tracepb.ResourceSpans) error {
		<-release
		return nil
	}}
	usage := 0
	r, err := queueCase{
		workload: map[string]any{
			"producers": 1, "consumers": 1, "queue_capacity_spans": 1, "batch_min_spans": 1, "batch_max_spans": 1,
			"block_on_overflow": true, "flush_ms": 1, "export_timeout_ms": 3600000,
		},
		deadline: 300 * time.Millisecond, joinBound: 200 * time.Millisecond, sink: sink, usage: &usage,
	}.run(t)
	var u *unsettledError
	if !errors.As(err, &u) {
		t.Fatalf("ingest: %v", err)
	}
	if u.admitted != 1 || u.received < 2 || u.frames != u.received || !errors.Is(u, context.DeadlineExceeded) {
		t.Fatalf("unsettled facts %+v", u)
	}
	if usage != 1 {
		t.Fatalf("%d resource samples, want only the window's start", usage)
	}
	if r.CollectorJoined != nil || r.Queue != nil || r.IngestWallNS != 0 || r.IngestResources != nil || len(r.Samples) != 0 {
		t.Fatalf("settled values were written for an unsettled Collector: %+v", r)
	}
}

// The untimed precheck is the Rust driver's admission check: a frame it refuses is
// never timed or submitted.
func TestPrecheckRefusesBeforeTheTimedCall(t *testing.T) {
	t.Parallel()
	_, frames := testCorpus(t, 4)
	mutate := func(edit func(*tracepb.Span)) []byte {
		rs, err := decodeFrame(frames[2])
		if err != nil {
			t.Fatal(err)
		}
		edit(rs.ScopeSpans[0].Spans[0])
		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(rs)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	for _, c := range []struct {
		name  string
		frame []byte
		calls int
	}{
		{"valid", frames[2], 4},
		{"15-byte trace ID", mutate(func(s *tracepb.Span) { s.TraceId = s.TraceId[:15] }), 2},
		{"zero trace ID", mutate(func(s *tracepb.Span) { s.TraceId = make([]byte, 16) }), 2},
		{"9-byte span ID", mutate(func(s *tracepb.Span) { s.SpanId = append(s.SpanId, 1) }), 2},
		{"zero span ID", mutate(func(s *tracepb.Span) { s.SpanId = make([]byte, 8) }), 2},
		{"start past int64", mutate(func(s *tracepb.Span) { s.StartTimeUnixNano = math.MaxInt64 + 1 }), 2},
	} {
		var buf bytes.Buffer
		for _, f := range [][]byte{frames[0], frames[1], c.frame, frames[3]} {
			if err := writeFrame(&buf, f); err != nil {
				t.Fatal(err)
			}
		}
		p := filepath.Join(t.TempDir(), "corpus")
		writeFile(t, p, buf.Bytes())
		in, err := openCorpus(p, 4)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		prod := produce(t.Context(), in, 1, 1, func(context.Context, []*tracepb.ResourceSpans) (int64, *int64, error) {
			calls++
			return 1, nil, nil
		})
		in.close()
		if calls != c.calls || len(prod.samples) != c.calls || (prod.err() == nil) != (c.calls == 4) {
			t.Fatalf("%s: %d calls, %d samples, error %v", c.name, calls, len(prod.samples), prod.err())
		}
	}
}

// A sink that cannot open fails before the window: integer zeros and no measurement.
func TestCollectorStartFailureReportsNoWindow(t *testing.T) {
	t.Parallel()
	openErr := errors.New("injected open failure")
	r, err := queueCase{deadline: time.Minute, joinBound: time.Minute, openErr: openErr}.run(t)
	if !errors.Is(err, openErr) {
		t.Fatalf("ingest: %v", err)
	}
	if r.IngestWallNS != 0 || r.AdmissionPhaseNS != 0 || r.DrainNS != 0 || r.Frames != 0 || len(r.Samples) != 0 || r.IngestResources != nil || *r.IngestResourcesUnavailableReason != notStarted {
		t.Fatalf("an unstarted report %+v", r)
	}
	msg := err.Error()
	r.Status, r.Error = "error", &msg
	checkIngestShape(t, reportJSON(t, r))
}

func TestLatencyNearestRank(t *testing.T) {
	t.Parallel()
	var samples []sample
	for i := range 100 {
		samples = append(samples, sample{NS: int64(100 - i)})
	}
	l := latencyOf(samples)
	if l.Population != 100 || *l.MinNS != 1 || *l.P50NS != 50 || *l.P95NS != 95 || *l.P99NS != 99 || *l.MaxNS != 100 {
		t.Fatalf("latency %+v", l)
	}
	if empty := latencyOf(nil); empty.Population != 0 || empty.MinNS != nil {
		t.Fatalf("empty latency %+v", empty)
	}
}
