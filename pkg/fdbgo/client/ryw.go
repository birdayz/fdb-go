package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
)

// rywCache implements a read-your-writes cache that intercepts reads and
// merges them with pending writes from the same transaction. It sits between
// the public Transaction API and the wire-level read functions.
//
// Key invariant: writes entries take precedence over cleared ranges. If a key
// was ClearRange'd then Set, the Set wins.
type rywCache struct {
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
	c.writes = nil
	c.cleared = nil
}

// set records a Set operation.
func (c *rywCache) set(key, value []byte) {
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	c.writes[string(key)] = rywEntry{value: append([]byte(nil), value...)}
	// A Set after ClearRange wins — no need to remove from cleared, because
	// get() checks writes before cleared.
}

// clear records a Clear operation (single key).
func (c *rywCache) clear(key []byte) {
	// Remove from writes.
	delete(c.writes, string(key))
	// Add [key, key+\x00) to cleared.
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	c.addClearedRange(key, end)
}

// clearRange records a ClearRange [begin, end).
func (c *rywCache) clearRange(begin, end []byte) {
	// Remove all writes in [begin, end).
	for k := range c.writes {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(c.writes, k)
		}
	}
	c.addClearedRange(begin, end)
}

// atomic records an atomic mutation.
func (c *rywCache) atomic(op MutationType, key, param []byte) {
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	k := string(key)
	entry, exists := c.writes[k]
	if exists && !entry.hasAtomics {
		// Key has a plain Set value — apply atomic to it immediately.
		entry.value = applyAtomic(op, entry.value, param)
		c.writes[k] = entry
		return
	}
	if !exists && c.isCleared(key) {
		// Key was cleared — base is nil (zero). Apply atomic immediately against
		// nil rather than deferring to server (which would return the stale
		// pre-clear value). Matches C++ WriteMap::mutate which inserts
		// SetValue(nil) as base when the key is in a cleared range.
		c.writes[k] = rywEntry{value: applyAtomic(op, nil, param)}
		return
	}
	// Either no entry (unknown base, server-dependent) or already an atomics list — append.
	entry.hasAtomics = true
	entry.atomics = append(entry.atomics, rywMutation{typ: op, param: append([]byte(nil), param...)})
	c.writes[k] = entry
}

// get intercepts a single-key read and merges with pending writes.
func (c *rywCache) get(ctx context.Context, key []byte, serverGet func(ctx context.Context, key []byte) ([]byte, error)) ([]byte, error) {
	k := string(key)
	if entry, ok := c.writes[k]; ok {
		if entry.hasAtomics {
			// Read base value from server, apply atomics, cache result.
			base, err := serverGet(ctx, key)
			if err != nil {
				return nil, err
			}
			for _, m := range entry.atomics {
				base = applyAtomic(m.typ, base, m.param)
			}
			// Cache as plain Set so subsequent Gets don't re-read.
			if c.writes == nil {
				c.writes = make(map[string]rywEntry)
			}
			c.writes[k] = rywEntry{value: base}
			return base, nil
		}
		// Plain Set — return directly. nil value means empty bytes were Set.
		return entry.value, nil
	}
	if c.isCleared(key) {
		return nil, nil // definitely deleted
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
	hasOverlap := c.hasWritesInRange(begin, end) || c.hasClearsInRange(begin, end)
	if !hasOverlap {
		// Fast path: no writes or clears overlap this range.
		return serverGetRange(ctx, begin, end, limit, reverse)
	}

	// Slow path: request unlimited from server and merge client-side.
	serverKVs, _, err := serverGetRange(ctx, begin, end, 0x7FFFFFFF, reverse)
	if err != nil {
		return nil, false, err
	}

	// Build a map from server results for fast lookup.
	merged := make(map[string][]byte, len(serverKVs))
	for _, kv := range serverKVs {
		merged[string(kv.Key)] = kv.Value
	}

	// Remove cleared keys from server results.
	for k := range merged {
		if c.isCleared([]byte(k)) {
			delete(merged, k)
		}
	}

	// Apply writes: replace or add.
	for k, entry := range c.writes {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			if entry.hasAtomics {
				// Resolve atomics against server base.
				base := merged[k] // may be nil if not in server results
				for _, m := range entry.atomics {
					base = applyAtomic(m.typ, base, m.param)
				}
				merged[k] = base
				// Cache resolved value.
				if c.writes == nil {
					c.writes = make(map[string]rywEntry)
				}
				c.writes[k] = rywEntry{value: base}
			} else {
				merged[k] = entry.value
			}
		}
	}

	// Collect and sort.
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

	// Apply limit.
	more := false
	if limit > 0 && len(result) > limit {
		result = result[:limit]
		more = true
	}

	return result, more, nil
}

