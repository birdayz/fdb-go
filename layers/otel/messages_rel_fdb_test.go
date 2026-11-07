package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Ports of the message tests (messages_fdb_test.go and the
// message_*_regression_test.go files, rfcs/relational-exporter-migration.md
// §2.3) to the relational exporter. relMsgEnv is one relExporter with its
// message store (relParts.messages' default, newRelMessageStore): the tests
// export and read through the exporter's public methods and reach the
// message store's writers, queue and stage hook for the mechanism tests.

// relMsgEnv is one relExporter of a tenant store and its message store.
type relMsgEnv struct {
	h *qrHarness
	x *relExporter
	r *SpanReader      // x's reader
	m *relMessageStore // nil when messages are off
}

// relLayout sets cfg's message flags for layout.
func relLayout(c *ExporterConfig, layout string) {
	c.EnableMessageDeltas = layout != messagesNone
	c.EnableMessageFullText = layout == messagesFullText
}

// msgEnv opens an exporter on a fresh run's tenant 11 with the message layout.
func (h *qrHarness) msgEnv(t *testing.T, ctx context.Context, layout string, mutate func(*ExporterConfig)) *relMsgEnv {
	t.Helper()
	return h.msgEnvOn(t, ctx, freshRun(t), layout, mutate)
}

// msgEnvOn opens another exporter of run's tenant 11: its own writers,
// queue, claims and marker cache.
func (h *qrHarness) msgEnvOn(t *testing.T, ctx context.Context, run, layout string, mutate func(*ExporterConfig)) *relMsgEnv {
	t.Helper()
	return h.msgEnvWith(t, ctx, run, layout, mutate, relParts{})
}

func (h *qrHarness) msgEnvWith(t *testing.T, ctx context.Context, run, layout string, mutate func(*ExporterConfig), parts relParts) *relMsgEnv {
	t.Helper()
	cfg := relTestConfig(t, h.cluster, func(c *ExporterConfig) {
		c.Run = run
		relLayout(c, layout)
		if mutate != nil {
			mutate(c)
		}
	})
	x := startRelExporter(t, ctx, cfg, parts)
	e := &relMsgEnv{h: h, x: x, r: x.reader}
	if x.messages != nil {
		m, ok := x.messages.(*relMessageStore)
		if !ok && parts.messages == nil {
			t.Fatalf("default message hooks are %T", x.messages)
		}
		e.m = m
	}
	return e
}

// txn is ctx under the exporter's transaction policy and write slots, for
// calls into the message store's internals.
func (e *relMsgEnv) txn(ctx context.Context) context.Context {
	return withTxnLimiter(e.x.txnContext(ctx), e.x.txnSlots)
}

func (e *relMsgEnv) export(ctx context.Context, in []*tracepb.ResourceSpans) (ExportStats, error) {
	return e.x.ExportResourceSpans(ctx, in)
}

func (e *relMsgEnv) load(ctx context.Context, trace, span string) (*tracepb.ResourceSpans, error) {
	return e.x.LoadSpanOTLP(ctx, trace, span)
}

// loadWork is LoadSpanOTLP's read and the MESSAGES rows its reconstruction
// counts, which the public method does not report.
func (e *relMsgEnv) loadWork(ctx context.Context, trace, span string) (*tracepb.ResourceSpans, QueryResult, error) {
	const op = "load span"
	tid, _ := hex.DecodeString(trace)
	sid, _ := hex.DecodeString(span)
	var out *tracepb.ResourceSpans
	var work QueryResult
	err := e.r.store.read(e.txn(ctx), op, func(store *recordlayer.FDBRecordStore) error {
		pk, err := tableKey(store, op, spansRecordType, tid, sid)
		if err != nil {
			return err
		}
		rec, err := store.LoadRecord(pk)
		if err != nil || rec == nil {
			return fmt.Errorf("span %s/%s: %v %w", trace, span, rec, err)
		}
		rs, err := rowOTLP(op, rec.Record.ProtoReflect(), tid, sid)
		if err != nil {
			return err
		}
		w := newQueryWork(e.r.cfg)
		if err := e.m.reconstruct(ctx, store, rs, w); err != nil {
			return err
		}
		w.result(&work)
		out = rs
		return nil
	})
	return out, work, err
}

// run runs fn on the store in one transaction.
func (e *relMsgEnv) run(t *testing.T, ctx context.Context, fn func(*recordlayer.FDBRecordStore) error) {
	t.Helper()
	if err := e.r.store.run(e.txn(ctx), "test", fn); err != nil {
		t.Fatal(err)
	}
}

// entries counts index's entries under prefix (nil: all).
func (e *relMsgEnv) entries(t *testing.T, ctx context.Context, index string, prefix tuple.Tuple) int {
	t.Helper()
	n := 0
	e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
		n = 0
		w := &queryWork{maxCandidates: 1 << 30, maxBytes: 1 << 40}
		return scanIndex(ctx, store, "test", index, recordlayer.TupleRangeAllOf(prefix), false, w, func(*recordlayer.IndexEntry) (bool, error) {
			n++
			return false, nil
		})
	})
	return n
}

// presence is the stored ID of each element of rss's messages, invalid when absent.
func (e *relMsgEnv) presence(t *testing.T, ctx context.Context, rss []*tracepb.ResourceSpans) []messageID {
	t.Helper()
	out, err := e.presenceErr(ctx, rss)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// presenceErr is presence for goroutines other than the test's.
func (e *relMsgEnv) presenceErr(ctx context.Context, rss []*tracepb.ResourceSpans) ([]messageID, error) {
	d, err := decomposeMessages(ctx, rss, e.r.cfg.Tenant)
	if err != nil {
		return nil, err
	}
	hashes := map[messageDigest][]byte{}
	for _, el := range d.elements {
		hashes[el.digest] = otelsql.MessageContentHash(el.encoding, el.raw)
	}
	found, err := e.m.readMessagePresence(e.txn(ctx), d.elements, hashes)
	if err != nil {
		return nil, err
	}
	out := make([]messageID, len(d.elements))
	for i, el := range d.elements {
		out[i] = found[el.digest]
	}
	return out, nil
}

// relFtsLess is the v2 hit order: index order on (start, trace, span),
// descending (D3; v1's time order had span IDs ascending), then attribute
// key and message ID ascending.
func relFtsLess(t *testing.T, a, b MessageSpanHit) bool {
	if a.Start != b.Start {
		return a.Start > b.Start
	}
	if a.Trace != b.Trace {
		return a.Trace > b.Trace
	}
	if a.Span != b.Span {
		return a.Span > b.Span
	}
	if a.AttributeKey != b.AttributeKey {
		return a.AttributeKey < b.AttributeKey
	}
	return ftsMessageID(t, a.MessageID).less(ftsMessageID(t, b.MessageID))
}

// relFtsDrain is ftsDrain on a SpanReader: it follows continuations to
// exhaustion, checking the page contract and the hit order.
func relFtsDrain(t *testing.T, ctx context.Context, e *relMsgEnv, q MessageFullTextQuery) ([]MessageSpanHit, int) {
	t.Helper()
	limit := q.Limit
	if limit == 0 {
		limit = e.r.cfg.K
	}
	all := []MessageSpanHit{}
	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
		page, err := e.x.QueryMessageFullText(ctx, q)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", pages, q, err)
		}
		if page.Order != "time" || page.ReadVersion <= 0 || page.Hits == nil {
			t.Fatalf("page metadata: %+v", page)
		}
		if len(page.Hits) > limit || (len(page.Hits) == limit) != (page.Continuation != nil) {
			t.Fatalf("page %d: %d hits, limit %d, continuation=%v", pages, len(page.Hits), limit, page.Continuation != nil)
		}
		all = append(all, page.Hits...)
		if page.Continuation == nil {
			for i := 1; i < len(all); i++ {
				if !relFtsLess(t, all[i-1], all[i]) {
					t.Fatalf("hits %d and %d out of order or duplicated: %+v %+v", i-1, i, all[i-1], all[i])
				}
			}
			return all, pages
		}
		q.Continuation = page.Continuation
	}
}

