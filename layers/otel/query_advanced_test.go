package otel

import (
	"bytes"
	"reflect"
	"sort"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Continuation tokens
// ---------------------------------------------------------------------------

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
	plan, err := (&SpanReader{cfg: Config{K: 7}}).newSameSpanPlan("test", SameSpanQuery{Predicates: []Attribute{a, c, a}})
	if err != nil {
		t.Fatal(err)
	}
	var preds []Attribute
	for _, x := range plan.attrs {
		preds = append(preds, x.a)
	}
	if !reflect.DeepEqual(preds, []Attribute{a, c}) || plan.limit != 7 || !bytes.Equal(plan.digest, base) {
		t.Fatalf("plan dedupe/default limit: %+v", plan)
	}
}

// ---------------------------------------------------------------------------
// Trace graph construction
// ---------------------------------------------------------------------------

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
