package client

import (
	"math/rand/v2"
	"sync/atomic"
)

// pseq is a persistent sequence, an implicit treap: the structure of C++ PTree
// (fdbclient/include/fdbclient/VersionedMap.h:39-450), which the WriteMap is
// built on, indexed by position instead of by key. A snapshot (share) is O(1).
//
// Versions follow PTree. Each node has a spare slot (PTree::pointer[2] with
// lastUpdateVersion and replacedPointer, VersionedMap.h:46-59) replacing one
// child and the subtree size as of a version: a reader at an older version keeps
// the old child. A writer that changes one child of a node fills a free slot
// instead of copying the node (PTreeImpl::update, :133-173), so the node's
// parent, and the path above it, are left alone; only a node whose slot is taken
// is copied. A sequence reads at version rv; a writer stamps what it changes
// with ver, a fresh version taken after its last snapshot, and owns the nodes
// it created at ver, which it updates in place. A shared node's fields are never
// written apart from a free slot, which is published by an atomic stamp, so
// readers of older versions may run concurrently with the writer.
//
// Only one sequence of a lineage, the head, writes slots. A snapshot (share) and
// a sequence that handed its writing on (handoff) are frozen: they read at their
// version forever and, if written, copy every shared node they change.
//
// Ordered stores keep their items sorted and locate positions with search.
// Item fields that point to memory (slices) are shared between snapshots and
// must never be written in place.
type pseq[T any] struct {
	root *pnode[T]
	// rv is the version read: a slot stamped at or before it is visible.
	rv uint64
	// ver stamps this sequence's writes; 0 until its first write after a snapshot.
	ver    uint64
	frozen bool
	// free is storage for the next nodes, so a small sequence can live in its
	// owner's struct; it is never reused once handed out.
	free  []pnode[T]
	chunk int32 // the size of the last chunk alloc made
}

// pchunkMax bounds the nodes alloc takes from the heap at once.
const pchunkMax = 64

// The fields a descent reads come before the item, in the node's first cache line.
type pnode[T any] struct {
	left, right *pnode[T]
	spare       *pnode[T] // replaces left, or right when spareRight
	// stamp is the version the node was created at, or, with pslot set, the
	// version its spare slot was written at. Readers load it atomically.
	stamp           uint64
	size, spareSize int32
	prio            uint32
	spareRight      bool
	item            T
}

// pslot marks a stamp as a written spare slot's version (PTree::updated).
const pslot = 1 << 63

var pseqOwners atomic.Uint64

// spareAt reports whether n's slot is visible at version at (PTree::child,
// :54-59). Flipping pslot leaves a slot's version, and puts a creation stamp
// above every version, in one comparison.
func (n *pnode[T]) spareAt(at uint64) bool {
	return atomic.LoadUint64(&n.stamp)^pslot <= at
}

// kids returns n's children at version at.
func (n *pnode[T]) kids(at uint64) (l, r *pnode[T]) {
	l, r = n.left, n.right
	if n.spareAt(at) {
		if n.spareRight {
			r = n.spare
		} else {
			l = n.spare
		}
	}
	return l, r
}

func (n *pnode[T]) leftAt(at uint64) *pnode[T] {
	if n.spareAt(at) && !n.spareRight {
		return n.spare
	}
	return n.left
}

func (n *pnode[T]) rightAt(at uint64) *pnode[T] {
	if n.spareAt(at) && n.spareRight {
		return n.spare
	}
	return n.right
}

func psize[T any](n *pnode[T], at uint64) int {
	if n == nil {
		return 0
	}
	size := n.size
	if n.spareAt(at) {
		size = n.spareSize
	}
	return int(size)
}

// own starts a write: a fresh version after a snapshot. A head reads its own
// writes; a frozen sequence keeps reading its version.
func (s *pseq[T]) own() {
	if s.ver == 0 {
		s.ver = pseqOwners.Add(1)
		if !s.frozen {
			s.rv = s.ver
		}
	}
}

// alloc returns storage for a node. Nodes come from chunks that double up to
// pchunkMax, where C++ takes PTree nodes from FastAllocator pools
// (VersionedMap.h:46) rather than the heap one by one. A
// chunk is garbage collected once none of its nodes is reachable; no node is
// handed out twice, so a version a reader holds is never overwritten.
func (s *pseq[T]) alloc() *pnode[T] {
	if len(s.free) == 0 {
		s.chunk = min(max(2*s.chunk, 4), pchunkMax)
		s.free = make([]pnode[T], s.chunk)
	}
	n := &s.free[0]
	s.free = s.free[1:]
	return n
}

