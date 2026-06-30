package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// AggregateDataAccessRule matches a GroupByExpression against aggregate
// index candidates (SUM, COUNT, etc.). When a match is found, the rule
// directly produces an index scan that reads pre-computed aggregates
// from the aggregate index — no runtime aggregation needed.
//
//	GroupBy(keys=[k1], aggs=[SUM(col)], inner=Scan)
//	  → IndexScan(aggregate_index)   [when aggregate index matches]
//
// For single-aggregate queries, one AggregateIndexMatchCandidate covers
// the entire GroupBy and produces a direct index scan.
//
// For multi-aggregate queries (e.g. SUM(a), COUNT(*)), each aggregate
// is served by a separate aggregate index. The rule intersects them
// via RecordQueryMultiIntersectionOnValuesPlan: all streams are
// ordered by the same grouping columns (comparison key), and the
// result row picks grouping values from any stream (they're identical)
// plus each aggregate from its respective stream.
//
// Mirrors Java's AggregateDataAccessRule including
// createIntersectionAndCompensation().
type AggregateDataAccessRule struct {
	matcher matching.BindingMatcher
}

func NewAggregateDataAccessRule() *AggregateDataAccessRule {
	return &AggregateDataAccessRule{
		matcher: NewExpressionMatcher[*expressions.GroupByExpression]("agg_data_access"),
	}
}

func (r *AggregateDataAccessRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *AggregateDataAccessRule) OnMatch(call *ExpressionRuleCall) {
	gb := matching.Get[*expressions.GroupByExpression](call.Bindings, r.matcher)

	candidates := call.Context.GetMatchCandidates()
	if len(candidates) == 0 {
		return
	}

	innerRef := gb.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	scan := findFullScanThroughFilter(innerRef)
	if scan == nil {
		return
	}
	scanTypes := scan.GetRecordTypes()

	// Extract filter predicates from the GroupBy's inner when it wraps
	// a Filter(pred, Scan). Predicates on group key columns become scan
	// bounds on the aggregate index (bounded AISCAN).
	innerFilterPreds := extractInnerFilterPredicates(innerRef)

	// Path 1: single-aggregate match — one candidate covers the full GroupBy.
	singleMatched := false
	for _, cand := range candidates {
		aggCand, ok := cand.(*AggregateIndexMatchCandidate)
		if !ok {
			continue
		}
		if !recordTypesOverlap(scanTypes, aggCand.GetRecordTypes()) {
			continue
		}
		if !aggCand.MatchesGroupBy(gb) {
			continue
		}
		// An aggregate index stores aggregates precomputed over ALL rows of the
		// group. A residual predicate that filters the aggregation INPUT (a
		// non-grouping column, or a non-equality on a grouping column) cannot be
		// compensated after the fact, so the index must NOT be used — Java's
		// data-access compensation marks such a match impossible and falls back
		// to StreamingAgg over a filtered scan. buildAggScanPrefix only turns
		// grouping-key EQUALITIES into scan bounds; if any other filter
		// predicate remains, decline this candidate.
		if !aggInnerFilterFullyConsumable(innerRef, aggCand) {
			continue
		}

		prefix := buildAggScanPrefix(aggCand, innerFilterPreds)
		scanPlan := aggCand.ToScanPlan(prefix, false)
		idxPlan := extractIndexPlan(scanPlan)
		if idxPlan == nil {
			continue
		}

		var recordTypeName string
		if rts := aggCand.GetRecordTypes(); len(rts) > 0 {
			recordTypeName = rts[0]
		}
		aggPlan := plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, values.UnknownType, aggCand.aggFunction.String(),
		).WithGroupColumns(aggCand.groupCols, aggCand.aggColumn)

		call.Yield(&physicalAggregateIndexWrapper{plan: aggPlan})
		singleMatched = true
	}
	if singleMatched {
		return
	}

	// Path 2: multi-aggregate intersection — multiple candidates, each
	// covering one of the GroupBy's aggregates with identical grouping.
	tryMultiAggregateIntersection(call, gb, candidates, scanTypes, innerFilterPreds)
}

