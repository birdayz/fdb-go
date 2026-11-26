package client

import (
	"bytes"
	"context"

	"fdb.dev/internal/fdbwire"
)

// A storage reply's more flag can be true even when its known span covers the
// requested endpoint. Re-walk the cached iterator to determine RYW completion.
// w is the write-map version to merge (nil: the snapshot cache alone).
func (c *rywCache) localRange(w *rywWrites, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localRangeLocked(w, begin, end, limit, byteTarget, reverse)
}

// localRangeLocked follows C++ getRangeValue's merged iterator before any
// NativeAPI read. A byte-limited page can end at unknown state after any row.
func (c *rywCache) localRangeLocked(w *rywWrites, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, bool, error) {
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, true, nil
	}
	cur := c.newSegCursor(end, w)
	if reverse {
		cur.seek(c.prevBoundaryLocked(end, end, w))
	} else {
		cur.seek(begin)
	}
	var rows []KeyValue
	remainingBytes := byteTarget
	for cur.valid() && bytes.Compare(cur.end, begin) > 0 {
		switch cur.typ {
		case segUnknown:
			if byteTarget > 0 && len(rows) > 0 {
				return rows, true, true, nil
			}
			return nil, false, false, nil
		case segUnreadable:
			return nil, false, true, &wire.FDBError{Code: ErrAccessedUnreadable}
		case segKV:
			value := c.segmentValueLocked(w, cur.begin)
			rows = append(rows, KeyValue{Key: bytes.Clone(cur.begin), Value: value})
			if byteTarget > 0 {
				// NativeAPI GetRangeLimits::decrement charges eight bytes per row
				// in addition to the key and value, including present-empty values.
				remainingBytes -= 8 + len(cur.begin) + len(value)
			}
			if (limit > 0 && len(rows) >= limit) || (byteTarget > 0 && remainingBytes <= 0) {
				return rows, true, true, nil
			}
		}
		if reverse {
			cur.prev()
		} else {
			cur.next()
		}
	}
	return rows, false, true, nil
}

// segmentValueLocked is RYWIterator::kv for a segment already classified segKV.
// Resolve against cached storage without rewriting dependent/unreadable flags.
func (c *rywCache) segmentValueLocked(w *rywWrites, key []byte) []byte {
	return segmentValueAt(&c.serverCache.entries, w, key)
}

// segmentValueAt is segmentValueLocked over a cache snapshot.
func segmentValueAt(cached *pseq[cacheEntry], w *rywWrites, key []byte) []byte {
	base, _ := cacheGetKey(cached, key)
	if w == nil {
		return base
	}
	if entry, ok := w.entryOf(key); ok {
		if !entry.hasAtomics {
			return entry.value
		}
		value, _, _ := resolveAtomics(base, entry.atomics)
		return value
	}
	return base
}

// fetchOrCached checks the snapshot cache before making a server call.
// If the range is fully cached, returns cached KVs (in scan direction).
// Otherwise fetches from server and caches the result.
func (c *rywCache) fetchOrCached(
	ctx context.Context,
	begin, end []byte,
	limit int,
	byteTarget int,
	reverse bool,
	serverGetRange rangeServerFunc,
) ([]KeyValue, bool, error) {
	c.mu.Lock()
	cachedKVs, fullyKnown := c.serverCache.getRangeKVsLimited(begin, end, cacheWalkBudget(limit), reverse)
	c.mu.Unlock()

	if fullyKnown {
		kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
		more := computeMore(cachedKVs, limit)
		return kvs, more, nil
	}
	return c.fetchServer(ctx, begin, end, limit, byteTarget, reverse, serverGetRange)
}

// fetchServer reads storage past any range the snapshot cache knows is empty at the scan
// origin, where C++'s RYW iterator starts its storage read (ReadYourWrites.actor.cpp:487-508,
// 760-764), and caches the reply up to its readThrough.
func (c *rywCache) fetchServer(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool, serverGetRange rangeServerFunc) ([]KeyValue, bool, error) {
	c.mu.Lock()
	begin, end = c.serverCache.skipKnownEmpty(begin, end, reverse)
	c.mu.Unlock()
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, nil
	}
	kvs, more, readThrough, err := serverGetRange(ctx, begin, end, limit, byteTarget, reverse)
	if err != nil {
		return nil, false, err
	}
	c.cacheServerReply(begin, end, kvs, more, reverse, readThrough)
	return kvs, more, nil
}

