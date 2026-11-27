// Portions derived from FoundationDB Record Layer (
// ValueIndexLikeMatchCandidate.java, OrderingProperty.java,
// AggregateIndexMatchCandidate.java, RecordQueryIndexPlan.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package plans

import (
	"fdb.dev/internal/recordlayer/query/plan/cascades/properties"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

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
		if sk.ValueExpr == nil {
			// ValueExpr is REQUIRED of every SortKey (see in_memory_sort.go),
			// and the executor rejects a nil one as a malformed plan, never a
			// name read. Minting a lazy FieldValue from SortKey.Field here would
			// advertise an ordering for a plan the cursor refuses to run, and
			// in a second vocabulary: SortKey.Field is a DISPLAY rendering
			// (ExplainValue's output, correlation and `#ordinal` included),
			// which the match-domain identity (AccessorNamePath) declines, so
			// the ordering would not compare with a baked one.
			//
			// UNKNOWN rather than a panic: HintOrdering has no error channel,
			// and the executor is the one loud enforcement point. Under-claiming
			// costs a plan shape; over-claiming advertises an ordering the plan
			// does not provide.
			return properties.Ordering{}
		}
		// The key's OWN Value is the identity, and the sort re-orders rows
		// without reshaping them, so the Value the executor evaluates per row is
		// exactly the Value this plan provides an ordering ON.
		keys[i] = sk.ValueExpr
		desc[i] = sk.Desc
		nullsFirst[i] = sk.NullsFirst
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc, NullsFirst: nullsFirst}
}

// HintOrdering: a merge-sort union emits rows in its comparison-key order,
// in the direction it merges. See RecordQueryInUnionPlan.HintOrdering for why
// the single reverse flag is the whole truth about that direction.
func (p *RecordQueryMergeSortUnionPlan) HintOrdering() properties.Ordering {
	return mergeComparisonKeyOrdering(p.GetComparisonKeys(), p.IsReverse())
}

// HintOrdering: an intersection emits rows in its semantic comparison-key
// order. Use the ordering parts rather than the executable comparison Values:
// a future mixed/counterflow key may be physically encoded as ordered bytes,
// but the SQL-visible ordering remains over the original columns.
func (p *RecordQueryIntersectionPlan) HintOrdering() properties.Ordering {
	if p == nil {
		return properties.Ordering{}
	}
	parts := p.GetComparisonKeyOrderingParts()
	if len(parts) == 0 {
		return properties.Ordering{}
	}
	keys := make([]values.Value, len(parts))
	descending := make([]bool, len(parts))
	nullsFirst := make([]bool, len(parts))
	for i, part := range parts {
		if !part.SortOrder.IsDirectional() {
			return properties.Ordering{}
		}
		keys[i] = part.Value
		descending[i] = part.SortOrder.IsAnyDescending()
		nullsFirst[i] = part.SortOrder == properties.ProvidedSortOrderAscending ||
			part.SortOrder == properties.ProvidedSortOrderDescendingNullsFirst
	}
	return properties.Ordering{
		IsKnown:    true,
		Keys:       keys,
		Descending: descending,
		NullsFirst: nullsFirst,
	}
}

// HintOrdering: an InUnion emits rows in its comparison-key order, in the
// direction it merges its per-binding legs.
//
// The reverse flag IS the direction of every comparison key: the rules that
// build these merges refuse any candidate whose parts do not all agree with it
// (properties.NaturalComparisonKeyValues), because the executable comparison
// key is the raw Value and a key read forward cannot express a descending
// component. Reporting the keys without their direction advertised a
// descending merge as ascending, so a matching ORDER BY DESC saw its own
// access path as unsatisfying and kept an in-memory sort over it.
func (p *RecordQueryInUnionPlan) HintOrdering() properties.Ordering {
	return mergeComparisonKeyOrdering(p.GetComparisonKeys(), p.IsReverse())
}

