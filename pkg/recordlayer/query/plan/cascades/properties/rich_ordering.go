package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RichOrdering captures a provided ordering as a binding map
// (Value → []OrderingBinding) plus a partially ordered set of value
// keys and a distinctness flag.
//
// The ordering set is a PartiallyOrderedSet[string] keyed by
// ExplainValue(v). A reverse map resolves keys back to values.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.Ordering.
type RichOrdering struct {
	bindingMap  map[values.Value][]OrderingBinding
	keys        []values.Value
	orderingSet *combinatorics.PartiallyOrderedSet[string]
	keyLookup   map[string]values.Value
	distinct    bool
}

// NewRichOrdering creates a new ordering from bindings, key sequence,
// and distinctness. The ordering set is a total order matching the key sequence.
func NewRichOrdering(
	bindingMap map[values.Value][]OrderingBinding,
	keys []values.Value,
	distinct bool,
) *RichOrdering {
	return NewRichOrderingWithDeps(bindingMap, keys, nil, distinct)
}

// NewRichOrderingWithDeps creates an ordering with explicit dependency edges
// in the ordering set.
func NewRichOrderingWithDeps(
	bindingMap map[values.Value][]OrderingBinding,
	keys []values.Value,
	deps combinatorics.SetMultimap[string],
	distinct bool,
) *RichOrdering {
	bm := make(map[values.Value][]OrderingBinding, len(bindingMap))
	for k, v := range bindingMap {
		copied := make([]OrderingBinding, len(v))
		copy(copied, v)
		bm[k] = copied
	}

	keysCopy := make([]values.Value, len(keys))
	copy(keysCopy, keys)

	keyStrings := make([]string, len(keys))
	lookup := make(map[string]values.Value, len(keys))
	for i, k := range keys {
		s := values.ExplainValue(k)
		keyStrings[i] = s
		lookup[s] = k
	}

	var oset *combinatorics.PartiallyOrderedSet[string]
	if deps != nil {
		oset = combinatorics.NewPartiallyOrderedSet(keyStrings, deps)
	} else {
		bd := combinatorics.NewBuilder[string]()
		bd.AddAll(keyStrings)
		var lastSorted string
		for _, ks := range keyStrings {
			v := lookup[ks]
			bindings := bm[v]
			if !AreAllBindingsFixed(bindings) {
				if lastSorted != "" {
					bd.AddDependency(ks, lastSorted)
				}
				lastSorted = ks
			}
		}
		oset = bd.Build()
	}

	return &RichOrdering{
		bindingMap:  bm,
		keys:        keysCopy,
		orderingSet: oset,
		keyLookup:   lookup,
		distinct:    distinct,
	}
}

// EmptyOrdering returns an ordering with no keys.
func EmptyOrdering() *RichOrdering {
	return &RichOrdering{
		bindingMap:  map[values.Value][]OrderingBinding{},
		orderingSet: combinatorics.EmptyPartiallyOrderedSet[string](),
		keyLookup:   map[string]values.Value{},
	}
}

// GetBindingMap returns the binding map.
func (o *RichOrdering) GetBindingMap() map[values.Value][]OrderingBinding {
	return o.bindingMap
}

// GetKeys returns the ordering keys in sequence.
func (o *RichOrdering) GetKeys() []values.Value {
	return o.keys
}

// OrderingSet returns the partially ordered set of value keys.
func (o *RichOrdering) OrderingSet() *combinatorics.PartiallyOrderedSet[string] {
	return o.orderingSet
}

// ValueForKey resolves a string key back to its Value.
func (o *RichOrdering) ValueForKey(key string) values.Value {
	return o.keyLookup[key]
}

// IsDistinct returns whether the ordering guarantees distinct output.
func (o *RichOrdering) IsDistinct() bool {
	return o.distinct
}

// GetEqualityBoundValues returns the set of values that have at least
// one fixed (equality-bound) binding. Matches Java's
// Ordering.getEqualityBoundValues() which uses
// Multimaps.filterValues(Binding::isFixed).keySet().
func (o *RichOrdering) GetEqualityBoundValues() map[values.Value]struct{} {
	result := make(map[values.Value]struct{})
	for v, bindings := range o.bindingMap {
		for _, b := range bindings {
			if b.IsFixed() {
				result[v] = struct{}{}
				break
			}
		}
	}
	return result
}

// GetOrderingKeys returns the non-equality-bound ordering keys (those
// that contribute to the sort order). Mirrors Java's
// Ordering.getOrderingSet().getSet() filtered to the keys list.
func (o *RichOrdering) GetOrderingKeys() []values.Value {
	var result []values.Value
	for _, v := range o.keys {
		bindings := o.bindingMap[v]
		if !AreAllBindingsFixed(bindings) {
			result = append(result, v)
		}
	}
	return result
}

// SortOrder returns the aggregate sort order of a value's bindings.
// If all bindings are sorted with the same direction, returns that
// direction. If mixed or all fixed, returns ProvidedSortOrderFixed.
func SortOrderOf(bindings []OrderingBinding) ProvidedSortOrder {
	if len(bindings) == 0 {
		return ProvidedSortOrderFixed
	}
	var seenSorted ProvidedSortOrder
	hasSorted := false
	for _, b := range bindings {
		if b.IsSorted() {
			if !hasSorted {
				seenSorted = b.GetSortOrder()
				hasSorted = true
			} else if seenSorted != b.GetSortOrder() {
				return ProvidedSortOrderFixed
			}
		}
	}
	if hasSorted {
		return seenSorted
	}
	return ProvidedSortOrderFixed
}

