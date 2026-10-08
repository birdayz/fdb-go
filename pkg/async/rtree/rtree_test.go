package rtree

import (
	"context"
	"math/big"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HilbertValue", func() {
	It("computes known values for simple 2D coordinates", func() {
		// (0, 0) should give a deterministic Hilbert value
		hv00 := hilbertValue([]int64{0, 0})
		Expect(hv00).NotTo(BeNil())
		Expect(hv00.Sign()).To(BeNumerically(">=", 0))

		// Different coordinates must produce different Hilbert values
		hv10 := hilbertValue([]int64{1, 0})
		hv01 := hilbertValue([]int64{0, 1})
		hv11 := hilbertValue([]int64{1, 1})

		Expect(hv00.Cmp(hv10)).NotTo(Equal(0), "(0,0) and (1,0) should differ")
		Expect(hv00.Cmp(hv01)).NotTo(Equal(0), "(0,0) and (0,1) should differ")
		Expect(hv00.Cmp(hv11)).NotTo(Equal(0), "(0,0) and (1,1) should differ")
		Expect(hv10.Cmp(hv01)).NotTo(Equal(0), "(1,0) and (0,1) should differ")

		// Same coordinates must give the same value (deterministic)
		hv00Again := hilbertValue([]int64{0, 0})
		Expect(hv00.Cmp(hv00Again)).To(Equal(0))
	})

	It("handles negative coordinates", func() {
		hvNeg := hilbertValue([]int64{-1, -1})
		hvPos := hilbertValue([]int64{1, 1})
		Expect(hvNeg).NotTo(BeNil())
		Expect(hvPos).NotTo(BeNil())
		Expect(hvNeg.Cmp(hvPos)).NotTo(Equal(0))
	})

	It("empty dimensions returns zero", func() {
		hv := hilbertValue([]int64{})
		Expect(hv.Cmp(big.NewInt(0))).To(Equal(0))
	})

	It("preserves locality — nearby points have closer Hilbert values than distant points", func() {
		// Core property of Hilbert curves: spatial locality preservation.
		hvOrigin := hilbertValue([]int64{100, 100})
		hvNear := hilbertValue([]int64{101, 100})
		hvFar := hilbertValue([]int64{100000, 100000})

		distNear := new(big.Int).Abs(new(big.Int).Sub(hvOrigin, hvNear))
		distFar := new(big.Int).Abs(new(big.Int).Sub(hvOrigin, hvFar))

		Expect(distNear.Cmp(distFar)).To(Equal(-1),
			"nearby point should have closer Hilbert value than distant point")
	})
})

