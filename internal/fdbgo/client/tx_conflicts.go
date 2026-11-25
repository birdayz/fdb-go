package client

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"fdb.dev/internal/fdbwire"
)

// addConflictMu protects readConflicts/writeConflicts from concurrent append.
// The Apple C binding uses a single-threaded actor model so doesn't need this.
// Our Go futures use goroutines, so concurrent Get/Set calls on the same
// transaction race on the conflict slices.

// KeyRange represents a range [Begin, End).
type KeyRange struct {
	Begin []byte
	End   []byte
}

// addGetKeyConflictRange adds the read-conflict range(s) for a getKey, spanning the
// selector base ↔ resolved key (orEqual-adjusted, oriented by offset sign) — a port of
// C++ addConflictRange(GetKeyReq) (ReadYourWrites.actor.cpp:230-243), THEN filtered by
// updateConflictMap (:335). The base↔resolved span is the same range C++ computes; the
// filter (conflictRangesLocked) then SUBTRACTS segments satisfied locally with no DB read —
// INDEPENDENT writes (plain Set / folded atomic / matched-CAC phantom) and cleared ranges —
// keeping only UNMODIFIED gaps + DEPENDENT writes (which did read the DB base). With op-type
// now preserved (RFC-058), this subtraction is SAFE: a Get-folded DEPENDENT atomic keeps its
// dependent flag, so it is NOT dropped (the unsafe under-conflict hazard came
// from a naive !hasAtomics filter on the lossy pre-fold state — not possible now). Matches
// libfdb_c exactly instead of over-conflicting on the no-DB-read segments.
func (tx *Transaction) addGetKeyConflictRange(selKey []byte, orEqual bool, offset int32, resolved []byte) {
	begin, end := getKeyConflictSpan(KeySelector{Key: selKey, OrEqual: orEqual, Offset: offset}, resolved)
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	// When RYW is disabled, GetKey resolved against STORAGE only (Transaction.GetKey uses
	// tx.getKey, not the RYW walk) — the local write-map did NOT satisfy the read, so every
	// segment in the span was a real DB read. Filtering through the (bypassed) write-map
	// would subtract a local Set/Clear segment and MISS a conflict from a concurrent insert
	// into that gap. Add the full span, matching C++ (RYW-disabled reads go through
	// the underlying transaction, which records the full read-conflict; updateConflictMap is
	// an RYW-layer step that does not run).
	if tx.rywDisabled {
		tx.addReadConflict(begin, end)
		return
	}
	tx.addReadConflictRYW(begin, end) // atomic insert of the filtered sub-ranges
}

// addReadConflictRYW is updateConflictMap over [begin, end) (ReadYourWrites.actor.cpp:335-351):
// the live write map's filtered sub-ranges, inserted into the read-conflict map together. The
// write map changes only under conflictMu, so the filter and the inserts see one version; the
// sub-ranges alias it and the read's bounds, and only what the map keeps is copied.
func (tx *Transaction) addReadConflictRYW(begin, end []byte) {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	var scratch [4]conflictSpan
	tx.ryw.mu.Lock()
	spans := tx.ryw.liveLocked().appendConflictSpans(scratch[:0], begin, end)
	tx.ryw.mu.Unlock()
	tx.addReadConflictSpansLocked(spans)
}

// addReadConflictForKeyRYW records the read-conflict for a single-key Get, routed through the
// RYW write-map filter — mirroring C++ updateConflictMap(ryw, key, it)
// (ReadYourWrites.actor.cpp:322-332): a conflict is added only when the key sits in an UNMODIFIED
// range or a DEPENDENT operation, and SKIPPED when a local INDEPENDENT write (a plain Set or a
// folded atomic) already satisfies the read — so `Set(K); Get(K)` adds no read-conflict on K, the
// way libfdb_c behaves. When RYW is disabled there is no write map (the read went straight to
// storage, recording the full read-conflict), so the full single-key conflict is added; this is
// the exact rywDisabled/else split addGetKeyConflictRange already uses for GetKey. (RFC-121 D2.)
//
// Callers add this at the PRE-read position (unlike getRangeDir, which moved the conflict after the
// read to clamp the extent). That is intentional and equivalent for a single key: the classification
// is read-invariant (the read never mutates the write map), and a single-key read that errors poisons
// the transaction (trackReadError → commit fails / reset clears conflicts), so a conflict from a
// failed read can never survive to a successful commit — matching C++'s success-only add without
// pushing the conflict into GetPipelined's Resolve/cache-hit branches.
func (tx *Transaction) addReadConflictForKeyRYW(key []byte) {
	if tx.readConflictForKeyRYW(key) {
		tx.addReadConflictForKey(key)
	}
}

