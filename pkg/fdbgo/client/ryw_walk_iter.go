package client

import (
	"bytes"
	"sort"
)

// rywIter is the walk's iterator (RFC-262 D2): C++ RYWIterator over one version
// of the write map (w), or SnapshotCache::iterator when w is nil (A2.2). Like
// RYWIterator (RYWIterator.cpp:94-126) it composes two iterators, the write
// map's (rywNodeCursor) and the snapshot cache's (rywCacheCursor): its segment
// is their intersection, and a step advances whichever ends (or begins) first,
// in O(1) amortized time. Its segments are therefore the union of the write
// map's node boundaries (D1b) and the snapshot cache's boundaries, over ["",
// \xff\xff), plus the sentinel [\xff\xff, \xff\xff\0), which is EMPTY
// (SnapshotCache.h:308-313, WriteMap.h:129-134). With w nil the write side is
// the sentinel nodes alone, an unmodified range over the whole keyspace.
// It is valid only inside the critical section that created it.
type rywIter struct {
	c      *rywCache
	w      *rywWrites
	writes rywNodeCursor
	cache  rywCacheCursor
	// beginCmp and endCmp are begin_key_cmp and end_key_cmp: the cache
	// iterator's begin (end) key compared with the write iterator's.
	beginCmp, endCmp int
	begin, end       []byte
	typ              rywSegType
	sentinel         bool
	// broken records a step past the sentinel, which the walk never takes (A6
	// stops at L4/L6 first); the walk turns it into internal_error.
	broken bool
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

func (c *rywCache) newIterLocked(w *rywWrites) *rywIter {
	nodes := sentinelNodes
	if w != nil {
		// Legacy private spans have no node boundaries and are neither live nor pinned.
		if w != c.w && w.pins <= 0 {
			panic("rywCache.newIterLocked: the write-map version is neither live nor pinned")
		}
		w.nodes.build()
		nodes = &w.nodes.seq
	}
	return &rywIter{c: c, w: w, writes: rywNodeCursor{seq: nodes}, cache: rywCacheCursor{es: &c.serverCache.entries}}
}

func (it *rywIter) clone() *rywIter {
	n := *it
	n.writes = it.writes.clone()
	n.cache = it.cache.clone()
	return &n
}

// skip is RYWIterator::skip: the segment containing key (RYWIterator.cpp:114-118).
// A key at or past \xff\xff lands on the sentinel.
func (it *rywIter) skip(key []byte) {
	if bytes.Compare(key, allKeysEnd) >= 0 {
		key = allKeysEnd
	}
	it.cache.skip(key)
	it.writes.skip(key)
	it.beginCmp = it.cache.beginKey().compare(it.writes.beginKey()) // updateCmp (:161-164)
	it.endCmp = it.cache.endKey().compare(it.writes.endKey())
	it.settle()
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
	if len(it.begin) == 0 && !it.sentinel {
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

// settle computes the segment's keys (RYWIterator.cpp:65-70) and type.
func (it *rywIter) settle() {
	b := it.writes.beginKey()
	if it.beginCmp > 0 {
		b = it.cache.beginKey()
	}
	e := it.writes.endKey()
	if it.endCmp <= 0 {
		e = it.cache.endKey()
	}
	it.sentinel = it.writes.offset && bytes.Equal(it.writes.entry().key, allKeysEnd)
	if it.sentinel {
		it.begin, it.end, it.typ = allKeysEnd, sentinelEnd, segEmpty
		return
	}
	it.begin, it.end = b.materialize(), e.materialize()
	it.typ = it.classify()
}

// classify is RYWIterator::type (RYWIterator.cpp:44-49) in segTypeAtLocked's
// terms: the write map decides an unreadable, cleared or written segment, and
// the cache an unmodified one. A written key's type comes from its entry,
// resolved against the cached base. The node flags agree with the stores
// segTypeAtLocked reads (TestRYWNodesAgreeWithStores).
func (it *rywIter) classify() rywSegType {
	wr := &it.writes
	if wr.isUnreadable() {
		return segUnreadable
	}
	if wr.isOperation() {
		entry, ok := it.w.entryOf(it.begin)
		if !ok {
			return it.c.segTypeAtLocked(it.w, it.begin)
		}
		if !entry.hasAtomics {
			if entry.absent {
				return segPhantom
			}
			return segKV
		}
		base, known := it.cache.value()
		if !known {
			return segUnknown // DEPENDENT_WRITE over unknown base — must read server.
		}
		_, cleared, unresolved := resolveAtomics(base, entry.atomics)
		switch {
		case unresolved:
			return segUnreadable
		case cleared:
			return segPhantom
		}
		return segKV
	}
	if wr.isClearedRange() {
		return segEmpty
	}
	return it.cache.typ()
}

// same is it == itEnd: the same segment of the same partition.
func (it *rywIter) same(o *rywIter) bool { return bytes.Equal(it.begin, o.begin) }

func (it *rywIter) unreadable() bool { return it.typ == segUnreadable }
func (it *rywIter) unknown() bool    { return it.typ == segUnknown }
func (it *rywIter) empty() bool      { return it.typ == segEmpty }
func (it *rywIter) isKV() bool       { return it.typ == segKV || it.typ == segPhantom }

// kv is RYWIterator::kv: the segment's key and value, false for a matched
// CompareAndClear (RYWIterator.cpp:82-87). Only on a KV segment.
func (it *rywIter) kv() (KeyValue, bool) {
	if it.typ != segKV {
		return KeyValue{}, false
	}
	base, _ := it.cache.value()
	if !it.writes.isOperation() {
		return KeyValue{Key: it.begin, Value: base}, true
	}
	entry, ok := it.w.entryOf(it.begin)
	switch {
	case !ok:
		return KeyValue{Key: it.begin, Value: it.c.segmentValueLocked(it.w, it.begin)}, true
	case !entry.hasAtomics:
		return KeyValue{Key: it.begin, Value: entry.value}, true
	}
	value, _, _ := resolveAtomics(base, entry.atomics)
	return KeyValue{Key: it.begin, Value: value}, true
}

// skipContiguous is RYWIterator::skipContiguous (RYWIterator.cpp:128-133) over
// SnapshotCache::iterator::skipContiguous (SnapshotCache.h:276-282): on a cache KV
// under an unmodified write segment, land on the last value of the same cache
// entry that is < min(bound, the write segment's end). It returns the values
// from the current one to the landing, in key order. Elsewhere it returns the
// current KV alone. A snapshot-cache-only iterator has no write segment to cap
// the run.
func (it *rywIter) skipContiguous(bound []byte) []KeyValue {
	kv, _ := it.kv()
	if !it.writes.isUnmodifiedRange() {
		return []KeyValue{kv}
	}
	b := rywBoundary{key: bound}
	if we := it.writes.endKey(); it.w != nil && we.compare(b) < 0 {
		b = we
	}
	kvs, i := it.cache.kvs(), it.cache.kvIndex()
	// offset = 2 * lower_bound(values[i+1:], bound): the KV just below it.
	j := i + 1 + sort.Search(len(kvs)-i-1, func(n int) bool { return (rywBoundary{key: kvs[i+1+n].Key}).compare(b) >= 0 })
	if j-1 > i {
		it.cache.offset = 2*(j-1) + 2
		it.beginCmp = it.cache.beginKey().compare(it.writes.beginKey())
		it.endCmp = it.cache.endKey().compare(it.writes.endKey())
		it.settle()
	}
	return kvs[i:j]
}

// skipContiguousBack is RYWIterator::skipContiguousBack (RYWIterator.cpp:135-140)
// over SnapshotCache.h:283-290: land on the first value of the same entry that
// is >= max(bound, keyAfter(the write segment's begin)). It returns the values
// from the landing to the current one, in key order.
func (it *rywIter) skipContiguousBack(bound []byte) []KeyValue {
	kv, _ := it.kv()
	if !it.writes.isUnmodifiedRange() {
		return []KeyValue{kv}
	}
	if it.w != nil {
		if a := keyAfterBytes(it.writes.beginKey().materialize()); bytes.Compare(a, bound) > 0 {
			bound = a
		}
	}
	kvs, i := it.cache.kvs(), it.cache.kvIndex()
	// offset = 2 * lower_bound(values[:i], bound) + 2.
	j := sort.Search(i, func(n int) bool { return bytes.Compare(kvs[n].Key, bound) >= 0 })
	if j < i {
		it.cache.offset = 2*j + 2
		it.beginCmp = it.cache.beginKey().compare(it.writes.beginKey())
		it.endCmp = it.cache.endKey().compare(it.writes.endKey())
		it.settle()
	}
	return kvs[j : i+1]
}

// rywNodeCursor is WriteMap::iterator (WriteMap.h:152-205, WriteMap.cpp:312-347)
// for the walk: cursors at entry() and nextEntry(), so a step is O(1)
// amortized. The mutators use rywNodeIter, which addresses nodes by index.
type rywNodeCursor struct {
	seq      *pseq[rywNode]
	cur, nxt pcursor[rywNode]
	offset   bool
}

func (it *rywNodeCursor) clone() rywNodeCursor {
	return rywNodeCursor{seq: it.seq, cur: it.cur.clone(), nxt: it.nxt.clone(), offset: it.offset}
}

func (it *rywNodeCursor) entry() *rywNode     { return it.cur.item() } // WriteMap.h:195
func (it *rywNodeCursor) nextEntry() *rywNode { return it.nxt.item() } // WriteMap.h:196

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

// skip is WriteMap.cpp:335-347, as rywNodeIter.skip.
func (it *rywNodeCursor) skip(key []byte) {
	it.nxt.seekFirst(it.seq, func(nd *rywNode) bool { return bytes.Compare(nd.key, key) > 0 })
	if last := it.seq.Len() - 1; it.nxt.idx > last {
		it.nxt.reset(it.seq, last) // past afterAllKeys: the entry is allKeys.end (:339-340)
	}
	it.cur.copyFrom(&it.nxt)
	it.cur.prev()
	it.offset = !it.entry().op || !bytes.Equal(it.entry().key, key) // :346
}

// inc is operator++ (WriteMap.cpp:312-322).
func (it *rywNodeCursor) inc() {
	if !it.offset && !equalsKeyAfter(it.entry().key, it.nextEntry().key) { // :313
		it.offset = true // :314
	} else {
		it.cur.next() // :316-318
		it.nxt.next()
		it.offset = !it.entry().op // :319
	}
}

// dec is operator-- (WriteMap.cpp:323-333).
func (it *rywNodeCursor) dec() {
	if it.offset && it.entry().op { // :324
		it.offset = false // :325
	} else {
		it.cur.prev() // :327-329
		it.nxt.prev()
		it.offset = !it.entry().op || !equalsKeyAfter(it.entry().key, it.nextEntry().key) // :330
	}
}

// rywCacheCursor is SnapshotCache::iterator (SnapshotCache.h:146-297) over the
// cache entries, with C++'s degenerate first entry ["", "") and last entry
// [\xff\xff, \xff\xff\0) (:310-313) implied: index Len is the last one. offset 0
// is the unknown range before entry i; offset 1 the empty range from its begin
// to its first value; 2k+2 the KV of value k; 2k+3 the empty range after it.
type rywCacheCursor struct {
	es     *pseq[cacheEntry]
	ent    pcursor[cacheEntry]
	offset int
	// prevEnd is the end of entry i-1, the begin of the unknown range at
	// offset 0; known says it is set for the current entry.
	prevEnd []byte
	known   bool
}

func (c *rywCacheCursor) clone() rywCacheCursor {
	n := *c
	n.ent = c.ent.clone()
	return n
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

func (c *rywCacheCursor) segments() int { return 2 * (len(c.kvs()) + 1) } // :152

// typ is SnapshotCache::iterator::type (:169-175) in segment types.
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

// beginKey is SnapshotCache.h:184-194.
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

// endKey is SnapshotCache.h:195-202.
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

// inc is operator++ (:208-219): degenerate segments are skipped.
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
		if c.endKey().compare(orig) != 0 {
			return
		}
	}
}

// dec is operator-- (:220-231).
func (c *rywCacheCursor) dec() {
	orig := c.beginKey()
	for {
		c.offset--
		if c.offset < 0 {
			c.ent.prev()
			c.known = false
			c.offset = c.segments() - 1
		}
		if c.beginKey().compare(orig) != 0 {
			return
		}
	}
}

// skip is SnapshotCache::iterator::skip (:236-254): the segment containing key.
func (c *rywCacheCursor) skip(key []byte) {
	if bytes.Equal(key, allKeysEnd) { // :238-242
		c.ent.reset(c.es, c.es.Len())
		c.offset, c.known = 1, false
		return
	}
	// lastLessOrEqual(key) among the entries: the one before the first entry
	// beginning after key; none is the degenerate first entry.
	c.ent.seekFirst(c.es, func(e *cacheEntry) bool { return bytes.Compare(e.begin, key) > 0 })
	c.ent.prev()
	prevEnd := allKeysBegin
	if c.ent.valid() {
		e := c.ent.item()
		if bytes.Compare(key, e.end) < 0 {
			idx := sort.Search(len(e.kvs), func(n int) bool { return bytes.Compare(e.kvs[n].Key, key) >= 0 })
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
