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
}

// tryCache returns the cached version if it's fresh enough.
func (c *grvCache) tryCache() (int64, bool) {
	v := c.version.Load()
	if v == 0 {
		return 0, false
	}

	now := time.Now().UnixNano()
	lastTime := c.lastTime.Load()
	if time.Duration(now-lastTime) > maxVersionCacheLag {
		return 0, false // stale
	}

	// Check ratekeeper throttle cooldown (default priority).
	lastThrottle := c.lastRkDefault.Load()
	if lastThrottle > 0 && time.Duration(now-lastThrottle) < grvCacheRKCooldown {
		return 0, false // throttled — must contact proxy
	}

	return v, true
}

// update updates the cache with a new version.
// Monotonic: only accepts versions >= current cached version.
// Called after GRV response and after successful commit.
func (c *grvCache) update(t time.Time, v int64) {
	for {
		cur := c.version.Load()
		if v < cur {
			return // don't go backwards
		}
		if c.version.CompareAndSwap(cur, v) {
			break
		}
	}
	// Update time only if strictly newer (matching C++).
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
	err     error
}

// getReadVersion returns a read version, using the cache if fresh.
func (b *grvBatcher) getReadVersion(db *database, ctx context.Context, flags uint32) (int64, error) {
	// Fast path: serve from cache if fresh and not throttled.
	// SYSTEM_IMMEDIATE bypasses cache — it needs a guaranteed-fresh version.
	isImmediate := b.priority == grvPrioritySystemImmediate
	if !isImmediate {
		if v, ok := db.grvCache.tryCache(); ok {
			// Start background refresher on first cache hit.
			b.refreshOnce.Do(func() {
				db.wg.Add(1)
				go b.backgroundRefresher(db)
			})
			return v, nil
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
		return result.version, result.err
	case <-ctx.Done():
		return 0, ctx.Err()
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
	version, rkDefault, rkBatch, err := b.sendGRVRequest(db, batchCtx, flags, uint32(len(batch)))
	elapsed := time.Since(requestTime)

	if err == nil {
		// Update cache with fresh version.
		db.grvCache.update(requestTime, version)
		db.grvCache.lastProxyContact.Store(time.Now().UnixNano())
		// Track minimum version for client-side validateVersion().
		updateMinAcceptable(&db.minAcceptableReadVersion, version)

		// Track ratekeeper throttle state.
		if rkDefault {
			db.grvCache.lastRkDefault.Store(time.Now().UnixNano())
		}
		if rkBatch {
			db.grvCache.lastRkBatch.Store(time.Now().UnixNano())
		}
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

	result := grvResult{version: version, err: err}
	for _, req := range batch {
		req.reply <- result
	}
}

// backgroundRefresher proactively keeps the cache fresh.
// Matches C++ backgroundGrvUpdater: contacts proxy before cache goes stale.
func (b *grvBatcher) backgroundRefresher(db *database) {
	defer db.wg.Done()
	ticker := time.NewTicker(maxVersionCacheLag / 2) // refresh at half the staleness window
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now().UnixNano()
			lastProxy := db.grvCache.lastProxyContact.Load()
			lastGrv := db.grvCache.lastTime.Load()

			// Refresh if cache is getting stale or we haven't contacted proxy recently.
			needsRefresh := time.Duration(now-lastGrv) > (maxVersionCacheLag/2) ||
				time.Duration(now-lastProxy) > maxProxyContactLag

			if needsRefresh {
				requestTime := time.Now()
				refreshCtx, refreshCancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
				// Background refresher uses default priority (8 << 24).
				version, rkDefault, rkBatch, err := b.sendGRVRequest(db, refreshCtx, grvPriorityDefault, 1)
				refreshCancel()
				if err == nil {
					db.grvCache.update(requestTime, version)
					db.grvCache.lastProxyContact.Store(time.Now().UnixNano())
					if rkDefault {
						db.grvCache.lastRkDefault.Store(time.Now().UnixNano())
					}
					if rkBatch {
						db.grvCache.lastRkBatch.Store(time.Now().UnixNano())
					}
				}
			}
		case <-db.ctx.Done():
			return
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
func (b *grvBatcher) sendGRVRequest(db *database, ctx context.Context, flags uint32, txnCount uint32) (version int64, rkDefaultThrottled, rkBatchThrottled bool, err error) {
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
			case <-ctx.Done():
				timer.Stop()
				return 0, false, false, ctx.Err()
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

			replyToken, replyCh, cancelReply := conn.PrepareReply()
			body := buildGetReadVersionRequest(replyToken, flags, txnCount)

			if err := conn.SendFrame(proxy.Token, body); err != nil {
				cancelReply()
				db.handleConnError(proxy.Address)
				continue
			}

			resp, rpcErr := waitReply(replyCh, ctx, DefaultRPCTimeout)
			if rpcErr != nil {
				cancelReply()
				if ctx.Err() != nil {
					return 0, false, false, ctx.Err()
				}
				db.failMon.markFailed(proxy.Address)
				continue
			}
			if resp.Err != nil {
				db.handleConnError(proxy.Address)
				continue
			}
			db.failMon.markAlive(proxy.Address)
			return parseGetReadVersionReply(resp.Body)
		}

		// All proxies exhausted — backoff with recovery wakeup.
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
		case <-ctx.Done():
			timer.Stop()
			return 0, false, false, ctx.Err()
		}
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
// Returns (version, rkDefaultThrottled, rkBatchThrottled, error).
func parseGetReadVersionReply(data []byte) (int64, bool, bool, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return 0, false, false, fmt.Errorf("GRV: %w", err)
	}
	var reply types.GetReadVersionReply
	reply.UnmarshalFromReader(&r)
	return reply.Version, reply.RkDefaultThrottled, reply.RkBatchThrottled, nil
}
