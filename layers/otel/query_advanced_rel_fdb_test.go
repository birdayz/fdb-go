package otel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Ports of TestAdvancedQueriesRealFDB (query_advanced_test.go) to the
// relational store: tenant stores created as the Exporter names them, spans
// written as compiled SPANS rows, read through SpanReader.

// qrHarness is one FDB testcontainer with the OTEL_V1 template.
type qrHarness struct {
	db    *recordlayer.FDBDatabase
	setup *sql.DB
	mu    sync.Mutex
	// keys is each store's continuation key, shared by its readers as the
	// STORE_META row will be.
	keys map[string]*memoryContinuationKeys
}

func newQRHarness(t *testing.T) *qrHarness {
	t.Helper()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := fdb.APIVersion(730); err != nil {
		t.Fatal(err)
	}
	sqldriver.RegisterDomainIfNotExists(relDomain)
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", cluster))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setup.Close() })
	if err := otelsql.CreateTemplate(ctx, setup); err != nil {
		t.Fatal(err)
	}
	raw, err := fdb.OpenDatabase(cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	return &qrHarness{db: recordlayer.NewFDBDatabase(raw), setup: setup, keys: map[string]*memoryContinuationKeys{}}
}

// create makes (run, tenant)'s schema store.
func (h *qrHarness) create(t *testing.T, ctx context.Context, run string, tenant int) {
	t.Helper()
	path, schema := relTenantPaths(run, tenant)
	if err := otelsql.CreateTenant(ctx, h.setup, path, schema); err != nil {
		t.Fatal(err)
	}
}

// reader opens a SpanReader on (run, tenant)'s store.
func (h *qrHarness) reader(t *testing.T, ctx context.Context, run string, tenant int, mutate func(*Config)) *SpanReader {
	t.Helper()
	path, schema := relTenantPaths(run, tenant)
	ts, err := otelsql.OpenTenantStoreCompiled(ctx, h.db, path, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Run: run, Tenant: tenant, K: 100, MaxCandidates: 20000, MaxBytes: 4 << 20}
	if mutate != nil {
		mutate(&cfg)
	}
	r := NewSpanReader(h.db, ts, cfg)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.keys[path+"/"+schema] == nil {
		h.keys[path+"/"+schema] = &memoryContinuationKeys{}
	}
	r.keys = h.keys[path+"/"+schema]
	return r
}

// open is reader on a fresh run's tenant 11.
func (h *qrHarness) open(t *testing.T, ctx context.Context, mutate func(*Config)) *SpanReader {
	t.Helper()
	run := freshRun(t)
	h.create(t, ctx, run, 11)
	return h.reader(t, ctx, run, 11, mutate)
}

// qrExport saves every span of rss as its compiled SPANS row in one
// transaction; the otlp column is the span's single-span ResourceSpans.
// tamper, when set, edits a span's row before the save.
func qrExport(t *testing.T, ctx context.Context, r *SpanReader, rss []*tracepb.ResourceSpans, tamper func(*tracepb.Span, protoreflect.Message)) {
	t.Helper()
	var recs []proto.Message
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				wire, err := proto.Marshal(exSingle(rs, ss, sp))
				if err != nil {
					t.Fatal(err)
				}
				rec, err := otelsql.BuildCompiled(otelsql.Span{Resource: rs.Resource, ResourceSchemaURL: rs.SchemaUrl, Scope: ss.Scope, ScopeSchemaURL: ss.SchemaUrl, Span: sp, OTLP: wire})
				if err != nil {
					t.Fatal(err)
				}
				if tamper != nil {
					tamper(sp, rec.ProtoReflect())
				}
				recs = append(recs, rec)
			}
		}
	}
	if _, err := r.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := r.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			if _, err := store.SaveRecord(rec); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// qrTyped is the typed attribute (D1) an equality query names for kv, or
// false when no equality reaches it: bytes, arrays and kvlists, values over
// the indexed cap.
func qrTyped(scope string, kv *commonpb.KeyValue) (Attribute, bool) {
	a := Attribute{Scope: scope, Key: kv.GetKey()}
	switch x := kv.GetValue().GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		a.Type, a.Value = "string", x.StringValue
	case *commonpb.AnyValue_IntValue:
		a.Type, a.Value = "int", strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		a.Type, a.Value = "double", formatDouble(x.DoubleValue)
	case *commonpb.AnyValue_BoolValue:
		a.Type, a.Value = "bool", strconv.FormatBool(x.BoolValue)
	default:
		return a, false
	}
	return a, len(a.Key) <= maxIndexedKeyBytes && len(a.Value) <= maxIndexedValueBytes
}

