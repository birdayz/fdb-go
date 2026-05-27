package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementRecursiveLevelUnionRule converts a RecursiveUnionExpression
// (where level-order traversal is allowed) into a physical
// RecordQueryRecursiveLevelUnionPlan.
//
// Pattern:
//
//	RecursiveUnion(initial_state, recursive_state)
//	  where levelAllowed()
//	  → RecursiveLevelUnion(physical(initial), physical(recursive), scanAlias, insertAlias)
//
// Both legs must already have physical plans available (yielded by
// prior TempTableInsert → inner plan implement rules).
//
// Mirrors Java's ImplementRecursiveLevelUnionRule.
type ImplementRecursiveLevelUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementRecursiveLevelUnionRule() *ImplementRecursiveLevelUnionRule {
	return &ImplementRecursiveLevelUnionRule{
		matcher: NewExpressionMatcher[*expressions.RecursiveUnionExpression]("recursive_union_level"),
	}
}

func (r *ImplementRecursiveLevelUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementRecursiveLevelUnionRule) OnMatch(call *ExpressionRuleCall) {
	recUnion := matching.Get[*expressions.RecursiveUnionExpression](call.Bindings, r.matcher)

	if !recUnion.LevelAllowed() {
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

	var plan *plans.RecordQueryRecursiveLevelUnionPlan
	if recUnion.IsDistinct() {
		plan = plans.NewRecordQueryRecursiveLevelUnionPlanDistinct(
			initPh.GetRecordQueryPlan(), recPh.GetRecordQueryPlan(),
			recUnion.GetTempTableScanAlias(),
			recUnion.GetTempTableInsertAlias(),
		)
	} else {
		plan = plans.NewRecordQueryRecursiveLevelUnionPlan(
			initPh.GetRecordQueryPlan(), recPh.GetRecordQueryPlan(),
			recUnion.GetTempTableScanAlias(),
			recUnion.GetTempTableInsertAlias(),
		)
	}

	initQ := expressions.ForEachQuantifier(call.MemoizeExpression(initialWinner))
	recQ := expressions.ForEachQuantifier(call.MemoizeExpression(recursiveWinner))
	call.Yield(newPhysicalRecursiveLevelUnionWrapper(plan, initQ, recQ))
}

var _ ExpressionRule = (*ImplementRecursiveLevelUnionRule)(nil)
