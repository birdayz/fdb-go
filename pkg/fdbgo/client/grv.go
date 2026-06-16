package client

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/internal/diag"
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
	// No `locked` rides the cache: the cache is now opt-in (USE_GRV_CACHE,
	// default off, RFC-104), so every DEFAULT transaction takes the fresh-GRV
	// path and hits the real locked check at the consumption site
	// (transaction.go ensureReadVersion). C++ fail-opens its cached path by
	// contract (NativeAPI.actor.cpp:7514-7516 returns the version with zero
	// lock inspection; lockAware appears only on the fresh fall-through, :7425),
	// so an opted-in Go transaction does too. The RFC-096 lastLocked ride-along
	// existed ONLY to compensate for the previous always-on cache and is gone.
}

// tryCache returns the cached version if it's fresh enough and not throttled.
// priority determines which ratekeeper throttle to check:
// BATCH checks lastRkBatch, DEFAULT checks lastRkDefault.
// Matches C++ DatabaseContext::getConsistentReadVersion throttle checks. The
// opt-in gate (USE_GRV_CACHE) is enforced by the CALLER (getReadVersion);
// reaching here already implies the transaction opted in (RFC-104).
func (c *grvCache) tryCache(priority uint32) (int64, bool) {
	v := c.version.Load()
	if v == 0 {
		return 0, false
	}

	now := time.Now().UnixNano()
	lastTime := c.lastTime.Load()
	if time.Duration(now-lastTime) > maxVersionCacheLag {
		return 0, false // stale
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
		return 0, false // SYSTEM_IMMEDIATE must always contact proxy
	default:
		lastThrottle = c.lastRkDefault.Load()
	}
	if lastThrottle > 0 && time.Duration(now-lastThrottle) < grvCacheRKCooldown {
		return 0, false // throttled — must contact proxy
	}

	return v, true
}

// update advances the cached version + freshness clock from a successful
// commit, matching C++ updateCachedReadVersion at the commit site
// (NativeAPI.actor.cpp:6657, t=now()). Monotonic on both: version only accepts
// >= current (the CAS loop), lastTime only advances (updateTime's guard).
// Population is UNCONDITIONAL — it runs for every committing transaction
// regardless of USE_GRV_CACHE (RFC-104, codex P3); only cache READS are opt-in,
// so a default transaction's commit can warm the cache for a later opted-in
// reader. No lock state: the cached path fail-opens (RFC-104; the RFC-096
// commit-must-not-extend-freshness divergence is reverted now that the cache is
// no longer always-on + enforcement-carrying).
func (c *grvCache) update(v int64) {
	for {
		cur := c.version.Load()
		if v < cur {
			return
		}
		if c.version.CompareAndSwap(cur, v) {
			break
		}
	}
	c.updateTime(time.Now())
}