// AreAllBindingsFixed returns true if all bindings are fixed
// (equality-bound).
func AreAllBindingsFixed(bindings []OrderingBinding) bool {
	for _, b := range bindings {
		if !b.IsFixed() {
			return false
		}
	}
	return true
}

// HasMultipleFixedBindings returns true if there are 2+ fixed bindings.
func HasMultipleFixedBindings(bindings []OrderingBinding) bool {
	count := 0
	for _, b := range bindings {
		if b.IsFixed() {
			count++
			if count >= 2 {
				return true
			}
		}
	}
	return false
}

// FixedBinding returns the single fixed binding from the list.
// Panics if there isn't exactly one.
func SingleFixedBinding(bindings []OrderingBinding) OrderingBinding {
	for _, b := range bindings {
		if b.IsFixed() {
			return b
		}
	}
	panic("no fixed binding found")
}

// IsSingularNonFixedValue returns true if the value has exactly one
// binding and that binding is not fixed (sorted or choose).
func (o *RichOrdering) IsSingularNonFixedValue(v values.Value) bool {
	bindings := o.bindingMap[v]
	return len(bindings) == 1 && !bindings[0].IsFixed()
}

// Satisfies checks whether this provided ordering satisfies the given
// requested ordering. Uses the partial order to enumerate valid
// permutations that match the request prefix.
func (o *RichOrdering) Satisfies(requested *RequestedOrdering) bool {
	if requested.IsDistinct() && !o.distinct {
		return false
	}
	if requested.IsPreserve() {
		return true
	}
	parts := requested.GetParts()
	if len(parts) == 0 {
		return true
	}

	requestedValues := make(map[string]struct{}, len(parts))
	requestedKeys := make([]string, len(parts))
	for i, part := range parts {
		k, ok := o.orderingKeyFor(part.Value)
		if !ok {
			return false
		}
		bindings := o.bindingMapForExplain(k)
		if bindings == nil {
			return false
		}
		sortOrder := SortOrderOf(bindings)
		if !sortOrder.IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return false
		}
		requestedValues[k] = struct{}{}
		requestedKeys[i] = k
	}

	filtered := o.orderingSet.FilterElements(func(s string) bool {
		_, ok := requestedValues[s]
		return ok
	})

	iter := combinatorics.SatisfyingPermutations(
		filtered,
		requestedKeys,
		func(_ []string) int { return len(parts) },
	)
	return iter.Next() != nil
}

func valuesEqual(a, b values.Value) bool {
	return values.ValuesStructurallyEqual(a, b)
}

func (o *RichOrdering) bindingMapForExplain(explain string) []OrderingBinding {
	v, ok := o.keyLookup[explain]
	if !ok {
		return nil
	}
	return o.bindingMap[v]
}

// orderingKeyFor resolves a requested-ordering part's Value to the ordering
// set's key for it: the exact ExplainValue rendering first, then the narrow
// quantifier-root bridge. ok=false when neither identifies a known ordering key.
//
// The separate ORDINAL-FREE same-column arm that used to sit between the two is
// GONE, and its removal is a SIMPLIFICATION, not a change of behaviour: it was a
// fast path for a comparison the quantifier-root arm below already performs.
// CanBridgeOrderingValueRoots begins by calling CanBridgeOrderingFieldValues,
// which is that same lazy-vs-baked ordinal-free name comparison, so every pair
// the deleted arm could resolve the loop below still resolves. Measured over the
// 2481-query corpus its own hit count fell 5289 → 0 as the ordering providers
// resolved their metadata column lists against the layouts they flow; the pairs
// did not stop meeting, they stopped needing this arm to meet.
//
// So the display name has NOT left this function. It survives inside the root
// arm, where 1824 corpus resolutions still run through it, and retiring it means
// putting the request into the provider's correlation space before matching —
// which is a translation, not a stricter comparison. Until that lands, what
// keeps the remaining name comparison from being a conflation is
// CanBridgeOrderingFieldValues' own refusal to bridge two BAKED values: two
// stated ordinals are decided by their ordinals, never by their spelling.
func (o *RichOrdering) orderingKeyFor(v values.Value) (string, bool) {
	k := values.ExplainValue(v)
	if _, ok := o.keyLookup[k]; ok {
		return k, true
	}

	// A requested key scoped to one SELECT child remains rooted at that
	// quantifier (C.NAME), while a scan/index ordering is source-local (NAME).
	// The two renderings cannot meet, so use the narrow full-accessor-path
	// bridge and reject an ambiguous multi-key match.
	var bridged string
	for key, provided := range o.keyLookup {
		if !values.CanBridgeOrderingValueRoots(v, provided) {
			continue
		}
		if bridged != "" && bridged != key {
			return "", false
		}
		bridged = key
	}
	if bridged != "" {
		return bridged, true
	}
	return "", false
}

