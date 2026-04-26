package matching

import "sync/atomic"

// AllElementsMatcher matches an []any input where EVERY element binds
// the same downstream matcher. The downstream is invoked once per
// element with the running partial bindings (AllOfMatcher convention),
// and a single failing element collapses the whole match to nil.
//
// Ports Java's `com.apple.foundationdb.record.query.plan.cascades.
// matching.structure.MultiMatcher.AllMatcher` (the static factory
// is called `all(downstream)` in Java). Name disambiguated as
// `AllElementsMatcher` so it isn't confused with the seed's existing
// `AllOfMatcher` (the AND combinator over disjoint downstreams; this
// matcher applies one downstream to every collection element).
//
// Empty input matches successfully — the collection's "every element"
// statement is vacuously true. Java's AllMatcher has the same
// semantics.
//
// Cartesian-product semantics across elements: if a downstream
// produces multiple bindings for one element, every accumulated
// partial multiplies through it. Useful for matching a
// ScalarFunctionValue.Args slice ("every arg matches the constant
// matcher") + extending later to richer per-element matches.
//
// Difference from ListMatcher: ListMatcher pairs each position with
// its own downstream and is length-strict. AllElementsMatcher uses
// a single downstream against every element regardless of count.
// The downstream pointer gives the struct non-zero size so two
// `new(AllElementsMatcher)` calls receive distinct map-key identities
// (no nonce field needed here — see AnyValue in matcher.go for the
// zero-size-struct gotcha).
// CollectionMatcher is the interface every collection-shaped matcher
// implements — AllElementsMatcher / SomeElementsMatcher /
// AtLeastElementsMatcher / ListMatcher. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.matching.structure.
// CollectionMatcher`. Used by rule patterns that want to constrain
// "this matcher must be one that matches a slice", as opposed to a
// scalar Value or a Predicate.
//
// Rule authors typically use the concrete factories
// (NewAllElementsMatcher, etc.) directly; the interface exists so
// future combinators that compose collection matchers (e.g.
// `combine(allElements, atLeastTwo)`) can take it as a parameter.
type CollectionMatcher interface {
	BindingMatcher
	// isCollectionMatcher is a marker method — its only purpose is
	// to confine the interface to the four concrete impls in this
	// package. A bare `BindingMatcher` won't satisfy it.
	isCollectionMatcher()
}

type AllElementsMatcher struct {
	downstream BindingMatcher
}

func (*AllElementsMatcher) isCollectionMatcher() {}

// EmptyCollectionMatcher matches an []any input ONLY when it's
// empty (length 0). Returns nil for non-slice input or any non-
// empty slice. Mirrors Java's `CollectionMatcher.empty()` factory.
//
// Useful for rule patterns that want to ASSERT a list is empty —
// e.g. an OrPredicate with no children (degenerate input the
// simplifier should have culled), or a record constructor with no
// fields (the unit type).
//
// id gives the struct non-zero size so two NewEmptyCollectionMatcher
// instances bind to distinct map-key identities — see the zero-size
// struct gotcha at AnyValue in matcher.go.
type EmptyCollectionMatcher struct {
	id uint64
}

func (*EmptyCollectionMatcher) isCollectionMatcher() {}

// emptyCollectionCounter is the per-process atomic counter that
// gives each NewEmptyCollectionMatcher instance a distinct id. Same
// pattern as AnyValue's anyValueCounter.
var emptyCollectionCounter atomic.Uint64

// NewEmptyCollectionMatcher constructs a fresh EmptyCollectionMatcher
// with a unique id so PlannerBindings map-key collisions can't
// happen. Rule authors MUST use this factory rather than bare
// struct literals.
func NewEmptyCollectionMatcher() *EmptyCollectionMatcher {
	return &EmptyCollectionMatcher{id: emptyCollectionCounter.Add(1)}
}

// RootType implements BindingMatcher.
func (*EmptyCollectionMatcher) RootType() string { return "EmptyCollection" }

// BindMatches succeeds with a single binding when in is an empty
// []any; nil otherwise.
func (m *EmptyCollectionMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	items, ok := in.([]any)
	if !ok || len(items) > 0 {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}

// NewAllElementsMatcher constructs an AllElementsMatcher with the
// given downstream applied to every input element.
func NewAllElementsMatcher(downstream BindingMatcher) *AllElementsMatcher {
	return &AllElementsMatcher{
		downstream: downstream,
	}
}

func (*AllElementsMatcher) RootType() string { return "AllElements" }

// BindMatches threads outer through each element's downstream call
// (AllOfMatcher convention) and returns the accumulated bindings.
// Empty []any → return outer.Bind(self, []any{}) (empty input matches
// vacuously, which mirrors Java's AllMatcher).
func (m *AllElementsMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	items, ok := in.([]any)
	if !ok {
		return nil
	}
	current := []*PlannerBindings{outer}
	for _, item := range items {
		next := make([]*PlannerBindings, 0, len(current))
		for _, partial := range current {
			matches := m.downstream.BindMatches(partial, item)
			next = append(next, matches...)
		}
		if len(next) == 0 {
			return nil
		}
		current = next
	}
	for i, b := range current {
		current[i] = b.Bind(m, in)
	}
	return current
}
