package collector

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestCollectorBenchGenerator(t *testing.T) {
	t.Parallel()
	cfg := benchUnitConfig().WithDefaults()
	const n, convs = 5, 6
	gen, again := newBenchGen(cfg), newBenchGen(cfg)
	var raw int64
	var requests []*tracepb.ResourceSpans
	spanIDs := map[string]bool{}
	for c := range convs {
		req := gen.request(c)
		if !proto.Equal(req, again.request(c)) {
			t.Fatalf("conversation %d is not deterministic", c)
		}
		trace := gen.traceID(c)
		if got, ok := benchConversationOf(trace); !ok || got != c {
			t.Fatalf("trace %x decodes to conversation %d, %v; want %d", trace, got, ok, c)
		}
		raw += int64(proto.Size(req))
		requests = append(requests, req)
		spans := req.ScopeSpans[0].Spans
		if len(spans) != n {
			t.Fatalf("conversation %d has %d spans", c, len(spans))
		}
		for i, sp := range spans {
			k := i + 1
			spanIDs[hex.EncodeToString(sp.SpanId)] = true
			if !slices.Equal(sp.TraceId, trace) || !slices.Equal(sp.SpanId, gen.spanID(c, k)) {
				t.Fatalf("conversation %d turn %d identity", c, k)
			}
			if k == 1 && len(sp.ParentSpanId) != 0 || k > 1 && !slices.Equal(sp.ParentSpanId, gen.spanID(c, k-1)) {
				t.Fatalf("conversation %d turn %d parent %x", c, k, sp.ParentSpanId)
			}
			if benchAttr(t, sp, "gen_ai.turn.number").GetIntValue() != int64(k) || benchAttr(t, sp, "gen_ai.conversation.id").GetStringValue() != fmt.Sprintf("conv-bench_unit_run-%d", c) ||
				benchAttr(t, sp, "gen_ai.request.model").GetStringValue() != "gpt-4o" || benchAttr(t, sp, "gen_ai.operation.name").GetStringValue() != "chat" ||
				benchAttr(t, sp, "incident.block").GetStringValue() != "block_0" {
				t.Fatalf("conversation %d turn %d attributes %v", c, k, sp.Attributes)
			}
			if got := benchSpanMessages(sp); got != int64(2*k+3) {
				t.Fatalf("conversation %d turn %d counts %d messages, want %d", c, k, got, 2*k+3)
			}
			input := benchAttr(t, sp, benchInputKey).GetArrayValue().GetValues()
			output := benchAttr(t, sp, benchOutputKey).GetArrayValue().GetValues()
			if len(input) != 2*k-1 || len(output) != 1 || len(benchAttr(t, sp, "gen_ai.tool.definitions").GetArrayValue().GetValues()) != 2 ||
				len(benchAttr(t, sp, "gen_ai.system_instructions").GetArrayValue().GetValues()) != 1 {
				t.Fatalf("conversation %d turn %d message shape", c, k)
			}
			if role, text := benchMessageText(t, input[len(input)-1]); role != "user" || text != BenchUserText(cfg.Seed, c, k) || len(text) < benchFillerBytes ||
				!strings.Contains(text, fmt.Sprintf(" conversation %d conv_%d incident_token_0 ", c, c)) {
				t.Fatalf("conversation %d turn %d user message %q/%q", c, k, role, text)
			}
			if role, text := benchMessageText(t, output[0]); role != "assistant" || text != BenchAssistantText(cfg.Seed, c, k) || !strings.Contains(text, " resolved_token_0:") {
				t.Fatalf("conversation %d turn %d reply %q/%q", c, k, role, text)
			}
			if k > 1 {
				prev := spans[i-1]
				prevInput := benchAttr(t, prev, benchInputKey).GetArrayValue().GetValues()
				if !proto.Equal(input[len(input)-2], benchAttr(t, prev, benchOutputKey).GetArrayValue().GetValues()[0]) || !proto.Equal(&commonpb.ArrayValue{Values: input[:len(prevInput)]}, &commonpb.ArrayValue{Values: prevInput}) {
					t.Fatalf("conversation %d turn %d does not extend turn %d's history", c, k, k-1)
				}
			}
			if single := benchSingle(req, k); len(single.ScopeSpans) != 1 || len(single.ScopeSpans[0].Spans) != 1 || !proto.Equal(single.ScopeSpans[0].Spans[0], sp) || !proto.Equal(single.Resource, req.Resource) {
				t.Fatalf("conversation %d turn %d single projection", c, k)
			}
		}
		l := gen.log(c)
		if l.TraceID != hex.EncodeToString(trace) || l.SpanID != gen.spanHex(c, 1) || l.EventID != fmt.Sprintf("bench_unit_run-log-%d", c) ||
			benchStringAttr(t, l.Attributes, "incident.bucket") != "bucket_0" || !proto.Equal(l.Body, again.log(c).Body) {
			t.Fatalf("conversation %d log %+v", c, l)
		}
	}
	want := CollectorBenchCorpus{
		Conversations: convs, TurnsPerConversation: n, Spans: convs * n, Logs: convs, RawInputBytes: raw,
		TotalMessageElements: int64(convs * (n*(n+1) + 3*n)), UniqueMessageElements: int64(2*convs*n + 3),
	}
	if got := benchCorpusStats(n, convs, raw); got != want || len(spanIDs) != convs*n {
		t.Fatalf("corpus stats %+v, want %+v (%d span IDs)", got, want, len(spanIDs))
	}
	if got := benchCorpusStats(n, 0, 0); got != (CollectorBenchCorpus{TurnsPerConversation: n}) {
		t.Fatalf("empty corpus stats %+v", got)
	}
	// The elements are what the exporter stores once each.
	var total int64
	unique := map[string]bool{}
	for _, req := range requests {
		for _, sp := range req.ScopeSpans[0].Spans {
			for _, kv := range sp.Attributes {
				if !otel.IsMessageDeltaAttribute(kv.Key) {
					continue
				}
				for _, el := range kv.Value.GetArrayValue().GetValues() {
					wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(el)
					if err != nil {
						t.Fatal(err)
					}
					total++
					unique[string(wire)] = true
				}
			}
		}
	}
	if total != want.TotalMessageElements || int64(len(unique)) != want.UniqueMessageElements {
		t.Fatalf("elements %d total / %d unique, want %d / %d", total, len(unique), want.TotalMessageElements, want.UniqueMessageElements)
	}
	// Filler never contains a query token, so answers are decided by prefixes.
	for _, s := range benchSentences {
		lower := strings.ToLower(s)
		for _, bad := range []string{"user", "turn", "conversation", "conv", "assistant", "token", "resolved", "block", "bucket"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("filler %q contains %q", s, bad)
			}
		}
		if strings.ContainsAny(strings.ReplaceAll(strings.ReplaceAll(s, "p99", ""), "hel1", ""), "0123456789") {
			t.Fatalf("filler %q contains a number", s)
		}
	}
}

