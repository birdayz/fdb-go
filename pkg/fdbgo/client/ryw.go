package client

import (
	"bytes"
	"context"
	"encoding/binary"
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

	// cleared is a sorted, non-overlapping list of [begin, end) byte ranges
	// that were ClearRange'd.
	cleared []rywRange
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

// reset clears all cached state.
func (c *rywCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = nil
	c.cleared = nil
}

// set records a Set operation.
func (c *rywCache) set(key, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	// Defensive copy. FDB treats Set(key, nil) and Set(key, []byte{}) as equivalent
	// — both set the key to empty bytes. The cached value must be non-nil so that
	// get() can distinguish "key exists with empty value" from "key not found" (nil).
	copied := make([]byte, len(value))
	copy(copied, value)
	c.writes[string(key)] = rywEntry{value: copied}
	// A Set after ClearRange wins — no need to remove from cleared, because
	// get() checks writes before cleared.
}

// clear records a Clear operation (single key).
func (c *rywCache) clear(key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove from writes.
	delete(c.writes, string(key))
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
	// Remove all writes in [begin, end).
	for k := range c.writes {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(c.writes, k)
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
		// Key has a plain Set value — apply atomic to it immediately.
		val, clr := applyAtomic(op, entry.value, param)
		if clr {
			delete(c.writes, k)
			c.addClearedRangeLocked(append([]byte(nil), key...), append(append([]byte(nil), key...), 0))
		} else {
			entry.value = val
			c.writes[k] = entry
		}
		return
	}
	if !exists && c.isClearedLocked(key) {
		// Key was cleared — base is nil. Apply atomic against nil.
		val, clr := applyAtomic(op, nil, param)
		if !clr {
			c.writes[k] = rywEntry{value: val}
		}
		// If clr, key stays cleared — no action needed.
		return
	}
	// Either no entry (unknown base, server-dependent) or already an atomics list — append.
	entry.hasAtomics = true
	entry.atomics = append(entry.atomics, rywMutation{typ: op, param: append([]byte(nil), param...)})
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
	c.mu.Unlock()
	if isClr {
		return nil, nil
	}
	return serverGet(ctx, key)
}

// getRange intercepts a range read and merges with pending writes/clears.
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
		return serverGetRange(ctx, begin, end, limit, reverse)
	}

	// Slow path: fetch from server and merge with local writes/clears.
	// Over-fetch to compensate for clears removing server results.
	serverLimit := limit
	c.mu.Lock()
	if c.hasClearsInRangeLocked(begin, end) {
		// Clears can remove server results. Fetch more to compensate.
		// Cap at 10000 to avoid unbounded fetches.
		serverLimit = limit * 4
		if serverLimit < 100 {
			serverLimit = 100
		}
		if serverLimit > 10000 {
			serverLimit = 10000
		}
	}
	c.mu.Unlock()

	// Server call outside lock. Track the server's `more` flag so we can
	// propagate it correctly: if clears remove results and server had more
	// data, we must not claim the range is exhausted.
	serverKVs, serverMore, err := serverGetRange(ctx, begin, end, serverLimit, reverse)
	if err != nil {
		return nil, false, err
	}

	// Determine the boundary of server-fetched data. When serverMore=true,
	// we only know the DB state up to the last returned key. Local writes
	// beyond this boundary MUST NOT be included — there may be un-fetched
	// server keys between the boundary and those local writes.
	// C++ avoids this entirely via segment-tree iterator (RYWIterator);
	// we approximate by restricting local writes to the fetched range.
	var serverBoundary []byte
	if serverMore && len(serverKVs) > 0 {
		if reverse {
			serverBoundary = serverKVs[len(serverKVs)-1].Key // lowest key fetched
		} else {
			serverBoundary = serverKVs[len(serverKVs)-1].Key // highest key fetched
		}
	}

	// Build a map from server results for fast lookup.
	merged := make(map[string][]byte, len(serverKVs))
	for _, kv := range serverKVs {
		merged[string(kv.Key)] = kv.Value
	}

	// Lock for merge with cache state.
	c.mu.Lock()

	// Remove cleared keys from server results.
	for k := range merged {
		if c.isClearedLocked([]byte(k)) {
			delete(merged, k)
		}
	}

	// Apply writes: replace or add, but only within the server-fetched boundary.
	for k, entry := range c.writes {
		kb := []byte(k)
		if bytes.Compare(kb, begin) < 0 || bytes.Compare(kb, end) >= 0 {
			continue // outside requested range
		}
		// When server had more data, only include writes within the fetched boundary.
		// Beyond the boundary, un-fetched server keys may interleave with our writes.
		if serverBoundary != nil {
			if reverse {
				if bytes.Compare(kb, serverBoundary) < 0 {
					continue // below the fetched boundary in reverse scan
				}
			} else {
				if bytes.Compare(kb, serverBoundary) > 0 {
					continue // above the fetched boundary in forward scan
				}
			}
		}
		if entry.hasAtomics {
			// Resolve atomics against server base.
			base := merged[k] // may be nil if not in server results
			cleared := false
			for _, m := range entry.atomics {
				base, cleared = applyAtomic(m.typ, base, m.param)
				if cleared {
					base = nil
				}
			}
			if cleared {
				delete(merged, k)
			} else {
				merged[k] = base
			}
			// Cache resolved value.
			if c.writes == nil {
				c.writes = make(map[string]rywEntry)
			}
			if cleared {
				delete(c.writes, k)
				c.addClearedRangeLocked([]byte(k), append([]byte(k), 0))
			} else {
				c.writes[k] = rywEntry{value: base}
			}
		} else if entry.value != nil {
			// Only add non-nil values (nil = key was cleared via CompareAndClear).
			merged[k] = entry.value
		}
	}

	c.mu.Unlock()

	// Collect and sort (no lock needed — merged is local).
	result := make([]KeyValue, 0, len(merged))
	for k, v := range merged {
		result = append(result, KeyValue{Key: []byte(k), Value: v})
	}
	if reverse {
		sort.Slice(result, func(i, j int) bool {
			return bytes.Compare(result[i].Key, result[j].Key) > 0
		})
	} else {
		sort.Slice(result, func(i, j int) bool {
			return bytes.Compare(result[i].Key, result[j].Key) < 0
		})
	}

	// Apply limit. Track whether more data may exist beyond what we return.
	more := false
	if limit > 0 && len(result) > limit {
		result = result[:limit]
		more = true
	} else if serverMore {
		// Server had more data beyond what we fetched. Propagate `more`
		// only if we actually have room for more results (len(result) > 0
		// or there are excluded local writes beyond the boundary).
		// Without this guard, a range that's entirely cleared locally
		// would return more=true, 0 results → infinite loop.
		if len(result) > 0 {
			more = true
		}
		// If result is empty but serverMore=true, the caller needs to
		// advance the range. This is safe because the server range was
		// non-empty — the merged result being empty means all server
		// keys were cleared. The caller's continuation mechanism will
		// advance begin past the cleared region.
	}

	return result, more, nil
}

