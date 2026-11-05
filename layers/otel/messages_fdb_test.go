package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ftsBase is deltaSpan's start time origin: turn k starts at ftsBase+k.
const ftsBase = int64(1700000000000000000)

func ftsTrace(k int) string { return hex.EncodeToString(bytes.Repeat([]byte{byte(k + 1)}, 16)) }
func ftsSpan(k int) string  { return hex.EncodeToString(bytes.Repeat([]byte{byte(k + 101)}, 8)) }

func openFTSExporter(t *testing.T, ctx context.Context, cluster string, mutate func(*ExporterConfig)) *Exporter {
	t.Helper()
	return openTestExporter(t, ctx, cluster, func(c *ExporterConfig) {
		c.EnableMessageFullText = true
		if mutate != nil {
			mutate(c)
		}
	})
}

// ftsDrain follows continuations to exhaustion, checking the page contract.
func ftsDrain(t *testing.T, ctx context.Context, e *Exporter, q MessageFullTextQuery) ([]MessageSpanHit, int) {
	t.Helper()
	limit := q.Limit
	if limit == 0 {
		limit = e.Config().K
	}
	all := []MessageSpanHit{}
	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
		page, err := e.QueryMessageFullText(ctx, q)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", pages, q, err)
		}
		if page.Order != e.Config().Order || page.ReadVersion <= 0 || page.Hits == nil {
			t.Fatalf("page metadata: %+v", page)
		}
		if len(page.Hits) > limit || (len(page.Hits) == limit) != (page.Continuation != nil) {
			t.Fatalf("page %d: %d hits, limit %d, continuation=%v", pages, len(page.Hits), limit, page.Continuation != nil)
		}
		all = append(all, page.Hits...)
		if page.Continuation == nil {
			ftsRequireSorted(t, e.Config().Order, all)
			return all, pages
		}
		q.Continuation = page.Continuation
	}
}

func ftsMessageID(t *testing.T, s string) messageID {
	t.Helper()
	var id messageID
	if _, err := fmt.Sscanf(s, "%d.%d", &id.writer, &id.seq); err != nil || !id.valid() {
		t.Fatalf("message id %q: %v", s, err)
	}
	return id
}

func ftsLess(t *testing.T, order string, a, b MessageSpanHit) bool {
	if order == "time" {
		if a.Start != b.Start {
			return a.Start > b.Start
		}
		if a.Trace != b.Trace {
			return a.Trace > b.Trace // reverseTrace ascending
		}
	} else if a.Trace != b.Trace {
		return a.Trace < b.Trace
	}
	if a.Span != b.Span {
		return a.Span < b.Span
	}
	if a.AttributeKey != b.AttributeKey {
		return a.AttributeKey < b.AttributeKey
	}
	return ftsMessageID(t, a.MessageID).less(ftsMessageID(t, b.MessageID))
}

func ftsRequireSorted(t *testing.T, order string, hits []MessageSpanHit) {
	t.Helper()
	for i := 1; i < len(hits); i++ {
		if !ftsLess(t, order, hits[i-1], hits[i]) {
			t.Fatalf("%s hits %d and %d out of order or duplicated: %+v %+v", order, i-1, i, hits[i-1], hits[i])
		}
	}
}

// ftsTurns returns the deltaSpan turn of every hit, in hit order.
func ftsTurns(hits []MessageSpanHit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = int(h.Start - ftsBase)
	}
	return out
}

func ftsRange(lo, hi int, order string) []int {
	var out []int
	for k := lo; k <= hi; k++ {
		out = append(out, k)
	}
	if order == "time" {
		sort.Sort(sort.Reverse(sort.IntSlice(out)))
	}
	return out
}

func ftsCount(t *testing.T, ctx context.Context, e *Exporter, q MessageFullTextQuery) int {
	t.Helper()
	hits, _ := ftsDrain(t, ctx, e, q)
	return len(hits)
}

func deltaRun(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, fn func(*recordlayer.FDBRecordContext) error) error {
	t.Helper()
	_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) { return nil, fn(rc) })
	return err
}

// ftsIndexEntries counts an index's entries under prefix.
func ftsIndexEntries(t *testing.T, ctx context.Context, e *Exporter, index string, prefix tuple.Tuple) int {
	t.Helper()
	n := 0
	_, err := e.backend.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		n = 0
		store, err := e.backend.open(rc)
		if err != nil {
			return nil, err
		}
		cur := store.ScanIndex(e.backend.md.GetIndex(index), recordlayer.TupleRangeAllOf(prefix), nil, recordlayer.ForwardScan())
		defer cur.Close()
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !r.HasNext() {
				return nil, nil
			}
			n++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLiveMessageQueueReleasesBackingOnPartialFailureAndCancel(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
			backing := make([]queuedMessage, 2)
			e.backend.queue.pending = backing[:0]
			callCtx, stop := context.WithCancel(ctx)
			defer stop()
			sentinel := errors.New("queue drain terminal failure")
			partial := false
			e.backend.messageTxnHook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				q := &e.backend.queue
				q.mu.Lock()
				if len(q.pending) == 1 {
					partial = true
					if backing[0].plan != nil || backing[0].sub != nil || backing[1].plan == nil || backing[1].sub == nil {
						t.Error("live partial take retained consumed slot or lost tail")
					}
				}
				q.mu.Unlock()
				if outcome == "cancel" {
					stop()
					return context.Canceled
				}
				if outcome == "failure" {
					return sentinel
				}
				return nil
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Repeat("a", 900000)), deltaStr(strings.Repeat("b", 900000)))))
			st, err := e.ExportResourceSpans(callCtx, []*tracepb.ResourceSpans{input})
			if outcome == "success" && (err != nil || st.Messages.Written != 2) {
				t.Fatalf("live queue success: %+v / %v", st.Messages, err)
			}
			if outcome == "failure" && !errors.Is(err, sentinel) {
				t.Fatalf("live queue failure: %v", err)
			}
			if outcome == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("live queue cancellation: %v", err)
			}
			// Join the actual queue loops by their protected ownership count.
			// No elapsed-time or heap-size inference is used as the release oracle.
			for {
				q := &e.backend.queue
				q.mu.Lock()
				drained := q.loops == 0
				if drained {
					if q.pending != nil || !partial {
						t.Error("live partial queue did not drain to nil")
					}
					for i, slot := range backing {
						if slot.plan != nil || slot.sub != nil {
							t.Errorf("live drained backing slot%d retained plan/submission", i)
						}
					}
				}
				q.mu.Unlock()
				if drained {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				runtime.Gosched()
			}
			e.backend.messageTxnHook = nil
			if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("live neighbor"))))}); err != nil {
				t.Fatalf("exporter must remain alive after drain: %v", err)
			}
		})
	}
}

