package matching

import "sync/atomic"

// NegationMatcher inverts a downstream matcher: succeeds (with a
// single empty binding) when the downstream FAILS to match, fails
// when it succeeds. Mirrors Java's `NotMatcher.not(downstream)`.
//
// Naming note: avoided "NotMatcher" to disambiguate from the
// pre-existing `predicateMatcher[*predicates.NotPredicate]`
// (constructed via `newNotPredicateMatcher()`) — that one matches
// the input WHEN it's a NotPredicate; this one is a negating
// combinator that fires the OPPOSITE way of its downstream.
//
// Use case: rule patterns that assert absence — "this AND chain
// has NO ConstantPredicate child", "this expression is NOT a
// FieldValue". Pairs with the various "is" matchers (Instance,
// AnyValue) for predicate negation in pattern shapes.
//
// id field gives the struct non-zero size so two NewNegationMatcher
// calls bind to distinct map-key identities (zero-size struct
// gotcha — see AnyValue in matcher.go).
type NegationMatcher struct {
	downstream BindingMatcher
	id         uint64
}

var negationMatcherCounter atomic.Uint64

// NewNegationMatcher constructs a NegationMatcher. downstream MUST
// NOT be nil — a nil downstream would always "fail" trivially,
// which is a degenerate matcher (matches everything). Rule
// authors who want "always match" should use a real Any-equivalent
// matcher.
func NewNegationMatcher(downstream BindingMatcher) *NegationMatcher {
	if downstream == nil {
		panic("NewNegationMatcher: downstream is nil")
	}
	return &NegationMatcher{
		downstream: downstream,
		id:         negationMatcherCounter.Add(1),
	}
}

// RootType implements BindingMatcher.
func (*NegationMatcher) RootType() string { return "Negation" }

// BindMatches inverts the downstream's verdict: any non-empty
// downstream match → nil; empty downstream → a single binding
// with this matcher bound to in. Carries no downstream-derived
// bindings forward — by definition there's nothing to extract from
// the downstream's failure.
func (m *NegationMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	matches := m.downstream.BindMatches(outer, in)
	if len(matches) > 0 {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}
