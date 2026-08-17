package simfdb

import (
	"bytes"
	"encoding/binary"
	"sort"
	"time"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// mutationKind tags a queued write in the RYW buffer.
type mutationKind int

const (
	mutSet mutationKind = iota
	mutClear
	mutClearRange
	mutAtomic
	mutVersionstampedKey
	mutVersionstampedValue
)

// mutation is one queued write. For mutClearRange, key/end bound the range; otherwise key is
// the target and value is the Set value / atomic param / versionstamp param. noWriteConflict
// records that SetNextWriteNoWriteConflictRange was active when this write was issued (the
// versionstamp path), so it adds no write conflict range here.
type mutation struct {
	kind            mutationKind
	key             []byte
	end             []byte
	value           []byte
	op              atomicOp
	noWriteConflict bool
}

// keyRange is a half-open [begin, end) byte range used for conflict tracking.
type keyRange struct{ begin, end []byte }

// simTxn is SimFDB's WritableTransaction. It is single-goroutine (the sim driver serializes
// use); the db mutex only guards the shared store/version counter against a concurrent commit.
type simTxn struct {
	db *SimDB

	// viewRowsTouched counts store entries examined while building range views. It makes
	// the per-page cost observable: a page is supposed to cost the page, not the tail
	// still ahead of it, and no assertion on the returned rows can tell the two apart.
	// Read only by tests.
	viewRowsTouched int

	// wholeViewBuilds counts UNBOUNDED buildView calls — the ones that clone, map and
	// sort the entire keyspace regardless of what the caller asked for. viewRowsTouched
	// cannot stand in for it: that counter measures how much of a RANGE a bounded build
	// walked, and the whole-keyspace build is the case where no range bounded anything.
	// Read only by tests.
	wholeViewBuilds int

	readVersion int64
	rvSet       bool
	// rvInstant is when the read version was pinned, on the ENV clock — the sim's
	// analog of the client's GRV stamp. It must come from env.Now() and never
	// time.Now(): a budget measured against it decides how much work a page does,
	// and a run whose page boundaries move with the host's speed is not a
	// deterministic replay. Zero until rvSet.
	rvInstant time.Time

	buffer         []mutation // RYW write buffer, in issue order
	readConflicts  []keyRange
	writeConflicts []keyRange

	// SetNextWriteNoWriteConflictRange arms this for exactly the next write (FDB semantics).
	nextWriteNoConflict bool
	// SetReadYourWritesDisable — record layer rarely sets it; when true, reads skip the buffer.
	rywDisabled bool
	// SetWriteConflictsDisabled — when true, writes add no write conflict ranges at all.
	writeConflictsDisabled bool

	// unreadable is the set of keys carrying a pending versionstamped op. A read that REACHES
	// one throws accessed_unreadable(1036): the stamp is assigned by the cluster at commit, so
	// the value is not knowable client-side and there is nothing honest to return. SimFDB used
	// to hand back the unstamped operand as though it were a real value — a phantom present key
	// whose contents no real client could ever have observed, and which the same transaction
	// would then find changed after commit.
	//
	// STICKY, mirroring C++ WriteMap.cpp:97 (`is_unreadable = it.is_unreadable() || op is a
	// versionstamped op`, ported at client/ryw.go:483-488): a later plain Set on the key does NOT
	// clear it (client/ryw.go:225-229 — the stack-replacing SetValue fast path is gated on
	// !is_unreadable, so the Set is pushed and the flag survives). Only a clear removes it
	// (client/ryw.go:243-244, 270-271) — a cleared key is readable because you know it is empty.
	unreadable map[string]struct{}

	// unreadableRanges are the SVK CANDIDATE STAMP ranges: sorted, non-overlapping. A
	// SetVersionstampedKey does not just make one key unknowable — nobody knows WHERE the
	// stamped key will land, only that it lands somewhere in [template@minStamp, template@\xff×10),
	// so every position in that span is unknowable and a read reaching ANY of them throws 1036.
	// C++ writes.addUnmodifiedAndUnreadableRange(getVersionstampKeyRange(...))
	// (ReadYourWrites.actor.cpp:2271 over Atomic.h:268-287), ported at client/ryw.go:1699
	// (addUnreadableRangeLocked into rywCache.unreadableRanges).
	//
	// Marking only the exact template key — which is what this modelled before — UNDER-THROWS:
	// a Get of a different key inside the candidate range returned a value where libfdb_c raises
	// accessed_unreadable, so a record-layer path that reads near its own pending stamp would
	// pass under the sim and fail against a real cluster.
	unreadableRanges []keyRange

	// SetBypassUnreadable (FDB_TR_OPTION_BYPASS_UNREADABLE): reads of unreadable keys return the
	// write-map value with the versionstamp placeholder bytes AS WRITTEN instead of throwing
	// (client/ryw.go:55-60).
	bypassUnreadable bool

	committed        bool
	committedVersion int64
	cancelled        bool

	// versionstamp assigned at commit (10-byte tx version), for GetVersionstamp.
	versionstamp []byte

	opts fdb.TransactionOptions
}

var (
	_ fdb.WritableTransaction        = (*simTxn)(nil)
	_ fdb.ReadVersionInstantReporter = (*simTxn)(nil)
)

func (db *SimDB) newTxn() *simTxn {
	tx := &simTxn{db: db}
	tx.opts = &txnOptions{tx: tx}
	return tx
}

// ensureReadVersion lazily pins the read version the first time the transaction reads or commits
// (matching a lazy GRV). The version comes from db.latestVersion — the highest committed version
// OR the simulated clock's, whichever is further along — because that is what a real GRV returns:
// the cluster's version advances with time even while idle, so a transaction that starts after a
// long quiet period does not read at the version of the last write.
func (tx *simTxn) ensureReadVersion() {
	if tx.rvSet {
		return
	}
	tx.db.mu.Lock()
	tx.readVersion = tx.db.latestVersion()
	tx.db.mu.Unlock()
	tx.rvSet = true
	tx.rvInstant = tx.db.env.Now()
}

// ReadVersionInstant implements fdb.ReadVersionInstantReporter. ok is false before
// the lazy GRV has fired, so a caller cannot mistake "not yet pinned" for "pinned
// at the zero time" — the distinction the whole capability exists to make.
func (tx *simTxn) ReadVersionInstant() (time.Time, bool) {
	if !tx.rvSet || tx.rvInstant.IsZero() {
		return time.Time{}, false
	}
	return tx.rvInstant, true
}

// ---- reads ----------------------------------------------------------------------------

// cloneVal returns a copy of v that preserves nil-ness: nil stays nil (absent/cleared), a
// present value — including an empty one — stays non-nil.
func cloneVal(v []byte) []byte {
	if v == nil {
		return nil
	}
	c := make([]byte, len(v))
	copy(c, v)
	return c
}

// presentVal copies v as a PRESENT value: a nil operand becomes an empty, non-nil slice.
//
// nil is SimFDB's spelling of "absent" everywhere below this line — store.put(k, nil) writes a
// tombstone, resolveKey returns nil for a cleared key, buildView drops nil-valued entries. A
// Set operand must therefore never be allowed to carry that meaning: there is no such thing as
// "Set to absent" in FDB. The C API takes a (pointer, length) pair, so a nil operand is
// indistinguishable from a zero-length one and both store a PRESENT, zero-length value; the
// pure-Go client makes that explicit — rywCache.set copies with `make([]byte, len(value))`
// (client/ryw.go:223-224), which turns nil into non-nil empty, and rywEntry documents
// `value==nil ("Set to empty bytes", a present key)` as distinct from its `absent` flag
// (client/ryw.go:88-91).
//
// Without the normalization one Set(k, nil) produced three different answers: Get saw nil and
// reported absent, GetRange's view map held the key with a nil value and reported it PRESENT,
// and the commit wrote a tombstone so the key vanished from the store. Normalizing at the write
// boundary — rather than teaching each of the three readers about a fourth state — is what makes
// them agree, and it is what the client does.
func presentVal(v []byte) []byte {
	c := make([]byte, len(v))
	copy(c, v)
	return c
}

// snapshotValue returns key's committed value at the read version, copied.
func (tx *simTxn) snapshotValue(key []byte) []byte {
	tx.db.mu.Lock()
	v := tx.db.store.valueAt(key, tx.readVersion)
	tx.db.mu.Unlock()
	return cloneVal(v)
}

// resolveKey computes the RYW-merged value of key: the snapshot value with the transaction's
// own pending writes replayed in order. nil means absent/cleared.
func (tx *simTxn) resolveKey(key []byte) []byte {
	cur := tx.snapshotValue(key)
	if tx.rywDisabled {
		return cur
	}
	for _, m := range tx.buffer {
		switch m.kind {
		case mutSet, mutVersionstampedValue:
			if bytes.Equal(m.key, key) {
				cur = cloneVal(m.value)
			}
		case mutVersionstampedKey:
			// The key isn't finalized until commit; a RYW read of the placeholder key sees
			// its param value. Exact-key match on the (still-placeholder) key.
			if bytes.Equal(m.key, key) {
				cur = cloneVal(m.value)
			}
		case mutClear:
			if bytes.Equal(m.key, key) {
				cur = nil
			}
		case mutClearRange:
			if bytes.Compare(key, m.key) >= 0 && bytes.Compare(key, m.end) < 0 {
				cur = nil
			}
		case mutAtomic:
			if bytes.Equal(m.key, key) {
				nv, clear := applyAtomic(m.op, cur, m.value)
				if clear {
					cur = nil
				} else {
					cur = nv
				}
			}
		}
	}
	return cur
}

// ---- unreadable (pending versionstamp) tracking -----------------------------------------

// markUnreadable records that a versionstamped op landed on key.
func (tx *simTxn) markUnreadable(key []byte) {
	if tx.unreadable == nil {
		tx.unreadable = make(map[string]struct{})
	}
	tx.unreadable[string(key)] = struct{}{}
}

// clearUnreadable makes key readable again. Called only from the clear paths: a cleared key is
// known to be empty, so there is nothing unknowable about it (client/ryw.go:243-244). The
// single-key clear also subtracts [key, key+\x00) from the candidate ranges, so clearing the one
// position inside an SVK span makes that position — and only it — readable.
func (tx *simTxn) clearUnreadable(key []byte) {
	delete(tx.unreadable, string(key))
	tx.subtractUnreadableRange(key, keyAfter(key))
}

// clearUnreadableRange makes every unreadable key in [begin, end) readable — the ClearRange half
// of the same rule (client/ryw.go:270-271, 280) — and subtracts the span from the SVK candidate
// ranges (client/ryw.go's subtractRangeList; C++ gets it free from the shared PTree, where
// clear() inserts readable entries over the span, WriteMap.cpp:195).
func (tx *simTxn) clearUnreadableRange(begin, end []byte) {
	for k := range tx.unreadable {
		if bytes.Compare([]byte(k), begin) >= 0 && bytes.Compare([]byte(k), end) < 0 {
			delete(tx.unreadable, k)
		}
	}
	tx.subtractUnreadableRange(begin, end)
}

// hasUnreadable reports whether the transaction carries any unreadable state at all — the
// fast-path guard every gate opens with (client/ryw.go:1853 tests BOTH populations; testing only
// the key set is how the range half stayed dead).
func (tx *simTxn) hasUnreadable() bool {
	return len(tx.unreadable) > 0 || len(tx.unreadableRanges) > 0
}

// addUnreadableRange merges [begin, end) into the sorted, non-overlapping candidate ranges.
// Port of client/ryw.go's addUnreadableRangeLocked (C++
// writes.addUnmodifiedAndUnreadableRange, ReadYourWrites.actor.cpp:2271).
func (tx *simTxn) addUnreadableRange(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	n := len(tx.unreadableRanges)
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].begin, end) > 0
	})
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].end, begin) >= 0
	})
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)
	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(tx.unreadableRanges[i].begin, newBegin) < 0 {
			newBegin = tx.unreadableRanges[i].begin
		}
		if bytes.Compare(tx.unreadableRanges[i].end, newEnd) > 0 {
			newEnd = tx.unreadableRanges[i].end
		}
	}
	merged := append([]keyRange(nil), tx.unreadableRanges[:loIdx]...)
	merged = append(merged, keyRange{begin: newBegin, end: newEnd})
	merged = append(merged, tx.unreadableRanges[hiIdx:]...)
	tx.unreadableRanges = merged
}