// cacheServerResult inserts a server getRange result into the snapshot cache.
//
// serverMore with ZERO rows would over-claim: the narrowing below is what keeps
// a truncated fetch from asserting knowledge past its last returned key, and
// with no rows there is nothing to narrow to, so [fetchBegin, fetchEnd) would be
// marked entirely known-empty. insert then erases every fragment strictly inside
// it and real cached rows are lost — silently, since the invariant still holds
// and later reads just see a known-empty range.
//
// The read path cannot produce that pair. rangeScanImpl returns more=true in
// four places: the two soft byte stops (readpath.go:794 and :916), which
// require len(allKVs) > 0; isReached after a reply's decrement (:891); and the
// final `limits.rows == 0` (:957). The last two need a limit that was not
// reached on entry to reach zero, and decrement lowers rows and minRows only by
// rows appended — the RYW range read's limits come from newRangeLimits
// (readpath.go:642), which gives rows >= 1 (0 becomes unlimited; callers reject
// rows below -1) and minRows = 1, and a byte limit alone is not reached until
// minRows is. So more=true implies rows were returned. The zero-row break at
// readpath.go:908 is a different guard: it stops a misbehaving storage server
// that reports more=true with an empty batch from reaching the narrowing at
// :921, which would index kvs[len(kvs)-1] on an empty slice. It bounds the
// inner re-query loop; it is not what establishes the invariant relied on here.
func (c *rywCache) cacheServerResult(fetchBegin, fetchEnd []byte, serverKVs []KeyValue, serverMore bool, reverse bool) {
	c.cacheServerReply(fetchBegin, fetchEnd, serverKVs, serverMore, reverse, nil)
}

// cacheServerReply is cacheServerResult knowing also through readThrough, as C++
// getKnownKeyRange extends a reply's known range (ReadYourWrites.actor.cpp:496-499).
func (c *rywCache) cacheServerReply(fetchBegin, fetchEnd []byte, serverKVs []KeyValue, serverMore bool, reverse bool, readThrough []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheBegin := fetchBegin
	cacheEnd := fetchEnd
	var cacheKVs []KeyValue

	if reverse {
		if serverMore && len(serverKVs) > 0 {
			// Reverse scan: last element is the smallest key returned.
			// Known range is [lastKey, fetchEnd).
			cacheBegin = serverKVs[len(serverKVs)-1].Key
		}
		if readThrough != nil && bytes.Compare(readThrough, cacheBegin) < 0 {
			cacheBegin = readThrough
		}
		// Store in forward order.
		cacheKVs = make([]KeyValue, len(serverKVs))
		for i, kv := range serverKVs {
			cacheKVs[len(serverKVs)-1-i] = kv
		}
	} else {
		if serverMore && len(serverKVs) > 0 {
			// Forward scan: known range is [fetchBegin, keyAfter(lastKey)).
			lastKey := serverKVs[len(serverKVs)-1].Key
			cacheEnd = append(append([]byte(nil), lastKey...), 0)
		}
		if readThrough != nil && bytes.Compare(readThrough, cacheEnd) > 0 {
			cacheEnd = readThrough
		}
		// insert retains its own descriptors; reply bytes remain immutable internally.
		cacheKVs = serverKVs
	}

	c.serverCache.insert(cacheBegin, cacheEnd, cacheKVs)
}

// applyLimitAndDirection returns KVs with limit and direction applied.
// Input KVs must be in ascending order.
func applyLimitAndDirection(kvs []KeyValue, limit int, reverse bool) []KeyValue {
	if reverse {
		// Reverse the order.
		out := make([]KeyValue, len(kvs))
		for i, kv := range kvs {
			out[len(kvs)-1-i] = kv
		}
		kvs = out
	}
	if limit > 0 && len(kvs) > limit {
		kvs = kvs[:limit]
	}
	return kvs
}

// computeMore returns true if applying the limit would leave remaining KVs.
func computeMore(kvs []KeyValue, limit int) bool {
	return limit > 0 && len(kvs) > limit
}

// cacheWalkBudget is how many cached KVs a snapshot-cache walk must produce before the
// answer to a `limit`-row read is final: one more than the limit. The limit itself
// supplies the page; the extra row is what computeMore's `len(kvs) > limit` reads to
// decide `more`, so stopping at exactly `limit` would report more=false on a range that
// has further rows. limit <= 0 is unlimited and cannot be bounded.
func cacheWalkBudget(limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit + 1
}