// qrFixture is qaFixture with typed attribute sets (D1); qaFixture's
// canonical strings matched bools and doubles as strings.
func qrFixture(rss []*tracepb.ResourceSpans) []qaFixtureSpan {
	var out []qaFixtureSpan
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				attrs := map[Attribute]bool{}
				add := func(scope string, kvs []*commonpb.KeyValue) {
					for _, kv := range kvs {
						if a, ok := qrTyped(scope, kv); ok {
							attrs[a] = true
						}
					}
				}
				add("resource", rs.GetResource().GetAttributes())
				add("scope", ss.GetScope().GetAttributes())
				add("span", sp.Attributes)
				out = append(out, qaFixtureSpan{SpanMatch{hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId), int64(sp.StartTimeUnixNano)}, attrs})
			}
		}
	}
	return out
}

// qrSortSpans is index order: start desc, trace desc, span desc. D3: v1's
// time order emitted span IDs ascending at equal (start, trace).
func qrSortSpans(xs []SpanMatch) {
	sort.Slice(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if a.Start != b.Start {
			return a.Start > b.Start
		}
		if a.Trace != b.Trace {
			return a.Trace > b.Trace
		}
		return a.ID > b.ID
	})
}

// qrSameSpanOracle is qaSameSpanOracle over typed sets in index order (D1, D3).
func qrSameSpanOracle(spans []qaFixtureSpan, preds []Attribute, w *TimeRange) []SpanMatch {
	out := []SpanMatch{}
	for _, s := range spans {
		ok := w == nil || (s.match.Start >= w.Start && s.match.Start < w.End)
		for _, p := range preds {
			ok = ok && s.attrs[p]
		}
		if ok {
			out = append(out, s.match)
		}
	}
	qrSortSpans(out)
	return out
}

// qrPaginate is qaPaginate on a SpanReader: it drains a same-span query and
// checks the page contract. Pages report the one order (D2: time only).
func qrPaginate(t *testing.T, ctx context.Context, r *SpanReader, q SameSpanQuery) ([]SpanMatch, int) {
	t.Helper()
	limit := q.Limit
	if limit == 0 {
		limit = r.cfg.K
	}
	all := []SpanMatch{}
	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
		page, err := r.QuerySameSpan(ctx, q)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", pages, q.Predicates, err)
		}
		if page.Order != "time" || page.ReadVersion <= 0 || page.Spans == nil {
			t.Fatalf("page metadata: %+v", page)
		}
		if len(page.Spans) > limit || (len(page.Spans) == limit) != (page.Continuation != nil) {
			t.Fatalf("page %d: %d spans, limit %d, continuation=%v", pages, len(page.Spans), limit, page.Continuation != nil)
		}
		all = append(all, page.Spans...)
		if page.Continuation == nil {
			return all, pages
		}
		q.Continuation = page.Continuation
	}
}

// One FDB testcontainer serves every subtest; each opens a fresh store.
func TestRelAdvancedQueriesRealFDB(t *testing.T) {
	t.Parallel()
	h := newQRHarness(t)
	for name, test := range map[string]func(*testing.T, *qrHarness){
		"same_span":             testRelSameSpanPagination,
		"same_span_typed":       testRelSameSpanTyped,
		"same_span_budget":      testRelSameSpanBudget,
		"continuation_security": testRelContinuationSecurity,
		"trace_existential":     testRelTraceExistential,
		"trace_graph":           testRelTraceGraph,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, h)
		})
	}
}

