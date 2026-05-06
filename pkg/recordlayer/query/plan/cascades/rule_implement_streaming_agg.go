package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementStreamingAggregationRule implements a GroupByExpression as a
// physical RecordQueryStreamingAggregationPlan when the inner Reference
// has at least one member whose ordering satisfies the grouping keys.
//
//	GroupBy(keys=[k1, k2], aggs=[...], inner)
//	  → StreamingAggPlan(inner-physical)   [when inner ordered by k1, k2, ...]
//
// The streaming aggregation is the cheapest aggregation strategy when
// the input is already sorted — it processes rows in one pass with
// O(1) memory per group. When the inner is NOT ordered, this rule
// does not fire; a future hash-aggregation rule would handle that case.
//
// Java equivalent: ImplementStreamingAggregationRule in the OPTIMIZE
// phase, which requires OrderingProperty satisfaction.
type ImplementStreamingAggregationRule struct {
	matcher matching.BindingMatcher
}

func NewImplementStreamingAggregationRule() *ImplementStreamingAggregationRule {
	return &ImplementStreamingAggregationRule{
		matcher: NewExpressionMatcher[*expressions.GroupByExpression]("group_by"),
	}
}

func (r *ImplementStreamingAggregationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementStreamingAggregationRule) OnMatch(call *ExpressionRuleCall) {
	gb := matching.Get[*expressions.GroupByExpression](call.Bindings, r.matcher)

	innerRef := gb.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	groupingKeys := gb.GetGroupingKeys()
	if len(groupingKeys) == 0 {
		aggPlan := plans.NewRecordQueryStreamingAggregationPlan(innerPlan, groupingKeys, gb.GetAggregates())
		innerExpr := findPhysicalExpr(innerRef)
		if innerExpr == nil {
			return
		}
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
		call.Yield(newPhysicalStreamingAggWrapper(aggPlan, innerQ))
		return
	}

	innerExpr := findOrderedPhysicalExpr(innerRef, groupingKeys)
	if innerExpr == nil {
		return
	}

	aggPlan := plans.NewRecordQueryStreamingAggregationPlan(innerPlan, groupingKeys, gb.GetAggregates())
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(newPhysicalStreamingAggWrapper(aggPlan, innerQ))
}

// findOrderedPhysicalExpr scans the Reference for a physical-plan
// member whose ordering satisfies the grouping keys (in order).
func findOrderedPhysicalExpr(ref *expressions.Reference, groupingKeys []values.Value) expressions.RelationalExpression {
	for _, m := range ref.Members() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		o := properties.EstimateOrdering(m)
		if !o.IsKnown {
			continue
		}
		if orderingSatisfiesGroupingKeys(o, groupingKeys) {
			return m
		}
	}
	return nil
}

// orderingSatisfiesGroupingKeys returns true if the ordering's leading
// keys cover all grouping keys (order matters — grouping-key[i] must
// match ordering-key[i]).
func orderingSatisfiesGroupingKeys(o properties.Ordering, groupingKeys []values.Value) bool {
	if len(o.Keys) < len(groupingKeys) {
		return false
	}
	for i, gk := range groupingKeys {
		fv, ok := gk.(*values.FieldValue)
		if !ok {
			return false
		}
		oFV, ok := o.Keys[i].(*values.FieldValue)
		if !ok {
			return false
		}
		if !strings.EqualFold(fv.Field, oFV.Field) {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*ImplementStreamingAggregationRule)(nil)
