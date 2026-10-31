package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// qaID is a deterministic nonzero lowercase hex identity of size bytes.
func qaID(n, size int) string { return fmt.Sprintf("%0*x", size*2, n+1) }

func qaBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireConfigOrCorruption(t *testing.T, label string, err error) {
	t.Helper()
	var cfg *ConfigError
	var corrupt *CorruptionError
	if !errors.As(err, &cfg) && !errors.As(err, &corrupt) {
		t.Fatalf("%s: want ConfigError or CorruptionError, got %T: %v", label, err, err)
	}
}

func requireTyped[T error](t *testing.T, label string, err error) {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("%s: want %T, got %T: %v", label, target, err, err)
	}
}

func qaBackend(order string) *FDBBackend {
	return &FDBBackend{cfg: Config{Run: "qa_run_0001", Tenant: 3, Order: order, K: 100}}
}

// qaCanonical is the indexed attribute set of one stored single-span ResourceSpans.
func qaCanonical(rs *tracepb.ResourceSpans) map[Attribute]bool {
	out := map[Attribute]bool{}
	add := func(scope string, kvs []*commonpb.KeyValue) {
		for _, kv := range canonicalAttributes(kvs).indexed {
			out[CanonicalAttribute(scope, kv.Key, kv.Value)] = true
		}
	}
	add("resource", rs.GetResource().GetAttributes())
	add("scope", rs.ScopeSpans[0].GetScope().GetAttributes())
	add("span", rs.ScopeSpans[0].Spans[0].Attributes)
	return out
}

type qaFixtureSpan struct {
	match SpanMatch
	attrs map[Attribute]bool
}

func qaFixture(rss []*tracepb.ResourceSpans) []qaFixtureSpan {
	var out []qaFixtureSpan
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				single := &tracepb.ResourceSpans{Resource: rs.Resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, Spans: []*tracepb.Span{sp}}}}
				out = append(out, qaFixtureSpan{SpanMatch{hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId), int64(sp.StartTimeUnixNano)}, qaCanonical(single)})
			}
		}
	}
	return out
}

func qaSortSpans(order string, xs []SpanMatch) {
	sort.Slice(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if order == "time" {
			if a.Start != b.Start {
				return a.Start > b.Start
			}
			if a.Trace != b.Trace {
				return a.Trace > b.Trace // reverseTrace ascending
			}
			return a.ID < b.ID
		}
		if a.Trace != b.Trace {
			return a.Trace < b.Trace
		}
		return a.ID < b.ID
	})
}

func qaSameSpanOracle(spans []qaFixtureSpan, preds []Attribute, w *TimeRange, order string) []SpanMatch {
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
	qaSortSpans(order, out)
	return out
}

// ---------------------------------------------------------------------------
// Continuation tokens
// ---------------------------------------------------------------------------

func TestContinuationTokenRoundTrip(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{7}, continuationKeyBytes)
	preds := []Attribute{{"span", "gen_ai.request.model", "string", "gpt-4o"}}
	for _, order := range []string{"time", "trace"} {
		b := qaBackend(order)
		digest := queryDigest(continuationKind, preds, timeWindow{})
		last := postingSuffix(Span{Trace: qaID(1, 16), ID: qaID(2, 8), Start: 42}, order)
		token := signContinuation(secret, b.cfg.Run, b.cfg.Tenant, order, digest, last.Pack())
		c, err := parseContinuation(token)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.verify(secret, b, digest)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, last) {
			t.Fatalf("%s resume key: got %#v want %#v", order, got, last)
		}
	}
}