// subtractUnreadableRange removes [begin, end) from the candidate ranges, splitting any range
// that straddles the span. Port of client/ryw.go's subtractRangeList.
func (tx *simTxn) subtractUnreadableRange(begin, end []byte) {
	if len(tx.unreadableRanges) == 0 || bytes.Compare(begin, end) >= 0 {
		return
	}
	out := make([]keyRange, 0, len(tx.unreadableRanges)+1)
	for _, r := range tx.unreadableRanges {
		if bytes.Compare(r.end, begin) <= 0 || bytes.Compare(r.begin, end) >= 0 {
			out = append(out, r) // no overlap
			continue
		}
		if bytes.Compare(r.begin, begin) < 0 {
			out = append(out, keyRange{begin: r.begin, end: append([]byte(nil), begin...)})
		}
		if bytes.Compare(r.end, end) > 0 {
			out = append(out, keyRange{begin: append([]byte(nil), end...), end: r.end})
		}
	}
	tx.unreadableRanges = out
}

// inUnreadableRange reports whether key falls inside a candidate stamp range
// (client/ryw.go's isUnreadableLocked).
func (tx *simTxn) inUnreadableRange(key []byte) bool {
	// First range with end > key; key is inside iff that range's begin <= key.
	i := sort.Search(len(tx.unreadableRanges), func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].end, key) > 0
	})
	return i < len(tx.unreadableRanges) && bytes.Compare(tx.unreadableRanges[i].begin, key) <= 0
}

// firstUnreadableRangeIn returns the smallest position in [begin, end) covered by a candidate
// range, or nil. Port of client/ryw.go's firstUnreadableInLocked.
func (tx *simTxn) firstUnreadableRangeIn(begin, end []byte) []byte {
	i := sort.Search(len(tx.unreadableRanges), func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].end, begin) > 0
	})
	if i < len(tx.unreadableRanges) && bytes.Compare(tx.unreadableRanges[i].begin, end) < 0 {
		// The intersection starts at max(range.begin, begin).
		if bytes.Compare(tx.unreadableRanges[i].begin, begin) > 0 {
			return tx.unreadableRanges[i].begin
		}
		return begin
	}
	return nil
}

// lastUnreadableRangeIn returns the exclusive END of the last candidate range intersecting
// [begin, end), or nil — the reverse-scan counterpart. Port of client/ryw.go's
// lastUnreadableInLocked.
func (tx *simTxn) lastUnreadableRangeIn(begin, end []byte) []byte {
	i := sort.Search(len(tx.unreadableRanges), func(i int) bool {
		return bytes.Compare(tx.unreadableRanges[i].begin, end) >= 0
	}) - 1
	if i < 0 || bytes.Compare(tx.unreadableRanges[i].end, begin) <= 0 {
		return nil
	}
	if bytes.Compare(tx.unreadableRanges[i].end, end) < 0 {
		return tx.unreadableRanges[i].end
	}
	return end
}

// isUnreadable reports whether a read of key must throw accessed_unreadable — either because the
// key itself carries a pending versionstamped op, or because it sits inside an SVK candidate
// stamp range whose winner could land exactly there.
func (tx *simTxn) isUnreadable(key []byte) bool {
	if tx.bypassUnreadable || !tx.hasUnreadable() {
		return false
	}
	if _, ok := tx.unreadable[string(key)]; ok {
		return true
	}
	return tx.inUnreadableRange(key)
}

// unreadableScanCap returns the position at which a scan of [begin, end) must stop because of a
// pending versionstamped op, or nil if there is none in the window. A port of the client's
// unreadableScanCapLocked (client/ryw.go:1805-1839):
//
//   - forward: the SMALLEST unreadable key in the window becomes the scan's exclusive END, so
//     the key itself is excluded.
//   - reverse: the LARGEST one, and its cap is keyAfter(k) because that becomes the scan's
//     INCLUSIVE begin — without the +\x00 the reverse scan would return the very key it must
//     not read. Only the largest matters: a reverse scan walks down from end, so the highest
//     unreadable key is the first one it can reach and everything below hides behind it.
//
// Both populations participate: the pending-op entry keys AND the SVK candidate stamp ranges.
func (tx *simTxn) unreadableScanCap(begin, end []byte, reverse bool) []byte {
	if tx.bypassUnreadable || !tx.hasUnreadable() {
		return nil
	}
	// The SVK candidate ranges cap the scan too, and they cap it EARLIER than any entry key: a
	// range's head holds no write-map key at all, so a walk that consulted only the entry set
	// would sail through the unknowable span and report rows for positions the stamp may occupy
	// (client/ryw.go:1858/1867 folds both populations into one cap for the same reason).
	// The two populations produce caps in the SAME coordinate — a scan bound — but reach it
	// differently: a range already carries an exclusive end, while an entry key needs the
	// keyAfter(+\x00) on the reverse path to fall below the window. Convert first, combine
	// second; combining raw positions would apply the +\x00 to a range end and let a reverse
	// scan read the range's last position.
	var entryPos []byte
	for k := range tx.unreadable {
		kb := []byte(k)
		if bytes.Compare(kb, begin) < 0 || bytes.Compare(kb, end) >= 0 {
			continue
		}
		switch {
		case entryPos == nil:
			entryPos = kb
		case reverse && bytes.Compare(kb, entryPos) > 0:
			entryPos = kb
		case !reverse && bytes.Compare(kb, entryPos) < 0:
			entryPos = kb
		}
	}
	if entryPos != nil && reverse {
		entryPos = keyAfter(entryPos)
	}
	var rangePos []byte
	if reverse {
		rangePos = tx.lastUnreadableRangeIn(begin, end)
	} else {
		rangePos = tx.firstUnreadableRangeIn(begin, end)
	}
	switch {
	case entryPos == nil:
		return rangePos
	case rangePos == nil:
		return entryPos
	case reverse:
		// Reverse: the scan walks down from end, so the HIGHER cap is the first thing it meets.
		if bytes.Compare(rangePos, entryPos) > 0 {
			return rangePos
		}
		return entryPos
	default:
		if bytes.Compare(rangePos, entryPos) < 0 {
			return rangePos
		}
		return entryPos
	}
}

