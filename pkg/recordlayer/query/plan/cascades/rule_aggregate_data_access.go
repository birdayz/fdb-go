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
			mergePlan := buildGroupExistenceMerge(call, companion, aggCand, innerFilterPreds)
			if mergePlan == nil {
				continue
			}
			call.Yield(mergePlan)
			singleMatched = true
			continue
		}

		prefix := buildAggScanPrefix(aggCand, innerFilterPreds)
		if !candidateBindingRangesEligible(aggCand, prefix) {
			continue
		}
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
		).WithGroupColumns(aggCand.groupCols, aggCand.aggColumn).
			WithGroupColumnLayout(aggCand.GetBaseRowType()).
			WithLiveGroupsOnly(dropsVacatedGroups(aggCand))

		// The aggregate-index plan is its own cascades expression now (RFC-184 W2).
		call.Yield(aggPlan)
		singleMatched = true
	}
	if singleMatched {
		return
	}

	// Path 2: multi-aggregate intersection — multiple candidates, each
	// covering one of the GroupBy's aggregates with identical grouping.
	tryMultiAggregateIntersection(call, gb, candidates, scanTypes, innerFilterPreds)
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
	innerFilterPreds []*predicates.ComparisonPredicate,
) plans.RecordQueryPlan {
	groupCols := owner.groupCols
	if len(groupCols) == 0 {
		return nil
	}

	// The companion carries the same grouping columns, so the WHERE-equality
	// bounds derived for the owner apply to it verbatim; both streams then emit
	// exactly the groups the query asked for.
	legs := []*AggregateIndexMatchCandidate{companion, owner}
	childPlans := make([]plans.RecordQueryPlan, len(legs))
	for i, cand := range legs {
		prefix := buildAggScanPrefix(cand, innerFilterPreds)
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
		childPlans[i] = plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, values.UnknownType, cand.aggFunction.String(),
		).WithGroupColumns(cand.groupCols, cand.aggColumn).
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
	comparisonKey := make([]values.Value, len(groupCols))
	for i, col := range groupCols {
		comparisonKey[i] = values.NewFieldValueWithResolvedOrdinal(col, i, groupKeyTypes[i])
	}

	// Result row = grouping columns from the DRIVING stream, then the owner's
	// aggregate from its own stream. The grouping values must come from the
	// companion: for a group the owner has no entry for, the owner's slots are
	// the absent filler and carry nothing.
	fields := make([]values.RecordConstructorField, 0, len(groupCols)+1)
	for i, col := range groupCols {
		fields = append(fields, values.RecordConstructorField{
			Name:  col,
			Value: values.NewFieldValueWithResolvedOrdinal(col, i, groupKeyTypes[i]),
		})
	}
	aggName := aggregateFlowedColumnName(owner.aggFunction.String(), owner.aggColumn)
	aggField := values.Value(values.NewFieldValueWithResolvedOrdinal(
		aggName, childWidth+len(groupCols), values.UnknownType))
	fields = append(fields, values.RecordConstructorField{
		Name:  aggName,
		Value: emptyGroupIdentity(owner, aggField),
	})

	childQuants := make([]expressions.Quantifier, len(childPlans))
	for i, cp := range childPlans {
		childQuants[i] = expressions.NewPhysicalQuantifier(
			call.MemoizeFinalExpression(&scanPlanExpression{plan: cp}))
	}
	merge := plans.NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
		childQuants, comparisonKey, values.NewRecordConstructorValue(fields...))
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
	return values.NewScalarFunctionValue("COALESCE", values.UnknownType,
		pickUp, &values.ConstantValue{Value: int64(0)})
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
		// Match the grouping column by full accessor PATH, not leaf name, so a
		// nested `addr.city` group-key predicate never binds a same-leaf-named
		// top-level `city` aggregate index (RFC-187 S5). Dropping the former
		// dotted-leaf-strip fallback means a form-(c) flat-dotted qualified key
		// (`T.city`) that cannot be structurally resolved falls back to a
		// StreamingAgg (correct rows) rather than risk the nested collision.
		if aggColumnMatches(fv, col) {
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
		prefix := buildAggScanPrefix(mc, innerFilterPreds)
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
		childPlans[i] = plans.NewRecordQueryAggregateIndexPlan(
			idxPlan, recordTypeName, values.UnknownType, mc.aggFunction.String(),
		).WithGroupColumns(mc.groupCols, mc.aggColumn).
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
	comparisonKey := make([]values.Value, len(groupCols))
	for i, col := range groupCols {
		comparisonKey[i] = values.NewFieldValueWithResolvedOrdinal(col, i, values.UnknownType)
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
	// ([groupCols..., FUNC(col)]). A grouping column reads child 0's slot i
	// (identical across children); child i's aggregate sits at its span's
	// last slot. Baked, both read positionally.
	childWidth := len(groupCols) + 1
	fields := make([]values.RecordConstructorField, 0, len(groupCols)+len(aggs))
	for i, col := range groupCols {
		fields = append(fields, values.RecordConstructorField{
			Name:  col,
			Value: values.NewFieldValueWithResolvedOrdinal(col, i, values.UnknownType),
		})
	}
	for i := range aggs {
		colName := aggregateFlowedColumnName(matched[i].aggFunction.String(), matched[i].aggColumn)
		// aggLegOffset shifts past the driving companion when one is present.
		pickUp := values.Value(values.NewFieldValueWithResolvedOrdinal(
			colName, (i+aggLegOffset)*childWidth+len(groupCols), values.UnknownType))
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
	merge := plans.NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
		childQuants, comparisonKey, resultValue,
	)
	if needsCompanion {
		// The designation travels as an ALIAS so a later relink cannot silently
		// point it at a different stream.
		merge = merge.WithDrivingStream(childQuants[drivingLeg].GetAlias())
	}
	call.Yield(merge)
}

var _ ExpressionRule = (*AggregateDataAccessRule)(nil)
