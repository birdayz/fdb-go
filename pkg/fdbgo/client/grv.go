package client

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// GRV cache knobs — matching C++ CLIENT_KNOBS defaults.
const (
	maxVersionCacheLag = 100 * time.Millisecond // MAX_VERSION_CACHE_LAG = 0.1s
	maxProxyContactLag = 200 * time.Millisecond // MAX_PROXY_CONTACT_LAG = 0.2s
	grvCacheRKCooldown = 60 * time.Second       // GRV_CACHE_RK_COOLDOWN = 60s
)

// grvCache holds the cached read version state.
// C++: DatabaseContext fields cachedReadVersion, lastGrvTime,
//
//	lastProxyRequestTime, lastRkBatchThrottleTime, lastRkDefaultThrottleTime.
//
// C++ does NOT explicitly invalidate this cache on proxy change — it relies
// on natural expiry via MAX_VERSION_CACHE_LAG (100ms). We match this behavior.
type grvCache struct {
	version          atomic.Int64 // monotonic (CAS loop, matches C++ guarded store)
	lastTime         atomic.Int64 // UnixNano
	lastProxyContact atomic.Int64 // UnixNano
	lastRkDefault    atomic.Int64 // ratekeeper throttle
	lastRkBatch      atomic.Int64
	// lastLocked is the database-locked flag from the most recent GRV reply
	// accepted by the version CAS. C++ never lock-checks ITS cached path —
	// but C++'s cache is opt-in (USE_GRV_CACHE, default off,
	// NativeAPI.actor.cpp:7505/:6148), so every default C++ transaction
	// reaches extractReadVersion's locked check (:7425). This cache is
	// ALWAYS-ON (a filed divergence; see TODO.md), so `locked` must ride it
	// or enforcement would fire roughly once per warm handle, ever. Stored
	// only on version-CAS acceptance (updateFromGRV) so a late stale reply
	// cannot overwrite fresher lock state. RFC-096.
	lastLocked atomic.Bool
}

// tryCache returns the cached version (and the database-locked flag stored
// with it) if it's fresh enough.
// priority determines which ratekeeper throttle to check:
// BATCH checks lastRkBatch, DEFAULT checks lastRkDefault.
// Matches C++ DatabaseContext::getConsistentReadVersion throttle checks.
func (c *grvCache) tryCache(priority uint32) (int64, bool, bool) {
	v := c.version.Load()
	if v == 0 {
		return 0, false, false
	}

	now := time.Now().UnixNano()
	lastTime := c.lastTime.Load()
	if time.Duration(now-lastTime) > maxVersionCacheLag {
		return 0, false, false // stale
	}

	// Check ratekeeper throttle cooldown for the requesting priority.
	// C++ checks lastRkBatchThrottleTime for BATCH, lastRkDefaultThrottleTime for DEFAULT.
	// SYSTEM_IMMEDIATE never reaches here (bypasses cache at callsite), but guard
	// explicitly to prevent bugs if the bypass is ever refactored.
	var lastThrottle int64
	switch priority {
	case grvPriorityBatch:
		lastThrottle = c.lastRkBatch.Load()
	case grvPrioritySystemImmediate:
		return 0, false, false // SYSTEM_IMMEDIATE must always contact proxy
	default:
		lastThrottle = c.lastRkDefault.Load()
	}
	if lastThrottle > 0 && time.Duration(now-lastThrottle) < grvCacheRKCooldown {
		return 0, false, false // throttled — must contact proxy
	}

	return v, c.lastLocked.Load(), true
}

