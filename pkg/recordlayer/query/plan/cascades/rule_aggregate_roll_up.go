// Portions derived from FoundationDB Record Layer (GroupByExpression.java,
// AggregateIndexExpansionVisitor.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// yieldAggregateGroupingSubsumption serves a GroupBy whose grouping is not the
// candidate's grouping column for column, the rest of Java's
// GroupByExpression.groupingSubsumedBy beyond the exact match MatchesGroupBy
// takes:
//
//   - Each query grouping key must be one of the candidate's grouping columns,
//     in any order (explicitly matched).
//   - A candidate grouping column bound by an equality in the GroupBy's inner
//     filter is implicitly matched: `WHERE b = 5 GROUP BY a` over an index
//     grouped by (a, b).
//   - When every candidate column is matched, the index rows are the GroupBy's
//     groups, projected onto its row: `select sum(col1) from T2 where col2 = 0`
//     is `AISCAN(T2_I6 [EQUALS ...]) | MAP | ON EMPTY NULL`.
//   - Otherwise the candidate is rolled up to the longest prefix of its
//     grouping columns that are all matched, which must hold every explicitly
//     matched one (computeRollUpToValuesMaybe), and only for an index type
//     Java can roll up (AggregateIndexExpansionVisitor.canBeRolledUp: SUM,
//     COUNT, COUNT_NOT_NULL and the _EVER extrema). The roll-up is a streaming
//     aggregation over the index scan, SUM for the sums and counts, MAX/MIN for
//     the extrema (rollUpAggregateValueMaybe): a range on the grouping column is
//     `AISCAN(... [[GREATER_THAN ...]]) | AGG sum_l(_._1) GROUP BY ()`.
//
// A roll-up needs every predicate to be a scan bound: Java cannot compensate a
// predicate across a roll-up (GroupByExpression.compensate, the open issue
// 4115 its comment names), so a residual declines the candidate.
//
// It reports whether it yielded a plan.
func yieldAggregateGroupingSubsumption(
	call *ExpressionRuleCall,
	gb *expressions.GroupByExpression,
	cand *AggregateIndexMatchCandidate,
	innerFilterPreds []aggregateFilterPredicate,
) bool {
	aggs := gb.GetAggregates()
	if len(aggs) != 1 || !cand.aggregateMatches(aggs[0]) {
		return false
	}
	groupingKeys := gb.GetGroupingKeys()
	for _, k := range groupingKeys {
		if _, isRecord := k.Type().(*values.RecordType); isRecord {
			return false
		}
	}
	keys, err := expandGroupingKeysToPrimitives(groupingKeys)
	if err != nil || len(keys) != len(groupingKeys) {
		return false
	}
	nGroup := len(cand.groupCols)
	matched := make([]bool, nGroup)
	explicit := make([]int, len(keys))
	for i, k := range keys {
		explicit[i] = -1
		for j := 0; j < nGroup; j++ {
			if cand.groupKeyMatches(k, j) {
				explicit[i] = j
				break
			}
		}
		if explicit[i] < 0 {
			return false
		}
		matched[explicit[i]] = true
	}
	for _, fp := range innerFilterPreds {
		cp, ok := fp.pred.(*predicates.ComparisonPredicate)
		if !ok || !cp.Comparison.Type.IsEquality() {
			continue
		}
		if idx := groupColComparisonIndex(cp, cand.groupPaths, fp.input); idx >= 0 {
			matched[idx] = true
		}
	}
	rollUp := -1
	for j := 0; j < nGroup; j++ {
		if !matched[j] {
			rollUp = j
			break
		}
	}
	if rollUp >= 0 {
		for _, j := range explicit {
			if j >= rollUp {
				return false
			}
		}
		if _, ok := cand.rollUpFunction(); !ok {
			return false
		}
	}

	partition, ok := partitionAggregatePredicates(cand, innerFilterPreds)
	if !ok || (rollUp >= 0 && len(partition.residuals) > 0) {
		return false
	}
	if !candidateBindingRangesEligible(cand, partition.scanPrefix) {
		return false
	}
	idxPlan := extractIndexPlan(cand.ToScanPlan(partition.scanPrefix, false))
	if idxPlan == nil {
		return false
	}
	var recordTypeName string
	if rts := cand.GetRecordTypes(); len(rts) > 0 {
		recordTypeName = rts[0]
	}
	rowType, ok := aggregateIndexOutputType(cand)
	if !ok {
		return false
	}
	aggPlan, err := plans.NewRecordQueryAggregateIndexPlan(idxPlan, recordTypeName, rowType, cand.aggFunction.String())
	if err != nil {
		call.Fail(err)
		return false
	}
	aggPlan = aggPlan.WithGroupColumns(cand.groupCols, cand.aggColumn).
		WithCandidateGroupingCount(nGroup).
		WithEntryReader(cand.indexEntryToRecordValue(rowType)).
		WithColumnPaths(cand.groupPaths, cand.aggPath).
		WithGroupColumnLayout(cand.GetBaseRowType()).
		WithPermutedOrdering(cand.permuted)
	scanned, ok := partition.applyResiduals(aggPlan)
	if !ok {
		return false
	}

	// The row the projection reads: the index row, or the rolled-up row of the
	// prefix columns and the rolled-up aggregate.
	inner := scanned
	aggOrdinal := nGroup
	if rollUp >= 0 {
		rolled, ok := rollUpAggregateScan(call, cand, scanned, rowType, rollUp)
		if !ok {
			return false
		}
		inner = rolled
		aggOrdinal = rollUp
	}
	innerRow, ok := inner.GetResultType().(*values.RecordType)
	if !ok {
		return false
	}
	innerAlias := values.UniqueCorrelationIdentifier()
	innerQOV, err := values.NewQuantifiedObjectValue(innerAlias, innerRow)
	if err != nil {
		return false
	}
	outputNames := gb.OutputColumnNames()
	if len(outputNames) != len(keys)+1 {
		return false
	}
	fields := make([]values.RecordConstructorField, 0, len(keys)+1)
	for i, j := range explicit {
		v, err := values.ResolveFieldOrdinals(innerQOV, []int{j})
		if err != nil {
			return false
		}
		fields = append(fields, values.RecordConstructorField{Name: outputNames[i], Value: v})
	}
	aggValue, err := values.ResolveFieldOrdinals(innerQOV, []int{aggOrdinal})
	if err != nil {
		return false
	}
	fields = append(fields, values.RecordConstructorField{Name: outputNames[len(keys)], Value: aggValue})
	mapped, err := plans.NewRecordQueryMapPlanFromQuantifier(
		expressions.NamedPhysicalQuantifier(innerAlias, call.MemoizeExpression(inner)),
		values.NewRawRecordConstructorValue(fields...))
	if err != nil {
		call.Fail(err)
		return false
	}
	logicalPlan, err := answerUngroupedOnEmpty(mapped, gb)
	if err != nil {
		call.Fail(err)
		return false
	}
	call.Yield(logicalPlan)
	return true
}

