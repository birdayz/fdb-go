package recordlayer

import (
	"context"
	"sync/atomic"
)

// HNSWStats tracks I/O counters for a single HNSW operation.
// Attach to a context via WithHNSWStats and retrieve via GetHNSWStats.
// Zero cost when not attached — all tracking is behind a nil check.
type HNSWStats struct {
	FDBGets       atomic.Int64 // individual Get() calls (point reads)
	FDBBatchGets  atomic.Int64 // batched Get() calls (pipelined, count as 1 RT)
	FDBRangeReads atomic.Int64 // GetRange() calls
	CacheHits     atomic.Int64 // reads served from in-tx cache
}

type hnswStatsKey struct{}

// WithHNSWStats attaches an HNSWStats to the context for I/O tracking.
func WithHNSWStats(ctx context.Context) (context.Context, *HNSWStats) {
	stats := &HNSWStats{}
	return context.WithValue(ctx, hnswStatsKey{}, stats), stats
}

// GetHNSWStats retrieves HNSWStats from a context, or nil if not tracking.
func GetHNSWStats(ctx context.Context) *HNSWStats {
	if v := ctx.Value(hnswStatsKey{}); v != nil {
		return v.(*HNSWStats)
	}
	return nil
}

func hnswStatGet(stats *HNSWStats) {
	if stats != nil {
		stats.FDBGets.Add(1)
	}
}

func hnswStatBatchGet(stats *HNSWStats) {
	if stats != nil {
		stats.FDBBatchGets.Add(1)
	}
}

func hnswStatRangeRead(stats *HNSWStats) {
	if stats != nil {
		stats.FDBRangeReads.Add(1)
	}
}

func hnswStatCacheHit(stats *HNSWStats) {
	if stats != nil {
		stats.CacheHits.Add(1)
	}
}
