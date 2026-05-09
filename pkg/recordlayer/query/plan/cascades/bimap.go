package cascades

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// BiMap is a bidirectional map: every key maps to exactly one value,
// and every value maps to exactly one key. Mirrors Guava's
// ImmutableBiMap used in Java's GroupByMappings and related matching
// infrastructure.
//
// Unlike Go's built-in maps which use pointer identity for interface
// keys, BiMap uses configurable stringifier functions to determine
// equality — matching Java's equals()/hashCode() semantics.
type BiMap[K any, V any] struct {
	entries map[string]*biMapEntry[K, V] // keyStr → entry
	inverse map[string]string            // valStr → keyStr
	keyStr  func(K) string
	valStr  func(V) string
}

type biMapEntry[K any, V any] struct {
	key K
	val V
}

// NewBiMapWith creates an empty BiMap with the given stringifier
// functions for keys and values.
func NewBiMapWith[K any, V any](keyStr func(K) string, valStr func(V) string) *BiMap[K, V] {
	return &BiMap[K, V]{
		entries: make(map[string]*biMapEntry[K, V]),
		inverse: make(map[string]string),
		keyStr:  keyStr,
		valStr:  valStr,
	}
}

// NewValueBiMap creates a BiMap[values.Value, values.Value] using
// ExplainValue for structural equality — the standard representation
// used throughout the codebase for plan equality and hashing.
func NewValueBiMap() *BiMap[values.Value, values.Value] {
	return NewBiMapWith[values.Value, values.Value](
		func(v values.Value) string { return values.ExplainValue(v) },
		func(v values.Value) string { return values.ExplainValue(v) },
	)
}

// NewCorrValueBiMap creates a BiMap[values.CorrelationIdentifier, values.Value]
// using Name() for correlation identifiers and ExplainValue for values.
func NewCorrValueBiMap() *BiMap[values.CorrelationIdentifier, values.Value] {
	return NewBiMapWith[values.CorrelationIdentifier, values.Value](
		func(c values.CorrelationIdentifier) string { return c.Name() },
		func(v values.Value) string { return values.ExplainValue(v) },
	)
}

// NewBiMap creates an empty BiMap for comparable types using fmt.Sprintf
// as the stringifier. This preserves backward compatibility for simple
// types like string, int, etc.
func NewBiMap[K comparable, V comparable]() *BiMap[K, V] {
	return NewBiMapWith[K, V](
		func(k K) string { return fmt.Sprintf("%v", k) },
		func(v V) string { return fmt.Sprintf("%v", v) },
	)
}

// NewBiMapFromMap creates a BiMap from a regular map. Panics if the map
// contains duplicate values (violating the bijection constraint).
func NewBiMapFromMap[K comparable, V comparable](m map[K]V) *BiMap[K, V] {
	bm := NewBiMap[K, V]()
	for k, v := range m {
		ks := bm.keyStr(k)
		vs := bm.valStr(v)
		if _, exists := bm.inverse[vs]; exists {
			panic("BiMap: duplicate value — bijection violated")
		}
		bm.entries[ks] = &biMapEntry[K, V]{key: k, val: v}
		bm.inverse[vs] = ks
	}
	return bm
}

// Put inserts a key-value pair. Panics if the value already maps to a
// different key (bijection constraint).
func (b *BiMap[K, V]) Put(key K, value V) {
	ks := b.keyStr(key)
	vs := b.valStr(value)

	if existingKS, exists := b.inverse[vs]; exists {
		if existingKS != ks {
			panic("BiMap: duplicate value — bijection violated")
		}
	}
	// Remove old forward mapping if key already exists (replaces).
	if oldEntry, exists := b.entries[ks]; exists {
		oldVS := b.valStr(oldEntry.val)
		delete(b.inverse, oldVS)
	}
	b.entries[ks] = &biMapEntry[K, V]{key: key, val: value}
	b.inverse[vs] = ks
}

// Get returns the value for the given key and whether it was found.
func (b *BiMap[K, V]) Get(key K) (V, bool) {
	ks := b.keyStr(key)
	entry, ok := b.entries[ks]
	if !ok {
		var zero V
		return zero, false
	}
	return entry.val, true
}

// GetInverse returns the key for the given value and whether it was found.
func (b *BiMap[K, V]) GetInverse(value V) (K, bool) {
	vs := b.valStr(value)
	ks, ok := b.inverse[vs]
	if !ok {
		var zero K
		return zero, false
	}
	entry, ok := b.entries[ks]
	if !ok {
		var zero K
		return zero, false
	}
	return entry.key, true
}

// Len returns the number of entries.
func (b *BiMap[K, V]) Len() int {
	return len(b.entries)
}

// Range iterates over all key-value pairs. If fn returns false,
// iteration stops.
func (b *BiMap[K, V]) Range(fn func(key K, value V) bool) {
	for _, entry := range b.entries {
		if !fn(entry.key, entry.val) {
			return
		}
	}
}

// Copy returns a deep copy of the BiMap.
func (b *BiMap[K, V]) Copy() *BiMap[K, V] {
	if b == nil {
		panic("BiMap.Copy on nil receiver")
	}
	c := &BiMap[K, V]{
		entries: make(map[string]*biMapEntry[K, V], len(b.entries)),
		inverse: make(map[string]string, len(b.inverse)),
		keyStr:  b.keyStr,
		valStr:  b.valStr,
	}
	for ks, entry := range b.entries {
		c.entries[ks] = &biMapEntry[K, V]{key: entry.key, val: entry.val}
	}
	for vs, ks := range b.inverse {
		c.inverse[vs] = ks
	}
	return c
}

// PutAll copies all entries from other into this BiMap. Panics if any
// value in other already maps to a different key in this BiMap.
func (b *BiMap[K, V]) PutAll(other *BiMap[K, V]) {
	if other == nil {
		return
	}
	other.Range(func(k K, v V) bool {
		b.Put(k, v)
		return true
	})
}
