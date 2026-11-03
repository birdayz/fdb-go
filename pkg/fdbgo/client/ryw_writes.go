package client

import (
	"bytes"
	"slices"
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
// guarded by rywCache.mu, except that a pinned version is only read.
//
// Its ordered stores are persistent sequences (pseq): the clone a write after a
// pin needs is O(1), and the write then copies only its O(log n) path, as C++
// gets from the persistent PTree under WriteMap (VersionedMap.h).
type rywWrites struct {
	// writes maps key → written value until the first ordered access; then
	// sorted holds the entries and writes is nil. A write-only transaction
	// keeps O(1) writes. entry.value == nil means the key was Set to empty
	// bytes, NOT cleared.
	writes   map[string]rywEntry
	sorted   pseq[rywWrite]
	isSorted bool

	// cleared is a sorted, non-overlapping list of [begin, end) byte ranges
	// that were ClearRange'd.
	cleared pseq[rywRange]

	// unreadableRanges: sorted, non-overlapping SVK candidate-stamp ranges (RFC-098).
	// Contract: a read REACHING a range throws accessed_unreadable (1036) unless
	// it is a BYPASS_UNREADABLE point read; a bypassed point with no local entry reads
	// through to storage (the range is UNMODIFIED); Clear/ClearRange SUBTRACT their
	// span. Pending writes inside a range are KEPT — a deliberate divergence from
	// C++'s span-wipe; rationale + observability analysis in RFC-098 ("the SVK
	// candidate range in the Go write map").
	unreadableRanges pseq[rywRange]

	// A separate index keeps unreadable-cap checks proportional to unreadable keys,
	// without building the full write-key index for an otherwise write-only transaction.
	unreadableKeys pseq[string]

	// bypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE
	// (ReadYourWrites.actor.cpp:2611-2613, applied only to points at :98): point reads of
	// unreadable keys return the write-map value with the versionstamp placeholder
	// bytes as written instead of throwing 1036; SVK's unmodified-unreadable range
	// reads through to storage.
	bypassUnreadable bool

	// nodes are the C++ WriteMap's nodes: the write-side segment boundaries (D1b).
	nodes rywNodes

	// pins counts the reads holding this version.
	pins int
}

// rywWrite is one sorted write-map entry.
type rywWrite struct {
	key string
	e   rywEntry
}

func newRYWWrites() *rywWrites { return &rywWrites{} }

// liveLocked is the live version, created on first use in the cache itself (no
// allocation). Caller holds c.mu.
func (c *rywCache) liveLocked() *rywWrites {
	if c.w == nil {
		c.base = rywWrites{}
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
		*c.w = rywWrites{}
	} else {
		c.w = newRYWWrites()
	}
	c.dropObsoleteBaseLocked()
}

// withLock runs fn holding c.mu, released even if fn panics: a recovered panic
// must not leave the cache locked for the read's complete() or a later write.
func (c *rywCache) withLock(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

// pinWritesLocked pins the live version for a read that will outlive the
// critical section. Its lazy indexes are built first, so the pinned version is
// never written again. Caller holds c.mu.
func (c *rywCache) pinWritesLocked() *rywWrites {
	w := c.liveLocked()
	w.ensureSorted()
	w.nodes.build()
	w.pins++
	if c.onPin != nil {
		observed := false
		defer func() {
			if !observed { // a panicking observer never transfers the pin to the caller
				w.pins--
				c.dropObsoleteBaseLocked()
			}
		}()
		c.onPin(w, true)
		observed = true
	}
	return w
}

// pinWritesForCommitLocked pins the live version for a commit, which reads its
// sorted writes and cleared ranges but not its nodes: a write-only transaction's
// nodes stay unbuilt (the commit takes their log). Caller holds c.mu.
func (c *rywCache) pinWritesForCommitLocked() *rywWrites {
	w := c.liveLocked()
	w.ensureSorted()
	w.pins++
	if c.onPin != nil {
		observed := false
		defer func() {
			if !observed { // a panicking observer never transfers the pin to the caller
				w.pins--
				c.dropObsoleteBaseLocked()
			}
		}()
		c.onPin(w, true)
		observed = true
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
	c.dropObsoleteBaseLocked()
}

// dropObsoleteBaseLocked empties the cache's embedded first version once it is
// neither live nor pinned, so its snapshot is not kept for the transaction's
// life (a heap version is collected instead). C++ releases a WriteMap version
// with its last iterator, and reset drops the map (RYW:2704-2706).
func (c *rywCache) dropObsoleteBaseLocked() {
	if c.w != &c.base && c.base.pins == 0 && !c.base.empty() {
		c.base = rywWrites{}
	}
}

// empty reports a version holding nothing (a zero rywWrites).
func (w *rywWrites) empty() bool {
	return w.writes == nil && !w.isSorted && w.sorted.root == nil && w.cleared.root == nil &&
		w.unreadableRanges.root == nil && w.unreadableKeys.root == nil && !w.nodes.built && w.nodes.log == nil
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

// clone snapshots the version in O(1): the copy shares every node and takes over
// writing them (pseq.handoff), and w keeps reading its version. Key, value and
// parameter bytes and atomic stacks are shared; they are never written after
// insertion (appendIfFitsOwned writes only past a slice's length).
func (w *rywWrites) clone() *rywWrites {
	w.ensureSorted()
	return &rywWrites{
		sorted:           w.sorted.handoff(),
		isSorted:         true,
		cleared:          w.cleared.handoff(),
		unreadableRanges: w.unreadableRanges.handoff(),
		unreadableKeys:   w.unreadableKeys.handoff(),
		bypassUnreadable: w.bypassUnreadable,
		nodes:            w.nodes.clone(),
	}
}

// ensureSorted moves the entries into the sorted store on the first ordered
// access after construction or reset. Must be called under c.mu.
func (w *rywWrites) ensureSorted() {
	if w.isSorted {
		return
	}
	items := make([]rywWrite, 0, len(w.writes))
	for k, e := range w.writes {
		items = append(items, rywWrite{key: k, e: e})
	}
	slices.SortFunc(items, func(a, b rywWrite) int { return compareStrings(a.key, b.key) })
	w.sorted.build(items)
	w.writes = nil
	w.isSorted = true
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// numWrites is the number of keys with an operation.
func (w *rywWrites) numWrites() int {
	if w.isSorted {
		return w.sorted.Len()
	}
	return len(w.writes)
}

// writeIndex is the index of the first sorted entry whose key is >= k. The
// version must be sorted.
func (w *rywWrites) writeIndex(k string) int {
	return w.sorted.search(func(x *rywWrite) bool { return x.key >= k })
}

// writeIndexOf is writeIndex for a byte key, without converting it.
func (w *rywWrites) writeIndexOf(k []byte) int {
	return w.sorted.search(func(x *rywWrite) bool { return x.key >= string(k) })
}

// entryOf is entry for a byte key, without converting it.
func (w *rywWrites) entryOf(k []byte) (rywEntry, bool) {
	if !w.isSorted {
		e, ok := w.writes[string(k)]
		return e, ok
	}
	if _, x := w.sorted.find(func(x *rywWrite) bool { return x.key >= string(k) }); x != nil && x.key == string(k) {
		return x.e, true
	}
	return rywEntry{}, false
}

// writeAt is the sorted entry at index i; it must not be written.
func (w *rywWrites) writeAt(i int) *rywWrite { return w.sorted.at(i) }

// writeKeys returns the written keys in order.
func (w *rywWrites) writeKeys() []string {
	w.ensureSorted()
	keys := make([]string, 0, w.sorted.Len())
	for cur := w.sorted.cursor(0); cur.valid(); cur.next() {
		keys = append(keys, cur.item().key)
	}
	return keys
}

// entry returns the entry for key k.
func (w *rywWrites) entry(k string) (rywEntry, bool) {
	if !w.isSorted {
		e, ok := w.writes[k]
		return e, ok
	}
	if _, x := w.sorted.find(func(x *rywWrite) bool { return x.key >= k }); x != nil && x.key == k {
		return x.e, true
	}
	return rywEntry{}, false
}

// putEntry stores the entry for key k, inserting the key if it is new.
func (w *rywWrites) putEntry(k string, e rywEntry) {
	if !w.isSorted {
		if w.writes == nil {
			w.writes = make(map[string]rywEntry)
		}
		w.writes[k] = e
		return
	}
	i, x := w.sorted.find(func(x *rywWrite) bool { return x.key >= k })
	if x != nil && x.key == k {
		w.sorted.mutAt(i).e = e
		return
	}
	w.sorted.insert(i, rywWrite{key: k, e: e})
}

// deleteEntry removes the entry for key k and returns it.
func (w *rywWrites) deleteEntry(k string) (rywEntry, bool) {
	if !w.isSorted {
		e, ok := w.writes[k]
		if ok {
			delete(w.writes, k)
		}
		return e, ok
	}
	i, x := w.sorted.find(func(x *rywWrite) bool { return x.key >= k })
	if x == nil || x.key != k {
		return rywEntry{}, false
	}
	e := x.e
	w.sorted.delete(i, i+1)
	return e, true
}

// rangeIndex is the first index whose range satisfies pred over a sorted,
// non-overlapping range list.
func rangeIndex(s *pseq[rywRange], pred func(r *rywRange) bool) int { return s.search(pred) }

// rangeContaining returns the range of the sorted, non-overlapping list s that
// contains key: the first one ending after key, when it begins at or before it.
func rangeContaining(s *pseq[rywRange], key []byte) *rywRange {
	_, r := s.find(func(r *rywRange) bool { return bytes.Compare(r.end, key) > 0 })
	if r != nil && bytes.Compare(r.begin, key) <= 0 {
		return r
	}
	return nil
}

// rangeList returns the ranges in order.
func rangeList(s *pseq[rywRange]) []rywRange { return s.appendTo(nil) }

func (w *rywWrites) isCleared(key []byte) bool {
	return rangeContaining(&w.cleared, key) != nil
}

// A point conflict check does not need to build the full write-key index.
// Matches updateConflictMap (ReadYourWrites.actor.cpp:322-332). Caller holds c.mu.
func (w *rywWrites) conflictForKey(key []byte) bool {
	if e, ok := w.entryOf(key); ok {
		return e.isDependentLocked() // operation key: conflict iff DEPENDENT_WRITE
	}
	if w.isCleared(key) {
		return false // cleared range: known empty locally, no DB read resolved it
	}
	return true // unmodified gap: a DB read resolved it
}

// conflictSpan is one read-conflict sub-range. Its bounds alias the read's own
// bounds and write-map bytes, which are never written after insertion, and keep
// keyAfter virtual: nothing is copied until the read-conflict map keeps a bound.
type conflictSpan struct{ begin, end rywBoundary }

// appendConflictSpans walks the write-map over [begin, end) and appends the maximal
// sub-ranges that must be added to the read-conflict map — a faithful port of C++
// updateConflictMap (ReadYourWrites.actor.cpp:335-351), which iterates the WriteMap (NOT the
// merged RYWIterator — the snapshot cache is irrelevant) and records a conflict for a segment
// iff `is_unmodified_range() || (is_operation() && !is_independent())`. I.e.:
//   - UNMODIFIED gap (no write key, not cleared) → conflict (a DB read resolved it).
//   - operation key with isDependentLocked() (DEPENDENT_WRITE — a standalone atomic that read
//     the DB base) → conflict, as the single key [k, keyAfter(k)).
//   - INDEPENDENT write key (plain Set, folded atomic, matched-CAC phantom) → NO conflict.
//   - CLEARED range (no operation key) → NO conflict.
//
// Adjacent qualifying sub-ranges are coalesced. (RFC-058 sub-edge b.) Like the C++ iterator it
// moves forward from segment to segment: one cursor over the writes and one over the cleared
// ranges, each positioned once, so a step compares against the next boundary only. It visits only
// the writes and cleared ranges in [begin, end) and allocates nothing but dst's growth.
// Caller holds c.mu, or the version is pinned (immutable).
func (w *rywWrites) appendConflictSpans(dst []conflictSpan, begin, end []byte) []conflictSpan {
	if bytes.Compare(begin, end) >= 0 {
		return dst
	}
	w.ensureSorted()
	first := len(dst)
	emit := func(lo, hi rywBoundary) {
		if n := len(dst); n > first && dst[n-1].end.equal(lo) {
			dst[n-1].end = hi
			return
		}
		dst = append(dst, conflictSpan{begin: lo, end: hi})
	}
	// Fingers allocate nothing, on the live version or a pinned one; each is
	// placed by one descent (lower_bound, VersionedMap.h:188-200).
	var wc pfinger[rywWrite]
	wc.seekFirst(&w.sorted, func(x *rywWrite) bool { return x.key >= string(begin) })
	var cc pfinger[rywRange]
	cc.seekFirst(&w.cleared, func(r *rywRange) bool { return bytes.Compare(r.end, begin) > 0 })
	cur, last := rywBoundary{key: begin}, rywBoundary{key: end}
	for cur.compare(last) < 0 {
		if wc.valid() {
			x := wc.item()
			key := rywBoundary{key: stringBytes(x.key)}
			if key.equal(cur) {
				// Operation key occupying the single-key segment [key, keyAfter(key)): nothing
				// lies strictly between a key and its successor, so the segment ends in range.
				next := key.keyAfter()
				if x.e.isDependentLocked() {
					emit(cur, next)
				}
				cur = next
				wc.next()
				continue
			}
		}
		// A gap up to the next boundary: the next write key, the next cleared bound or end.
		hi := last
		if wc.valid() {
			if key := (rywBoundary{key: stringBytes(wc.item().key)}); key.compare(hi) < 0 {
				hi = key
			}
		}
		for cc.valid() && (rywBoundary{key: cc.item().end}).compare(cur) <= 0 {
			cc.next()
		}
		cleared := false
		if cc.valid() {
			r := cc.item()
			if rb := (rywBoundary{key: r.begin}); rb.compare(cur) <= 0 {
				cleared = true // CLEARED range — no DB read.
				if re := (rywBoundary{key: r.end}); re.compare(hi) < 0 {
					hi = re
				}
			} else if rb.compare(hi) < 0 {
				hi = rb
			}
		}
		if !cleared {
			emit(cur, hi) // UNMODIFIED gap — a DB read resolved it.
		}
		cur = hi
	}
	return dst
}

// conflictRanges is appendConflictSpans with the bounds copied out.
func (w *rywWrites) conflictRanges(begin, end []byte) [][2][]byte {
	spans := w.appendConflictSpans(nil, begin, end)
	if len(spans) == 0 {
		return nil
	}
	out := make([][2][]byte, len(spans))
	for i, s := range spans {
		out[i] = [2][]byte{bytes.Clone(s.begin.materialize()), bytes.Clone(s.end.materialize())}
	}
	return out
}

func (w *rywWrites) hasWritesInRange(begin, end []byte) bool {
	w.ensureSorted()
	// The first key >= begin: if it is < end, there's a write in range.
	_, x := w.sorted.find(func(x *rywWrite) bool { return x.key >= string(begin) })
	return x != nil && x.key < string(end)
}

func (w *rywWrites) hasClearsInRange(begin, end []byte) bool {
	// Two ranges [a,b) and [c,d) overlap iff a < d && c < b. Cleared ranges are
	// sorted by begin and non-overlapping, so the candidate is the range just before
	// the first one with begin >= end (largest begin < end): if it does not overlap,
	// no earlier range can either (their end <= this one's begin).
	i := rangeIndex(&w.cleared, func(r *rywRange) bool { return bytes.Compare(r.begin, end) >= 0 })
	return i > 0 && bytes.Compare(w.cleared.at(i-1).end, begin) > 0
}

// addRange merges [begin, end) into a sorted, non-overlapping range list,
// joining the ranges it overlaps or touches.
func addRange(s *pseq[rywRange], begin, end []byte) {
	addRangeOwned(s, append([]byte(nil), begin...), append([]byte(nil), end...))
}

// addRangeOwned is addRange keeping begin and end, which nobody writes.
func addRangeOwned(s *pseq[rywRange], begin, end []byte) {
	// A range r overlaps/is-adjacent to [begin, end) if r.begin <= end && begin <= r.end.
	// [loIdx, hiIdx) are those ranges: ends are sorted too, as ranges do not overlap.
	hiIdx := rangeIndex(s, func(r *rywRange) bool { return bytes.Compare(r.begin, end) > 0 })
	loIdx := rangeIndex(s, func(r *rywRange) bool { return bytes.Compare(r.end, begin) >= 0 })

	newBegin, newEnd := begin, end
	if loIdx < hiIdx {
		if r := s.at(loIdx); bytes.Compare(r.begin, newBegin) < 0 {
			newBegin = r.begin
		}
		if r := s.at(hiIdx - 1); bytes.Compare(r.end, newEnd) > 0 {
			newEnd = r.end
		}
	}
	merged := rywRange{begin: newBegin, end: newEnd}
	if hiIdx-loIdx == 1 {
		s.set(loIdx, merged)
		return
	}
	s.delete(loIdx, hiIdx)
	s.insert(loIdx, merged)
}

// addClearedRangeOwned merges [begin, end) into cleared, keeping begin and end, which nobody writes.
func (w *rywWrites) addClearedRangeOwned(begin, end []byte) { addRangeOwned(&w.cleared, begin, end) }

// addUnreadableRange merges [begin, end) into the SVK candidate-stamp
// unreadable ranges (sorted, non-overlapping; same shape as `cleared`).
// C++ writes.addUnmodifiedAndUnreadableRange (ReadYourWrites.actor.cpp:2271).
func (w *rywWrites) addUnreadableRange(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	addRange(&w.unreadableRanges, begin, end)
}

// subtractRange removes [begin, end) from a sorted, non-overlapping range
// list, splitting a range that straddles the span. Used to make cleared spans
// readable again (C++ gets this free from the shared PTree: clear() inserts
// readable entries over the span, WriteMap.cpp:195).
func subtractRange(s *pseq[rywRange], begin, end []byte) {
	if s.Len() == 0 || bytes.Compare(begin, end) >= 0 {
		return
	}
	// [lo, hi) overlap the span: only the first can keep a left remainder and
	// only the last a right one.
	lo := rangeIndex(s, func(r *rywRange) bool { return bytes.Compare(r.end, begin) > 0 })
	hi := rangeIndex(s, func(r *rywRange) bool { return bytes.Compare(r.begin, end) >= 0 })
	if lo >= hi {
		return
	}
	var keep []rywRange
	if r := s.at(lo); bytes.Compare(r.begin, begin) < 0 {
		keep = append(keep, rywRange{begin: r.begin, end: append([]byte(nil), begin...)})
	}
	if r := s.at(hi - 1); bytes.Compare(r.end, end) > 0 {
		keep = append(keep, rywRange{begin: append([]byte(nil), end...), end: r.end})
	}
	s.delete(lo, hi)
	for i, r := range keep {
		s.insert(lo+i, r)
	}
}

// isUnreadable reports whether key falls inside an SVK candidate-stamp
// unreadable range. Caller holds c.mu.
func (w *rywWrites) isUnreadable(key []byte) bool {
	return rangeContaining(&w.unreadableRanges, key) != nil
}

// firstUnreadableIn returns the begin of the first unreadable range
// intersecting [begin, end), or nil. Drives GetRange REACH semantics: a scan
// throws only when iteration reaches the unreadable segment
// (ReadYourWrites.actor.cpp:685 limit-break precedes the :692 throw).
// Caller holds c.mu.
func (w *rywWrites) firstUnreadableIn(begin, end []byte) []byte {
	_, r := w.unreadableRanges.find(func(r *rywRange) bool { return bytes.Compare(r.end, begin) > 0 })
	if r != nil && bytes.Compare(r.begin, end) < 0 {
		// The intersection starts at max(range.begin, begin).
		if bytes.Compare(r.begin, begin) > 0 {
			return r.begin
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
	i := rangeIndex(&w.unreadableRanges, func(r *rywRange) bool { return bytes.Compare(r.begin, end) >= 0 }) - 1
	if i < 0 {
		return nil
	}
	r := w.unreadableRanges.at(i)
	if bytes.Compare(r.end, begin) <= 0 {
		return nil
	}
	if bytes.Compare(r.end, end) < 0 {
		return r.end
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
	if w.unreadableKeys.Len() == 0 && w.unreadableRanges.Len() == 0 {
		return nil
	}
	var cap_ []byte
	if !reverse {
		cap_ = w.firstUnreadableIn(begin, end)
		// Smallest unreadable entry key in [begin, end).
		if _, k := w.unreadableKeys.find(func(k *string) bool { return *k >= string(begin) }); k != nil && *k < string(end) {
			if cap_ == nil || *k < string(cap_) {
				cap_ = []byte(*k)
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
	if i := w.unreadableKeys.search(func(k *string) bool { return *k >= string(end) }) - 1; i >= 0 && *w.unreadableKeys.at(i) >= string(begin) {
		kb := append([]byte(*w.unreadableKeys.at(i)), 0)
		if cap_ == nil || string(kb) > string(cap_) {
			cap_ = kb
		}
	}
	return cap_
}

// insertUnreadableKey adds k to the sorted unreadableKeys index (no-op
// if present). Called when an entry's unreadable flag transitions on.
func (w *rywWrites) insertUnreadableKey(k string) {
	i, x := w.unreadableKeys.find(func(x *string) bool { return *x >= k })
	if x != nil && *x == k {
		return
	}
	w.unreadableKeys.insert(i, k)
}

// removeUnreadableKey drops k from the unreadableKeys index. Called when
// an unreadable entry is deleted (clear/clearRange — the only flag-off paths).
func (w *rywWrites) removeUnreadableKey(k string) {
	i, x := w.unreadableKeys.find(func(x *string) bool { return *x >= k })
	if x != nil && *x == k {
		w.unreadableKeys.delete(i, i+1)
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