func TestCollectorBenchBlocksAndIDs(t *testing.T) {
	t.Parallel()
	for c, b := range map[int]int{0: 0, 49: 0, 50: 1, 99: 1, 100: 2, 123_456: 2469} {
		if got := benchBlock(c); got != b {
			t.Fatalf("block(%d) = %d, want %d", c, got, b)
		}
	}
	for _, tc := range []struct{ b, total, want int }{{0, 120, 50}, {1, 120, 50}, {2, 120, 20}, {3, 120, 0}, {0, 6, 6}, {1, 6, 0}, {0, 0, 0}} {
		if got := benchBlockMembers(tc.b, tc.total); got != tc.want {
			t.Fatalf("members(%d, %d) = %d, want %d", tc.b, tc.total, got, tc.want)
		}
	}
	if s := benchSamples(120); len(s) != 16 || s[0] != 0 || s[1] != 7 || s[15] != 112 {
		t.Fatalf("samples %v", s)
	}
	if s := benchSamples(6); !slices.Equal(s, []int{0, 1, 2, 3, 4, 5}) || len(benchSamples(0)) != 0 {
		t.Fatalf("small samples %v", s)
	}
	for i := range 12 {
		if got := benchSpread(i, 12, 6); got != i/2 {
			t.Fatalf("spread(%d, 12, 6) = %d", i, got)
		}
	}
	prev := -1
	for i := range 100 {
		got := benchSpread(i, 100, 800_000)
		if got <= prev || got >= 800_000 {
			t.Fatalf("spread(%d, 100, 800000) = %d after %d", i, got, prev)
		}
		prev = got
	}
	if benchSpread(3, 4, 1) != 0 || benchSpread(14, 12, 6) != 1 {
		t.Fatal("spread wraps")
	}

	gen := newBenchGen(benchUnitConfig().WithDefaults())
	seen := map[string]bool{}
	for _, c := range []int{0, 1, 49, 50, 12_345_678, maxBenchConversations - 1} {
		id := gen.traceID(c)
		if len(id) != 16 || binary.BigEndian.Uint64(id[:8]) != uint64(c) || !slices.ContainsFunc(id[8:], func(b byte) bool { return b != 0 }) {
			t.Fatalf("trace ID %x of conversation %d", id, c)
		}
		if got, ok := benchConversationOf(id); !ok || got != c {
			t.Fatalf("decode %x = %d, %v", id, got, ok)
		}
		if seen[hex.EncodeToString(id)] || len(gen.spanID(c, 1)) != 8 || slices.Equal(gen.spanID(c, 1), gen.spanID(c, 2)) {
			t.Fatalf("conversation %d identities collide", c)
		}
		seen[hex.EncodeToString(id)] = true
	}
	over := make([]byte, 16)
	binary.BigEndian.PutUint64(over, maxBenchConversations)
	if _, ok := benchConversationOf(over); ok {
		t.Fatal("decoded a conversation beyond the cap")
	}
	if _, ok := benchConversationOf(make([]byte, 15)); ok {
		t.Fatal("decoded a short trace ID")
	}

	// The selective terms survive query tokenization as single tokens.
	toks, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(BenchUserText(11, 123, 2), 0, recordlayer.TokenizerModeQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toks, "conv_123") || !slices.Contains(toks, "incident_token_2") || slices.Contains(toks, "conv") {
		t.Fatalf("user text tokens %v", toks)
	}
	phrase, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList("user turn 2 conversation 123 conv_123", 0, recordlayer.TokenizerModeQuery)
	if err != nil || !slices.Equal(phrase, toks[:6]) {
		t.Fatalf("phrase tokens %v, %v; text starts %v", phrase, err, toks[:6])
	}
}