type markerReadProbe struct {
	fdb.Database
	prefix         []byte
	mu             sync.Mutex
	reads, present int
}

type (
	markerProbeTransaction struct {
		fdb.WritableTransaction
		probe *markerReadProbe
	}
	markerProbeSnapshot struct {
		fdb.ReadTransaction
		probe *markerReadProbe
	}
	markerProbeRange struct {
		fdb.RangeResult
		probe *markerReadProbe
	}
)

func (tx markerProbeTransaction) Snapshot() fdb.ReadTransaction {
	return markerProbeSnapshot{tx.WritableTransaction.Snapshot(), tx.probe}
}

func (tx markerProbeSnapshot) GetRange(r fdb.Range, opts fdb.RangeOptions) fdb.RangeResult {
	result := tx.ReadTransaction.GetRange(r, opts)
	begin, _ := r.FDBRangeKeySelectors()
	if bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), tx.probe.prefix) {
		return markerProbeRange{result, tx.probe}
	}
	return result
}

func (r markerProbeRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	rows, err := r.RangeResult.GetSliceWithError()
	r.probe.mu.Lock()
	r.probe.reads++
	if err == nil && len(rows) > 0 {
		r.probe.present++
	}
	r.probe.mu.Unlock()
	return rows, err
}

func (p *markerReadProbe) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return p.TransactCtx(context.Background(), fn)
}

func (p *markerReadProbe) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return p.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) { return fn(markerProbeTransaction{tx, p}) })
}

func TestEvictedPersistedMarkerPublicReplayReadsAndReuses(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	input := []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("quokka"))))}
	if st, err := e.ExportResourceSpans(ctx, input); err != nil || st.Messages.Written != 1 || st.Messages.TraceMarkers != 1 {
		t.Fatalf("initial persisted marker: %+v / %v", st.Messages, err)
	}
	cache := &e.backend.markers
	if len(cache.ring) != 1 {
		t.Fatalf("initial cache=%d", len(cache.ring))
	}
	originalMarker := cache.ring[0]
	prefix := e.backend.store.Sub(recordlayer.RecordKey).Pack(tuple.Tuple{messageTraceTypeKey, int64(e.cfg.Tenant)})
	markerRows := func() map[string]string {
		t.Helper()
		var found map[string]string
		_, err := e.backend.raw.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
			found = map[string]string{}
			r, err := fdb.PrefixRange(prefix)
			if err != nil {
				return nil, err
			}
			rows, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			for _, row := range rows {
				found[string(row.Key)] = string(row.Value)
			}
			return nil, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	before := markerRows()
	if len(before) != 1 {
		t.Fatalf("real persisted marker rows=%d", len(before))
	}
	for i := range markerCacheSize {
		cache.add([]traceMarker{cacheFixtureMarker(i)})
	}
	if cache.has(originalMarker) || len(cache.set) != markerCacheSize {
		t.Fatalf("real marker not evicted / map not bounded: %d", len(cache.set))
	}
	probe := &markerReadProbe{Database: e.backend.raw, prefix: prefix}
	originalDB := e.backend.db
	e.backend.db = recordlayer.NewFDBDatabaseWithTransactor(probe, e.backend.raw)
	defer func() { e.backend.db = originalDB }()
	st, err := e.ExportResourceSpans(ctx, input)
	if err != nil || st.Messages.Written != 0 || st.Messages.Present != 1 || st.Messages.TraceMarkers != 0 {
		t.Fatalf("public evicted-marker replay: %+v / %v", st.Messages, err)
	}
	probe.mu.Lock()
	reads, present := probe.reads, probe.present
	probe.mu.Unlock()
	if reads == 0 || reads != present || !cache.has(originalMarker) || len(cache.set) != markerCacheSize {
		t.Fatalf("replay did not read/reuse persisted marker: reads=%d present=%d cached=%v size=%d", reads, present, cache.has(originalMarker), len(cache.set))
	}
	if after := markerRows(); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay duplicated/changed persisted marker: before=%v after=%v", before, after)
	}
	if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "quokka"}); n != 1 {
		t.Fatalf("replay duplicated query result: %d", n)
	}
}

func TestMessageRecordsRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, c := range []struct{ form, order string }{{"array", "time"}, {"json", "time"}, {"array", "trace"}} {
		t.Run("conversation/"+c.form+"_"+c.order, func(t *testing.T) {
			t.Parallel()
			testMessageConversation(t, cluster, c.form, c.order)
		})
	}
	t.Run("trace_per_turn", func(t *testing.T) { t.Parallel(); testMessageTracePerTurn(t, cluster) })
	t.Run("unicode_events_replacement", func(t *testing.T) { t.Parallel(); testMessageReplacement(t, cluster) })
	t.Run("budgets", func(t *testing.T) { t.Parallel(); testMessageBudget(t, cluster) })
	t.Run("trace_existential", func(t *testing.T) { t.Parallel(); testMessageExistential(t, cluster) })
	t.Run("rare_term_intersection", func(t *testing.T) { t.Parallel(); testMessageRareTerm(t, cluster) })
	t.Run("chunked_references", func(t *testing.T) { t.Parallel(); testMessageChunkedRefs(t, cluster) })
	t.Run("large_message", func(t *testing.T) { t.Parallel(); testMessageLarge(t, cluster) })
	t.Run("concurrent_same_message", func(t *testing.T) { t.Parallel(); testMessageConcurrentSave(t, cluster) })
	t.Run("group_commit", func(t *testing.T) { t.Parallel(); testMessageGroupCommit(t, cluster) })
	t.Run("layout_rejection", func(t *testing.T) { t.Parallel(); testMessageLayoutRejection(t, cluster) })
}