// readConflictForKeyRYW classifies a single-key read against the current write map.
func (tx *Transaction) readConflictForKeyRYW(key []byte) bool {
	if tx.rywDisabled {
		return true
	}
	// One map lookup (conflictForKey), not the range path (appendConflictSpans): Get is the hot
	// path, and a range walk per Get made write-heavy transactions quadratic.
	tx.ryw.mu.Lock()
	defer tx.ryw.mu.Unlock()
	return tx.ryw.liveLocked().conflictForKey(key)
}

// rangeConflictExtent clamps a completed GetRange's read-conflict to the data actually returned,
// mirroring libfdb_c's post-read addConflictRange (ReadYourWrites.actor.cpp:245-319) and the
// RYW-disabled native path (NativeAPI.actor.cpp:4558-4587). For a plain [begin,end) — both
// selectors firstGreaterOrEqual, offset +1 — the two C++ clamp sites reduce to one rule:
//   - forward, more, non-empty → [begin, keyAfter(lastReturnedKey))
//   - reverse, more, non-empty → [firstReturnedKey, end)
//   - !more or empty           → [begin, end)
//
// lastReturnedKey/firstReturnedKey are both kvs[len-1].Key: forward results are ascending (so
// kvs[last] is the highest) and reverse results descending (so kvs[last] is the lowest =
// C++ result.end()[-1].key; readpath.go:781). A more=true read genuinely did not observe the
// unread tail (forward) / head (reverse), so narrowing there cannot under-conflict; an empty or
// fully-drained (!more) read keeps the full [begin,end) so a concurrent insert ANYWHERE in the
// requested range still trips a conflict (phantom protection). more⇒non-empty for Go's row-limited
// GetRange (readpath.go:752-757,774), so the !more/empty arm is what fires on an empty read. (RFC-121 D1.)
func rangeConflictExtent(begin, end []byte, kvs []KeyValue, more, reverse bool) (cBegin, cEnd []byte) {
	if !more || len(kvs) == 0 {
		return begin, end
	}
	last := kvs[len(kvs)-1].Key
	if reverse {
		return last, end
	}
	return begin, keyAfterBytes(last)
}

const (
	smallConflictBufSize = 512
	bulkConflictBufSize  = 32768
)

// Small transactions avoid a 32 KiB cold allocation; batches promote directly
// to the large pool instead of growing through intermediate buffers.
var smallConflictBufPool = sync.Pool{
	New: func() any {
		return &conflictBuf{b: make([]byte, 0, smallConflictBufSize)}
	},
}

var conflictBufPool = sync.Pool{
	New: func() any {
		return &conflictBuf{b: make([]byte, 0, bulkConflictBufSize)}
	},
}

type conflictBuf struct {
	b []byte
	// Captured commits still reading this slab; while nonzero the transaction
	// may neither reuse it in place nor return it to the pool.
	borrows atomic.Int32
}

// dropConflictBufLocked drops the slab, pooling it unless it is over 32 KiB
// or an in-flight commit still reads it. Reports whether it was pooled.
func (tx *Transaction) dropConflictBufLocked() (pooled bool) {
	if pool := conflictBufPoolFor(cap(tx.conflictBuf)); pool != nil && cap(tx.conflictBuf) > 0 && !tx.conflictBufBorrowedLocked() {
		cb := tx.conflictBufOwner
		if cb == nil {
			cb = &conflictBuf{}
		}
		cb.b = tx.conflictBuf[:0]
		pool.Put(cb)
		pooled = true
	}
	tx.conflictBufOwner = nil
	tx.conflictBuf = nil
	return pooled
}

