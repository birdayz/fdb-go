package collector

// bench.go drives a custom in-process Collector-model queue, not the upstream Collector.
// Synthetic spans/logs feed *otel.Exporter alongside queries, then reconstruction and query audits.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
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

// ---------------------------------------------------------------------------
// Synthetic corpus
// ---------------------------------------------------------------------------

const benchSystemPrompt = "You are the on-call reliability copilot for the checkout platform. " +
	"Investigate production incidents using only evidence returned by the tools: traces, spans, and correlated logs. " +
	"Always cite the trace identifier and the span name behind every claim, and say plainly when the evidence is incomplete. " +
	"Prefer the least risky mitigation, such as shifting traffic, pausing a rollout, or raising a queue limit, over invasive changes. " +
	"Never invent metrics, never guess at credentials, and never run destructive operations against the storage cluster. " +
	"Summaries must list the suspected root cause, the blast radius, the affected services, and the next verification step for the human operator."

var benchTools = [...]struct{ name, description, parameters string }{
	{
		"query_traces",
		"Search stored spans by typed attribute equality, time window, and GenAI message text; returns trace identifiers, span names, durations, and status codes ordered newest first.",
		`{"type":"object","properties":{"service":{"type":"string","description":"service.name resource attribute"},"attributes":{"type":"object","description":"span attribute equality predicates"},"text":{"type":"string","description":"message full-text query"},"window_minutes":{"type":"integer","minimum":1,"maximum":1440}},"required":["service"]}`,
	},
	{
		"fetch_logs",
		"Fetch OTLP log records correlated with a trace or span, including severity, body, and attributes, ordered by effective time; use it to confirm error details behind a slow span.",
		`{"type":"object","properties":{"trace_id":{"type":"string","pattern":"^[0-9a-f]{32}$"},"span_id":{"type":"string","pattern":"^[0-9a-f]{16}$"},"severity":{"type":"string","enum":["DEBUG","INFO","WARN","ERROR","FATAL"]},"limit":{"type":"integer","minimum":1,"maximum":500}},"required":["trace_id"]}`,
	},
}

// benchSentences carry no standalone numbers and none of the query tokens, so
// every searchable answer is decided by the deterministic message prefixes.
var benchSentences = [...]string{
	"Checkout p99 latency doubled after the canary rollout reached the hel1 region.",
	"The storage queue on the log servers kept growing while compaction lagged behind.",
	"Commit proxies reported elevated conflict rates for the cart reservation keys.",
	"Correlate the slow spans with the resolver queue depth and the ratekeeper limits.",
	"The payment gateway client retried idempotent requests with exponential backoff.",
	"Cilium reported no packet drops between the checkout pods and the storage nodes.",
	"A hot shard on the inventory keyspace concentrated writes on a single storage team.",
	"Dashboards show the regression started shortly after the deploy window opened.",
	"The operator rebalanced the cluster but the durable version lag persisted.",
	"Please summarize the evidence and propose a mitigation with the lowest risk.",
	"Error budgets for the checkout service burned faster than the alert threshold.",
	"Trace exemplars point at the order confirmation path and its storage reads.",
}

func splitmix64(x *uint64) uint64 {
	*x += 0x9e3779b97f4a7c15
	z := *x
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func benchFiller(seed uint64, c, k int, role string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "filler\x00%d\x00%d\x00%d\x00%s", seed, c, k, role))
	state := uint64(0)
	for _, b := range h[:8] {
		state = state<<8 | uint64(b)
	}
	var b strings.Builder
	for b.Len() < benchFillerBytes {
		b.WriteByte(' ')
		b.WriteString(benchSentences[splitmix64(&state)%uint64(len(benchSentences))])
	}
	return b.String()
}

func benchDigest(seed uint64, run, kind string, c, k int) [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "otelbench/collector/v1\x00%d\x00%s\x00%s\x00%d\x00%d", seed, run, kind, c, k))
}

func nonzero(b []byte) []byte {
	if !slices.ContainsFunc(b, func(c byte) bool { return c != 0 }) {
		b[len(b)-1] = 1
	}
	return b
}

