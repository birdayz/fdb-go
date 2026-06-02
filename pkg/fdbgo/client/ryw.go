package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"sort"
	"sync"
)

// rywCache implements a read-your-writes cache that intercepts reads and
// merges them with pending writes from the same transaction. It sits between
// the public Transaction API and the wire-level read functions.
//
// Key invariant: writes entries take precedence over cleared ranges. If a key
// was ClearRange'd then Set, the Set wins.
type rywCache struct {
	mu sync.Mutex

	// writes maps key → written value. Present means Set was called.
	// entry.value == nil means the key was Set to empty bytes, NOT cleared.
	writes map[string]rywEntry

	// sortedKeys is a lazily-maintained sorted copy of write map keys.
	// Set to nil when writes changes (dirty). Rebuilt on demand for getRange
	// to enable O(log N) binary search instead of O(N) linear scan.
	sortedKeys []string

	// cleared is a sorted, non-overlapping list of [begin, end) byte ranges
	// that were ClearRange'd.
	cleared []rywRange

	// serverCache caches server-side state at the read version, avoiding
	// redundant server round-trips for repeated reads of the same range.
	// Matches C++'s SnapshotCache. Not invalidated by local writes/clears.
	serverCache snapshotCache

	// byteBuf batch-allocates small byte copies (e.g. atomic mutation params).
	// Reduces per-op allocs for small values.
	byteBuf []byte
}

// rywEntry represents a pending write for a single key.
type rywEntry struct {
	value []byte
	// If true, this entry has pending atomic mutations instead of a plain Set.
	hasAtomics bool
	atomics    []rywMutation
}

// rywMutation represents a single atomic mutation.
type rywMutation struct {
	typ   MutationType
	param []byte
}

// rywRange represents a cleared range [begin, end).
type rywRange struct {
	begin []byte
	end   []byte
}

// allocBytes returns a slice of n bytes from the cache's shared buffer.
// Must be called with c.mu held. Reduces per-op allocs for small byte copies.
func (c *rywCache) allocBytes(n int) []byte {
	if cap(c.byteBuf)-len(c.byteBuf) < n {
		newCap := max(2*cap(c.byteBuf), len(c.byteBuf)+n)
		if newCap < 2048 {
			newCap = 2048
		}
		newBuf := make([]byte, len(c.byteBuf), newCap)
		copy(newBuf, c.byteBuf)
		c.byteBuf = newBuf
	}
	start := len(c.byteBuf)
	c.byteBuf = c.byteBuf[:start+n]
	return c.byteBuf[start : start+n]
}

// reset clears all cached state.
func (c *rywCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = nil
	c.sortedKeys = nil
	c.cleared = nil
	c.byteBuf = c.byteBuf[:0]
	c.serverCache.reset()
}

// ensureSortedLocked rebuilds sortedKeys from the writes map if dirty (nil).
// Must be called under c.mu.
func (c *rywCache) ensureSortedLocked() {
	if c.sortedKeys != nil {
		return
	}
	if len(c.writes) == 0 {
		c.sortedKeys = []string{}
		return
	}
	c.sortedKeys = make([]string, 0, len(c.writes))
	for k := range c.writes {
		c.sortedKeys = append(c.sortedKeys, k)
	}
	sort.Strings(c.sortedKeys)
}

// set records a Set operation.
func (c *rywCache) set(key, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	// Defensive copy — value must have its own backing array.
	// Cannot share byteBuf: the READABLE_UNIQUE_PENDING test proves
	// that read-back of cached Set values fails when values alias
	// the shared buffer (root cause: byteBuf growth copies data,
	// but sub-slices of the old buffer become stale if the buffer
	// pointer advances during the same transaction).
	copied := make([]byte, len(value))
	copy(copied, value)
	c.writes[string(key)] = rywEntry{value: copied}
	c.sortedKeys = nil // invalidate sorted index
	// A Set after ClearRange wins — no need to remove from cleared, because
	// get() checks writes before cleared.
}

// clear records a Clear operation (single key).
func (c *rywCache) clear(key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove from writes.
	if _, existed := c.writes[string(key)]; existed {
		delete(c.writes, string(key))
		c.sortedKeys = nil // invalidate sorted index
	}
	// Add [key, key+\x00) to cleared.
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	c.addClearedRangeLocked(key, end)
}

