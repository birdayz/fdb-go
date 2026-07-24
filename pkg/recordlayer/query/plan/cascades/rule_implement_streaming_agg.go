package cascades

import (
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
		if isCountOnlyAggregation(gb.GetAggregates()) {
			if idxPlan := findIndexScanPlan(innerRef); idxPlan != nil &&
				idxPlan.ProducesDistinctRecords() && !idxPlan.IsCovering() {
				// The covering index scan is its own cascades expression (RFC-184 W2);
				// WithCovering preserves the metadata already on the plan (struct copy).
				coveringPlan := idxPlan.WithCovering(nil)
				// Count-only, no grouping keys → no ordering precondition, so carry
				// the LIVE shared-group edge (RFC-184 W2, no physicalStreamingAggWrapper).
				coveringQ := expressions.ForEachQuantifier(call.MemoizeExpression(coveringPlan))
				call.Yield(plans.NewRecordQueryStreamingAggregationPlanFromQuantifier(coveringQ, groupingKeys, gb.GetAggregates()))
			}
		}
		// Yield an aggregate over EVERY physical alternative of the
		// inner (the memo dedups and the cost model picks the winner) —
		// the former first-physical single pick was ORDER-DEPENDENT: it
		// happened to see the Fetch(IndexScan) alternative first only
		// because dual insertion interleaved physicals into the
		// exploratory member order. With finals-only physicals the
		// first member is whatever landed first, and a single pick
		// silently drops the cheaper (or the only CORRECT-for-Explain)
		// alternative.
		for _, m := range innerRef.AllMembers() {
			if _, ok := m.(physicalPlanExpression); !ok {
				continue
			}
			// Count-only, no grouping keys → no ordering precondition, so carry the
			// LIVE shared-group edge over the member (RFC-184 W2, no
			// physicalStreamingAggWrapper). GetInner resolves the member's plan.
			innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(m))
			call.Yield(plans.NewRecordQueryStreamingAggregationPlanFromQuantifier(innerQ, groupingKeys, gb.GetAggregates()))
		}
		return
	}

	sortKeys := make([]plans.SortKey, len(groupingKeys))
	for i, gk := range groupingKeys {
		// ValueExpr ALWAYS drives the pre-aggregate sort — the aggregateCursor
		// groups by `gk.Evaluate(row)`, so the REQUIRED sort must order by the
		// SAME evaluated key (a qualified `V.V` leg reference collapsed to its
		// bare Field would sort by the last-leg-wins bare key and split
		// contiguous groups — the streaming-aggregate twin of the in-memory
		// ORDER BY key-collapse hazard, RFC-142). The key's ordinal is baked
		// at plan time; Field is DISPLAY-ONLY (Explain + the ordering-hint
		// name match): a bare childless key renders its bare name, anything
		// else the full explain rendering.
		field := ""
		if fv, ok := gk.(*values.FieldValue); ok && fv.Child == nil && fv.Resolved == nil {
			field = fv.Field
		} else {
			field = values.ExplainValue(gk)
		}
		// NullsFirst: the pre-aggregate sort is ascending, and the default for
		// ascending order is NULLS FIRST (Java ParseHelpers.isNullsLast —
		// `ASC NULLS FIRST, DESC NULLS LAST` — which is also FDB tuple order,
		// where null is the smallest element). Omitting it (zero-value = NULLS
		// LAST) is not cosmetic: the aggregate's provided-ordering hint lets
		// `GROUP BY k ORDER BY k` elide the enforcer sort, so the NULL group
		// must physically arrive where that default ordering puts it.
		sortKeys[i] = plans.SortKey{Field: field, NullsFirst: true, ValueExpr: gk}
	}

	// Always yield InMemorySort(FullScan) path as a Go extension.
	// Java refuses GROUP BY without sorted input; Go inserts an
	// in-memory sort so GROUP BY works without a supporting index.
	// When an ordered index also exists, both alternatives are yielded
	// and the cost model picks the cheaper one.
	rawExpr := findPhysicalExpr(innerRef)
	if rawExpr != nil {
		// The InMemorySort is now its own cascades expression (RFC-184 W2, no
		// physicalInMemorySortWrapper): a self-contained PRODUCER that provides the
		// grouping-key order intrinsically. Build the bare sort over the first
		// physical member's plan (a frozen QuantifierOverPlan snapshot) and carry it
		// as the LIVE shared-group edge under the aggregation.
		sortedPlan := plans.NewRecordQueryInMemorySortPlan(innerPlan, sortKeys)
		sortQ := expressions.ForEachQuantifier(call.MemoizeExpression(sortedPlan))
		call.Yield(plans.NewRecordQueryStreamingAggregationPlanFromQuantifier(sortQ, groupingKeys, gb.GetAggregates()))
	}

	// If an ordered physical expression exists (e.g. index scan whose
	// leading columns match the grouping keys), yield that path too.
	// Skip full-range Fetch wrappers: a Fetch(IndexScan(full-range))
	// reads every row via random PK lookups, always slower than
	// InMemorySort(FullScan). Selective Fetches (WHERE predicate
	// consumed by the index) are kept — they read fewer rows.
	orderedExpr := findOrderedPhysicalExpr(innerRef, groupingKeys)
	if orderedExpr != nil {
		if fw, isFetch := orderedExpr.(*plans.RecordQueryFetchFromPartialRecordPlan); isFetch && isFullRangeFetch(fw) {
			// Skip — InMemorySort(FullScan) is cheaper than Fetch(IndexScan(full-range)).
		} else if _, ok := orderedExpr.(physicalPlanExpression); ok {
			// PIN the whole ordering SPINE, not just the top expression: the
			// grouping-key ordering is a CORRECTNESS precondition of
			// streaming aggregation, and an order-PRESERVING delegator
			// (Fetch/Filter) reports its ordering from SOME member of its
			// source group while extraction's generic rebuild relinks that
			// group to its winner — a cheaper UNORDERED sibling would put
			// equal keys in separate runs and silently split groups.
			// pinOrderedSpine bakes each delegation level to its cheapest
			// satisfying member as a FinalOf singleton and verifies the pin
			// reached the EXECUTABLE plan; a non-delegator passes through
			// unchanged. Java bakes the concrete ordered child at rule time
			// (memoizePlan); this is that discipline extended through the
			// delegation spine. Declining (nil) keeps the InMemorySort
			// alternative, which is always order-correct.
			parts := make([]properties.RequestedOrderingPart, len(groupingKeys))
			for i, gk := range groupingKeys {
				// Streaming aggregation needs equal keys ADJACENT — any
				// consistent per-key direction works, matching the
				// direction-agnostic admission above.
				parts[i] = properties.RequestedOrderingPart{Value: gk, SortOrder: properties.RequestedSortOrderAny}
			}
			groupReq := properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false)
			pinned := pinOrderedSpine(orderedExpr, groupReq, call.CostModel())
			if _, isPE := pinned.(physicalPlanExpression); pinned != nil && isPE {
				// The ordered inner is a DELEGATING spine (Fetch/Filter over an
				// index): pinOrderedSpine baked it to FinalOf singletons so it
				// cannot float to an unordered sibling and split groups. Carry that
				// FROZEN edge — the correct freeze for a delegating ordered inner
				// (RFC-184 W2, no physicalStreamingAggWrapper).
				orderedQ := expressions.ForEachQuantifier(expressions.FinalOf(pinned))
				call.Yield(plans.NewRecordQueryStreamingAggregationPlanFromQuantifier(orderedQ, groupingKeys, gb.GetAggregates()))
			}
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
		// Compare grouping key and ordering key by full accessor path, not leaf
		// name (RFC-187 S10): a nested grouping key is not satisfied by an
		// ordering on a same-leaf-named top-level column.
		if !values.ColumnNamePathsEqual(gk, o.Keys[i]) {
			return false
		}
	}
	return true
}

