package client

import (
	"math/rand"
	"slices"
	"testing"
)

// checkPseq compares s with the reference slice through every accessor: at,
// appendTo, search, find, and cursors walked both ways from every position.
func checkPseq(t *testing.T, step string, s *pseq[int], want []int) {
	t.Helper()
	if s.Len() != len(want) {
		t.Fatalf("%s: Len %d, want %d", step, s.Len(), len(want))
	}
	if got := s.appendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("%s: items %v, want %v", step, got, want)
	}
	for i := range want {
		if got := *s.at(i); got != want[i] {
			t.Fatalf("%s: at(%d) = %d, want %d", step, i, got, want[i])
		}
	}
	// The reference items are distinct, so a position predicate is monotone.
	for k := 0; k <= len(want); k++ {
		i, item := s.find(func(v *int) bool { return indexOf(want, v) >= k })
		if i != k || (k < len(want)) != (item != nil) || (item != nil && *item != want[k]) {
			t.Fatalf("%s: find(index >= %d) = %d, %v", step, k, i, item)
		}
		if got := s.search(func(v *int) bool { return indexOf(want, v) >= k }); got != k {
			t.Fatalf("%s: search(index >= %d) = %d", step, k, got)
		}
		j, prev := s.before(func(v *int) bool { return indexOf(want, v) >= k })
		if j != k-1 || (k > 0) != (prev != nil) || (prev != nil && *prev != want[k-1]) {
			t.Fatalf("%s: before(index >= %d) = %d, %v", step, k, j, prev)
		}
	}
	for start := -1; start <= len(want); start++ {
		c := s.cursor(start)
		for i := start; ; i++ {
			if c.valid() != (i >= 0 && i < len(want)) || c.idx != i {
				t.Fatalf("%s: forward cursor from %d at %d: valid %t idx %d", step, start, i, c.valid(), c.idx)
			}
			if !c.valid() && i >= len(want) {
				break
			}
			if c.valid() && *c.item() != want[i] {
				t.Fatalf("%s: forward cursor from %d at %d: %d, want %d", step, start, i, *c.item(), want[i])
			}
			c.next()
		}
		c = s.cursor(start)
		for i := start; ; i-- {
			if c.valid() != (i >= 0 && i < len(want)) || c.idx != i {
				t.Fatalf("%s: backward cursor from %d at %d: valid %t idx %d", step, start, i, c.valid(), c.idx)
			}
			if !c.valid() && i < 0 {
				break
			}
			if c.valid() && *c.item() != want[i] {
				t.Fatalf("%s: backward cursor from %d at %d: %d, want %d", step, start, i, *c.item(), want[i])
			}
			d := c.clone()
			c.prev()
			if d.idx != i || d.valid() != (i >= 0 && i < len(want)) {
				t.Fatalf("%s: a clone moved with its original", step)
			}
		}
	}
}

// indexOf is the position of the item v points at, recovered from its value:
// the reference items are distinct.
func indexOf(want []int, v *int) int { return slices.Index(want, *v) }

// Random histories of every mutator, with snapshots taken along the way that
// must keep their contents however the sequence changes after them.
func TestPseqMatchesSlice(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(17))
	next := 0
	fresh := func() int { next++; return next }
	type snap struct {
		s    pseq[int]
		want []int
	}
	ops := map[string]int{}
	for h := 0; h < 300; h++ {
		var s pseq[int]
		var want []int
		var snaps []snap
		for i := 0; i < 1+rng.Intn(60); i++ {
			switch op := rng.Intn(9); op {
			case 0, 1, 2:
				p := rng.Intn(len(want) + 1)
				v := fresh()
				s.insert(p, v)
				want = slices.Insert(want, p, v)
				ops["insert"]++
			case 3:
				if len(want) == 0 {
					continue
				}
				p, v := rng.Intn(len(want)), fresh()
				s.set(p, v)
				want[p] = v
				ops["set"]++
			case 4:
				if len(want) == 0 {
					continue
				}
				p, v := rng.Intn(len(want)), fresh()
				*s.mutAt(p) = v
				want[p] = v
				ops["mutAt"]++
			case 5:
				a, b := rng.Intn(len(want)+1), rng.Intn(len(want)+1)
				if a > b {
					a, b = b, a
				}
				s.delete(a, b)
				want = slices.Delete(want, a, b)
				ops["delete"]++
			case 6:
				snaps = append(snaps, snap{s.share(), slices.Clone(want)})
				ops["share"]++
			case 7:
				a, b := rng.Intn(len(want)+1), rng.Intn(len(want)+1)
				if a > b {
					a, b = b, a
				}
				sub := s.slice(a, b)
				snaps = append(snaps, snap{sub, slices.Clone(want[a:b])})
				ops["slice"]++
			case 8:
				n := rng.Intn(20)
				want = want[:0]
				for j := 0; j < n; j++ {
					want = append(want, fresh())
				}
				s.build(want)
				want = slices.Clone(want)
				ops["build"]++
			}
			checkPseq(t, "live", &s, want)
		}
		for i := range snaps {
			checkPseq(t, "snapshot", &snaps[i].s, snaps[i].want)
			// A snapshot is a sequence of its own: writing it leaves the others alone.
			if n := snaps[i].s.Len(); n > 0 {
				snaps[i].s.set(rng.Intn(n), -1)
			}
		}
		checkPseq(t, "live after snapshot writes", &s, want)
	}
	for _, op := range []string{"insert", "set", "mutAt", "delete", "share", "slice", "build"} {
		if ops[op] == 0 {
			t.Fatalf("history never ran %s: %v", op, ops)
		}
	}
}

// An unshared sequence updates its nodes in place; a shared one copies only its path.
// Not parallel: AllocsPerRun counts process-wide allocations.
func TestPseqCopiesOnlySharedPaths(t *testing.T) {
	var s pseq[int]
	for i := 0; i < 1000; i++ {
		s.insert(i, i)
	}
	if allocs := testing.AllocsPerRun(10, func() { s.set(500, 7) }); allocs != 0 {
		t.Fatalf("an unshared set allocated %v times, want 0", allocs)
	}
	snap := s.share()
	s.set(500, 8)
	if *snap.at(500) != 7 || *s.at(500) != 8 {
		t.Fatalf("snapshot %d live %d, want 7 and 8", *snap.at(500), *s.at(500))
	}
	depth := 0
	for n, i := s.root, 500; n != nil; depth++ {
		ls := psize(n.left)
		if i == ls {
			break
		} else if i < ls {
			n = n.left
		} else {
			i -= ls + 1
			n = n.right
		}
	}
	copied := 0
	var walk func(a, b *pnode[int])
	walk = func(a, b *pnode[int]) {
		if a == nil || b == nil {
			return
		}
		if a != b {
			copied++
			walk(a.left, b.left)
			walk(a.right, b.right)
		}
	}
	walk(s.root, snap.root)
	if copied != depth+1 {
		t.Fatalf("set after share copied %d nodes, want the %d on its path", copied, depth+1)
	}
}
