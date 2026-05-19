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

	if getExplodeExpression(leftRef) != nil || getExplodeExpression(rightRef) != nil {
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

	// Try FlatMap path: if the inner side is a full scan and we can
	// push an equi-join predicate into a correlated PK scan, emit a
	// FlatMap plan (Java's RecordQueryFlatMapPlan). This turns O(N×M)
	// into O(N×logM) via correlated index probes.
	// Skip when quantifiers are swapped — alias/plan mapping would be
	// inconsistent. The non-swapped version will also be explored.
	if !sel.IsQuantifiersSwapped() {
		if r.tryFlatMapPlan(call, sel, leftPlan, rightPlan, leftAlias, rightAlias, leftExpr, rightExpr, joinType) {
			return
		}
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

	// Try FlatMap for correlated EXISTS: if a correlated predicate
	// matches the inner table's PK or index, use a correlated scan.
	// Residual predicates are stripped of inner alias prefix and
	// wrapped inside the inner plan as a filter.
	if len(regularPreds) > 0 && !sel.IsQuantifiersSwapped() {
		if r.tryExistsFlatMap(call, sel, outerPlan, innerPlan, outerAlias, innerAlias, outerExpr, innerExpr, joinType, regularPreds) {
			return
		}
	}

	// Fallback: NLJ with predicate filtering.
	// Split predicates: inner-referencing go to NLJ (evaluated against
	// merged outer+inner row), outer-only go as a filter on the outer
	// plan (they're WHERE predicates, not join predicates).
	var joinPreds []predicates.QueryPredicate
	var outerOnlyPreds []predicates.QueryPredicate
	for _, p := range regularPreds {
		if predicateReferencesAlias(p, innerAlias) {
			joinPreds = append(joinPreds, p)
		} else {
			outerOnlyPreds = append(outerOnlyPreds, p)
		}
	}

	var nljOuter plans.RecordQueryPlan = outerPlan
	if len(outerOnlyPreds) > 0 {
		outerPrefix := strings.ToUpper(outerAlias) + "."
		stripped := stripAliasFromPredicates(outerOnlyPreds, outerPrefix)
		nljOuter = plans.NewRecordQueryPredicatesFilterPlan(outerPlan, stripped)
	}

	var nljInner plans.RecordQueryPlan
	if len(joinPreds) > 0 {
		nljInner = innerPlan
	} else {
		nljInner = fodPlan
	}

	joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		nljOuter, nljInner,
		joinPreds,
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
	upperAlias := strings.ToUpper(alias)
	prefix := upperAlias + "."
	found := false
	walkPredicateFieldValues(p, func(fv *values.FieldValue) {
		if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
			if strings.ToUpper(qov.Correlation.String()) == upperAlias {
				found = true
			}
			return
		}
		if strings.HasPrefix(strings.ToUpper(fv.Field), prefix) {
			found = true
		}
	})
	return found
}

