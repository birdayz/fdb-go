package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// AggregateDataAccessRule matches a GroupByExpression against aggregate
// index candidates (SUM, COUNT, etc.). When a match is found, the rule
// directly produces an index scan that reads pre-computed aggregates
// from the aggregate index — no runtime aggregation needed.
//
//	GroupBy(keys=[k1], aggs=[SUM(col)], inner=Scan)
//	  → IndexScan(aggregate_index)   [when aggregate index matches]
//
// This is a massive optimization for aggregate queries: the aggregate
// answer is pre-computed and maintained by FDB's mutation mechanism.
// The scan returns one row per group with the aggregate value ready.
//
// Mirrors Java's `AggregateDataAccessRule` at the structural level.
// The seed simplifies: single-aggregate matching only (Java handles
// multi-aggregate via intersection of aggregate indexes).
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
	scan := findFullScan(innerRef)
	if scan == nil {
		return
	}
	scanTypes := scan.GetRecordTypes()

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

		emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
		scanPlan := aggCand.ToScanPlan(emptyPrefix, false)
		idxPlan, ok := scanPlan.(*plans.RecordQueryIndexPlan)
		if !ok {
			continue
		}

		wrapper := &physicalIndexScanWrapper{
			plan:        idxPlan,
			columnNames: aggCand.GetColumnNames(),
			unique:      false,
		}
		call.Yield(wrapper)
	}
}

var _ ExpressionRule = (*AggregateDataAccessRule)(nil)