// IsCompatibleWithRequestedSortOrder checks if a provided sort order is
// compatible with a requested sort order. Ports Java
// OrderingPart.ProvidedSortOrder.isCompatibleWithRequestedSortOrder
// (OrderingPart.java:322-332) exactly:
//
//   - ANY request, or a CHOOSE/FIXED provided order, is always compatible
//     (and these early-returns must come BEFORE the counterflow check, since
//     IsCounterflowNulls is only meaningful for directional values);
//   - otherwise the NULL placement must agree (counterflow-nulls equality);
//   - and the direction (ascending-ness) must agree, compared via
//     IsAnyAscending on BOTH sides (so e.g. ASC_NULLS_LAST provided is
//     compatible with an ASC_NULLS_LAST request, not just plain ASCENDING).
//
// This is the gate that keeps a forward (ASC_NULLS_FIRST) scan from
// satisfying an `ORDER BY x ASC NULLS LAST` (ASC_NULLS_LAST) request, so the
// sort is not wrongly elided.
func (p ProvidedSortOrder) IsCompatibleWithRequestedSortOrder(req RequestedSortOrder) bool {
	if req == RequestedSortOrderAny || p == ProvidedSortOrderChoose || p == ProvidedSortOrderFixed {
		return true
	}
	if p.IsCounterflowNulls() != req.IsCounterflowNulls() {
		return false
	}
	return p.IsAnyAscending() == req.IsAnyAscending()
}

// EnumerateSatisfyingComparisonKeyValues enumerates the comparison key
// sequences from this ordering that satisfy the requested ordering.
// Uses TopologicalSort.satisfyingPermutations on the partial order to
// find all valid orderings.
func (o *RichOrdering) EnumerateSatisfyingComparisonKeyValues(
	requested *RequestedOrdering,
) [][]values.Value {
	if requested.IsPreserve() || requested.Size() == 0 {
		return [][]values.Value{o.keys}
	}

	reqParts := requested.GetParts()
	requestedKeys := make([]string, len(reqParts))
	for i, part := range reqParts {
		// Counterflow NULL placement (e.g. ASC NULLS LAST) cannot be
		// satisfied by a set-operation comparison key here: Go emits a
		// plain comparison Value, not the ToOrderedBytesValue physical
		// encoding Java uses (ProvidedOrderingPart.comparisonKeyValue) to
		// re-order NULLs against the natural tuple order. Refusing here
		// (rather than relying on IsCompatibleWithRequestedSortOrder, which
		// early-returns true for FIXED/CHOOSE bindings) prevents a merge
		// set-op from claiming to satisfy a counterflow request while
		// emitting a natural-null key. Such requests fall back to an
		// in-memory sort. Porting ToOrderedBytesValue is tracked in
		// DIVERGENCES.md.
		if part.SortOrder.IsCounterflowNulls() {
			return nil
		}
		k, ok := o.orderingKeyFor(part.Value)
		if !ok {
			return nil
		}
		bindings := o.bindingMapForExplain(k)
		if bindings == nil {
			return nil
		}
		sortOrder := SortOrderOf(bindings)
		if !sortOrder.IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return nil
		}
		requestedKeys[i] = k
	}

	iter := combinatorics.SatisfyingPermutations(
		o.orderingSet,
		requestedKeys,
		func(_ []string) int { return len(reqParts) },
	)

	var results [][]values.Value
	for {
		perm := iter.Next()
		if perm == nil {
			break
		}
		keyValues := make([]values.Value, len(perm))
		for i, k := range perm {
			keyValues[i] = o.keyLookup[k]
		}
		results = append(results, keyValues)
	}
	if len(results) == 0 {
		return nil
	}
	return results
}

// EnumerateSatisfyingIntersectionComparisonKeyValues enumerates comparison
// keys with Java's intersection semantics. Equality-bound values remain in
// the merged ordering's binding metadata, but they do not participate in the
// physical merge key: every surviving row has one constant value there.
//
// This intentionally differs from EnumerateSatisfyingComparisonKeyValues,
// whose older Go callers treat o.keys as the physical key wholesale. Java's
// SetOperationsOrdering filters to singular non-fixed values, removes fixed
// values from the requested prefix, and then enumerates full topological
// permutations of that reduced set.
func (o *RichOrdering) EnumerateSatisfyingIntersectionComparisonKeyValues(
	requested *RequestedOrdering,
) [][]values.Value {
	if o == nil || requested == nil {
		return nil
	}
	if requested.IsDistinct() && !o.distinct {
		return nil
	}

	var reducedRequestedKeys []string
	for _, part := range requested.GetParts() {
		key, ok := o.orderingKeyFor(part.Value)
		if !ok {
			return nil
		}
		bindings := o.bindingMapForExplain(key)
		if bindings == nil {
			return nil
		}
		sortOrder := SortOrderOf(bindings)
		if !sortOrder.IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return nil
		}
		if sortOrder != ProvidedSortOrderFixed {
			reducedRequestedKeys = append(reducedRequestedKeys, key)
		}
	}

	filtered := o.orderingSet.FilterElements(func(key string) bool {
		value := o.keyLookup[key]
		return value != nil && o.IsSingularNonFixedValue(value)
	})
	if filtered.Size() == 0 {
		// Java's topological iterator yields the one empty permutation.
		return [][]values.Value{{}}
	}

	iter := combinatorics.SatisfyingPermutations(
		filtered,
		reducedRequestedKeys,
		func(_ []string) int { return len(reducedRequestedKeys) },
	)
	var results [][]values.Value
	for {
		permutation := iter.Next()
		if permutation == nil {
			break
		}
		keyValues := make([]values.Value, len(permutation))
		for i, key := range permutation {
			keyValues[i] = o.keyLookup[key]
		}
		results = append(results, keyValues)
	}
	return results
}