var _ = Describe("RTree", func() {
	ctx := context.Background()

	It("insert and scan all", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_insert_scan")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 points.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)}, // key suffix (PK)
				tuple.Tuple{},         // value
			)).To(Succeed())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(30), int64(40)}},
				tuple.Tuple{int64(2)},
				tuple.Tuple{},
			)).To(Succeed())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(50), int64(60)}},
				tuple.Tuple{int64(3)},
				tuple.Tuple{},
			)).To(Succeed())

			// Scan all — should return all 3 items.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(3))

			// Collect all key suffixes.
			pks := make(map[int64]bool)
			for _, item := range items {
				pk, ok := item.KeySuffix[0].(int64)
				Expect(ok).To(BeTrue())
				pks[pk] = true
			}
			Expect(pks).To(HaveKey(int64(1)))
			Expect(pks).To(HaveKey(int64(2)))
			Expect(pks).To(HaveKey(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete removes point", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_delete")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 2 points.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{},
			)).To(Succeed())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(30), int64(40)}},
				tuple.Tuple{int64(2)},
				tuple.Tuple{},
			)).To(Succeed())

			// Delete the first point.
			Expect(rt.Delete(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
			)).To(Succeed())

			// Scan — only second point should remain.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].KeySuffix).To(Equal(tuple.Tuple{int64(2)}))
			Expect(items[0].Point.Coordinates).To(Equal(tuple.Tuple{int64(30), int64(40)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete nonexistent point is a no-op", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_delete_noop")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{},
			)).To(Succeed())

			// Delete a point that was never inserted.
			Expect(rt.Delete(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(99), int64(99)}},
				tuple.Tuple{int64(999)},
			)).To(Succeed())

			// Original point still there.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update replaces value for same key", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_update")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert a point with value.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{int64(100)}, // original value
			)).To(Succeed())

			// Update same point + suffix with a new value.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{int64(200)}, // updated value
			)).To(Succeed())

			// Scan — should have exactly 1 item with the new value.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].Value).To(Equal(tuple.Tuple{int64(200)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan empty tree returns nil", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_empty")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with MBR predicate prunes subtrees not individual items", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_mbr_predicate")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert points at (10, 20), (50, 60), (100, 200).
			for _, pt := range []struct {
				x, y, pk int64
			}{
				{10, 20, 1},
				{50, 60, 2},
				{100, 200, 3},
			} {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{pt.x, pt.y}},
					tuple.Tuple{pt.pk},
					tuple.Tuple{},
				)).To(Succeed())
			}

			// MBR predicate only prunes intermediate child slots, NOT individual
			// items in leaf nodes (matches Java). With 3 items in a root leaf,
			// all items are returned regardless of the predicate.
			queryMBR := MBR{Low: []int64{0, 0}, High: []int64{70, 70}}
			items, err := rt.Scan(rtx.Transaction(), nil, nil, func(m MBR) bool {
				return queryMBR.Overlaps(m)
			})
			Expect(err).NotTo(HaveOccurred())
			// Root leaf: predicate not applied to items, all 3 returned.
			Expect(items).To(HaveLen(3))

			pks := make(map[int64]bool)
			for _, item := range items {
				pk, _ := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			Expect(pks).To(HaveKey(int64(1)))
			Expect(pks).To(HaveKey(int64(2)))
			Expect(pks).To(HaveKey(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clear removes all data", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_clear")
			config := DefaultRTreeConfig(2)
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			for i := int64(0); i < 5; i++ {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{i * 10, i * 20}},
					tuple.Tuple{i},
					tuple.Tuple{},
				)).To(Succeed())
			}

			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(5))

			Expect(rt.Clear(rtx.Transaction())).To(Succeed())

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("small MaxM forces leaf split and intermediate overflow", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_small_maxm_overflow")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 25 items. With MaxM=4:
			// - root leaf splits at 5 items
			// - intermediate splits when it gets >4 children
			// 25 items distributed across leaves of capacity 4 = ~7 children,
			// which forces intermediate overflow.
			const n = 25
			for i := 0; i < n; i++ {
				err := rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 7), int64(i * 13)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{int64(i * 100)},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			// All items must be retrievable.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			// Verify all PKs present.
			pks := make(map[int64]bool)
			for _, item := range items {
				pk, ok := item.KeySuffix[0].(int64)
				Expect(ok).To(BeTrue())
				pks[pk] = true
			}
			for i := 0; i < n; i++ {
				Expect(pks).To(HaveKey(int64(i)), "missing PK %d", i)
			}

			// Verify items are in Hilbert order (non-decreasing HV).
			for i := 1; i < len(items); i++ {
				cmp := items[i-1].HilbertValue.Cmp(items[i].HilbertValue)
				if cmp == 0 {
					// Same HV: compare keys.
					cmp = tupleCompare(items[i-1].ItemKey(), items[i].ItemKey())
				}
				Expect(cmp).To(BeNumerically("<=", 0),
					"items[%d] should be <= items[%d] in Hilbert order", i-1, i)
			}

			// Verify values are correct.
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				val := item.Value[0].(int64)
				Expect(val).To(Equal(pk * 100))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("small MaxM split then delete forces underflow and fuse", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_small_maxm_fuse")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 20 items to create a multi-level tree.
			const n = 20
			type testItem struct {
				x, y, pk int64
			}
			inserted := make([]testItem, n)
			for i := 0; i < n; i++ {
				inserted[i] = testItem{int64(i * 5), int64(i * 11), int64(i)}
				err := rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{inserted[i].x, inserted[i].y}},
					tuple.Tuple{inserted[i].pk},
					tuple.Tuple{},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify all present before deletion.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			// Delete 15 items to force underflow/fuse at leaf and intermediate levels.
			// With MinM=2 and MaxM=4, deleting most items should trigger fuse cascades.
			remaining := make(map[int64]bool)
			for i := 0; i < n; i++ {
				if i%4 == 0 {
					// Keep every 4th item.
					remaining[int64(i)] = true
					continue
				}
				err := rt.Delete(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{inserted[i].x, inserted[i].y}},
					tuple.Tuple{inserted[i].pk},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan and verify only remaining items exist.
			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(len(remaining)))

			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				Expect(remaining).To(HaveKey(pk), "unexpected PK %d in scan results", pk)
			}

			// Verify Hilbert order is maintained after fuse.
			for i := 1; i < len(items); i++ {
				cmp := items[i-1].HilbertValue.Cmp(items[i].HilbertValue)
				if cmp == 0 {
					cmp = tupleCompare(items[i-1].ItemKey(), items[i].ItemKey())
				}
				Expect(cmp).To(BeNumerically("<=", 0),
					"items[%d] should be <= items[%d] in Hilbert order after fuse", i-1, i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deep tree with small MaxM creates 3+ levels and scan returns all items", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_deep_tree")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 60 items. With MaxM=4, each leaf holds up to 4 items.
			// 60 items / 4 = 15 leaves. 15 children / 4 = ~4 level-2 nodes.
			// 4 children / 4 = 1 root. That's a 3-level tree (root + 2 intermediate + leaves).
			const n = 60
			for i := 0; i < n; i++ {
				err := rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 3), int64(i * 7)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{int64(i)},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			// All items must survive.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			// Verify all PKs are present.
			pks := make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			for i := 0; i < n; i++ {
				Expect(pks).To(HaveKey(int64(i)), "missing PK %d in deep tree scan", i)
			}

			// Verify Hilbert order.
			for i := 1; i < len(items); i++ {
				cmp := items[i-1].HilbertValue.Cmp(items[i].HilbertValue)
				if cmp == 0 {
					cmp = tupleCompare(items[i-1].ItemKey(), items[i].ItemKey())
				}
				Expect(cmp).To(BeNumerically("<=", 0),
					"deep tree items[%d] should be <= items[%d] in Hilbert order", i-1, i)
			}

			// Now delete half and verify integrity.
			for i := 0; i < n; i += 2 {
				err := rt.Delete(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 3), int64(i * 7)}},
					tuple.Tuple{int64(i)},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n / 2))

			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				Expect(pk%2).To(Equal(int64(1)), "only odd PKs should remain, got %d", pk)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MBR predicate prunes subtrees after rebalancing with small MaxM", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_mbr_after_rebalancing")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 30 items spread across coordinate space.
			const n = 30
			for i := 0; i < n; i++ {
				err := rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 10), int64(i * 10)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			// Query: points in [0, 100] x [0, 100].
			// MBR predicate prunes at child slot level (intermediate nodes).
			// Items i=0..10 at (0,0)...(100,100) MUST be in the results.
			// Items beyond the query range MAY also appear if they share a leaf
			// with qualifying items (MBR predicate does NOT filter individual items,
			// matching Java behavior).
			queryMBR := MBR{Low: []int64{0, 0}, High: []int64{100, 100}}
			items, err := rt.Scan(rtx.Transaction(), nil, nil, func(m MBR) bool {
				return queryMBR.Overlaps(m)
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify all expected items are present (superset guarantee).
			expectedPKs := make(map[int64]bool)
			for i := 0; i <= 10; i++ {
				expectedPKs[int64(i)] = true
			}

			gotPKs := make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				gotPKs[pk] = true
			}

			for pk := range expectedPKs {
				Expect(gotPKs).To(HaveKey(pk), "expected PK %d in MBR query", pk)
			}

			// The result set may be larger than the exact match set — that's correct.
			// Items in pruned subtrees must NOT appear: items with ALL dimensions
			// far from the query range (high PKs like 20+) should be absent.
			// But items near the boundary may appear due to leaf cohabitation.
			Expect(len(items)).To(BeNumerically(">=", 11))
			Expect(len(items)).To(BeNumerically("<", n), "MBR pruning should exclude at least some items")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan continuation works after rebalancing with small MaxM", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_continuation_rebalancing")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 20 items.
			const n = 20
			for i := 0; i < n; i++ {
				err := rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 3), int64(i * 7)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{int64(i)},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan all to get full sorted list.
			allItems, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(allItems).To(HaveLen(n))

			// Simulate paginated scan: scan first half, then continue from midpoint.
			midIdx := n / 2
			midHV := allItems[midIdx-1].HilbertValue
			midKey := allItems[midIdx-1].ItemKey()

			rest, err := rt.Scan(rtx.Transaction(), midHV, midKey, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(rest).To(HaveLen(n - midIdx))

			// Verify the second half matches.
			for i, item := range rest {
				Expect(item.KeySuffix).To(Equal(allItems[midIdx+i].KeySuffix))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert, delete all, reinsert with small MaxM", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_reinsert_after_empty")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 15 items.
			const n = 15
			for i := 0; i < n; i++ {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i), int64(i * 2)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{},
				)).To(Succeed())
			}

			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			// Delete all items one by one.
			for i := 0; i < n; i++ {
				Expect(rt.Delete(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i), int64(i * 2)}},
					tuple.Tuple{int64(i)},
				)).To(Succeed())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())

			// Reinsert different items.
			for i := 0; i < n; i++ {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i + 100), int64(i + 200)}},
					tuple.Tuple{int64(i + 100)},
					tuple.Tuple{int64(i)},
				)).To(Succeed())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			pks := make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			for i := 0; i < n; i++ {
				Expect(pks).To(HaveKey(int64(i + 100)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("config validation rejects invalid configs", func() {
		// NumDimensions=0
		_, err := NewRTree(nil, RTreeConfig{
			MinM: 2, MaxM: 4, SplitS: 2,
			StoreHilbertValues: true, NumDimensions: 0,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("NumDimensions"))

		// MinM=0
		_, err = NewRTree(nil, RTreeConfig{
			MinM: 0, MaxM: 4, SplitS: 2,
			StoreHilbertValues: true, NumDimensions: 2,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("MinM"))

		// MaxM=1 (must be >= 2)
		_, err = NewRTree(nil, RTreeConfig{
			MinM: 1, MaxM: 1, SplitS: 1,
			StoreHilbertValues: true, NumDimensions: 2,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("MaxM"))

		// SplitS=0
		_, err = NewRTree(nil, RTreeConfig{
			MinM: 2, MaxM: 4, SplitS: 0,
			StoreHilbertValues: true, NumDimensions: 2,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SplitS"))

		// Split constraint violation: S*MaxM < (S+1)*MinM
		// MinM=10, MaxM=12, SplitS=2: 2*12=24 < 3*10=30
		_, err = NewRTree(nil, RTreeConfig{
			MinM: 10, MaxM: 12, SplitS: 2,
			StoreHilbertValues: true, NumDimensions: 2,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("split constraint"))

		// Default config should be valid.
		err = ValidateRTreeConfig(DefaultRTreeConfig(2))
		Expect(err).NotTo(HaveOccurred())

		// Default config for 3D should be valid.
		err = ValidateRTreeConfig(DefaultRTreeConfig(3))
		Expect(err).NotTo(HaveOccurred())
	})

	It("tree height transitions: grow and shrink", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_height_transitions")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 30 items to force multi-level tree.
			type testItem struct {
				x, y, pk int64
			}
			inserted := make([]testItem, 30)
			for i := 0; i < 30; i++ {
				inserted[i] = testItem{int64(i * 11), int64(i * 17), int64(i)}
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{inserted[i].x, inserted[i].y}},
					tuple.Tuple{inserted[i].pk},
					tuple.Tuple{},
				)).To(Succeed())
			}

			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(30))

			// Delete down to 3 items — forces tree shrinking (root promotion,
			// fuse cascades). Keep items 0, 15, 29.
			remaining := map[int64]bool{0: true, 15: true, 29: true}
			for i := 0; i < 30; i++ {
				if remaining[int64(i)] {
					continue
				}
				Expect(rt.Delete(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{inserted[i].x, inserted[i].y}},
					tuple.Tuple{inserted[i].pk},
				)).To(Succeed())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(3))

			pks := make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			for pk := range remaining {
				Expect(pks).To(HaveKey(pk), "missing surviving PK %d", pk)
			}

			// Insert 10 more items to grow the tree again.
			for i := 100; i < 110; i++ {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 3), int64(i * 5)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{},
				)).To(Succeed())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(13))

			pks = make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			for pk := range remaining {
				Expect(pks).To(HaveKey(pk))
			}
			for i := int64(100); i < 110; i++ {
				Expect(pks).To(HaveKey(i))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("StoreHilbertValues=false — recomputes HV on read", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_no_store_hv")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: false, NumDimensions: 2,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 10 items with known coordinates.
			const n = 10
			type testItem struct {
				x, y, pk int64
			}
			inserted := make([]testItem, n)
			for i := 0; i < n; i++ {
				inserted[i] = testItem{int64(i * 7), int64(i * 13), int64(i)}
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{inserted[i].x, inserted[i].y}},
					tuple.Tuple{inserted[i].pk},
					tuple.Tuple{int64(inserted[i].pk * 10)},
				)).To(Succeed())
			}

			// Scan — all 10 items must be present.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			// Verify all PKs and coordinates round-tripped correctly.
			pks := make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true

				// Coordinates must match the original insert.
				x := item.Point.Coordinate(0)
				y := item.Point.Coordinate(1)
				Expect(x).To(Equal(pk * 7))
				Expect(y).To(Equal(pk * 13))

				// Value must match.
				Expect(item.Value).To(Equal(tuple.Tuple{int64(pk * 10)}))

				// HilbertValue must have been recomputed (not nil).
				Expect(item.HilbertValue).NotTo(BeNil())

				// Recomputed HV must match freshly computed HV.
				expectedHV := hilbertValue([]int64{x, y})
				Expect(item.HilbertValue.Cmp(expectedHV)).To(Equal(0),
					"recomputed HV for PK %d should match hilbertValue([%d, %d])", pk, x, y)
			}
			for i := 0; i < n; i++ {
				Expect(pks).To(HaveKey(int64(i)), "missing PK %d", i)
			}

			// Verify Hilbert ordering (non-decreasing HV).
			for i := 1; i < len(items); i++ {
				cmp := items[i-1].HilbertValue.Cmp(items[i].HilbertValue)
				if cmp == 0 {
					cmp = tupleCompare(items[i-1].ItemKey(), items[i].ItemKey())
				}
				Expect(cmp).To(BeNumerically("<=", 0),
					"items[%d] should be <= items[%d] in Hilbert order", i-1, i)
			}

			// Insert more items to force a split, then re-scan.
			for i := n; i < n+10; i++ {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{int64(i * 7), int64(i * 13)}},
					tuple.Tuple{int64(i)},
					tuple.Tuple{int64(i * 10)},
				)).To(Succeed())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n + 10))

			// Verify all 20 items present with correct recomputed HVs.
			pks = make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true

				Expect(item.HilbertValue).NotTo(BeNil(), "HV should be recomputed for PK %d", pk)

				x := item.Point.Coordinate(0)
				y := item.Point.Coordinate(1)
				expectedHV := hilbertValue([]int64{x, y})
				Expect(item.HilbertValue.Cmp(expectedHV)).To(Equal(0),
					"recomputed HV for PK %d should match after split", pk)
			}
			for i := 0; i < n+10; i++ {
				Expect(pks).To(HaveKey(int64(i)), "missing PK %d after split", i)
			}

			// Hilbert order must still hold after split.
			for i := 1; i < len(items); i++ {
				cmp := items[i-1].HilbertValue.Cmp(items[i].HilbertValue)
				if cmp == 0 {
					cmp = tupleCompare(items[i-1].ItemKey(), items[i].ItemKey())
				}
				Expect(cmp).To(BeNumerically("<=", 0),
					"after split items[%d] should be <= items[%d] in Hilbert order", i-1, i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("3D R-tree insert, scan, and delete", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			sub := ks.Sub("rtree_3d")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 3,
			}
			storage := NewStorageAdapter(sub, config)
			rt, err := NewRTree(storage, config)
			Expect(err).NotTo(HaveOccurred())

			// Insert 15 items with 3 coordinates.
			const n = 15
			type item3D struct {
				x, y, z, pk int64
			}
			items3D := make([]item3D, n)
			for i := 0; i < n; i++ {
				items3D[i] = item3D{int64(i * 7), int64(i * 13), int64(i * 3), int64(i)}
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{items3D[i].x, items3D[i].y, items3D[i].z}},
					tuple.Tuple{items3D[i].pk},
					tuple.Tuple{int64(i * 100)},
				)).To(Succeed())
			}

			// Scan all — should return all 15.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(n))

			pks := make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true
				// Verify 3 coordinates present.
				Expect(item.Point.Coordinates).To(HaveLen(3))
			}
			for i := 0; i < n; i++ {
				Expect(pks).To(HaveKey(int64(i)), "missing PK %d in 3D tree", i)
			}

			// Delete 5 items (even indices 0, 2, 4, 6, 8).
			for i := 0; i < 10; i += 2 {
				Expect(rt.Delete(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{items3D[i].x, items3D[i].y, items3D[i].z}},
					tuple.Tuple{items3D[i].pk},
				)).To(Succeed())
			}

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(10))

			pks = make(map[int64]bool)
			for _, item := range items {
				pk := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			// Deleted: 0, 2, 4, 6, 8. Remaining: 1, 3, 5, 7, 9, 10, 11, 12, 13, 14.
			for _, deleted := range []int64{0, 2, 4, 6, 8} {
				Expect(pks).NotTo(HaveKey(deleted))
			}
			for _, kept := range []int64{1, 3, 5, 7, 9, 10, 11, 12, 13, 14} {
				Expect(pks).To(HaveKey(kept))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