// testMessageConversation exports one 25-turn conversation in one trace.
func testMessageConversation(t *testing.T, cluster, form, order string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Order = order })
	n := deltaTurns
	input := sameTrace(deltaConversation(n, form))
	trace := ftsTrace(1)
	before := cloneDeltaSpans(input)
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	refs := n*(n+1) + 3*n
	if stats.Spans != int64(n) || stats.MessageDeltas.TotalMessages != int64(refs) || stats.MessageDeltas.UniqueMessages != int64(2*n+3) {
		t.Fatalf("export stats %+v", stats)
	}
	// Each message is stored once; every span lists each of its elements.
	// The trace gets one marker per distinct message.
	if m := stats.Messages; m.Written != int64(2*n+3) || m.Present != 0 || m.Parts != m.Written || m.Postings == 0 || m.References != int64(refs) || m.TraceMarkers != int64(2*n+3) || m.Transactions < 2 || m.TextIndexWrites == 0 {
		t.Fatalf("first message stats %+v", m)
	}
	requireDeltaSpansEqual(t, input, before)
	// Storing a present message writes nothing to any index.
	again, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if m := again.Messages; m.Written != 0 || m.Present != int64(2*n+3) || m.Postings != 0 || m.TextIndexWrites != 0 || m.TextIndexKeyBytes != 0 || m.References != int64(refs) || m.TraceMarkers != 0 || m.Transactions != 1 {
		t.Fatalf("idempotent re-export message stats %+v", m)
	}
	for k, want := range before {
		got, err := e.LoadSpanOTLP(ctx, trace, ftsSpan(k+1))
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("turn %d", k+1), got, want)
		gw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(got)
		ww, _ := proto.MarshalOptions{Deterministic: true}.Marshal(want)
		if !bytes.Equal(gw, ww) {
			t.Fatalf("turn %d is not reconstructed byte for byte", k+1)
		}
	}
	tenant := int64(e.Config().Tenant)
	if got := ftsIndexEntries(t, ctx, e, messageHashIndexName, tuple.Tuple{tenant}); got != 2*n+3 {
		t.Fatalf("dedup index holds %d messages", got)
	}
	// One reference entry per span, and one marker per message of the trace
	// even when another backend writes the trace again.
	if got := ftsIndexEntries(t, ctx, e, envelopeRefsIndexName, tuple.Tuple{tenant, trace}); got != n {
		t.Fatalf("%d span reference entries, want %d", got, n)
	}
	other := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Run = e.Config().Run; c.Order = order })
	if third, err := other.ExportResourceSpans(ctx, input); err != nil || third.Messages.TraceMarkers != 0 {
		t.Fatalf("re-export through a fresh backend: %+v %v", third.Messages, err)
	}
	if got := ftsIndexEntries(t, ctx, e, messageLargeIndexName, tuple.Tuple{tenant}); got != 0 {
		t.Fatalf("%d large messages", got)
	}

	// Turn 3's prompt is in the input of turns 3..n and the newest element of
	// turn 3's.
	prompt := MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase}
	hits, _ := ftsDrain(t, ctx, e, prompt)
	if !reflect.DeepEqual(ftsTurns(hits), ftsRange(3, n, order)) {
		t.Fatalf("phrase turns %v", ftsTurns(hits))
	}
	for _, h := range hits {
		k := int(h.Start - ftsBase)
		if h.Trace != trace || h.Span != ftsSpan(k) || h.AttributeKey != "gen_ai.input.messages" || h.Role != "user" || h.MessageID != hits[0].MessageID || !reflect.DeepEqual(h.TurnOrdinals, []int{4}) {
			t.Fatalf("turn %d hit %+v", k, h)
		}
	}
	first := prompt
	first.FirstTurnOnly = true
	if got, _ := ftsDrain(t, ctx, e, first); len(got) != 1 || got[0].Span != ftsSpan(3) || !reflect.DeepEqual(got[0].TurnOrdinals, []int{4}) {
		t.Fatalf("first-turn prompt: %+v", got)
	}
	reply := MessageFullTextQuery{Query: "Assistant Turn 3", Mode: MessageMatchPhrase, FirstTurnOnly: true}
	if got, _ := ftsDrain(t, ctx, e, reply); len(got) != 1 || got[0].Span != ftsSpan(3) || got[0].AttributeKey != "gen_ai.output.messages" || got[0].Role != "assistant" {
		t.Fatalf("first-turn reply: %+v", got)
	}
	reply.FirstTurnOnly = false
	if got := ftsCount(t, ctx, e, reply); got != 1+(n-3) {
		t.Fatalf("reply references %d", got)
	}

	userRefs, assistantRefs := n*(n+1)/2, n*(n+1)/2
	for name, c := range map[string]struct {
		q    MessageFullTextQuery
		want int
	}{
		"non-adjacent phrase":   {MessageFullTextQuery{Query: "user 3", Mode: MessageMatchPhrase}, 0},
		"reversed phrase":       {MessageFullTextQuery{Query: "turn user", Mode: MessageMatchPhrase}, 0},
		"all terms":             {MessageFullTextQuery{Query: "user 3"}, userRefs},
		"every reference":       {MessageFullTextQuery{Query: "turn"}, refs},
		"absent term":           {MessageFullTextQuery{Query: "turn zeppelin"}, 0},
		"any term":              {MessageFullTextQuery{Query: "query_traces fetch_logs zeppelin", Mode: MessageMatchAny}, 2 * n},
		"any term wrong attr":   {MessageFullTextQuery{Query: "query_traces fetch_logs", Mode: MessageMatchAny, AttributeKey: "gen_ai.input.messages"}, 0},
		"tool attribute":        {MessageFullTextQuery{Query: "fetch_logs", AttributeKey: "gen_ai.tool.definitions"}, n},
		"type is structural":    {MessageFullTextQuery{Query: "function"}, 0},
		"system attribute":      {MessageFullTextQuery{Query: "system", AttributeKey: "gen_ai.system_instructions"}, n},
		"system first turn":     {MessageFullTextQuery{Query: "system", FirstTurnOnly: true}, n}, // the only element of each span's value
		"system in inputs":      {MessageFullTextQuery{Query: "system", AttributeKey: "gen_ai.input.messages"}, 0},
		"role user":             {MessageFullTextQuery{Query: "turn", Role: " USER "}, userRefs},
		"role assistant":        {MessageFullTextQuery{Query: "turn", Role: "assistant"}, assistantRefs},
		"role user first turn":  {MessageFullTextQuery{Query: "turn", Role: "user", FirstTurnOnly: true}, n},
		"role output":           {MessageFullTextQuery{Query: "turn", Role: "assistant", AttributeKey: "gen_ai.output.messages"}, n},
		"role absent":           {MessageFullTextQuery{Query: "turn", Role: "tool"}, 0},
		"same-span predicate":   {MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Predicates: []Attribute{{"span", "gen_ai.conversation.id", "string", "conv-283"}, {"resource", "service.name", "string", "agent"}}}, n - 2},
		"failing predicate":     {MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Predicates: []Attribute{{"span", "gen_ai.operation.name", "string", "embeddings"}}}, 0},
		"time window":           {MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, TimeWindow: &TimeRange{Start: ftsBase + 10, End: ftsBase + 13}}, 3},
		"empty window":          {MessageFullTextQuery{Query: "turn", TimeWindow: &TimeRange{Start: 5, End: 5}}, 0},
		"window before history": {MessageFullTextQuery{Query: "turn", TimeWindow: &TimeRange{Start: 0, End: ftsBase + 1}}, 0},
	} {
		if got := ftsCount(t, ctx, e, c.q); got != c.want {
			t.Fatalf("%s: %d hits, want %d", name, got, c.want)
		}
	}
	for _, role := range []string{"user", "assistant"} {
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Role: role, Limit: 50})
		for _, h := range hits {
			if h.Role != role {
				t.Fatalf("role %s filter returned %+v", role, h)
			}
		}
	}
	// Pages of any size concatenate to one answer.
	all, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Limit: 1000})
	for _, limit := range []int{1, 7, 100} {
		paged, pages := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Limit: limit})
		if !reflect.DeepEqual(paged, all) || pages != len(all)/limit+1 {
			t.Fatalf("limit %d: %d hits in %d pages, want %d", limit, len(paged), pages, len(all))
		}
	}
	// A message attribute holds manifests and is not an equality predicate.
	_, err = e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}})
	requireTyped[*ConfigError](t, "message attribute predicate", err)
	_, err = e.Query(ctx, Attribute{"span", "gen_ai.output.messages", "string", "x"})
	requireTyped[*ConfigError](t, "message attribute query", err)
}

