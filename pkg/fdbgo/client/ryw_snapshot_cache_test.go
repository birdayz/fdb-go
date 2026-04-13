package client

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
)

// --- snapshotCache unit tests ---

func TestSnapshotCache_InsertAndGetKey(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("d"), []KeyValue{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("c"), Value: []byte("3")},
	})

	// Key in range, exists.
	val, known := sc.getKey([]byte("b"))
	g.Expect(known).To(BeTrue())
	g.Expect(val).To(Equal([]byte("2")))

	// Key in range, doesn't exist at server.
	val, known = sc.getKey([]byte("bb"))
	g.Expect(known).To(BeTrue())
	g.Expect(val).To(BeNil())

	// Key outside range.
	_, known = sc.getKey([]byte("d"))
	g.Expect(known).To(BeFalse())
	_, known = sc.getKey([]byte("z"))
	g.Expect(known).To(BeFalse())
}

func TestSnapshotCache_InsertAndGetRangeKVs(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("f"), []KeyValue{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("c"), Value: []byte("3")},
		{Key: []byte("e"), Value: []byte("5")},
	})

	// Exact range.
	kvs, ok := sc.getRangeKVs([]byte("a"), []byte("f"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(HaveLen(3))

	// Sub-range.
	kvs, ok = sc.getRangeKVs([]byte("b"), []byte("d"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(HaveLen(1))
	g.Expect(kvs[0].Key).To(Equal([]byte("c")))

	// Range extends beyond cached.
	_, ok = sc.getRangeKVs([]byte("a"), []byte("g"))
	g.Expect(ok).To(BeFalse())

	// Range before cached.
	_, ok = sc.getRangeKVs([]byte("0"), []byte("a"))
	g.Expect(ok).To(BeFalse())
}

func TestSnapshotCache_MergeAdjacentInserts(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("c"), []KeyValue{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
	})
	sc.insert([]byte("c"), []byte("f"), []KeyValue{
		{Key: []byte("c"), Value: []byte("3")},
		{Key: []byte("d"), Value: []byte("4")},
	})

	// After two adjacent inserts, the full range should be known.
	kvs, ok := sc.getRangeKVs([]byte("a"), []byte("f"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(HaveLen(4))
	g.Expect(sc.entries).To(HaveLen(1)) // merged into one entry
}

func TestSnapshotCache_MergeOverlappingInserts(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("d"), []KeyValue{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("c"), Value: []byte("3")},
	})
	sc.insert([]byte("b"), []byte("f"), []KeyValue{
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("e"), Value: []byte("5")},
	})

	// Overlapping inserts should merge.
	kvs, ok := sc.getRangeKVs([]byte("a"), []byte("f"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(HaveLen(4)) // a, b, c, e
	g.Expect(sc.entries).To(HaveLen(1))
}

func TestSnapshotCache_NonOverlappingInserts(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("c"), []KeyValue{
		{Key: []byte("a"), Value: []byte("1")},
	})
	sc.insert([]byte("e"), []byte("g"), []KeyValue{
		{Key: []byte("f"), Value: []byte("6")},
	})

	// Gap between [a,c) and [e,g) — full range is NOT known.
	_, ok := sc.getRangeKVs([]byte("a"), []byte("g"))
	g.Expect(ok).To(BeFalse())

	// Each sub-range individually IS known.
	kvs, ok := sc.getRangeKVs([]byte("a"), []byte("c"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(HaveLen(1))
	kvs, ok = sc.getRangeKVs([]byte("e"), []byte("g"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(HaveLen(1))
}

func TestSnapshotCache_EmptyRange(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	// Cache a range with no KVs — the range is known to be empty.
	sc.insert([]byte("a"), []byte("z"), nil)

	kvs, ok := sc.getRangeKVs([]byte("a"), []byte("z"))
	g.Expect(ok).To(BeTrue())
	g.Expect(kvs).To(BeEmpty())

	val, known := sc.getKey([]byte("m"))
	g.Expect(known).To(BeTrue())
	g.Expect(val).To(BeNil())
}

func TestSnapshotCache_Reset(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("z"), []KeyValue{
		{Key: []byte("m"), Value: []byte("mid")},
	})
	sc.reset()

	_, ok := sc.getRangeKVs([]byte("a"), []byte("z"))
	g.Expect(ok).To(BeFalse())
	_, known := sc.getKey([]byte("m"))
	g.Expect(known).To(BeFalse())
}

func TestSnapshotCache_GetKeyAtBoundary(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var sc snapshotCache
	sc.insert([]byte("a"), []byte("d"), []KeyValue{
		{Key: []byte("a"), Value: []byte("1")},
	})

	// Begin key is inclusive.
	val, known := sc.getKey([]byte("a"))
	g.Expect(known).To(BeTrue())
	g.Expect(val).To(Equal([]byte("1")))

	// End key is exclusive — "d" is NOT in the range.
	_, known = sc.getKey([]byte("d"))
	g.Expect(known).To(BeFalse())
}

// --- rywCache + snapshotCache integration tests ---

func TestRYWSnapshotCache_GetCachesServerResult(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGet := func(ctx context.Context, key []byte) ([]byte, error) {
		calls++
		if string(key) == "k1" {
			return []byte("v1"), nil
		}
		return nil, nil
	}

	// First call: goes to server.
	val, err := c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal([]byte("v1")))
	g.Expect(calls).To(Equal(1))

	// Second call: served from cache — no server hit.
	val, err = c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal([]byte("v1")))
	g.Expect(calls).To(Equal(1)) // still 1
}

func TestRYWSnapshotCache_GetCachesNonExistentKey(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGet := func(ctx context.Context, key []byte) ([]byte, error) {
		calls++
		return nil, nil // key doesn't exist
	}

	val, err := c.get(context.Background(), []byte("missing"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(BeNil())
	g.Expect(calls).To(Equal(1))

	// Second call: cache knows the key doesn't exist.
	val, err = c.get(context.Background(), []byte("missing"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(BeNil())
	g.Expect(calls).To(Equal(1)) // still 1
}

func TestRYWSnapshotCache_WriteShadowsServerCache(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	serverGet := func(ctx context.Context, key []byte) ([]byte, error) {
		return []byte("server-value"), nil
	}

	// Populate cache.
	val, err := c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal([]byte("server-value")))

	// Local write shadows the cached server value.
	c.set([]byte("k1"), []byte("local-value"))

	val, err = c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal([]byte("local-value")))
}

func TestRYWSnapshotCache_ClearShadowsServerCache(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGet := func(ctx context.Context, key []byte) ([]byte, error) {
		calls++
		return []byte("server-value"), nil
	}

	// Populate cache.
	c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(calls).To(Equal(1))

	// Clear the key locally — should return nil.
	c.clear([]byte("k1"))

	val, err := c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(BeNil())
	g.Expect(calls).To(Equal(1)) // no additional server call
}

func TestRYWSnapshotCache_GetRangeCachesServerResult(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// First call.
	kvs, more, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(more).To(BeFalse())
	g.Expect(kvs).To(HaveLen(3))
	g.Expect(calls).To(Equal(1))

	// Second call: fully cached.
	kvs, more, err = c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(more).To(BeFalse())
	g.Expect(kvs).To(HaveLen(3))
	g.Expect(calls).To(Equal(1)) // no additional server call
}

func TestRYWSnapshotCache_GetRangeSubrangeHit(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// Fetch full range.
	c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(calls).To(Equal(1))

	// Sub-range of cached data.
	kvs, more, err := c.getRange(context.Background(), []byte("b"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(more).To(BeFalse())
	g.Expect(kvs).To(HaveLen(2)) // b, c
	g.Expect(kvs[0].Key).To(Equal([]byte("b")))
	g.Expect(calls).To(Equal(1)) // still 1
}

func TestRYWSnapshotCache_GetRangeReverseFromCache(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		// Simulate forward fetch.
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// Fetch in forward direction.
	c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(calls).To(Equal(1))

	// Now request the same range in reverse — should come from cache.
	kvs, _, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 0, true, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(3))
	g.Expect(kvs[0].Key).To(Equal([]byte("c"))) // reverse order
	g.Expect(kvs[1].Key).To(Equal([]byte("b")))
	g.Expect(kvs[2].Key).To(Equal([]byte("a")))
	g.Expect(calls).To(Equal(1)) // still cached
}

func TestRYWSnapshotCache_GetRangeWithWritesMerge(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// First call populates cache.
	c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(calls).To(Equal(1))

	// Add a local write.
	c.set([]byte("b"), []byte("2"))

	// Second call: has writes, but server cache should prevent re-fetch.
	kvs, _, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(3)) // a, b (write), c
	g.Expect(kvs[1].Key).To(Equal([]byte("b")))
	g.Expect(kvs[1].Value).To(Equal([]byte("2")))
	g.Expect(calls).To(Equal(1)) // still 1 — no re-fetch
}

func TestRYWSnapshotCache_GetRangeWithClearsMerge(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// First call populates cache.
	c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(calls).To(Equal(1))

	// Clear a key locally.
	c.clear([]byte("b"))

	// Second call: has clears, but server cache prevents re-fetch.
	kvs, _, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(2)) // a, c (b was cleared)
	g.Expect(calls).To(Equal(1)) // still 1
}

func TestRYWSnapshotCache_ResetClearsCache(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGet := func(ctx context.Context, key []byte) ([]byte, error) {
		calls++
		return []byte("val"), nil
	}

	c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(calls).To(Equal(1))

	c.reset()

	// After reset, cache is cleared — must go to server again.
	c.get(context.Background(), []byte("k1"), serverGet)
	g.Expect(calls).To(Equal(2))
}

func TestRYWSnapshotCache_GetRangeWithLimit(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// Fetch full range (no limit).
	c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)

	// Now request with limit from cache.
	kvs, more, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 2, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(2))
	g.Expect(more).To(BeTrue())
	g.Expect(calls).To(Equal(1))
}

func TestRYWSnapshotCache_PartialServerMoreSlowPath(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		// Simulate paged results for the slow path's iterative loop.
		if string(begin) <= "a" {
			return []KeyValue{
				{Key: []byte("a"), Value: []byte("1")},
				{Key: []byte("b"), Value: []byte("2")},
			}, true, nil
		}
		return []KeyValue{
			{Key: []byte("c"), Value: []byte("3")},
		}, false, nil
	}

	// Force slow path by adding a write inside the range.
	c.set([]byte("a1"), []byte("inserted"))

	// First call: slow path iterates, caching both server chunks.
	kvs, more, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(4)) // a, a1 (write), b, c
	g.Expect(more).To(BeFalse())
	g.Expect(calls).To(Equal(2))

	// Second call: both server chunks cached — no additional server calls.
	kvs, more, err = c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(4))
	g.Expect(more).To(BeFalse())
	g.Expect(calls).To(Equal(2)) // no additional calls
}

