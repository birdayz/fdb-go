package client

import (
	"bytes"
	"context"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
)

// range_selector_issue.go is getRange over key selectors (RFC-262 D10). Line
// numbers are ReadYourWrites.actor.cpp (RYW) and NativeAPI.actor.cpp (NAPI) of
// FoundationDB 7.3.77. The RYW routes resolve the selectors against the write
// map and snapshot cache and send what is left to storage inside the range
// request (getRangeValue, RYW:592-813 and getRangeValueBack, :896-1123, reading
// through NAPI getRange with selectors, :4651-4679); with RYW disabled the
// selectors go to NAPI getRange as given, except a reverse read's end of offset
// > 1, which readThrough resolves with getKey first (RYW:187-215). A selector the
// storage server cannot resolve inside its shard is resolved by
// getRangeFallback (NAPI:4457-4500).

// selectorRangeRead is one getRange over selectors past its call: RYW getRange
// (RYW:1711-1769) checks, routes and builds its iterator in the call, and adds
// the read to `reading` (:1767).
type selectorRangeRead struct {
	tx      *Transaction
	ctx     context.Context // the read's operation: &op when admitted into it
	op      readOperation   // admitted in place, so a read answered at issue allocates none
	release opRelease
	inc     *readIncarnation // counted in inc.issuedReads until complete; nil if not counted
	// begin and end are GetRangeReq's selectors, after removeOrEqual (:1752-1756):
	// the conflict range is computed from them (RYW:245-319).
	begin, end        KeySelector
	limits            rangeLimits
	reverse, snapshot bool
	maxKey            []byte
	disabled          bool     // RYW disabled: readThrough (RYW:187-215)
	walk              *rywWalk // the RYW and snapshot-cache routes
	// view is the pinned write-map version (RFC-262 D11): set when the walk reads
	// the write map, which it does without c.mu. Released once: at issue if the
	// walk finishes there, else in complete.
	view              *rywWrites
	localConflicts    []conflictSpan // a read answered at issue when > 4 spans
	localConflictsBuf [4]conflictSpan
	localConflictsLen int8
	finished          bool // answered by the synchronous checks; nothing to run
	local             bool // the walk finished at issue
	kvs               []KeyValue
	more              bool
	err               error
	afterReply        func() // test seam, captured at issue
}

// selectorBoundary is a selector's key after removeOrEqual as a zero-alloc rywBoundary.
func selectorBoundary(s KeySelector) rywBoundary {
	if s.OrEqual && !bytes.Equal(s.Key, allKeysEnd) {
		return rywBoundary{key: s.Key, after: true}
	}
	return rywBoundary{key: s.Key}
}

// issueSelectorRangeInto runs the synchronous part of RYW getRange (RYW:1711-1769):
// the checks in C++ order, the route (readWithConflictRange, :397-406), the walk
// to its first wait, the pin, and registration in the commit barrier.
func (tx *Transaction) issueSelectorRangeInto(r *selectorRangeRead, parent context.Context, begin, end KeySelector, limit, byteTarget int, reverse, snapshot bool) {
	if tx.routesSpecialKeyRange(begin.Key, end.Key) { // RYW:1716-1721, before every other check
		*r = selectorRangeRead{tx: tx}
		r.finish(tx.specialKeyRange(begin, end, limit, byteTarget, reverse))
		return
	}
	*r = selectorRangeRead{tx: tx, reverse: reverse, snapshot: snapshot}
	counted, stateErr := tx.admitSelectorRead(r, parent)
	if stateErr != nil { // used_during_commit, resetPromise (:1732-1737)
		r.finish(nil, false, r.trackError(stateErr))
		return
	}
	r.maxKey = tx.maxReadKey()
	if bytes.Compare(begin.Key, r.maxKey) > 0 || bytes.Compare(end.Key, r.maxKey) > 0 { // :1739-1741
		r.finish(nil, false, &wire.FDBError{Code: 2004})
		return
	}
	// GetRangeLimits(limit, target_bytes) as fdb_c builds it: 0 is unlimited
	// (fdb_c.cpp:991-994, 1057).
	r.limits = newRangeLimits(limit, byteTarget)
	if r.limits.isReached() { // :1744-1747
		r.finish(nil, false, nil)
		return
	}
	if !r.limits.isValid() { // :1749-1750
		r.finish(nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid})
		return
	}
	if begin.Offset >= end.Offset && selectorBoundary(begin).compare(selectorBoundary(end)) >= 0 { // :1758-1761
		r.finish(nil, false, nil)
		return
	}
	tx.hadRead.Store(true)
	if tx.rywDisabled {
		var buf []byte
		r.begin, r.end = removeOrEqualPair(&buf, begin, end)
		r.disabled = true
	} else if r.startWalk(begin, end) {
		r.finish(nil, false, r.trackError(&wire.FDBError{Code: ErrInternalError}))
		return
	}
	if !counted && !r.local {
		r.inc = tx.enterIssuedRead(r.ctx) // a borrowed operation's lease does not count
	}
	r.afterReply = tx.afterRangeReply
}