func relFtsCount(t *testing.T, ctx context.Context, e *relMsgEnv, q MessageFullTextQuery) int {
	t.Helper()
	hits, _ := relFtsDrain(t, ctx, e, q)
	return len(hits)
}

// relFtsDesc is the turns lo..hi newest first, the v2 order of one span per turn.
func relFtsDesc(lo, hi int) []int {
	var out []int
	for k := hi; k >= lo; k-- {
		out = append(out, k)
	}
	return out
}

func TestRelMessagesRealFDB(t *testing.T) {
	t.Parallel()
	h := newQRHarness(t)
	for name, test := range map[string]func(*testing.T, *qrHarness){
		"conversation/array":         func(t *testing.T, h *qrHarness) { testRelMessageConversation(t, h, "array") },
		"conversation/json":          func(t *testing.T, h *qrHarness) { testRelMessageConversation(t, h, "json") },
		"trace_per_turn":             testRelMessageTracePerTurn,
		"unicode_events_replacement": testRelMessageReplacement,
		"budgets":                    testRelMessageBudget,
		"trace_existential":          testRelMessageExistential,
		"rare_term_intersection":     testRelMessageRareTerm,
		"chunked_references":         testRelMessageChunkedRefs,
		"large_message":              testRelMessageLarge,
		"concurrent_same_message":    testRelMessageConcurrentSave,
		"group_commit":               testRelMessageGroupCommit,
		"deltas_layout":              testRelMessageDeltas,
		"unknown_commit":             testRelMessageUnknownCommit,
		"size_fallback":              testRelMessageSizeFallback,
		"corruption":                 testRelMessageCorruption,
		"schema_dropped":             testRelMessageSchemaDropped,
		"interrupted_multipart":      testRelMessageInterruptedMultipart,
		"token_free_boundary":        testRelMessageTokenFreeBoundary,
		"canceled_owner":             testRelMessageCanceledOwner,
		"role_discovery_budget":      testRelMessageRoleDiscoveryBudget,
		"cross_part_all":             testRelMessageCrossPartAll,
		"span_batch_conflicts":       testRelMessageSpanBatchConflicts,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, h)
		})
	}
}

// Port of testMessageConversation. The trace order arm is gone (D2); the
// hash and reference index counts read MESSAGES_HASH and SPANS_REF_CHUNKS.
func testRelMessageConversation(t *testing.T, h *qrHarness, form string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	n := deltaTurns
	input := sameTrace(deltaConversation(n, form))
	trace := ftsTrace(1)
	before := cloneDeltaSpans(input)
	stats, err := e.export(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	refs := n*(n+1) + 3*n
	if stats.Spans != int64(n) || stats.MessageDeltas.TotalMessages != int64(refs) || stats.MessageDeltas.UniqueMessages != int64(2*n+3) {
		t.Fatalf("export stats %+v", stats)
	}
	if m := stats.Messages; m.Written != int64(2*n+3) || m.Present != 0 || m.Parts != m.Written || m.Postings == 0 || m.References != int64(refs) || m.TraceMarkers != int64(2*n+3) || m.Transactions < 2 || m.TextIndexWrites == 0 {
		t.Fatalf("first message stats %+v", m)
	}
	requireDeltaSpansEqual(t, input, before)
	// Storing a present message writes nothing to any index.
	again, err := e.export(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if m := again.Messages; m.Written != 0 || m.Present != int64(2*n+3) || m.Postings != 0 || m.TextIndexWrites != 0 || m.TextIndexKeyBytes != 0 || m.References != int64(refs) || m.TraceMarkers != 0 || m.Transactions != 1 {
		t.Fatalf("idempotent re-export message stats %+v", m)
	}
	for k, want := range before {
		got, err := e.load(ctx, trace, ftsSpan(k+1))
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
	if got := e.entries(t, ctx, messagesHashIndex, nil); got != 2*n+3 {
		t.Fatalf("dedup index holds %d messages", got)
	}
	// One reference entry per span, and one marker per message of the trace
	// even when another exporter writes the trace again.
	if got := e.entries(t, ctx, spansRefChunksIndex, tuple.Tuple{qaBytes(t, trace)}); got != n {
		t.Fatalf("%d span reference entries, want %d", got, n)
	}
	other := h.msgEnvOn(t, ctx, e.x.cfg.Run, messagesFullText, nil)
	if third, err := other.export(ctx, input); err != nil || third.Messages.TraceMarkers != 0 || third.Messages.Written != 0 {
		t.Fatalf("re-export through another exporter: %+v %v", third.Messages, err)
	}
	if got := e.entries(t, ctx, messagesLargeIndex, nil); got != 0 {
		t.Fatalf("%d large messages", got)
	}

	// Turn 3's prompt is in the input of turns 3..n and the newest element of turn 3's.
	prompt := MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase}
	hits, _ := relFtsDrain(t, ctx, e, prompt)
	if !reflect.DeepEqual(ftsTurns(hits), relFtsDesc(3, n)) {
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
	if got, _ := relFtsDrain(t, ctx, e, first); len(got) != 1 || got[0].Span != ftsSpan(3) || !reflect.DeepEqual(got[0].TurnOrdinals, []int{4}) {
		t.Fatalf("first-turn prompt: %+v", got)
	}
	reply := MessageFullTextQuery{Query: "Assistant Turn 3", Mode: MessageMatchPhrase, FirstTurnOnly: true}
	if got, _ := relFtsDrain(t, ctx, e, reply); len(got) != 1 || got[0].Span != ftsSpan(3) || got[0].AttributeKey != "gen_ai.output.messages" || got[0].Role != "assistant" {
		t.Fatalf("first-turn reply: %+v", got)
	}
	reply.FirstTurnOnly = false
	if got := relFtsCount(t, ctx, e, reply); got != 1+(n-3) {
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
		"system first turn":     {MessageFullTextQuery{Query: "system", FirstTurnOnly: true}, n},
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
		if got := relFtsCount(t, ctx, e, c.q); got != c.want {
			t.Fatalf("%s: %d hits, want %d", name, got, c.want)
		}
	}
	for _, role := range []string{"user", "assistant"} {
		hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Role: role, Limit: 50})
		for _, h := range hits {
			if h.Role != role {
				t.Fatalf("role %s filter returned %+v", role, h)
			}
		}
	}
	// Pages of any size concatenate to one answer.
	all, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Limit: 1000})
	for _, limit := range []int{1, 7, 100} {
		paged, pages := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Limit: limit})
		if !reflect.DeepEqual(paged, all) || pages != len(all)/limit+1 {
			t.Fatalf("limit %d: %d hits in %d pages, want %d", limit, len(paged), pages, len(all))
		}
	}
	// A token of one query does not resume another.
	page, err := e.x.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn", Limit: 1})
	if err != nil || page.Continuation == nil {
		t.Fatalf("first page: %+v %v", page, err)
	}
	_, err = e.x.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn", Role: "user", Limit: 1, Continuation: page.Continuation})
	requireTyped[*ConfigError](t, "token of another query", err)
	_, err = e.x.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.conversation.id", "string", "conv-283"}}, Continuation: page.Continuation})
	requireTyped[*ConfigError](t, "message token as same-span", err)
	// A message attribute holds manifests and is not an equality predicate.
	_, err = e.x.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}})
	requireTyped[*ConfigError](t, "message attribute predicate", err)
	_, err = e.x.Query(ctx, Attribute{"span", "gen_ai.output.messages", "string", "x"})
	requireTyped[*ConfigError](t, "message attribute query", err)
}

