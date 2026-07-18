package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementRecursiveDfsJoinRule converts a RecursiveUnionExpression
// (where DFS traversal is allowed) into a physical
// RecordQueryRecursiveDfsJoinPlan.
//
// Pattern:
//
//	RecursiveUnion(initial_state, recursive_state)
//	  where dfsAllowed()
//	  → RecursiveDfsJoin(physical(initial), physical(recursive), priorCorrelation, strategy)
//
// The initial-state leg must already have a physical plan (yielded by
// prior TempTableInsert → inner plan implement rules). The recursive
// leg similarly must have a physical plan available.
//
// Mirrors Java's ImplementRecursiveDfsJoinRule.
type ImplementRecursiveDfsJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementRecursiveDfsJoinRule() *ImplementRecursiveDfsJoinRule {
	return &ImplementRecursiveDfsJoinRule{
		matcher: NewExpressionMatcher[*expressions.RecursiveUnionExpression]("recursive_union_dfs"),
	}
}

func (r *ImplementRecursiveDfsJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementRecursiveDfsJoinRule) OnMatch(call *ExpressionRuleCall) {
	recUnion := matching.Get[*expressions.RecursiveUnionExpression](call.Bindings, r.matcher)

	if !recUnion.DfsAllowed() {
		return
	}

	initialRef := recUnion.GetInitialState().GetRangesOver()
	recursiveRef := recUnion.GetRecursiveState().GetRangesOver()
	if initialRef == nil || recursiveRef == nil {
		return
	}

	initialWinner := getWinnerForOrdering(initialRef, PreserveOrdering(), call.CostModel())
	recursiveWinner := getWinnerForOrdering(recursiveRef, PreserveOrdering(), call.CostModel())
	if initialWinner == nil || recursiveWinner == nil {
		return
	}
	initPh, ok := initialWinner.(physicalPlanExpression)
	if !ok {
		return
	}
	recPh, ok := recursiveWinner.(physicalPlanExpression)
	if !ok {
		return
	}

	strategy := plans.DfsPreorder
	if !recUnion.PreOrderAllowed() && recUnion.PostOrderAllowed() {
		strategy = plans.DfsPostorder
	}

	// The prior-value correlation is the temp table scan alias: the
	// recursive leg reads from the temp table that the prior iteration
	// populated.
	priorCorrelation := recUnion.GetTempTableScanAlias()

	// Java's rule (ImplementRecursiveDfsJoinRule) matches
	// tempTableInsertPlanOverQuantifier and builds the DFS plan from the
	// plans UNDER the inserts: the DFS traversal binds the prior row per
	// level (no ping-pong tables), so the level-union's insert tops are
	// dead plumbing here — and under the streaming RecursiveCursor a
	// per-level TempTableInsertCursor continuation would snapshot the
	// accumulator table into EVERY level of the DFS continuation.
	rootPlan := stripTempTableInsertTop(initPh.GetRecordQueryPlan())
	childPlan := stripTempTableInsertTop(recPh.GetRecordQueryPlan())

	var plan *plans.RecordQueryRecursiveDfsJoinPlan
	if recUnion.IsDistinct() {
		plan = plans.NewRecordQueryRecursiveDfsJoinPlanDistinct(
			rootPlan, childPlan,
			priorCorrelation, strategy,
		)
	} else {
		plan = plans.NewRecordQueryRecursiveDfsJoinPlan(
			rootPlan, childPlan,
			priorCorrelation, strategy,
		)
	}

	rootQ := expressions.ForEachQuantifier(call.MemoizeExpression(initialWinner))
	childQ := expressions.ForEachQuantifier(call.MemoizeExpression(recursiveWinner))
	call.Yield(newPhysicalRecursiveDfsJoinWrapper(plan, rootQ, childQ))
}

// stripTempTableInsertTop unwraps a TempTableInsert at the top of a leg plan
// (Java's initialInnerPlanMatcher / recursive TempTableInsertExpression
// matcher shapes take the plan UNDER the insert). Match-surface divergence:
// Java's matcher REQUIRES the insert top (the rule does not fire without
// it), while this strip tolerates its absence — benign because the front
// end always builds recursive legs insert-topped, and a hypothetical bare
// leg would plan identically rather than silently mis-fire.
func stripTempTableInsertTop(p plans.RecordQueryPlan) plans.RecordQueryPlan {
	if ins, ok := p.(*plans.RecordQueryTempTableInsertPlan); ok {
		return ins.GetInner()
	}
	return p
}

var _ ExpressionRule = (*ImplementRecursiveDfsJoinRule)(nil)
