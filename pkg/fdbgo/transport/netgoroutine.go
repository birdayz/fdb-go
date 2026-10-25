package transport

import (
	"context"
	"os"
	"time"

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
			default:
				NotifySuspend(ctx) // waits behind the cold cohort, as enqueueHeld does
			}
			select {
			case <-order.Drained():
			case <-ctx.Done():
				return false, ctx.Err()
			case <-c.ctx.Done():
				return false, errConnClosed
			}
		}
	}
	if c.ctx.Err() != nil {
		return false, errConnClosed
	}
	if flush {
		return true, nil
	}
	if err := c.labSend(token, body); err != nil {
		// D-M writes on this goroutine: a failed write may have reached the peer.
		// Not sending at all reports errConnClosed itself; anything else is a write failure.
		return c.m != nil && err != errConnClosed, err
	}
	return true, nil
}

// coalesceFromSender is experiment (b): writeLoop times its coalescing delay
// from when the batch's first frame was queued, not from when it picked it up.
var coalesceFromSender = os.Getenv("FDBGO_COALESCE_FROM_SENDER") == "1"

func (c *Conn) stampQueued() {
	if coalesceFromSender && c.firstQueued.Load() == 0 {
		c.firstQueued.CompareAndSwap(0, monoNow())
	}
}

var monoBase = time.Now()

func monoNow() int64 { return int64(time.Since(monoBase)) }
