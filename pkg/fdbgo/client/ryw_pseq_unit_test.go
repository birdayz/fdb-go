package client

import (
	"math/rand"
	"slices"
	"sync"
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
		var sc pcursor[int]
		sc.seekFirst(s, func(v *int) bool { return indexOf(want, v) >= k })
		if sc.idx != k || sc.valid() != (k < len(want)) || (sc.valid() && *sc.item() != want[k]) {
			t.Fatalf("%s: seekFirst(index >= %d) at %d valid %t", step, k, sc.idx, sc.valid())
		}
		var cc pcursor[int]
		cc.copyFrom(&sc)
		cc.prev()
		if cc.idx != k-1 || cc.valid() != (k > 0) || (cc.valid() && *cc.item() != want[k-1]) || sc.idx != k {
			t.Fatalf("%s: prev of a copy at %d moved to %d (original at %d)", step, k, cc.idx, sc.idx)
		}
		var sf pfinger[int]
		sf.seekFirst(s, func(v *int) bool { return indexOf(want, v) >= k })
		if sf.idx != k || sf.valid() != (k < len(want)) || (sf.valid() && *sf.item() != want[k]) {
			t.Fatalf("%s: finger seekFirst(index >= %d) at %d valid %t", step, k, sf.idx, sf.valid())
		}
		cf := sf // a finger is a value: its copy moves alone
		cf.prev()
		if cf.idx != k-1 || cf.valid() != (k > 0) || (cf.valid() && *cf.item() != want[k-1]) || sf.idx != k ||
			(sf.valid() && *sf.item() != want[k]) {
			t.Fatalf("%s: prev of a finger copy at %d moved to %d (original at %d)", step, k, cf.idx, sf.idx)
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
			if c.valid() {
				next := c.peekNext()
				if (i+1 < len(want)) != (next != nil) || (next != nil && *next != want[i+1]) {
					t.Fatalf("%s: peekNext at %d = %v", step, i, next)
				}
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
		checkFinger(t, step, s, want, start)
	}
}

// checkFinger walks fingers from start both ways and back past each end,
// comparing every step with the reference.
func checkFinger(t *testing.T, step string, s *pseq[int], want []int, start int) {
	t.Helper()
	at := func(f *pfinger[int], i int, dir string) {
		t.Helper()
		if f.valid() != (i >= 0 && i < len(want)) || f.idx != i || (f.valid() && *f.item() != want[i]) {
			t.Fatalf("%s: %s finger from %d at %d: valid %t idx %d", step, dir, start, i, f.valid(), f.idx)
		}
		if f.valid() {
			next := f.peekNext()
			if (i+1 < len(want)) != (next != nil) || (next != nil && *next != want[i+1]) {
				t.Fatalf("%s: finger peekNext at %d = %v", step, i, next)
			}
		}
	}
	f := s.finger(start)
	i := start
	for ; i <= len(want); i++ {
		at(&f, i, "forward")
		f.next()
	}
	for i--; i >= start; i-- { // back from past the end
		f.prev()
		at(&f, i, "returning")
	}
	var g pfinger[int]
	g.reset(s, start)
	for i = start; i >= -1; i-- {
		at(&g, i, "backward")
		g.prev()
	}
	g.next()
	at(&g, -1, "returning backward")
	g.next()
	at(&g, 0, "returning backward")
}

// A finger lives in its value: walking one allocates nothing. Not parallel:
// AllocsPerRun counts process-wide allocations.
func TestPfingerAllocatesNothing(t *testing.T) {
	var s pseq[int]
	for i := 0; i < 1000; i++ {
		s.insert(i, i)
	}
	sum := 0
	allocs := testing.AllocsPerRun(10, func() {
		for f := s.finger(0); f.valid(); f.next() {
			sum += *f.item()
		}
		var f pfinger[int]
		f.seekFirst(&s, func(v *int) bool { return *v >= 500 })
		for ; f.valid(); f.prev() {
			sum -= *f.item()
		}
	})
	if allocs != 0 || sum == 0 {
		t.Fatalf("finger walks allocated %v times (sum %d)", allocs, sum)
	}
}

// A path reaching PTreeFinger::N nodes is C++'s ASSERT (VersionedMap.h:119-122): the finger panics.
// A random treap never gets there; a chain of nodes does.
func TestPfingerDepthBound(t *testing.T) {
	t.Parallel()
	var root *pnode[int]
	for i := pfingerN - 1; i >= 0; i-- { // a left chain of pfingerN nodes
		root = &pnode[int]{left: root, item: i, size: int32(pfingerN - i)}
	}
	s := pseq[int]{root: root}
	// In order, the chain's deepest node comes first: index 1 is pfingerN-1 deep.
	f := s.finger(1)
	if !f.valid() || *f.item() != pfingerN-2 || f.n != pfingerN-1 {
		t.Fatalf("a finger at depth %d holds %v", pfingerN-1, f.valid())
	}
	f.next()
	f.prev()
	if !f.valid() || *f.item() != pfingerN-2 || f.n != pfingerN-1 {
		t.Fatalf("a finger moved back to depth %d holds %v", pfingerN-1, f.valid())
	}
	// Each way of reaching depth pfingerN panics: a seek, a step, and a lower_bound
	// descent that passes it even though its answer is shallower (VersionedMap.h:188-200).
	for name, deep := range map[string]func(){
		"seek":      func() { s.finger(0) },
		"prev":      func() { g := s.finger(1); g.prev() },
		"seekFirst": func() { var g pfinger[int]; g.seekFirst(&s, func(v *int) bool { return *v < pfingerN-1 }) },
	} {
		func() {
			defer func() {
				if r := recover(); r != "pfinger: treap deeper than PTreeFinger::N" {
					t.Fatalf("%s to depth %d recovered %v", name, pfingerN, r)
				}
			}()
			deep()
			t.Fatalf("%s to PTreeFinger::N did not panic", name)
		}()
	}
}

// indexOf is the position of the item v points at, recovered from its value:
// the reference items are distinct.
func indexOf(want []int, v *int) int { return slices.Index(want, *v) }

// pseqRef is a sequence under test and the reference slice it must equal.
type pseqRef struct {
	s    pseq[int]
	want []int
}

// Random histories of every mutator on a head and on its frozen versions, with
// snapshots and handoffs taken along the way: every version must keep its
// contents however the others change after it, whichever of them writes slots.
func TestPseqMatchesSlice(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(17))
	next := 0
	fresh := func() int { next++; return next }
	ops := map[string]int{}
	for h := 0; h < 300; h++ {
		seqs := []*pseqRef{{}} // seqs[0] is the head
		for i := 0; i < 1+rng.Intn(80); i++ {
			target := seqs[0]
			if len(seqs) > 1 && rng.Intn(5) == 0 {
				target = seqs[1+rng.Intn(len(seqs)-1)]
				ops["frozen write"]++
			}
			s, want := &target.s, target.want
			switch op := rng.Intn(10); op {
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
				seqs = append(seqs, &pseqRef{s.share(), slices.Clone(want)})
				ops["share"]++
			case 7:
				a, b := rng.Intn(len(want)+1), rng.Intn(len(want)+1)
				if a > b {
					a, b = b, a
				}
				seqs = append(seqs, &pseqRef{s.slice(a, b), slices.Clone(want[a:b])})
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
			case 9:
				// The writer moves to the returned sequence; s keeps its version.
				moved := &pseqRef{s.handoff(), slices.Clone(want)}
				if target == seqs[0] {
					seqs[0] = moved
					seqs = append(seqs, target)
				} else {
					seqs = append(seqs, moved)
				}
				ops["handoff"]++
			}
			target.want = want
			checkPseq(t, "written", s, want)
			if i%8 == 0 {
				for j, r := range seqs {
					if got := r.s.appendTo(nil); !slices.Equal(got, r.want) {
						t.Fatalf("history %d step %d: sequence %d holds %v, want %v", h, i, j, got, r.want)
					}
				}
			}
		}
		for _, r := range seqs {
			checkPseq(t, "version", &r.s, r.want)
		}
	}
	for _, op := range []string{"insert", "set", "mutAt", "delete", "share", "slice", "build", "handoff", "frozen write"} {
		if ops[op] == 0 {
			t.Fatalf("history never ran %s: %v", op, ops)
		}
	}
}

