package matching

import "sync/atomic"

// TypedMatcher is the generic matcher that type-asserts input to T,
// applies an extractor T → U, then runs a downstream matcher on U.
// Mirrors Java's `TypedMatcherWithExtractAndDownstream<T>`.
//
// Use case: rule patterns of the shape "match a host of type T, drill
// into one of its fields, then match that field with the downstream".
// Without this combinator each rule has to either write a custom
// matcher struct OR chain matchers via AllOf with nested type
// assertions in rule bodies — both noisier than the one-liner this
// enables.
//
// Example — match an ArithmeticValue and run downstream on its Left:
//
//	NewTypedMatcher[*ArithmeticValue, Value](
//	    "ArithLeft",
//	    func(av *ArithmeticValue) Value { return av.Left },
//	    constantMatcher,
//	)
//
// id gives the struct non-zero size so two NewTypedMatcher instances
// bind distinctly in PlannerBindings (mirrors the AnyValue + Empty
// CollectionMatcher pattern; same zero-size-struct gotcha
// documented at AnyValue in matcher.go).
type TypedMatcher[T any, U any] struct {
	rootType   string
	extract    func(T) U
	downstream BindingMatcher
	id         uint64
}

// typedMatcherCounter is the per-process atomic id counter so each
// NewTypedMatcher call produces a distinct identity.
var typedMatcherCounter atomic.Uint64

// NewTypedMatcher constructs a TypedMatcher. rootType is the
// human-readable label returned from RootType — typically the
// downstream's pattern shape (e.g. "ArithLeft" for "drills into
// ArithmeticValue.Left"). extract MUST NOT be nil; downstream MUST
// NOT be nil. Both are dereferenced on every BindMatches.
func NewTypedMatcher[T any, U any](rootType string, extract func(T) U, downstream BindingMatcher) *TypedMatcher[T, U] {
	if extract == nil {
		panic("NewTypedMatcher: extract is nil")
	}
	if downstream == nil {
		panic("NewTypedMatcher: downstream is nil")
	}
	return &TypedMatcher[T, U]{
		rootType:   rootType,
		extract:    extract,
		downstream: downstream,
		id:         typedMatcherCounter.Add(1),
	}
}

// RootType implements BindingMatcher.
func (m *TypedMatcher[T, U]) RootType() string { return m.rootType }

// BindMatches type-asserts in to T, runs the extractor, dispatches
// downstream on the extracted value, and binds the original input
// to this matcher on success. Returns nil on T-mismatch or empty
// downstream.
func (m *TypedMatcher[T, U]) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	host, ok := in.(T)
	if !ok {
		return nil
	}
	extracted := m.extract(host)
	matches := m.downstream.BindMatches(outer, extracted)
	if len(matches) == 0 {
		return nil
	}
	for i, b := range matches {
		matches[i] = b.Bind(m, in)
	}
	return matches
}
