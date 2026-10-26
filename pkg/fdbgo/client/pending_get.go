package client

import (
	"bytes"
	"context"
	"errors"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// Resolve serializes the owner of a pending read's response, timer, handle and
// retained context. A retired future may consume a ready reply, never retry
// through the replacement transaction. Repeated callers receive the memo.
func (p *PendingGet) Resolve() ([]byte, error) {
	val, err, _ := p.ResolveOrAbort(nil)
	return val, err
}

// handoff carries an aborted wait's remaining work to a goroutine that finishes
// the read, so commit still sees it, as C++ keeps a cancelled read in `reading`.
type handoff struct{ finish func() ([]byte, error) }

func (*handoff) Error() string { return "fdbgo: pending get handed off" }

// ResolveOrAbort is Resolve whose waits also end when abort closes, as a
// cancelled future's waiter wakes in libfdb_c; the read still completes.
func (p *PendingGet) ResolveOrAbort(abort <-chan struct{}) ([]byte, error, bool) {
	if p.settledFlag.Load() {
		return p.memoVal, p.memoErr, false
	}
	op := p.tx.readOperation(p.ctx)
	p.tx.readErrMu.Lock()
	inc := p.tx.readLife
	if op != nil {
		inc = op.inc
	} else if inc == nil {
		inc = p.tx.readIncarnationLocked()
	}
	p.tx.readErrMu.Unlock()
	var lease *executionLease
	if inc.gen == p.gen {
		lease = p.tx.enterReadState(p.ctx, inc, false)
	}
	if !p.lockOrWait(abort) {
		lease.release()
		if p.settledFlag.Load() {
			return p.memoVal, p.memoErr, false
		}
		return nil, nil, true
	}
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		if !p.done { // only a panic gets here unsettled; settle so queued waiters wake
			p.completeLocked(nil, errReadPanicked)
		}
		p.mu.Unlock()
		lease.release()
	}()
	if p.done {
		return p.memoVal, p.memoErr, false
	}
	if lease == nil {
		p.retireLocked()
		return p.memoVal, p.memoErr, false
	}
	admission := readAdmission{tx: p.tx, inc: inc, parent: p.ctx}
	if op != nil {
		admission = op.readAdmission
	}
	ctx := &readOperation{Context: p.ctx, readAdmission: admission, lease: lease}
	val, err := p.resolve(ctx, abort)
	if h, ok := err.(*handoff); ok {
		handedOff = true
		go p.finishHandoff(h, lease)
		return nil, nil, true
	}
	p.completeLocked(val, err)
	return p.memoVal, p.memoErr, false
}

// finishHandoff completes a handed-off read, then releases what the aborted
// waiter held.
func (p *PendingGet) finishHandoff(h *handoff, lease *executionLease) {
	defer lease.release()
	defer p.mu.Unlock()
	v, e := recoverRead(p, h.finish)
	p.completeLocked(v, e)
}

var errReadPanicked = errors.New("fdbgo: panic resolving a pipelined read")

// recoverRead runs fn on a read's own goroutine, turning a panic into the
// read's error so the process survives, as the network thread does in C++.
func recoverRead(p *PendingGet, fn func() ([]byte, error)) (val []byte, err error) {
	pb := &panicBackstop{name: "pipelined read", db: p.tx.db}
	if pb.run(func() { val, err = fn() }) {
		return nil, errReadPanicked
	}
	return val, err
}

// lockOrWait takes mu. With an abort it does not block on mu: it waits for the
// holder, which always completes the read, to settle it, or for abort.
func (p *PendingGet) lockOrWait(abort <-chan struct{}) bool {
	if abort == nil {
		p.mu.Lock()
		return true
	}
	if p.mu.TryLock() {
		return true
	}
	ch := p.settled.Load()
	if ch == nil {
		c := make(chan struct{})
		if p.settled.CompareAndSwap(nil, &c) {
			ch = &c
		} else {
			ch = p.settled.Load()
		}
	}
	if !p.settledFlag.Load() {
		select {
		case <-*ch:
		case <-abort:
		}
	}
	return false
}