// HintRichOrdering: the merge dedups on its comparison key, so its output is
// distinct over exactly those coordinates, whatever its legs claimed.
func (p *RecordQueryInUnionPlan) HintRichOrdering() *properties.RichOrdering {
	keys := p.GetComparisonKeys()
	if len(keys) == 0 {
		return properties.EmptyOrdering()
	}
	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	bm := make(map[values.Value][]properties.OrderingBinding, len(keys))
	for _, k := range keys {
		bm[k] = []properties.OrderingBinding{properties.SortedBinding(dir)}
	}
	return properties.NewRichOrdering(bm, keys, properties.DistinctOverAllKeys())
}

// mergeComparisonKeyOrdering builds the provided ordering of a merge set
// operation: its comparison keys, every one of them in the merge direction.
// NULL placement stays natural (ASC → nulls first, DESC → nulls last) — the
// merge compares raw tuple-encoded values, which is exactly the natural
// placement.
func mergeComparisonKeyOrdering(keys []values.Value, reverse bool) properties.Ordering {
	if !reverse {
		return properties.Ordering{IsKnown: true, Keys: keys}
	}
	descending := make([]bool, len(keys))
	for i := range descending {
		descending[i] = true
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: descending}
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
	// The comparison key is CHILD-row relative (the merge cursor evaluates it
	// against each stream's [groupCols..., FUNC(col)] row); the ordering this
	// plan ADVERTISES is over the row it EMITS. The two agree on the grouping
	// ordinals — group column i is slot i of both — but they are different
	// layouts, and only the output row is the one a requested ORDER BY key is
	// baked against. So the advertised key restates the same ordinal in the
	// OUTPUT row's domain instead of handing out the child-relative Value.
	//
	// No derivable output layout leaves the keys as they are: an
	// unaddressable ordering key costs an elision, one domained in a layout
	// nothing can name would be a claim rather than a proof.
	names := p.outputColumnNames()
	if len(names) == 0 {
		return mergeComparisonKeyOrdering(compKey, p.IsReverse())
	}
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil || outputLayout == nil || outputLayout.Carrier() == nil {
		return mergeComparisonKeyOrdering(compKey, p.IsReverse())
	}
	keys := make([]values.Value, len(compKey))
	for i, k := range compKey {
		fv, isField := values.AsFieldValue(k)
		path := values.FieldPathView(nil)
		if isField {
			path = fv.Path()
		}
		if !isField || fv.ChildValue() == nil || path == nil ||
			path.Len() != 1 || i >= len(names) {
			// Anything but "grouping column i, read at slot i" is not the
			// shape this restatement is proven for.
			return mergeComparisonKeyOrdering(compKey, p.IsReverse())
		}
		accessor, ok := path.Accessor(0)
		if !ok || accessor.Ordinal() != i {
			return mergeComparisonKeyOrdering(compKey, p.IsReverse())
		}
		request, err := values.FieldByNameAndOrdinal(names[i], i)
		if err != nil {
			return mergeComparisonKeyOrdering(compKey, p.IsReverse())
		}
		resolved, err := values.ResolveFieldAccess(outputLayout.Carrier(), []values.FieldRequest{request})
		if err != nil {
			return mergeComparisonKeyOrdering(compKey, p.IsReverse())
		}
		keys[i] = resolved
	}
	return mergeComparisonKeyOrdering(keys, p.IsReverse())
}

