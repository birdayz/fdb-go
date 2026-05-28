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

	leftExpr := findBestPhysicalExpr(leftRef, PlanningCostModelLess)
	rightExpr := findBestPhysicalExpr(rightRef, PlanningCostModelLess)
	if leftExpr == nil || rightExpr == nil {
		return
	}
	leftPlan := leftExpr.(physicalPlanExpression).GetRecordQueryPlan()
	rightPlan := rightExpr.(physicalPlanExpression).GetRecordQueryPlan()
	if leftPlan == nil || rightPlan == nil {
		return
	}

	aliases := sel.GetSourceAliases()
	var leftAlias, rightAlias string
	if len(aliases) >= 2 {
		leftAlias = aliases[0]
		rightAlias = aliases[1]
	}
	if leftAlias == "" {
		leftAlias = quants[0].GetAlias().Name()
	}
	if rightAlias == "" {
		rightAlias = quants[1].GetAlias().Name()
	}

	var joinType plans.JoinType
	switch sel.GetJoinType() {
	case expressions.JoinLeftOuter:
		joinType = plans.JoinLeftOuter
	case expressions.JoinCross:
		joinType = plans.JoinCross
	default:
		joinType = plans.JoinInner
	}

	// Correlated scan FlatMap: O(N×logM) via correlated PK/index probes.
	// Yield as an alternative — do NOT early-return. The cost model
	// compares this against the NLJ below and picks the better plan.
	r.tryFlatMapPlan(call, sel, leftPlan, rightPlan, leftAlias, rightAlias, leftExpr, rightExpr, joinType)

	leftCorr := values.NamedCorrelationIdentifier(leftAlias)
	rightCorr := values.NamedCorrelationIdentifier(rightAlias)

	leftDepsRight := referenceIsCorrelatedTo(leftRef, quants[1].GetAlias())
	rightDepsLeft := referenceIsCorrelatedTo(rightRef, quants[0].GetAlias())
	canSwap := joinType != plans.JoinLeftOuter
	hasCorrelation := leftDepsRight || rightDepsLeft
	if !hasCorrelation {
		joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
			leftPlan, rightPlan,
			sel.GetPredicates(),
			joinType,
			leftAlias, rightAlias,
			sel.GetResultValue(),
		)
		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, leftQ, rightQ))
	}

	// Correlated FlatMap: for PartitionBinarySelectRule output where
	// predicates are absorbed into sub-Selects creating correlation.
	if leftDepsRight && !rightDepsLeft && canSwap {
		r.yieldGeneralFlatMap(call, sel,
			rightPlan, leftPlan, rightCorr, leftCorr,
			rightExpr, leftExpr, joinType)
	} else if rightDepsLeft && !leftDepsRight {
		r.yieldGeneralFlatMap(call, sel,
			leftPlan, rightPlan, leftCorr, rightCorr,
			leftExpr, rightExpr, joinType)
	}
}

func referenceIsCorrelatedTo(ref *expressions.Reference, targetAlias values.CorrelationIdentifier) bool {
	_, ok := ref.GetCorrelatedTo()[targetAlias]
	return ok
}

