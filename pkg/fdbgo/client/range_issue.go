package client

import (
	"bytes"
	"context"

	"fdb.dev/pkg/fdbgo/wire"
)

// rangeRead is one range read past its call: C++ RYW getRange picks the path, builds
// the read's RYWIterator and adds it to `reading` in the call (ReadYourWrites.actor.cpp:1764-1767, :382).
type rangeRead struct {
	tx                *Transaction
	ctx               context.Context
	release           context.CancelFunc
	inc               *readIncarnation // counted in inc.issuedReads until complete; nil if not counted
	begin, end        []byte
	limit, byteTarget int
	reverse, snapshot bool
	path              rangePath
	ryw               *rywRangeRead
	finished          bool // answered by the synchronous checks; nothing to run
	local             bool // answered from local state at issue; run needs no I/O
	kvs               []KeyValue
	more              bool
	err               error
	afterReply        func() // test seam, captured at issue
}

type rangePath uint8

const (
	rangePathRYW rangePath = iota
	rangePathRYWDisabled
	rangePathSnapshotNoRYW
)

// issueRange runs a range read's synchronous part: entry checks, path choice,
// the issue-time write view, and registration in the commit barrier.
func (tx *Transaction) issueRange(parent context.Context, begin, end []byte, limit, byteTarget int, reverse, snapshot bool) *rangeRead {
	ctx, release := tx.opContext(parent)
	r := &rangeRead{tx: tx, ctx: ctx, release: release, begin: begin, end: end, limit: limit, reverse: reverse, snapshot: snapshot}
	if err := tx.checkReadState(ctx); err != nil {
		return r.finish(nil, false, tx.trackReadOperation(ctx, err))
	}
	// C++ RYW::getRange: begin or end > maxKey → key_outside_legal_range.
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return r.finish(nil, false, &wire.FDBError{Code: 2004})
	}
	// C++ RYW::getRange: !limits.isValid() → range_limits_invalid (ReadYourWrites.actor.cpp:1749),
	// after key_outside_legal_range (:1740). isValid accepts rows >= 0 or -1 (FDBTypes.h:754).
	if limit < -1 {
		return r.finish(nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid})
	}
	byteTarget, err := normalizeByteTarget(byteTarget)
	if err != nil {
		return r.finish(nil, false, err)
	}
	r.byteTarget = byteTarget
	if bytes.Compare(begin, end) >= 0 {
		return r.finish(nil, false, nil)
	}
	tx.hadRead.Store(true)
	switch {
	case tx.rywDisabled:
		r.path = rangePathRYWDisabled
	case snapshot && tx.snapshotRYWDisableCount > 0:
		r.path = rangePathSnapshotNoRYW
		r.kvs, r.more, r.local, r.err = tx.ryw.localRange(begin, end, limit, byteTarget, reverse, false)
	default:
		r.path = rangePathRYW
		r.ryw = tx.ryw.issueRange(begin, end, limit, byteTarget, reverse)
		r.local = r.ryw.known
	}
	if !r.local {
		r.inc = tx.enterIssuedRead(ctx)
	}
	r.afterReply = tx.afterRangeReply
	return r
}

// enterIssuedRead counts a read Commit must wait for (C++ `reading`); nil when the
// read's incarnation already ended, since that read can only fail.
func (tx *Transaction) enterIssuedRead(ctx context.Context) *readIncarnation {
	op := tx.readOperation(ctx)
	if op == nil {
		return nil
	}
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if tx.readLife != op.inc || op.inc.cause != nil {
		return nil
	}
	op.inc.issuedReads++
	return op.inc
}

func (tx *Transaction) leaveIssuedRead(inc *readIncarnation) {
	if inc == nil {
		return
	}
	tx.readErrMu.Lock()
	inc.issuedReads--
	var changed chan struct{}
	if inc.issuedReads == 0 {
		changed, inc.issuedReadsDone = inc.issuedReadsDone, nil
	}
	tx.readErrMu.Unlock()
	if changed != nil {
		close(changed)
	}
}

func (r *rangeRead) finish(kvs []KeyValue, more bool, err error) *rangeRead {
	r.finished, r.kvs, r.more, r.err = true, kvs, more, err
	r.release()
	return r
}

