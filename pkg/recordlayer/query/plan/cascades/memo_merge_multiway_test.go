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
	tRow := values.NewRecordType("T", false, []values.Field{
		{Name: "active", FieldType: values.TypeBool, Ordinal: 0},
	})
	uRow := values.NewRecordType("U", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	predRoot, predRootErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("predicate_source"),
		tRow,
	)
	predRoot = mustConstruct(t, predRoot, predRootErr)
	predValue, predValueErr := values.ResolveFieldOrdinals(predRoot, []int{0})
	predValue = mustConstruct(t, predValue, predValueErr)
	pred := predicates.NewValuePredicate(predValue)
	scanT := expressions.InitialOf(mustFullUnorderedScan(t, []string{"T"}, tRow))
	scanU := expressions.InitialOf(mustFullUnorderedScan(t, []string{"U"}, uRow))
	// Two join inputs that rewrite to the same sub-product (merge candidates).
	distinct, distinctErr := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanT))
	distinct = mustConstruct(t, distinct, distinctErr)
	distinctRef := expressions.InitialOf(distinct)
	filterA, filterAErr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(distinctRef),
	)
	filterA = mustConstruct(t, filterA, filterAErr)
	a := expressions.InitialOf(filterA)
	innerFilterExpr, innerFilterErr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanT),
	)
	innerFilterExpr = mustConstruct(t, innerFilterExpr, innerFilterErr)
	innerFilter := expressions.InitialOf(innerFilterExpr)
	distinctB, distinctBErr := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(innerFilter))
	distinctB = mustConstruct(t, distinctB, distinctBErr)
	b := expressions.InitialOf(distinctB)
	qA := expressions.ForEachQuantifier(a)
	qB := expressions.ForEachQuantifier(b)
	qC := expressions.ForEachQuantifier(scanU)
	result, resultErr := qA.RequireFlowedObjectValue()
	result = mustConstruct(t, result, resultErr)
	joinExpr, joinErr := expressions.NewSelectExpression(
		result,
		[]expressions.Quantifier{qA, qB, qC},
		nil,
	)
	joinExpr = mustConstruct(t, joinExpr, joinErr)
	join := expressions.InitialOf(joinExpr)

	p := NewPlanner(DefaultExpressionRules(), nil)
	// Must not panic, and must converge.
	_, conv := exploreRewriting(p, join)
	if !conv {
		t.Fatal("planner did not converge on multi-way join")
	}
}