// isCleared returns true if key falls within any cleared range (acquires lock).
func (c *rywCache) isCleared(key []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isClearedLocked(key)
}

func (c *rywCache) isClearedLocked(key []byte) bool {
	for _, r := range c.cleared {
		if bytes.Compare(key, r.begin) >= 0 && bytes.Compare(key, r.end) < 0 {
			return true
		}
	}
	return false
}

func (c *rywCache) hasWritesInRangeLocked(begin, end []byte) bool {
	for k := range c.writes {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			return true
		}
	}
	return false
}

func (c *rywCache) hasClearsInRangeLocked(begin, end []byte) bool {
	for _, r := range c.cleared {
		// Two ranges [a,b) and [c,d) overlap iff a < d && c < b.
		if bytes.Compare(r.begin, end) < 0 && bytes.Compare(begin, r.end) < 0 {
			return true
		}
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
	newRange := rywRange{
		begin: append([]byte(nil), begin...),
		end:   append([]byte(nil), end...),
	}

	// Find all existing ranges that overlap or are adjacent to [begin, end).
	var merged []rywRange
	for _, r := range c.cleared {
		// r overlaps/adjacent if r.begin <= end && begin <= r.end
		if bytes.Compare(r.begin, end) <= 0 && bytes.Compare(begin, r.end) <= 0 {
			// Merge: extend newRange.
			if bytes.Compare(r.begin, newRange.begin) < 0 {
				newRange.begin = r.begin
			}
			if bytes.Compare(r.end, newRange.end) > 0 {
				newRange.end = r.end
			}
		} else {
			merged = append(merged, r)
		}
	}
	merged = append(merged, newRange)
	sort.Slice(merged, func(i, j int) bool {
		return bytes.Compare(merged[i].begin, merged[j].begin) < 0
	})
	c.cleared = merged
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
	const valueSizeLimit = 100000 // CLIENT_KNOBS->VALUE_SIZE_LIMIT
	if len(e)+len(param) > valueSizeLimit {
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
