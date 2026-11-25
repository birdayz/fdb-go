package rtree

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Persisted one-dimensional Hilbert routing", func() {
	for _, layout := range []RTreeStorage{RTreeStorageByNode, RTreeStorageBySlot} {
		for _, storeHV := range []bool{false, true} {
			It(fmt.Sprintf("reopens, updates, deletes and paginates a split %s tree with stored Hilbert values %t", layout, storeHV), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				DeferCleanup(cancel)
				sub := specSubspace()
				config := DefaultRTreeConfig(1)
				config.Storage, config.StoreHilbertValues = layout, storeHV
				open := func() (*StorageAdapter, *RTree) {
					storage := NewStorageAdapter(sub, config)
					rt, err := NewRTree(storage, config)
					Expect(err).NotTo(HaveOccurred())
					return storage, rt
				}
				point := func(pk int64) Point { return Point{Coordinates: tuple.Tuple{pk}} }
				// Each coordinate is nonnegative: all bounds are in [2^63, 2^64),
				// which tuple.Unpack represents as uint64, not int64 or *big.Int.
				const count = 33
				_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
					_, rt := open()
					for pk := int64(0); pk < count; pk++ {
						if err := rt.InsertOrUpdate(rtx.Transaction(), point(pk), tuple.Tuple{pk}, tuple.Tuple{pk * 10}); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// A separate transaction and adapter must read a persisted split root.
				_, err = sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
					storage, _ := open()
					root, err := storage.fetchIntermediateNode(rtx.Transaction(), rootNodeID)
					Expect(err).NotTo(HaveOccurred())
					Expect(root).NotTo(BeNil())
					Expect(root.slots).To(HaveLen(2))
					Expect(root.slots[0].SmallestHV.String()).To(Equal("9223372036854775808"))
					Expect(root.slots[1].LargestHV.String()).To(Equal("9223372036854775840"))

					// Inspect the packed root too, so the regression cannot vacuously
					// pass without exercising uint64 tuple decoding in either layout.
					prefix := sub.Pack(tuple.Tuple{rootNodeID})
					var slots tuple.Tuple
					if layout == RTreeStorageByNode {
						data, err := rtx.Transaction().Get(fdb.Key(prefix)).Get()
						Expect(err).NotTo(HaveOccurred())
						node, err := tuple.Unpack(data)
						Expect(err).NotTo(HaveOccurred())
						slots = node[1].(tuple.Tuple)
					} else {
						r, err := fdb.PrefixRange(prefix)
						Expect(err).NotTo(HaveOccurred())
						kvs, err := rtx.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
						Expect(err).NotTo(HaveOccurred())
						for _, kv := range kvs {
							key, err := tuple.Unpack(kv.Key[len(prefix):])
							Expect(err).NotTo(HaveOccurred())
							slots = append(slots, tuple.Tuple(key[1:]))
						}
					}
					Expect(slots).To(HaveLen(2))
					for _, elem := range slots {
						slot := elem.(tuple.Tuple)
						Expect(slot[0]).To(BeAssignableToTypeOf(uint64(0)))
						Expect(slot[2]).To(BeAssignableToTypeOf(uint64(0)))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				checkItems := func(items []ItemSlot, updated, deleted bool) {
					wantCount := count
					if deleted {
						wantCount--
					}
					Expect(items).To(HaveLen(wantCount))
					position := 0
					seen := make(map[int64]bool, wantCount)
					for pk := int64(0); pk < count; pk++ {
						if deleted && pk == 0 {
							continue
						}
						item := items[position]
						position++
						Expect(item.KeySuffix).To(Equal(tuple.Tuple{pk}))
						Expect(item.Point.Coordinates).To(Equal(tuple.Tuple{pk}))
						actualPK := item.KeySuffix[0].(int64)
						Expect(seen).NotTo(HaveKey(actualPK), "duplicate point %d", actualPK)
						seen[actualPK] = true
						wantValue := pk * 10
						if updated && pk == 0 {
							wantValue = 999
						}
						Expect(item.Value).To(Equal(tuple.Tuple{wantValue}))
						Expect(item.HilbertValue).NotTo(BeNil())
						Expect(item.HilbertValue.Cmp(new(big.Int).SetUint64(uint64(1)<<63 + uint64(pk)))).To(BeZero())
					}
					if deleted {
						Expect(seen).NotTo(HaveKey(int64(0)), "Delete must remove the updated target, not report success as a no-op")
					}
				}
				checkFullScan := func(updated, deleted bool) {
					result, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
						_, rt := open()
						return rt.Scan(rtx.Transaction(), nil, nil, nil)
					})
					Expect(err).NotTo(HaveOccurred())
					checkItems(result.([]ItemSlot), updated, deleted)
				}
				checkPagination := func(updated, deleted bool) {
					var lastHV *big.Int
					var lastKey tuple.Tuple
					var all []ItemSlot
					const pageSize = 5
					exhausted := false
					for range count + 1 {
						// Each page reopens the persisted tree in a new transaction.
						result, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
							_, rt := open()
							iter := rt.ScanIterator(rtx.Transaction(), lastHV, lastKey, nil)
							page := make([]ItemSlot, 0, pageSize)
							for range pageSize {
								item, ok, err := iter.Next()
								if err != nil {
									return nil, err
								}
								if !ok {
									break
								}
								page = append(page, item)
							}
							return page, nil
						})
						Expect(err).NotTo(HaveOccurred())
						page := result.([]ItemSlot)
						if len(page) == 0 {
							exhausted = true
							break
						}
						all = append(all, page...)
						last := page[len(page)-1]
						lastHV, lastKey = last.HilbertValue, last.ItemKey()
					}
					Expect(exhausted).To(BeTrue(), "continuation failed to terminate")
					checkItems(all, updated, deleted)
				}

				checkFullScan(false, false)
				checkPagination(false, false)
				// Updating the first leaf must replace one row, not route to the
				// last leaf and create a second copy of the same point and suffix.
				_, err = sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
					_, rt := open()
					return nil, rt.InsertOrUpdate(rtx.Transaction(), point(0), tuple.Tuple{int64(0)}, tuple.Tuple{int64(999)})
				})
				Expect(err).NotTo(HaveOccurred())
				checkFullScan(true, false)
				_, err = sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
					_, rt := open()
					return nil, rt.Delete(rtx.Transaction(), point(0), tuple.Tuple{int64(0)})
				})
				Expect(err).NotTo(HaveOccurred())
				checkFullScan(true, true)
				checkPagination(true, true)
			})
		}
	}
})