// tryFlatMapPlan checks whether the join can be implemented as a
// FlatMap with correlated inner PK scan. Returns true (and yields)
// if successful, false otherwise. Mirrors Java's pattern where
// RecordQueryFlatMapPlan re-executes the inner plan per outer row
// with correlation bindings that parameterize the inner scan range.
func (r *ImplementNestedLoopJoinRule) tryFlatMapPlan(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	leftPlan, rightPlan plans.RecordQueryPlan,
	leftAlias, rightAlias string,
	leftExpr, rightExpr expressions.RelationalExpression,
	joinType plans.JoinType,
) bool {
	// Only applies when the inner side is a full table scan.
	innerScan, ok := rightPlan.(*plans.RecordQueryScanPlan)
	if !ok {
		return false
	}

	// Need the inner table's PK columns to match against predicates.
	recordTypes := innerScan.GetRecordTypes()
	if len(recordTypes) != 1 {
		return false
	}

	pkCols := call.Context.GetPrimaryKeyColumns(recordTypes[0])
	if len(pkCols) == 0 {
		return false
	}

	// Find equality predicates matching leading PK columns. For composite
	// PKs like (customer_id, order_num), match as many leading columns as
	// have equality predicates. Unmatched trailing PK columns become
	// residual filters. This turns O(N×M) NLJ into O(N×logM) prefix scan.
	preds := flattenAndPredicates(sel.GetPredicates())
	innerPrefix := strings.ToUpper(rightAlias) + "."
	outerPrefix := strings.ToUpper(leftAlias) + "."
	outerCorrelation := values.NamedCorrelationIdentifier(leftAlias)

	var matchedRanges []*predicates.ComparisonRange
	matchedPreds := make(map[int]bool)
	for _, pkColRaw := range pkCols {
		pkCol := strings.ToUpper(pkColRaw)
		found := false
		for pi, pred := range preds {
			if matchedPreds[pi] {
				continue
			}
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal, _ := r.matchJoinPKPredicate(cp, outerPrefix, innerPrefix, pkCol)
			if outerVal == nil {
				continue
			}
			bareField := outerVal.Field
			if outerVal.Child == nil && strings.HasPrefix(strings.ToUpper(bareField), outerPrefix) {
				bareField = bareField[len(outerPrefix):]
			}
			correlatedOperand := values.NewFieldValue(
				values.NewQuantifiedObjectValue(outerCorrelation),
				bareField, outerVal.Typ,
			)
			correlatedComp := &predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: correlatedOperand,
			}
			cr := predicates.EmptyComparisonRange()
			mergeResult := cr.Merge(correlatedComp)
			if !mergeResult.Ok {
				continue
			}
			matchedRanges = append(matchedRanges, mergeResult.Range)
			matchedPreds[pi] = true
			found = true
			break
		}
		if !found {
			break
		}
	}

	if len(matchedRanges) > 0 {
		correlatedScan := innerScan.WithScanComparisons(matchedRanges)

		innerCorrelation := values.NamedCorrelationIdentifier(rightAlias)
		flatMapPlan := plans.NewRecordQueryFlatMapPlan(
			leftPlan, correlatedScan,
			outerCorrelation, innerCorrelation,
			sel.GetResultValue(), false,
		)
		switch joinType {
		case plans.JoinLeftOuter:
			flatMapPlan.SetLeftOuter(true)
		case plans.JoinExists:
			flatMapPlan.SetExists(true)
		case plans.JoinNotExists:
			flatMapPlan.SetNotExists(true)
		}

		var outerPreds, abovePreds []predicates.QueryPredicate
		for pi, p := range preds {
			if matchedPreds[pi] {
				continue
			}
			if predicateReferencesAlias(p, rightAlias) {
				abovePreds = append(abovePreds, p)
			} else {
				outerPreds = append(outerPreds, p)
			}
		}

		if len(outerPreds) > 0 {
			// Strip the outer alias prefix from predicates so they match
			// unqualified keys in the raw scan output.
			stripped := stripAliasFromPredicates(outerPreds, outerPrefix)
			outerWithFilter := plans.NewRecordQueryPredicatesFilterPlan(flatMapPlan.GetOuter(), stripped)
			flatMapPlan = plans.NewRecordQueryFlatMapPlan(
				outerWithFilter, flatMapPlan.GetInner(),
				flatMapPlan.GetOuterAlias(), flatMapPlan.GetInnerAlias(),
				flatMapPlan.GetResultValue(), flatMapPlan.InheritOuterRecordProperties(),
			)
			switch joinType {
			case plans.JoinLeftOuter:
				flatMapPlan.SetLeftOuter(true)
			case plans.JoinExists:
				flatMapPlan.SetExists(true)
			case plans.JoinNotExists:
				flatMapPlan.SetNotExists(true)
			}
		}

		var finalPlan plans.RecordQueryPlan = flatMapPlan
		if len(abovePreds) > 0 {
			finalPlan = plans.NewRecordQueryPredicatesFilterPlan(flatMapPlan, abovePreds)
		}

		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		call.Yield(newPhysicalFlatMapWrapper(finalPlan, leftQ, rightQ))
		return true
	}

	// PK didn't match. Try secondary indexes: for each MatchCandidate
	// whose first column matches the predicate's inner column, create a
	// correlated INDEX scan.
	for _, cand := range call.Context.GetMatchCandidates() {
		candCols := cand.GetColumnNames()
		if len(candCols) == 0 {
			continue
		}
		candTypes := cand.GetRecordTypes()
		if len(candTypes) == 0 || candTypes[0] != recordTypes[0] {
			continue
		}
		idxFirstCol := strings.ToUpper(candCols[0])

		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal, _ := r.matchJoinPKPredicate(cp, outerPrefix, innerPrefix, idxFirstCol)
			if outerVal == nil {
				continue
			}

			outerCorrelation := values.NamedCorrelationIdentifier(leftAlias)
			bareField := outerVal.Field
			if outerVal.Child == nil && strings.HasPrefix(strings.ToUpper(bareField), outerPrefix) {
				bareField = bareField[len(outerPrefix):]
			}
			correlatedOperand := values.NewFieldValue(
				values.NewQuantifiedObjectValue(outerCorrelation),
				bareField, outerVal.Typ,
			)
			correlatedComp := &predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: correlatedOperand,
			}
			cr := predicates.EmptyComparisonRange()
			mergeResult := cr.Merge(correlatedComp)
			if !mergeResult.Ok {
				continue
			}

			correlatedIndexScan := plans.NewRecordQueryIndexPlan(
				cand.CandidateName(),
				[]*predicates.ComparisonRange{mergeResult.Range},
				recordTypes,
				innerScan.GetFlowedType(),
				false,
			)

			innerCorrelation := values.NamedCorrelationIdentifier(rightAlias)
			flatMapPlan := plans.NewRecordQueryFlatMapPlan(
				leftPlan, correlatedIndexScan,
				outerCorrelation, innerCorrelation,
				sel.GetResultValue(), false,
			)
			switch joinType {
			case plans.JoinLeftOuter:
				flatMapPlan.SetLeftOuter(true)
			case plans.JoinExists:
				flatMapPlan.SetExists(true)
			case plans.JoinNotExists:
				flatMapPlan.SetNotExists(true)
			}

			var residualPreds []predicates.QueryPredicate
			for _, p := range preds {
				if p != pred {
					residualPreds = append(residualPreds, p)
				}
			}
			var finalPlan plans.RecordQueryPlan = flatMapPlan
			if len(residualPreds) > 0 {
				finalPlan = plans.NewRecordQueryPredicatesFilterPlan(flatMapPlan, residualPreds)
			}

			leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
			rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
			call.Yield(newPhysicalFlatMapWrapper(finalPlan, leftQ, rightQ))
			return true
		}
	}

	return false
}

