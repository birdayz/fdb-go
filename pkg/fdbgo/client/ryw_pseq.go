package client

import (
	"math/rand/v2"
	"sync/atomic"
)

// pseq is a persistent sequence, an implicit treap with path copying: the
// structure of C++ PTree (fdbclient/include/fdbclient/VersionedMap.h:39-450),
// which the WriteMap is built on, indexed by position instead of by key. A
// snapshot (share) is O(1); a mutation copies only the O(log n) nodes on its
// path that another snapshot can see. Nodes stamped with the sequence's owner
// are its alone and are updated in place, so a sequence nobody snapshotted
// mutates without copying. A shared node is never written.
//
// Ordered stores keep their items sorted and locate positions with search.
// Item fields that point to memory (slices) are shared between snapshots and
// must never be written in place.
type pseq[T any] struct {
	root  *pnode[T]
	owner uint64 // 0: no node is owned yet
	// free is storage for the next nodes, so a small sequence can live in its
	// owner's struct; it is never reused once handed out.
	free []pnode[T]
}

type pnode[T any] struct {
	left, right *pnode[T]
	item        T
	size        int
	prio        uint32
	owner       uint64
}

var pseqOwners atomic.Uint64

func psize[T any](n *pnode[T]) int {
	if n == nil {
		return 0
	}
	return n.size
}

func (n *pnode[T]) fix() { n.size = 1 + psize(n.left) + psize(n.right) }

func (s *pseq[T]) own() uint64 {
	if s.owner == 0 {
		s.owner = pseqOwners.Add(1)
	}
	return s.owner
}

func (s *pseq[T]) alloc() *pnode[T] {
	if len(s.free) > 0 {
		n := &s.free[0]
		s.free = s.free[1:]
		return n
	}
	return new(pnode[T])
}

// mut returns n itself when s owns it, else a copy that s owns.
func (s *pseq[T]) mut(n *pnode[T]) *pnode[T] {
	o := s.own()
	if n.owner == o {
		return n
	}
	c := s.alloc()
	*c = *n
	c.owner = o
	return c
}

// Len is the number of items.
func (s *pseq[T]) Len() int { return psize(s.root) }

// share returns a snapshot of s. From now on neither s nor the snapshot owns a
// node, so each copies what it changes.
func (s *pseq[T]) share() pseq[T] {
	s.owner = 0
	return pseq[T]{root: s.root}
}

// at returns the item at index i, 0 <= i < Len. The item must not be written.
func (s *pseq[T]) at(i int) *T {
	n := s.root
	for {
		ls := psize(n.left)
		switch {
		case i < ls:
			n = n.left
		case i == ls:
			return &n.item
		default:
			i -= ls + 1
			n = n.right
		}
	}
}

// search returns the first index whose item satisfies pred, or Len when none
// does. pred must be false for a prefix of the sequence and true after it.
func (s *pseq[T]) search(pred func(*T) bool) int {
	i, n, base := s.Len(), s.root, 0
	for n != nil {
		ls := psize(n.left)
		if pred(&n.item) {
			i = base + ls
			n = n.left
		} else {
			base += ls + 1
			n = n.right
		}
	}
	return i
}

// find is search that also returns the item found, nil when none.
func (s *pseq[T]) find(pred func(*T) bool) (int, *T) {
	i, n, base := s.Len(), s.root, 0
	var item *T
	for n != nil {
		ls := psize(n.left)
		if pred(&n.item) {
			i, item = base+ls, &n.item
			n = n.left
		} else {
			base += ls + 1
			n = n.right
		}
	}
	return i, item
}

// before returns the last index whose item does not satisfy pred, and the
// item: search(pred)-1, or -1 and nil when every item does.
func (s *pseq[T]) before(pred func(*T) bool) (int, *T) {
	i, n, base := -1, s.root, 0
	var item *T
	for n != nil {
		ls := psize(n.left)
		if pred(&n.item) {
			n = n.left
		} else {
			i, item = base+ls, &n.item
			base += ls + 1
			n = n.right
		}
	}
	return i, item
}