// HintRichOrdering is the comparison-key ordering with each grouping column a
// stream fixes carried as that stream's FixedBinding, as Java derives the
// intersection's ordering from its children's (Ordering.merge with INTERSECTION:
// a binding fixed in one stream is fixed in the rows every stream agrees on).
// Grouping column i is slot i of every stream and of the output.
func (p *RecordQueryMultiIntersectionOnValuesPlan) HintRichOrdering() *properties.RichOrdering {
	plain := p.HintOrdering()
	if !plain.IsKnown || len(plain.Keys) == 0 {
		return properties.EmptyOrdering()
	}
	fixed := make(map[int]any)
	for _, stream := range p.GetChildren() {
		hinter, ok := stream.(properties.RichOrderingHinter)
		if !ok {
			continue
		}
		rich := hinter.HintRichOrdering()
		if rich == nil {
			continue
		}
		for key, bindings := range rich.GetBindingMap() {
			if len(bindings) != 1 || !bindings[0].IsFixed() {
				continue
			}
			fv, isField := values.AsFieldValue(key)
			if !isField || fv.Path() == nil || fv.Path().Len() != 1 {
				continue
			}
			accessor, ok := fv.Path().Accessor(0)
			if ok && accessor.Ordinal() >= 0 && accessor.Ordinal() < len(plain.Keys) {
				fixed[accessor.Ordinal()] = bindings[0].GetComparison()
			}
		}
	}
	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	bm := make(map[values.Value][]properties.OrderingBinding, len(plain.Keys))
	for i, key := range plain.Keys {
		if comparison, isFixed := fixed[i]; isFixed {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comparison)}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	return properties.NewRichOrdering(bm, plain.Keys, properties.NotDistinct())
}

