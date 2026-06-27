package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMemoMerge_MultiWayJoinNoPanic is the regression for the latent crash
// RFC-037 cross-group merging introduced on multi-way joins: after a merge,
// two child Quantifiers can resolve to the same canonical Reference, and
// SemanticEquals' positional child-matching then built an alias bijection
// that conflicted — matchChildrenPositional panicked instead of returning
// "not equal". The dimensional gap (no multi-way-join-with-mergeable-inputs
// test) let it pass 46/46 originally. Master does not merge, so it never
// crashed there; this exercises the exact shape that did.
func TestMemoMerge_MultiWayJoinNoPanic(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scanT := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	scanU := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"U"}, values.UnknownType))
	// Two join inputs that rewrite to the same sub-product (merge candidates).
	distinctRef := expressions.InitialOf(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanT)))
	a := expressions.InitialOf(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(distinctRef)))
	innerFilter := expressions.InitialOf(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(scanT)))
	b := expressions.InitialOf(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(innerFilter)))
	qA := expressions.ForEachQuantifier(a)
	qB := expressions.ForEachQuantifier(b)
	qC := expressions.ForEachQuantifier(scanU)
	join := expressions.InitialOf(expressions.NewSelectExpression(qA.GetFlowedObjectValue(), []expressions.Quantifier{qA, qB, qC}, nil))

	p := NewPlanner(DefaultExpressionRules(), nil)
	// Must not panic, and must converge.
	_, conv := p.Explore(join)
	if !conv {
		t.Fatal("planner did not converge on multi-way join")
	}
}