// pseqNodes counts the distinct nodes reachable from s's root at any version.
func pseqNodes(roots ...*pnode[int]) int {
	seen := map[*pnode[int]]bool{}
	var walk func(n *pnode[int])
	walk = func(n *pnode[int]) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		walk(n.left)
		walk(n.right)
		walk(n.spare)
	}
	for _, r := range roots {
		walk(r)
	}
	return len(seen)
}

// An unshared sequence updates its nodes in place. After a snapshot, a set
// copies only the node it changes: its parent takes the copy in its spare slot
// (PTreeImpl::update, VersionedMap.h:129-165), so the path above stays shared.
// Not parallel: AllocsPerRun counts process-wide allocations.
func TestPseqCopiesOnlyChangedNodes(t *testing.T) {
	var s pseq[int]
	for i := 0; i < 1000; i++ {
		s.insert(i, i)
	}
	if allocs := testing.AllocsPerRun(10, func() { s.set(500, 7) }); allocs != 0 {
		t.Fatalf("an unshared set allocated %v times, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(10, func() { s.insert(500, 9); s.delete(500, 501) }); allocs > 1 {
		t.Fatalf("an unshared insert and delete allocated %v times, want at most the new node", allocs)
	}
	snap := s.share()
	before := pseqNodes(s.root)
	s.set(500, 8)
	if *snap.at(500) != 7 || *s.at(500) != 8 {
		t.Fatalf("snapshot %d live %d, want 7 and 8", *snap.at(500), *s.at(500))
	}
	if s.root != snap.root {
		t.Fatal("a set below the root copied the root")
	}
	if added := pseqNodes(s.root, snap.root) - before; added != 1 {
		t.Fatalf("set after share added %d nodes, want the one it changed", added)
	}
	// A second write at the same version reuses the slot and the copy.
	s.set(500, 10)
	if added := pseqNodes(s.root, snap.root) - before; added != 1 || *snap.at(500) != 7 {
		t.Fatalf("a second set at one version added %d nodes, snapshot reads %d", added, *snap.at(500))
	}
}

// With a snapshot before every write, as pinned reads take them, a set copies
// only until it reaches a node whose slot is free: a constant amortized number
// of nodes where path copying copies the whole O(log n) path. An insert changes
// every ancestor's size, so each ancestor fills its slot or, when that is taken,
// is copied: about half the path.
func TestPseqWritesAfterSnapshotsCopyLittle(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(5))
	var s pseq[int]
	for i := 0; i < 1<<14; i++ {
		s.insert(i, i)
	}
	depth := 0
	for i := 0; i < s.Len(); i += 97 {
		c := s.cursor(i)
		depth += len(c.path)
	}
	avgDepth := float64(depth) / float64((s.Len()+96)/97)
	// created counts the nodes n writes add, every version they leave kept alive.
	created := func(n int, write func()) float64 {
		roots := []*pnode[int]{s.root}
		before := pseqNodes(roots...)
		for i := 0; i < n; i++ {
			roots = append(roots, s.share().root)
			write()
		}
		return float64(pseqNodes(append(roots, s.root)...)-before) / float64(n)
	}
	sets := created(2000, func() { s.set(rng.Intn(s.Len()), -1) })
	if sets > 3 {
		t.Fatalf("a set after a snapshot created %.2f nodes on average, want at most 3 (path depth %.1f)", sets, avgDepth)
	}
	inserts := created(2000, func() { s.insert(rng.Intn(s.Len()+1), -2) })
	if inserts > 0.65*avgDepth {
		t.Fatalf("an insert after a snapshot created %.2f nodes on average, want at most 0.65 of the path depth %.1f", inserts, avgDepth)
	}
	t.Logf("nodes created per write after a snapshot: set %.2f, insert %.2f; path depth %.1f", sets, inserts, avgDepth)
}