// outputColumnNames returns the column names of the row this plan emits, taken
// from its result value's record-constructor fields, or nil when the result
// value is not a record constructor (no nameable output layout).
func (p *RecordQueryMultiIntersectionOnValuesPlan) outputColumnNames() []string {
	rc, isRecord := p.GetResultValue().(*values.RecordConstructorValue)
	if !isRecord {
		return nil
	}
	names := make([]string, len(rc.Fields))
	for i, f := range rc.Fields {
		names[i] = f.Name
	}
	return names
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
// A grouping key that TERMINATES the ordering claim truncates it here exactly
// as it does on an index scan, and for the same reason: a streaming
// aggregation emits its groups in the order its input hands them over, so a
// FLOAT/DOUBLE grouping key means the groups come out in tuple-key order, with
// a negative-NaN group physically FIRST and logically LAST.
//
// The truncation is asked of the same authority the scan producers ask
// (values.TypeTerminatesOrderingClaim, via claimableKeyLimit) because the
// grouping keys are Values that already carry their declared type. This plan
// does NOT inherit the decision from its inner: StreamingAggFromIndexRule
// builds it by matching grouping keys against index column NAMES and never
// reads the inner's ordering claim at all, so terminating the inner scan's
// claim does not reach this producer.
func (p *RecordQueryStreamingAggregationPlan) HintOrdering() properties.Ordering {
	if p == nil || len(p.GetGroupingKeys()) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	groupKeys := p.GetGroupingKeys()
	groupKeys = groupKeys[:claimableTypedKeyLimit(groupKeys)]
	if len(groupKeys) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil || outputLayout == nil || outputLayout.Carrier() == nil {
		return properties.Ordering{IsKnown: false}
	}
	keys := make([]values.Value, len(groupKeys))
	for i := range groupKeys {
		// NAME AND ORDINAL together, and the pairing is load-bearing on a row
		// that carries two same-named group keys — `GROUP BY ot.k, it.k` emits
		// [K, K, COUNT(*)]. The ordinal selects the slot and the name verifies
		// it, so the canonical output name stays the authority it is documented
		// to be without the duplicate making the request unanswerable.
		request, err := values.FieldByNameAndOrdinal(p.outputNames[i], i)
		if err != nil {
			return properties.Ordering{IsKnown: false}
		}
		keys[i], err = values.ResolveFieldAccess(outputLayout.Carrier(), []values.FieldRequest{request})
		if err != nil {
			return properties.Ordering{IsKnown: false}
		}
	}
	desc := make([]bool, len(keys))
	if orderProducingScanIsReverse(p.GetInner()) {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// orderProducingScanIsReverse reports whether the index scan that actually
// produces plan's rows runs in reverse.
//
// The DIRECTION of a delegating producer's ordering is a correctness claim, not
// decoration: a consumer that reads "ascending" elides its own sort, so an
// ascending claim over a descending scan returns wrongly-ordered rows with
// nothing failing. Matching only the bare *RecordQueryIndexPlan made that claim
// fail SILENTLY in the ascending direction — the type assertion misses, the
// direction slice stays all-false, and the miss is indistinguishable from a
// genuinely forward scan.
//
// Two wrappers stand between an operator and its scan, and neither changes row
// ORDER, so both are traversed:
//
//   - RecordQueryCoveringIndexPlan holds its scan as a FIELD, not a child, so
//     no child walk reaches it. Java answers the same way — by DELEGATING the
//     getter to the wrapped plan rather than by walking children
//     (RecordQueryCoveringIndexPlan.java:224 for getCorrelatedTo).
//   - RecordQueryFetchFromPartialRecordPlan resolves entries to base records in
//     the order its inner hands them over, which is why it delegates IsReverse
//     itself. It is the shape the access path now always builds beneath an
//     operator that needs the full record, so an ordered inner reaching this
//     function is normally Fetch(Covering(IndexScan)).
func orderProducingScanIsReverse(plan RecordQueryPlan) bool {
	switch p := plan.(type) {
	case *RecordQueryIndexPlan:
		return p.IsReverse()
	case *RecordQueryCoveringIndexPlan:
		return orderProducingScanIsReverse(p.GetIndexPlan())
	case *RecordQueryFetchFromPartialRecordPlan:
		return orderProducingScanIsReverse(p.GetInner())
	default:
		return false
	}
}

// HintOrdering is the plain projection of HintRichOrdering.
func (p *RecordQueryInJoinPlan) HintOrdering() properties.Ordering {
	return p.HintRichOrdering().PlainOrdering()
}

// HintRichOrdering ports Java's OrderingProperty.visitInJoinPlan. The inner
// runs once per IN value, so its own order holds only within one value: a
// sorted source makes the IN value the leading sorted key, while an unsorted
// one leaves only the inner's other equality-bound keys.
func (p *RecordQueryInJoinPlan) HintRichOrdering() *properties.RichOrdering {
	inner := richOrderingOf(p.innerQ.GetRangesOver())
	inValue := inJoinValueForIn(inner, p.bindingAlias)
	if inValue == nil || !p.sorted {
		bm := make(map[values.Value][]properties.OrderingBinding)
		var keys []values.Value
		for _, k := range inner.GetKeys() {
			if inValue != nil && values.ExplainValue(k) == values.ExplainValue(inValue) {
				continue
			}
			var fixed []properties.OrderingBinding
			for _, b := range inner.GetBindingMap()[k] {
				if b.IsFixed() {
					fixed = append(fixed, b)
				}
			}
			if len(fixed) > 0 {
				bm[k] = fixed
				keys = append(keys, k)
			}
		}
		return properties.NewRichOrdering(bm, keys, properties.NotDistinct())
	}
	dir := properties.ProvidedSortOrderAscending
	if p.reverse {
		dir = properties.ProvidedSortOrderDescending
	}
	outer := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{inValue: {properties.SortedBinding(dir)}},
		[]values.Value{inValue}, properties.DistinctOverAllKeys())
	filtered := inner.WithoutKeys(map[string]struct{}{values.ExplainValue(inValue): {}})
	return properties.ConcatOrderings(outer, filtered)
}

// inJoinValueForIn is Java's findValueForIn: the inner ordering key whose
// equality binding reads exactly the IN binding.
func inJoinValueForIn(inner *properties.RichOrdering, inAlias values.CorrelationIdentifier) values.Value {
	for _, k := range inner.GetKeys() {
		for _, b := range inner.GetBindingMap()[k] {
			if !b.IsFixed() {
				continue
			}
			correlatedTo := b.ComparisonCorrelatedTo()
			if len(correlatedTo) != 1 {
				continue
			}
			if _, ok := correlatedTo[inAlias]; ok {
				return k
			}
		}
	}
	return nil
}
