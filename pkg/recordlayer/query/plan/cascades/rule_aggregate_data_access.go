package cascades

import (
	"fmt"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
// Mirrors Java's AggregateDataAccessRule, including the structure of
// createIntersectionAndCompensation().
//
// The compensation-projection ELISION is a Go-only read-side extension, not a
// mirrored behaviour, and the header used to imply otherwise. Java's equivalent gate
// cannot fire for a grouped aggregate: AggregateIndexExpansionVisitor publishes
// Column.unnamedOf(...) — a flat unnamed (_0.._n, agg) row whose getAvailableFields()
// is NO_FIELDS — so Java always compensates, which is why nearly every AISCAN in its
// yaml-tests is followed by a MAP. Go's aggregate-index leaf publishes NAMED columns
// instead, a pre-existing divergence, and that is what makes the elision expressible
// here at all. Allowed as an extension because wire format is untouched and the
// elision is checked end to end; do not read it as parity.
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

	// The GroupBy's inner filter, when it wraps a Filter(pred, Scan), is
	// partitioned per candidate into scan bounds, residuals over the
	// aggregate row, or a decline (RFC-248, partitionAggregatePredicates).
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
		// group. A predicate on the aggregation INPUT (a non-grouping column)
		// cannot be compensated after the fact, so the candidate declines —
		// Java's GroupByExpression.compensate returns impossibleCompensation
		// for it. A predicate over grouping columns the scan does not bind
		// filters whole groups and is applied as a residual above the scan.
		partition, ok := partitionAggregatePredicates(aggCand, innerFilterPreds)
		if !ok {
			continue
		}

		// The index answers alone, as Java's does: a group whose rows were all
		// deleted keeps its key at the atomic-add residue 0, and a SUM or
		// COUNT(col) group whose values are all NULL has no entry (Java's own
		// corpus marks both as open bugs, aggregate-empty-table.yamsql).
		if !candidateBindingRangesEligible(aggCand, partition.scanPrefix) {
			continue
		}
		for _, reverse := range []bool{false, true} {
			scanPlan := aggCand.ToScanPlan(partition.scanPrefix, reverse)
			idxPlan := extractIndexPlan(scanPlan)
			if idxPlan == nil {
				break
			}

			var recordTypeName string
			if rts := aggCand.GetRecordTypes(); len(rts) > 0 {
				recordTypeName = rts[0]
			}
			resultType, ok := aggregateIndexOutputType(aggCand)
			if !ok {
				break
			}
			resultType, ok = groupByRowOverAggregateRow(resultType, gb.OutputColumnNames())
			if !ok {
				break
			}
			aggPlan, err := plans.NewRecordQueryAggregateIndexPlan(
				idxPlan, recordTypeName, resultType, aggCand.aggFunction.String(),
			)
			if err != nil {
				call.Fail(err)
				return
			}
			aggPlan = aggPlan.WithGroupColumns(aggCand.groupCols, aggCand.aggColumn).
				WithCandidateGroupingCount(len(aggCand.groupCols)).
				WithEntryReader(aggCand.indexEntryToRecordValue(resultType)).
				WithColumnPaths(aggCand.groupPaths, aggCand.aggPath).
				WithGroupColumnLayout(aggCand.GetBaseRowType()).
				WithPermutedOrdering(aggCand.permuted)
			filtered, ok := partition.applyResiduals(aggPlan)
			if !ok {
				break
			}
			logicalPlan, err := answerUngroupedOnEmpty(filtered, gb)
			if err != nil {
				call.Fail(err)
				return
			}
			call.Yield(logicalPlan)
			singleMatched = true
			if reverse || !reverseServesARequest(aggPlan, call.GetRequestedOrderings()) {
				break
			}
		}
	}
	if singleMatched {
		return
	}

	// Path 2: multi-aggregate intersection — multiple candidates, each
	// covering one of the GroupBy's aggregates with identical grouping.
	tryMultiAggregateIntersection(call, gb, candidates, scanTypes, innerFilterPreds)
}

// reverseServesARequest reports whether scanning forward's aggregate index (or
// every leg of its merge) in reverse serves a requested ordering the forward
// plan does not, which is when Java's data-access rule picks the reverse scan
// (AbstractDataAccessRule.satisfiesRequestedOrdering resolves the direction per
// request, BOTH to forward). A reverse scan provides the mirror of the forward
// ordering, so it satisfies a request exactly when forward satisfies the
// request's mirror.
func reverseServesARequest(forward plans.RecordQueryPlan, requested []*properties.RequestedOrdering) bool {
	expr, ok := forward.(expressions.RelationalExpression)
	if !ok {
		return false
	}
	for _, r := range requested {
		if r == nil || r.IsPreserve() || memberSatisfiesOrdering(expr, r) {
			continue
		}
		if memberSatisfiesOrdering(expr, r.Mirrored()) {
			return true
		}
	}
	return false
}