func (r *selectorRangeRead) startWalk(begin, end KeySelector) (panicked bool) {
	tx := r.tx
	c := &tx.ryw
	backstop := tx.rangeReadBackstop()
	c.mu.Lock()
	// As keyRead: c.mu covers only pinning the version and snapshotting the
	// cache; the walk then runs to its first wait on the caller's goroutine,
	// and a panic fails the read with nothing locked, pinned or leased (RFC-110).
	panicked = backstop.run(func() {
		var view *rywWrites
		if !r.snapshot || tx.snapshotRYWDisableCount <= 0 {
			// RYWIterator (:378-395); nil: SnapshotCache::iterator (:366-377)
			view = c.pinWritesLocked()
			r.view = view
		}
		r.walk = c.newRangeWalkLocked(view, begin, end, r.limits, r.reverse, r.maxKey)
		r.begin, r.end = r.walk.begin, r.walk.end
	})
	if panicked && r.view != nil {
		c.unpinWritesLocked(r.view)
		r.view = nil
	}
	c.mu.Unlock()
	if panicked {
		return true
	}
	panicked = backstop.run(func() {
		w := r.walk
		w.onRequest = tx.walkOnRequest
		w.start()
		if w.done() {
			r.local = true
			if w.err == nil && !r.snapshot {
				b, e := rywRangeConflictSpan(r.begin, r.end, w.rangeResult(), r.reverse, r.maxKey)
				var scratch [4]conflictSpan
				spans := r.view.appendConflictSpans(scratch[:0], b, e)
				if len(spans) <= len(r.localConflictsBuf) {
					copy(r.localConflictsBuf[:], spans)
					r.localConflictsLen = int8(len(spans))
				} else {
					r.localConflicts = spans
				}
			}
			if r.view != nil {
				c.withLock(func() { c.unpinWritesLocked(r.view) })
				r.view = nil
			}
		}
	})
	if panicked && r.view != nil {
		c.withLock(func() { c.unpinWritesLocked(r.view) })
		r.view = nil
	}
	return panicked
}

var selectorReadPool = sync.Pool{New: func() any { return new(selectorRangeRead) }}

func (r *selectorRangeRead) recycle() {
	if r.finished || r.local {
		*r = selectorRangeRead{}
		selectorReadPool.Put(r)
	}
}

// bindHeap readies a read that outlives its call. Unless the caller handed its
// selector keys over, the read copies the keys that may still be the caller's.
func (r *selectorRangeRead) bindHeap(handed bool) *selectorRangeRead {
	if !handed {
		r.begin, r.end = ownedSelector(r.begin), ownedSelector(r.end)
		if r.walk != nil {
			r.walk.begin, r.walk.end = ownedSelector(r.walk.begin), ownedSelector(r.walk.end)
		}
	}
	return r
}

func (r *selectorRangeRead) finish(kvs []KeyValue, more bool, err error) *selectorRangeRead {
	r.finished, r.kvs, r.more, r.err = true, kvs, more, err
	r.release.release()
	return r
}

func (r *selectorRangeRead) trackError(err error) error {
	return r.tx.trackReadOperation(r.ctx, err)
}

// run performs the read's I/O and records its read conflict. Exactly once. With
// clone the rows are copied out of RYW storage for the caller.
func (r *selectorRangeRead) run(clone bool) (kvs []KeyValue, more bool, err error) {
	return r.runInto(clone, nil)
}