// Port of testMessageTracePerTurn (AGNOSTIC).
func testRelMessageTracePerTurn(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	const n = 6
	for _, reverse := range []bool{false, true} {
		e := h.msgEnv(t, ctx, messagesFullText, nil)
		conv := deltaConversation(n, "array")
		var refs int64
		if reverse {
			sameTrace(conv)
			for k := n - 1; k >= 0; k-- {
				stats, err := e.export(ctx, conv[k:k+1])
				if err != nil {
					t.Fatal(err)
				}
				refs += stats.Messages.References
			}
		} else {
			stats, err := e.export(ctx, conv)
			if err != nil {
				t.Fatal(err)
			}
			refs = stats.Messages.References
		}
		if want := int64(n*(n+1) + 3*n); refs != want {
			t.Fatalf("reverse=%v: %d references, want %d", reverse, refs, want)
		}
		hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, FirstTurnOnly: true})
		if len(hits) != 1 || hits[0].Span != ftsSpan(3) || !reflect.DeepEqual(hits[0].TurnOrdinals, []int{4}) {
			t.Fatalf("reverse=%v: first turn %+v", reverse, hits)
		}
		hits, _ = relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase})
		if !reflect.DeepEqual(ftsTurns(hits), []int{6, 5, 4, 3}) {
			t.Fatalf("reverse=%v: references %v", reverse, ftsTurns(hits))
		}
	}
}

// Port of testMessageReplacement (AGNOSTIC).
func testRelMessageReplacement(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	dessert := "Crème Brûlée ÜBER façade"
	msgs := func(form, role, text string) *commonpb.KeyValue {
		return deltaKV("gen_ai.input.messages", deltaMessages(form, []deltaMsg{{role, text}}))
	}
	event := deltaSpan(53)
	event.ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "gen_ai.client.inference.operation.details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.output.messages", deltaMessages("json", []deltaMsg{{"Assistant", "event borne zebra"}}))}}}
	if _, err := e.export(ctx, []*tracepb.ResourceSpans{deltaSpan(50, msgs("array", "user", dessert)), deltaSpan(51, msgs("json", "user", dessert)), event}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"creme brulee", "CRÈME BRÛLÉE", "Über FAÇADE"} {
		hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: q, Mode: MessageMatchPhrase})
		if len(hits) != 2 || hits[0].MessageID == hits[1].MessageID || hits[0].Role != "user" || hits[1].Role != "user" {
			t.Fatalf("%q: %+v", q, hits)
		}
	}
	for q, want := range map[string]int{"brulee creme": 0, "user": 0, "text": 0} {
		if got := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: q, Mode: MessageMatchPhrase}); got != want {
			t.Fatalf("%q: %d hits, want %d", q, got, want)
		}
	}
	zebra, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "zebra", Role: "assistant"})
	if len(zebra) != 1 || zebra[0].Span != ftsSpan(53) || zebra[0].AttributeKey != "gen_ai.output.messages" {
		t.Fatalf("event message: %+v", zebra)
	}

	// Re-exporting a span replaces its record; the last duplicate wins.
	moved := deltaSpan(51, msgs("json", "user", dessert))
	moved.ScopeSpans[0].Spans[0].StartTimeUnixNano += 1000
	stats, err := e.export(ctx, []*tracepb.ResourceSpans{
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
	hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "creme brulee", Mode: MessageMatchPhrase})
	if len(hits) != 1 || hits[0].Span != ftsSpan(51) || hits[0].Start != ftsBase+51+1000 {
		t.Fatalf("moved span: %+v", hits)
	}
	for q, want := range map[string]int{"vanilla": 1, "alpha": 0, "charlie delta": 1, "zebra": 1} {
		if got := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: q}); got != want {
			t.Fatalf("%q after replacement: %d hits, want %d", q, got, want)
		}
	}
	got, err := e.load(ctx, ftsTrace(52), ftsSpan(52))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "last duplicate", got, deltaSpan(52, msgs("array", "user", "charlie delta")))

	// A span that stops referencing a message loses its reference; the
	// other spans of its trace keep theirs.
	shared := msgs("array", "user", "kiwi mango papaya")
	a, b := deltaSpan(60, shared), deltaSpan(61, shared)
	sameTrace([]*tracepb.ResourceSpans{a, b})
	if _, err := e.export(ctx, []*tracepb.ResourceSpans{a, b}); err != nil {
		t.Fatal(err)
	}
	a2 := deltaSpan(60, msgs("array", "user", "plain vanilla"))
	sameTrace([]*tracepb.ResourceSpans{a, a2})
	stats, err = e.export(ctx, []*tracepb.ResourceSpans{a2})
	if err != nil || stats.Messages.References != 1 {
		t.Fatalf("re-export: %+v %v", stats.Messages, err)
	}
	hits, _ = relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "mango papaya", Mode: MessageMatchPhrase})
	if len(hits) != 1 || hits[0].Span != ftsSpan(61) {
		t.Fatalf("history after a span dropped it: %+v", hits)
	}
	if got, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: "mango", FirstTurnOnly: true}); len(got) != 1 || got[0].Span != ftsSpan(61) {
		t.Fatalf("first turn after a span dropped it: %+v", got)
	}
}