// update advances the cached version from a successful commit.
// Monotonic: only accepts versions >= current cached version.
//
// It deliberately does NOT advance the freshness clock (lastTime) and does
// NOT touch lock state: a commit proves nothing about either. C++ DOES
// extend lastGrvTime here (NativeAPI.actor.cpp:6657 → :357-359), but its
// cache is opt-in and entirely fail-open by contract (no lock check on
// hits); with Go's ALWAYS-ON, enforcement-carrying cache, commit-extended
// freshness would let a handle that locks the database and keeps committing
// serve post-lock versions with stale locked=false metadata indefinitely
// (codex P1, RFC-096). Invariant: cache freshness == recency of the last
// ACCEPTED real GRV reply (updateFromGRV is the only lastTime writer).
func (c *grvCache) update(v int64) {
	for {
		cur := c.version.Load()
		if v < cur {
			return
		}
		if c.version.CompareAndSwap(cur, v) {
			return
		}
	}
}

// updateFromGRV updates the cache from a real GRV reply: version plus the
// database-locked flag. lastLocked is stored ONLY when the version CAS
// accepts the reply — a late, stale reply (older version, locked=false) must
// not overwrite fresher locked=true state (a fail-open hazard). The residual
// CAS→Store interleaving window between two concurrently-accepted replies:
// a spurious locked=true is genuinely benign (a retryable 1038 until the
// next refresh); a missed locked=true is bounded by maxVersionCacheLag and
// then corrected by the background refresher's next real fetch. RFC-096.
func (c *grvCache) updateFromGRV(t time.Time, v int64, locked bool) {
	for {
		cur := c.version.Load()
		if v < cur {
			return // stale reply — don't go backwards, don't touch lock state
		}
		if c.version.CompareAndSwap(cur, v) {
			break
		}
	}
	c.lastLocked.Store(locked)
	c.updateTime(t)
}

// updateTime advances lastTime only if strictly newer (matching C++).
func (c *grvCache) updateTime(t time.Time) {
	tNano := t.UnixNano()
	for {
		cur := c.lastTime.Load()
		if tNano <= cur {
			return
		}
		if c.lastTime.CompareAndSwap(cur, tNano) {
			return
		}
	}
}

// invalidate clears the cached version.
func (c *grvCache) invalidate() {
	c.version.Store(0)
	c.lastTime.Store(0)
}

// updateMinAcceptable atomically ratchets minAcceptableReadVersion upward.
// Matches C++ DatabaseContext::minAcceptableReadVersion.
func updateMinAcceptable(min *atomic.Int64, v int64) {
	for {
		cur := min.Load()
		if v <= cur {
			return
		}
		if min.CompareAndSwap(cur, v) {
			return
		}
	}
}

// validateVersion checks that a user-set read version is within the
// acceptable range. Matches C++ DatabaseContext::validateVersion().
// Returns transaction_too_old (1007) if version < minAcceptableReadVersion.
// Returns future_version (1009) for obviously absurd versions (>10^15) that
// the storage server would block on indefinitely. The server normally returns
// future_version after MAX_READ_TRANSACTION_LIFE_VERSIONS (5s), but our RPC
// timeout races with it — client-side detection is more reliable for extreme values.
func (db *database) validateVersion(version int64) error {
	min := db.minAcceptableReadVersion.Load()
	if min > 0 && version < min {
		return &wire.FDBError{Code: ErrTransactionTooOld}
	}
	// Reject absurd future versions client-side. Real FDB versions are ~10^7
	// per second; even at 100 years that's ~3×10^16. 10^15 is a safe threshold.
	if version > 1_000_000_000_000_000 {
		return &wire.FDBError{Code: ErrFutureVersion}
	}
	return nil
}

// grvBatcherIndex maps GRV priority bits to a batcher array index.
// C++ uses separate batchers for BATCH, DEFAULT, and SYSTEM_IMMEDIATE.
const (
	grvBatcherBatch           = 0
	grvBatcherDefault         = 1
	grvBatcherSystemImmediate = 2
)

// grvBatcherIndex returns the array index for the given priority flags.
func grvBatcherIndex(flags uint32) int {
	switch flags & grvPriorityMask {
	case grvPriorityBatch:
		return grvBatcherBatch
	case grvPrioritySystemImmediate:
		return grvBatcherSystemImmediate
	default:
		return grvBatcherDefault
	}
}

