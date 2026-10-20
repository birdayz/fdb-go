package transport

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// writeCompletion has two owners after enqueue: the waiter and writeLoop.
// Cancellation drops only the waiter's reference. The last owner may recycle
// the channel, after the writer can no longer publish an acknowledgment.
type writeCompletion struct {
	refs atomic.Int32
	done chan error
}

var writeCompletionPool = sync.Pool{New: func() any {
	return &writeCompletion{done: make(chan error, 1)}
}}

func newWriteCompletion() *writeCompletion {
	c := writeCompletionPool.Get().(*writeCompletion)
	c.refs.Store(2)
	return c
}

func (c *writeCompletion) release() {
	if c.releaseRef() {
		select {
		case <-c.done:
		default:
		}
		writeCompletionPool.Put(c)
	}
}

func (c *writeCompletion) releaseRef() bool { return c.refs.Add(-1) == 0 }

func (c *writeCompletion) complete(err error) {
	c.done <- err
	c.release()
}

// SendFrameContext cancels the wait, not already-enqueued bytes. A direct write
// never blocks; queued bytes own a copy, so the caller may recycle body on return.
// enqueued reports ownership transfer, NOT delivery to the peer. An at-most-once
// commit must use its native-owner context, never public/caller cancellation.
func (c *Conn) SendFrameContext(ctx context.Context, token UID, body []byte) (enqueued bool, err error) {
	return c.writeContext(ctx, token, body, false, nil)
}

// FlushContext waits for an ordered flush marker without coupling a read's
// cancellation to the lifetime of the shared connection.
func (c *Conn) FlushContext(ctx context.Context) error {
	if c.queued.Load() == 0 && !c.peerSendPending() && c.ctx.Err() == nil {
		return ctx.Err()
	}
	_, err := c.writeContext(ctx, UID{}, nil, true, nil)
	return err
}

// SendFrameContextOrdered sends using a place reserved before a cold peer dial.
// The place is released on acceptance, before any flush or reply wait. Its owner
// must pass it here: a nil-permit send on the same connection can wait for that
// still-owned place and deadlock. Release abandoned places instead.
func (c *Conn) SendFrameContextOrdered(ctx context.Context, token UID, body []byte, permit *sendqueue.Permit) (bool, error) {
	return c.writeContext(ctx, token, body, false, permit)
}

// Keep hot-path defers open-coded: returns × defers must stay ≤ 15 in Go 1.26.
func (c *Conn) writeContext(ctx context.Context, token UID, body []byte, flush bool, permit *sendqueue.Permit) (bool, error) {
	if err := ctx.Err(); err != nil {
		permit.Release()
		return false, err
	}
	admission, err := c.beginSend(ctx, permit)
	if err != nil {
		return false, err
	}
	if admission.permit != nil {
		return c.writePrefixContext(ctx, token, body, flush, admission)
	}
	if !flush {
		LogSend(token, body)
		if admission.ordinary() {
			if direct, err := c.tryWriteDirect(token, body); direct {
				return true, err
			}
			if took, err := c.takeTurn(ctx, token, body); took {
				return true, err
			}
		}
	}
	return c.writeAdmittedContext(ctx, token, body, admission, newWriteCompletion())
}

// Keep this cold owner separate: returns × defers must stay ≤ 15 in Go 1.26.
func (c *Conn) writePrefixContext(ctx context.Context, token UID, body []byte, flush bool, admission peerAdmission) (bool, error) {
	defer admission.permit.Release()
	if !flush {
		LogSend(token, body)
	}
	return c.writeAdmittedContext(ctx, token, body, admission, newWriteCompletion())
}