func (tx *simTxn) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return tx.get(key, false)
}

func (tx *simTxn) get(key fdb.KeyConvertible, snapshot bool) fdb.FutureByteSlice {
	if tx.cancelled {
		return newReadyByteSlice(nil, fdb.Error{Code: 1025}) // transaction_cancelled
	}
	tx.ensureReadVersion()
	k := []byte(key.FDBKey())
	// The unreadable gate fires BEFORE any conflict range is recorded and before the value is
	// resolved, exactly as the client's does (client/ryw.go:511-517 throws at the top of get,
	// ahead of the server read). The read did not happen, so it takes no conflict.
	if tx.isUnreadable(k) {
		return newReadyByteSlice(nil, fdb.Error{Code: 1036}) // accessed_unreadable
	}
	if !snapshot {
		tx.addFilteredReadConflictKey(k)
	}
	return newReadyByteSlice(tx.resolveKey(k), nil)
}

// buildView returns the RYW-merged, sorted, live keyspace at the read version. Used to resolve
// key selectors for GetKey and GetRange. A transient map coalesces per-key state; the result
// is re-sorted so iteration order is deterministic regardless of map order.
func (tx *simTxn) buildView() []fdb.KeyValue {
	tx.wholeViewBuilds++
	m := make(map[string][]byte)
	tx.db.mu.Lock()
	for _, e := range tx.db.store.entries {
		if v := tx.db.store.valueAtEntry(e, tx.readVersion); v != nil {
			m[string(e.key)] = cloneVal(v)
		}
	}
	tx.db.mu.Unlock()
	if !tx.rywDisabled {
		for _, mut := range tx.buffer {
			applyMutationToView(m, mut)
		}
	}
	return sortedView(m)
}

// buildViewRange is buildView restricted to [begin, end) — the RYW-merged, sorted, live rows in
// that window as of the write buffer's CURRENT contents.
//
// This is what a single fetch resolves against, and it is deliberately recomputed per fetch
// rather than shared: the whole point of a lazy range read is that a write issued between two
// fetches is visible to the second one. Windowing is not just an optimization over buildView
// either — the store scan is a binary search plus a walk to `end`, so a batch costs the size of
// the window it reads rather than the size of the whole keyspace.
func (tx *simTxn) buildViewRange(begin, end []byte) []fdb.KeyValue {
	view, _ := tx.buildViewRangeLimited(begin, end, 0, false)
	return view
}

// buildViewRangeLimited is buildViewRange bounded by demand: it resolves only as much of
// [begin, end) as it takes to answer for `want` rows from the `reverse` end, and reports
// whether the range was consumed in full. want <= 0 is unbounded.
//
// Why this exists: every cursor fetch calls this, and the unbounded form cloned, mapped and
// SORTED the entire unconsumed range before slicing off one page. Bounding the ITERATOR
// progression turned a large drain into linearly many pages, so linearly many full builds —
// a quadratic drain, which is what makes big deterministic-sim workloads impractical. The
// sim's job is to mirror the real backend's observable semantics, above all its batch
// BOUNDARIES; it is not obliged to mirror how much it copies to find them.
//
// The merge is what needs care, and it is handled by the store's sub-window rather than by
// truncating a merged stream. A bounded store read is COMPLETE within [coveredBegin,
// coveredEnd) and says nothing outside it, so folding in exactly the mutations that overlap
// that sub-window yields the same rows the whole-range build would have produced there — a
// write above it cannot belong in a forward page it sorts after, and a clear above it cannot
// remove a row from one. What the sub-window cannot predict is how many rows SURVIVE the
// merge: a clear inside it can leave the page short. So the budget grows geometrically until
// the page is filled or the range is exhausted, which costs O(final window) in total rather
// than O(range) per page.
func (tx *simTxn) buildViewRangeLimited(begin, end []byte, want int, reverse bool) ([]fdb.KeyValue, bool) {
	if bytes.Compare(begin, end) >= 0 {
		return nil, true
	}
	budget := want
	for {
		tx.db.mu.Lock()
		rows, cBegin, cEnd, exhausted, touched := tx.db.store.rangeAtLimited(
			begin, end, tx.readVersion, budget, reverse)
		tx.db.mu.Unlock()
		tx.viewRowsTouched += touched

		m := make(map[string][]byte, len(rows))
		for _, kv := range rows {
			m[string(kv.Key)] = kv.Value
		}
		if !tx.rywDisabled {
			for _, mut := range tx.buffer {
				// Mutations outside the window are skipped so they cannot introduce a key
				// beyond it; applyMutationToView's clear-range arm only walks keys already
				// in the map, so an overlapping clear range trims to the intersection on
				// its own. The window is the store's covered sub-window, not the caller's
				// range: outside it the store result is not complete, so a mutation there
				// has nothing well-defined to apply to.
				if mutationOverlaps(mut, cBegin, cEnd) {
					applyMutationToView(m, mut)
				}
			}
		}
		view := sortedView(m)
		// Stop as soon as the REQUESTED page is filled. Testing against the current
		// (doubled) budget instead is what makes a buffered clear pathological: the store
		// hands back `budget` live rows, the clear removes some, so len(view) lands just
		// under budget at every retry and the loop widens until the tail is exhausted —
		// one clear turned a 1024-row page over 10k rows into 25,360 rows examined, and
		// clears spread through a saturated drain put the quadratic back, worse than the
		// one-pass build this replaced. Headroom past `want` is not information anyone
		// asked for.
		//
		// Stopping at `want` is sound because the sub-window makes the short view a
		// PREFIX (forward) or SUFFIX (reverse) of the full merged view, not an arbitrary
		// subset of it. Forward, coveredBegin is always `begin`, so V_sub is exactly the
		// rows of V_full below coveredEnd; reverse, coveredEnd is always `end`, so V_sub
		// is exactly the rows at or above coveredBegin. Both are complete within that
		// span. So once |V_sub| >= want, every row of the requested page already lies
		// inside the covered span, and no row outside it can displace one: a key the
		// buffer ADDS outside the span sorts past every row in V_sub (beyond coveredEnd
		// forward, below coveredBegin reverse), hence past the page; a clear outside the
		// span can only remove rows that were already not in the page. Widening further
		// would return the same `want` rows after more work.
		if exhausted || want <= 0 || len(view) >= want {
			return view, exhausted
		}
		// Still short of the page: a clear inside the sub-window removed rows the store
		// had counted. Widen and re-derive rather than guess how many were lost.
		budget *= 2
	}
}

// mutationOverlaps reports whether mut can change the contents of [begin, end).
func mutationOverlaps(mut mutation, begin, end []byte) bool {
	if mut.kind == mutClearRange {
		return bytes.Compare(mut.key, end) < 0 && bytes.Compare(begin, mut.end) < 0
	}
	return bytes.Compare(mut.key, begin) >= 0 && bytes.Compare(mut.key, end) < 0
}

// sortedView turns the transient per-key map into the sorted slice the readers consume. The
// re-sort is what makes iteration order deterministic regardless of Go's randomized map order.
func sortedView(m map[string][]byte) []fdb.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]fdb.KeyValue, len(keys))
	for i, k := range keys {
		out[i] = fdb.KeyValue{Key: fdb.Key(k), Value: m[k]}
	}
	return out
}

// rangeRows resolves ONE fetch: the RYW-merged rows of [begin, end) as of now, in the read's own
// direction, truncated to limit. `more` reports that the limit was MET, not that more rows are
// known to exist — C++ getExactRange sets output.more = (data.size() == limit) unconditionally
// at the limit (client/readpath.go:745-751 ports this), because a real storage server stops at
// the limit and cannot know whether the next key exists. SimFDB holds the whole result set and
// could tell, but reporting more=false at exactly-the-limit is a DIVERGENCE with two
// consequences: the conflict extent stays the full requested range instead of narrowing to the
// returned data (over-conflict), and a cursor over SimFDB sees a different exhaustion signal
// than over real FDB.
func (tx *simTxn) rangeRows(begin, end []byte, limit int, reverse bool) ([]fdb.KeyValue, bool) {
	// Bounded by the page the caller asked for: rangeRows is the cursor fetch path, so an
	// unbounded build here is what made a saturated drain quadratic. more is still
	// len(rows) >= limit, which the bounded build preserves — it returns fewer than limit
	// rows only when it exhausted the range.
	rows, _ := tx.buildViewRangeLimited(begin, end, limit, reverse)
	if reverse {
		reverseKVs(rows)
	}
	if limit > 0 && len(rows) >= limit {
		return rows[:limit], true
	}
	return rows, false
}

