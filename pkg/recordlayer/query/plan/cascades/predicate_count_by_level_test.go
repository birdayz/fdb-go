package cascades

import "testing"

// TestComparePredicateCountByLevel_AsymmetricLevels pins RFC-188 finding 6:
// Java's PredicateCountByLevelProperty.compare iterates ONLY the first map's
// entries in ascending key order (a SortedMap), reading the second map's count
// via getOrDefault(level, 0); the first differing level decides. It never
// visits levels present only in the second map (except via the highest-level
// tiebreak). The Go port iterated the dense UNION of both maps' levels, so for
// asymmetric-depth maps it stopped at a level present only in `b` and returned
// the opposite sign. This comparator is deliberately ASYMMETRIC — Go must match
// that, not "fix" it.
func TestComparePredicateCountByLevel_AsymmetricLevels(t *testing.T) {
	t.Parallel()

	// a has a deeper predicate (level 2), b has a shallower extra (level 1).
	// Java visits a's levels [0,2]: level 0 ties, level 2 → compare(5, 0) = +1.
	// The dense-union bug visited level 1 first → compare(a[1]=0, b[1]=3) = -1.
	a := map[int]int{0: 1, 2: 5}
	b := map[int]int{0: 1, 1: 3}
	if got := comparePredicateCountByLevel(a, b); got != 1 {
		t.Fatalf("compare(a,b) = %d, want +1 (first-map iteration must reach a's level 2)", got)
	}

	// Intentional asymmetry: compare(b, a) also returns +1. Java visits b's
	// levels [0,1]: level 0 ties, level 1 → compare(3, 0) = +1.
	if got := comparePredicateCountByLevel(b, a); got != 1 {
		t.Fatalf("compare(b,a) = %d, want +1 (asymmetric comparator, b's level 1 decides)", got)
	}
}

func TestComparePredicateCountByLevel_SanityCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b map[int]int
		want int
	}{
		{"equal maps", map[int]int{0: 1, 1: 2}, map[int]int{0: 1, 1: 2}, 0},
		{"more at same level", map[int]int{0: 2}, map[int]int{0: 1}, 1},
		{"fewer at same level", map[int]int{0: 1}, map[int]int{0: 2}, -1},
		{"both empty", map[int]int{}, map[int]int{}, 0},
		// b-only deeper level is not visited in the loop; decided by the
		// highest-level tiebreak (a highest 0 vs b highest 2 → -1).
		{"b-only deeper level via tiebreak", map[int]int{0: 1}, map[int]int{0: 1, 2: 3}, -1},
		// a-only deeper level: a's level 2 has count 3, b.getOrDefault(2,0)=0 → +1.
		{"a-only deeper level in loop", map[int]int{0: 1, 2: 3}, map[int]int{0: 1}, 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := comparePredicateCountByLevel(c.a, c.b); got != c.want {
				t.Fatalf("compare = %d, want %d", got, c.want)
			}
		})
	}
}