func TestBenchTracker(t *testing.T) {
	t.Parallel()
	var claimed atomic.Int64
	claimed.Store(5)
	tr := newBenchTracker(3, &claimed)
	check := func(step string, wantW int64, wantTracked int, wantAnomalies int64) {
		t.Helper()
		tracked, _, anomalies := tr.state()
		if tr.Watermark() != wantW || tracked != wantTracked || anomalies != wantAnomalies {
			t.Fatalf("%s: watermark %d tracked %d anomalies %d, want %d %d %d", step, tr.Watermark(), tracked, anomalies, wantW, wantTracked, wantAnomalies)
		}
	}
	check("empty", -1, 0, 0)
	tr.spansExported(map[int]int{1: 3})
	tr.logsWritten([]int{1})
	check("conversation 1 complete before 0", -1, 2, 0)
	tr.spansExported(map[int]int{0: 2})
	tr.logsWritten([]int{0})
	check("conversation 0 lacks a span", -1, 2, 0)
	tr.spansExported(map[int]int{0: 1, 2: 3})
	check("0 and 1 complete", 1, 1, 0)
	tr.spansExported(map[int]int{0: 1})
	check("already committed", 1, 1, 1)
	tr.spansExported(map[int]int{5: 1, -1: 1})
	check("unclaimed and undecodable", 1, 1, 3)
	tr.spansExported(map[int]int{3: 4})
	check("over-full", 1, 2, 4)
	tr.logsWritten([]int{2, 2})
	check("duplicate log", 2, 1, 5)
	tr.spansExported(map[int]int{3: 3, 4: 3})
	tr.logsWritten([]int{4, 3})
	check("all committed", 4, 0, 5)
	if _, peak, _ := tr.state(); peak != 3 {
		t.Fatalf("peak tracked %d, want 3", peak)
	}
	if size := reflect.TypeFor[benchCommit]().Size(); size != 2 {
		t.Fatalf("benchCommit is %d bytes, want 2", size)
	}
}

// retains reports a path from typ to a forbidden type, not descending into skip.
func retains(typ reflect.Type, forbidden, skip []reflect.Type) string {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string) string
	walk = func(t reflect.Type, path string) string {
		if slices.Contains(forbidden, t) {
			return path
		}
		if seen[t] || slices.Contains(skip, t) {
			return ""
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			return walk(t.Elem(), path+"[]")
		case reflect.Map:
			if p := walk(t.Key(), path+"{key}"); p != "" {
				return p
			}
			return walk(t.Elem(), path+"{}")
		case reflect.Struct:
			for i := range t.NumField() {
				f := t.Field(i)
				if p := walk(f.Type, path+"."+f.Name); p != "" {
					return p
				}
			}
		}
		return ""
	}
	return walk(typ, typ.Name())
}

// The ingest state retains no request, span or log; the Collector queue and
// the exporter are the bounded buffers under test and are not walked.
func TestBenchRunRetainsNoCorpus(t *testing.T) {
	t.Parallel()
	forbidden := []reflect.Type{reflect.TypeFor[tracepb.ResourceSpans](), reflect.TypeFor[tracepb.ScopeSpans](), reflect.TypeFor[tracepb.Span](), reflect.TypeFor[otel.LogRecord]()}
	skip := []reflect.Type{reflect.TypeFor[TracesExporter](), reflect.TypeFor[otel.Exporter]()}
	if p := retains(reflect.TypeFor[benchRun](), forbidden, skip); p != "" {
		t.Fatalf("benchRun retains corpus data at %s", p)
	}
	// Positive controls: the walker finds retention through every container
	// kind, and the skipped queue does hold requests.
	for name, typ := range map[string]reflect.Type{
		"slice": reflect.TypeFor[struct{ reqs []*tracepb.ResourceSpans }](),
		"map":   reflect.TypeFor[struct{ logs map[int]otel.LogRecord }](),
		"chan":  reflect.TypeFor[struct{ ch chan *tracepb.Span }](),
		"nested": reflect.TypeFor[struct {
			c *struct{ s [2]*tracepb.ScopeSpans }
		}](),
		"queue": reflect.TypeFor[TracesExporter](),
	} {
		if retains(typ, forbidden, nil) == "" {
			t.Errorf("%s: retention not detected", name)
		}
	}
	if p := retains(reflect.TypeFor[struct{ x *TracesExporter }](), forbidden, skip); p != "" {
		t.Fatalf("skipped queue reported at %s", p)
	}
}

func TestBenchQueryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		turns, k   int
		candidates int
		bytes      int64
	}{
		// spans = 50n; chunks = 1 + (2n+3)*48/8000; rows = spans(4+chunks) + 50 + 4(k+64) + 16;
		// bytes = 512 rows + 50n(n+4)*48.
		{5, 100, 2*(250*5+50+656+16) + 1000, 2*(1972*512+50*5*9*48) + 1<<20},
		{15, 100, 2*(750*5+50+656+16) + 1000, 2*(4472*512+50*15*19*48) + 1<<20},
		{1, 10, 2*(50*5+50+296+16) + 1000, 2*(612*512+50*1*5*48) + 1<<20},
		// 403 references per span at the last turn take three chunks.
		{200, 100, 2*(10000*7+50+656+16) + 1000, 2*(70722*512+50*200*204*48) + 1<<20},
	} {
		c, b := benchQueryBudget(tc.turns, tc.k)
		if c != tc.candidates || b != tc.bytes {
			t.Fatalf("budget(%d, %d) = %d, %d; want %d, %d", tc.turns, tc.k, c, b, tc.candidates, tc.bytes)
		}
	}
	// The budget is independent of the corpus size and the load shape.
	small := benchUnitConfig().WithDefaults()
	large := small
	large.Conversations, large.Duration, large.Warmup = maxBenchConversations, time.Hour, time.Minute
	cs, cl := small.collectorConfig().Exporter, large.collectorConfig().Exporter
	if cs.MaxCandidates != 4944 || cs.MaxBytes != 3_283_904 || cl.MaxCandidates != cs.MaxCandidates || cl.MaxBytes != cs.MaxBytes || cs.K != 100 {
		t.Fatalf("budgets %d/%d and %d/%d", cs.MaxCandidates, cs.MaxBytes, cl.MaxCandidates, cl.MaxBytes)
	}
	if err := large.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBenchRates(t *testing.T) {
	t.Parallel()
	s := int64(time.Second)
	windows := []CollectorBenchWindow{
		{Phase: benchPhaseLoad, StartNs: 0, EndNs: s, ExportedSpans: 10, Messages: 20, RawInputBytes: 100, Logs: 1},
		{Phase: benchPhaseLoad, StartNs: s, EndNs: 3 * s, ExportedSpans: 40, Messages: 80, RawInputBytes: 400, Logs: 4},
		{Phase: benchPhaseDrain, StartNs: 3 * s, EndNs: 4 * s, ExportedSpans: 6, Messages: 12, RawInputBytes: 60, Logs: 2},
	}
	r := benchRates(windows, time.Second)
	if r.WholeRun != (CollectorBenchRate{Windows: 3, WallNs: 4 * s, Spans: 56, SpansPerSec: 14, MessagesPerSec: 28, RawBytesPerSec: 140, LogsPerSec: 1.75}) {
		t.Fatalf("whole run %+v", r.WholeRun)
	}
	if r.SteadyState != (CollectorBenchRate{Windows: 1, WallNs: 2 * s, Spans: 40, SpansPerSec: 20, MessagesPerSec: 40, RawBytesPerSec: 200, LogsPerSec: 2}) {
		t.Fatalf("steady state %+v", r.SteadyState)
	}
	if r := benchRates(windows, 0); r.SteadyState.Windows != 2 || r.SteadyState.Spans != 50 {
		t.Fatalf("no warmup %+v", r.SteadyState)
	}
	if r := benchRates(windows, 10*time.Second); r.SteadyState != (CollectorBenchRate{}) {
		t.Fatalf("warmup past the load %+v", r.SteadyState)
	}
	if r := benchRates(nil, 0); r != (CollectorBenchRates{}) {
		t.Fatalf("no windows %+v", r)
	}
}

func TestBenchCPU(t *testing.T) {
	t.Parallel()
	ms := int64(time.Millisecond)
	prev := benchCPUSample{user: 100 * ms, sys: 50 * ms, gc: 10 * ms, assist: 1 * ms, dedicated: 2 * ms, idle: 3 * ms, pause: ms / 2}
	cur := benchCPUSample{user: 2100 * ms, sys: 1050 * ms, gc: 640 * ms, assist: 101 * ms, dedicated: 302 * ms, idle: 203 * ms, pause: ms}
	// 3 s of CPU over 2 s wall on 4 Ps, 1500 spans.
	c := benchCPUDelta(prev, cur, 2000*ms, 1500, 4, 8)
	want := CollectorBenchCPU{
		UserNs: 2000 * ms, SysNs: 1000 * ms, TotalNs: 3000 * ms,
		GCNs: 630 * ms, GCAssistNs: 100 * ms, GCDedicatedNs: 300 * ms, GCIdleNs: 200 * ms, GCPauseNs: ms / 2,
		CoresUsed: 1.5, Utilization: 0.375, MsPerSpan: 2, GCFraction: 0.21, GOMAXPROCS: 4, NumCPU: 8,
	}
	if c != want {
		t.Fatalf("delta %+v, want %+v", c, want)
	}
	if z := benchCPUDelta(cur, cur, 0, 0, 4, 8); z != (CollectorBenchCPU{GOMAXPROCS: 4, NumCPU: 8}) {
		t.Fatalf("empty interval %+v", z)
	}
	// Utilization is over min(GOMAXPROCS, NumCPU): 16 Ps on 4 CPUs is 4 cores.
	if u := benchCPUDelta(prev, cur, 2000*ms, 1500, 16, 4).Utilization; u != 0.375 {
		t.Fatalf("GOMAXPROCS above NumCPU: utilization %v", u)
	}
	for _, tc := range []struct{ gomaxprocs, numCPU, want int }{
		{4, 8, 4}, {16, 4, 4}, {6, 6, 6}, {0, 4, 4}, {4, 0, 4}, {0, 0, 0}, {-1, -1, 0},
	} {
		if got := benchCPUProcs(tc.gomaxprocs, tc.numCPU); got != tc.want {
			t.Fatalf("benchCPUProcs(%d, %d) = %d, want %d", tc.gomaxprocs, tc.numCPU, got, tc.want)
		}
	}
	if u := benchCPUDelta(prev, cur, 2000*ms, 1500, 0, 0).Utilization; u != 0 {
		t.Fatalf("unknown processors: utilization %v", u)
	}

	// Rates sum the windows' counters and derive over their wall and spans.
	s := int64(time.Second)
	windows := []CollectorBenchWindow{
		{Phase: benchPhaseLoad, StartNs: 0, EndNs: s, ExportedSpans: 100, CPU: benchCPUDelta(benchCPUSample{}, benchCPUSample{user: s, gc: s / 10}, s, 100, 4, 8)},
		{Phase: benchPhaseLoad, StartNs: s, EndNs: 3 * s, ExportedSpans: 1000, CPU: benchCPUDelta(benchCPUSample{}, benchCPUSample{user: 5 * s, sys: s, gc: s}, 2*s, 1000, 4, 8)},
		{Phase: benchPhaseDrain, StartNs: 3 * s, EndNs: 4 * s, ExportedSpans: 0, CPU: benchCPUDelta(benchCPUSample{}, benchCPUSample{user: s}, s, 0, 4, 8)},
	}
	r := benchRates(windows, time.Second)
	if got, want := r.SteadyState.CPU, (CollectorBenchCPU{UserNs: 5 * s, SysNs: s, TotalNs: 6 * s, GCNs: s, CoresUsed: 3, Utilization: 0.75, MsPerSpan: 6, GCFraction: 1.0 / 6, GOMAXPROCS: 4, NumCPU: 8}); got != want {
		t.Fatalf("steady CPU %+v, want %+v", got, want)
	}
	if got, want := r.WholeRun.CPU, (CollectorBenchCPU{UserNs: 7 * s, SysNs: s, TotalNs: 8 * s, GCNs: s + s/10, CoresUsed: 2, Utilization: 0.5, MsPerSpan: 8000.0 / 1100, GCFraction: 1.1 / 8, GOMAXPROCS: 4, NumCPU: 8}); got != want {
		t.Fatalf("whole-run CPU %+v, want %+v", got, want)
	}

	// The live sample moves forward and reports the runtime's processors.
	c, next := benchCPUWindow(benchCPUSample{}, s, 1)
	if next.user+next.sys <= 0 || c.TotalNs != next.user+next.sys || c.GOMAXPROCS != runtime.GOMAXPROCS(0) || c.NumCPU != runtime.NumCPU() {
		t.Fatalf("live sample %+v from %+v", c, next)
	}
}