func benchStr(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func benchKV(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

func benchKvlist(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
}

func benchArray(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
}

// benchMessage is a GenAI semconv chat message: role plus one text part.
func benchMessage(role, text string) *commonpb.AnyValue {
	return benchKvlist(benchKV("role", benchStr(role)), benchKV("parts", benchArray(benchKvlist(benchKV("type", benchStr("text")), benchKV("content", benchStr(text))))))
}

func benchClone(v *commonpb.AnyValue) *commonpb.AnyValue { return proto.Clone(v).(*commonpb.AnyValue) }

// benchBlock groups benchBlockSize conversations sharing incident tokens,
// so a committed block has a fixed query answer regardless of corpus size.
func benchBlock(c int) int { return c / benchBlockSize }

// benchBlockMembers is the number of conversations of block b in a corpus of total.
func benchBlockMembers(b, total int) int {
	return max(min(benchBlockSize, total-b*benchBlockSize), 0)
}

// BenchUserText is turn k's new user message text in conversation c. The
// conv_<c> token is the selective term of the phrase classes.
func BenchUserText(seed uint64, c, k int) string {
	return fmt.Sprintf("user turn %d conversation %d conv_%d incident_token_%d shard_key_%d: investigate latency spike in region hel1 service checkout.", k, c, c, benchBlock(c), (c*31+k)%97) + benchFiller(seed, c, k, "user")
}

// BenchAssistantText is turn k's assistant reply text in conversation c.
func BenchAssistantText(seed uint64, c, k int) string {
	return fmt.Sprintf("assistant turn %d conversation %d resolved_token_%d: root cause identified in storage queue.", k, c, benchBlock(c)) + benchFiller(seed, c, k, "assistant")
}

// benchConversationOf decodes the conversation index from the first 8 bytes
// of a benchmark trace ID.
func benchConversationOf(trace []byte) (int, bool) {
	if len(trace) != 16 {
		return 0, false
	}
	c := binary.BigEndian.Uint64(trace[:8])
	if c >= maxBenchConversations {
		return 0, false
	}
	return int(c), true
}

func benchStart(c, k int) int64 {
	return benchBaseTimeNs + int64(c)*60_000_000_000 + int64(k)*2_000_000_000
}

// benchGen regenerates any conversation from (seed, run, c); nothing per
// conversation is retained.
type benchGen struct {
	seed   uint64
	run    string
	tenant int
	turns  int
	system *commonpb.AnyValue
	tools  []*commonpb.AnyValue
}

func newBenchGen(c CollectorBenchConfig) *benchGen {
	g := &benchGen{seed: c.Seed, run: c.Run, tenant: c.Tenant, turns: c.TurnsPerConversation}
	g.system = benchArray(benchKvlist(benchKV("type", benchStr("text")), benchKV("content", benchStr(benchSystemPrompt))))
	for _, t := range benchTools {
		g.tools = append(g.tools, benchKvlist(benchKV("type", benchStr("function")), benchKV("name", benchStr(t.name)), benchKV("description", benchStr(t.description)), benchKV("parameters", benchStr(t.parameters))))
	}
	return g
}

// traceID is c big-endian followed by 8 digest bytes, which are nonzero.
func (g *benchGen) traceID(c int) []byte {
	d := benchDigest(g.seed, g.run, "trace", c, 0)
	id := make([]byte, 16)
	binary.BigEndian.PutUint64(id, uint64(c))
	copy(id[8:], d[:8])
	nonzero(id[8:])
	return id
}

func (g *benchGen) spanID(c, k int) []byte {
	d := benchDigest(g.seed, g.run, "span", c, k)
	return nonzero(d[:8:8])
}

func (g *benchGen) traceHex(c int) string   { return hex.EncodeToString(g.traceID(c)) }
func (g *benchGen) spanHex(c, k int) string { return hex.EncodeToString(g.spanID(c, k)) }
func (g *benchGen) convID(c int) string     { return fmt.Sprintf("conv-%s-%d", g.run, c) }
func (g *benchGen) logEventID(c int) string { return fmt.Sprintf("%s-log-%d", g.run, c) }

// request builds conversation c's ResourceSpans: turn 1 is the root span and
// turn k's parent is turn k-1.
func (g *benchGen) request(c int) *tracepb.ResourceSpans {
	n := g.turns
	providers := [...]string{"openai", "anthropic"}
	trace := g.traceID(c)
	block := benchBlock(c)
	var history []*commonpb.AnyValue
	var parent []byte
	spans := make([]*tracepb.Span, 0, n)
	for k := 1; k <= n; k++ {
		id := g.spanID(c, k)
		start := benchStart(c, k)
		user := benchMessage("user", BenchUserText(g.seed, c, k))
		reply := benchMessage("assistant", BenchAssistantText(g.seed, c, k))
		history = append(history, user)
		input := make([]*commonpb.AnyValue, len(history))
		for i, m := range history {
			input[i] = benchClone(m)
		}
		toolDefs := make([]*commonpb.AnyValue, len(g.tools))
		for i, t := range g.tools {
			toolDefs[i] = benchClone(t)
		}
		sp := &tracepb.Span{
			TraceId:           trace,
			SpanId:            id,
			ParentSpanId:      parent,
			Name:              "chat gpt-4o",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start),
			EndTimeUnixNano:   uint64(start + 1_500_000_000),
			Attributes: []*commonpb.KeyValue{
				benchKV("gen_ai.operation.name", benchStr("chat")),
				benchKV("gen_ai.provider.name", benchStr(providers[c%len(providers)])),
				benchKV("gen_ai.request.model", benchStr("gpt-4o")),
				benchKV("gen_ai.conversation.id", benchStr(g.convID(c))),
				benchKV("incident.block", benchStr(fmt.Sprintf("block_%d", block))),
				benchKV("gen_ai.turn.number", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(k)}}),
				benchKV("gen_ai.system_instructions", benchClone(g.system)),
				benchKV("gen_ai.tool.definitions", benchArray(toolDefs...)),
				benchKV(benchInputKey, benchArray(input...)),
				benchKV(benchOutputKey, benchArray(benchClone(reply))),
			},
		}
		history = append(history, reply)
		parent = id
		spans = append(spans, sp)
	}
	return &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			benchKV("service.name", benchStr("checkout-agent")),
			benchKV("deployment.environment", benchStr("bench")),
			benchKV("tenant.ordinal", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(g.tenant)}}),
		}},
		SchemaUrl: "https://opentelemetry.io/schemas/1.37.0",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "fdb.dev/otelbench/genai", Version: "1"},
			Spans: spans,
		}},
	}
}

