package client

import (
	"context"
	"fmt"
	"math"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// grvRefreshMin is the floor for backgroundRefresher's adaptive delay.
// Matches C++ backgroundGrvUpdater's std::max(0.001, ...) clamp.
const grvRefreshMin = 1 * time.Millisecond

// nextGRVRefreshDelay computes how long backgroundRefresher should sleep
// before the next GRV proxy contact. Pure-function port of C++
// NativeAPI.actor.cpp:backgroundGrvUpdater's wait expression.
//
// proxyBudget = MAX_PROXY_CONTACT_LAG - (now - lastProxy)
// cacheBudget = (MAX_VERSION_CACHE_LAG - grvDelay) - (now - lastTime)
// next = max(grvRefreshMin, min(proxyBudget, cacheBudget))
//
// `grvDelay` is the EMA of recent GRV RPC latency — subtracted from the
// cache budget so the refresh completes before the cache becomes stale.
func nextGRVRefreshDelay(now, lastProxy, lastTime time.Time, grvDelay time.Duration) time.Duration {
	proxyBudget := maxProxyContactLag - now.Sub(lastProxy)
	cacheBudget := (maxVersionCacheLag - grvDelay) - now.Sub(lastTime)
	wait := proxyBudget
	if cacheBudget < wait {
		wait = cacheBudget
	}
	if wait < grvRefreshMin {
		wait = grvRefreshMin
	}
	return wait
}

// backgroundRefresher proactively keeps the cache fresh.
//
// Matches C++ NativeAPI.actor.cpp:backgroundGrvUpdater. Each iteration:
//
//	wait( max(0.001,
//	          min(MAX_PROXY_CONTACT_LAG - (now - lastProxyTime),
//	              (MAX_VERSION_CACHE_LAG - grvDelay) - (now - lastTime))) )
//	... issue GRV ...
//	grvDelay = (grvDelay + (now - curTime)) / 2  // EMA of RPC latency
//
// `grvDelay` is the EMA of GRV RPC latency, used as lead time so the
// refresh completes BEFORE the cache would expire. `lastProxyTime` is
// the last successful proxy contact (kept warm). `lastTime` is the
// timestamp of the cached read version.
//
// Pre-2026-04-25 this was a fixed `maxVersionCacheLag/2` ticker — 2× the
// RPC rate of C++ under low latency (when the cache budget is large).
// Now matches C++: refresh just-in-time, scaled by observed latency.
func (b *grvBatcher) backgroundRefresher(db *database) {
	defer db.wg.Done()

	// RFC-110: a panic in the refresh (sendGRVRequest decode, applyGRVReply)
	// must not abort the host — C++'s backgroundGrvUpdater catches every error,
	// backs off, and loops, leaving the stale cached version usable. The backstop
	// recovers + counts + rate-limited-logs; on a recovered panic the next wait
	// is the backoff (≤1s) instead of the just-in-time refresh delay, so a
	// deterministic bug re-fires at ≤1/s (the C++ Backoff analog) rather than
	// hot-spinning at grvRefreshMin (1ms).
	pb := &panicBackstop{name: "backgroundRefresher", db: db}

	// EMA of GRV RPC latency. C++ initializes to 0.001s (1ms).
	grvDelay := grvRefreshMin

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		wait := nextGRVRefreshDelay(
			time.Now(),
			time.Unix(0, db.grvCache.lastProxyContact.Load()),
			db.grvCache.freshnessInstant(),
			grvDelay,
		)
		if bo := pb.backoff(); bo > wait {
			wait = bo // a recovered panic last iteration: back off, don't hot-spin
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-timer.C:
			pb.run(func() {
				// Captured before dispatch, like the foreground path: a
				// background refresh must not resurrect a version an
				// invalidation retired while its RPC was in flight.
				tok := db.grvCache.token()
				requestTime := time.Now()
				refreshCtx, refreshCancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
				defer refreshCancel() // RFC-110: release the GRV timer even if sendGRVRequest panics
				// Refresh at this batcher's own priority, not always DEFAULT.
				// The BATCH batcher must refresh BATCH ratekeeper state
				// (lastRkBatch); the DEFAULT batcher refreshes DEFAULT
				// (lastRkDefault). SYSTEM_IMMEDIATE never reaches here because
				// its tryCache always returns false (refreshOnce never fires).
				// No tx waiters on a background refresh, so the batcher span has no
				// links: batchGRVSpanContext(nil) = {traceID 0, random spanID,
				// unsampled} — the no-sampled-link case, matching a C++ updater GRV.
				version, _, rkDefault, rkBatch, tagThrottleInfoEntries, _, attemptEpoch, err := b.sendGRVRequest(db, refreshCtx, b.priority, 1, batchGRVSpanContext(nil), nil)
				if err == nil {
					// The refresher ignores the reply's `locked` flag — equivalent
					// to C++'s background updater, whose non-lock-aware txn THROWS
					// 1038 on a locked DB after the cache update (:7409 precedes
					// :7425) and is caught by its own onError loop. Nothing surfaces
					// to users from a background refresh, and the cached path
					// fail-opens anyway (RFC-104).
					b.applyGRVReply(db, tok.withEpoch(attemptEpoch), requestTime, version, rkDefault, rkBatch, tagThrottleInfoEntries)
					// EMA update: grvDelay = (grvDelay + measured_latency) / 2.
					grvDelay = (grvDelay + time.Since(requestTime)) / 2
				}
			})
		case <-db.ctx.Done():
			return
		}
	}
}