// Port of testSameSpanPagination. The layout matrix (qaLayouts) is gone: the
// store has one layout (D2). Expectations: typed predicates (D1), index-order
// ties (D3), and the page's own work instead of LastQueryWork (§1.3).
func testRelSameSpanPagination(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	r := h.open(t, ctx, nil)
	rss := qaCorpus(42)
	qrExport(t, ctx, r, rss, nil)
	fixture := qrFixture(rss)

	model := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	timeout := Attribute{"span", "error.type", "string", "timeout"}
	window := &TimeRange{Start: int64(qaBase + 10000), End: int64(qaBase + 30000)}
	queries := []SameSpanQuery{
		{Predicates: []Attribute{model, timeout}, Limit: 2},
		{Predicates: []Attribute{model, {"resource", "service.name", "string", "svc-a"}, {"span", "feature.flag", "int", "2"}}, Limit: 1},
		// D1: the bool is a bool, not v1's canonical string "true".
		{Predicates: []Attribute{{"span", "cached", "bool", "true"}, {"span", "gen_ai.request.model", "string", "claude-3"}}, TimeWindow: window, Limit: 3},
		{Predicates: []Attribute{{"span", "gen_ai.request.model", "string", collision("gpt-4o")}}, Limit: 4},
		{Predicates: []Attribute{{"span", collision("gen_ai.request.model"), "string", "gpt-4o"}, model}, Limit: 5},
		{Predicates: []Attribute{model, timeout}},
		{Predicates: []Attribute{model, {"span", "error.type", "string", "absent"}}, Limit: 2},
		{Predicates: []Attribute{model}, TimeWindow: &TimeRange{Start: 7, End: 7}},
	}
	nonempty, ties := 0, 0
	for _, q := range queries {
		want := qrSameSpanOracle(fixture, q.Predicates, q.TimeWindow)
		got, pages := qrPaginate(t, ctx, r, q)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("same-span %+v window %+v\n got %+v\nwant %+v", q.Predicates, q.TimeWindow, got, want)
		}
		if q.Limit > 0 && len(want) > q.Limit && pages < 2 {
			t.Fatalf("expected multiple pages for %+v", q.Predicates)
		}
		if len(want) > 0 {
			nonempty++
		}
		for i := 1; i < len(want); i++ {
			if want[i].Start == want[i-1].Start && want[i].Trace == want[i-1].Trace {
				ties++
			}
		}
	}
	if nonempty < 5 || ties == 0 {
		t.Fatalf("vacuous corpus: %d of %d queries matched, %d (start, trace) ties", nonempty, len(queries), ties)
	}

	// The continuation binds the predicate set, so a resumed page may reorder the driver.
	first, err := r.QuerySameSpan(ctx, queries[0])
	if err != nil || first.Continuation == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	swapped := SameSpanQuery{Predicates: []Attribute{timeout, model}, Limit: 2, Continuation: first.Continuation}
	second, err := r.QuerySameSpan(ctx, swapped)
	if err != nil {
		t.Fatal(err)
	}
	want := qrSameSpanOracle(fixture, queries[0].Predicates, nil)
	if !reflect.DeepEqual(append(first.Spans, second.Spans...), want[:min(len(want), 4)]) {
		t.Fatalf("driver-swapped resume:\n got %+v %+v\nwant %+v", first.Spans, second.Spans, want)
	}
	// §1.3: work is per call; the page reports it (was LastQueryWork).
	if second.Candidates == 0 || second.Bytes == 0 {
		t.Fatalf("query work accounting: page %+v", second)
	}

	// Live pagination: a new matching span sorting after the cursor is visible on the next page.
	tail := want[1]
	late := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: qaBytes(t, qaID(900, 16)), SpanId: qaBytes(t, qaID(900, 8)), StartTimeUnixNano: uint64(tail.Start - 1), Attributes: []*commonpb.KeyValue{exKV("gen_ai.request.model", exStr("gpt-4o")), exKV("error.type", exStr("timeout"))}}}}}}
	qrExport(t, ctx, r, []*tracepb.ResourceSpans{late}, nil)
	next, err := r.QuerySameSpan(ctx, SameSpanQuery{Predicates: queries[0].Predicates, Limit: 100, Continuation: first.Continuation})
	if err != nil {
		t.Fatal(err)
	}
	fixture = append(fixture, qrFixture([]*tracepb.ResourceSpans{late})...)
	all := qrSameSpanOracle(fixture, queries[0].Predicates, nil)
	if !reflect.DeepEqual(next.Spans, all[2:]) {
		t.Fatalf("resumed live page:\n got %+v\nwant %+v", next.Spans, all[2:])
	}
}