// SatisfiesGroupingValues checks whether the given set of values can
// form a valid prefix of some topological ordering of this ordering set.
func (o *RichOrdering) SatisfiesGroupingValues(groupingValues map[string]struct{}) bool {
	if len(groupingValues) == 0 {
		return true
	}
	if o.orderingSet.Size() < len(groupingValues) {
		return false
	}
	for gv := range groupingValues {
		bindings := o.bindingMapForExplain(gv)
		if bindings == nil {
			return false
		}
		if AreAllBindingsFixed(bindings) && HasMultipleFixedBindings(bindings) {
			return false
		}
	}

	filtered := o.orderingSet.FilterElements(func(s string) bool {
		_, ok := groupingValues[s]
		return ok
	})
	iter := combinatorics.TopologicalOrderPermutations(filtered)
	for {
		perm := iter.Next()
		if perm == nil {
			break
		}
		if len(perm) >= len(groupingValues) {
			prefixOK := true
			for i := 0; i < len(groupingValues); i++ {
				if _, ok := groupingValues[perm[i]]; !ok {
					prefixOK = false
					break
				}
			}
			if prefixOK {
				return true
			}
		}
	}
	return false
}

// EnumerateCompatibleRequestedOrderings enumerates all valid orderings
// of the full ordering set that are compatible with the requested
// ordering prefix. Each result is a full-length sequence of
// RequestedOrderingParts (one per key in the ordering set).
func (o *RichOrdering) EnumerateCompatibleRequestedOrderings(
	requested *RequestedOrdering,
) [][]RequestedOrderingPart {
	parts := requested.GetParts()
	requestedKeys := make([]string, len(parts))
	for i, part := range parts {
		k, ok := o.orderingKeyFor(part.Value)
		if !ok {
			return nil
		}
		bindings := o.bindingMapForExplain(k)
		if bindings == nil {
			return nil
		}
		if !SortOrderOf(bindings).IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return nil
		}
		requestedKeys[i] = k
	}

	iter := combinatorics.SatisfyingPermutations(
		o.orderingSet,
		requestedKeys,
		func(_ []string) int { return len(parts) },
	)

	var results [][]RequestedOrderingPart
	for {
		perm := iter.Next()
		if perm == nil {
			break
		}
		reqParts := make([]RequestedOrderingPart, len(perm))
		for i, k := range perm {
			v := o.keyLookup[k]
			bindings := o.bindingMap[v]
			if AreAllBindingsFixed(bindings) {
				reqParts[i] = RequestedOrderingPart{Value: v, SortOrder: RequestedSortOrderAny}
			} else {
				reqParts[i] = RequestedOrderingPart{Value: v, SortOrder: SortOrderOf(bindings).ToRequestedSortOrder()}
			}
		}
		results = append(results, reqParts)
	}
	return results
}

// DirectionalOrderingParts creates ProvidedOrderingParts from a key
// sequence with sort directions from the binding map. Fixed keys get
// the provided fixedOrder direction.
func (o *RichOrdering) DirectionalOrderingParts(
	keyValues []values.Value,
	requested *RequestedOrdering,
	fixedOrder ProvidedSortOrder,
) []ProvidedOrderingPart {
	parts := make([]ProvidedOrderingPart, 0, len(keyValues))
	for _, key := range keyValues {
		bindings := o.bindingMap[key]
		sortOrder := SortOrderOf(bindings)

		if !sortOrder.IsDirectional() {
			if reqSort, ok := o.requestedSortOrderForKey(requested, key); ok && reqSort.IsDirectional() {
				// Map to the natural provided value for the direction. We
				// never stamp a counterflow provided order here: a counterflow
				// request is refused at the satisfaction gate
				// (IsCompatibleWithRequestedSortOrder) and in the comparison-key
				// enumeration, so this can never claim to satisfy NULLS LAST
				// via a plain (non-ToOrderedBytes) comparison key.
				if reqSort.IsAnyAscending() {
					sortOrder = ProvidedSortOrderAscending
				} else {
					sortOrder = ProvidedSortOrderDescending
				}
			} else {
				sortOrder = fixedOrder
			}
		}

		parts = append(parts, ProvidedOrderingPart{
			Value:     key,
			SortOrder: sortOrder,
		})
	}
	return parts
}

// requestedSortOrderForKey resolves a requested part through the ordering's
// semantic/normalized key bridge. Requested and provided FieldValues are
// routinely rebuilt independently (and may differ in case or bake state), so
// interface-key identity is not a valid lookup. Ambiguous conflicting requests
// collapse to ANY, matching RequestedOrdering's map semantics.
func (o *RichOrdering) requestedSortOrderForKey(
	requested *RequestedOrdering,
	value values.Value,
) (RequestedSortOrder, bool) {
	if requested == nil {
		return RequestedSortOrderAny, false
	}
	target, ok := o.orderingKeyFor(value)
	if !ok {
		return RequestedSortOrderAny, false
	}
	var result RequestedSortOrder
	found := false
	for _, part := range requested.GetParts() {
		key, partOK := o.orderingKeyFor(part.Value)
		if !partOK || key != target {
			continue
		}
		if !found {
			result = part.SortOrder
			found = true
		} else if result != part.SortOrder {
			return RequestedSortOrderAny, true
		}
	}
	return result, found
}

// OrderingMergeKind determines the semantics of merging two orderings.
type OrderingMergeKind int