func (s *pseq[T]) split(n *pnode[T], k int) (l, r *pnode[T]) {
	if n == nil {
		return nil, nil
	}
	n = s.mut(n)
	if psize(n.left) >= k {
		l, n.left = s.split(n.left, k)
		n.fix()
		return l, n
	}
	n.right, r = s.split(n.right, k-psize(n.left)-1)
	n.fix()
	return n, r
}

func (s *pseq[T]) merge(a, b *pnode[T]) *pnode[T] {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case a.prio > b.prio:
		a = s.mut(a)
		a.right = s.merge(a.right, b)
		a.fix()
		return a
	default:
		b = s.mut(b)
		b.left = s.merge(a, b.left)
		b.fix()
		return b
	}
}

func (s *pseq[T]) newNode(v T) *pnode[T] {
	n := s.alloc()
	*n = pnode[T]{item: v, size: 1, prio: rand.Uint32(), owner: s.own()}
	return n
}

// insert puts v at index i, 0 <= i <= Len: down the path to where its priority
// places it, then splitting the subtree there (PTreeImpl::insert, VersionedMap.h:284-303).
func (s *pseq[T]) insert(i int, v T) {
	s.root = s.insertAt(s.root, i, s.newNode(v))
}

func (s *pseq[T]) insertAt(n *pnode[T], i int, nn *pnode[T]) *pnode[T] {
	if n == nil {
		return nn
	}
	if nn.prio > n.prio {
		nn.left, nn.right = s.split(n, i)
		nn.fix()
		return nn
	}
	n = s.mut(n)
	if ls := psize(n.left); i <= ls {
		n.left = s.insertAt(n.left, i, nn)
	} else {
		n.right = s.insertAt(n.right, i-ls-1, nn)
	}
	n.fix()
	return n
}

// mutAt returns the item at index i for writing, copying its path first.
func (s *pseq[T]) mutAt(i int) *T {
	s.root = s.mut(s.root)
	n := s.root
	for {
		ls := psize(n.left)
		switch {
		case i < ls:
			n.left = s.mut(n.left)
			n = n.left
		case i == ls:
			return &n.item
		default:
			i -= ls + 1
			n.right = s.mut(n.right)
			n = n.right
		}
	}
}

// set replaces the item at index i.
func (s *pseq[T]) set(i int, v T) { *s.mutAt(i) = v }

// delete removes the items at [i, j).
func (s *pseq[T]) delete(i, j int) {
	if i >= j {
		return
	}
	if j == i+1 {
		s.root = s.deleteAt(s.root, i)
		return
	}
	l, r := s.split(s.root, i)
	_, r = s.split(r, j-i)
	s.root = s.merge(l, r)
}

// deleteAt removes the item at index i: its node is replaced by the merge of
// its children, so only the path to it is copied.
func (s *pseq[T]) deleteAt(n *pnode[T], i int) *pnode[T] {
	ls := psize(n.left)
	if i == ls {
		return s.merge(n.left, n.right)
	}
	n = s.mut(n)
	if i < ls {
		n.left = s.deleteAt(n.left, i)
	} else {
		n.right = s.deleteAt(n.right, i-ls-1)
	}
	n.fix()
	return n
}

// slice returns a sequence holding the items at [i, j), sharing nodes with s.
func (s *pseq[T]) slice(i, j int) pseq[T] {
	t := s.share()
	if i >= j {
		return pseq[T]{}
	}
	_, r := t.split(t.root, i)
	l, _ := t.split(r, j-i)
	return pseq[T]{root: l}
}

