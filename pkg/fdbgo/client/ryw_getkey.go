package client

import (
	"bytes"
	"context"
	"sort"
)

// Read-your-writes key-selector resolution — a faithful port of C++
// resolveKeySelectorFromCache (fdbclient/ReadYourWrites.actor.cpp:409) + the
// getRangeValue unknown-range server-read-then-remerge loop (RFC-056).
//
// The Go client's Transaction.GetKey historically resolved selectors against storage
// only, ignoring the txn's own pending writes — a divergence from libfdb_c, where
// getKey merges the pending writes (the write-map) with the snapshot cache. This file
// closes that gap by walking a merged SEGMENT view of the keyspace, like C++'s
// RYWIterator (a zip of WriteMap::iterator + SnapshotCache::iterator).
//
// Segment model (mirrors C++ RYWIterator SEGMENT_TYPE): the keyspace [allKeysBegin,
// maxKey) is partitioned into half-open [begin, end) segments, each:
//   - segKV:      exactly one present key (a plain Set, a resolved atomic, or a
//                 snapshot-cache key not shadowed by a clear).
//   - segEmpty:   known to contain no key (a cleared range, a known-absent cache gap,
//                 or — per #234 — a pending versionstamp key, which Go reads as absent).
//   - segUnknown: contents not determinable locally; must read the server.
// type() = the C++ typeMap cross-product of the write-map view and the cache view:
// CLEARED→empty, INDEPENDENT_WRITE(plain Set)→KV, DEPENDENT_WRITE(atomic)→KV if the
// cache base is known else unknown, UNMODIFIED→passthrough cache type, versionstamp→
// empty (the #234 unreadable→absent approximation, consistent across Get/GetRange).
//
// DEFERRED (RFC-056 follow-up): a PENDING atomic that resolves to no value — a
// CompareAndClear, or an atomic layered on a locally-cleared range — is modeled here as
// segEmpty (not a key). libfdb_c keeps such a write-map entry as an is_kv "phantom"
// slot that is COUNTED in the offset walk (but yields no value: it never appears in
// GetRange, and getKey resolves THROUGH it to the next valued key). Matching that
// requires the rywCache to preserve every atomic-touched key as a slot, which conflicts
// with its eager value-resolution (the optimization Get/GetRange depend on) — a deeper
// change. The getKey differential is therefore scoped to non-atomic pending writes
// (Set/Clear/ClearRange — the primary divergence); pending-atomic getKey offset
// resolution is tracked in TODO.md under the RFC-056 audit.

type rywSegType int

const (
	segUnknown rywSegType = iota
	segEmpty
	segKV
)

type rywSegment struct {
	begin []byte
	end   []byte // exclusive
	typ   rywSegType
}

// allKeysBegin is the empty key — the absolute start of the keyspace (C++ allKeys.begin).
var allKeysBegin = []byte{}

// buildSegmentsLocked materializes the merged segment partition of [allKeysBegin, hi).
// Caller holds c.mu. includeWrites=false models a snapshot read (C++
// SnapshotCache::iterator only — the write map is bypassed).
//
// The keyspace is tiled contiguously: boundaries are every write-key (and its
// successor), cleared-range bound, snapshot-cache entry bound, and cache key (and its
// successor); the gaps between them inherit their type from segTypeAtLocked. This is
// O(writes + cacheKeys) per resolution attempt — fine for the small per-txn write set
// and the bounded ranges a getKey actually reads; a windowed/lazy iterator (matching
// C++'s laziness) is a perf follow-up if profiling shows it hot.
func (c *rywCache) buildSegmentsLocked(hi []byte, includeWrites bool) []rywSegment {
	var bounds [][]byte
	add := func(b []byte) {
		if bytes.Compare(b, allKeysBegin) >= 0 && bytes.Compare(b, hi) <= 0 {
			bounds = append(bounds, b)
		}
	}
	add(allKeysBegin)
	add(hi)
	if includeWrites {
		c.ensureSortedLocked()
		for _, k := range c.sortedKeys {
			kb := []byte(k)
			add(kb)
			add(keyAfterBytes(kb))
		}
		for _, r := range c.cleared {
			add(r.begin)
			add(r.end)
		}
	}
	for i := range c.serverCache.entries {
		e := &c.serverCache.entries[i]
		add(e.begin)
		add(e.end)
		for _, kv := range e.kvs {
			add(kv.Key)
			add(keyAfterBytes(kv.Key))
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bytes.Compare(bounds[i], bounds[j]) < 0 })
	// Dedupe in place.
	uniq := bounds[:0]
	var last []byte
	for _, b := range bounds {
		if len(uniq) == 0 || !bytes.Equal(b, last) {
			uniq = append(uniq, b)
			last = b
		}
	}
	segs := make([]rywSegment, 0, len(uniq))
	for i := 0; i+1 < len(uniq); i++ {
		segs = append(segs, rywSegment{
			begin: uniq[i],
			end:   uniq[i+1],
			typ:   c.segTypeAtLocked(uniq[i], includeWrites),
		})
	}
	return segs
}