// fetchRange is ONE round-trip of a range read: cap the window at any pending versionstamped op,
// resolve the rows over what remains, and decide whether iteration REACHED the cap. It is the
// sim's rywCache.getRange (client/ryw.go:645-700) and, like it, runs once per FETCH — so the cap
// is recomputed for every batch, and a versionstamped write landing mid-scan truncates the
// batches after it while leaving the ones already returned alone.
//
// cBegin/cEnd are the window the fetch ACTUALLY read, and they are returned because that is what
// the caller must record its read conflict over. The client caps begin/end before issuing the
// read for exactly that reason (client/ryw.go:667-673); conflicting over the REQUESTED window
// instead claims a read of keys past the cap that provably never happened.
// byteTarget is the reply's per-fetch byte budget (fdb.ByteLimitUnlimited for none). It is
// applied HERE rather than by the caller because it has to take effect before the
// unreadable-cap predicate below reads `more`: on a real cluster the storage server truncates
// the reply, so a byte-limited fetch comes back short WITH more=true, and the client's
// "would iteration continue into the unreadable position?" test sees a filled batch. Cutting
// the rows after fetchRange returned would leave `more` describing the untruncated read, and a
// scan that should have crossed a batch boundary and continued would instead raise
// accessed_unreadable on its first fetch having yielded nothing.
func (tx *simTxn) fetchRange(begin, end []byte, limit int, byteTarget int, reverse bool) (
	kvs []fdb.KeyValue, more bool, cBegin, cEnd []byte, err error,
) {
	cBegin, cEnd = begin, end
	// Truncate at the first (forward) / last (reverse) key carrying a pending versionstamped op.
	// unreadableScanCap already honours BYPASS_UNREADABLE and returns nil when nothing
	// unreadable intersects the window.
	capKey := tx.unreadableScanCap(begin, end, reverse)
	if capKey != nil {
		if reverse {
			// The cap is keyAfter(the unreadable key), so it becomes the INCLUSIVE begin and the
			// unreadable key itself falls below the window.
			cBegin = capKey
		} else {
			cEnd = capKey
		}
	}
	// Bound the STORE WALK by the byte target as well as the row budget. Without this an
	// unlimited cursor asks for its whole remaining row budget (effectiveLimit(0) = MaxInt32)
	// and the walk clones, maps and sorts the entire remaining tail on EVERY fetch, with the
	// byte cut trimming it afterwards — a saturated drain then costs O(n) per page and O(n^2)
	// overall. The real client does not have this problem because the row budget it puts on the
	// wire is clamped and the STORAGE SERVER stops reading at the byte limit; this is the
	// simulator's equivalent of that stop.
	//
	// The ceiling can never cut a row the server would have returned: the cheapest possible row
	// costs 1 (shortest key) + 0 (empty value) + serverRowOverheadBytes against the budget, so
	// no reply limited to byteTarget can contain more than byteTarget/minRowCost + 1 rows.
	// serverByteCut below still decides the actual boundary — this only stops the walk.
	//
	// WHY A ROW CEILING HERE RATHER THAN A BYTE-AWARE WALK, which looks tidier: the walk reads
	// STORED rows, while the division must be taken over the MERGED rows the fetch returns —
	// local writes add, remove and resize rows before the boundary is decided. Byte accounting
	// inside the walk would therefore be measuring a different collection from the one the
	// boundary is about, and serverByteCut would STILL have to make the real cut afterwards.
	// That splits the authority for the batch boundary across two accountings over two
	// different row sets, which is exactly how the two drift apart later. This ceiling is a
	// pure performance guard that provably cannot move the boundary, so the boundary keeps a
	// single definition.
	if byteTarget != fdb.ByteLimitUnlimited {
		const minRowCost = 1 + serverRowOverheadBytes
		if maxRows := byteTarget/minRowCost + 1; maxRows < limit {
			limit = maxRows
		}
	}
	kvs, more = tx.rangeRows(cBegin, cEnd, limit, reverse)
	// Stand in for the storage server truncating the reply at the request's byte limit.
	if cut := serverByteCut(kvs, byteTarget); cut < len(kvs) {
		kvs = kvs[:cut]
		more = true
	}
	if capKey == nil {
		return kvs, more, cBegin, cEnd, nil
	}
	// "reached" is the client's predicate (client/ryw.go:676-681): iteration of the capped window
	// would continue INTO the unreadable position unless the limit was filled strictly inside it,
	// and an unlimited scan always reaches. rangeRows' `more` is precisely "the limit was filled",
	// so !more IS that predicate. The client discards the partial rows on this branch too
	// (client/ryw.go:686-688 returns nil alongside the error rather than the prefix it read).
	if !more {
		return nil, false, nil, nil, fdb.Error{Code: 1036} // accessed_unreadable
	}
	// The scan stopped of its own accord strictly inside the truncated window, so there is
	// provably more beyond it — the client reports more unconditionally once a cap applies
	// (client/ryw.go:692, `|| unreadableCap != nil`).
	return kvs, true, cBegin, cEnd, nil
}

// applyMutationToView folds one pending mutation into the merged-view map.
func applyMutationToView(m map[string][]byte, mut mutation) {
	switch mut.kind {
	case mutSet, mutVersionstampedKey, mutVersionstampedValue:
		m[string(mut.key)] = cloneVal(mut.value)
	case mutClear:
		delete(m, string(mut.key))
	case mutClearRange:
		for k := range m {
			if bytes.Compare([]byte(k), mut.key) >= 0 && bytes.Compare([]byte(k), mut.end) < 0 {
				delete(m, k)
			}
		}
	case mutAtomic:
		nv, clear := applyAtomic(mut.op, m[string(mut.key)], mut.value)
		if clear {
			delete(m, string(mut.key))
		} else {
			m[string(mut.key)] = nv
		}
	}
}

// resolveSelector returns the index into view of the key selected by ks. The result may be < 0
// (before the first key) or >= len(view) (past the last key). FDB selector arithmetic: base =
// keyAfter(key) if OrEqual else key; i = first index with view[i].Key >= base; target = i +
// Offset - 1.
func resolveSelector(view []fdb.KeyValue, ks fdb.KeySelector) int {
	base := []byte(ks.Key.FDBKey())
	if ks.OrEqual {
		base = keyAfter(base)
	}
	i := sort.Search(len(view), func(i int) bool {
		return bytes.Compare(view[i].Key, base) >= 0
	})
	return i + ks.Offset - 1
}

// endKeyMarker is the resolved key returned when a selector points past the last key: the
// exclusive upper bound of the user keyspace (\xff), matching FDB's clamp.
var endKeyMarker = fdb.Key{0xff}

func (tx *simTxn) GetKey(sel fdb.Selectable) fdb.FutureKey {
	return tx.getKey(sel, false)
}

func (tx *simTxn) getKey(sel fdb.Selectable, snapshot bool) fdb.FutureKey {
	if tx.cancelled {
		return newReadyKey(nil, fdb.Error{Code: 1025})
	}
	tx.ensureReadVersion()
	ks := sel.FDBKeySelector()
	view := tx.buildView()
	// A selector resolution is a READ of the keyspace it walks, so it is subject to the same
	// unreadable gate — C++ RYWIterator's type()/kv() throw from the offset walk itself
	// (RYWIterator.cpp:45-46/:75-76), which is what the client's getKeyRYW inherits. The span
	// walked runs between the selector's anchor and the key it lands on, in whichever direction
	// the offset points; if a pending versionstamped op sits anywhere in it, the walk cannot
	// know how many slots it just crossed, because it does not know where the stamped key will
	// land.
	if idx := resolveSelector(view, ks); !tx.bypassUnreadable && tx.hasUnreadable() {
		anchor := []byte(ks.Key.FDBKey())
		var lo, hi []byte
		switch {
		case idx < 0:
			lo, hi = []byte{}, keyAfter(anchor)
		case idx >= len(view):
			lo, hi = anchor, append([]byte(nil), endKeyMarker...)
		case bytes.Compare(view[idx].Key, anchor) < 0:
			lo, hi = []byte(view[idx].Key), keyAfter(anchor)
		default:
			lo, hi = anchor, keyAfter([]byte(view[idx].Key))
		}
		if bytes.Compare(lo, hi) < 0 && tx.unreadableScanCap(lo, hi, false) != nil {
			return newReadyKey(nil, fdb.Error{Code: 1036}) // accessed_unreadable
		}
	}
	idx := resolveSelector(view, ks)
	var result fdb.Key
	switch {
	case idx < 0:
		result = fdb.Key{} // before the first key: empty key
	case idx >= len(view):
		result = endKeyMarker
	default:
		result = append(fdb.Key(nil), view[idx].Key...)
	}
	if !snapshot {
		tx.addGetKeyConflictRange(ks, []byte(result))
	}
	return newReadyKey(result, nil)
}

