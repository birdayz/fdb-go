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

// GRVBatcher batches concurrent GetReadVersion requests into a single
// RPC to a GRV proxy, then fans out the result.
//
// GRV caching (matching C++ DatabaseContext):
// - Cached version served if <100ms old (MAX_VERSION_CACHE_LAG)
// - Background refresher keeps cache warm (MAX_PROXY_CONTACT_LAG)
// - Monotonic: cache only accepts newer versions
// - Updated on GRV response AND after successful commit
// - Ratekeeper throttle awareness: cache disabled for 60s after throttle
type GRVBatcher struct {
	cluster *Cluster

	// Batching state.
	mu        sync.Mutex
	pending   []grvRequest
	batchTime time.Duration
	timer     *time.Timer

	// Cache state (atomic for lock-free reads on hot path).
	cachedVersion    atomic.Int64 // monotonic: only increases
	lastGrvTime      atomic.Int64 // UnixNano of last cache update
	lastProxyContact atomic.Int64 // UnixNano of last proxy RPC

	// Ratekeeper throttle tracking.
	lastRkDefaultThrottle atomic.Int64 // UnixNano
	lastRkBatchThrottle   atomic.Int64 // UnixNano

	// Background refresher.
	refreshOnce sync.Once
	stopRefresh chan struct{}
}

type grvRequest struct {
	reply chan grvResult
}

type grvResult struct {
	version int64
	err     error
}

// NewGRVBatcher creates a batcher with GRV caching.
func NewGRVBatcher(cluster *Cluster) *GRVBatcher {
	return &GRVBatcher{
		cluster:     cluster,
		batchTime:   1 * time.Millisecond,
		stopRefresh: make(chan struct{}),
	}
}

// GetReadVersion returns a read version, using the cache if fresh.
func (b *GRVBatcher) GetReadVersion(ctx context.Context) (int64, error) {
	// Fast path: serve from cache if fresh and not throttled.
	if v, ok := b.tryCache(); ok {
		return v, nil
	}

	// Slow path: batch request to proxy.
	req := grvRequest{reply: make(chan grvResult, 1)}

	b.mu.Lock()
	b.pending = append(b.pending, req)
	if len(b.pending) == 1 {
		b.timer = time.AfterFunc(b.batchTime, b.flush)
	}
	b.mu.Unlock()

	select {
	case result := <-req.reply:
		return result.version, result.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// tryCache returns the cached version if it's fresh enough.
func (b *GRVBatcher) tryCache() (int64, bool) {
	v := b.cachedVersion.Load()
	if v == 0 {
		return 0, false
	}

	now := time.Now().UnixNano()
	lastTime := b.lastGrvTime.Load()
	if time.Duration(now-lastTime) > maxVersionCacheLag {
		return 0, false // stale
	}

	// Check ratekeeper throttle cooldown (default priority).
	lastThrottle := b.lastRkDefaultThrottle.Load()
	if lastThrottle > 0 && time.Duration(now-lastThrottle) < grvCacheRKCooldown {
		return 0, false // throttled — must contact proxy
	}

	// Start background refresher on first cache hit.
	b.refreshOnce.Do(func() {
		go b.backgroundRefresher()
	})

	return v, true
}

// UpdateCachedReadVersion updates the cache with a new version.
// Monotonic: only accepts versions >= current cached version.
// Called after GRV response and after successful commit.
func (b *GRVBatcher) UpdateCachedReadVersion(t time.Time, v int64) {
	for {
		cur := b.cachedVersion.Load()
		if v < cur {
			return // don't go backwards
		}
		if b.cachedVersion.CompareAndSwap(cur, v) {
			break
		}
	}
	// Update time only if strictly newer (matching C++).
	tNano := t.UnixNano()
	for {
		cur := b.lastGrvTime.Load()
		if tNano <= cur {
			return
		}
		if b.lastGrvTime.CompareAndSwap(cur, tNano) {
			return
		}
	}
}

// flush sends the batched GRV request and updates the cache.
func (b *GRVBatcher) flush() {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	requestTime := time.Now()
	version, rkDefault, rkBatch, err := b.sendGRVRequest()
	elapsed := time.Since(requestTime)

	if err == nil {
		// Update cache with fresh version.
		b.UpdateCachedReadVersion(requestTime, version)
		b.lastProxyContact.Store(time.Now().UnixNano())

		// Track ratekeeper throttle state.
		if rkDefault {
			b.lastRkDefaultThrottle.Store(time.Now().UnixNano())
		}
		if rkBatch {
			b.lastRkBatchThrottle.Store(time.Now().UnixNano())
		}
	}

	// Adaptive batch window.
	b.mu.Lock()
	b.batchTime = time.Duration(0.1*float64(elapsed)/2 + 0.9*float64(b.batchTime))
	if b.batchTime < 100*time.Microsecond {
		b.batchTime = 100 * time.Microsecond
	}
	if b.batchTime > 10*time.Millisecond {
		b.batchTime = 10 * time.Millisecond
	}
	b.mu.Unlock()

	result := grvResult{version: version, err: err}
	for _, req := range batch {
		req.reply <- result
	}
}

// backgroundRefresher proactively keeps the cache fresh.
// Matches C++ backgroundGrvUpdater: contacts proxy before cache goes stale.
func (b *GRVBatcher) backgroundRefresher() {
	ticker := time.NewTicker(maxVersionCacheLag / 2) // refresh at half the staleness window
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now().UnixNano()
			lastProxy := b.lastProxyContact.Load()
			lastGrv := b.lastGrvTime.Load()

			// Refresh if cache is getting stale or we haven't contacted proxy recently.
			needsRefresh := time.Duration(now-lastGrv) > (maxVersionCacheLag/2) ||
				time.Duration(now-lastProxy) > maxProxyContactLag

			if needsRefresh {
				requestTime := time.Now()
				version, rkDefault, rkBatch, err := b.sendGRVRequest()
				if err == nil {
					b.UpdateCachedReadVersion(requestTime, version)
					b.lastProxyContact.Store(time.Now().UnixNano())
					if rkDefault {
						b.lastRkDefaultThrottle.Store(time.Now().UnixNano())
					}
					if rkBatch {
						b.lastRkBatchThrottle.Store(time.Now().UnixNano())
					}
				}
			}
		case <-b.stopRefresh:
			return
		}
	}
}

// InvalidateCache clears the cached version. Called on reconnect or error recovery.
func (b *GRVBatcher) InvalidateCache() {
	b.cachedVersion.Store(0)
	b.lastGrvTime.Store(0)
}

// Stop shuts down the background refresher.
func (b *GRVBatcher) Stop() {
	select {
	case b.stopRefresh <- struct{}{}:
	default:
	}
}

func (b *GRVBatcher) sendGRVRequest() (version int64, rkDefaultThrottled, rkBatchThrottled bool, err error) {
	proxy, err := b.cluster.GetGRVProxy()
	if err != nil {
		return 0, false, false, err
	}

	conn, err := b.cluster.getOrDial(context.Background(), proxy.Address)
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
