package collector

// bench.go drives the full fdb_otel production path end to end: synthetic
// multi-turn GenAI conversations flow from concurrent producers through the
// Collector sending queue into *otel.Exporter (message delta dedup and message
// full-text indexing), followed by correlated OTLP logs, a bit-for-bit
// LoadSpanOTLP reconstruction audit, and a verified query latency suite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"fdb.dev/layers/otel"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// CollectorBenchTool identifies collector-bench reports.
	CollectorBenchTool = "otelbench-collector-bench-v1"

	maxBenchConversations = 100_000
	maxBenchTurns         = 200
	maxBenchProducers     = 256
	maxBenchIterations    = 1_000_000
	maxBenchSamples       = 16
	benchTokenBuckets     = 50
	benchFillerBytes      = 400
	benchBaseTimeNs       = int64(1_700_000_000_000_000_000)

	benchInputKey  = "gen_ai.input.messages"
	benchOutputKey = "gen_ai.output.messages"
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

// CollectorBenchConfig configures RunBenchmark. Zero numeric and string fields
// take DefaultCollectorBenchConfig values; booleans are used as given.
type CollectorBenchConfig struct {
	ClusterFile           string `json:"cluster_file"`
	Run                   string `json:"run"`
	Tenant                int    `json:"tenant"`
	Seed                  uint64 `json:"seed"`
	Conversations         int    `json:"conversations"`
	TurnsPerConversation  int    `json:"turns_per_conversation"`
	Producers             int    `json:"producers"`
	NumConsumers          int    `json:"num_consumers"`
	QueueSize             int    `json:"queue_size"`
	BatchMinSize          int    `json:"batch_min_size"`
	BatchMaxSize          int    `json:"batch_max_size"`
	ExporterBatchSize     int    `json:"exporter_batch_size"`
	ExporterWorkers       int    `json:"exporter_workers"`
	RecordCompression     string `json:"record_compression"`
	PostingValueFormat    string `json:"posting_value_format"`
	HashBytes             int    `json:"hash_bytes"`
	Order                 string `json:"order"`
	EnableMessageDeltas   bool   `json:"enable_message_deltas"`
	EnableMessageFullText bool   `json:"enable_message_full_text"`
	QueryIterations       int    `json:"query_iterations"`
	Revision              string `json:"revision"`
}

// DefaultCollectorBenchConfig returns the defaults; cluster_file and run
// remain required.
func DefaultCollectorBenchConfig() CollectorBenchConfig {
	return CollectorBenchConfig{
		Seed:                  283,
		Conversations:         100,
		TurnsPerConversation:  15,
		Producers:             8,
		NumConsumers:          4,
		QueueSize:             10000,
		BatchMinSize:          64,
		BatchMaxSize:          256,
		ExporterBatchSize:     64,
		ExporterWorkers:       8,
		RecordCompression:     "brotli-4",
		PostingValueFormat:    "tuple",
		HashBytes:             8,
		Order:                 "time",
		EnableMessageDeltas:   true,
		EnableMessageFullText: true,
		QueryIterations:       100,
		Revision:              "unrecorded",
	}
}