// D1: double, bool and int predicates are typed, strings over the indexed
// bound are matched by digest and verified on the record, as driver and as
// probe. One span's row is tampered so its long string's digest names a value
// the row does not hold: verification, not the digest, decides.
func testRelSameSpanTyped(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	r := h.open(t, ctx, nil)
	l1 := strings.Repeat("a", maxIndexedKeyBytes) + "-one"
	l2 := strings.Repeat("a", maxIndexedKeyBytes) + "-two"
	T := func(n int) string { return qaID(n, 16) }
	S := func(n int) string { return qaID(n, 8) }
	kvs := func(long string, ratio float64, ok bool) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{exKV("prompt", exStr(long)), exKV("ratio", exDouble(ratio)), exKV("ok", exBool(ok)), exKV("n", exInt(7))}
	}
	rss := qaSpans(t,
		qaSpec{T(1), S(1), 100, kvs(l1, 0.5, true)},
		qaSpec{T(1), S(2), 200, kvs(l1, 0.25, false)},
		qaSpec{T(2), S(3), 300, kvs(l1, 0.5, true)},
		qaSpec{T(3), S(4), 400, kvs(l1, 0.5, true)},
		qaSpec{T(4), S(5), 500, kvs(l2, 0.5, false)},
		qaSpec{T(5), S(6), 600, []*commonpb.KeyValue{exKV("tie", exStr("x"))}},
		qaSpec{T(5), S(7), 600, []*commonpb.KeyValue{exKV("tie", exStr("x"))}},
		qaSpec{T(6), S(8), 600, []*commonpb.KeyValue{exKV("tie", exStr("x"))}},
	)
	tamperedID := qaBytes(t, S(4))
	qrExport(t, ctx, r, rss, func(sp *tracepb.Span, m protoreflect.Message) {
		if !bytes.Equal(sp.SpanId, tamperedID) {
			return
		}
		arr := m.Mutable(m.Descriptor().Fields().ByName("LONG_STR_ATTRS")).Message()
		el := arr.Mutable(arr.Descriptor().Fields().ByName("values")).List().Get(0).Message()
		el.Set(el.Descriptor().Fields().ByName("STR_VALUE"), protoreflect.ValueOfString(l2))
	})
	m := func(tr, sp string, start int64) SpanMatch { return SpanMatch{tr, sp, start} }
	s1, s2, s3, s5 := m(T(1), S(1), 100), m(T(1), S(2), 200), m(T(2), S(3), 300), m(T(4), S(5), 500)
	s4 := m(T(3), S(4), 400)
	long := func(v string) Attribute { return Attribute{"span", "prompt", "string", v} }
	half := Attribute{"span", "ratio", "double", "0.5"}
	for name, c := range map[string]struct {
		preds []Attribute
		want  []SpanMatch
	}{
		"long driver":        {[]Attribute{long(l1)}, []SpanMatch{s3, s2, s1}},
		"long probe":         {[]Attribute{half, long(l1)}, []SpanMatch{s3, s1}},
		"digest not record":  {[]Attribute{long(l2)}, []SpanMatch{s5}},
		"bool driver":        {[]Attribute{{"span", "ok", "bool", "true"}, half}, []SpanMatch{s4, s3, s1}},
		"double other value": {[]Attribute{{"span", "ratio", "double", "0.25"}}, []SpanMatch{s2}},
		"int and long":       {[]Attribute{{"span", "n", "int", "7"}, long(l1)}, []SpanMatch{s3, s2, s1}},
		// D1: v1 matched bools and doubles by their canonical strings.
		"bool as string":   {[]Attribute{{"span", "ok", "string", "true"}}, []SpanMatch{}},
		"double as string": {[]Attribute{{"span", "ratio", "string", "0.5"}}, []SpanMatch{}},
		"int as double":    {[]Attribute{{"span", "n", "double", "7"}}, []SpanMatch{}},
		// D3: at equal (start, trace) span IDs descend, index order; v1 emitted
		// S(6) before S(7). Limit 1 resumes between the tied spans.
		"tie order": {[]Attribute{{"span", "tie", "string", "x"}}, []SpanMatch{m(T(6), S(8), 600), m(T(5), S(7), 600), m(T(5), S(6), 600)}},
	} {
		for _, limit := range []int{0, 1} {
			got, _ := qrPaginate(t, ctx, r, SameSpanQuery{Predicates: c.preds, Limit: limit})
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s (limit %d)\n got %+v\nwant %+v", name, limit, got, c.want)
			}
		}
	}
	got, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{long(l1), {"span", "ratio", "double", "0.25"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []TraceExistentialHit{{T(1), 200, [][]SpanMatch{{s2, s1}, {s2}}, nil, nil}}; !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("typed existential\n got %+v\nwant %+v", got.Hits, want)
	}
	got, err = r.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{long(l1)}})
	if err != nil || len(got.Hits) != 2 || got.Hits[0].Trace != T(2) || got.Hits[1].Trace != T(1) {
		t.Fatalf("long-string existential keeps the tampered span out: %+v %v", got.Hits, err)
	}

	// A probed index that is not readable is ownership, as a driver's is.
	if _, err := r.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := r.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		return store.MarkIndexDisabled(spansBoolAttrIndex)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = r.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{half, {"span", "ok", "bool", "true"}}})
	requireTyped[*OwnershipError](t, "disabled probe index", err)
	_, err = r.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "ok", "bool", "true"}}})
	requireTyped[*OwnershipError](t, "disabled driver index", err)
	if p, err := r.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{half}}); err != nil || len(p.Spans) != 4 {
		t.Fatalf("a readable index beside the disabled one: %+v %v", p.Spans, err)
	}
}

