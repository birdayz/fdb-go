package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RichOrdering captures a provided ordering as a binding map
// (Value → []OrderingBinding) plus a linear sequence of ordering keys
// and a distinctness flag.
//
// This enriches the simple properties.Ordering type with the binding
// information Java's Ordering class carries. Values can be bound in
// multiple ways (e.g., both equality-bound via a scan comparison AND
// sorted via index order), and the union/intersection of orderings
// requires reasoning over binding compatibility.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.Ordering.
type RichOrdering struct {
	bindingMap map[values.Value][]OrderingBinding
	keys       []values.Value
	distinct   bool
}

// NewRichOrdering creates a new ordering from bindings, key sequence,
// and distinctness.
func NewRichOrdering(
	bindingMap map[values.Value][]OrderingBinding,
	keys []values.Value,
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
	return &RichOrdering{
		bindingMap: bm,
		keys:       keysCopy,
		distinct:   distinct,
	}
}

// EmptyOrdering returns an ordering with no keys.
func EmptyOrdering() *RichOrdering {
	return &RichOrdering{
		bindingMap: map[values.Value][]OrderingBinding{},
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
// requested ordering. A provided ordering satisfies a request when the
// provided keys form a prefix that covers all requested parts, with
// compatible sort directions.
func (o *RichOrdering) Satisfies(requested *RequestedOrdering) bool {
	if requested.IsPreserve() {
		return true
	}
	parts := requested.GetParts()
	if len(parts) == 0 {
		return true
	}

	keyIdx := 0
	for _, part := range parts {
		found := false
		for keyIdx < len(o.keys) {
			key := o.keys[keyIdx]
			bindings := o.bindingMap[key]

			if AreAllBindingsFixed(bindings) {
				keyIdx++
				continue
			}

			if !valuesEqual(key, part.Value) {
				return false
			}

			if part.SortOrder.IsDirectional() {
				sortOrder := SortOrderOf(bindings)
				if sortOrder.IsDirectional() {
					isAsc := !sortOrder.IsAnyDescending()
					wantAsc := part.SortOrder == RequestedSortOrderAscending
					if isAsc != wantAsc {
						return false
					}
				}
			}

			keyIdx++
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func valuesEqual(a, b values.Value) bool {
	if a == b {
		return true
	}
	return values.ExplainValue(a) == values.ExplainValue(b)
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

	return &RichOrdering{
		bindingMap: bm,
		keys:       keys,
		distinct:   outer.distinct || inner.distinct,
	}
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
	return &RichOrdering{
		bindingMap: bm,
		keys:       keys,
		distinct:   o.distinct,
	}
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

	return &RichOrdering{
		bindingMap: bm,
		keys:       keys,
		distinct:   a.distinct && b.distinct,
	}
}
