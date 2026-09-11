package cascades

import (
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

		// RFC-209 §5.3(b)/(c). A grouped SUM or COUNT(col) index cannot answer on
		// its own: its stored zero is byte-identical whether the group cancelled
		// to zero or was vacated, and an all-NULL group has no entry at all. It is
		// therefore companion-joined when a readable COUNT(*) over the same
		// grouping key and predicate exists, and DECLINED otherwise so planning
		// falls back to streaming aggregation over base rows.
		//
		// Declining is the fail-closed direction and the only acceptable one:
		// the alternative is the index-backed plan that is fast and wrong.
		if aggCand.NeedsGroupExistenceCompanion() {
			companion := findGroupCountCompanion(aggCand, candidates)
			if companion == nil {
				continue
			}
			mergePlan := buildGroupExistenceMerge(call, companion, aggCand, partition.scanPrefix)
			if mergePlan == nil {
				continue
			}
			filtered, ok := partition.applyResiduals(mergePlan)
			if !ok {
				continue
			}
			logicalPlan, err := projectAggregateResultToGroupBy(filtered, gb)
			if err != nil {
				call.Fail(err)
				return
			}
			call.Yield(logicalPlan)
			singleMatched = true
			continue
		}

		if !candidateBindingRangesEligible(aggCand, partition.scanPrefix) {
			continue
		}
		scanPlan := aggCand.ToScanPlan(partition.scanPrefix, false)
		idxPlan := extractIndexPlan(scanPlan)
		if idxPlan == nil {
			continue
		}

		var recordTypeName string
		if rts := aggCand.GetRecordTypes(); len(rts) > 0 {
			recordTypeName = rts[0]
		}
		resultType, ok := aggregateIndexOutputType(aggCand)
		if !ok {
			continue
		}
		aggPlan, err := plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, resultType, aggCand.aggFunction.String(),
		)
		if err != nil {
			call.Fail(err)
			return
		}
		aggPlan = aggPlan.WithGroupColumns(aggCand.groupCols, aggCand.aggColumn).
			WithGroupColumnLayout(aggCand.GetBaseRowType()).
			WithLiveGroupsOnly(dropsVacatedGroups(aggCand))
		filtered, ok := partition.applyResiduals(aggPlan)
		if !ok {
			continue
		}
		logicalPlan, err := projectAggregateResultToGroupBy(filtered, gb)
		if err != nil {
			call.Fail(err)
			return
		}

		// The yielded member must state the exact result type of the Reference it
		// joins. projectAggregateResultToGroupBy is what guarantees that, and it
		// publishes the GroupBy row through an ordinal projection ONLY when the
		// leaf's own row does not already carry those column names — which, on this
		// corpus, it always does. See its doc for the census.
		call.Yield(logicalPlan)
		singleMatched = true
	}
	if singleMatched {
		return
	}

	// Path 2: multi-aggregate intersection — multiple candidates, each
	// covering one of the GroupBy's aggregates with identical grouping.
	tryMultiAggregateIntersection(call, gb, candidates, scanTypes, innerFilterPreds)
}

func projectAggregateResultToGroupBy(
	aggPlan plans.RecordQueryPlan,
	groupBy *expressions.GroupByExpression,
) (plans.RecordQueryPlan, error) {
	innerQ := plans.QuantifierOverPlan(aggPlan)
	root, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}
	outputNames := expressions.GroupByOutputColumnNames(
		groupBy.GetGroupingKeys(), groupBy.GetAggregates())
	if aggregateLeafPublishesGroupByRow(root, outputNames) {
		// Nothing to publish: the leaf's row already IS the GroupBy's row, so the
		// projection would map ordinal i to ordinal i under the name the column
		// already has. Wrapping it anyway put a per-group operator in the plan for
		// a rename that is not happening, and it is not free downstream either: a
		// HAVING filter above it then reads the PROJECTION's row, which forces the
		// projection to be materialized BELOW the filter and the same list to be
		// projected again above it. Measured on the 1M stress suite,
		// `GROUP BY customer HAVING SUM(amount) > n` ran 1.88x that way.
		return aggPlan, nil
	}
	projected := make([]values.Value, len(outputNames))
	for i := range projected {
		projected[i], err = values.ResolveFieldOrdinals(root, []int{i})
		if err != nil {
			return nil, err
		}
	}
	return plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
		projected, nil, nil, outputNames, innerQ)
}