// Port of testSameSpanBudget, unchanged.
func testRelSameSpanBudget(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	q := SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.request.model", "string", "gpt-4o"}, {"span", "error.type", "string", "timeout"}}}
	for name, mutate := range map[string]func(*Config){
		"candidates": func(c *Config) { c.MaxCandidates = 3 },
		"bytes":      func(c *Config) { c.MaxBytes = 128 },
	} {
		r := h.open(t, ctx, mutate)
		qrExport(t, ctx, r, qaCorpus(7), nil)
		page, err := r.QuerySameSpan(ctx, q)
		requireOtelBudget(t, err)
		if page.Candidates == 0 || page.Continuation != nil {
			t.Fatalf("%s: budget failure must report work and carry no continuation: %+v", name, page)
		}
		if _, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: q.Predicates}); err == nil {
			t.Fatalf("%s: existential within budget unexpectedly", name)
		} else {
			requireOtelBudget(t, err)
		}
		// A term with no entries fits any budget.
		empty, err := r.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "nope", "string", "x"}}})
		if err != nil || len(empty.Spans) != 0 || empty.Candidates != 0 {
			t.Fatalf("%s: empty term: %+v %v", name, empty, err)
		}
	}
}

// Port of testContinuationSecurity. The v2 signing key is per tenant store
// (§3.2: the STORE_META row), where v1's was per run: a token of another
// tenant's store therefore fails its MAC (corruption) where v1 reported the
// binding (config). TestRelContinuationRejections pins the store binding
// under a shared key.
func testRelContinuationSecurity(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	r := h.open(t, ctx, nil)
	run := r.cfg.Run
	rss := qaCorpus(9)
	qrExport(t, ctx, r, rss, nil)
	model := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	q := SameSpanQuery{Predicates: []Attribute{model}, Limit: 2}
	first, err := r.QuerySameSpan(ctx, q)
	if err != nil || first.Continuation == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	want := qrSameSpanOracle(qrFixture(rss), q.Predicates, nil)
	resume := func(r *SpanReader, q SameSpanQuery, token []byte) (SpanQueryPage, error) {
		q.Continuation = token
		return r.QuerySameSpan(ctx, q)
	}

	// A second reader of the same store shares its signing key.
	same := h.reader(t, ctx, run, 11, nil)
	page, err := resume(same, q, first.Continuation)
	if err != nil || !reflect.DeepEqual(page.Spans, want[2:4]) {
		t.Fatalf("resume on second reader: %+v %v", page.Spans, err)
	}

	tampered := append([]byte(nil), first.Continuation...)
	tampered[len(tampered)/2] ^= 0x01
	_, err = resume(r, q, tampered)
	requireConfigOrCorruption(t, "tampered payload", err)
	macFlip := append([]byte(nil), first.Continuation...)
	macFlip[len(macFlip)-1] ^= 0x01
	_, err = resume(r, q, macFlip)
	requireTyped[*CorruptionError](t, "tampered MAC", err)

	_, err = resume(r, SameSpanQuery{Predicates: []Attribute{model, {"span", "error.type", "string", "timeout"}}, Limit: 2}, first.Continuation)
	requireTyped[*ConfigError](t, "wrong query", err)
	_, err = resume(r, SameSpanQuery{Predicates: []Attribute{model}, Limit: 2, TimeWindow: &TimeRange{Start: 0, End: math.MaxInt64}}, first.Continuation)
	requireTyped[*ConfigError](t, "wrong window", err)

	// Other stores never share the signing key: before and after they publish their own.
	h.create(t, ctx, run, 12)
	otherTenant := h.reader(t, ctx, run, 12, nil)
	otherRun := h.open(t, ctx, nil)
	for name, other := range map[string]*SpanReader{"other tenant": otherTenant, "other run": otherRun} {
		_, err = resume(other, q, first.Continuation)
		requireTyped[*CorruptionError](t, name+" without key", err)
		qrExport(t, ctx, other, rss, nil)
		if p, err := other.QuerySameSpan(ctx, q); err != nil || p.Continuation == nil {
			t.Fatalf("%s first page: %+v %v", name, p, err)
		}
		_, err = resume(other, q, first.Continuation)
		requireTyped[*CorruptionError](t, name+" with key", err)
	}

	digest := queryDigest(continuationKind, q.Predicates, timeWindow{})
	for name, token := range map[string][]byte{
		"garbage":  bytes.Repeat([]byte{0xab}, 80),
		"short":    {1, 2, 3},
		"v1 token": signContinuation(bytes.Repeat([]byte{7}, continuationKeyBytes), run, 11, "time", digest, tupleKey(want[1])),
	} {
		_, err = resume(r, q, token)
		requireTyped[*ConfigError](t, name, err)
	}

	// Rejections leave the original continuation valid.
	page, err = resume(r, q, first.Continuation)
	if err != nil || !reflect.DeepEqual(page.Spans, want[2:4]) {
		t.Fatalf("original continuation after rejections: %+v %v", page.Spans, err)
	}

	// Until STORE_META lands, a reader that cannot publish a key fails a full
	// page rather than returning it without a continuation.
	stub := h.reader(t, ctx, run, 11, nil)
	stub.keys = storeMetaContinuationKeys{}
	_, err = stub.QuerySameSpan(ctx, q)
	requireTyped[*OperationError](t, "STORE_META stub", err)
	if p, err := stub.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{model}}); err != nil || len(p.Spans) != len(want) || p.Continuation != nil {
		t.Fatalf("a partial page needs no key: %d spans, %v", len(p.Spans), err)
	}
}