func (r *ImplementNestedLoopJoinRule) yieldGeneralFlatMap(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	outerPlan, innerPlan plans.RecordQueryPlan,
	outerCorr, innerCorr values.CorrelationIdentifier,
	outerExpr, innerExpr expressions.RelationalExpression,
	joinType plans.JoinType,
) {
	preds := flattenAndPredicates(sel.GetPredicates())

	var outerPreds, joinPreds []predicates.QueryPredicate
	for _, pred := range preds {
		corrSet := predicates.GetCorrelatedToOfPredicate(pred)
		if _, ok := corrSet[innerCorr]; ok {
			joinPreds = append(joinPreds, pred)
		} else {
			outerPreds = append(outerPreds, pred)
		}
	}

	var innerWrapped plans.RecordQueryPlan = innerPlan
	if len(joinPreds) > 0 {
		innerWrapped = plans.NewRecordQueryPredicatesFilterPlanWithAlias(
			innerPlan, joinPreds, innerCorr)
	}

	var outerWrapped plans.RecordQueryPlan = outerPlan
	if len(outerPreds) > 0 {
		outerWrapped = plans.NewRecordQueryPredicatesFilterPlanWithAlias(
			outerPlan, outerPreds, outerCorr)
	}

	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		outerWrapped, innerWrapped,
		outerCorr, innerCorr,
		sel.GetResultValue(), false,
	)
	switch joinType {
	case plans.JoinLeftOuter:
		flatMapPlan.SetLeftOuter(true)
	}

	outerQ := expressions.ForEachQuantifier(call.MemoizeExpression(outerExpr))
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, outerQ, innerQ))
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

	outerExpr := getWinnerForOrdering(outerRef, PreserveOrdering())
	if outerExpr == nil {
		return
	}
	outerPh, ok := outerExpr.(physicalPlanExpression)
	if !ok {
		return
	}
	outerPlan := outerPh.GetRecordQueryPlan()

	innerExpr := getWinnerForOrdering(innerRef, PreserveOrdering())
	if innerExpr == nil {
		return
	}
	innerPh, ok := innerExpr.(physicalPlanExpression)
	if !ok {
		return
	}
	innerPlan := innerPh.GetRecordQueryPlan()

	// Separate predicates into EXISTS-related and non-EXISTS.
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

	// Extract source aliases for datum qualification.
	aliases := sel.GetSourceAliases()
	var outerAlias, innerAlias string
	if len(aliases) >= 1 {
		outerAlias = aliases[0]
	}
	if len(aliases) >= 2 {
		innerAlias = aliases[1]
	}

	// Try correlated-scan FlatMap: if a correlated predicate matches the
	// inner table's PK or index, use a correlated scan (fast path).
	if len(regularPreds) > 0 && !sel.IsQuantifiersSwapped() {
		if r.tryExistsFlatMap(call, sel, outerPlan, innerPlan, outerAlias, innerAlias, outerExpr, innerExpr, joinType, regularPreds) {
			return
		}
	}

	outerQuant := expressions.NamedPhysicalQuantifier(quants[0].GetAlias(), outerMemoRef)

	// Non-correlated: NLJ with materialized inner (one-shot).
	fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(innerPlan, values.NewNullValue(values.UnknownType))
	fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan,
		expressions.NamedPhysicalQuantifier(quants[1].GetAlias(), call.MemoizeExpression(innerExpr)))
	fodRef := call.MemoizeExpression(fodWrapper)

	innerCorr := values.NamedCorrelationIdentifier(innerAlias)
	var joinPreds []predicates.QueryPredicate
	var outerOnlyPreds []predicates.QueryPredicate
	for _, p := range regularPreds {
		if _, ok := predicates.GetCorrelatedToOfPredicate(p)[innerCorr]; ok {
			joinPreds = append(joinPreds, p)
		} else {
			outerOnlyPreds = append(outerOnlyPreds, p)
		}
	}

	var nljOuter plans.RecordQueryPlan = outerPlan
	if len(outerOnlyPreds) > 0 {
		outerCorr := values.NamedCorrelationIdentifier(outerAlias)
		nljOuter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, outerOnlyPreds, outerCorr)
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
		sel.GetResultValue(),
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

	leftExpr := getWinnerForOrdering(leftRef, PreserveOrdering())
	rightExpr := getWinnerForOrdering(rightRef, PreserveOrdering())
	existExpr := getWinnerForOrdering(existRef, PreserveOrdering())
	if leftExpr == nil || rightExpr == nil || existExpr == nil {
		return
	}
	leftPh, ok1 := leftExpr.(physicalPlanExpression)
	rightPh, ok2 := rightExpr.(physicalPlanExpression)
	existPh, ok3 := existExpr.(physicalPlanExpression)
	if !ok1 || !ok2 || !ok3 {
		return
	}
	leftPlan := leftPh.GetRecordQueryPlan()
	rightPlan := rightPh.GetRecordQueryPlan()
	existPlan := existPh.GetRecordQueryPlan()

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
		existCorr := values.NamedCorrelationIdentifier(existAlias)
		if _, ok := predicates.GetCorrelatedToOfPredicate(p)[existCorr]; ok {
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
		sel.GetResultValue(),
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
		nil,
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
			bareField := bareColumnName(outerVal, leftAlias)
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
		rightCorr := values.NamedCorrelationIdentifier(rightAlias)
		leftCorr := values.NamedCorrelationIdentifier(leftAlias)
		var outerPreds, innerOnlyPreds, abovePreds []predicates.QueryPredicate
		for pi, p := range preds {
			if matchedPreds[pi] {
				continue
			}
			corrSet := predicates.GetCorrelatedToOfPredicate(p)
			if _, hasRight := corrSet[rightCorr]; hasRight {
				_, hasLeft := corrSet[leftCorr]
				if joinType == plans.JoinLeftOuter && !hasLeft {
					innerOnlyPreds = append(innerOnlyPreds, p)
				} else {
					abovePreds = append(abovePreds, p)
				}
			} else {
				outerPreds = append(outerPreds, p)
			}
		}

		if len(innerOnlyPreds) > 0 {
			innerWithFilter := plans.NewRecordQueryPredicatesFilterPlanWithAlias(flatMapPlan.GetInner(), innerOnlyPreds, rightCorr)
			flatMapPlan = plans.NewRecordQueryFlatMapPlan(
				flatMapPlan.GetOuter(), innerWithFilter,
				flatMapPlan.GetOuterAlias(), flatMapPlan.GetInnerAlias(),
				flatMapPlan.GetResultValue(), flatMapPlan.InheritOuterRecordProperties(),
			)
			flatMapPlan.SetLeftOuter(true)
		}

		if len(outerPreds) > 0 {
			pushedOuter := tryPushPredicatesIntoScan(flatMapPlan.GetOuter(), outerPreds, call.Context, leftAlias, leftCorr)
			flatMapPlan = plans.NewRecordQueryFlatMapPlan(
				pushedOuter, flatMapPlan.GetInner(),
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

		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		if len(abovePreds) > 0 {
			flatMapWrapper := newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ)
			flatMapRef := call.MemoizeExpression(flatMapWrapper)
			aboveFilterPlan := plans.NewRecordQueryPredicatesFilterPlan(flatMapPlan, abovePreds)
			call.Yield(NewPhysicalPredicatesFilterWrapper(aboveFilterPlan, expressions.ForEachQuantifier(flatMapRef)))
		} else {
			call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ))
		}
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
			bareField := bareColumnName(outerVal, leftAlias)
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
			idxRightCorr := values.NamedCorrelationIdentifier(rightAlias)
			idxLeftCorr := values.NamedCorrelationIdentifier(leftAlias)
			var innerOnlyResiduals, otherResiduals []predicates.QueryPredicate
			for _, p := range preds {
				if p == pred {
					continue
				}
				corrSet := predicates.GetCorrelatedToOfPredicate(p)
				_, hasRight := corrSet[idxRightCorr]
				_, hasLeft := corrSet[idxLeftCorr]
				if joinType == plans.JoinLeftOuter && hasRight && !hasLeft {
					innerOnlyResiduals = append(innerOnlyResiduals, p)
				} else {
					otherResiduals = append(otherResiduals, p)
				}
			}
			if len(innerOnlyResiduals) > 0 {
				innerWithFilter := plans.NewRecordQueryPredicatesFilterPlanWithAlias(flatMapPlan.GetInner(), innerOnlyResiduals, idxRightCorr)
				flatMapPlan = plans.NewRecordQueryFlatMapPlan(
					flatMapPlan.GetOuter(), innerWithFilter,
					flatMapPlan.GetOuterAlias(), flatMapPlan.GetInnerAlias(),
					flatMapPlan.GetResultValue(), flatMapPlan.InheritOuterRecordProperties(),
				)
				flatMapPlan.SetLeftOuter(true)
			}
			leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
			rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
			if len(otherResiduals) > 0 {
				flatMapWrapper := newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ)
				flatMapRef := call.MemoizeExpression(flatMapWrapper)
				aboveFilterPlan := plans.NewRecordQueryPredicatesFilterPlan(flatMapPlan, otherResiduals)
				call.Yield(NewPhysicalPredicatesFilterWrapper(aboveFilterPlan, expressions.ForEachQuantifier(flatMapRef)))
			} else {
				call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ))
			}
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
			return r.buildExistsFlatMap(call, sel, outerPlan, innerScan, outerAlias, innerAlias, outerExpr, innerExpr, joinType, outerVal, pred, preds)
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
			bareField := bareColumnName(outerVal, outerAlias)
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

			existInnerCorr := values.NamedCorrelationIdentifier(innerAlias)
			var innerResiduals, outerResiduals []predicates.QueryPredicate
			for _, p := range preds {
				if p == pred {
					continue
				}
				if _, ok := predicates.GetCorrelatedToOfPredicate(p)[existInnerCorr]; ok {
					innerResiduals = append(innerResiduals, p)
				} else {
					outerResiduals = append(outerResiduals, p)
				}
			}

			existInnerCorr2 := values.NamedCorrelationIdentifier(innerAlias)
			var innerWithFilter plans.RecordQueryPlan = correlatedIndexScan
			if len(innerResiduals) > 0 {
				innerWithFilter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(correlatedIndexScan, innerResiduals, existInnerCorr2)
			}

			var outerWithFilter plans.RecordQueryPlan = outerPlan
			if len(outerResiduals) > 0 {
				outerWithFilter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, outerResiduals, outerCorrelation)
			}

			innerCorrelation := existInnerCorr2
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
	outerVal *values.FieldValue,
	matchedPred predicates.QueryPredicate,
	allPreds []predicates.QueryPredicate,
) bool {
	outerCorrelation := values.NamedCorrelationIdentifier(outerAlias)
	bareField := bareColumnName(outerVal, outerAlias)
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

	buildInnerCorr := values.NamedCorrelationIdentifier(innerAlias)
	var innerResiduals, outerResiduals []predicates.QueryPredicate
	for _, p := range allPreds {
		if p == matchedPred {
			continue
		}
		if _, ok := predicates.GetCorrelatedToOfPredicate(p)[buildInnerCorr]; ok {
			innerResiduals = append(innerResiduals, p)
		} else {
			outerResiduals = append(outerResiduals, p)
		}
	}

	var innerWithFilter plans.RecordQueryPlan = correlatedScan
	if len(innerResiduals) > 0 {
		innerWithFilter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(correlatedScan, innerResiduals, buildInnerCorr)
	}

	var outerWithFilter plans.RecordQueryPlan = outerPlan
	if len(outerResiduals) > 0 {
		outerWithFilter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, outerResiduals, outerCorrelation)
	}

	innerCorrelation := buildInnerCorr
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