// tryExistsFlatMap is like tryFlatMapPlan but for EXISTS subqueries.
// The key difference: residual predicates wrap the INNER plan (filter
// inner rows before EXISTS check) rather than wrapping above the FlatMap.
func (r *ImplementNestedLoopJoinRule) tryExistsFlatMap(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	outerPlan, innerPlan plans.RecordQueryPlan,
	outerAlias, innerAlias string,
	outerExpr, innerExpr expressions.RelationalExpression,
	joinType plans.JoinType,
	preds []predicates.QueryPredicate,
) bool {
	innerScan, ok := innerPlan.(*plans.RecordQueryScanPlan)
	if !ok {
		return false
	}
	recordTypes := innerScan.GetRecordTypes()
	if len(recordTypes) != 1 {
		return false
	}

	innerPrefix := strings.ToUpper(innerAlias) + "."
	outerPrefix := strings.ToUpper(outerAlias) + "."

	// Try PK first.
	pkCols := call.Context.GetPrimaryKeyColumns(recordTypes[0])
	if len(pkCols) > 0 {
		pkCol := strings.ToUpper(pkCols[0])
		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal, _ := r.matchJoinPKPredicate(cp, outerPrefix, innerPrefix, pkCol)
			if outerVal == nil {
				continue
			}
			return r.buildExistsFlatMap(call, sel, outerPlan, innerScan, outerAlias, innerAlias, outerExpr, innerExpr, joinType, outerPrefix, innerPrefix, outerVal, pred, preds)
		}
	}

	// Try secondary indexes.
	for _, cand := range call.Context.GetMatchCandidates() {
		candCols := cand.GetColumnNames()
		if len(candCols) == 0 {
			continue
		}
		candTypes := cand.GetRecordTypes()
		if len(candTypes) == 0 || candTypes[0] != recordTypes[0] {
			continue
		}
		idxFirstCol := strings.ToUpper(candCols[0])
		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal, _ := r.matchJoinPKPredicate(cp, outerPrefix, innerPrefix, idxFirstCol)
			if outerVal == nil {
				continue
			}
			// Build correlated index scan.
			outerCorrelation := values.NamedCorrelationIdentifier(outerAlias)
			bareField := outerVal.Field
			if outerVal.Child == nil && strings.HasPrefix(strings.ToUpper(bareField), outerPrefix) {
				bareField = bareField[len(outerPrefix):]
			}
			correlatedOperand := values.NewFieldValue(
				values.NewQuantifiedObjectValue(outerCorrelation),
				bareField, outerVal.Typ,
			)
			correlatedComp := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: correlatedOperand}
			cr := predicates.EmptyComparisonRange()
			mergeResult := cr.Merge(correlatedComp)
			if !mergeResult.Ok {
				continue
			}
			correlatedIndexScan := plans.NewRecordQueryIndexPlan(
				cand.CandidateName(),
				[]*predicates.ComparisonRange{mergeResult.Range},
				recordTypes, innerScan.GetFlowedType(), false,
			)

			// Split residual predicates: inner-referencing → filter inside
			// inner plan. Outer-only → filter on the outer plan.
			var innerResiduals, outerResiduals []predicates.QueryPredicate
			for _, p := range preds {
				if p == pred {
					continue
				}
				if predicateReferencesAlias(p, innerAlias) {
					innerResiduals = append(innerResiduals, p)
				} else {
					outerResiduals = append(outerResiduals, p)
				}
			}

			var innerWithFilter plans.RecordQueryPlan = correlatedIndexScan
			if len(innerResiduals) > 0 {
				stripped := stripAliasFromPredicates(innerResiduals, innerPrefix)
				innerWithFilter = plans.NewRecordQueryPredicatesFilterPlan(correlatedIndexScan, stripped)
			}

			var outerWithFilter plans.RecordQueryPlan = outerPlan
			if len(outerResiduals) > 0 {
				stripped := stripAliasFromPredicates(outerResiduals, outerPrefix)
				outerWithFilter = plans.NewRecordQueryPredicatesFilterPlan(outerPlan, stripped)
			}

			innerCorrelation := values.NamedCorrelationIdentifier(innerAlias)
			flatMapPlan := plans.NewRecordQueryFlatMapPlan(
				outerWithFilter, innerWithFilter,
				outerCorrelation, innerCorrelation,
				sel.GetResultValue(), true,
			)
			switch joinType {
			case plans.JoinExists:
				flatMapPlan.SetExists(true)
			case plans.JoinNotExists:
				flatMapPlan.SetNotExists(true)
			}

			leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(outerExpr))
			rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
			call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ))
			return true
		}
	}
	return false
}