// answerUngroupedOnEmpty makes an ungrouped GroupBy's aggregate plan answer
// its one row. An ungrouped aggregate answers exactly one row (the streaming
// aggregation emits it over an empty input; Java ranges a null-on-empty
// quantifier over an ungrouped GroupByExpression, LogicalOperator.java:462-464),
// while its index holds no entry for a table that never had a row. The scan is
// extended with the row of NULLs when it yields none, Java's
// `AISCAN … | ON EMPTY NULL`.
func answerUngroupedOnEmpty(
	aggPlan plans.RecordQueryPlan,
	groupBy *expressions.GroupByExpression,
) (plans.RecordQueryPlan, error) {
	if len(groupBy.GetGroupingKeys()) != 0 {
		return aggPlan, nil
	}
	row, ok := aggPlan.GetResultValue().Type().(*values.RecordType)
	if !ok {
		return nil, fmt.Errorf("ungrouped aggregate scan publishes %v, not a record", aggPlan.GetResultValue().Type())
	}
	nulls := make([]values.RecordConstructorField, len(row.Fields))
	for i, f := range row.Fields {
		nulls[i] = values.RecordConstructorField{Name: f.Name, Value: values.NewNullValue(f.FieldType)}
	}
	return plans.NewRecordQueryDefaultOnEmptyPlan(aggPlan, values.NewRawRecordConstructorValue(nulls...))
}

// groupByRowOverAggregateRow is the row an aggregate scan publishes for a
// GroupBy: the scan's slots, which carry the GroupBy's keys and aggregate in
// order (MatchesGroupBy matches them positionally), under the GroupBy's column
// names. The index names its columns after its own definition (COUNT(*)), the
// GroupBy after the query (COUNT(1)), and the Reference the plan joins states
// the GroupBy's row; Java's aggregate plan likewise publishes the result Value
// chosen at toEquivalentPlan. A width mismatch means the slots do not carry
// the GroupBy's row, and the candidate declines.
func groupByRowOverAggregateRow(row *values.RecordType, names []string) (*values.RecordType, bool) {
	if row == nil || len(row.Fields) != len(names) {
		return nil, false
	}
	fields := make([]values.Field, len(row.Fields))
	for i, f := range row.Fields {
		fields[i] = values.Field{Name: names[i], FieldType: f.FieldType, Ordinal: i}
	}
	exact, err := values.SnapshotExactType(values.NewRecordType(row.RecordName, row.Nullable, fields))
	if err != nil {
		return nil, false
	}
	resolved, ok := exact.Type().(*values.RecordType)
	return resolved, ok
}

// aggregatePredicatePartition is RFC-248's three-way partition of the
// GroupBy's inner filter for one aggregate-index candidate: the scan bounds
// the candidate's scan will apply, and the predicates left over that the
// aggregate row can still answer. A predicate that is neither — one that
// reads the aggregation input — makes the candidate decline, so a partition
// is only ever built for a candidate that can serve the whole filter.
type aggregatePredicatePartition struct {
	cand *AggregateIndexMatchCandidate
	// scanPrefix is the leading run ToScanPlan applies, keyed by the
	// candidate's aliases: equalities, then at most one inequality — the
	// candidate's ComputeBoundParameterPrefixMap over the per-column folds.
	scanPrefix map[values.CorrelationIdentifier]*predicates.ComparisonRange
	// residuals are the predicates the scan does not apply, still in the
	// query's own terms (over the base row, plus any outer parameters);
	// applyResiduals rewrites the base-row reads onto the row of the plan
	// being yielded and carries the outer parameters unchanged.
	residuals []aggregateFilterPredicate
}

