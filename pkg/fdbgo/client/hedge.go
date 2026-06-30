package client

import (
	"context"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// rpcAccount carries the QueueModel bookkeeping for one started request:
// the value returned by startRequest (delta) plus the server and start time.
// Every startRequest must be matched by exactly one endRequest or its
// smoothOutstanding contribution leaks permanently.
type rpcAccount struct {
	addr  string
	delta float64
	start time.Time
}

// hedgeResult is the result of a hedged RPC.
//
// addr/delta/start describe the WINNER (or the single in-flight request) — the
// caller does its full accounting (latency, penalty, futureVersion) on these.
// others lists every OTHER request that startRequest was called for but which
// did not become the winner: the hedge loser, or BOTH arms on a timeout/cancel.
// The caller must endRequest each of these exactly once, else their delta leaks.
// Matches C++ LoadBalance.actor.h, where ModelHolder releases the delta for the
// winning request and every cancelled laggard alike.
type hedgeResult struct {
	body    []byte
	addr    string
	delta   float64
	start   time.Time
	err     error
	connErr bool // true if connection error (not a reply error)
	others  []rpcAccount
}

// sendFrameWithHedge sends an RPC to the primary server and, after hedgeDelay,
// sends the same RPC to a secondary server. Whichever replies first wins;
// the other's reply is discarded.
//
// This is the core of the speculative second request (secondDelay) optimization
// from C++ LoadBalance.actor.h. It improves p99 latency by racing two servers
// when the primary is slow.
//
// The caller provides two pre-built request functions (one per server).
// Each function dials, sends, and returns the reply channel + cleanup.
//
// Returns the winning result. The losing server's reply is cancelled.
func sendFrameWithHedge(
	ctx context.Context,
	hedgeDelay time.Duration,
	primary sendFunc,
	secondary sendFunc, // may be nil if only 1 server
	timeout time.Duration,
) hedgeResult {
	// Send to primary immediately.
	pResult := primary()
	if pResult.err != nil {
		// Primary failed to send — try secondary immediately (no hedge delay).
		if secondary != nil {
			sResult := secondary()
			if sResult.err != nil {
				return hedgeResult{err: sResult.err, connErr: true}
			}
			return waitForReply(ctx, sResult, timeout)
		}
		return hedgeResult{err: pResult.err, connErr: true}
	}

	if secondary == nil {
		// No secondary — just wait for primary.
		return waitForReply(ctx, pResult, timeout)
	}

	// Start hedge timer.
	timer := time.NewTimer(hedgeDelay)
	defer timer.Stop()

	// Wait for primary reply or hedge timer.
	select {
	case resp := <-pResult.replyCh:
		// Primary replied before hedge timer. Use it.
		pResult.replyHandle.Release()
		return processReply(pResult, resp)

	case <-timer.C:
		// Hedge timer fired — send to secondary.
		sResult := secondary()
		if sResult.err != nil {
			// Secondary failed to send — wait for primary only.
			return waitForReply(ctx, pResult, timeout)
		}

		// Race both replies.
		return raceReplies(ctx, pResult, sResult, timeout)

	case <-ctx.Done():
		pResult.replyHandle.Cancel()
		pResult.replyHandle.Release()
		// Return the primary's accounting so the caller endRequests its startRequest delta — like
		// waitForReply's ctx.Done branch. Dropping it (returning a bare {err}) leaked the started
		// request's QueueModel delta permanently, biasing server selection (RFC-010 #5; C++
		// LoadBalance's ModelHolder RAII releases on cancel too).
		return hedgeResult{addr: pResult.addr, delta: pResult.delta, start: pResult.start, err: ctx.Err()}
	}
}

// sendFunc prepares and sends an RPC to a server. Returns the in-flight state
// needed to wait for the reply. If err is non-nil, the send failed.
type sendFunc func() inFlightRPC

// inFlightRPC represents an in-flight RPC request.
type inFlightRPC struct {
	replyCh     <-chan transport.Response
	replyHandle *transport.ReplyHandle
	addr        string
	delta       float64
	start       time.Time
	err         error
}

func waitForReply(ctx context.Context, inflight inFlightRPC, timeout time.Duration) hedgeResult {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-inflight.replyCh:
		inflight.replyHandle.Release()
		return processReply(inflight, resp)
	case <-timer.C:
		inflight.replyHandle.Cancel()
		inflight.replyHandle.Release()
		// Internal reply timeout, distinct from a caller deadline: the read
		// path re-sends (libfdb_c loadBalance has no per-read client timeout).
		return hedgeResult{addr: inflight.addr, delta: inflight.delta, start: inflight.start, err: errReplyTimeout}
	case <-ctx.Done():
		inflight.replyHandle.Cancel()
		inflight.replyHandle.Release()
		return hedgeResult{addr: inflight.addr, delta: inflight.delta, start: inflight.start, err: ctx.Err()}
	}
}

func raceReplies(ctx context.Context, a, b inFlightRPC, timeout time.Duration) hedgeResult {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-a.replyCh:
		a.replyHandle.Release() // Winner: release
		b.replyHandle.Cancel()  // Loser: cancel + release
		b.replyHandle.Release()
		res := processReply(a, resp)
		res.others = []rpcAccount{accountOf(b)} // loser b: caller must endRequest it
		return res
	case resp := <-b.replyCh:
		b.replyHandle.Release() // Winner: release
		a.replyHandle.Cancel()  // Loser: cancel + release
		a.replyHandle.Release()
		res := processReply(b, resp)
		res.others = []rpcAccount{accountOf(a)} // loser a: caller must endRequest it
		return res
	case <-timer.C:
		a.replyHandle.Cancel()
		a.replyHandle.Release()
		b.replyHandle.Cancel()
		b.replyHandle.Release()
		// No winner before the internal reply timeout — both alternatives were
		// slow. Retryable internal signal, not a caller deadline (the read path
		// re-sends). Both started requests must still be ended by the caller.
		return hedgeResult{err: errReplyTimeout, others: []rpcAccount{accountOf(a), accountOf(b)}}
	case <-ctx.Done():
		a.replyHandle.Cancel()
		a.replyHandle.Release()
		b.replyHandle.Cancel()
		b.replyHandle.Release()
		return hedgeResult{err: ctx.Err(), others: []rpcAccount{accountOf(a), accountOf(b)}}
	}
}

// accountOf captures the QueueModel bookkeeping for an in-flight request so the
// caller can endRequest it exactly once.
func accountOf(r inFlightRPC) rpcAccount {
	return rpcAccount{addr: r.addr, delta: r.delta, start: r.start}
}

func processReply(inflight inFlightRPC, resp transport.Response) hedgeResult {
	if resp.Err != nil {
		return hedgeResult{
			addr:    inflight.addr,
			delta:   inflight.delta,
			start:   inflight.start,
			err:     resp.Err,
			connErr: true,
		}
	}
	return hedgeResult{
		body:  resp.Body,
		addr:  inflight.addr,
		delta: inflight.delta,
		start: inflight.start,
	}
}
