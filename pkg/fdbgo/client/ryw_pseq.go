package client

import (
	"math/rand/v2"
	"sync/atomic"
)

// pseq is C++ PTree (VersionedMap.h:39-450) indexed by position, with its versioned spare slots (:46-59);
// only the head writes slots, and frozen versions (share, handoff) copy what they change.
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
	item            T // shared by every version holding the node: never write its memory in place
}

// pslot marks a stamp as a written spare slot's version (PTree::updated).
const pslot = 1 << 63

var pseqOwners atomic.Uint64

// spareAt reports whether n's slot is visible at version at (PTree::child, VersionedMap.h:54-59);
// flipping pslot puts a creation stamp above every version.
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

// child is PTree::child (VersionedMap.h:54-59): the right child when right is set.
func (n *pnode[T]) child(right bool, at uint64) *pnode[T] {
	if right {
		return n.rightAt(at)
	}
	return n.leftAt(at)
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

// alloc takes nodes from chunks, as C++ from FastAllocator pools (VersionedMap.h:46); no node is
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

// update is PTreeImpl::update (VersionedMap.h:133-173): n in place if s created it at this version,
// else n with its free (or own) slot taking the one changed child, else a copy. s must own a version.
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

// handoff moves s's writing to the returned sequence and freezes s for its readers; it does not
// write s.root or s.rv, which they load.
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

// pcursor walks one version of a sequence either way, O(1) amortized per step; it is valid while
// that version's nodes are not written in place.
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

func (c *pcursor[T]) seek(i int) { c.path, c.idx = pathSeek(c.path, c.root, c.at, i), i }

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
	c.path, c.idx = pathSeekFirst(c.path, s.root, c.at, pred)
}

func (c *pcursor[T]) valid() bool { return len(c.path) > 0 }

// peekNext is the item after the current one without moving, nil at the end;
// the cursor must be valid.
func (c *pcursor[T]) peekNext() *T { return pathPeekNext(c.path, c.at) }

// item is the current item; the cursor must be valid. It must not be written.
func (c *pcursor[T]) item() *T { return &c.path[len(c.path)-1].item }

func (c *pcursor[T]) next() { c.move(true) }

func (c *pcursor[T]) prev() { c.move(false) }

func (c *pcursor[T]) move(forward bool) {
	if forward {
		c.idx++
	} else {
		c.idx--
	}
	if len(c.path) == 0 {
		c.seek(c.idx)
		return
	}
	c.path = pathMove(c.path, c.at, forward)
}

// pathSeek returns path, emptied, extended from root to index i; empty off the ends.
func pathSeek[T any](path []*pnode[T], root *pnode[T], at uint64, i int) []*pnode[T] {
	path = path[:0]
	if i < 0 || i >= psize(root, at) {
		return path
	}
	for n := root; ; {
		path = append(path, n)
		l, r := n.kids(at)
		ls := psize(l, at)
		switch {
		case i < ls:
			n = l
		case i == ls:
			return path
		default:
			i -= ls + 1
			n = r
		}
	}
}

// pathSeekFirst is PTreeImpl::lower_bound with its finger trim (VersionedMap.h:188-200):
// the path to the first item satisfying pred, empty when none, and its index.
func pathSeekFirst[T any](path []*pnode[T], root *pnode[T], at uint64, pred func(*T) bool) ([]*pnode[T], int) {
	path = path[:0]
	depth, i, base := 0, psize(root, at), 0
	for n := root; n != nil; {
		path = append(path, n)
		l, r := n.kids(at)
		ls := psize(l, at)
		if pred(&n.item) {
			depth, i = len(path), base+ls
			n = l
		} else {
			base += ls + 1
			n = r
		}
	}
	return path[:depth], i
}

// pathPeekNext is the item after the end of a non-empty path, nil at the end.
func pathPeekNext[T any](path []*pnode[T], at uint64) *T {
	n := path[len(path)-1]
	if r := n.rightAt(at); r != nil {
		for n = r; ; {
			l := n.leftAt(at)
			if l == nil {
				return &n.item
			}
			n = l
		}
	}
	for i := len(path) - 1; i > 0; i-- {
		if path[i-1].leftAt(at) == path[i] {
			return &path[i-1].item
		}
	}
	return nil
}