// testMessageTracePerTurn exports each turn in its own trace, then a
// conversation in one trace with its turns in reverse order, each in its own
// call: a first turn depends only on the span, not on the trace or the order
// spans arrive in.
func testMessageTracePerTurn(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	const n = 6
	for _, reverse := range []bool{false, true} {
		e := openFTSExporter(t, ctx, cluster, nil)
		conv := deltaConversation(n, "array")
		var refs int64
		if reverse {
			sameTrace(conv)
			for k := n - 1; k >= 0; k-- {
				stats, err := e.ExportResourceSpans(ctx, conv[k:k+1])
				if err != nil {
					t.Fatal(err)
				}
				refs += stats.Messages.References
			}
		} else {
			stats, err := e.ExportResourceSpans(ctx, conv)
			if err != nil {
				t.Fatal(err)
			}
			refs = stats.Messages.References
		}
		if want := int64(n*(n+1) + 3*n); refs != want {
			t.Fatalf("reverse=%v: %d references, want %d", reverse, refs, want)
		}
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, FirstTurnOnly: true})
		if len(hits) != 1 || hits[0].Span != ftsSpan(3) || !reflect.DeepEqual(hits[0].TurnOrdinals, []int{4}) {
			t.Fatalf("reverse=%v: first turn %+v", reverse, hits)
		}
		hits, _ = ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase})
		if !reflect.DeepEqual(ftsTurns(hits), []int{6, 5, 4, 3}) {
			t.Fatalf("reverse=%v: references %v", reverse, ftsTurns(hits))
		}
	}
}

func testMessageReplacement(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	dessert := "Crème Brûlée ÜBER façade"
	msgs := func(form, role, text string) *commonpb.KeyValue {
		return deltaKV("gen_ai.input.messages", deltaMessages(form, []deltaMsg{{role, text}}))
	}
	event := deltaSpan(53)
	event.ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "gen_ai.client.inference.operation.details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.output.messages", deltaMessages("json", []deltaMsg{{"Assistant", "event borne zebra"}}))}}}
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(50, msgs("array", "user", dessert)), deltaSpan(51, msgs("json", "user", dessert)), event}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"creme brulee", "CRÈME BRÛLÉE", "Über FAÇADE"} {
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: q, Mode: MessageMatchPhrase})
		if len(hits) != 2 || hits[0].MessageID == hits[1].MessageID || hits[0].Role != "user" || hits[1].Role != "user" {
			t.Fatalf("%q: %+v", q, hits)
		}
	}
	for q, want := range map[string]int{"brulee creme": 0, "user": 0, "text": 0} {
		if got := ftsCount(t, ctx, e, MessageFullTextQuery{Query: q, Mode: MessageMatchPhrase}); got != want {
			t.Fatalf("%q: %d hits, want %d", q, got, want)
		}
	}
	zebra, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "zebra", Role: "assistant"})
	if len(zebra) != 1 || zebra[0].Span != ftsSpan(53) || zebra[0].AttributeKey != "gen_ai.output.messages" {
		t.Fatalf("event message: %+v", zebra)
	}

	// Re-exporting a span replaces its record; the last duplicate wins.
	moved := deltaSpan(51, msgs("json", "user", dessert))
	moved.ScopeSpans[0].Spans[0].StartTimeUnixNano += 1000
	stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{
		deltaSpan(50, msgs("array", "user", "plain vanilla")),
		moved,
		deltaSpan(52, msgs("array", "user", "alpha bravo")),
		deltaSpan(52, msgs("array", "user", "charlie delta")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DuplicateSpans != 1 || stats.Messages.Written != 3 || stats.Messages.Present != 1 {
		t.Fatalf("replacement stats %+v", stats)
	}
	hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "creme brulee", Mode: MessageMatchPhrase})
	if len(hits) != 1 || hits[0].Span != ftsSpan(51) || hits[0].Start != ftsBase+51+1000 {
		t.Fatalf("moved span: %+v", hits)
	}
	for q, want := range map[string]int{"vanilla": 1, "alpha": 0, "charlie delta": 1, "zebra": 1} {
		if got := ftsCount(t, ctx, e, MessageFullTextQuery{Query: q}); got != want {
			t.Fatalf("%q after replacement: %d hits, want %d", q, got, want)
		}
	}
	got, err := e.LoadSpanOTLP(ctx, ftsTrace(52), ftsSpan(52))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "last duplicate", got, deltaSpan(52, msgs("array", "user", "charlie delta")))

	// A span that stops referencing a message loses its reference; the
	// other spans of its trace keep theirs.
	shared := msgs("array", "user", "kiwi mango papaya")
	a, b := deltaSpan(60, shared), deltaSpan(61, shared)
	sameTrace([]*tracepb.ResourceSpans{a, b})
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{a, b}); err != nil {
		t.Fatal(err)
	}
	a2 := deltaSpan(60, msgs("array", "user", "plain vanilla"))
	sameTrace([]*tracepb.ResourceSpans{a, a2})
	stats, err = e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{a2})
	if err != nil || stats.Messages.References != 1 {
		t.Fatalf("re-export: %+v %v", stats.Messages, err)
	}
	hits, _ = ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "mango papaya", Mode: MessageMatchPhrase})
	if len(hits) != 1 || hits[0].Span != ftsSpan(61) {
		t.Fatalf("history after a span dropped it: %+v", hits)
	}
	if got, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "mango", FirstTurnOnly: true}); len(got) != 1 || got[0].Span != ftsSpan(61) {
		t.Fatalf("first turn after a span dropped it: %+v", got)
	}
}