// aggregateLeafPublishesGroupByRow reports whether the aggregate leaf's own row
// is already, column for column, the row the GroupBy publishes.
//
// The projection this decides against is an ORDINAL identity — slot i reads
// ordinal i — so the only thing it can change is the column NAMES. Equal names in
// equal order therefore make it a no-op, and the comparison is EXACT rather than
// case-insensitive on purpose: a case-only difference is still a difference the
// driver's column labels would show, and eliding it would answer a query with
// names the GroupBy did not choose.
//
// THE RENAME ARM IS NOT REACHED BY THE CORPUS. Censused over the explain-differ
// dump at 2540 queries, the guard is consulted 32 times and answers "already
// published" every time: an aggregate ALIAS and a reordered select list are both
// applied by the OUTER projection, so the GroupBy's own output names stay the
// canonical ones the leaf already carries. The arm is kept rather than made an
// error because a name mismatch would otherwise cost the query its columns'
// labels, which is worse than an extra operator — but it is untested by the
// corpus, so aggregate_projection_elision_test.go drives BOTH directions of this
// predicate directly. If that census ever reports a non-identity firing, this arm
// is what will run, and it has not run in anger.
func aggregateLeafPublishesGroupByRow(
	root values.QuantifiedObjectValue,
	outputNames []string,
) bool {
	record, isRecord := values.SharedFlowedType(root).(*values.RecordType)
	if !isRecord || record == nil || len(record.Fields) != len(outputNames) {
		return false
	}
	for i := range outputNames {
		if record.Fields[i].Name != outputNames[i] {
			return false
		}
	}
	return true
}

// dropsVacatedGroups reports whether a scan of this candidate may drop entries
// whose stored value is zero (RFC-209 §5.3(a)).
//
// It holds for a GROUPED COUNT(*) index and nothing else. There, the index
// being scanned is already the group-existence oracle: the stored value is the
// group's row count, a live group's row count is never zero, so a zero can only
// be the residue an atomic ADD left behind when the group was emptied. The drop
// is exact and needs no second stream.
//
// It does NOT hold for SUM or COUNT(col), where a live group legitimately
// answers zero (values cancelling out, or every value NULL) — those need a
// companion COUNT(*) to decide existence. Nor for the ungrouped spelling, whose
// single group exists whether or not the table has rows.
func dropsVacatedGroups(cand *AggregateIndexMatchCandidate) bool {
	return cand.countsRows && len(cand.groupCols) > 0
}

// findGroupCountCompanion returns the candidate whose index can act as owner's
// group-existence source (RFC-209 §5.2), or nil.
//
// Discovery is STRUCTURAL and re-runs against the current candidate list at
// plan time: a COUNT(*) index over the same record types, the same normalized
// grouping key expression and the same sparse predicate. The name is never
// consulted, so a user-declared COUNT(*) serves exactly as well as one the DDL
// emitted, and a companion written by an older binary under a different name
// still matches.
//
// READABILITY comes for free and must: the candidate list has already been
// filtered to indexes the store can read, so an index in WRITE_ONLY, disabled
// or mid-backfill is not here to be found. That matters more than it looks —
// such an index has a PARTIAL key set, and driving the merge from it would drop
// LIVE groups, which is a brand-new wrong answer strictly worse than today's
// phantom.
func findGroupCountCompanion(owner *AggregateIndexMatchCandidate, candidates []MatchCandidate) *AggregateIndexMatchCandidate {
	if len(owner.groupingSignature) == 0 {
		// No derivable signature declines every match rather than guessing.
		return nil
	}
	for _, cand := range candidates {
		ac, ok := cand.(*AggregateIndexMatchCandidate)
		if !ok || ac == owner || !ac.countsRows {
			continue
		}
		if string(ac.groupingSignature) != string(owner.groupingSignature) {
			continue
		}
		if !samePredicateSignature(ac.predicateSignature, owner.predicateSignature) {
			continue
		}
		if !sameRecordTypeNames(ac.GetRecordTypes(), owner.GetRecordTypes()) {
			continue
		}
		if len(ac.groupCols) != len(owner.groupCols) {
			// The signature already implies this; the physical column list is what
			// the merge's comparison key is built from, so disagreement here would
			// bake ordinals against the wrong width.
			continue
		}
		return ac
	}
	return nil
}

