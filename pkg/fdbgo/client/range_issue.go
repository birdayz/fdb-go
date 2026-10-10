package client

import (
	"bytes"
	"context"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
)

// rangeRead is one issued range read. C++ RYW getRange picks the read's path,
// builds its RYWIterator and adds it to `reading` inside the synchronous call
// (ReadYourWrites.actor.cpp:1764-1767, :382); only the storage I/O is later.
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
	finished          bool // answered by the synchronous checks
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
	// C++ RYW::getRange: !limits.isValid() → range_limits_invalid (ReadYourWrites.actor.cpp:1749).
	// GetRangeLimits::isValid (FDBTypes.h:754) accepts rows >= 0 || rows == ROW_LIMIT_UNLIMITED(-1);
	// key_outside_legal_range is checked first (C++ order :1740 before :1749).
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
	default:
		r.path = rangePathRYW
		r.ryw = tx.ryw.issueRange(begin, end, limit, byteTarget, reverse)
	}
	r.inc = tx.enterIssuedRead(ctx)
	r.afterReply = tx.afterRangeReply
	return r
}

// enterIssuedRead counts a read Commit must wait for, as C++ adds each read's
// future to `reading` at the call. It returns nil when the read's incarnation
// has already ended, since that read can only fail.
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

// run performs the read's I/O and records its read conflict. Exactly once.
func (r *rangeRead) run() ([]KeyValue, bool, error) {
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
	switch r.path {
	case rangePathRYWDisabled:
		kvs, more, err = server(ctx, r.begin, r.end, r.limit, r.byteTarget, r.reverse)
	case rangePathSnapshotNoRYW:
		kvs, more, err = tx.ryw.getSnapshotRange(ctx, r.begin, r.end, r.limit, r.byteTarget, r.reverse, server)
		kvs = cloneReadKVs(kvs)
	default:
		kvs, more, err = r.ryw.run(ctx, server)
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

// addConflicts adds the read conflict CLAMPED to the data returned (RFC-121 D1)
// and, on the RYW path, filtered through the issue-time write map (RFC-121 D2).
// An inverted range and \xff\xff special keys add none, as in the C++ client.
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

// waitIssuedReads is Commit's wait on `reading` (ReadYourWrites.actor.cpp:1359-1360):
// every issued range read and watch setup completes, recording its conflict or
// error, before the commit request is built.
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

// PendingRange is a range read whose request was sent when it was started,
// as fdb_transaction_get_range returns a future for an already-issued read.
type PendingRange struct {
	done chan struct{}
	kvs  []KeyValue
	more bool
	err  error
}

// Get waits for the read and returns its rows, whether more remain, and its error.
// Each call returns its own copy of the rows, as each Get of a libfdb_c range
// future copies out of the future's buffer.
func (p *PendingRange) Get() ([]KeyValue, bool, error) {
	<-p.done
	return cloneReadKVs(p.kvs), p.more, p.err
}

// StartGetRange issues a range read and returns once its first storage request
// is sent (or the read finished without one). The read completes, records its
// read conflict and is waited for by Commit whether or not Get is ever called,
// as an unconsumed libfdb_c range future does (ReadYourWrites.actor.cpp:1767).
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
	if r.finished {
		p.kvs, p.more, p.err = r.kvs, r.more, r.err
		close(p.done)
		return p
	}
	sent := &readSent{ch: make(chan struct{})}
	r.ctx = context.WithValue(r.ctx, readSentKey{}, sent)
	go func() {
		defer close(p.done)
		p.kvs, p.more, p.err = r.run()
	}()
	select {
	case <-sent.ch:
	case <-p.done:
	}
	return p
}

type readSentKey struct{}

// readSent is closed when a started read's first storage request is on the wire.
type readSent struct {
	once sync.Once
	ch   chan struct{}
}

func markReadSent(ctx context.Context) {
	if s, ok := ctx.Value(readSentKey{}).(*readSent); ok {
		s.once.Do(func() { close(s.ch) })
	}
}
