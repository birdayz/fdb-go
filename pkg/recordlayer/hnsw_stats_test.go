package recordlayer

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
)

func TestHNSWStatsContext(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	// No stats attached
	ctx := context.Background()
	g.Expect(GetHNSWStats(ctx)).To(gomega.BeNil())

	// Attach stats
	ctx2, stats := WithHNSWStats(ctx)
	g.Expect(stats).NotTo(gomega.BeNil())
	g.Expect(GetHNSWStats(ctx2)).To(gomega.Equal(stats))
}

func TestHNSWStatsCounters(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	_, stats := WithHNSWStats(context.Background())

	hnswStatGet(stats)
	hnswStatGet(stats)
	hnswStatBatchGet(stats)
	hnswStatRangeRead(stats)
	hnswStatRangeRead(stats)
	hnswStatRangeRead(stats)
	hnswStatCacheHit(stats)

	g.Expect(stats.FDBGets.Load()).To(gomega.Equal(int64(2)))
	g.Expect(stats.FDBBatchGets.Load()).To(gomega.Equal(int64(1)))
	g.Expect(stats.FDBRangeReads.Load()).To(gomega.Equal(int64(3)))
	g.Expect(stats.CacheHits.Load()).To(gomega.Equal(int64(1)))
}

func TestHNSWStatsNilSafe(t *testing.T) {
	t.Parallel()
	// All stat functions should be safe with nil stats (no panic)
	hnswStatGet(nil)
	hnswStatBatchGet(nil)
	hnswStatRangeRead(nil)
	hnswStatCacheHit(nil)
}
