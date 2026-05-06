package matching

// SomeElementsMatcher matches an []any input where AT LEAST ONE
// element binds the supplied downstream. The matcher's bound value
// is the input slice itself; per-element downstream bindings
// accumulate through outer (AllOfMatcher convention) so rule bodies
// retrieving the downstream's binding via Get / GetAll see every
// matching element's contribution.
//
// Ports Java's `com.apple.foundationdb.record.query.plan.cascades.
// matching.structure.MultiMatcher.SomeMatcher`. Java's static
// factory is `some(downstream)`. Name disambiguated as
// `SomeElementsMatcher` so it pairs with the existing
// `AllElementsMatcher` rather than colliding with the seed's `AnyOf`
// combinator (which picks one of N alternatives, not "some elements
// of a slice").
//
// Empty input does NOT match — there's no element to bind, so
// "at least one matches" is false. Mirrors Java's SomeMatcher
// (`stream.anyMatch(...)` semantics) and distinguishes this matcher
// from AllElementsMatcher (which DOES vacuously match the empty
// case).
//
// Cartesian-product across multiple matching elements: if k
// elements each produce m_i bindings, the matcher yields
// sum(m_i) bindings (one per per-element match), each carrying the
// input slice as the matcher's bound value.
type SomeElementsMatcher struct {
	downstream BindingMatcher
}

func (*SomeElementsMatcher) isCollectionMatcher() {}

// NewSomeElementsMatcher constructs a SomeElementsMatcher with the
// given downstream applied per-element.
func NewSomeElementsMatcher(downstream BindingMatcher) *SomeElementsMatcher {
	return &SomeElementsMatcher{downstream: downstream}
}

// RootType implements BindingMatcher.
func (*SomeElementsMatcher) RootType() string { return "SomeElements" }

// BindMatches walks the input slice and accumulates per-element
// downstream matches against outer. Returns nil on non-slice input
// or when no element matches.
func (m *SomeElementsMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	items, ok := in.([]any)
	if !ok {
		return nil
	}
	if len(items) == 0 {
		// Empty input — "at least one" is vacuously false. Differs
		// from AllElementsMatcher's vacuous-true semantics.
		return nil
	}
	out := make([]*PlannerBindings, 0)
	for _, item := range items {
		matches := m.downstream.BindMatches(outer, item)
		out = append(out, matches...)
	}
	if len(out) == 0 {
		return nil
	}
	for i, b := range out {
		out[i] = b.Bind(m, in)
	}
	return out
}