// clearRange records a ClearRange [begin, end).
func (c *rywCache) clearRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove all writes in [begin, end) using sorted keys for O(log N + k).
	if len(c.writes) > 0 {
		c.ensureSortedLocked()
		wStart := sort.SearchStrings(c.sortedKeys, string(begin))
		wEnd := sort.SearchStrings(c.sortedKeys, string(end))
		if wStart < wEnd {
			for i := wStart; i < wEnd; i++ {
				delete(c.writes, c.sortedKeys[i])
			}
			c.sortedKeys = nil // invalidate sorted index
		}
	}
	c.addClearedRangeLocked(begin, end)
}

// atomic records an atomic mutation.
func (c *rywCache) atomic(op MutationType, key, param []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	k := string(key)
	entry, exists := c.writes[k]
	if exists && !entry.hasAtomics {
		val, clr := applyAtomic(op, entry.value, param)
		if clr {
			delete(c.writes, k)
			c.sortedKeys = nil
			c.addClearedRangeLocked(append([]byte(nil), key...), append(append([]byte(nil), key...), 0))
		} else {
			entry.value = val
			c.writes[k] = entry
		}
		return
	}
	if !exists && c.isClearedLocked(key) {
		val, clr := applyAtomic(op, nil, param)
		if !clr {
			c.writes[k] = rywEntry{value: val}
			c.sortedKeys = nil
		}
		return
	}
	entry.hasAtomics = true
	paramCopy := c.allocBytes(len(param))
	copy(paramCopy, param)
	entry.atomics = append(entry.atomics, rywMutation{typ: op, param: paramCopy})
	if !exists {
		c.sortedKeys = nil
	}
	c.writes[k] = entry
}

// get intercepts a single-key read and merges with pending writes.
func (c *rywCache) get(ctx context.Context, key []byte, serverGet func(ctx context.Context, key []byte) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	k := string(key)
	if entry, ok := c.writes[k]; ok {
		if entry.hasAtomics {
			// Copy atomics list, unlock for server call.
			atomics := make([]rywMutation, len(entry.atomics))
			copy(atomics, entry.atomics)
			c.mu.Unlock()

			base, err := serverGet(ctx, key)
			if err != nil {
				return nil, err
			}
			cleared := false
			for _, m := range atomics {
				base, cleared = applyAtomic(m.typ, base, m.param)
				if cleared {
					base = nil
				}
			}
			// Re-lock to cache result.
			c.mu.Lock()
			if c.writes == nil {
				c.writes = make(map[string]rywEntry)
			}
			if cleared {
				delete(c.writes, k)
				c.sortedKeys = nil // key removed, invalidate sorted index
				c.addClearedRangeLocked(append([]byte(nil), key...), append(append([]byte(nil), key...), 0))
				c.mu.Unlock()
				return nil, nil
			}
			c.writes[k] = rywEntry{value: base}
			c.mu.Unlock()
			return base, nil
		}
		val := entry.value
		c.mu.Unlock()
		return val, nil
	}
	isClr := c.isClearedLocked(key)
	if isClr {
		c.mu.Unlock()
		return nil, nil
	}
	// Check snapshot cache for prior server read.
	if val, known := c.serverCache.getKey(key); known {
		c.mu.Unlock()
		return val, nil
	}
	c.mu.Unlock()

	val, err := serverGet(ctx, key)
	if err != nil {
		return nil, err
	}
	// Cache the server result.
	c.mu.Lock()
	keyAfter := append(append([]byte(nil), key...), 0)
	var kvs []KeyValue
	if val != nil {
		kvs = []KeyValue{{Key: append([]byte(nil), key...), Value: val}}
	}
	c.serverCache.insert(key, keyAfter, kvs)
	c.mu.Unlock()
	return val, nil
}

