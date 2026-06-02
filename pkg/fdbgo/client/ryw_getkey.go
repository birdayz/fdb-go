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
)

// allKeysBegin is the empty key — the absolute start of the keyspace (C++ allKeys.begin).
var allKeysBegin = []byte{}

// segTypeAtLocked computes the merged segment type for the segment beginning at p.
// Caller holds c.mu.
func (c *rywCache) segTypeAtLocked(p []byte, includeWrites bool) rywSegType {
	if includeWrites {
		if entry, ok := c.writes[string(p)]; ok {
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
				// Versionstamp in the chain: unreadable client-side, folded to absent per
				// #234 (a distinct axis, deliberately out of RFC-058 scope).
				return segEmpty
			}
			if cleared {
				// DEPENDENT_WRITE over a known base, cleared by a matched CompareAndClear:
				// an is_kv phantom — counted by the offset walk, skipped at the landing.
				return segPhantom
			}
			// DEPENDENT_WRITE over a KNOWN base, present → is_kv with a value.
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
	c             *rywCache
	includeWrites bool
	hi            []byte
	begin, end    []byte
	typ           rywSegType
	state         int8
}

func (c *rywCache) newSegCursor(hi []byte, includeWrites bool) *rywSegCursor {
	return &rywSegCursor{c: c, includeWrites: includeWrites, hi: hi}
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
	cur.begin, cur.end = cur.c.mergedBoundsLocked(key, cur.hi, cur.includeWrites)
	cur.typ = cur.c.segTypeAtLocked(cur.begin, cur.includeWrites)
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
	cur.seek(cur.c.prevBoundaryLocked(cur.begin, cur.hi, cur.includeWrites))
}

// boundCandidatesLocked gathers merged-boundary candidates in a bounded neighborhood of
// p from every source (write keys + their successors, cleared bounds, cache-entry
// bounds, cache keys + their successors), plus allKeysBegin and hi. It must contain the
// floor (largest boundary <= p) and ceil (smallest boundary > p); prevBoundaryLocked
// re-centers on its own argument, so the below-floor case is covered by a separate call.
//
// Why the windows suffice: cleared ranges and cache entries are sorted, non-overlapping
// AND coalesced, so the only boundaries that can be floor/ceil are at lo-1 / lo / lo+1
// (where lo = first range with end > p) — the [lo-1, lo+1] window is load-bearing and
// exactly sufficient. For the key sources (sortedKeys, cache kvs) the floor/ceil come
// from the keys at Search(>=p) and Search(>=p)-1 plus their keyAfter successors; the
// extra -2 index is conservative slack (the Search-2 successor is always dominated by
// Search-1's contributions), kept as a cheap margin. Caller holds c.mu.
func (c *rywCache) boundCandidatesLocked(p, hi []byte, includeWrites bool) [][]byte {
	var cands [][]byte
	add := func(b []byte) {
		if bytes.Compare(b, allKeysBegin) >= 0 && bytes.Compare(b, hi) <= 0 {
			cands = append(cands, b)
		}
	}
	addKey := func(k []byte) { add(k); add(keyAfterBytes(k)) }
	add(allKeysBegin)
	add(hi)
	if includeWrites {
		c.ensureSortedLocked()
		i := sort.SearchStrings(c.sortedKeys, string(p)) // first key >= p
		for j := i - 2; j <= i+1; j++ {
			if j >= 0 && j < len(c.sortedKeys) {
				addKey([]byte(c.sortedKeys[j]))
			}
		}
		n := len(c.cleared)
		lo := sort.Search(n, func(i int) bool { return bytes.Compare(c.cleared[i].end, p) > 0 })
		for j := lo - 1; j <= lo+1; j++ {
			if j >= 0 && j < n {
				add(c.cleared[j].begin)
				add(c.cleared[j].end)
			}
		}
	}
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
	return cands
}

// mergedBoundsLocked returns the merged segment [floor, ceil) containing p:
// floor = largest boundary <= p, ceil = smallest boundary > p. Caller holds c.mu.
func (c *rywCache) mergedBoundsLocked(p, hi []byte, includeWrites bool) (floor, ceil []byte) {
	floor, ceil = allKeysBegin, hi
	for _, b := range c.boundCandidatesLocked(p, hi, includeWrites) {
		if bytes.Compare(b, p) <= 0 {
			if bytes.Compare(b, floor) > 0 {
				floor = b
			}
		} else if bytes.Compare(b, ceil) < 0 {
			ceil = b
		}
	}
	return floor, ceil
}

// prevBoundaryLocked returns the largest merged boundary strictly < x. Caller holds c.mu.
func (c *rywCache) prevBoundaryLocked(x, hi []byte, includeWrites bool) []byte {
	best := allKeysBegin
	for _, b := range c.boundCandidatesLocked(x, hi, includeWrites) {
		if bytes.Compare(b, x) < 0 && bytes.Compare(b, best) > 0 {
			best = b
		}
	}
	return best
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
func resolveKeySelectorFromCache(cur *rywSegCursor, key []byte, orEqual bool, offset int32, maxKey []byte, backward bool) keySelResult {
	// removeOrEqual: if orEqual, key = keyAfter(key); orEqual = false.
	if orEqual {
		key = keyAfterBytes(key)
		orEqual = false
	}

	cur.seek(key)
	if cur.offEnd() {
		// key is at/after maxKey — off the end.
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}

	// if offset <= 0 && it.beginKey() == key && key != allKeysBegin: --it
	if offset <= 0 && bytes.Equal(cur.begin, key) && !bytes.Equal(key, allKeysBegin) {
		cur.prev()
	}

	keykey := key

	// Forward walk toward FGE form. A phantom (matched CompareAndClear) is is_kv → it
	// COUNTS toward the offset, exactly like a present key (C++ resolveKeySelectorFromCache
	// counts it.is_kv(), which is true for a CAC-cleared DEPENDENT/INDEPENDENT segment).
	for offset > 1 && cur.valid() && cur.typ != segUnknown && bytes.Compare(cur.end, maxKey) < 0 {
		if cur.typ == segKV || cur.typ == segPhantom {
			offset--
		}
		cur.next()
		if cur.offEnd() {
			break
		}
		keykey = cur.begin
	}
	// Backward walk. Phantoms count here too (C++ lands on an is_kv phantom; the directional
	// skip below moves off it to the first present key).
	for offset < 1 && cur.valid() && cur.typ != segUnknown && !bytes.Equal(cur.begin, allKeysBegin) {
		if cur.typ == segKV || cur.typ == segPhantom {
			offset++
			if offset == 1 {
				keykey = cur.begin
				break
			}
		}
		cur.prev()
		if cur.offBegin() {
			break
		}
		keykey = cur.end
	}

	// Terminal clamps — only valid on fully-known data (not an unknown stop).
	known := cur.valid() && cur.typ != segUnknown
	if known && offset < 1 {
		return keySelResult{key: allKeysBegin, offset: 1, readToBegin: true}
	}
	if known && offset > 1 {
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}

	// Directional skip-to-present: getKey is a limit-1 range read (C++ getRangeValue /
	// getRangeValueBack over RYWIterator), so the resolved key is the first PRESENT (segKV)
	// key from the landing in the RESOLUTION direction — segEmpty and segPhantom (matched
	// CompareAndClear: is_kv but no value, RYWIterator::kv()==nullptr) are skipped. Forward
	// for offset>0 selectors, backward for offset<=0. (Without phantoms a backward selector
	// already lands on its segKV, so this loop is a no-op there — preserving prior behavior.)
	if backward {
		for cur.valid() && (cur.typ == segEmpty || cur.typ == segPhantom) {
			if bytes.Equal(cur.begin, allKeysBegin) {
				return keySelResult{key: allKeysBegin, offset: 1, readToBegin: true}
			}
			cur.prev()
			if cur.offBegin() {
				return keySelResult{key: allKeysBegin, offset: 1, readToBegin: true}
			}
			// Track the FGE-form key adjoining the new segment, ONLY for segments this skip
			// reached (a no-skip landing keeps the offset walk's keykey). For a present KV the
			// resolved key is its begin; for an UNKNOWN segment the BACKWARD server read window
			// is [unknownBegin, res.key), so res.key must be the segment END — else the window
			// is empty [begin,begin) and getKey wrongly returns allKeysBegin without reading the
			// preceding storage (codex P2-1). Mirrors the backward offset walk.
			if cur.typ == segUnknown {
				keykey = cur.end
			} else {
				keykey = cur.begin
			}
		}
	} else {
		for cur.valid() && (cur.typ == segEmpty || cur.typ == segPhantom) {
			if bytes.Compare(cur.end, maxKey) >= 0 {
				return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
			}
			cur.next()
			if cur.offEnd() {
				return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
			}
			keykey = cur.begin
		}
	}

	if !cur.valid() {
		// Walked off the end of known data.
		if backward {
			return keySelResult{key: allKeysBegin, offset: 1, readToBegin: true}
		}
		return keySelResult{key: maxKey, offset: 1, readThroughEnd: true}
	}
	switch cur.typ {
	case segUnknown:
		// Read the FULL unknown segment [begin, end) — the FGE-form key may sit at the
		// segment's END (backward resolution), so reading [key, end) could be empty.
		return keySelResult{
			key: keykey, offset: offset, stoppedUnknown: true,
			unknownBegin: cur.begin, unknownEnd: cur.end,
		}
	case segKV:
		// Resolved on a present key.
		return keySelResult{key: keykey, offset: offset}
	default: // segEmpty/segPhantom at the keyspace edge — no present key in direction.
		// (Unreachable: the directional skip above exits only on segUnknown/segKV or a
		// terminal return. Direction-correct anyway as a defensive backstop.)
		if backward {
			return keySelResult{key: allKeysBegin, offset: 1, readToBegin: true}
		}
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
		cur := c.newSegCursor(maxKey, includeWrites)
		res := resolveKeySelectorFromCache(cur, key, orEqual, offset, maxKey, backward)
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