// log is conversation c's correlated log, attached to its root span.
func (g *benchGen) log(c int) otel.LogRecord {
	return otel.LogRecord{
		EventID:            g.logEventID(c),
		Timestamp:          benchStart(c, 1) + 1_000_000,
		TraceID:            g.traceHex(c),
		SpanID:             g.spanHex(c, 1),
		SeverityText:       "ERROR",
		SeverityNumber:     17,
		Body:               benchStr(fmt.Sprintf("checkout latency spike in conversation %d: storage queue saturated", c)),
		ResourceAttributes: []*commonpb.KeyValue{benchKV("service.name", benchStr("checkout-agent"))},
		ScopeName:          "fdb.dev/otelbench/genai",
		Attributes: []*commonpb.KeyValue{
			benchKV("log.severity", benchStr("ERROR")),
			benchKV("incident.bucket", benchStr(fmt.Sprintf("bucket_%d", benchBlock(c)))),
		},
	}
}

// benchSingle returns the stored projection of turn k: its own resource, scope and span.
func benchSingle(req *tracepb.ResourceSpans, k int) *tracepb.ResourceSpans {
	ss := req.ScopeSpans[0]
	return proto.Clone(&tracepb.ResourceSpans{Resource: req.Resource, SchemaUrl: req.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{ss.Spans[k-1]}}}}).(*tracepb.ResourceSpans)
}

// benchCorpusStats describes total conversations of turns spans each.
func benchCorpusStats(turns, total int, raw int64) CollectorBenchCorpus {
	n := turns
	out := CollectorBenchCorpus{
		Conversations:        total,
		TurnsPerConversation: n,
		Spans:                total * n,
		Logs:                 total,
		RawInputBytes:        raw,
		TotalMessageElements: int64(total) * int64(n*(n+1)+3*n),
	}
	if total > 0 {
		out.UniqueMessageElements = int64(2*total*n + 1 + len(benchTools))
	}
	return out
}

// benchSamples picks min(total, 16) conversations spread evenly over the corpus.
func benchSamples(total int) []int {
	count := min(total, maxBenchSamples)
	out := make([]int, count)
	for s := range count {
		out[s] = s * total / count
	}
	return out
}

