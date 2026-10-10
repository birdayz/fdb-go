package embedded

import (
	"context"
	"database/sql/driver"
	"math"
	"testing"

	"fdb.dev/pkg/relational/core/parser"
)

// Exact binding encodings constrain specialization and identify duplicate
// values before runtime slots are assigned. Float bits and carrier types matter.
func TestBindStatementParameters_KeyIsExact(t *testing.T) {
	t.Parallel()
	key := func(v any) string {
		t.Helper()
		root, err := parser.Parse("SELECT ? FROM t")
		if err != nil {
			t.Fatal(err)
		}
		k, release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: v}})
		release()
		if err != nil {
			t.Fatalf("bind %#v: %v", v, err)
		}
		return k
	}
	for _, pair := range [][2]any{
		{true, false},
		{math.Float64frombits(0x7ff8000000000000), math.Float64frombits(0xfff8000000000000)},
		{math.Float64frombits(0x7ff8000000000000), math.Float64frombits(0x7ff800000000abcd)},
		{0.0, math.Copysign(0, -1)},
		{float32(0), float32(math.Copysign(0, -1))},
		{nil, ""},
		{"", []byte{}},
		{nil, []byte{}},
		{[]float64{0}, []float64{math.Copysign(0, -1)}},
		{[]string{"a", "b"}, []string{"a,b"}},
		{[]bool{true}, []bool{false}},
		{"a\x00b", "a"},
	} {
		if key(pair[0]) == key(pair[1]) {
			t.Errorf("%#v and %#v share the plan-cache key %q", pair[0], pair[1], key(pair[0]))
		}
	}
	// Equal values in distinct carriers (a fresh slice, a fresh NaN of the
	// same bits) key alike, so a warm hit still happens.
	for _, mk := range []func() any{
		func() any { return true },
		func() any { return math.Float64frombits(0xfff8000000000000) },
		func() any { return []byte{1} },
		func() any { return []int64{1, 2} },
		func() any { return nil },
	} {
		if a, b := key(mk()), key(mk()); a != b {
			t.Errorf("%#v keys differently on each binding: %q, %q", mk(), a, b)
		}
	}
}

// Boolean folding is constrained by evaluation, as in Java EvaluatesToValue.
func TestPlanCache_BooleanEvaluationConstraint(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	plan := func(v any) {
		t.Helper()
		root, err := parser.Parse("SELECT id FROM orders WHERE amount = 1 AND ?")
		if err != nil {
			t.Fatal(err)
		}
		k, release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: v}})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		g.paramKey = k
		q := root.Statements().AllStatement()[0].SelectStatement().Query()
		if _, err := g.planSelectCascades(context.Background(), q, md, true, statementOptions{}); err != nil {
			t.Fatalf("plan with %v: %v", v, err)
		}
	}
	plan(true)
	plan(false)
	plan(false)
	if len(cap.events) != 3 {
		t.Fatalf("want 3 plan events, got %d", len(cap.events))
	}
	for i, want := range []PlanCacheEvent{PlanCacheMiss, PlanCacheMiss, PlanCacheHit} {
		if got := cap.events[i].Cache; got != want {
			t.Errorf("binding %d: cache %v, want %v", i, got, want)
		}
	}
}