// aggInnerFilterFullyConsumable reports whether EVERY predicate on the
// aggregation's input Filter can be turned into a grouping-key equality scan
// bound (i.e. consumed by buildAggScanPrefix). If any predicate cannot — a
// non-ComparisonPredicate, a non-equality comparison, or a comparison whose LHS
// is not a grouping column — the aggregate index cannot faithfully serve the
// query (the dropped predicate would be silently ignored, returning aggregates
// over the unfiltered population), so the caller must decline the match and let
// StreamingAgg-over-filtered-scan handle it. This is the Go analog of Java's
// data-access compensation declaring the match impossible.
func aggInnerFilterFullyConsumable(ref *expressions.Reference, cand *AggregateIndexMatchCandidate) bool {
	for _, m := range ref.Members() {
		f, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		preds := f.GetPredicates()
		// Record which grouping column each predicate equality-binds. Anything
		// that is not an equality on a grouping column (a non-comparison
		// predicate, a non-equality, a non-group column) makes the index unable
		// to serve the query → decline.
		bound := make([]bool, len(cand.groupCols))
		n := 0
		for _, p := range preds {
			cp, ok := p.(*predicates.ComparisonPredicate)
			if !ok {
				return false
			}
			idx := groupColEqualityIndex(cp, cand.groupCols)
			if idx < 0 || bound[idx] {
				// not a grouping-key equality, or a duplicate bound on the same
				// column (e.g. `a=1 AND a=2`) — the scan applies only one.
				return false
			}
			bound[idx] = true
			n++
		}
		// ToScanPlan consumes ONLY the contiguous LEADING prefix of bound
		// columns (it breaks at the first gap), so an equality on a non-leading
		// grouping key (`WHERE subregion=… GROUP BY region, subregion`) would be
		// silently dropped. Require the n bound columns to be exactly the leading
		// prefix groupCols[0..n-1] — then every predicate maps 1:1 to a column
		// the scan actually applies.
		for i := 0; i < n; i++ {
			if !bound[i] {
				return false
			}
		}
	}
	return true
}

// groupColEqualityIndex returns the index of the grouping column that cp is an
// EQUALITY bound on (matching buildAggScanPrefix's column matching), or -1 if cp
// is not an equality predicate whose LHS is a grouping column. Shared by the
// consumption guard (aggInnerFilterFullyConsumable) and the bound builder
// (buildAggScanPrefix) so the two cannot drift — the drift between guard and
// consumer is what let the original residual-drop bug ship.
func groupColEqualityIndex(cp *predicates.ComparisonPredicate, groupCols []string) int {
	if cp.Comparison.Type != predicates.ComparisonEquals {
		return -1
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok {
		return -1
	}
	// The comparand (RHS) must be a constant the scan can bind to — a literal or
	// parameter — NOT a value that reads a record field. `region = status`
	// correlates two columns of the SAME record and can never be an index bound;
	// it must stay a residual (decline -> StreamingAgg). Without this, the field
	// comparand makes buildAggScanPrefix.Merge fail to bind while the guard still
	// marks the predicate "consumed", silently dropping it (wrong rows). A rare
	// genuinely-correlated bound is conservatively declined too.
	if valueReadsField(cp.Comparison.Operand) {
		return -1
	}
	for i, col := range groupCols {
		if eqFold(fv.Field, col) || eqFold(fieldNameOnly(fv.Field), col) {
			return i
		}
	}
	return -1
}

// valueReadsField reports whether v references a record field anywhere in
// its tree (i.e. it is not a pure literal/parameter constant the index scan can
// bind to). A bare literal, a parameter, or a cast/arithmetic over constants
// returns false; anything containing a FieldValue returns true.
func valueReadsField(v values.Value) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(*values.FieldValue); ok {
		return true
	}
	for _, c := range v.Children() {
		if valueReadsField(c) {
			return true
		}
	}
	return false
}

// extractInnerFilterPredicates returns ComparisonPredicates from the
// inner Reference's Filter expressions. Used by AggregateDataAccessRule
// to push WHERE predicates on group keys into the aggregate index scan
// range. Returns nil if no filter predicates are found.
func extractInnerFilterPredicates(ref *expressions.Reference) []*predicates.ComparisonPredicate {
	var result []*predicates.ComparisonPredicate
	for _, m := range ref.Members() {
		f, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		for _, p := range f.GetPredicates() {
			if cp, ok := p.(*predicates.ComparisonPredicate); ok {
				result = append(result, cp)
			}
		}
	}
	return result
}