func (r *selectorRangeRead) runInto(clone bool, dst []KeyValue) (kvs []KeyValue, more bool, err error) {
	if r.finished {
		kvs, more, err = r.kvs, r.more, r.err
		r.recycle()
		return kvs, more, err
	}
	defer r.complete()
	pb := r.tx.rangeReadBackstop()
	if pb.run(func() { kvs, more, err = r.read(clone, dst) }) {
		return nil, false, r.trackError(&wire.FDBError{Code: ErrInternalError})
	}
	return kvs, more, err
}

// reset reports the resetPromise that wins over a suspended read (RYW:353-395).
func (r *selectorRangeRead) reset() bool {
	op := r.tx.readOperation(r.ctx)
	return op != nil && r.tx.readIncarnationCause(op.inc) != nil
}

func (r *selectorRangeRead) read(clone bool, dst []KeyValue) ([]KeyValue, bool, error) {
	if r.disabled {
		return r.readThrough()
	}
	// Its rows are the cache's and the write map's, not the walk's.
	defer func() { r.walk.release(); r.walk = nil }()
	tx, ctx := r.tx, r.ctx
	if !r.local {
		storage := tx.nativeRange
		native := func(ctx context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error) {
			var res nativeRangeResult
			var err error
			if storage != nil {
				res, err = storage(ctx, begin, end, limits, reverse)
			} else {
				res, err = tx.nativeGetRange(ctx, begin, end, limits, true, reverse) // Snapshot::True (RYW:759, 1063)
			}
			if r.afterReply != nil {
				r.afterReply()
			}
			if r.reset() {
				return nativeRangeResult{}, context.Canceled
			}
			return res, err
		}
		if err := r.walk.run(ctx, native); err != nil {
			return nil, false, r.trackError(err)
		}
	} else if r.walk.err != nil {
		return nil, false, r.trackError(r.walk.err)
	}
	res := r.walk.rangeResult()
	if !r.snapshot { // addConflictRange after a successful read only (RYW:387-388)
		spans := r.localConflicts
		if spans == nil {
			spans = r.localConflictsBuf[:r.localConflictsLen]
		}
		if !r.local {
			var scratch [4]conflictSpan
			b, e := rywRangeConflictSpan(r.begin, r.end, res, r.reverse, r.maxKey)
			spans = r.view.appendConflictSpans(scratch[:0], b, e) // a pinned version is immutable: no lock
		}
		if err := tx.addReadConflictSpansFor(ctx, spans); err != nil {
			return nil, false, r.trackError(err)
		}
	}
	kvs := res.kvs
	switch {
	case len(kvs) == 0:
		kvs = nil
	case clone:
		kvs = cloneReadKVs(kvs)
	default:
		kvs = append(dst, kvs...) // the walk's slice is reused
	}
	return kvs, res.more, nil
}

// readThrough is readThrough(GetRangeReq) (RYW:187-215): NativeAPI getRange with
// the caller's snapshot flag, after resolving a backward read's end selector of
// offset > 1 with NativeAPI getKey, then the forward clip to getMaxReadKey.
func (r *selectorRangeRead) readThrough() ([]KeyValue, bool, error) {
	tx, ctx := r.tx, r.ctx
	begin, end := r.begin, r.end
	if r.reverse && end.Offset > 1 { // :191-199
		key, err := tx.getKey(ctx, end.Key, end.OrEqual, end.Offset)
		if r.reset() {
			return nil, false, tx.trackReadOperation(ctx, context.Canceled)
		}
		if err != nil {
			return nil, false, tx.trackReadOperation(ctx, err)
		}
		if !r.snapshot { // Transaction::getKey adds getKeyAndConflictRange's range (NAPI:5761-5787)
			if b, e := getKeyConflictSpan(end, key); bytes.Compare(b, e) < 0 {
				if err := tx.addReadConflictsFor(ctx, [][2][]byte{{b, e}}); err != nil {
					return nil, false, tx.trackReadOperation(ctx, err)
				}
			}
		}
		if bytes.Compare(key, r.maxKey) > 0 {
			end = firstGreaterOrEqual(r.maxKey)
		} else {
			end = firstGreaterOrEqual(key)
		}
	}
	res, err := tx.nativeGetRange(ctx, begin, end, r.limits, r.snapshot, r.reverse) // :201-202
	if r.afterReply != nil {
		r.afterReply()
	}
	if r.reset() {
		return nil, false, tx.trackReadOperation(ctx, context.Canceled)
	}
	if err != nil {
		return nil, false, tx.trackReadOperation(ctx, err)
	}
	// getRangeFinished's conflict (NAPI:4558-4586); an empty one is not added
	// (commit keeps extraConflictRanges with first < second, NAPI:6840-6844).
	if c := res.conflict; c != nil && bytes.Compare(c[0], c[1]) < 0 {
		if err := tx.addReadConflictsFor(ctx, [][2][]byte{*c}); err != nil {
			return nil, false, tx.trackReadOperation(ctx, err)
		}
	}
	kvs, more := res.kvs, res.more
	// :203-212: a forward result ending at or past maxKey drops those rows and more.
	if n := len(kvs); n > 0 && !r.reverse && bytes.Compare(kvs[n-1].Key, r.maxKey) >= 0 {
		i := n - 2
		for ; i >= 0 && bytes.Compare(kvs[i].Key, r.maxKey) >= 0; i-- {
		}
		kvs, more = kvs[:i+1], false
	}
	return kvs, more, tx.trackReadOperation(ctx, nil)
}

