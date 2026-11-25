package collector

// bench.go drives a custom in-process Collector-model queue, not the upstream Collector.
// Synthetic spans/logs feed *otel.Exporter alongside queries, then reconstruction and query audits.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"google.golang.org/protobuf/proto"
)

const (
	// CollectorBenchTool identifies collector-bench reports.
	CollectorBenchTool = "otelbench-collector-bench-v2"

	maxBenchConversations  = 100_000_000
	maxBenchTurns          = 200
	maxBenchProducers      = 256
	maxBenchIterations     = 1_000_000
	maxBenchQueryWorkers   = 64
	maxBenchDuration       = time.Hour
	minBenchReportInterval = 100 * time.Millisecond
	maxBenchSamples        = 16
	benchBlockSize         = 50
	benchFillerBytes       = 400
	benchBaseTimeNs        = int64(1_700_000_000_000_000_000)
	benchLogBatch          = 256
	benchLogFlushInterval  = 200 * time.Millisecond
	benchLogAttempts       = 5
	benchQueryIdle         = 10 * time.Millisecond
	// Budget bounds: one index entry, posting, marker or probe row, and one
	// packed span reference in a ref chunk's index key (two IDs at otelsql's
	// bound, attribute, newest and an ordinal).
	benchRowBytes    = 512
	benchRefKeyBytes = 48

	benchInputKey  = "gen_ai.input.messages"
	benchOutputKey = "gen_ai.output.messages"

	benchPhaseLoad  = "load"
	benchPhaseDrain = "drain"
)

// Query classes measured by RunBenchmark, in execution order. The four fts_*
// classes run only with EnableMessageFullText.
const (
	QueryFTSSingleToken      = "fts_single_token"
	QueryFTSPhraseFirstTurn  = "fts_phrase_first_turn"
	QueryFTSMultiTurnHistory = "fts_multi_turn_history"
	QueryFTSSameSpanPreds    = "fts_same_span_predicates"
	QuerySameSpanConjunction = "same_span_conjunction"
	QueryTraceExistential3   = "trace_existential_3way"
	QueryTraceGraph          = "trace_graph_traversal"
	QueryLogsByTrace         = "correlated_logs_by_trace"
)

// CollectorBenchQueryClasses lists every query class in execution order.
var CollectorBenchQueryClasses = []string{QueryFTSSingleToken, QueryFTSPhraseFirstTurn, QueryFTSMultiTurnHistory, QueryFTSSameSpanPreds, QuerySameSpanConjunction, QueryTraceExistential3, QueryTraceGraph, QueryLogsByTrace}

// CollectorBenchConfig configures RunBenchmark; zero non-booleans use defaults (Duration 0: fixed corpus).
// With Duration > 0, producers run until the deadline and ignore Conversations.
type CollectorBenchConfig struct {
	ClusterFile           string        `json:"cluster_file"`
	Run                   string        `json:"run"`
	Tenant                int           `json:"tenant"`
	Seed                  uint64        `json:"seed"`
	Conversations         int           `json:"conversations"`
	TurnsPerConversation  int           `json:"turns_per_conversation"`
	Duration              time.Duration `json:"duration_ns"`
	ReportInterval        time.Duration `json:"report_interval_ns"`
	Warmup                time.Duration `json:"warmup_ns"`
	Producers             int           `json:"producers"`
	NumConsumers          int           `json:"num_consumers"`
	QueryWorkers          int           `json:"query_workers"`
	QueueSize             int           `json:"queue_size"`
	BatchMinSize          int           `json:"batch_min_size"`
	BatchMaxSize          int           `json:"batch_max_size"`
	ExporterBatchSize     int           `json:"exporter_batch_size"`
	ExporterWorkers       int           `json:"exporter_workers"`
	RecordCompression     string        `json:"record_compression"`
	EnableMessageDeltas   bool          `json:"enable_message_deltas"`
	EnableMessageFullText bool          `json:"enable_message_full_text"`
	QueryIterations       int           `json:"query_iterations"`
	Revision              string        `json:"revision"`
	// QueryOnly skips ingest and the fidelity audit: Run must already hold
	// the fixed corpus of Conversations, and only the post-ingest queries
	// run, each iteration reported as a sample (diagnostics).
	QueryOnly bool `json:"query_only,omitempty"`
	// QuerySamples reports every post-ingest query iteration as a sample.
	QuerySamples bool `json:"query_samples,omitempty"`
	// QueryClasses restricts the queries to these comma-separated classes;
	// empty runs all.
	QueryClasses string `json:"query_classes,omitempty"`
	runtime      *benchRuntime
}

