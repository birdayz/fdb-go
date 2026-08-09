package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementStreamingAggregationRule implements a GroupByExpression as a
// physical RecordQueryStreamingAggregationPlan.
//
//	GroupBy(keys=[k1, k2], aggs=[...], inner)
//	  → StreamingAggPlan(inner-physical)   [when inner ordered by k1, k2, ...]
//
// The streaming aggregation is the cheapest aggregation strategy when
// the input is already sorted — it processes rows in one pass with
// O(1) memory per group.
//
// TWO PRECONDITIONS on the inner, and Java enforces both
// (ImplementStreamingAggregationRule.java:68-78 filters the plan partitions on
// the second before consulting the first at :118-119):
//
//  1. ORDERING. The inner must deliver equal grouping keys ADJACENTLY, or a
//     group is split across runs and aggregated twice. Where no inner provides
//     it, this rule inserts an in-memory sort (a Go extension — Java refuses
//     GROUP BY without sorted input).
//
//  2. CONTINUABLE WITHOUT DUPLICATES. The inner must never re-emit, across a
//     continuation, a row it already emitted. An aggregate FOLDS each row into
//     an accumulator, so a row delivered twice is counted twice and COUNT/SUM/AVG
//     come out wrong — this is the one consumer for which a re-emitted row is
//     incorrect rather than merely redundant. Checked by
//     plans.EvaluateContinuableWithoutDuplicates.
//
// Precondition 2's false set is EMPTY in Go, so its filter currently admits
// everything — see the property for the audit and for what would re-arm it.
// The guard is kept because it is correct-by-construction: it is the wrong
// thing to add reactively, after a cursor that re-emits has already shipped
// with an aggregate silently over-counting on top of it.
//
// IF THE FALSE SET EVER BECOMES NON-EMPTY, THIS RULE IS NOT ENOUGH ON ITS OWN.
// Go has no hash-aggregation rule or plan; streaming aggregation is the only
// aggregation strategy, and the in-memory-sort path below is a sort, not a
// fallback — a sort over a re-emitting inner buffers the duplicates, so the
// property (default-from-children) declines that path too. Declining the only
// path means GROUP BY over the offending shape FAILS TO PLAN rather than
// falling back. A hash aggregation has to land before anything is added to the
// false set.
//
// Java equivalent: ImplementStreamingAggregationRule in the OPTIMIZE phase.
type ImplementStreamingAggregationRule struct {
	matcher matching.BindingMatcher
}

func NewImplementStreamingAggregationRule() *ImplementStreamingAggregationRule {
	return &ImplementStreamingAggregationRule{
		matcher: NewExpressionMatcher[*expressions.GroupByExpression]("group_by"),
	}
}

func (r *ImplementStreamingAggregationRule) Matcher() matching.BindingMatcher { return r.matcher }