func tupleKey(s SpanMatch) []byte {
	return postingSuffix(Span{Trace: s.Trace, ID: s.ID, Start: s.Start}, "time").Pack()
}

// Port of testTraceExistential, unchanged: span clauses, and log clauses over
// logs exported through relLogStore into the same tenant store.
func testRelTraceExistential(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	r := h.open(t, ctx, nil)
	modelKV := exKV("gen_ai.request.model", exStr("gpt-4o"))
	failedKV := exKV("gen_ai.tool.status", exStr("failed"))
	decoy := exKV("gen_ai.request.model", exStr(collision("gpt-4o")))
	T1, T2, T3, T4, T5 := qaID(1, 16), qaID(2, 16), qaID(3, 16), qaID(4, 16), qaID(5, 16)
	s := func(n int) string { return qaID(n, 8) }
	qrExport(t, ctx, r, qaSpans(
		t,
		qaSpec{T1, s(11), 100, []*commonpb.KeyValue{modelKV}},
		qaSpec{T1, s(12), 200, []*commonpb.KeyValue{failedKV}},
		qaSpec{T1, s(13), 50, []*commonpb.KeyValue{modelKV}},
		qaSpec{T2, s(21), 300, []*commonpb.KeyValue{modelKV, failedKV}},
		qaSpec{T3, s(31), 400, []*commonpb.KeyValue{modelKV}},
		qaSpec{T3, s(32), 410, []*commonpb.KeyValue{decoy}},
		qaSpec{T4, s(41), 500, []*commonpb.KeyValue{failedKV, decoy}},
		qaSpec{T5, s(51), 600, []*commonpb.KeyValue{modelKV}},
		qaSpec{T5, s(52), 50, []*commonpb.KeyValue{failedKV}},
	), nil)
	model := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	failed := Attribute{"span", "gen_ai.tool.status", "string", "failed"}
	m := func(tr, sp string, start int64) SpanMatch { return SpanMatch{tr, sp, start} }
	hitT1 := TraceExistentialHit{T1, 200, [][]SpanMatch{{m(T1, s(11), 100), m(T1, s(13), 50)}, {m(T1, s(12), 200)}}, nil, nil}
	hitT2 := TraceExistentialHit{T2, 300, [][]SpanMatch{{m(T2, s(21), 300)}, {m(T2, s(21), 300)}}, nil, nil}
	hitT5 := TraceExistentialHit{T5, 600, [][]SpanMatch{{m(T5, s(51), 600)}, {m(T5, s(52), 50)}}, nil, nil}
	for name, c := range map[string]struct {
		q    TraceExistentialQuery
		want []TraceExistentialHit
	}{
		"both":       {TraceExistentialQuery{Clauses: []Attribute{model, failed}}, []TraceExistentialHit{hitT5, hitT2, hitT1}},
		"limit":      {TraceExistentialQuery{Clauses: []Attribute{model, failed}, Limit: 2}, []TraceExistentialHit{hitT5, hitT2}},
		"window":     {TraceExistentialQuery{Clauses: []Attribute{model, failed}, TimeWindow: &TimeRange{Start: 0, End: 250}}, []TraceExistentialHit{hitT1}},
		"empty join": {TraceExistentialQuery{Clauses: []Attribute{model, {"span", "gen_ai.tool.status", "string", "ok"}}}, []TraceExistentialHit{}},
		"single": {TraceExistentialQuery{Clauses: []Attribute{failed}}, []TraceExistentialHit{
			{T4, 500, [][]SpanMatch{{m(T4, s(41), 500)}}, nil, nil}, {T2, 300, [][]SpanMatch{{m(T2, s(21), 300)}}, nil, nil}, {T1, 200, [][]SpanMatch{{m(T1, s(12), 200)}}, nil, nil}, {T5, 50, [][]SpanMatch{{m(T5, s(52), 50)}}, nil, nil},
		}},
	} {
		got, err := r.QueryTraceExistential(ctx, c.q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got.Hits, c.want) {
			t.Fatalf("%s\n got %+v\nwant %+v", name, got.Hits, c.want)
		}
		if got.ReadVersion <= 0 || (len(c.want) > 0 && got.Candidates == 0) {
			t.Fatalf("%s: metadata %+v", name, got)
		}
	}
	if got, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{model}, TimeWindow: &TimeRange{Start: 5, End: 5}}); err != nil || len(got.Hits) != 0 || got.Candidates != 0 || got.ReadVersion <= 0 {
		t.Fatalf("empty window: %+v %v", got, err)
	}

	// Correlated logs join through their trace; uncorrelated ones never fabricate a trace.
	evalKV := exKV("gen_ai.evaluation.name", exStr("relevance"))
	if _, err := qrLogStore(t, r).ExportLogs(lrCtx(ctx), []LogRecord{
		{Timestamp: 150, TraceID: T1, SpanID: s(12), EventName: "gen_ai.evaluation.result", Attributes: []*commonpb.KeyValue{evalKV}},
		{EventID: "t3-eval", Timestamp: 450, TraceID: T3, Attributes: []*commonpb.KeyValue{evalKV}},
		{EventID: "orphan-eval", Timestamp: 999, Attributes: []*commonpb.KeyValue{evalKV}},
		{EventID: "t4-other", Timestamp: 700, TraceID: T4, Attributes: []*commonpb.KeyValue{exKV("gen_ai.evaluation.name", exStr("toxicity"))}},
	}); err != nil {
		t.Fatal(err)
	}
	eval := Attribute{LogScopeRecord, "gen_ai.evaluation.name", "string", "relevance"}
	got, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{model}, LogClauses: []Attribute{eval}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 2 || got.Hits[0].Trace != T3 || got.Hits[1].Trace != T1 || got.Hits[0].Latest != 450 || got.Hits[1].Latest != 150 {
		t.Fatalf("span+log existential: %+v", got.Hits)
	}
	if lw := got.Hits[0].LogWitnesses; len(lw) != 1 || len(lw[0]) != 1 || lw[0][0] != (LogMatch{"t3-eval", T3, "", 450}) {
		t.Fatalf("T3 log witnesses: %+v", lw)
	}
	if lw := got.Hits[1].LogWitnesses; len(lw) != 1 || len(lw[0]) != 1 || lw[0][0].Trace != T1 || lw[0][0].Span != s(12) || lw[0][0].Time != 150 {
		t.Fatalf("T1 log witnesses: %+v", lw)
	}
	logOnly, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{{LogScopeIntrinsic, "event_name", "string", "gen_ai.evaluation.result"}}})
	if err != nil || len(logOnly.Hits) != 1 || logOnly.Hits[0].Trace != T1 || len(logOnly.Hits[0].Witnesses) != 0 {
		t.Fatalf("log-only existential: %+v %v", logOnly, err)
	}

	for name, q := range map[string]TraceExistentialQuery{
		"no clauses":      {},
		"bad scope":       {Clauses: []Attribute{{LogScopeRecord, "k", "string", "v"}}},
		"bad log scope":   {LogClauses: []Attribute{{"span", "k", "string", "v"}}},
		"negative limit":  {Clauses: []Attribute{model}, Limit: -1},
		"inverted window": {Clauses: []Attribute{model}, TimeWindow: &TimeRange{Start: 2, End: 1}},
		"bytes clause":    {Clauses: []Attribute{{"span", "k", "bytes", "AQI="}}},
		// Not yet supported on the relational store (stream 2C).
		"message clause": {MessageClauses: []MessageFullTextQuery{{Query: "hello"}}},
	} {
		_, err := r.QueryTraceExistential(ctx, q)
		requireTyped[*ConfigError](t, name, err)
	}
	fullText := h.reader(t, ctx, r.cfg.Run, 11, func(c *Config) { c.Messages = messagesFullText })
	_, err = fullText.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{model}, MessageClauses: []MessageFullTextQuery{{Query: "hello"}}})
	requireTyped[*ConfigError](t, "message clause on a fulltext store", err)
	_, err = fullText.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "hello", Limit: 1}}})
	requireTyped[*ConfigError](t, "message clause with a limit", err)
}