func TestContinuationTokenRejections(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{7}, continuationKeyBytes)
	b := qaBackend("time")
	preds := []Attribute{{"span", "k", "string", "v"}}
	digest := queryDigest(continuationKind, preds, timeWindow{})
	last := postingSuffix(Span{Trace: qaID(1, 16), ID: qaID(2, 8), Start: 42}, "time").Pack()
	token := signContinuation(secret, b.cfg.Run, b.cfg.Tenant, "time", digest, last)
	check := func(label string, tok, key, dig []byte) error {
		c, err := parseContinuation(tok)
		if err != nil {
			return err
		}
		_, err = c.verify(key, b, dig)
		return err
	}

	// Every single-bit flip anywhere in the token must be rejected.
	for i := range token {
		for bit := range 8 {
			mutated := append([]byte(nil), token...)
			mutated[i] ^= 1 << bit
			err := check("flip", mutated, secret, digest)
			if err == nil {
				t.Fatalf("bit flip at byte %d bit %d accepted", i, bit)
			}
			requireConfigOrCorruption(t, fmt.Sprintf("flip %d/%d", i, bit), err)
		}
	}
	macFlip := append([]byte(nil), token...)
	macFlip[len(macFlip)-1] ^= 0x80
	requireTyped[*CorruptionError](t, "MAC flip", check("mac", macFlip, secret, digest))
	requireTyped[*CorruptionError](t, "wrong secret", check("secret", token, bytes.Repeat([]byte{8}, continuationKeyBytes), digest))
	requireTyped[*CorruptionError](t, "absent secret", check("absent", token, nil, digest))

	other := queryDigest(continuationKind, []Attribute{{"span", "k", "string", "w"}}, timeWindow{})
	requireTyped[*ConfigError](t, "wrong query", check("query", token, secret, other))
	requireTyped[*ConfigError](t, "wrong tenant", check("tenant", signContinuation(secret, b.cfg.Run, 4, "time", digest, last), secret, digest))
	requireTyped[*ConfigError](t, "wrong run", check("run", signContinuation(secret, "qa_run_other", b.cfg.Tenant, "time", digest, last), secret, digest))
	requireTyped[*ConfigError](t, "wrong order", check("order", signContinuation(secret, b.cfg.Run, b.cfg.Tenant, "trace", digest, last), secret, digest))

	// Authenticated but semantically malformed sort keys are corruption.
	for name, key := range map[string][]byte{
		"not a tuple":      {0xff, 0x01},
		"trace-order key":  postingSuffix(Span{Trace: qaID(1, 16), ID: qaID(2, 8)}, "trace").Pack(),
		"positive start":   tuple.Tuple{int64(5), reverseTrace(qaID(1, 16)), qaID(2, 8)}.Pack(),
		"invalid span":     tuple.Tuple{int64(-5), reverseTrace(qaID(1, 16)), "zz"}.Pack(),
		"zero trace":       tuple.Tuple{int64(-5), reverseTrace(qaID(-1, 16)), qaID(2, 8)}.Pack(),
		"extra key fields": tuple.Tuple{int64(-5), reverseTrace(qaID(1, 16)), qaID(2, 8), int64(0)}.Pack(),
	} {
		requireTyped[*CorruptionError](t, name, check(name, signContinuation(secret, b.cfg.Run, b.cfg.Tenant, "time", digest, key), secret, digest))
	}

	// Structurally malformed tokens are configuration errors before any read.
	payload := tuple.Tuple{int64(2), continuationKind, b.cfg.Run, int64(b.cfg.Tenant), "time", digest, last}.Pack()
	badVersion := append(payload, continuationMAC(secret, payload)...)
	payload = tuple.Tuple{continuationVersion, "other", b.cfg.Run, int64(b.cfg.Tenant), "time", digest, last}.Pack()
	badKind := append(payload, continuationMAC(secret, payload)...)
	payload = tuple.Tuple{continuationVersion, continuationKind, b.cfg.Run}.Pack()
	short := append(payload, continuationMAC(secret, payload)...)
	for name, tok := range map[string][]byte{
		"empty MAC only": token[len(token)-32:],
		"one byte":       {1},
		"oversized":      bytes.Repeat([]byte{1}, maxContinuationBytes+1),
		"garbage":        bytes.Repeat([]byte{0xfe}, 64),
		"bad version":    badVersion,
		"bad kind":       badKind,
		"few fields":     short,
	} {
		_, err := parseContinuation(tok)
		requireTyped[*ConfigError](t, name, err)
	}
}

func TestQueryDigestBindsPredicateSetAndWindow(t *testing.T) {
	t.Parallel()
	a := Attribute{"span", "a", "string", "1"}
	c := Attribute{"resource", "c", "int", "3"}
	base := queryDigest(continuationKind, []Attribute{a, c}, timeWindow{})
	if !bytes.Equal(base, queryDigest(continuationKind, []Attribute{c, a}, timeWindow{})) {
		t.Fatal("digest must not depend on predicate order")
	}
	w, err := newTimeWindow(&TimeRange{Start: 1, End: 2})
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string][]byte{
		"value":  queryDigest(continuationKind, []Attribute{{"span", "a", "string", "2"}, c}, timeWindow{}),
		"type":   queryDigest(continuationKind, []Attribute{{"span", "a", "int", "1"}, c}, timeWindow{}),
		"scope":  queryDigest(continuationKind, []Attribute{{"resource", "a", "string", "1"}, c}, timeWindow{}),
		"subset": queryDigest(continuationKind, []Attribute{a}, timeWindow{}),
		"window": queryDigest(continuationKind, []Attribute{a, c}, w),
		"kind":   queryDigest("other", []Attribute{a, c}, timeWindow{}),
		// Tuple encoding is length-delimited: shifting bytes between fields changes the digest.
		"boundary": queryDigest(continuationKind, []Attribute{{"span", "a1", "string", ""}, c}, timeWindow{}),
	} {
		if bytes.Equal(base, d) {
			t.Fatalf("digest ignores %s", name)
		}
	}
	plan, err := newSameSpanPlan(SameSpanQuery{Predicates: []Attribute{a, c, a}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.preds, []Attribute{a, c}) || plan.limit != 7 || !bytes.Equal(plan.digest, base) {
		t.Fatalf("plan dedupe/default limit: %+v", plan)
	}
}