// addGetKeyConflictRange records the read-conflict range a non-snapshot GetKey takes when it
// resolves ks to resolved — a port of the client's addGetKeyConflictRange
// (C++ addConflictRange(GetKeyReq)). The span runs between the selector base and the resolved
// key, ORIENTED BY THE OFFSET SIGN: a backward selector (offset <= 0) resolves BELOW the base,
// so the span is [resolved, base); a naive [base, keyAfter(resolved)) would be inverted and add
// no conflict at all on reverse cursors.
//
// GetRange shares this: for a bound the real backend has to resolve with an actual GetKey, the
// resolution ITSELF is a read and takes this range. See the call site there.
func (tx *simTxn) addGetKeyConflictRange(ks fdb.KeySelector, resolved []byte) {
	selKey := []byte(ks.Key.FDBKey())
	var cBegin, cEnd []byte
	if ks.Offset <= 0 {
		cBegin = resolved
		if ks.OrEqual {
			cEnd = keyAfter(selKey)
		} else {
			cEnd = selKey
		}
	} else {
		if ks.OrEqual {
			cBegin = keyAfter(selKey)
		} else {
			cBegin = selKey
		}
		cEnd = keyAfter(resolved)
	}
	if bytes.Compare(cBegin, cEnd) < 0 {
		tx.addFilteredReadConflict(cBegin, cEnd)
	}
}

// selectorResolvesViaGetKey reports whether the real backend has to issue an actual GetKey to
// turn ks into a byte key, mirroring fdb/range_result.go resolveSelector's two client-side
// short-circuits: FirstGreaterOrEqual(k) IS k, and FirstGreaterThan(k) is keyAfter(k). Every
// other selector costs a GetKey — which is a READ, and therefore takes a conflict range.
func selectorResolvesViaGetKey(ks fdb.KeySelector) bool {
	if !ks.OrEqual && ks.Offset == 1 { // FirstGreaterOrEqual
		return false
	}
	if ks.OrEqual && ks.Offset == 1 { // FirstGreaterThan
		return false
	}
	return true
}

func (tx *simTxn) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return tx.getRange(r, options, false)
}

// getRange returns a LAZY handle. Nothing is read here — not the rows, not the bounds, not even
// the read version — exactly as the client's GetRange returns a bare goRangeResult{tx, r,
// options} (fdb/range_result.go:39-45). All of it happens at consumption, in
// resolveRangeForRead and rangeRows. See simRangeResult for why that ordering is the whole
// point.
func (tx *simTxn) getRange(r fdb.Range, options fdb.RangeOptions, snapshot bool) fdb.RangeResult {
	return &simRangeResult{tx: tx, r: r, options: options, snapshot: snapshot}
}

// resolveRangeForRead turns a range request's selectors into the raw [begin, end) a fetch reads
// over, and records the conflict ranges that resolution itself takes. It is the sim's
// resolveRange (fdb/range_result.go), and like it, it runs at CONSUMPTION — once per
// GetSliceWithError call and once per Iterator() call — never at GetRange() time.
func (tx *simTxn) resolveRangeForRead(r fdb.Range, snapshot bool) (begin, end []byte, err error) {
	// transaction_cancelled(1025) before anything else, as on every other read entry point: the
	// client reaches it through ensureReadVersion, whose FIRST act is checkCancelled
	// (client/transaction.go:662-665), and getRangeDir calls ensureReadVersion before its own
	// legal-range and limit validation (client/transaction.go:1252-1268). Checking it HERE
	// rather than in getRange also matches the client's timing: a transaction cancelled after
	// GetRange() but before the result is consumed fails the read, because the read had not
	// happened yet.
	if tx.cancelled {
		return nil, nil, fdb.Error{Code: 1025}
	}
	tx.ensureReadVersion()
	beginSel, endSel := r.FDBRangeKeySelectors()
	bks, eks := beginSel.FDBKeySelector(), endSel.FDBKeySelector()

	// The bounds are the RESOLVED keys, not the raw selector anchors: the real backend resolves
	// a non-trivial selector to a key BEFORE issuing the read (fdb/range_result.go
	// resolveRange). Taking the anchor instead under-conflicts on a backward selector —
	// LastLessThan(k) resolves BELOW k, so rows the transaction really read fall outside the
	// recorded range and a concurrent write to them does not conflict.
	// Built only when a bound actually needs it. buildView clones, maps and SORTS the
	// WHOLE keyspace, and resolveRangeBound's two trivial arms return before reading a
	// single element of it — so for every exact range in the tree (KeyRange, subspace,
	// Tuple all report FirstGreaterOrEqual bounds) this was an O(keyspace·log keyspace)
	// build per GetRange whose result was discarded unread. It measured 17.7% of total
	// CPU in `sort.Strings` alone on a 20k-row SimFDB scan benchmark, on the read path
	// that every SimFDB-backed test drives.
	//
	// selectorResolvesViaGetKey is the same predicate resolveRangeBound branches on, not
	// a second opinion about it, which is what makes the nil safe: a selector it reports
	// false for cannot reach the view, and the conflict loop below only resolves the ones
	// it reports true for.
	var view []fdb.KeyValue
	if selectorResolvesViaGetKey(bks) || selectorResolvesViaGetKey(eks) {
		view = tx.buildView()
	}
	begin = resolveRangeBound(view, bks)
	end = resolveRangeBound(view, eks)

	if !snapshot {
		// Resolving a non-trivial bound is itself a READ. The real backend turns such a bound
		// into a key by issuing a non-snapshot GetKey (fdb/range_result.go resolveRange ->
		// resolveSelector), and that GetKey takes its own conflict range over the span it had
		// to scan. Omitting it under-conflicts in a way the range extent alone cannot express:
		// when both bounds resolve to the SAME key the range is empty and contributes nothing,
		// yet the resolution still read the keyspace between each anchor and its resolved key.
		// A concurrent write there conflicts on real FDB and did not here.
		for _, ks := range [2]fdb.KeySelector{bks, eks} {
			if selectorResolvesViaGetKey(ks) {
				tx.addGetKeyConflictRange(ks, resolveRangeBound(view, ks))
			}
		}
	}

	// The client's guard: an INVERTED requested range records no data conflict at all
	// (client/transaction.go:1290-1293). Without it a degenerate or inverted [begin,end)
	// reaches the conflict list, where rangesOverlap can still match it against a real write
	// range and abort a transaction that read nothing. Collapse it to an empty extent, which
	// also makes every fetch over it return nothing.
	if bytes.Compare(begin, end) > 0 {
		begin, end = nil, nil
	}
	return begin, end, nil
}

// resolveRangeBound returns the byte key a GetRange bound resolves to, mirroring the real
// backend's resolveSelector case for case (fdb/range_result.go:309-355) so the conflict
// extent SimFDB records is the one the client would have recorded:
//
//   - FirstGreaterOrEqual(k) is trivial and resolves to k with no lookup. Every exact range
//     in the tree (KeyRange, subspace, Tuple) reports its bounds as FirstGreaterOrEqual of
//     its raw keys, so this arm is also what keeps phantom protection intact for the common
//     case: [a,z) stays [a,z) including its leading and trailing gaps, rather than narrowing
//     to the first and last keys that happen to exist. The backend's separate ExactRange
//     short-circuit is a round-trip optimization it needs and SimFDB does not — there is no
//     GetKey to avoid here, and adding the branch would only duplicate this arm.
//   - FirstGreaterThan(k) (OrEqual && Offset==1) resolves to keyAfter(k) in the KEY SPACE,
//     not to the next key that exists. The distinction is observable: resolving to an
//     existing key would pull the gap between k and that key into the conflict range and
//     over-conflict on an insert there.
//   - everything else (LastLessThan / LastLessOrEqual / any other offset) needs the GetKey
//     resolution, which is index-based over the merged view, with GetKey's own clamps: the
//     empty key before the first key, endKeyMarker past the last.
func resolveRangeBound(view []fdb.KeyValue, ks fdb.KeySelector) []byte {
	// The two trivial arms are gated on selectorResolvesViaGetKey rather than on a
	// second copy of its predicates: getRange SKIPS building the view when that
	// function reports false for both bounds, so a drift between the two would not
	// be a wrong answer, it would be a nil view reaching the index walk below.
	if !selectorResolvesViaGetKey(ks) {
		if ks.OrEqual { // FirstGreaterThan(k) == FirstGreaterOrEqual(k+\x00)
			return keyAfter([]byte(ks.Key.FDBKey()))
		}
		return []byte(ks.Key.FDBKey()) // FirstGreaterOrEqual — the key itself
	}
	switch idx := resolveSelector(view, ks); {
	case idx < 0:
		return []byte{}
	case idx >= len(view):
		return append([]byte(nil), endKeyMarker...)
	default:
		return append([]byte(nil), view[idx].Key...)
	}
}