// complete releases the pin, leaves the commit barrier and releases the read's
// operation, in that order (RFC-262 D11).
func (r *selectorRangeRead) complete() {
	if r.view != nil {
		c := &r.tx.ryw
		c.withLock(func() { c.unpinWritesLocked(r.view) })
		r.view = nil
	}
	r.tx.leaveIssuedRead(r.inc)
	r.release.release()
	r.recycle()
}

// removeOrEqualPair is removeOrEqual of both selectors (RYW:1752-1756), their
// successors appended to buf as to C++'s request arena; selector keys are never written.
func removeOrEqualPair(buf *[]byte, begin, end KeySelector) (KeySelector, KeySelector) {
	if begin.Key == nil {
		begin.Key = allKeysBegin
	}
	if end.Key == nil {
		end.Key = allKeysBegin
	}
	b, e := selectorBoundary(begin), selectorBoundary(end)
	begin.OrEqual, end.OrEqual = false, false
	if b.after {
		begin.Key = materializeInto(buf, b)
	}
	if e.after {
		if b.after && bytes.Equal(b.key, e.key) {
			end.Key = begin.Key
		} else {
			end.Key = materializeInto(buf, e)
		}
	}
	return begin, end
}

// rywRangeConflictSpan is addConflictRange(GetRangeReq<false>) (RYW:245-281) and
// addConflictRange(GetRangeReq<true>) (:284-319) before updateConflictMap: the
// span a serializable selector range read depends on, from its selectors (after
// removeOrEqual) and its result. res.kvs is in scan order.
func rywRangeConflictSpan(begin, end KeySelector, res rywRangeResult, reverse bool, maxKey []byte) (rangeBegin, rangeEnd []byte) {
	n := len(res.kvs)
	if bytes.Compare(begin.Key, end.Key) < 0 {
		rangeBegin, rangeEnd = begin.Key, end.Key
		if !reverse && end.Offset > 0 && res.more { // :257
			rangeEnd = begin.Key
		}
		if reverse && begin.Offset <= 1 && res.more { // :295
			rangeBegin = end.Key
		}
	} else {
		rangeBegin, rangeEnd = end.Key, begin.Key
	}
	if res.readToBegin && begin.Offset <= 0 {
		rangeBegin = allKeysBegin
	}
	if res.readThroughEnd && end.Offset > 0 {
		rangeEnd = maxKey
	}
	if n == 0 {
		return rangeBegin, rangeEnd
	}
	if !reverse { // :268-275
		if begin.Offset <= 0 && bytes.Compare(res.kvs[0].Key, rangeBegin) < 0 {
			rangeBegin = res.kvs[0].Key
		}
		if last := res.kvs[n-1].Key; bytes.Compare(rangeEnd, last) <= 0 {
			rangeEnd = keyAfterSelectorKey(last)
		}
		return rangeBegin, rangeEnd
	}
	// :307-313: the result is descending, so its last row is the smallest key.
	if last := res.kvs[n-1].Key; bytes.Compare(last, rangeBegin) < 0 {
		rangeBegin = last
	}
	if first := res.kvs[0].Key; end.Offset > 0 && bytes.Compare(rangeEnd, first) <= 0 {
		rangeEnd = keyAfterSelectorKey(first)
	}
	return rangeBegin, rangeEnd
}

