package cascades

import (
	"strings"

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

	// 3 quantifiers: 2 ForEach + 1 Existential = join with EXISTS filter.
	// Build the inner join first, then wrap with the EXISTS semi-join.
	if len(quants) == 3 &&
		quants[0].Kind() == expressions.QuantifierForEach &&
		quants[1].Kind() == expressions.QuantifierForEach &&
		quants[2].Kind() == expressions.QuantifierExistential {
		r.implementJoinWithExistential(call, sel, quants)
		return
	}

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
	// When the SelectExpression was created via WithSwappedQuantifiers
	// (ChildrenAsSet permutation), mark the plan so column derivation
	// can restore the original SQL FROM-clause column ordering.
	if sel.IsQuantifiersSwapped() {
		joinPlan.SetSQLColumnOrderReversed(true)
	}

	leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
	rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
	call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, leftQ, rightQ))
}

// implementExistentialSelect handles a SelectExpression with a
// ForEach outer and an Existential inner (EXISTS subquery).
// Wraps the inner in FirstOrDefault and uses a semi-join (EXISTS
// or NOT EXISTS) plan shape. Non-EXISTS predicates (e.g. `x > 5`
// in `WHERE x > 5 AND EXISTS (...)`) are passed as NLJ join
// predicates evaluated against the merged outer+inner row.
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
	// Non-EXISTS predicates become NLJ join predicates evaluated
	// against the merged outer+inner row; EXISTS/NOT-EXISTS drives
	// the join type.
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

// implementJoinWithExistential handles a flat SelectExpression with
// ForEach(left), ForEach(right), Existential(exists_scan). This shape
// comes from a cross-join + WHERE EXISTS filter. The method builds a
// two-level NLJ: an inner join for left × right, then an outer EXISTS
// semi-join wrapping the join result with the existential inner.
func (r *ImplementNestedLoopJoinRule) implementJoinWithExistential(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	quants []expressions.Quantifier,
) {
	leftRef := quants[0].GetRangesOver()
	rightRef := quants[1].GetRangesOver()
	existRef := quants[2].GetRangesOver()
	if leftRef == nil || rightRef == nil || existRef == nil {
		return
	}

	leftPlan := findPhysicalPlan(leftRef)
	rightPlan := findPhysicalPlan(rightRef)
	existPlan := findPhysicalPlan(existRef)
	if leftPlan == nil || rightPlan == nil || existPlan == nil {
		return
	}

	leftExpr := findPhysicalExpr(leftRef)
	rightExpr := findPhysicalExpr(rightRef)
	existExpr := findPhysicalExpr(existRef)
	if leftExpr == nil || rightExpr == nil || existExpr == nil {
		return
	}

	aliases := sel.GetSourceAliases()
	var leftAlias, rightAlias, existAlias string
	if len(aliases) >= 1 {
		leftAlias = aliases[0]
	}
	if len(aliases) >= 2 {
		rightAlias = aliases[1]
	}
	if len(aliases) >= 3 {
		existAlias = aliases[2]
	}

	// Split predicates into join predicates (for the inner NLJ) and
	// EXISTS-related predicates (for the outer NLJ). EXISTS predicates
	// reference the existential alias and belong on the outer level.
	allPreds := flattenAndPredicates(sel.GetPredicates())
	var joinPreds, existPreds []predicates.QueryPredicate
	negated := false
	for _, p := range allPreds {
		if _, ok := p.(*predicates.ExistsPredicate); ok {
			// Pure EXISTS predicate — belongs on the outer level.
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
		// Heuristic: predicates with field references from the existential
		// source belong on the outer (EXISTS) level. All others are join
		// predicates. This is a simplification — a full implementation
		// would check which quantifiers each predicate references.
		if predicateReferencesAlias(p, existAlias) {
			existPreds = append(existPreds, p)
		} else {
			joinPreds = append(joinPreds, p)
		}
	}

	// Map join type.
	var joinType plans.JoinType
	switch sel.GetJoinType() {
	case expressions.JoinLeftOuter:
		joinType = plans.JoinLeftOuter
	default:
		joinType = plans.JoinInner
	}

	// Step 1: build inner join (left × right).
	innerJoinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		leftPlan, rightPlan,
		joinPreds,
		joinType,
		leftAlias, rightAlias,
	)

	// Step 2: build EXISTS semi-join on top.
	existJoinType := plans.JoinExists
	if negated {
		existJoinType = plans.JoinNotExists
	}

	// Use raw existPlan when there are correlated predicates; FOD when
	// uncorrelated (same logic as implementExistentialSelect).
	var nljExistInner plans.RecordQueryPlan
	if len(existPreds) > 0 {
		nljExistInner = existPlan
	} else {
		nljExistInner = plans.NewRecordQueryFirstOrDefaultPlan(
			existPlan, values.NewNullValue(values.UnknownType))
	}

	outerJoinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		innerJoinPlan, nljExistInner,
		existPreds,
		existJoinType,
		"", existAlias,
	)

	// Build quantifiers for the physical wrapper. The wrapper needs
	// exactly 2 quantifiers. Use left + right as a representative
	// pair for the memoized expression structure.
	leftMemoRef := call.MemoizeExpression(leftExpr)
	rightMemoRef := call.MemoizeExpression(rightExpr)

	// The outerJoinPlan is the full physical plan (inner join + EXISTS
	// semi-join). The wrapper quantifiers are for Cascades bookkeeping.
	call.Yield(newPhysicalNestedLoopJoinWrapper(
		outerJoinPlan,
		expressions.ForEachQuantifier(leftMemoRef),
		expressions.ForEachQuantifier(rightMemoRef),
	))
}

// predicateReferencesAlias checks whether a predicate tree contains
// a FieldValue whose field name starts with the given alias prefix
// (case-insensitive). Used to classify predicates as belonging to the
// join level or the EXISTS level.
//
// Uses walkPredicateFieldValues (shared with PushFilterBelowJoinRule)
// to recursively visit ALL FieldValues in the predicate's value trees,
// regardless of nesting depth or value type.
func predicateReferencesAlias(p predicates.QueryPredicate, alias string) bool {
	if alias == "" {
		return false
	}
	prefix := strings.ToUpper(alias) + "."
	found := false
	walkPredicateFieldValues(p, func(fv *values.FieldValue) {
		if strings.HasPrefix(strings.ToUpper(fv.Field), prefix) {
			found = true
		}
	})
	return found
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
