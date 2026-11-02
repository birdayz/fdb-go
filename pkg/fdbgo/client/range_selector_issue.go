package client

import (
	"bytes"
	"context"

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
	ctx     context.Context
	release context.CancelFunc
	inc     *readIncarnation // counted in inc.issuedReads until complete; nil if not counted
	// begin and end are GetRangeReq's selectors, after removeOrEqual (:1752-1756):
	// the conflict range is computed from them (RYW:245-319).
	begin, end        KeySelector
	limits            rangeLimits
	reverse, snapshot bool
	maxKey            []byte
	disabled          bool     // RYW disabled: readThrough (RYW:187-215)
	walk              *rywWalk // the RYW and snapshot-cache routes
	// view is the pinned write-map version (RFC-262 D11): set when the walk did not
	// finish at issue and reads the write map. Released once, in complete.
	view           *rywWrites
	localConflicts [][2][]byte // a read answered at issue: its filtered conflict
	finished       bool        // answered by the synchronous checks; nothing to run
	local          bool        // the walk finished at issue
	kvs            []KeyValue
	more           bool
	err            error
	afterReply     func() // test seam, captured at issue
}

// issueSelectorRange runs the synchronous part of RYW getRange (RYW:1711-1769):
// the checks in C++ order, the route (readWithConflictRange, :397-406), the walk
// to its first wait, the pin, and registration in the commit barrier.
func (tx *Transaction) issueSelectorRange(parent context.Context, begin, end KeySelector, limit, byteTarget int, reverse, snapshot bool) *selectorRangeRead {
	ctx, release, counted := tx.opContextFor(parent, true)
	r := &selectorRangeRead{tx: tx, ctx: ctx, release: release, reverse: reverse, snapshot: snapshot}
	if err := tx.checkReadState(ctx); err != nil { // used_during_commit, resetPromise (:1732-1737)
		return r.finish(nil, false, tx.trackReadOperation(ctx, err))
	}
	r.maxKey = tx.maxReadKey()
	if bytes.Compare(begin.Key, r.maxKey) > 0 || bytes.Compare(end.Key, r.maxKey) > 0 { // :1739-1741
		return r.finish(nil, false, &wire.FDBError{Code: 2004})
	}
	// GetRangeLimits(limit, target_bytes) as fdb_c builds it: 0 is unlimited
	// (fdb_c.cpp:991-994, 1057).
	r.limits = newRangeLimits(limit, byteTarget)
	if r.limits.isReached() { // :1744-1747
		return r.finish(nil, false, nil)
	}
	if !r.limits.isValid() { // :1749-1750
		return r.finish(nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid})
	}
	r.begin, r.end = begin.removeOrEqual(), end.removeOrEqual()                       // :1752-1756
	if r.begin.Offset >= r.end.Offset && bytes.Compare(r.begin.Key, r.end.Key) >= 0 { // :1758-1761
		return r.finish(nil, false, nil)
	}
	tx.hadRead.Store(true)
	if tx.rywDisabled {
		r.disabled = true
	} else {
		c := &tx.ryw
		var panicked bool
		c.withLock(func() {
			// As keyRead: the walk runs to its first wait on the caller's goroutine, and a
			// panic fails the read with nothing locked, pinned or leased (RFC-110).
			panicked = tx.rangeReadBackstop().run(func() {
				var view *rywWrites
				if !snapshot || tx.snapshotRYWDisableCount <= 0 {
					view = c.liveLocked() // RYWIterator (:378-395); nil: SnapshotCache::iterator (:366-377)
				}
				r.walk = c.newRangeWalkLocked(view, r.begin, r.end, r.limits, reverse, r.maxKey)
				r.walk.onRequest = tx.walkOnRequest
				r.walk.startLocked()
				switch {
				case r.walk.done():
					r.local = true
					if r.walk.err == nil && !snapshot {
						b, e := rywRangeConflictSpan(r.begin, r.end, r.walk.rangeResult(), reverse, r.maxKey)
						r.localConflicts = view.conflictRanges(b, e)
					}
				case view != nil:
					r.view = c.pinWritesLocked()
				}
			})
			if panicked && r.view != nil {
				c.unpinWritesLocked(r.view)
				r.view = nil
			}
		})
		if panicked {
			return r.finish(nil, false, tx.trackReadOperation(ctx, &wire.FDBError{Code: ErrInternalError}))
		}
	}
	if !counted && !r.local {
		r.inc = tx.enterIssuedRead(ctx) // a borrowed operation's lease does not count
	}
	r.afterReply = tx.afterRangeReply
	return r
}

