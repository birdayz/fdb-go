package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
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
	return NewRichOrderingWithDeps(bindingMap, keys, combinatorics.NewSetMultimap[string](), distinct)
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
	if deps.Size() > 0 {
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
	if requested.IsPreserve() {
		return true
	}
	parts := requested.GetParts()
	if len(parts) == 0 {
		return true
	}

	for _, part := range parts {
		bindings := o.bindingMapForExplain(values.ExplainValue(part.Value))
		if bindings == nil {
			return false
		}
		sortOrder := SortOrderOf(bindings)
		if !sortOrder.IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return false
		}
	}

	requestedValues := make(map[string]struct{}, len(parts))
	requestedKeys := make([]string, len(parts))
	for i, part := range parts {
		k := values.ExplainValue(part.Value)
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

// IsCompatibleWithRequestedSortOrder checks if a provided sort order
// is compatible with a requested sort order.
func (p ProvidedSortOrder) IsCompatibleWithRequestedSortOrder(req RequestedSortOrder) bool {
	if req == RequestedSortOrderAny {
		return true
	}
	if p == ProvidedSortOrderFixed {
		return true
	}
	if req.IsDirectional() && p.IsDirectional() {
		reqAsc := req == RequestedSortOrderAscending
		provAsc := !p.IsAnyDescending()
		return reqAsc == provAsc
	}
	return true
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
		bindings := o.bindingMapForExplain(values.ExplainValue(part.Value))
		if bindings == nil {
			return nil
		}
		sortOrder := SortOrderOf(bindings)
		if !sortOrder.IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return nil
		}
		requestedKeys[i] = values.ExplainValue(part.Value)
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
	for _, part := range parts {
		bindings := o.bindingMapForExplain(values.ExplainValue(part.Value))
		if bindings == nil {
			return nil
		}
		if !SortOrderOf(bindings).IsCompatibleWithRequestedSortOrder(part.SortOrder) {
			return nil
		}
	}

	requestedKeys := make([]string, len(parts))
	for i, part := range parts {
		requestedKeys[i] = values.ExplainValue(part.Value)
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
	reqMap := requested.GetValueRequestedSortOrderMap()
	parts := make([]ProvidedOrderingPart, 0, len(keyValues))
	for _, key := range keyValues {
		bindings := o.bindingMap[key]
		sortOrder := SortOrderOf(bindings)

		if !sortOrder.IsDirectional() {
			if reqSort, ok := reqMap[key]; ok && reqSort.IsDirectional() {
				if reqSort == RequestedSortOrderAscending {
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

	return NewRichOrderingWithDeps(bm, keys, deps, outer.distinct || inner.distinct)
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
	newBM := make(map[values.Value][]OrderingBinding, len(o.bindingMap))
	var newKeys []values.Value
	for _, key := range o.keys {
		keyStr := values.ExplainValue(key)
		if mapped, ok := mapping[keyStr]; ok {
			newBM[mapped] = o.bindingMap[key]
			newKeys = append(newKeys, mapped)
		}
	}
	if len(newKeys) == 0 {
		return NewRichOrdering(nil, nil, o.distinct)
	}
	return NewRichOrdering(newBM, newKeys, o.distinct)
}

// PushDown is the inverse of PullUp — translates ordering keys
// back through a reverse mapping.
func (o *RichOrdering) PushDown(mapping map[string]values.Value) *RichOrdering {
	return o.PullUp(mapping)
}

// PullUpThroughValue translates this ordering through a result value.
// For each ordering key, uses Value.PullUpValue to compute the
// pulled-up key. Bindings are preserved. Keys that cannot be pulled
// up are dropped.
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

	newBM := make(map[values.Value][]OrderingBinding, len(pulledUpMap))
	var newKeys []values.Value
	for _, key := range o.keys {
		if pulledUp, ok := pulledUpMap[key]; ok {
			newBM[pulledUp] = o.bindingMap[key]
			newKeys = append(newKeys, pulledUp)
		}
	}
	if len(newKeys) == 0 {
		return NewRichOrdering(nil, nil, o.distinct)
	}
	return NewRichOrdering(newBM, newKeys, o.distinct)
}

// PushDownThroughValue translates this ordering's keys from output
// space back to input space through a result value. For each ordering
// key, uses Value.PushDownValue to compute the pushed-down key.
// Bindings are preserved. Keys that cannot be pushed down are dropped.
//
// Ports Java's Ordering.pushDown(Value, EvaluationContext, AliasMap,
// Set<CorrelationIdentifier>).
func (o *RichOrdering) PushDownThroughValue(resultValue values.Value, upperAlias values.CorrelationIdentifier) *RichOrdering {
	if o == nil {
		return nil
	}

	pushed := values.PushDownValues(o.keys, resultValue, upperAlias)

	newBM := make(map[values.Value][]OrderingBinding, len(o.bindingMap))
	var newKeys []values.Value
	for i, key := range o.keys {
		if pushed[i] != nil {
			newBM[pushed[i]] = o.bindingMap[key]
			newKeys = append(newKeys, pushed[i])
		}
	}
	if len(newKeys) == 0 {
		return NewRichOrdering(nil, nil, o.distinct)
	}
	return NewRichOrdering(newBM, newKeys, o.distinct)
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
	return mergeOrderings(a, b, combineBindingsForUnion, a.distinct && b.distinct)
}

// MergeOrderingsForIntersection merges two orderings with intersection semantics.
func MergeOrderingsForIntersection(a, b *RichOrdering) *RichOrdering {
	return mergeOrderings(a, b, combineBindingsForIntersection, a.distinct && b.distinct)
}

// MergeOrderings merges two orderings (union semantics, backward-compat alias).
func MergeOrderings(a, b *RichOrdering) *RichOrdering {
	return MergeOrderingsForUnion(a, b)
}

type bindingCombiner func(left, right []OrderingBinding) []OrderingBinding

func mergeOrderings(a, b *RichOrdering, combine bindingCombiner, distinct bool) *RichOrdering {
	leftES := a.orderingSet.EligibleSet()
	rightES := b.orderingSet.EligibleSet()

	var elements []string
	deps := combinatorics.NewSetMultimap[string]()
	bm := make(map[values.Value][]OrderingBinding)
	lookup := make(map[string]values.Value)

	var lastElements []string

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
						elements = append(elements, le)
						if lv != nil {
							lookup[le] = lv
							bm[lv] = combined
						} else if rv != nil {
							lookup[le] = rv
							bm[rv] = combined
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
					elements = append(elements, le)
					lookup[le] = lv
					bm[lv] = combined
				}
			}
		}
		for re := range rightElems {
			if _, inLeft := leftElems[re]; !inLeft {
				rv := b.keyLookup[re]
				combined := combine(nil, b.bindingMap[rv])
				if len(combined) > 0 {
					elements = append(elements, re)
					lookup[re] = rv
					bm[rv] = combined
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

	return NewRichOrderingWithDeps(bm, vals, deps, distinct)
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
