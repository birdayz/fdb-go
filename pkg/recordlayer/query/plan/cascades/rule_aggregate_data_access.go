package cascades

import (
	"fmt"

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
	scan := findFullScan(innerRef)
	if scan == nil {
		return
	}
	scanTypes := scan.GetRecordTypes()

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

		emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
		scanPlan := aggCand.ToScanPlan(emptyPrefix, false)
		idxPlan := extractIndexPlan(scanPlan)
		if idxPlan == nil {
			continue
		}

		wrapper := &physicalIndexScanWrapper{
			plan:        idxPlan,
			columnNames: aggCand.GetColumnNames(),
			unique:      false,
			covering:    true,
		}
		call.Yield(wrapper)
		singleMatched = true
	}
	if singleMatched {
		return
	}

	// Path 2: multi-aggregate intersection — multiple candidates, each
	// covering one of the GroupBy's aggregates with identical grouping.
	tryMultiAggregateIntersection(call, gb, candidates, scanTypes)
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

	// Build child index scan plans.
	childPlans := make([]plans.RecordQueryPlan, len(matched))
	for i, mc := range matched {
		emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
		sp := mc.ToScanPlan(emptyPrefix, false)
		idxPlan := extractIndexPlan(sp)
		if idxPlan == nil {
			return
		}
		childPlans[i] = idxPlan
	}

	// Comparison key = grouping column FieldValues.
	comparisonKey := make([]values.Value, len(groupCols))
	for i, col := range groupCols {
		comparisonKey[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
	}

	// Result value = Record(groupCol0, groupCol1, ..., agg0, agg1, ...).
	// Grouping columns come from the first child; each aggregate comes
	// from its respective child. Mirrors Java's
	// computeIntersectionResultValue(). Use unique field names to avoid
	// collisions between grouping columns and aggregate pick-ups.
	fields := make([]values.RecordConstructorField, 0, len(groupCols)+len(aggs))
	usedNames := make(map[string]struct{}, len(groupCols)+len(aggs))
	for _, col := range groupCols {
		fields = append(fields, values.RecordConstructorField{
			Name:  col,
			Value: &values.FieldValue{Field: col, Typ: values.UnknownType},
		})
		usedNames[col] = struct{}{}
	}
	for i, agg := range aggs {
		name := matched[i].aggColumn
		if agg.Alias != "" {
			name = agg.Alias
		}
		// Ensure uniqueness — append _N suffix on collision.
		base := name
		for seq := 1; ; seq++ {
			if _, dup := usedNames[name]; !dup {
				break
			}
			name = fmt.Sprintf("%s_%d", base, seq)
		}
		usedNames[name] = struct{}{}
		fields = append(fields, values.RecordConstructorField{
			Name:  name,
			Value: &values.FieldValue{Field: matched[i].aggColumn, Typ: values.UnknownType},
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