const (
	OrderingMergeUnion OrderingMergeKind = iota
	OrderingMergeIntersection
)

// ConcatOrderings concatenates two orderings using proper partial-order
// semantics. The maximum elements of the left ordering become dependencies
// of the minimum elements of the right ordering (for non-fixed keys).
func ConcatOrderings(outer, inner *RichOrdering) *RichOrdering {
	if outer == nil || inner == nil {
		panic("ConcatOrderings: both orderings are required")
	}
	if !outer.distinct {
		panic("ConcatOrderings: outer ordering must be distinct")
	}

	bm := make(map[values.Value][]OrderingBinding, len(outer.bindingMap)+len(inner.bindingMap))
	for k, v := range outer.bindingMap {
		bm[k] = append([]OrderingBinding{}, v...)
	}
	for k, v := range inner.bindingMap {
		if _, exists := bm[k]; !exists {
			bm[k] = append([]OrderingBinding{}, v...)
		}
	}

	outerKeySet := make(map[string]struct{}, len(outer.keys))
	for _, k := range outer.keys {
		outerKeySet[values.ExplainValue(k)] = struct{}{}
	}

	keys := make([]values.Value, len(outer.keys))
	copy(keys, outer.keys)
	for _, k := range inner.keys {
		if _, exists := outerKeySet[values.ExplainValue(k)]; !exists {
			keys = append(keys, k)
		}
	}

	deps := combinatorics.NewSetMultimap[string]()
	for _, entry := range outer.orderingSet.DependencyMap().Entries() {
		deps.Put(entry.Key, entry.Value)
	}
	for _, entry := range inner.orderingSet.DependencyMap().Entries() {
		deps.Put(entry.Key, entry.Value)
	}

	leftMax := outer.orderingSet.DualOrder().EligibleSet().EligibleElements()
	rightMin := inner.orderingSet.EligibleSet().EligibleElements()

	for lm := range leftMax {
		lv := outer.keyLookup[lm]
		if lv != nil && AreAllBindingsFixed(outer.bindingMap[lv]) {
			continue
		}
		for rm := range rightMin {
			rv := inner.keyLookup[rm]
			if rv != nil && AreAllBindingsFixed(inner.bindingMap[rv]) {
				continue
			}
			deps.Put(rm, lm)
		}
	}

	// Java Ordering.concatOrderings requires the left ordering to be
	// distinct: only then does one complete inner run precede the next
	// without interleaving. The concatenated rows themselves are distinct
	// exactly when the RIGHT ordering is distinct; outer distinctness alone
	// does not prevent duplicate rows within one inner run.
	return NewRichOrderingWithDeps(bm, keys, deps, inner.distinct)
}

// PullUp translates this ordering through a string-keyed value mapping.
// For each ordering key, if the mapping contains a replacement (keyed
// by ExplainValue string), the key is renamed. Bindings are preserved.
// Keys not in the mapping are dropped.
//
// This is the simpler of two pull-up paths:
//   - PullUp(mapping): string-keyed FieldValue-to-FieldValue renaming.
//     Used by projection rules and callers that build an explicit
//     rename map.
//   - PullUpThroughValue(resultValue, alias): full Value-tree-aware
//     pull-up via values.PullUpValue. Handles RecordConstructorValue
//     decomposition and passthrough (QOV/ObjectValue) semantics.
//     Used by callers that have a result-value structure rather than
//     a flat rename map.
//
// Both paths produce correct results for their use cases. Callers
// should prefer PullUpThroughValue when the result value is available,
// as it handles nested value structures (e.g. record constructors)
// that a flat string map cannot represent.
func (o *RichOrdering) PullUp(mapping map[string]values.Value) *RichOrdering {
	if o == nil {
		return nil
	}
	return o.translateKeys(mapping)
}

// PushDown is the inverse of PullUp — translates ordering keys
// back through a reverse mapping.
func (o *RichOrdering) PushDown(mapping map[string]values.Value) *RichOrdering {
	return o.PullUp(mapping)
}

// PullUpThroughValue translates this ordering through a result value.
// For each ordering key, uses Value.PullUpValue to compute the pulled-up key.
// Value-backed fixed comparison operands are translated through the same
// result value; literal, parameter, and opaque fixed bindings are unchanged.
// A dynamic fixed binding that cannot be translated is dropped, and a key
// whose last binding is dropped cannot contribute an ordering above the
// projection.
//
// Ports Java's Ordering.pullUp(Value, EvaluationContext, AliasMap,
// Set<CorrelationIdentifier>) using the direct algorithmic pullUp
// from values.PullUpValue.
func (o *RichOrdering) PullUpThroughValue(resultValue values.Value, alias values.CorrelationIdentifier) *RichOrdering {
	if o == nil {
		return nil
	}

	pulledUpMap := values.PullUpValues(o.keys, resultValue, alias)
	if len(pulledUpMap) == 0 {
		return NewRichOrdering(nil, nil, o.distinct)
	}

	translated := make(map[string]values.Value, len(pulledUpMap))
	for _, key := range o.keys {
		if pulledUp, ok := pulledUpMap[key]; ok {
			translated[values.ExplainValue(key)] = pulledUp
		}
	}
	return o.translateKeysAndBindings(translated, func(value values.Value) values.Value {
		return values.PullUpValue(value, resultValue, alias)
	})
}

