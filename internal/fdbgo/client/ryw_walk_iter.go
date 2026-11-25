package client

import (
	"bytes"
	"sort"
)

// rywIter is RYWIterator (RYWIterator.cpp:94-126): the intersection of the write
// map's and the snapshot cache's segments, over immutable versions so without a lock.
type rywIter struct {
	w      *rywWrites
	cached *pseq[cacheEntry] // the cache snapshot the cache cursor reads
	writes rywNodeCursor
	cache  rywCacheCursor
	// ents is the last written entry looked up (entryOf): the walk meets written
	// keys in key order, so the next lookup is a short finger search from it.
	ents pcursor[rywWrite]
	// opValue is a written KV's value, resolved once for kv; opFound reports
	// that the segment's key has an entry.
	opValue []byte
	opFound bool
	// opBase and opAtomics are a KV segment's pending atomics over the cached
	// base, until kv resolves them into opValue.
	opBase    []byte
	opAtomics []rywMutation
	valBuf    *[]byte // the walk's buffer for resolved AppendIfFits row values, never reused
	// opPending marks a write classified KV without its entry (typeMap over a
	// known cache segment); segType looks the entry up.
	opPending bool
	// beginCmp and endCmp are begin_key_cmp and end_key_cmp: the cache
	// iterator's begin (end) key compared with the write iterator's.
	beginCmp, endCmp int
	b, e             rywBoundary // the segment, [b, e)
	typ              rywSegType
	sentinel         bool
	// broken records a step past the sentinel, which the walk never takes (A6
	// stops at L4/L6 first); the walk turns it into internal_error.
	broken bool
	one    [1]KeyValue // a contiguous run of one, without allocating
}

// sentinelEnd is \xff\xff\0, the end of the last segment.
var sentinelEnd = []byte{0xff, 0xff, 0x00}

// sentinelNodes is the write side of a snapshot-cache-only iterator: the
// constructor's nodes alone (WriteMap.h:129-134). It is never written.
var sentinelNodes = func() *pseq[rywNode] {
	s := &pseq[rywNode]{}
	s.build(rywNodeSentinels())
	s.share()
	return s
}()

// checkViewLocked rejects a version a walk may not read: the legacy private
// spans have no node boundaries and are neither live nor pinned.
func (c *rywCache) checkViewLocked(w *rywWrites) {
	if w != nil && w != c.w && w.pins <= 0 {
		panic("rywCache.checkViewLocked: the write-map version is neither live nor pinned")
	}
	if w != nil {
		w.nodes.build()
	}
}

// initIter points it at w and the cache snapshot, keeping its cursors' storage.
// w's nodes must be built.
func initIter(it *rywIter, w *rywWrites, cached *pseq[cacheEntry]) {
	nodes := sentinelNodes
	if w != nil {
		nodes = &w.nodes.seq
	}
	it.w, it.cached = w, cached
	it.writes.seq = nodes
	it.cache.es = cached
	it.broken, it.sentinel = false, false
}

// scrub drops every reference it holds, keeping its cursors' storage for reuse.
func (it *rywIter) scrub() {
	wp, cp, ep := scrubPath(it.writes.cur.path), scrubPath(it.cache.ent.path), scrubPath(it.ents.path)
	*it = rywIter{}
	it.writes.cur.path, it.cache.ent.path, it.ents.path = wp, cp, ep
}

// scrubPath clears the nodes a cursor path held and returns it empty: written
// slots, filled by append only, end at the first nil.
func scrubPath[T any](p []*pnode[T]) []*pnode[T] {
	p = p[:cap(p)]
	n := 0
	for n < len(p) && p[n] != nil {
		n++
	}
	clear(p[:n])
	return p[:0]
}

// copyFrom makes it a copy of o, keeping its cursors' storage; the entry
// cursor too, as a copy's lookups are near o's.
func (it *rywIter) copyFrom(o *rywIter) {
	writes, cache, ents := it.writes, it.cache, it.ents
	*it = *o
	it.writes, it.cache, it.ents = writes, cache, ents
	it.writes.copyFrom(&o.writes)
	it.cache.copyFrom(&o.cache)
	it.ents.copyFrom(&o.ents)
}

