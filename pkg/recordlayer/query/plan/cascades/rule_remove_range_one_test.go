package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRemoveRangeOneRule pins RFC-189 F2: Java's RemoveRangeOneRule drops an
// UNREFERENCED RANGE(0,1) table-function quantifier from a SelectExpression that
// has more than one quantifier. (Latent today — Go only mints the single-row
// placeholder in the single-quantifier shape the >1 guard excludes — so the pin
// is memo-level.)
func TestRemoveRangeOneRule(t *testing.T) {
	t.Parallel()
	scanQ := func() expressions.Quantifier {
		return expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)))
	}
	rangeOneQ := func() expressions.Quantifier {
		return expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewTableFunctionExpression(values.NewRangeValue(
				&values.ConstantValue{Value: int64(0)},
				&values.ConstantValue{Value: int64(1)},
				&values.ConstantValue{Value: int64(1)}))))
	}

	t.Run("drops_unreferenced_range_one", func(t *testing.T) {
		t.Parallel()
		sQ, rQ := scanQ(), rangeOneQ()
		sel := expressions.NewSelectExpression(sQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{sQ, rQ}, nil)
		yielded := FireExpressionRule(NewRemoveRangeOneRule(), expressions.InitialOf(sel))
		// FireExpressionRule also fires on the quantifier-swapped permutation
		// (this Select is a 2-ForEach ChildrenAsSet join), so both orderings
		// yield the range-one-removed Select — one or more equivalent yields.
		if len(yielded) == 0 {
			t.Fatal("expected the rule to fire (RANGE(0,1) removed), got 0 yields")
		}
		for _, y := range yielded {
			newSel := y.(*expressions.SelectExpression)
			if got := len(newSel.GetQuantifiers()); got != 1 {
				t.Fatalf("expected 1 surviving quantifier, got %d", got)
			}
			if newSel.GetQuantifiers()[0].GetAlias() != sQ.GetAlias() {
				t.Fatal("the surviving quantifier must be the scan, not the RANGE(0,1)")
			}
		}
	})

	t.Run("declines_when_referenced", func(t *testing.T) {
		t.Parallel()
		sQ, rQ := scanQ(), rangeOneQ()
		// A predicate referencing the RANGE(0,1) quantifier's flowed value → the
		// alias IS referenced, so the rule must not fire.
		pred := predicates.NewValuePredicate(rQ.GetFlowedObjectValue())
		sel := expressions.NewSelectExpression(sQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{sQ, rQ}, []predicates.QueryPredicate{pred})
		yielded := FireExpressionRule(NewRemoveRangeOneRule(), expressions.InitialOf(sel))
		if len(yielded) != 0 {
			t.Fatalf("must not fire when the RANGE(0,1) alias is referenced, yielded %d", len(yielded))
		}
	})

	t.Run("declines_single_quantifier", func(t *testing.T) {
		t.Parallel()
		rQ := rangeOneQ()
		sel := expressions.NewSelectExpression(rQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{rQ}, nil)
		yielded := FireExpressionRule(NewRemoveRangeOneRule(), expressions.InitialOf(sel))
		if len(yielded) != 0 {
			t.Fatalf("must not fire on a single-quantifier Select (cardinality guard), yielded %d", len(yielded))
		}
	})
}