// partitionAggregatePredicates sorts the flattened filter predicates into
// scan bounds and residuals, or reports that the candidate cannot serve the
// filter at all.
//
// Scan bounds: every comparison on a grouping column whose other side reads
// no field (groupColComparisonIndex) is FOLDED per column through
// foldPlaceholderBindings — `a > 5 AND a < 10` is one range; `a = 1 AND a > 0`
// and `a = 1 AND a = 2` bind the equality and re-apply the other comparison
// as a residual. The candidate's ComputeBoundParameterPrefixMap then truncates the
// folds to the leading run the scan can apply (equalities, then at most one
// inequality — the port of Java's MatchCandidate.computeBoundParameterPrefixMap);
// ToScanPlan itself breaks only on an ABSENT column, never after an inequality,
// so the TRUNCATED map is the one every scan site receives. Predicates on a
// column outside the run are residuals.
//
// Residuals: admitted by an allow-list of predicate kinds whose FieldValue
// leaves, enumerated, each name a grouping column (aggColumnMatches). A leaf
// that names anything else reads the aggregation input — no filter above a
// pre-aggregated stream can reconstruct it — and declines the candidate; a
// predicate with no FieldValue leaf is declined too, rather than admitted by a
// vacuous "every leaf is a grouping column". The rewrite onto the yielded row
// and the asserted bridge that a rewritten residual reads ONLY that row are in
// applyResiduals.
func partitionAggregatePredicates(
	cand *AggregateIndexMatchCandidate,
	filterPreds []aggregateFilterPredicate,
) (*aggregatePredicatePartition, bool) {
	partition := &aggregatePredicatePartition{cand: cand}
	perColumn := make([][]aggregateFilterPredicate, len(cand.groupCols))
	var others []aggregateFilterPredicate
	for _, fp := range filterPreds {
		if cp, ok := fp.pred.(*predicates.ComparisonPredicate); ok {
			if idx := groupColComparisonIndex(cp, cand.groupPaths, fp.input); idx >= 0 {
				perColumn[idx] = append(perColumn[idx], fp)
				continue
			}
		}
		others = append(others, fp)
	}

	// Per column, the same fold the value index runs at binding time
	// (foldPlaceholderBindings): an equality takes the range and the rest of
	// the column's comparisons are residuals over the aggregate row; otherwise
	// every distinct inequality accumulates. `a = 1 AND a > 0` binds `a = 1`
	// with `a > 0` in bucket 2; `a = 1 AND a = 2` binds the first equality
	// and re-applies the second, which reads no group.
	folded := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	columnResiduals := make([][]aggregateFilterPredicate, len(perColumn))
	for idx, comparisons := range perColumn {
		if len(comparisons) == 0 {
			continue
		}
		bound := make([]placeholderBinding, 0, len(comparisons))
		for _, fp := range comparisons {
			cp := fp.pred.(*predicates.ComparisonPredicate)
			bound = append(bound, placeholderBinding{pred: fp.pred, cp: cp, comparison: &cp.Comparison})
		}
		merged, members := foldPlaceholderBindings(bound)
		if len(members) == 0 {
			columnResiduals[idx] = comparisons
			continue
		}
		folded[cand.aliases[idx]] = merged
		carried := make(map[predicates.QueryPredicate]struct{}, len(members))
		for _, m := range members {
			carried[m.pred] = struct{}{}
		}
		for _, fp := range comparisons {
			if _, isMember := carried[fp.pred]; !isMember {
				columnResiduals[idx] = append(columnResiduals[idx], fp)
			}
		}
	}
	partition.scanPrefix = cand.ComputeBoundParameterPrefixMap(folded)
	// A bound the scan cannot execute — a FLOAT range whose tuple order is not
	// the comparator's, an unknown physical type, a constant NaN — is not a
	// reason to decline the candidate: the predicate filters whole groups just
	// as well above the scan. Peel the run from its end (the ineligible bound
	// is on the last column of the run or before it) until what remains is
	// eligible; the peeled columns' predicates become residuals.
	for len(partition.scanPrefix) > 0 && !candidateBindingRangesEligible(cand, partition.scanPrefix) {
		last := -1
		for idx, alias := range cand.aliases {
			if _, bound := partition.scanPrefix[alias]; bound {
				last = idx
			}
		}
		if last < 0 {
			// Only the candidate's own aliases are ever keys here; a map with
			// entries and no such key would be a construction error, and the
			// safe reading of it is "bind nothing".
			partition.scanPrefix = map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
			break
		}
		delete(partition.scanPrefix, cand.aliases[last])
	}

	var residuals []aggregateFilterPredicate
	for idx, comparisons := range perColumn {
		if _, bound := partition.scanPrefix[cand.aliases[idx]]; bound {
			// The column's range is in the run: only the comparisons the fold
			// did not carry are residuals.
			residuals = append(residuals, columnResiduals[idx]...)
			continue
		}
		residuals = append(residuals, comparisons...)
	}
	residuals = append(residuals, others...)
	for _, fp := range residuals {
		if !residualOverGroupingColumns(fp.pred, cand.groupPaths, fp.input) {
			return nil, false
		}
	}
	partition.residuals = residuals
	return partition, true
}