// conflictBufBorrowedLocked runs under conflictMu, which captureCommit also
// holds to borrow, so a zero count cannot become nonzero before the caller acts.
func (tx *Transaction) conflictBufBorrowedLocked() bool {
	return tx.conflictBufOwner != nil && tx.conflictBufOwner.borrows.Load() != 0
}

// Arenas over 32 KiB are not pooled: large values must not inflate later transactions.
func conflictBufPoolFor(size int) *sync.Pool {
	if size <= smallConflictBufSize {
		return &smallConflictBufPool
	}
	if size <= bulkConflictBufSize {
		return &conflictBufPool
	}
	return nil
}

// keyAfterBytes returns a copy of key with \x00 appended.
// Always allocates — safe for storing in conflict ranges.
func keyAfterBytes(key []byte) []byte {
	r := make([]byte, len(key)+1)
	copy(r, key)
	return r
}

// addReadConflictForKey adds a read conflict range [key, key\x00) using the
// shared conflictBuf. Zero allocs on the hot path (buffer pooled across txns).
func (tx *Transaction) addReadConflictForKey(key []byte) {
	n := len(key)
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		tx.rywConflictSize += int64(2*n+1) + sizeofKeyRangeRef
		tx.insertReadConflictLocked(rywBoundary{key: key}, rywBoundary{key: key, after: true})
		return
	}
	buf := tx.conflictBufAlloc(n + n + 1)
	copy(buf, key)
	copy(buf[n:], key)
	buf[2*n] = 0 // explicit zero — pooled buffer may have stale data
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: buf[:n:n], End: buf[n : n+n+1]})
}

// addReadConflict adds a read conflict range using the shared conflictBuf.
func (tx *Transaction) addReadConflict(begin, end []byte) {
	tx.conflictMu.Lock()
	tx.addReadConflictLocked(begin, end)
	tx.conflictMu.Unlock()
}

// addReadConflicts appends ALL of `ranges` under a SINGLE conflictMu acquisition. When the RYW
// write-map filter (conflictRangesLocked) splits one logical read conflict into several sub-ranges,
// they must land atomically: the published contract makes conflict-range adders and Commit safe to
// use concurrently, so a concurrent Commit snapshotting `readConflicts` mid-append would otherwise
// ship a transaction carrying only a PREFIX of the sub-ranges and silently drop the rest of the
// caller's read-conflict protection. One lock → the snapshot sees all sub-ranges or none.
func (tx *Transaction) addReadConflicts(ranges [][2][]byte) {
	if len(ranges) == 0 {
		return
	}
	tx.conflictMu.Lock()
	tx.addReadConflictsLocked(ranges)
	tx.conflictMu.Unlock()
}

func (tx *Transaction) addReadConflictsLocked(ranges [][2][]byte) {
	for _, r := range ranges {
		tx.addReadConflictLocked(r[0], r[1])
	}
}

// conflictBufAlloc reserves n bytes for conflict keys or mutation operands.
// Must be called with conflictMu held. A full chunk is left to the ranges and
// operands carved from it and a new one, twice its size, starts empty: an arena,
// as C++'s, never copies what it handed out.
func (tx *Transaction) conflictBufAlloc(n int) []byte {
	if cap(tx.conflictBuf)-len(tx.conflictBuf) < n {
		reused := false
		if len(tx.conflictBuf) == 0 || cap(tx.conflictBuf) <= smallConflictBufSize {
			need := len(tx.conflictBuf) + n
			if pool := conflictBufPoolFor(need); pool != nil {
				cb := pool.Get().(*conflictBuf)
				if cap(cb.b) >= n {
					// Existing ranges/mutations may alias the old chunk: it is not pooled.
					tx.conflictBuf = cb.b[:0]
					tx.conflictBufOwner = cb
					reused = true
				} else if returnPool := conflictBufPoolFor(cap(cb.b)); returnPool != nil {
					returnPool.Put(cb)
				}
			}
		}
		if !reused {
			newCap := max(2*cap(tx.conflictBuf), n)
			if newCap < 4096 {
				newCap = 4096 // ~30 typical keys
			}
			tx.conflictBuf = make([]byte, 0, newCap)
		}
	}
	start := len(tx.conflictBuf)
	tx.conflictBuf = tx.conflictBuf[:start+n]
	// Capped: a borrowed commit reads neighbouring bytes an append could clobber.
	return tx.conflictBuf[start : start+n : start+n]
}