func TestSameSpanPlanValidation(t *testing.T) {
	t.Parallel()
	ok := Attribute{"span", "k", "string", "v"}
	many := make([]Attribute, maxQueryPredicates+1)
	for i := range many {
		many[i] = Attribute{"span", fmt.Sprint(i), "string", "v"}
	}
	for name, q := range map[string]SameSpanQuery{
		"no predicates":     {},
		"too many":          {Predicates: many},
		"unknown scope":     {Predicates: []Attribute{{"event", "k", "string", "v"}}},
		"unknown type":      {Predicates: []Attribute{{"span", "k", "bool", "true"}}},
		"noncanonical int":  {Predicates: []Attribute{{"span", "k", "int", "007"}}},
		"int overflow":      {Predicates: []Attribute{{"span", "k", "int", "9223372036854775808"}}},
		"oversized key":     {Predicates: []Attribute{{"span", string(make([]byte, maxIndexedKeyBytes+1)), "string", "v"}}},
		"oversized value":   {Predicates: []Attribute{{"span", "k", "string", string(make([]byte, maxIndexedValueBytes+1))}}},
		"negative limit":    {Predicates: []Attribute{ok}, Limit: -1},
		"inverted interval": {Predicates: []Attribute{ok}, TimeWindow: &TimeRange{Start: 5, End: 4}},
	} {
		_, err := newSameSpanPlan(q, 100)
		requireTyped[*ConfigError](t, name, err)
	}
	if _, err := newSameSpanPlan(SameSpanQuery{Predicates: []Attribute{{"resource", "k", "int", "-12"}}}, 100); err != nil {
		t.Fatalf("canonical negative int rejected: %v", err)
	}
}

func TestTimeWindowDescendingRange(t *testing.T) {
	t.Parallel()
	term := subspace.Sub("qa_term")
	key := func(start int64) []byte {
		return term.Pack(tuple.Tuple{-start, reverseTrace(qaID(1, 16)), qaID(1, 8), int64(3)})
	}
	for _, r := range []TimeRange{{10, 20}, {-5, 3}, {0, 1}, {math.MaxInt64 - 3, math.MaxInt64}, {0, math.MaxInt64}} {
		w, err := newTimeWindow(&r)
		if err != nil {
			t.Fatal(err)
		}
		begin, end := w.descendingRange(term)
		probes := []int64{0, 1, 2, 9, 10, 11, 19, 20, 21, math.MaxInt64 - 4, math.MaxInt64 - 3, math.MaxInt64 - 1, math.MaxInt64}
		for _, s := range probes {
			k := key(s)
			in := bytes.Compare(k, begin) >= 0 && bytes.Compare(k, end) < 0
			if want := s >= r.Start && s < r.End; in != want || w.contains(s) != want {
				t.Fatalf("window %+v start %d: range=%v contains=%v want %v", r, s, in, w.contains(s), want)
			}
		}
	}
	for _, r := range []TimeRange{{5, 5}, {-9, -1}, {-9, 0}} {
		w, err := newTimeWindow(&r)
		if err != nil || !w.empty {
			t.Fatalf("window %+v: empty=%v err=%v", r, w.empty, err)
		}
	}
	if w, _ := newTimeWindow(nil); w.set || !w.contains(math.MaxInt64) || !w.contains(0) {
		t.Fatal("absent window must contain every stored time")
	}
}

func TestSpanTermRangeResumesStrictlyAfter(t *testing.T) {
	t.Parallel()
	term := subspace.Sub("qa_resume")
	for _, order := range []string{"time", "trace"} {
		cur := Span{Trace: qaID(5, 16), ID: qaID(5, 8), Start: 100}
		next := Span{Trace: qaID(5, 16), ID: qaID(6, 8), Start: 100}
		if order == "time" {
			next = Span{Trace: qaID(4, 16), ID: qaID(1, 8), Start: 100} // smaller trace sorts later
		}
		rng, nonempty, err := spanTermRange(term, order, timeWindow{}, postingSuffix(cur, order))
		if err != nil || !nonempty {
			t.Fatalf("%s: nonempty=%v err=%v", order, nonempty, err)
		}
		for ordinal := range int64(4) {
			k := term.Pack(append(postingSuffix(cur, order), ordinal))
			if bytes.Compare(k, rng.Begin.FDBKey()) >= 0 {
				t.Fatalf("%s: resumed range still includes the last emitted span (ordinal %d)", order, ordinal)
			}
		}
		k := term.Pack(append(postingSuffix(next, order), int64(0)))
		if bytes.Compare(k, rng.Begin.FDBKey()) < 0 || bytes.Compare(k, rng.End.FDBKey()) >= 0 {
			t.Fatalf("%s: resumed range excludes the next span", order)
		}
	}
}

// ---------------------------------------------------------------------------
// Trace graph construction
// ---------------------------------------------------------------------------

func qaGraphSpan(t *testing.T, trace string, id, parent string, start int64, links ...*tracepb.Span_Link) graphSpan {
	sp := &tracepb.Span{TraceId: qaBytes(t, trace), SpanId: qaBytes(t, id), StartTimeUnixNano: uint64(start), Links: links}
	if parent != "" {
		sp.ParentSpanId = qaBytes(t, parent)
	}
	return graphSpan{SpanMatch{trace, id, start}, sp}
}