// buildAggScanPrefix matches filter predicates against an aggregate
// index candidate's group columns. For each group column that has an
// equality predicate in the filter, creates a ComparisonRange bound
// in the prefix map. This converts WHERE group_key = X into a bounded
// AISCAN [EQUALS X] range.
func buildAggScanPrefix(
	cand *AggregateIndexMatchCandidate,
	filterPreds []*predicates.ComparisonPredicate,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	if len(filterPreds) == 0 {
		return prefix
	}
	for _, cp := range filterPreds {
		idx := groupColEqualityIndex(cp, cand.groupCols)
		if idx < 0 {
			continue
		}
		if _, exists := prefix[cand.aliases[idx]]; exists {
			continue // first equality on a column wins
		}
		cr := predicates.EmptyComparisonRange()
		if result := cr.Merge(&cp.Comparison); result.Ok {
			prefix[cand.aliases[idx]] = result.Range
		}
	}
	return prefix
}

// aggregateFlowedColumnName returns the column name under which the
// aggregate-index executor (aggregateIndexCursor.OnNext) flows an
// aggregate value into the row. COUNT(*) (empty aggColumn) flows under
// "FUNC(*)"; a column aggregate flows under "FUNC(col)". The
// multi-aggregate intersection result value references these names so the
// pick-up resolves against the row the child stream produces. Keep this in
// sync with the executor's aggregateIndexCursor.
func aggregateFlowedColumnName(aggFunc, aggColumn string) string {
	if aggColumn == "" {
		return aggFunc + "(*)"
	}
	return aggFunc + "(" + aggColumn + ")"
}

// fieldNameOnly strips a "TABLE." prefix from a qualified field name.
func fieldNameOnly(qualified string) string {
	for i := len(qualified) - 1; i >= 0; i-- {
		if qualified[i] == '.' {
			return qualified[i+1:]
		}
	}
	return qualified
}