func TestMessageRoleDiscoveryChargesRawBunchForAbsentToken(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Tenant, c.MaxBytes = 0, 128 })
	values := make([]*commonpb.AnyValue, 20)
	for i := range values {
		values[i] = deltaKvlist(deltaKV("role", deltaStr("user")), deltaKV("content", deltaStr("quokka")), deltaKV("metadata", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i)}}))
	}
	input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(values...)))
	if st, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input}); err != nil || st.Messages.Written != 20 || st.Messages.Postings != 20 {
		t.Fatalf("fixture: %+v / %v", st.Messages, err)
	}
	var rawBytes int64
	if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
		store, err := e.backend.open(rc)
		if err != nil {
			return err
		}
		idx := e.backend.md.GetIndex(messageTextIndexName)
		rows, err := rc.Transaction().GetRange(store.IndexSubspace(idx), fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(rows) != 1 || len(rows[0].Value) <= 128 {
			return fmt.Errorf("need one multi-entry raw bunch >128: %+v", rows)
		}
		rawBytes = int64(len(rows[0].Key) + len(rows[0].Value))
		props := recordlayer.ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = 1
		cur := store.ScanIndexByType(idx, recordlayer.IndexScanByTextToken, recordlayer.TupleRangeAllOf(tuple.Tuple{int64(0)}), nil, props)
		defer cur.Close()
		r, err := cur.OnNext(ctx)
		if err != nil {
			return err
		}
		if !r.HasNext() {
			return errors.New("missing logical positive control")
		}
		entry := r.GetValue()
		if logical := len(entry.Key.Pack()) + len(entry.Value.Pack()); logical != 24 {
			return fmt.Errorf("first logical entry=%d want24, raw=%d", logical, rawBytes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page, err := e.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "zzzzzzzzzz"})
	requireTyped[*QueryBudgetError](t, "absent-token role discovery", err)
	if page.Bytes != rawBytes || page.Candidates != 1 || page.Continuation != nil || len(page.Hits) != 0 || e.backend.LastQueryWork().Bytes != rawBytes {
		t.Fatalf("raw failure accounting: %+v want%d", page, rawBytes)
	}
	cfg := e.Config()
	cfg.MaxBytes = 4 << 20
	other, err := NewExporter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Shutdown(ctx)
	page, err = other.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "zzzzzzzzzz"})
	if err != nil || len(page.Hits) != 0 || page.Continuation != nil || page.Bytes != rawBytes || page.Candidates != 1 || other.backend.LastQueryWork().Bytes != rawBytes {
		t.Fatalf("empty-result raw positive accounting: %+v / %v want%d", page, err, rawBytes)
	}
	if n := ftsCount(t, ctx, other, MessageFullTextQuery{Query: "quokka", Role: "user"}); n != 20 {
		t.Fatalf("searchable neighbor: %d", n)
	}
}

func testMessageBudget(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	for name, mutate := range map[string]func(*ExporterConfig){
		"candidates": func(c *ExporterConfig) { c.MaxCandidates = 3 },
		"bytes":      func(c *ExporterConfig) { c.MaxBytes = 128 },
	} {
		e := openFTSExporter(t, ctx, cluster, mutate)
		if _, err := e.ExportResourceSpans(ctx, deltaConversation(5, "array")); err != nil {
			t.Fatal(err)
		}
		page, err := e.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn"})
		requireOtelBudget(t, err)
		if page.Candidates == 0 || page.Continuation != nil || e.backend.LastQueryWork().Candidates != page.Candidates {
			t.Fatalf("%s: budget failure must report work and carry no continuation: %+v", name, page)
		}
		_, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "turn"}}})
		requireOtelBudget(t, err)
		for _, mode := range []string{MessageMatchAll, MessageMatchPhrase} {
			page, err = e.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "user turn", Mode: mode, Role: "user"})
			requireOtelBudget(t, err)
			if page.Candidates == 0 || page.Continuation != nil {
				t.Fatalf("%s %s: intersection budget failure: %+v", name, mode, page)
			}
		}
	}
	off := openTestExporter(t, ctx, cluster, nil)
	_, err := off.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn"})
	requireTyped[*ConfigError](t, "full text disabled", err)
	_, err = off.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "turn"}}})
	requireTyped[*ConfigError](t, "existential full text disabled", err)
	deltas := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	if deltas.backend.md.GetIndex(messageTextIndexName) != nil || deltas.backend.md.GetIndex(envelopeRefsIndexName) != nil {
		t.Fatal("the deltas layout has no text index or back references")
	}
	_, err = deltas.backend.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn"})
	requireTyped[*ConfigError](t, "deltas layout full text", err)
}

