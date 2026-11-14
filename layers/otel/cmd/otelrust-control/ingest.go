package main

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"syscall"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/collector"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	ingestBoundary = "span-only in-process Go ingest of one sealed otel_paired_job_v1 job on the job's client (pure-Go, or native libfdb_c 7.3.77 API 730 with the multi-version client disabled); 10000ms transaction timeout, no retry-count limit; collector_queue is producer -> TracesExporter.ConsumeTraces -> the real Collector queue/retry -> production Exporter -> SpanWriter, writer_direct is producer -> SpanWriter.Write; admission is not an ACK; the six counters are availability markers and the Go collector exposes only export attempts and queue retries; the measured path records exactly one sample per producer call (its admission or write ns and span counts) plus the final settled counters, and the optional queue and transaction diagnostics are explicit unavailable markers, not observations; a sample's accepted_spans counts spans ConsumeTraces admitted (collector_queue) or prechecked frames submitted to Write (writer_direct), never ACKs; no OTLP receiver, logs, messages, TEXT, HA or open-loop SLO claim; wall is first corpus read until the pipeline drained (Flush, Shutdown and the join of every Collector worker), not durable storage drain, stability or audit"
	directLatency  = "decode-inclusive SpanWriter.Write call per producer batch; frame shape precheck excluded"
	queueLatency   = "decode-inclusive TracesExporter.ConsumeTraces admission per producer batch; frame shape precheck excluded; not an ACK"
	ackReason      = "Go collector does not expose acknowledged write operations including replays: its stats keep only each export's final attempt (the frozen policy applies to writer_direct too)"
	batchesReason  = "Go collector does not expose committed transaction batches"
	outerReason    = "Go collector does not expose RecordLayer outer transaction retries"
	nativeReason   = "Go collector does not expose native libfdb_c transaction retries"
	pureReason     = "not applicable: go_pure uses the pure-Go client, which has no native libfdb_c retries"
	noQueue        = "not applicable: writer_direct has no collector queue"
	notStarted     = "ingest did not start, so there is no ingest-window measurement"
	diagnosticsOff = "Go collector and SpanWriter public stats do not expose this observation, and the measured path collects nothing optional; optional diagnostics are not comparison inputs"
	rusageSource   = "getrusage(RUSAGE_SELF) user/system CPU deltas from before the first corpus read to after Flush, Shutdown and the join of every Collector worker, all threads including native client threads; peak_rss_bytes is the whole-process high-water mark then, not a phase peak"
)

type sample struct {
	NS            int64 `json:"ns"`
	ReceivedSpans int64 `json:"received_spans"`
	AcceptedSpans int64 `json:"accepted_spans"`
	// Present only on writer_direct samples: that call's committed spans.
	DirectWriteAckedOperations *int64  `json:"direct_write_acked_operations,omitempty"`
	Error                      *string `json:"error"`
}