func (r *selectorRangeRead) finish(kvs []KeyValue, more bool, err error) *selectorRangeRead {
	r.finished, r.kvs, r.more, r.err = true, kvs, more, err
	r.release()
	return r
}

// run performs the read's I/O and records its read conflict. Exactly once. With
// clone the rows are copied out of RYW storage for the caller.
func (r *selectorRangeRead) run(clone bool) (kvs []KeyValue, more bool, err error) {
	if r.finished {
		return r.kvs, r.more, r.err
	}
	defer r.complete()
	pb := r.tx.rangeReadBackstop()
	if pb.run(func() { kvs, more, err = r.read(clone) }) {
		return nil, false, r.tx.trackReadOperation(r.ctx, &wire.FDBError{Code: ErrInternalError})
	}
	return kvs, more, err
}

// reset reports the resetPromise that wins over a suspended read (RYW:353-395).
func (r *selectorRangeRead) reset() bool {
	op := r.tx.readOperation(r.ctx)
	return op != nil && r.tx.readIncarnationCause(op.inc) != nil
}

func (r *selectorRangeRead) read(clone bool) ([]KeyValue, bool, error) {
	if r.disabled {
		return r.readThrough()
	}
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
			return nil, false, tx.trackReadOperation(ctx, err)
		}
	} else if r.walk.err != nil {
		return nil, false, tx.trackReadOperation(ctx, r.walk.err)
	}
	res := r.walk.rangeResult()
	if !r.snapshot { // addConflictRange after a successful read only (RYW:387-388)
		ranges := r.localConflicts
		if !r.local {
			b, e := rywRangeConflictSpan(r.begin, r.end, res, r.reverse, r.maxKey)
			tx.ryw.withLock(func() { ranges = r.view.conflictRanges(b, e) })
		}
		if err := tx.addReadConflictsFor(ctx, ranges); err != nil {
			return nil, false, tx.trackReadOperation(ctx, err)
		}
	}
	kvs := res.kvs
	if clone {
		kvs = cloneReadKVs(kvs)
	}
	return kvs, res.more, tx.trackReadOperation(ctx, nil)
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
	r.release()
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
	return tx.startSelectorRange(ctx, begin, end, limit, byteTarget, reverse, false)
}

// StartGetRangeSelectors is the snapshot form of Transaction.StartGetRangeSelectors.
func (s *Snapshot) StartGetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) *PendingRange {
	return s.tx.startSelectorRange(ctx, begin, end, limit, byteTarget, reverse, true)
}

// GetRangeSelectors is StartGetRangeSelectors waited for in the call.
func (tx *Transaction) GetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return tx.issueSelectorRange(ctx, ownedSelector(begin), ownedSelector(end), limit, byteTarget, reverse, false).run(true)
}

// GetRangeSelectors is the snapshot form of Transaction.GetRangeSelectors.
func (s *Snapshot) GetRangeSelectors(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return s.tx.issueSelectorRange(ctx, ownedSelector(begin), ownedSelector(end), limit, byteTarget, reverse, true).run(true)
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

func (tx *Transaction) startSelectorRange(ctx context.Context, begin, end KeySelector, limit, byteTarget int, reverse, snapshot bool) *PendingRange {
	r := tx.issueSelectorRange(ctx, ownedSelector(begin), ownedSelector(end), limit, byteTarget, reverse, snapshot)
	if r.finished || r.local {
		p := &PendingRange{done: closedDone}
		p.state.Store(pendingClaimed)
		p.kvs, p.more, p.err = r.run(false)
		return p
	}
	return tx.startPendingRange(r)
}