func testMessageExistential(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	if _, err := e.ExportResourceSpans(ctx, deltaConversation(6, "array")); err != nil {
		t.Fatal(err)
	}
	eval := exKV("gen_ai.evaluation.name", exStr("relevance"))
	if _, err := e.ExportLogs(ctx, []LogRecord{{EventID: "eval-5", Timestamp: ftsBase + 100, TraceID: ftsTrace(5), Attributes: []*commonpb.KeyValue{eval}}}); err != nil {
		t.Fatal(err)
	}
	chat := Attribute{"span", "gen_ai.operation.name", "string", "chat"}
	prompt := MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase}
	traces := func(r TraceExistentialResult) []int {
		var out []int
		for _, h := range r.Hits {
			out = append(out, int(h.Latest-ftsBase))
		}
		return out
	}
	got, err := e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{chat}, MessageClauses: []MessageFullTextQuery{prompt}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(traces(got), []int{6, 5, 4, 3}) || got.Candidates == 0 {
		t.Fatalf("span+message existential: %v", traces(got))
	}
	for _, h := range got.Hits {
		k := int(h.Latest - ftsBase)
		mw := h.MessageWitnesses
		if h.Trace != ftsTrace(k) || len(h.Witnesses) != 1 || len(mw) != 1 || len(mw[0]) != 1 || mw[0][0].Span != ftsSpan(k) || mw[0][0].AttributeKey != "gen_ai.input.messages" {
			t.Fatalf("turn %d witnesses: %+v", k, h)
		}
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{chat}, LogClauses: []Attribute{{LogScopeRecord, "gen_ai.evaluation.name", "string", "relevance"}}, MessageClauses: []MessageFullTextQuery{prompt}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Trace != ftsTrace(5) || got.Hits[0].Latest != ftsBase+100 || len(got.Hits[0].LogWitnesses) != 1 || len(got.Hits[0].MessageWitnesses[0]) != 1 {
		t.Fatalf("span+log+message existential: %+v", got.Hits)
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{
		{Query: "user turn 2", Mode: MessageMatchPhrase},
		{Query: "assistant turn 4", Mode: MessageMatchPhrase},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(traces(got), []int{6, 5, 4}) {
		t.Fatalf("two message clauses: %v", traces(got))
	}
	if w := got.Hits[2].MessageWitnesses; len(got.Hits[2].Witnesses) != 0 || len(w) != 2 || w[0][0].AttributeKey != "gen_ai.input.messages" || w[1][0].AttributeKey != "gen_ai.output.messages" {
		t.Fatalf("turn 4 message witnesses: %+v", got.Hits[2])
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "assistant turn 2", Mode: MessageMatchPhrase, FirstTurnOnly: true, AttributeKey: "gen_ai.output.messages"}}, TimeWindow: &TimeRange{Start: 0, End: ftsBase + 6}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{2}) {
		t.Fatalf("first-turn existential: %v %v", traces(got), err)
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "user turn 2", Mode: MessageMatchPhrase, TimeWindow: &TimeRange{Start: ftsBase + 3, End: ftsBase + 10}}}, TimeWindow: &TimeRange{Start: 0, End: ftsBase + 5}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{4, 3}) {
		t.Fatalf("intersected windows: %v %v", traces(got), err)
	}
	for name, q := range map[string]TraceExistentialQuery{
		"message limit":        {MessageClauses: []MessageFullTextQuery{{Query: "turn", Limit: 2}}},
		"message continuation": {MessageClauses: []MessageFullTextQuery{{Query: "turn", Continuation: []byte{1}}}},
		"message mode":         {MessageClauses: []MessageFullTextQuery{{Query: "turn", Mode: "regex"}}},
		"message no terms":     {MessageClauses: []MessageFullTextQuery{{Query: "--"}}},
		"message attribute":    {Clauses: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}},
	} {
		_, err := e.QueryTraceExistential(ctx, q)
		requireTyped[*ConfigError](t, name, err)
	}
}

// testMessageRareTerm pins the leapfrog intersection: pairing a term in
// every message with a rare one reads far fewer entries than the common
// term's postings, and answers exactly.
// testMessageChunkedRefs exports spans whose references exceed one
// envelope_refs_v1 value; every reference stays findable, in every chunk.
func testMessageChunkedRefs(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	const n = 2500
	msgs := make([]deltaMsg, n)
	for i := range msgs {
		msgs[i] = deltaMsg{"user", fmt.Sprintf("wombat%d numbat", i)}
	}
	// Two spans of one trace: the second repeats the first's input.
	spans := sameTrace([]*tracepb.ResourceSpans{
		deltaSpan(70, deltaKV("gen_ai.input.messages", deltaMessages("array", msgs))),
		deltaSpan(71, deltaKV("gen_ai.input.messages", deltaMessages("array", append(msgs, deltaMsg{"user", "last wombat"})))),
	})
	stats, err := e.ExportResourceSpans(ctx, spans)
	if err != nil {
		t.Fatal(err)
	}
	if m := stats.Messages; m.References != 2*n+1 || m.TraceMarkers != n+1 {
		t.Fatalf("message stats %+v", m)
	}
	if got := ftsIndexEntries(t, ctx, e, envelopeRefsIndexName, tuple.Tuple{int64(e.Config().Tenant), ftsTrace(70)}); got < 4 {
		t.Fatalf("%d span reference entries, want at least two chunks per span", got)
	}
	for _, i := range []int{0, n / 2, n - 1} {
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("wombat%d", i)})
		if len(hits) != 2 || !reflect.DeepEqual(hits[0].TurnOrdinals, []int{i}) || !reflect.DeepEqual(hits[1].TurnOrdinals, []int{i}) {
			t.Fatalf("wombat%d: %+v", i, hits)
		}
		first, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("wombat%d", i), FirstTurnOnly: true})
		if want := map[bool]int{true: 1, false: 0}[i == n-1]; len(first) != want || (want == 1 && first[0].Span != ftsSpan(70)) {
			t.Fatalf("wombat%d first turn: %+v", i, first)
		}
	}
}