// Port of testTraceGraphRealFDB: the work is the call's result (was
// LastQueryWork, §1.3).
func testRelTraceGraph(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	r := h.open(t, ctx, nil)
	tr, other := qaID(70, 16), qaID(71, 16)
	R, A, B, O, M, X, Y := qaID(1, 8), qaID(2, 8), qaID(3, 8), qaID(4, 8), qaID(5, 8), qaID(6, 8), qaID(7, 8)
	span := func(id, parent string, start uint64, links ...*tracepb.Span_Link) *tracepb.ResourceSpans {
		sp := &tracepb.Span{TraceId: qaBytes(t, tr), SpanId: qaBytes(t, id), Name: "op", StartTimeUnixNano: start, Links: links}
		if parent != "" {
			sp.ParentSpanId = qaBytes(t, parent)
		}
		return &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}
	}
	// Children arrive before their parents, in separate writes.
	qrExport(t, ctx, r, []*tracepb.ResourceSpans{span(A, R, 30), span(O, M, 5), span(X, Y, 60)}, nil)
	qrExport(t, ctx, r, []*tracepb.ResourceSpans{span(Y, X, 70), span(B, R, 20), span(R, "", 10,
		&tracepb.Span_Link{TraceId: qaBytes(t, tr), SpanId: qaBytes(t, A), Attributes: []*commonpb.KeyValue{exKV("k", exStr("v"))}},
		&tracepb.Span_Link{TraceId: qaBytes(t, other), SpanId: qaBytes(t, A)})}, nil)
	qrExport(t, ctx, r, []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: qaBytes(t, other), SpanId: qaBytes(t, A), StartTimeUnixNano: 1}}}}}}, nil)

	g, work, err := r.TraverseTraceGraph(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	links := g.Links
	g.Links = nil
	want := TraceGraph{
		Trace:          tr,
		Spans:          []SpanMatch{{tr, O, 5}, {tr, R, 10}, {tr, B, 20}, {tr, A, 30}, {tr, X, 60}, {tr, Y, 70}},
		Roots:          []string{R},
		Children:       map[string][]string{R: {B, A}, X: {Y}, Y: {X}},
		Parent:         map[string]string{A: R, B: R, O: M, X: Y, Y: X},
		MissingParents: []string{M},
		Orphans:        []string{O},
		Cycles:         []string{X, Y},
		Depth:          map[string]int{R: 0, B: 1, A: 1},
		MaxDepth:       1,
	}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("graph\n got %+v\nwant %+v", g, want)
	}
	if len(links) != 1 || len(links[R]) != 2 || !links[R][0].Resolved || links[R][1].Resolved || links[R][1].TraceID != other || !proto.Equal(links[R][0].Attributes[0], exKV("k", exStr("v"))) {
		t.Fatalf("links: %+v", links)
	}
	if work.Candidates != 6 || work.Bytes == 0 {
		t.Fatalf("graph work: %+v", work)
	}

	empty, work, err := r.TraverseTraceGraph(ctx, qaID(99, 16))
	if err != nil || len(empty.Spans) != 0 || empty.Roots == nil || work.Candidates != 0 {
		t.Fatalf("absent trace: %+v %+v %v", empty, work, err)
	}
	for _, id := range []string{"ABC", strings.ToUpper(qaID(0xab, 16)), strings.Repeat("0", 32)} {
		_, _, err = r.TraverseTraceGraph(ctx, id)
		requireTyped[*ConfigError](t, "invalid trace "+id, err)
	}

	small := h.open(t, ctx, func(c *Config) { c.MaxCandidates = 3 })
	qrExport(t, ctx, small, []*tracepb.ResourceSpans{span(A, R, 30), span(B, R, 20), span(R, "", 10), span(O, M, 5)}, nil)
	_, _, err = small.TraverseTraceGraph(ctx, tr)
	requireOtelBudget(t, err)
	if _, work, err := small.TraverseTraceGraph(ctx, other); err != nil || work.Candidates != 0 {
		t.Fatalf("a trace absent from the small store: %+v %v", work, err)
	}
}