// Port of testMessageBudget. The work is the page's (§1.3, was
// LastQueryWork). Every store has MESSAGES_TEXT and SPANS_REF_CHUNKS now;
// the deltas layout leaves them empty (testRelMessageDeltas).
func testRelMessageBudget(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	for name, mutate := range map[string]func(*ExporterConfig){
		"candidates": func(c *ExporterConfig) { c.MaxCandidates = 3 },
		"bytes":      func(c *ExporterConfig) { c.MaxBytes = 128 },
	} {
		e := h.msgEnv(t, ctx, messagesFullText, mutate)
		if _, err := e.export(ctx, deltaConversation(5, "array")); err != nil {
			t.Fatal(err)
		}
		page, err := e.x.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn"})
		requireOtelBudget(t, err)
		if page.Candidates == 0 || page.Continuation != nil {
			t.Fatalf("%s: budget failure must report work and carry no continuation: %+v", name, page)
		}
		res, err := e.x.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "turn"}}})
		requireOtelBudget(t, err)
		if res.Candidates == 0 {
			t.Fatalf("%s: existential budget failure reports no work: %+v", name, res)
		}
		for _, mode := range []string{MessageMatchAll, MessageMatchPhrase} {
			page, err = e.x.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "user turn", Mode: mode, Role: "user"})
			requireOtelBudget(t, err)
			if page.Candidates == 0 || page.Continuation != nil {
				t.Fatalf("%s %s: intersection budget failure: %+v", name, mode, page)
			}
		}
	}
	off := h.msgEnv(t, ctx, messagesNone, nil)
	_, err := off.r.QueryMessageFullText(off.txn(ctx), MessageFullTextQuery{Query: "turn"})
	requireTyped[*ConfigError](t, "full text disabled", err)
	_, err = off.r.QueryTraceExistential(off.txn(ctx), TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "turn"}}})
	requireTyped[*ConfigError](t, "existential full text disabled", err)
	deltas := h.msgEnv(t, ctx, messagesDeltas, nil)
	_, err = deltas.r.QueryMessageFullText(deltas.txn(ctx), MessageFullTextQuery{Query: "turn"})
	requireTyped[*ConfigError](t, "deltas layout full text", err)
}

// Port of testMessageExistential (AGNOSTIC).
func testRelMessageExistential(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	if _, err := e.export(ctx, deltaConversation(6, "array")); err != nil {
		t.Fatal(err)
	}
	eval := exKV("gen_ai.evaluation.name", exStr("relevance"))
	if _, err := e.x.ExportLogs(ctx, []LogRecord{{EventID: "eval-5", Timestamp: ftsBase + 100, TraceID: ftsTrace(5), Attributes: []*commonpb.KeyValue{eval}}}); err != nil {
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
	query := func(q TraceExistentialQuery) (TraceExistentialResult, error) {
		return e.x.QueryTraceExistential(ctx, q)
	}
	got, err := query(TraceExistentialQuery{Clauses: []Attribute{chat}, MessageClauses: []MessageFullTextQuery{prompt}})
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
	got, err = query(TraceExistentialQuery{Clauses: []Attribute{chat}, LogClauses: []Attribute{{LogScopeRecord, "gen_ai.evaluation.name", "string", "relevance"}}, MessageClauses: []MessageFullTextQuery{prompt}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Trace != ftsTrace(5) || got.Hits[0].Latest != ftsBase+100 || len(got.Hits[0].LogWitnesses) != 1 || len(got.Hits[0].MessageWitnesses[0]) != 1 {
		t.Fatalf("span+log+message existential: %+v", got.Hits)
	}
	got, err = query(TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{
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
	got, err = query(TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "assistant turn 2", Mode: MessageMatchPhrase, FirstTurnOnly: true, AttributeKey: "gen_ai.output.messages"}}, TimeWindow: &TimeRange{Start: 0, End: ftsBase + 6}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{2}) {
		t.Fatalf("first-turn existential: %v %v", traces(got), err)
	}
	got, err = query(TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "user turn 2", Mode: MessageMatchPhrase, TimeWindow: &TimeRange{Start: ftsBase + 3, End: ftsBase + 10}}}, TimeWindow: &TimeRange{Start: 0, End: ftsBase + 5}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{4, 3}) {
		t.Fatalf("intersected windows: %v %v", traces(got), err)
	}
	// A typed predicate inside a message clause (D1).
	got, err = query(TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "user turn 2", Mode: MessageMatchPhrase, Predicates: []Attribute{{"span", "gen_ai.conversation.id", "string", "conv-283"}}}}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{6, 5, 4, 3, 2}) {
		t.Fatalf("message clause predicate: %v %v", traces(got), err)
	}
	for name, q := range map[string]TraceExistentialQuery{
		"message limit":        {MessageClauses: []MessageFullTextQuery{{Query: "turn", Limit: 2}}},
		"message continuation": {MessageClauses: []MessageFullTextQuery{{Query: "turn", Continuation: []byte{1}}}},
		"message mode":         {MessageClauses: []MessageFullTextQuery{{Query: "turn", Mode: "regex"}}},
		"message no terms":     {MessageClauses: []MessageFullTextQuery{{Query: "--"}}},
		"message attribute":    {Clauses: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}},
		"message predicate":    {MessageClauses: []MessageFullTextQuery{{Query: "turn", Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}}}},
	} {
		_, err := query(q)
		requireTyped[*ConfigError](t, name, err)
	}
}

// Port of testMessageChunkedRefs. A chunk is bounded by its index key
// element (otelsql.MaxRefChunkKeyBytes, was a 16 KB value).
func testRelMessageChunkedRefs(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
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
	stats, err := e.export(ctx, spans)
	if err != nil {
		t.Fatal(err)
	}
	if m := stats.Messages; m.References != 2*n+1 || m.TraceMarkers != n+1 {
		t.Fatalf("message stats %+v", m)
	}
	if got := e.entries(t, ctx, spansRefChunksIndex, tuple.Tuple{qaBytes(t, ftsTrace(70))}); got < 4 {
		t.Fatalf("%d span reference entries, want at least two chunks per span", got)
	}
	for _, i := range []int{0, n / 2, n - 1} {
		hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("wombat%d", i)})
		if len(hits) != 2 || !reflect.DeepEqual(hits[0].TurnOrdinals, []int{i}) || !reflect.DeepEqual(hits[1].TurnOrdinals, []int{i}) {
			t.Fatalf("wombat%d: %+v", i, hits)
		}
		first, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("wombat%d", i), FirstTurnOnly: true})
		if want := map[bool]int{true: 1, false: 0}[i == n-1]; len(first) != want || (want == 1 && first[0].Span != ftsSpan(70)) {
			t.Fatalf("wombat%d first turn: %+v", i, first)
		}
	}
}

// Port of testMessageRareTerm (AGNOSTIC): rare-term intersections charge
// far fewer candidates than the common postings.
func testRelMessageRareTerm(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
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
	stats, err := e.export(ctx, input)
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
		hits, _ := relFtsDrain(t, ctx, e, q)
		if len(hits) != want {
			t.Fatalf("%s: %d hits, want %d", c.name, len(hits), want)
		}
		if c.rare {
			page, err := e.x.QueryMessageFullText(ctx, q)
			if err != nil || len(page.Hits) != want {
				t.Fatalf("%s: page %+v %v", c.name, page, err)
			}
			if page.Candidates >= n/4 {
				t.Fatalf("%s: page charged %d candidates against %d common postings", c.name, page.Candidates, n)
			}
		}
	}
}