// addWriteConflictForKey and addWriteConflict add an explicit native-list write conflict for the dummy
// transaction and tests, leaving NEXT_WRITE_NO_WRITE_CONFLICT_RANGE to the next write as RYW does.
func (tx *Transaction) addWriteConflictForKey(key []byte) {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !tx.writeConflictsDisabled {
		tx.appendWriteConflictPointListLocked(key)
	}
}

func (tx *Transaction) addWriteConflict(begin, end []byte) {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !tx.writeConflictsDisabled {
		tx.appendWriteConflictListLocked(begin, end, false)
	}
}

// appendWriteConflictPointListLocked appends [key, key\x00) to the native list,
// copied into the conflict arena. Caller holds conflictMu.
func (tx *Transaction) appendWriteConflictPointListLocked(key []byte) {
	n := len(key)
	buf := tx.conflictBufAlloc(n + n + 1)
	copy(buf, key)
	copy(buf[n:], key)
	buf[2*n] = 0 // explicit zero — pooled buffer may have stale data
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: buf[:n:n], End: buf[n : n+n+1]})
}

// selfConflictPrefix is C++ makeSelfConflicting's key prefix "\xFF/SC/" (NativeAPI.actor.cpp:5953).
var selfConflictPrefix = []byte("\xff/SC/")

// intersectRanges mirrors C++ intersects (NativeAPI.actor.cpp:6211-6228): it sorts
// both range vectors by begin, then walks them in a single linear pass, returning
// the FIRST overlapping pair's intersection [max(begins), min(ends)) — or ok=false
// when the vectors are disjoint. O(n log n) like the native client, NOT the O(w*r)
// nested scan that stalls large mixed commits (thousands of small point read/write
// conflict ranges still fit under the size limit). Sorts COPIES so it never reorders
// the live tx.readConflicts/writeConflicts slices (whose headers alias the conflictBuf
// arena) — C++ sorts the request's vectors in place only because they are already the
// committed snapshot at that call site; the intersection RESULT is identical either way.
func intersectRanges(lhs, rhs []KeyRange) (KeyRange, bool) {
	if len(lhs) == 0 || len(rhs) == 0 {
		return KeyRange{}, false
	}
	ls := append([]KeyRange(nil), lhs...)
	rs := append([]KeyRange(nil), rhs...)
	sort.Slice(ls, func(i, j int) bool { return bytes.Compare(ls[i].Begin, ls[j].Begin) < 0 })
	sort.Slice(rs, func(i, j int) bool { return bytes.Compare(rs[i].Begin, rs[j].Begin) < 0 })
	return intersectSorted(ls, rs)
}

// intersectSorted is intersectRanges's walk over vectors sorted by begin.
func intersectSorted(ls, rs []KeyRange) (KeyRange, bool) {
	l, r := 0, 0
	for l < len(ls) && r < len(rs) {
		switch {
		case bytes.Compare(ls[l].End, rs[r].Begin) <= 0: // ls[l] entirely before rs[r]
			l++
		case bytes.Compare(rs[r].End, ls[l].Begin) <= 0: // rs[r] entirely before ls[l]
			r++
		default:
			// Overlap. C++ returns lhs[l] & rhs[r] = [max(begins), min(ends)).
			begin := ls[l].Begin
			if bytes.Compare(rs[r].Begin, begin) > 0 {
				begin = rs[r].Begin
			}
			end := ls[l].End
			if bytes.Compare(rs[r].End, end) < 0 {
				end = rs[r].End
			}
			return KeyRange{Begin: begin, End: end}, true
		}
	}
	return KeyRange{}, false
}