// admissibleStreamingAggInner is Java's plan-partition filter on
// ContinuableWithoutDuplicatesProperty (ImplementStreamingAggregationRule.java:
// 68-78). Java filters the candidate partitions once, upstream of the ordering
// check it then applies per partition at :118-119; Go's rule reaches into the
// inner Reference at several selection sites instead of binding one filtered
// partition, so the same filter is applied at each of them — every point where
// a physical member is admitted as the aggregation's inner passes through here.
//
// A member that is not a physical plan is not admissible as an inner at all,
// which is the caller's existing precondition; returning false for it keeps
// that decision in one place.
func admissibleStreamingAggInner(expr expressions.RelationalExpression) bool {
	if expr == nil {
		return false
	}
	var p plans.RecordQueryPlan
	if ph, ok := expr.(physicalPlanExpression); ok {
		p = ph.GetRecordQueryPlan()
	} else if bare, ok := expr.(plans.RecordQueryPlan); ok {
		p = bare
	}
	if p == nil {
		return false
	}
	return plans.EvaluateContinuableWithoutDuplicates(p)
}

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
			// The covering scan is REACHED, not built: the access path already
			// emitted Fetch(Covering(IndexScan)), so this rule reaches through
			// the fetch to the covering child rather than stamping a flag onto
			// an index plan. Nothing here decides coveringness.
			//
			// EVERY reachable covering scan yields its own parent. A group with
			// two index accesses to the same table offers two count-answering
			// scans of different widths, and taking the first is a cost decision
			// made where the cost model cannot see it — the runner-up is not
			// out-priced, it is never built. Java's onMatch likewise yields once
			// per candidate plan partition (ImplementStreamingAggregationRule.java:117-122).
			for _, coveringPlan := range findCoveringIndexPlans(innerRef) {
				if !admissibleStreamingAggInner(coveringPlan) || !coveringPlan.ProducesDistinctRecords() {
					continue
				}
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
		//
		// Same member-set policy as every other enumeration in this rule:
		// finals, with exploratory only as the no-finals fallback. See
		// findOrderedPhysicalExprs for why Java settles it that way.
		for _, m := range physicalMembersForParentEnumeration(innerRef) {
			if !admissibleStreamingAggInner(m) {
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

	// The pre-aggregate ordering requirement is expressed over the PRIMITIVE
	// LEAF accessors of each grouping key, not the key itself — Java's
	// Values.primitiveAccessorsForType (Values.java:99-121), applied on the
	// grouping path at ImplementStreamingAggregationRule.java:111. This is
	// what makes GROUP BY <struct> answerable: a RECORD-typed key flattens
	// into its leaves in field order (recursively), so the sort comparator
	// only ever orders primitives — the "no ordering defined between
	// *dynamicpb.Message" row-time crash is unreachable. Group-break
	// EQUALITY still evaluates the original keys (the executor packs
	// composite key values losslessly), matching Java, where equality is
	// message-level while ordering is leaf-level.
	orderingKeys, expandErr := expandGroupingKeysToPrimitives(groupingKeys)
	if expandErr != nil {
		// A grouping key with no leaf decomposition (ARRAY/RELATION):
		// declining yields no plan — the ORDER-path outcome for an
		// unsatisfiable ordering — rather than a bolted-on type check.
		return
	}
	sortKeys := make([]plans.SortKey, len(orderingKeys))
	for i, gk := range orderingKeys {
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
	if rawExpr != nil && admissibleStreamingAggInner(rawExpr) {
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
	//
	// EVERY ordered physical member yields its own parent, not just the first
	// one found. A single pick makes the non-picked members invisible to cost
	// rather than out-priced by it, and the two filters below are precisely
	// where that goes wrong: `admissibleStreamingAggInner` and the full-range
	// Fetch skip can REJECT the first ordered member while a later member — the
	// bare index scan over the same group — would have been admitted. The rule
	// then yields no index path at all and the aggregate falls back to
	// InMemorySort(FullScan), reading and materializing the whole table where an
	// ordered index scan was available. Enumerating is also what the parent
	// construction does elsewhere; a rule-time single pick is a cost decision
	// taken where the cost model cannot see it.
	for _, orderedExpr := range findOrderedPhysicalExprs(innerRef, orderingKeys) {
		if !admissibleStreamingAggInner(orderedExpr) {
			continue
		}
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
			parts := make([]properties.RequestedOrderingPart, len(orderingKeys))
			for i, gk := range orderingKeys {
				// Streaming aggregation needs equal keys ADJACENT — any
				// consistent per-key direction works, matching the
				// direction-agnostic admission above. Leaf accessors, same
				// set the admission matched on.
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

// findOrderedPhysicalExprs returns EVERY physical-plan member of ref whose
// ordering satisfies the grouping keys (in order), in member order.
//
// It returns all of them rather than the first because its callers apply
// admissibility filters afterwards: returning one member means a rejected
// first candidate suppresses an admissible later one, and the difference is
// invisible — the plan simply comes out without the index, with nothing
// reporting that an alternative existed.
//
// The member set is physicalMembersForParentEnumeration's, not AllMembers().
// Two enumeration policies for one concept is how a divergence starts, and here
// Java settles which one is right: the plan partitions this rule matches over
// come from Reference.toPlanPartitions, which reads the reference's
// propertiesMap, and that map is fed ONLY by FINAL expressions — at construction
// (Reference.java:182) and in insertUnchecked under `if (isFinal)`
// (Reference.java:372-378). Exploratory members never enter it, so Java's
// ImplementStreamingAggregationRule cannot see one. Go keeps exploratory members
// as a FALLBACK only, for references that hold no finals yet because a rule
// fired mid-planning — a situation Java does not have.
func findOrderedPhysicalExprs(ref *expressions.Reference, groupingKeys []values.Value) []expressions.RelationalExpression {
	var out []expressions.RelationalExpression
	for _, m := range physicalMembersForParentEnumeration(ref) {
		o := properties.EstimateOrdering(m)
		if !o.IsKnown {
			continue
		}
		if orderingSatisfiesGroupingKeys(o, groupingKeys) {
			out = append(out, m)
		}
	}
	return out
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

// findCoveringIndexPlans locates EVERY covering index scan reachable in ref,
// descending through a Fetch exactly as findIndexScanPlan does. The access path
// builds Fetch(Covering(IndexScan)), so the covering scan a rule wants to place
// under an operator that needs no base record is one level below the fetch.
//
// It returns all of them rather than the first for the same reason
// findOrderedPhysicalExprs does: the caller yields a parent per covering scan,
// and a group holding several index accesses to the same table offers several
// count-answering scans of different widths. Picking one at rule time makes the
// rest invisible to the cost model rather than out-priced by it — a cost
// decision taken where cost cannot see it. Java never picks one either: its
// onMatch loops over every candidate plan partition and yields a plan per
// partition (ImplementStreamingAggregationRule.java:117-122), memoizing ALL of
// the partition's plans into the new inner reference (:130).
//
// Members are returned in group order and may repeat a plan reachable by two
// routes; the memo dedups on insertion, so the caller yields per distinct scan.
// The member set is findOrderedPhysicalExprs's — finals, exploratory only as the
// no-finals fallback — because this is the same kind of decision: which
// alternatives get a parent built over them.
func findCoveringIndexPlans(ref *expressions.Reference) []*plans.RecordQueryCoveringIndexPlan {
	if ref == nil {
		return nil
	}
	var out []*plans.RecordQueryCoveringIndexPlan
	for _, m := range physicalMembersForParentEnumeration(ref) {
		if p, ok := m.(*plans.RecordQueryCoveringIndexPlan); ok {
			out = append(out, p)
			continue
		}
		if fw, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
			if innerRef := fw.GetInnerQuantifier().GetRangesOver(); innerRef != nil {
				out = append(out, findCoveringIndexPlans(innerRef)...)
			}
		}
	}
	return out
}

// findIndexScanPlan scans the Reference for the *plans.RecordQueryIndexPlan
// that performs the actual index read, traversing through the two wrappers that
// stand between a group member and its scan. The Fetch operator is a
// transparent enforcer — rules that need index properties look through it.
// Since RFC-184 W2 the index scan is its own cascades expression (no
// physicalIndexScanWrapper), carrying its metadata (columns/pk/unique) on the
// plan itself.
//
// The COVERING wrapper is traversed by DELEGATION, not by descending into a
// child Reference, because it holds its scan as a FIELD (RFC-220 C1) — the same
// way Java reaches it, by forwarding the getter to the wrapped plan
// (RecordQueryCoveringIndexPlan.java:224 for getCorrelatedTo). Missing it is
// not a missing optimization but a WRONG ANSWER: callers ask this for the scan
// comparisons, and a nil answer reads as "no comparisons", i.e. as a full-range
// scan. Since the access path builds Fetch(Covering(IndexScan)) for every
// value-index access, stopping at the wrapper makes every fetch answer
// full-range.
//
// This one keeps AllMembers() on purpose, unlike every enumeration above it.
// It is a LOOKUP, not a choice: it answers "what index does this subtree read,
// and with what ranges" for a caller that has already decided which member it
// cares about. No parent is built over what it returns, so restricting it to
// finals would only make it fail to answer for a group mid-planning — and its
// callers read a non-answer as "unrestricted scan", the expensive direction.
func findIndexScanPlan(ref *expressions.Reference) *plans.RecordQueryIndexPlan {
	if ref == nil {
		return nil
	}
	for _, m := range ref.AllMembers() {
		if p, ok := m.(*plans.RecordQueryIndexPlan); ok {
			return p
		}
		if cov, ok := m.(*plans.RecordQueryCoveringIndexPlan); ok {
			if p := cov.GetIndexPlan(); p != nil {
				return p
			}
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
