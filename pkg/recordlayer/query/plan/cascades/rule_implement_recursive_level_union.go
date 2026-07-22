package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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

	initialWinner, _ := getWinnerForOrdering(initialRef, properties.PreserveOrdering(), call.CostModel())
	recursiveWinner, _ := getWinnerForOrdering(recursiveRef, properties.PreserveOrdering(), call.CostModel())
	if initialWinner == nil || recursiveWinner == nil {
		return
	}
	if _, ok := initialWinner.(physicalPlanExpression); !ok {
		return
	}
	if _, ok := recursiveWinner.(physicalPlanExpression); !ok {
		return
	}

	// The plan carries its two leg edges directly — one live quantifier per
	// winner, no separate physical wrapper (RFC-184 W2).
	initQ := expressions.ForEachQuantifier(call.MemoizeExpression(initialWinner))
	recQ := expressions.ForEachQuantifier(call.MemoizeExpression(recursiveWinner))
	call.Yield(plans.NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
		initQ, recQ,
		recUnion.GetTempTableScanAlias(),
		recUnion.GetTempTableInsertAlias(),
		recUnion.IsDistinct(),
	))
}

var _ ExpressionRule = (*ImplementRecursiveLevelUnionRule)(nil)
