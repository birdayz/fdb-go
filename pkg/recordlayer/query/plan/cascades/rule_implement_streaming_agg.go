package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
		if fv, ok := gk.(*values.FieldValue); ok && fv.Child == nil {
			// A BARE field reference — the fast path: the InMemorySort executor reads
			// the row's `Field` key directly (compareByField).
			sortKeys[i] = plans.SortKey{Field: fv.Field}
		} else {
			// A CORRELATED/qualified group key (a FieldValue carrying a Child QOV,
			// e.g. a lateral-unnest `v.v`, or any non-FieldValue computed key) MUST be
			// EVALUATED per row, exactly like ImplementInMemorySortRule's ValueExpr
			// path. The aggregateCursor groups by `gk.Evaluate(row)` — the QUALIFIED
			// merged-row key (`V.V`) — so the REQUIRED pre-aggregate sort must order by
			// the SAME key. Collapsing a qualified FieldValue to its bare `Field` here
			// sorts by the last-leg-wins BARE `V` key (`mergeRows` keys it as a later
			// same-named column, e.g. `U.V`), which DISAGREES with the group key — so
			// contiguous unnest elements split into multiple non-contiguous groups with
			// wrong counts (RFC-142, the streaming-aggregate twin of the
			// in-memory ORDER BY P2a). Field is still set (to the rendered
			// `LEG.COL`) for Explain; ValueExpr drives the sort.
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
	// Skip full-range Fetch wrappers: a Fetch(IndexScan(full-range))
	// reads every row via random PK lookups, always slower than
	// InMemorySort(FullScan). Selective Fetches (WHERE predicate
	// consumed by the index) are kept — they read fewer rows.
	orderedExpr := findOrderedPhysicalExpr(innerRef, groupingKeys)
	if orderedExpr != nil {
		if fw, isFetch := orderedExpr.(*physicalFetchFromPartialRecordWrapper); isFetch && isFullRangeFetch(fw) {
			// Skip — InMemorySort(FullScan) is cheaper than Fetch(IndexScan(full-range)).
		} else if ppe, ok := orderedExpr.(physicalPlanExpression); ok {
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

// isFullRangeFetch reports whether a Fetch wrapper's inner index scan has
// no bound comparison ranges — i.e., it scans the entire index. A full-range
// Fetch reads every row via random PK lookups, which is always worse than
// a sequential full scan + in-memory sort.
func isFullRangeFetch(fw *physicalFetchFromPartialRecordWrapper) bool {
	innerRef := fw.innerQuant.GetRangesOver()
	if innerRef == nil {
		return true
	}
	idxWrapper := findIndexScanWrapper(innerRef)
	if idxWrapper == nil || idxWrapper.plan == nil {
		return true
	}
	for _, cr := range idxWrapper.plan.GetScanComparisons() {
		if !cr.IsEmpty() {
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

// isCountOnlyAggregation reports whether every aggregate is a COUNT that reads
// NO base-record field. When true, an index scan feeding this aggregation can be
// marked covering — the index entries alone provide the count without a PK fetch.
//
// The covering decision is about FIELD ACCESS, not count-star semantics: a true
// COUNT(*) (nil operand) and a COUNT over a constant (COUNT(1), COUNT(TRUE),
// COUNT(NULL)) read no field, so a zero-column covering scan serves them (the
// executor evaluates the constant operand per row without the record). Only
// COUNT(col) / COUNT(expr-over-col) actually reads a field — covering it would
// make col evaluate to NULL for every row and return 0.
func isCountOnlyAggregation(aggs []expressions.AggregateSpec) bool {
	if len(aggs) == 0 {
		return false
	}
	for _, a := range aggs {
		if a.Function != expressions.AggCount {
			return false
		}
		if a.Operand == nil {
			continue // COUNT(*)
		}
		if comparandReadsField(a.Operand) {
			return false // COUNT(col) / COUNT(expr-over-col) needs the field
		}
		// COUNT(<constant>) reads no base-record field — covering is safe.
	}
	return true
}

var _ ExpressionRule = (*ImplementStreamingAggregationRule)(nil)
