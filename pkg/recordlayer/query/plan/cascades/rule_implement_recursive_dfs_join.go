package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	initialWinner := getWinnerForOrdering(initialRef, PreserveOrdering())
	recursiveWinner := getWinnerForOrdering(recursiveRef, PreserveOrdering())
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

	var plan *plans.RecordQueryRecursiveDfsJoinPlan
	if recUnion.IsDistinct() {
		plan = plans.NewRecordQueryRecursiveDfsJoinPlanDistinct(
			initPh.GetRecordQueryPlan(), recPh.GetRecordQueryPlan(),
			priorCorrelation, strategy,
		)
	} else {
		plan = plans.NewRecordQueryRecursiveDfsJoinPlan(
			initPh.GetRecordQueryPlan(), recPh.GetRecordQueryPlan(),
			priorCorrelation, strategy,
		)
	}

	rootQ := expressions.ForEachQuantifier(call.MemoizeExpression(initialWinner))
	childQ := expressions.ForEachQuantifier(call.MemoizeExpression(recursiveWinner))
	call.Yield(newPhysicalRecursiveDfsJoinWrapper(plan, rootQ, childQ))
}

var _ ExpressionRule = (*ImplementRecursiveDfsJoinRule)(nil)