// getRange intercepts a range read and merges with pending writes/clears.
//
// Uses iterative fetching to avoid the silent truncation bug: when the server
// has more data (serverMore=true) but all fetched results are locally cleared,
// we advance the scan range and re-fetch instead of returning more=false.
// This matches the spirit of C++'s RYWIterator which handles unknown ranges
// by issuing server reads and continuing iteration.
func (c *rywCache) getRange(
	ctx context.Context,
	begin, end []byte,
	limit int,
	reverse bool,
	serverGetRange func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error),
) ([]KeyValue, bool, error) {
	c.mu.Lock()
	hasWrites := c.hasWritesInRangeLocked(begin, end)
	hasClears := c.hasClearsInRangeLocked(begin, end)
	c.mu.Unlock()
	if !hasWrites && !hasClears {
		// Fast path: no local mutations. Check snapshot cache first.
		c.mu.Lock()
		cachedKVs, fullyKnown := c.serverCache.getRangeKVs(begin, end)
		c.mu.Unlock()
		if fullyKnown {
			return applyLimitAndDirection(cachedKVs, limit, reverse), computeMore(cachedKVs, limit), nil
		}
		kvs, more, err := serverGetRange(ctx, begin, end, limit, reverse)
		if err != nil {
			return nil, false, err
		}
		c.cacheServerResult(begin, end, kvs, more, reverse)
		return kvs, more, nil
	}

	// Slow path: iterative fetch + merge. Loop until we either fill
	// the limit or the server is exhausted for the remaining range.
	var result []KeyValue
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
		// Fetch from server with headroom to compensate for clears.
		// Cap at 10000 before doubling to avoid overflow when remaining=math.MaxInt.
		fetchLimit := 10000
		if remaining <= 5000 {
			fetchLimit = remaining * 2
			if fetchLimit < 256 {
				fetchLimit = 256
			}
		}

		serverKVs, serverMore, err := c.fetchOrCached(ctx, curBegin, curEnd, fetchLimit, reverse, serverGetRange)
		if err != nil {
			return nil, false, err
		}

		// Knowledge boundary: when serverMore=true, we only know the DB
		// state up to the last returned key. Writes beyond this boundary
		// MUST NOT be included — un-fetched server keys may interleave.
		// When serverMore=false, boundary is nil → all writes in range
		// are safe to include.
		var boundary []byte
		if serverMore && len(serverKVs) > 0 {
			boundary = serverKVs[len(serverKVs)-1].Key
		}

		batch := c.mergeBatch(serverKVs, curBegin, curEnd, boundary, reverse)

		take := len(batch)
		if take > remaining {
			take = remaining
		}
		result = append(result, batch[:take]...)
		remaining -= take

		if remaining <= 0 {
			// Hit limit. More data exists if we truncated batch or server had more.
			return result, take < len(batch) || serverMore, nil
		}

		if !serverMore {
			// Server exhausted this range. All writes included. Done.
			return result, false, nil
		}

		// Server had more data, but we still need results.
		// Advance the scan range past the last fetched server key.
		if len(serverKVs) == 0 {
			break // Shouldn't happen: serverMore=true with 0 results.
		}

		if reverse {
			curEnd = serverKVs[len(serverKVs)-1].Key // [curBegin, lastKey)
		} else {
			// keyAfter(lastKey): append \x00 to step past the last fetched key.
			lastKey := serverKVs[len(serverKVs)-1].Key
			curBegin = append(append([]byte{}, lastKey...), 0)
		}
	}

	return result, false, nil
}

// fetchOrCached checks the snapshot cache before making a server call.
// If the range is fully cached, returns cached KVs (in scan direction).
// Otherwise fetches from server and caches the result.
func (c *rywCache) fetchOrCached(
	ctx context.Context,
	begin, end []byte,
	limit int,
	reverse bool,
	serverGetRange func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error),
) ([]KeyValue, bool, error) {
	c.mu.Lock()
	cachedKVs, fullyKnown := c.serverCache.getRangeKVs(begin, end)
	c.mu.Unlock()

	if fullyKnown {
		kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
		more := computeMore(cachedKVs, limit)
		return kvs, more, nil
	}

	kvs, more, err := serverGetRange(ctx, begin, end, limit, reverse)
	if err != nil {
		return nil, false, err
	}
	c.cacheServerResult(begin, end, kvs, more, reverse)
	return kvs, more, nil
}

