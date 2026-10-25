package client

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// sendReadFrame owns a copy of the encoding through eventual writer completion.
// A caller interrupted during the flush must still observe an already-published
// reply (the server can reply before the socket writer acknowledges its batch).
func sendReadFrame(ctx context.Context, conn *transport.Conn, token transport.UID, body []byte, reply *transport.ReplyHandle, permit *sendqueue.Permit, replyWait func(context.Context)) error {
	_, err := conn.SendFrameContextOrdered(ctx, token, body, permit)
	if err != nil && ctx.Err() != nil {
		if !reply.KeepReadyOrCancel() {
			return ctx.Err()
		}
		err = nil
	}
	if err == nil {
		if replyWait != nil {
			replyWait(ctx)
		} else {
			reply.NotifyWait(ctx)
		}
	}
	return err
}

func (db *database) handleReadConnError(addr string, conn *transport.Conn, err error) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !isEndpointNotFound(err) {
		db.handleConnError(addr, conn)
	}
}

// endOther ends a started request that did not win the hedge, reporting the
// transport failure it carried.
func (db *database) endOther(o rpcAccount) {
	if o.err == nil {
		db.queueModel.endRequest(o.addr, o.delta, 0, false)
		return
	}
	db.handleReadConnError(o.addr, o.conn, o.err)
	db.queueModel.endRequest(o.addr, o.delta, readFailureLatency(o.err, o.start), false)
}

// allAlternativesFailed waits C++'s allAlternativesFailedDelay, jittered like
// delayJittered (flow.h:1481-1484), before reporting all_alternatives_failed,
// so a dead-endpoint storm cannot spin location lookups.
func (db *database) allAlternativesFailed(ctx context.Context) error {
	d := db.failMon.allAlternativesFailedDelay(nowSeconds())
	if err := sleepCtx(ctx, time.Duration(float64(d)*(0.9+0.2*rand.Float64()))); err != nil {
		return err
	}
	return &wire.FDBError{Code: ErrAllAlternativesFailed}
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