// beginKey and endKey are the segment's keys as bytes; a key successor is
// allocated, as ExtStringRef::toArena does.
func (it *rywIter) beginKey() []byte { return it.b.materialize() }
func (it *rywIter) endKey() []byte   { return it.e.materialize() }

// atStart reports a segment beginning at "".
func (it *rywIter) atStart() bool { return it.b.size() == 0 }

// skip is RYWIterator::skip: the segment containing key (RYWIterator.cpp:114-118).
// A key at or past \xff\xff lands on the sentinel.
func (it *rywIter) skip(key []byte) {
	if bytes.Compare(key, allKeysEnd) >= 0 {
		key = allKeysEnd
	}
	it.cache.skip(key)
	it.writes.skip(key)
	it.updateCmp()
	it.settle()
}

// updateCmp is RYWIterator::updateCmp (RYWIterator.cpp:161-164).
func (it *rywIter) updateCmp() {
	it.beginCmp = it.cache.beginKey().compare(it.writes.beginKey())
	it.endCmp = it.cache.endKey().compare(it.writes.endKey())
}

// next is operator++ (RYWIterator.cpp:94-103).
func (it *rywIter) next() {
	if it.sentinel {
		it.broken = true
		return
	}
	if it.endCmp <= 0 {
		it.cache.inc()
	}
	if it.endCmp >= 0 {
		it.writes.inc()
	}
	it.beginCmp = -it.endCmp
	it.endCmp = it.cache.endKey().compare(it.writes.endKey())
	it.settle()
}

// prev is operator-- (RYWIterator.cpp:105-112).
func (it *rywIter) prev() {
	if it.atStart() && !it.sentinel {
		it.broken = true
		return
	}
	if it.beginCmp >= 0 {
		it.cache.dec()
	}
	if it.beginCmp <= 0 {
		it.writes.dec()
	}
	it.endCmp = -it.beginCmp
	it.beginCmp = it.cache.beginKey().compare(it.writes.beginKey())
	it.settle()
}

// sentinelBegin and sentinelBoundEnd are the sentinel segment's keys.
var (
	sentinelBegin    = rywBoundary{key: allKeysEnd}
	sentinelBoundEnd = rywBoundary{key: allKeysEnd, after: true}
)

// settle takes the segment's keys (RYWIterator.cpp:65-70) and type.
func (it *rywIter) settle() {
	it.sentinel = it.writes.offset && bytes.Equal(it.writes.entry().key, allKeysEnd)
	if it.sentinel {
		it.b, it.e, it.typ, it.opPending = sentinelBegin, sentinelBoundEnd, segEmpty, false
		return
	}
	it.b = it.writes.beginKey()
	if it.beginCmp > 0 {
		it.b = it.cache.beginKey()
	}
	it.e = it.writes.endKey()
	if it.endCmp <= 0 {
		it.e = it.cache.endKey()
	}
	it.typ = it.classify()
}

// classify is RYWIterator::type (RYWIterator.cpp:44-49). A write over a known
// cache segment is KV pending its entry (typeMap, :25-42); segType resolves it.
func (it *rywIter) classify() rywSegType {
	wr := &it.writes
	it.opPending = false
	if wr.isUnreadable() {
		return segUnreadable
	}
	if wr.isOperation() {
		it.opValue, it.opFound, it.opBase, it.opAtomics = nil, false, nil, nil
		if it.cache.typ() != segUnknown {
			it.opPending = true
			return segKV
		}
		return it.classifyOp()
	}
	if wr.isClearedRange() {
		return segEmpty
	}
	return it.cache.typ()
}

// segType is the segment's type with a pending write classified: KV, or
// segPhantom for a matched CompareAndClear.
func (it *rywIter) segType() rywSegType {
	if it.opPending {
		it.opPending = false
		it.typ = it.classifyOp()
	}
	return it.typ
}