func TestRYWSnapshotCache_FastPathCachesServerMore(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	calls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
		}, true, nil // serverMore=true (real callback handles paging internally)
	}

	// Fast path: returns server result as-is, but caches partial range.
	kvs, more, err := c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(2))
	g.Expect(more).To(BeTrue())
	g.Expect(calls).To(Equal(1))

	// Sub-range [a, b\x00) is cached — request for [a, b] hits cache.
	kvs, more, err = c.getRange(context.Background(), []byte("a"), []byte("b"), 0, false, serverGetRange)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kvs).To(HaveLen(1)) // just "a" (end "b" is exclusive)
	g.Expect(more).To(BeFalse())
	g.Expect(calls).To(Equal(1)) // cached
}

func TestRYWSnapshotCache_GetAfterRangeCacheHit(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var c rywCache
	rangeCalls := 0
	getCalls := 0
	serverGetRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		rangeCalls++
		return []KeyValue{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
		}, false, nil
	}
	serverGet := func(ctx context.Context, key []byte) ([]byte, error) {
		getCalls++
		return []byte("server"), nil
	}

	// Range scan populates cache for [a, d).
	c.getRange(context.Background(), []byte("a"), []byte("d"), 0, false, serverGetRange)
	g.Expect(rangeCalls).To(Equal(1))

	// Single key Get for "b" — should be served from range cache.
	val, err := c.get(context.Background(), []byte("b"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal([]byte("2")))
	g.Expect(getCalls).To(Equal(0)) // no server call

	// Key "c" is in the cached range but has no KV — doesn't exist.
	val, err = c.get(context.Background(), []byte("c"), serverGet)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(BeNil())
	g.Expect(getCalls).To(Equal(0)) // still no server call
}
