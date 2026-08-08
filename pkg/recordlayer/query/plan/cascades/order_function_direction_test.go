package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// OrderFunctionDirection is what eight sites in the planner use to decide
// whether an index column is an ORDER function and, if so, in which direction
// its bytes are encoded — the answer feeds matched sort order, so getting it
// wrong is a wrong plan, not an error. It had no direct test; this is it.
//
// The recognised names are asserted through the FunctionKind constants rather
// than as literals, so a rename cannot silently narrow what the planner
// recognises while a literal table keeps passing.
func TestOrderFunctionDirection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want values.OrderedBytesDirection
		ok   bool
	}{
		{FunctionKindOrderAscNullsFirst, values.OrderedBytesAscNullsFirst, true},
		{FunctionKindOrderAscNullsLast, values.OrderedBytesAscNullsLast, true},
		{FunctionKindOrderDescNullsFirst, values.OrderedBytesDescNullsFirst, true},
		{FunctionKindOrderDescNullsLast, values.OrderedBytesDescNullsLast, true},
		{"unknown", 0, false},
		{"", 0, false},
		{"order_asc", 0, false},
		{"rank", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := OrderFunctionDirection(tc.name)
			if ok != tc.ok {
				t.Fatalf("OrderFunctionDirection(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("OrderFunctionDirection(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// The case-folding is a Go-side LENIENCY, not parity, and it is pinned here so
// it is a visible decision rather than an accident of a ToLower nobody meant.
//
// Java resolves these names through an exact-match registry: the factory builds
// one entry per Direction from Direction.values() and looks the name up by
// equality (OrderFunctionKeyExpressionFactory), so `ORDER_ASC_NULLS_FIRST` does
// not resolve to an order function there. Go accepts it.
//
// It is unreachable today because every producer of these names in this tree
// mints the lowercase constant above, so no metadata Go writes can carry the
// upper-case spelling. If that ever stops being true — a name arriving from
// externally authored metadata, or from a SQL identifier that is upper-cased on
// the way in — the two engines will disagree about whether a column is ordered,
// and Go will be the one that plans an ordered scan Java does not. This test is
// what makes that a decision someone can find.
func TestOrderFunctionDirectionAcceptsUpperCaseUnlikeJava(t *testing.T) {
	t.Parallel()

	got, ok := OrderFunctionDirection("ORDER_ASC_NULLS_FIRST")
	if !ok {
		t.Fatal("OrderFunctionDirection no longer folds case. If that was deliberate — it matches Java's " +
			"exact-match registry — delete this test and say so; if it was not, an upper-case order-function " +
			"name now silently reads as 'not an order function' and the column loses its direction")
	}
	if got != values.OrderedBytesAscNullsFirst {
		t.Fatalf("folded lookup = %v, want %v", got, values.OrderedBytesAscNullsFirst)
	}
}
