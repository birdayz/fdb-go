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
// Wraps the inner in FirstOrDefault and uses a semi-join (EXISTS
// or NOT EXISTS) plan shape. Non-EXISTS predicates (e.g. `x > 5`
// in `WHERE x > 5 AND EXISTS (...)`) are applied as a separate
// PredicatesFilterPlan on the outer before the semi-join.
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
	// or null.
	fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(innerPlan, values.NewNullValue(values.UnknownType))
	fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan,
		expressions.NamedPhysicalQuantifier(quants[1].GetAlias(), call.MemoizeExpression(innerExpr)))
	fodRef := call.MemoizeExpression(fodWrapper)

	// Separate predicates into EXISTS-related and non-EXISTS.
	// Non-EXISTS predicates are applied as a filter on the outer;
	// the EXISTS/NOT-EXISTS predicate drives the join type.
	allPreds := sel.GetPredicates()
	var regularPreds []predicates.QueryPredicate
	negated := false
	for _, p := range flattenAndPredicates(allPreds) {
		if _, ok := p.(*predicates.ExistsPredicate); ok {
			continue
		}
		if not, ok := p.(*predicates.NotPredicate); ok {
			ch := not.Children()
			if len(ch) == 1 {
				if _, ok := ch[0].(*predicates.ExistsPredicate); ok {
					negated = true
					continue
				}
			}
		}
		regularPreds = append(regularPreds, p)
	}

	joinType := plans.JoinExists
	if negated {
		joinType = plans.JoinNotExists
	}

	outerMemoRef := call.MemoizeExpression(outerExpr)
	outerQuant := expressions.NamedPhysicalQuantifier(quants[0].GetAlias(), outerMemoRef)

	// Extract source aliases for datum qualification.
	aliases := sel.GetSourceAliases()
	var outerAlias, innerAlias string
	if len(aliases) >= 1 {
		outerAlias = aliases[0]
	}
	if len(aliases) >= 2 {
		innerAlias = aliases[1]
	}

	// When there are correlated join predicates (e.g. `sub.v = a.v`),
	// use the raw inner scan instead of the FOD wrapper. The NLJ
	// executor's with-predicates path collects all inner rows and
	// tests each outer+inner combination against the predicates; a
	// FOD limits the inner to one row, making correlation incomplete.
	// Don't pre-filter the outer either -- correlated predicates
	// reference inner columns that aren't in the outer row map.
	//
	// When there are NO correlated predicates (pure uncorrelated
	// EXISTS), the FOD handles the "any row?" semantics correctly.
	var nljInner plans.RecordQueryPlan
	if len(regularPreds) > 0 {
		nljInner = innerPlan
	} else {
		nljInner = fodPlan
	}

	joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		outerPlan, nljInner,
		regularPreds,
		joinType,
		outerAlias, innerAlias,
	)

	fodQuant := expressions.NewPhysicalQuantifier(fodRef)
	call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, outerQuant, fodQuant))
}

// flattenAndPredicates extracts individual predicates from an AND
// chain. If the list is a single AND predicate, returns its sub-
// predicates. Otherwise returns the list as-is.
func flattenAndPredicates(preds []predicates.QueryPredicate) []predicates.QueryPredicate {
	if len(preds) == 1 {
		if and, ok := preds[0].(*predicates.AndPredicate); ok {
			return and.SubPredicates
		}
	}
	return preds
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