// WithDefaults fills zero numeric and string fields from DefaultCollectorBenchConfig.
func (c CollectorBenchConfig) WithDefaults() CollectorBenchConfig {
	d := DefaultCollectorBenchConfig()
	for _, f := range []struct{ v, d *int }{
		{&c.Conversations, &d.Conversations},
		{&c.TurnsPerConversation, &d.TurnsPerConversation},
		{&c.Producers, &d.Producers},
		{&c.NumConsumers, &d.NumConsumers},
		{&c.QueueSize, &d.QueueSize},
		{&c.BatchMinSize, &d.BatchMinSize},
		{&c.BatchMaxSize, &d.BatchMaxSize},
		{&c.ExporterBatchSize, &d.ExporterBatchSize},
		{&c.ExporterWorkers, &d.ExporterWorkers},
		{&c.HashBytes, &d.HashBytes},
		{&c.QueryIterations, &d.QueryIterations},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *string }{
		{&c.RecordCompression, &d.RecordCompression}, {&c.PostingValueFormat, &d.PostingValueFormat}, {&c.Order, &d.Order}, {&c.Revision, &d.Revision},
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

// Validate checks the benchmark bounds and the derived exporter configuration.
func (c CollectorBenchConfig) Validate() error {
	c = c.WithDefaults()
	var errs []error
	if c.Conversations < 1 || c.Conversations > maxBenchConversations {
		errs = append(errs, fmt.Errorf("conversations must be 1..%d", maxBenchConversations))
	}
	if c.TurnsPerConversation < 1 || c.TurnsPerConversation > maxBenchTurns {
		errs = append(errs, fmt.Errorf("turns_per_conversation must be 1..%d", maxBenchTurns))
	}
	if c.Producers < 1 || c.Producers > maxBenchProducers {
		errs = append(errs, fmt.Errorf("producers must be 1..%d", maxBenchProducers))
	}
	if c.QueryIterations < 1 || c.QueryIterations > maxBenchIterations {
		errs = append(errs, fmt.Errorf("query_iterations must be 1..%d", maxBenchIterations))
	}
	if c.QueueSize < c.TurnsPerConversation {
		errs = append(errs, errors.New("queue_size must hold one conversation request (turns_per_conversation spans)"))
	}
	if len(errs) == 0 {
		cc := c.collectorConfig()
		if err := cc.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("collector bench config: %w", err)
	}
	return nil
}

func (c CollectorBenchConfig) spans() int { return c.Conversations * c.TurnsPerConversation }

// collectorConfig maps the benchmark onto the fdb_otel exporter. Query budgets
// grow with the corpus because the existential span clause scans every span.
func (c CollectorBenchConfig) collectorConfig() *Config {
	cc := defaultConfig()
	e := &cc.Exporter
	e.ClusterFile, e.Run, e.Tenant = c.ClusterFile, c.Run, c.Tenant
	e.Order, e.HashBytes, e.PostingValueFormat, e.RecordCompression = c.Order, c.HashBytes, c.PostingValueFormat, c.RecordCompression
	e.EnableMessageDeltas, e.EnableMessageFullText = c.EnableMessageDeltas, c.EnableMessageFullText
	e.BatchSize, e.Workers = c.ExporterBatchSize, c.ExporterWorkers
	e.MaxCandidates = max(e.MaxCandidates, 4*c.spans()+1000)
	e.MaxBytes = max(e.MaxBytes, int64(c.spans())*4096)
	cc.Timeout = 2 * time.Minute
	cc.SendingQueue = QueueConfig{
		Enabled: true, NumConsumers: c.NumConsumers, QueueSize: c.QueueSize, BlockOnOverflow: true,
		Batch: BatchConfig{FlushTimeout: 200 * time.Millisecond, MinSize: c.BatchMinSize, MaxSize: c.BatchMaxSize},
	}
	cc.RetryOnFailure = RetryConfig{Enabled: true, InitialInterval: 500 * time.Millisecond, MaxInterval: 10 * time.Second, MaxElapsedTime: 5 * time.Minute}
	return &cc
}

// CollectorBenchCorpus describes the synthetic workload. TotalMessageElements
// counts every message element across all spans' content attributes;
// UniqueMessageElements counts distinct elements (2 per turn plus the shared
// system instructions and two tool definitions).
type CollectorBenchCorpus struct {
	Conversations         int   `json:"conversations"`
	TurnsPerConversation  int   `json:"turns_per_conversation"`
	Spans                 int   `json:"spans"`
	Logs                  int   `json:"logs"`
	RawInputBytes         int64 `json:"raw_input_bytes"`
	TotalMessageElements  int64 `json:"total_message_elements"`
	UniqueMessageElements int64 `json:"unique_message_elements"`
}

// CollectorBenchIngest reports phase 1. Export sums the ExportStats of every
// successful sink call; its MessageDeltas.UniqueMessages counts per-batch
// uniques, while MessageFTS.IndexedBlobs counts blobs first indexed in FDB.
type CollectorBenchIngest struct {
	IngestWallNs      int64               `json:"ingest_wall_ns"`
	SpansPerSec       float64             `json:"spans_per_sec"`
	MessagesPerSec    float64             `json:"messages_per_sec"`
	RawBytesPerSec    float64             `json:"raw_bytes_per_sec"`
	Collector         Stats               `json:"collector"`
	Export            otel.ExportStats    `json:"export"`
	FailedExportCalls int64               `json:"failed_export_calls"`
	LogWallNs         int64               `json:"log_wall_ns"`
	Logs              otel.LogExportStats `json:"logs"`
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
}

// CollectorBenchReport is the collector-bench evidence receipt.
type CollectorBenchReport struct {
	Tool     string                         `json:"tool"`
	Mode     string                         `json:"mode"`
	Revision string                         `json:"revision"`
	Go       string                         `json:"go"`
	Start    string                         `json:"start_utc"`
	Config   CollectorBenchConfig           `json:"config"`
	Exporter otel.ExporterConfig            `json:"exporter_config"`
	Corpus   CollectorBenchCorpus           `json:"corpus"`
	Ingest   CollectorBenchIngest           `json:"ingest"`
	Fidelity CollectorBenchFidelity         `json:"fidelity"`
	Queries  map[string]CollectorBenchQuery `json:"queries"`
	Failures int                            `json:"failures"`
	WallNs   int64                          `json:"wall_ns"`
	Error    string                         `json:"error,omitempty"`
	Boundary string                         `json:"boundary"`
}

// benchSink sums ExportStats over successful calls into the real exporter.
type benchSink struct {
	exp    *otel.Exporter
	mu     sync.Mutex
	stats  otel.ExportStats
	failed int64
}

func (s *benchSink) ExportResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans) (otel.ExportStats, error) {
	st, err := s.exp.ExportResourceSpans(ctx, spans)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failed++
	} else {
		addExportStats(&s.stats, st)
	}
	return st, err
}

