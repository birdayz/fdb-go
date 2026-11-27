package combinatorics

import (
	"testing"
)

func FuzzTopologicalSort(f *testing.F) {
	f.Add(uint8(3), uint8(0))
	f.Add(uint8(4), uint8(0))
	f.Add(uint8(5), uint8(3))
	f.Add(uint8(6), uint8(15))
	f.Add(uint8(2), uint8(1))

	f.Fuzz(func(t *testing.T, n uint8, depBits uint8) {
		size := int(n%6) + 1
		elems := make([]string, size)
		for i := range elems {
			elems[i] = string(rune('a' + i))
		}

		deps := NewSetMultimap[string]()
		bit := 0
		for i := 0; i < size; i++ {
			for j := i + 1; j < size; j++ {
				if bit < 8 && depBits&(1<<bit) != 0 {
					deps.Put(elems[j], elems[i])
				}
				bit++
			}
		}

		p := NewPartiallyOrderedSet(elems, deps)
		iter := TopologicalOrderPermutations(p)
		count := 0
		for {
			perm := iter.Next()
			if perm == nil {
				break
			}
			if len(perm) != size {
				t.Fatalf("perm len %d != size %d", len(perm), size)
			}

			seen := make(map[string]struct{})
			for _, e := range perm {
				if _, dup := seen[e]; dup {
					t.Fatalf("duplicate element %s in perm", e)
				}
				seen[e] = struct{}{}
			}

			for i, e := range perm {
				for dep := range deps.Get(e) {
					found := false
					for j := 0; j < i; j++ {
						if perm[j] == dep {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("dependency %s of %s not before it in perm %v", dep, e, perm)
					}
				}
			}

			count++
			if count > 1000 {
				break
			}
		}
	})
}

func FuzzEligibleSet(f *testing.F) {
	f.Add(uint8(3), uint8(0))
	f.Add(uint8(4), uint8(5))

	f.Fuzz(func(t *testing.T, n uint8, depBits uint8) {
		size := int(n%5) + 1
		elems := make([]string, size)
		for i := range elems {
			elems[i] = string(rune('a' + i))
		}

		deps := NewSetMultimap[string]()
		bit := 0
		for i := 0; i < size; i++ {
			for j := i + 1; j < size; j++ {
				if bit < 8 && depBits&(1<<bit) != 0 {
					deps.Put(elems[j], elems[i])
				}
				bit++
			}
		}

		p := NewPartiallyOrderedSet(elems, deps)
		es := p.EligibleSet()

		consumed := make(map[string]struct{})
		for !es.IsEmpty() {
			eligible := es.EligibleElements()
			if len(eligible) == 0 {
				break
			}
			for e := range eligible {
				for dep := range deps.Get(e) {
					if _, ok := consumed[dep]; !ok {
						t.Fatalf("eligible element %s has unsatisfied dep %s", e, dep)
					}
				}
				consumed[e] = struct{}{}
			}
			es = es.RemoveEligibleElements(eligible)
		}
	})
}
