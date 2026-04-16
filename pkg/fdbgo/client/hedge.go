package client

import (
	"context"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

// hedgeResult is the result of a hedged RPC.
type hedgeResult struct {
	body    []byte
	addr    string
	delta   float64
	start   time.Time
	err     error
	connErr bool // true if connection error (not a reply error)
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
		return hedgeResult{err: ctx.Err()}
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
		return hedgeResult{addr: inflight.addr, delta: inflight.delta, start: inflight.start, err: context.DeadlineExceeded}
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
		return processReply(a, resp)
	case resp := <-b.replyCh:
		b.replyHandle.Release() // Winner: release
		a.replyHandle.Cancel()  // Loser: cancel + release
		a.replyHandle.Release()
		return processReply(b, resp)
	case <-timer.C:
		a.replyHandle.Cancel()
		a.replyHandle.Release()
		b.replyHandle.Cancel()
		b.replyHandle.Release()
		return hedgeResult{err: context.DeadlineExceeded}
	case <-ctx.Done():
		a.replyHandle.Cancel()
		a.replyHandle.Release()
		b.replyHandle.Cancel()
		b.replyHandle.Release()
		return hedgeResult{err: ctx.Err()}
	}
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