// rangeConflictExtent clamps a GetRange's read-conflict range exactly as the pure-Go client and
// libfdb_c do (client transaction.go rangeConflictExtent). begin/end are the requested range
// bounds. A !more (exhausted) or empty read keeps the full [begin,end) — phantom protection; only
// a limit-truncated non-empty read narrows to the returned data (forward: [begin,keyAfter(last));
// reverse: [first,end)). kvs[len-1] is the highest returned (forward) or lowest (reverse).
func rangeConflictExtent(begin, end []byte, kvs []fdb.KeyValue, more, reverse bool) (cBegin, cEnd []byte) {
	if !more || len(kvs) == 0 {
		return begin, end
	}
	last := []byte(kvs[len(kvs)-1].Key)
	if reverse {
		return last, end
	}
	return begin, keyAfter(last)
}

func reverseKVs(kvs []fdb.KeyValue) {
	for i, j := 0, len(kvs)-1; i < j; i, j = i+1, j-1 {
		kvs[i], kvs[j] = kvs[j], kvs[i]
	}
}

func (tx *simTxn) GetReadVersion() fdb.FutureInt64 {
	// The client's GetReadVersion is ensureReadVersion + a field read
	// (client/transaction.go:2435-2447), and ensureReadVersion opens with checkCancelled
	// (client/transaction.go:662-665).
	if tx.cancelled {
		return newReadyInt64(0, fdb.Error{Code: 1025})
	}
	tx.ensureReadVersion()
	return newReadyInt64(tx.readVersion, nil)
}

func (tx *simTxn) Snapshot() fdb.ReadTransaction {
	return simSnapshot{tx: tx}
}

// simSnapshot is the Snapshot() view of a transaction. It HOLDS the transaction — the shape of
// the real client's snapshot struct (fdb/snapshot.go: `type snapshot struct { tx *transaction }`,
// every method reaching through sn.s.tx) — rather than copying it.
//
// A value copy is not a snapshot, it is a FORK, and each forked field breaks a different
// invariant:
//
//   - readVersion/rvSet: a copy that pins the GRV first leaves the parent to pin its own, later,
//     one. The parent then resolves its read conflicts against a version NEWER than the one the
//     snapshot read at, so a concurrent write in between is invisible to the resolver and the
//     transaction commits on stale data — a silent lost update where real FDB returns 1020. The
//     record layer writes exactly that shape: BunchedMap.Put snapshot-reads the bunch, then
//     writes the merged bunch back under an explicit read conflict on the same key.
//   - buffer: the copy freezes the write buffer at its length as of Snapshot(). Writes issued on
//     the parent afterwards are then invisible to snapshot reads — but a real FDB snapshot read
//     is read-your-writes (SNAPSHOT_RYW_ENABLE is the default); snapshot suppresses CONFLICT
//     RANGES, never the RYW merge.
//   - opts: the copy's opts still points at the parent's txnOptions, so an option set through the
//     snapshot handle lands on the parent and the copy's own flag fields silently disagree.
//
// Holding the transaction makes all three correct by construction: there is one readVersion, one
// buffer, one option set. The only difference between the two views is that reads through this
// one pass snapshot=true and so add no read-conflict range.
type simSnapshot struct{ tx *simTxn }

var _ fdb.ReadTransaction = simSnapshot{}

func (s simSnapshot) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return s.tx.get(key, true)
}

func (s simSnapshot) GetKey(sel fdb.Selectable) fdb.FutureKey {
	return s.tx.getKey(sel, true)
}

func (s simSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return s.tx.getRange(r, options, true)
}

func (s simSnapshot) GetReadVersion() fdb.FutureInt64 { return s.tx.GetReadVersion() }

// Snapshot of a snapshot is the same view (fdb/snapshot.go: `func (sn Snapshot) Snapshot()
// ReadTransaction { return sn }`).
func (s simSnapshot) Snapshot() fdb.ReadTransaction { return s }

func (s simSnapshot) GetEstimatedRangeSizeBytes(r fdb.ExactRange) fdb.FutureInt64 {
	return s.tx.GetEstimatedRangeSizeBytes(r)
}

func (s simSnapshot) GetRangeSplitPoints(r fdb.ExactRange, chunkSize int64) fdb.FutureKeyArray {
	return s.tx.GetRangeSplitPoints(r, chunkSize)
}

// Options reaches through to the transaction's option handle, as the real client's
// Snapshot.Options does (goTransactionOptions{tx: sn.s.tx}): options are a property of the
// transaction, not of the view.
func (s simSnapshot) Options() fdb.TransactionOptions { return s.tx.Options() }

func (s simSnapshot) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return fn(s)
}

func (tx *simTxn) GetEstimatedRangeSizeBytes(r fdb.ExactRange) fdb.FutureInt64 {
	begin, end := r.FDBRangeKeys()
	b, e := []byte(begin.FDBKey()), []byte(end.FDBKey())
	// The client's precedence, verbatim (client/metrics.go:29-38): inverted_range(2005) FIRST —
	// libfdb_c's KeyRangeRef constructor throws before the metric op runs — and only then
	// transaction_cancelled(1025), which this path must gate EXPLICITLY because it never fetches a
	// read version and so never passes through ensureReadVersion's checkCancelled. Adding the
	// cancel gate without the 2005 above it would invert the order the client reports.
	if bytes.Compare(b, e) > 0 {
		return newReadyInt64(0, fdb.Error{Code: 2005}) // inverted_range
	}
	if tx.cancelled {
		return newReadyInt64(0, fdb.Error{Code: 1025})
	}
	tx.ensureReadVersion()
	tx.db.mu.Lock()
	kvs := tx.db.store.rangeAt(b, e, tx.readVersion)
	tx.db.mu.Unlock()
	var total int64
	for _, kv := range kvs {
		total += int64(len(kv.Key) + len(kv.Value))
	}
	return newReadyInt64(total, nil)
}

func (tx *simTxn) GetRangeSplitPoints(r fdb.ExactRange, chunkSize int64) fdb.FutureKeyArray {
	// Same entry-point precedence as its sibling GetEstimatedRangeSizeBytes
	// (client/metrics.go:177-187): inverted_range(2005), then transaction_cancelled(1025), gated
	// explicitly because this path fetches no read version either.
	begin, end := r.FDBRangeKeys()
	if bytes.Compare([]byte(begin.FDBKey()), []byte(end.FDBKey())) > 0 {
		return newReadyKeyArray(nil, fdb.Error{Code: 2005}) // inverted_range
	}
	if tx.cancelled {
		return newReadyKeyArray(nil, fdb.Error{Code: 1025})
	}
	// Single logical shard: no interior split points (begin/end only, per FDB when the range
	// is smaller than a shard). v1 returns the empty set of interior boundaries.
	return newReadyKeyArray(nil, nil)
}

func (tx *simTxn) Options() fdb.TransactionOptions { return tx.opts }

// ---- writes ---------------------------------------------------------------------------

// enqueue appends a mutation and (unless armed off) its write conflict range. For a point
// write the range is [key, keyAfter(key)); for a clear range it is [begin, end).
func (tx *simTxn) enqueue(m mutation) {
	m.noWriteConflict = tx.nextWriteNoConflict
	tx.nextWriteNoConflict = false // arms exactly one write (FDB semantics)
	tx.buffer = append(tx.buffer, m)
	if !m.noWriteConflict && !tx.writeConflictsDisabled {
		switch m.kind {
		case mutClearRange:
			tx.addWriteConflict(m.key, m.end)
		default:
			tx.addWriteConflict(m.key, keyAfter(m.key))
		}
	}
}

func (tx *simTxn) Set(key fdb.KeyConvertible, value []byte) {
	// presentVal, not cloneVal: Set(k, nil) writes a present empty value, never a tombstone.
	tx.enqueue(mutation{kind: mutSet, key: []byte(key.FDBKey()), value: presentVal(value)})
}

func (tx *simTxn) Clear(key fdb.KeyConvertible) {
	k := []byte(key.FDBKey())
	// A clear is the ONE thing that makes a key readable again: the transaction knows it is
	// empty, so there is no pending stamp to be ignorant of (client/ryw.go:243-244).
	tx.clearUnreadable(k)
	tx.enqueue(mutation{kind: mutClear, key: k})
}

func (tx *simTxn) ClearRange(er fdb.ExactRange) {
	b, e := er.FDBRangeKeys()
	begin, end := []byte(b.FDBKey()), []byte(e.FDBKey())
	tx.clearUnreadableRange(begin, end)
	tx.enqueue(mutation{kind: mutClearRange, key: begin, end: end})
}