func TestCollectorBenchConfigValidate(t *testing.T) {
	t.Parallel()
	want := DefaultCollectorBenchConfig()
	want.ClusterFile, want.Run = "/c", "bench_unit_run"
	if got := (CollectorBenchConfig{ClusterFile: "/c", Run: "bench_unit_run", EnableMessageDeltas: true, EnableMessageFullText: true}).WithDefaults(); got != want {
		t.Fatalf("WithDefaults = %+v, want %+v", got, want)
	}
	if want.Duration != 0 || want.ReportInterval != 10*time.Second || want.Warmup != time.Minute || want.QueryWorkers != 2 || want.NumConsumers != 4 {
		t.Fatalf("load defaults %+v", want)
	}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := benchUnitConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	sustained := benchUnitConfig()
	sustained.Duration, sustained.QueryWorkers, sustained.NumConsumers = maxBenchDuration, maxBenchQueryWorkers, 16
	if err := sustained.Validate(); err != nil {
		t.Fatal(err)
	}
	cc := benchUnitConfig().WithDefaults().collectorConfig()
	if q := cc.SendingQueue; !q.Enabled || !q.BlockOnOverflow || q.NumConsumers != 1 || q.QueueSize != 1000 || q.Batch.MinSize != 4 || q.Batch.MaxSize != 16 {
		t.Fatalf("sending queue %+v", q)
	}
	if e := cc.Exporter; !e.EnableMessageDeltas || !e.EnableMessageFullText || e.BatchSize != 8 || e.Workers != 4 || e.Tenant != 3 || e.RecordCompression != "zlib-1" {
		t.Fatalf("exporter %+v", e)
	}
	for name, mutate := range map[string]func(*CollectorBenchConfig){
		"cluster file":    func(c *CollectorBenchConfig) { c.ClusterFile = "" },
		"run":             func(c *CollectorBenchConfig) { c.Run = "BAD" },
		"conversations":   func(c *CollectorBenchConfig) { c.Conversations = -1 },
		"many convs":      func(c *CollectorBenchConfig) { c.Conversations = maxBenchConversations + 1 },
		"turns":           func(c *CollectorBenchConfig) { c.TurnsPerConversation = maxBenchTurns + 1 },
		"duration":        func(c *CollectorBenchConfig) { c.Duration = -time.Second },
		"long duration":   func(c *CollectorBenchConfig) { c.Duration = maxBenchDuration + time.Second },
		"report interval": func(c *CollectorBenchConfig) { c.ReportInterval = time.Millisecond },
		"long interval":   func(c *CollectorBenchConfig) { c.ReportInterval = maxBenchDuration + time.Second },
		"warmup":          func(c *CollectorBenchConfig) { c.Warmup = -time.Second },
		"warmup past run": func(c *CollectorBenchConfig) { c.Duration, c.Warmup = 5*time.Second, 5*time.Second },
		"default warmup":  func(c *CollectorBenchConfig) { c.Duration = 30 * time.Second },
		"producers":       func(c *CollectorBenchConfig) { c.Producers = -1 },
		"query workers":   func(c *CollectorBenchConfig) { c.QueryWorkers = -1 },
		"many workers":    func(c *CollectorBenchConfig) { c.QueryWorkers = maxBenchQueryWorkers + 1 },
		"iterations":      func(c *CollectorBenchConfig) { c.QueryIterations = -1 },
		"queue":           func(c *CollectorBenchConfig) { c.QueueSize = 4 },
		"batch min":       func(c *CollectorBenchConfig) { c.BatchMinSize = 2000 },
		"batch max":       func(c *CollectorBenchConfig) { c.BatchMaxSize = 2 },
		"consumers":       func(c *CollectorBenchConfig) { c.NumConsumers = maxConsumers + 1 },
		"exporter batch":  func(c *CollectorBenchConfig) { c.ExporterBatchSize = 65 },
		"workers":         func(c *CollectorBenchConfig) { c.ExporterWorkers = 17 },
		"compression":     func(c *CollectorBenchConfig) { c.RecordCompression = "lz4" },
		"brotli":          func(c *CollectorBenchConfig) { c.RecordCompression = "brotli-4" },
		"query only run":  func(c *CollectorBenchConfig) { c.QueryOnly, c.Duration = true, 30*time.Second },
		"query class":     func(c *CollectorBenchConfig) { c.QueryClasses = QueryFTSSingleToken + ",fts_bogus" },
	} {
		cfg := benchUnitConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
		r, err := RunBenchmark(t.Context(), cfg)
		if err == nil || r.Error == "" || r.Tool != CollectorBenchTool || r.Mode != "collector-bench" || r.Config != cfg.WithDefaults() || len(r.Queries) != 0 || len(r.Series) != 0 {
			t.Errorf("%s: RunBenchmark = %+v, %v", name, r, err)
		}
	}
}