// Port of testMessageLarge: 1MB messages, one of text cut into overlapping
// segments, one of incompressible bytes cut into raw parts.
func testRelMessageLarge(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	text := largeText(1 << 20)
	segs, _, err := segmentMessageText(text, messageSegmentTokens)
	if err != nil || len(segs) < 3 {
		t.Fatalf("fixture: %d segments, %v", len(segs), err)
	}
	whole, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
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
	stats, err := e.export(ctx, []*tracepb.ResourceSpans{span})
	if err != nil {
		t.Fatal(err)
	}
	if m := stats.Messages; m.Written != 2 || m.Parts < int64(len(segs))+2 || m.UnindexedTokens != 0 || m.Transactions < 4 {
		t.Fatalf("large message stats %+v", m)
	}
	got, work, err := e.loadWork(ctx, ftsTrace(70), ftsSpan(70))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "1MB messages", got, before)
	// Every row read is counted: both heads and the second raw part of each
	// (each element is over 1 MiB); the text's other parts hold only text.
	// Bytes are stored bytes, zlib-1 compressed: the random blob's at least.
	if work.Candidates != 4 || work.Bytes < int64(len(blob)) {
		t.Fatalf("reconstruction work %+v", work)
	}
	if n := e.entries(t, ctx, messagesLargeIndex, nil); n != 1 {
		t.Fatalf("%d large messages", n)
	}
	// Phrases across each segment boundary, of the longest supported length.
	for i := 1; i < len(segs); i++ {
		prevEnd := segs[i].offset + messageSegmentOverlap // segment i-1 ends here
		if prevEnd+maxMessageQueryTerms/2 > len(whole) {
			continue
		}
		phrase := whole[prevEnd-maxMessageQueryTerms/2 : prevEnd+maxMessageQueryTerms/2]
		hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: strings.Join(phrase, " "), Mode: MessageMatchPhrase})
		if len(hits) != 1 || hits[0].Span != ftsSpan(70) || hits[0].Role != "user" {
			t.Fatalf("phrase across boundary %d: %+v", i, hits)
		}
	}
	// Terms only in the first and only in the last segment match "all".
	firstOnly, lastOnly := whole[4], whole[len(whole)-1]
	if firstOnly != "0" {
		t.Fatalf("fixture: first line number token %q", firstOnly)
	}
	if got := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: firstOnly + " " + lastOnly}); got != 1 {
		t.Fatalf("all across segments %q %q: %d hits", firstOnly, lastOnly, got)
	}
	if got := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: firstOnly + " " + lastOnly, Mode: MessageMatchPhrase}); got != 0 {
		t.Fatalf("phrase of distant terms: %d hits", got)
	}
	// Storing it again finds it whole.
	stats, err = e.export(ctx, []*tracepb.ResourceSpans{before})
	if err != nil || stats.Messages.Written != 0 || stats.Messages.Present != 2 {
		t.Fatalf("re-export: %+v %v", stats.Messages, err)
	}
}

// Port of testMessageConcurrentSave: exporters of one store dedup a message
// through the serializable MESSAGES_HASH read.
func testRelMessageConcurrentSave(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	first := h.msgEnv(t, ctx, messagesFullText, nil)
	const writers = 8
	envs := []*relMsgEnv{first}
	for len(envs) < writers {
		envs = append(envs, h.msgEnvOn(t, ctx, first.r.cfg.Run, messagesFullText, nil))
	}
	shared := deltaMessages("array", []deltaMsg{{"user", "concurrent shared quokka message"}})
	var wg sync.WaitGroup
	errs := make([]error, writers)
	spans := make([]*tracepb.ResourceSpans, writers)
	for i, e := range envs {
		spans[i] = deltaSpan(80+i, deltaKV("gen_ai.input.messages", shared), deltaKV("gen_ai.output.messages", deltaMessages("array", []deltaMsg{{"assistant", fmt.Sprintf("own reply %d", i)}})))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.export(ctx, []*tracepb.ResourceSpans{proto.Clone(spans[i]).(*tracepb.ResourceSpans)})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	// A read's cached version is only as new as its own client's last commit
	// (RFC §3.2 "Read version"): first commits after the others did, so it
	// reads their writes.
	if _, err := first.export(ctx, []*tracepb.ResourceSpans{deltaSpan(99)}); err != nil {
		t.Fatal(err)
	}
	hits, _ := relFtsDrain(t, ctx, first, MessageFullTextQuery{Query: "quokka"})
	if len(hits) != writers {
		t.Fatalf("%d hits, want %d", len(hits), writers)
	}
	for _, h := range hits {
		if h.MessageID != hits[0].MessageID {
			t.Fatalf("one message stored under two IDs: %+v", hits)
		}
	}
	if n := first.entries(t, ctx, messagesHashIndex, nil); n != writers+1 {
		t.Fatalf("dedup index holds %d messages, want %d", n, writers+1)
	}
	// Every exporter allocated its writer from the one store-wide counter.
	ids := map[int64]bool{}
	for _, e := range envs {
		for _, w := range e.m.writers.free {
			if ids[w.id] {
				t.Fatalf("writer %d allocated twice", w.id)
			}
			ids[w.id] = true
		}
	}
	for i, want := range spans {
		got, err := first.load(ctx, ftsTrace(80+i), ftsSpan(80+i))
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("span %d", i), got, want)
	}
}

// Port of testMessageGroupCommit: shared transactions, per-call counts and a
// canceled caller's queued messages dropped, while every writer is held.
func testRelMessageGroupCommit(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	m := e.m
	held := make([]*messageWriter, messageWriterLimit)
	for i := range held {
		w, err := m.acquireWriter(e.txn(ctx))
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
			stats[i], errs[i] = e.export(callCtx, spans)
		}()
	}
	awaitMessageCondition(t, ctx, func() bool {
		m.queue.mu.Lock()
		defer m.queue.mu.Unlock()
		return len(m.queue.pending) == (calls+1)*n
	})
	cancelCall()
	<-cancelledDone
	for _, w := range held {
		m.releaseWriter(w)
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
		hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("call %d", i), Mode: MessageMatchPhrase})
		if len(hits) != n {
			t.Fatalf("call %d: %d hits, want %d", i, len(hits), n)
		}
	}
	if hits, _ := relFtsDrain(t, ctx, e, MessageFullTextQuery{Query: fmt.Sprintf("call %d", calls), Mode: MessageMatchPhrase}); len(hits) != 0 {
		t.Fatalf("the cancelled call's messages were stored: %+v", hits)
	}
}

// The deltas layout stores MESSAGES rows without text: MESSAGES_TEXT,
// MESSAGES_LARGE, MESSAGE_TRACES and SPANS_REF_CHUNKS stay empty, and spans
// reconstruct byte for byte.
func testRelMessageDeltas(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesDeltas, nil)
	input := deltaConversation(4, "json")
	before := cloneDeltaSpans(input)
	big := deltaSpan(9, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(largeText(3<<20)))))
	stats, err := e.export(ctx, append(input, big))
	if err != nil {
		t.Fatal(err)
	}
	if m := stats.Messages; m.Written == 0 || m.Postings != 0 || m.TextIndexWrites != 0 || m.TraceMarkers != 0 || m.References != 0 {
		t.Fatalf("deltas stats %+v", m)
	}
	e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
		// Raw rows: a TEXT index cannot be scanned by value.
		for _, name := range []string{messagesTextIndex, messagesLargeIndex, spansRefChunksIndex} {
			r, err := fdb.PrefixRange(store.IndexSubspace(store.GetRecordMetaData().GetIndex(name)).Bytes())
			if err != nil {
				return err
			}
			rows, err := store.Context().Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				t.Errorf("%s holds entries", name)
			}
		}
		pk, err := tableKey(store, "test", messageTracesRecordType)
		if err != nil {
			return err
		}
		r, err := fdb.PrefixRange(store.RecordsSubspace().Pack(pk))
		if err != nil {
			return err
		}
		rows, err := store.Context().Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		if err == nil && len(rows) != 0 {
			t.Error("deltas layout wrote trace markers")
		}
		return err
	})
	for k, want := range append(before, big) {
		sp := want.ScopeSpans[0].Spans[0]
		got, err := e.load(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("span %d", k), got, want)
	}
}