// conflictRangesIntersect reports whether any write conflict range overlaps any read conflict range,
// mirroring C++ intersects(write_conflict_ranges, read_conflict_ranges).present() (NativeAPI.actor.cpp:6859).
// Only presence matters, so a vector already sorted by begin (a coalesced one) is walked
// as it is rather than copied and sorted.
func conflictRangesIntersect(writes, reads []KeyRange) bool {
	if len(writes) == 0 || len(reads) == 0 {
		return false
	}
	_, ok := intersectSorted(sortedByBegin(writes), sortedByBegin(reads))
	return ok
}

// sortedByBegin returns rs if it is sorted by begin, else a sorted copy.
func sortedByBegin(rs []KeyRange) []KeyRange {
	byBegin := func(a, b KeyRange) int { return bytes.Compare(a.Begin, b.Begin) }
	if slices.IsSortedFunc(rs, byBegin) {
		return rs
	}
	rs = slices.Clone(rs)
	slices.SortFunc(rs, byBegin)
	return rs
}

// selfConflictShipReads makes the commit self-conflicting when its frozen write
// and read conflicts do not intersect, and returns the read conflicts to ship
// with the self-conflicting read range last: C++ adds it to the native
// transaction after the RYW read conflicts (NativeAPI.actor.cpp:5952-5958).
func (tx *Transaction) selfConflictShipReads(writeSnap, shipReads []KeyRange) (reads []KeyRange, sc KeyRange, added bool) {
	sc, scRead, added := tx.maybeMakeSelfConflicting(writeSnap, shipReads)
	if added {
		shipReads = append(shipReads, scRead)
	}
	return shipReads, sc, added
}

// maybeMakeSelfConflicting decides on the read conflicts the commit ships and
// returns the write and read self-conflicting ranges it added.
// Only shipped conflicts may suppress the fence's synthetic key (NativeAPI.actor.cpp:6858).
// Tenant marshalling prefixes that key in both commits. Caller must not hold conflictMu.
func (tx *Transaction) maybeMakeSelfConflicting(writeSnap, readSnap []KeyRange) (write, read KeyRange, added bool) {
	// The snapshots are frozen: only the append needs the lock.
	if !conflictRangesIntersect(writeSnap, readSnap) {
		tx.conflictMu.Lock()
		defer tx.conflictMu.Unlock()
		write, read = tx.makeSelfConflictingLocked()
		return write, read, true
	}
	return KeyRange{}, KeyRange{}, false
}

// A synthetic key avoids fencing on a hot user key (NativeAPI.actor.cpp:5952-5959).
// It stays unprefixed until marshalling; caller must hold conflictMu.
func (tx *Transaction) makeSelfConflictingLocked() (write, read KeyRange) {
	sc := make([]byte, len(selfConflictPrefix)+16)
	copy(sc, selfConflictPrefix)
	binary.LittleEndian.PutUint64(sc[len(selfConflictPrefix):], rand.Uint64())
	binary.LittleEndian.PutUint64(sc[len(selfConflictPrefix)+8:], rand.Uint64())
	end := keyAfterBytes(sc)
	rb := tx.conflictBufAlloc(len(sc) + len(end))
	copy(rb, sc)
	copy(rb[len(sc):], end)
	read = KeyRange{Begin: rb[:len(sc):len(sc)], End: rb[len(sc):]}
	tx.readConflicts = append(tx.readConflicts, read)
	wb := tx.conflictBufAlloc(len(sc) + len(end))
	copy(wb, sc)
	copy(wb[len(sc):], end)
	wcr := KeyRange{Begin: wb[:len(sc):len(sc)], End: wb[len(sc):]}
	tx.writeConflicts = append(tx.writeConflicts, wcr)
	return wcr, read
}