// DefaultCollectorBenchConfig returns the defaults; cluster_file and run
// remain required.
func DefaultCollectorBenchConfig() CollectorBenchConfig {
	return CollectorBenchConfig{
		Seed:                  283,
		Conversations:         100,
		TurnsPerConversation:  15,
		ReportInterval:        10 * time.Second,
		Warmup:                60 * time.Second,
		Producers:             8,
		NumConsumers:          4,
		QueryWorkers:          2,
		QueueSize:             10000,
		BatchMinSize:          64,
		BatchMaxSize:          256,
		ExporterBatchSize:     64,
		ExporterWorkers:       8,
		RecordCompression:     "zlib-1",
		EnableMessageDeltas:   true,
		EnableMessageFullText: true,
		QueryIterations:       100,
		Revision:              "unrecorded",
	}
}

// WithDefaults fills zero numeric, duration and string fields from
// DefaultCollectorBenchConfig.
func (c CollectorBenchConfig) WithDefaults() CollectorBenchConfig {
	d := DefaultCollectorBenchConfig()
	for _, f := range []struct{ v, d *int }{
		{&c.Conversations, &d.Conversations},
		{&c.TurnsPerConversation, &d.TurnsPerConversation},
		{&c.Producers, &d.Producers},
		{&c.NumConsumers, &d.NumConsumers},
		{&c.QueryWorkers, &d.QueryWorkers},
		{&c.QueueSize, &d.QueueSize},
		{&c.BatchMinSize, &d.BatchMinSize},
		{&c.BatchMaxSize, &d.BatchMaxSize},
		{&c.ExporterBatchSize, &d.ExporterBatchSize},
		{&c.ExporterWorkers, &d.ExporterWorkers},
		{&c.QueryIterations, &d.QueryIterations},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *time.Duration }{
		{&c.ReportInterval, &d.ReportInterval}, {&c.Warmup, &d.Warmup},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *string }{
		{&c.RecordCompression, &d.RecordCompression}, {&c.Revision, &d.Revision},
	} {
		if *f.v == "" {
			*f.v = *f.d
		}
	}
	if c.Seed == 0 {
		c.Seed = d.Seed
	}
	return c
}

// queryClasses is QueryClasses as a list.
func (c CollectorBenchConfig) queryClasses() []string {
	if c.QueryClasses == "" {
		return nil
	}
	return strings.Split(c.QueryClasses, ",")
}

// Validate checks the benchmark bounds and the derived exporter configuration.
func (c CollectorBenchConfig) Validate() error {
	c = c.WithDefaults()
	var errs []error
	if c.Conversations < 1 || c.Conversations > maxBenchConversations {
		errs = append(errs, componentErrorf("conversations must be 1..%d", maxBenchConversations))
	}
	if c.TurnsPerConversation < 1 || c.TurnsPerConversation > maxBenchTurns {
		errs = append(errs, componentErrorf("turns_per_conversation must be 1..%d", maxBenchTurns))
	}
	if c.Duration < 0 || c.Duration > maxBenchDuration {
		errs = append(errs, componentErrorf("duration must be 0..%s", maxBenchDuration))
	}
	if c.ReportInterval < minBenchReportInterval || c.ReportInterval > maxBenchDuration {
		errs = append(errs, componentErrorf("report_interval must be %s..%s", minBenchReportInterval, maxBenchDuration))
	}
	if c.Warmup < 0 || c.Duration > 0 && c.Warmup >= c.Duration {
		errs = append(errs, componentErrorf("warmup must be non-negative and shorter than duration"))
	}
	if c.Producers < 1 || c.Producers > maxBenchProducers {
		errs = append(errs, componentErrorf("producers must be 1..%d", maxBenchProducers))
	}
	if c.QueryWorkers < 1 || c.QueryWorkers > maxBenchQueryWorkers {
		errs = append(errs, componentErrorf("query_workers must be 1..%d", maxBenchQueryWorkers))
	}
	if c.QueryIterations < 1 || c.QueryIterations > maxBenchIterations {
		errs = append(errs, componentErrorf("query_iterations must be 1..%d", maxBenchIterations))
	}
	if c.QueueSize < c.TurnsPerConversation {
		errs = append(errs, componentErrorf("queue_size must hold one conversation request (turns_per_conversation spans)"))
	}
	if c.QueryOnly && c.Duration > 0 {
		errs = append(errs, componentErrorf("query_only reads a fixed corpus; duration must be 0"))
	}
	for _, class := range c.queryClasses() {
		if !slices.Contains(CollectorBenchQueryClasses, class) {
			errs = append(errs, componentErrorf("unknown query class %q", class))
		}
	}
	if len(errs) == 0 {
		cc := c.collectorConfig()
		if err := cc.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return componentErrorf("collector bench config: %w", err)
	}
	return nil
}

