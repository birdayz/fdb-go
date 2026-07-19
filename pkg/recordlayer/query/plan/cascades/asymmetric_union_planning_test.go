package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A union whose two legs implement to physical plans via DIFFERENT rewrite
// paths ("asymmetric" legs) once regressed to a no-plan result: the leg
// reached through a merged/unfinalized group crossed the REWRITING→PLANNING
// boundary via the no-finals path, which changed the stage but did NOT reset
// the per-stage exploration state. The leg kept its REWRITING "explorationDone"
// stamp, never re-explored in PLANNING, never fired its implement rules, and
// so had no physical member — leaving the union with fewer than two physical
// children and no winner, even though Plan() reported success (RFC-182,
// FuzzPlanner_PlanFullPipeline "root has no BestMember" seed).
//
// The fix (AdvanceStagePreservingMembers) resets exploration bookkeeping on
// that boundary path so the surviving logical members implement in PLANNING.
// These tests pin that every reachable union leg acquires a physical plan
// and the root wins.

func scanExpr() expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
}

func typeFilter(inner expressions.RelationalExpression) expressions.RelationalExpression {
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewLogicalTypeFilterExpression([]string{"X"}, q)
}

func trueFilter(inner expressions.RelationalExpression) expressions.RelationalExpression {
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
}

func union(legs ...expressions.RelationalExpression) expressions.RelationalExpression {
	qs := make([]expressions.Quantifier, len(legs))
	for i, l := range legs {
		qs[i] = expressions.ForEachQuantifier(expressions.InitialOf(l))
	}
	return expressions.NewLogicalUnionExpression(qs)
}

func planAndAssertWinner(t *testing.T, root expressions.RelationalExpression) {
	t.Helper()
	ref := expressions.InitialOf(root)
	p := NewPlanner(DefaultExpressionRules(), nil).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	p.MaxTasks = 100_000

	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil plan")
	}
	// The invariant FuzzPlanner_PlanFullPipeline asserts: a successful Plan
	// must leave the root with a stamped winner.
	if !p.HasBestMember(ref) {
		t.Fatal("Plan succeeded but root Reference has no BestMember stamp")
	}
}

func TestAsymmetricUnion_BothLegsPlan(t *testing.T) {
	t.Parallel()
	// Union(TypeFilter(TypeFilter(Scan)), TypeFilter(Filter(Scan))) — the
	// legs reach a physical form through different rewrites; a constant-true
	// filter elimination merges the right leg's group with the left leg's
	// inner, and that merged group historically failed to implement.
	root := union(
		typeFilter(typeFilter(scanExpr())),
		typeFilter(trueFilter(scanExpr())),
	)
	planAndAssertWinner(t, root)
}

func TestAsymmetricUnion_SymmetricStillPlans(t *testing.T) {
	t.Parallel()
	root := union(
		typeFilter(typeFilter(scanExpr())),
		typeFilter(typeFilter(scanExpr())),
	)
	planAndAssertWinner(t, root)
}

func TestAsymmetricUnion_FilterLegStandalonePlans(t *testing.T) {
	t.Parallel()
	// The right leg alone must plan (it always did — the regression was only
	// in the union context), a control for the two above.
	planAndAssertWinner(t, typeFilter(trueFilter(scanExpr())))
}