// Port of TestMessageUnknownCommitDoesNotReuseSequence: a commit reported
// ambiguous after it landed is retried, finds the message, and the writer's
// next message takes a later seq.
func testRelMessageUnknownCommit(t *testing.T, h *qrHarness) {
	for _, code := range []int{1021, 1039} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			m := e.m
			w, err := m.acquireWriter(e.txn(ctx))
			if err != nil {
				t.Fatal(err)
			}
			defer m.releaseWriter(w)
			inputs := []*tracepb.ResourceSpans{
				deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra original")))),
				deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("platypus distinct")))),
			}
			var ids []messageID
			var mu sync.Mutex
			committed := false
			m.hook = func(stage string, store *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" || committed {
					return nil
				}
				// Commit the real transaction, then report an ambiguous result to
				// the retry loop. Its next attempt must observe the dedup hit.
				if err := store.Context().Transaction().Commit().Get(); err != nil {
					return err
				}
				committed = true
				return fdb.Error{Code: code}
			}
			for i, input := range inputs {
				d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.r.cfg.Tenant)
				if err != nil {
					t.Fatal(err)
				}
				el := d.elements[0]
				p, err := planRelMessage(el, otelsql.MessageContentHash(el.encoding, el.raw), true, messageSegmentTokens)
				if err != nil {
					t.Fatal(err)
				}
				var stats MessageStoreStats
				if err := m.writeSmallMessages(e.txn(ctx), w, []*messagePlan{p}, &stats, &mu); err != nil {
					t.Fatal(err)
				}
				if i == 0 && (!committed || stats.Retries != 1 || stats.Present != 1) {
					t.Fatalf("ambiguous commit/retry control: committed=%t stats=%+v", committed, stats)
				}
				ids = append(ids, p.element.id)
				m.hook = nil
				if _, err := e.export(ctx, []*tracepb.ResourceSpans{input}); err != nil {
					t.Fatal(err)
				}
			}
			if ids[0].writer != ids[1].writer || ids[0].seq >= ids[1].seq {
				t.Fatalf("reused sequence after ambiguous commit: %v", ids)
			}
			for i, input := range inputs {
				got, err := e.load(ctx, ftsTrace(i+1), ftsSpan(i+1))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "reconstruction", got, input)
				if found := e.presence(t, ctx, []*tracepb.ResourceSpans{input}); found[0] != ids[i] {
					t.Fatalf("dedup identity changed: %v, want %v", found, ids[i])
				}
			}
			for _, word := range []string{"zebra original", "platypus distinct"} {
				if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: word, Mode: MessageMatchAll}); n != 1 {
					t.Fatalf("TEXT references for %q: %d", word, n)
				}
			}
		})
	}
}

// Port of TestQueuedMessageSizeFallbackCountersAndExhaustion:
// transaction_too_large replans at 256, 64 and 16 tokens, then fails with
// no span published.
func testRelMessageSizeFallback(t *testing.T, h *qrHarness) {
	for _, exhaust := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery", true: "exhaustion"}[exhaust], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			words := make([]string, 600)
			for i := range words {
				words[i] = fmt.Sprintf("term%04d", i)
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Join(words, " ")))))
			var calls atomic.Int64
			e.m.hook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage == "message-before-commit" {
					if n := calls.Add(1); n == 1 || exhaust {
						return fdb.Error{Code: 2101}
					}
				}
				return nil
			}
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if exhaust {
				if code, ok := fdbErrorCode(err); !ok || code != 2101 {
					t.Fatalf("expected terminal 2101: %v", err)
				}
				if calls.Load() != 4 {
					t.Fatalf("fallback attempts %d, want initial plus 256/64/16", calls.Load())
				}
				_, loadErr := e.load(ctx, ftsTrace(1), ftsSpan(1))
				var missing *SpanNotFoundError
				if !errors.As(loadErr, &missing) {
					t.Fatalf("failed export published a span: %v", loadErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() < 3 {
				t.Fatal("multipart fallback was not committed")
			}
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.r.cfg.Tenant)
			if err != nil {
				t.Fatal(err)
			}
			el := d.elements[0]
			plan, err := planRelMessage(el, otelsql.MessageContentHash(el.encoding, el.raw), true, messageSegmentTokens/4)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.records) <= 1 || stats.Messages.Parts != int64(len(plan.records)) || stats.Messages.Postings != plan.postings {
				t.Fatalf("actual fallback %+v; want parts %d/postings %d", stats.Messages, len(plan.records), plan.postings)
			}
			got, err := e.load(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "fallback reconstruction", got, input)
			if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "term0000 term0599", Mode: MessageMatchAll}); n != 1 {
				t.Fatalf("cross-part search got %d", n)
			}
			again, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if err != nil || again.Messages.Written != 0 || again.Messages.Present != 1 {
				t.Fatalf("fallback dedup %+v/%v", again, err)
			}
		})
	}
}

// Port of TestPersistedMessageCorruptionFailsClosed: tampered MESSAGES rows
// fail the load as corruption with no partial span.
func testRelMessageCorruption(t *testing.T, h *qrHarness) {
	for _, damage := range []string{"head", "part", "encoding", "parts", "length"} {
		t.Run(damage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			rawBytes := messagePartRawBytes + 100
			if damage == "parts" {
				rawBytes = 100 // a complete head isolates the parts<1 guard from the raw length check
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Repeat(" ", rawBytes)))))
			if _, err := e.export(ctx, []*tracepb.ResourceSpans{input}); err != nil {
				t.Fatal(err)
			}
			id := e.presence(t, ctx, []*tracepb.ResourceSpans{input})[0]
			if !id.valid() {
				t.Fatal("message not persisted")
			}
			e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
				if damage == "head" || damage == "part" {
					part := int64(0)
					if damage == "part" {
						part = 1
					}
					pk, err := tableKey(store, "test", messagesRecordType, id.writer, id.seq, part)
					if err != nil {
						return err
					}
					deleted, err := store.DeleteRecord(pk)
					if !deleted && err == nil {
						t.Error("mutation did not remove stored part")
					}
					return err
				}
				pk, err := tableKey(store, "test", messagesRecordType, id.writer, id.seq, int64(0))
				if err != nil {
					return err
				}
				r, err := store.LoadRecord(pk)
				if err != nil {
					return err
				}
				m := r.Record.(*otelv1pb.MESSAGES)
				switch damage {
				case "encoding":
					m.ENCODING = proto.String("wrong")
				case "parts":
					if m.GetPARTS() != 1 {
						return fmt.Errorf("single-head fixture has %d parts, want 1", m.GetPARTS())
					}
					m.PARTS = proto.Int64(0)
				case "length":
					m.RAW_BYTES = proto.Int64(messagePartRawBytes + 101)
				}
				_, err = store.SaveRecord(m)
				return err
			})
			sp := input.ScopeSpans[0].Spans[0]
			got, err := e.load(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
			requireOtelKind(t, err, "corruption")
			if got != nil {
				t.Fatal("corrupt load returned a partial span")
			}
		})
	}
}