// build replaces the contents with items, in O(len(items)): a Cartesian tree
// over random priorities is a treap.
func (s *pseq[T]) build(items []T) {
	s.root = nil
	if len(items) == 0 {
		return
	}
	stack := make([]*pnode[T], 0, 64)
	slab := make([]pnode[T], len(items)) // one allocation for every node
	o := s.own()
	for i, v := range items {
		n := &slab[i]
		*n = pnode[T]{item: v, size: 1, prio: rand.Uint32(), owner: o}
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
	var sizes func(n *pnode[T]) int
	sizes = func(n *pnode[T]) int {
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
	var walk func(n *pnode[T])
	walk = func(n *pnode[T]) {
		if n == nil {
			return
		}
		walk(n.left)
		dst = append(dst, n.item)
		walk(n.right)
	}
	walk(s.root)
	return dst
}

// pcursor walks a sequence in either direction, O(1) amortized per step. It
// reads the nodes of one snapshot, so it stays valid only while that version
// of the sequence is not written in place.
type pcursor[T any] struct {
	root *pnode[T]
	path []*pnode[T] // root to the current node; empty off either end
	idx  int
}

// cursor returns a cursor at index i; i == -1 or i == Len are off the ends.
func (s *pseq[T]) cursor(i int) pcursor[T] {
	c := pcursor[T]{root: s.root, path: make([]*pnode[T], 0, 32)}
	c.seek(i)
	return c
}

// reset moves the cursor to index i of s, reusing its path storage.
func (c *pcursor[T]) reset(s *pseq[T], i int) {
	c.root = s.root
	if c.path == nil {
		c.path = make([]*pnode[T], 0, 32)
	}
	c.seek(i)
}

func (c *pcursor[T]) seek(i int) {
	c.path, c.idx = c.path[:0], i
	if i < 0 || i >= psize(c.root) {
		return
	}
	n := c.root
	for {
		c.path = append(c.path, n)
		ls := psize(n.left)
		switch {
		case i < ls:
			n = n.left
		case i == ls:
			return
		default:
			i -= ls + 1
			n = n.right
		}
	}
}

// copyFrom moves the cursor to o's position, reusing its path storage.
func (c *pcursor[T]) copyFrom(o *pcursor[T]) {
	c.root, c.idx = o.root, o.idx
	c.path = append(c.path[:0], o.path...)
}

// seekFirst moves the cursor of s to the first index whose item satisfies pred
// (search), in one descent; off the end when none does.
func (c *pcursor[T]) seekFirst(s *pseq[T], pred func(*T) bool) {
	c.root = s.root
	if c.path == nil {
		c.path = make([]*pnode[T], 0, 32)
	}
	c.path = c.path[:0]
	found, depth, i, base := false, 0, psize(s.root), 0
	for n := s.root; n != nil; {
		c.path = append(c.path, n)
		ls := psize(n.left)
		if pred(&n.item) {
			found, depth, i = true, len(c.path), base+ls
			n = n.left
		} else {
			base += ls + 1
			n = n.right
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
	if n.right != nil {
		for n = n.right; n.left != nil; n = n.left {
		}
		return &n.item
	}
	for i := len(c.path) - 1; i > 0; i-- {
		if c.path[i-1].left == c.path[i] {
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
	if n.right != nil {
		for n = n.right; n != nil; n = n.left {
			c.path = append(c.path, n)
		}
		return
	}
	for {
		child := c.path[len(c.path)-1]
		c.path = c.path[:len(c.path)-1]
		if len(c.path) == 0 || c.path[len(c.path)-1].left == child {
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
	if n.left != nil {
		for n = n.left; n != nil; n = n.right {
			c.path = append(c.path, n)
		}
		return
	}
	for {
		child := c.path[len(c.path)-1]
		c.path = c.path[:len(c.path)-1]
		if len(c.path) == 0 || c.path[len(c.path)-1].right == child {
			return
		}
	}
}

func (c *pcursor[T]) clone() pcursor[T] {
	return pcursor[T]{root: c.root, path: append(make([]*pnode[T], 0, max(32, cap(c.path))), c.path...), idx: c.idx}
}
