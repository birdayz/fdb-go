package collector

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func benchUnitConfig() CollectorBenchConfig {
	return CollectorBenchConfig{
		ClusterFile: "/nonexistent/fdb.cluster", Run: "bench_unit_run", Tenant: 3, Seed: 11,
		Conversations: 6, TurnsPerConversation: 5, Producers: 3,
		NumConsumers: 1, QueueSize: 1000, BatchMinSize: 4, BatchMaxSize: 16, ExporterBatchSize: 8, ExporterWorkers: 4,
		EnableMessageDeltas: true, EnableMessageFullText: true, QueryIterations: 12,
	}
}

func benchAttr(t *testing.T, sp *tracepb.Span, key string) *commonpb.AnyValue {
	t.Helper()
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("span %x lacks %s", sp.SpanId, key)
	return nil
}

func benchMessageText(t *testing.T, v *commonpb.AnyValue) (string, string) {
	t.Helper()
	var role, text string
	for _, kv := range v.GetKvlistValue().GetValues() {
		switch kv.Key {
		case "role":
			role = kv.Value.GetStringValue()
		case "parts":
			text = kv.Value.GetArrayValue().GetValues()[0].GetKvlistValue().GetValues()[1].Value.GetStringValue()
		}
	}
	return role, text
}

func TestCollectorBenchCorpus(t *testing.T) {
	t.Parallel()
	cfg := benchUnitConfig().WithDefaults()
	n, convs := cfg.TurnsPerConversation, cfg.Conversations
	corpus := newBenchCorpus(cfg)
	again := newBenchCorpus(cfg)
	want := CollectorBenchCorpus{
		Conversations: convs, TurnsPerConversation: n, Spans: convs * n, Logs: convs,
		TotalMessageElements: int64(convs * (n*(n+1) + 3*n)), UniqueMessageElements: int64(2*convs*n + 3),
	}
	var raw int64
	var requests []*tracepb.ResourceSpans
	spanIDs := map[string]bool{}
	for c, conv := range corpus.convs {
		if !proto.Equal(conv.request, again.convs[c].request) || !slices.Equal(conv.trace, again.convs[c].trace) {
			t.Fatalf("conversation %d is not deterministic", c)
		}
		if corpus.traces[conv.traceHex()] != c {
			t.Fatalf("trace index of conversation %d", c)
		}
		raw += int64(proto.Size(conv.request))
		requests = append(requests, conv.request)
		spans := conv.request.ScopeSpans[0].Spans
		if len(spans) != n {
			t.Fatalf("conversation %d has %d spans", c, len(spans))
		}
		for i, sp := range spans {
			k := i + 1
			spanIDs[hex.EncodeToString(sp.SpanId)] = true
			if !slices.Equal(sp.TraceId, conv.trace) || !slices.Equal(sp.SpanId, conv.spans[i]) {
				t.Fatalf("conversation %d turn %d identity", c, k)
			}
			if k == 1 && len(sp.ParentSpanId) != 0 || k > 1 && !slices.Equal(sp.ParentSpanId, conv.spans[i-1]) {
				t.Fatalf("conversation %d turn %d parent %x", c, k, sp.ParentSpanId)
			}
			if benchAttr(t, sp, "gen_ai.turn.number").GetIntValue() != int64(k) || benchAttr(t, sp, "gen_ai.conversation.id").GetStringValue() != fmt.Sprintf("conv-bench_unit_run-%d", c) ||
				benchAttr(t, sp, "gen_ai.request.model").GetStringValue() != "gpt-4o" || benchAttr(t, sp, "gen_ai.operation.name").GetStringValue() != "chat" {
				t.Fatalf("conversation %d turn %d attributes %v", c, k, sp.Attributes)
			}
			input := benchAttr(t, sp, benchInputKey).GetArrayValue().GetValues()
			output := benchAttr(t, sp, benchOutputKey).GetArrayValue().GetValues()
			if len(input) != 2*k-1 || len(output) != 1 || len(benchAttr(t, sp, "gen_ai.tool.definitions").GetArrayValue().GetValues()) != 2 ||
				len(benchAttr(t, sp, "gen_ai.system_instructions").GetArrayValue().GetValues()) != 1 {
				t.Fatalf("conversation %d turn %d message shape", c, k)
			}
			if role, text := benchMessageText(t, input[len(input)-1]); role != "user" || text != BenchUserText(cfg.Seed, c, k) || len(text) < benchFillerBytes {
				t.Fatalf("conversation %d turn %d user message %q/%q", c, k, role, text)
			}
			if role, text := benchMessageText(t, output[0]); role != "assistant" || text != BenchAssistantText(cfg.Seed, c, k) {
				t.Fatalf("conversation %d turn %d reply %q/%q", c, k, role, text)
			}
			if k > 1 {
				prev := spans[i-1]
				prevInput := benchAttr(t, prev, benchInputKey).GetArrayValue().GetValues()
				if !proto.Equal(input[len(input)-2], benchAttr(t, prev, benchOutputKey).GetArrayValue().GetValues()[0]) || !proto.Equal(&commonpb.ArrayValue{Values: input[:len(prevInput)]}, &commonpb.ArrayValue{Values: prevInput}) {
					t.Fatalf("conversation %d turn %d does not extend turn %d's history", c, k, k-1)
				}
			}
		}
		l := corpus.logs[c]
		if l.TraceID != conv.traceHex() || l.SpanID != conv.spanHex(1) || l.EventID == "" {
			t.Fatalf("conversation %d log %+v", c, l)
		}
	}
	want.RawInputBytes = raw
	if corpus.stats != want || len(corpus.traces) != convs || len(spanIDs) != convs*n {
		t.Fatalf("corpus stats %+v, want %+v (%d traces, %d spans)", corpus.stats, want, len(corpus.traces), len(spanIDs))
	}
	_, blobs, st, err := otel.DecomposeResourceSpans(t.Context(), requests, cfg.Tenant)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalMessages != want.TotalMessageElements || st.UniqueMessages != want.UniqueMessageElements || len(blobs) != int(want.UniqueMessageElements) || st.SavedBytes <= 0 {
		t.Fatalf("decomposition %+v, want %d total / %d unique", st, want.TotalMessageElements, want.UniqueMessageElements)
	}
	samples := corpus.samples()
	if len(samples) != 2*convs {
		t.Fatalf("%d samples", len(samples))
	}
	for _, s := range samples {
		if s.turn != 1 && s.turn != n || !proto.Equal(s.want.ScopeSpans[0].Spans[0], corpus.convs[s.conv].request.ScopeSpans[0].Spans[s.turn-1]) {
			t.Fatalf("sample %d/%d", s.conv, s.turn)
		}
	}
	// Filler never contains a query token, so answers are decided by prefixes.
	for _, s := range benchSentences {
		lower := strings.ToLower(s)
		for _, bad := range []string{"user", "turn", "conversation", "assistant", "token", "resolved"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("filler %q contains %q", s, bad)
			}
		}
		if strings.ContainsAny(strings.ReplaceAll(strings.ReplaceAll(s, "p99", ""), "hel1", ""), "0123456789") {
			t.Fatalf("filler %q contains a number", s)
		}
	}
}