func (r *ImplementNestedLoopJoinRule) buildExistsFlatMap(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	outerPlan plans.RecordQueryPlan, innerScan *plans.RecordQueryScanPlan,
	outerAlias, innerAlias string,
	outerExpr, innerExpr expressions.RelationalExpression,
	joinType plans.JoinType,
	outerPrefix, innerPrefix string,
	outerVal *values.FieldValue,
	matchedPred predicates.QueryPredicate,
	allPreds []predicates.QueryPredicate,
) bool {
	outerCorrelation := values.NamedCorrelationIdentifier(outerAlias)
	bareField := outerVal.Field
	if outerVal.Child == nil && strings.HasPrefix(strings.ToUpper(bareField), outerPrefix) {
		bareField = bareField[len(outerPrefix):]
	}
	correlatedOperand := values.NewFieldValue(
		values.NewQuantifiedObjectValue(outerCorrelation),
		bareField, outerVal.Typ,
	)
	correlatedComp := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: correlatedOperand}
	cr := predicates.EmptyComparisonRange()
	mergeResult := cr.Merge(correlatedComp)
	if !mergeResult.Ok {
		return false
	}

	correlatedScan := innerScan.WithScanComparisons([]*predicates.ComparisonRange{mergeResult.Range})

	// Split residual predicates: inner-referencing → filter inside inner
	// plan (alias-stripped). Outer-only → filter on the outer plan.
	var innerResiduals, outerResiduals []predicates.QueryPredicate
	for _, p := range allPreds {
		if p == matchedPred {
			continue
		}
		if predicateReferencesAlias(p, innerAlias) {
			innerResiduals = append(innerResiduals, p)
		} else {
			outerResiduals = append(outerResiduals, p)
		}
	}

	var innerWithFilter plans.RecordQueryPlan = correlatedScan
	if len(innerResiduals) > 0 {
		stripped := stripAliasFromPredicates(innerResiduals, innerPrefix)
		innerWithFilter = plans.NewRecordQueryPredicatesFilterPlan(correlatedScan, stripped)
	}

	var outerWithFilter plans.RecordQueryPlan = outerPlan
	if len(outerResiduals) > 0 {
		stripped := stripAliasFromPredicates(outerResiduals, outerPrefix)
		outerWithFilter = plans.NewRecordQueryPredicatesFilterPlan(outerPlan, stripped)
	}

	innerCorrelation := values.NamedCorrelationIdentifier(innerAlias)
	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		outerWithFilter, innerWithFilter,
		outerCorrelation, innerCorrelation,
		sel.GetResultValue(), true,
	)
	switch joinType {
	case plans.JoinExists:
		flatMapPlan.SetExists(true)
	case plans.JoinNotExists:
		flatMapPlan.SetNotExists(true)
	}

	leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(outerExpr))
	rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ))
	return true
}

