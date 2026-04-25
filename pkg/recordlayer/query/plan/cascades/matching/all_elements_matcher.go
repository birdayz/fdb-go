package matching

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
type AllElementsMatcher struct {
	downstream BindingMatcher
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
