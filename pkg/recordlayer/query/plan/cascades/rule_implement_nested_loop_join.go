package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementNestedLoopJoinRule implements a SelectExpression with
// exactly 2 quantifiers (a binary join) as a physical nested-loop join
// plan. The left (first) quantifier becomes the outer and the right
// (second) becomes the inner.
//
//	Select(predicates, [Q_left, Q_right])
//	  → NestedLoopJoin(outer=physical(Q_left), inner=physical(Q_right), predicates)
//
// This is the simplest and most general join implementation — it works
// for all join shapes without requiring sorted input or hash tables.
// Cost model: O(N_outer × N_inner) with predicate filtering.
//
// Mirrors Java's `ImplementNestedLoopJoinRule`.
type ImplementNestedLoopJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementNestedLoopJoinRule() *ImplementNestedLoopJoinRule {
	return &ImplementNestedLoopJoinRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("select_for_nlj"),
	}
}

func (r *ImplementNestedLoopJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementNestedLoopJoinRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

	quants := sel.GetQuantifiers()
	if len(quants) != 2 {
		return
	}

	leftRef := quants[0].GetRangesOver()
	rightRef := quants[1].GetRangesOver()
	if leftRef == nil || rightRef == nil {
		return
	}

	leftPlan := findPhysicalPlan(leftRef)
	rightPlan := findPhysicalPlan(rightRef)
	if leftPlan == nil || rightPlan == nil {
		return
	}

	leftExpr := findPhysicalExpr(leftRef)
	rightExpr := findPhysicalExpr(rightRef)
	if leftExpr == nil || rightExpr == nil {
		return
	}

	aliases := sel.GetSourceAliases()
	var leftAlias, rightAlias string
	if len(aliases) >= 2 {
		leftAlias = aliases[0]
		rightAlias = aliases[1]
	}

	joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		leftPlan, rightPlan,
		sel.GetPredicates(),
		plans.JoinInner,
		leftAlias, rightAlias,
	)

	leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
	rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
	call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, leftQ, rightQ))
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