// classifyOp classifies a readable write from its entry, which every operation
// node has (TestRYWNodesAgreeWithStores); a written segment begins at its key.
func (it *rywIter) classifyOp() rywSegType {
	entry, ok := it.entryOf(it.b.key)
	it.opValue, it.opFound, it.opBase, it.opAtomics = nil, ok, nil, nil
	if !ok {
		return segTypeAt(it.cached, it.w, it.b.key)
	}
	if !entry.hasAtomics {
		if entry.absent {
			return segPhantom
		}
		it.opValue = entry.value
		return segKV
	}
	base, known := it.cache.value()
	if !known {
		return segUnknown // DEPENDENT_WRITE over unknown base — must read server.
	}
	// The type needs no value (RYWIterator.cpp:44-49); kv resolves it once.
	cleared, unresolved := atomicsOutcome(base, entry.atomics)
	switch {
	case unresolved:
		return segUnreadable
	case cleared:
		return segPhantom
	}
	it.opBase, it.opAtomics = base, entry.atomics
	return segKV
}

// entryOf is it.w.entryOf(key), searched from the entry looked up last while
// that is on the same (pinned, immutable) version.
func (it *rywIter) entryOf(key []byte) (rywEntry, bool) {
	w := it.w
	if !w.isSorted {
		return w.entryOf(key)
	}
	ks := bytesString(key)
	it.ents.seekFirstFrom(&w.sorted, func(x *rywWrite) bool { return x.key >= ks })
	if it.ents.valid() {
		if x := it.ents.item(); x.key == ks {
			return x.e, true
		}
	}
	return rywEntry{}, false
}

// same is it == itEnd (RYWIterator.cpp:114-116, SnapshotCache.h:255, WriteMap.h:186-188):
// positions, not keys; on one version no step or skip rests on an empty segment.
func (it *rywIter) same(o *rywIter) bool {
	return it.cache.offset == o.cache.offset && it.cache.ent.idx == o.cache.ent.idx &&
		it.writes.offset == o.writes.offset && it.writes.cur.idx == o.writes.cur.idx
}

func (it *rywIter) unreadable() bool { return it.typ == segUnreadable }
func (it *rywIter) unknown() bool    { return it.typ == segUnknown }
func (it *rywIter) empty() bool      { return it.typ == segEmpty }
func (it *rywIter) isKV() bool       { return it.typ == segKV || it.typ == segPhantom }

// kv is RYWIterator::kv (RYWIterator.cpp:73-87): the segment's key and value,
// false for a matched CompareAndClear.
func (it *rywIter) kv() (KeyValue, bool) {
	if it.segType() != segKV {
		return KeyValue{}, false
	}
	if !it.writes.isOperation() {
		return it.cache.kvs()[it.cache.kvIndex()], true
	}
	key := it.b.key
	if !it.opFound {
		return KeyValue{Key: key, Value: segmentValueAt(it.cached, it.w, key)}, true
	}
	if it.opAtomics != nil {
		if it.valBuf != nil && len(it.opAtomics) == 1 && it.opAtomics[0].typ == MutAppendIfFits {
			it.opValue = appendIfFitsInto(it.valBuf, it.opBase, it.opAtomics[0].param)
		} else {
			it.opValue, _, _ = resolveAtomics(it.opBase, it.opAtomics) // once, as RYWIterator::kv (RYWIterator.cpp:73-87)
		}
		it.opBase, it.opAtomics = nil, nil
	}
	return KeyValue{Key: key, Value: it.opValue}, true // classifyOp's value
}

// appendIfFitsInto is doAppendIfFits building a concatenation in *buf; an operand
// it keeps is returned uncopied (rows are never written).
func appendIfFitsInto(buf *[]byte, base, param []byte) []byte {
	e := existing(base)
	if kept, concat := appendIfFitsKept(e, param); !concat {
		if len(kept) == 0 {
			return []byte{} // present and empty
		}
		return kept
	}
	n := len(e) + len(param)
	b := *buf
	if cap(b)-len(b) < n {
		b = make([]byte, 0, max(512, 2*cap(b), n))
	}
	off := len(b)
	s := b[off : off+n : off+n]
	copy(s, e)
	copy(s[len(e):], param)
	*buf = b[:off+n]
	return s
}