// benchQueryBudget allows twice the reads of one block's query on the
// OTEL_V1 store, independent of corpus size, so an accidental corpus scan
// fails rather than degrading as a sustained run grows.
func benchQueryBudget(turns, k int) (int, int64) {
	n := int64(turns)
	spans := benchBlockSize * n
	// SPANS_REF_CHUNKS entries per span: turn j's span references its 2j+3
	// elements, packed to at most otelsql.MaxRefChunkKeyBytes per chunk.
	chunks := 1 + (2*n+3)*benchRefKeyBytes/otelsql.MaxRefChunkKeyBytes
	// The widest classes read, per span of a block: an FTS block query one
	// TEXT posting, one MESSAGE_TRACES marker, its ref chunks and two
	// same-span predicate probes; the 3-way existential one span-attribute
	// entry, a posting, a marker and the chunks, plus one log per
	// conversation. Role discovery, bunch boundaries and the K-limited
	// classes' verification add the constant terms.
	rows := spans*(4+chunks) + benchBlockSize + 4*int64(k+64) + 16
	// Every row at benchRowBytes, plus the block's packed references (n(n+4)
	// per conversation) at their key bound. It also covers one trace stored
	// without decomposition, whose n spans carry n(n+4) message elements.
	bytes := rows*benchRowBytes + benchBlockSize*n*(n+4)*benchRefKeyBytes
	return int(2*rows + 1000), 2*bytes + 1<<20
}

// collectorConfig maps the benchmark onto the fdb_otel exporter.
func (c CollectorBenchConfig) collectorConfig() *Config {
	cc := defaultConfig()
	e := &cc.Exporter
	e.ClusterFile, e.Run, e.Tenant = c.ClusterFile, c.Run, c.Tenant
	e.RecordCompression = c.RecordCompression
	e.EnableMessageDeltas, e.EnableMessageFullText = c.EnableMessageDeltas, c.EnableMessageFullText
	e.BatchSize, e.Workers = c.ExporterBatchSize, c.ExporterWorkers
	e.MaxCandidates, e.MaxBytes = benchQueryBudget(c.TurnsPerConversation, e.K)
	cc.Timeout = 2 * time.Minute
	cc.SendingQueue = QueueConfig{
		Enabled: true, NumConsumers: c.NumConsumers, QueueSize: c.QueueSize, BlockOnOverflow: true,
		Batch: BatchConfig{FlushTimeout: 200 * time.Millisecond, MinSize: c.BatchMinSize, MaxSize: c.BatchMaxSize},
	}
	cc.RetryOnFailure = RetryConfig{Enabled: true, InitialInterval: 500 * time.Millisecond, MaxInterval: 10 * time.Second, MaxElapsedTime: 5 * time.Minute}
	return &cc
}

// CollectorBenchEffective is the load shape the run actually used.
type CollectorBenchEffective struct {
	Load           string `json:"load"` // "duration" or "fixed"
	Duration       string `json:"duration"`
	ReportInterval string `json:"report_interval"`
	Warmup         string `json:"warmup"`
	Producers      int    `json:"producers"`
	NumConsumers   int    `json:"num_consumers"`
	QueryWorkers   int    `json:"query_workers"`
	BlockSize      int    `json:"block_size"`
}

// CollectorBenchCorpus counts all message elements and distinct elements
// (two per turn plus shared system instructions and two tool definitions).
type CollectorBenchCorpus struct {
	Conversations         int   `json:"conversations"`
	TurnsPerConversation  int   `json:"turns_per_conversation"`
	Spans                 int   `json:"spans"`
	Logs                  int   `json:"logs"`
	RawInputBytes         int64 `json:"raw_input_bytes"`
	TotalMessageElements  int64 `json:"total_message_elements"`
	UniqueMessageElements int64 `json:"unique_message_elements"`
}