// bareColumnName returns the unqualified column name from a FieldValue,
// stripping the table alias prefix when it matches expectedAlias. For
// QOV-based FieldValues the Field is already bare; for flat
// "ALIAS.col" strings, the alias is stripped via fieldValueAliasAndCol.
func bareColumnName(fv *values.FieldValue, expectedAlias string) string {
	if fv.Child != nil {
		return fv.Field
	}
	fvAlias, col := fieldValueAliasAndCol(fv)
	if fvAlias != "" && fvAlias == strings.ToUpper(expectedAlias) {
		return col
	}
	return fv.Field
}

func tryPushPredicatesIntoScan(
	outerPlan plans.RecordQueryPlan,
	preds []predicates.QueryPredicate,
	ctx PlanContext,
	alias string,
	correlation values.CorrelationIdentifier,
) plans.RecordQueryPlan {
	scan, ok := outerPlan.(*plans.RecordQueryScanPlan)
	if !ok {
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, preds, correlation)
	}
	recordTypes := scan.GetRecordTypes()
	if len(recordTypes) != 1 || ctx == nil {
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, preds, correlation)
	}
	pkCols := ctx.GetPrimaryKeyColumns(recordTypes[0])
	if len(pkCols) == 0 {
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, preds, correlation)
	}

	var matchedRanges []*predicates.ComparisonRange
	matchedPreds := make(map[int]bool)
	for _, pkCol := range pkCols {
		pkUpper := strings.ToUpper(pkCol)
		found := false
		for pi, p := range preds {
			if matchedPreds[pi] {
				continue
			}
			cp, ok := p.(*predicates.ComparisonPredicate)
			if !ok {
				continue
			}
			fv, ok := cp.Operand.(*values.FieldValue)
			if !ok {
				continue
			}
			if !isScanRangeCompatible(cp.Comparison.Type) {
				continue
			}
			if strings.ToUpper(fv.Field) != pkUpper {
				continue
			}
			cr := predicates.EmptyComparisonRange()
			mergeResult := cr.Merge(&cp.Comparison)
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
		narrowedScan := scan.WithScanComparisons(matchedRanges)
		var residual []predicates.QueryPredicate
		for i, p := range preds {
			if !matchedPreds[i] {
				residual = append(residual, p)
			}
		}
		if len(residual) > 0 {
			return plans.NewRecordQueryPredicatesFilterPlanWithAlias(narrowedScan, residual, correlation)
		}
		return narrowedScan
	}
	return plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, preds, correlation)
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