// cacheServerResult inserts a server getRange result into the snapshot cache.
func (c *rywCache) cacheServerResult(fetchBegin, fetchEnd []byte, serverKVs []KeyValue, serverMore bool, reverse bool) {
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
		cacheKVs = make([]KeyValue, len(serverKVs))
		copy(cacheKVs, serverKVs)
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

// mergeBatch merges a batch of server results with local writes and clears.
// boundary is the knowledge boundary (last fetched key); nil means the entire
// range is known. Returns sorted key-value pairs.
//
// Uses sorted write keys + two-pointer merge for O(k + S) instead of the
// previous O(W + S log S) where W = total writes, k = writes in range, S = server results.
func (c *rywCache) mergeBatch(
	serverKVs []KeyValue,
	rangeBegin, rangeEnd []byte,
	boundary []byte,
	reverse bool,
) []KeyValue {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ensureSortedLocked()

	// Phase 1: Filter server results — remove cleared keys.
	// Server results are already sorted in scan direction.
	filteredServer := make([]KeyValue, 0, len(serverKVs))
	// Build server key lookup only if needed for atomic resolution.
	// Most Record Layer transactions have no atomics — skip the map allocation.
	var serverValues map[string][]byte
	for _, kv := range serverKVs {
		if !c.isClearedLocked(kv.Key) {
			filteredServer = append(filteredServer, kv)
		}
	}

	// atomicCleared tracks keys where atomic resolution resulted in deletion.
	// These must be excluded from filteredServer during the merge phase,
	// because the atomic was resolved AFTER building filteredServer.
	var atomicCleared map[string]bool

	// Phase 2: Find write keys in the effective range using binary search.
	// For forward scans: include writes in [rangeBegin, boundary] (inclusive).
	// For reverse scans: include writes in [boundary, rangeEnd) (inclusive begin).
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

	wStart := sort.SearchStrings(c.sortedKeys, effectiveBegin)
	wEnd := sort.SearchStrings(c.sortedKeys, effectiveEnd)

	// Process writes in range: resolve atomics, collect into sorted slice.
	writeKVs := make([]KeyValue, 0, wEnd-wStart)
	for i := wStart; i < wEnd; i++ {
		k := c.sortedKeys[i]
		entry, exists := c.writes[k]
		if !exists {
			continue // phantom key (deleted by prior atomic caching)
		}
		if entry.hasAtomics {
			// Lazily build server values map on first atomic encounter.
			if serverValues == nil {
				serverValues = make(map[string][]byte, len(serverKVs))
				for _, kv := range serverKVs {
					serverValues[string(kv.Key)] = kv.Value
				}
			}
			// Resolve atomics against server base.
			base := serverValues[k]
			cleared := false
			for _, m := range entry.atomics {
				base, cleared = applyAtomic(m.typ, base, m.param)
				if cleared {
					base = nil
				}
			}
			// Cache resolved value.
			if cleared {
				delete(c.writes, k)
				c.addClearedRangeLocked([]byte(k), append([]byte(k), 0))
				// Track for merge phase: this key must also be excluded
				// from filteredServer (it was built before atomic resolution).
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
				// Note: sortedKeys is intentionally NOT invalidated here.
				// We're mid-iteration over sortedKeys; the deleted key leaves
				// a phantom that's handled by the `if !exists { continue }`
				// guard at the top of this loop. Future mergeBatch calls
				// will rebuild sortedKeys via ensureSortedLocked if needed.
			} else {
				c.writes[k] = rywEntry{value: base}
				writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: base})
			}
		} else {
			// A plain (non-atomic) entry is always a PRESENT key: cleared keys are
			// removed from c.writes (never tombstoned), so entry.value == nil means
			// "Set to empty bytes" / an atomic resolved to empty — NOT absent. The
			// previous `entry.value != nil` guard dropped such empty-value keys from
			// the merged range (e.g. after a Get resolved a pending Xor(k,"") into a
			// nil-value entry), so getRange disagreed with libfdb_c. Found by the
			// RFC-055 RYW-read differential.
			writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: entry.value})
		}
	}
	// writeKVs is sorted ascending (from sortedKeys iteration).

	// Phase 3: Two-pointer merge.
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

// isCleared returns true if key falls within any cleared range (acquires lock).
func (c *rywCache) isCleared(key []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isClearedLocked(key)
}

func (c *rywCache) isClearedLocked(key []byte) bool {
	// Binary search: cleared is sorted by begin, non-overlapping.
	// Find the last range whose begin <= key, then check key < end.
	n := len(c.cleared)
	if n == 0 {
		return false
	}
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].begin, key) > 0
	})
	// i is the first range with begin > key. Check i-1.
	if i == 0 {
		return false
	}
	r := c.cleared[i-1]
	return bytes.Compare(key, r.end) < 0
}

