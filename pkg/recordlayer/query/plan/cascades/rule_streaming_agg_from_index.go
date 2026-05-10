package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	innerRef := gb.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

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
		if !recordTypesOverlap(scanTypes, cand.GetRecordTypes()) {
			continue
		}

		colNames := cand.GetColumnNames()
		if len(colNames) < len(groupingKeys) {
			continue
		}

		matches := true
		for i, gk := range groupingKeys {
			fv, ok := gk.(*values.FieldValue)
			if !ok {
				matches = false
				break
			}
			if !eqFold(fv.Field, colNames[i]) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}

		emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
		for _, reverse := range []bool{false, true} {
			scanPlan := cand.ToScanPlan(emptyPrefix, reverse)
			idxPlan := extractIndexPlan(scanPlan)
			if idxPlan == nil {
				continue
			}

			idxWrapper := &physicalIndexScanWrapper{
				plan:        idxPlan,
				columnNames: colNames,
				unique:      cand.IsUnique(),
			}

			aggPlan := plans.NewRecordQueryStreamingAggregationPlan(
				idxPlan, groupingKeys, gb.GetAggregates(),
			)
			innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(idxWrapper))
			call.Yield(newPhysicalStreamingAggWrapper(aggPlan, innerQ))
		}
	}
}

var _ ExpressionRule = (*StreamingAggFromIndexRule)(nil)
