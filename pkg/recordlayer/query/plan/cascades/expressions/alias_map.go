// Portions derived from FoundationDB Record Layer (AliasMap.java),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// AliasMap is a bidirectional bijection between CorrelationIdentifiers.
//
// Used during semantic equality checks between two RelationalExpression
// trees. When checking whether `e1` ≡ `e2`, child Quantifiers may carry
// different alias names — AliasMap binds those aliases together so the
// equality check treats them as equal.
//
// Ports Java's `com.apple.foundationdb.record.query.plan.cascades.AliasMap`.
// The Java class is 750 lines; Go exposes the surface this package's
// expressions actually use:
//   - construction (Empty, Of, Builder)
//   - lookup (GetTarget, GetSource)
//   - composition (Compose) — used when descending into nested expressions
//   - emptiness check
//   - equality
//
// Bigger pieces (zip-with-alias-permutations enumerator, dependency-aware
// matching) are deferred to subsequent shifts as they're needed by rules.
type AliasMap struct {
	// view is the validated bijection itself. Extending it by one pair adds a
	// node rather than copying the pairs, which is what a backtracking match
	// does at every step.
	view values.AliasMap
}

// EmptyAliasMap returns the empty AliasMap, a shared singleton: an AliasMap
// is immutable, so every reader can share one.
func EmptyAliasMap() *AliasMap { return emptyAliasMap }

var emptyAliasMap = &AliasMap{view: values.EmptyAliasMap()}

// AliasMapOf builds an AliasMap from explicit (source, target) pairs.
// Panics if pairs has odd length, or if a source/target appears twice
// (would break the bijection invariant).
func AliasMapOf(pairs ...values.CorrelationIdentifier) *AliasMap {
	if len(pairs)%2 != 0 {
		panic("AliasMapOf requires an even number of arguments")
	}
	sources := make(map[values.CorrelationIdentifier]struct{}, len(pairs)/2)
	targets := make(map[values.CorrelationIdentifier]struct{}, len(pairs)/2)
	valuePairs := make([]values.AliasPair, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		s, t := pairs[i], pairs[i+1]
		if _, exists := sources[s]; exists {
			panic("AliasMapOf: duplicate source " + s.Name())
		}
		if _, exists := targets[t]; exists {
			panic("AliasMapOf: duplicate target " + t.Name())
		}
		sources[s], targets[t] = struct{}{}, struct{}{}
		valuePairs = append(valuePairs, values.AliasPair{Source: s, Target: t})
	}
	view, err := values.NewAliasMap(valuePairs)
	if err != nil {
		panic("AliasMapOf: " + err.Error())
	}
	return &AliasMap{view: view}
}

// IsEmpty reports whether the map has no bindings.
func (a *AliasMap) IsEmpty() bool {
	return values.AliasMapSize(a.view) == 0
}

// Size returns the number of (source, target) bindings.
func (a *AliasMap) Size() int {
	return values.AliasMapSize(a.view)
}

// GetTarget looks up the target of source. Returns the zero
// CorrelationIdentifier and ok=false if source is not bound.
func (a *AliasMap) GetTarget(source values.CorrelationIdentifier) (values.CorrelationIdentifier, bool) {
	return a.view.Target(source)
}

// GetSource looks up the source mapped to target. Returns the zero
// CorrelationIdentifier and ok=false if target is not bound.
func (a *AliasMap) GetSource(target values.CorrelationIdentifier) (values.CorrelationIdentifier, bool) {
	return a.view.Source(target)
}

// ContainsSource reports whether source has a binding.
func (a *AliasMap) ContainsSource(source values.CorrelationIdentifier) bool {
	_, ok := a.view.Target(source)
	return ok
}

// ContainsTarget reports whether target has a binding.
func (a *AliasMap) ContainsTarget(target values.CorrelationIdentifier) bool {
	_, ok := a.view.Source(target)
	return ok
}