func (c *rywCache) hasWritesInRangeLocked(begin, end []byte) bool {
	c.ensureSortedLocked()
	if len(c.sortedKeys) == 0 {
		return false
	}
	// Binary search: find the first key >= begin.
	i := sort.SearchStrings(c.sortedKeys, string(begin))
	// If that key is < end, there's a write in range.
	return i < len(c.sortedKeys) && c.sortedKeys[i] < string(end)
}

func (c *rywCache) hasClearsInRangeLocked(begin, end []byte) bool {
	// Binary search: find the last cleared range that could overlap [begin, end).
	// Two ranges [a,b) and [c,d) overlap iff a < d && c < b.
	// Cleared ranges are sorted by begin and non-overlapping.
	n := len(c.cleared)
	if n == 0 {
		return false
	}
	// Find the first range with begin >= end (definitely can't overlap).
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].begin, end) >= 0
	})
	// The candidate is the range just before i (largest begin < end).
	// Since ranges are non-overlapping and sorted, if this one doesn't
	// overlap, no earlier range can either (their end <= this one's begin).
	if i > 0 && bytes.Compare(c.cleared[i-1].end, begin) > 0 {
		return true
	}
	return false
}

// addClearedRange adds [begin, end) to the cleared list, merging overlapping
// and adjacent ranges to keep the list sorted and non-overlapping.
func (c *rywCache) addClearedRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addClearedRangeLocked(begin, end)
}

func (c *rywCache) addClearedRangeLocked(begin, end []byte) {
	n := len(c.cleared)
	if n == 0 {
		c.cleared = []rywRange{{
			begin: append([]byte(nil), begin...),
			end:   append([]byte(nil), end...),
		}}
		return
	}

	// Binary search to find the overlap window.
	// Ranges are sorted by begin, non-overlapping.
	// A range r overlaps/is-adjacent to [begin, end) if r.begin <= end && begin <= r.end.

	// First overlapping range: last range with begin <= end.
	// We find the first range with begin > end, then look back.
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].begin, end) > 0
	})
	// Last overlapping range: first range with end >= begin.
	// We find the first range whose end > begin, starting from 0.
	// Since ranges are sorted and non-overlapping, end values are also sorted.
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].end, begin) >= 0
	})

	// [loIdx, hiIdx) are the ranges that overlap or are adjacent.
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)

	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(c.cleared[i].begin, newBegin) < 0 {
			newBegin = c.cleared[i].begin
		}
		if bytes.Compare(c.cleared[i].end, newEnd) > 0 {
			newEnd = c.cleared[i].end
		}
	}

	// Replace [loIdx, hiIdx) with the merged range.
	merged := rywRange{begin: newBegin, end: newEnd}
	overlapCount := hiIdx - loIdx
	if overlapCount == 0 {
		// No overlaps — insert at loIdx.
		c.cleared = append(c.cleared, rywRange{})
		copy(c.cleared[loIdx+1:], c.cleared[loIdx:])
		c.cleared[loIdx] = merged
	} else if overlapCount == 1 {
		// Replace single overlapping range in-place.
		c.cleared[loIdx] = merged
	} else {
		// Replace multiple overlapping ranges with one.
		c.cleared[loIdx] = merged
		c.cleared = append(c.cleared[:loIdx+1], c.cleared[hiIdx:]...)
	}
}

// applyAtomic applies an atomic mutation to a base value, mirroring the C++
// implementations in fdbclient/include/fdbclient/Atomic.h exactly.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present).
// base==[]byte{} means "key present with empty value".
// Returns (result, cleared). cleared=true means the key should be removed
// (only happens for CompareAndClear).
func applyAtomic(op MutationType, base, param []byte) (result []byte, cleared bool) {
	switch op {
	case MutSetValue:
		return append([]byte(nil), param...), false
	case MutAddValue:
		return doAdd(base, param), false
	case MutAnd:
		return doAnd(base, param), false
	case MutAndV2:
		return doAndV2(base, param), false
	case MutOr:
		return doOr(base, param), false
	case MutXor:
		return doXor(base, param), false
	case MutMax:
		return doMax(base, param), false
	case MutMin:
		return doMin(base, param), false
	case MutMinV2:
		return doMinV2(base, param), false
	case MutByteMax:
		return doByteMax(base, param), false
	case MutByteMin:
		return doByteMin(base, param), false
	case MutAppendIfFits:
		return doAppendIfFits(base, param), false
	case MutCompareAndClear:
		return doCompareAndClear(base, param)
	default:
		// Versionstamped mutations can't be resolved client-side.
		if base != nil {
			return append([]byte(nil), base...), false
		}
		return nil, false
	}
}