func (s *pseq[T]) copyOf(n, l, r *pnode[T], size int32) *pnode[T] {
	c := s.alloc() // zeroed: no node is handed out twice
	c.left, c.right, c.stamp, c.size, c.prio = l, r, s.ver, size, n.prio
	c.item = n.item
	return c
}

// update returns n with children l and r: n itself when nothing changes, n
// written in place when s created it at this version, n with its slot written
// when one child changes and the slot is free or s wrote it, else a copy
// (PTreeImpl::update, VersionedMap.h:133-173). s must own a version.
func (s *pseq[T]) update(n, l, r *pnode[T]) *pnode[T] {
	at := s.rv
	size := int32(1 + psize(l, at) + psize(r, at))
	st := n.stamp // only the writer stores stamps
	if st == s.ver {
		n.left, n.right, n.size = l, r, size
		return n
	}
	if vl, vr := n.kids(at); l == vl && r == vr && int(size) == psize(n, at) {
		return n
	}
	switch {
	case s.frozen:
	case st == s.ver|pslot:
		if n.spareRight && l == n.left {
			n.spare, n.spareSize = r, size
			return n
		}
		if !n.spareRight && r == n.right {
			n.spare, n.spareSize = l, size
			return n
		}
	case st&pslot == 0:
		switch {
		case l == n.left:
			n.spare, n.spareRight = r, true
		case r == n.right:
			n.spare, n.spareRight = l, false
		default:
			return s.copyOf(n, l, r, size)
		}
		n.spareSize = size
		atomic.StoreUint64(&n.stamp, s.ver|pslot) // publishes the slot
		return n
	}
	return s.copyOf(n, l, r, size)
}

// mutItem returns n itself when s created it at this version, else a copy s
// owns, so its item may be written (PTreeImpl::insert of an existing key, VersionedMap.h:289-290).
func (s *pseq[T]) mutItem(n *pnode[T]) *pnode[T] {
	if n.stamp == s.ver {
		return n
	}
	l, r := n.kids(s.rv)
	return s.copyOf(n, l, r, int32(psize(n, s.rv)))
}

// Len is the number of items.
func (s *pseq[T]) Len() int { return psize(s.root, s.rv) }

// share returns a frozen snapshot of s. s goes on writing at a new version, so
// it copies or slots whatever it changes from now on.
func (s *pseq[T]) share() pseq[T] {
	s.ver = 0
	return pseq[T]{root: s.root, rv: s.rv, frozen: true}
}

// handoff returns a sequence that takes over s's contents and writing, and
// leaves s a frozen snapshot of them: for a writer that moves to a clone while
// readers keep the original. s's root and version, which readers load, are not
// written.
func (s *pseq[T]) handoff() pseq[T] {
	h := pseq[T]{root: s.root, rv: s.rv, frozen: s.frozen, free: s.free, chunk: s.chunk}
	s.ver, s.frozen, s.free = 0, true, nil
	return h
}

// at returns the item at index i, 0 <= i < Len. The item must not be written.
func (s *pseq[T]) at(i int) *T {
	n, at := s.root, s.rv
	for {
		l, r := n.kids(at)
		ls := psize(l, at)
		switch {
		case i < ls:
			n = l
		case i == ls:
			return &n.item
		default:
			i -= ls + 1
			n = r
		}
	}
}

// search returns the first index whose item satisfies pred, or Len when none
// does. pred must be false for a prefix of the sequence and true after it.
func (s *pseq[T]) search(pred func(*T) bool) int {
	i, _ := s.find(pred)
	return i
}

// find is search that also returns the item found, nil when none.
func (s *pseq[T]) find(pred func(*T) bool) (int, *T) {
	at := s.rv
	i, n, base := s.Len(), s.root, 0
	var item *T
	for n != nil {
		l, r := n.kids(at)
		ls := psize(l, at)
		if pred(&n.item) {
			i, item = base+ls, &n.item
			n = l
		} else {
			base += ls + 1
			n = r
		}
	}
	return i, item
}

// before returns the last index whose item does not satisfy pred, and the
// item: search(pred)-1, or -1 and nil when every item does.
func (s *pseq[T]) before(pred func(*T) bool) (int, *T) {
	at := s.rv
	i, n, base := -1, s.root, 0
	var item *T
	for n != nil {
		l, r := n.kids(at)
		ls := psize(l, at)
		if pred(&n.item) {
			n = l
		} else {
			i, item = base+ls, &n.item
			base += ls + 1
			n = r
		}
	}
	return i, item
}