// Port of TestMessageWritesCannotRepopulateDroppedRun: "dropped run" is a
// dropped schema. A message transaction racing DROP SCHEMA conflicts on the
// store header, its retry fails to open the store (ownership), and nothing
// is written under the dropped store's subspace.
func testRelMessageSchemaDropped(t *testing.T, h *qrHarness) {
	for _, stage := range []string{"writer-before-write", "message-before-write", "marker-before-write"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			var once sync.Once
			var dropped atomic.Bool
			e.m.hook = func(at string, _ *recordlayer.FDBRecordStore) error {
				if at == stage {
					once.Do(func() {
						if _, err := h.setup.ExecContext(ctx, `DROP SCHEMA "`+e.r.store.databasePath+"/"+e.r.store.schema+`"`); err != nil {
							t.Errorf("drop: %v", err)
						} else {
							dropped.Store(true)
						}
					})
				}
				return nil
			}
			_, err := e.export(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra"))))})
			if !dropped.Load() {
				t.Fatal("write boundary was not reached")
			}
			requireOtelKind(t, err, "ownership")
			if _, err := e.r.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				r, err := fdb.PrefixRange(e.r.store.ts.Subspace.Bytes())
				if err != nil {
					return nil, err
				}
				rows, err := rc.Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
				if err == nil && len(rows) != 0 {
					t.Errorf("dropped store repopulated: %x", rows[0].Key)
				}
				return nil, err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Port of TestInterruptedMultipartDoesNotPublishHeadOrSpan: a large message
// interrupted after a committed tail publishes no head, no hash and no span;
// the next export stores it whole.
func testRelMessageInterruptedMultipart(t *testing.T, h *qrHarness) {
	for _, failure := range []string{"error", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra "+strings.Repeat(" ", 3<<20)+" omega"))))
			attempt, stop := context.WithCancel(ctx)
			defer stop()
			sentinel := errors.New("later multipart transaction refused")
			var extraRetry, tailProven atomic.Bool
			var tailID messageID
			e.m.hook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				if !extraRetry.Swap(true) {
					return fdb.Error{Code: 1020}
				}
				tailProven.Store(false)
				tailID = messageID{}
				if _, err := e.r.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := e.r.store.ts.Builder(rc).Open()
					if err != nil {
						return nil, err
					}
					records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
					if err != nil {
						return nil, err
					}
					for _, r := range records {
						if m, ok := r.Record.(*otelv1pb.MESSAGES); ok && m.GetPART() != 0 {
							tailID = messageID{m.GetWRITER(), m.GetSEQ()}
							tailProven.Store(true)
						}
					}
					return nil, nil
				}); err != nil {
					return err
				}
				if !tailProven.Load() {
					return nil // still attempting the first tail transaction
				}
				if failure == "cancel" {
					stop()
					return context.Canceled
				}
				return sentinel
			}
			stats, err := e.export(attempt, []*tracepb.ResourceSpans{input})
			if failure == "error" && !errors.Is(err, sentinel) {
				t.Fatalf("terminal cause: %v", err)
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel cause: %v", err)
			}
			if !extraRetry.Load() || !tailProven.Load() || stats.Messages.Written != 0 || stats.Messages.Parts == 0 {
				t.Fatalf("committed-tail evidence/counters: %v / %+v", tailProven.Load(), stats.Messages)
			}
			e.m.hook = nil
			e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
				pk, err := tableKey(store, "test", messagesRecordType, tailID.writer, tailID.seq, int64(0))
				if err != nil {
					return err
				}
				head, err := store.LoadRecord(pk)
				if err == nil && head != nil {
					t.Error("interrupted message published head")
				}
				return err
			})
			if found := e.presence(t, ctx, []*tracepb.ResourceSpans{input}); found[0].valid() {
				t.Fatalf("interrupted hash present: %v", found)
			}
			_, err = e.load(ctx, ftsTrace(1), ftsSpan(1))
			var missing *SpanNotFoundError
			if !errors.As(err, &missing) {
				t.Fatalf("interrupted span published: %v", err)
			}
			if _, err := e.export(ctx, []*tracepb.ResourceSpans{input}); err != nil {
				t.Fatal(err)
			}
			got, err := e.load(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "multipart recovery", got, input)
			if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra omega", Mode: MessageMatchAll}); n != 1 {
				t.Fatalf("recovered search: %d", n)
			}
		})
	}
}

// Port of TestMessageTokenFreeByteBoundary (AGNOSTIC).
func testRelMessageTokenFreeBoundary(t *testing.T, h *qrHarness) {
	for _, n := range []int{messageSegmentBytes - 1, messageSegmentBytes, messageSegmentBytes + 1} {
		for _, char := range []string{" ", "!"} {
			t.Run(fmt.Sprintf("%x/%d", char, n), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				e := h.msgEnv(t, ctx, messagesFullText, nil)
				input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Repeat(char, n)))))
				stats, err := e.export(ctx, []*tracepb.ResourceSpans{input})
				if err != nil {
					t.Fatal(err)
				}
				if stats.Messages.Written != 1 || stats.Messages.Postings != 0 {
					t.Fatalf("unexpected token-free counters: %+v", stats.Messages)
				}
				sp := input.ScopeSpans[0].Spans[0]
				got, err := e.load(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "token-free message", got, input)
			})
		}
	}
}

// Port of TestCanceledMessageOwnerTwoWaitersShareOneDigest: when the call
// storing a message is canceled, the two calls awaiting it store it once.
func testRelMessageCanceledOwner(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	m := e.m
	var held []*messageWriter
	for range messageWriterLimit {
		w, err := m.acquireWriter(e.txn(ctx))
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, w)
	}
	defer func() {
		for _, w := range held {
			m.releaseWriter(w)
		}
	}()
	var awaits atomic.Int64
	m.hook = func(stage string, _ *recordlayer.FDBRecordStore) error {
		if stage == "message-await" {
			awaits.Add(1)
		}
		return nil
	}
	payload := func(i int) *tracepb.ResourceSpans {
		return deltaSpan(i, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("shared zebra"))))
	}
	ownerCtx, ownerCancel := context.WithCancel(ctx)
	defer ownerCancel()
	owner := make(chan error, 1)
	go func() { _, err := e.export(ownerCtx, []*tracepb.ResourceSpans{payload(1)}); owner <- err }()
	awaitMessageCondition(t, ctx, func() bool { m.queue.mu.Lock(); defer m.queue.mu.Unlock(); return len(m.queue.pending) == 1 })
	type outcome struct {
		stats ExportStats
		err   error
	}
	waiters := make(chan outcome, 2)
	for _, i := range []int{2, 3} {
		go func() {
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{payload(i)})
			waiters <- outcome{stats, err}
		}()
	}
	awaitMessageCondition(t, ctx, func() bool { return awaits.Load() == 2 })
	ownerCancel()
	requireOtelKind(t, <-owner, "canceled")
	awaitMessageCondition(t, ctx, func() bool {
		m.queue.mu.Lock()
		defer m.queue.mu.Unlock()
		n := 0
		for _, q := range m.queue.pending {
			if q.sub.err == nil {
				n++
			}
		}
		return n == 2
	})
	m.releaseWriter(held[0])
	held = held[1:]
	var written, present int64
	for range 2 {
		r := <-waiters
		if r.err != nil {
			t.Fatal(r.err)
		}
		written += r.stats.Messages.Written
		present += r.stats.Messages.Present
	}
	if written != 1 || present != 1 {
		t.Fatalf("same digest wrote %d/present %d", written, present)
	}
	if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra", Mode: MessageMatchAll}); n != 2 {
		t.Fatalf("waiters searchable spans %d", n)
	}
}

