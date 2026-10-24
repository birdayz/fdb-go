package transport

import (
	"context"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// enqueueOrdered keeps full-queue senders in peer order before reporting their
// first wait. Every reservation remains counted until enqueue or abandonment.
// Keep hot-path defers open-coded: returns × defers must stay ≤ 15 in Go 1.26.
func (c *Conn) enqueueOrdered(ctx context.Context, request writeReq, done <-chan struct{}) (enqueued bool, err error) {
	c.queued.Add(1)
	c.stampQueued()
	reader := false
	defer func() {
		if reader {
			c.writeAdmission.RUnlock()
		}
		if !enqueued {
			c.queued.Add(-1)
		}
	}()

	var permit *sendqueue.Permit
	var first bool
	if c.sendWaiters.Pending() {
		// Reserve before RLock: a queued exclusive admission barrier must not
		// hide this sender's first wait behind a blocked FIFO head.
		permit, first = c.sendWaiters.Reserve()
	} else {
		c.writeAdmission.RLock()
		reader = true
		if c.writeClosed || c.ctx.Err() != nil {
			return false, errConnClosed
		}
		if !c.sendWaiters.Pending() && c.tryEnqueueCounted(request) {
			return true, nil
		}
		// Publish while still admitted, before a child can start at NotifySuspend.
		permit, first = c.sendWaiters.Reserve()
		reader = false
		c.writeAdmission.RUnlock()
	}
	return c.enqueueWaiting(ctx, request, done, permit, first)
}

func (c *Conn) enqueueWaiting(ctx context.Context, request writeReq, done <-chan struct{}, permit *sendqueue.Permit, first bool) (bool, error) {
	defer permit.Release()
	if first && !c.finishAdmissionBarrier(ctx) {
		return false, errConnClosed
	}
	select {
	case <-permit.Ready():
	default:
		NotifySuspend(ctx)
	}
	select {
	case <-permit.Ready():
	case <-done:
		return false, c.enqueueError(ctx)
	case <-c.ctx.Done():
		return false, errConnClosed
	}

	c.writeAdmission.RLock()
	reader := true
	defer func() {
		if reader {
			c.writeAdmission.RUnlock()
		}
	}()
	if c.writeClosed || c.ctx.Err() != nil {
		return false, errConnClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !c.tryEnqueueCounted(request) {
		reader = false
		c.writeAdmission.RUnlock()
		NotifySuspend(ctx) // the FIFO place exists before channel backpressure
		c.writeAdmission.RLock()
		reader = true
		if c.writeClosed || c.ctx.Err() != nil {
			return false, errConnClosed
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		select {
		case c.writeCh <- request:
		case <-done:
			return false, c.enqueueError(ctx)
		case <-c.ctx.Done():
			return false, errConnClosed
		}
	}
	return true, nil
}

// The first reservation drains warm readers that checked an empty FIFO before
// publication. It completes this barrier even on cancellation or a hook panic.
func (c *Conn) finishAdmissionBarrier(ctx context.Context) (open bool) {
	finished := false
	defer func() {
		if !finished {
			c.writeAdmission.Lock()
			open = !c.writeClosed
			c.writeAdmission.Unlock()
		}
	}()
	if !c.writeAdmission.TryLock() {
		NotifySuspend(ctx)
		c.writeAdmission.Lock()
	}
	open = !c.writeClosed
	c.writeAdmission.Unlock()
	finished = true
	return open
}

func (c *Conn) enqueueError(ctx context.Context) error {
	if err := ctx.Err(); err != nil && c.ctx.Err() == nil {
		return err
	}
	return errConnClosed
}

func (c *Conn) tryEnqueueCounted(request writeReq) bool {
	select {
	case c.writeCh <- request:
		return true
	default:
		return false
	}
}