func (s *pseq[T]) split(n *pnode[T], k int) (l, r *pnode[T]) {
	if n == nil {
		return nil, nil
	}
	nl, nr := n.kids(s.rv)
	if ls := psize(nl, s.rv); ls >= k {
		l, x := s.split(nl, k)
		return l, s.update(n, x, nr)
	} else {
		x, r := s.split(nr, k-ls-1)
		return s.update(n, nl, x), r
	}
}

func (s *pseq[T]) merge(a, b *pnode[T]) *pnode[T] {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case a.prio > b.prio:
		al, ar := a.kids(s.rv)
		return s.update(a, al, s.merge(ar, b))
	default:
		bl, br := b.kids(s.rv)
		return s.update(b, s.merge(a, bl), br)
	}
}

func (s *pseq[T]) newNode(v T) *pnode[T] {
	n := s.alloc()
	n.stamp, n.size, n.prio = s.ver, 1, rand.Uint32()
	n.item = v
	return n
}

// insert puts v at index i, 0 <= i <= Len: down the path to where its priority
// places it, then splitting the subtree there (PTreeImpl::insert, VersionedMap.h:284-303).
func (s *pseq[T]) insert(i int, v T) {
	s.own()
	s.root = s.insertAt(s.root, i, s.newNode(v))
}

func (s *pseq[T]) insertAt(n *pnode[T], i int, nn *pnode[T]) *pnode[T] {
	if n == nil {
		return nn
	}
	if nn.prio > n.prio {
		nn.left, nn.right = s.split(n, i)
		nn.size = int32(1 + psize(nn.left, s.rv) + psize(nn.right, s.rv))
		return nn
	}
	l, r := n.kids(s.rv)
	if ls := psize(l, s.rv); i <= ls {
		return s.update(n, s.insertAt(l, i, nn), r)
	} else {
		return s.update(n, l, s.insertAt(r, i-ls-1, nn))
	}
}

// mutAt returns the item at index i for writing: its node is copied unless s
// owns it, and its parent takes the copy in its slot.
func (s *pseq[T]) mutAt(i int) *T {
	s.own()
	var item *T
	s.root = s.mutAtNode(s.root, i, &item)
	return item
}

func (s *pseq[T]) mutAtNode(n *pnode[T], i int, item **T) *pnode[T] {
	l, r := n.kids(s.rv)
	ls := psize(l, s.rv)
	switch {
	case i < ls:
		return s.update(n, s.mutAtNode(l, i, item), r)
	case i > ls:
		return s.update(n, l, s.mutAtNode(r, i-ls-1, item))
	}
	c := s.mutItem(n)
	*item = &c.item
	return c
}

// set replaces the item at index i.
func (s *pseq[T]) set(i int, v T) { *s.mutAt(i) = v }

// delete removes the items at [i, j).
func (s *pseq[T]) delete(i, j int) {
	if i >= j {
		return
	}
	s.own()
	if j == i+1 {
		s.root = s.deleteAt(s.root, i)
		return
	}
	l, r := s.split(s.root, i)
	_, r = s.split(r, j-i)
	s.root = s.merge(l, r)
}

// deleteAt removes the item at index i: its node is replaced by the merge of
// its children (PTreeImpl::removeRoot, VersionedMap.h:339-352).
func (s *pseq[T]) deleteAt(n *pnode[T], i int) *pnode[T] {
	l, r := n.kids(s.rv)
	ls := psize(l, s.rv)
	switch {
	case i < ls:
		return s.update(n, s.deleteAt(l, i), r)
	case i > ls:
		return s.update(n, l, s.deleteAt(r, i-ls-1))
	}
	return s.merge(l, r)
}

// slice returns a frozen sequence holding the items at [i, j), sharing nodes
// with s.
func (s *pseq[T]) slice(i, j int) pseq[T] {
	t := s.share()
	if i >= j {
		return pseq[T]{rv: t.rv, frozen: true}
	}
	t.own()
	_, r := t.split(t.root, i)
	l, _ := t.split(r, j-i)
	return pseq[T]{root: l, rv: t.rv, frozen: true}
}

