package client

import (
	"bytes"
	"context"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/wire"
)

// keyRead is one getKey past its call (RFC-262 D6): C++ RYW::getKey routes the read,
// builds its iterator and adds it to `reading` in the call (ReadYourWrites.actor.cpp:1695-1709).
// It owns the pin on the write-map version its walk reads (D11).
type keyRead struct {
	tx       *Transaction
	ctx      context.Context
	release  context.CancelFunc
	inc      *readIncarnation // counted in inc.issuedReads until complete; nil if not counted
	sel      KeySelector      // the caller's selector: the conflict range uses it (RYW:230-243)
	snapshot bool
	disabled bool     // RYW disabled: NativeAPI getKey (RYW:176-185)
	walk     *rywWalk // the RYW and snapshot-cache routes
	// view is the pinned version (D11): set when the walk did not finish at issue
	// and reads the write map; the conflict is filtered through it. Released once,
	// in complete.
	view *rywWrites
	// localConflicts is the filtered conflict of a read answered at issue, computed
	// then through the version it read.
	localConflicts [][2][]byte
	finished       bool // answered by the synchronous checks; nothing to run
	local          bool // the walk finished at issue
	key            []byte
	err            error
	afterReply     func() // test seam, captured at issue
}

// issueKey runs a getKey's synchronous part: entry checks, routing, the walk to its
// first wait, the pin, and registration in the commit barrier.
func (tx *Transaction) issueKey(parent context.Context, sel KeySelector, snapshot bool) *keyRead {
	ctx, release, counted := tx.opContextFor(parent, true)
	r := &keyRead{tx: tx, ctx: ctx, release: release, sel: sel, snapshot: snapshot}
	if err := tx.checkReadState(ctx); err != nil {
		return r.finish(nil, tx.trackReadOperation(ctx, err))
	}
	// C++ RYW::getKey: key > maxReadKey → key_outside_legal_range (RYW:1699-1701).
	// Eager validation — NOT tracked (C++ returns it before the read future exists).
	if bytes.Compare(sel.Key, tx.maxReadKey()) > 0 {
		return r.finish(nil, &wire.FDBError{Code: 2004})
	}
	tx.hadRead.Store(true)
	if tx.rywDisabled {
		r.disabled = true
	} else {
		c := &tx.ryw
		var panicked bool
		c.withLock(func() {
			// The walk runs to its first wait here, on the caller's goroutine: a panic
			// fails the read (RFC-110) and leaves nothing locked, pinned or leased.
			panicked = tx.rangeReadBackstop().run(func() {
				var view *rywWrites
				if !snapshot || tx.snapshotRYWDisableCount <= 0 {
					view = c.liveLocked() // RYWIterator (A2.3); nil: SnapshotCache::iterator (A2.2)
				}
				r.walk = c.newGetKeyWalkLocked(view, sel, tx.maxReadKey())
				r.walk.onRequest = tx.walkOnRequest
				r.walk.startLocked()
				switch {
				case r.walk.done():
					r.local = true
					if r.walk.err == nil && !snapshot {
						if b, e := getKeyConflictSpan(sel, r.walk.keyResult()); bytes.Compare(b, e) < 0 {
							r.localConflicts = view.conflictRanges(b, e)
						}
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
			return r.finish(nil, tx.trackReadOperation(ctx, &wire.FDBError{Code: ErrInternalError}))
		}
	}
	if !counted && !r.local {
		r.inc = tx.enterIssuedRead(ctx) // a borrowed operation's lease does not count
	}
	r.afterReply = tx.afterRangeReply
	return r
}

func (r *keyRead) finish(key []byte, err error) *keyRead {
	r.finished, r.key, r.err = true, key, err
	r.release()
	return r
}

// run performs the read's I/O and records its read conflict. Exactly once.
func (r *keyRead) run() (key []byte, err error) {
	if r.finished {
		return r.key, r.err
	}
	defer r.complete()
	// RFC-110: a panic fails the read before complete() releases Commit's wait.
	pb := r.tx.rangeReadBackstop()
	if pb.run(func() { key, err = r.read() }) {
		return nil, r.tx.trackReadOperation(r.ctx, &wire.FDBError{Code: ErrInternalError})
	}
	return key, err
}

func (r *keyRead) read() ([]byte, error) {
	tx, ctx, sel := r.tx, r.ctx, r.sel
	if r.disabled {
		resolved, err := tx.getKey(ctx, sel.Key, sel.OrEqual, sel.Offset)
		// The read-through route also races resetPromise (RYW:353-364).
		if op := tx.readOperation(ctx); op != nil && tx.readIncarnationCause(op.inc) != nil {
			return nil, tx.trackReadOperation(ctx, context.Canceled)
		}
		if err != nil {
			return nil, tx.trackReadOperation(ctx, err)
		}
		if !r.snapshot { // getKeyAndConflictRange (NAPI:5761-5787)
			tx.addGetKeyConflictRange(sel.Key, sel.OrEqual, sel.Offset, resolved)
		}
		// readThrough(GetKeyReq) clamps the returned key to getMaxReadKey (RYW:182-183);
		// the conflict above stays on the unclamped key (NAPI:5767).
		if bytes.Compare(resolved, tx.maxReadKey()) > 0 {
			resolved = tx.maxReadKey()
		}
		return resolved, tx.trackReadOperation(ctx, nil)
	}
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
			// resetPromise wins while the read is suspended (RYW:378-395). Reset
			// retires this incarnation before waiting for our lease or resetting c.gen.
			if op := tx.readOperation(ctx); op != nil && tx.readIncarnationCause(op.inc) != nil {
				return nativeRangeResult{}, context.Canceled
			}
			return res, err
		}
		if err := r.walk.run(ctx, native); err != nil {
			return nil, tx.trackReadOperation(ctx, err)
		}
	} else if r.walk.err != nil {
		return nil, tx.trackReadOperation(ctx, r.walk.err)
	}
	key := bytes.Clone(r.walk.keyResult())
	if !r.snapshot { // RYW getKey adds its conflict for every key (RYW:1695-1709, 230-243)
		if r.local {
			tx.addReadConflicts(r.localConflicts)
		} else if b, e := getKeyConflictSpan(sel, key); bytes.Compare(b, e) < 0 {
			// Filtered through the read's own version (RYW:388, 322-351).
			var ranges [][2][]byte
			tx.ryw.withLock(func() { ranges = r.view.conflictRanges(b, e) })
			tx.addReadConflicts(ranges)
		}
	}
	return key, tx.trackReadOperation(ctx, nil)
}

// complete releases the pin, leaves the commit barrier and releases the read's
// operation, in that order (D11).
func (r *keyRead) complete() {
	if r.view != nil {
		c := &r.tx.ryw
		c.withLock(func() { c.unpinWritesLocked(r.view) })
		r.view = nil
	}
	r.tx.leaveIssuedRead(r.inc)
	r.release()
}

// getKeyConflictSpan is addConflictRange(GetKeyReq) (RYW:230-243), and NativeAPI
// getKeyAndConflictRange's span (NAPI:5767-5770): between the caller's selector
// and the resolved key, with C++ keyAfter, which leaves \xff\xff unchanged.
func getKeyConflictSpan(sel KeySelector, resolved []byte) (begin, end []byte) {
	if sel.Offset <= 0 {
		begin = resolved
		if sel.OrEqual {
			end = keyAfterSelectorKey(sel.Key)
		} else {
			end = sel.Key
		}
		return begin, end
	}
	if sel.OrEqual {
		begin = keyAfterSelectorKey(sel.Key)
	} else {
		begin = sel.Key
	}
	return begin, keyAfterSelectorKey(resolved)
}

// PendingKey is a started getKey, like the future fdb_transaction_get_key returns.
type PendingKey struct {
	r     *keyRead
	state atomic.Int32 // pendingReady → pendingClaimed by whichever of the goroutine or Get runs r
	done  chan struct{}
	key   []byte
	err   error
	// ranByCaller reports that Get ran the read itself (test observability).
	ranByCaller bool
}

// Get waits for the read and returns the resolved key (the caller's own copy).
func (p *PendingKey) Get() ([]byte, error) {
	if p.state.CompareAndSwap(pendingReady, pendingClaimed) {
		p.ranByCaller = true
		p.execute()
	}
	<-p.done
	return bytes.Clone(p.key), p.err
}

func (p *PendingKey) execute() {
	defer close(p.done)
	r := p.r
	p.r = nil
	p.key, p.err = r.run()
}

// StartGetKey starts a getKey and returns at once. The selector is resolved against
// the write map as of this call; GRV, location lookup and requests run in the
// background, and Commit waits for the read whether or not Get is called (RYW:1706).
func (tx *Transaction) StartGetKey(ctx context.Context, sel KeySelector) *PendingKey {
	return tx.startKey(ctx, sel, false)
}

// StartGetKey is the snapshot form of Transaction.StartGetKey.
func (s *Snapshot) StartGetKey(ctx context.Context, sel KeySelector) *PendingKey {
	return s.tx.startKey(ctx, sel, true)
}

func (tx *Transaction) startKey(ctx context.Context, sel KeySelector, snapshot bool) *PendingKey {
	// The read outlives the call, so it owns its selector: C++ copies the
	// KeySelector before dispatch (ThreadSafeTransaction.cpp:436-442).
	sel.Key = bytes.Clone(sel.Key)
	r := tx.issueKey(ctx, sel, snapshot)
	if r.finished || r.local {
		p := &PendingKey{done: closedDone}
		p.state.Store(pendingClaimed)
		p.key, p.err = r.run()
		return p
	}
	p := &PendingKey{r: r, done: make(chan struct{})}
	hook := tx.beforeRangeGoroutine
	go func() {
		if hook != nil {
			hook()
		}
		if p.state.CompareAndSwap(pendingReady, pendingClaimed) {
			p.execute()
		}
	}()
	return p
}
