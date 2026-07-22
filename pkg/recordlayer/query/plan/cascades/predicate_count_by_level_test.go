package cascades

import "testing"

// TestComparePredicateCountByLevel_Antisymmetric pins RFC-188 finding 6: the
// predicate-count-by-level comparator must be ANTISYMMETRIC (compare(a,b) ==
// -compare(b,a)) so the REWRITING survivor is deterministic. Java's
// PredicateCountByLevelProperty.compare iterates the first map's SortedMap
// entries, but Java's PRODUCER is DENSE (every level 0..highest has an entry,
// 0 for a non-predicate node), so it effectively visits all levels — equivalent
// to the ascending UNION pass Go uses. A first-map-only pass over a SPARSE map
// is non-antisymmetric: {2:1} vs {1:1} would return the same sign in both
// orientations, leaving the survivor insertion-order dependent.
func TestComparePredicateCountByLevel_Antisymmetric(t *testing.T) {
	t.Parallel()

	pairs := [][2]map[int]int{
		// The classic asymmetric-depth pair. The lowest differing level (1)
		// decides: a has 0 there, b has 3 → a < b.
		{{0: 1, 2: 5}, {0: 1, 1: 3}},
		// The non-antisymmetry trap a first-map-only pass fell into
		// (Filter(Distinct(Scan)) {2:1} vs Distinct(Filter(Scan)) {1:1}).
		{{2: 1}, {1: 1}},
		{{0: 2}, {0: 1}},
		{{0: 1}, {0: 1, 2: 3}},
	}
	for _, p := range pairs {
		a, b := p[0], p[1]
		ab := comparePredicateCountByLevel(a, b)
		ba := comparePredicateCountByLevel(b, a)
		if ab != -ba {
			t.Fatalf("not antisymmetric: compare(%v,%v)=%d, compare(%v,%v)=%d", a, b, ab, b, a, ba)
		}
	}

	// The lowest differing level decides (matches Java's dense first-map / the
	// union pass), NOT the deepest.
	if got := comparePredicateCountByLevel(map[int]int{0: 1, 2: 5}, map[int]int{0: 1, 1: 3}); got != -1 {
		t.Fatalf("compare({0:1,2:5},{0:1,1:3}) = %d, want -1 (level 1: 0<3 decides)", got)
	}
	if got := comparePredicateCountByLevel(map[int]int{2: 1}, map[int]int{1: 1}); got != -1 {
		t.Fatalf("compare({2:1},{1:1}) = %d, want -1 (level 1: 0<1 decides)", got)
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
		// b has a deeper level with a count → the loop finds it (a=0 < b) at
		// that level, before the highest-level tiebreak.
		{"b-only deeper level", map[int]int{0: 1}, map[int]int{0: 1, 2: 3}, -1},
		{"a-only deeper level", map[int]int{0: 1, 2: 3}, map[int]int{0: 1}, 1},
		// All counts equal, differ only in depth (dense zero at the deeper
		// level) → highest-level tiebreak.
		{"depth tiebreak a deeper", map[int]int{0: 1, 2: 0}, map[int]int{0: 1}, 1},
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
