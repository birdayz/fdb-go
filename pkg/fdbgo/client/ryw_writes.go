package client

import (
	"bytes"
	"maps"
	"slices"
	"sort"
	"unsafe"
)

// stringBytes views a map key's bytes without copying; the write map never
// writes them, so a node can share the key with the map entry.
func stringBytes(s string) []byte { return unsafe.Slice(unsafe.StringData(s), len(s)) }

// rywWrites is one version of the write map (RFC-262 D1). A pinned version is
// immutable: it is the C++ WriteMap::iterator snapshot, "unaffected by future
// writes" (WriteMap.h:158-160). The live version is rywCache.w; a writer that
// finds it pinned replaces it with a clone first (mutableWritesLocked), so a
// read that pinned a version keeps seeing it until it unpins. All fields are
// guarded by rywCache.mu.
type rywWrites struct {
	// writes maps key → written value. Present means Set was called.
	// entry.value == nil means the key was Set to empty bytes, NOT cleared.
	writes map[string]rywEntry

	// Built lazily, then kept sorted across mutations to avoid re-sorting on reads.
	// Inserting or deleting a key still shifts an O(N) slice suffix.
	sortedKeys []string

	// cleared is a sorted, non-overlapping list of [begin, end) byte ranges
	// that were ClearRange'd.
	cleared []rywRange

	// unreadableRanges: sorted, non-overlapping SVK candidate-stamp ranges (RFC-098).
	// Contract: a read REACHING a range throws accessed_unreadable (1036) unless
	// it is a BYPASS_UNREADABLE point read; a bypassed point with no local entry reads
	// through to storage (the range is UNMODIFIED); Clear/ClearRange SUBTRACT their
	// span. Pending writes inside a range are KEPT — a deliberate divergence from
	// C++'s span-wipe; rationale + observability analysis in RFC-098 ("the SVK
	// candidate range in the Go write map").
	unreadableRanges []rywRange

	// A separate index keeps unreadable-cap checks proportional to unreadable keys,
	// without building the full write-key index for an otherwise write-only transaction.
	unreadableKeys []string

	// bypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE
	// (ReadYourWrites.actor.cpp:2611-2613, applied only to points at :98): point reads of
	// unreadable keys return the write-map value with the versionstamp placeholder
	// bytes as written instead of throwing 1036; SVK's unmodified-unreadable range
	// reads through to storage.
	bypassUnreadable bool

	// nodes are the C++ WriteMap's nodes: the write-side segment boundaries (D1b).
	nodes rywNodes
	// hasNodes is false for a private span copy (captureWriteSpanLocked), which keeps
	// the legacy boundary sources until phase 3 deletes the old range loop (D2).
	hasNodes bool

	// pins counts the reads holding this version.
	pins int
}

func newRYWWrites() *rywWrites { return &rywWrites{hasNodes: true} }

// liveLocked is the live version, created on first use in the cache itself (no
// allocation). Caller holds c.mu.
func (c *rywCache) liveLocked() *rywWrites {
	if c.w == nil {
		c.base = rywWrites{hasNodes: true}
		c.w = &c.base
	}
	return c.w
}

// resetWritesLocked installs an empty live version: the current one, emptied in
// place, when no read pins it (no read holds an unpinned version past its critical
// section); a new one otherwise. Caller holds c.mu.
func (c *rywCache) resetWritesLocked() {
	if c.w == nil {
		return // liveLocked starts the cache's own version
	}
	if c.w.pins == 0 {
		*c.w = rywWrites{hasNodes: true}
		return
	}
	c.w = newRYWWrites()
}

// pinWritesLocked pins the live version for a read that will outlive the
// critical section. Caller holds c.mu.
func (c *rywCache) pinWritesLocked() *rywWrites {
	w := c.liveLocked()
	w.pins++
	if c.onPin != nil {
		c.onPin(w, true)
	}
	return w
}

// unpinWritesLocked releases a pin taken by pinWritesLocked. Caller holds c.mu.
func (c *rywCache) unpinWritesLocked(w *rywWrites) {
	if w.pins <= 0 {
		panic("rywWrites: unpin of an unpinned version")
	}
	w.pins--
	if c.onPin != nil {
		c.onPin(w, false)
	}
}

