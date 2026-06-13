package recordlayer

import (
	"context"
	"errors"
	"math"
	"math/big"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_insert_scan")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_delete")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_delete_noop")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_update")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_empty")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_mbr_predicate")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_clear")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_small_maxm_overflow")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_small_maxm_fuse")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_deep_tree")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_mbr_after_rebalancing")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_continuation_rebalancing")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_reinsert_after_empty")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_height_transitions")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_no_store_hv")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: false, NumDimensions: 2,
			}
			storage := newRTreeStorage(sub, config)
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_3d")
			config := RTreeConfig{
				MinM: 2, MaxM: 4, SplitS: 2,
				StoreHilbertValues: true, NumDimensions: 3,
			}
			storage := newRTreeStorage(sub, config)
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

var _ = Describe("MultidimensionalIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("basic lifecycle — save records and scan index", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 orders with different (price, quantity).
			for _, o := range []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 20},
				{3, 300, 30},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index — should return 3 entries.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Verify that all coordinate pairs are present.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				Expect(len(e.Key)).To(BeNumerically(">=", 2))
				x, ok := e.Key[0].(int64)
				Expect(ok).To(BeTrue())
				y, ok := e.Key[1].(int64)
				Expect(ok).To(BeTrue())
				found[coordPair{x, y}] = true
			}
			Expect(found).To(HaveKey(coordPair{100, 10}))
			Expect(found).To(HaveKey(coordPair{200, 20}))
			Expect(found).To(HaveKey(coordPair{300, 30}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan honors the aggregate scanned-records budget (RFC-106a)", func() {
		ks := specSubspace()

		// PrefixSize=1: price is the partition prefix; coord_x/coord_y are the 2
		// R-tree dims. Distinct prices → distinct R-tree partitions, so an
		// unbounded-prefix scan must skip-scan across them (prefixSkipScanCursor).
		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x"), Field("coord_y")), 1, 2)
		mdIdx := NewMultidimensionalIndex("md_prefix_skip", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const partitions = 20 // 20 distinct prices → 20 small (1-point) prefixes
		const scanLimit = 5

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < partitions; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i)), // distinct prefix per record
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Unbounded-prefix scan with a small aggregate scan budget + fail. Each
			// prefix holds 1 point (< scanLimit), so WITHOUT the shared budget the
			// skip-scan reads all 20 cleanly; WITH it, totalScanned (per-prefix scans
			// + findNextPrefix enumeration reads) trips ScanLimitReached at the cap.
			scan := ForwardScan()
			scan.ExecuteProperties = scan.ExecuteProperties.WithScannedRecordsLimit(scanLimit)
			scan.ExecuteProperties.FailOnScanLimitReached = true

			_, scanErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, scan))
			var sle *ScanLimitReachedError
			Expect(errors.As(scanErr, &sle)).To(BeTrue(),
				"the cross-prefix scan budget must trip ScanLimitReachedError, got: %v", scanErr)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan honors the aggregate scanned-BYTES budget (RFC-106a)", func() {
		ks := specSubspace()

		// Same prefix-partitioned shape as the records test, but the shared budget
		// is a BYTE cap. Each prefix is one small point; per-prefix byte counters
		// reset, so only the cross-prefix totalBytesScanned (per-prefix point reads
		// + findNextPrefix enumeration reads) trips the cap.
		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x"), Field("coord_y")), 1, 2)
		mdIdx := NewMultidimensionalIndex("md_prefix_skip_bytes", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const partitions = 20

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < partitions; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i)),
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// A small byte cap (one prefix's point + enumeration read is already
			// tens of bytes) trips the cross-prefix byte budget. The skip-scan
			// cannot resume cross-prefix, so an aggregate budget stop is a terminal
			// ScanLimitReachedError regardless of FailOnScanLimitReached.
			scan := ForwardScan()
			scan.ExecuteProperties = scan.ExecuteProperties.WithScannedBytesLimit(40)

			_, scanErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, scan))
			var sle *ScanLimitReachedError
			Expect(errors.As(scanErr, &sle)).To(BeTrue(),
				"the cross-prefix byte budget must trip ScanLimitReachedError, got: %v", scanErr)
			Expect(sle.Reason).To(Equal(ByteLimitReached))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete record clears index entry", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save one order.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify it is in the index.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Delete the record.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Index should be empty.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update record updates index entry", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save original order.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update same order with new price and quantity.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(500),
				Quantity: proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			// Should have exactly 1 entry with the new coordinates.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(500)))
			Expect(entries[0].Key[1]).To(Equal(int64(50)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multiple records — save 5 and verify all present", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		type order struct {
			id       int64
			price    int32
			quantity int32
		}
		orders := []order{
			{1, 10, 100},
			{2, 20, 200},
			{3, 30, 300},
			{4, 40, 400},
			{5, 50, 500},
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, _ := e.Key[0].(int64)
				y, _ := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("mixed save and delete — interleaved operations", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3.
			for _, id := range []int64{1, 2, 3} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(id),
					Price:    proto.Int32(int32(id * 100)),
					Quantity: proto.Int32(int32(id * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete #2.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Save #4.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(4),
				Price:    proto.Int32(400),
				Quantity: proto.Int32(40),
			})
			Expect(err).NotTo(HaveOccurred())

			// Should have 3 entries: #1, #3, #4.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, _ := e.Key[0].(int64)
				y, _ := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			Expect(found).To(HaveKey(coordPair{100, 10}))
			Expect(found).NotTo(HaveKey(coordPair{200, 20})) // deleted
			Expect(found).To(HaveKey(coordPair{300, 30}))
			Expect(found).To(HaveKey(coordPair{400, 40}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MULTIDIMENSIONAL index with small MaxM forces R-tree splits via index maintainer", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty_small", dimExpr)
		// Configure small MaxM via index options.
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Generate enough orders to force multiple R-tree splits.
		// With MaxM=4, 25 records should create a multi-level tree.
		const n = 25
		type order struct {
			id       int64
			price    int32
			quantity int32
		}
		orders := make([]order, n)
		for i := 0; i < n; i++ {
			orders[i] = order{
				id:       int64(i + 1),
				price:    int32((i + 1) * 50),
				quantity: int32((i + 1) * 7),
			}
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index and verify all entries present.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Verify all coordinate pairs are present.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, ok := e.Key[0].(int64)
				Expect(ok).To(BeTrue())
				y, ok := e.Key[1].(int64)
				Expect(ok).To(BeTrue())
				found[coordPair{x, y}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}),
					"missing order %d at (%d, %d)", o.id, o.price, o.quantity)
			}

			// Now delete half the records and verify remaining.
			for i := 0; i < n; i += 2 {
				existed, err := store.DeleteRecord(tuple.Tuple{orders[i].id})
				Expect(err).NotTo(HaveOccurred())
				Expect(existed).To(BeTrue())
			}

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			remaining := n / 2 // odd-indexed orders survive
			Expect(entries).To(HaveLen(remaining))

			found = make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 1; i < n; i += 2 {
				o := orders[i]
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}),
					"missing surviving order %d", o.id)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MULTIDIMENSIONAL index with small MaxM — update records after splits", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty_update", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const n = 15

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert n records.
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Update every record with new coordinates. This exercises
			// delete-old-entry + insert-new-entry through the split tree.
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i*100 + 1)),
					Quantity: proto.Int32(int32(i*10 + 1)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Verify updated coordinates.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 1; i <= n; i++ {
				Expect(found).To(HaveKey(coordPair{int64(i*100 + 1), int64(i*10 + 1)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("continuation token round-trip through ScanIndex", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_cont_roundtrip", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 15 records.
			const n = 15
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Paginate with limit=5.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(5),
			}

			// Page 1.
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(5))
			Expect(cont1).NotTo(BeNil(), "continuation should be non-nil after page 1")

			// Page 2.
			page2, cont2, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, TupleRangeAll, cont1, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(5))
			Expect(cont2).NotTo(BeNil(), "continuation should be non-nil after page 2")

			// Page 3.
			page3, _, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, TupleRangeAll, cont2, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page3).To(HaveLen(5))

			// Verify all 15 unique entries, no duplicates, no gaps.
			type coordPair struct{ x, y int64 }
			allCoords := make(map[coordPair]bool)
			for _, pages := range [][]*IndexEntry{page1, page2, page3} {
				for _, e := range pages {
					x := e.Key[0].(int64)
					y := e.Key[1].(int64)
					cp := coordPair{x, y}
					Expect(allCoords).NotTo(HaveKey(cp), "duplicate entry at (%d, %d)", x, y)
					allCoords[cp] = true
				}
			}
			Expect(allCoords).To(HaveLen(n))
			for i := 1; i <= n; i++ {
				Expect(allCoords).To(HaveKey(coordPair{int64(i * 100), int64(i * 10)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("row limit enforcement", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_row_limit", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 10 records.
			for i := 1; i <= 10; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 50)),
					Quantity: proto.Int32(int32(i * 5)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// ScanIndex with ReturnedRowLimit=3.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(3),
			}
			cursor := store.ScanIndex(mdIdx, TupleRangeAll, nil, props)
			defer func() { _ = cursor.Close() }()

			count := 0
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					// Must stop due to row limit, not source exhaustion.
					Expect(result.GetNoNextReason()).To(Equal(ReturnLimitReached))
					cont := result.GetContinuation()
					Expect(cont).NotTo(BeNil())
					Expect(cont.IsEnd()).To(BeFalse(), "continuation should not be end when limit reached")
					break
				}
				count++
			}
			Expect(count).To(Equal(3), "exactly 3 entries should be returned")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("negative and boundary coordinates", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_neg_boundary", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save records with extreme coordinates.
			type testCase struct {
				id       int64
				price    int32
				quantity int32
			}
			cases := []testCase{
				{1, -100, -200},
				{2, 0, 0},
				{3, math.MaxInt32, math.MinInt32},
				{4, 1, -1},
			}

			for _, tc := range cases {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(tc.id),
					Price:    proto.Int32(tc.price),
					Quantity: proto.Int32(tc.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan — all 4 entries must be present.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(4))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for _, tc := range cases {
				Expect(found).To(HaveKey(coordPair{int64(tc.price), int64(tc.quantity)}),
					"missing entry for order %d at (%d, %d)", tc.id, tc.price, tc.quantity)
			}

			// Delete one (the extreme one) and verify rest survive.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			found = make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			Expect(found).NotTo(HaveKey(coordPair{int64(math.MaxInt32), int64(math.MinInt32)}))
			Expect(found).To(HaveKey(coordPair{-100, -200}))
			Expect(found).To(HaveKey(coordPair{0, 0}))
			Expect(found).To(HaveKey(coordPair{1, -1}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("duplicate coordinate points with different PKs", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_dup_coords", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save two orders with identical (price=100, quantity=10) but different PKs.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(2),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan — both entries must be present.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Both entries have the same coordinates but different PK in the key suffix.
			for _, e := range entries {
				Expect(e.Key[0]).To(Equal(int64(100)))
				Expect(e.Key[1]).To(Equal(int64(10)))
			}

			// Delete one. The other must survive.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[0].Key[1]).To(Equal(int64(10)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteAllRecords clears R-tree completely", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_delete_all", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 20 records (enough for multi-level R-tree with MaxM=4).
			const n = 20
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 50)),
					Quantity: proto.Int32(int32(i * 7)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// DeleteAllRecords.
			Expect(store.DeleteAllRecords()).To(Succeed())

			// Index should be empty.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			// Save new records — only new ones should appear.
			for i := 100; i < 105; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 10)),
					Quantity: proto.Int32(int32(i * 3)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 100; i < 105; i++ {
				Expect(found).To(HaveKey(coordPair{int64(i * 10), int64(i * 3)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with MBR predicate from scanRange prunes subtrees", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_mbr_scan", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 30 records spread across coordinate space.
			const n = 30
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 10)),
					Quantity: proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with spatial bounds [0, 100] x [0, 100].
			// This should return items with (price, quantity) in that range via MBR pruning.
			spatialRange := TupleRange{
				Low:  tuple.Tuple{int64(0), int64(0)},
				High: tuple.Tuple{int64(100), int64(100)},
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, spatialRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Items with (i*10, i*10) where i*10 <= 100 → i = 1..10 → 10 items must be present.
			// MBR predicate only prunes intermediate subtrees, not leaf items,
			// so we may get extra items sharing leaves with qualifying ones.
			expectedCount := 10
			foundExpected := 0
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				if x >= 0 && x <= 100 && y >= 0 && y <= 100 {
					foundExpected++
				}
			}
			Expect(foundExpected).To(Equal(expectedCount),
				"all 10 qualifying items must be in results")

			// Scan all (no bounds) to compare.
			allEntries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(n))

			// With MBR pruning on a multi-level tree (MaxM=4, 30 items),
			// the bounded scan should return fewer entries than the full scan.
			Expect(len(entries)).To(BeNumerically("<", len(allEntries)),
				"MBR-bounded scan should return fewer entries than full scan")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with one-sided MBR bounds from scanRange", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_mbr_onesided", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 5 orders.
			for i := 1; i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with only Low bound: [200, 20] to +inf.
			// Items: (100,10), (200,20), (300,30), (400,40), (500,50)
			// Filter: coord >= [200, 20] → items 2,3,4,5 match.
			lowOnly := TupleRange{
				Low: tuple.Tuple{int64(200), int64(20)},
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, lowOnly, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(4))

			// Scan with only High bound: -inf to [300, 30].
			// Filter: coord <= [300, 30] → items 1,2,3 match.
			highOnly := TupleRange{
				High: tuple.Tuple{int64(300), int64(30)},
			}
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, highOnly, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with MBR predicate and continuation tokens", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_mbr_cont", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 20 records.
			const n = 20
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 10)),
					Quantity: proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with MBR bounds and a row limit, then continue.
			spatialRange := TupleRange{
				Low:  tuple.Tuple{int64(0), int64(0)},
				High: tuple.Tuple{int64(200), int64(200)},
			}
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(5),
			}

			// Page 1.
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, spatialRange, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(5))
			Expect(cont1).NotTo(BeNil())

			// Page 2.
			page2, _, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, spatialRange, cont1, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(len(page2)).To(BeNumerically(">", 0), "page 2 should have entries")

			// No duplicates between pages.
			type coordPair struct{ x, y int64 }
			seen := make(map[coordPair]bool)
			for _, e := range page1 {
				cp := coordPair{e.Key[0].(int64), e.Key[1].(int64)}
				seen[cp] = true
			}
			for _, e := range page2 {
				cp := coordPair{e.Key[0].(int64), e.Key[1].(int64)}
				Expect(seen).NotTo(HaveKey(cp), "duplicate entry in page 2: (%d, %d)", cp.x, cp.y)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan enumerates all prefixes", func() {
		ks := specSubspace()

		// quantity is prefix (PrefixSize=1), price is 1D spatial dimension.
		dimExpr := Dimensions(Concat(Field("quantity"), Field("price")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_skip", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save orders with 3 distinct quantity values (prefixes).
			// quantity=10: orders 1,2,3; quantity=20: orders 4,5; quantity=30: orders 6,7,8.
			orders := []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 10},
				{3, 300, 10},
				{4, 400, 20},
				{5, 500, 20},
				{6, 600, 30},
				{7, 700, 30},
				{8, 800, 30},
			}

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with TupleRangeAll (no prefix specified) — triggers prefix skip-scan.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(8))

			// Verify all entries from all prefixes are present.
			type entry struct{ qty, price int64 }
			found := make(map[entry]bool)
			for _, e := range entries {
				qty := e.Key[0].(int64)
				price := e.Key[1].(int64)
				found[entry{qty, price}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(entry{int64(o.quantity), int64(o.price)}),
					"missing order %d at (qty=%d, price=%d)", o.id, o.quantity, o.price)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan with specific prefix scans only that prefix", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("price")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_specific", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orders := []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 10},
				{3, 300, 20},
				{4, 400, 20},
				{5, 500, 20},
				{6, 600, 30},
			}

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with a specific prefix (quantity=20) — should return only orders 3,4,5.
			specificPrefix := TupleRange{
				Low:  tuple.Tuple{int64(20)},
				High: tuple.Tuple{int64(20)},
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, specificPrefix, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			for _, e := range entries {
				Expect(e.Key[0]).To(Equal(int64(20)), "all entries should have quantity=20")
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan with row limit", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("price")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_limit", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 12 orders across 3 prefixes.
			for i := 1; i <= 12; i++ {
				qty := int32(((i-1)/4 + 1) * 10) // 10, 10, 10, 10, 20, 20, 20, 20, 30, 30, 30, 30
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(qty),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with limit=5 and no prefix (prefix skip-scan).
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(5),
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5), "should return exactly 5 entries with limit=5")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan with empty index returns empty", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("price")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_empty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// No records saved — scan should return empty.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan after delete from one prefix", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("price")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_delete", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orders := []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 10},
				{3, 300, 20},
				{4, 400, 20},
			}

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete both orders in prefix quantity=10.
			for _, id := range []int64{1, 2} {
				existed, err := store.DeleteRecord(tuple.Tuple{id})
				Expect(err).NotTo(HaveOccurred())
				Expect(existed).To(BeTrue())
			}

			// Prefix skip-scan should only find the quantity=20 entries.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			for _, e := range entries {
				Expect(e.Key[0]).To(Equal(int64(20)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RebuildIndex for MULTIDIMENSIONAL", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_rebuild", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 10 records.
			const n = 10
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify all entries present before rebuild.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Rebuild the index (WRITE_ONLY -> re-index -> READABLE).
			Expect(store.RebuildIndex(mdIdx)).To(Succeed())

			// Verify all 10 entries match after rebuild.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 1; i <= n; i++ {
				Expect(found).To(HaveKey(coordPair{int64(i * 100), int64(i * 10)}),
					"missing entry for order %d after rebuild", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("DimensionsKeyExpression", func() {
	It("proto round-trip preserves expression", func() {
		wholeKey := Concat(Field("price"), Field("quantity"))
		dimExpr := Dimensions(wholeKey, 0, 2)

		// Serialize to proto.
		protoExpr := dimExpr.ToKeyExpression()
		Expect(protoExpr).NotTo(BeNil())
		Expect(protoExpr.Dimensions).NotTo(BeNil())
		Expect(protoExpr.Dimensions.GetPrefixSize()).To(Equal(int32(0)))
		Expect(protoExpr.Dimensions.GetDimensionsSize()).To(Equal(int32(2)))

		// Deserialize from proto.
		restored, err := KeyExpressionFromProto(protoExpr)
		Expect(err).NotTo(HaveOccurred())

		restoredDim, ok := restored.(*DimensionsKeyExpression)
		Expect(ok).To(BeTrue(), "restored expression should be *DimensionsKeyExpression")
		Expect(restoredDim.PrefixSize).To(Equal(0))
		Expect(restoredDim.DimensionsSize).To(Equal(2))
		Expect(restoredDim.ColumnSize()).To(Equal(dimExpr.ColumnSize()))
		Expect(restoredDim.FieldNames()).To(Equal(dimExpr.FieldNames()))
	})

	It("proto round-trip with prefix", func() {
		// 3-column key: 1 prefix + 2 dimensions
		wholeKey := Concat(Field("tags"), Field("price"), Field("quantity"))
		dimExpr := Dimensions(wholeKey, 1, 2)

		protoExpr := dimExpr.ToKeyExpression()
		restored, err := KeyExpressionFromProto(protoExpr)
		Expect(err).NotTo(HaveOccurred())

		restoredDim := restored.(*DimensionsKeyExpression)
		Expect(restoredDim.PrefixSize).To(Equal(1))
		Expect(restoredDim.DimensionsSize).To(Equal(2))
		Expect(restoredDim.SuffixSize()).To(Equal(0))
		Expect(restoredDim.ColumnSize()).To(Equal(3))
	})

	It("SplitIndexEntry correctly partitions tuple", func() {
		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)

		entry := tuple.Tuple{int64(100), int64(200)}
		prefix, dims, suffix := dimExpr.SplitIndexEntry(entry)

		Expect(prefix).To(BeNil())
		Expect(dims).To(Equal(tuple.Tuple{int64(100), int64(200)}))
		Expect(suffix).To(BeNil())
	})

	It("SplitIndexEntry with prefix and suffix", func() {
		// 4-column: 1 prefix, 2 dimensions, 1 suffix
		wholeKey := Concat(Field("tags"), Field("price"), Field("quantity"), Field("order_id"))
		dimExpr := Dimensions(wholeKey, 1, 2)

		entry := tuple.Tuple{"group1", int64(100), int64(200), int64(42)}
		prefix, dims, suffix := dimExpr.SplitIndexEntry(entry)

		Expect(prefix).To(Equal(tuple.Tuple{"group1"}))
		Expect(dims).To(Equal(tuple.Tuple{int64(100), int64(200)}))
		Expect(suffix).To(Equal(tuple.Tuple{int64(42)}))
	})
})
