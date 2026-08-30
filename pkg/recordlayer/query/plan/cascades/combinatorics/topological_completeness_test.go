package combinatorics

import (
	"sort"
	"strings"
	"testing"
)

// TopologicalOrderPermutations feeds RichOrdering's enumeration of the
// comparison-key orders a merge or intersection may use. FuzzTopologicalSort
// checks that every permutation it yields is SOUND — right length, no repeated
// element, every dependency ahead of its dependent. It does not check that the
// enumeration is COMPLETE, and it cannot see a permutation emitted twice
// (its duplicate check is within one permutation, not across them).
//
// Both halves matter and they fail differently. A missing permutation is a lost
// plan: a valid comparison-key order the planner never considers. A repeated one
// is wasted enumeration, and any caller counting or indexing the results is then
// counting the same order twice.
//
// This settles both by brute force. For each dependency relation it generates
// every permutation of the element set, keeps the ones respecting the relation,
// and requires the enumerator's output to be that set EXACTLY — same members,
// same cardinality.
//
// It covers BOTH implementations, which matters because complexIterable routes
// on dependency density (`depRatio > 0.5`) and the two iterators are separate
// algorithms. Measured by instrumenting that branch over this exact sweep: 1014
// relations take the Kahn iterator, 84 take the backtrack iterator, and the one
// remaining is the size-1 singleton path — 1099 in total. A sweep that happened
// to sit on one side of the threshold would leave the other algorithm entirely
// unchecked while looking exhaustive.

// allPermutations returns every ordering of elems, by Heap's algorithm.
func allPermutations(elems []string) [][]string {
	var out [][]string
	work := append([]string(nil), elems...)
	var generate func(k int)
	generate = func(k int) {
		if k == 1 {
			out = append(out, append([]string(nil), work...))
			return
		}
		for i := 0; i < k; i++ {
			generate(k - 1)
			if k%2 == 0 {
				work[i], work[k-1] = work[k-1], work[i]
			} else {
				work[0], work[k-1] = work[k-1], work[0]
			}
		}
	}
	if len(work) == 0 {
		return [][]string{{}}
	}
	generate(len(work))
	return out
}

// respectsDependencies reports whether perm places every dependency of an
// element before that element.
func respectsDependencies(perm []string, deps SetMultimap[string]) bool {
	position := make(map[string]int, len(perm))
	for i, e := range perm {
		position[e] = i
	}
	for i, e := range perm {
		for dep := range deps.Get(e) {
			j, ok := position[dep]
			if !ok || j >= i {
				return false
			}
		}
	}
	return true
}

func TestTopologicalOrderPermutations_EnumeratesExactlyTheValidOrders(t *testing.T) {
	t.Parallel()

	// Sizes 1..5. The dependency relation is drawn over the i<j pairs only,
	// which makes every generated relation acyclic by construction — a cyclic
	// one has no topological order at all and is a different question.
	totalRelations, totalOrders := 0, 0
	for size := 1; size <= 5; size++ {
		elems := make([]string, size)
		for i := range elems {
			elems[i] = string(rune('a' + i))
		}
		pairCount := size * (size - 1) / 2
		perms := allPermutations(elems)

		for mask := 0; mask < 1<<pairCount; mask++ {
			deps := NewSetMultimap[string]()
			bit := 0
			for i := 0; i < size; i++ {
				for j := i + 1; j < size; j++ {
					if mask&(1<<bit) != 0 {
						deps.Put(elems[j], elems[i]) // elems[j] depends on elems[i]
					}
					bit++
				}
			}
			totalRelations++

			want := map[string]struct{}{}
			for _, perm := range perms {
				if respectsDependencies(perm, deps) {
					want[strings.Join(perm, "")] = struct{}{}
				}
			}

			got := map[string]int{}
			iter := TopologicalOrderPermutations(NewPartiallyOrderedSet(elems, deps))
			for {
				perm := iter.Next()
				if perm == nil {
					break
				}
				got[strings.Join(perm, "")]++
				totalOrders++
			}

			for key, n := range got {
				if _, valid := want[key]; !valid {
					t.Errorf("size %d mask %d: enumerated %q, which does NOT respect the "+
						"dependency relation — the soundness fuzz would have caught this",
						size, mask, key)
				}
				if n > 1 {
					t.Errorf("size %d mask %d: enumerated %q %d times; a caller counting or "+
						"indexing these orders counts the same one repeatedly", size, mask, key, n)
				}
			}
			if len(got) != len(want) {
				missing := make([]string, 0, len(want))
				for key := range want {
					if _, emitted := got[key]; !emitted {
						missing = append(missing, key)
					}
				}
				sort.Strings(missing)
				t.Errorf("size %d mask %d: enumerated %d orders, %d are valid. Missing: %v. "+
					"Each missing order is a comparison-key sequence the planner will never "+
					"consider.", size, mask, len(got), len(want), missing)
			}
		}
	}

	// 1 + 2 + 8 + 64 + 1024 dependency relations across sizes 1..5.
	if totalRelations != 1+2+8+64+1024 {
		t.Fatalf("covered %d dependency relations, want %d — the enumeration changed shape",
			totalRelations, 1+2+8+64+1024)
	}
	// The vacuity guard: every assertion above is over the orders actually
	// emitted, so an enumerator returning nothing would leave the per-key loops
	// empty. Only the cardinality check would fire, and only where want is
	// non-empty — so guard the total explicitly. Measured: 10105.
	if totalOrders < 5000 {
		t.Fatalf("the enumerator produced only %d orders across %d relations — far too few; "+
			"it has stopped enumerating and most checks above ran on an empty set",
			totalOrders, totalRelations)
	}
	t.Logf("topological completeness: %d dependency relations, %d orders enumerated",
		totalRelations, totalOrders)
}
