package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// StreamingAggFromIndexRule directly converts a GroupByExpression into a
// streaming aggregation over an ordered index scan when an index's leading
// columns cover the grouping keys. This fires even without an explicit
// Sort expression in the tree — the index ordering is sufficient.
//
//	GroupBy(keys=[k1, k2], aggs=[...], FullScan)
//	  → StreamingAgg(IndexScan(full-range, index on (k1, k2, ...)))
//
// Without this rule, the planner would need Sort(keys, Scan) below the
// GroupBy for the streaming agg path to trigger. This rule closes the
// gap for queries like "SELECT region, COUNT(*) FROM t GROUP BY region"
// where the user doesn't specify ORDER BY but an index on (region) exists.
type StreamingAggFromIndexRule struct {
	matcher matching.BindingMatcher
}

func NewStreamingAggFromIndexRule() *StreamingAggFromIndexRule {
	return &StreamingAggFromIndexRule{
		matcher: NewExpressionMatcher[*expressions.GroupByExpression]("streaming_agg_from_index"),
	}
}

func (r *StreamingAggFromIndexRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *StreamingAggFromIndexRule) OnMatch(call *ExpressionRuleCall) {
	gb := matching.Get[*expressions.GroupByExpression](call.Bindings, r.matcher)

	groupingKeys := gb.GetGroupingKeys()
	if len(groupingKeys) == 0 {
		return
	}
	// A streaming aggregation compares each row against the PREVIOUS group
	// only. That is sound exactly when rows equal under the GROUPING IDENTITY
	// arrive ADJACENT — which is what an index gives you, for every coordinate
	// whose tuple encoding clusters that identity.
	//
	// A FLOAT/DOUBLE coordinate does not. The grouping identity is
	// java.lang.Double.equals (Java decides a group break with
	// `!currentGroup.equals(nextGroup)` over a protobuf message,
	// StreamGrouping.java:186), which collapses every NaN payload to one value,
	// while tuple encoding scatters those payloads into two blocks at OPPOSITE
	// ENDS of the key space. So the NaN group opens near the start of the scan,
	// closes, and reopens at the end — and the aggregation emits it TWICE. That
	// is a wrong answer, and it is one this rule creates: without it the query
	// is served by a sort beneath the aggregation, whose comparator
	// (CompareFloat64) makes every NaN one value and lands them adjacent.
	//
	// The predicate is the one values.TypeTerminatesOrderingClaim authority the
	// ordering producers ask, because it is the same underlying fact — physical
	// key order is not this column's logical order. The DIFFERENCE is that a
	// claim may be TRUNCATED to its good prefix while clustering may not:
	// clustering is a property of the whole key, so anything short of every
	// grouping key disqualifies the shortcut.
	if values.ClaimableTypedKeyPrefix(groupingKeys) < len(groupingKeys) {
		return
	}

	innerRef := gb.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	inputAlias := gb.GetInner().GetAlias()

	scan := findFullScan(innerRef)
	if scan == nil {
		return
	}

	candidates := call.Context.GetMatchCandidates()
	if len(candidates) == 0 {
		return
	}

	scanTypes := scan.GetRecordTypes()

	for _, cand := range candidates {
		if _, isAgg := cand.(*AggregateIndexMatchCandidate); isAgg {
			continue
		}
		colNames, plainFields := candidatePlainFieldColumnsForShortcut(cand)
		if !plainFields {
			continue
		}
		// The direct GroupBy(Scan) shortcut aggregates raw index entries and
		// bypasses data-access compensation. A fan-out candidate would count
		// entries rather than base records and omit empty repeated fields.
		if !candidatePreservesBaseRecordCardinality(cand) {
			continue
		}
		if !recordTypesOverlap(scanTypes, cand.GetRecordTypes()) {
			continue
		}

		if len(colNames) < len(groupingKeys) {
			continue
		}

		matches := true
		for i, gk := range groupingKeys {
			// Full accessor path, not leaf name (RFC-187 S8): a nested grouping
			// key must not match a same-leaf-named top-level index column.
			if !aggColumnMatches(gk, colNames[i]) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}

		emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
		// Forward-only: reverse ordering is handled by ImplementSortRule
		// when ORDER BY DESC is present above the GroupBy.
		scanPlan := cand.ToScanPlan(emptyPrefix, false)
		idxPlan := extractIndexPlan(scanPlan)
		if idxPlan == nil {
			continue
		}

		if !aggregatesCoveredByIndex(gb.GetAggregates(), colNames) {
			continue
		}
		// Coveringness is a plan TYPE wrapping the scan (RFC-220), not a flag on
		// it. The covered entry columns are derived from the stamped inner —
		// stampIndexMetadata installs the candidate's column names, so the derived
		// list is the same `colNames` this site used to pass explicitly, minus the
		// possibility of the two disagreeing.
		coveringPlan, err := plans.NewRecordQueryCoveringIndexPlan(stampIndexMetadata(cand, idxPlan))
		if err != nil {
			call.Fail(err)
			return
		}
		// The inner is this covering index scan — a self-contained PRODUCER whose
		// grouping-key order is intrinsic to the index (not a delegator floating to
		// a winner), so carry the LIVE shared-group edge over it (RFC-184 W2, no
		// physicalStreamingAggWrapper).
		innerQ := expressions.NamedPhysicalQuantifier(inputAlias, call.MemoizeExpression(coveringPlan))
		aggPlan, err := plans.NewRecordQueryStreamingAggregationPlanFromQuantifier(innerQ, groupingKeys, gb.GetAggregates())
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(aggPlan)
	}
}

// aggregatesCoveredByIndex returns true when every field referenced by
// the aggregates is present in the index columns. COUNT(*) has no operand
// and is trivially covered. SUM(amount) is covered iff "amount" is in
// the index. This lets the index scan skip the per-row PK fetch.
func aggregatesCoveredByIndex(aggs []expressions.AggregateSpec, indexCols []string) bool {
	for _, a := range aggs {
		if a.Operand == nil {
			continue
		}
		if _, isConst := a.Operand.(*values.ConstantValue); isConst {
			continue // COUNT(*) / COUNT(1) — no field access needed
		}
		fv, ok := values.AsFieldValue(a.Operand)
		if !ok {
			return false // complex expression (e.g. SUM(a+1)) — may reference fields not in the index
		}
		found := false
		for _, col := range indexCols {
			if aggColumnMatches(fv, col) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*StreamingAggFromIndexRule)(nil)
