package client

import (
	"context"
	"fmt"
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

// grvBatcher batches concurrent GetReadVersion calls.
// C++: DatabaseContext::VersionBatcher + readVersionBatcher actor.
//
// Methods receive *database as argument — no stored back-pointer.
type grvBatcher struct {
	mu          sync.Mutex
	pending     []grvRequest
	batchTime   time.Duration
	timer       *time.Timer
	refreshOnce sync.Once
}

type grvRequest struct {
	reply chan grvResult
}

type grvResult struct {
	version int64
	err     error
}

// getReadVersion returns a read version, using the cache if fresh.
func (b *grvBatcher) getReadVersion(db *database, ctx context.Context) (int64, error) {
	// Fast path: serve from cache if fresh and not throttled.
	if v, ok := db.grvCache.tryCache(); ok {
		// Start background refresher on first cache hit.
		b.refreshOnce.Do(func() {
			db.wg.Add(1)
			go b.backgroundRefresher(db)
		})
		return v, nil
	}

	// Slow path: batch request to proxy.
	req := grvRequest{reply: make(chan grvResult, 1)}

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
func (b *grvBatcher) flush(db *database) {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	requestTime := time.Now()
	version, rkDefault, rkBatch, err := b.sendGRVRequest(db)
	elapsed := time.Since(requestTime)

	if err == nil {
		// Update cache with fresh version.
		db.grvCache.update(requestTime, version)
		db.grvCache.lastProxyContact.Store(time.Now().UnixNano())

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
				version, rkDefault, rkBatch, err := b.sendGRVRequest(db)
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

func (b *grvBatcher) sendGRVRequest(db *database) (version int64, rkDefaultThrottled, rkBatchThrottled bool, err error) {
	proxy, err := db.getGRVProxy()
	if err != nil {
		return 0, false, false, err
	}

	conn, err := db.getOrDial(context.Background(), proxy.Address)
	if err != nil {
		return 0, false, false, err
	}

	replyToken, replyCh := conn.PrepareReply()
	body := buildGetReadVersionRequest(replyToken)

	if err := conn.SendFrame(proxy.Token, body); err != nil {
		return 0, false, false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultRPCTimeout)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return 0, false, false, resp.Err
		}
		return parseGetReadVersionReply(resp.Body)
	case <-ctx.Done():
		return 0, false, false, ctx.Err()
	}
}

func buildGetReadVersionRequest(replyToken transport.UID) []byte {
	req := types.GetReadVersionRequest{
		TransactionCount: 1,
		MaxVersion:       -1,
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
	}
	return req.MarshalFDB()
}

// parseGetReadVersionReply parses the ErrorOr-wrapped GRV response.
// Returns (version, rkDefaultThrottled, rkBatchThrottled, error).
func parseGetReadVersionReply(data []byte) (int64, bool, bool, error) {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return 0, false, false, fmt.Errorf("GRV: %w", err)
	}
	var reply types.GetReadVersionReply
	if err := reply.UnmarshalFDB(data); err != nil {
		return 0, false, false, fmt.Errorf("unmarshal GRV reply: %w", err)
	}
	return reply.Version, reply.RkDefaultThrottled, reply.RkBatchThrottled, nil
}
