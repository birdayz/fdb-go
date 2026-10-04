package cascades

// Java's RewritingCostModel penalizes a surviving outer join FIRST
// (ExpressionCountProperty.outerJoinCount), ahead of selectCount, so the
// REWRITING prune keeps RewriteOuterJoinRule's canonical form and SelectMerge
// and PredicatePushDown work on it. Go's LEFT OUTER select is implementable as
// a materialized nested-loop join (RFC-152); OuterJoinMaterializationRule
// re-forms it from the canonical form in PLANNING.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustOuterJoinCostConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct outer-join cost fixture: " + err.Error())
	}
	return value
}

func outerJoinCostRowType() values.Type {
	return values.NewRecordType("OuterJoinCostRow", false, []values.Field{
		{Name: "flag", FieldType: values.NullableLong, Ordinal: 0},
	})
}

// isOuterJoinSelectForTest reports whether e is a SelectExpression carrying LEFT or
// FULL OUTER semantics — Go's encoding of what Java models as a distinct
// OuterJoinExpression.
func isOuterJoinSelectForTest(e expressions.RelationalExpression) bool {
	sel, ok := e.(*expressions.SelectExpression)
	if !ok {
		return false
	}
	switch sel.GetJoinType() {
	case expressions.JoinLeftOuter, expressions.JoinFullOuter:
		return true
	default:
		return false
	}
}

// buildCorrelatedLeftOuterSelect constructs the flat un-rewritten LEFT-OUTER
// SelectExpression `... FROM A LEFT JOIN B ON A.flag = 1` — the ON-predicate
// references the preserved leg A, so RewriteOuterJoinRule's correlation guard passes
// and the rule fires (same fixture shape as rfc152).
func buildCorrelatedLeftOuterSelect() *expressions.SelectExpression {
	aliasA := values.NamedCorrelationIdentifier("A")
	aliasB := values.NamedCorrelationIdentifier("B")

	scanA := mustOuterJoinCostConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"A"}, outerJoinCostRowType()))
	scanB := mustOuterJoinCostConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"B"}, outerJoinCostRowType()))
	qA := expressions.NamedForEachQuantifier(aliasA, expressions.InitialOf(scanA))
	qB := expressions.NamedForEachQuantifier(aliasB, expressions.InitialOf(scanB))

	rootA := mustOuterJoinCostConstruct(values.NewQuantifiedObjectValue(aliasA, outerJoinCostRowType()))
	flagField := mustOuterJoinCostConstruct(values.ResolveFieldOrdinals(rootA, []int{0}))
	pred := predicates.NewComparisonPredicate(flagField, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: mustOuterJoinCostConstruct(qA.RequireFlowedObjectValue())},
		values.RecordConstructorField{Name: "B", Value: mustOuterJoinCostConstruct(qB.RequireFlowedObjectValue())},
	)

	return mustOuterJoinCostConstruct(expressions.NewSelectExpressionWithJoinType(
		result,
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{pred},
		[]string{"A", "B"},
		expressions.JoinLeftOuter,
	))
}

// deriveCanonicalRewrite runs RewriteOuterJoinRule and returns the canonical INNER
// rewritten SelectExpression (2 inner selects, 0 outer-join selects).
func deriveCanonicalRewrite(t *testing.T, unrewritten *expressions.SelectExpression) *expressions.SelectExpression {
	t.Helper()
	yielded := mustFireExpressionRule(t, NewRewriteOuterJoinRule(), expressions.InitialOf(unrewritten))
	for _, e := range yielded {
		if s, ok := e.(*expressions.SelectExpression); ok && s.GetJoinType() == expressions.JoinInner {
			return s
		}
	}
	t.Fatalf("RewriteOuterJoinRule yielded no canonical INNER SelectExpression (got %d expressions)", len(yielded))
	return nil
}

// TestRewritingCostModel_PrefersCanonicalOuterJoin: the canonical form (2
// selects, no outer join) beats the LEFT OUTER select (1 select) on
// outerJoinCount before selectCount is consulted.
func TestRewritingCostModel_PrefersCanonicalOuterJoin(t *testing.T) {
	t.Parallel()

	unrewritten := buildCorrelatedLeftOuterSelect()
	canonical := deriveCanonicalRewrite(t, unrewritten)

	if got := properties.EvaluateExpressionCount(unrewritten, isSelectExpression); got != 1 {
		t.Errorf("un-rewritten selectCount = %d, want 1", got)
	}
	if got := properties.EvaluateExpressionCount(canonical, isSelectExpression); got != 2 {
		t.Errorf("canonical selectCount = %d, want 2", got)
	}
	if got := properties.EvaluateExpressionCount(unrewritten, isOuterJoinSelectForTest); got != 1 {
		t.Errorf("un-rewritten outer-join count = %d, want 1", got)
	}
	if got := properties.EvaluateExpressionCount(canonical, isOuterJoinSelectForTest); got != 0 {
		t.Errorf("canonical outer-join count = %d, want 0", got)
	}
	if !RewritingCostModelLess(canonical, unrewritten) {
		t.Fatal("RewritingCostModelLess(canonical, unrewritten) = false; outerJoinCount no longer ranks first")
	}
	if RewritingCostModelLess(unrewritten, canonical) {
		t.Fatal("RewritingCostModelLess(unrewritten, canonical) = true; the comparator is not antisymmetric")
	}
}

// TestRewritingBoundary_KeepsCanonicalOuterJoin: after the REWRITING prune the
// promoted PLANNING seed is the canonical form, with the outer join carried by
// a null-on-empty quantifier.
func TestRewritingBoundary_KeepsCanonicalOuterJoin(t *testing.T) {
	t.Parallel()

	rootRef := expressions.InitialOf(buildCorrelatedLeftOuterSelect())
	rules := append(DefaultExpressionRules(), RewritingRules()...)
	p := NewPlanner(rules, nil)
	if _, converged := exploreRewriting(p, rootRef); !converged {
		t.Fatalf("REWRITING phase did not converge (MaxTasks hit)")
	}
	rootRef.AdvancePlannerStage(expressions.StagePlanned)

	members := rootRef.Members()
	if len(members) == 0 {
		t.Fatalf("root reference has no promoted PLANNING-seed members after the boundary")
	}
	for _, m := range members {
		if isOuterJoinSelectForTest(m) {
			t.Fatalf("the LEFT OUTER select survived the REWRITING prune: %v", members)
		}
		sel, ok := m.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("promoted member %T, want the canonical SelectExpression", m)
		}
		noe := 0
		for _, q := range sel.GetQuantifiers() {
			if q.IsNullOnEmpty() {
				noe++
			}
		}
		if noe != 1 {
			t.Fatalf("promoted select carries %d null-on-empty quantifiers, want the canonical one", noe)
		}
	}
}