// issuedRangeRead is what a PendingRange runs: an exact or a selector range read.
type issuedRangeRead interface {
	run(clone bool) ([]KeyValue, bool, error)
}

// StartGetRangeSelectors starts a range read between two key selectors and
// returns at once, like fdb_transaction_get_range: the selectors are resolved
// within the range read, against the transaction's writes and snapshot cache
// and by the storage server (RYW:592-813, 896-1123).
// limit 0 and -1 are unlimited rows; byteTarget 0 is no byte limit. Commit
// waits for the read whether or not Get is called (RYW:1767).
func (tx *Transaction) StartGetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) *PendingRange {
	return tx.startSelectorRange(ctx, begin, end, limit, byteTarget, reverse, false, false)
}

// StartGetRangeSelectors is the snapshot form of Transaction.StartGetRangeSelectors.
func (s Snapshot) StartGetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) *PendingRange {
	return s.tx.startSelectorRange(ctx, begin, end, limit, byteTarget, reverse, true, false)
}

// StartGetRangeOwnedSelectors is StartGetRangeSelectors for selectors whose keys
// the caller hands over: it never modifies them afterwards, so the read keeps
// them instead of copying them.
func (tx *Transaction) StartGetRangeOwnedSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) *PendingRange {
	return tx.startSelectorRange(ctx, begin, end, limit, byteTarget, reverse, false, true)
}

// StartGetRangeOwnedSelectors is the snapshot form of Transaction.StartGetRangeOwnedSelectors.
func (s Snapshot) StartGetRangeOwnedSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) *PendingRange {
	return s.tx.startSelectorRange(ctx, begin, end, limit, byteTarget, reverse, true, true)
}

// GetRangeOwnedSelectors is StartGetRangeOwnedSelectors waited for in the call.
func (tx *Transaction) GetRangeOwnedSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return tx.getSelectorRange(ctx, begin, end, limit, byteTarget, reverse, false, true)
}

// GetRangeOwnedSelectors is the snapshot form of Transaction.GetRangeOwnedSelectors.
func (s Snapshot) GetRangeOwnedSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return s.tx.getSelectorRange(ctx, begin, end, limit, byteTarget, reverse, true, true)
}

// GetRangeSelectors is StartGetRangeSelectors waited for in the call.
func (tx *Transaction) GetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return tx.getSelectorRange(ctx, begin, end, limit, byteTarget, reverse, false, false)
}

// GetRangeSelectors is the snapshot form of Transaction.GetRangeSelectors.
func (s Snapshot) GetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return s.tx.getSelectorRange(ctx, begin, end, limit, byteTarget, reverse, true, false)
}

// ownedSelector copies the selector's key: the read outlives the call, as C++
// copies the KeySelector before dispatch (ThreadSafeTransaction.cpp).
func ownedSelector(s KeySelector) KeySelector {
	s.Key = bytes.Clone(s.Key)
	if s.Key == nil {
		s.Key = []byte{}
	}
	return s
}

// getSelectorRange and startSelectorRange copy no selector key for a read answered
// at issue, and for one that runs on only the keys the caller did not hand over.
func (tx *Transaction) getSelectorRange(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse, snapshot, handed bool) ([]KeyValue, bool, error) {
	r := selectorReadPool.Get().(*selectorRangeRead)
	tx.issueSelectorRangeInto(r, ctx, begin, end, limit, byteTarget, reverse, snapshot)
	if r.finished || r.local {
		return r.run(true)
	}
	return r.bindHeap(handed).run(true)
}

func (tx *Transaction) startSelectorRange(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse, snapshot, handed bool) *PendingRange {
	r := selectorReadPool.Get().(*selectorRangeRead)
	tx.issueSelectorRangeInto(r, ctx, begin, end, limit, byteTarget, reverse, snapshot)
	if r.finished || r.local {
		// The rows stay in RYW storage, which is never written; each Get copies them.
		p := &PendingRange{done: closedDone}
		p.state.Store(pendingClaimed)
		p.kvs, p.more, p.err = r.runInto(false, p.kv0[:0])
		return p
	}
	return tx.startPendingRange(r.bindHeap(handed))
}