// benchSpread maps iteration i of iters evenly onto [0, n).
func benchSpread(i, iters, n int) int {
	return int(uint64(i%iters) * uint64(n) / uint64(iters))
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

// queryOnly runs the post-ingest query classes on the stored corpus.
func (r *benchRun) queryOnly(ctx context.Context, rep *CollectorBenchReport, exp *otel.Exporter) error {
	if start, ok := ctx.Value(benchmarkIngestStartKey{}).(func()); ok {
		start()
	}
	return r.postQueries(ctx, rep, exp)
}

// postQueries runs every query class over the whole corpus, one iteration
// at a time. With QuerySamples or QueryOnly it reports every iteration: its
// reads and the client requests issued while it ran, which are the query's
// own because nothing else runs.
func (r *benchRun) postQueries(ctx context.Context, rep *CollectorBenchReport, exp *otel.Exporter) error {
	sampled := r.cfg.QuerySamples || r.cfg.QueryOnly
	for _, q := range r.queries {
		if !sampled {
			res := measureQuery(ctx, r.cfg.QueryIterations, func(ctx context.Context, i int) (int, error) {
				return q.run(ctx, r.postTarget(q, i))
			})
			rep.Queries[q.class] = res
			rep.Failures += res.Failures
			continue
		}
		samples := make([]CollectorBenchSample, 0, r.cfg.QueryIterations)
		res := measureQuery(ctx, r.cfg.QueryIterations, func(ctx context.Context, i int) (int, error) {
			t := r.postTarget(q, i)
			w := &benchWork{}
			before := exp.ClientMetrics()
			t0 := time.Now()
			rows, err := q.run(context.WithValue(ctx, benchWorkKey{}, w), t)
			d := time.Since(t0)
			after := exp.ClientMetrics()
			s := CollectorBenchSample{
				Iteration: i, Block: t.block, Conversation: t.conv, Ns: d.Nanoseconds(), Rows: rows, Candidates: w.candidates, Bytes: w.bytes,
				GRVs:         after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted,
				GRVCacheHits: after.GRVCacheHits - before.GRVCacheHits,
				GetRanges:    after.TransactionGetRangeRequests - before.TransactionGetRangeRequests,
				GetValues:    int64(after.ReadLatency.Count) - int64(before.ReadLatency.Count),
			}
			if err != nil {
				s.Error = err.Error()
			}
			samples = append(samples, s)
			return rows, err
		})
		res.Samples = samples
		rep.Queries[q.class] = res
		rep.Failures += res.Failures
	}
	return ctx.Err()
}

type benchWorkKey struct{}

// benchWork is the reads one sampled post-ingest iteration's query charged.
type benchWork struct{ candidates, bytes int64 }

// noteBenchWork adds a query's charged reads to the iteration's benchWork.
func noteBenchWork(ctx context.Context, candidates int, bytes int64) {
	if w, ok := ctx.Value(benchWorkKey{}).(*benchWork); ok {
		w.candidates += int64(candidates)
		w.bytes += bytes
	}
}

type benchmarkIngestStartKey struct{}

// WithBenchmarkIngestStart attaches an instrumentation callback invoked once,
// after exporter setup at the actual ingest clock origin, before producers run;
// in a QueryOnly run, before the first query.
func WithBenchmarkIngestStart(ctx context.Context, start func()) context.Context {
	return context.WithValue(ctx, benchmarkIngestStartKey{}, start)
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

	var producers sync.WaitGroup
	var errMu sync.Mutex
	var produceErrs []error
	for range cfg.Producers {
		producers.Go(func() {
			for ictx.Err() == nil {
				c, ok := r.nextConversation(limit, deadline)
				if !ok {
					return
				}
				if cfg.runtime != nil && cfg.runtime.beforeAdmission != nil {
					if err := cfg.runtime.beforeAdmission(ictx, r, c); err != nil {
						errMu.Lock()
						produceErrs = append(produceErrs, err)
						errMu.Unlock()
						cancel(err)
						return
					}
				}
				req := r.gen.request(c)
				r.raw.Add(int64(proto.Size(req)))
				if err := r.x.ConsumeTraces(ictx, []*tracepb.ResourceSpans{req}); err != nil {
					errMu.Lock()
					produceErrs = append(produceErrs, componentErrorf("conversation %d: %w", c, err))
					errMu.Unlock()
					cancel(err)
					return
				}
				select {
				case r.logs.in <- c:
				case <-ictx.Done():
					return
				}
			}
		})
	}
	producers.Wait()
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

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// benchTarget selects what one query iteration asks about: a conversation,
// or for block classes a block and its member count.
type benchTarget struct {
	iter, conv, block, size int
}

// benchQuery is one verified query class.
type benchQuery struct {
	class string
	block bool
	run   func(context.Context, benchTarget) (int, error)
}

// postTarget spreads post-ingest iterations evenly over the whole corpus.
func (r *benchRun) postTarget(q benchQuery, i int) benchTarget {
	t := benchTarget{iter: i}
	if q.block {
		t.block = benchSpread(i, r.cfg.QueryIterations, (r.total+benchBlockSize-1)/benchBlockSize)
		t.size = benchBlockMembers(t.block, r.total)
	} else {
		t.conv = benchSpread(i, r.cfg.QueryIterations, r.total)
	}
	return t
}

// benchLoad accumulates the under-load query results.
type benchLoad struct {
	mu      sync.Mutex
	classes map[string]*benchLoadClass
	done    atomic.Int64
}

type benchLoadClass struct {
	attempts, failures int
	rows               int64
	samples            []int64
	err                string
}

func newBenchLoad(queries []benchQuery) *benchLoad {
	l := &benchLoad{classes: map[string]*benchLoadClass{}}
	for _, q := range queries {
		l.classes[q.class] = &benchLoadClass{}
	}
	return l
}

func (l *benchLoad) record(class string, rows int, d int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.classes[class]
	c.attempts++
	if err != nil {
		c.failures++
		if c.err == "" {
			c.err = fmt.Sprintf("iteration %d: %v", c.attempts-1, err)
		}
	} else {
		c.rows += int64(rows)
		c.samples = append(c.samples, d)
	}
	l.done.Add(1)
}

func (l *benchLoad) results(wall time.Duration) map[string]CollectorBenchQuery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]CollectorBenchQuery, len(l.classes))
	for class, c := range l.classes {
		q := CollectorBenchQuery{Iterations: c.attempts, Failures: c.failures, Rows: c.rows, WallNs: wall.Nanoseconds(), Error: c.err, Histogram: benchHistogram(slices.Clone(c.samples))}
		if secs := wall.Seconds(); secs > 0 {
			q.QPS = float64(len(c.samples)) / secs
		}
		out[class] = q
	}
	return out
}

