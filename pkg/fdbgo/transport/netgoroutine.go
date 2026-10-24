package transport

import (
	"context"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// ngWrite is the experimental network goroutine's send entry (netgoroutine_linux.go):
// it keeps cold-dial permit order, then hands the frame over without waiting
// for the write. A flush has nothing to wait for: the loop owns every frame.
func (c *Conn) ngWrite(ctx context.Context, token UID, body []byte, flush bool, permit *sendqueue.Permit) (bool, error) {
	if err := ctx.Err(); err != nil {
		permit.Release()
		return false, err
	}
	admission, err := c.beginSend(ctx, permit)
	if err != nil {
		return false, err
	}
	defer admission.permit.Release()
	if admission.held {
		if order := c.peerSendOrder.Load(); order != nil {
			select {
			case <-order.Drained():
			case <-ctx.Done():
				return false, ctx.Err()
			case <-c.ctx.Done():
				return false, errConnClosed
			}
		}
	}
	if flush {
		return true, nil
	}
	if err := c.ngSend(token, body); err != nil {
		return false, err
	}
	return true, nil
}