// PushDownThroughValue translates this ordering's keys from output
// space back to input space through a result value. For each ordering
// key, uses Value.PushDownValue to compute the pushed-down key. Value-backed
// fixed comparison operands are pushed down through the same result value;
// literal, parameter, and opaque fixed bindings are unchanged. Keys that
// cannot be pushed down, or whose last dynamic fixed binding cannot be pushed
// down, are dropped.
//
// Ports Java's Ordering.pushDown(Value, EvaluationContext, AliasMap,
// Set<CorrelationIdentifier>).
func (o *RichOrdering) PushDownThroughValue(resultValue values.Value, upperAlias values.CorrelationIdentifier) *RichOrdering {
	if o == nil {
		return nil
	}

	pushed := values.PushDownValues(o.keys, resultValue, upperAlias)

	translated := make(map[string]values.Value, len(o.keys))
	for i, key := range o.keys {
		if pushed[i] != nil {
			translated[values.ExplainValue(key)] = pushed[i]
		}
	}
	return o.translateKeysAndBindings(translated, func(value values.Value) values.Value {
		return values.PushDownValue(value, resultValue, upperAlias)
	})
}

// translateKeys maps the ordering-set domain without inventing dependencies.
// Rebuilding the surviving keys through NewRichOrdering would create a total
// order and could turn independent keys into a false lexicographic guarantee.
// MapAll also removes a key whose last prerequisite was not translatable; that
// conservative loss is necessary because the dependent key can reset whenever
// the omitted prerequisite changes.
func (o *RichOrdering) translateKeys(mapping map[string]values.Value) *RichOrdering {
	return o.translateKeysWithOverrides(mapping, nil)
}

// translateKeysAndBindings applies the same value translation to fixed
// comparison operands before remapping the ordering-set domain. Java's
// Ordering.translateBindings translates only ValueComparison bindings:
// literal/simple and parameter comparisons stay unchanged, while an
// untranslatable value comparison disappears. Go represents scan equality
// bindings as *predicates.ComparisonRange, so the equivalent operation rebuilds
// its equality comparison with the translated Operand.
func (o *RichOrdering) translateKeysAndBindings(
	mapping map[string]values.Value,
	translateValue func(values.Value) values.Value,
) *RichOrdering {
	filteredMapping := make(map[string]values.Value, len(mapping))
	bindingOverrides := make(map[string][]OrderingBinding, len(mapping))
	for oldKey, mappedValue := range mapping {
		oldValue := o.keyLookup[oldKey]
		if oldValue == nil || mappedValue == nil {
			continue
		}
		translatedBindings := translateOrderingBindings(
			o.bindingMap[oldValue], translateValue)
		if len(translatedBindings) == 0 {
			continue
		}
		filteredMapping[oldKey] = mappedValue
		bindingOverrides[oldKey] = translatedBindings
	}
	return o.translateKeysWithOverrides(filteredMapping, bindingOverrides)
}

func (o *RichOrdering) translateKeysWithOverrides(
	mapping map[string]values.Value,
	bindingOverrides map[string][]OrderingBinding,
) *RichOrdering {
	if len(mapping) == 0 {
		return NewRichOrdering(nil, nil, o.distinct)
	}

	stringMapping := make(map[string]string, len(mapping))
	valueByMappedKey := make(map[string]values.Value, len(mapping))
	oldKeyByMappedKey := make(map[string]string, len(mapping))
	for oldKey, mappedValue := range mapping {
		if mappedValue == nil {
			continue
		}
		mappedKey := values.ExplainValue(mappedValue)
		if previous, collision := oldKeyByMappedKey[mappedKey]; collision && previous != oldKey {
			// Two source keys collapsing onto one output key require binding
			// and dependency coalescing. Decline rather than guess.
			return NewRichOrdering(nil, nil, o.distinct)
		}
		stringMapping[oldKey] = mappedKey
		valueByMappedKey[mappedKey] = mappedValue
		oldKeyByMappedKey[mappedKey] = oldKey
	}

	mappedSet := combinatorics.MapAll(o.orderingSet, stringMapping)
	if mappedSet.IsEmpty() {
		return NewRichOrdering(nil, nil, o.distinct)
	}

	newBM := make(map[values.Value][]OrderingBinding, mappedSet.Size())
	newKeys := make([]values.Value, 0, mappedSet.Size())
	for _, mappedKey := range mappedSet.Set() {
		mappedValue := valueByMappedKey[mappedKey]
		oldValue := o.keyLookup[oldKeyByMappedKey[mappedKey]]
		if mappedValue == nil || oldValue == nil {
			// Malformed translations fail closed.
			return NewRichOrdering(nil, nil, o.distinct)
		}
		newKeys = append(newKeys, mappedValue)
		if translatedBindings, ok := bindingOverrides[oldKeyByMappedKey[mappedKey]]; ok {
			newBM[mappedValue] = translatedBindings
		} else {
			newBM[mappedValue] = o.bindingMap[oldValue]
		}
	}
	return NewRichOrderingWithDeps(
		newBM, newKeys, mappedSet.DependencyMap(), o.distinct)
}