// pathMove is PTreeImpl::move<forward> (VersionedMap.h:213-230) on a non-empty path.
func pathMove[T any](path []*pnode[T], at uint64, forward bool) []*pnode[T] {
	if c := path[len(path)-1].child(forward, at); c != nil {
		for ; c != nil; c = c.child(!forward, at) {
			path = append(path, c)
		}
		return path
	}
	for {
		child := path[len(path)-1]
		path = path[:len(path)-1]
		if len(path) == 0 || path[len(path)-1].child(forward, at) != child {
			return path
		}
	}
}

// seekFirstFrom is seekFirst as a finger search from the cursor, O(log d) for an answer d items away:
// it climbs to the nearest ancestor past the answer, then descends. Off s's version it is seekFirst.
func (c *pcursor[T]) seekFirstFrom(s *pseq[T], pred func(*T) bool) {
	if len(c.path) == 0 || c.root != s.root || c.at != s.rv {
		c.seekFirst(s, pred)
		return
	}
	p := c.path
	cur, idx := len(p)-1, c.idx // p[cur] is at index idx
	if pred(&p[cur].item) {
		// The answer is p[cur] or before: climb while the nearest ancestor before p[cur]
		// satisfies pred; everything between the two is p[cur].left.
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

// descend extends the path down subtree n (first item at index base) to its first item
// satisfying pred; false, with the path unchanged, when none does.
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

// pfingerN is PTreeFinger::N (VersionedMap.h:79-83): a random treap of n items
// is about 4.3 ln n deep.
const pfingerN = 96

// pfinger keeps its path inline like PTreeFinger (VersionedMap.h:76-131): short reads allocate nothing.
// Kept cursors (the walk's pooled iterators, the snapshot cache's insert cursor) reuse pcursor paths.
type pfinger[T any] struct {
	root *pnode[T]
	at   uint64
	idx  int // the index of the current item; -1 or Len off the ends
	n    int // path[:n] is root to the current node; 0 off either end
	// C++ asserts once a path reaches N nodes (push_back's ASSERT(size_ < N)).
	path [pfingerN - 1]*pnode[T]
}

// finger returns a finger at index i; i == -1 or i == Len are off the ends.
func (s *pseq[T]) finger(i int) pfinger[T] {
	f := pfinger[T]{root: s.root, at: s.rv}
	f.seek(i)
	return f
}

// fit takes a path built in f.path as f's. Outgrowing the array, by an answer or a descent,
// is PTreeFinger::push_back's ASSERT (VersionedMap.h:119-122).
func (f *pfinger[T]) fit(p []*pnode[T]) {
	if cap(p) != len(f.path) {
		panic("pfinger: treap deeper than PTreeFinger::N")
	}
	f.n = len(p)
}

// reset moves the finger to index i of s.
func (f *pfinger[T]) reset(s *pseq[T], i int) {
	f.root, f.at = s.root, s.rv
	f.seek(i)
}

func (f *pfinger[T]) seek(i int) {
	f.idx = i
	f.fit(pathSeek(f.path[:0], f.root, f.at, i))
}

// seekFirst moves the finger to the first item of s satisfying pred; off the end when none does.
func (f *pfinger[T]) seekFirst(s *pseq[T], pred func(*T) bool) {
	f.root, f.at = s.root, s.rv
	p, i := pathSeekFirst(f.path[:0], s.root, s.rv, pred)
	f.idx = i
	f.fit(p)
}

func (f *pfinger[T]) valid() bool { return f.n > 0 }

// item is the current item; the finger must be valid. It must not be written.
func (f *pfinger[T]) item() *T { return &f.path[f.n-1].item }

// peekNext is the item after the current one, nil at the end; the finger must be valid.
func (f *pfinger[T]) peekNext() *T { return pathPeekNext(f.path[:f.n], f.at) }

func (f *pfinger[T]) next() { f.move(true) }

func (f *pfinger[T]) prev() { f.move(false) }

func (f *pfinger[T]) move(forward bool) {
	if forward {
		f.idx++
	} else {
		f.idx--
	}
	if f.n == 0 {
		f.seek(f.idx)
		return
	}
	f.fit(pathMove(f.path[:f.n], f.at, forward))
}