// queryWorker runs the classes round-robin against committed data only:
// conversations at or below the watermark and blocks wholly below it.
func (r *benchRun) queryWorker(ctx context.Context, stop <-chan struct{}, next *atomic.Int64) {
	idle := time.NewTimer(0)
	defer idle.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		i := int(next.Add(1) - 1)
		q := r.queries[i%len(r.queries)]
		committed := int(r.track.Watermark() + 1)
		state := r.cfg.Seed ^ uint64(i)*0x9e3779b97f4a7c15
		pick := int(splitmix64(&state) >> 1)
		t := benchTarget{iter: i}
		avail := committed
		if q.block {
			avail = committed / benchBlockSize
			t.block, t.size = pick%max(avail, 1), benchBlockSize
		} else {
			t.conv = pick % max(avail, 1)
		}
		if avail == 0 {
			idle.Reset(benchQueryIdle)
			select {
			case <-stop:
				return
			case <-idle.C:
			}
			continue
		}
		t0 := time.Now()
		rows, err := q.run(ctx, t)
		r.load.record(q.class, rows, time.Since(t0).Nanoseconds(), err)
	}
}

func measureQuery(ctx context.Context, iterations int, run func(context.Context, int) (int, error)) CollectorBenchQuery {
	out := CollectorBenchQuery{Iterations: iterations}
	samples := make([]int64, 0, iterations)
	start := time.Now()
	for i := range iterations {
		t0 := time.Now()
		rows, err := run(ctx, i)
		d := time.Since(t0).Nanoseconds()
		if err != nil {
			out.Failures++
			if out.Error == "" {
				out.Error = fmt.Sprintf("iteration %d: %v", i, err)
			}
			continue
		}
		samples = append(samples, d)
		out.Rows += int64(rows)
	}
	wall := time.Since(start)
	out.WallNs = wall.Nanoseconds()
	if secs := wall.Seconds(); secs > 0 {
		out.QPS = float64(len(samples)) / secs
	}
	out.Histogram = benchHistogram(samples)
	return out
}