// OpaquePredicateSignatureMarker is the signature a sparse index carries when
// its predicate is a programmatic Go closure rather than a serialized proto.
//
// It lives here, not in the record layer, because the record layer imports the
// planner and not the other way round — and both sides must agree on the exact
// bytes or companion matching silently changes meaning. The record layer's
// PredicateSignature produces it; SamePredicateSignature there and
// samePredicateSignature here both refuse it.
const OpaquePredicateSignatureMarker = "p!opaque"

// samePredicateSignature reports whether two sparse-predicate signatures denote
// the same row population.
//
// An opaque signature is unmatchable in principle — the predicate does not
// serialize, so there is nothing to compare — and therefore declines against
// everything, including another opaque one.
func samePredicateSignature(a, b []byte) bool {
	if string(a) == OpaquePredicateSignatureMarker || string(b) == OpaquePredicateSignatureMarker {
		return false
	}
	return string(a) == string(b)
}

// sameRecordTypeNames reports set equality. A companion over a different (or
// wider) set of record types counts different rows, so equality — not overlap —
// is required.
func sameRecordTypeNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, n := range a {
		seen[n] = struct{}{}
	}
	for _, n := range b {
		if _, ok := seen[n]; !ok {
			return false
		}
	}
	return true
}

// buildGroupExistenceMerge builds the two-stream outer merge of RFC-209
// §5.3(b): the companion COUNT(*) scan drives the group set, the owner's
// aggregate scan supplies the value, and a group the owner has no entry for
// gets the aggregate's empty-group identity.
//
// Returns nil when the merge cannot be built, in which case the caller declines
// the candidate entirely and planning falls back to streaming aggregation. The
// resolved companion is a precondition of construction, so there is no path
// that builds this plan and then validates it.
func buildGroupExistenceMerge(
	call *ExpressionRuleCall,
	companion, owner *AggregateIndexMatchCandidate,
	ownerScanPrefix map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) plans.RecordQueryPlan {
	groupCols := owner.groupCols
	if len(groupCols) == 0 {
		return nil
	}

	// The companion carries the same grouping columns, so the owner's scan
	// bounds — already truncated to the leading run the scan applies — apply
	// to it verbatim once re-keyed to its aliases; both streams then emit
	// exactly the groups the query asked for.
	legs := []*AggregateIndexMatchCandidate{companion, owner}
	childPlans := make([]plans.RecordQueryPlan, len(legs))
	for i, cand := range legs {
		prefix := rekeyScanPrefix(ownerScanPrefix, owner, cand)
		if !candidateBindingRangesEligible(cand, prefix) {
			return nil
		}
		idxPlan := extractIndexPlan(cand.ToScanPlan(prefix, false))
		if idxPlan == nil {
			return nil
		}
		// A merge, not a hash match: both streams must physically stream in the
		// complete logical grouping-key order, or the driven advance compares
		// keys that are not congruent and silently mis-pairs groups.
		if cand.GetPhysicalGroupingPrefixCount() != len(groupCols) ||
			properties.PhysicalOrderingPrefixLength(
				idxPlan.GetScanComparisons(), idxPlan.GetKeyComponentTypes(), len(groupCols),
			) != len(groupCols) {
			return nil
		}
		var recordTypeName string
		if rts := cand.GetRecordTypes(); len(rts) > 0 {
			recordTypeName = rts[0]
		}
		resultType, ok := aggregateIndexOutputType(cand)
		if !ok {
			return nil
		}
		aggPlan, err := plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, resultType, cand.aggFunction.String(),
		)
		if err != nil {
			call.Fail(err)
			return nil
		}
		childPlans[i] = aggPlan.WithGroupColumns(cand.groupCols, cand.aggColumn).
			WithGroupColumnLayout(cand.GetBaseRowType()).
			// The COMPANION carries the vacated-group drop, and it is load-bearing
			// here: the companion is itself subject to the over-approximation
			// defect, so without the drop its own zero-valued keys would put every
			// vacated group straight back into the merged group set.
			WithLiveGroupsOnly(dropsVacatedGroups(cand))
	}

	childWidth := len(groupCols) + 1
	// The grouping columns' types come from the candidate's declared key types,
	// which are positionally aligned with groupCols and normalized to its
	// length. Stating them here rather than minting UnknownType keeps every
	// downstream reader off name-keyed re-derivation; where the index genuinely
	// cannot state a component's type the candidate already carries Unknown for
	// it, so the honest answer flows through unchanged.
	groupKeyTypes := owner.GetKeyComponentTypes()
	comparisonRoot, ok := aggregateRowQOV(childPlans[0].GetResultType())
	if !ok {
		return nil
	}
	comparisonKey := make([]values.Value, len(groupCols))
	for i := range groupCols {
		resolved, resolveErr := values.ResolveFieldOrdinals(comparisonRoot, []int{i})
		if resolveErr != nil || !sameExactType(resolved.Type(), groupKeyTypes[i]) {
			return nil
		}
		comparisonKey[i] = resolved
	}

	// Result row = grouping columns from the DRIVING stream, then the owner's
	// aggregate from its own stream. The grouping values must come from the
	// companion: for a group the owner has no entry for, the owner's slots are
	// the absent filler and carry nothing.
	mergedRoot, ok := aggregateMergedRowQOV(childPlans)
	if !ok {
		return nil
	}
	fields := make([]values.RecordConstructorField, 0, len(groupCols)+1)
	for i, col := range groupCols {
		field, resolveErr := values.ResolveFieldOrdinals(mergedRoot, []int{i})
		if resolveErr != nil || !sameExactType(field.Type(), groupKeyTypes[i]) {
			return nil
		}
		fields = append(fields, values.RecordConstructorField{
			Name:  col,
			Value: field,
		})
	}
	aggName := aggregateFlowedColumnName(owner.aggFunction.String(), owner.aggColumn)
	aggField, err := values.ResolveFieldOrdinals(mergedRoot, []int{childWidth + len(groupCols)})
	if err != nil {
		return nil
	}
	fields = append(fields, values.RecordConstructorField{
		Name:  aggName,
		Value: emptyGroupIdentity(owner, aggField),
	})

	childQuants := make([]expressions.Quantifier, len(childPlans))
	for i, cp := range childPlans {
		childQuants[i] = expressions.NewPhysicalQuantifier(
			call.MemoizeFinalExpression(&scanPlanExpression{plan: cp}))
	}
	merge, err := plans.NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
		childQuants, comparisonKey, values.NewRecordConstructorValue(fields...))
	if err != nil {
		call.Fail(err)
		return nil
	}
	// Leg 0 is the companion, and the designation travels as its ALIAS so a
	// later relink cannot silently point it at the aggregate stream.
	return merge.WithDrivingStream(childQuants[0].GetAlias())
}