// applyGRVReply updates all database state from a successful GRV response:
// version cache, proxy contact time, minAcceptableReadVersion, ratekeeper
// throttle state, and tag throttle info.
// Called from both flush() (batched request) and backgroundRefresher().
func (b *grvBatcher) applyGRVReply(db *database, tok cacheToken, requestTime time.Time, version int64, rkDefault, rkBatch bool, tagThrottleInfoEntries []types.TransactionTagThrottle) {
	// ONE publication for everything tryCache consults. Serving is decided by
	// freshness AND cooldown together, so publishing them separately — in
	// either order — leaves a state a concurrent reader can catch. Renewing
	// freshness first is the harmful one: an opted-in transaction is served
	// from an entry this reply just refreshed while the throttling the same
	// reply carried has not landed, and it bypasses the ratekeeper.
	//
	// C++ writes them as separate statements in the opposite order
	// (updateCachedReadVersion at NativeAPI.actor.cpp:7409, then the
	// lastRk*ThrottleTime stores at :7411-7416) and is safe regardless: no
	// wait() separates them, so the actor cannot yield and no other actor sees
	// the intermediate state. Go has real threads and no such guarantee, so
	// matching C++ here means reproducing which states an observer can SEE.
	// Publishing atomically is stronger than ordering the stores, and unlike an
	// ordering it cannot be undone by an innocent-looking edit to this
	// function.
	replyTime := time.Now()
	sameEpoch := db.grvCache.publishReplyAt(tok, false, requestTime, version, replyTime, rkDefault, rkBatch)
	// The REQUEST instant, not the reply instant: C++ sets
	// lastProxyRequestTime = trState->startTime (NativeAPI.actor.cpp:7408) while
	// the reply instant is reserved for the ratekeeper cooldowns (:7411-7416).
	// The refresher subtracts this from MAX_PROXY_CONTACT_LAG, so dating it at the
	// reply understates how long the proxy has gone uncontacted by a whole round
	// trip and lets the contact budget run over.
	//
	// Gated on the epoch, like the cooldowns: "when we last reached the proxies"
	// is a fact about a CLUSTER, and a reply from the previous one says nothing
	// about this one — it would suppress the very refresh that repopulates the
	// cache under the new epoch. The predicate comes back from the publication
	// rather than being recomputed here, so there is exactly one answer.
	//
	// It stays a separate atomic rather than joining the entry: nothing tryCache
	// consults reads it, so it is not part of the serve decision that has to be
	// observed as one value. It only paces the background refresher.
	//
	// The minimum-acceptable floor is gated on the SAME predicate, and for the
	// same reason with sharper consequences: it is a claim about a VERSION SPACE,
	// and a straggler from the previous cluster writes that cluster's numbers into
	// this one's floor. validateVersion then rejects every user-set version below
	// the previous cluster's space with transaction_too_old. The pollution also
	// closes the path that would fix it FASTEST: the bootstrap GRV in
	// ensureReadVersion fires only while the floor reads 0, and the handoff resets
	// it to 0 precisely to arm that. A handle that only ever uses SetReadVersion
	// takes no other GRV, so for it the straggler's write is not a delay — it is
	// permanent.
	//
	// So the floor does NOT rely on this sameEpoch verdict, which is decided in
	// publishReplyAt a few instructions earlier and can be overtaken by a handoff
	// in between. updateMinAcceptable re-reads the epoch AT ITS OWN CAS and stamps
	// what it writes, so a straggler is refused at the write rather than racing
	// it, and a value written just before a handoff goes inert on its own. The
	// sameEpoch gate here is the outer of two checks, not the only one.
	//
	// Gated on sameEpoch and NOT on mayInstall: an invalidation does not change
	// whose version space a reply came from, so a generation-blocked reply from
	// THIS cluster still describes this cluster's numbers. Same asymmetry the
	// floor and the cooldowns are built on — and this is one more arm the epoch
	// gate had not covered.
	//
	// C++ does not gate this at all: its minAcceptableReadVersion is an
	// unconditional std::min at NativeAPI.actor.cpp:7287 and :3871, and
	// switchConnectionRecord is equally exposed to a reply in flight across its
	// own switch. Go is stricter than the spec here, deliberately, because Go's
	// bootstrap-GRV repair is what a polluted floor suppresses.
	//
	// THE REMAINING RESIDUAL, and it is only the contact instant: it is a bare
	// atomic with no stamp, so a handoff between the verdict above and this store
	// can still leave a previous cluster's instant behind. That costs at most one
	// delayed refresh — the refresher re-derives it on its next contact and the
	// lag is bounded by MAX_PROXY_CONTACT_LAG — and it can wedge nothing, because
	// nothing gates a repair on it. Stamping it too, or moving it inside the
	// publication's CAS, is real work for a bounded, self-healing skew; the floor
	// got the treatment because the floor could wedge a handle and this cannot.
	if sameEpoch {
		db.grvCache.lastProxyContact.Store(requestTime.UnixNano())
	}
	// Outside the gate above ON PURPOSE — it carries its own, applied at the CAS.
	db.updateMinAcceptable(tok.epoch(), version)

	if len(tagThrottleInfoEntries) > 0 {
		parsed := parseTagThrottleInfo(tagThrottleInfoEntries)
		if parsed != nil {
			priority := grvPriorityToPriority(b.priority)
			db.tagThrottles.replace(priority, parsed)
		}
	}
}