// mutableWritesLocked is the live version, cloned first if a read pins it. Every
// write-map mutation goes through it. Caller holds c.mu.
func (c *rywCache) mutableWritesLocked() *rywWrites {
	w := c.liveLocked()
	if w.pins > 0 {
		c.w = w.clone()
		if c.onClone != nil {
			c.onClone(w, c.w)
		}
	}
	return c.w
}

// clone copies the version: the map with entries by value and atomic stacks
// re-sliced (coalesceOverAtomics rewrites stack[top] in place), the slices, and
// the nodes. Key, value and parameter bytes are shared; they are never written
// after insertion.
func (w *rywWrites) clone() *rywWrites {
	n := &rywWrites{
		writes:           maps.Clone(w.writes),
		sortedKeys:       slices.Clone(w.sortedKeys),
		cleared:          slices.Clone(w.cleared),
		unreadableRanges: slices.Clone(w.unreadableRanges),
		unreadableKeys:   slices.Clone(w.unreadableKeys),
		bypassUnreadable: w.bypassUnreadable,
		nodes:            w.nodes.clone(),
		hasNodes:         w.hasNodes,
	}
	for k, e := range n.writes {
		if e.hasAtomics {
			e.atomics = slices.Clone(e.atomics)
			n.writes[k] = e
		}
	}
	return n
}

// ensureSorted builds sortedKeys on first use after construction or reset.
// Must be called under c.mu.
func (w *rywWrites) ensureSorted() {
	if w.sortedKeys != nil {
		return
	}
	if len(w.writes) == 0 {
		w.sortedKeys = []string{}
		return
	}
	w.sortedKeys = make([]string, 0, len(w.writes))
	for k := range w.writes {
		w.sortedKeys = append(w.sortedKeys, k)
	}
	sort.Strings(w.sortedKeys)
}

func (w *rywWrites) insertSortedKey(k string) {
	if w.sortedKeys == nil {
		return // not built yet; ensureSortedLocked builds it on demand
	}
	i, _ := slices.BinarySearch(w.sortedKeys, k)
	w.sortedKeys = slices.Insert(w.sortedKeys, i, k)
}

func (w *rywWrites) removeSortedKey(k string) {
	if i, found := slices.BinarySearch(w.sortedKeys, k); found {
		w.sortedKeys = slices.Delete(w.sortedKeys, i, i+1)
	}
}

func (w *rywWrites) isCleared(key []byte) bool {
	// Binary search: cleared is sorted by begin, non-overlapping.
	// Find the last range whose begin <= key, then check key < end.
	n := len(w.cleared)
	if n == 0 {
		return false
	}
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(w.cleared[i].begin, key) > 0
	})
	// i is the first range with begin > key. Check i-1.
	if i == 0 {
		return false
	}
	r := w.cleared[i-1]
	return bytes.Compare(key, r.end) < 0
}

// A point conflict check does not need to build the full write-key index.
// Matches updateConflictMap (ReadYourWrites.actor.cpp:322-332). Caller holds c.mu.
func (w *rywWrites) conflictForKey(key []byte) bool {
	if e, ok := w.writes[string(key)]; ok {
		return e.isDependentLocked() // operation key: conflict iff DEPENDENT_WRITE
	}
	if w.isCleared(key) {
		return false // cleared range: known empty locally, no DB read resolved it
	}
	return true // unmodified gap: a DB read resolved it
}

