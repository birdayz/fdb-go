package properties

import (
	"testing"
)

// IntersectClaims is the conjunction of two distinctness proofs — Java's
// `leftOrdering.isDistinct() && rightOrdering.isDistinct()` — and it feeds
// MergeOrderingsForUnion, so it runs once per pair of union legs and is folded
// across a multi-leg union.
//
// The direction that matters is one-way. Losing a claim costs a redundant
// DISTINCT; INVENTING one elides a DISTINCT that was needed and returns
// duplicate rows. So the safety law below is stated as an implication rather
// than an equality: a claimed result requires two claimed inputs.
//
// Nothing exercised this as an algebra. The package's other tests build
// orderings and ask them questions; these ask the combinator itself.

// claimCorpus spans the shapes IntersectClaims dispatches on: the empty claim,
// an UNBOUND claim (bindAll, whose coordinates are decided at construction and
// which the combinator deliberately refuses to reason about), and bound claims
// over overlapping and disjoint coordinate sets.
func claimCorpus() []CoordinateBoundClaim {
	return []CoordinateBoundClaim{
		NotDistinct(),
		DistinctOverAllKeys(),
		DistinctOverAllKeysIf(false),
		DistinctOverAllKeys().bindTo([]string{"a"}),
		DistinctOverAllKeys().bindTo([]string{"a", "b"}),
		DistinctOverAllKeys().bindTo([]string{"c"}),
	}
}

func claimsEqual(a, b CoordinateBoundClaim) bool {
	if a.claimed != b.claimed || a.bindAll != b.bindAll || len(a.over) != len(b.over) {
		return false
	}
	for i := range a.over {
		if a.over[i] != b.over[i] {
			return false
		}
	}
	return true
}

// TestIntersectClaims_NeverInventsDistinctness is the safety law. A claimed
// result must have come from two claimed inputs; anything else is a proof
// manufactured out of nothing, and the cost of that is a DISTINCT elided over
// rows that are not distinct.
func TestIntersectClaims_NeverInventsDistinctness(t *testing.T) {
	t.Parallel()

	corpus := claimCorpus()
	claimedResults := 0
	for _, a := range corpus {
		for _, b := range corpus {
			got := IntersectClaims(a, b)
			if !got.claimed {
				continue
			}
			claimedResults++
			if !a.claimed || !b.claimed {
				t.Errorf("IntersectClaims(%+v, %+v) claims distinctness while an input does "+
					"not — a proof manufactured from nothing, which elides a DISTINCT over "+
					"rows that are not distinct", a, b)
			}
			// The coordinates a claim is bound to are what IsDistinct checks
			// against; a result bound to FEWER coordinates than its inputs would
			// answer true for orderings neither input covered.
			for _, key := range a.over {
				if !containsKey(got.over, key) {
					t.Errorf("IntersectClaims(%+v, %+v) = %+v drops coordinate %q from the "+
						"left input; the conjunction must be bound to the UNION", a, b, got, key)
				}
			}
			for _, key := range b.over {
				if !containsKey(got.over, key) {
					t.Errorf("IntersectClaims(%+v, %+v) = %+v drops coordinate %q from the "+
						"right input", a, b, got, key)
				}
			}
		}
	}

	// Vacuity guard: every assertion sits behind `if !got.claimed { continue }`,
	// so a combinator that claimed nothing would skip them all and pass clean.
	if claimedResults < 4 {
		t.Fatalf("only %d of %d pairs produced a claimed result — too few to exercise the "+
			"law", claimedResults, len(corpus)*len(corpus))
	}
	t.Logf("intersect-claims safety: %d claimed results over %d pairs",
		claimedResults, len(corpus)*len(corpus))
}

func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// TestIntersectClaims_IsCommutativeAndAssociative pins that the conjunction is
// order-independent. It is folded across the legs of a multi-leg union, and the
// legs arrive in whatever order the planner enumerated them — the same property
// IntersectCompensations turned out to lack.
func TestIntersectClaims_IsCommutativeAndAssociative(t *testing.T) {
	t.Parallel()

	corpus := claimCorpus()
	for _, a := range corpus {
		for _, b := range corpus {
			if !claimsEqual(IntersectClaims(a, b), IntersectClaims(b, a)) {
				t.Errorf("IntersectClaims is not commutative at (%+v, %+v)", a, b)
			}
			for _, c := range corpus {
				left := IntersectClaims(IntersectClaims(a, b), c)
				right := IntersectClaims(a, IntersectClaims(b, c))
				if !claimsEqual(left, right) {
					t.Errorf("IntersectClaims is not associative at (%+v, %+v, %+v): %+v vs %+v",
						a, b, c, left, right)
				}
			}
		}
	}
}

// TestIntersectClaims_UnboundOperandsFailClosed pins the arm that makes the
// combinator NOT idempotent, deliberately.
//
// An unbound claim (DistinctOverAllKeys, whose coordinates are decided when an
// ordering is constructed with it) has nothing to contribute to a conjunction,
// so IntersectClaims refuses rather than guessing — which means intersecting an
// unbound claim WITH ITSELF yields NotDistinct. That looks like a bug and is the
// safe direction: it loses a claim rather than stating one over coordinates
// nobody proved.
func TestIntersectClaims_UnboundOperandsFailClosed(t *testing.T) {
	t.Parallel()

	unbound := DistinctOverAllKeys()
	if !unbound.claimed || !unbound.bindAll {
		t.Fatal("DistinctOverAllKeys is no longer an unbound claim; this test's premise is gone")
	}

	if got := IntersectClaims(unbound, unbound); got.claimed {
		t.Error("intersecting an unbound claim with itself produced a claim. The coordinates " +
			"it would be bound to are not known here, so stating one asserts distinctness " +
			"over a key set nobody proved.")
	}
	bound := DistinctOverAllKeys().bindTo([]string{"a"})
	if got := IntersectClaims(unbound, bound); got.claimed {
		t.Error("intersecting an unbound claim with a bound one produced a claim")
	}

	// The control: two BOUND claims do combine, so the refusals above are about
	// unboundedness and not about the combinator refusing everything.
	if got := IntersectClaims(bound, DistinctOverAllKeys().bindTo([]string{"b"})); !got.claimed {
		t.Fatal("two bound claims must combine; without this the refusals above prove nothing")
	}
}