// Compose layers another AliasMap on top of this one. Bindings in `other`
// shadow / extend bindings in `a`. Conflicting bindings (same source
// mapped to different targets) panic — callers must ensure compatibility
// by other means (typically dependency analysis).
//
// This is a MERGE, Java's combine(). It is NOT the same operation as
// cascades.AliasMap.Compose, which carries the identical method name on an
// identically-named type and performs FUNCTION COMPOSITION instead: given A→B
// here and B→C there, that one yields A→C while this one yields both bindings.
// Reaching for the wrong one compiles and does something else entirely.
//
// Java's equivalent throws if a binding would break the bijection. We
// match that strict contract; rules that need a "best-effort" merge
// should layer their own conflict policy.
func (a *AliasMap) Compose(other *AliasMap) *AliasMap {
	if other.IsEmpty() {
		return a
	}
	valuePairs := make([]values.AliasPair, 0, a.Size()+other.Size())
	values.RangeAliasPairs(a.view, func(pair values.AliasPair) bool {
		valuePairs = append(valuePairs, pair)
		return true
	})
	values.RangeAliasPairs(other.view, func(pair values.AliasPair) bool {
		if existingT, ok := a.view.Target(pair.Source); ok {
			if existingT != pair.Target {
				panic("AliasMap.Compose: conflict on source " + pair.Source.Name())
			}
			return true
		}
		if existingS, ok := a.view.Source(pair.Target); ok && existingS != pair.Source {
			panic("AliasMap.Compose: conflict on target " + pair.Target.Name())
		}
		valuePairs = append(valuePairs, pair)
		return true
	})
	view, err := values.NewAliasMap(valuePairs)
	if err != nil {
		panic("AliasMap.Compose: " + err.Error())
	}
	return &AliasMap{view: view}
}

// With returns a copy of the map with the (source, target) binding added,
// ok=true. An already-present binding is idempotent (ok=true); a source or
// target already bound to a DIFFERENT partner returns the receiver unchanged,
// ok=false (would break the bijection). Non-panicking copy-on-write analogue
// of composing one pair; memoEqual builds a node's quantifier-alias map with it
// and treats ok=false as "not equal".
func (a *AliasMap) With(source, target values.CorrelationIdentifier) (*AliasMap, bool) {
	view, compatible, err := values.ExtendAliasMap(a.view, []values.AliasPair{{Source: source, Target: target}})
	if err != nil || !compatible {
		return a, false
	}
	if view == a.view {
		return a, true
	}
	cp := *a
	cp.view = view
	return &cp, true
}

// DefinesOnlyIdentities reports whether every binding maps a source to itself
// (s↦s); an empty map qualifies. Mirrors Java AliasMap.definesOnlyIdentities —
// the fast path in correlated-to matching where no alias translation is needed.
func (a *AliasMap) DefinesOnlyIdentities() bool {
	return values.RangeAliasPairs(a.view, func(pair values.AliasPair) bool {
		return pair.Source == pair.Target
	})
}

// GetTargetOrDefault returns the target of source, or def when source is
// unbound.
func (a *AliasMap) GetTargetOrDefault(source, def values.CorrelationIdentifier) values.CorrelationIdentifier {
	if t, ok := a.view.Target(source); ok {
		return t
	}
	return def
}

// ToValuesAliasMap returns the values-package view of this bijection (the
// simple source→target map the values/predicates alias-aware equality helpers
// consume). Read-only view; callers must not mutate the result. Nil-safe: a
// nil receiver (some EqualsWithoutChildren callers pass a nil *AliasMap)
// yields a nil values.AliasMap, which the helpers read as identity-alias.
func (a *AliasMap) ToValuesAliasMap() values.AliasMap {
	if a == nil {
		return values.EmptyAliasMap()
	}
	return a.view
}

// Equals reports whether two AliasMaps have identical bindings.
func (a *AliasMap) Equals(other *AliasMap) bool {
	if a.Size() != other.Size() {
		return false
	}
	return values.RangeAliasPairs(a.view, func(pair values.AliasPair) bool {
		target, ok := other.view.Target(pair.Source)
		return ok && target == pair.Target
	})
}