// skipContiguous is RYWIterator::skipContiguous (RYWIterator.cpp:128-133, SnapshotCache.h:276-282);
// it returns the run passed, in key order, valid until the next call.
func (it *rywIter) skipContiguous(bound []byte) []KeyValue {
	if !it.writes.isUnmodifiedRange() {
		it.one[0], _ = it.kv()
		return it.one[:]
	}
	b := rywBoundary{key: bound}
	if we := it.writes.endKey(); it.w != nil && we.compare(b) < 0 {
		b = we
	}
	kvs, i := it.cache.kvs(), it.cache.kvIndex()
	// offset = 2 * lower_bound(values[i+1:], bound): the KV just below it. The
	// search gallops from i+1, so a short run costs O(log run) compares.
	j := gallopSearch(i+1, len(kvs), i+1, func(n int) bool { return (rywBoundary{key: kvs[n].Key}).compare(b) >= 0 })
	if j-1 > i {
		it.cache.offset = 2*(j-1) + 2
		it.updateCmp()
		it.settle()
	}
	return kvs[i:j]
}

// skipContiguousBack is RYWIterator::skipContiguousBack (RYWIterator.cpp:135-140,
// SnapshotCache.h:283-290); it returns the run passed, in key order.
func (it *rywIter) skipContiguousBack(bound []byte) []KeyValue {
	if !it.writes.isUnmodifiedRange() {
		it.one[0], _ = it.kv()
		return it.one[:]
	}
	b := rywBoundary{key: bound}
	if it.w != nil {
		if a := it.writes.beginKey().keyAfter(); a.compare(b) > 0 {
			b = a
		}
	}
	kvs, i := it.cache.kvs(), it.cache.kvIndex()
	// offset = 2 * lower_bound(values[:i], bound) + 2, galloping down from i.
	j := gallopSearch(0, i, i, func(n int) bool { return (rywBoundary{key: kvs[n].Key}).compare(b) >= 0 })
	if j < i {
		it.cache.offset = 2*j + 2
		it.updateCmp()
		it.settle()
	}
	return kvs[j : i+1]
}

// rywNodeCursor is WriteMap::iterator (WriteMap.h:152-205, WriteMap.cpp:312-347)
// for the walk, which searches from its position; the mutators use rywNodeIter.
type rywNodeCursor struct {
	seq    *pseq[rywNode]
	cur    pcursor[rywNode]
	next   *rywNode // nextEntry()
	offset bool
}

// copyFrom makes it a copy of o, keeping its cursor's storage.
func (it *rywNodeCursor) copyFrom(o *rywNodeCursor) {
	it.seq, it.next, it.offset = o.seq, o.next, o.offset
	it.cur.copyFrom(&o.cur)
}

func (it *rywNodeCursor) entry() *rywNode     { return it.cur.item() } // WriteMap.h:195
func (it *rywNodeCursor) nextEntry() *rywNode { return it.next }       // WriteMap.h:196

func (it *rywNodeCursor) isClearedRange() bool { return it.offset && it.entry().followingCleared } // :165

func (it *rywNodeCursor) isUnmodifiedRange() bool { return it.offset && !it.entry().followingCleared } // :166
func (it *rywNodeCursor) isOperation() bool       { return !it.offset }                                // :167

// isUnreadable is WriteMap.h:169.
func (it *rywNodeCursor) isUnreadable() bool {
	if it.offset {
		return it.entry().followingUnreadable
	}
	return it.entry().isUnreadable
}

// beginKey is WriteMap.h:176.
func (it *rywNodeCursor) beginKey() rywBoundary {
	return rywBoundary{key: it.entry().key, after: it.offset && it.entry().op}
}

// endKey is WriteMap.h:177.
func (it *rywNodeCursor) endKey() rywBoundary {
	if it.offset {
		return rywBoundary{key: it.nextEntry().key}
	}
	return rywBoundary{key: it.entry().key, after: true}
}