func (s *benchSink) Shutdown(ctx context.Context) error { return s.exp.Shutdown(ctx) }

func (s *benchSink) snapshot() (otel.ExportStats, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats, s.failed
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
	f, n := &dst.MessageFTS, s.MessageFTS
	f.IndexedBlobs += n.IndexedBlobs
	f.SkippedBlobs += n.SkippedBlobs
	f.UniqueTokens += n.UniqueTokens
	f.TokenPostings += n.TokenPostings
	f.SpanRefs += n.SpanRefs
	f.Bytes += n.Bytes
	f.Transactions += n.Transactions
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

// BenchUserText is turn k's new user message text in conversation c.
func BenchUserText(seed uint64, c, k int) string {
	return fmt.Sprintf("user turn %d conversation %d incident_token_%d shard_key_%d: investigate latency spike in region hel1 service checkout.", k, c, c%benchTokenBuckets, (c*31+k)%97) + benchFiller(seed, c, k, "user")
}

// BenchAssistantText is turn k's assistant reply text in conversation c.
func BenchAssistantText(seed uint64, c, k int) string {
	return fmt.Sprintf("assistant turn %d conversation %d resolved_token_%d: root cause identified in storage queue.", k, c, c%benchTokenBuckets) + benchFiller(seed, c, k, "assistant")
}

type benchConversation struct {
	trace   []byte
	spans   [][]byte // span IDs, index k-1 for turn k
	starts  []int64
	request *tracepb.ResourceSpans
}

func (b *benchConversation) traceHex() string     { return hex.EncodeToString(b.trace) }
func (b *benchConversation) spanHex(k int) string { return hex.EncodeToString(b.spans[k-1]) }

type benchCorpus struct {
	convs  []*benchConversation
	logs   []otel.LogRecord
	traces map[string]int // trace hex -> conversation
	stats  CollectorBenchCorpus
}

// newBenchCorpus deterministically builds one ResourceSpans request per
// conversation: turn 1 is the root span and turn k's parent is turn k-1.
func newBenchCorpus(c CollectorBenchConfig) *benchCorpus {
	n := c.TurnsPerConversation
	system := benchArray(benchKvlist(benchKV("type", benchStr("text")), benchKV("content", benchStr(benchSystemPrompt))))
	tools := make([]*commonpb.AnyValue, len(benchTools))
	for i, t := range benchTools {
		tools[i] = benchKvlist(benchKV("type", benchStr("function")), benchKV("name", benchStr(t.name)), benchKV("description", benchStr(t.description)), benchKV("parameters", benchStr(t.parameters)))
	}
	providers := [...]string{"openai", "anthropic"}
	out := &benchCorpus{traces: make(map[string]int, c.Conversations)}
	out.stats = CollectorBenchCorpus{
		Conversations:         c.Conversations,
		TurnsPerConversation:  n,
		Spans:                 c.spans(),
		Logs:                  c.Conversations,
		TotalMessageElements:  int64(c.Conversations) * int64(n*(n+1)+3*n),
		UniqueMessageElements: int64(2*c.spans() + 1 + len(benchTools)),
	}
	for ci := range c.Conversations {
		td := benchDigest(c.Seed, c.Run, "trace", ci, 0)
		conv := &benchConversation{trace: nonzero(td[:16:16])}
		convID := fmt.Sprintf("conv-%s-%d", c.Run, ci)
		var history []*commonpb.AnyValue
		spans := make([]*tracepb.Span, 0, n)
		for k := 1; k <= n; k++ {
			sd := benchDigest(c.Seed, c.Run, "span", ci, k)
			id := nonzero(sd[:8:8])
			start := benchBaseTimeNs + int64(ci)*60_000_000_000 + int64(k)*2_000_000_000
			user := benchMessage("user", BenchUserText(c.Seed, ci, k))
			reply := benchMessage("assistant", BenchAssistantText(c.Seed, ci, k))
			history = append(history, user)
			input := make([]*commonpb.AnyValue, len(history))
			for i, m := range history {
				input[i] = benchClone(m)
			}
			toolDefs := make([]*commonpb.AnyValue, len(tools))
			for i, t := range tools {
				toolDefs[i] = benchClone(t)
			}
			sp := &tracepb.Span{
				TraceId:           conv.trace,
				SpanId:            id,
				Name:              "chat gpt-4o",
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: uint64(start),
				EndTimeUnixNano:   uint64(start + 1_500_000_000),
				Attributes: []*commonpb.KeyValue{
					benchKV("gen_ai.operation.name", benchStr("chat")),
					benchKV("gen_ai.provider.name", benchStr(providers[ci%len(providers)])),
					benchKV("gen_ai.request.model", benchStr("gpt-4o")),
					benchKV("gen_ai.conversation.id", benchStr(convID)),
					benchKV("gen_ai.turn.number", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(k)}}),
					benchKV("gen_ai.system_instructions", benchClone(system)),
					benchKV("gen_ai.tool.definitions", benchArray(toolDefs...)),
					benchKV(benchInputKey, benchArray(input...)),
					benchKV(benchOutputKey, benchArray(benchClone(reply))),
				},
			}
			if k > 1 {
				sp.ParentSpanId = conv.spans[k-2]
			}
			history = append(history, reply)
			conv.spans = append(conv.spans, id)
			conv.starts = append(conv.starts, start)
			spans = append(spans, sp)
		}
		conv.request = &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				benchKV("service.name", benchStr("checkout-agent")),
				benchKV("deployment.environment", benchStr("bench")),
				benchKV("tenant.ordinal", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(c.Tenant)}}),
			}},
			SchemaUrl: "https://opentelemetry.io/schemas/1.37.0",
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "fdb.dev/otelbench/genai", Version: "1"},
				Spans: spans,
			}},
		}
		out.stats.RawInputBytes += int64(proto.Size(conv.request))
		out.traces[conv.traceHex()] = ci
		out.convs = append(out.convs, conv)
		out.logs = append(out.logs, otel.LogRecord{
			EventID:            fmt.Sprintf("%s-log-%d", c.Run, ci),
			Timestamp:          conv.starts[0] + 1_000_000,
			TraceID:            conv.traceHex(),
			SpanID:             conv.spanHex(1),
			SeverityText:       "ERROR",
			SeverityNumber:     17,
			Body:               benchStr(fmt.Sprintf("checkout latency spike in conversation %d: storage queue saturated", ci)),
			ResourceAttributes: []*commonpb.KeyValue{benchKV("service.name", benchStr("checkout-agent"))},
			ScopeName:          "fdb.dev/otelbench/genai",
			Attributes: []*commonpb.KeyValue{
				benchKV("log.severity", benchStr("ERROR")),
				benchKV("incident.bucket", benchStr(fmt.Sprintf("bucket_%d", ci%benchTokenBuckets))),
			},
		})
	}
	return out
}