// run performs the read's I/O and records its read conflict. Exactly once. With
// clone the rows are copied out of RYW storage for the caller.
func (r *rangeRead) run(clone bool) ([]KeyValue, bool, error) {
	if r.finished {
		return r.kvs, r.more, r.err
	}
	defer r.complete()
	tx, ctx := r.tx, r.ctx
	server := tx.getRange
	if hook := r.afterReply; hook != nil {
		server = func(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
			kvs, more, err := tx.getRange(ctx, begin, end, limit, byteTarget, reverse)
			hook()
			return kvs, more, err
		}
	}
	var kvs []KeyValue
	var more bool
	var err error
	switch {
	case r.path == rangePathRYWDisabled:
		kvs, more, err = server(ctx, r.begin, r.end, r.limit, r.byteTarget, r.reverse)
		clone = false // fresh from the reply, owned by nobody else
	case r.path == rangePathSnapshotNoRYW && r.local:
		kvs, more, err = r.kvs, r.more, r.err
	case r.path == rangePathSnapshotNoRYW:
		kvs, more, err = tx.ryw.getSnapshotRange(ctx, r.begin, r.end, r.limit, r.byteTarget, r.reverse, server)
	default:
		kvs, more, err = r.ryw.run(ctx, server)
	}
	if clone {
		kvs = cloneReadKVs(kvs)
	}
	if err != nil {
		// C++ adds the read-conflict only in the read's SUCCESS branch
		// (ReadYourWrites.actor.cpp:388) — a failed read records no conflict.
		return kvs, more, tx.trackReadOperation(ctx, err)
	}
	if !r.snapshot {
		r.addConflicts(kvs, more)
	}
	return kvs, more, tx.trackReadOperation(ctx, nil)
}

// addConflicts adds the conflict clamped to the rows returned (RFC-121 D1), filtered through
// the issue-time write map (D2); inverted ranges and \xff\xff keys add none, as in C++.
func (r *rangeRead) addConflicts(kvs []KeyValue, more bool) {
	begin, end, tx := r.begin, r.end, r.tx
	if bytes.Compare(begin, end) > 0 || isSpecialKey(begin) || isSpecialKey(end) {
		return
	}
	cBegin, cEnd := rangeConflictExtent(begin, end, kvs, more, r.reverse)
	if bytes.Compare(cBegin, cEnd) >= 0 {
		return
	}
	if r.path == rangePathRYWDisabled {
		tx.addReadConflict(cBegin, cEnd)
		return
	}
	tx.addReadConflicts(r.ryw.conflictRanges(cBegin, cEnd))
}

// complete leaves the commit barrier and releases the read's operation.
func (r *rangeRead) complete() {
	r.tx.leaveIssuedRead(r.inc)
	r.release()
}

// waitIssuedReads is Commit's wait on `reading` (ReadYourWrites.actor.cpp:1359-1360): issued
// range reads and watch setups finish, recording conflict or error, before commit is built.
func (tx *Transaction) waitIssuedReads(ctx context.Context, inc *readIncarnation) error {
	for {
		tx.readErrMu.Lock()
		n := inc.issuedReads
		if n == 0 {
			tx.readErrMu.Unlock()
			return nil
		}
		ch := inc.issuedReadsDone
		if ch == nil {
			ch = make(chan struct{})
			inc.issuedReadsDone = ch
		}
		tx.readErrMu.Unlock()
		if tx.beforeIssuedReadWait != nil {
			tx.beforeIssuedReadWait(n)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return tx.mapReadError(ctx, ctx.Err())
		}
	}
}

// PendingRange is a started range read, like the future fdb_transaction_get_range returns.
type PendingRange struct {
	done chan struct{}
	kvs  []KeyValue // shared RYW/cache storage; Get copies
	more bool
	err  error
}

// Get waits for the read and returns its rows (the caller's own copy, as each Get of
// a libfdb_c future copies), whether more remain, and its error.
func (p *PendingRange) Get() ([]KeyValue, bool, error) {
	<-p.done
	return cloneReadKVs(p.kvs), p.more, p.err
}

// StartGetRange starts a range read and returns at once; GRV, location lookup and request run
// in the background, and Commit waits for the read whether or not Get is called (:1767).
func (tx *Transaction) StartGetRange(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) *PendingRange {
	return tx.startRange(ctx, begin, end, limit, byteTarget, reverse, false)
}

// StartGetRange is the snapshot form of Transaction.StartGetRange.
func (s *Snapshot) StartGetRange(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) *PendingRange {
	return s.tx.startRange(ctx, begin, end, limit, byteTarget, reverse, true)
}

func (tx *Transaction) startRange(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse, snapshot bool) *PendingRange {
	r := tx.issueRange(ctx, begin, end, limit, byteTarget, reverse, snapshot)
	p := &PendingRange{done: make(chan struct{})}
	if r.finished || r.local {
		p.kvs, p.more, p.err = r.run(false)
		close(p.done)
		return p
	}
	go func() {
		defer close(p.done)
		// RFC-110: a read-path panic fails this read; it must not abort the host.
		pb := panicBackstop{name: "rangeRead", db: tx.db}
		if pb.run(func() { p.kvs, p.more, p.err = r.run(false) }) {
			p.kvs, p.more, p.err = nil, false, &wire.FDBError{Code: ErrInternalError}
		}
	}()
	return p
}
