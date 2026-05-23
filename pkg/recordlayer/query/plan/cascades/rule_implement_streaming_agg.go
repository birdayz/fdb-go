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
		innerExpr := findPhysicalExpr(innerRef)
		if innerExpr == nil {
			return
		}
		if isCountOnlyAggregation(gb.GetAggregates()) {
			if idxWrapper := findIndexScanWrapper(innerRef); idxWrapper != nil && !idxWrapper.covering {
				coveringWrapper := &physicalIndexScanWrapper{
					plan:        idxWrapper.plan.WithCovering(nil),
					columnNames: idxWrapper.columnNames,
					unique:      idxWrapper.unique,
					covering:    true,
				}
				coveringQ := expressions.ForEachQuantifier(call.MemoizeExpression(coveringWrapper))
				aggPlan := plans.NewRecordQueryStreamingAggregationPlan(coveringWrapper.plan, groupingKeys, gb.GetAggregates())
				call.Yield(newPhysicalStreamingAggWrapper(aggPlan, coveringQ))
			}
		}
		aggPlan := plans.NewRecordQueryStreamingAggregationPlan(innerPlan, groupingKeys, gb.GetAggregates())
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
		call.Yield(newPhysicalStreamingAggWrapper(aggPlan, innerQ))
		return
	}

	sortKeys := make([]plans.SortKey, len(groupingKeys))
	for i, gk := range groupingKeys {
		if fv, ok := gk.(*values.FieldValue); ok {
			sortKeys[i] = plans.SortKey{Field: fv.Field}
		} else {
			sortKeys[i] = plans.SortKey{
				Field:     values.ExplainValue(gk),
				ValueExpr: gk,
			}
		}
	}

	// Always yield InMemorySort(FullScan) path as a Go extension.
	// Java refuses GROUP BY without sorted input; Go inserts an
	// in-memory sort so GROUP BY works without a supporting index.
	// When an ordered index also exists, both alternatives are yielded
	// and the cost model picks the cheaper one.
	rawExpr := findPhysicalExpr(innerRef)
	if rawExpr != nil {
		sortedPlan := plans.NewRecordQueryInMemorySortPlan(innerPlan, sortKeys)
		rawQ := expressions.ForEachQuantifier(call.MemoizeExpression(rawExpr))
		sortExpr := newPhysicalInMemorySortWrapper(sortedPlan, rawQ)
		aggPlan := plans.NewRecordQueryStreamingAggregationPlan(sortedPlan, groupingKeys, gb.GetAggregates())
		sortQ := expressions.ForEachQuantifier(call.MemoizeExpression(sortExpr))
		call.Yield(newPhysicalStreamingAggWrapper(aggPlan, sortQ))
	}

	// If an ordered physical expression exists (e.g. index scan whose
	// leading columns match the grouping keys), yield that path too.
	orderedExpr := findOrderedPhysicalExpr(innerRef, groupingKeys)
	if orderedExpr != nil {
		if ppe, ok := orderedExpr.(physicalPlanExpression); ok {
			orderedInnerPlan := ppe.GetRecordQueryPlan()
			aggPlan := plans.NewRecordQueryStreamingAggregationPlan(orderedInnerPlan, groupingKeys, gb.GetAggregates())
			orderedQ := expressions.ForEachQuantifier(call.MemoizeExpression(orderedExpr))
			call.Yield(newPhysicalStreamingAggWrapper(aggPlan, orderedQ))
		}
	}
}

// findOrderedPhysicalExpr scans the Reference for a physical-plan
// member whose ordering satisfies the grouping keys (in order).
func findOrderedPhysicalExpr(ref *expressions.Reference, groupingKeys []values.Value) expressions.RelationalExpression {
	for _, m := range ref.AllMembers() {
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

// findIndexScanWrapper scans the Reference for a physicalIndexScanWrapper,
// traversing through Fetch wrappers. The Fetch operator is a transparent
// enforcer — rules that need index properties look through it.
func findIndexScanWrapper(ref *expressions.Reference) *physicalIndexScanWrapper {
	if ref == nil {
		return nil
	}
	for _, m := range ref.AllMembers() {
		if w, ok := m.(*physicalIndexScanWrapper); ok {
			return w
		}
		if fw, ok := m.(*physicalFetchFromPartialRecordWrapper); ok {
			if innerRef := fw.innerQuant.GetRangesOver(); innerRef != nil {
				if w := findIndexScanWrapper(innerRef); w != nil {
					return w
				}
			}
		}
	}
	return nil
}

// isCountOnlyAggregation reports whether all aggregates are COUNT(*)
// (no field access needed from the base record). When true, an index
// scan feeding this aggregation can be marked covering — the index
// entries alone provide the count without PK fetch.
func isCountOnlyAggregation(aggs []expressions.AggregateSpec) bool {
	if len(aggs) == 0 {
		return false
	}
	for _, a := range aggs {
		if a.Function != expressions.AggCount {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*ImplementStreamingAggregationRule)(nil)