// Port of TestMessageRoleDiscoveryChargesRawBunchForAbsentToken: role
// discovery charges the raw bunch it reads, one candidate and its key and
// value bytes, even for a query whose token is absent; the budget edge is
// exact.
func testRelMessageRoleDiscoveryBudget(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	values := make([]*commonpb.AnyValue, 20)
	for i := range values {
		values[i] = deltaKvlist(deltaKV("role", deltaStr("user")), deltaKV("content", deltaStr("quokka")), deltaKV("metadata", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i)}}))
	}
	input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(values...)))
	if st, err := e.export(ctx, []*tracepb.ResourceSpans{input}); err != nil || st.Messages.Written != 20 || st.Messages.Postings != 20 {
		t.Fatalf("fixture: %+v / %v", st.Messages, err)
	}
	var rawBytes int64
	e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
		idx := store.GetRecordMetaData().GetIndex(messagesTextIndex)
		rows, err := store.Context().Transaction().GetRange(store.IndexSubspace(idx), fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(rows) != 1 || len(rows[0].Value) <= 128 {
			return fmt.Errorf("need one multi-entry raw bunch over 128 bytes: %d rows", len(rows))
		}
		rawBytes = int64(len(rows[0].Key) + len(rows[0].Value))
		return nil
	})
	for _, limit := range []int64{rawBytes, rawBytes - 1} {
		bounded := h.msgEnvOn(t, ctx, e.x.cfg.Run, messagesFullText, func(c *ExporterConfig) { c.MaxBytes = limit })
		page, err := bounded.x.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "zzzzzzzzzz"})
		if limit == rawBytes {
			if err != nil {
				t.Fatalf("exact byte budget: %v", err)
			}
		} else {
			requireTyped[*QueryBudgetError](t, "one byte under the bunch", err)
		}
		if page.Bytes != rawBytes || page.Candidates != 1 || page.Continuation != nil || len(page.Hits) != 0 {
			t.Fatalf("limit %d: role discovery work %+v, want 1 candidate of %d bytes", limit, page, rawBytes)
		}
	}
	if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "quokka", Role: "user"}); n != 20 {
		t.Fatalf("searchable neighbor: %d", n)
	}
}

// Port of TestCrossPartAllReadsEarlierSameWriterSignpost's query half: an
// "all" match whose terms lie in different parts of one message is found
// through MESSAGES_LARGE, as a page and as an existential clause.
func testRelMessageCrossPartAll(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	small := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra"))))
	if _, err := e.export(ctx, []*tracepb.ResourceSpans{small}); err != nil {
		t.Fatal(err)
	}
	words := []string{"zebra"}
	for i := range 1100 {
		words = append(words, fmt.Sprintf("term%04d", i))
	}
	words = append(words, "platypus")
	text := strings.Join(words, " ")
	segments, _, err := segmentMessageText(text, messageSegmentTokens)
	if err != nil || len(segments) < 2 {
		t.Fatalf("multipart text fixture: %d / %v", len(segments), err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.text, "zebra") && strings.Contains(segment.text, "platypus") {
			t.Fatal("single part satisfies all terms")
		}
	}
	large := deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
	if _, err := e.export(ctx, []*tracepb.ResourceSpans{large}); err != nil {
		t.Fatal(err)
	}
	ids := []messageID{e.presence(t, ctx, []*tracepb.ResourceSpans{small})[0], e.presence(t, ctx, []*tracepb.ResourceSpans{large})[0]}
	if !ids[0].valid() || ids[0].writer != ids[1].writer || ids[0].seq >= ids[1].seq {
		t.Fatalf("not earlier same writer: %v", ids)
	}
	q := MessageFullTextQuery{Query: "zebra platypus", Mode: MessageMatchAll, Limit: 1}
	hits, pages := relFtsDrain(t, ctx, e, q)
	if len(hits) != 1 || hits[0].Span != ftsSpan(2) || pages == 0 {
		t.Fatalf("bounded cross-part match: %+v pages=%d", hits, pages)
	}
	result, err := e.x.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: q.Query, Mode: q.Mode}}})
	if err != nil || len(result.Hits) != 1 || result.Hits[0].Trace != ftsTrace(2) {
		t.Fatalf("trace clause: %+v / %v", result, err)
	}
	if got := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra platypus", Mode: MessageMatchPhrase}); got != 0 {
		t.Fatalf("phrase of terms in different parts: %d", got)
	}
}

// R4 (§7): span batches saved with CoalesceIndexWriteConflicts widen write
// conflicts over the SPANS indexes only. A message transaction whose reads
// precede another exporter's committed coalesced span batch commits without
// a retry. The control, the other exporter storing the same message, writes
// into the transaction's serializable MESSAGES_HASH read and makes it retry
// and find the message. (Writers of other messages append to TEXT bunches
// outside each other's read conflicts by design: shard and (writer, seq).)
func testRelMessageSpanBatchConflicts(t *testing.T, h *qrHarness) {
	for _, control := range []bool{false, true} {
		t.Run(map[bool]string{false: "span_batch", true: "same_message_control"}[control], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			warm := deltaConversation(3, "array")
			if _, err := e.export(ctx, cloneDeltaSpans(warm)); err != nil {
				t.Fatal(err)
			}
			other := h.msgEnvOn(t, ctx, e.x.cfg.Run, messagesFullText, nil)
			message := deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra quokka")))
			var once sync.Once
			var inner ExportStats
			var innerErr error
			e.m.hook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage == "message-before-commit" {
					once.Do(func() {
						if control {
							inner, innerErr = other.export(ctx, []*tracepb.ResourceSpans{deltaSpan(8, message)})
							return
						}
						// Present messages and markers: the call writes SPANS rows only,
						// with a changed attribute so their indexes are maintained.
						again := cloneDeltaSpans(warm)
						for _, rs := range again {
							sp := rs.ScopeSpans[0].Spans[0]
							sp.Attributes = append(sp.Attributes, deltaKV("retry.probe", deltaStr("again")))
						}
						inner, innerErr = other.export(ctx, again)
					})
				}
				return nil
			}
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{deltaSpan(9, message)})
			if err != nil || innerErr != nil {
				t.Fatalf("exports: %v / %v", err, innerErr)
			}
			if control {
				if inner.Messages.Written != 1 || stats.Messages.Retries == 0 || stats.Messages.Written != 0 || stats.Messages.Present != 1 {
					t.Fatalf("same-message control: inner %+v, outer %+v", inner.Messages, stats.Messages)
				}
			} else {
				if inner.Spans != int64(len(warm)) || inner.Messages.Written != 0 || inner.Messages.TraceMarkers != 0 {
					t.Fatalf("span-only fixture: %+v", inner)
				}
				if stats.Messages.Retries != 0 || stats.Messages.Written != 1 {
					t.Fatalf("a coalesced span batch conflicted with a message transaction: %+v", stats.Messages)
				}
			}
			if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra quokka"}); n != map[bool]int{false: 1, true: 2}[control] {
				t.Fatalf("hits after the race: %d", n)
			}
		})
	}
}