func TestBuildTraceGraph(t *testing.T) {
	t.Parallel()
	tr := qaID(1, 16)
	other := qaID(2, 16)
	R, A, B, C, O, M, X, Y, Z, S, Q := qaID(10, 8), qaID(11, 8), qaID(12, 8), qaID(13, 8), qaID(14, 8), qaID(15, 8), qaID(16, 8), qaID(17, 8), qaID(18, 8), qaID(19, 8), qaID(20, 8)
	zero := "0000000000000000"
	linkAttrs := []*commonpb.KeyValue{exKV("link.kind", exStr("follows"))}
	in := []graphSpan{
		qaGraphSpan(t, tr, C, A, 40),
		qaGraphSpan(t, tr, Z, X, 90),
		qaGraphSpan(t, tr, A, R, 30, &tracepb.Span_Link{TraceId: qaBytes(t, tr), SpanId: qaBytes(t, M)}),
		qaGraphSpan(t, tr, X, Y, 60),
		qaGraphSpan(t, tr, O, M, 5),
		qaGraphSpan(t, tr, S, S, 80),
		qaGraphSpan(t, tr, B, R, 20),
		qaGraphSpan(t, tr, Y, X, 70),
		qaGraphSpan(t, tr, Q, zero, 50),
		qaGraphSpan(t, tr, R, "", 10,
			&tracepb.Span_Link{TraceId: qaBytes(t, tr), SpanId: qaBytes(t, C), TraceState: "k=v", Flags: 1, Attributes: linkAttrs},
			&tracepb.Span_Link{TraceId: qaBytes(t, other), SpanId: qaBytes(t, A)}),
	}
	g := buildTraceGraph(tr, in)
	links := g.Links
	g.Links = nil
	want := TraceGraph{
		Trace:          tr,
		Spans:          []SpanMatch{{tr, O, 5}, {tr, R, 10}, {tr, B, 20}, {tr, A, 30}, {tr, C, 40}, {tr, Q, 50}, {tr, X, 60}, {tr, Y, 70}, {tr, S, 80}, {tr, Z, 90}},
		Roots:          []string{R, Q},
		Children:       map[string][]string{R: {B, A}, A: {C}, X: {Y, Z}, Y: {X}, S: {S}},
		Parent:         map[string]string{A: R, B: R, C: A, O: M, X: Y, Y: X, Z: X, S: S},
		MissingParents: []string{M},
		Orphans:        []string{O},
		Cycles:         []string{X, Y, S},
		Depth:          map[string]int{R: 0, Q: 0, B: 1, A: 1, C: 2},
		MaxDepth:       2,
	}
	sort.Strings(want.Cycles)
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("graph\n got %+v\nwant %+v", g, want)
	}
	if len(links) != 2 || len(links[R]) != 2 || len(links[A]) != 1 {
		t.Fatalf("links: %+v", links)
	}
	if l := links[R][0]; l.TraceID != tr || l.SpanID != C || !l.Resolved || l.TraceState != "k=v" || l.Flags != 1 || len(l.Attributes) != 1 || !proto.Equal(l.Attributes[0], linkAttrs[0]) {
		t.Fatalf("same-trace stored link: %+v", l)
	}
	if l := links[R][1]; l.TraceID != other || l.Resolved {
		t.Fatalf("cross-trace link must stay unresolved: %+v", l)
	}
	if l := links[A][0]; l.SpanID != M || l.Resolved {
		t.Fatalf("link to an absent span must stay unresolved: %+v", l)
	}

	empty := buildTraceGraph(tr, nil)
	if empty.Spans == nil || empty.Roots == nil || empty.Cycles == nil || empty.MissingParents == nil || empty.Orphans == nil || len(empty.Children) != 0 {
		t.Fatalf("empty graph must use empty, non-nil collections: %+v", empty)
	}
}

// Long chains and cycles are walked iteratively in linear time.
func TestBuildTraceGraphDeepChainAndLongCycle(t *testing.T) {
	t.Parallel()
	tr := qaID(1, 16)
	const n = 50000
	chain := make([]graphSpan, n)
	for i := range n {
		parent := ""
		if i > 0 {
			parent = qaID(i-1, 8)
		}
		chain[n-1-i] = qaGraphSpan(t, tr, qaID(i, 8), parent, int64(i))
	}
	g := buildTraceGraph(tr, chain)
	if g.MaxDepth != n-1 || len(g.Roots) != 1 || len(g.Cycles) != 0 || len(g.Depth) != n {
		t.Fatalf("chain: depth=%d roots=%d cycles=%d", g.MaxDepth, len(g.Roots), len(g.Cycles))
	}
	ring := make([]graphSpan, n)
	for i := range n {
		ring[i] = qaGraphSpan(t, tr, qaID(i, 8), qaID((i+1)%n, 8), int64(i))
	}
	g = buildTraceGraph(tr, ring)
	if len(g.Cycles) != n || len(g.Roots) != 0 || len(g.Depth) != 0 || len(g.Orphans) != 0 {
		t.Fatalf("ring: cycles=%d roots=%d depth=%d", len(g.Cycles), len(g.Roots), len(g.Depth))
	}
}

// ---------------------------------------------------------------------------
// Real FDB integration
// ---------------------------------------------------------------------------