// Load balance knobs — match C++ FLOW_KNOBS.
const (
	loadBalanceStartBackoff = 10 * time.Millisecond // LOAD_BALANCE_START_BACKOFF
	loadBalanceMaxBackoff   = 5 * time.Second       // LOAD_BALANCE_MAX_BACKOFF
	loadBalanceBackoffRate  = 2.0                   // LOAD_BALANCE_BACKOFF_RATE
)

type grvAttemptOutcome uint8

const (
	grvAttemptNextProxy grvAttemptOutcome = iota
	grvAttemptRetryProxies
	grvAttemptReply
	grvAttemptEnded
)

// sendGRVRequest cycles all GRV proxies, matching C++ basicLoadBalance
// with AtMostOnce::False. On broken_promise (transport error), tries next
// proxy. On FDB application error, propagates immediately. If all proxies
// fail, applies exponential backoff and retries — loops until success, ctx
// cancellation or Close (matching C++ infinite loop + quorum(ok,1) wait).
func (b *grvBatcher) sendGRVRequest(db *database, ctx context.Context, flags uint32, txnCount uint32, span types.SpanContext, tags []types.TransactionTagCount) (version int64, locked bool, rkDefaultThrottled, rkBatchThrottled bool, tagThrottleInfo []types.TransactionTagThrottle, proxyTagThrottledDuration float64, attemptEpoch int64, err error) {
	var backoff time.Duration
	closed := db.ctx.Done() // ctx may be a caller's, which Close does not cancel

retry:
	for {
		// A cancelled caller must not kick a topology refresh on its way out.
		if err := grvRPCEnded(ctx, db); err != nil {
			return 0, false, false, false, nil, 0, attemptEpoch, err
		}
		proxiesChanged := db.waitProxiesChanged()
		// Re-read the proxy list each cycle — topology may have refreshed — and
		// take the epoch FROM THAT SAME LOAD. A retry can cross a cluster
		// handoff, and a token minted once at dispatch would then describe a
		// cluster this attempt never talked to. There is no epoch-before-proxies
		// ordering to reason about any more: the epoch rides the snapshot, so it
		// is by construction the epoch of the proxies this attempt uses.
		var proxies []ProxyInfo
		proxies, attemptEpoch = db.getGRVProxies()
		if len(proxies) == 0 {
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			timedOut, waitErr := waitGRVBackoff(ctx, db, closed, proxiesChanged, backoff)
			if waitErr != nil {
				return 0, false, false, false, nil, 0, attemptEpoch, waitErr
			}
			if timedOut {
				backoff = nextGRVBackoff(backoff)
			} else {
				backoff = 0
			}
			continue
		}

		// Start from a round-robin offset to distribute load across proxies.
		startIdx := db.proxyRR.nextGRV(len(proxies))
		for i := 0; i < len(proxies); i++ {
			proxy := proxies[(startIdx+i)%len(proxies)]
			// basicLoadBalance skips a permanently failed alternative (LoadBalance.actor.h:790).
			if db.failMon.endpointFailed(proxy.Address, proxy.Token) {
				continue
			}
			v, lk, rkD, rkB, tti, ptd, outcome, attemptErr := b.sendGRVToProxy(db, ctx, closed, proxiesChanged, proxy, flags, txnCount, span, tags)
			switch outcome {
			case grvAttemptNextProxy:
				continue
			case grvAttemptRetryProxies:
				backoff = 0
				continue retry
			case grvAttemptEnded, grvAttemptReply:
				return v, lk, rkD, rkB, tti, ptd, attemptEpoch, attemptErr
			}
		}

		// All proxies exhausted — back off, woken by recovery or a topology change.
		if err := grvRPCEnded(ctx, db); err != nil {
			return 0, false, false, false, nil, 0, attemptEpoch, err
		}
		backoff = nextGRVBackoff(backoff)
		timedOut, waitErr := waitGRVBackoff(ctx, db, closed, proxiesChanged, backoff)
		if waitErr != nil {
			return 0, false, false, false, nil, 0, attemptEpoch, waitErr
		}
		if !timedOut {
			backoff = 0
		}
	}
}