// limitReached ports C++ GetRangeLimits::isReached() for a row-only limit: the
// requested row limit was fully consumed (rows==0). FDB forces more=true in this
// case — ReadYourWrites.actor.cpp:799 `result.more = result.more || limits.isReached()` —
// the canonical "the limit was the stop reason" contract that continuation-based
// iteration relies on, INDEPENDENT of whether further data actually exists. `returned`
// is the count after the limit was applied, so returned==limit ⟺ the limit was reached.
// (Go's merge already clamps results to [begin,end), so C++'s subsequent
// resize-clears-more branch — which only fires for items BEYOND end — never applies here.)
func limitReached(limit, returned int) bool {
	return limit > 0 && returned >= limit
}

// mergeBatch merges a batch of server results with local writes and clears.
// boundary is the knowledge boundary (last fetched key); nil means the entire
// range is known. Returns sorted key-value pairs.
//
// Uses sorted write keys + two-pointer merge for O(k + S) instead of the
// previous O(W + S log S) where W = total writes, k = writes in range, S = server results.
func (c *rywCache) mergeBatch(
	w *rywWrites,
	serverKVs []KeyValue,
	rangeBegin, rangeEnd []byte,
	boundary []byte,
	reverse bool,
) []KeyValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mergeBatchLocked(w, serverKVs, rangeBegin, rangeEnd, boundary, reverse)
}

// It stores resolved atomics into w, so w must be a private span copy
// (captureWriteSpanLocked): never the live version and never a pinned one (RFC-262 D1).
func (c *rywCache) mergeBatchLocked(
	w *rywWrites,
	serverKVs []KeyValue,
	rangeBegin, rangeEnd []byte,
	boundary []byte,
	reverse bool,
) []KeyValue {
	if w == c.w || w.pins != 0 {
		panic("rywCache.mergeBatchLocked: the write-map version is shared (live or pinned)")
	}
	w.ensureSorted()

	// Phase 1: Filter server results — remove cleared keys.
	// Server results are already sorted in scan direction.
	filteredServer := make([]KeyValue, 0, len(serverKVs))
	for _, kv := range serverKVs {
		if !w.isCleared(kv.Key) {
			filteredServer = append(filteredServer, kv)
		}
	}

	// Phase 2: Find write keys in the effective range, resolve atomics, and collect sorted write KVs.
	effectiveBegin, effectiveEnd := mergeEffectiveBounds(rangeBegin, rangeEnd, boundary, reverse)
	writeKVs, atomicCleared := collectMergeWrites(w, serverKVs, effectiveBegin, effectiveEnd)

	// Phase 3: Two-pointer merge.
	return mergeServerAndWriteKVs(filteredServer, writeKVs, atomicCleared, reverse)
}

// mergeEffectiveBounds computes [effectiveBegin, effectiveEnd) for writes safe to include:
// for forward scans [rangeBegin, boundary] (inclusive); for reverse scans [boundary, rangeEnd).
func mergeEffectiveBounds(rangeBegin, rangeEnd, boundary []byte, reverse bool) (string, string) {
	effectiveBegin := string(rangeBegin)
	effectiveEnd := string(rangeEnd)

	if boundary != nil {
		if reverse {
			// Include writes >= boundary.
			if string(boundary) > effectiveBegin {
				effectiveBegin = string(boundary)
			}
		} else {
			// Include writes <= boundary. Use boundary+"\x00" as exclusive end
			// so sort.SearchStrings returns an index that includes boundary itself
			// (the boundary key is the last fetched server key — safe to include).
			boundaryAfter := string(append(append([]byte(nil), boundary...), 0))
			if boundaryAfter < effectiveEnd {
				effectiveEnd = boundaryAfter
			}
		}
	}
	return effectiveBegin, effectiveEnd
}