// Keep this hot helper's defers open-coded: returns × defers must stay ≤ 15 in Go 1.26.
func (c *Conn) writeAdmittedContext(ctx context.Context, token UID, body []byte, admission peerAdmission, completion *writeCompletion) (enqueued bool, err error) {
	defer func() {
		completion.release() // caller reference, including abandonment
		if !enqueued {
			completion.release() // writer reference never transferred, including panic
		}
	}()
	request := writeReq{token: token, body: bytes.Clone(body), owned: completion}
	enqueued, err = c.enqueueAdmitted(ctx, request, ctx.Done(), admission)
	if !enqueued {
		return false, err
	}
	select {
	case err := <-completion.done:
		return true, err // an already-acknowledged flush does not suspend
	default:
	}
	NotifySuspend(ctx) // queued in order; the sender now waits for the flush
	select {
	case err := <-completion.done:
		return true, err
	case <-ctx.Done():
		select {
		case err := <-completion.done:
			return true, err
		default:
			return true, ctx.Err()
		}
	case <-c.ctx.Done():
		select {
		case err := <-completion.done:
			return true, err
		default:
			return true, errConnClosed
		}
	}
}

// SendFrameDeferredContext transfers an immutable copy to the write queue, but
// does not wait for its acknowledgment. The boolean reports enqueue, not wire
// delivery. FlushContext may subsequently wait for the batch.
func (c *Conn) SendFrameDeferredContext(ctx context.Context, token UID, body []byte) (bool, error) {
	return c.sendWithoutAck(ctx, token, body, false, nil)
}

// SendFrameEagerContext is SendFrameDeferredContext for a frame nothing is about
// to join: an idle writer lets the caller write it at once. (true, err) means the
// direct write failed: maybe delivered, and the connection is failed.
func (c *Conn) SendFrameEagerContext(ctx context.Context, token UID, body []byte) (bool, error) {
	return c.sendWithoutAck(ctx, token, body, true, nil)
}

// SendFrameDeferredContextOrdered is the deferred form with a cold dial place.
func (c *Conn) SendFrameDeferredContextOrdered(ctx context.Context, token UID, body []byte, permit *sendqueue.Permit) (bool, error) {
	return c.sendWithoutAck(ctx, token, body, false, permit)
}

// SendFrameEagerContextOrdered is the eager form with a cold dial place.
func (c *Conn) SendFrameEagerContextOrdered(ctx context.Context, token UID, body []byte, permit *sendqueue.Permit) (bool, error) {
	return c.sendWithoutAck(ctx, token, body, true, permit)
}

func (c *Conn) sendWithoutAck(ctx context.Context, token UID, body []byte, eager bool, permit *sendqueue.Permit) (bool, error) {
	if err := ctx.Err(); err != nil {
		permit.Release()
		return false, err
	}
	admission, err := c.beginSend(ctx, permit)
	if err != nil {
		return false, err
	}
	defer admission.permit.Release()
	LogSend(token, body)
	if eager && admission.ordinary() {
		if direct, err := c.tryWriteDirect(token, body); direct {
			return true, err
		}
	}
	return c.enqueueAdmitted(ctx, writeReq{token: token, body: bytes.Clone(body)}, ctx.Done(), admission)
}

// finishOwnedWrites runs after writer recovery has cancelled the connection,
// so blocked enqueue attempts release their admission read locks first.
func (c *Conn) finishOwnedWrites(batch []*writeCompletion) {
	for _, completion := range batch {
		if completion != nil {
			completion.complete(errConnClosed)
		}
	}
	c.writeAdmission.Lock()
	c.writeClosed = true
	for _, queue := range []chan writeReq{c.prefixCh, c.writeCh} {
		c.failWriteQueue(queue, false)
	}
	c.writeAdmission.Unlock()
	c.heldAdmission.Lock()
	c.heldClosed = true
	c.failWriteQueue(c.heldCh, true)
	c.heldAdmission.Unlock()
	if c.hasPendingWrite {
		c.queued.Add(-1)
		if c.pendingWrite.owned != nil {
			c.pendingWrite.owned.complete(errConnClosed)
		}
		c.pendingWrite, c.hasPendingWrite = writeReq{}, false
	}
}

func (c *Conn) failWriteQueue(queue <-chan writeReq, held bool) {
	for {
		select {
		case request := <-queue:
			c.queued.Add(-1)
			if held {
				c.heldCount.Add(-1)
			}
			if request.owned != nil {
				request.owned.complete(errConnClosed)
			}
		default:
			return
		}
	}
}