func TestAddLogStats(t *testing.T) {
	t.Parallel()
	one := otel.LogExportStats{ReceivedLogs: 1, DuplicateLogs: 2, GeneratedIDs: 3, Logs: 4, Batches: 5, Replaced: 6, Correlated: 7, LogicalBytes: 8, StoredBytes: 9, UnindexedAttributes: 10, EventIDs: []string{"a"}}
	var sum otel.LogExportStats
	addLogStats(&sum, one)
	addLogStats(&sum, one)
	want := otel.LogExportStats{ReceivedLogs: 2, DuplicateLogs: 4, GeneratedIDs: 6, Logs: 8, Batches: 10, Replaced: 12, Correlated: 14, LogicalBytes: 16, StoredBytes: 18, UnindexedAttributes: 20}
	if !reflect.DeepEqual(sum, want) {
		t.Fatalf("sum %+v, want %+v", sum, want)
	}
}

func TestBenchHistogram(t *testing.T) {
	t.Parallel()
	if h := benchHistogram(nil); h != (otel.Histogram{}) {
		t.Fatalf("empty histogram %+v", h)
	}
	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(100 - i)
	}
	if h := benchHistogram(samples); h != (otel.Histogram{Population: 100, Min: 1, P50: 50, P95: 95, P99: 99, Max: 100}) {
		t.Fatalf("histogram %+v", h)
	}
}

func TestAddExportStats(t *testing.T) {
	t.Parallel()
	one := otel.ExportStats{
		ReceivedSpans: 1, DuplicateSpans: 2, Spans: 3, Batches: 4, LogicalBytes: 5, NormalizedSpans: 6, UnindexedAttributes: 7,
		MessageDeltas: otel.MessageDeltaStats{TotalMessages: 8, UniqueMessages: 9, ReusedMessages: 10, RawMessageBytes: 11, StoredDeltaBytes: 12, SavedBytes: 13},
		Messages:      otel.MessageStoreStats{Written: 14, Present: 15, Parts: 16, Postings: 17, UnindexedTokens: 18, References: 19, Transactions: 20, Retries: 21, TextIndexWrites: 22, TextIndexKeyBytes: 23, TextIndexValueBytes: 24, TextIndexReadBytes: 25, WallNs: 26, TxnNs: 27},
	}
	var sum otel.ExportStats
	addExportStats(&sum, one)
	addExportStats(&sum, one)
	want := otel.ExportStats{
		ReceivedSpans: 2, DuplicateSpans: 4, Spans: 6, Batches: 8, LogicalBytes: 10, NormalizedSpans: 12, UnindexedAttributes: 14,
		MessageDeltas: otel.MessageDeltaStats{TotalMessages: 16, UniqueMessages: 18, ReusedMessages: 20, RawMessageBytes: 22, StoredDeltaBytes: 24, SavedBytes: 26},
		Messages:      otel.MessageStoreStats{Written: 28, Present: 30, Parts: 32, Postings: 34, UnindexedTokens: 36, References: 38, Transactions: 40, Retries: 42, TextIndexWrites: 44, TextIndexKeyBytes: 46, TextIndexValueBytes: 48, TextIndexReadBytes: 50, WallNs: 52, TxnNs: 54},
	}
	if sum != want {
		t.Fatalf("sum %+v, want %+v", sum, want)
	}
}