func collectMergeWrites(w *rywWrites, serverKVs []KeyValue, effectiveBegin, effectiveEnd string) ([]KeyValue, map[string]bool) {
	// Build server key lookup only if needed for atomic resolution.
	// Most Record Layer transactions have no atomics — skip the map allocation.
	var serverValues map[string][]byte
	// atomicCleared tracks keys where atomic resolution resulted in deletion.
	// These must be excluded from filteredServer during the merge phase,
	// because the atomic was resolved AFTER building filteredServer.
	var atomicCleared map[string]bool

	wStart := w.writeIndex(effectiveBegin)
	wEnd := w.writeIndex(effectiveEnd)
	// The entries are copied out first: resolving an atomic stores into w.
	inRange := make([]rywWrite, 0, max(wEnd-wStart, 0))
	for cur := w.sorted.finger(wStart); cur.idx < wEnd; cur.next() {
		inRange = append(inRange, *cur.item())
	}

	// Process writes in range: resolve atomics, collect into sorted slice.
	writeKVs := make([]KeyValue, 0, len(inRange))
	for _, x := range inRange {
		k, entry := x.key, x.e
		if entry.absent {
			// Phantom slot (a matched CompareAndClear, RFC-058): an is_kv slot for getKey
			// but ABSENT in a range read. Skip it from writeKVs AND shadow any server-present
			// value at this key (the local clear wins) via atomicCleared — else this
			// pre-existing absent entry would fall through to the plain-entry branch below
			// and be emitted as a phantom KV.
			if atomicCleared == nil {
				atomicCleared = make(map[string]bool)
			}
			atomicCleared[k] = true
			continue
		}
		if entry.hasAtomics {
			// Lazily build server values map on first atomic encounter.
			if serverValues == nil {
				serverValues = make(map[string][]byte, len(serverKVs))
				for _, kv := range serverKVs {
					serverValues[string(kv.Key)] = kv.Value
				}
			}
			// Resolve atomics against server base. nil = absent, non-nil = present
			// (incl. present-empty) — mirroring C++ Optional<ValueRef> so V2 ops
			// (MinV2/AndV2) distinguish "missing → operand" from "present-empty → op".
			base, basePresent := serverValues[k]
			if basePresent && base == nil {
				base = []byte{} // present-empty storage value — keep non-nil
			}
			base, cleared, unresolved := resolveAtomics(base, entry.atomics)
			// Cache resolved value.
			if unresolved {
				// Unreadable entries are outside the capped scan window. Never
				// expose their operands: BYPASS_UNREADABLE applies only to points.
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
			} else if cleared {
				// Site E: a standalone CompareAndClear matched. Cache as a DEPENDENT phantom
				// (absent+dependent) — an is_kv slot for getKey that reads ABSENT here — NOT
				// a cleared range (RFC-058). Still exclude the server entry from this merge
				// (filteredServer was built before atomic resolution) via atomicCleared.
				w.putEntry(k, rywEntry{absent: true, dependent: true})
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
				// The key stays in the write map (now a phantom); it is not removed.
			} else if base == nil {
				// Defensive: resolveAtomics normalizes a resolved non-clear chain to
				// non-nil (present-empty). Unreachable today; treat as absent.
			} else {
				// Site E: a resolved standalone atomic is a DEPENDENT_WRITE — preserve the
				// op-type for conflict filtering.
				w.putEntry(k, rywEntry{value: base, dependent: true})
				writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: base})
			}
		} else {
			// A plain (non-atomic) entry is always a PRESENT key: cleared keys are
			// removed from w.writes (never tombstoned), so entry.value == nil means
			// "Set to empty bytes" / an atomic resolved to empty — NOT absent. The
			// previous `entry.value != nil` guard dropped such empty-value keys from
			// the merged range (e.g. after a Get resolved a pending Xor(k,"") into a
			// nil-value entry), so getRange disagreed with libfdb_c. Found by the
			// RFC-055 RYW-read differential.
			writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: entry.value})
		}
	}
	return writeKVs, atomicCleared
}

func mergeServerAndWriteKVs(filteredServer, writeKVs []KeyValue, atomicCleared map[string]bool, reverse bool) []KeyValue {
	// filteredServer: sorted in scan direction (forward=ascending, reverse=descending).
	// writeKVs: sorted ascending. Reverse it for reverse scans.
	if reverse {
		for i, j := 0, len(writeKVs)-1; i < j; i, j = i+1, j-1 {
			writeKVs[i], writeKVs[j] = writeKVs[j], writeKVs[i]
		}
	}

	result := make([]KeyValue, 0, len(filteredServer)+len(writeKVs))
	si, wi := 0, 0

	for si < len(filteredServer) || wi < len(writeKVs) {
		if si >= len(filteredServer) {
			result = append(result, writeKVs[wi:]...)
			break
		}
		if wi >= len(writeKVs) {
			// Append remaining server entries, skipping any cleared by atomics.
			for ; si < len(filteredServer); si++ {
				if !atomicCleared[string(filteredServer[si].Key)] {
					result = append(result, filteredServer[si])
				}
			}
			break
		}

		// Skip server entries cleared by atomic resolution.
		if atomicCleared[string(filteredServer[si].Key)] {
			si++
			continue
		}

		cmp := bytes.Compare(filteredServer[si].Key, writeKVs[wi].Key)
		if cmp == 0 {
			// Write shadows server — take write value, skip server.
			result = append(result, writeKVs[wi])
			si++
			wi++
		} else if (reverse && cmp > 0) || (!reverse && cmp < 0) {
			result = append(result, filteredServer[si])
			si++
		} else {
			result = append(result, writeKVs[wi])
			wi++
		}
	}

	return result
}
