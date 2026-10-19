package client

import (
	"bytes"
	"cmp"
	"sort"
)

// The merged segment view of the keyspace: segment types and the legacy cursor.
// The walk (ryw_walk.go, RFC-262) is C++ getRangeValue / resolveKeySelectorFromCache
// over the same types with the write map's node boundaries (ryw_walk_iter.go); the
// legacy cursor here serves the old range loop until RFC-262 phase 3.
//
// Segment model (mirrors C++ RYWIterator SEGMENT_TYPE): the keyspace [allKeysBegin,
// maxKey) is partitioned into half-open [begin, end) segments, each:
//   - segKV:      exactly one present key (a plain Set, a resolved atomic, or a
//                 snapshot-cache key not shadowed by a clear).
//   - segEmpty:   known to contain no key (a cleared range, a known-absent cache gap,
//                 or — per #234 — a pending versionstamp key, which Go reads as absent).
//   - segUnknown: contents not determinable locally; must read the server.
//   - segPhantom: a matched CompareAndClear — is_kv (COUNTED in the offset walk like segKV)
//                 but with no value (skipped at the landing like segEmpty). See segPhantom.
// type() = the C++ typeMap cross-product of the write-map view and the cache view:
// CLEARED→empty, INDEPENDENT_WRITE(plain Set / folded atomic)→KV, DEPENDENT_WRITE(standalone
// atomic)→KV if the cache base is known else unknown, UNMODIFIED→passthrough cache type,
// versionstamp→empty (the #234 unreadable→absent approximation), matched-CAC→phantom.
//
// getKey is a limit-1 range read (RFC-058): C++ read(GetKeyReq) = getRangeValue /
// getRangeValueBack(limit=1) over the RYWIterator. resolveKeySelectorFromCache counts is_kv
// segments for the offset (a matched CompareAndClear is is_kv → COUNTED), but the range
// iteration returns the first kv()-NON-NULL key, so a phantom is SKIPPED at the landing and
// the selector resolves to the adjacent present key. The rywCache preserves the matched CAC
// as a write-map entry (`absent:true`, never moved to the cleared list — so the conflict map
// still sees it as a DEPENDENT operation), and segTypeAtLocked classifies it segPhantom:
// counted by the offset walk, skipped at the landing, absent in Get/GetRange. An atomic over
// a locally-cleared range is INDEPENDENT in C++ (synthetic SetValue base) and matches Go's
// fold-over-empty; only CompareAndClear yields a phantom (no value).

type rywSegType int

const (
	segUnknown rywSegType = iota
	segEmpty
	segKV
	// segPhantom: a matched CompareAndClear — an is_kv segment in C++ (COUNTED in the
	// getKey offset walk, like segKV) whose resolved value is "no value" (skipped by the
	// limit-1 range read that getKey actually is — RYWIterator::kv() returns nullptr). So a
	// selector COUNTS a phantom but cannot LAND on it: at the resolved landing it is skipped
	// in the resolution direction to the first present (segKV) key — exactly C++ getKey =
	// getRangeValue/getRangeValueBack(limit=1) over RYWIterator (RFC-058).
	segPhantom
	// segUnreadable: the segment holds a pending versionstamped write (sticky
	// entry flag, an unresolved versionstamp chain) or lies inside an SVK
	// candidate stamp range (RFC-098). C++ RYWIterator type()/kv() THROW
	// accessed_unreadable when the walk reaches such a segment
	// (RYWIterator.cpp:45-46/:75-76). BYPASS_UNREADABLE applies only to
	// point reads; selector/range resolution still aborts with 1036.
	segUnreadable
)

// allKeysBegin is the empty key — the absolute start of the keyspace (C++ allKeys.begin).
var allKeysBegin = []byte{}