// emptyGroupIdentity wraps an aggregate pick-up so a group the aggregate index
// has no entry for answers what SQL requires rather than NULL.
//
// SUM's empty group IS NULL, so a SUM pick-up is returned unchanged. COUNT(col)
// counts non-NULL values, so a group whose every value is NULL has no index
// entry yet must answer 0 — a COALESCE supplies it. This is the same split Java
// uses for the ungrouped case, where the plan yields NULL on an empty stream
// and a coalesce_long above it turns that into 0 for COUNT.
func emptyGroupIdentity(owner *AggregateIndexMatchCandidate, pickUp values.Value) values.Value {
	if owner.countsRows || owner.aggFunction != expressions.AggCount {
		return pickUp
	}
	return values.NewScalarFunctionValue("COALESCE", pickUp.Type(),
		pickUp, &values.ConstantValue{Value: int64(0)})
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
			if idx := groupColComparisonIndex(cp, cand.groupCols, fp.input); idx >= 0 {
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
		if !residualOverGroupingColumns(fp.pred, cand.groupCols, fp.input) {
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
	groupCols []string,
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
			if !residualOverGroupingColumns(sub, groupCols, input) {
				return false
			}
		}
		return true
	case *predicates.OrPredicate:
		for _, sub := range pred.SubPredicates {
			if !residualOverGroupingColumns(sub, groupCols, input) {
				return false
			}
		}
		return true
	case *predicates.NotPredicate:
		return residualOverGroupingColumns(pred.Child, groupCols, input)
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
				if groupingColumnIndex(node, groupCols, input) < 0 {
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
func groupingColumnIndex(v values.Value, groupCols []string, input values.CorrelationIdentifier) int {
	if !rootedAt(v, input) {
		return -1
	}
	for i, col := range groupCols {
		if aggColumnMatches(v, col) {
			return i
		}
	}
	return -1
}

// applyResiduals wraps plan in ONE PredicatesFilter carrying the residuals
// rewritten onto plan's row — grouping column i is ordinal i of the row the
// aggregate scan, the companion merge and the multi-aggregate intersection
// all flow — and asserts the bridge: a rewritten residual must read plan's
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
	groupCols := partition.cand.groupCols
	rewritten := make([]predicates.QueryPredicate, 0, len(partition.residuals))
	for _, residual := range partition.residuals {
		input := residual.input
		failed := false
		moved := predicates.ReplaceValues(residual.pred, func(v values.Value) values.Value {
			if _, isField := values.AsFieldValue(v); !isField || !rootedAt(v, input) {
				return v
			}
			idx := groupingColumnIndex(v, groupCols, input)
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

// rekeyScanPrefix carries the owner's truncated scan bounds onto a companion
// candidate over the same grouping columns, alias by POSITION. Position is the
// right key because both callers hold the legs' grouping columns equal AND in
// the same order: the group-existence companion is found by groupingSignature
// equality (findGroupCountCompanion), the normalised proto encoding of the
// index's grouping KeyExpression, which is order-sensitive and is what
// groupCols derives from; the multi-aggregate intersection checks its legs'
// groupCols name by name in order before it gets here.
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
	groupCols []string,
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
	return groupingColumnIndex(fv, groupCols, input)
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
	rowType, ok := cand.GetBaseRowType().(*values.RecordType)
	if !ok || cand.aggColumn == "" {
		return nil, false
	}
	field, ok := rowType.LookupFieldUnique(cand.aggColumn)
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
		if len(mc.groupCols) != len(groupCols) {
			return
		}
		for k := range groupCols {
			if !eqFold(mc.groupCols[k], groupCols[k]) {
				return
			}
		}
	}

	// The same partition as the single-aggregate path, over the first leg's
	// candidate (every leg shares its grouping columns): scan bounds for the
	// legs, residuals above the merge, or a decline.
	partition, ok := partitionAggregatePredicates(matched[0], innerFilterPreds)
	if !ok {
		return
	}

	// RFC-209 §5.3, "Multi-aggregate": group existence is decided ONCE, not per
	// aggregate. If any leg is a grouped SUM or COUNT(col) — neither of which
	// can decide it alone — the companion COUNT(*) becomes an additional
	// DRIVING stream with outer semantics against every aggregate stream, and
	// each aggregate independently contributes its stored value or its identity.
	//
	// Without this the multi-aggregate shape keeps both defects the
	// single-aggregate path just lost, and keeps them in their worst form:
	// inner intersection means a vacated group present in BOTH indexes survives
	// as a phantom, and an all-NULL group absent from both is dropped twice
	// over. It would also be the one route by which a SUM index still answers a
	// grouped query with no group-existence source at all, which is exactly the
	// fail-closed property §5.3(c) asserts.
	//
	// EVERY leg needing a companion must resolve to the SAME one: the legs can
	// differ in sparse predicate, and a companion matching one leg's row
	// population is the wrong group set for another's.
	legs := matched
	aggLegOffset := 0
	var companion *AggregateIndexMatchCandidate
	needsCompanion := false
	for _, mc := range matched {
		if mc.NeedsGroupExistenceCompanion() {
			needsCompanion = true
			c := findGroupCountCompanion(mc, candidates)
			if c == nil {
				return // fail closed — streaming aggregation over base rows
			}
			if companion != nil && c != companion {
				return
			}
			companion = c
		}
	}
	drivingLeg := -1
	if needsCompanion {
		// The query may already SELECT the companion's own COUNT(*) over this
		// grouping key, in which case that aggregate leg IS the group-existence
		// stream and must be designated rather than duplicated. Prepending a
		// second copy would scan one index twice in a single merge and decide
		// existence twice over — the opposite of §5.3's "decided once, not per
		// aggregate". The leg already carries the vacated-group drop, because
		// that is a property of being a grouped COUNT(*), not of the position.
		for i, mc := range matched {
			if mc == companion {
				drivingLeg = i
				break
			}
		}
		if drivingLeg < 0 {
			// No leg is the companion: it becomes an extra leg 0 and every
			// aggregate shifts one child-span right in the merged row.
			legs = append([]*AggregateIndexMatchCandidate{companion}, matched...)
			aggLegOffset = 1
			drivingLeg = 0
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
	childPlans := make([]plans.RecordQueryPlan, len(legs))
	for i, mc := range legs {
		prefix := rekeyScanPrefix(partition.scanPrefix, matched[0], mc)
		if !candidateBindingRangesEligible(mc, prefix) {
			return
		}
		sp := mc.ToScanPlan(prefix, false)
		idxPlan := extractIndexPlan(sp)
		if idxPlan == nil {
			return
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
			return
		}
		var recordTypeName string
		if rts := mc.GetRecordTypes(); len(rts) > 0 {
			recordTypeName = rts[0]
		}
		resultType, ok := aggregateIndexOutputType(mc)
		if !ok {
			return
		}
		aggPlan, err := plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, resultType, mc.aggFunction.String(),
		)
		if err != nil {
			call.Fail(err)
			return
		}
		childPlans[i] = aggPlan.WithGroupColumns(mc.groupCols, mc.aggColumn).
			WithGroupColumnLayout(mc.GetBaseRowType()).
			WithLiveGroupsOnly(dropsVacatedGroups(mc))
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
		return
	}
	comparisonKey := make([]values.Value, len(groupCols))
	for i := range groupCols {
		resolved, resolveErr := values.ResolveFieldOrdinals(comparisonRoot, []int{i})
		if resolveErr != nil {
			call.Fail(resolveErr)
			return
		}
		comparisonKey[i] = resolved
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
	// The merge cursor evaluates the result value against the CONCATENATION
	// of the matched child rows, each child spanning len(groupCols)+1 slots
	// ([groupCols..., FUNC(col)]). Child i's aggregate sits at its span's last
	// slot. Baked, both read positionally.
	//
	// The grouping columns must be read from the DRIVING leg's span, not from
	// leg 0's. The two coincide only when the companion was prepended as a new
	// leg 0; when the query already selects the companion's own COUNT(*) that
	// leg is designated in place and sits at drivingLeg > 0. Reading ordinal i
	// then takes the grouping key from a NON-driving aggregate leg, and for
	// every group that leg has no entry for the absent filler is all-NULL — so
	// the group's key is destroyed, not merely its aggregate. `SELECT g, SUM(v),
	// COUNT(*)` over all-NULL v returned [NULL NULL 1] per group instead of
	// [g NULL 1]; the driving leg is by construction the one with an entry for
	// every live group, which is the whole reason it drives.
	childWidth := len(groupCols) + 1
	groupingBase := 0
	if needsCompanion && drivingLeg > 0 {
		groupingBase = drivingLeg * childWidth
	}
	mergedRoot, ok := aggregateMergedRowQOV(childPlans)
	if !ok {
		return
	}
	fields := make([]values.RecordConstructorField, 0, len(groupCols)+len(aggs))
	for i, col := range groupCols {
		groupValue, resolveErr := values.ResolveFieldOrdinals(mergedRoot, []int{groupingBase + i})
		if resolveErr != nil {
			call.Fail(resolveErr)
			return
		}
		fields = append(fields, values.RecordConstructorField{
			Name:  col,
			Value: groupValue,
		})
	}
	for i := range aggs {
		colName := aggregateFlowedColumnName(matched[i].aggFunction.String(), matched[i].aggColumn)
		// aggLegOffset shifts past the driving companion when one is present.
		pickUp, resolveErr := values.ResolveFieldOrdinals(
			mergedRoot, []int{(i+aggLegOffset)*childWidth + len(groupCols)})
		if resolveErr != nil {
			call.Fail(resolveErr)
			return
		}
		if needsCompanion {
			// A group the companion lists but this aggregate's index has no entry
			// for arrives as NULL, which is already SUM's empty-group answer but
			// not COUNT(col)'s — that one owes 0.
			pickUp = emptyGroupIdentity(matched[i], pickUp)
		}
		fields = append(fields, values.RecordConstructorField{
			Name:  colName,
			Value: pickUp,
		})
	}
	resultValue := values.NewRecordConstructorValue(fields...)

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
		return
	}
	if needsCompanion {
		// The designation travels as an ALIAS so a later relink cannot silently
		// point it at a different stream.
		merge = merge.WithDrivingStream(childQuants[drivingLeg].GetAlias())
	}
	filtered, ok := partition.applyResiduals(merge)
	if !ok {
		return
	}
	logicalPlan, err := projectAggregateResultToGroupBy(filtered, gb)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(logicalPlan)
}

var _ ExpressionRule = (*AggregateDataAccessRule)(nil)