var qaLayouts = []struct {
	name   string
	mutate func(*ExporterConfig)
}{
	{"time_tuple_h8", nil},
	{"trace_json_h32", func(c *ExporterConfig) {
		c.Order, c.PostingValueFormat, c.RecordCompression, c.HashBytes = "trace", "json", "none", 32
	}},
	// One-byte digests make most distinct keys/values collide inside a term.
	{"time_tuple_h1", func(c *ExporterConfig) { c.HashBytes = 1 }},
	{"trace_tuple_h1", func(c *ExporterConfig) { c.Order, c.HashBytes = "trace", 1 }},
}

const qaBase = uint64(1_700_000_000_000_000_000)

// qaCorpus is a seeded corpus with many start-time ties and digest collisions.
func qaCorpus(seed uint64) []*tracepb.ResourceSpans {
	rng := rand.New(rand.NewPCG(seed, 283))
	id := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.UintN(256))
		}
		b[0] |= 1
		return b
	}
	models := []string{"gpt-4o", "claude-3", collision("gpt-4o")}
	collidingKey := collision("gen_ai.request.model")
	var out []*tracepb.ResourceSpans
	for tr := range 16 {
		traceID := id(16)
		service := []string{"svc-a", "svc-b"}[tr%2]
		rs := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr(service))}}, ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "qa"}}}}
		for range 6 {
			attrs := []*commonpb.KeyValue{exKV("gen_ai.request.model", exStr(models[rng.IntN(len(models))]))}
			if rng.IntN(3) == 0 {
				attrs = append(attrs, exKV(collidingKey, exStr("gpt-4o")))
			}
			switch rng.IntN(3) {
			case 0:
				attrs = append(attrs, exKV("error.type", exStr("timeout")))
			case 1:
				attrs = append(attrs, exKV("error.type", exStr("rate_limit")))
			}
			attrs = append(attrs, exKV("feature.flag", exInt(int64(rng.IntN(3)+1))), exKV("cached", exBool(rng.IntN(2) == 0)))
			sp := &tracepb.Span{TraceId: traceID, SpanId: id(8), Name: "op", StartTimeUnixNano: qaBase + uint64(rng.IntN(40))*1000, Attributes: attrs}
			rs.ScopeSpans[0].Spans = append(rs.ScopeSpans[0].Spans, sp)
		}
		out = append(out, rs)
	}
	return out
}

func qaExport(t *testing.T, ctx context.Context, e *Exporter, rss []*tracepb.ResourceSpans) {
	t.Helper()
	want := int64(0)
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			want += int64(len(ss.Spans))
		}
	}
	stats, err := e.ExportResourceSpans(ctx, rss)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Spans != want {
		t.Fatalf("exported %d spans, want %d", stats.Spans, want)
	}
}

// qaPaginate drains a same-span query and checks the page contract.
func qaPaginate(t *testing.T, ctx context.Context, e *Exporter, q SameSpanQuery) ([]SpanMatch, int) {
	t.Helper()
	limit := q.Limit
	if limit == 0 {
		limit = e.Config().K
	}
	all := []SpanMatch{}
	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
		page, err := e.QuerySameSpan(ctx, q)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", pages, q.Predicates, err)
		}
		if page.Order != e.Config().Order || page.ReadVersion <= 0 || page.Spans == nil {
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

func TestAdvancedQueriesRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, layout := range qaLayouts {
		t.Run("same_span/"+layout.name, func(t *testing.T) { testSameSpanPagination(t, cluster, layout.mutate) })
	}
	t.Run("same_span_budget", func(t *testing.T) { testSameSpanBudget(t, cluster) })
	t.Run("continuation_security", func(t *testing.T) { testContinuationSecurity(t, cluster) })
	for _, layout := range qaLayouts {
		t.Run("trace_existential/"+layout.name, func(t *testing.T) { testTraceExistential(t, cluster, layout.mutate) })
	}
	t.Run("trace_graph", func(t *testing.T) { testTraceGraphRealFDB(t, cluster) })
}