func (tx *simTxn) atomic(op atomicOp, key fdb.KeyConvertible, param []byte) {
	tx.enqueue(mutation{kind: mutAtomic, op: op, key: []byte(key.FDBKey()), value: cloneVal(param)})
}
func (tx *simTxn) Add(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicAdd, key, param) }
func (tx *simTxn) And(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicAnd, key, param) }
func (tx *simTxn) BitAnd(key fdb.KeyConvertible, param []byte)  { tx.atomic(atomicAnd, key, param) }
func (tx *simTxn) Or(key fdb.KeyConvertible, param []byte)      { tx.atomic(atomicOr, key, param) }
func (tx *simTxn) BitOr(key fdb.KeyConvertible, param []byte)   { tx.atomic(atomicOr, key, param) }
func (tx *simTxn) Xor(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicXor, key, param) }
func (tx *simTxn) BitXor(key fdb.KeyConvertible, param []byte)  { tx.atomic(atomicXor, key, param) }
func (tx *simTxn) Max(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicMax, key, param) }
func (tx *simTxn) Min(key fdb.KeyConvertible, param []byte)     { tx.atomic(atomicMin, key, param) }
func (tx *simTxn) ByteMax(key fdb.KeyConvertible, param []byte) { tx.atomic(atomicByteMax, key, param) }
func (tx *simTxn) ByteMin(key fdb.KeyConvertible, param []byte) { tx.atomic(atomicByteMin, key, param) }
func (tx *simTxn) AppendIfFits(key fdb.KeyConvertible, param []byte) {
	tx.atomic(atomicAppendIfFits, key, param)
}

func (tx *simTxn) CompareAndClear(key fdb.KeyConvertible, param []byte) {
	tx.atomic(atomicCompareAndClear, key, param)
}

// SetVersionstampedKey / SetVersionstampedValue make their key UNREADABLE for the rest of the
// transaction: the cluster fills the stamp at commit, so no client can know the resulting bytes
// beforehand, and a read must say so rather than invent an answer (client/ryw.go:483-488).
// SetVersionstampedKey additionally marks the whole CANDIDATE STAMP RANGE unreadable and buffers
// the mutation at the key TRANSFORMED with the min-bound stamp — both halves of C++ RYW::atomicOp
// (ReadYourWrites.actor.cpp:2263-2277, over getVersionstampKeyRange in Atomic.h:268-300), ported
// at client/transaction.go:1553-1613. The transform is invisible after commit (the proxy
// overwrites [pos,pos+10) with the assigned stamp either way) but it decides WHERE the pending
// entry sits during the transaction, which is what the range head's boundaries are measured from.
func (tx *simTxn) SetVersionstampedKey(key fdb.KeyConvertible, param []byte) {
	k := []byte(key.FDBKey())
	// C++ captures getCachedReadVersion().orDefault(0): a transaction that has not yet pinned a
	// read version stamps from 0, so the candidate range starts at the all-zero stamp.
	minVersion := int64(0)
	if tx.rvSet {
		minVersion = tx.readVersion
	}
	// The legal-write-range guard is C++'s (:2266 rejects an out-of-legal-range key BEFORE
	// transforming). SimFDB models no system keyspace — every option that would widen it is a
	// no-op here — so the bound is the plain user-keyspace end, the same key endKeyMarker names.
	if bytes.Compare(k, []byte(endKeyMarker)) < 0 {
		if rb, re, transformed, ok := versionstampKeyRange(k, minVersion, []byte(endKeyMarker)); ok {
			k = transformed
			tx.addUnreadableRange(rb, re)
		}
	}
	tx.markUnreadable(k)
	tx.enqueue(mutation{kind: mutVersionstampedKey, key: k, value: cloneVal(param)})
}

// versionstampKeyRange ports C++ getVersionstampKeyRange plus the in-place key transform
// (Atomic.h:258-300), identically to client/transaction.go:1623-1655. `key` carries a trailing
// 4-byte little-endian offset naming the position of its 10-byte placeholder. Returns the
// candidate stamp range [key@stamp(minVersion,0), key@\xff×10 + \x00) clamped to maxKey, and the
// key transformed with the min-bound stamp — offset suffix PRESERVED, because the suffix is what
// the commit path reads to find the placeholder again. ok=false on a malformed key.
func versionstampKeyRange(key []byte, minVersion int64, maxKey []byte) (begin, end, transformed []byte, ok bool) {
	if len(key) < 4 {
		return nil, nil, nil, false
	}
	pos := int(int32(binary.LittleEndian.Uint32(key[len(key)-4:])))
	// pos > len-14 rather than pos+10 > len-4: the subtraction form cannot overflow for any
	// int32 pos on a 32-bit int.
	if pos < 0 || pos > len(key)-4-10 {
		return nil, nil, nil, false
	}
	begin = append([]byte(nil), key[:len(key)-4]...)
	placeVersionstamp(begin[pos:], minVersion, 0)
	// end = key[:len-3] with a trailing 0x00 and \xff×10 at pos (Atomic.h:277-284).
	end = append([]byte(nil), key[:len(key)-3]...)
	end[len(end)-1] = 0x00
	for i := 0; i < 10; i++ {
		end[pos+i] = 0xff
	}
	if bytes.Compare(end, maxKey) > 0 {
		end = append([]byte(nil), maxKey...)
	}
	transformed = append([]byte(nil), key...)
	placeVersionstamp(transformed[pos:], minVersion, 0)
	return begin, end, transformed, true
}

// placeVersionstamp writes the 10-byte versionstamp: 8-byte BIG-endian version followed by a
// 2-byte BIG-endian transaction number (Atomic.h:243-256).
func placeVersionstamp(dst []byte, version int64, txnNumber uint16) {
	binary.BigEndian.PutUint64(dst[:8], uint64(version))
	binary.BigEndian.PutUint16(dst[8:10], txnNumber)
}

func (tx *simTxn) SetVersionstampedValue(key fdb.KeyConvertible, param []byte) {
	k := []byte(key.FDBKey())
	tx.markUnreadable(k)
	tx.enqueue(mutation{kind: mutVersionstampedValue, key: k, value: cloneVal(param)})
}

// []byte fast-path overloads: delegate to the KeyConvertible forms (fdb.Key is a KeyConvertible).
func (tx *simTxn) SetBytes(key, value []byte)         { tx.Set(fdb.Key(key), value) }
func (tx *simTxn) ClearBytes(key []byte)              { tx.Clear(fdb.Key(key)) }
func (tx *simTxn) AddBytes(key, param []byte)         { tx.Add(fdb.Key(key), param) }
func (tx *simTxn) MaxBytes(key, param []byte)         { tx.Max(fdb.Key(key), param) }
func (tx *simTxn) MinBytes(key, param []byte)         { tx.Min(fdb.Key(key), param) }
func (tx *simTxn) CompareAndClearBytes(key, p []byte) { tx.CompareAndClear(fdb.Key(key), p) }

// ---- conflict ranges ------------------------------------------------------------------

func (tx *simTxn) addReadConflict(begin, end []byte) {
	tx.readConflicts = append(tx.readConflicts, keyRange{append([]byte(nil), begin...), append([]byte(nil), end...)})
}

func (tx *simTxn) addWriteConflict(begin, end []byte) {
	tx.writeConflicts = append(tx.writeConflicts, keyRange{append([]byte(nil), begin...), append([]byte(nil), end...)})
}

// AddReadConflictRange records an EXPLICIT read conflict. It is filtered through the write map
// exactly like an implicit one: C++ addReadConflictRange runs updateConflictMap
// (ReadYourWrites.actor.cpp:1986, ported at client/transaction.go:3140), because a segment the
// transaction already wrote independently was satisfied with no database read whether the
// caller asked for the conflict or not.
func (tx *simTxn) AddReadConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	begin, end, ok, err := clampConflictRange([]byte(b.FDBKey()), []byte(e.FDBKey()))
	if err != nil || !ok {
		return err
	}
	tx.addFilteredReadConflict(begin, end)
	return nil
}

func (tx *simTxn) AddReadConflictKey(key fdb.KeyConvertible) error {
	tx.addFilteredReadConflictKey([]byte(key.FDBKey()))
	return nil
}

// AddWriteConflictRange records an EXPLICIT write conflict, validated exactly as the client
// validates it (client/transaction.go:3153-3176).
//
// An UNVALIDATED inverted range is not inert here — it is actively harmful. rangesOverlap
// (conflict.go) is the plain half-open predicate `a.begin < b.end && b.begin < a.end`, which an
// inverted [hi, lo) satisfies against any read range that STRADDLES it: an explicit write
// conflict of ["n","c") "overlaps" a reader's ["a","z") and aborts it with not_committed(1020).
// So the sim answered a caller error with a spurious conflict verdict — in the harness whose one
// job is to certify conflict verdicts.
func (tx *simTxn) AddWriteConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	begin, end, ok, err := clampConflictRange([]byte(b.FDBKey()), []byte(e.FDBKey()))
	if err != nil || !ok {
		return err
	}
	tx.addWriteConflict(begin, end)
	return nil
}

func (tx *simTxn) AddWriteConflictKey(key fdb.KeyConvertible) error {
	k := []byte(key.FDBKey())
	tx.addWriteConflict(k, keyAfter(k))
	return nil
}

// ---- lifecycle ------------------------------------------------------------------------

