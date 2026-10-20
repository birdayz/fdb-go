package client

import (
	"context"
	"errors"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// rpcAccount carries the QueueModel bookkeeping for one started request:
// the value returned by startRequest (delta) plus the server and start time.
// Every startRequest must be matched by exactly one endRequest or its
// smoothOutstanding contribution leaks permanently.
type rpcAccount struct {
	addr  string
	delta float64
	start time.Time
	err   error // transport failure of this request, for the caller to report
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
	primaryCleanup := replyGuard{handle: pResult.replyHandle}
	defer primaryCleanup.close()
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
		primaryCleanup.transfer()
		return waitForReply(ctx, pResult, timeout)
	}

	// Start hedge timer.
	timer := getTimer(hedgeDelay)
	defer putTimer(timer)

	// Wait for primary reply or hedge timer.
	select {
	case resp := <-pResult.replyCh:
		// Primary replied before hedge timer. Use it.
		primaryCleanup.release()
		res := processReply(pResult, resp)
		if res.err == nil || !movesToNextAlternative(ctx, res.err) {
			return res
		}
		// A failed alternative is not a tried secondary: C++ loadBalance moves on
		// to the next one (LoadBalance.actor.h:703-708, nextAlt at :728).
		sResult := secondary()
		if sResult.err != nil {
			return res
		}
		out := waitForReply(ctx, sResult, timeout)
		out.others = append(out.others, rpcAccount{addr: res.addr, delta: res.delta, start: res.start, err: res.err})
		return out

	case <-timer.C:
		// Hedge timer fired — send to secondary.
		sResult := secondary()
		if sResult.err != nil {
			// Secondary failed to send — wait for primary only.
			primaryCleanup.transfer()
			return waitForReply(ctx, pResult, timeout)
		}

		// Race both replies.
		primaryCleanup.transfer()
		return raceReplies(ctx, pResult, sResult, timeout)

	case <-ctx.Done():
		if result, ready := readyReplyOwned(pResult, &primaryCleanup); ready {
			return result
		}
		pResult.replyHandle.Cancel()
		primaryCleanup.release()
		// Return the primary's accounting so the caller endRequests its startRequest delta — like
		// waitForReply's ctx.Done branch. Dropping it (returning a bare {err}) leaked the started
		// request's QueueModel delta permanently, biasing server selection (RFC-010 #5; C++
		// LoadBalance's ModelHolder RAII releases on cancel too).
		return hedgeResult{addr: pResult.addr, delta: pResult.delta, start: pResult.start, err: ctx.Err()}
	}
}

// movesToNextAlternative reports whether C++ loadBalance retries the next
// alternative after this outcome (checkAndProcessResultImpl,
// LoadBalance.actor.h:344-377): a transport or maybe-delivered failure,
// server_overloaded or process_behind. Any other error is the answer, and a
// cancelled caller sends nothing more.
func movesToNextAlternative(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		return true
	}
	return isMaybeDelivered(err) || fdbErr.Code == ErrProcessBehind || fdbErr.Code == ErrServerOverloaded
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
	replyCleanup := replyGuard{handle: inflight.replyHandle}
	defer replyCleanup.close()
	timer := getTimer(timeout)
	defer putTimer(timer)

	select {
	case resp := <-inflight.replyCh:
		replyCleanup.release()
		return processReply(inflight, resp)
	case <-timer.C:
		inflight.replyHandle.Cancel()
		replyCleanup.release()
		// Internal reply timeout, distinct from a caller deadline: the read
		// path re-sends (libfdb_c loadBalance has no per-read client timeout).
		return hedgeResult{addr: inflight.addr, delta: inflight.delta, start: inflight.start, err: errReplyTimeout}
	case <-ctx.Done():
		if result, ready := readyReplyOwned(inflight, &replyCleanup); ready {
			return result
		}
		inflight.replyHandle.Cancel()
		replyCleanup.release()
		return hedgeResult{addr: inflight.addr, delta: inflight.delta, start: inflight.start, err: ctx.Err()}
	}
}

func raceReplies(ctx context.Context, a, b inFlightRPC, timeout time.Duration) hedgeResult {
	aCleanup, bCleanup := replyGuard{handle: a.replyHandle}, replyGuard{handle: b.replyHandle}
	defer aCleanup.close()
	defer bCleanup.close()
	timer := getTimer(timeout)
	defer putTimer(timer)

	select {
	case resp := <-a.replyCh:
		aCleanup.release()     // Winner: release
		b.replyHandle.Cancel() // Loser: cancel + release
		bCleanup.release()
		res := processReply(a, resp)
		res.others = []rpcAccount{accountOf(b)} // loser b: caller must endRequest it
		return res
	case resp := <-b.replyCh:
		bCleanup.release()     // Winner: release
		a.replyHandle.Cancel() // Loser: cancel + release
		aCleanup.release()
		res := processReply(b, resp)
		res.others = []rpcAccount{accountOf(a)} // loser a: caller must endRequest it
		return res
	case <-timer.C:
		a.replyHandle.Cancel()
		aCleanup.release()
		b.replyHandle.Cancel()
		bCleanup.release()
		// No winner before the internal reply timeout — both alternatives were
		// slow. Retryable internal signal, not a caller deadline (the read path
		// re-sends). Both started requests must still be ended by the caller.
		return hedgeResult{err: errReplyTimeout, others: []rpcAccount{accountOf(a), accountOf(b)}}
	case <-ctx.Done():
		if result, ready := readyReplyOwned(a, &aCleanup); ready {
			b.replyHandle.Cancel()
			bCleanup.release()
			result.others = []rpcAccount{accountOf(b)}
			return result
		}
		if result, ready := readyReplyOwned(b, &bCleanup); ready {
			a.replyHandle.Cancel()
			aCleanup.release()
			result.others = []rpcAccount{accountOf(a)}
			return result
		}
		a.replyHandle.Cancel()
		aCleanup.release()
		b.replyHandle.Cancel()
		bCleanup.release()
		return hedgeResult{err: ctx.Err(), others: []rpcAccount{accountOf(a), accountOf(b)}}
	}
}

// accountOf captures the QueueModel bookkeeping for an in-flight request so the
// caller can endRequest it exactly once.
func accountOf(r inFlightRPC) rpcAccount {
	return rpcAccount{addr: r.addr, delta: r.delta, start: r.start}
}

// readyReply preserves a queued response when cancellation and reply readiness
// are observed together, matching the read-first ordering of C++ RYW choose.
func readyReply(inflight inFlightRPC) (hedgeResult, bool) {
	owner := replyGuard{handle: inflight.replyHandle}
	return readyReplyOwned(inflight, &owner)
}

func readyReplyOwned(inflight inFlightRPC, owner *replyGuard) (hedgeResult, bool) {
	select {
	case resp := <-inflight.replyCh:
		owner.release()
		return processReply(inflight, resp), true
	default:
		return hedgeResult{}, false
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