// benchHistogram uses the nearest-rank percentiles of otel evidence histograms.
func benchHistogram(samples []int64) otel.Histogram {
	if len(samples) == 0 {
		return otel.Histogram{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	q := func(p int) int64 { return samples[max((len(samples)*p+99)/100-1, 0)] }
	return otel.Histogram{Population: len(samples), Min: samples[0], P50: q(50), P95: q(95), P99: q(99), Max: samples[len(samples)-1]}
}

func benchQueries(cfg CollectorBenchConfig, gen *benchGen, exp *otel.Exporter) []benchQuery {
	n := cfg.TurnsPerConversation
	k := exp.Config().K
	// inBlock checks the trace is exactly the generated trace of a member of block b.
	inBlock := func(trace string, b int) bool {
		raw, err := hex.DecodeString(trace)
		if err != nil {
			return false
		}
		c, ok := benchConversationOf(raw)
		return ok && benchBlock(c) == b && trace == gen.traceHex(c)
	}
	checkHits := func(hits []otel.MessageSpanHit, want int, ok func(otel.MessageSpanHit) error) (int, error) {
		if len(hits) != want {
			return len(hits), componentErrorf("%d hits, want %d", len(hits), want)
		}
		seen := map[string]bool{}
		for _, h := range hits {
			key := h.Trace + "/" + h.Span + "/" + h.AttributeKey + "/" + h.MessageID
			if seen[key] {
				return len(hits), componentErrorf("duplicate hit %s", key)
			}
			seen[key] = true
			if err := ok(h); err != nil {
				return len(hits), err
			}
		}
		return len(hits), nil
	}
	chat := otel.Attribute{Scope: "span", Key: "gen_ai.operation.name", Type: "string", Value: "chat"}
	model := otel.Attribute{Scope: "span", Key: "gen_ai.request.model", Type: "string", Value: "gpt-4o"}
	var out []benchQuery
	if cfg.EnableMessageFullText {
		out = append(
			out,
			benchQuery{QueryFTSSingleToken, true, func(ctx context.Context, t benchTarget) (int, error) {
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("incident_token_%d", t.block), Mode: otel.MessageMatchAll, Role: "user"})
				noteBenchWork(ctx, page.Candidates, page.Bytes)
				if err != nil {
					return 0, err
				}
				// User message j of each conversation is in the input of turns j..n.
				return checkHits(page.Hits, min(t.size*n*(n+1)/2, k), func(h otel.MessageSpanHit) error {
					if h.Role != "user" || h.AttributeKey != benchInputKey || !inBlock(h.Trace, t.block) {
						return componentErrorf("unexpected hit %+v", h)
					}
					return nil
				})
			}},
			benchQuery{QueryFTSPhraseFirstTurn, false, func(ctx context.Context, t benchTarget) (int, error) {
				c, turn := t.conv, t.iter%n+1
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("user turn %d conversation %d conv_%d", turn, c, c), Mode: otel.MessageMatchPhrase, FirstTurnOnly: true})
				noteBenchWork(ctx, page.Candidates, page.Bytes)
				if err != nil {
					return 0, err
				}
				trace, span := gen.traceHex(c), gen.spanHex(c, turn)
				return checkHits(page.Hits, 1, func(h otel.MessageSpanHit) error {
					if h.Trace != trace || h.Span != span || h.AttributeKey != benchInputKey || h.Role != "user" {
						return componentErrorf("hit %+v, want conversation %d turn %d", h, c, turn)
					}
					return nil
				})
			}},
			benchQuery{QueryFTSMultiTurnHistory, false, func(ctx context.Context, t benchTarget) (int, error) {
				c := t.conv
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("user turn 1 conversation %d conv_%d", c, c), Mode: otel.MessageMatchPhrase, AttributeKey: benchInputKey})
				noteBenchWork(ctx, page.Candidates, page.Bytes)
				if err != nil {
					return 0, err
				}
				trace := gen.traceHex(c)
				return checkHits(page.Hits, min(n, k), func(h otel.MessageSpanHit) error {
					if h.Trace != trace || h.AttributeKey != benchInputKey || !slices.Equal(h.TurnOrdinals, []int{0}) {
						return componentErrorf("hit %+v outside conversation %d's first message", h, c)
					}
					return nil
				})
			}},
			benchQuery{QueryFTSSameSpanPreds, true, func(ctx context.Context, t benchTarget) (int, error) {
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("incident_token_%d", t.block), FirstTurnOnly: true, Predicates: []otel.Attribute{chat, model}})
				noteBenchWork(ctx, page.Candidates, page.Bytes)
				if err != nil {
					return 0, err
				}
				return checkHits(page.Hits, min(t.size*n, k), func(h otel.MessageSpanHit) error {
					if h.Role != "user" || h.AttributeKey != benchInputKey || !inBlock(h.Trace, t.block) {
						return componentErrorf("unexpected hit %+v", h)
					}
					return nil
				})
			}},
		)
	}
	out = append(
		out,
		benchQuery{QuerySameSpanConjunction, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			convID := otel.Attribute{Scope: "span", Key: "gen_ai.conversation.id", Type: "string", Value: gen.convID(c)}
			page, err := exp.QuerySameSpan(ctx, otel.SameSpanQuery{Predicates: []otel.Attribute{convID, chat, model}})
			if err != nil {
				return 0, err
			}
			if len(page.Spans) != min(n, k) {
				return len(page.Spans), componentErrorf("%d spans, want %d", len(page.Spans), min(n, k))
			}
			trace := gen.traceHex(c)
			ids := map[string]bool{}
			for turn := 1; turn <= n; turn++ {
				ids[gen.spanHex(c, turn)] = true
			}
			seen := map[string]bool{}
			for _, s := range page.Spans {
				if s.Trace != trace || seen[s.ID] || !ids[s.ID] {
					return len(page.Spans), componentErrorf("unexpected span %+v for conversation %d", s, c)
				}
				seen[s.ID] = true
			}
			return len(page.Spans), nil
		}},
		benchQuery{QueryTraceExistential3, true, func(ctx context.Context, t benchTarget) (int, error) {
			q := otel.TraceExistentialQuery{
				Clauses:    []otel.Attribute{{Scope: "span", Key: "incident.block", Type: "string", Value: fmt.Sprintf("block_%d", t.block)}},
				LogClauses: []otel.Attribute{{Scope: otel.LogScopeRecord, Key: "incident.bucket", Type: "string", Value: fmt.Sprintf("bucket_%d", t.block)}},
			}
			if cfg.EnableMessageFullText {
				q.MessageClauses = []otel.MessageFullTextQuery{{Query: fmt.Sprintf("resolved_token_%d", t.block), Role: "assistant", FirstTurnOnly: true}}
			}
			res, err := exp.QueryTraceExistential(ctx, q)
			if err != nil {
				return 0, err
			}
			want := min(t.size, k)
			if len(res.Hits) != want {
				return len(res.Hits), componentErrorf("%d traces, want %d", len(res.Hits), want)
			}
			seen := map[string]bool{}
			for _, h := range res.Hits {
				if !inBlock(h.Trace, t.block) || seen[h.Trace] {
					return len(res.Hits), componentErrorf("unexpected trace %s for block %d", h.Trace, t.block)
				}
				seen[h.Trace] = true
				if len(h.Witnesses) != 1 || len(h.Witnesses[0]) == 0 || len(h.LogWitnesses) != 1 || len(h.LogWitnesses[0]) == 0 || len(h.MessageWitnesses) != len(q.MessageClauses) {
					return len(res.Hits), componentErrorf("trace %s lacks witnesses", h.Trace)
				}
				for _, w := range h.MessageWitnesses {
					if len(w) == 0 {
						return len(res.Hits), componentErrorf("trace %s lacks message witnesses", h.Trace)
					}
				}
			}
			return len(res.Hits), nil
		}},
		benchQuery{QueryTraceGraph, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			g, err := exp.TraverseTraceGraph(ctx, gen.traceHex(c))
			if err != nil {
				return 0, err
			}
			if len(g.Spans) != n || len(g.Roots) != 1 || g.Roots[0] != gen.spanHex(c, 1) || g.MaxDepth != n-1 || len(g.Orphans) != 0 || len(g.Cycles) != 0 {
				return len(g.Spans), componentErrorf("conversation %d graph: %d spans, roots %v, max depth %d, orphans %v, cycles %v", c, len(g.Spans), g.Roots, g.MaxDepth, g.Orphans, g.Cycles)
			}
			return len(g.Spans), nil
		}},
		benchQuery{QueryLogsByTrace, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			logs, err := exp.QueryLogsByTrace(ctx, gen.traceHex(c), "")
			if err != nil {
				return 0, err
			}
			if len(logs) != 1 || logs[0].EventID != gen.logEventID(c) || logs[0].SpanID != gen.spanHex(c, 1) {
				return len(logs), componentErrorf("conversation %d: %d correlated logs, want its one log", c, len(logs))
			}
			return len(logs), nil
		}},
	)
	return out
}