func testSameSpanPagination(t *testing.T, cluster string, mutate func(*ExporterConfig)) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, mutate)
	order := e.Config().Order
	rss := qaCorpus(42)
	qaExport(t, ctx, e, rss)
	fixture := qaFixture(rss)

	model := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	timeout := Attribute{"span", "error.type", "string", "timeout"}
	window := &TimeRange{Start: int64(qaBase + 10000), End: int64(qaBase + 30000)}
	queries := []SameSpanQuery{
		{Predicates: []Attribute{model, timeout}, Limit: 2},
		{Predicates: []Attribute{model, {"resource", "service.name", "string", "svc-a"}, {"span", "feature.flag", "int", "2"}}, Limit: 1},
		{Predicates: []Attribute{{"span", "cached", "string", "true"}, {"span", "gen_ai.request.model", "string", "claude-3"}}, TimeWindow: window, Limit: 3},
		{Predicates: []Attribute{{"span", "gen_ai.request.model", "string", collision("gpt-4o")}}, Limit: 4},
		{Predicates: []Attribute{{"span", collision("gen_ai.request.model"), "string", "gpt-4o"}, model}, Limit: 5},
		{Predicates: []Attribute{model, timeout}},
		{Predicates: []Attribute{model, {"span", "error.type", "string", "absent"}}, Limit: 2},
		{Predicates: []Attribute{model}, TimeWindow: &TimeRange{Start: 7, End: 7}},
	}
	nonempty := 0
	for _, q := range queries {
		want := qaSameSpanOracle(fixture, q.Predicates, q.TimeWindow, order)
		got, pages := qaPaginate(t, ctx, e, q)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s same-span %+v window %+v\n got %+v\nwant %+v", order, q.Predicates, q.TimeWindow, got, want)
		}
		if q.Limit > 0 && len(want) > q.Limit && pages < 2 {
			t.Fatalf("expected multiple pages for %+v", q.Predicates)
		}
		if len(want) > 0 {
			nonempty++
		}
	}
	if nonempty < 5 {
		t.Fatalf("vacuous corpus: only %d of %d queries matched", nonempty, len(queries))
	}

	// The continuation binds the predicate set, so a resumed page may reorder the driver.
	first, err := e.QuerySameSpan(ctx, queries[0])
	if err != nil || first.Continuation == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	swapped := SameSpanQuery{Predicates: []Attribute{timeout, model}, Limit: 2, Continuation: first.Continuation}
	second, err := e.QuerySameSpan(ctx, swapped)
	if err != nil {
		t.Fatal(err)
	}
	want := qaSameSpanOracle(fixture, queries[0].Predicates, nil, order)
	if !reflect.DeepEqual(append(first.Spans, second.Spans...), want[:min(len(want), 4)]) {
		t.Fatalf("driver-swapped resume:\n got %+v %+v\nwant %+v", first.Spans, second.Spans, want)
	}
	if e.backend.LastQueryWork().Candidates != second.Candidates || second.Candidates == 0 {
		t.Fatalf("query work accounting: last=%+v page=%d", e.backend.LastQueryWork(), second.Candidates)
	}

	// Live pagination: a new matching span sorting after the cursor is visible on the next page.
	if order == "time" {
		tail := want[1]
		late := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: qaBytes(t, qaID(900, 16)), SpanId: qaBytes(t, qaID(900, 8)), StartTimeUnixNano: uint64(tail.Start - 1), Attributes: []*commonpb.KeyValue{exKV("gen_ai.request.model", exStr("gpt-4o")), exKV("error.type", exStr("timeout"))}}}}}}
		qaExport(t, ctx, e, []*tracepb.ResourceSpans{late})
		next, err := e.QuerySameSpan(ctx, SameSpanQuery{Predicates: queries[0].Predicates, Limit: 100, Continuation: first.Continuation})
		if err != nil {
			t.Fatal(err)
		}
		fixture = append(fixture, qaFixture([]*tracepb.ResourceSpans{late})...)
		all := qaSameSpanOracle(fixture, queries[0].Predicates, nil, order)
		if !reflect.DeepEqual(next.Spans, all[2:]) {
			t.Fatalf("resumed live page:\n got %+v\nwant %+v", next.Spans, all[2:])
		}
	}
}

func testSameSpanBudget(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	q := SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.request.model", "string", "gpt-4o"}, {"span", "error.type", "string", "timeout"}}}
	for name, mutate := range map[string]func(*ExporterConfig){
		"candidates": func(c *ExporterConfig) { c.MaxCandidates = 3 },
		"bytes":      func(c *ExporterConfig) { c.MaxBytes = 128 },
	} {
		e := openTestExporter(t, ctx, cluster, mutate)
		qaExport(t, ctx, e, qaCorpus(7))
		page, err := e.QuerySameSpan(ctx, q)
		requireOtelBudget(t, err)
		if page.Candidates == 0 || page.Continuation != nil {
			t.Fatalf("%s: budget failure must report work and carry no continuation: %+v", name, page)
		}
		if _, err := e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: q.Predicates}); err == nil {
			t.Fatalf("%s: existential within budget unexpectedly", name)
		} else {
			requireOtelBudget(t, err)
		}
		// A term with no postings fits any budget.
		empty, err := e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "nope", "string", "x"}}})
		if err != nil || len(empty.Spans) != 0 || empty.Candidates != 0 {
			t.Fatalf("%s: empty term: %+v %v", name, empty, err)
		}
	}
}

