package client

import (
	"context"
	"errors"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
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
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !isEndpointNotFound(err) {
		db.handleConnError(addr)
	}
}

// isEndpointNotFound reports a reply failed by ENDPOINT_NOT_FOUND. The connection
// is healthy: C++ fails only that endpoint, never the address
// (FailureMonitor.actor.cpp:117-139), so callers must not close or mark it.
func isEndpointNotFound(err error) bool {
	return errors.Is(err, transport.ErrEndpointNotFound)
}

// noteBrokenPromise marks the replying endpoint not found on an in-band
// broken_promise, as C++ waitValueOrSignal does (genericactors.actor.h:397-402).
func (db *database) noteBrokenPromise(err error, addr string, token transport.UID) {
	var fdbErr *wire.FDBError
	if err != nil && errors.As(err, &fdbErr) && fdbErr.Code == ErrBrokenPromise {
		db.failMon.markEndpointNotFound(addr, token)
	}
}

// noteReadBrokenPromise is noteBrokenPromise for a storage read answered by the
// replica at addr on the given interface stream.
func (db *database) noteReadBrokenPromise(err error, servers []ServerInfo, addr string, endpoint int) {
	if err == nil {
		return
	}
	for _, s := range servers {
		if s.Address == addr {
			db.noteBrokenPromise(err, addr, getAdjustedEndpoint(s.Token, endpoint))
			return
		}
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