// CollectorBenchIngest covers production through final flush; Export sums successful calls.
// UniqueMessages counts per-batch uniques; Written/Present count committed headers, not abandoned tails.
type CollectorBenchIngest struct {
	IngestWallNs      int64               `json:"ingest_wall_ns"`
	SpansPerSec       float64             `json:"spans_per_sec"`
	MessagesPerSec    float64             `json:"messages_per_sec"`
	RawBytesPerSec    float64             `json:"raw_bytes_per_sec"`
	Collector         Stats               `json:"collector"`
	Export            otel.ExportStats    `json:"export"`
	FailedExportCalls int64               `json:"failed_export_calls"`
	LogWallNs         int64               `json:"log_wall_ns"`
	LogCalls          int64               `json:"log_calls"`
	FailedLogCalls    int64               `json:"failed_log_calls"`
	Logs              otel.LogExportStats `json:"logs"`
}

// CollectorBenchWindow reports ingest-relative load/drain intervals with delta counters.
// ExportLatency covers completed calls; queue, watermark and memory are end-of-window samples.
type CollectorBenchWindow struct {
	Phase         string         `json:"phase"`
	StartNs       int64          `json:"start_ns"`
	EndNs         int64          `json:"end_ns"`
	ExportedSpans int64          `json:"exported_spans"`
	Messages      int64          `json:"messages"`
	RawInputBytes int64          `json:"raw_input_bytes"`
	Logs          int64          `json:"logs"`
	ExportCalls   int64          `json:"export_calls"`
	FailedCalls   int64          `json:"failed_calls"`
	Retries       int64          `json:"retries"`
	Queries       int64          `json:"queries"`
	ExportLatency otel.Histogram `json:"export_latency"`
	QueuedSpans   int            `json:"queued_spans"`
	Watermark     int64          `json:"watermark"`
	SpansPerSec   float64        `json:"spans_per_sec"`
	SysBytes      uint64         `json:"sys_bytes"`
	HeapInuse     uint64         `json:"heap_inuse_bytes"`
	// CPU is the process's CPU over the window.
	CPU CollectorBenchCPU `json:"cpu"`
}

// CollectorBenchRate is a throughput over a set of windows.
type CollectorBenchRate struct {
	Windows        int     `json:"windows"`
	WallNs         int64   `json:"wall_ns"`
	Spans          int64   `json:"spans"`
	SpansPerSec    float64 `json:"spans_per_sec"`
	MessagesPerSec float64 `json:"messages_per_sec"`
	RawBytesPerSec float64 `json:"raw_bytes_per_sec"`
	LogsPerSec     float64 `json:"logs_per_sec"`
	// CPU sums the windows' CPU; its ratios are over WallNs and Spans.
	CPU CollectorBenchCPU `json:"cpu"`
}

// CollectorBenchRates reports the whole run (every window) and the steady
// state: load windows starting at or after Warmup.
type CollectorBenchRates struct {
	WholeRun    CollectorBenchRate `json:"whole_run"`
	SteadyState CollectorBenchRate `json:"steady_state"`
}

// CollectorBenchWatermark is the highest fully exported conversation W (-1 for none).
// Tracked/PeakTracked count pending conversations above W; Anomalies count invalid/repeated exports.
type CollectorBenchWatermark struct {
	Conversations int   `json:"conversations"`
	Watermark     int64 `json:"watermark"`
	Tracked       int   `json:"tracked"`
	PeakTracked   int   `json:"peak_tracked"`
	Anomalies     int64 `json:"anomalies"`
}

// CollectorBenchMemory reports the peak runtime.MemStats sampled each window.
type CollectorBenchMemory struct {
	PeakSysBytes       uint64 `json:"peak_sys_bytes"`
	PeakHeapInuseBytes uint64 `json:"peak_heap_inuse_bytes"`
}

// CollectorBenchUnderLoad reports the query classes run by QueryWorkers
// during ingest against committed data; QPS is per class over WallNs.
type CollectorBenchUnderLoad struct {
	Workers int                            `json:"workers"`
	WallNs  int64                          `json:"wall_ns"`
	Queries map[string]CollectorBenchQuery `json:"queries"`
}

// CollectorBenchFidelity reports the phase 2 LoadSpanOTLP audit.
type CollectorBenchFidelity struct {
	CheckedSpans int      `json:"checked_spans"`
	Mismatches   int      `json:"mismatches"`
	Errors       []string `json:"errors,omitempty"`
}