// segTypeAtLocked computes the merged segment type for the segment beginning at p.
// Caller holds c.mu.
func (c *rywCache) segTypeAtLocked(w *rywWrites, p []byte) rywSegType {
	if w != nil {
		// Unreadable gate (RFC-098): reaching a pending-versionstamp segment or
		// an SVK candidate range during resolution throws in C++ (the cursor
		// classifies it segUnreadable; the walk surfaces 1036). Checked before
		// every other classification, like C++'s type() throw at the top.
		// The entry flag is the single source of truth: atomic() sets it for
		// every versionstamped op, so checking the chain again would be
		// dead code (same gate shape as rywCache.get and GetPipelined).
		if entry, ok := w.writes[string(p)]; ok && entry.unreadable {
			return segUnreadable
		}
		if w.isUnreadable(p) {
			return segUnreadable
		}
		if entry, ok := w.writes[string(p)]; ok {
			// p is a pending write key (single-key segment [p, p+\x00)). getKey classifies
			// by SEGMENT TYPE (C++ RYWIterator type()), NOT by resolved value — the resolved
			// value only matters for Get/GetRange.
			if !entry.hasAtomics {
				if entry.absent {
					// Phantom (matched CompareAndClear): is_kv (counted) but no value
					// (skipped at the landing). See segPhantom.
					return segPhantom
				}
				// A resolved present write slot (plain Set incl. Set-to-empty, or a folded
				// atomic) → is_kv with a value.
				return segKV
			}
			// Unresolved atomic chain: resolve over the storage base for its TYPE.
			base, known := c.serverCache.getKey(p)
			if !known {
				return segUnknown // DEPENDENT_WRITE over unknown base — must read server.
			}
			_, cleared, unresolved := resolveAtomics(base, entry.atomics)
			if unresolved {
				return segUnreadable
			}
			if cleared {
				// DEPENDENT_WRITE over a known base, cleared by a matched CompareAndClear:
				// an is_kv phantom — counted by the offset walk, skipped at the landing.
				return segPhantom
			}
			// DEPENDENT_WRITE over a KNOWN base, present → is_kv with a value.
			return segKV
		}
		if w.isCleared(p) {
			return segEmpty // cleared range — known absent.
		}
		// Unmodified — fall through to the cache view.
	}
	val, known := c.serverCache.getKey(p)
	if !known {
		return segUnknown
	}
	if val != nil {
		return segKV
	}
	return segEmpty
}

// --- Lazy merged-segment cursor (RFC-057) --------------------------------------------
//
// rywSegCursor walks the same merged-segment partition as the (now test-only)
// buildSegmentsLocked materializer, but LAZILY: it computes each segment's [begin, end)
// on demand from the underlying sorted structures (sortedKeys, cleared, snapshotCache)
// via bounded boundary searches, so getKey costs O(walk distance · log N) instead of
// O(N) materialization per call. It mirrors C++ RYWIterator: next()/prev() are a single
// MERGED-boundary skip (not independent per-view bumps), so the two views can't desync.
// Behavior is identical to the materializer (pinned by the equivalence property test).
//
// state: 0 = valid (on a segment), +1 = off the end (>= hi), -1 = off the begin (< allKeysBegin).
type rywSegCursor struct {
	c          *rywCache
	w          *rywWrites // nil: the snapshot cache alone
	hi         []byte
	begin, end []byte
	typ        rywSegType
	state      int8
}

func (c *rywCache) newSegCursor(hi []byte, w *rywWrites) *rywSegCursor {
	return &rywSegCursor{c: c, w: w, hi: hi}
}

func (cur *rywSegCursor) valid() bool    { return cur.state == 0 }
func (cur *rywSegCursor) offEnd() bool   { return cur.state == 1 }
func (cur *rywSegCursor) offBegin() bool { return cur.state == -1 }

// seek positions the cursor on the segment containing key (begin <= key < end). Caller
// holds c.mu.
func (cur *rywSegCursor) seek(key []byte) {
	if bytes.Compare(key, cur.hi) >= 0 {
		cur.state = 1
		return
	}
	if bytes.Compare(key, allKeysBegin) < 0 {
		cur.state = -1
		return
	}
	cur.begin, cur.end = cur.c.mergedBoundsLocked(key, cur.hi, cur.w)
	cur.typ = cur.c.segTypeAtLocked(cur.w, cur.begin)
	cur.state = 0
}

// next advances to the segment beginning at the current endKey (C++ operator++): a
// single skip to the merged boundary, so the view(s) flush with endKey advance and the
// other stays.
func (cur *rywSegCursor) next() { cur.seek(cur.end) }

// prev retreats to the segment ending at the current beginKey (C++ operator--): skip to
// the merged predecessor boundary (largest boundary < begin across both views).
func (cur *rywSegCursor) prev() {
	if bytes.Equal(cur.begin, allKeysBegin) {
		cur.state = -1
		return
	}
	cur.seek(cur.c.prevBoundaryLocked(cur.begin, cur.hi, cur.w))
}