// isFullRangeFetch reports whether a Fetch wrapper's inner index scan has
// no bound comparison ranges — i.e., it scans the entire index. A full-range
// Fetch reads every row via random PK lookups, which is always worse than
// a sequential full scan + in-memory sort.
func isFullRangeFetch(fw *plans.RecordQueryFetchFromPartialRecordPlan) bool {
	innerRef := fw.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return true
	}
	idxPlan := findIndexScanPlan(innerRef)
	if idxPlan == nil {
		return true
	}
	for _, cr := range idxPlan.GetScanComparisons() {
		if !cr.IsEmpty() {
			return false
		}
	}
	return true
}

// findIndexScanPlan scans the Reference for a bare *plans.RecordQueryIndexPlan,
// traversing through Fetch operators. The Fetch operator is a transparent
// enforcer — rules that need index properties look through it. Since RFC-184 W2
// the index scan is its own cascades expression (no physicalIndexScanWrapper),
// carrying its metadata (columns/pk/unique/covering) on the plan itself.
func findIndexScanPlan(ref *expressions.Reference) *plans.RecordQueryIndexPlan {
	if ref == nil {
		return nil
	}
	for _, m := range ref.AllMembers() {
		if p, ok := m.(*plans.RecordQueryIndexPlan); ok {
			return p
		}
		if fw, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
			if innerRef := fw.GetInnerQuantifier().GetRangesOver(); innerRef != nil {
				if p := findIndexScanPlan(innerRef); p != nil {
					return p
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
		if valueReadsField(a.Operand) {
			return false // COUNT(col) / COUNT(expr-over-col) needs the field
		}
		// COUNT(<constant>) reads no base-record field — covering is safe.
	}
	return true
}

var _ ExpressionRule = (*ImplementStreamingAggregationRule)(nil)