// CollectorBenchQuery reports one query class. Histogram covers successful
// iterations only; QPS is successful iterations per class wall second.
type CollectorBenchQuery struct {
	Iterations int            `json:"iterations"`
	Failures   int            `json:"failures"`
	Rows       int64          `json:"rows"`
	WallNs     int64          `json:"wall_ns"`
	QPS        float64        `json:"qps"`
	Histogram  otel.Histogram `json:"histogram"`
	Error      string         `json:"error,omitempty"`
	// Samples are the post-ingest iterations of a QuerySamples or QueryOnly
	// run, in order.
	Samples []CollectorBenchSample `json:"samples,omitempty"`
}

// CollectorBenchSample is one sampled post-ingest iteration: its target, latency,
// the reads the query charged (FTS classes) and the FDB client requests of
// the exporter's database handle while it ran.
type CollectorBenchSample struct {
	Iteration    int    `json:"iteration"`
	Block        int    `json:"block"`
	Conversation int    `json:"conversation"`
	Ns           int64  `json:"ns"`
	Rows         int    `json:"rows"`
	Candidates   int64  `json:"candidates"`
	Bytes        int64  `json:"logical_read_bytes"`
	GRVs         int64  `json:"grvs"`
	GRVCacheHits int64  `json:"grv_cache_hits"`
	GetRanges    int64  `json:"get_range_requests"`
	GetValues    int64  `json:"get_values"`
	Error        string `json:"error,omitempty"`
}

// CollectorBenchReport is the collector-bench evidence receipt. Queries holds
// the post-ingest phase; UnderLoad the queries run during ingest.
type CollectorBenchReport struct {
	Tool      string                         `json:"tool"`
	Mode      string                         `json:"mode"`
	Revision  string                         `json:"revision"`
	Go        string                         `json:"go"`
	Start     string                         `json:"start_utc"`
	Config    CollectorBenchConfig           `json:"config"`
	Effective CollectorBenchEffective        `json:"effective"`
	Exporter  otel.ExporterConfig            `json:"exporter_config"`
	Corpus    CollectorBenchCorpus           `json:"corpus"`
	Ingest    CollectorBenchIngest           `json:"ingest"`
	Series    []CollectorBenchWindow         `json:"series"`
	Rates     CollectorBenchRates            `json:"rates"`
	Watermark CollectorBenchWatermark        `json:"watermark"`
	Memory    CollectorBenchMemory           `json:"memory"`
	UnderLoad CollectorBenchUnderLoad        `json:"under_load"`
	Fidelity  CollectorBenchFidelity         `json:"fidelity"`
	Queries   map[string]CollectorBenchQuery `json:"queries"`
	Failures  int                            `json:"failures"`
	WallNs    int64                          `json:"wall_ns"`
	Error     string                         `json:"error,omitempty"`
	Boundary  string                         `json:"boundary"`
}

// ---------------------------------------------------------------------------
// RunBenchmark
// ---------------------------------------------------------------------------