// isCleared returns true if key falls within any cleared range.
func (c *rywCache) isCleared(key []byte) bool {
	for _, r := range c.cleared {
		if bytes.Compare(key, r.begin) >= 0 && bytes.Compare(key, r.end) < 0 {
			return true
		}
	}
	return false
}

// hasWritesInRange returns true if any write key falls in [begin, end).
func (c *rywCache) hasWritesInRange(begin, end []byte) bool {
	for k := range c.writes {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			return true
		}
	}
	return false
}

// hasClearsInRange returns true if any cleared range overlaps [begin, end).
func (c *rywCache) hasClearsInRange(begin, end []byte) bool {
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

// applyAtomic applies an atomic mutation to a base value.
func applyAtomic(op MutationType, base, param []byte) []byte {
	switch op {
	case MutAddValue:
		return atomicAdd(base, param)
	case MutMax:
		return atomicMax(base, param)
	case MutMin:
		return atomicMin(base, param)
	case MutByteMax:
		return atomicByteMax(base, param)
	case MutByteMin:
		return atomicByteMin(base, param)
	case MutAnd, MutAndV2:
		return atomicAnd(base, param)
	case MutOr:
		return atomicBitwise(base, param, func(a, b byte) byte { return a | b })
	case MutXor:
		return atomicBitwise(base, param, func(a, b byte) byte { return a ^ b })
	case MutMinV2:
		return atomicMin(base, param)
	case MutSetValue:
		// SetValue overwrites — return param as the new value.
		return append([]byte(nil), param...)
	default:
		// For unsupported atomics (AppendIfFits, CompareAndClear, versionstamps),
		// we can't resolve client-side. Return the base value unchanged — the
		// server will do the real resolution at commit time.
		if base != nil {
			return append([]byte(nil), base...)
		}
		return nil
	}
}

// atomicAdd interprets both values as little-endian integers and adds them.
// FDB semantics: if base is nil/empty, treat as zero. Operands are zero-padded
// to the length of the longer one.
func atomicAdd(base, param []byte) []byte {
	size := len(param)
	if len(base) > size {
		size = len(base)
	}
	if size == 0 {
		return nil
	}
	// Pad both to size.
	a := make([]byte, size)
	copy(a, base)
	b := make([]byte, size)
	copy(b, param)

	// Handle common case (8 bytes = int64) fast.
	if size == 8 {
		va := binary.LittleEndian.Uint64(a)
		vb := binary.LittleEndian.Uint64(b)
		result := make([]byte, 8)
		binary.LittleEndian.PutUint64(result, va+vb)
		return result
	}

	// General case: byte-by-byte addition with carry.
	result := make([]byte, size)
	var carry uint16
	for i := 0; i < size; i++ {
		sum := carry + uint16(a[i]) + uint16(b[i])
		result[i] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// atomicMax compares as little-endian unsigned integers, keeps the larger.
// FDB semantics: if base is nil, param wins. Zero-pad to longer length.
func atomicMax(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if compareLE(base, param) >= 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// atomicMin compares as little-endian unsigned integers, keeps the smaller.
// FDB semantics: if base is nil, param wins. Zero-pad to longer length.
func atomicMin(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if compareLE(base, param) <= 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// compareLE compares two byte slices as little-endian unsigned integers.
// Returns -1, 0, or 1.
func compareLE(a, b []byte) int {
	size := len(a)
	if len(b) > size {
		size = len(b)
	}
	// Compare from most significant byte (highest index) down.
	for i := size - 1; i >= 0; i-- {
		var va, vb byte
		if i < len(a) {
			va = a[i]
		}
		if i < len(b) {
			vb = b[i]
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

// atomicByteMax compares as raw bytes (big-endian / lexicographic), keeps larger.
func atomicByteMax(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) >= 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// atomicByteMin compares as raw bytes (lexicographic), keeps smaller.
func atomicByteMin(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) <= 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// atomicAnd applies bitwise AND with FDB semantics:
// - If base is nil/absent, return param.
// - Result length = len(param) (param truncates the result).
// - Missing base bytes (base shorter than param) are treated as 0xFF.
func atomicAnd(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	for i := 0; i < len(param); i++ {
		var a byte = 0xFF // missing base bytes are 0xFF for AND
		if i < len(base) {
			a = base[i]
		}
		result[i] = a & param[i]
	}
	return result
}

// atomicBitwise applies a bitwise operation element-wise.
// FDB semantics for OR/XOR: zero-pad shorter operand.
func atomicBitwise(base, param []byte, op func(a, b byte) byte) []byte {
	size := len(param)
	if len(base) > size {
		size = len(base)
	}
	if size == 0 {
		return nil
	}
	result := make([]byte, size)
	for i := 0; i < size; i++ {
		var a, b byte
		if i < len(base) {
			a = base[i]
		}
		if i < len(param) {
			b = param[i]
		}
		result[i] = op(a, b)
	}
	return result
}
