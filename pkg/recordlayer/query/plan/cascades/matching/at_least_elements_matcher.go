package matching

import "fmt"

// AtLeastElementsMatcher matches an []any input when at least
// `minMatches` elements bind the supplied downstream. Mirrors Java's
// `MultiMatcher.AtLeastMatcher` (factory `atLeastOne` /
// `atLeastTwo`). The matcher's bound value is the input slice
// itself; per-element downstream bindings accumulate so rule
// bodies can retrieve them via Get / GetAll.
//
// minMatches == 0 → always matches (the threshold is trivially
// met). Empty input still binds in that case (vacuously) — the
// matcher returns a single binding to the matcher itself with no
// downstream contributions. minMatches > 0 + fewer than that many
// element-matches → returns nil.
//
// Use cases mirroring Java: requiring AT LEAST 2 ConstantPredicate
// children inside an AND/OR chain (`atLeastTwo(constantPredicate())`),
// or a one-shot "is there any match at all" (atLeastOne, equivalent
// to SomeElementsMatcher).
type AtLeastElementsMatcher struct {
	downstream BindingMatcher
	minMatches int
}

func (*AtLeastElementsMatcher) isCollectionMatcher() {}

// NewAtLeastElementsMatcher constructs an AtLeastElementsMatcher.
// Panics on negative minMatches — Java's Verify.verify(min >= 0)
// has the same effect.
func NewAtLeastElementsMatcher(minMatches int, downstream BindingMatcher) *AtLeastElementsMatcher {
	if minMatches < 0 {
		panic(fmt.Sprintf("NewAtLeastElementsMatcher: minMatches must be >= 0, got %d", minMatches))
	}
	return &AtLeastElementsMatcher{
		downstream: downstream,
		minMatches: minMatches,
	}
}

// RootType implements BindingMatcher.
func (*AtLeastElementsMatcher) RootType() string { return "AtLeastElements" }

// BindMatches walks the input slice, counts matching elements, and
// succeeds when the count meets or exceeds minMatches. Returns nil
// on non-slice input or under-threshold match counts.
func (m *AtLeastElementsMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	items, ok := in.([]any)
	if !ok {
		return nil
	}
	matchedCount := 0
	out := make([]*PlannerBindings, 0)
	for _, item := range items {
		matches := m.downstream.BindMatches(outer, item)
		if len(matches) == 0 {
			continue
		}
		matchedCount++
		out = append(out, matches...)
	}
	if matchedCount < m.minMatches {
		return nil
	}
	if matchedCount == 0 {
		// minMatches == 0 + no element-matches → vacuous-true.
		// Bind the matcher to the input so callers see something.
		return []*PlannerBindings{outer.Bind(m, in)}
	}
	for i, b := range out {
		out[i] = b.Bind(m, in)
	}
	return out
}