// single returns the stored projection of turn k: its own resource, scope and span.
func (b *benchConversation) single(k int) *tracepb.ResourceSpans {
	rs, ss := b.request, b.request.ScopeSpans[0]
	return proto.Clone(&tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{ss.Spans[k-1]}}}}).(*tracepb.ResourceSpans)
}

type benchSample struct {
	conv, turn int
	want       *tracepb.ResourceSpans
}

// samples picks min(Conversations, 16) evenly spread conversations and their
// first and final turns; the final turn carries the longest history.
func (bc *benchCorpus) samples() []benchSample {
	c := len(bc.convs)
	n := bc.stats.TurnsPerConversation
	count := min(c, maxBenchSamples)
	var out []benchSample
	for s := range count {
		ci := s * c / count
		for _, k := range slices.Compact([]int{1, n}) {
			out = append(out, benchSample{ci, k, bc.convs[ci].single(k)})
		}
	}
	return out
}

// bucketSize is the number of conversations c with c % 50 == t.
func (bc *benchCorpus) bucketSize(t int) int {
	c := len(bc.convs)
	if t >= c {
		return 0
	}
	return (c-1-t)/benchTokenBuckets + 1
}

func (bc *benchCorpus) bucketMod() int { return min(benchTokenBuckets, len(bc.convs)) }

