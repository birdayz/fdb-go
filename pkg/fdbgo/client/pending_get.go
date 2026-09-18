package client

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// Resolve serializes the owner of a pending read's response, timer, handle and
// retained context. A retired future may consume a ready reply, never retry
// through the replacement transaction. Repeated callers receive the memo.
func (p *PendingGet) Resolve() ([]byte, error) {
	p.mu.Lock()
	if p.done {
		value, err := p.memoVal, p.memoErr
		p.mu.Unlock()
		return value, err
	}
	p.mu.Unlock()
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
	defer lease.release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return p.memoVal, p.memoErr
	}
	if lease == nil {
		p.retireLocked()
		return p.memoVal, p.memoErr
	}
	parent := p.ctx
	if op := p.tx.readOperation(p.ctx); op != nil {
		parent = op.parent
	}
	ctx := context.WithValue(p.ctx, readContextKey{p.tx}, &readOperation{inc: inc, parent: parent, lease: lease})
	val, err := p.resolve(ctx)
	p.completeLocked(val, err)
	return p.memoVal, p.memoErr
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
	p.memoVal = value
	p.memoErr = p.tx.trackReadErrorGen(err, p.gen)
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.tx.readErrMu.Lock()
	if p.gen == p.tx.readGen {
		delete(p.tx.pendingReads, p)
	}
	p.tx.readErrMu.Unlock()
}

func (p *PendingGet) resolve(ctx context.Context) ([]byte, error) {
	if !p.flushed {
		p.flushed = true
		if err := p.conn.FlushContext(ctx); err != nil {
			if response, ready := p.replyHandle.TakeReadyOrCancel(); ready {
				p.replyConsumed = true
				return p.resolveReply(ctx, response)
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				p.tx.db.handleConnError(p.addr)
			}
			p.releaseWireLocked()
			return p.resolveFull(ctx)
		}
	}
	select {
	case response := <-p.replyCh:
		p.replyConsumed = true
		return p.resolveReply(ctx, response)
	case <-p.timer.C:
		p.releaseWireLocked()
		return p.resolveFull(ctx)
	case <-ctx.Done():
		if response, ready := p.replyHandle.TakeReadyOrCancel(); ready {
			p.replyConsumed = true
			return p.resolveReply(ctx, response)
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

func (p *PendingGet) resolveReply(ctx context.Context, response transport.Response) ([]byte, error) {
	value, err, retry := p.readReply(response)
	if !retry {
		return value, err
	}
	if response.Err != nil {
		p.tx.db.handleConnError(p.addr)
	} else {
		p.tx.db.locCache.invalidate(p.key, p.tenantID, false)
	}
	p.releaseWireLocked()
	return p.resolveFull(ctx)
}

func (p *PendingGet) resolveFull(ctx context.Context) ([]byte, error) {
	if err := p.tx.readEntryError(ctx); err != nil {
		return nil, err
	}
	return p.tx.getValue(ctx, p.key)
}
