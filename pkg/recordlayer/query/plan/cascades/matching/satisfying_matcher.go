package matching

import "sync/atomic"

// SatisfyingMatcher matches an input that (a) type-asserts to T and
// (b) satisfies the supplied predicate. Mirrors Java's
// `PrimitiveMatchers.satisfies(predicate)` (and indirectly
// `equalsObject(o)` which is `satisfies(x -> x.equals(o))`).
//
// Use case: ad-hoc rule pattern constraints that don't fit the
// shape of a dedicated matcher — "match any int64 greater than 0",
// "match a *FieldValue whose Field is 'id'". Without this matcher
// rule authors would write a TypedMatcher whose downstream is
// AnyValue-equivalent + check the constraint in the rule body
// (which loses the matching-fails-pattern-fires-elsewhere
// composability).
//
// id field gives the struct non-zero size — see AnyValue in
// matcher.go for the zero-size-struct gotcha.
type SatisfyingMatcher[T any] struct {
	rootType  string
	predicate func(T) bool
	id        uint64
}

var satisfyingMatcherCounter atomic.Uint64

// NewSatisfyingMatcher constructs a SatisfyingMatcher. predicate
// MUST NOT be nil; rootType is the human-readable label returned
// from RootType (typically a description of the constraint).
func NewSatisfyingMatcher[T any](rootType string, predicate func(T) bool) *SatisfyingMatcher[T] {
	if predicate == nil {
		panic("NewSatisfyingMatcher: predicate is nil")
	}
	return &SatisfyingMatcher[T]{
		rootType:  rootType,
		predicate: predicate,
		id:        satisfyingMatcherCounter.Add(1),
	}
}

// RootType implements BindingMatcher.
func (m *SatisfyingMatcher[T]) RootType() string { return m.rootType }

// BindMatches type-asserts to T, runs the predicate, and binds the
// input on a true result. Returns nil on type-assertion failure or
// false predicate.
func (m *SatisfyingMatcher[T]) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	host, ok := in.(T)
	if !ok {
		return nil
	}
	if !m.predicate(host) {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}