// residualOverGroupingColumns is bucket 2's admission: a predicate kind the
// rewrite walks, with at least one FieldValue leaf rooted at the aggregation
// input, every such leaf naming a grouping column. A field rooted elsewhere is
// an outer parameter and is neither a grouping column nor a reason to
// decline: it stays as it is. A bare quantified/object leaf rooted at the
// input is not a column read the aggregate row can serve and declines; so
// does a kind ReplaceValues would pass through unchanged, since that is
// indistinguishable from a rewrite that did nothing.
func residualOverGroupingColumns(
	p predicates.QueryPredicate,
	groupPaths [][]string,
	input values.CorrelationIdentifier,
) bool {
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		if isDistanceRankComparison(pred.Comparison.Type) {
			// A vector-distance rank is a scan construct, not a row predicate;
			// there is nothing to evaluate above a pre-aggregated stream.
			return false
		}
		// leaf kind: checked below
	case *predicates.ValuePredicate:
		// leaf kind: checked below
	case *predicates.AndPredicate:
		for _, sub := range pred.SubPredicates {
			if !residualOverGroupingColumns(sub, groupPaths, input) {
				return false
			}
		}
		return true
	case *predicates.OrPredicate:
		for _, sub := range pred.SubPredicates {
			if !residualOverGroupingColumns(sub, groupPaths, input) {
				return false
			}
		}
		return true
	case *predicates.NotPredicate:
		return residualOverGroupingColumns(pred.Child, groupPaths, input)
	default:
		return false
	}
	leaves := 0
	allGrouping := true
	for _, v := range predicateEmbeddedValues(p) {
		values.WalkValue(v, func(node values.Value) bool {
			if !allGrouping {
				return false
			}
			if _, isField := values.AsFieldValue(node); isField {
				if !rootedAt(node, input) {
					return false // an outer parameter: carried, not a grouping read
				}
				leaves++
				if groupingColumnIndex(node, groupPaths, input) < 0 {
					allGrouping = false
				}
				return false
			}
			switch node.(type) {
			case values.QuantifiedObjectValue, *values.ObjectValue:
				if rootedAt(node, input) {
					// A whole-row leaf is not a grouping column.
					allGrouping = false
				}
				return false
			}
			return true
		})
	}
	return allGrouping && leaves > 0
}

// rootedAt reports whether v reads exactly the quantifier alias — the
// aggregation input for a filter conjunct — and nothing else.
func rootedAt(v values.Value, alias values.CorrelationIdentifier) bool {
	correlated := values.GetCorrelatedToOfValue(v)
	_, reads := correlated[alias]
	return reads && len(correlated) == 1
}

// predicateEmbeddedValues lists the Value trees a leaf predicate embeds, the
// same trees ReplaceValues rewrites.
func predicateEmbeddedValues(p predicates.QueryPredicate) []values.Value {
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		out := []values.Value{pred.Operand}
		if pred.Comparison.Operand != nil {
			out = append(out, pred.Comparison.Operand)
		}
		return out
	case *predicates.ValuePredicate:
		return []values.Value{pred.Value}
	}
	return nil
}

// groupingColumnIndex is the grouping column a field read names — a field
// rooted at the aggregation input, matched by accessor path as the bound
// builder matches — or -1. A same-named field rooted at another quantifier is
// an outer parameter, not a grouping column.
func groupingColumnIndex(v values.Value, groupPaths [][]string, input values.CorrelationIdentifier) int {
	if !rootedAt(v, input) {
		return -1
	}
	for i, path := range groupPaths {
		if aggColumnMatches(v, path) {
			return i
		}
	}
	return -1
}

// applyResiduals wraps plan in ONE PredicatesFilter carrying the residuals
// rewritten onto plan's row — grouping column i is ordinal i of the row the
// aggregate scan and the multi-aggregate intersection both flow — and asserts the bridge: a rewritten residual must read plan's
// alias, must no longer read the aggregation input, and must read exactly
// the outer parameters it read before (an outer field is carried, never
// rewritten). A leaf the rewrite did not reach, or a predicate kind
// ReplaceValues passed through unchanged, still names the input and fails
// that assertion, so it declines rather than filtering on a row it does not
// read. With no residuals the plan is returned as is.
func (partition *aggregatePredicatePartition) applyResiduals(
	plan plans.RecordQueryPlan,
) (plans.RecordQueryPlan, bool) {
	if len(partition.residuals) == 0 {
		return plan, true
	}
	innerQ := plans.QuantifierOverPlan(plan)
	root, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, false
	}
	groupPaths := partition.cand.groupPaths
	rewritten := make([]predicates.QueryPredicate, 0, len(partition.residuals))
	for _, residual := range partition.residuals {
		input := residual.input
		failed := false
		moved := predicates.ReplaceValues(residual.pred, func(v values.Value) values.Value {
			if _, isField := values.AsFieldValue(v); !isField || !rootedAt(v, input) {
				return v
			}
			idx := groupingColumnIndex(v, groupPaths, input)
			if idx < 0 {
				failed = true
				return v
			}
			replacement, resolveErr := values.ResolveFieldOrdinals(root, []int{idx})
			if resolveErr != nil {
				failed = true
				return v
			}
			return replacement
		})
		if failed {
			return nil, false
		}
		if !residualBridgeHolds(residual.pred.GetCorrelatedTo(), moved.GetCorrelatedTo(), input, root.Correlation()) {
			return nil, false
		}
		rewritten = append(rewritten, moved)
	}
	filtered, err := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(innerQ, rewritten)
	if err != nil {
		return nil, false
	}
	return filtered, true
}