// grvBatcher batches concurrent GetReadVersion calls for a single priority.
// C++: DatabaseContext::VersionBatcher + readVersionBatcher actor.
//
// Each priority level (BATCH, DEFAULT, SYSTEM_IMMEDIATE) has its own batcher,
// so requests at different priorities never mix — matching C++ behavior.
//
// Methods receive *database as argument — no stored back-pointer.
type grvBatcher struct {
	mu        sync.Mutex
	pending   []grvRequest
	batchTime time.Duration
	timer     *time.Timer
	priority  uint32 // fixed priority bits for this batcher

	refreshOnce sync.Once
}

type grvRequest struct {
	reply chan grvResult
	flags uint32 // GRV Flags from the requesting transaction
}

type grvResult struct {
	version int64
	locked  bool // database-locked flag from the GRV reply (RFC-096)
	err     error
}

// getReadVersion returns a read version, using the cache if fresh. The
// second return is the database-locked flag from the reply (or from the
// cache entry) — the per-transaction lock check happens at the consumption
// site (the C++ extractReadVersion analog), NOT here: one batched reply
// fans out to transactions with different lock-awareness.
func (b *grvBatcher) getReadVersion(db *database, ctx context.Context, flags uint32) (int64, bool, error) {
	// Fast path: serve from cache if fresh and not throttled.
	// SYSTEM_IMMEDIATE bypasses cache — it needs a guaranteed-fresh version.
	isImmediate := b.priority == grvPrioritySystemImmediate
	if !isImmediate {
		if v, locked, ok := db.grvCache.tryCache(b.priority); ok {
			// Start background refresher on first cache hit.
			b.refreshOnce.Do(func() {
				db.wg.Add(1)
				go b.backgroundRefresher(db)
			})
			return v, locked, nil
		}
	}

	// Slow path: batch request to proxy.
	req := grvRequest{reply: make(chan grvResult, 1), flags: flags}

	b.mu.Lock()
	b.pending = append(b.pending, req)
	if len(b.pending) == 1 {
		b.timer = time.AfterFunc(b.batchTime, func() { b.flush(db) })
	}
	b.mu.Unlock()

	select {
	case result := <-req.reply:
		return result.version, result.locked, result.err
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}
}

