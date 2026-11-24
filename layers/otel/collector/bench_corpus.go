package collector

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"fdb.dev/layers/otel"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

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