func testMessageRareTerm(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	const spans, per = 60, 10
	const n = spans * per
	rare := map[int]string{7: "zircon common filler", 123: "zircon common filler", 456: "zircon common filler", 599: "common filler zircon"}
	text := func(i int) string {
		head, parity := "common filler", "even"
		if r, ok := rare[i]; ok {
			head = r
		}
		if i%2 == 1 {
			parity = "odd"
		}
		return fmt.Sprintf("%s item%d %s", head, i, parity)
	}
	role := func(i int) string { return map[bool]string{true: "user", false: "assistant"}[i%2 == 0] }
	input := make([]*tracepb.ResourceSpans, 0, spans)
	for s := 1; s <= spans; s++ {
		msgs := make([]deltaMsg, per)
		for j := range msgs {
			i := (s-1)*per + j
			msgs[j] = deltaMsg{role(i), text(i)}
		}
		input = append(input, deltaSpan(s, deltaKV("gen_ai.input.messages", deltaMessages("array", msgs))))
	}
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages.Written != n || stats.Messages.References != n {
		t.Fatalf("corpus message stats %+v", stats.Messages)
	}
	for _, c := range []struct {
		name string
		q    MessageFullTextQuery
		want func(i int) bool
		rare bool
	}{
		{"all rare", MessageFullTextQuery{Query: "zircon common", Role: "user"}, func(i int) bool { _, ok := rare[i]; return ok && i%2 == 0 }, true},
		{"all rare role", MessageFullTextQuery{Query: "common zircon", Role: "assistant"}, func(i int) bool { _, ok := rare[i]; return ok && i%2 == 1 }, true},
		{"phrase rare", MessageFullTextQuery{Query: "zircon common", Mode: MessageMatchPhrase, Role: "user"}, func(i int) bool { return rare[i] == "zircon common filler" && i%2 == 0 }, true},
		{"phrase rare tail", MessageFullTextQuery{Query: "filler zircon", Mode: MessageMatchPhrase}, func(i int) bool { return i == 599 }, false},
		{"phrase rare absent", MessageFullTextQuery{Query: "common zircon", Mode: MessageMatchPhrase}, func(int) bool { return false }, false},
		{"all rare item", MessageFullTextQuery{Query: "filler item123"}, func(i int) bool { return i == 123 }, false},
		{"all common", MessageFullTextQuery{Query: "common filler"}, func(int) bool { return true }, false},
		{"all common half", MessageFullTextQuery{Query: "common even"}, func(i int) bool { return i%2 == 0 }, false},
		{"all common wrong role", MessageFullTextQuery{Query: "common even", Role: "assistant"}, func(int) bool { return false }, false},
		{"phrase common reversed", MessageFullTextQuery{Query: "filler common", Mode: MessageMatchPhrase}, func(int) bool { return false }, false},
		{"any", MessageFullTextQuery{Query: "item7 item8 zeppelin", Mode: MessageMatchAny}, func(i int) bool { return i == 7 || i == 8 }, false},
	} {
		want := 0
		for i := range n {
			if c.want(i) {
				want++
			}
		}
		q := c.q
		q.Limit = 2 * n
		hits, _ := ftsDrain(t, ctx, e, q)
		if len(hits) != want {
			t.Fatalf("%s: %d hits, want %d", c.name, len(hits), want)
		}
		if c.rare {
			page, err := e.QueryMessageFullText(ctx, q)
			if err != nil || len(page.Hits) != want {
				t.Fatalf("%s: page %+v %v", c.name, page, err)
			}
			if page.Candidates >= n/4 {
				t.Fatalf("%s: page charged %d candidates against %d common postings", c.name, page.Candidates, n)
			}
		}
	}
}

// testMessageLarge stores 1MB messages: one of text, cut into overlapping
// segments, and one of incompressible bytes, cut into raw parts.
func testMessageLarge(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	text := largeText(1 << 20)
	segs, _, err := segmentMessageText(text, messageSegmentTokens)
	if err != nil || len(segs) < 3 {
		t.Fatalf("fixture: %d segments, %v", len(segs), err)
	}
	whole, err := messageTokenizer{}.TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	blob := deltaRandom(1<<20+17, 21)
	span := deltaSpan(
		70,
		deltaKV("gen_ai.input.messages", deltaArray(deltaKvlist(deltaKV("role", deltaStr("user")), deltaKV("content", deltaStr(text))))),
		deltaKV("gen_ai.output.messages", deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: blob}})),
	)
	before := proto.Clone(span).(*tracepb.ResourceSpans)
	stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{span})
	if err != nil {
		t.Fatal(err)
	}
	if m := stats.Messages; m.Written != 2 || m.Parts < int64(len(segs))+2 || m.UnindexedTokens != 0 || m.Transactions < 4 {
		t.Fatalf("large message stats %+v", m)
	}
	got, err := e.LoadSpanOTLP(ctx, ftsTrace(70), ftsSpan(70))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "1MB messages", got, before)
	if n := ftsIndexEntries(t, ctx, e, messageLargeIndexName, tuple.Tuple{int64(e.Config().Tenant)}); n != 1 {
		t.Fatalf("%d large messages", n)
	}
	// Phrases across each segment boundary, of the longest supported length.
	for i := 1; i < len(segs); i++ {
		prevEnd := segs[i].offset + messageSegmentOverlap // segment i-1 ends here
		if prevEnd+maxMessageQueryTerms/2 > len(whole) {
			continue
		}
		phrase := whole[prevEnd-maxMessageQueryTerms/2 : prevEnd+maxMessageQueryTerms/2]
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: strings.Join(phrase, " "), Mode: MessageMatchPhrase})
		if len(hits) != 1 || hits[0].Span != ftsSpan(70) || hits[0].Role != "user" {
			t.Fatalf("phrase across boundary %d: %+v", i, hits)
		}
	}
	// Terms only in the first and only in the last segment match "all".
	firstOnly, lastOnly := whole[4], whole[len(whole)-1]
	if firstOnly != "0" {
		t.Fatalf("fixture: first line number token %q", firstOnly)
	}
	if got := ftsCount(t, ctx, e, MessageFullTextQuery{Query: firstOnly + " " + lastOnly}); got != 1 {
		t.Fatalf("all across segments %q %q: %d hits", firstOnly, lastOnly, got)
	}
	if got := ftsCount(t, ctx, e, MessageFullTextQuery{Query: firstOnly + " " + lastOnly, Mode: MessageMatchPhrase}); got != 0 {
		t.Fatalf("phrase of distant terms: %d hits", got)
	}
	// Storing it again finds it whole.
	stats, err = e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{before})
	if err != nil || stats.Messages.Written != 0 || stats.Messages.Present != 2 {
		t.Fatalf("re-export: %+v %v", stats.Messages, err)
	}
}