// residualBridgeHolds is applyResiduals' assertion: after the rewrite the
// residual reads the yielded plan's alias, no longer reads the aggregation
// input, and reads exactly the other correlations (outer parameters) it read
// before — no more, no fewer.
func residualBridgeHolds(
	before, after map[values.CorrelationIdentifier]struct{},
	input, root values.CorrelationIdentifier,
) bool {
	if _, readsRoot := after[root]; !readsRoot {
		return false
	}
	if _, stillReadsInput := after[input]; stillReadsInput {
		return false
	}
	for alias := range before {
		if alias == input {
			continue
		}
		if _, kept := after[alias]; !kept {
			return false
		}
	}
	for alias := range after {
		if alias == root {
			continue
		}
		if _, had := before[alias]; !had {
			return false
		}
	}
	return true
}

// rekeyScanPrefix carries the owner's truncated scan bounds onto another leg
// over the same grouping columns, alias by POSITION. Position is the right key
// because the multi-aggregate intersection checks its legs' groupCols name by
// name in order before it gets here.
func rekeyScanPrefix(
	prefix map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	owner, target *AggregateIndexMatchCandidate,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	if owner == target {
		return prefix
	}
	out := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, len(prefix))
	for i, alias := range owner.aliases {
		if cr, ok := prefix[alias]; ok && i < len(target.aliases) {
			out[target.aliases[i]] = cr
		}
	}
	return out
}

// groupColComparisonIndex returns the index of the grouping column that cp is
// a scan-bindable comparison on — an equality, an inequality, IS NULL / IS NOT
// NULL or STARTS_WITH, exactly the value-index range set plus the two NULL
// comparisons ComparisonRange.Merge classifies (IS NULL an equality on the
// [null] key, IS NOT NULL the (null, +inf) range); NOT the vector
// DISTANCE_RANK bounds the index-match gate also admits, which a
// ComparisonRange cannot hold — or -1 if cp is not such a comparison whose LHS
// is a grouping column. Shared by the partition's bound fold and, through
// groupingColumnIndex, its residual rewrite, so the two cannot drift — the
// drift between guard and consumer is what let the original residual-drop bug
// ship.
func groupColComparisonIndex(
	cp *predicates.ComparisonPredicate,
	groupPaths [][]string,
	input values.CorrelationIdentifier,
) int {
	if !aggregateScanBindableComparison(cp.Comparison.Type) {
		return -1
	}
	fv, ok := values.AsFieldValue(cp.Operand)
	if !ok {
		return -1
	}
	// The comparand (RHS) must be a constant the scan can bind to — a literal or
	// parameter — NOT a value that reads a record field. `region = status`
	// correlates two columns of the SAME record and can never be an index bound;
	// it stays a residual (sound when both are grouping columns, declined
	// otherwise). Without this the field comparand makes Merge fail to bind
	// while the predicate would be counted as consumed, silently dropping it
	// (wrong rows).
	if valueReadsField(cp.Comparison.Operand) {
		return -1
	}
	return groupingColumnIndex(fv, groupPaths, input)
}

// aggregateScanBindableComparison is the comparison-type set the group-key
// scan can execute as a range: the value-index range set (=, <, <=, >, >=,
// STARTS_WITH) plus IS NULL / IS NOT NULL.
func aggregateScanBindableComparison(t predicates.ComparisonType) bool {
	if isScanRangeCompatible(t) {
		return true
	}
	return t == predicates.ComparisonIsNull || t == predicates.ComparisonIsNotNull
}

// isDistanceRankComparison reports a vector DISTANCE_RANK bound, which is a
// scan construct of a vector candidate and never a row predicate.
func isDistanceRankComparison(t predicates.ComparisonType) bool {
	switch t {
	case predicates.ComparisonDistanceRankEquals,
		predicates.ComparisonDistanceRankLessThan,
		predicates.ComparisonDistanceRankLessThanOrEq:
		return true
	}
	return false
}