// updateFromGRV updates the cache from a real GRV reply: version + freshness,
// monotonic on both (CAS for version, updateTime for lastTime). Unconditional —
// runs for every GRV reply regardless of USE_GRV_CACHE (C++ extractReadVersion,
// :7409, is not gated on the option). No lock state: the cached path fail-opens
// (RFC-104; the RFC-096 lastLocked ride-along is removed — the per-transaction
// locked check now lives only on the fresh-GRV consumption path).
func (c *grvCache) updateFromGRV(t time.Time, v int64) {
	for {
		cur := c.version.Load()
		if v < cur {
			return // stale reply — don't go backwards
		}
		if c.version.CompareAndSwap(cur, v) {
			break
		}
	}
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

// updateMinAcceptable lowers minAcceptableReadVersion toward the SMALLEST read
// version this client has seen. C++ DatabaseContext::minAcceptableReadVersion is
// a std::min over GRV reply versions (NativeAPI.actor.cpp:3871,:7287), inited to
// max(). Go inits the atomic to 0, so 0 means "unset": the first version sets it
// and later (necessarily ≥) versions leave it at the smallest. This is the floor
// validateVersion compares against — keeping it the smallest-seen (NOT a rising
// max) means only a genuinely-ancient pinned version is rejected, never a recent
// one that merely predates a later GRV. (Previously this ratcheted UPWARD, so
// RFC-104's fresh-GRV default raced it past pinned versions libfdb_c accepted —
// the root of the spurious 1007s in the snapshot/conflict/getKey differentials.)
func updateMinAcceptable(min *atomic.Int64, v int64) {
	if v <= 0 {
		return
	}
	for {
		cur := min.Load()
		if cur != 0 && v >= cur {
			return // already hold a smaller-or-equal floor
		}
		if min.CompareAndSwap(cur, v) {
			return
		}
	}
}

// validateVersion checks a user-set read version against the client's
// seen-version floor. A version below minAcceptableReadVersion — the SMALLEST
// read version this client has seen (std::min; see updateMinAcceptable) — is
// transaction_too_old. Because the floor is the smallest-seen and NOT a rising
// max (the RFC-104 fix), this rejects only a genuinely-ancient pinned version
// (below anything the client has observed), never a recent one that merely
// predates a later GRV — so a libfdb_c-current version pinned across both
// clients is honored, and the storage server remains the authority on the 5s
// MVCC window. (C++ gates the same throw on `switchable` at NativeAPI.actor.cpp:518;
// the Go client is non-switchable and keeps it as a client-side fail-fast for
// ancient versions — outcome-equivalent, since the server rejects exactly those.)
//
// The absurd-future check is a Go-only client-side guard (real FDB versions are
// ~10^7/s; even 100 years is ~3×10^16): a wildly-out-of-range version would
// block the storage server indefinitely, and our RPC timeout races the server's
// own future_version, so detecting it client-side is more reliable.
func (db *database) validateVersion(version int64) error {
	min := db.minAcceptableReadVersion.Load()
	if min > 0 && version < min {
		return &wire.FDBError{Code: ErrTransactionTooOld}
	}
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
	// refresherStarted is a deterministic test seam (RFC-104): true once the
	// background refresher has been launched (on the first opted-in request —
	// hit OR miss, matching C++ which starts the updater inside the opt-in gate
	// before the freshness check). Lets a test assert "the refresher never
	// started for a cache-off process" without a flaky goroutine count.
	refresherStarted atomic.Bool

	// lastPanicLog rate-limits recoverFlush's diagnostic log across overlapping
	// flushes (UnixNano; atomic — a new batch can flush while an earlier flush's
	// RPC is still in flight). RFC-110.
	lastPanicLog atomic.Int64
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
func (b *grvBatcher) getReadVersion(db *database, ctx context.Context, flags uint32, useGrvCache, skipGrvCache bool) (int64, bool, error) {
	// Fast path: serve from cache ONLY when the transaction opted in
	// (USE_GRV_CACHE, default off — RFC-104). C++ gate NativeAPI.actor.cpp:7504-7517.
	// SYSTEM_IMMEDIATE never caches (needs a guaranteed-fresh version).
	// The cached path FAIL-OPENS on locked (returns locked=false): C++ does not
	// lock-check the cached path (:7514-7516 returns the version with zero lock
	// inspection; lockAware appears only on the fresh fall-through, :7425). The
	// real locked check is at the consumption site (ensureReadVersion), which
	// every DEFAULT (cache-off) transaction reaches.
	isImmediate := b.priority == grvPrioritySystemImmediate
	// NOTE (divergence, TODO_client.md #16): the C++ gate also requires
	// rkThrottlingCooledDown(cx, priority) (NativeAPI.actor.cpp:7506); under active
	// ratekeeper throttling C++ skips the whole cache block (no updater start, no cached
	// serve). Go's gate omits that one condition, so under throttle Go starts the
	// refresher where C++ would not. tryCache still rechecks throttle on the serve path,
	// so this only affects WHEN the background updater launches, never correctness.
	if !isImmediate && useGrvCache && !skipGrvCache {
		// Start the background refresher on the FIRST opted-in request, BEFORE the
		// freshness check — matching C++ getReadVersion (NativeAPI.actor.cpp:7507-7509),
		// which launches backgroundGrvUpdater inside the opt-in gate regardless of
		// whether the cached version is usable ("Upon our first request to use cached
		// RVs, start the background updater"). Starting it only on a cache HIT (the
		// prior Go behavior) left a cold/stale cache un-warmed: every opted-in read with
		// lag > MAX_VERSION_CACHE_LAG fell through to a real GRV and the cache never
		// caught up, defeating the opt-in entirely for sparse workloads.
		b.refreshOnce.Do(func() {
			b.refresherStarted.Store(true)
			db.wg.Add(1)
			go b.backgroundRefresher(db)
		})
		if v, ok := db.grvCache.tryCache(b.priority); ok {
			db.metrics.countGRVCacheHit()
			return v, false, nil
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
	// Pop the pending batch under a closure-scoped lock (RFC-110: a panic in any
	// b.mu-holding region must unwind the lock, else later GRV requests blocking
	// on b.mu.Lock() deadlock — codex P2a).
	batch := func() []grvRequest {
		b.mu.Lock()
		defer b.mu.Unlock()
		p := b.pending
		b.pending = nil
		return p
	}()

	if len(batch) == 0 {
		return
	}

	// RFC-110 (codex P1): once the batch is popped, b.pending no longer references
	// it — a panic anywhere below (sendGRVRequest decode, applyGRVReply, the
	// adaptive-window math) would orphan it, and every waiter blocked on its
	// req.reply with a non-canceling ctx would hang forever. recoverFlush is the
	// fail-the-batch backstop: it delivers an error to every popped waiter and
	// keeps the process alive (the libfdb_c analog: a failed proxy GRV future
	// resolves with error, not a hung future). This is a one-shot per-batch
	// callback (not a standing loop) — there is nothing to re-arm, and the next
	// queued request arms a fresh timer; the log is rate-limited, the metric
	// counts every occurrence.
	defer b.recoverFlush(db, batch)

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
		// precedes :7425). `locked` is returned to waiters below but no longer
		// rides the cache (RFC-104).
		b.applyGRVReply(db, requestTime, version, rkDefault, rkBatch, tagThrottleInfoBytes)
		// C++ counts per-transaction in extractReadVersion (:7428-7440) — one
		// batched reply serves len(batch) transactions. Cache hits never reach
		// here (C++ parity: its cached path returns before the counters); the
		// background refresher has no waiters and adds nothing. Two accepted
		// edge divergences: waiters whose ctx expired mid-batch are still
		// counted (C++ cancels abandoned futures before its counter), and
		// under a LOCKED database non-lock-aware waiters are counted here
		// while C++'s database_locked throw (:7425-7426) precedes its
		// counters — fixing either would need per-waiter counting at the
		// consumption site, machinery a counter doesn't justify. RFC-097.
		db.metrics.countGRVBatchCompleted(b.priority, len(batch))
		// RFC-114: GRV round-trip latency (C++ GRVLatencies, NativeAPI.actor.cpp:7417).
		// Divergence (documented in RFC-114), on TWO axes: (1) count — Go samples
		// once per GRV BATCH, so Count is proxy round-trips, whereas C++ samples
		// per-transaction in extractReadVersion (Count == read-versions-completed);
		// (2) semantics — Go measures only the proxy RPC round-trip, whereas C++'s
		// latency = replyTime − startTime also folds in the per-transaction
		// batch-window queueing. Go's is the cleaner RPC-latency SLI. Cache hits
		// never reach here (they return before the flush — C++ parity, as above).
		db.metrics.observeGRVLatency(elapsed)
	}

	// Adaptive batch window (closure-scoped lock: a panic here unwinds b.mu).
	func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.batchTime = time.Duration(0.1*float64(elapsed)/2 + 0.9*float64(b.batchTime))
		if b.batchTime < 100*time.Microsecond {
			b.batchTime = 100 * time.Microsecond
		}
		if b.batchTime > 5*time.Millisecond { // C++ GRV_BATCH_TIMEOUT = 5ms
			b.batchTime = 5 * time.Millisecond
		}
	}()

	result := grvResult{version: version, locked: locked, err: err}
	for _, req := range batch {
		req.reply <- result
	}
}

// recoverFlush is flush's deferred RFC-110 backstop. On a recovered panic it
// FAILS THE BATCH — delivers an error to every popped waiter so none hangs
// (codex P1) — counts it, and rate-limited-logs. The batch reply channels are
// cap-1 and unsent on the panic path (the panic precedes the normal delivery
// loop), so the sends never block even for a waiter that already took its
// ctx.Done() branch. On the normal path recover() is nil → no-op. flushes are
// independent one-shots (not a standing loop), so this is fail-the-batch at
// request rate, not backoff-bounded.
func (b *grvBatcher) recoverFlush(db *database, batch []grvRequest) {
	r := recover()
	if r == nil {
		return
	}
	if db != nil {
		db.metrics.countRecoveredPanic(1)
	}
	b.logFlushPanic(r)
	result := grvResult{err: fmt.Errorf("fdbgo: panic in GRV flush: %v", r)}
	for _, req := range batch {
		req.reply <- result
	}
}

// logFlushPanic rate-limits recoverFlush's ERROR log to one per panicLogInterval
// (atomic CAS so overlapping flushes don't all log). The metric counts every
// panic; the log is the human breadcrumb, not the storm signal.
func (b *grvBatcher) logFlushPanic(r any) {
	now := time.Now().UnixNano()
	last := b.lastPanicLog.Load()
	if now-last < int64(panicLogInterval) || !b.lastPanicLog.CompareAndSwap(last, now) {
		return
	}
	diag.Recovered("fdbgo: recovered panic in client goroutine",
		"goroutine", "grvFlush",
		"err", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()),
	)
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
			time.Unix(0, db.grvCache.lastTime.Load()),
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
				requestTime := time.Now()
				refreshCtx, refreshCancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
				defer refreshCancel() // RFC-110: release the GRV timer even if sendGRVRequest panics
				// Refresh at this batcher's own priority, not always DEFAULT.
				// The BATCH batcher must refresh BATCH ratekeeper state
				// (lastRkBatch); the DEFAULT batcher refreshes DEFAULT
				// (lastRkDefault). SYSTEM_IMMEDIATE never reaches here because
				// its tryCache always returns false (refreshOnce never fires).
				version, _, rkDefault, rkBatch, tagThrottleInfoBytes, _, err := b.sendGRVRequest(db, refreshCtx, b.priority, 1)
				if err == nil {
					// The refresher ignores the reply's `locked` flag — equivalent
					// to C++'s background updater, whose non-lock-aware txn THROWS
					// 1038 on a locked DB after the cache update (:7409 precedes
					// :7425) and is caught by its own onError loop. Nothing surfaces
					// to users from a background refresh, and the cached path
					// fail-opens anyway (RFC-104).
					b.applyGRVReply(db, requestTime, version, rkDefault, rkBatch, tagThrottleInfoBytes)
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
func (b *grvBatcher) applyGRVReply(db *database, requestTime time.Time, version int64, rkDefault, rkBatch bool, tagThrottleInfoBytes []byte) {
	db.grvCache.updateFromGRV(requestTime, version)
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
				db.handleDialError(ctx, proxy.Address)
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