// RunBenchmark runs the collector benchmark against the cluster. The returned
// report is populated as far as the run progressed, including on error.
func RunBenchmark(ctx context.Context, cfg CollectorBenchConfig) (CollectorBenchReport, error) {
	cfg = cfg.WithDefaults()
	started := time.Now()
	load := "fixed"
	if cfg.Duration > 0 {
		load = "duration"
	}
	r := CollectorBenchReport{
		Tool: CollectorBenchTool, Mode: "collector-bench", Revision: cfg.Revision, Go: runtime.Version(),
		Start: started.UTC().Format(time.RFC3339Nano), Config: cfg, Queries: map[string]CollectorBenchQuery{},
		Effective: CollectorBenchEffective{
			Load: load, Duration: cfg.Duration.String(), ReportInterval: cfg.ReportInterval.String(), Warmup: cfg.Warmup.String(),
			Producers: cfg.Producers, NumConsumers: cfg.NumConsumers, QueryWorkers: cfg.QueryWorkers, BlockSize: benchBlockSize,
		},
		UnderLoad: CollectorBenchUnderLoad{Workers: cfg.QueryWorkers, Queries: map[string]CollectorBenchQuery{}},
		Boundary: "synthetic deterministic multi-turn GenAI conversations and correlated logs through the custom fdb_otel Collector-model queue (not upstream OpenTelemetry Collector) into one FDB run; " +
			"one producer host, closed-loop producers and query workers, not SLO/HA evidence; query answers verified against the generated model",
	}
	err := runBenchmark(ctx, cfg, &r)
	r.WallNs = time.Since(started).Nanoseconds()
	if err == nil && r.Failures > 0 {
		err = componentErrorf("collector bench: %d verification failures", r.Failures)
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r, err
}

func runBenchmark(ctx context.Context, cfg CollectorBenchConfig, rep *CollectorBenchReport) (err error) {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cc := cfg.collectorConfig()
	rep.Exporter = cc.Exporter
	r := &benchRun{cfg: cfg, gen: newBenchGen(cfg), phase: benchPhaseLoad}
	r.track = newBenchTracker(cfg.TurnsPerConversation, &r.claimed)
	r.sink = &benchSink{track: r.track}
	x, err := NewFactory().CreateTraces(Settings{ID: ComponentID{Type: ComponentType, Name: "bench"}, Open: func(ctx context.Context, ec otel.ExporterConfig) (Sink, error) {
		e, err := otel.NewExporter(ctx, ec)
		if err != nil {
			return nil, err
		}
		r.sink.exp = e
		return r.sink, nil
	}}, cc)
	if err != nil {
		return err
	}
	if err := x.Start(ctx); err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if serr := x.Shutdown(sctx); serr != nil && err == nil {
			err = componentErrorf("collector bench shutdown: %w", serr)
		}
	}()
	r.x = x
	exp := r.sink.exp
	r.logs = &benchLogWriter{gen: r.gen, exp: exp, track: r.track, in: make(chan int, benchLogBatch)}
	r.queries = benchQueries(cfg, r.gen, exp)
	if classes := cfg.queryClasses(); len(classes) > 0 {
		r.queries = slices.DeleteFunc(r.queries, func(q benchQuery) bool { return !slices.Contains(classes, q.class) })
		if len(r.queries) != len(slices.Compact(slices.Sorted(slices.Values(classes)))) {
			return componentErrorf("collector bench: query classes %q are not all enabled in this layout", cfg.QueryClasses)
		}
	}
	if cfg.QueryOnly {
		r.total = cfg.Conversations
		return r.queryOnly(ctx, rep, exp)
	}
	r.load = newBenchLoad(r.queries)

	// Phase 1: producers, logs and query workers until the deadline or the
	// fixed corpus is admitted, then flush.
	ingestErr := r.ingest(ctx, rep)
	r.report(rep)
	for _, q := range rep.UnderLoad.Queries {
		rep.Failures += q.Failures
	}
	if ingestErr != nil {
		return ingestErr
	}
	n := cfg.TurnsPerConversation
	if s := rep.Ingest.Collector; s.ExportedSpans != int64(r.total*n) || s.FailedSpans != 0 {
		return componentErrorf("collector bench ingest: exported %d of %d spans, %d failed", s.ExportedSpans, r.total*n, s.FailedSpans)
	}
	if w := rep.Watermark; w.Watermark != int64(r.total)-1 || w.Tracked != 0 || w.Anomalies != 0 {
		return componentErrorf("collector bench ingest: commit watermark %d with %d tracked and %d anomalies, want %d", w.Watermark, w.Tracked, w.Anomalies, r.total-1)
	}

	// Phase 2: bit-for-bit reconstruction of sampled spans.
	for _, c := range benchSamples(r.total) {
		req := r.gen.request(c)
		for _, k := range slices.Compact([]int{1, n}) {
			got, err := exp.LoadSpanOTLP(ctx, r.gen.traceHex(c), r.gen.spanHex(c, k))
			rep.Fidelity.CheckedSpans++
			switch {
			case err != nil:
				rep.Fidelity.Mismatches++
				rep.Fidelity.Errors = append(rep.Fidelity.Errors, fmt.Sprintf("conversation %d turn %d: %v", c, k, err))
			case !proto.Equal(got, benchSingle(req, k)):
				rep.Fidelity.Mismatches++
				rep.Fidelity.Errors = append(rep.Fidelity.Errors, fmt.Sprintf("conversation %d turn %d: reconstructed span differs from the original", c, k))
			}
		}
	}
	rep.Failures += rep.Fidelity.Mismatches

	// Phase 3: verified query classes over the whole ingested corpus.
	return r.postQueries(ctx, rep, exp)
}

type benchmarkIngestStartKey struct{}

// WithBenchmarkIngestStart attaches an instrumentation callback invoked once,
// after exporter setup at the actual ingest clock origin, before producers run;
// in a QueryOnly run, before the first query.
func WithBenchmarkIngestStart(ctx context.Context, start func()) context.Context {
	return context.WithValue(ctx, benchmarkIngestStartKey{}, start)
}