// skip is WriteMap.cpp:335-347, as rywNodeIter.skip, searching from the
// cursor's position.
func (it *rywNodeCursor) skip(key []byte) {
	it.cur.seekFirstFrom(it.seq, func(nd *rywNode) bool { return bytes.Compare(nd.key, key) > 0 })
	if last := it.seq.Len() - 1; it.cur.idx > last {
		it.cur.reset(it.seq, last) // past afterAllKeys: the entry is allKeys.end (:339-340)
	}
	it.next = it.cur.item()
	it.cur.prev()
	it.offset = !it.entry().op || !bytes.Equal(it.entry().key, key) // :346
}

// inc is operator++ (WriteMap.cpp:312-322).
func (it *rywNodeCursor) inc() {
	if !it.offset && !equalsKeyAfter(it.entry().key, it.nextEntry().key) { // :313
		it.offset = true // :314
	} else {
		it.cur.next() // :316-318
		it.next = it.cur.peekNext()
		it.offset = !it.entry().op // :319
	}
}

// dec is operator-- (WriteMap.cpp:323-333).
func (it *rywNodeCursor) dec() {
	if it.offset && it.entry().op { // :324
		it.offset = false // :325
	} else {
		it.next = it.cur.item() // :327-329
		it.cur.prev()
		it.offset = !it.entry().op || !equalsKeyAfter(it.entry().key, it.nextEntry().key) // :330
	}
}

// rywCacheCursor is SnapshotCache::iterator (SnapshotCache.h:175-306); index Len is
// C++'s degenerate last entry [\xff\xff, \xff\xff\0) (:309-312).
type rywCacheCursor struct {
	es     *pseq[cacheEntry]
	ent    pcursor[cacheEntry]
	offset int
	// prevEnd is the end of entry i-1, the begin of the unknown range at
	// offset 0; known says it is set for the current entry.
	prevEnd []byte
	known   bool
}

// copyFrom makes c a copy of o, keeping its cursor's storage.
func (c *rywCacheCursor) copyFrom(o *rywCacheCursor) {
	ent := c.ent
	*c = *o
	c.ent = ent
	c.ent.copyFrom(&o.ent)
}

// entry is the current entry's keys and values.
func (c *rywCacheCursor) entry() (begin, end []byte, kvs []KeyValue) {
	if c.ent.idx >= c.es.Len() {
		return allKeysEnd, sentinelEnd, nil
	}
	e := c.ent.item()
	return e.begin, e.end, e.kvs
}

func (c *rywCacheCursor) kvs() []KeyValue { _, _, kvs := c.entry(); return kvs }

// kvIndex is the index of the current KV's value; only on a KV segment.
func (c *rywCacheCursor) kvIndex() int { return (c.offset - 2) >> 1 }

func (c *rywCacheCursor) segments() int { return 2 * (len(c.kvs()) + 1) } // :167

// typ is SnapshotCache::iterator::type (:193-199) in segment types.
func (c *rywCacheCursor) typ() rywSegType {
	switch {
	case c.offset == 0:
		return segUnknown
	case c.offset&1 == 1:
		return segEmpty
	}
	return segKV
}

// value is the cached state at the segment's begin: the value of a KV, nil
// for an empty range, unknown for an unknown one.
func (c *rywCacheCursor) value() ([]byte, bool) {
	switch c.typ() {
	case segUnknown:
		return nil, false
	case segKV:
		return c.kvs()[c.kvIndex()].Value, true
	}
	return nil, true
}

func (c *rywCacheCursor) prevEndKey() []byte {
	if !c.known {
		c.prevEnd, c.known = allKeysBegin, true
		if i := c.ent.idx; i > 0 {
			c.prevEnd = c.es.at(i - 1).end
		}
	}
	return c.prevEnd
}