// Readers of frozen versions run while the head writes slots into the nodes
// they read, as pinned RYW reads do without the cache lock. Under the race
// detector this checks the slot publication; without it, the contents.
func TestPseqFrozenReadersRunWithWriter(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(3))
	var s pseq[int]
	want := make([]int, 0, 4096)
	for i := 0; i < 2000; i++ {
		want = append(want, i)
	}
	s.build(want)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for r := 0; r < 32; r++ {
		snap, ref := s.share(), slices.Clone(want)
		if r%2 == 0 { // the head moves on and this version stays behind
			moved := s.handoff()
			snap, s = s, moved
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pass := 0; pass < 3; pass++ {
				if got := snap.appendTo(nil); !slices.Equal(got, ref) {
					errs <- "a frozen version changed under its reader"
					return
				}
				for c := snap.cursor(snap.Len() - 1); c.valid(); c.prev() {
					if *c.item() != ref[c.idx] {
						errs <- "a cursor over a frozen version read a later write"
						return
					}
				}
			}
		}()
		for w := 0; w < 50; w++ {
			switch p := rng.Intn(len(want)); rng.Intn(3) {
			case 0:
				s.insert(p, -w)
				want = slices.Insert(want, p, -w)
			case 1:
				s.set(p, -w)
				want[p] = -w
			default:
				s.delete(p, p+1)
				want = slices.Delete(want, p, p+1)
			}
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if got := s.appendTo(nil); !slices.Equal(got, want) {
		t.Fatal("the head lost writes made while frozen versions were read")
	}
}