func testContinuationSecurity(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	rss := qaCorpus(9)
	qaExport(t, ctx, e, rss)
	model := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	q := SameSpanQuery{Predicates: []Attribute{model}, Limit: 2}
	first, err := e.QuerySameSpan(ctx, q)
	if err != nil || first.Continuation == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	want := qaSameSpanOracle(qaFixture(rss), q.Predicates, nil, "time")
	resume := func(e *Exporter, q SameSpanQuery, token []byte) (SpanQueryPage, error) {
		q.Continuation = token
		return e.QuerySameSpan(ctx, q)
	}

	// A second handle on the same run and tenant shares the persisted signing key.
	same := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Run = e.Config().Run })
	page, err := resume(same, q, first.Continuation)
	if err != nil || !reflect.DeepEqual(page.Spans, want[2:4]) {
		t.Fatalf("resume on second handle: %+v %v", page.Spans, err)
	}

	tampered := append([]byte(nil), first.Continuation...)
	tampered[len(tampered)/2] ^= 0x01
	_, err = resume(e, q, tampered)
	requireConfigOrCorruption(t, "tampered payload", err)
	macFlip := append([]byte(nil), first.Continuation...)
	macFlip[len(macFlip)-1] ^= 0x01
	_, err = resume(e, q, macFlip)
	requireTyped[*CorruptionError](t, "tampered MAC", err)

	_, err = resume(e, SameSpanQuery{Predicates: []Attribute{model, {"span", "error.type", "string", "timeout"}}, Limit: 2}, first.Continuation)
	requireTyped[*ConfigError](t, "wrong query", err)
	_, err = resume(e, SameSpanQuery{Predicates: []Attribute{model}, Limit: 2, TimeWindow: &TimeRange{Start: 0, End: math.MaxInt64}}, first.Continuation)
	requireTyped[*ConfigError](t, "wrong window", err)

	otherTenant := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Run, c.Tenant = e.Config().Run, 12 })
	_, err = resume(otherTenant, q, first.Continuation)
	requireTyped[*ConfigError](t, "wrong tenant", err)

	// Another run never shares the signing key: before and after it publishes its own.
	otherRun := openTestExporter(t, ctx, cluster, nil)
	_, err = resume(otherRun, q, first.Continuation)
	requireTyped[*CorruptionError](t, "other run without key", err)
	qaExport(t, ctx, otherRun, rss)
	if p, err := otherRun.QuerySameSpan(ctx, q); err != nil || p.Continuation == nil {
		t.Fatalf("other run first page: %+v %v", p, err)
	}
	_, err = resume(otherRun, q, first.Continuation)
	requireTyped[*CorruptionError](t, "other run with key", err)

	for name, token := range map[string][]byte{"garbage": bytes.Repeat([]byte{0xab}, 80), "short": {1, 2, 3}} {
		_, err = resume(e, q, token)
		requireTyped[*ConfigError](t, name, err)
	}

	// Rejections leave the original continuation valid.
	page, err = resume(e, q, first.Continuation)
	if err != nil || !reflect.DeepEqual(page.Spans, want[2:4]) {
		t.Fatalf("original continuation after rejections: %+v %v", page.Spans, err)
	}
}

type qaSpec struct {
	trace, span string
	start       uint64
	attrs       []*commonpb.KeyValue
}

func qaSpans(t *testing.T, specs ...qaSpec) []*tracepb.ResourceSpans {
	t.Helper()
	var out []*tracepb.ResourceSpans
	for _, s := range specs {
		sp := &tracepb.Span{TraceId: qaBytes(t, s.trace), SpanId: qaBytes(t, s.span), Name: "op", StartTimeUnixNano: s.start, Attributes: s.attrs}
		out = append(out, &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}})
	}
	return out
}