// rywBoundary keeps keyAfter virtual until a selected boundary must escape the search,
// like C++ ExtStringRef (SnapshotCache.h). A successor appends zero; it never increments.
type rywBoundary struct {
	key   []byte
	after bool
}

func (b rywBoundary) size() int {
	if b.after {
		return len(b.key) + 1
	}
	return len(b.key)
}

func (b rywBoundary) compare(other rywBoundary) int {
	n := min(len(b.key), len(other.key))
	if c := bytes.Compare(b.key[:n], other.key[:n]); c != 0 {
		return c
	}
	for _, v := range b.key[n:] {
		if v != 0 {
			return 1
		}
	}
	for _, v := range other.key[n:] {
		if v != 0 {
			return -1
		}
	}
	return cmp.Compare(b.size(), other.size())
}

func (b rywBoundary) materialize() []byte {
	if b.after {
		return keyAfterBytes(b.key)
	}
	return b.key
}

// boundCandidatesLocked visits merged-boundary candidates in a bounded neighborhood of
// p from every source (write keys + their successors, cleared bounds, cache-entry
// bounds, cache keys + their successors), plus allKeysBegin and hi. It must contain the
// floor (largest boundary <= p) and ceil (smallest boundary > p); prevBoundaryLocked
// re-centers on its own argument, so the below-floor case is covered by a separate call.
//
// Why the windows suffice. The merged floor is the MAX of the per-source floors and the
// merged ceil is the MIN of the per-source ceils, so it is enough that each source
// contributes its own floor and ceil; every source can then be argued separately.
//
// Range sources (cleared, unreadableRanges, cache entries) need only two properties:
// sorted, and non-overlapping in the weak sense end_j <= begin_{j+1}. Adjacency
// (end_j == begin_{j+1}) is allowed and is now the common case for cache entries, since
// each fetch leaves its own fragment instead of being merged into its neighbour. Let
// lo = first range with end > p. Then for j < lo: begin_j < end_j <= p, and for j > lo:
// begin_j >= end_{lo} > p. So the boundary multiset splits cleanly around entry lo:
//
//   - Floor (largest boundary <= p): the candidates <= p are all boundaries of ranges
//     below lo, plus begin_lo when begin_lo <= p. The sequence begin_0, end_0, begin_1,
//     end_1, ... is non-decreasing, so the largest among the below-lo boundaries is
//     end_{lo-1} — index lo-1 alone dominates every range beneath it. Floor is therefore
//     max(end_{lo-1}, begin_lo), both inside the window.
//   - Ceil (smallest boundary > p): the candidates > p are end_lo, begin_lo when
//     begin_lo > p, and all boundaries of ranges above lo — but the smallest of those is
//     begin_{lo+1} >= end_lo. So ceil is already in {begin_lo, end_lo}; index lo+1 is
//     slack, not load-bearing.
//
// Nothing above uses coalescing. Merging adjacent ranges would only turn end_j <=
// begin_{j+1} into a strict <, which the argument never appeals to.
//
// Key sources (sortedKeys, and the kvs inside cache entries) reduce to the same shape.
// Within one entry, kvs is sorted, so with ki = first kv >= p the floor contribution is
// kvs[ki-1] or its keyAfter and the ceil contribution is kvs[ki] or keyAfter(kvs[ki-1]);
// the extra -2 index is conservative slack, since keyAfter(kvs[ki-2]) <= kvs[ki-1].
// Across entries, only the windowed ones are scanned, and the rest are dominated by
// bounds the window already contributes: a key k in entry j satisfies begin_j <= k <
// end_j, hence keyAfter(k) <= end_j as well, so for j < lo-1 every contribution is
// <= end_j <= begin_{lo-1} <= p, and for j > lo+1 every contribution is >= begin_j >=
// end_lo > p. Both dominators are added unconditionally by the window.
//
// That domination step is what covers an entry carrying NO kvs — a zero-row fetch now
// leaves an empty fragment of its own rather than being absorbed by a neighbour, so the
// windowed entries can contribute no keys at all while non-empty entries sit outside the
// window. Their keys are still dominated by the windowed entries' bounds, which are
// contributed whether or not the fragment holds rows.
//
// Caller holds c.mu.
func (c *rywCache) boundCandidatesLocked(p, hi []byte, w *rywWrites, visit func(rywBoundary)) {
	addBound := func(b rywBoundary) {
		if b.compare(rywBoundary{key: allKeysBegin}) >= 0 && b.compare(rywBoundary{key: hi}) <= 0 {
			visit(b)
		}
	}
	add := func(k []byte) { addBound(rywBoundary{key: k}) }
	addKey := func(k []byte) { add(k); addBound(rywBoundary{key: k, after: true}) }
	add(allKeysBegin)
	add(hi)
	if w != nil {
		w.ensureSorted()
		i := sort.Search(len(w.sortedKeys), func(i int) bool { return w.sortedKeys[i] >= string(p) })
		for j := i - 2; j <= i+1; j++ {
			if j >= 0 && j < len(w.sortedKeys) {
				addKey([]byte(w.sortedKeys[j]))
			}
		}
		n := len(w.cleared)
		lo := sort.Search(n, func(i int) bool { return bytes.Compare(w.cleared[i].end, p) > 0 })
		for j := lo - 1; j <= lo+1; j++ {
			if j >= 0 && j < n {
				add(w.cleared[j].begin)
				add(w.cleared[j].end)
			}
		}
		// SVK candidate stamp ranges are segments of their own (C++ inserts
		// explicit boundary nodes via addUnmodifiedAndUnreadableRange,
		// WriteMap.cpp:205-242). Without these edges the range's head [begin,
		// entry) — which contains no write-map key — is swallowed into the
		// unknown segment that starts BELOW the range, and a selector walk
		// crosses it where libfdb_c throws (RFC-098). Same sorted +
		// non-overlapping lo-1..lo+1 window argument as cleared above.
		un := len(w.unreadableRanges)
		ulo := sort.Search(un, func(i int) bool { return bytes.Compare(w.unreadableRanges[i].end, p) > 0 })
		for j := ulo - 1; j <= ulo+1; j++ {
			if j >= 0 && j < un {
				add(w.unreadableRanges[j].begin)
				add(w.unreadableRanges[j].end)
			}
		}
	}
	c.cacheCandidatesLocked(p, addBound)
}

