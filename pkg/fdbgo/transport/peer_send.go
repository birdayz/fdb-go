package transport

import (
	"context"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// WithSendOrder attaches one finite cold cohort before the connection loops
// start. Prefix and held frames have independent bounded admission channels.
func WithSendOrder(order *sendqueue.Queue) DialOption {
	return func(c *Conn) {
		c.peerSendOrder.Store(order)
		if order != nil {
			c.prefixCh = make(chan writeReq, cap(c.writeCh))
			c.heldCh = make(chan writeReq, cap(c.writeCh))
			c.coldControls = make(chan coldControl, cap(c.writeCh))
			c.prefixDone = order.Drained()
			c.prefixOpen = true
			c.coldOrderActive.Store(true)
		}
	}
}

// ClearSendOrder detaches a drained dial generation. The writer retains its
// finite prefix and held channels until those accepted frames have drained.
func (c *Conn) ClearSendOrder(order *sendqueue.Queue) {
	c.peerSendOrder.CompareAndSwap(order, nil)
}

type peerAdmission struct {
	permit *sendqueue.Permit
	held   bool
}

func (a peerAdmission) ordinary() bool { return a.permit == nil && !a.held }

func (c *Conn) beginSend(ctx context.Context, permit *sendqueue.Permit) (peerAdmission, error) {
	if permit == nil {
		if order := c.peerSendOrder.Load(); order != nil {
			var reserved bool
			permit, _, reserved = order.TryReserve()
			if !reserved && order.Pending() {
				// Waiting for Drained here would let an unbuilt frame suspend
				// before establishing its held-outbox position. Nil is not a
				// permit whose Ready a post-dial sender should wait on.
				return peerAdmission{held: true}, nil
			}
		}
	}
	if permit == nil {
		return peerAdmission{}, nil
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
		return peerAdmission{}, ctx.Err()
	case <-c.ctx.Done():
		return peerAdmission{}, errConnClosed
	}
	if err := ctx.Err(); err != nil {
		return peerAdmission{}, err
	}
	if c.ctx.Err() != nil {
		return peerAdmission{}, errConnClosed
	}
	transferred = true
	return peerAdmission{permit: permit}, nil
}

func (c *Conn) peerSendPending() bool {
	order := c.peerSendOrder.Load()
	return order != nil && order.Pending()
}
