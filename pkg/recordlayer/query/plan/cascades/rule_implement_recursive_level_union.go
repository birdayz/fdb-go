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

	initialPlan := findPhysicalPlan(initialRef)
	recursivePlan := findPhysicalPlan(recursiveRef)
	if initialPlan == nil || recursivePlan == nil {
		return
	}

	initialExpr := findPhysicalExpr(initialRef)
	recursiveExpr := findPhysicalExpr(recursiveRef)
	if initialExpr == nil || recursiveExpr == nil {
		return
	}

	plan := plans.NewRecordQueryRecursiveLevelUnionPlan(
		initialPlan, recursivePlan,
		recUnion.GetTempTableScanAlias(),
		recUnion.GetTempTableInsertAlias(),
	)

	initQ := expressions.ForEachQuantifier(call.MemoizeExpression(initialExpr))
	recQ := expressions.ForEachQuantifier(call.MemoizeExpression(recursiveExpr))
	call.Yield(newPhysicalRecursiveLevelUnionWrapper(plan, initQ, recQ))
}

var _ ExpressionRule = (*ImplementRecursiveLevelUnionRule)(nil)