// build replaces the contents with items, in O(len(items)): a Cartesian tree
// over random priorities is a treap.
func (s *pseq[T]) build(items []T) {
	s.root = nil
	if len(items) == 0 {
		return
	}
	s.own()
	stack := make([]*pnode[T], 0, 64)
	slab := make([]pnode[T], len(items)) // one allocation for every node
	for i, v := range items {
		n := &slab[i]
		*n = pnode[T]{item: v, stamp: s.ver, size: 1, prio: rand.Uint32()}
		var last *pnode[T]
		for len(stack) > 0 && stack[len(stack)-1].prio < n.prio {
			last = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		}
		n.left = last
		if len(stack) > 0 {
			stack[len(stack)-1].right = n
		}
		stack = append(stack, n)
	}
	s.root = stack[0]
	var sizes func(n *pnode[T]) int32
	sizes = func(n *pnode[T]) int32 {
		if n == nil {
			return 0
		}
		n.size = 1 + sizes(n.left) + sizes(n.right)
		return n.size
	}
	sizes(s.root)
}

// appendTo appends the items in order.
func (s *pseq[T]) appendTo(dst []T) []T {
	at := s.rv
	var walk func(n *pnode[T])
	walk = func(n *pnode[T]) {
		if n == nil {
			return
		}
		l, r := n.kids(at)
		walk(l)
		dst = append(dst, n.item)
		walk(r)
	}
	walk(s.root)
	return dst
}

// pcursor walks a sequence in either direction, O(1) amortized per step. It
// reads the nodes of one snapshot at its version, so it stays valid while that
// version's nodes are not written in place.
type pcursor[T any] struct {
	root *pnode[T]
	at   uint64
	path []*pnode[T] // root to the current node; empty off either end
	idx  int
}

// cursor returns a cursor at index i; i == -1 or i == Len are off the ends.
func (s *pseq[T]) cursor(i int) pcursor[T] {
	c := pcursor[T]{root: s.root, at: s.rv, path: make([]*pnode[T], 0, 32)}
	c.seek(i)
	return c
}

// reset moves the cursor to index i of s, reusing its path storage.
func (c *pcursor[T]) reset(s *pseq[T], i int) {
	c.root, c.at = s.root, s.rv
	if c.path == nil {
		c.path = make([]*pnode[T], 0, 32)
	}
	c.seek(i)
}

func (c *pcursor[T]) seek(i int) {
	c.path, c.idx = c.path[:0], i
	if i < 0 || i >= psize(c.root, c.at) {
		return
	}
	n := c.root
	for {
		c.path = append(c.path, n)
		l, r := n.kids(c.at)
		ls := psize(l, c.at)
		switch {
		case i < ls:
			n = l
		case i == ls:
			return
		default:
			i -= ls + 1
			n = r
		}
	}
}

// copyFrom moves the cursor to o's position, reusing its path storage.
func (c *pcursor[T]) copyFrom(o *pcursor[T]) {
	c.root, c.at, c.idx = o.root, o.at, o.idx
	c.path = append(c.path[:0], o.path...)
}

// seekFirst moves the cursor of s to the first index whose item satisfies pred
// (search), in one descent; off the end when none does.
func (c *pcursor[T]) seekFirst(s *pseq[T], pred func(*T) bool) {
	c.root, c.at = s.root, s.rv
	if c.path == nil {
		c.path = make([]*pnode[T], 0, 32)
	}
	c.path = c.path[:0]
	at := c.at
	found, depth, i, base := false, 0, psize(s.root, at), 0
	for n := s.root; n != nil; {
		c.path = append(c.path, n)
		l, r := n.kids(at)
		ls := psize(l, at)
		if pred(&n.item) {
			found, depth, i = true, len(c.path), base+ls
			n = l
		} else {
			base += ls + 1
			n = r
		}
	}
	c.idx = i
	if !found {
		c.path = c.path[:0]
		return
	}
	c.path = c.path[:depth] // the descent's prefix ending at the answer
}

func (c *pcursor[T]) valid() bool { return len(c.path) > 0 }

// peekNext is the item after the current one without moving, nil at the end;
// the cursor must be valid.
func (c *pcursor[T]) peekNext() *T {
	n := c.path[len(c.path)-1]
	if r := n.rightAt(c.at); r != nil {
		for n = r; ; {
			l := n.leftAt(c.at)
			if l == nil {
				break
			}
			n = l
		}
		return &n.item
	}
	for i := len(c.path) - 1; i > 0; i-- {
		if c.path[i-1].leftAt(c.at) == c.path[i] {
			return &c.path[i-1].item
		}
	}
	return nil
}

// item is the current item; the cursor must be valid. It must not be written.
func (c *pcursor[T]) item() *T { return &c.path[len(c.path)-1].item }