// conflictRanges walks the write-map over [begin, end) and returns the maximal
// sub-ranges that must be added to the getKey read-conflict map — a faithful port of C++
// updateConflictMap (ReadYourWrites.actor.cpp:335-351), which iterates the WriteMap (NOT the
// merged RYWIterator — the snapshot cache is irrelevant) and records a conflict for a segment
// iff `is_unmodified_range() || (is_operation() && !is_independent())`. I.e.:
//   - UNMODIFIED gap (no write key, not cleared) → conflict (a DB read resolved it).
//   - operation key with isDependentLocked() (DEPENDENT_WRITE — a standalone atomic that read
//     the DB base) → conflict, as the single key [k, keyAfter(k)).
//   - INDEPENDENT write key (plain Set, folded atomic, matched-CAC phantom) → NO conflict.
//   - CLEARED range (no operation key) → NO conflict.
//
// Adjacent qualifying sub-ranges are coalesced. Caller holds c.mu. The over-conflict that the
// old full-range conflict caused (on INDEPENDENT writes + cleared ranges) is thereby removed,
// matching C++ exactly. (RFC-058 sub-edge b.)
func (w *rywWrites) conflictRanges(begin, end []byte) [][2][]byte {
	if bytes.Compare(begin, end) >= 0 {
		return nil
	}
	w.ensureSorted()

	// Collect the segment boundaries within [begin, end]: begin, end, every write key in
	// range AND its keyAfter (so an operation occupies its own single-key segment [k,
	// keyAfter(k)) and the following gap is classified separately), and every cleared-range
	// bound clamped into the window.
	bounds := [][]byte{append([]byte(nil), begin...), append([]byte(nil), end...)}
	addBound := func(b []byte) {
		if bytes.Compare(b, begin) > 0 && bytes.Compare(b, end) < 0 {
			bounds = append(bounds, append([]byte(nil), b...))
		}
	}
	wStart := sort.SearchStrings(w.sortedKeys, string(begin))
	for i := wStart; i < len(w.sortedKeys) && w.sortedKeys[i] < string(end); i++ {
		k := []byte(w.sortedKeys[i])
		addBound(k)
		addBound(keyAfterBytes(k))
	}
	for _, r := range w.cleared {
		if bytes.Compare(r.begin, end) >= 0 {
			break
		}
		if bytes.Compare(r.end, begin) <= 0 {
			continue
		}
		addBound(r.begin)
		addBound(r.end)
	}
	sort.Slice(bounds, func(i, j int) bool { return bytes.Compare(bounds[i], bounds[j]) < 0 })

	var out [][2][]byte
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		if bytes.Equal(lo, hi) {
			continue // deduped boundary
		}
		conflict := false
		if entry, ok := w.writes[string(lo)]; ok && bytes.Equal(hi, keyAfterBytes(lo)) {
			// Operation key occupying the single-key segment [lo, keyAfter(lo)).
			conflict = entry.isDependentLocked()
		} else if w.isCleared(lo) {
			conflict = false // CLEARED range — no DB read.
		} else {
			conflict = true // UNMODIFIED gap — a DB read resolved it.
		}
		if !conflict {
			continue
		}
		// Coalesce with the previous sub-range if adjacent.
		if n := len(out); n > 0 && bytes.Equal(out[n-1][1], lo) {
			out[n-1][1] = append([]byte(nil), hi...)
		} else {
			out = append(out, [2][]byte{append([]byte(nil), lo...), append([]byte(nil), hi...)})
		}
	}
	return out
}

func (w *rywWrites) hasWritesInRange(begin, end []byte) bool {
	w.ensureSorted()
	if len(w.sortedKeys) == 0 {
		return false
	}
	// Binary search: find the first key >= begin.
	i := sort.SearchStrings(w.sortedKeys, string(begin))
	// If that key is < end, there's a write in range.
	return i < len(w.sortedKeys) && w.sortedKeys[i] < string(end)
}

func (w *rywWrites) hasClearsInRange(begin, end []byte) bool {
	// Binary search: find the last cleared range that could overlap [begin, end).
	// Two ranges [a,b) and [c,d) overlap iff a < d && c < b.
	// Cleared ranges are sorted by begin and non-overlapping.
	n := len(w.cleared)
	if n == 0 {
		return false
	}
	// Find the first range with begin >= end (definitely can't overlap).
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(w.cleared[i].begin, end) >= 0
	})
	// The candidate is the range just before i (largest begin < end).
	// Since ranges are non-overlapping and sorted, if this one doesn't
	// overlap, no earlier range can either (their end <= this one's begin).
	if i > 0 && bytes.Compare(w.cleared[i-1].end, begin) > 0 {
		return true
	}
	return false
}

