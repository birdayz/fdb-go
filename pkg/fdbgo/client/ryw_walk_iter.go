package client

import (
	"bytes"
	"sort"
)

// rywIter is the walk's iterator (RFC-262 D2): C++ RYWIterator over one version
// of the write map (w), or SnapshotCache::iterator when w is nil (A2.2). Its
// segments are C++'s: the union of the write map's node boundaries (D1b) and the
// snapshot cache's boundaries, over ["", \xff\xff), plus the sentinel
// [\xff\xff, \xff\xff\0), which is EMPTY (SnapshotCache.h:308-313, WriteMap.h:129-134).
// It is valid only inside the critical section that created it.
type rywIter struct {
	c          *rywCache
	w          *rywWrites
	begin, end []byte
	typ        rywSegType
	sentinel   bool
	// broken records a step past the sentinel, which the walk never takes (A6
	// stops at L4/L6 first); the walk turns it into internal_error.
	broken bool
}

// sentinelEnd is \xff\xff\0, the end of the last segment.
var sentinelEnd = []byte{0xff, 0xff, 0x00}

func (c *rywCache) newIterLocked(w *rywWrites) *rywIter {
	if w != nil {
		w.nodes.build()
	}
	return &rywIter{c: c, w: w}
}

func (it *rywIter) clone() *rywIter { n := *it; return &n }

// skip is RYWIterator::skip: the segment containing key (RYWIterator.cpp:114-118).
func (it *rywIter) skip(key []byte) {
	if bytes.Compare(key, allKeysEnd) >= 0 {
		it.begin, it.end, it.typ, it.sentinel = allKeysEnd, sentinelEnd, segEmpty, true
		return
	}
	it.sentinel = false
	it.begin, it.end = it.c.walkBoundsLocked(key, it.w)
	it.typ = it.c.segTypeAtLocked(it.w, it.begin)
}

// next is operator++ (RYWIterator.cpp:94-103).
func (it *rywIter) next() {
	if it.sentinel {
		it.broken = true
		return
	}
	it.skip(it.end)
}

// prev is operator-- (RYWIterator.cpp:105-112).
func (it *rywIter) prev() {
	if len(it.begin) == 0 && !it.sentinel {
		it.broken = true
		return
	}
	it.skip(it.c.walkPrevLocked(it.begin, it.w))
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
	return KeyValue{Key: it.begin, Value: it.c.segmentValueLocked(it.w, it.begin)}, true
}

// writeUnmodified is WriteMap::iterator::is_unmodified_range for the write
// segment containing the current key, with its bounds.
func (it *rywIter) writeSegment() (unmodified bool, begin, end []byte) {
	if it.w == nil {
		return true, nil, nil
	}
	if _, op := it.w.writes[string(it.begin)]; op || it.w.isCleared(it.begin) {
		return false, nil, nil
	}
	f, c := it.w.nodes.floorCeil(it.begin)
	return true, f.materialize(), c.materialize()
}

// cacheRun returns the cache entry holding the current KV and its index there.
func (it *rywIter) cacheRun() ([]KeyValue, int, bool) {
	es := it.c.serverCache.entries
	i := sort.Search(len(es), func(i int) bool { return bytes.Compare(es[i].end, it.begin) > 0 })
	if i == len(es) || bytes.Compare(es[i].begin, it.begin) > 0 {
		return nil, 0, false
	}
	kvs := es[i].kvs
	j := sort.Search(len(kvs), func(j int) bool { return bytes.Compare(kvs[j].Key, it.begin) >= 0 })
	if j == len(kvs) || !bytes.Equal(kvs[j].Key, it.begin) {
		return nil, 0, false
	}
	return kvs, j, true
}