// stateAddReadConflictRange returns only inverted_range, which libfdb_c raises from the
// KeyRangeRef built on the calling thread (fdb_c.cpp:1223, FDBTypes.h:318-323).
func (tx *Transaction) stateAddReadConflictRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
	}
	// libfdb_c runs the rest on the network thread and defers its throw, skipping the work once an
	// error is deferred (ThreadSafeTransaction.cpp:589-594, ThreadHelper.actor.h:50-61).
	if tx.deferredErr.Load() == nil {
		if poison := tx.addReadConflictRangeRYW(begin, end); poison != nil {
			tx.recordDeferredError(poison)
		}
	}
	return nil
}

// stateAddReadConflictKey is the range form over [key, keyAfter(key)), the C binding's key form
// (bindings/go/src/fdb/transaction.go:548-554).
func (tx *Transaction) stateAddReadConflictKey(key []byte) {
	if tx.deferredErr.Load() == nil {
		if poison := tx.addReadConflictKeyRYW(key); poison != nil {
			tx.recordDeferredError(poison)
		}
	}
}

// addReadConflictRangeRYW is RYW addReadConflictRange (ReadYourWrites.actor.cpp:1945-1987)
// for a range that is not inverted.
func (tx *Transaction) addReadConflictRangeRYW(begin, end []byte) *wire.FDBError {
	// C++ addReadConflictRange (ReadYourWrites.actor.cpp:1950-1955, apiVersionAtLeast(300)): reject a
	// range whose begin/end exceeds getMaxReadKey(), EXCEPT the exact metadataVersionKey range. Unlike
	// the write check (:2466-2468) it uses getMaxReadKey and has the exception.
	maxKey := tx.maxReadKey()
	if (bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0) &&
		!(bytes.Equal(begin, metadataVersionKeyBytes) && bytes.Equal(end, metadataVersionKeyEndBytes)) {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// C++ addReadConflictRange (ReadYourWrites.actor.cpp:1957-1975) then CLAMPS an oversized endpoint
	// to getMaxReadKeySize+1 bytes and DROPS the whole range if the clamp collapses it to empty.
	// getMaxReadKeySize == getMaxKeySize == getMaxClearKeySize (hasRawAccess is always true here). The
	// legal-range check above runs on the RAW keys first (C++ order); a non-system key >10 KB is < \xff
	// so it PASSES that check and reaches the clamp. There are no stored keys longer than the max, so
	// the truncated range is equivalent — but without this Go shipped the full oversized key to the
	// resolver (wire + tx-size divergence) instead of libfdb_c's truncation.
	if bmax := getMaxClearKeySize(begin); len(begin) > bmax {
		begin = begin[:bmax+1]
	}
	if emax := getMaxClearKeySize(end); len(end) > emax {
		end = end[:emax+1]
	}
	if bytes.Compare(begin, end) >= 0 {
		return nil // C++ r.empty() → returns without recording a conflict range
	}
	// C++ addReadConflictRange (ReadYourWrites.actor.cpp:1977-1986): when RYW is DISABLED, add the
	// full range directly (tr.addReadConflictRange, :1979); otherwise run updateConflictMap (:1986) —
	// the write-map filter that SUBTRACTS locally-written independent segments (a plain Set before
	// this explicit add was satisfied with no DB read, so it must not conflict). Mirrors the read
	// path (getRangeDir) and addGetKeyConflictRange; without it Go over-conflicts → spurious 1020.
	if tx.rywDisabled {
		tx.addReadConflict(begin, end)
		return nil
	}
	tx.addReadConflictRYW(begin, end) // atomic: a concurrent Commit sees all sub-ranges or none
	return nil
}

// addReadConflictKeyRYW is addReadConflictRangeRYW on [key, keyAfter(key)) without allocating
// keyAfter(key) or walking the write map.
func (tx *Transaction) addReadConflictKeyRYW(key []byte) *wire.FDBError {
	// keyAfter(key) is past maxReadKey iff key >= maxReadKey; the range is metadataVersionKey's iff key is.
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// Past the size limit the clamp cuts both endpoints to key[:max+1], an empty range (RYW:1962-1975).
	if len(key) > getMaxClearKeySize(key) {
		return nil
	}
	tx.addReadConflictForKeyRYW(key) // the single-key updateConflictMap
	return nil
}

// stateAddWriteConflictRange returns only inverted_range, which libfdb_c raises from the
// KeyRangeRef built on the calling thread (fdb_c.cpp:1223, FDBTypes.h:318-323). The rest runs
// as RYW addWriteConflictRange on the network thread, whose errors are deferred.
func (tx *Transaction) stateAddWriteConflictRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
	}
	tx.deferWriteConflictRange(begin, end, false)
	return nil
}