func (b *grvBatcher) sendGRVToProxy(db *database, ctx context.Context, closed, proxiesChanged <-chan struct{}, proxy ProxyInfo, flags uint32, txnCount uint32, span types.SpanContext, tags []types.TransactionTagCount) (version int64, locked bool, rkDefaultThrottled, rkBatchThrottled bool, tagThrottleInfo []types.TransactionTagThrottle, proxyTagThrottledDuration float64, outcome grvAttemptOutcome, err error) {
	var replyCleanup replyGuard
	defer replyCleanup.close()
	var sendCleanup sendGuard
	defer sendCleanup.close()

	conn, permit, err := db.getOrDialForSend(ctx, proxy.Address)
	if err != nil {
		if ended := grvRPCEnded(ctx, db); ended != nil {
			return 0, false, false, false, nil, 0, grvAttemptEnded, ended
		}
		db.handleDialError(ctx, proxy.Address)
		return 0, false, false, false, nil, 0, grvAttemptNextProxy, nil
	}

	sendCleanup.permit = permit
	replyToken, replyCh, replyHandle := conn.PrepareReplyTo(proxy.Token)
	replyCleanup.handle = replyHandle
	body := buildGetReadVersionRequest(replyToken, flags, txnCount, span, tags)

	if err := sendReadFrame(ctx, conn, proxy.Token, body, replyHandle, permit, nil); err != nil {
		replyHandle.Cancel()
		replyCleanup.release()
		if ended := grvRPCEnded(ctx, db); ended != nil {
			return 0, false, false, false, nil, 0, grvAttemptEnded, ended
		}
		db.handleReadConnError(proxy.Address, conn, err)
		return 0, false, false, false, nil, 0, grvAttemptNextProxy, nil
	}

	// A throttled proxy may hold a GRV indefinitely. Like NativeAPI's
	// getConsistentReadVersion, retry only on transport or topology change.
	var resp transport.Response
	select {
	case resp = <-replyCh:
	case <-proxiesChanged:
		replyHandle.Cancel()
		replyCleanup.release()
		return 0, false, false, false, nil, 0, grvAttemptRetryProxies, nil
	case <-ctx.Done():
		replyHandle.Cancel()
		replyCleanup.release()
		return 0, false, false, false, nil, 0, grvAttemptEnded, ctx.Err()
	case <-closed:
		replyHandle.Cancel()
		replyCleanup.release()
		return 0, false, false, false, nil, 0, grvAttemptEnded, db.ctx.Err()
	}
	replyCleanup.release()
	if resp.Err != nil {
		db.handleReadConnError(proxy.Address, conn, resp.Err)
		return 0, false, false, false, nil, 0, grvAttemptNextProxy, nil
	}
	// A well-formed frame proves address reachability; clear failures before
	// classifying its reply so a retry on another proxy cannot skip the update.
	db.failMon.markAlive(proxy.Address)
	v, lk, rkD, rkB, tti, ptd, perr := parseGetReadVersionReply(resp.Body)
	// GRV is not at-most-once: C++ basicLoadBalance retries in-band 1100/1030
	// on the next alternative rather than returning them to the transaction.
	if dispositionForReplyError(perr, false) == replyTryNextAlternative {
		db.metrics.grvInBandMaybeDelivered.Add(1)
		db.noteBrokenPromise(perr, proxy.Address, proxy.Token)
		// Only that endpoint is marked (genericactors.actor.h:397-402); keep the
		// healthy connection: another role at this address may have an in-flight commit.
		return 0, false, false, false, nil, 0, grvAttemptNextProxy, nil
	}
	return v, lk, rkD, rkB, tti, ptd, grvAttemptReply, perr
}

