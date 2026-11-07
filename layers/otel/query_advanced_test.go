package otel

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

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
