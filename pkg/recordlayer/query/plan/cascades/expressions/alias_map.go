package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// AliasMap is a bidirectional bijection between CorrelationIdentifiers.
//
// Used during semantic equality checks between two RelationalExpression
// trees. When checking whether `e1` ≡ `e2`, child Quantifiers may carry
// different alias names — AliasMap binds those aliases together so the
// equality check treats them as equal.
//
// Ports Java's `com.apple.foundationdb.record.query.plan.cascades.AliasMap`.
// The Java class is 750 lines; this seed exposes the surface used by the
// seed expressions:
//   - construction (Empty, Of, Builder)
//   - lookup (GetTarget, GetSource)
//   - composition (Compose) — used when descending into nested expressions
//   - emptiness check
//   - equality
//
// Bigger pieces (zip-with-alias-permutations enumerator, dependency-aware
// matching) are deferred to subsequent shifts as they're needed by rules.
type AliasMap struct {
	// Forward and reverse maps of the bijection. Both kept in sync;
	// every (s, t) pair has Forward[s] == t and Reverse[t] == s.
	forward map[values.CorrelationIdentifier]values.CorrelationIdentifier
	reverse map[values.CorrelationIdentifier]values.CorrelationIdentifier
}

// EmptyAliasMap returns the unique empty AliasMap singleton-equivalent.
// Equal AliasMaps are considered equal regardless of identity, so a fresh
// empty map is fine.
func EmptyAliasMap() *AliasMap {
	return &AliasMap{
		forward: map[values.CorrelationIdentifier]values.CorrelationIdentifier{},
		reverse: map[values.CorrelationIdentifier]values.CorrelationIdentifier{},
	}
}

// AliasMapOf builds an AliasMap from explicit (source, target) pairs.
// Panics if pairs has odd length, or if a source/target appears twice
// (would break the bijection invariant).
func AliasMapOf(pairs ...values.CorrelationIdentifier) *AliasMap {
	if len(pairs)%2 != 0 {
		panic("AliasMapOf requires an even number of arguments")
	}
	m := EmptyAliasMap()
	for i := 0; i < len(pairs); i += 2 {
		s, t := pairs[i], pairs[i+1]
		if _, exists := m.forward[s]; exists {
			panic("AliasMapOf: duplicate source " + s.Name())
		}
		if _, exists := m.reverse[t]; exists {
			panic("AliasMapOf: duplicate target " + t.Name())
		}
		m.forward[s] = t
		m.reverse[t] = s
	}
	return m
}

// IsEmpty reports whether the map has no bindings.
func (a *AliasMap) IsEmpty() bool {
	return len(a.forward) == 0
}

// Size returns the number of (source, target) bindings.
func (a *AliasMap) Size() int {
	return len(a.forward)
}

// GetTarget looks up the target of source. Returns the zero
// CorrelationIdentifier and ok=false if source is not bound.
func (a *AliasMap) GetTarget(source values.CorrelationIdentifier) (values.CorrelationIdentifier, bool) {
	t, ok := a.forward[source]
	return t, ok
}

// GetSource looks up the source mapped to target. Returns the zero
// CorrelationIdentifier and ok=false if target is not bound.
func (a *AliasMap) GetSource(target values.CorrelationIdentifier) (values.CorrelationIdentifier, bool) {
	s, ok := a.reverse[target]
	return s, ok
}

// ContainsSource reports whether source has a binding.
func (a *AliasMap) ContainsSource(source values.CorrelationIdentifier) bool {
	_, ok := a.forward[source]
	return ok
}

// ContainsTarget reports whether target has a binding.
func (a *AliasMap) ContainsTarget(target values.CorrelationIdentifier) bool {
	_, ok := a.reverse[target]
	return ok
}

// Compose layers another AliasMap on top of this one. Bindings in `other`
// shadow / extend bindings in `a`. Conflicting bindings (same source
// mapped to different targets) panic — callers must ensure compatibility
// by other means (typically dependency analysis).
//
// Java's equivalent throws if a binding would break the bijection. We
// match that strict contract; rules that need a "best-effort" merge
// should layer their own conflict policy.
func (a *AliasMap) Compose(other *AliasMap) *AliasMap {
	if other.IsEmpty() {
		return a
	}
	out := EmptyAliasMap()
	for s, t := range a.forward {
		out.forward[s] = t
		out.reverse[t] = s
	}
	for s, t := range other.forward {
		if existingT, ok := out.forward[s]; ok && existingT != t {
			panic("AliasMap.Compose: conflict on source " + s.Name())
		}
		if existingS, ok := out.reverse[t]; ok && existingS != s {
			panic("AliasMap.Compose: conflict on target " + t.Name())
		}
		out.forward[s] = t
		out.reverse[t] = s
	}
	return out
}

// ToValuesAliasMap returns the forward bindings as a values.AliasMap (the
// simple source→target map the values/predicates alias-aware equality helpers
// consume). Read-only view; callers must not mutate the result. Nil-safe: a
// nil receiver (some EqualsWithoutChildren callers pass a nil *AliasMap)
// yields a nil values.AliasMap, which the helpers read as identity-alias.
// With returns a copy of the map with the (source, target) binding added,
// ok=true. An already-present binding is idempotent (ok=true); a source or
// target already bound to a DIFFERENT partner returns the receiver unchanged,
// ok=false (would break the bijection). Non-panicking copy-on-write analogue
// of composing one pair; memoEqual builds a node's quantifier-alias map with it
// and treats ok=false as "not equal".
func (a *AliasMap) With(source, target values.CorrelationIdentifier) (*AliasMap, bool) {
	if existingT, ok := a.forward[source]; ok {
		if existingT != target {
			return a, false
		}
		if existingS, ok2 := a.reverse[target]; ok2 && existingS != source {
			return a, false
		}
		return a, true
	}
	if existingS, ok := a.reverse[target]; ok && existingS != source {
		return a, false
	}
	out := EmptyAliasMap()
	for s, t := range a.forward {
		out.forward[s] = t
		out.reverse[t] = s
	}
	out.forward[source] = target
	out.reverse[target] = source
	return out, true
}

// DefinesOnlyIdentities reports whether every binding maps a source to itself
// (s↦s); an empty map qualifies. Mirrors Java AliasMap.definesOnlyIdentities —
// the fast path in correlated-to matching where no alias translation is needed.
func (a *AliasMap) DefinesOnlyIdentities() bool {
	for s, t := range a.forward {
		if s != t {
			return false
		}
	}
	return true
}

// GetTargetOrDefault returns the target bound to source, or def if unbound.
func (a *AliasMap) GetTargetOrDefault(source, def values.CorrelationIdentifier) values.CorrelationIdentifier {
	if t, ok := a.forward[source]; ok {
		return t
	}
	return def
}

func (a *AliasMap) ToValuesAliasMap() values.AliasMap {
	if a == nil {
		return nil
	}
	return values.AliasMap(a.forward)
}

// Equals reports whether two AliasMaps have identical bindings.
func (a *AliasMap) Equals(other *AliasMap) bool {
	if a.Size() != other.Size() {
		return false
	}
	for s, t := range a.forward {
		if otherT, ok := other.forward[s]; !ok || otherT != t {
			return false
		}
	}
	return true
}
