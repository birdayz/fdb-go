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

// altFailureDelay is C++'s allAlternativesFailedDelay, jittered like delayJittered
// (flow.h:1481-1484).
func (db *database) altFailureDelay() time.Duration {
	d := db.failMon.allAlternativesFailedDelay(nowSeconds())
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

// altFailureWait returns how long to wait for one of servers to recover, zero unless
// all are excluded, and the recovery signal taken before that check.
func (db *database) altFailureWait(servers []ServerInfo) (<-chan struct{}, time.Duration) {
	if db.failMon == nil {
		return nil, 0
	}
	recovered := db.failMon.waitForRecovery()
	if !db.allExcluded(servers) {
		return nil, 0
	}
	return recovered, db.altFailureDelay()
}

// awaitAlternative waits until one of servers is no longer excluded, as okFuture
// (LoadBalance.actor.cpp:47-48), or reports all_alternatives_failed after wait.
func (db *database) awaitAlternative(ctx context.Context, closed <-chan struct{}, servers []ServerInfo, recovered <-chan struct{}, wait time.Duration) error {
	if hook := db.beforeAltFailureWait.Load(); hook != nil {
		(*hook)()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-recovered:
			// Any address's recovery signals; only these replicas end the wait.
			recovered = db.failMon.waitForRecovery()
			if !db.allExcluded(servers) {
				return nil
			}
		case <-timer.C:
			return &wire.FDBError{Code: ErrAllAlternativesFailed}
		case <-ctx.Done():
			return ctx.Err()
		case <-closed:
			return db.ctx.Err()
		}
	}
}

// notExcluded returns the replicas the failure monitor does not exclude.
func (db *database) notExcluded(servers []ServerInfo) []ServerInfo {
	if db.failMon == nil {
		return servers
	}
	now := nowSeconds()
	live := make([]ServerInfo, 0, len(servers))
	for _, s := range servers {
		if !db.failMon.excluded(s.Address, now) {
			live = append(live, s)
		}
	}
	return live
}

// allExcluded reports whether the failure monitor excludes every replica's address,
// loadBalance's "everything is down" (LoadBalance.actor.h:617-618).
func (db *database) allExcluded(servers []ServerInfo) bool {
	if db.failMon == nil || len(servers) == 0 {
		return false
	}
	now := nowSeconds()
	for _, s := range servers {
		if !db.failMon.excluded(s.Address, now) {
			return false
		}
	}
	return true
}

// allAlternativesFailed waits C++'s allAlternativesFailedDelay, jittered like
// delayJittered (flow.h:1481-1484), before reporting all_alternatives_failed,
// so a dead-endpoint storm cannot spin location lookups.
func (db *database) allAlternativesFailed(ctx context.Context) error {
	if err := sleepCtx(ctx, db.altFailureDelay()); err != nil {
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