// skipContiguous is RYWIterator::skipContiguous (RYWIterator.cpp:128-133) over
// SnapshotCache::iterator::skipContiguous (SnapshotCache.h:276-282): on a cache KV
// under an unmodified write segment, land on the last value of the same cache
// entry that is < min(bound, the write segment's end). It returns the values
// from the current one to the landing, in key order. Elsewhere it returns the
// current KV alone.
func (it *rywIter) skipContiguous(bound []byte) []KeyValue {
	kv, _ := it.kv()
	unmodified, _, wEnd := it.writeSegment()
	if !unmodified {
		return []KeyValue{kv}
	}
	if wEnd != nil && bytes.Compare(wEnd, bound) < 0 {
		bound = wEnd
	}
	kvs, i, ok := it.cacheRun()
	if !ok {
		return []KeyValue{kv}
	}
	// offset = 2 * lower_bound(values[i+1:], bound): the KV just below it.
	j := i + 1 + sort.Search(len(kvs)-i-1, func(n int) bool { return bytes.Compare(kvs[i+1+n].Key, bound) >= 0 })
	run := kvs[i:j]
	if j-1 > i {
		it.skip(kvs[j-1].Key)
	}
	return run
}

// skipContiguousBack is RYWIterator::skipContiguousBack (RYWIterator.cpp:135-140)
// over SnapshotCache.h:283-290: land on the first value of the same entry that
// is >= max(bound, keyAfter(the write segment's begin)). It returns the values
// from the landing to the current one, in key order.
func (it *rywIter) skipContiguousBack(bound []byte) []KeyValue {
	kv, _ := it.kv()
	unmodified, wBegin, _ := it.writeSegment()
	if !unmodified {
		return []KeyValue{kv}
	}
	if wBegin != nil {
		if a := keyAfterBytes(wBegin); bytes.Compare(a, bound) > 0 {
			bound = a
		}
	}
	kvs, i, ok := it.cacheRun()
	if !ok {
		return []KeyValue{kv}
	}
	// offset = 2 * lower_bound(values[:i], bound) + 2.
	j := sort.Search(i, func(n int) bool { return bytes.Compare(kvs[n].Key, bound) >= 0 })
	if j < i {
		it.skip(kvs[j].Key)
	}
	return kvs[j : i+1]
}

// walkCandidatesLocked visits the walk's boundary candidates near p: the write
// map's node boundaries (D1b) instead of the legacy write sources, the cache
// sources of boundCandidatesLocked, and "" and \xff\xff.
func (c *rywCache) walkCandidatesLocked(p []byte, w *rywWrites, visit func(rywBoundary)) {
	hi := rywBoundary{key: allKeysEnd}
	add := func(b rywBoundary) {
		if b.compare(hi) <= 0 {
			visit(b)
		}
	}
	add(rywBoundary{key: allKeysBegin})
	add(hi)
	if w != nil {
		f, ce := w.nodes.floorCeil(p)
		add(f)
		add(ce)
		add(w.nodes.prevBoundary(p))
	}
	c.cacheCandidatesLocked(p, add)
}

// walkBoundsLocked is the walk segment [floor, ceil) containing p < \xff\xff.
func (c *rywCache) walkBoundsLocked(p []byte, w *rywWrites) (floor, ceil []byte) {
	lo, up := rywBoundary{key: allKeysBegin}, rywBoundary{key: allKeysEnd}
	probe := rywBoundary{key: p}
	c.walkCandidatesLocked(p, w, func(b rywBoundary) {
		if b.compare(probe) <= 0 {
			if b.compare(lo) > 0 {
				lo = b
			}
		} else if b.compare(up) < 0 {
			up = b
		}
	})
	return lo.materialize(), up.materialize()
}

// walkPrevLocked is the largest walk boundary < x.
func (c *rywCache) walkPrevLocked(x []byte, w *rywWrites) []byte {
	best, probe := rywBoundary{key: allKeysBegin}, rywBoundary{key: x}
	c.walkCandidatesLocked(x, w, func(b rywBoundary) {
		if b.compare(probe) < 0 && b.compare(best) > 0 {
			best = b
		}
	})
	return best.materialize()
}
