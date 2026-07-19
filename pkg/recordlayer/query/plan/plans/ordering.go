package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Output ordering, owned by the plans themselves (RFC-183 P5).
//
// Two shapes live here:
//
//   - PRODUCERS advertise an order derived from their own state (an index
//     scan's key columns, a sort's sort keys, a merge union's comparison
//     keys). They answer properties.OrderingHinter directly.
//
//   - DELEGATORS preserve their input's order rather than producing one
//     (filter, projection, limit, fetch, …). They expose OrderingSourceRef so
//     ordering satisfaction and extraction-time sort elision can resolve
//     through the SOURCE, and their HintOrdering inherits from it.
//
// A delegator plan resolves through its OWN child quantifier — the concrete
// child plan — whereas the physical wrapper of the same operator resolves
// through the memo GROUP it was built over. Those are different questions
// (one concrete child vs. every explored alternative), which is why the two
// are not collapsed: the memo keeps asking the wrapper.

// orderingSourceOf returns the reference a single-child plan's ordering flows
// from — its one child quantifier's group.
func orderingSourceOf(p RecordQueryPlan) *expressions.Reference {
	qs := p.GetQuantifiers()
	if len(qs) == 0 {
		return nil
	}
	return qs[0].GetRangesOver()
}

// inheritOrdering returns the first known ordering among a reference's
// members. After exploration the ordering-providing alternative may not be
// the first member, so every member is consulted.
func inheritOrdering(ref *expressions.Reference) properties.Ordering {
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		if o := properties.EstimateOrdering(m); o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

// --- delegators -------------------------------------------------------------

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a filter preserves its input's order.
func (p *RecordQueryFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryPredicatesFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a filter preserves its input's order.
func (p *RecordQueryPredicatesFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryTypeFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a type filter preserves its input's order.
func (p *RecordQueryTypeFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryDistinctPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: duplicate elimination drops repeats without reordering the
// survivors.
func (p *RecordQueryDistinctPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryProjectionPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a projection reshapes rows without reordering them.
func (p *RecordQueryProjectionPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryMapPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a map reshapes rows without reordering them.
func (p *RecordQueryMapPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryLimitPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a limit truncates a stream without reordering it.
func (p *RecordQueryLimitPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryDefaultOnEmptyPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: passing the child through preserves its order.
func (p *RecordQueryDefaultOnEmptyPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryFetchFromPartialRecordPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: fetching the full record per index entry preserves the index
// scan's order.
func (p *RecordQueryFetchFromPartialRecordPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// --- producers --------------------------------------------------------------

// HintOrdering: a primary scan produces rows in primary-key order.
func (p *RecordQueryScanPlan) HintOrdering() properties.Ordering {
	return PKScanOrdering(p)
}

// PKScanOrdering returns a primary scan's PK ordering. Shared with the
// data-access path's plan-backed leaf, which memoizes a SARGed PK scan.
func PKScanOrdering(plan *RecordQueryScanPlan) properties.Ordering {
	if plan == nil {
		return properties.Ordering{}
	}
	pk := plan.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.Ordering{}
	}
	desc := make([]bool, len(pk))
	if plan.IsReverse() {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: pk, Descending: desc}
}

// HintOrdering: an index scan produces rows in index-key order for the
// non-equality-bound suffix columns, extended by the trimmed primary-key
// suffix (index entries are (index key, primary key), so the PK columns
// continue the sort order). E.g. index(a, b, c) with a = 1 over PK (id)
// produces output sorted by (b, c, id). Mirrors the full-key ordering of
// Java's ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons.
func (p *RecordQueryIndexPlan) HintOrdering() properties.Ordering {
	if p == nil || len(p.GetColumnNames()) == 0 {
		return properties.Ordering{}
	}
	columnNames, pkColumnNames := p.GetColumnNames(), p.GetPKColumnNames()
	comps := p.GetScanComparisons()
	firstNonEq := 0
	for i, cr := range comps {
		if cr.IsEquality() {
			firstNonEq = i + 1
		} else {
			break
		}
	}
	rev := p.IsReverse()
	keys := make([]values.Value, 0, len(columnNames)-firstNonEq+len(pkColumnNames))
	desc := make([]bool, 0, cap(keys))
	for i := firstNonEq; i < len(columnNames); i++ {
		keys = append(keys, &values.FieldValue{Field: columnNames[i], Typ: values.UnknownType})
		desc = append(desc, rev)
	}
	for _, col := range TrimmedPKSuffix(columnNames, pkColumnNames) {
		keys = append(keys, &values.FieldValue{Field: col, Typ: values.UnknownType})
		desc = append(desc, rev)
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// TrimmedPKSuffix returns the primary-key columns not already present in the
// index key columns, in PK order. Ports Java's Index.trimPrimaryKey semantics
// as used by ValueIndexExpansionVisitor.fullKey: PK components that appear in
// the index key are trimmed, the remainder is appended after the index key.
func TrimmedPKSuffix(columnNames, pkColumnNames []string) []string {
	if len(pkColumnNames) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(columnNames))
	for _, c := range columnNames {
		seen[c] = struct{}{}
	}
	suffix := make([]string, 0, len(pkColumnNames))
	for _, col := range pkColumnNames {
		if _, dup := seen[col]; dup {
			continue
		}
		seen[col] = struct{}{}
		suffix = append(suffix, col)
	}
	return suffix
}

// HintOrdering: an in-memory sort produces exactly its sort keys. NULL
// placement is carried so a parent sort does not elide against a counterflow
// (e.g. ASC NULLS LAST) stream as if it were natural order.
func (p *RecordQueryInMemorySortPlan) HintOrdering() properties.Ordering {
	if p == nil {
		return properties.Ordering{}
	}
	sks := p.GetSortKeys()
	keys := make([]values.Value, len(sks))
	desc := make([]bool, len(sks))
	nullsFirst := make([]bool, len(sks))
	for i, sk := range sks {
		keys[i] = &values.FieldValue{Field: sk.Field, Typ: values.UnknownType}
		desc[i] = sk.Desc
		nullsFirst[i] = sk.NullsFirst
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc, NullsFirst: nullsFirst}
}

// HintOrdering: a merge-sort union emits rows in its comparison-key order.
func (p *RecordQueryMergeSortUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: true, Keys: p.GetComparisonKeys()}
}

// HintOrdering: an InUnion emits rows in its comparison-key order.
func (p *RecordQueryInUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: true, Keys: p.GetComparisonKeys()}
}

// HintOrdering: a multi-way intersection emits rows in its comparison-key
// order, when it has one.
func (p *RecordQueryMultiIntersectionOnValuesPlan) HintOrdering() properties.Ordering {
	if p == nil {
		return properties.Ordering{}
	}
	compKey := p.GetComparisonKey()
	if len(compKey) == 0 {
		return properties.Ordering{}
	}
	return properties.Ordering{IsKnown: true, Keys: compKey}
}

// HintOrdering: the advertised ordering is over the aggregate's OUTPUT row —
// group key i flows as output column i, NAMED by the canonical group-key
// output name (AggregateKeyColumnName, the same authority the runtime output
// row and the ORDER-BY-over-aggregate bake use). Advertising the raw
// grouping-key VALUES (input-relative bakes over the pre-aggregate row)
// mis-rendered the provided keys and made a satisfied ORDER BY look
// unsatisfied — a spurious second InMemorySort above the aggregate; an
// evaluating consumer (a merge comparison key) would also have read the
// aggregate's output row with a dead pre-aggregate ordinal.
func (p *RecordQueryStreamingAggregationPlan) HintOrdering() properties.Ordering {
	if p == nil || len(p.GetGroupingKeys()) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	groupKeys := p.GetGroupingKeys()
	keys := make([]values.Value, len(groupKeys))
	for i, k := range groupKeys {
		keys[i] = values.NewFieldValueWithResolvedOrdinal(
			expressions.AggregateKeyColumnName(k), i, values.UnknownType)
	}
	desc := make([]bool, len(keys))
	if idx, ok := p.GetInner().(*RecordQueryIndexPlan); ok && idx.IsReverse() {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// HintOrdering: an aggregate index is stored grouped, so it emits one row per
// group in group-column order.
func (p *RecordQueryAggregateIndexPlan) HintOrdering() properties.Ordering {
	groupCols := p.GetGroupCols()
	if len(groupCols) == 0 {
		return properties.Ordering{IsKnown: true}
	}
	keys := make([]values.Value, len(groupCols))
	desc := make([]bool, len(groupCols))
	for i, col := range groupCols {
		keys[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
		desc[i] = p.IsReverse()
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// --- unordered --------------------------------------------------------------

// HintOrdering: an InJoin iterates IN-values one at a time. Each batch
// preserves the inner scan's ordering, but the GLOBAL result ordering depends
// on the IN-source order, not the inner scan. Claiming the inner's ordering
// would let sort elimination remove a necessary ORDER BY.
func (p *RecordQueryInJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: an unordered union interleaves its legs arbitrarily.
func (p *RecordQueryUnorderedUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: a K-NN probe returns its neighbours in an order the ordering
// property does not model.
func (p *RecordQueryVectorIndexPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: exploding a collection yields elements in no modeled order.
func (p *RecordQueryExplodePlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a dependent join's output order is not modeled.
func (p *RecordQueryFlatMapPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a nested-loop join's output order is not modeled.
func (p *RecordQueryNestedLoopJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: recursive traversal order is not modeled.
func (p *RecordQueryRecursiveDfsJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: recursive level order is not modeled.
func (p *RecordQueryRecursiveLevelUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a table function's output order is opaque.
func (p *RecordQueryTableFunctionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a temp-table insert's output order is not modeled.
func (p *RecordQueryTempTableInsertPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a temp-table scan reads an unordered buffer.
func (p *RecordQueryTempTableScanPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// --- rich orderings ---------------------------------------------------------
//
// HintRichOrdering is the binding-carrying form of HintOrdering: it keeps the
// distinction between a key that is EQUALITY-BOUND (FixedBinding — every row
// holds the same value, so the key satisfies a request in EITHER direction)
// and one that is merely SORTED in a given direction. That distinction is what
// lets `WHERE a = 1 ORDER BY a DESC` elide its sort over a forward scan, and
// plain Ordering cannot express it.
//
// Only the plans whose ordering carries bindings implement it. Everything else
// is served by the caller's fallback, which synthesizes a sorted-only binding
// map from HintOrdering.

// HintRichOrdering returns a primary scan's PK ordering with bindings: PK
// positions bound by an equality comparison become FixedBinding entries, the
// rest SortedBinding. A primary scan is a value-index-like candidate in Java
// (PrimaryScanMatchCandidate implements ValueIndexLikeMatchCandidate), so its
// ordering comes from the same computeOrderingFromScanComparisons: the
// equality prefix is Binding.fixed, which is compatible with ANY requested
// direction.
func (p *RecordQueryScanPlan) HintRichOrdering() *properties.RichOrdering {
	if p == nil {
		return properties.EmptyOrdering()
	}
	pk := p.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.EmptyOrdering()
	}
	comps := p.GetScanComparisons()
	bm := make(map[values.Value][]properties.OrderingBinding, len(pk))
	keys := make([]values.Value, 0, len(pk))
	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	for i, key := range pk {
		keys = append(keys, key)
		if i < len(comps) && comps[i].IsEquality() {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	return properties.NewRichOrdering(bm, keys, false)
}

// HintRichOrdering returns the index scan's full ordering with bindings:
// equality-bound prefix columns become FixedBinding entries (carrying the
// comparison), non-equality suffix columns become SortedBinding entries. The
// trimmed primary-key suffix continues the sorted keys — this is what lets an
// equality-prefixed scan (status = ?) satisfy ORDER BY pk, exactly as Java's
// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons derives the
// ordering over getFullKeyExpression() (index key + trimmed PK) with
// Binding.fixed for the equality prefix and Binding.sorted for the rest.
//
// Note this differs from HintOrdering, which DROPS the equality prefix
// entirely; here the prefix is retained as fixed, which is strictly more
// information.
func (p *RecordQueryIndexPlan) HintRichOrdering() *properties.RichOrdering {
	if p == nil || len(p.GetColumnNames()) == 0 {
		return properties.EmptyOrdering()
	}
	columnNames, pkColumnNames := p.GetColumnNames(), p.GetPKColumnNames()
	comps := p.GetScanComparisons()
	bm := make(map[values.Value][]properties.OrderingBinding)
	keys := make([]values.Value, 0, len(columnNames)+len(pkColumnNames))

	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	for i, col := range columnNames {
		key := &values.FieldValue{Field: col, Typ: values.UnknownType}
		keys = append(keys, key)
		if i < len(comps) && comps[i].IsEquality() {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	for _, col := range TrimmedPKSuffix(columnNames, pkColumnNames) {
		key := &values.FieldValue{Field: col, Typ: values.UnknownType}
		keys = append(keys, key)
		bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
	}
	return properties.NewRichOrdering(bm, keys, p.IsUnique())
}

// HintRichOrdering: an HNSW probe returns its neighbours in distance order,
// which is not a column ordering the planner models. Empty rather than a
// synthesized fallback, so no caller mistakes distance order for key order.
func (p *RecordQueryVectorIndexPlan) HintRichOrdering() *properties.RichOrdering {
	return properties.EmptyOrdering()
}

// HintRichOrdering: fetching the full record per index entry preserves the
// index scan's rich ordering, so inherit it from the source.
//
// This DUPLICATES the physical wrapper's body rather than delegating to it,
// for the same reason the plain-ordering delegators do (see this file's
// header): the wrapper resolves through the memo GROUP it was built over,
// consulting every explored alternative, while the plan resolves through its
// OWN concrete child quantifier. Those are different questions and must stay
// two answers.
func (p *RecordQueryFetchFromPartialRecordPlan) HintRichOrdering() *properties.RichOrdering {
	ref := p.OrderingSourceRef()
	if ref == nil {
		return properties.EmptyOrdering()
	}
	for _, m := range ref.AllMembers() {
		if rh, ok := m.(properties.RichOrderingHinter); ok {
			return rh.HintRichOrdering()
		}
	}
	return properties.EmptyOrdering()
}
