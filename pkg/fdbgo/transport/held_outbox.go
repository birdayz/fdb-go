package transport

import (
	"context"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// NotifySuspend may block on a handoff. Publish the sender's FIFO position first,
// and hold no admission locks needed by its successor or transport loops.
func (c *Conn) enqueueAdmitted(ctx context.Context, request writeReq, done <-chan struct{}, admission peerAdmission) (bool, error) {
	if admission.permit != nil {
		return c.enqueuePrefix(ctx, request, done, admission.permit, false)
	}
	if admission.held {
		return c.enqueueHeld(ctx, request, done)
	}
	return c.enqueueOrdered(ctx, request, done)
}

// A granted prefix owner cannot be blocked behind held admission: its frame is
// needed to release the finite cohort that lets the writer reach held frames.
func (c *Conn) enqueuePrefix(ctx context.Context, request writeReq, done <-chan struct{}, permit *sendqueue.Permit, counted bool) (enqueued bool, err error) {
	if !counted {
		c.queued.Add(1)
	}
	defer func() {
		if !enqueued {
			c.queued.Add(-1)
		}
	}()
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
	select {
	case c.prefixCh <- request:
	default:
		reader = false
		c.writeAdmission.RUnlock()
		NotifySuspend(ctx)
		c.writeAdmission.RLock()
		reader = true
		if c.writeClosed || c.ctx.Err() != nil {
			return false, errConnClosed
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		select {
		case c.prefixCh <- request:
		case <-done:
			return false, c.enqueueError(ctx)
		case <-c.ctx.Done():
			return false, errConnClosed
		}
	}
	enqueued = true
	permit.Release() // the writer owns this position before its successor runs
	return true, nil
}

// Only callers classified while the sealed cohort was still pending enter
// this channel. Later warm traffic stays ordinary; it cannot extend the cohort.
func (c *Conn) enqueueHeld(ctx context.Context, request writeReq, done <-chan struct{}) (enqueued bool, err error) {
	c.queued.Add(1)
	c.heldCount.Add(1)
	defer func() {
		if !enqueued {
			c.queued.Add(-1)
			c.heldCount.Add(-1)
			c.kickOutbox() // the last abandoned head may have no channel frame
		}
	}()
	var permit *sendqueue.Permit
	var first bool
	if c.heldWaiters.Pending() {
		permit, first = c.heldWaiters.Reserve()
	} else {
		c.heldAdmission.RLock()
		if c.heldClosed || c.ctx.Err() != nil {
			c.heldAdmission.RUnlock()
			return false, errConnClosed
		}
		if !c.heldWaiters.Pending() && c.tryHeldCounted(request) {
			enqueued = true
			c.heldAdmission.RUnlock()
			return true, nil
		}
		permit, first = c.heldWaiters.Reserve()
		c.heldAdmission.RUnlock()
	}
	defer permit.Release()
	if first && !c.finishHeldBarrier(ctx) {
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
	c.heldAdmission.RLock()
	reader := true
	defer func() {
		if reader {
			c.heldAdmission.RUnlock()
		}
	}()
	if c.heldClosed || c.ctx.Err() != nil {
		return false, errConnClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !c.tryHeldCounted(request) {
		reader = false
		c.heldAdmission.RUnlock()
		NotifySuspend(ctx)
		c.heldAdmission.RLock()
		reader = true
		if c.heldClosed || c.ctx.Err() != nil {
			return false, errConnClosed
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		select {
		case c.heldCh <- request:
			c.kickOutbox()
		case <-done:
			return false, c.enqueueError(ctx)
		case <-c.ctx.Done():
			return false, errConnClosed
		}
	}
	enqueued = true
	return true, nil
}

func (c *Conn) finishHeldBarrier(ctx context.Context) (open bool) {
	finished := false
	defer func() {
		if !finished {
			c.heldAdmission.Lock()
			open = !c.heldClosed
			c.heldAdmission.Unlock()
		}
	}()
	if !c.heldAdmission.TryLock() {
		NotifySuspend(ctx)
		c.heldAdmission.Lock()
	}
	open = !c.heldClosed
	c.heldAdmission.Unlock()
	finished = true
	return open
}

func (c *Conn) tryHeldCounted(request writeReq) bool {
	select {
	case c.heldCh <- request:
		c.kickOutbox()
		return true
	default:
		return false
	}
}

func (c *Conn) tryEnqueueHeld(request writeReq) bool {
	if c.heldWaiters.Pending() {
		return false
	}
	c.heldAdmission.RLock()
	defer c.heldAdmission.RUnlock()
	if c.heldClosed || c.ctx.Err() != nil || c.heldWaiters.Pending() {
		return false
	}
	c.queued.Add(1)
	c.heldCount.Add(1)
	if c.tryHeldCounted(request) {
		return true
	}
	c.heldCount.Add(-1)
	c.queued.Add(-1)
	c.kickOutbox()
	return false
}

// Held publication and abandonment share the tail wake. A warm writer must
// still observe a sender classified as held before the prefix drained, even
// when that sender had not incremented heldCount when the writer went idle.
func (c *Conn) kickOutbox() {
	select {
	case c.tailKick <- struct{}{}:
	default:
	}
}

// nextWrite is used only by writeLoop, including handoff to a sender's turn.
// A nonblocking call ends the current batch instead of waiting for a missing
// prefix member or held head: available prefix bytes must flush first.
func (c *Conn) nextWrite(block bool) (request writeReq, frame, ready bool) {
	for {
		if c.prefixOpen {
			select {
			case request = <-c.prefixCh:
				return request, true, true
			default:
			}
			select {
			case <-c.prefixDone:
				// Acceptance happens before the final permit releases. Recheck
				// after observing Drained, or its last frame could be overtaken.
				select {
				case request = <-c.prefixCh:
					return request, true, true
				default:
				}
				c.prefixOpen = false
				continue
			default:
			}
			if !block {
				return writeReq{}, false, false
			}
			select {
			case request = <-c.prefixCh:
				return request, true, true
			case <-c.prefixDone:
				continue
			case <-c.tailKick:
				return writeReq{}, false, true
			case <-c.ctx.Done():
				return writeReq{}, false, false
			}
		}
		if c.heldCount.Load() != 0 {
			select {
			case request = <-c.heldCh:
				c.heldCount.Add(-1)
				return request, true, true
			default:
			}
			if !block {
				return writeReq{}, false, false
			}
			select {
			case request = <-c.heldCh:
				c.heldCount.Add(-1)
				return request, true, true
			case <-c.tailKick:
				return writeReq{}, false, true
			case <-c.ctx.Done():
				return writeReq{}, false, false
			}
		}
		if c.hasPendingWrite {
			request = c.pendingWrite
			c.pendingWrite, c.hasPendingWrite = writeReq{}, false
			return request, true, true
		}
		if !block {
			select {
			case request = <-c.writeCh:
				if c.heldCount.Load() == 0 {
					return request, true, true
				}
				c.pendingWrite, c.hasPendingWrite = request, true
				continue // a held reservation published during receive takes priority
			default:
				return writeReq{}, false, false
			}
		}
		select {
		case request = <-c.writeCh:
			if c.heldCount.Load() == 0 {
				return request, true, true
			}
			c.pendingWrite, c.hasPendingWrite = request, true
		case <-c.tailKick:
			return writeReq{}, false, true
		case <-c.ctx.Done():
			return writeReq{}, false, false
		}
	}
}

type coldControl struct {
	request writeReq
	permit  *sendqueue.Permit
	held    bool
}

// The single worker must see controls in reservation order: consuming a later
// permit first would strand its predecessor behind that same worker.
func (c *Conn) reserveColdControl(order *sendqueue.Queue, request writeReq) (reserved, accepted, held bool) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	permit, _, reserved := order.TryReserve()
	if reserved {
		return true, c.tryEnqueueColdControl(request, permit, false), false
	}
	return false, false, order.Pending()
}

// readLoop cannot wait for a cold predecessor. One connection-owned worker
// drains this bounded queue; post-seal controls append directly to heldCh (or
// for D-NG wait in coldControls behind the prefix and heldWaiters).
func (c *Conn) tryEnqueueColdControl(request writeReq, permit *sendqueue.Permit, held bool) (enqueued bool) {
	defer func() {
		if !enqueued {
			permit.Release()
		}
	}()
	c.writeAdmission.RLock()
	defer c.writeAdmission.RUnlock()
	if c.controlsClosed || c.writeClosed || c.ctx.Err() != nil {
		return false
	}
	c.queued.Add(1)
	select {
	case c.coldControls <- coldControl{request: request, permit: permit, held: held}:
		return true
	default:
		c.queued.Add(-1)
		return false
	}
}

func (c *Conn) runColdControl(control coldControl) {
	defer control.permit.Release()
	ownsCredit := true
	defer func() {
		if ownsCredit {
			c.queued.Add(-1)
		}
	}()
	select {
	case <-control.permit.Ready():
	case <-c.ctx.Done():
		return
	}
	if control.held {
		select {
		case <-c.prefixDone:
		case <-c.ctx.Done():
			return
		}
	}
	if c.na != nil {
		_ = c.naSend(c.ctx, control.request.token, control.request.body)
		return
	}
	ownsCredit = false // enqueuePrefix owns the existing queued credit
	_, _ = c.enqueuePrefix(c.ctx, control.request, nil, control.permit, true)
}

func (c *Conn) coldControlLoop() {
	defer c.loopWG.Done()
	defer c.finishColdControls()
	defer c.recoverLoop("coldControlLoop")
	if c.na != nil {
		for {
			select {
			case control := <-c.coldControls:
				c.runColdControl(control)
			case <-c.ctx.Done():
				return
			}
		}
	}
	for {
		select {
		case control := <-c.coldControls:
			c.runColdControl(control)
		case <-c.prefixDone:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Conn) finishColdControls() {
	c.writeAdmission.Lock()
	defer c.writeAdmission.Unlock()
	c.controlsClosed = true
	for {
		select {
		case control := <-c.coldControls:
			c.queued.Add(-1)
			control.permit.Release()
		default:
			return
		}
	}
}