func (w *rywWrites) addClearedRange(begin, end []byte) {
	n := len(w.cleared)
	if n == 0 {
		w.cleared = []rywRange{{
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
		return bytes.Compare(w.cleared[i].begin, end) > 0
	})
	// Last overlapping range: first range with end >= begin.
	// We find the first range whose end > begin, starting from 0.
	// Since ranges are sorted and non-overlapping, end values are also sorted.
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(w.cleared[i].end, begin) >= 0
	})

	// [loIdx, hiIdx) are the ranges that overlap or are adjacent.
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)

	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(w.cleared[i].begin, newBegin) < 0 {
			newBegin = w.cleared[i].begin
		}
		if bytes.Compare(w.cleared[i].end, newEnd) > 0 {
			newEnd = w.cleared[i].end
		}
	}

	// Replace [loIdx, hiIdx) with the merged range.
	merged := rywRange{begin: newBegin, end: newEnd}
	overlapCount := hiIdx - loIdx
	if overlapCount == 0 {
		// No overlaps — insert at loIdx.
		w.cleared = append(w.cleared, rywRange{})
		copy(w.cleared[loIdx+1:], w.cleared[loIdx:])
		w.cleared[loIdx] = merged
	} else if overlapCount == 1 {
		// Replace single overlapping range in-place.
		w.cleared[loIdx] = merged
	} else {
		// Replace multiple overlapping ranges with one.
		w.cleared[loIdx] = merged
		w.cleared = append(w.cleared[:loIdx+1], w.cleared[hiIdx:]...)
	}
}

// addUnreadableRange merges [begin, end) into the SVK candidate-stamp
// unreadable ranges (sorted, non-overlapping; same shape as `cleared`).
// C++ writes.addUnmodifiedAndUnreadableRange (ReadYourWrites.actor.cpp:2271).
func (w *rywWrites) addUnreadableRange(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	n := len(w.unreadableRanges)
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(w.unreadableRanges[i].begin, end) > 0
	})
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(w.unreadableRanges[i].end, begin) >= 0
	})
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)
	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(w.unreadableRanges[i].begin, newBegin) < 0 {
			newBegin = w.unreadableRanges[i].begin
		}
		if bytes.Compare(w.unreadableRanges[i].end, newEnd) > 0 {
			newEnd = w.unreadableRanges[i].end
		}
	}
	merged := append([]rywRange(nil), w.unreadableRanges[:loIdx]...)
	merged = append(merged, rywRange{begin: newBegin, end: newEnd})
	merged = append(merged, w.unreadableRanges[hiIdx:]...)
	w.unreadableRanges = merged
}

// isUnreadable reports whether key falls inside an SVK candidate-stamp
// unreadable range. Caller holds c.mu.
func (w *rywWrites) isUnreadable(key []byte) bool {
	// First range with end > key; key is inside iff that range's begin <= key.
	i := sort.Search(len(w.unreadableRanges), func(i int) bool {
		return bytes.Compare(w.unreadableRanges[i].end, key) > 0
	})
	return i < len(w.unreadableRanges) && bytes.Compare(w.unreadableRanges[i].begin, key) <= 0
}

// firstUnreadableIn returns the begin of the first unreadable range
// intersecting [begin, end), or nil. Drives GetRange REACH semantics: a scan
// throws only when iteration reaches the unreadable segment
// (ReadYourWrites.actor.cpp:685 limit-break precedes the :692 throw).
// Caller holds c.mu.
func (w *rywWrites) firstUnreadableIn(begin, end []byte) []byte {
	i := sort.Search(len(w.unreadableRanges), func(i int) bool {
		return bytes.Compare(w.unreadableRanges[i].end, begin) > 0
	})
	if i < len(w.unreadableRanges) && bytes.Compare(w.unreadableRanges[i].begin, end) < 0 {
		// The intersection starts at max(range.begin, begin).
		if bytes.Compare(w.unreadableRanges[i].begin, begin) > 0 {
			return w.unreadableRanges[i].begin
		}
		return begin
	}
	return nil
}