// testMessageConcurrentSave stores one new message from several exporters
// of one run at once: it is stored once and every span reconstructs.
// testMessageGroupCommit holds every writer while calls queue their messages:
// once writers free up, one transaction stores the messages of several
// calls, each call gets its own counts, and a call cancelled while queued
// stores nothing and leaves the others unaffected.
func testMessageGroupCommit(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	b := e.backend
	held := make([]*messageWriter, messageWriterLimit)
	for i := range held {
		w, err := b.acquireWriter(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held[i] = w
	}
	const calls, n = 4, 3
	var wg sync.WaitGroup
	stats := make([]ExportStats, calls+1)
	errs := make([]error, calls+1)
	cancelled, cancelCall := context.WithCancel(ctx)
	cancelledDone := make(chan struct{})
	for i := range calls + 1 {
		// n spans of one trace, each with its own message.
		var spans []*tracepb.ResourceSpans
		for k := range n {
			spans = append(spans, deltaSpan(100+k, deltaKV("gen_ai.input.messages", deltaMessages("array", []deltaMsg{{"user", fmt.Sprintf("call %d span %d platypus", i, k)}}))))
		}
		for _, rs := range spans {
			rs.ScopeSpans[0].Spans[0].TraceId = bytes.Repeat([]byte{byte(0x90 + i)}, 16)
		}
		callCtx := ctx
		if i == calls {
			callCtx = cancelled
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == calls {
				defer close(cancelledDone)
			}
			stats[i], errs[i] = e.ExportResourceSpans(callCtx, spans)
		}()
	}
	// Every call's messages are queued, none stored.
	waitFor := func(cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(time.Minute); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("messages were not queued")
			}
		}
	}
	queued := func() int {
		b.queue.mu.Lock()
		defer b.queue.mu.Unlock()
		return len(b.queue.pending)
	}
	waitFor(func() bool { return queued() == (calls+1)*n })
	cancelCall()
	<-cancelledDone
	for _, w := range held {
		b.releaseWriter(w)
	}
	wg.Wait()
	requireOtelKind(t, errs[calls], "canceled")
	if err := errors.Join(errs[:calls]...); err != nil {
		t.Fatal(err)
	}
	var total MessageStoreStats
	for i, s := range stats[:calls] {
		if s.Messages.Written != n || s.Messages.Present != 0 {
			t.Fatalf("call %d message stats %+v", i, s.Messages)
		}
		total.Add(s.Messages)
	}
	// One presence read per call, and one transaction for all their messages.
	if total.Transactions != calls+1 {
		t.Fatalf("%d transactions for %d calls, want %d", total.Transactions, calls, calls+1)
	}
	for i := range calls {
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("call %d", i), Mode: MessageMatchPhrase})
		if len(hits) != n {
			t.Fatalf("call %d: %d hits, want %d", i, len(hits), n)
		}
	}
	if hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("call %d", calls), Mode: MessageMatchPhrase}); len(hits) != 0 {
		t.Fatalf("the cancelled call's messages were stored: %+v", hits)
	}
}

func testMessageConcurrentSave(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	first := openFTSExporter(t, ctx, cluster, nil)
	const writers = 8
	exporters := []*Exporter{first}
	for len(exporters) < writers {
		e, err := NewExporter(ctx, first.Config())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
		exporters = append(exporters, e)
	}
	shared := deltaMessages("array", []deltaMsg{{"user", "concurrent shared quokka message"}})
	var wg sync.WaitGroup
	errs := make([]error, writers)
	spans := make([]*tracepb.ResourceSpans, writers)
	for i, e := range exporters {
		spans[i] = deltaSpan(80+i, deltaKV("gen_ai.input.messages", shared), deltaKV("gen_ai.output.messages", deltaMessages("array", []deltaMsg{{"assistant", fmt.Sprintf("own reply %d", i)}})))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{proto.Clone(spans[i]).(*tracepb.ResourceSpans)})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	hits, _ := ftsDrain(t, ctx, first, MessageFullTextQuery{Query: "quokka"})
	if len(hits) != writers {
		t.Fatalf("%d hits, want %d", len(hits), writers)
	}
	for _, h := range hits {
		if h.MessageID != hits[0].MessageID {
			t.Fatalf("one message stored under two IDs: %+v", hits)
		}
	}
	if n := ftsIndexEntries(t, ctx, first, messageHashIndexName, tuple.Tuple{int64(first.Config().Tenant)}); n != writers+1 {
		t.Fatalf("dedup index holds %d messages, want %d", n, writers+1)
	}
	for i, want := range spans {
		got, err := first.LoadSpanOTLP(ctx, ftsTrace(80+i), ftsSpan(80+i))
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("span %d", i), got, want)
	}
}

// testMessageLayoutRejection refuses runs whose stored message layout is not
// the configured one, including runs written before message records.
func testMessageLayoutRejection(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	plain := openTestExporter(t, ctx, cluster, nil)
	if _, err := plain.ExportResourceSpans(ctx, deltaConversation(1, "array")); err != nil {
		t.Fatal(err)
	}
	cfg := plain.Config()
	cfg.EnableMessageFullText = true
	_, err := NewExporter(ctx, cfg)
	requireTyped[*OwnershipError](t, "pre-record layout", err)
	if !strings.Contains(err.Error(), "msg_blob_v1") {
		t.Fatalf("rejection does not name the old layout: %v", err)
	}
	// An owner manifest as the old code wrote it, without a messages field.
	var m map[string]any
	key := plain.backend.keys.owner()
	if _, err := plain.backend.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		v, err := rc.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		return nil, json.Unmarshal(v, &m)
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["messages"]; ok || m["tool"] != ToolVersion {
		t.Fatalf("a run without messages must keep the old manifest wire format: %v", m)
	}
	ft := openFTSExporter(t, ctx, cluster, nil)
	cfg = ft.Config()
	cfg.EnableMessageFullText, cfg.EnableMessageDeltas = false, true
	_, err = NewExporter(ctx, cfg)
	requireTyped[*OwnershipError](t, "deltas over fulltext", err)
	cfg.EnableMessageDeltas = false
	_, err = NewExporter(ctx, cfg)
	requireTyped[*OwnershipError](t, "none over fulltext", err)
}