func nextGRVBackoff(backoff time.Duration) time.Duration {
	if backoff == 0 {
		return loadBalanceStartBackoff
	}
	return time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
}

func waitGRVBackoff(ctx context.Context, db *database, closed, proxiesChanged <-chan struct{}, wait time.Duration) (timedOut bool, err error) {
	timer := time.NewTimer(wait)
	select {
	case <-timer.C:
		return true, nil
	case <-db.failMon.waitForRecovery():
		timer.Stop()
		return false, nil
	case <-proxiesChanged:
		timer.Stop()
		return false, nil
	case <-ctx.Done():
		timer.Stop()
		return false, ctx.Err()
	case <-closed:
		timer.Stop()
		return false, db.ctx.Err()
	}
}

// grvRPCEnded reports a caller's or the database's end, which must neither count
// against a proxy nor kick a topology refresh.
func grvRPCEnded(ctx context.Context, db *database) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return db.ctx.Err()
}

// grvPriorityToPriority converts GRV wire priority bits to TransactionPriority.
func grvPriorityToPriority(flags uint32) TransactionPriority {
	switch flags & grvPriorityMask {
	case grvPriorityBatch:
		return PriorityBatch
	case grvPrioritySystemImmediate:
		return PrioritySystemImmediate
	default:
		return PriorityDefault
	}
}

func buildGetReadVersionRequest(replyToken transport.UID, flags uint32, txnCount uint32, span types.SpanContext, tags []types.TransactionTagCount) []byte {
	req := types.GetReadVersionRequest{
		TransactionCount: txnCount,
		Flags:            flags,
		MaxVersion:       InvalidVersion,
		SpanContext:      span, // C++ GetReadVersionRequest req(span.context, …) (:7245)
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		// Tags carries the batch's per-tag occurrence counts. There is no
		// tag-specific GRV flag — flags encode priority only, and the throttle
		// decision is made per priority against cx->throttledTags client-side
		// (NativeAPI.actor.cpp:7538-7568).
		Tags: tags,
	}
	return req.MarshalFDB()
}

// parseGetReadVersionReply parses the ErrorOr-wrapped GRV response.
// Returns (version, locked, rkDefaultThrottled, rkBatchThrottled,
// tagThrottleInfo, proxyTagThrottledDuration, error). `locked` is the
// database-locked flag the proxy reports unconditionally
// (GrvProxyServer.actor.cpp:673); enforcement is client-side, per
// transaction (RFC-096).
func parseGetReadVersionReply(data []byte) (int64, bool, bool, bool, []types.TransactionTagThrottle, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return 0, false, false, false, nil, 0, fmt.Errorf("GRV: %w", err)
	}
	var reply types.GetReadVersionReply
	if err := reply.UnmarshalFromReader(&r); err != nil {
		return 0, false, false, false, nil, 0, fmt.Errorf("GRV: %w", err)
	}
	return reply.Version, reply.Locked, reply.RkDefaultThrottled, reply.RkBatchThrottled, reply.TagThrottleInfo, reply.ProxyTagThrottledDuration, nil
}
