package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

	// EXISTS subquery: when the right quantifier is existential, wrap
	// the inner in FirstOrDefault and use a semi-join (EXISTS) plan
	// shape. The ExistsPredicate in the predicate list evaluates to
	// TRUE when FirstOrDefault returns a non-null row.
	if quants[1].Kind() == expressions.QuantifierExistential {
		r.implementExistentialSelect(call, sel, quants)
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

	// Map the expression-level JoinType to the plans-level JoinType.
	// The expressions package defines its own JoinType to avoid a
	// circular dependency (plans imports expressions).
	var joinType plans.JoinType
	switch sel.GetJoinType() {
	case expressions.JoinLeftOuter:
		joinType = plans.JoinLeftOuter
	case expressions.JoinCross:
		joinType = plans.JoinCross
	default:
		joinType = plans.JoinInner
	}

	joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		leftPlan, rightPlan,
		sel.GetPredicates(),
		joinType,
		leftAlias, rightAlias,
	)

	leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
	rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
	call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, leftQ, rightQ))
}

// implementExistentialSelect handles a SelectExpression with a
// ForEach outer and an Existential inner (EXISTS subquery).
// Wraps the inner in FirstOrDefault and applies predicates as a
// filter on the outer. The ExistsPredicate evaluates against the
// FirstOrDefault binding.
func (r *ImplementNestedLoopJoinRule) implementExistentialSelect(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	quants []expressions.Quantifier,
) {
	outerRef := quants[0].GetRangesOver()
	innerRef := quants[1].GetRangesOver()
	if outerRef == nil || innerRef == nil {
		return
	}

	outerPlan := findPhysicalPlan(outerRef)
	innerPlan := findPhysicalPlan(innerRef)
	if outerPlan == nil || innerPlan == nil {
		return
	}

	outerExpr := findPhysicalExpr(outerRef)
	innerExpr := findPhysicalExpr(innerRef)
	if outerExpr == nil || innerExpr == nil {
		return
	}

	// Wrap the existential inner in FirstOrDefault — returns one row
	// or null. The ExistsPredicate.Eval checks whether this binding
	// is non-null.
	fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(innerPlan, values.NewNullValue(values.UnknownType))
	fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan,
		expressions.NamedPhysicalQuantifier(quants[1].GetAlias(), call.MemoizeExpression(innerExpr)))
	fodRef := call.MemoizeExpression(fodWrapper)

	// Now we have: outer plan + FOD plan. Apply the predicates
	// (including ExistsPredicate) as a filter on the outer.
	outerMemoRef := call.MemoizeExpression(outerExpr)
	outerQuant := expressions.NamedPhysicalQuantifier(quants[0].GetAlias(), outerMemoRef)

	// Detect NOT EXISTS: a single NOT(ExistsPredicate) wrapping.
	joinType := plans.JoinExists
	if isNotExistsPredicate(sel.GetPredicates()) {
		joinType = plans.JoinNotExists
	}

	// Build a NLJ-style plan: outer × FOD, with predicates.
	joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		outerPlan, fodPlan,
		sel.GetPredicates(),
		joinType,
		"", "",
	)

	fodQuant := expressions.NewPhysicalQuantifier(fodRef)
	call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, outerQuant, fodQuant))
}

// isNotExistsPredicate checks whether the predicate list is a single
// NOT(ExistsPredicate) — i.e. a NOT EXISTS shape.
func isNotExistsPredicate(preds []predicates.QueryPredicate) bool {
	if len(preds) != 1 {
		return false
	}
	not, ok := preds[0].(*predicates.NotPredicate)
	if !ok {
		return false
	}
	children := not.Children()
	if len(children) != 1 {
		return false
	}
	_, isExists := children[0].(*predicates.ExistsPredicate)
	return isExists
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