// translateOrderingBindings mirrors Java Ordering.translateBindings for Go's
// binding representation. Non-fixed bindings need no comparand translation.
// Unknown fixed payloads remain opaque, matching Java's treatment of
// non-ValueComparison comparison subclasses.
func translateOrderingBindings(
	bindings []OrderingBinding,
	translateValue func(values.Value) values.Value,
) []OrderingBinding {
	if !AreAllBindingsFixed(bindings) {
		// Java deliberately discards any comparison detail once a value is
		// directional: a valid non-fixed binding set is represented by one
		// sorted binding. Go accepts a few broader intermediate shapes, so
		// collapse every consistently directional one and fail closed for an
		// inconsistent mix. CHOOSE is already a single, comparison-free
		// binding and can pass through unchanged.
		if len(bindings) == 1 && bindings[0].IsChoose() {
			return []OrderingBinding{bindings[0]}
		}
		sortOrder := SortOrderOf(bindings)
		if !sortOrder.IsDirectional() {
			return nil
		}
		return []OrderingBinding{SortedBinding(sortOrder)}
	}

	translated := make([]OrderingBinding, 0, len(bindings))
	for _, binding := range bindings {
		comparison, ok := translateFixedOrderingComparison(
			binding.GetComparison(), translateValue)
		if ok {
			translated = append(translated, FixedBinding(comparison))
		}
	}
	return translated
}

func translateFixedOrderingComparison(
	comparison any,
	translateValue func(values.Value) values.Value,
) (any, bool) {
	if comparison == nil {
		return nil, true
	}

	switch comparison := comparison.(type) {
	case *predicates.ComparisonRange:
		if comparison == nil || !comparison.IsEquality() {
			// A non-equality range cannot soundly be a FIXED binding.
			return nil, false
		}
		translatedComparison, ok := translateOrderingComparison(
			comparison.GetEqualityComparison(), translateValue)
		if !ok {
			return nil, false
		}
		if translatedComparison == comparison.GetEqualityComparison() {
			return comparison, true
		}
		merged := predicates.EmptyComparisonRange().Merge(translatedComparison)
		if !merged.Ok {
			return nil, false
		}
		return merged.Range, true

	case *predicates.Comparison:
		return translateOrderingComparison(comparison, translateValue)

	case predicates.Comparison:
		translatedComparison, ok := translateOrderingComparison(
			&comparison, translateValue)
		if !ok {
			return nil, false
		}
		return *translatedComparison, true

	default:
		return comparison, true
	}
}

func translateOrderingComparison(
	comparison *predicates.Comparison,
	translateValue func(values.Value) values.Value,
) (*predicates.Comparison, bool) {
	if comparison == nil {
		return nil, false
	}
	if comparison.Operand == nil ||
		comparison.ParameterName != "" ||
		values.IsConstantValue(comparison.Operand) {
		return comparison, true
	}
	if _, isParameterValue := comparison.Operand.(*values.ParameterValue); isParameterValue {
		return comparison, true
	}

	translatedOperand := translateValue(comparison.Operand)
	if translatedOperand == nil {
		return nil, false
	}
	translatedComparison := *comparison
	translatedComparison.Operand = translatedOperand
	return &translatedComparison, true
}

// CreateUnionOrdering creates a RichOrdering from a single provided
// ordering, treating all sorted bindings as union-compatible.
// Used as the starting point for union-merge in ImplementDistinctUnionRule.
func CreateUnionOrdering(o *RichOrdering) *RichOrdering {
	bm := make(map[values.Value][]OrderingBinding, len(o.bindingMap))
	for k, v := range o.bindingMap {
		bm[k] = append([]OrderingBinding{}, v...)
	}
	keys := make([]values.Value, len(o.keys))
	copy(keys, o.keys)
	return NewRichOrdering(bm, keys, o.distinct)
}

// MergeOrderingsForUnion merges two orderings with union semantics.
// Uses the full EligibleSet-based merge algorithm from Java.
func MergeOrderingsForUnion(a, b *RichOrdering) *RichOrdering {
	return mergeOrderings(a, b, combineBindingsForUnion, a.distinct && b.distinct, false)
}

// MergeOrderingsForIntersection merges two orderings with intersection semantics.
func MergeOrderingsForIntersection(a, b *RichOrdering) *RichOrdering {
	// Java passes (left, right) -> true for intersection: rows surviving
	// every leg are distinct under the comparison contract even when an
	// individual input ordering does not advertise distinctness.
	return mergeOrderings(a, b, combineBindingsForIntersection, true, true)
}

// MergeOrderings merges two orderings (union semantics, backward-compat alias).
func MergeOrderings(a, b *RichOrdering) *RichOrdering {
	return MergeOrderingsForUnion(a, b)
}

type bindingCombiner func(left, right []OrderingBinding) []OrderingBinding