// flush sends the batched GRV request and updates the cache.
//
// Lock is held only to pop the pending slice; the RPC executes without
// holding mu, so new requests can queue (and start a new timer) while
// the RPC is in flight.
func (b *grvBatcher) flush(db *database) {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	// Bound the GRV request. C++ cancels the actor when callers drop
	// the future. Our equivalent: context with timeout. If all callers
	// have given up (their ctx expired), this ensures the batcher
	// goroutine doesn't hang forever.
	batchCtx, batchCancel := context.WithTimeout(db.ctx, CoordinatorTimeout)
	defer batchCancel()

	// Each batcher has a fixed priority. OR all option flags (bits 0-23)
	// from requests in this batch.
	var optionBits uint32
	for _, r := range batch {
		optionBits |= r.flags &^ grvPriorityMask
	}
	flags := b.priority | optionBits

	requestTime := time.Now()
	version, locked, rkDefault, rkBatch, tagThrottleInfoBytes, _, err := b.sendGRVRequest(db, batchCtx, flags, uint32(len(batch)))
	elapsed := time.Since(requestTime)

	if err == nil {
		// Unconditional, even when locked: C++ updates the shared cache
		// BEFORE the per-transaction locked throw (NativeAPI.actor.cpp:7409
		// precedes :7425).
		b.applyGRVReply(db, requestTime, version, locked, rkDefault, rkBatch, tagThrottleInfoBytes)
		// C++ counts per-transaction in extractReadVersion (:7428-7440) — one
		// batched reply serves len(batch) transactions. Cache hits never reach
		// here (C++ parity: its cached path returns before the counters); the
		// background refresher has no waiters and adds nothing. Waiters whose
		// ctx expired mid-batch are still counted (C++ cancels abandoned
		// futures before its counter; accepted edge noise). RFC-097.
		db.metrics.countGRVBatchCompleted(b.priority, len(batch))
	}

	// Adaptive batch window.
	b.mu.Lock()
	b.batchTime = time.Duration(0.1*float64(elapsed)/2 + 0.9*float64(b.batchTime))
	if b.batchTime < 100*time.Microsecond {
		b.batchTime = 100 * time.Microsecond
	}
	if b.batchTime > 5*time.Millisecond { // C++ GRV_BATCH_TIMEOUT = 5ms
		b.batchTime = 5 * time.Millisecond
	}
	b.mu.Unlock()

	result := grvResult{version: version, locked: locked, err: err}
	for _, req := range batch {
		req.reply <- result
	}
}

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

	// EMA of GRV RPC latency. C++ initializes to 0.001s (1ms).
	grvDelay := grvRefreshMin

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		wait := nextGRVRefreshDelay(
			time.Now(),
			time.Unix(0, db.grvCache.lastProxyContact.Load()),
			time.Unix(0, db.grvCache.lastTime.Load()),
			grvDelay,
		)

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-timer.C:
			requestTime := time.Now()
			refreshCtx, refreshCancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
			// Refresh at this batcher's own priority, not always DEFAULT.
			// The BATCH batcher must refresh BATCH ratekeeper state
			// (lastRkBatch); the DEFAULT batcher refreshes DEFAULT
			// (lastRkDefault). SYSTEM_IMMEDIATE never reaches here because
			// its tryCache always returns false (refreshOnce never fires).
			version, locked, rkDefault, rkBatch, tagThrottleInfoBytes, _, err := b.sendGRVRequest(db, refreshCtx, b.priority, 1)
			refreshCancel()
			if err == nil {
				// The refresher stores `locked` into the cache and otherwise
				// ignores it — functionally equivalent to C++'s background
				// updater, whose non-lock-aware txn THROWS 1038 on a locked
				// DB after the cache update (:7409 precedes :7425) and is
				// caught by its own onError loop. Nothing surfaces to users
				// from a background refresh either way. RFC-096.
				b.applyGRVReply(db, requestTime, version, locked, rkDefault, rkBatch, tagThrottleInfoBytes)
				// EMA update: grvDelay = (grvDelay + measured_latency) / 2.
				grvDelay = (grvDelay + time.Since(requestTime)) / 2
			}
		case <-db.ctx.Done():
			return
		}
	}
}