func testTraceExistential(t *testing.T, cluster string, mutate func(*ExporterConfig)) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, mutate)
	modelKV := exKV("gen_ai.request.model", exStr("gpt-4o"))
	failedKV := exKV("gen_ai.tool.status", exStr("failed"))
	decoy := exKV("gen_ai.request.model", exStr(collision("gpt-4o")))
	T1, T2, T3, T4, T5 := qaID(1, 16), qaID(2, 16), qaID(3, 16), qaID(4, 16), qaID(5, 16)
	s := func(n int) string { return qaID(n, 8) }
	qaExport(t, ctx, e, qaSpans(
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
	))
	model := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	failed := Attribute{"span", "gen_ai.tool.status", "string", "failed"}
	m := func(tr, sp string, start int64) SpanMatch { return SpanMatch{tr, sp, start} }
	hitT1 := TraceExistentialHit{T1, 200, [][]SpanMatch{{m(T1, s(11), 100), m(T1, s(13), 50)}, {m(T1, s(12), 200)}}, nil}
	hitT2 := TraceExistentialHit{T2, 300, [][]SpanMatch{{m(T2, s(21), 300)}, {m(T2, s(21), 300)}}, nil}
	hitT5 := TraceExistentialHit{T5, 600, [][]SpanMatch{{m(T5, s(51), 600)}, {m(T5, s(52), 50)}}, nil}
	for name, c := range map[string]struct {
		q    TraceExistentialQuery
		want []TraceExistentialHit
	}{
		"both":       {TraceExistentialQuery{Clauses: []Attribute{model, failed}}, []TraceExistentialHit{hitT5, hitT2, hitT1}},
		"limit":      {TraceExistentialQuery{Clauses: []Attribute{model, failed}, Limit: 2}, []TraceExistentialHit{hitT5, hitT2}},
		"window":     {TraceExistentialQuery{Clauses: []Attribute{model, failed}, TimeWindow: &TimeRange{Start: 0, End: 250}}, []TraceExistentialHit{hitT1}},
		"empty join": {TraceExistentialQuery{Clauses: []Attribute{model, {"span", "gen_ai.tool.status", "string", "ok"}}}, []TraceExistentialHit{}},
		"single": {TraceExistentialQuery{Clauses: []Attribute{failed}}, []TraceExistentialHit{
			{T4, 500, [][]SpanMatch{{m(T4, s(41), 500)}}, nil}, {T2, 300, [][]SpanMatch{{m(T2, s(21), 300)}}, nil}, {T1, 200, [][]SpanMatch{{m(T1, s(12), 200)}}, nil}, {T5, 50, [][]SpanMatch{{m(T5, s(52), 50)}}, nil},
		}},
	} {
		got, err := e.QueryTraceExistential(ctx, c.q)
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

	// Correlated logs join through their trace; uncorrelated ones never fabricate a trace.
	eval := exKV("gen_ai.evaluation.name", exStr("relevance"))
	if _, err := e.ExportLogs(ctx, []LogRecord{
		{Timestamp: 150, TraceID: T1, SpanID: s(12), EventName: "gen_ai.evaluation.result", Attributes: []*commonpb.KeyValue{eval}},
		{EventID: "t3-eval", Timestamp: 450, TraceID: T3, Attributes: []*commonpb.KeyValue{eval}},
		{EventID: "orphan-eval", Timestamp: 999, Attributes: []*commonpb.KeyValue{eval}},
		{EventID: "t4-other", Timestamp: 700, TraceID: T4, Attributes: []*commonpb.KeyValue{exKV("gen_ai.evaluation.name", exStr("toxicity"))}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{model}, LogClauses: []Attribute{{LogScopeRecord, "gen_ai.evaluation.name", "string", "relevance"}}})
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
	logOnly, err := e.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{{LogScopeIntrinsic, "event_name", "string", "gen_ai.evaluation.result"}}})
	if err != nil || len(logOnly.Hits) != 1 || logOnly.Hits[0].Trace != T1 || len(logOnly.Hits[0].Witnesses) != 0 {
		t.Fatalf("log-only existential: %+v %v", logOnly, err)
	}

	for name, q := range map[string]TraceExistentialQuery{
		"no clauses":      {},
		"bad scope":       {Clauses: []Attribute{{LogScopeRecord, "k", "string", "v"}}},
		"bad log scope":   {LogClauses: []Attribute{{"span", "k", "string", "v"}}},
		"negative limit":  {Clauses: []Attribute{model}, Limit: -1},
		"inverted window": {Clauses: []Attribute{model}, TimeWindow: &TimeRange{Start: 2, End: 1}},
	} {
		_, err := e.QueryTraceExistential(ctx, q)
		requireTyped[*ConfigError](t, name, err)
	}
}

func testTraceGraphRealFDB(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	tr, other := qaID(70, 16), qaID(71, 16)
	R, A, B, O, M, X, Y := qaID(1, 8), qaID(2, 8), qaID(3, 8), qaID(4, 8), qaID(5, 8), qaID(6, 8), qaID(7, 8)
	span := func(id, parent string, start uint64, links ...*tracepb.Span_Link) *tracepb.ResourceSpans {
		sp := &tracepb.Span{TraceId: qaBytes(t, tr), SpanId: qaBytes(t, id), Name: "op", StartTimeUnixNano: start, Links: links}
		if parent != "" {
			sp.ParentSpanId = qaBytes(t, parent)
		}
		return &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}
	}
	// Children arrive before their parents, in separate calls.
	qaExport(t, ctx, e, []*tracepb.ResourceSpans{span(A, R, 30), span(O, M, 5), span(X, Y, 60)})
	qaExport(t, ctx, e, []*tracepb.ResourceSpans{span(Y, X, 70), span(B, R, 20), span(R, "", 10,
		&tracepb.Span_Link{TraceId: qaBytes(t, tr), SpanId: qaBytes(t, A), Attributes: []*commonpb.KeyValue{exKV("k", exStr("v"))}},
		&tracepb.Span_Link{TraceId: qaBytes(t, other), SpanId: qaBytes(t, A)})})
	qaExport(t, ctx, e, []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: qaBytes(t, other), SpanId: qaBytes(t, A), StartTimeUnixNano: 1}}}}}})

	g, err := e.TraverseTraceGraph(ctx, tr)
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
	if e.backend.LastQueryWork().Candidates != 6 {
		t.Fatalf("graph work: %+v", e.backend.LastQueryWork())
	}

	empty, err := e.TraverseTraceGraph(ctx, qaID(99, 16))
	if err != nil || len(empty.Spans) != 0 || empty.Roots == nil {
		t.Fatalf("absent trace: %+v %v", empty, err)
	}
	_, err = e.TraverseTraceGraph(ctx, "ABC")
	requireTyped[*ConfigError](t, "invalid trace", err)

	small := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.MaxCandidates = 3 })
	qaExport(t, ctx, small, []*tracepb.ResourceSpans{span(A, R, 30), span(B, R, 20), span(R, "", 10), span(O, M, 5)})
	_, err = small.TraverseTraceGraph(ctx, tr)
	requireOtelBudget(t, err)
}