// valueReadsField reports whether v references a record field anywhere in
// its tree (i.e. it is not a pure literal/parameter constant the index scan can
// bind to). A bare literal, a parameter, or a cast/arithmetic over constants
// returns false; anything containing a FieldValue returns true.
func valueReadsField(v values.Value) bool {
	if v == nil {
		return false
	}
	if _, ok := values.AsFieldValue(v); ok {
		return true
	}
	for _, c := range v.Children() {
		if valueReadsField(c) {
			return true
		}
	}
	return false
}

// aggregateFilterPredicate is one conjunct of the GroupBy's inner filter with
// the alias of the quantifier that filter reads — the aggregation INPUT. A
// field the predicate reads is a grouping column only when it is rooted at
// that alias: a same-named field rooted anywhere else is an OUTER parameter
// of a correlated query, which the residual must carry unchanged, never
// rewrite onto the aggregate row (that would turn `o.region = c.region` into
// `group.region = group.region` and pass every group).
type aggregateFilterPredicate struct {
	pred  predicates.QueryPredicate
	input values.CorrelationIdentifier
}

// extractInnerFilterPredicates returns the predicates of the inner
// Reference's Filter members, each with the alias of the filter's inner
// quantifier. The one list partitionAggregatePredicates reads, so no second
// reader can disagree on what a filter holds. Returns nil if no filter
// predicates are found.
//
// The Reference holds EQUIVALENT members, and several of them are filters:
// the base-row filter over the scan and, once index matching has run, a
// compensation filter over each index scan re-applying the conjuncts that
// scan did not bind (`Filter([a > 0], IndexScan(IDX_A, [= 1]))` beside
// `Filter([a = 1, a > 0], Scan)`). Their predicate sets are subsets of one
// conjunction, so the union over members is that conjunction — read once
// per distinct predicate. Without the dedup every compensation member
// contributed its copy, the per-column fold made each copy a residual, and
// `a = 1 AND a > 0 GROUP BY a` carried four residuals above the aggregate
// scan and lost on cost to a full scan. A filter's list is already its
// conjunction (the constructor lifts every top-level AND).
func extractInnerFilterPredicates(ref *expressions.Reference) []aggregateFilterPredicate {
	var result []aggregateFilterPredicate
	for _, m := range ref.Members() {
		f, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		input := f.GetInner().GetAlias()
		for _, p := range f.GetPredicates() {
			dup := false
			for _, existing := range result {
				if existing.input == input && predicates.StructurallyEqual(existing.pred, p) {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			result = append(result, aggregateFilterPredicate{pred: p, input: input})
		}
	}
	return result
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

// aggregateIndexOutputType is the exact row schema emitted by one aggregate
// index cursor: grouping keys in logical order followed by the aggregate
// result. Optional candidates whose metadata cannot prove every field type are
// declined instead of minting UnknownType into an executable QOV.
func aggregateIndexOutputType(cand *AggregateIndexMatchCandidate) (*values.RecordType, bool) {
	if cand == nil {
		return nil, false
	}
	groupTypes := cand.GetKeyComponentTypes()
	if len(groupTypes) != len(cand.groupCols) {
		return nil, false
	}
	fields := make([]values.Field, 0, len(groupTypes)+1)
	for i, groupType := range groupTypes {
		if _, err := values.SnapshotExactType(groupType); err != nil {
			return nil, false
		}
		fields = append(fields, values.Field{
			Name: cand.groupCols[i], FieldType: groupType, Ordinal: i,
		})
	}

	aggregateType, ok := aggregateIndexResultType(cand)
	if !ok {
		return nil, false
	}
	fields = append(fields, values.Field{
		Name:      aggregateFlowedColumnName(cand.aggFunction.String(), cand.aggColumn),
		FieldType: aggregateType,
		Ordinal:   len(fields),
	})
	result := &values.RecordType{Fields: fields}
	exact, err := values.SnapshotExactType(result)
	if err != nil {
		return nil, false
	}
	resolved, ok := exact.Type().(*values.RecordType)
	return resolved, ok
}

func aggregateIndexResultType(cand *AggregateIndexMatchCandidate) (values.Type, bool) {
	switch cand.aggFunction {
	case expressions.AggBitmapConstructAgg:
		return values.NullableBytes, true
	case expressions.AggCount:
		return values.NullableLong, true
	case expressions.AggAvg:
		return values.NullableDouble, aggregateIndexOperandIsNumeric(cand)
	case expressions.AggSum, expressions.AggMin, expressions.AggMax:
		operand, ok := aggregateIndexOperandType(cand)
		if !ok {
			return nil, false
		}
		if _, ok := values.JavaAggregateResultCode(cand.aggFunction.String(), operand.Code()); !ok {
			return nil, false
		}
		result := values.WithNullability(operand, true)
		if _, err := values.SnapshotExactType(result); err != nil {
			return nil, false
		}
		return result, true
	case expressions.AggMinEver, expressions.AggMaxEver:
		// The index-only aggregates keep their operand's type, nullable
		// (IndexOnlyAggregateValue; the GroupBy row types them the same way).
		operand, ok := aggregateIndexOperandType(cand)
		if !ok {
			return nil, false
		}
		result := values.WithNullability(operand, true)
		if _, err := values.SnapshotExactType(result); err != nil {
			return nil, false
		}
		return result, true
	default:
		return nil, false
	}
}

func aggregateIndexOperandIsNumeric(cand *AggregateIndexMatchCandidate) bool {
	operand, ok := aggregateIndexOperandType(cand)
	if !ok {
		return false
	}
	_, ok = values.JavaAggregateResultCode(cand.aggFunction.String(), operand.Code())
	return ok
}

func aggregateIndexOperandType(cand *AggregateIndexMatchCandidate) (values.Type, bool) {
	field, ok := values.LookupFieldPathUnique(cand.GetBaseRowType(), cand.aggPath)
	if !ok || field.FieldType == nil {
		return nil, false
	}
	exact, err := values.SnapshotExactType(field.FieldType)
	if err != nil {
		return nil, false
	}
	return exact.Type(), true
}

func aggregateRowQOV(rowType values.Type) (values.QuantifiedObjectValue, bool) {
	qov, err := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier(), rowType)
	return qov, err == nil
}

// aggregateMergedRowQOV describes the actual flat carrier consumed by the
// multi-intersection result program. Its field order is the executor's child
// order, and each child contributes its complete exact aggregate row.
func aggregateMergedRowQOV(children []plans.RecordQueryPlan) (values.QuantifiedObjectValue, bool) {
	fields := make([]values.Field, 0)
	for _, child := range children {
		if child == nil {
			return nil, false
		}
		row, ok := child.GetResultType().(*values.RecordType)
		if !ok {
			return nil, false
		}
		for _, field := range row.Fields {
			if _, err := values.SnapshotExactType(field.FieldType); err != nil {
				return nil, false
			}
			fields = append(fields, values.Field{
				Name: field.Name, FieldType: field.FieldType, Ordinal: len(fields),
			})
		}
	}
	merged := &values.RecordType{Fields: fields}
	if _, err := values.SnapshotExactType(merged); err != nil {
		return nil, false
	}
	return aggregateRowQOV(merged)
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
	innerFilterPreds []aggregateFilterPredicate,
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
		if !slices.EqualFunc(mc.groupPaths, matched[0].groupPaths, func(x, y []string) bool {
			return slices.EqualFunc(x, y, eqFold)
		}) {
			return
		}
	}

	// The same partition as the single-aggregate path, over the first leg's
	// candidate (every leg shares its grouping columns): scan bounds for the
	// legs, residuals above the merge, or a decline.
	partition, ok := partitionAggregatePredicates(matched[0], innerFilterPreds)
	if !ok {
		return
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
	// The legs and the merge run in one direction; see reverseServesARequest.
	build := func(reverse bool) *plans.RecordQueryMultiIntersectionOnValuesPlan {
		childPlans := make([]plans.RecordQueryPlan, len(matched))
		for i, mc := range matched {
			prefix := rekeyScanPrefix(partition.scanPrefix, matched[0], mc)
			if !candidateBindingRangesEligible(mc, prefix) {
				return nil
			}
			sp := mc.ToScanPlan(prefix, reverse)
			idxPlan := extractIndexPlan(sp)
			if idxPlan == nil {
				return nil
			}
			// Multi-intersection is a merge, not a hash match: every child must
			// physically stream in the complete logical grouping-key order. A
			// permuted grouping layout is not contiguous, and an unbound raw
			// FLOAT/DOUBLE (or Unknown) coordinate has tuple NaN regions that are
			// not congruent with the query comparator. Decline the optimization;
			// the ordinary streaming aggregate remains correct.
			if mc.GetPhysicalGroupingPrefixCount() != len(groupCols) ||
				properties.PhysicalOrderingPrefixLength(
					idxPlan.GetScanComparisons(), idxPlan.GetKeyComponentTypes(), len(groupCols),
				) != len(groupCols) {
				return nil
			}
			var recordTypeName string
			if rts := mc.GetRecordTypes(); len(rts) > 0 {
				recordTypeName = rts[0]
			}
			resultType, ok := aggregateIndexOutputType(mc)
			if !ok {
				return nil
			}
			aggPlan, err := plans.NewRecordQueryAggregateIndexPlan(
				idxPlan, recordTypeName, resultType, mc.aggFunction.String(),
			)
			if err != nil {
				call.Fail(err)
				return nil
			}
			childPlans[i] = aggPlan.WithGroupColumns(mc.groupCols, mc.aggColumn).
				WithCandidateGroupingCount(len(mc.groupCols)).
				WithEntryReader(mc.indexEntryToRecordValue(resultType)).
				WithColumnPaths(mc.groupPaths, mc.aggPath).
				WithGroupColumnLayout(mc.GetBaseRowType())
		}

		// Comparison key = grouping column FieldValues. The aggregate-index
		// cursor flows each grouping column under its (uppercased) metadata
		// name, so the comparison key matches identical group values across the
		// per-aggregate streams. With a WHERE-equality prefix (cat='books')
		// each stream emits exactly that one group; the keys still match.
		// Each child row's layout is [groupCols..., FUNC(col)] (the
		// aggregateIndexCursor's posType), so a grouping-column comparison key IS
		// slot i — baked at plan time, read positionally per child row.
		comparisonRoot, ok := aggregateRowQOV(childPlans[0].GetResultType())
		if !ok {
			return nil
		}
		comparisonKey := make([]values.Value, len(groupCols))
		for i := range groupCols {
			resolved, resolveErr := values.ResolveFieldOrdinals(comparisonRoot, []int{i})
			if resolveErr != nil {
				call.Fail(resolveErr)
				return nil
			}
			comparisonKey[i] = resolved
		}

		// Result value = Record(groupCol0, ..., agg0, agg1, ...) under the
		// GroupBy's column names, so the merge publishes the GroupBy's row.
		// Grouping columns are identical across all streams; each aggregate is
		// picked up from its respective stream. Mirrors Java's
		// computeIntersectionResultValue().
		// The merge cursor evaluates the result value against the CONCATENATION
		// of the matched child rows, each child spanning len(groupCols)+1 slots
		// ([groupCols..., FUNC(col)]). Child i's aggregate sits at its span's last
		// slot. Baked, both read positionally.
		childWidth := len(groupCols) + 1
		mergedRoot, ok := aggregateMergedRowQOV(childPlans)
		if !ok {
			return nil
		}
		outputNames := gb.OutputColumnNames()
		if len(outputNames) != len(groupCols)+len(aggs) {
			return nil
		}
		fields := make([]values.RecordConstructorField, 0, len(groupCols)+len(aggs))
		for i := range groupCols {
			groupValue, resolveErr := values.ResolveFieldOrdinals(mergedRoot, []int{i})
			if resolveErr != nil {
				call.Fail(resolveErr)
				return nil
			}
			fields = append(fields, values.RecordConstructorField{
				Name:  outputNames[i],
				Value: groupValue,
			})
		}
		for i := range aggs {
			pickUp, resolveErr := values.ResolveFieldOrdinals(
				mergedRoot, []int{i*childWidth + len(groupCols)})
			if resolveErr != nil {
				call.Fail(resolveErr)
				return nil
			}
			fields = append(fields, values.RecordConstructorField{
				Name:  outputNames[len(groupCols)+i],
				Value: pickUp,
			})
		}
		// The GroupBy's native row is positional: a repeated aggregate keeps its name.
		resultValue := values.NewRawRecordConstructorValue(fields...)

		// Memoize each leg and hand the plan real quantifiers. Passing nil left
		// a two-leg intersection with NO edges in the memo at all: both children
		// executed while the optimizer could see neither, so nothing costed the
		// legs and nothing could rewrite through them (6 unreachable edges,
		// RFC-183 §14, ReasonNoQuantifier).
		//
		// MemoizeFinalExpression, not MemoizeExpression: a fresh singleton per leg.
		// The legs here differ (SUM vs COUNT over different indexes) so interning
		// would not collapse them today, but this is the construction that DID
		// collapse the recursive-DFS legs, and the cost of being explicit is zero.
		childQuants := make([]expressions.Quantifier, len(childPlans))
		for i, cp := range childPlans {
			childQuants[i] = expressions.NewPhysicalQuantifier(
				call.MemoizeFinalExpression(&scanPlanExpression{plan: cp}))
		}

		// The multi-intersection carries its stream edges directly (RFC-184 W2).
		merge, err := plans.NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
			childQuants, comparisonKey, resultValue,
		)
		if err != nil {
			call.Fail(err)
			return nil
		}
		return merge.WithReverse(reverse)
	}
	for _, reverse := range []bool{false, true} {
		merge := build(reverse)
		if merge == nil {
			return
		}
		filtered, ok := partition.applyResiduals(merge)
		if !ok {
			return
		}
		logicalPlan, err := answerUngroupedOnEmpty(filtered, gb)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(logicalPlan)
		if reverse || !reverseServesARequest(merge, call.GetRequestedOrderings()) {
			return
		}
	}
}

var _ ExpressionRule = (*AggregateDataAccessRule)(nil)
