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

// Order-function classification is EXACT, and this is the arm that used to be
// inverted: OrderFunctionDirection folded case, so `ORDER_ASC_NULLS_FIRST` read
// as the built-in ascending-nulls-first tuple encoding.
//
// The old pin called that an unreachable Go-side leniency, on the grounds that
// "every producer of these names in this tree mints the lowercase constant".
// That argument scoped the wrong population: RegisterFunction
// (key_expression.go) is public API over a case-sensitive map, so an
// application can register its OWN evaluator under the upper-case spelling.
// The record layer then encodes that column with the application's function
// while the planner believes it is tuple-order bytes — and derives ordered
// ranges, or drops a sort, from an ordering nothing produced.
//
// Java cannot reach this state at all: it dispatches on the
// OrderFunctionKeyExpression TYPE, which carries the direction as a field, and
// its factory registers names already lowercased.
func TestOrderFunctionDirectionIsExactMatch(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"ORDER_ASC_NULLS_FIRST",
		"Order_Asc_Nulls_First",
		"ORDER_DESC_NULLS_LAST",
	} {
		if _, ok := OrderFunctionDirection(name); ok {
			t.Errorf("OrderFunctionDirection(%q) classified a non-registered spelling as a built-in "+
				"order function. RegisterFunction is case-sensitive, so this name can belong to an "+
				"application's own evaluator, and treating it as tuple-order encoding lets the planner "+
				"derive an ordering from bytes that evaluator never wrote", name)
		}
	}

	// The control: the registered spellings must still resolve, or the check
	// above would be satisfied by a function that classifies nothing at all.
	for name, want := range map[string]values.OrderedBytesDirection{
		FunctionKindOrderAscNullsFirst:  values.OrderedBytesAscNullsFirst,
		FunctionKindOrderAscNullsLast:   values.OrderedBytesAscNullsLast,
		FunctionKindOrderDescNullsFirst: values.OrderedBytesDescNullsFirst,
		FunctionKindOrderDescNullsLast:  values.OrderedBytesDescNullsLast,
	} {
		got, ok := OrderFunctionDirection(name)
		if !ok || got != want {
			t.Errorf("OrderFunctionDirection(%q) = (%v, %v), want (%v, true)", name, got, ok, want)
		}
	}
}
