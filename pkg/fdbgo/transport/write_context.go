package transport

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
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

// SendFrameContext is a cancellable read-send. It copies body: after return the
// caller may recycle its own encoding even if the queued copy has not yet been
// written. enqueued reports ownership transfer, NOT delivery to the peer.
// At-most-once commit dispatch must continue to use SendFrame instead.
func (c *Conn) SendFrameContext(ctx context.Context, token UID, body []byte) (enqueued bool, err error) {
	return c.writeContext(ctx, token, body, false)
}

// FlushContext waits for an ordered flush marker without coupling a read's
// cancellation to the lifetime of the shared connection.
func (c *Conn) FlushContext(ctx context.Context) error {
	if !c.hasDirty.Load() {
		return ctx.Err()
	}
	_, err := c.writeContext(ctx, UID{}, nil, true)
	return err
}

func (c *Conn) writeContext(ctx context.Context, token UID, body []byte, flush bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	completion := newWriteCompletion()
	defer completion.release() // caller reference, including abandonment
	request := writeReq{token: token, body: bytes.Clone(body), owned: completion}
	if !flush {
		LogSend(token, request.body)
	}
	enqueued, err := c.enqueueOwned(ctx, request)
	if !enqueued {
		completion.release() // ownership never transferred to the writer
		return false, err
	}
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
	if err := ctx.Err(); err != nil {
		return false, err
	}
	request := writeReq{token: token, body: bytes.Clone(body)}
	LogSend(token, request.body)
	enqueued, err := c.enqueueOwned(ctx, request)
	if enqueued {
		c.hasDirty.Store(true)
	}
	return enqueued, err
}

func (c *Conn) enqueueOwned(ctx context.Context, request writeReq) (bool, error) {
	// The writer closes admission before its final queue drain. A sender that
	// races connection cancellation cannot enqueue behind that drain.
	c.writeAdmission.RLock()
	defer c.writeAdmission.RUnlock()
	if c.writeClosed || c.ctx.Err() != nil {
		return false, errConnClosed
	}
	select {
	case c.writeCh <- request:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.ctx.Done():
		return false, errConnClosed
	}
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
	defer c.writeAdmission.Unlock()
	c.writeClosed = true
	for {
		select {
		case request := <-c.writeCh:
			if request.owned != nil {
				request.owned.complete(errConnClosed)
			}
		default:
			return
		}
	}
}
