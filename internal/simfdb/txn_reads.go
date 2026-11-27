package simfdb

import (
	"bytes"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

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

func (tx *simTxn) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return tx.get(key, false)
}

func (tx *simTxn) get(key fdb.KeyConvertible, snapshot bool) fdb.FutureByteSlice {
	if tx.deferredErr != nil {
		return newReadyByteSlice(nil, tx.deferredErr)
	}
	if tx.cancelled.Load() {
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
	value := tx.resolveKey(k)
	tx.rememberRead(k, keyAfter(k))
	return newReadyByteSlice(value, nil)
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
// sim's rywCache.issueRange + run (client/ryw.go) and, like it, runs once per FETCH — so the cap
// is recomputed for every batch, and a versionstamped write landing mid-scan truncates the
// batches after it while leaving the ones already returned alone.
//
// cBegin/cEnd are the window the fetch ACTUALLY read, and they are returned because that is what
// the caller must record its read conflict over. The client caps begin/end before issuing the
// read for exactly that reason (rywRangeRead.run); conflicting over the REQUESTED window
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
	if tx.deferredErr != nil {
		return nil, false, nil, nil, tx.deferredErr
	}
	// Every fetch is a read call of its own and fails once the transaction is cancelled.
	if tx.cancelled.Load() {
		return nil, false, nil, nil, fdb.Error{Code: 1025}
	}
	defer func() {
		if err == nil {
			tx.rememberRangeRead(cBegin, cEnd, kvs, more, reverse)
		}
	}()
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
	if tx.deferredErr != nil {
		return newReadyKey(nil, tx.deferredErr)
	}
	if tx.cancelled.Load() {
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
	knownBegin, knownEnd := selectorReadSpan(view, removeOrEqual(ks))
	tx.rememberRead(knownBegin, knownEnd)
	return newReadyKey(result, nil)
}

// addGetKeyConflictRange records the read-conflict range a non-snapshot GetKey takes when it
// resolves ks to resolved — a port of the client's addGetKeyConflictRange
// (C++ addConflictRange(GetKeyReq)). The span runs between the selector base and the resolved
// key, ORIENTED BY THE OFFSET SIGN: a backward selector (offset <= 0) resolves BELOW the base,
// so the span is [resolved, base); a naive [base, keyAfter(resolved)) would be inverted and add
// no conflict at all on reverse cursors.
//
// A selector range read takes it only for the getKey readThrough issues with RYW disabled
// (selectorFetch); otherwise its selectors are resolved within the read.
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

func (tx *simTxn) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return tx.getRange(r, options, false)
}

// getRange fetches batch 1 at the call, as the client's GetRange does; later batches are
// fetched at consumption. See simRangeResult.
func (tx *simTxn) getRange(r fdb.Range, options fdb.RangeOptions, snapshot bool) fdb.RangeResult {
	rr := &simRangeResult{tx: tx, r: r, options: options, snapshot: snapshot}
	rr.issueFirst()
	return rr
}

// resolveRangeForRead turns a range whose selectors are both trivial (isTrivialSelector) into
// the raw [begin, end) a fetch reads over. It is the client's exact-range path
// (fdb/range_result.go resolveRange): firstGreaterOrEqual(k) is k and firstGreaterThan(k) is
// keyAfter(k), with no read. A range with any other selector is a selector read
// (selectorFetch). It runs once per GetRange() call.
func (tx *simTxn) resolveRangeForRead(r fdb.Range) (begin, end []byte, err error) {
	if tx.deferredErr != nil {
		return nil, nil, tx.deferredErr
	}
	// After deferred admission, cancellation precedes range resolution (C++ RYW getRange).
	if tx.cancelled.Load() {
		return nil, nil, fdb.Error{Code: 1025}
	}
	tx.ensureReadVersion()
	beginSel, endSel := r.FDBRangeKeySelectors()
	// No view: trivial bounds never read one. Building it cloned, mapped and SORTED the whole
	// keyspace per GetRange, measured at 17.7% of total CPU in `sort.Strings` alone on a 20k-row
	// SimFDB scan benchmark (TestTrivialRangeBoundsBuildNoWholeKeyspaceView).
	begin = resolveRangeBound(nil, beginSel.FDBKeySelector())
	end = resolveRangeBound(nil, endSel.FDBKeySelector())

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

// resolveRangeBound returns the byte key a range bound resolves to over view:
//
//   - FirstGreaterOrEqual(k) is k with no lookup. Every exact range in the tree (KeyRange,
//     subspace, Tuple) reports FirstGreaterOrEqual bounds, so this arm also keeps phantom
//     protection for the common case: [a,z) stays [a,z) including its leading and trailing
//     gaps, rather than narrowing to the first and last keys that happen to exist.
//   - FirstGreaterThan(k) (OrEqual && Offset==1) resolves to keyAfter(k) in the KEY SPACE,
//     not to the next key that exists. Resolving to an existing key would pull the gap
//     between k and that key into the range.
//   - every other selector is resolved over the merged view, with getKey's clamps: the empty
//     key before the first key, endKeyMarker past the last. view must be built for these.
func resolveRangeBound(view []fdb.KeyValue, ks fdb.KeySelector) []byte {
	if isTrivialSelector(ks) {
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
