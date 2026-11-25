package client

import (
	"math/rand"
	"testing"
)

// seekFirstFrom lands where seekFirst does, with the same root-to-node path,
// from every start position (valid, off either end, empty) toward every answer,
// and searches from the root once the sequence has a new version.
func TestPseqSeekFirstFromMatchesSeekFirst(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(262))
	fingered := 0
	for round := 0; round < 300; round++ {
		n := rng.Intn(70)
		items := make([]int, n)
		for i := range items {
			items[i] = 2 * i // even, so odd targets fall between items
		}
		var s pseq[int]
		if rng.Intn(2) == 0 {
			s.build(items)
		} else {
			for i, v := range items {
				s.insert(i, v)
			}
		}
		snap := s.share()
		atLeast := func(k int) func(*int) bool { return func(v *int) bool { return *v >= k } }
		for start := -1; start <= n; start++ {
			for k := -1; k <= 2*n+1; k++ {
				var want pcursor[int]
				want.seekFirst(&snap, atLeast(k))
				got := snap.cursor(start)
				depth := len(got.path)
				got.seekFirstFrom(&snap, atLeast(k))
				if got.idx != want.idx || len(got.path) != len(want.path) {
					t.Fatalf("round %d n %d: from %d to >= %d: idx %d depth %d, want idx %d depth %d", round, n, start, k, got.idx, len(got.path), want.idx, len(want.path))
				}
				for d := range got.path {
					if got.path[d] != want.path[d] {
						t.Fatalf("round %d: from %d to >= %d: path differs at depth %d", round, start, k, d)
					}
				}
				if depth > 0 {
					fingered++
				}
				// Stepping on from the answer agrees with a fresh cursor.
				if got.valid() {
					got.next()
					if fresh := snap.cursor(want.idx + 1); got.idx != fresh.idx || got.valid() != fresh.valid() || (got.valid() && *got.item() != *fresh.item()) {
						t.Fatalf("round %d: next after seekFirstFrom(>= %d) at %d", round, k, got.idx)
					}
				}
			}
		}
		// A new version: the old cursor's root is not the sequence's, so the search
		// starts over rather than reading the old path.
		if n > 0 {
			c := snap.cursor(rng.Intn(n))
			s.insert(0, -2)
			c.seekFirstFrom(&s, atLeast(2*(n-1)))
			if c.root != s.root || c.idx != n || *c.item() != 2*(n-1) {
				t.Fatalf("round %d: after a new version: root updated %t, idx %d", round, c.root == s.root, c.idx)
			}
		}
	}
	if fingered == 0 {
		t.Fatal("vacuous: no search started from a valid cursor")
	}
}

// A search for the next item from the last answer costs O(1) predicate calls
// amortized, where a search from the root costs O(log n).
func TestPseqSeekFirstFromIsAFingerSearch(t *testing.T) {
	t.Parallel()
	const n = 1 << 14
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	var s pseq[int]
	s.build(items)
	calls := 0
	atLeast := func(k int) func(*int) bool {
		return func(v *int) bool { calls++; return *v >= k }
	}
	var finger, root pcursor[int]
	fingerCalls, rootCalls := 0, 0
	for k := 0; k < n; k++ {
		calls = 0
		finger.seekFirstFrom(&s, atLeast(k))
		fingerCalls += calls
		calls = 0
		root.seekFirst(&s, atLeast(k))
		rootCalls += calls
		if finger.idx != k || root.idx != k {
			t.Fatalf("seek to %d: finger at %d, root at %d", k, finger.idx, root.idx)
		}
	}
	if fingerCalls > 4*n || rootCalls < 10*n {
		t.Fatalf("predicate calls over %d sequential seeks: finger %d, root %d", n, fingerCalls, rootCalls)
	}
	t.Logf("predicate calls over %d sequential seeks: finger %d, root %d", n, fingerCalls, rootCalls)
}