// counter is an exact observed value, or unavailable with a reason; never a stand-in zero.
type counter struct {
	Availability string `json:"availability"`
	Value        *int64 `json:"value,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func available(v int64) counter { return counter{Availability: "available", Value: &v} }

func unavailable(reason string) counter { return counter{Availability: "unavailable", Reason: reason} }

// unavailableMarker is an optional diagnostic the Go arms do not observe: exactly an
// availability and a reason, never an empty map, array or zero in its place.
type unavailableMarker struct {
	Availability string `json:"availability"`
	Reason       string `json:"reason"`
}

func diagnosticOff() unavailableMarker {
	return unavailableMarker{Availability: "unavailable", Reason: diagnosticsOff}
}

// queueDiagnostics are the Rust collector's optional queue observations, by the same
// names; a joined Go Collector reports each as unavailable.
type queueDiagnostics struct {
	MaxOutstandingSpans      unavailableMarker `json:"max_outstanding_spans"`
	BlockedRequests          unavailableMarker `json:"blocked_requests"`
	BlockedNS                unavailableMarker `json:"blocked_ns"`
	ExportCallSpans          unavailableMarker `json:"export_call_spans"`
	CompletionSamples        unavailableMarker `json:"completion_samples"`
	CompletionSamplesOmitted unavailableMarker `json:"completion_samples_omitted"`
}

func queueDiagnosticsOff() *queueDiagnostics {
	off := diagnosticOff()
	return &queueDiagnostics{off, off, off, off, off, off}
}

type latency struct {
	Population int64  `json:"population"`
	MinNS      *int64 `json:"min_ns"`
	P50NS      *int64 `json:"p50_ns"`
	P95NS      *int64 `json:"p95_ns"`
	P99NS      *int64 `json:"p99_ns"`
	MaxNS      *int64 `json:"max_ns"`
}

// latencyOf is nearest-rank percentiles over every timed call, failed calls included.
func latencyOf(samples []sample) latency {
	ns := make([]int64, 0, len(samples))
	for _, s := range samples {
		ns = append(ns, s.NS)
	}
	slices.Sort(ns)
	l := latency{Population: int64(len(ns))}
	if len(ns) == 0 {
		return l
	}
	rank := func(q float64) *int64 {
		i := min(max(int(math.Ceil(q*float64(len(ns)))), 1), len(ns))
		v := ns[i-1]
		return &v
	}
	first, last := ns[0], ns[len(ns)-1]
	l.MinNS, l.P50NS, l.P95NS, l.P99NS, l.MaxNS = &first, rank(0.50), rank(0.95), rank(0.99), &last
	return l
}

type resources struct {
	Scope             string  `json:"scope"`
	UserCPUSeconds    float64 `json:"user_cpu_seconds"`
	SystemCPUSeconds  float64 `json:"system_cpu_seconds"`
	CPUSeconds        float64 `json:"cpu_seconds"`
	PeakRSSBytes      int64   `json:"peak_rss_bytes"`
	MeasurementSource string  `json:"measurement_source"`
}

// measured is the ingest window's CPU and RSS, or why the readings are not a measurement.
func measured(start syscall.Rusage, startErr error, end syscall.Rusage, endErr error) (*resources, string) {
	if startErr != nil || endErr != nil {
		return nil, "getrusage(RUSAGE_SELF) failed"
	}
	user := float64(end.Utime.Nano()-start.Utime.Nano()) / 1e9
	system := float64(end.Stime.Nano()-start.Stime.Nano()) / 1e9
	valid := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 }
	if !valid(user) || !valid(system) || end.Maxrss <= 0 {
		return nil, "getrusage returned negative or non-finite CPU or no RSS"
	}
	return &resources{
		Scope: "first_read_through_flush", UserCPUSeconds: user, SystemCPUSeconds: system, CPUSeconds: user + system,
		PeakRSSBytes: end.Maxrss * 1024, MeasurementSource: rusageSource,
	}, ""
}

type transactionPolicy struct {
	Client                   string `json:"client"`
	TransactionTimeoutMS     int64  `json:"transaction_timeout_ms"`
	TransactionRetryLimit    int    `json:"transaction_retry_limit"`
	TransactionMaxSpans      int    `json:"transaction_max_spans"`
	TransactionEstimateBytes int    `json:"transaction_estimate_bytes"`
	TransactionActualBytes   int    `json:"transaction_actual_bytes"`
}

type ingestReport struct {
	reportCommon
	LatencyBoundary   string            `json:"latency_boundary"`
	GlobalWriteBound  int               `json:"global_write_bound"`
	TransactionPolicy transactionPolicy `json:"transaction_policy"`
	Frames            int64             `json:"frames"`
	// Spans in timed calls.
	ReceivedSpans     int64  `json:"received_spans"`
	AcceptedSpans     int64  `json:"accepted_spans"`
	RejectedSpans     int64  `json:"rejected_spans"`
	FailedSpans       int64  `json:"failed_spans"`
	QueuedSpansAtExit int64  `json:"queued_spans_at_exit"`
	ExportedSpans     *int64 `json:"exported_spans"`
	// CollectorJoined is true once a collector_queue report exists: a Collector that does
	// not join yields a diagnostic, never a report. Null for writer_direct or before Start.
	CollectorJoined     *bool   `json:"collector_joined"`
	AckedSpanOperations counter `json:"acked_span_operations"`
	CommittedBatches    counter `json:"committed_batches"`
	NativeRetries       counter `json:"native_retries"`
	OuterRetries        counter `json:"outer_retries"`
	ExportAttempts      counter `json:"export_attempts"`
	QueueRetries        counter `json:"queue_retries"`
	// Always integers: 0 when ingest never started; writer_direct has no drain (0).
	IngestWallNS     int64      `json:"ingest_wall_ns"`
	AdmissionPhaseNS int64      `json:"admission_phase_ns"`
	DrainNS          int64      `json:"drain_ns"`
	Samples          []sample   `json:"samples"`
	AdmissionLatency latency    `json:"admission_latency"`
	IngestResources  *resources `json:"ingest_resources"`
	// Present only when IngestResources is null; the report is then an error.
	IngestResourcesUnavailableReason *string           `json:"ingest_resources_unavailable_reason,omitempty"`
	TransactionSpans                 unavailableMarker `json:"transaction_spans"`
	// Null for writer_direct and for a Collector that never joined.
	Queue *queueDiagnostics `json:"queue"`
	// Set only once a go_native measured handle opened: the rule that proved its mapping.
	NativeMappingRule *string `json:"native_mapping_rule,omitempty"`
}

// claimMapping records the rule of the measured native handle; pure opens prove none.
func (r *ingestReport) claimMapping(rule string) {
	if rule != "" {
		r.NativeMappingRule = &rule
	}
}

func newIngestReport(j *job, c reportCommon) *ingestReport {
	client := "pure-Go fdbgo client"
	if j.ClientBackend == "native_c" {
		client = "libfdb_c 7.3.77, API 730, multi-version client disabled"
	}
	r := &ingestReport{
		reportCommon: c, GlobalWriteBound: j.globalWriteBound(), Samples: []sample{},
		TransactionPolicy: transactionPolicy{
			Client: client, TransactionTimeoutMS: txnTimeout.Milliseconds(), TransactionRetryLimit: -1,
			TransactionMaxSpans: 64, TransactionEstimateBytes: 300000, TransactionActualBytes: 500000,
		},
		AckedSpanOperations: unavailable(ackReason), CommittedBatches: unavailable(batchesReason),
		NativeRetries: unavailable(pureReason), OuterRetries: unavailable(outerReason),
		ExportAttempts: unavailable(noQueue), QueueRetries: unavailable(noQueue),
		TransactionSpans: diagnosticOff(),
	}
	if j.Arm == "go_native" {
		r.NativeRetries = unavailable(nativeReason)
	}
	r.LatencyBoundary = directLatency
	if j.collector() {
		// Before Start there is no Collector: these zeros are exact, not stand-ins.
		zero := int64(0)
		r.LatencyBoundary, r.ExportedSpans = queueLatency, &zero
		r.ExportAttempts, r.QueueRetries = available(0), available(0)
	}
	reason := notStarted
	r.IngestResourcesUnavailableReason = &reason
	return r
}

// collectorConfig maps every job pipeline field onto the Collector explicitly: no queue,
// retry, timeout or write-slot default survives.
func collectorConfig(j *job) collector.Config {
	w := j.Workload
	ms := func(v int64) time.Duration { return time.Duration(v) * time.Millisecond }
	return collector.Config{
		Exporter: exporterConfig(j),
		Timeout:  ms(w.ExportTimeoutMS),
		SendingQueue: collector.QueueConfig{
			Enabled: true, NumConsumers: w.Consumers, QueueSize: w.QueueCapacitySpans, BlockOnOverflow: w.BlockOnOverflow,
			Batch: collector.BatchConfig{FlushTimeout: ms(w.FlushMS), MinSize: w.BatchMinSpans, MaxSize: w.BatchMaxSpans},
		},
		RetryOnFailure: collector.RetryConfig{
			Enabled: w.RetryEnabled, InitialInterval: ms(w.RetryInitialMS), MaxInterval: ms(w.RetryMaxMS), MaxElapsedTime: ms(w.RetryElapsedMS),
		},
	}
}

// callFunc is one timed producer call: spans admitted or submitted, the call's
// committed spans when known (writer_direct), and its outcome.
type callFunc func(context.Context, []*tracepb.ResourceSpans) (int64, *int64, error)

// producers share the corpus; the first error wins and stops publishing.
type producers struct {
	inMu    sync.Mutex
	in      *corpusReader
	mu      sync.Mutex
	samples []sample
	first   error
	cancel  context.CancelFunc
}

func (p *producers) fail(err error) {
	p.mu.Lock()
	if p.first == nil {
		p.first = err
	}
	p.mu.Unlock()
	p.cancel()
}

func (p *producers) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.first
}

// produce runs n producers reading batch frames each: an untimed shape precheck, then
// a timed fresh decode and call. It returns after every producer has returned.
func produce(ctx context.Context, in *corpusReader, n, batch int, call callFunc) *producers {
	publish, cancel := context.WithCancel(ctx)
	p := &producers{in: in, cancel: cancel}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(publish, batch, call)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		p.fail(err)
	}
	cancel()
	return p
}

func (p *producers) loop(ctx context.Context, batch int, call callFunc) {
	for ctx.Err() == nil {
		p.inMu.Lock()
		wires, err := p.in.next(batch)
		p.inMu.Unlock()
		if err != nil {
			p.fail(err)
			return
		}
		if len(wires) == 0 {
			return
		}
		for _, wire := range wires {
			if err := precheckFrame(wire); err != nil {
				p.fail(err)
				return
			}
		}
		begin := time.Now()
		spans := make([]*tracepb.ResourceSpans, 0, len(wires))
		for _, wire := range wires {
			rs, derr := decodeFrame(wire)
			if derr != nil {
				err = derr
				break
			}
			spans = append(spans, rs)
		}
		var accepted int64
		var acked *int64
		if err == nil {
			accepted, acked, err = call(ctx, spans)
		}
		s := sample{NS: time.Since(begin).Nanoseconds(), ReceivedSpans: int64(len(wires)), AcceptedSpans: accepted, DirectWriteAckedOperations: acked}
		if err != nil {
			msg := err.Error()
			s.Error = &msg
		}
		p.mu.Lock()
		p.samples = append(p.samples, s)
		p.mu.Unlock()
		if err != nil {
			p.fail(err)
			return
		}
	}
}

// window is the measured ingest window: wall time and process CPU/RSS readings.
type window struct {
	rt       runtime
	t0       time.Time
	admitted time.Time
	start    syscall.Rusage
	startErr error
}

func openWindow(rt runtime) *window {
	w := &window{rt: rt}
	w.start, w.startErr = rt.usage()
	w.t0 = time.Now()
	return w
}

func (w *window) admissionDone(r *ingestReport) {
	w.admitted = time.Now()
	r.AdmissionPhaseNS = w.admitted.Sub(w.t0).Nanoseconds()
}

// close ends the window; success needs a real measurement, never a CPU or RSS fallback.
func (w *window) close(r *ingestReport, drained bool) error {
	if drained {
		r.DrainNS = time.Since(w.admitted).Nanoseconds()
	}
	end, endErr := w.rt.usage()
	r.IngestWallNS = time.Since(w.t0).Nanoseconds()
	res, reason := measured(w.start, w.startErr, end, endErr)
	if res == nil {
		r.IngestResources, r.IngestResourcesUnavailableReason = nil, &reason
		return fault("ingest", "ingest resources could not be measured")
	}
	r.IngestResources, r.IngestResourcesUnavailableReason = res, nil
	return nil
}

// sampleTotals fills the report's producer-side populations from the joined samples.
func sampleTotals(r *ingestReport, p *producers) {
	r.Frames = int64(p.in.frames)
	r.Samples = p.samples
	if r.Samples == nil {
		r.Samples = []sample{}
	}
	r.ReceivedSpans = 0
	for _, s := range r.Samples {
		r.ReceivedSpans += s.ReceivedSpans
	}
	r.AdmissionLatency = latencyOf(r.Samples)
}

func sealHoldsOn(ctx context.Context, backend fdb.BackendDatabase, m *layoutManifest) error {
	return sealHolds(ctx, recordlayer.NewFDBDatabaseWithBackend(backend), m)
}

// runIngest measures one Go arm. Setup and checks run on the arm's client but outside
// the window; the window is first corpus read through the settled pipeline.
func runIngest(ctx context.Context, rt runtime, j *job, m *layoutManifest, r *ingestReport) error {
	backend, rule, err := rt.ingestBackend(ctx, j)
	if err != nil {
		return err
	}
	// Seals first, on the arm's own client, without committing or repairing anything.
	ts, _, err := openSealed(ctx, backend, j, m, true)
	if err != nil {
		backend.Close()
		return err
	}
	in, err := openCorpus(j.CorpusFile, j.RowBudget)
	if err != nil {
		backend.Close()
		return err
	}
	defer in.close()
	if !j.collector() {
		defer backend.Close()
		// This handle is the measured one.
		r.claimMapping(rule)
		return ingestDirect(ctx, rt, j, backend, ts, in, m, r)
	}
	// The Exporter owns its own handle; this one is not kept open through the window.
	backend.Close()
	return ingestCollector(ctx, rt, j, rule, in, m, r)
}

// sameMapping requires a native reopen to be proved by the preflight open's rule, so one
// reported rule cannot hide a mixed-rule success.
func sameMapping(preflight, rule string) error {
	if rule != preflight {
		return fault("client", "the native client's opens were proved by different mapping rules")
	}
	return nil
}

func ingestDirect(ctx context.Context, rt runtime, j *job, backend fdb.BackendDatabase, ts *otelsql.TenantStore, in *corpusReader, m *layoutManifest, r *ingestReport) error {
	writer := otel.NewSpanWriter(recordlayer.NewFDBDatabaseWithBackend(backend), ts, otel.SpanWriterOptions{
		BatchSize: j.WriterBatchSize, Workers: j.WriterWorkers, BulkIndexConflicts: false, TransactionTimeout: txnTimeout,
	})
	write := func(ctx context.Context, spans []*tracepb.ResourceSpans) (int64, *int64, error) {
		st, err := writer.Write(ctx, spans)
		committed := st.Spans
		return int64(len(spans)), &committed, err
	}
	w := openWindow(rt)
	p := produce(ctx, in, j.Workload.Producers, j.Workload.ProducerBatchSpans, write)
	w.admissionDone(r)
	measureErr := w.close(r, false)
	sampleTotals(r, p)
	var committed int64
	for _, s := range r.Samples {
		r.AcceptedSpans += s.AcceptedSpans
		if s.Error != nil {
			r.FailedSpans += s.ReceivedSpans
		}
		if s.DirectWriteAckedOperations != nil {
			committed += *s.DirectWriteAckedOperations
		}
	}
	if err := errors.Join(p.err(), measureErr); err != nil {
		return err
	}
	if r.Frames != int64(j.ExpectedFrames) || !in.ended || committed != int64(j.ExpectedFrames) {
		return fault("ingest", "the exact source/committed population did not reconcile")
	}
	return sealHoldsOn(ctx, backend, m)
}

// ingestCollector drives the real Collector over the production Exporter on the arm's
// client; counters are read only once every Collector worker has joined. preflight is
// the mapping rule of runIngest's open; every later open must prove the same one.
func ingestCollector(ctx context.Context, rt runtime, j *job, preflight string, in *corpusReader, m *layoutManifest, r *ingestReport) error {
	// Start calls Open synchronously, so exporterRule is set before Start returns.
	exporterRule := ""
	open := rt.sink
	if open == nil {
		open = func(ctx context.Context, cfg otel.ExporterConfig) (collector.Sink, error) {
			backend, rule, err := rt.ingestBackend(ctx, j)
			if err != nil {
				return nil, err
			}
			if err := sameMapping(preflight, rule); err != nil {
				backend.Close()
				return nil, err
			}
			exporterRule = rule
			// From here the Exporter owns backend, closing it on failure or at Shutdown.
			e, err := otel.NewExporterWithBackend(ctx, cfg, backend)
			if err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	cfg := collectorConfig(j)
	x, err := collector.NewFactory().CreateTraces(collector.Settings{ID: collector.ComponentID{Type: collector.ComponentType, Name: "otelrust_control"}, Open: open}, &cfg)
	if err != nil {
		return err
	}
	if err := x.Start(ctx); err != nil {
		return errors.Join(err, x.Shutdown(context.Background()))
	}
	r.claimMapping(exporterRule)
	admit := func(ctx context.Context, spans []*tracepb.ResourceSpans) (int64, *int64, error) {
		if err := x.ConsumeTraces(ctx, spans); err != nil {
			return 0, nil, err
		}
		return int64(len(spans)), nil, nil
	}
	w := openWindow(rt)
	p := produce(ctx, in, j.Workload.Producers, j.Workload.ProducerBatchSpans, admit)
	w.admissionDone(r)
	// Source errors stop publishing, but what was admitted still drains under the job
	// deadline: ctx, not the canceled publishing context.
	if !settle(ctx, x, rt.joinBound, p.fail) {
		// No window end, resources or counters: nothing settled can be reported.
		return unsettledOutcome(p)
	}
	measureErr := w.close(r, true)
	sampleTotals(r, p)
	joined := true
	r.CollectorJoined, r.Queue = &joined, queueDiagnosticsOff()
	var admitted int64
	for _, s := range r.Samples {
		admitted += s.AcceptedSpans
	}
	s := x.Stats()
	exported := s.ExportedSpans
	r.AcceptedSpans, r.RejectedSpans, r.FailedSpans = s.AcceptedSpans, s.RejectedSpans, s.FailedSpans
	r.QueuedSpansAtExit, r.ExportedSpans = int64(s.QueuedSpans), &exported
	r.ExportAttempts, r.QueueRetries = available(s.Exports), available(s.Retries)
	if err := errors.Join(p.err(), measureErr); err != nil {
		return err
	}
	if admitted != s.AcceptedSpans {
		return fault("ingest", "producer admissions differ from the Collector's accepted spans")
	}
	want := int64(j.ExpectedFrames)
	if r.Frames != want || !in.ended || s.AcceptedSpans != want || s.ExportedSpans != want || s.RejectedSpans != 0 || s.FailedSpans != 0 || s.QueuedSpans != 0 {
		return fault("ingest", "the exact source/exported population did not reconcile")
	}
	backend, rule, err := rt.ingestBackend(ctx, j)
	if err != nil {
		return err
	}
	defer backend.Close()
	if err := sameMapping(preflight, rule); err != nil {
		return err
	}
	return sealHoldsOn(ctx, backend, m)
}

// unsettledError is a Collector whose workers did not join within the bounded wait
// after its work was canceled. No report can hold settled values, so run writes a
// diagnostic of only what is stable once the producers have returned.
type unsettledError struct {
	frames, received, admitted int64
	first                      error
}

func (e *unsettledError) Error() string {
	msg := "Go collector workers did not join within the bounded join-only wait; no settled counters, resources or report exist"
	if e.first != nil {
		msg += ": first error: " + e.first.Error()
	}
	return msg
}

func (e *unsettledError) Unwrap() error { return e.first }

func unsettledOutcome(p *producers) *unsettledError {
	u := &unsettledError{frames: int64(p.in.frames), first: p.err()}
	for _, s := range p.samples {
		u.received += s.ReceivedSpans
		u.admitted += s.AcceptedSpans
	}
	return u
}

// settle runs the sequence-covering Flush and Shutdown under the job deadline. An
// expired Shutdown cancels the Collector's work; the bounded wait after it only joins
// and grants no export time. It reports whether every worker joined.
func settle(ctx context.Context, x *collector.TracesExporter, joinBound time.Duration, fail func(error)) bool {
	if err := x.Flush(ctx); err != nil {
		fail(err)
	}
	err := x.Shutdown(ctx)
	if !shutdownExpired(ctx, err) {
		if err != nil {
			fail(err)
		}
		return true
	}
	fail(err)
	join, cancel := context.WithTimeout(context.Background(), joinBound)
	defer cancel()
	err = x.Shutdown(join)
	if shutdownExpired(join, err) {
		return false
	}
	if err != nil {
		fail(err)
	}
	return true
}

func shutdownExpired(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
}