// lastUnreadableIn returns the (exclusive) end of the last unreadable
// range intersecting [begin, end), or nil — the reverse-scan counterpart of
// firstUnreadableIn, using the same binary search (ranges are sorted
// and non-overlapping, so only the last range with r.begin < end can
// intersect). Caller holds c.mu.
func (w *rywWrites) lastUnreadableIn(begin, end []byte) []byte {
	i := sort.Search(len(w.unreadableRanges), func(i int) bool {
		return bytes.Compare(w.unreadableRanges[i].begin, end) >= 0
	}) - 1
	if i < 0 || bytes.Compare(w.unreadableRanges[i].end, begin) <= 0 {
		return nil
	}
	if bytes.Compare(w.unreadableRanges[i].end, end) < 0 {
		return w.unreadableRanges[i].end
	}
	return end
}

// unreadableScanCap computes how far a scan over [begin, end) may
// iterate before REACHING unreadable state (an SVK candidate range or an
// entry with a pending versionstamped op / sticky unreadable flag): the cap
// is an exclusive end bound for forward scans, an inclusive begin bound for
// reverse scans, or nil when nothing unreadable intersects. Reach semantics:
// C++ throws only when the iterator lands on the segment
// (ReadYourWrites.actor.cpp:685 limit-break precedes the :692 throw), so the
// caller emits results inside the capped window and errors only if iteration
// would cross the cap. Caller holds c.mu.
func (w *rywWrites) unreadableScanCap(begin, end []byte, reverse bool) []byte {
	// Fast path: no unreadable state at all — the overwhelmingly common case.
	// The dedicated unreadable index avoids building or scanning all write keys.
	if len(w.unreadableKeys) == 0 && len(w.unreadableRanges) == 0 {
		return nil
	}
	var cap_ []byte
	if !reverse {
		cap_ = w.firstUnreadableIn(begin, end)
		// Smallest unreadable entry key in [begin, end).
		if i := sort.SearchStrings(w.unreadableKeys, string(begin)); i < len(w.unreadableKeys) && w.unreadableKeys[i] < string(end) {
			if k := w.unreadableKeys[i]; cap_ == nil || k < string(cap_) {
				cap_ = []byte(k)
			}
		}
		return cap_
	}
	cap_ = w.lastUnreadableIn(begin, end)
	// Largest unreadable entry key in [begin, end); its exclusive upper bound
	// (key+\x00) becomes the reverse scan's begin cap. Only the LARGEST key
	// matters: a reverse scan iterates downward from end, so the highest
	// unreadable key is the first one it can reach — any lower entries sit
	// behind it (the forward path symmetrically needs only the smallest).
	if i := sort.SearchStrings(w.unreadableKeys, string(end)) - 1; i >= 0 && w.unreadableKeys[i] >= string(begin) {
		kb := append([]byte(w.unreadableKeys[i]), 0)
		if cap_ == nil || string(kb) > string(cap_) {
			cap_ = kb
		}
	}
	return cap_
}

// insertUnreadableKey adds k to the sorted unreadableKeys index (no-op
// if present). Called when an entry's unreadable flag transitions on.
func (w *rywWrites) insertUnreadableKey(k string) {
	i := sort.SearchStrings(w.unreadableKeys, k)
	if i < len(w.unreadableKeys) && w.unreadableKeys[i] == k {
		return
	}
	w.unreadableKeys = append(w.unreadableKeys, "")
	copy(w.unreadableKeys[i+1:], w.unreadableKeys[i:])
	w.unreadableKeys[i] = k
}

// removeUnreadableKey drops k from the unreadableKeys index. Called when
// an unreadable entry is deleted (clear/clearRange — the only flag-off paths).
func (w *rywWrites) removeUnreadableKey(k string) {
	i := sort.SearchStrings(w.unreadableKeys, k)
	if i < len(w.unreadableKeys) && w.unreadableKeys[i] == k {
		w.unreadableKeys = append(w.unreadableKeys[:i], w.unreadableKeys[i+1:]...)
	}
}

// viewLocked is the version a legacy cursor reads: the live one, or nil for the
// snapshot cache alone. Caller holds c.mu.
func (c *rywCache) viewLocked(includeWrites bool) *rywWrites {
	if !includeWrites {
		return nil
	}
	return c.liveLocked()
}