// cacheCandidatesLocked is boundCandidatesLocked's snapshot-cache source (entry
// bounds, cache keys and their successors), windowed around p.
func (c *rywCache) cacheCandidatesLocked(p []byte, addBound func(rywBoundary)) {
	add := func(k []byte) { addBound(rywBoundary{key: k}) }
	addKey := func(k []byte) { add(k); addBound(rywBoundary{key: k, after: true}) }
	es := c.serverCache.entries
	n := len(es)
	lo := sort.Search(n, func(i int) bool { return bytes.Compare(es[i].end, p) > 0 })
	for j := lo - 1; j <= lo+1; j++ {
		if j < 0 || j >= n {
			continue
		}
		add(es[j].begin)
		add(es[j].end)
		kvs := es[j].kvs
		ki := sort.Search(len(kvs), func(i int) bool { return bytes.Compare(kvs[i].Key, p) >= 0 })
		for m := ki - 2; m <= ki+1; m++ {
			if m >= 0 && m < len(kvs) {
				addKey(kvs[m].Key)
			}
		}
	}
}

// mergedBoundsLocked returns the merged segment [floor, ceil) containing p:
// floor = largest boundary <= p, ceil = smallest boundary > p. Caller holds c.mu.
func (c *rywCache) mergedBoundsLocked(p, hi []byte, w *rywWrites) (floor, ceil []byte) {
	lo, up := rywBoundary{key: allKeysBegin}, rywBoundary{key: hi}
	probe := rywBoundary{key: p}
	c.boundCandidatesLocked(p, hi, w, func(b rywBoundary) {
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

// prevBoundaryLocked returns the largest merged boundary strictly < x. Caller holds c.mu.
func (c *rywCache) prevBoundaryLocked(x, hi []byte, w *rywWrites) []byte {
	best, probe := rywBoundary{key: allKeysBegin}, rywBoundary{key: x}
	c.boundCandidatesLocked(x, hi, w, func(b rywBoundary) {
		if b.compare(probe) < 0 && b.compare(best) > 0 {
			best = b
		}
	})
	return best.materialize()
}
