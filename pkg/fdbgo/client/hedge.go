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
	conn  *transport.Conn // the connection the request was sent on
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
	conn    *transport.Conn
	delta   float64
	start   time.Time
	err     error
	connErr bool // true if connection error (not a reply error)
	others  []rpcAccount
	// triedSecondary reports that the secondary send was attempted.
	triedSecondary bool
}

// sendFrameWithHedge races the primary against a budget-funded secondary, or
// advances a failed lone primary (LoadBalance.actor.h:687-719; RFC-281).
func sendFrameWithHedge(
	ctx context.Context,
	model *QueueModel,
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
		noteLoneSendFailure(ctx, model)
		if secondary != nil {
			sResult := secondary()
			if sResult.err != nil {
				noteLoneSendFailure(ctx, model)
				return hedgeResult{err: sResult.err, connErr: true, triedSecondary: true}
			}
			out := waitLoneReply(ctx, model, sResult, timeout)
			out.triedSecondary = true
			return out
		}
		return hedgeResult{err: pResult.err, connErr: true}
	}

	if secondary == nil {
		primaryCleanup.transfer()
		return waitLoneReply(ctx, model, pResult, timeout)
	}

	timer := getTimer(hedgeDelay)
	defer putTimer(timer)

	select {
	case resp := <-pResult.replyCh:
		primaryCleanup.release()
		return primaryReplied(ctx, model, pResult, resp, secondary, timeout)

	case <-timer.C:
		// choose takes the first request's reply before the delay (:694, :710).
		select {
		case resp := <-pResult.replyCh:
			primaryCleanup.release()
			return primaryReplied(ctx, model, pResult, resp, secondary, timeout)
		default:
		}
		if ctx.Err() != nil {
			primaryCleanup.transfer()
			return waitLoneReply(ctx, model, pResult, timeout)
		}
		if !model.spendSecond() {
			primaryCleanup.transfer()
			return retryPrimaryReply(ctx, model, waitLoneReply(ctx, model, pResult, timeout), secondary, timeout)
		}
		sResult := secondary()
		primaryCleanup.transfer()
		if sResult.err != nil {
			// Paid for but unsent: the primary is waited for alone without
			// accruing, as loadBalance's !stream branch (:640-647).
			out := waitForReply(ctx, pResult, timeout)
			out.triedSecondary = true
			return out
		}
		out := raceReplies(ctx, pResult, sResult, timeout)
		out.triedSecondary = true
		return out

	case <-ctx.Done():
		if result, ready := readyReplyOwned(pResult, &primaryCleanup); ready {
			model.firstReplied()
			return result
		}
		pResult.replyHandle.Cancel()
		primaryCleanup.release()
		// Return the primary's accounting so the caller endRequests its startRequest delta — like
		// waitForReply's ctx.Done branch. Dropping it (returning a bare {err}) leaked the started
		// request's QueueModel delta permanently, biasing server selection (RFC-010 #5; C++
		// LoadBalance's ModelHolder RAII releases on cancel too).
		return hedgeResult{addr: pResult.addr, conn: pResult.conn, delta: pResult.delta, start: pResult.start, err: ctx.Err()}
	}
}

// primaryReplied accrues a lone reply and advances a failed alternative
// (LoadBalance.actor.h:694-708, nextAlt at :728).
func primaryReplied(ctx context.Context, model *QueueModel, p inFlightRPC, resp transport.Response, secondary sendFunc, timeout time.Duration) hedgeResult {
	model.firstReplied()
	return retryPrimaryReply(ctx, model, processReply(p, resp), secondary, timeout)
}

func retryPrimaryReply(ctx context.Context, model *QueueModel, res hedgeResult, secondary sendFunc, timeout time.Duration) hedgeResult {
	if res.err == nil || !res.connErr || !movesToNextAlternative(ctx, res.err) {
		return res
	}
	sResult := secondary()
	if sResult.err != nil {
		noteLoneSendFailure(ctx, model)
		res.triedSecondary = true
		return res
	}
	out := waitLoneReply(ctx, model, sResult, timeout)
	out.triedSecondary = true
	out.others = append(out.others, rpcAccount{addr: res.addr, conn: res.conn, delta: res.delta, start: res.start, err: res.err})
	return out
}

// waitLoneReply accrues a lone reply or transport failure, but not a timeout
// or caller cancellation.
func waitLoneReply(ctx context.Context, model *QueueModel, inflight inFlightRPC, timeout time.Duration) hedgeResult {
	res := waitForReply(ctx, inflight, timeout)
	if res.err == nil || res.connErr {
		model.firstReplied()
	}
	return res
}

// A failed lone send completes as request_maybe_delivered in C++ (fdbrpc.h:775-794).
// A caller-cancelled send is not a reply and earns nothing.
func noteLoneSendFailure(ctx context.Context, model *QueueModel) {
	if ctx.Err() == nil {
		model.firstReplied()
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
	conn        *transport.Conn
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
		return hedgeResult{addr: inflight.addr, conn: inflight.conn, delta: inflight.delta, start: inflight.start, err: errReplyTimeout}
	case <-ctx.Done():
		if result, ready := readyReplyOwned(inflight, &replyCleanup); ready {
			return result
		}
		inflight.replyHandle.Cancel()
		replyCleanup.release()
		return hedgeResult{addr: inflight.addr, conn: inflight.conn, delta: inflight.delta, start: inflight.start, err: ctx.Err()}
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
	return rpcAccount{addr: r.addr, conn: r.conn, delta: r.delta, start: r.start}
}

// readyReplyOwned preserves a queued response when cancellation and reply
// readiness are observed together, matching C++ RYW choose's read-first ordering.
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
			conn:    inflight.conn,
			delta:   inflight.delta,
			start:   inflight.start,
			err:     resp.Err,
			connErr: true,
		}
	}
	return hedgeResult{
		body:  resp.Body,
		addr:  inflight.addr,
		conn:  inflight.conn,
		delta: inflight.delta,
		start: inflight.start,
	}
}