func (p *PendingGet) interruption() error {
	if op := p.tx.readOperation(p.ctx); op != nil {
		if err := op.parent.Err(); err != nil {
			return err
		}
		p.tx.readErrMu.Lock()
		cause := op.inc.cause
		p.tx.readErrMu.Unlock()
		if cause != nil {
			return cause
		}
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return &wire.FDBError{Code: ErrTransactionCancelled}
}

func (p *PendingGet) retireLocked() {
	if p.done {
		return
	}
	var value []byte
	err := p.interruption()
	if p.replyHandle != nil {
		if response, ready := p.replyHandle.TakeReadyOrCancel(); ready {
			p.replyConsumed = true
			v, e, retry := p.readReply(response)
			if !retry {
				value, err = v, e
			}
		}
	}
	p.completeLocked(value, err)
}

func (p *PendingGet) releaseWireLocked() {
	if p.timer != nil {
		putTimer(p.timer)
		p.timer = nil
	}
	if p.replyHandle != nil {
		if !p.replyConsumed {
			p.replyHandle.Cancel()
		}
		p.replyHandle.Release()
		p.replyHandle = nil
	}
}

func (p *PendingGet) completeLocked(value []byte, err error) {
	p.releaseWireLocked()
	p.done = true
	// The cache retains the reply; this future owns a separate, mutable memo.
	p.memoVal = bytes.Clone(value)
	p.memoErr = p.tx.trackReadErrorGen(err, p.gen)
	// After the memo: lock-free readers return it once they see the flag.
	if !p.settledFlag.Swap(true) {
		if ch := p.settled.Load(); ch != nil {
			close(*ch)
		}
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.tx.readErrMu.Lock()
	if p.gen == p.tx.readGen {
		p.tx.removePendingReadLocked(p)
	}
	p.tx.readErrMu.Unlock()
}

// full re-sends the read; with an abort it runs on its own goroutine so the
// wait can be handed off. keepTags continues this request's read tag draw.
func (p *PendingGet) full(ctx context.Context, abort <-chan struct{}, keepTags bool) ([]byte, error) {
	if abort == nil {
		return p.resolveFull(ctx, keepTags)
	}
	type result struct {
		val []byte
		err error
	}
	res := make(chan result, 1)
	go func() {
		v, e := recoverRead(p, func() ([]byte, error) { return p.resolveFull(ctx, keepTags) })
		res <- result{v, e}
	}()
	select {
	case r := <-res:
		return r.val, r.err
	case <-abort:
		return nil, &handoff{finish: func() ([]byte, error) {
			r := <-res
			return r.val, r.err
		}}
	}
}

func (p *PendingGet) resolve(ctx context.Context, abort <-chan struct{}) ([]byte, error) {
	if !p.flushed {
		p.flushed = true
		// FlushContext checks ctx, cold-dial order, and liveness on D-NG (or waits
		// for the deferred batch's flush acknowledgment on WithLegacyWriter()).
		if err := p.conn.FlushContext(ctx); err != nil {
			if response, ready := p.replyHandle.TakeReadyOrCancel(); ready {
				p.replyConsumed = true
				return p.resolveReply(ctx, response, abort)
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				p.tx.db.handleConnError(p.addr, p.conn)
			}
			p.releaseWireLocked()
			return p.full(ctx, abort, true)
		}
	}
	select {
	case response := <-p.replyCh:
		p.replyConsumed = true
		return p.resolveReply(ctx, response, abort)
	default:
	}
	if p.timer == nil {
		p.timer = getTimer(max(p.tx.readRPCTimeout()-time.Since(p.sentAt), 0))
	}
	select {
	case response := <-p.replyCh:
		p.replyConsumed = true
		return p.resolveReply(ctx, response, abort)
	case <-p.timer.C:
		p.releaseWireLocked()
		return p.full(ctx, abort, true)
	case <-abort:
		return nil, &handoff{finish: func() ([]byte, error) { return p.resolve(ctx, nil) }}
	case <-ctx.Done():
		if response, ready := p.replyHandle.TakeReadyOrCancel(); ready {
			p.replyConsumed = true
			return p.resolveReply(ctx, response, abort)
		}
		return nil, p.interruption()
	}
}

func (p *PendingGet) readReply(response transport.Response) ([]byte, error, bool) {
	if response.Err != nil {
		return nil, response.Err, true
	}
	value, _, err := parseGetValueReply(response.Body)
	if isWrongShardServer(err) || isAllAlternativesFailed(err) {
		return nil, err, true
	}
	if err == nil && p.tx.db != nil && !response.RecvAt.IsZero() {
		p.tx.db.metrics.observeReadLatency(response.RecvAt.Sub(p.sentAt))
	}
	return value, err, false
}

func (p *PendingGet) resolveReply(ctx context.Context, response transport.Response, abort <-chan struct{}) ([]byte, error) {
	value, err, retry := p.readReply(response)
	if !retry {
		if err == nil && !p.tx.rywDisabled {
			p.tx.ryw.cachePointResult(p.key, value)
		}
		return value, err
	}
	if response.Err != nil {
		if !isEndpointNotFound(response.Err) {
			p.tx.db.handleConnError(p.addr, p.conn)
		}
	} else {
		p.tx.db.locCache.invalidate(p.key, p.tenantID, false)
	}
	p.releaseWireLocked()
	// A transport failure is retried inside loadBalance with the same request;
	// wrong_shard_server or all_alternatives_failed rebuilds it.
	return p.full(ctx, abort, response.Err != nil)
}

func (p *PendingGet) resolveFull(ctx context.Context, keepTags bool) ([]byte, error) {
	if err := p.tx.readEntryError(ctx); err != nil {
		return nil, err
	}
	get := p.tx.getValue
	if keepTags {
		tags := p.tags
		get = func(ctx context.Context, key []byte) ([]byte, error) { return p.tx.getValueSampled(ctx, key, &tags) }
	}
	if p.tx.rywDisabled {
		return get(ctx, p.key)
	}
	return p.tx.ryw.getSnapshot(ctx, p.key, get)
}