// stripAliasFromPredicates delegates to stripAliasPrefixFromPredicates
// (rule_push_filter_below_join.go) which correctly recurses into all
// predicate types (AND/OR/NOT/Value) and all value types (Arithmetic,
// Cast, ScalarFunction, etc.) via values.MapFieldValues.
func stripAliasFromPredicates(preds []predicates.QueryPredicate, prefix string) []predicates.QueryPredicate {
	alias := strings.TrimSuffix(prefix, ".")
	return stripAliasPrefixFromPredicates(preds, alias)
}

// matchJoinPKPredicate checks if a comparison predicate matches the
// pattern outer.FK = inner.PK (or reversed). Returns the outer-side
// FieldValue and the inner column name if matched, nil otherwise.
func (r *ImplementNestedLoopJoinRule) matchJoinPKPredicate(
	cp *predicates.ComparisonPredicate,
	outerPrefix, innerPrefix, pkCol string,
) (*values.FieldValue, string) {
	lhsFV, lhsOk := cp.Operand.(*values.FieldValue)
	rhsFV, rhsOk := cp.Comparison.Operand.(*values.FieldValue)
	if !lhsOk || !rhsOk {
		return nil, ""
	}

	lhsAlias, lhsCol := fieldValueAliasAndCol(lhsFV)
	rhsAlias, rhsCol := fieldValueAliasAndCol(rhsFV)

	outerAlias := strings.TrimSuffix(outerPrefix, ".")
	innerAlias := strings.TrimSuffix(innerPrefix, ".")

	if lhsAlias == outerAlias && rhsAlias == innerAlias {
		if rhsCol == pkCol {
			return lhsFV, rhsCol
		}
	}
	if lhsAlias == innerAlias && rhsAlias == outerAlias {
		if lhsCol == pkCol {
			return rhsFV, lhsCol
		}
	}

	return nil, ""
}

func fieldValueAliasAndCol(fv *values.FieldValue) (alias, col string) {
	if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
		return strings.ToUpper(qov.Correlation.String()), strings.ToUpper(fv.Field)
	}
	upper := strings.ToUpper(fv.Field)
	if dot := strings.IndexByte(upper, '.'); dot >= 0 {
		return upper[:dot], upper[dot+1:]
	}
	return "", upper
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