func (tx *simTxn) Commit() fdb.FutureNil {
	if tx.cancelled {
		return newReadyNil(fdb.Error{Code: 1025})
	}
	if tx.committed && len(tx.buffer) == 0 {
		// Idempotent no-op: real FDB resets the transaction to active+empty after a
		// successful commit (client postCommitReset), so committing again with nothing new
		// buffered succeeds as a no-op. The record layer relies on this — the SQL DDL path
		// commits inside a db.Run closure (ddl.go:829 txn.Commit()), then Run's Transact
		// commits the same transaction again.
		return newReadyNil(nil)
	}
	if tx.committed {
		// Re-commit newly-buffered mutations. The read version and conflict ranges were
		// already cleared by postCommitReset when the previous commit succeeded, so this
		// commit resolves as the fresh logical transaction it is.
		tx.committed = false
	}
	if err := tx.db.commit(tx); err != nil {
		return newReadyNil(err)
	}
	return newReadyNil(nil)
}

// postCommitReset returns a successfully-committed handle to the state a NEW logical
// transaction starts in, mirroring the client's Transaction.postCommitReset
// (client/transaction.go:3198-3213). Both the read version AND the three buffers go:
// clearing the buffers alone leaves the handle reading at its pre-commit snapshot, which
// breaks read-your-writes across the commit boundary AND makes the handle's next commit
// conflict against its own previous one (its commit version is greater than the stale read
// version, and its read-conflict ranges are still attached).
//
// committedVersion and versionstamp deliberately SURVIVE: GetCommittedVersion and
// GetVersionstamp are defined on a committed handle, and the client keeps them for the
// same reason.
func (tx *simTxn) postCommitReset() {
	tx.readVersion = 0
	tx.rvSet = false
	tx.rvInstant = time.Time{}
	tx.buffer = nil
	tx.unreadable = nil // the stamps are resolved; the keys are readable again
	tx.unreadableRanges = nil
	tx.readConflicts = nil
	tx.writeConflicts = nil
}

func (tx *simTxn) Cancel() { tx.cancelled = true }

// Reset returns the transaction to a fresh state for reuse (matching FDB Transaction.reset):
// clears the buffer, conflict ranges, and read version; keeps the db handle.
func (tx *simTxn) Reset() {
	tx.readVersion = 0
	tx.rvSet = false
	tx.rvInstant = time.Time{}
	tx.buffer = nil
	tx.unreadable = nil
	tx.unreadableRanges = nil
	tx.readConflicts = nil
	tx.writeConflicts = nil
	tx.nextWriteNoConflict = false
	tx.committed = false
	tx.committedVersion = 0
	tx.cancelled = false
	tx.versionstamp = nil
}

// maybeCommitted reports whether code is in C++'s FDB_ERROR_PREDICATE_MAYBE_COMMITTED set —
// the errors after which the transaction's mutations MAY already be durable. Same two codes
// the client's OnError special-cases (client/transaction.go:2382, ErrCommitUnknownResult /
// ErrClusterVersionChanged).
func maybeCommitted(code int) bool {
	return code == 1021 || code == 1039
}

// OnError classifies e: for a retryable code it resets the transaction (fresh read version,
// dropped buffer) and resolves success so the runner retries; otherwise it resolves the error.
// Matches FDB Transaction.onError.
//
// A MAYBE_COMMITTED error additionally makes the retry SELF-CONFLICTING: the write conflict
// ranges of the attempt that may have landed are carried over as READ conflict ranges, so if
// that commit did land, the retry sees its own write as a concurrent one and aborts 1020
// instead of applying the mutations a second time. This is the client's behaviour
// (client/transaction.go:2382-2400) and it is the only thing that makes a non-idempotent
// mutation — an Add — survive a 1021 retry with the right value.
//
// The sim is synchronous, so the specific interleaving the client guards against — a commit
// still in flight at the retry's GRV — cannot arise here. The port is load-bearing anyway,
// because the promoted ranges are ORDINARY read conflicts and are resolved as such: a
// write-only transaction (an atomic Add takes no read conflict of its own) that is retried
// after a maybe-committed error carries a read conflict on the keys it wrote, so a concurrent
// writer landing after the retry's read version aborts it. Without the promotion that retry
// has an empty read-conflict set and overwrites the concurrent write — a lost update SimFDB
// would certify and a real client would not.
func (tx *simTxn) OnError(e fdb.Error) fdb.FutureNil {
	if !fdb.IsOnErrorRetryable(e.Code) {
		return newReadyNil(e)
	}
	if !maybeCommitted(e.Code) {
		tx.Reset()
		return newReadyNil(nil)
	}
	selfConflicts := make([]keyRange, len(tx.writeConflicts))
	for i, wr := range tx.writeConflicts {
		selfConflicts[i] = keyRange{
			begin: append([]byte(nil), wr.begin...),
			end:   append([]byte(nil), wr.end...),
		}
	}
	tx.Reset()
	tx.readConflicts = append(tx.readConflicts, selfConflicts...)
	return newReadyNil(nil)
}

func (tx *simTxn) SetReadVersion(version int64) {
	tx.readVersion = version
	tx.rvSet = true
	// A caller-supplied version's MVCC window opened on the cluster at an
	// instant this process never observed, so there is no anchor to report.
	// Clearing (rather than leaving a prior GRV's stamp) keeps "the instant
	// describes the CURRENT read version" true — a stale stamp reads as a
	// window that is still open, and a budget anchored on it under-counts the
	// transaction's age. rvSet stays true, so the accessor's rvSet gate cannot
	// stand in for this clear; the zero instant is what makes it report
	// ok=false. Same contract as the pure client (client/transaction.go
	// SetReadVersion), which a backend of the same interface owes.
	tx.rvInstant = time.Time{}
}

// ---- post-commit ----------------------------------------------------------------------

func (tx *simTxn) GetCommittedVersion() (int64, error) {
	if !tx.committed {
		return -1, fdb.Error{Code: 2017}
	}
	return tx.committedVersion, nil
}

// GetVersionstamp returns a LAZY future: the record layer obtains it BEFORE commit
// (database.go:372) and resolves it AFTER commit, so it must read the versionstamp at Get()
// time, not at construction. A ready future capturing the (nil) pre-commit stamp would surface
// as used_during_commit(2017) on the metadata-version-stamp path.
func (tx *simTxn) GetVersionstamp() fdb.FutureKey {
	return &lazyVersionstamp{tx: tx}
}

type lazyVersionstamp struct{ tx *simTxn }

func (f *lazyVersionstamp) Get() (fdb.Key, error) {
	// transaction_cancelled(1025) out-ranks the not-yet-committed verdict, as in the client
	// (client/transaction.go:2217-2219: checkCancelled precedes the hasCommitted check).
	if f.tx.cancelled {
		return nil, fdb.Error{Code: 1025}
	}
	if !f.tx.committed || f.tx.versionstamp == nil {
		return nil, fdb.Error{Code: 2017}
	}
	return append(fdb.Key(nil), f.tx.versionstamp...), nil
}

func (f *lazyVersionstamp) MustGet() fdb.Key {
	k, err := f.Get()
	if err != nil {
		panic(err)
	}
	return k
}

func (f *lazyVersionstamp) BlockUntilReady() {}
func (f *lazyVersionstamp) IsReady() bool    { return f.tx.committed }
func (f *lazyVersionstamp) Cancel()          {}

// GetApproximateSize returns the transaction's size the way the client's RYW counter does
// (client/transaction.go:2510-2543): the commit accounting — every mutation charged
// sizeof(MutationRef), every read and write conflict range charged sizeof(KeyRangeRef) — with one
// correction. C++ models a single-key clear as a write-map RANGE entry and charges its MUTATION
// half sizeof(KeyRangeRef) rather than sizeof(MutationRef) (ReadYourWrites.actor.cpp:2431), so
// each one is refunded the difference. The commit-time 2101 gate deliberately does NOT apply that
// refund — in the native commit a single-key clear IS a ClearRange mutation charged the full 44 —
// which is why the two callers share commitSize but not this line.
//
// Deliberately NOT gated on Cancel(): C++ gates getApproximateSize on the deferred error and
// nothing else (ThreadSafeTransaction.cpp:715-721 — no resetPromise race), so a cancelled txn
// still reports its size. See TestCancelledTransactionApproximateSizeStillAnswers.
func (tx *simTxn) GetApproximateSize() fdb.FutureInt64 {
	var singleKeyClears int64
	for _, m := range tx.buffer {
		if m.kind == mutClear {
			singleKeyClears++
		}
	}
	return newReadyInt64(commitSize(tx)-singleKeyClears*(sizeofMutationRef-sizeofKeyRangeRef), nil)
}

// A transaction is itself a Transactor (the interface embeds ReadTransactor via
// ReadTransaction, and WritableTransaction is used where a Transactor is expected in the
// record layer's nested-context paths): run fn against itself, no new transaction.
func (tx *simTxn) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return fn(tx)
}

func (tx *simTxn) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return fn(tx)
}
