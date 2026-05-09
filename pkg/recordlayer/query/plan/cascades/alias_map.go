package cascades

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

// AliasMap is an immutable bidirectional mapping between
// CorrelationIdentifiers. Used during graph traversal to track
// which alias on the "left" side corresponds to which alias on the
// "right" side when comparing or rebasing expressions.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.AliasMap.
//
// Key use cases:
//   - Subquery decorrelation: rebasing correlated references when
//     pulling a subquery out of its enclosing context.
//   - Expression equivalence: determining if two sub-graphs are
//     semantically equal under an alias mapping.
//   - Push/pull rules: translating alias references when moving
//     operators across quantifier boundaries.
type AliasMap struct {
	forward  map[values.CorrelationIdentifier]values.CorrelationIdentifier
	inverse  map[values.CorrelationIdentifier]values.CorrelationIdentifier
	identity bool
}

var emptyAliasMap = &AliasMap{
	forward:  map[values.CorrelationIdentifier]values.CorrelationIdentifier{},
	inverse:  map[values.CorrelationIdentifier]values.CorrelationIdentifier{},
	identity: true,
}

// EmptyAliasMap returns the shared empty alias map.
func EmptyAliasMap() *AliasMap { return emptyAliasMap }

// AliasMapOfAliases creates a single-entry alias map.
func AliasMapOfAliases(source, target values.CorrelationIdentifier) *AliasMap {
	return &AliasMap{
		forward:  map[values.CorrelationIdentifier]values.CorrelationIdentifier{source: target},
		inverse:  map[values.CorrelationIdentifier]values.CorrelationIdentifier{target: source},
		identity: source == target,
	}
}

// AliasMapBuilder constructs an AliasMap incrementally.
type AliasMapBuilder struct {
	forward map[values.CorrelationIdentifier]values.CorrelationIdentifier
	inverse map[values.CorrelationIdentifier]values.CorrelationIdentifier
}

// NewAliasMapBuilder creates an empty builder.
func NewAliasMapBuilder() *AliasMapBuilder {
	return &AliasMapBuilder{
		forward: make(map[values.CorrelationIdentifier]values.CorrelationIdentifier),
		inverse: make(map[values.CorrelationIdentifier]values.CorrelationIdentifier),
	}
}

// Put adds a source→target mapping. Returns false if either source
// or target is already mapped (would create an inconsistent bi-map).
func (b *AliasMapBuilder) Put(source, target values.CorrelationIdentifier) bool {
	if _, exists := b.forward[source]; exists {
		return false
	}
	if _, exists := b.inverse[target]; exists {
		return false
	}
	b.forward[source] = target
	b.inverse[target] = source
	return true
}

// PutAll copies all entries from other into this builder. Each
// entry is added via Put; conflicts (source or target already
// mapped) are silently skipped to match Java's Builder.putAll
// semantics.
//
// Ports Java's AliasMap.Builder.putAll.
func (b *AliasMapBuilder) PutAll(other *AliasMap) {
	for source, target := range other.forward {
		b.Put(source, target)
	}
}

// Build creates an immutable AliasMap from the builder's state.
func (b *AliasMapBuilder) Build() *AliasMap {
	fwd := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier, len(b.forward))
	inv := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier, len(b.inverse))
	identity := true
	for k, v := range b.forward {
		fwd[k] = v
		if k != v {
			identity = false
		}
	}
	for k, v := range b.inverse {
		inv[k] = v
	}
	return &AliasMap{forward: fwd, inverse: inv, identity: identity}
}

// ContainsSource reports whether source is mapped.
func (m *AliasMap) ContainsSource(source values.CorrelationIdentifier) bool {
	_, ok := m.forward[source]
	return ok
}

// ContainsTarget reports whether target is mapped.
func (m *AliasMap) ContainsTarget(target values.CorrelationIdentifier) bool {
	_, ok := m.inverse[target]
	return ok
}

// GetTarget returns the target for the given source, or the source
// itself if not mapped (identity fallback).
func (m *AliasMap) GetTarget(source values.CorrelationIdentifier) values.CorrelationIdentifier {
	if target, ok := m.forward[source]; ok {
		return target
	}
	return source
}

// GetTargetOrEmpty returns the target for the given source, or empty
// if not mapped. Unlike GetTarget, does NOT fall back to identity.
func (m *AliasMap) GetTargetOrEmpty(source values.CorrelationIdentifier) (values.CorrelationIdentifier, bool) {
	target, ok := m.forward[source]
	return target, ok
}

// GetSource returns the source for the given target, or the target
// itself if not mapped (identity fallback).
func (m *AliasMap) GetSource(target values.CorrelationIdentifier) values.CorrelationIdentifier {
	if source, ok := m.inverse[target]; ok {
		return source
	}
	return target
}

// IsIdentity reports whether all mappings are identity (source==target)
// or the map is empty.
func (m *AliasMap) IsIdentity() bool { return m.identity }

// IsEmpty reports whether the map contains no mappings.
func (m *AliasMap) IsEmpty() bool { return len(m.forward) == 0 }

// Size returns the number of mappings.
func (m *AliasMap) Size() int { return len(m.forward) }

// ForwardMap returns the forward (source→target) map suitable for
// passing to values.RebaseValue. This is the bridge between the
// cascades-level AliasMap and the values-level rebase infrastructure.
func (m *AliasMap) ForwardMap() values.AliasMap {
	return values.AliasMap(m.forward)
}

// Sources returns all source aliases.
func (m *AliasMap) Sources() []values.CorrelationIdentifier {
	result := make([]values.CorrelationIdentifier, 0, len(m.forward))
	for k := range m.forward {
		result = append(result, k)
	}
	return result
}

// Derived creates a new AliasMap that extends this one with
// additional mappings. Existing mappings are preserved; new mappings
// that conflict with existing ones are skipped.
func (m *AliasMap) Derived(additions *AliasMap) *AliasMap {
	b := NewAliasMapBuilder()
	for k, v := range m.forward {
		b.Put(k, v)
	}
	for k, v := range additions.forward {
		b.Put(k, v)
	}
	return b.Build()
}

// Compose creates a new AliasMap where each source maps to the
// target of the target in `other`. If this maps A→B and other maps
// B→C, the result maps A→C.
func (m *AliasMap) Compose(other *AliasMap) *AliasMap {
	b := NewAliasMapBuilder()
	for source, intermediate := range m.forward {
		target := other.GetTarget(intermediate)
		b.Put(source, target)
	}
	return b.Build()
}
