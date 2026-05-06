package matching

import (
	"reflect"
	"sync/atomic"
)

// OptionalIfPresentMatcher matches an input when it is "present"
// (non-nil) AND the downstream matcher matches the value. Absent
// inputs (nil interface or typed-nil pointer) yield no match —
// the "if present" guard.
//
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.matching.
// structure.OptionalIfPresentMatcher<T extends Object>` with Go's
// idiomatic absence representation:
//
//   - `nil` interface → absent.
//   - Non-nil interface holding a non-nil pointer → present, value
//     forwarded as-is to the downstream.
//   - Non-pointer value → always present.
//
// Use case: a rule pattern that wants to assert "if this Optional
// child slot is filled, the value matches X" without forcing the
// outer expression to have it filled. Frequently used inside graph/
// matcher patterns where a property is sometimes computed and
// sometimes absent.
//
// id gives the struct non-zero size so two NewOptionalIfPresentMatcher
// instances bind to distinct map-key identities — same pattern as
// AnyValue / EmptyCollectionMatcher.
type OptionalIfPresentMatcher struct {
	id         uint64
	downstream BindingMatcher
}

// optionalIfPresentCounter is the per-process atomic id source.
var optionalIfPresentCounter atomic.Uint64

// NewOptionalIfPresentMatcher constructs an OptionalIfPresentMatcher
// over the given downstream. nil downstream panics — a present-guard
// without something to assert about the value is meaningless.
func NewOptionalIfPresentMatcher(downstream BindingMatcher) *OptionalIfPresentMatcher {
	if downstream == nil {
		panic("NewOptionalIfPresentMatcher: downstream matcher cannot be nil")
	}
	return &OptionalIfPresentMatcher{
		id:         optionalIfPresentCounter.Add(1),
		downstream: downstream,
	}
}

// RootType implements BindingMatcher.
func (*OptionalIfPresentMatcher) RootType() string { return "OptionalIfPresent" }

// BindMatches returns the downstream's bindings when in is present,
// nil when absent. Present means: not the bare nil interface, and
// not a typed-nil pointer.
func (m *OptionalIfPresentMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	if in == nil {
		return nil
	}
	rv := reflect.ValueOf(in)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
	}
	matches := m.downstream.BindMatches(outer, in)
	if len(matches) == 0 {
		return nil
	}
	// Bind the matcher itself to the input so rule bodies can recover
	// the present value via PlannerBindings.Get(matcher).
	out := make([]*PlannerBindings, len(matches))
	for i, b := range matches {
		out[i] = b.Bind(m, in)
	}
	return out
}