func TestCollectorBenchBuckets(t *testing.T) {
	t.Parallel()
	cfg := benchUnitConfig().WithDefaults()
	cfg.Conversations, cfg.TurnsPerConversation = 120, 1
	corpus := newBenchCorpus(cfg)
	if corpus.bucketMod() != 50 || corpus.bucketSize(0) != 3 || corpus.bucketSize(19) != 3 || corpus.bucketSize(20) != 2 || corpus.bucketSize(49) != 2 {
		t.Fatalf("bucket sizes %d %d %d %d", corpus.bucketSize(0), corpus.bucketSize(19), corpus.bucketSize(20), corpus.bucketSize(49))
	}
	if s := corpus.samples(); len(s) != 16 || s[1].conv != 7 || s[15].conv != 112 || s[0].turn != 1 {
		t.Fatalf("samples %+v", s)
	}
	small := newBenchCorpus(benchUnitConfig().WithDefaults())
	if small.bucketMod() != 6 || small.bucketSize(5) != 1 || small.bucketSize(6) != 0 {
		t.Fatal("small corpus buckets")
	}
}

func TestCollectorBenchConfigValidate(t *testing.T) {
	t.Parallel()
	want := DefaultCollectorBenchConfig()
	want.ClusterFile, want.Run = "/c", "bench_unit_run"
	if got := (CollectorBenchConfig{ClusterFile: "/c", Run: "bench_unit_run", EnableMessageDeltas: true, EnableMessageFullText: true}).WithDefaults(); got != want {
		t.Fatalf("WithDefaults = %+v, want %+v", got, want)
	}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := benchUnitConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	cc := benchUnitConfig().WithDefaults().collectorConfig()
	if q := cc.SendingQueue; !q.Enabled || !q.BlockOnOverflow || q.NumConsumers != 1 || q.QueueSize != 1000 || q.Batch.MinSize != 4 || q.Batch.MaxSize != 16 {
		t.Fatalf("sending queue %+v", q)
	}
	if e := cc.Exporter; !e.EnableMessageDeltas || !e.EnableMessageFullText || e.BatchSize != 8 || e.Workers != 4 || e.Tenant != 3 || e.Order != "time" || e.HashBytes != 8 || e.RecordCompression != "brotli-4" || e.PostingValueFormat != "tuple" {
		t.Fatalf("exporter %+v", e)
	}
	for name, mutate := range map[string]func(*CollectorBenchConfig){
		"cluster file":   func(c *CollectorBenchConfig) { c.ClusterFile = "" },
		"run":            func(c *CollectorBenchConfig) { c.Run = "BAD" },
		"conversations":  func(c *CollectorBenchConfig) { c.Conversations = -1 },
		"many convs":     func(c *CollectorBenchConfig) { c.Conversations = maxBenchConversations + 1 },
		"turns":          func(c *CollectorBenchConfig) { c.TurnsPerConversation = maxBenchTurns + 1 },
		"producers":      func(c *CollectorBenchConfig) { c.Producers = -1 },
		"iterations":     func(c *CollectorBenchConfig) { c.QueryIterations = -1 },
		"queue":          func(c *CollectorBenchConfig) { c.QueueSize = 4 },
		"batch min":      func(c *CollectorBenchConfig) { c.BatchMinSize = 2000 },
		"batch max":      func(c *CollectorBenchConfig) { c.BatchMaxSize = 2 },
		"consumers":      func(c *CollectorBenchConfig) { c.NumConsumers = maxConsumers + 1 },
		"exporter batch": func(c *CollectorBenchConfig) { c.ExporterBatchSize = 65 },
		"workers":        func(c *CollectorBenchConfig) { c.ExporterWorkers = 17 },
		"order":          func(c *CollectorBenchConfig) { c.Order = "random" },
		"hash bytes":     func(c *CollectorBenchConfig) { c.HashBytes = 33 },
		"compression":    func(c *CollectorBenchConfig) { c.RecordCompression = "lz4" },
	} {
		cfg := benchUnitConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
		r, err := RunBenchmark(t.Context(), cfg)
		if err == nil || r.Error == "" || r.Tool != CollectorBenchTool || r.Mode != "collector-bench" || r.Config != cfg.WithDefaults() || len(r.Queries) != 0 {
			t.Errorf("%s: RunBenchmark = %+v, %v", name, r, err)
		}
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
		MessageFTS:    otel.MessageFTSStats{IndexedBlobs: 14, SkippedBlobs: 15, UniqueTokens: 16, TokenPostings: 17, SpanRefs: 18, Bytes: 19, Transactions: 20},
	}
	var sum otel.ExportStats
	addExportStats(&sum, one)
	addExportStats(&sum, one)
	want := otel.ExportStats{
		ReceivedSpans: 2, DuplicateSpans: 4, Spans: 6, Batches: 8, LogicalBytes: 10, NormalizedSpans: 12, UnindexedAttributes: 14,
		MessageDeltas: otel.MessageDeltaStats{TotalMessages: 16, UniqueMessages: 18, ReusedMessages: 20, RawMessageBytes: 22, StoredDeltaBytes: 24, SavedBytes: 26},
		MessageFTS:    otel.MessageFTSStats{IndexedBlobs: 28, SkippedBlobs: 30, UniqueTokens: 32, TokenPostings: 34, SpanRefs: 36, Bytes: 38, Transactions: 40},
	}
	if sum != want {
		t.Fatalf("sum %+v, want %+v", sum, want)
	}
}

