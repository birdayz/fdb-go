package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/combinatorics"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
	if a == b {
		return true
	}
	return values.ExplainValue(a) == values.ExplainValue(b)
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

// ConcatOrderings concatenates two orderings: the result contains
// all keys from `outer` followed by all keys from `inner` that are
// not already in `outer`. Binding maps are merged. Used by
// ImplementInJoinRule to combine the IN-source ordering with the
// inner plan's ordering.
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

	return NewRichOrdering(bm, keys, outer.distinct || inner.distinct)
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

// MergeOrderings merges two orderings for union planning. The merged
// ordering contains only keys that appear in both orderings with
// compatible sort directions. Fixed keys are retained if they appear
// in both. This is a simplified version of Java's Ordering.merge.
func MergeOrderings(a, b *RichOrdering) *RichOrdering {
	bm := make(map[values.Value][]OrderingBinding)
	var keys []values.Value

	aKeySet := make(map[string]values.Value, len(a.keys))
	for _, k := range a.keys {
		aKeySet[values.ExplainValue(k)] = k
	}

	for _, bKey := range b.keys {
		explain := values.ExplainValue(bKey)
		aKey, inA := aKeySet[explain]
		if !inA {
			continue
		}

		aBindings := a.bindingMap[aKey]
		bBindings := b.bindingMap[bKey]

		aSorted := SortOrderOf(aBindings)
		bSorted := SortOrderOf(bBindings)

		if aSorted.IsDirectional() && bSorted.IsDirectional() && aSorted == bSorted {
			bm[aKey] = []OrderingBinding{SortedBinding(aSorted)}
			keys = append(keys, aKey)
		} else if AreAllBindingsFixed(aBindings) && AreAllBindingsFixed(bBindings) {
			bm[aKey] = []OrderingBinding{FixedBinding(nil)}
			keys = append(keys, aKey)
		}
	}

	return NewRichOrdering(bm, keys, a.distinct && b.distinct)
}
