package client

import (
	"context"
	"errors"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// sendReadFrame owns a copy of the encoding through eventual writer completion.
// A caller interrupted during the flush must still observe an already-published
// reply (the server can reply before the socket writer acknowledges its batch).
func sendReadFrame(ctx context.Context, conn *transport.Conn, token transport.UID, body []byte, reply *transport.ReplyHandle) error {
	_, err := conn.SendFrameContext(ctx, token, body)
	if err != nil && ctx.Err() != nil {
		if reply.KeepReadyOrCancel() {
			return nil
		}
		return ctx.Err()
	}
	return err
}

func (db *database) handleReadConnError(addr string, err error) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		db.handleConnError(addr)
	}
}

// A cancelled ModelHolder removes its outstanding delta without a latency
// sample (LoadBalance.actor.h:67, release(false, false, -1, false)).
func readFailureLatency(err error, start time.Time) time.Duration {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0
	}
	return time.Since(start)
}