// tryMultiAggregateIntersection attempts to satisfy a multi-aggregate
// GroupBy by intersecting aggregate index scans. For each aggregate in
// the GroupBy we find an AggregateIndexMatchCandidate that:
//   - covers exactly that aggregate (function + column)
//   - shares identical grouping columns with all other candidates
//   - has overlapping record types with the scan
//
// When all aggregates are covered, we build a
// RecordQueryMultiIntersectionOnValuesPlan whose comparison key is the
// grouping columns and whose result value is a record of (grouping
// columns from first child, aggregate from each child).
//
// Mirrors Java's createIntersectionAndCompensation() /
// computeCommonAndPickUpValues() / computeIntersectionResultValue().
func tryMultiAggregateIntersection(
	call *ExpressionRuleCall,
	gb *expressions.GroupByExpression,
	candidates []MatchCandidate,
	scanTypes []string,
	innerFilterPreds []*predicates.ComparisonPredicate,
) {
	aggs := gb.GetAggregates()
	if len(aggs) < 2 {
		return
	}

	// Collect aggregate-index candidates that are relevant (record
	// types overlap with the scan).
	var aggCands []*AggregateIndexMatchCandidate
	for _, cand := range candidates {
		ac, ok := cand.(*AggregateIndexMatchCandidate)
		if !ok {
			continue
		}
		if !recordTypesOverlap(scanTypes, ac.GetRecordTypes()) {
			continue
		}
		aggCands = append(aggCands, ac)
	}
	if len(aggCands) < len(aggs) {
		return
	}

	// For each aggregate in the GroupBy, find the first candidate that
	// matches it. Each candidate is used at most once.
	used := make([]bool, len(aggCands))
	matched := make([]*AggregateIndexMatchCandidate, len(aggs))
	for i := range aggs {
		for j, ac := range aggCands {
			if used[j] {
				continue
			}
			if ac.MatchesSingleAggregateOf(gb, i) {
				matched[i] = ac
				used[j] = true
				break
			}
		}
		if matched[i] == nil {
			return // aggregate not covered — can't intersect
		}
	}

	// Verify all candidates share the same grouping columns (Java's
	// commonGroupingKeyValuesMaybe). We already know each candidate
	// matched the GroupBy's grouping keys in MatchesSingleAggregateOf,
	// so they're all equal by transitivity. But let's be explicit.
	groupCols := matched[0].groupCols
	for _, mc := range matched[1:] {
		if len(mc.groupCols) != len(groupCols) {
			return
		}
		for k := range groupCols {
			if !eqFold(mc.groupCols[k], groupCols[k]) {
				return
			}
		}
	}

	// Same residual-compensation guard as the single-aggregate path: if any
	// input filter predicate is not a grouping-key equality (so it cannot
	// become a scan bound), the aggregate indexes cannot serve the filtered
	// query — decline and fall back to StreamingAgg over a filtered scan.
	if innerRef := gb.GetInner().GetRangesOver(); innerRef != nil {
		if !aggInnerFilterFullyConsumable(innerRef, matched[0]) {
			return
		}
	}

	// Build child aggregate-index scan plans. Each child MUST be a
	// RecordQueryAggregateIndexPlan (not a bare RecordQueryIndexPlan): an
	// aggregate index stores the running aggregate IN the index entry
	// (key=group cols, value=aggregate) and points at no base record, so a
	// plain index scan would try to fetch a non-existent record and emit
	// zero rows. The aggregate-index executor instead flows a row of
	// {groupCol: value, "FUNC(col)": aggregate} — the same shape the
	// single-aggregate path produces — which the comparison key and the
	// merge step below depend on.
	childPlans := make([]plans.RecordQueryPlan, len(matched))
	for i, mc := range matched {
		prefix := buildAggScanPrefix(mc, innerFilterPreds)
		sp := mc.ToScanPlan(prefix, false)
		idxPlan := extractIndexPlan(sp)
		if idxPlan == nil {
			return
		}
		var recordTypeName string
		if rts := mc.GetRecordTypes(); len(rts) > 0 {
			recordTypeName = rts[0]
		}
		childPlans[i] = plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, values.UnknownType, mc.aggFunction.String(),
		).WithGroupColumns(mc.groupCols, mc.aggColumn)
	}

	// Comparison key = grouping column FieldValues. The aggregate-index
	// cursor flows each grouping column under its (uppercased) metadata
	// name, so the comparison key matches identical group values across the
	// per-aggregate streams. With a WHERE-equality prefix (cat='books')
	// each stream emits exactly that one group; the keys still match.
	comparisonKey := make([]values.Value, len(groupCols))
	for i, col := range groupCols {
		comparisonKey[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
	}

	// Result value = Record(groupCol0, ..., agg0, agg1, ...).
	// Grouping columns are identical across all streams; each aggregate is
	// picked up from its respective stream. Mirrors Java's
	// computeIntersectionResultValue(). The aggregate fields reference the
	// canonical aggregate-column name the child cursor flows
	// ("FUNC(col)" / "FUNC(*)") — NOT the bare aggColumn — so the pick-up
	// resolves against the merged row the executor builds. Output field
	// names match the single-aggregate path so the projection above reads
	// the same keys regardless of which plan won.
	fields := make([]values.RecordConstructorField, 0, len(groupCols)+len(aggs))
	for _, col := range groupCols {
		fields = append(fields, values.RecordConstructorField{
			Name:  col,
			Value: &values.FieldValue{Field: col, Typ: values.UnknownType},
		})
	}
	for i := range aggs {
		colName := aggregateFlowedColumnName(matched[i].aggFunction.String(), matched[i].aggColumn)
		fields = append(fields, values.RecordConstructorField{
			Name:  colName,
			Value: &values.FieldValue{Field: colName, Typ: values.UnknownType},
		})
	}
	resultValue := values.NewRecordConstructorValue(fields...)

	multiPlan := plans.NewRecordQueryMultiIntersectionOnValuesPlan(
		childPlans, comparisonKey, resultValue,
	)

	wrapper := NewPhysicalMultiIntersectionWrapper(multiPlan, nil)
	call.Yield(wrapper)
}

var _ ExpressionRule = (*AggregateDataAccessRule)(nil)