func TestRunBenchmarkRealFDB(t *testing.T) {
	cluster := benchClusterFile(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	cfg := benchUnitConfig()
	cfg.ClusterFile, cfg.Run = cluster, freshCollectorRun(t)
	const convs, n, iters = 6, 5, 12
	total, unique := convs*(n*(n+1)+3*n), 2*convs*n+3
	r, err := RunBenchmark(ctx, cfg)
	if err != nil || r.Error != "" || r.Failures != 0 {
		t.Fatalf("RunBenchmark: %v (report error %q, failures %d, fidelity %+v, queries %+v, under load %+v)", err, r.Error, r.Failures, r.Fidelity, r.Queries, r.UnderLoad)
	}
	if r.Corpus.Spans != convs*n || r.Corpus.TotalMessageElements != int64(total) || r.Corpus.UniqueMessageElements != int64(unique) || r.Corpus.RawInputBytes == 0 {
		t.Fatalf("corpus %+v", r.Corpus)
	}
	if r.Effective != (CollectorBenchEffective{Load: "fixed", Duration: "0s", ReportInterval: "10s", Warmup: "1m0s", Producers: 3, NumConsumers: 1, QueryWorkers: 2, BlockSize: benchBlockSize}) {
		t.Fatalf("effective %+v", r.Effective)
	}
	in := r.Ingest
	if s := in.Collector; s.AcceptedRequests != convs || s.AcceptedSpans != convs*n || s.ExportedSpans != convs*n || s.StoredSpans != convs*n ||
		s.RejectedSpans != 0 || s.FailedSpans != 0 || s.QueuedSpans != 0 || s.Flushes < 2 || s.Exports < s.Flushes {
		t.Fatalf("collector stats %+v", s)
	}
	if in.IngestWallNs <= 0 || in.SpansPerSec <= 0 || in.MessagesPerSec <= 0 || in.FailedExportCalls != 0 || in.LogCalls < 1 || in.FailedLogCalls != 0 || in.LogWallNs <= 0 {
		t.Fatalf("ingest %+v", in)
	}
	if e := in.Export; e.ReceivedSpans != convs*n || e.Spans != convs*n || e.MessageDeltas.TotalMessages != int64(total) || e.MessageDeltas.ReusedMessages <= 0 ||
		e.MessageDeltas.SavedBytes <= 0 || e.MessageDeltas.RawMessageBytes <= e.MessageDeltas.StoredDeltaBytes {
		t.Fatalf("export stats %+v", e)
	}
	// One consumer exports batches serially, so every unique message is
	// stored once; every span references each of its elements once.
	if f := in.Export.Messages; f.Written != int64(unique) || f.References != int64(total) || f.Postings == 0 || f.TextIndexWrites == 0 {
		t.Fatalf("message stats %+v", f)
	}
	if in.Logs.Logs != convs || in.Logs.Correlated != convs || in.Logs.EventIDs != nil {
		t.Fatalf("log stats %+v", in.Logs)
	}
	if r.Watermark != (CollectorBenchWatermark{Conversations: convs, Watermark: convs - 1, PeakTracked: r.Watermark.PeakTracked}) || r.Watermark.PeakTracked < 1 {
		t.Fatalf("watermark %+v", r.Watermark)
	}
	checkBenchSeries(t, r)
	if r.Fidelity.CheckedSpans != 2*convs || r.Fidelity.Mismatches != 0 {
		t.Fatalf("fidelity %+v", r.Fidelity)
	}
	// One block holds all six conversations.
	wantRows := map[string]int64{
		QueryFTSSingleToken:      iters * convs * n * (n + 1) / 2,
		QueryFTSPhraseFirstTurn:  iters,
		QueryFTSMultiTurnHistory: iters * n,
		QueryFTSSameSpanPreds:    iters * convs * n,
		QuerySameSpanConjunction: iters * n,
		QueryTraceExistential3:   iters * convs,
		QueryTraceGraph:          iters * n,
		QueryLogsByTrace:         iters,
	}
	if len(r.Queries) != len(CollectorBenchQueryClasses) || len(r.UnderLoad.Queries) != len(CollectorBenchQueryClasses) {
		t.Fatalf("query classes %v, under load %v", r.Queries, r.UnderLoad.Queries)
	}
	for _, class := range CollectorBenchQueryClasses {
		q, ok := r.Queries[class]
		if !ok || q.Iterations != iters || q.Failures != 0 || q.Error != "" || q.Rows != wantRows[class] || q.Histogram.Population != iters ||
			q.Histogram.Min <= 0 || q.Histogram.Max < q.Histogram.P99 || q.QPS <= 0 {
			t.Fatalf("%s: %+v, want %d rows", class, q, wantRows[class])
		}
		// Fewer than one block commits during a fixed run, so only
		// conversation classes may run under load; none may fail.
		if u := r.UnderLoad.Queries[class]; u.Failures != 0 || u.Histogram.Population != u.Iterations {
			t.Fatalf("%s under load: %+v", class, u)
		}
	}

	// Re-running on the same run is an idempotent replace: nothing is
	// stored again and every answer is unchanged.
	cfg.QueryIterations = 1
	again, err := RunBenchmark(ctx, cfg)
	if err != nil || again.Failures != 0 {
		t.Fatalf("re-run: %v, %+v", err, again)
	}
	if f := again.Ingest.Export.Messages; f.Written != 0 || f.Present < int64(unique) || f.TextIndexWrites != 0 || f.References != int64(total) {
		t.Fatalf("re-run message stats %+v", f)
	}
	if again.Ingest.Collector.StoredSpans != convs*n || again.Ingest.Logs.Replaced != convs || again.Fidelity.Mismatches != 0 {
		t.Fatalf("re-run ingest %+v fidelity %+v", again.Ingest, again.Fidelity)
	}

	// QueryOnly reruns only the named classes on the stored corpus, writes
	// nothing, and reports each iteration's reads and client requests.
	only := cfg
	only.QueryOnly, only.QueryClasses, only.QueryIterations = true, QueryFTSSingleToken+","+QueryTraceGraph, 3
	qo, err := RunBenchmark(ctx, only)
	if err != nil || qo.Failures != 0 || len(qo.Queries) != 2 || qo.Ingest.Collector.AcceptedSpans != 0 || qo.Fidelity.CheckedSpans != 0 || len(qo.Series) != 0 {
		t.Fatalf("query-only run: %v, %+v", err, qo)
	}
	for class, rows := range map[string]int64{QueryFTSSingleToken: 3 * convs * n * (n + 1) / 2, QueryTraceGraph: 3 * n} {
		q := qo.Queries[class]
		if q.Rows != rows || len(q.Samples) != 3 {
			t.Fatalf("query-only %s: %+v, want %d rows in 3 samples", class, q, rows)
		}
		for i, s := range q.Samples {
			if s.Iteration != i || s.Ns <= 0 || s.Error != "" || int64(s.Rows)*3 != rows || s.GRVs+s.GRVCacheHits < 1 || s.GetRanges < 1 {
				t.Fatalf("query-only %s sample %d: %+v", class, i, s)
			}
			// Only the FTS classes report the reads they charged.
			if fts := class == QueryFTSSingleToken; fts != (s.Candidates > 0 && s.Bytes > 0) {
				t.Fatalf("query-only %s sample %d work: %+v", class, i, s)
			}
		}
	}
	if _, ok := qo.Queries[QueryFTSPhraseFirstTurn]; ok {
		t.Fatalf("query-only ran an unnamed class: %v", qo.Queries)
	}

	// Without message decomposition the FTS classes are skipped and the
	// remaining classes still answer exactly.
	plain := benchUnitConfig()
	plain.ClusterFile, plain.Run = cluster, freshCollectorRun(t)
	plain.Conversations, plain.TurnsPerConversation, plain.QueryIterations = 3, 3, 2
	plain.EnableMessageDeltas, plain.EnableMessageFullText = false, false
	p, err := RunBenchmark(ctx, plain)
	if err != nil || p.Failures != 0 {
		t.Fatalf("plain run: %v, %+v", err, p)
	}
	if len(p.Queries) != 4 || p.Ingest.Export.MessageDeltas != (otel.MessageDeltaStats{}) || p.Ingest.Export.Messages != (otel.MessageStoreStats{}) || p.Fidelity.CheckedSpans != 6 {
		t.Fatalf("plain report %+v", p)
	}
	for _, class := range []string{QuerySameSpanConjunction, QueryTraceExistential3, QueryTraceGraph, QueryLogsByTrace} {
		if q := p.Queries[class]; q.Iterations != 2 || q.Failures != 0 || q.Histogram.Population != 2 {
			t.Fatalf("plain %s: %+v", class, q)
		}
	}
}

func TestRunBenchmarkCommittedBlockQueriesRealFDB(t *testing.T) {
	t.Parallel()
	cluster := benchClusterFile(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	cfg := benchUnitConfig()
	cfg.ClusterFile, cfg.Run = cluster, freshCollectorRun(t)
	cfg.Duration, cfg.ReportInterval, cfg.Warmup = 0, time.Second, time.Second
	cfg.Conversations = benchBlockSize + 1
	cfg.TurnsPerConversation, cfg.Producers, cfg.NumConsumers, cfg.QueryWorkers = 3, 1, 2, 2
	cfg.runtime = &benchRuntime{beforeAdmission: func(ctx context.Context, run *benchRun, c int) error {
		if c != benchBlockSize {
			return nil
		}
		// Hold the last producer until a committed block and every query class
		// have been observed. No fixed wall-clock throughput is required.
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			complete := run.track.Watermark() >= benchBlockSize-1
			for _, q := range run.load.results(time.Second) {
				complete = complete && q.Iterations > 0 && q.Rows > 0
			}
			if complete {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
	}}
	cfg.QueueSize, cfg.QueryIterations = 60, 4
	const n = 3
	r, err := RunBenchmark(ctx, cfg)
	if err != nil || r.Error != "" || r.Failures != 0 {
		t.Fatalf("RunBenchmark: %v (report error %q, failures %d, watermark %+v, fidelity %+v, queries %+v, under load %+v)", err, r.Error, r.Failures, r.Watermark, r.Fidelity, r.Queries, r.UnderLoad)
	}
	if r.Effective != (CollectorBenchEffective{Load: "fixed", Duration: "0s", ReportInterval: "1s", Warmup: "1s", Producers: 1, NumConsumers: 2, QueryWorkers: 2, BlockSize: benchBlockSize}) {
		t.Fatalf("effective %+v", r.Effective)
	}
	convs := r.Corpus.Conversations
	if convs != cfg.Conversations || r.Corpus.Spans != convs*n || r.Corpus.Logs != convs {
		t.Fatalf("corpus %+v", r.Corpus)
	}
	if s := r.Ingest.Collector; s.AcceptedRequests != int64(convs) || s.ExportedSpans != int64(convs*n) || s.FailedSpans != 0 || s.QueuedSpans != 0 {
		t.Fatalf("collector stats %+v", s)
	}
	if r.Ingest.Logs.Logs != int64(convs) || r.Ingest.Logs.Correlated != int64(convs) || r.Ingest.FailedLogCalls != 0 {
		t.Fatalf("logs %+v", r.Ingest)
	}
	checkBenchSeries(t, r)
	// Every conversation committed; the tracker held only the in-flight window.
	if w := r.Watermark; w.Conversations != convs || w.Watermark != int64(convs)-1 || w.Tracked != 0 || w.Anomalies != 0 || w.PeakTracked < 1 {
		t.Fatalf("watermark %+v", w)
	}
	if r.UnderLoad.Workers != 2 || r.UnderLoad.WallNs <= 0 || len(r.UnderLoad.Queries) != len(CollectorBenchQueryClasses) {
		t.Fatalf("under load %+v", r.UnderLoad)
	}
	var ops int64
	for _, w := range r.Series {
		ops += w.Queries
	}
	for _, class := range CollectorBenchQueryClasses {
		u := r.UnderLoad.Queries[class]
		if u.Iterations == 0 || u.Failures != 0 || u.Error != "" || u.Histogram.Population != u.Iterations || u.Rows <= 0 || u.QPS <= 0 || u.WallNs != r.UnderLoad.WallNs {
			t.Fatalf("%s under load: %+v", class, u)
		}
		ops -= int64(u.Iterations)
		q := r.Queries[class]
		if q.Iterations != 4 || q.Failures != 0 || q.Error != "" || q.Histogram.Population != 4 {
			t.Fatalf("%s after ingest: %+v", class, q)
		}
	}
	if ops != 0 {
		t.Fatalf("window query counts disagree with the classes by %d", ops)
	}
	if r.Fidelity.CheckedSpans != 2*maxBenchSamples || r.Fidelity.Mismatches != 0 {
		t.Fatalf("fidelity %+v", r.Fidelity)
	}
}