func (c *pcursor[T]) next() {
	c.idx++
	if len(c.path) == 0 {
		c.seek(c.idx)
		return
	}
	n := c.path[len(c.path)-1]
	if r := n.rightAt(c.at); r != nil {
		for n = r; n != nil; n = n.leftAt(c.at) {
			c.path = append(c.path, n)
		}
		return
	}
	for {
		child := c.path[len(c.path)-1]
		c.path = c.path[:len(c.path)-1]
		if len(c.path) == 0 || c.path[len(c.path)-1].leftAt(c.at) == child {
			return
		}
	}
}

func (c *pcursor[T]) prev() {
	c.idx--
	if len(c.path) == 0 {
		c.seek(c.idx)
		return
	}
	n := c.path[len(c.path)-1]
	if l := n.leftAt(c.at); l != nil {
		for n = l; n != nil; n = n.rightAt(c.at) {
			c.path = append(c.path, n)
		}
		return
	}
	for {
		child := c.path[len(c.path)-1]
		c.path = c.path[:len(c.path)-1]
		if len(c.path) == 0 || c.path[len(c.path)-1].rightAt(c.at) == child {
			return
		}
	}
}

func (c *pcursor[T]) clone() pcursor[T] {
	return pcursor[T]{root: c.root, at: c.at, path: append(make([]*pnode[T], 0, max(32, cap(c.path))), c.path...), idx: c.idx}
}

// seekFirstFrom is seekFirst started from the cursor's position, a finger
// search: an answer d items away costs O(log d) expected predicate calls, not
// O(log n). It needs the cursor on s's version (c.root == s.root, read at s.rv);
// otherwise it is seekFirst. A version a reader holds is never overwritten: its
// nodes are not written in place and slots stamped later are invisible at c.at,
// and the cursor's reference keeps its root from being reused.
//
// From the current node x the items before x are x.left, then each ancestor
// below whose right subtree x lies (with that ancestor's left subtree, and so
// on up), and symmetrically after x. So the search climbs the path only as far
// as the nearest ancestor on the far side of the answer, then descends the one
// subtree between, as from the root.
func (c *pcursor[T]) seekFirstFrom(s *pseq[T], pred func(*T) bool) {
	if len(c.path) == 0 || c.root != s.root || c.at != s.rv {
		c.seekFirst(s, pred)
		return
	}
	p := c.path
	cur, idx := len(p)-1, c.idx // p[cur] is at index idx
	if pred(&p[cur].item) {
		// The answer is p[cur] or before it. Climb while the nearest ancestor
		// before p[cur] (the last step down to p[cur] that went right) satisfies
		// pred: everything between the two is p[cur].left.
		for {
			i := cur - 1
			for i >= 0 && p[i].rightAt(c.at) != p[i+1] {
				i--
			}
			if i < 0 || !pred(&p[i].item) {
				break
			}
			idx -= psize(p[cur].leftAt(c.at), c.at) + 1
			cur = i
		}
		n := p[cur]
		c.path = p[:cur+1]
		if !c.descend(n.leftAt(c.at), idx-psize(n.leftAt(c.at), c.at), pred) {
			c.idx = idx
		}
		return
	}
	// The answer is after p[cur]: climb while the nearest ancestor after it
	// fails pred; everything between the two is p[cur].right.
	for {
		i := cur - 1
		for i >= 0 && p[i].leftAt(c.at) != p[i+1] {
			i--
		}
		if i >= 0 && !pred(&p[i].item) {
			idx += psize(p[cur].rightAt(c.at), c.at) + 1
			cur = i
			continue
		}
		n := p[cur]
		c.path = p[:cur+1]
		if c.descend(n.rightAt(c.at), idx+1, pred) {
			return
		}
		if i < 0 { // no item satisfies pred
			c.path, c.idx = c.path[:0], psize(s.root, c.at)
			return
		}
		c.path, c.idx = c.path[:i+1], idx+psize(n.rightAt(c.at), c.at)+1
		return
	}
}

// descend extends the path down subtree n, whose first item is at index base,
// to its first item satisfying pred. It reports false, the path unchanged,
// when none does.
func (c *pcursor[T]) descend(n *pnode[T], base int, pred func(*T) bool) bool {
	start, depth := len(c.path), 0
	for n != nil {
		c.path = append(c.path, n)
		ls := psize(n.leftAt(c.at), c.at)
		if pred(&n.item) {
			depth, c.idx = len(c.path), base+ls
			n = n.leftAt(c.at)
		} else {
			base += ls + 1
			n = n.rightAt(c.at)
		}
	}
	if depth == 0 {
		c.path = c.path[:start]
		return false
	}
	c.path = c.path[:depth]
	return true
}