// beginKey is SnapshotCache.h:208-217.
func (c *rywCacheCursor) beginKey() rywBoundary {
	switch c.offset {
	case 0:
		return rywBoundary{key: c.prevEndKey()}
	case 1:
		begin, _, _ := c.entry()
		return rywBoundary{key: begin}
	}
	return rywBoundary{key: c.kvs()[(c.offset-2)>>1].Key, after: c.offset&1 == 1}
}

// endKey is SnapshotCache.h:218-225.
func (c *rywCacheCursor) endKey() rywBoundary {
	begin, end, kvs := c.entry()
	switch {
	case c.offset == 0:
		return rywBoundary{key: begin}
	case c.offset == 2*(len(kvs)+1)-1:
		return rywBoundary{key: end}
	}
	return rywBoundary{key: kvs[(c.offset-1)>>1].Key, after: c.offset&1 == 0}
}

// inc is operator++ (:231-242): degenerate segments are skipped.
func (c *rywCacheCursor) inc() {
	orig := c.endKey()
	for {
		c.offset++
		if c.offset == c.segments() {
			_, end, _ := c.entry()
			c.offset = 0
			c.ent.next()
			c.prevEnd, c.known = end, true
		}
		if !c.endKey().equal(orig) { // ExtStringRef ==, sizes first
			return
		}
	}
}

// dec is operator-- (:243-254).
func (c *rywCacheCursor) dec() {
	orig := c.beginKey()
	for {
		c.offset--
		if c.offset < 0 {
			c.ent.prev()
			c.known = false
			c.offset = c.segments() - 1
		}
		if !c.beginKey().equal(orig) {
			return
		}
	}
}

// skip is SnapshotCache::iterator::skip (:258-275): the segment containing key.
func (c *rywCacheCursor) skip(key []byte) {
	if bytes.Equal(key, allKeysEnd) { // :260-264
		c.ent.reset(c.es, c.es.Len())
		c.offset, c.known = 1, false
		return
	}
	// lastLessOrEqual(key) (:266), then lower_bound in its values (:271-272), each
	// from the cursor's position while it is on the same snapshot and entry.
	var was *cacheEntry
	hint := (c.offset - 1) >> 1 // the first value at or after the old position
	if c.ent.valid() && c.ent.root == c.es.root {
		was = c.ent.item()
	}
	c.ent.seekFirstFrom(c.es, func(e *cacheEntry) bool { return bytes.Compare(e.begin, key) > 0 })
	c.ent.prev()
	prevEnd := allKeysBegin
	if c.ent.valid() {
		e := c.ent.item()
		if bytes.Compare(key, e.end) < 0 {
			ge := func(n int) bool { return bytes.Compare(e.kvs[n].Key, key) >= 0 }
			var idx int
			if e == was {
				idx = gallopSearch(0, len(e.kvs), hint, ge)
			} else {
				idx = sort.Search(len(e.kvs), ge)
			}
			c.offset, c.known = idx*2+1, false
			if idx < len(e.kvs) && bytes.Equal(e.kvs[idx].Key, key) {
				c.offset++
			}
			return
		}
		prevEnd = e.end
	}
	c.ent.next() // key >= it->endKey: the unknown range before the next entry
	c.offset, c.prevEnd, c.known = 0, prevEnd, true
}

// gallopSearch is lo + sort.Search(hi-lo, ...), searched outward from hint by
// doubling steps: an answer d away costs O(log d) calls of pred.
func gallopSearch(lo, hi, hint int, pred func(int) bool) int {
	hint = max(lo, min(hint, hi))
	// The answer is in (f, t]: pred(f) is false or f == lo-1, pred(t) is true or t == hi.
	f, t := lo-1, hi
	if hint < hi && !pred(hint) {
		f = hint
		for step := 1; f+step < t; step <<= 1 {
			if pred(f + step) {
				t = f + step
				break
			}
			f += step
		}
	} else {
		t = hint
		for step := 1; t-step > f; step <<= 1 {
			if !pred(t - step) {
				f = t - step
				break
			}
			t -= step
		}
	}
	for f+1 < t { // binary search of (f, t)
		if m := int(uint(f+t) >> 1); pred(m) {
			t = m
		} else {
			f = m
		}
	}
	return t
}
