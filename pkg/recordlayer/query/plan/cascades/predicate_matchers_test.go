package cascades

// Sentinel test for the non-zero-size guard on the predicate matcher
// structs (newNotPredicateMatcher / newComparisonPredicateMatcher /
// newAndPredicateMatcher / newOrPredicateMatcher /
// newValuePredicateMatcher). Lives here in root cascades because the
// constructors themselves are private to this package; originally
// authored alongside matcher_bindings_test.go in the pre-split
// cascades/. Extracted during the cascades package split (RFC-025).

import "testing"

// TestPredicateMatchers_DistinctIdentity is the regression sentinel
// for the non-zero-size guard each predicate matcher relies on.
// After commit 70's generic refactor, the 5 matchers
// (notPredicateMatcher / comparisonPredicateMatcher /
// andPredicateMatcher / orPredicateMatcher / valuePredicateMatcher)
// are all `*predicateMatcher[T]` with a 16-byte `rootType string`
// field — that field is what keeps the struct non-zero-sized.
//
// Two consecutive `new...PredicateMatcher()` calls MUST land at
// distinct heap addresses; otherwise PlannerBindings's matcher →
// []any map collapses two distinct rule pattern instances onto the
// same key. If a future cleanup drops `rootType string` (or replaces
// it with a method-only computation) and the struct becomes zero-
// size, the two allocations would alias under Go's runtime.zerobase
// optimisation. This test catches that.
func TestPredicateMatchers_DistinctIdentity(t *testing.T) {
	t.Parallel()

	notA := newNotPredicateMatcher()
	notB := newNotPredicateMatcher()
	if notA == notB {
		t.Fatal("notPredicateMatcher: two allocations aliased — zero-size-struct hazard")
	}

	cmpA := newComparisonPredicateMatcher()
	cmpB := newComparisonPredicateMatcher()
	if cmpA == cmpB {
		t.Fatal("comparisonPredicateMatcher: two allocations aliased")
	}

	andA := newAndPredicateMatcher()
	andB := newAndPredicateMatcher()
	if andA == andB {
		t.Fatal("andPredicateMatcher: two allocations aliased")
	}

	orA := newOrPredicateMatcher()
	orB := newOrPredicateMatcher()
	if orA == orB {
		t.Fatal("orPredicateMatcher: two allocations aliased")
	}

	vpA := newValuePredicateMatcher()
	vpB := newValuePredicateMatcher()
	if vpA == vpB {
		t.Fatal("valuePredicateMatcher: two allocations aliased")
	}
}