func mergeOrderings(
	a, b *RichOrdering,
	combine bindingCombiner,
	distinct bool,
	normalizeFixedDependencies bool,
) *RichOrdering {
	leftES := a.orderingSet.EligibleSet()
	rightES := b.orderingSet.EligibleSet()

	var elements []string
	deps := combinatorics.NewSetMultimap[string]()
	bm := make(map[values.Value][]OrderingBinding)
	lookup := make(map[string]values.Value)
	seenElements := make(map[string]struct{})

	var lastElements []string

	addElement := func(element string, value values.Value, bindings []OrderingBinding) {
		if value == nil || len(bindings) == 0 {
			return
		}
		if _, seen := seenElements[element]; !seen {
			elements = append(elements, element)
			seenElements[element] = struct{}{}
		}
		lookup[element] = value
		bm[value] = bindings
	}

	for !leftES.IsEmpty() && !rightES.IsEmpty() {
		leftElems := leftES.EligibleElements()
		rightElems := rightES.EligibleElements()

		var intersected []string
		for le := range leftElems {
			for re := range rightElems {
				if le == re {
					lv := a.keyLookup[le]
					rv := b.keyLookup[re]
					combined := combine(a.bindingMap[lv], b.bindingMap[rv])
					if len(combined) > 0 {
						intersected = append(intersected, le)
						if lv != nil {
							addElement(le, lv, combined)
						} else if rv != nil {
							addElement(le, rv, combined)
						}
					}
				}
			}
		}

		for le := range leftElems {
			if _, inRight := rightElems[le]; !inRight {
				lv := a.keyLookup[le]
				combined := combine(a.bindingMap[lv], nil)
				if len(combined) > 0 {
					addElement(le, lv, combined)
				}
			}
		}
		for re := range rightElems {
			if _, inLeft := leftElems[re]; !inLeft {
				rv := b.keyLookup[re]
				combined := combine(nil, b.bindingMap[rv])
				if len(combined) > 0 {
					addElement(re, rv, combined)
				}
			}
		}

		if len(intersected) == 0 {
			break
		}

		for _, ie := range intersected {
			for _, le := range lastElements {
				if a.orderingSet.DependencyMap().Contains(ie, le) ||
					b.orderingSet.DependencyMap().Contains(ie, le) {
					deps.Put(ie, le)
				}
			}
		}

		removeSet := make(map[string]struct{}, len(intersected))
		for _, ie := range intersected {
			removeSet[ie] = struct{}{}
		}
		leftES = leftES.RemoveEligibleElements(removeSet)
		rightES = rightES.RemoveEligibleElements(removeSet)

		lastElements = intersected
	}

	vals := make([]values.Value, 0, len(elements))
	for _, e := range elements {
		if v, ok := lookup[e]; ok {
			vals = append(vals, v)
		}
	}

	if normalizeFixedDependencies {
		deps = normalizeOrderingDependencies(bm, lookup, deps)
	}
	return NewRichOrderingWithDeps(bm, vals, deps, distinct)
}

// normalizeOrderingDependencies mirrors Java Ordering.normalizeOrderingSet.
// A value that becomes fixed while two orderings are merged is independent of
// every other value. Remove dependencies from that value, and bypass it when a
// non-fixed value depended on it, retaining any transitive non-fixed
// dependencies beyond the fixed value.
func normalizeOrderingDependencies(
	bindingMap map[values.Value][]OrderingBinding,
	keyLookup map[string]values.Value,
	dependencyMap combinatorics.SetMultimap[string],
) combinatorics.SetMultimap[string] {
	normalized := combinatorics.NewSetMultimap[string]()
	isFixed := func(key string) bool {
		return AreAllBindingsFixed(bindingMap[keyLookup[key]])
	}

	for key, dependencies := range dependencyMap {
		if isFixed(key) {
			continue
		}

		queue := make([]string, 0, len(dependencies))
		for dependency := range dependencies {
			queue = append(queue, dependency)
		}
		visitedFixed := make(map[string]struct{})
		for len(queue) > 0 {
			dependency := queue[0]
			queue = queue[1:]
			if !isFixed(dependency) {
				normalized.Put(key, dependency)
				continue
			}
			if _, seen := visitedFixed[dependency]; seen {
				continue
			}
			visitedFixed[dependency] = struct{}{}
			for transitiveDependency := range dependencyMap.Get(dependency) {
				queue = append(queue, transitiveDependency)
			}
		}
	}

	return normalized
}

func combineBindingsForUnion(left, right []OrderingBinding) []OrderingBinding {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	leftSort := SortOrderOf(left)
	rightSort := SortOrderOf(right)

	if leftSort.IsDirectional() && rightSort.IsDirectional() {
		if leftSort != rightSort {
			return nil
		}
		return []OrderingBinding{SortedBinding(leftSort)}
	}
	if leftSort.IsDirectional() && rightSort == ProvidedSortOrderFixed {
		return []OrderingBinding{SortedBinding(leftSort)}
	}
	if leftSort == ProvidedSortOrderFixed && rightSort.IsDirectional() {
		return []OrderingBinding{SortedBinding(rightSort)}
	}
	result := make([]OrderingBinding, 0, len(left)+len(right))
	result = append(result, left...)
	result = append(result, right...)
	return result
}

func combineBindingsForIntersection(left, right []OrderingBinding) []OrderingBinding {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	if len(right) == 0 && AreAllBindingsFixed(left) {
		return append([]OrderingBinding{}, left...)
	}
	if len(left) == 0 && AreAllBindingsFixed(right) {
		return append([]OrderingBinding{}, right...)
	}
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	leftSort := SortOrderOf(left)
	rightSort := SortOrderOf(right)

	if leftSort.IsDirectional() && rightSort.IsDirectional() {
		if leftSort != rightSort {
			return nil
		}
		return []OrderingBinding{SortedBinding(leftSort)}
	}
	if leftSort.IsDirectional() && rightSort == ProvidedSortOrderFixed {
		return append([]OrderingBinding{}, right...)
	}
	if leftSort == ProvidedSortOrderFixed && rightSort.IsDirectional() {
		return append([]OrderingBinding{}, left...)
	}
	result := make([]OrderingBinding, 0, len(left)+len(right))
	result = append(result, left...)
	result = append(result, right...)
	return result
}