// segTypeAtLocked computes the merged segment type for the segment beginning at p.
// Caller holds c.mu.
func (c *rywCache) segTypeAtLocked(p []byte, includeWrites bool) rywSegType {
	if includeWrites {
		if entry, ok := c.writes[string(p)]; ok {
			// p is a pending write key (single-key segment [p, p+\x00)).
			if !entry.hasAtomics {
				return segKV // plain Set — present regardless of value (incl. Set-to-empty).
			}
			// Atomic chain: resolve over the storage base.
			base, known := c.serverCache.getKey(p)
			if !known {
				return segUnknown // DEPENDENT_WRITE over unknown base — must read server.
			}
			_, cleared, unresolved := resolveAtomics(base, entry.atomics)
			if cleared || unresolved {
				// CompareAndClear matched (no value), or a versionstamp (unreadable →
				// absent per #234). Modeled as absent here (see DEFERRED note above).
				return segEmpty
			}
			return segKV
		}
		if c.isClearedLocked(p) {
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

// segIdxContaining returns the index of the segment [begin, end) containing key
// (begin <= key < end), or len(segs) if key >= the last segment's end.
func segIdxContaining(segs []rywSegment, key []byte) int {
	return sort.Search(len(segs), func(i int) bool {
		return bytes.Compare(segs[i].end, key) > 0
	})
}

// keySelResult is the outcome of resolveKeySelectorFromCache.
type keySelResult struct {
	key            []byte // the transformed firstGreaterOrEqual key (resolved, or adjoining the stop)
	offset         int32
	readToBegin    bool
	readThroughEnd bool
	stoppedUnknown bool   // walk halted on an unknown segment (need a server read)
	unknownBegin   []byte // begin of that unknown segment (server-read lower bound)
	unknownEnd     []byte // end of that unknown segment (server-read upper bound)
}

// resolveKeySelectorFromCache is the faithful port of ReadYourWrites.actor.cpp:409.
// It transforms (key, orEqual, offset) toward firstGreaterOrEqual form (offset→1) by
// stepping over KNOWN segments, sets readToBegin/readThroughEnd if it walks off the
// ends of fully-known data, or stops at an unknown segment (leaving key as an
// equivalent FGE selector adjoining it). Versionstamp keys are segEmpty (absent), so
// there is no "unreadable stop" — the only halt is an unknown segment.
func resolveKeySelectorFromCache(key []byte, orEqual bool, offset int32, segs []rywSegment, maxKey []byte) keySelResult {
	// removeOrEqual: if orEqual, key = keyAfter(key); orEqual = false.
	if orEqual {
		key = keyAfterBytes(key)
		orEqual = false
	}

	i := segIdxContaining(segs, key)
	if i >= len(segs) {
		// key is at/after maxKey — off the end.
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}

	// if offset <= 0 && it.beginKey() == key && key != allKeysBegin: --it
	if offset <= 0 && bytes.Equal(segs[i].begin, key) && !bytes.Equal(key, allKeysBegin) && i > 0 {
		i--
	}

	keykey := key

	// Forward walk toward FGE form.
	for offset > 1 && segs[i].typ != segUnknown && bytes.Compare(segs[i].end, maxKey) < 0 {
		if segs[i].typ == segKV {
			offset--
		}
		i++
		if i >= len(segs) {
			break
		}
		keykey = segs[i].begin
	}
	// Backward walk.
	for offset < 1 && i >= 0 && segs[i].typ != segUnknown && !bytes.Equal(segs[i].begin, allKeysBegin) {
		if segs[i].typ == segKV {
			offset++
			if offset == 1 {
				keykey = segs[i].begin
				break
			}
		}
		i--
		if i < 0 {
			break
		}
		keykey = segs[i].end
	}

	// Terminal clamps — only valid on fully-known data (not an unknown stop).
	known := i >= 0 && i < len(segs) && segs[i].typ != segUnknown
	if known && offset < 1 {
		return keySelResult{key: allKeysBegin, offset: 1, readToBegin: true}
	}
	if known && offset > 1 {
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}

	// Skip known empty ranges forward to the first present key. (The backward walk above
	// already lands a backward selector on its KV, so a forward-only skip suffices for
	// non-atomic pending — the scoped axis; see the DEFERRED note for pending atomics.)
	for i >= 0 && i < len(segs) && segs[i].typ == segEmpty && bytes.Compare(segs[i].end, maxKey) < 0 {
		i++
		if i >= len(segs) {
			break
		}
		keykey = segs[i].begin
	}

	if i < 0 || i >= len(segs) {
		// Walked off the end of known data → read through end.
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}
	switch segs[i].typ {
	case segUnknown:
		// Read the FULL unknown segment [begin, end) — the FGE-form key may sit at the
		// segment's END (backward resolution), so reading [key, end) could be empty.
		return keySelResult{
			key: keykey, offset: offset, stoppedUnknown: true,
			unknownBegin: segs[i].begin, unknownEnd: segs[i].end,
		}
	case segKV:
		// Resolved on a present key.
		return keySelResult{key: keykey, offset: offset}
	default: // segEmpty at the maxKey edge — no present key >= the selector.
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}
}

// errGetKeyRYWLoop is a defensive backstop — the remerge loop always terminates
// because each server read strictly shrinks the unknown set.
var errGetKeyRYWLoop = &keySelLoopError{}

type keySelLoopError struct{}

func (*keySelLoopError) Error() string {
	return "getKeyRYW: resolution did not converge (unknown set failed to shrink)"
}

// getKeyRYW resolves a key selector against the read-your-writes view (pending writes
// merged with the snapshot cache), reading the server only for the unresolved tail —
// the faithful libfdb_c behavior. serverGetRange MUST be the RAW storage range read
// (no read conflicts, no RYW merge): the snapshot cache may only ever hold storage@V
// bytes. includeWrites=false models a snapshot read with the write map bypassed.
//
// Returns the resolved key, or allKeysBegin / maxKey for selectors that walk off the
// ends of the keyspace (C++ readToBegin / readThroughEnd).
func (c *rywCache) getKeyRYW(
	ctx context.Context,
	selectorKey []byte, orEqual bool, offset int32,
	maxKey []byte, includeWrites bool,
	serverGetRange func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error),
) ([]byte, error) {
	key := append([]byte(nil), selectorKey...)
	// Resolution direction is fixed by the ORIGINAL selector: offset<=0 → backward
	// (lastLess*), offset>0 → forward (firstGreater*). It anchors the unknown-tail
	// server read NEAR the selector in that direction — never from the segment start.
	backward := offset <= 0
	const fillBatch = 256
	for iter := 0; iter < 1<<20; iter++ {
		c.mu.Lock()
		segs := c.buildSegmentsLocked(maxKey, includeWrites)
		res := resolveKeySelectorFromCache(key, orEqual, offset, segs, maxKey)
		c.mu.Unlock()

		orEqual = false // removeOrEqual consumed it; re-resolve continues in FGE form.
		key = res.key
		offset = res.offset

		switch {
		case res.readToBegin:
			return append([]byte(nil), allKeysBegin...), nil
		case res.readThroughEnd:
			return append([]byte(nil), maxKey...), nil
		case !res.stoppedUnknown:
			return append([]byte(nil), res.key...), nil
		}

		// Unknown segment: a BOUNDED raw server read anchored at the resolved (FGE-form)
		// selector position res.key — NOT from res.unknownBegin, which may be "" on an
		// empty cache (reading from there would scan the whole keyspace up to the
		// selector — a severe regression vs the old bounded getKey). Mirrors C++
		// getRangeValue (forward, read_begin = transformed selector) / getRangeValueBack
		// (backward, reverse read below the selector).
		var kvs []KeyValue
		var more bool
		var err error
		var insBegin, insEnd []byte
		if backward {
			kvs, more, err = serverGetRange(ctx, res.unknownBegin, res.key, fillBatch, true)
			if err != nil {
				return nil, err
			}
			// Reverse read returns descending keys; normalize to ascending for the cache.
			for l, r := 0, len(kvs)-1; l < r; l, r = l+1, r-1 {
				kvs[l], kvs[r] = kvs[r], kvs[l]
			}
			insEnd = res.key
			insBegin = res.unknownBegin
			if more && len(kvs) > 0 {
				insBegin = kvs[0].Key // truncated below the smallest returned key — stays unknown
			}
		} else {
			kvs, more, err = serverGetRange(ctx, res.key, res.unknownEnd, fillBatch, false)
			if err != nil {
				return nil, err
			}
			insBegin = res.key
			insEnd = res.unknownEnd
			if more && len(kvs) > 0 {
				insEnd = keyAfterBytes(kvs[len(kvs)-1].Key)
			}
		}
		if bytes.Compare(insBegin, insEnd) >= 0 {
			// Empty read window: res.key sits at the segment edge, so there is no key to
			// fetch in the resolution direction within this segment. Terminal — nothing
			// further to resolve (readToBegin backward / readThroughEnd forward).
			if backward {
				return append([]byte(nil), allKeysBegin...), nil
			}
			return append([]byte(nil), maxKey...), nil
		}
		c.mu.Lock()
		c.serverCache.insert(insBegin, insEnd, kvs)
		c.mu.Unlock()
	}
	return nil, errGetKeyRYWLoop
}