// ---------------------------------------------------------------------------
// RunBenchmark
// ---------------------------------------------------------------------------

// RunBenchmark runs the collector benchmark against the cluster. The returned
// report is populated as far as the run progressed, including on error.
func RunBenchmark(ctx context.Context, cfg CollectorBenchConfig) (CollectorBenchReport, error) {
	cfg = cfg.WithDefaults()
	started := time.Now()
	r := CollectorBenchReport{
		Tool: CollectorBenchTool, Mode: "collector-bench", Revision: cfg.Revision, Go: runtime.Version(),
		Start: started.UTC().Format(time.RFC3339Nano), Config: cfg, Queries: map[string]CollectorBenchQuery{},
		Boundary: "synthetic deterministic multi-turn GenAI conversations through the fdb_otel Collector sending queue into one FDB run; " +
			"closed-loop single-client diagnostic, not SLO/HA evidence; query answers verified against the generated model",
	}
	err := runBenchmark(ctx, cfg, &r)
	r.WallNs = time.Since(started).Nanoseconds()
	if err == nil && r.Failures > 0 {
		err = fmt.Errorf("collector bench: %d verification failures", r.Failures)
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r, err
}

func runBenchmark(ctx context.Context, cfg CollectorBenchConfig, r *CollectorBenchReport) (err error) {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cc := cfg.collectorConfig()
	r.Exporter = cc.Exporter
	corpus := newBenchCorpus(cfg)
	r.Corpus = corpus.stats
	samples := corpus.samples()

	sink := &benchSink{}
	x, err := NewFactory().CreateTraces(Settings{ID: ComponentID{Type: ComponentType, Name: "bench"}, Open: func(ctx context.Context, ec otel.ExporterConfig) (Sink, error) {
		e, err := otel.NewExporter(ctx, ec)
		if err != nil {
			return nil, err
		}
		sink.exp = e
		return sink, nil
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
			err = fmt.Errorf("collector bench shutdown: %w", serr)
		}
	}()
	exp := sink.exp

	// Phase 1: concurrent producers through the sending queue, then logs.
	ingestStart := time.Now()
	jobs := make(chan *benchConversation)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var produceErrs []error
	for range cfg.Producers {
		wg.Go(func() {
			for conv := range jobs {
				if err := x.ConsumeTraces(ctx, []*tracepb.ResourceSpans{conv.request}); err != nil {
					errMu.Lock()
					produceErrs = append(produceErrs, err)
					errMu.Unlock()
				}
			}
		})
	}
feed:
	for _, conv := range corpus.convs {
		select {
		case jobs <- conv:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	flushErr := x.Flush(ctx)
	ingestWall := time.Since(ingestStart)
	in := &r.Ingest
	in.IngestWallNs = ingestWall.Nanoseconds()
	if secs := ingestWall.Seconds(); secs > 0 {
		in.SpansPerSec = float64(corpus.stats.Spans) / secs
		in.MessagesPerSec = float64(corpus.stats.TotalMessageElements) / secs
		in.RawBytesPerSec = float64(corpus.stats.RawInputBytes) / secs
	}
	in.Collector = x.Stats()
	in.Export, in.FailedExportCalls = sink.snapshot()
	if len(produceErrs) > 0 {
		return fmt.Errorf("collector bench ingest: %d rejected requests: %w", len(produceErrs), errors.Join(produceErrs[:min(len(produceErrs), 4)]...))
	}
	if flushErr != nil {
		return fmt.Errorf("collector bench flush: %w", flushErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s := in.Collector; s.ExportedSpans != int64(corpus.stats.Spans) || s.FailedSpans != 0 {
		return fmt.Errorf("collector bench ingest: exported %d of %d spans, %d failed", s.ExportedSpans, corpus.stats.Spans, s.FailedSpans)
	}
	logStart := time.Now()
	in.Logs, err = exp.ExportLogs(ctx, corpus.logs)
	in.LogWallNs = time.Since(logStart).Nanoseconds()
	in.Logs.EventIDs = nil
	if err != nil {
		return fmt.Errorf("collector bench logs: %w", err)
	}

	// Phase 2: bit-for-bit reconstruction of sampled spans.
	for _, s := range samples {
		conv := corpus.convs[s.conv]
		got, err := exp.LoadSpanOTLP(ctx, conv.traceHex(), conv.spanHex(s.turn))
		r.Fidelity.CheckedSpans++
		switch {
		case err != nil:
			r.Fidelity.Mismatches++
			r.Fidelity.Errors = append(r.Fidelity.Errors, fmt.Sprintf("conversation %d turn %d: %v", s.conv, s.turn, err))
		case !proto.Equal(got, s.want):
			r.Fidelity.Mismatches++
			r.Fidelity.Errors = append(r.Fidelity.Errors, fmt.Sprintf("conversation %d turn %d: reconstructed span differs from the original", s.conv, s.turn))
		}
	}
	r.Failures += r.Fidelity.Mismatches

	// Phase 3: verified query classes.
	for _, q := range benchQueries(cfg, corpus, exp) {
		res := measureQuery(ctx, cfg.QueryIterations, q.run)
		r.Queries[q.class] = res
		r.Failures += res.Failures
	}
	return ctx.Err()
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

type benchQuery struct {
	class string
	run   func(context.Context, int) (int, error)
}

func benchQueries(cfg CollectorBenchConfig, corpus *benchCorpus, exp *otel.Exporter) []benchQuery {
	n := cfg.TurnsPerConversation
	convs := len(corpus.convs)
	k := exp.Config().K
	mod := corpus.bucketMod()
	inBucket := func(trace string, t int) bool {
		c, ok := corpus.traces[trace]
		return ok && c%benchTokenBuckets == t
	}
	checkHits := func(hits []otel.MessageSpanHit, want int, ok func(otel.MessageSpanHit) error) (int, error) {
		if len(hits) != want {
			return len(hits), fmt.Errorf("%d hits, want %d", len(hits), want)
		}
		seen := map[string]bool{}
		for _, h := range hits {
			key := h.Trace + "/" + h.Span + "/" + h.AttributeKey + "/" + h.MessageHash
			if seen[key] {
				return len(hits), fmt.Errorf("duplicate hit %s", key)
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
		out = append(out,
			benchQuery{QueryFTSSingleToken, func(ctx context.Context, i int) (int, error) {
				t := i % mod
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("incident_token_%d", t), Mode: otel.MessageMatchAll, Role: "user"})
				if err != nil {
					return 0, err
				}
				// User message j of each conversation is in the input of turns j..n.
				return checkHits(page.Hits, min(corpus.bucketSize(t)*n*(n+1)/2, k), func(h otel.MessageSpanHit) error {
					if h.Role != "user" || h.AttributeKey != benchInputKey || !inBucket(h.Trace, t) {
						return fmt.Errorf("unexpected hit %+v", h)
					}
					return nil
				})
			}},
			benchQuery{QueryFTSPhraseFirstTurn, func(ctx context.Context, i int) (int, error) {
				c, turn := i%convs, i%n+1
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("user turn %d conversation %d", turn, c), Mode: otel.MessageMatchPhrase, FirstTurnOnly: true})
				if err != nil {
					return 0, err
				}
				conv := corpus.convs[c]
				return checkHits(page.Hits, 1, func(h otel.MessageSpanHit) error {
					if h.Trace != conv.traceHex() || h.Span != conv.spanHex(turn) || h.AttributeKey != benchInputKey || h.Role != "user" {
						return fmt.Errorf("hit %+v, want conversation %d turn %d", h, c, turn)
					}
					return nil
				})
			}},
			benchQuery{QueryFTSMultiTurnHistory, func(ctx context.Context, i int) (int, error) {
				c := i % convs
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("user turn 1 conversation %d", c), Mode: otel.MessageMatchPhrase, AttributeKey: benchInputKey})
				if err != nil {
					return 0, err
				}
				conv := corpus.convs[c]
				return checkHits(page.Hits, min(n, k), func(h otel.MessageSpanHit) error {
					if h.Trace != conv.traceHex() || h.AttributeKey != benchInputKey || !slices.Equal(h.TurnOrdinals, []int{0}) {
						return fmt.Errorf("hit %+v outside conversation %d's first message", h, c)
					}
					return nil
				})
			}},
			benchQuery{QueryFTSSameSpanPreds, func(ctx context.Context, i int) (int, error) {
				t := i % mod
				page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("incident_token_%d", t), FirstTurnOnly: true, Predicates: []otel.Attribute{chat, model}})
				if err != nil {
					return 0, err
				}
				return checkHits(page.Hits, min(corpus.bucketSize(t)*n, k), func(h otel.MessageSpanHit) error {
					if h.Role != "user" || h.AttributeKey != benchInputKey || !inBucket(h.Trace, t) {
						return fmt.Errorf("unexpected hit %+v", h)
					}
					return nil
				})
			}},
		)
	}
	out = append(out,
		benchQuery{QuerySameSpanConjunction, func(ctx context.Context, i int) (int, error) {
			c := i % convs
			conv := corpus.convs[c]
			convID := otel.Attribute{Scope: "span", Key: "gen_ai.conversation.id", Type: "string", Value: fmt.Sprintf("conv-%s-%d", cfg.Run, c)}
			page, err := exp.QuerySameSpan(ctx, otel.SameSpanQuery{Predicates: []otel.Attribute{convID, chat, model}})
			if err != nil {
				return 0, err
			}
			if len(page.Spans) != min(n, k) {
				return len(page.Spans), fmt.Errorf("%d spans, want %d", len(page.Spans), min(n, k))
			}
			seen := map[string]bool{}
			for _, s := range page.Spans {
				if s.Trace != conv.traceHex() || seen[s.ID] || !slices.ContainsFunc(conv.spans, func(id []byte) bool { return hex.EncodeToString(id) == s.ID }) {
					return len(page.Spans), fmt.Errorf("unexpected span %+v for conversation %d", s, c)
				}
				seen[s.ID] = true
			}
			return len(page.Spans), nil
		}},
		benchQuery{QueryTraceExistential3, func(ctx context.Context, i int) (int, error) {
			t := i % mod
			q := otel.TraceExistentialQuery{
				Clauses:    []otel.Attribute{chat},
				LogClauses: []otel.Attribute{{Scope: otel.LogScopeRecord, Key: "incident.bucket", Type: "string", Value: fmt.Sprintf("bucket_%d", t)}},
			}
			if cfg.EnableMessageFullText {
				q.MessageClauses = []otel.MessageFullTextQuery{{Query: fmt.Sprintf("resolved_token_%d", t), Role: "assistant", FirstTurnOnly: true}}
			}
			res, err := exp.QueryTraceExistential(ctx, q)
			if err != nil {
				return 0, err
			}
			want := min(corpus.bucketSize(t), k)
			if len(res.Hits) != want {
				return len(res.Hits), fmt.Errorf("%d traces, want %d", len(res.Hits), want)
			}
			seen := map[string]bool{}
			for _, h := range res.Hits {
				if !inBucket(h.Trace, t) || seen[h.Trace] {
					return len(res.Hits), fmt.Errorf("unexpected trace %s for bucket %d", h.Trace, t)
				}
				seen[h.Trace] = true
				if len(h.Witnesses) != 1 || len(h.Witnesses[0]) == 0 || len(h.LogWitnesses) != 1 || len(h.LogWitnesses[0]) == 0 || len(h.MessageWitnesses) != len(q.MessageClauses) {
					return len(res.Hits), fmt.Errorf("trace %s lacks witnesses", h.Trace)
				}
				for _, w := range h.MessageWitnesses {
					if len(w) == 0 {
						return len(res.Hits), fmt.Errorf("trace %s lacks message witnesses", h.Trace)
					}
				}
			}
			return len(res.Hits), nil
		}},
		benchQuery{QueryTraceGraph, func(ctx context.Context, i int) (int, error) {
			c := i % convs
			conv := corpus.convs[c]
			g, err := exp.TraverseTraceGraph(ctx, conv.traceHex())
			if err != nil {
				return 0, err
			}
			if len(g.Spans) != n || len(g.Roots) != 1 || g.Roots[0] != conv.spanHex(1) || g.MaxDepth != n-1 || len(g.Orphans) != 0 || len(g.Cycles) != 0 {
				return len(g.Spans), fmt.Errorf("conversation %d graph: %d spans, roots %v, max depth %d, orphans %v, cycles %v", c, len(g.Spans), g.Roots, g.MaxDepth, g.Orphans, g.Cycles)
			}
			return len(g.Spans), nil
		}},
		benchQuery{QueryLogsByTrace, func(ctx context.Context, i int) (int, error) {
			c := i % convs
			conv := corpus.convs[c]
			logs, err := exp.QueryLogsByTrace(ctx, conv.traceHex(), "")
			if err != nil {
				return 0, err
			}
			if len(logs) != 1 || logs[0].EventID != corpus.logs[c].EventID || logs[0].SpanID != conv.spanHex(1) {
				return len(logs), fmt.Errorf("conversation %d: %d correlated logs, want its one log", c, len(logs))
			}
			return len(logs), nil
		}},
	)
	return out
}