// applyGRVReply updates all database state from a successful GRV response:
// version cache, proxy contact time, minAcceptableReadVersion, ratekeeper
// throttle state, and tag throttle info.
// Called from both flush() (batched request) and backgroundRefresher().
func (b *grvBatcher) applyGRVReply(db *database, requestTime time.Time, version int64, locked bool, rkDefault, rkBatch bool, tagThrottleInfoBytes []byte) {
	db.grvCache.updateFromGRV(requestTime, version, locked)
	db.grvCache.lastProxyContact.Store(time.Now().UnixNano())
	updateMinAcceptable(&db.minAcceptableReadVersion, version)

	if rkDefault {
		db.grvCache.lastRkDefault.Store(time.Now().UnixNano())
	}
	if rkBatch {
		db.grvCache.lastRkBatch.Store(time.Now().UnixNano())
	}

	if len(tagThrottleInfoBytes) > 0 {
		parsed := parseTagThrottleInfo(tagThrottleInfoBytes)
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

// sendGRVRequest cycles all GRV proxies, matching C++ basicLoadBalance
// with AtMostOnce::False. On broken_promise (transport error), tries next
// proxy. On FDB application error, propagates immediately. If all proxies
// fail, applies exponential backoff and retries — loops until success or
// db.ctx cancellation (matching C++ infinite loop + quorum(ok,1) wait).
func (b *grvBatcher) sendGRVRequest(db *database, ctx context.Context, flags uint32, txnCount uint32) (version int64, locked bool, rkDefaultThrottled, rkBatchThrottled bool, tagThrottleInfo []byte, proxyTagThrottledDuration float64, err error) {
	var backoff time.Duration

	for {
		// Re-read proxy list each cycle — topology may have refreshed.
		proxies := db.getGRVProxies()
		if len(proxies) == 0 {
			db.kickTopology()
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
				continue
			case <-db.failMon.waitForRecovery():
				timer.Stop()
				backoff = 0
				continue
			case <-db.waitProxiesChanged():
				timer.Stop()
				backoff = 0
				continue
			case <-ctx.Done():
				timer.Stop()
				return 0, false, false, false, nil, 0, ctx.Err()
			}
		}

		// Start from a round-robin offset to distribute load across proxies.
		startIdx := db.proxyRR.nextGRV(len(proxies))
		for i := 0; i < len(proxies); i++ {
			proxy := proxies[(startIdx+i)%len(proxies)]
			conn, err := db.getOrDial(ctx, proxy.Address)
			if err != nil {
				db.handleConnError(proxy.Address)
				continue
			}

			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body := buildGetReadVersionRequest(replyToken, flags, txnCount)

			if err := conn.SendFrame(proxy.Token, body); err != nil {
				replyHandle.Cancel()
				replyHandle.Release()
				db.handleConnError(proxy.Address)
				continue
			}

			resp, rpcErr := waitReply(replyCh, ctx, DefaultRPCTimeout)
			if rpcErr != nil {
				replyHandle.Cancel()
				replyHandle.Release()
				if ctx.Err() != nil {
					return 0, false, false, false, nil, 0, ctx.Err()
				}
				db.failMon.markFailed(proxy.Address)
				continue
			}
			replyHandle.Release()
			if resp.Err != nil {
				db.handleConnError(proxy.Address)
				continue
			}
			db.failMon.markAlive(proxy.Address)
			return parseGetReadVersionReply(resp.Body)
		}

		// All proxies exhausted — backoff with recovery/topology wakeup.
		db.kickTopology()
		if backoff == 0 {
			backoff = loadBalanceStartBackoff
		} else {
			backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-db.failMon.waitForRecovery():
			timer.Stop()
			backoff = 0
		case <-db.waitProxiesChanged():
			timer.Stop()
			backoff = 0
		case <-ctx.Done():
			timer.Stop()
			return 0, false, false, false, nil, 0, ctx.Err()
		}
	}
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

func buildGetReadVersionRequest(replyToken transport.UID, flags uint32, txnCount uint32) []byte {
	req := types.GetReadVersionRequest{
		TransactionCount: txnCount,
		Flags:            flags,
		MaxVersion:       InvalidVersion,
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
	}
	return req.MarshalFDB()
}

// parseGetReadVersionReply parses the ErrorOr-wrapped GRV response.
// Returns (version, locked, rkDefaultThrottled, rkBatchThrottled,
// tagThrottleInfo, proxyTagThrottledDuration, error). `locked` is the
// database-locked flag the proxy reports unconditionally
// (GrvProxyServer.actor.cpp:673); enforcement is client-side, per
// transaction (RFC-096).
func parseGetReadVersionReply(data []byte) (int64, bool, bool, bool, []byte, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return 0, false, false, false, nil, 0, fmt.Errorf("GRV: %w", err)
	}
	var reply types.GetReadVersionReply
	reply.UnmarshalFromReader(&r)
	return reply.Version, reply.Locked, reply.RkDefaultThrottled, reply.RkBatchThrottled, reply.TagThrottleInfo, reply.ProxyTagThrottledDuration, nil
}