// rollUpFunction is the aggregate that rolls this candidate's stored values up
// to a coarser grouping, Java's AggregateIndexExpansionVisitor
// rollUpAggregateMap: SUM over SUM, COUNT and COUNT_NOT_NULL entries, MAX over
// MAX_EVER, MIN over MIN_EVER. Any other index type cannot be rolled up.
func (c *AggregateIndexMatchCandidate) rollUpFunction() (expressions.AggregateFunction, bool) {
	if c.bitmapEntrySize > 0 || c.permuted {
		return 0, false
	}
	switch c.aggFunction {
	case expressions.AggSum, expressions.AggCount:
		return expressions.AggSum, true
	case expressions.AggMaxEver:
		return expressions.AggMax, true
	case expressions.AggMinEver:
		return expressions.AggMin, true
	default:
		return 0, false
	}
}

// rollUpAggregateScan aggregates the index scan's rows to their first prefix
// grouping columns, AggregateIndexMatchCandidate.toEquivalentPlan's
// RecordQueryStreamingAggregationPlan. The scan streams its groups in grouping
// order, so a prefix of the grouping is contiguous.
func rollUpAggregateScan(
	call *ExpressionRuleCall,
	cand *AggregateIndexMatchCandidate,
	scan plans.RecordQueryPlan,
	rowType *values.RecordType,
	prefix int,
) (plans.RecordQueryPlan, bool) {
	fn, ok := cand.rollUpFunction()
	if !ok {
		return nil, false
	}
	scanRow, ok := scan.GetResultType().(*values.RecordType)
	if !ok || len(scanRow.Fields) != len(rowType.Fields) {
		return nil, false
	}
	scanAlias := values.UniqueCorrelationIdentifier()
	scanQOV, err := values.NewQuantifiedObjectValue(scanAlias, scanRow)
	if err != nil {
		return nil, false
	}
	groupingKeys := make([]values.Value, prefix)
	names := make([]string, 0, prefix+1)
	for i := range groupingKeys {
		v, err := values.ResolveFieldOrdinals(scanQOV, []int{i})
		if err != nil {
			return nil, false
		}
		groupingKeys[i] = v
		names = append(names, scanRow.Fields[i].Name)
	}
	operand, err := values.ResolveFieldOrdinals(scanQOV, []int{len(cand.groupCols)})
	if err != nil {
		return nil, false
	}
	spec := expressions.AggregateSpec{
		Function:    fn,
		Operand:     operand,
		OperandName: scanRow.Fields[len(cand.groupCols)].Name,
	}
	names = append(names, expressions.AggregateResultColumnName(spec))
	rolled, err := plans.NewRecordQueryStreamingAggregationPlanForGroupBy(
		expressions.NamedPhysicalQuantifier(scanAlias, call.MemoizeExpression(scan)),
		groupingKeys, []expressions.AggregateSpec{spec}, names)
	if err != nil {
		call.Fail(err)
		return nil, false
	}
	return rolled, true
}