// stateAddWriteConflictKey copies key\x00 into the conflict arena once: the
// range [key, keyAfter(key)) and any clamp of it are owned slices of that copy.
func (tx *Transaction) stateAddWriteConflictKey(key []byte) {
	if tx.deferredErr.Load() != nil {
		return
	}
	n := len(key)
	var end []byte
	if tx.rywDisabled {
		tx.conflictMu.Lock()
		end = tx.conflictBufAlloc(n + 1)
		tx.conflictMu.Unlock()
		copy(end, key)
		end[n] = 0 // explicit zero — pooled buffer may have stale data
	} else {
		// The write map's nodes keep the bounds: copied into its arena, as C++
		// copies the range into the RYW arena (ReadYourWrites.actor.cpp:2498).
		end = tx.ryw.keepKeyAfter(key)
	}
	tx.deferWriteConflictRange(end[:n:n], end, true)
}

// deferWriteConflictRange is ThreadSafeTransaction::addWriteConflictRange
// (ThreadSafeTransaction.cpp:659-663): onMainThreadVoid records a throw as the deferred error
// and skips the work once one is set (ThreadHelper.actor.h:50-61).
// owned marks begin and end as bytes the transaction keeps and nobody writes: the conflict
// arena's without RYW, the write map's arena (keepLocked) with it.
func (tx *Transaction) deferWriteConflictRange(begin, end []byte, owned bool) {
	if tx.deferredErr.Load() != nil {
		return
	}
	if poison := tx.addWriteConflictRangeRYW(begin, end, owned); poison != nil {
		tx.recordDeferredError(poison)
	}
}

// addWriteConflictRangeRYW is RYW addWriteConflictRange (ReadYourWrites.actor.cpp:2461-2500)
// for a range that is not inverted.
func (tx *Transaction) addWriteConflictRangeRYW(begin, end []byte, owned bool) *wire.FDBError {
	// C++ addWriteConflictRange (ReadYourWrites.actor.cpp:2466-2468, apiVersionAtLeast(300)): reject a
	// range whose begin/end exceeds getMaxWriteKey() — note getMaxWriteKey (gated on writeSystemKeys),
	// NOT getMaxReadKey, and NO metadataVersionKey exception (asymmetric with addReadConflictRangeRYW).
	maxKey := tx.maxWriteKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// C++ addWriteConflictRange (ReadYourWrites.actor.cpp:2474-2492) CLAMPS an oversized endpoint to
	// getMaxKeySize+1 (== getMaxClearKeySize) and DROPS the range if the clamp collapses it to empty —
	// identical to addReadConflictRangeRYW. Without this Go shipped the full oversized key to the resolver.
	if bmax := getMaxClearKeySize(begin); len(begin) > bmax {
		begin = begin[:bmax+1]
	}
	if emax := getMaxClearKeySize(end); len(end) > emax {
		end = end[:emax+1]
	}
	if bytes.Compare(begin, end) >= 0 {
		return nil // C++ r.empty() → returns without recording a conflict range
	}
	// With RYW the range lives only in the write map (RYW:2493-2499), updated under
	// conflictMu with the decision; without it, in the native list (:2488-2491).
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if tx.explicitWriteConflictLocked(begin, end, owned) && !tx.rywDisabled {
		tx.ryw.addConflictRangeOwned(begin, end, owned)
	}
	return nil
}