func benchClusterFile(t *testing.T) string {
	t.Helper()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
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
		t.Fatalf("RunBenchmark: %v (report error %q, failures %d, fidelity %+v, queries %+v)", err, r.Error, r.Failures, r.Fidelity, r.Queries)
	}
	if r.Corpus.Spans != convs*n || r.Corpus.TotalMessageElements != int64(total) || r.Corpus.UniqueMessageElements != int64(unique) || r.Corpus.RawInputBytes == 0 {
		t.Fatalf("corpus %+v", r.Corpus)
	}
	in := r.Ingest
	if s := in.Collector; s.AcceptedRequests != convs || s.AcceptedSpans != convs*n || s.ExportedSpans != convs*n || s.StoredSpans != convs*n ||
		s.RejectedSpans != 0 || s.FailedSpans != 0 || s.QueuedSpans != 0 || s.Flushes < 2 || s.Exports < s.Flushes {
		t.Fatalf("collector stats %+v", s)
	}
	if in.IngestWallNs <= 0 || in.SpansPerSec <= 0 || in.MessagesPerSec <= 0 || in.FailedExportCalls != 0 {
		t.Fatalf("ingest %+v", in)
	}
	if e := in.Export; e.ReceivedSpans != convs*n || e.Spans != convs*n || e.MessageDeltas.TotalMessages != int64(total) || e.MessageDeltas.ReusedMessages <= 0 ||
		e.MessageDeltas.SavedBytes <= 0 || e.MessageDeltas.RawMessageBytes <= e.MessageDeltas.StoredDeltaBytes {
		t.Fatalf("export stats %+v", e)
	}
	// One consumer exports batches serially, so every unique message is
	// tokenized exactly once across all batches.
	if f := in.Export.MessageFTS; f.IndexedBlobs != unique || f.SpanRefs != total || f.TokenPostings == 0 || f.UniqueTokens == 0 || f.Bytes == 0 {
		t.Fatalf("FTS stats %+v", f)
	}
	if in.Logs.Logs != convs || in.Logs.Correlated != convs || in.Logs.EventIDs != nil {
		t.Fatalf("log stats %+v", in.Logs)
	}
	if r.Fidelity.CheckedSpans != 2*convs || r.Fidelity.Mismatches != 0 {
		t.Fatalf("fidelity %+v", r.Fidelity)
	}
	wantRows := map[string]int64{
		QueryFTSSingleToken:      iters * n * (n + 1) / 2,
		QueryFTSPhraseFirstTurn:  iters,
		QueryFTSMultiTurnHistory: iters * n,
		QueryFTSSameSpanPreds:    iters * n,
		QuerySameSpanConjunction: iters * n,
		QueryTraceExistential3:   iters,
		QueryTraceGraph:          iters * n,
		QueryLogsByTrace:         iters,
	}
	if len(r.Queries) != len(CollectorBenchQueryClasses) {
		t.Fatalf("query classes %v", r.Queries)
	}
	for _, class := range CollectorBenchQueryClasses {
		q, ok := r.Queries[class]
		if !ok || q.Iterations != iters || q.Failures != 0 || q.Error != "" || q.Rows != wantRows[class] || q.Histogram.Population != iters ||
			q.Histogram.Min <= 0 || q.Histogram.Max < q.Histogram.P99 || q.QPS <= 0 {
			t.Fatalf("%s: %+v, want %d rows", class, q, wantRows[class])
		}
	}

	// Re-running on the same run is an idempotent replace: nothing is
	// tokenized again and every answer is unchanged.
	cfg.QueryIterations = 1
	again, err := RunBenchmark(ctx, cfg)
	if err != nil || again.Failures != 0 {
		t.Fatalf("re-run: %v, %+v", err, again)
	}
	if f := again.Ingest.Export.MessageFTS; f.IndexedBlobs != 0 || f.SkippedBlobs < unique || f.SpanRefs != total {
		t.Fatalf("re-run FTS stats %+v", f)
	}
	if again.Ingest.Collector.StoredSpans != convs*n || again.Ingest.Logs.Replaced != convs || again.Fidelity.Mismatches != 0 {
		t.Fatalf("re-run ingest %+v fidelity %+v", again.Ingest, again.Fidelity)
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
	if len(p.Queries) != 4 || p.Ingest.Export.MessageDeltas != (otel.MessageDeltaStats{}) || p.Ingest.Export.MessageFTS != (otel.MessageFTSStats{}) || p.Fidelity.CheckedSpans != 6 {
		t.Fatalf("plain report %+v", p)
	}
	for _, class := range []string{QuerySameSpanConjunction, QueryTraceExistential3, QueryTraceGraph, QueryLogsByTrace} {
		if q := p.Queries[class]; q.Iterations != 2 || q.Failures != 0 || q.Histogram.Population != 2 {
			t.Fatalf("plain %s: %+v", class, q)
		}
	}
}