// existing returns the "present" value for C++ Optional<ValueRef> semantics.
// nil → empty StringRef (for operations that treat absent as empty).
func existing(base []byte) []byte {
	if base == nil {
		return []byte{}
	}
	return base
}

// doAdd — C++ doAdd in Atomic.h. Little-endian addition.
// Result length = len(param), matching C++ which allocates otherOperand.size().
// Base bytes beyond len(param) are silently dropped (carry discarded).
func doAdd(base, param []byte) []byte {
	e := existing(base)
	size := len(param)
	if size == 0 {
		return []byte{}
	}
	a := make([]byte, size)
	copy(a, e) // zero-pads if len(e) < size; truncates if len(e) > size
	b := make([]byte, size)
	copy(b, param)

	if size == 8 {
		result := make([]byte, 8)
		binary.LittleEndian.PutUint64(result, binary.LittleEndian.Uint64(a)+binary.LittleEndian.Uint64(b))
		return result
	}
	result := make([]byte, size)
	var carry uint16
	for i := 0; i < size; i++ {
		sum := carry + uint16(a[i]) + uint16(b[i])
		result[i] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// doAnd — C++ doAnd in Atomic.h. Result length = param length. Missing base → 0x00.
func doAnd(base, param []byte) []byte {
	e := existing(base)
	if len(param) == 0 {
		return []byte{}
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] & param[i]
	}
	// Remaining positions: 0x00 (base bytes beyond existing are 0, 0 & anything = 0)
	return result
}

// doAndV2 — C++ doAndV2. If absent → return param. Otherwise → doAnd.
func doAndV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doAnd(base, param)
}

// doOr — C++ doOr. Result length = param length. Missing base → 0x00.
func doOr(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] | param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doXor — C++ doXor. Result length = param length. Missing base → 0x00.
func doXor(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] ^ param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doMax — C++ doMax. Little-endian unsigned compare. Result length = param length.
func doMax(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	// Compare from MSB of param down. Extra param bytes beyond existing treated as > 0.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			return append([]byte(nil), param...)
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			return append([]byte(nil), param...)
		} else if param[i] < e[i] {
			// Return existing truncated/zero-padded to param length.
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	return append([]byte(nil), param...)
}

// doMin — C++ doMin. Little-endian unsigned compare. Result length = param length.
func doMin(base, param []byte) []byte {
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	e := existing(base)
	// Compare from MSB of param down.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		} else if param[i] < e[i] {
			return append([]byte(nil), param...)
		}
	}
	return append([]byte(nil), param...)
}

// doMinV2 — C++ doMinV2. If absent → return param. Otherwise → doMin.
func doMinV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doMin(base, param)
}

// doByteMax — C++ doByteMax. Lexicographic (big-endian byte) comparison.
func doByteMax(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) > 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doByteMin — C++ doByteMin. Lexicographic comparison.
func doByteMin(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) < 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doAppendIfFits — C++ doAppendIfFits. Concatenates if within 100KB limit.
func doAppendIfFits(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), e...)
	}
	if len(e)+len(param) > valueSizeLimit { // CLIENT_KNOBS->VALUE_SIZE_LIMIT (pkg const)
		return append([]byte(nil), e...)
	}
	result := make([]byte, len(e)+len(param))
	copy(result, e)
	copy(result[len(e):], param)
	return result
}

// doCompareAndClear — C++ doCompareAndClear. If absent or equal → clear.
func doCompareAndClear(base, param []byte) ([]byte, bool) {
	if base == nil || bytes.Equal(base, param) {
		return nil, true // Clear the value.
	}
	return append([]byte(nil), base...), false // No change.
}
