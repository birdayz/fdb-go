package transport

import (
	"context"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// WithSendOrder shares a cold peer's outbox reservations with control and legacy
// senders that did not acquire their own place while waiting for its dial.
func WithSendOrder(order *sendqueue.Queue) DialOption {
	return func(c *Conn) { c.peerSendOrder.Store(order) }
}

// ClearSendOrder detaches a drained dial generation. Its pool owner serializes
// this with reservations; a stale generation cannot detach its replacement.
func (c *Conn) ClearSendOrder(order *sendqueue.Queue) {
	c.peerSendOrder.CompareAndSwap(order, nil)
}

func (c *Conn) beginSend(ctx context.Context, permit *sendqueue.Permit) (*sendqueue.Permit, error) {
	if permit == nil {
		for {
			order := c.peerSendOrder.Load()
			if order == nil || !order.Pending() {
				break
			}
			var reserved bool
			permit, _, reserved = order.TryReserve()
			if reserved {
				break
			}
			// A pool lookup sealed this drained generation after our Load.
			c.peerSendOrder.CompareAndSwap(order, nil)
		}
	}
	if permit == nil {
		return nil, nil
	}
	transferred := false
	defer func() {
		if !transferred {
			permit.Release()
		}
	}()
	select {
	case <-permit.Ready():
	default:
		NotifySuspend(ctx)
	}
	select {
	case <-permit.Ready():
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errConnClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.ctx.Err() != nil {
		return nil, errConnClosed
	}
	transferred = true
	return permit, nil
}

func (c *Conn) peerSendPending() bool {
	order := c.peerSendOrder.Load()
	return order != nil && order.Pending()
}

func acceptedPermit(permit *sendqueue.Permit) func() {
	if permit == nil {
		return nil
	}
	return permit.Release
}
