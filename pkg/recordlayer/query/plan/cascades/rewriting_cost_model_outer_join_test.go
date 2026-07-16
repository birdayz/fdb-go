package cascades

// F26 — REWRITING-phase cost model and the outer-join prune survivor.
//
// Java's RewritingCostModel.compare() penalizes any surviving OuterJoinExpression
// FIRST (ExpressionCountProperty.outerJoinCount), ahead of selectCount. That is a
// CORRECTNESS GUARD unique to Java: Java's OuterJoinExpression is a logical-only
// node with NO physical operator, so the single-final-expression prune MUST keep
// the implementable rewritten form or the query fails to plan.
//
// Go's outer join is DIFFERENT — it is a SelectExpression that is directly
// implementable as a materialized RecordQueryNestedLoopJoinPlan (RFC-152). Go
// therefore DELIBERATELY keeps the un-rewritten outer-join select as the prune
// survivor (it wins on selectCount, 1<2) so PLANNING can derive both the
// materialized NLJ and the correlated FlatMap and cost-choose. Porting Java's
// outerJoinCount would force the rewritten form to win the prune and SUPPRESS the
// materialized NLJ (a regression — TestFDB_ArrayUnnestOrdinality).
//
// These tests are the sentinel: they pin that Go keeps the un-rewritten form and
// go RED if outerJoinCount is (re-)introduced into RewritingCostModelLess.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// isOuterJoinSelectForTest reports whether e is a SelectExpression carrying LEFT or
// FULL OUTER semantics — Go's encoding of what Java models as a distinct
// OuterJoinExpression. Local to the test: production has no such helper because
// (unlike Java) Go's cost model must NOT count outer joins (see RewritingCostModelLess).
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

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	qA := expressions.NamedForEachQuantifier(aliasA, expressions.InitialOf(scanA))
	qB := expressions.NamedForEachQuantifier(aliasB, expressions.InitialOf(scanB))

	flagField := values.NewFieldValue(values.NewQuantifiedObjectValue(aliasA), "flag", values.UnknownType)
	pred := predicates.NewComparisonPredicate(flagField, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))

	return expressions.NewSelectExpressionWithJoinType(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{pred},
		[]string{"A", "B"},
		expressions.JoinLeftOuter,
	)
}

// deriveCanonicalRewrite runs RewriteOuterJoinRule and returns the canonical INNER
// rewritten SelectExpression (2 inner selects, 0 outer-join selects).
func deriveCanonicalRewrite(t *testing.T, unrewritten *expressions.SelectExpression) *expressions.SelectExpression {
	t.Helper()
	yielded := FireExpressionRule(NewRewriteOuterJoinRule(), expressions.InitialOf(unrewritten))
	for _, e := range yielded {
		if s, ok := e.(*expressions.SelectExpression); ok && s.GetJoinType() == expressions.JoinInner {
			return s
		}
	}
	t.Fatalf("RewriteOuterJoinRule yielded no canonical INNER SelectExpression (got %d expressions)", len(yielded))
	return nil
}

// TestRewritingCostModel_KeepsUnrewrittenOuterJoin pins that Go's RewritingCostModel
// PREFERS the un-rewritten LEFT-OUTER select (1 select, 1 outer-join select) over the
// canonical rewritten form (2 selects, 0 outer-join selects) — the OPPOSITE of Java.
// The un-rewritten form wins on selectCount (1<2), so it survives the REWRITING prune
// and PLANNING can still derive the materialized RecordQueryNestedLoopJoinPlan
// (RFC-152). Introducing Java's outerJoinCount criterion (which prefers the 0-outer
// form) flips this and this test goes RED.
func TestRewritingCostModel_KeepsUnrewrittenOuterJoin(t *testing.T) {
	t.Parallel()

	unrewritten := buildCorrelatedLeftOuterSelect()
	canonical := deriveCanonicalRewrite(t, unrewritten)

	// The counts that matter. The un-rewritten form carries the outer join; the
	// rewritten form has more selects but no outer join.
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

	// Go keeps the un-rewritten form: it is strictly preferred (fewer selects).
	if !RewritingCostModelLess(unrewritten, canonical) {
		t.Fatalf("RewritingCostModelLess(unrewritten, canonical) = false; want true — Go must keep the " +
			"un-rewritten outer-join select as the prune survivor so the materialized NLJ (RFC-152) stays " +
			"reachable. If this flipped, Java's outerJoinCount criterion was (wrongly) ported.")
	}
	if RewritingCostModelLess(canonical, unrewritten) {
		t.Fatalf("RewritingCostModelLess(canonical, unrewritten) = true; want false (comparator not antisymmetric, " +
			"or outerJoinCount was ported and now prefers the rewritten form)")
	}
}

// TestRewritingBoundary_KeepsUnrewrittenOuterJoin pins the phase boundary end-to-end:
// after the REWRITING phase prunes and the planner stage advances (promoting final
// members as the PLANNING seed), the promoted seed STILL contains the un-rewritten
// LEFT-OUTER select. That is what lets ImplementNestedLoopJoinRule produce the
// materialized RecordQueryNestedLoopJoinPlan in PLANNING. Porting outerJoinCount would
// discard the outer-join select at the prune and this assertion fails.
func TestRewritingBoundary_KeepsUnrewrittenOuterJoin(t *testing.T) {
	t.Parallel()

	rootRef := expressions.InitialOf(buildCorrelatedLeftOuterSelect())

	// Production REWRITING rule set (DefaultExpressionRules + RewritingRules, which
	// contains RewriteOuterJoinRule). exploreRewriting drives the real unified task
	// stack through REWRITING only, including the OptimizeGroup prune.
	rules := append(DefaultExpressionRules(), RewritingRules()...)
	p := NewPlanner(rules, nil)
	if _, converged := exploreRewriting(p, rootRef); !converged {
		t.Fatalf("REWRITING phase did not converge (MaxTasks hit)")
	}

	// Promote the pruned final members as the PLANNING seed — exactly what
	// ExploreGroup(PhasePlanning) does at the phase boundary via AdvancePlannerStage.
	rootRef.AdvancePlannerStage(expressions.StagePlanned)

	members := rootRef.Members()
	if len(members) == 0 {
		t.Fatalf("root reference has no promoted PLANNING-seed members after the boundary")
	}
	keptOuter := false
	for _, m := range members {
		if isOuterJoinSelectForTest(m) {
			keptOuter = true
		}
	}
	if !keptOuter {
		t.Fatalf("no promoted member is an outer-join SelectExpression — the un-rewritten LEFT-OUTER form was "+
			"discarded at the REWRITING prune, which suppresses the materialized NLJ (RFC-152). Promoted members: %v",
			members)
	}
}
