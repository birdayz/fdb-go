package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// markRangeSetComplete inserts the full range into the IndexingRangeSet so that
// checkIndexBuilt passes. This simulates the range set being fully built.
func markRangeSetComplete(store *FDBRecordStore, index *Index) {
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	_, err := rangeSet.InsertRange(store.context.Transaction(), nil, nil, false)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("IndexState", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	for _, disabled := range []bool{false, true} {
		It(fmt.Sprintf("index update boundary propagates cancellation disabled=%t", disabled), func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			rtx := sharedDB.NewRecordContext(tx)
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			record, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			if disabled {
				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())
			}
			tx.Cancel()
			var canceled fdb.Error
			Expect(errors.As(store.updateSecondaryIndexes(record, nil), &canceled)).To(BeTrue())
			Expect(canceled.Code).To(Equal(1025))
		})
	}

	It("index update dispatch does not maintain an index disabled after candidate selection", func() {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			record, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			other, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).Open()
			Expect(err).NotTo(HaveOccurred())
			store.stateMu.RLock()
			maintain, err := store.shouldMaintainIndex("Order$price")
			store.stateMu.RUnlock()
			Expect(err).NotTo(HaveOccurred())
			Expect(maintain).To(BeTrue())
			_, err = other.MarkIndexDisabled("Order$price")
			Expect(err).NotTo(HaveOccurred())
			index := md.GetIndex("Order$price")
			store.stateMu.RLock()
			err = store.updateOneIndex(index, nil, record)
			store.stateMu.RUnlock()
			Expect(err).NotTo(HaveOccurred())
			rows, err := rtx.Transaction().GetRange(store.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("lazy transition invalidates an independently warmed cache", func() {
		ss := specSubspace()
		cache := NewMetaDataVersionStampStoreStateCache()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SetStateCacheability(true)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetStoreStateCache(cache).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		cache.mu.Lock()
		cached := cache.entries[string(ss.Bytes())]
		cache.mu.Unlock()
		Expect(cached).NotTo(BeNil(), "control: the old state really was admitted")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexDisabled("Order$price")
			Expect(err).NotTo(HaveOccurred())
			Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetStoreStateCache(cache).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateDisabled))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("lazy transition propagates malformed header errors without state writes", func() {
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		rtx := sharedDB.NewRecordContext(tx)
		ss := specSubspace()
		tx.Set(ss.Pack(tuple.Tuple{StoreInfoKey}), []byte{0x80})
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Build()
		Expect(err).NotTo(HaveOccurred())
		changed, err := store.MarkIndexDisabled("Order$price")
		Expect(err).To(HaveOccurred())
		Expect(changed).To(BeFalse())
		Expect(rtx.HasDirtyStoreState()).To(BeFalse())
		Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeFalse())
		rows, err := tx.GetRange(store.indexStateSubspace(), fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
	})

	It("loads the header before a lazy state transition and retains record-update locks", func() {
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SetStateCacheability(true)
			Expect(err).NotTo(HaveOccurred())
			return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "locked before lazy open")
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Build()
			Expect(err).NotTo(HaveOccurred())
			changed, err := store.MarkIndexDisabled("Order$price")
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			Expect(store.GetStoreHeader()).NotTo(BeNil(), "state mutations must preload the persisted header")
			Expect(store.IsCacheable()).To(BeTrue())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			var locked *StoreIsLockedForRecordUpdatesError
			Expect(errors.As(err, &locked)).To(BeTrue())
			Expect(locked.Reason).To(Equal("locked before lazy open"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("Default state", func() {
		It("all indexes default to READABLE", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable))
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexDisabled("Order$price")).To(BeFalse())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeFalse())
				Expect(store.IsIndexScannable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MarkIndexDisabled", func() {
		It("disables an index and persists across reopens", func() {
			ss := specSubspace()

			// Mark disabled in one transaction
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				changed, err := store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				Expect(store.IsIndexDisabled("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$price")).To(BeFalse())
				Expect(store.IsIndexScannable("Order$price")).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Reopen in new transaction — state should be persisted
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}

				Expect(store.IsIndexDisabled("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$price")).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false if already disabled", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())

				changed, err := store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MarkIndexWriteOnly", func() {
		It("preserves built coverage when readable becomes write-only", func() {
			for _, partial := range []bool{false, true} {
				ss := specSubspace()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					ranges := NewIndexingRangeSet(ss, md.GetIndex("Order$price"))
					ranges.Clear(rtx.Transaction())
					if partial {
						_, err = ranges.InsertRange(rtx.Transaction(), []byte{0x20}, []byte{0x40}, false)
						Expect(err).NotTo(HaveOccurred())
					}
					before, err := ranges.ListMissingRanges(rtx.Transaction())
					Expect(err).NotTo(HaveOccurred())
					changed, err := store.MarkIndexWriteOnly("Order$price")
					Expect(err).NotTo(HaveOccurred())
					Expect(changed).To(BeTrue())
					after, err := ranges.ListMissingRanges(rtx.Transaction())
					Expect(err).NotTo(HaveOccurred())
					if partial {
						Expect(after).To(Equal(before))
						Expect(after).To(HaveLen(2))
					} else {
						Expect(after).To(BeEmpty())
						changed, err = store.MarkIndexReadable("Order$price")
						Expect(err).NotTo(HaveOccurred())
						Expect(changed).To(BeTrue())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})
		It("sets write-only state", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				changed, err := store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())
				Expect(store.IsIndexScannable("Order$price")).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MarkIndexReadable", func() {
		It("transitions from disabled back to readable when range set is complete", func() {
			ss := specSubspace()

			// Disable and mark range set complete
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				if err != nil {
					return nil, err
				}
				// Mark range set as complete so checkIndexBuilt passes.
				markRangeSetComplete(store, md.GetIndex("Order$price"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Re-enable
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexDisabled("Order$price")).To(BeTrue())

				changed, err := store.MarkIndexReadable("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify persisted
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails when range set is not complete", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Do NOT mark range set complete.
				_, err = store.MarkIndexReadable("Order$price")
				Expect(err).To(HaveOccurred())
				var builtErr *IndexNotBuiltError
				Expect(errors.As(err, &builtErr)).To(BeTrue())
				Expect(store.IsIndexDisabled("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails on unique index with violations", func() {
			ss := specSubspace()

			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Set up: disable index, mark range complete, add violations
				_, err = store.MarkIndexDisabled("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())

				idx := mdWithUnique.GetIndex("Order$unique_price")
				markRangeSetComplete(store, idx)
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(100)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				// MarkIndexReadable should fail due to violations
				_, err = store.MarkIndexReadable("Order$unique_price")
				Expect(err).To(HaveOccurred())
				var violationErr *RecordIndexUniquenessViolationError
				Expect(errors.As(err, &violationErr)).To(BeTrue())
				Expect(violationErr.IndexName).To(Equal("Order$unique_price"))

				// Index state should remain unchanged
				Expect(store.IsIndexDisabled("Order$unique_price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears build data on successful transition to READABLE", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$price")
				markRangeSetComplete(store, idx)

				// Verify range set is complete before MarkIndexReadable
				rangeSet := NewIndexingRangeSet(store.subspace, idx)
				complete, err := rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete).To(BeTrue())

				changed, err := store.MarkIndexReadable("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// Build data should be cleared after transition
				complete, err = rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				// After clearing, FirstMissingRange returns non-nil (whole range is missing)
				// so IsComplete returns false.
				Expect(complete).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ClearAndMarkIndexWriteOnly", func() {
		It("clears index data and sets write-only", func() {
			ss := specSubspace()

			// Save a record so the index has entries
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify index entry exists
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				idx := md.GetIndex("Order$price")
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(1))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Clear and mark write-only
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}

				changed, err := store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Mark readable (with range set complete) and verify index is empty (data was cleared)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				// Mark range set as complete so checkIndexBuilt passes.
				markRangeSetComplete(store, md.GetIndex("Order$price"))

				_, err = store.MarkIndexReadable("Order$price")
				if err != nil {
					return nil, err
				}
				idx := md.GetIndex("Order$price")
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Disabled index skips maintenance", func() {
		It("does not create index entries for disabled indexes", func() {
			ss := specSubspace()

			// Disable the index
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Save a record — should succeed but not create index entries
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Re-enable index (with range set complete) and scan — should be empty
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				// Mark range set as complete so checkIndexBuilt passes.
				markRangeSetComplete(store, md.GetIndex("Order$price"))

				_, err = store.MarkIndexReadable("Order$price")
				if err != nil {
					return nil, err
				}

				idx := md.GetIndex("Order$price")
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ScanIndex rejects non-readable index", func() {
		It("returns error when scanning disabled index", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				if err != nil {
					return nil, err
				}

				idx := md.GetIndex("Order$price")
				_, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).To(HaveOccurred())
				var notReadable *IndexNotReadableError
				Expect(errors.As(err, &notReadable)).To(BeTrue())
				Expect(notReadable.IndexName).To(Equal("Order$price"))
				Expect(notReadable.CurrentState).To(Equal(IndexStateDisabled))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error when scanning write-only index", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexWriteOnly("Order$price")
				if err != nil {
					return nil, err
				}

				idx := md.GetIndex("Order$price")
				_, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).To(HaveOccurred())
				var notReadable *IndexNotReadableError
				Expect(errors.As(err, &notReadable)).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Unknown index errors", func() {
		It("returns error for non-existent index", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("nonexistent")
				Expect(err).To(HaveOccurred())
				var notFound *IndexNotFoundError
				Expect(errors.As(err, &notFound)).To(BeTrue())
				Expect(notFound.IndexName).To(Equal("nonexistent"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MarkIndexReadableOrUniquePending", func() {
		It("marks non-unique index as READABLE when range set is complete", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Disable first, then mark readable-or-unique-pending
				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Mark range set as complete so checkIndexBuilt passes.
				markRangeSetComplete(store, md.GetIndex("Order$price"))

				changed, err := store.MarkIndexReadableOrUniquePending("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails when range set is not complete", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Do NOT mark range set complete.
				_, err = store.MarkIndexReadableOrUniquePending("Order$price")
				Expect(err).To(HaveOccurred())
				var builtErr *IndexNotBuiltError
				Expect(errors.As(err, &builtErr)).To(BeTrue())
				Expect(store.IsIndexDisabled("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("marks unique index with violations as READABLE_UNIQUE_PENDING", func() {
			ss := specSubspace()

			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}

				// Move to WRITE_ONLY first (simulates online index build in progress)
				_, err = store.MarkIndexWriteOnly("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())

				// Manually add a uniqueness violation entry
				idx := mdWithUnique.GetIndex("Order$unique_price")
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(100)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				// Mark range set as complete so checkIndexBuilt passes.
				markRangeSetComplete(store, idx)

				// Now mark it — should be READABLE_UNIQUE_PENDING
				changed, err := store.MarkIndexReadableOrUniquePending("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadableUniquePending))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("marks unique index without violations as READABLE", func() {
			ss := specSubspace()

			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Save records with distinct prices — no violations
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				if err != nil {
					return nil, err
				}
				// Disable then re-mark
				_, err = store.MarkIndexDisabled("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())

				// Mark range set as complete so checkIndexBuilt passes.
				idx := mdWithUnique.GetIndex("Order$unique_price")
				markRangeSetComplete(store, idx)

				changed, err := store.MarkIndexReadableOrUniquePending("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadable))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears build data on READABLE but not on READABLE_UNIQUE_PENDING", func() {
			ss := specSubspace()

			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			// Test READABLE_UNIQUE_PENDING: build data NOT cleared
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				_, err = store.MarkIndexWriteOnly("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())

				idx := mdWithUnique.GetIndex("Order$unique_price")
				markRangeSetComplete(store, idx)
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(100)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				changed, err := store.MarkIndexReadableOrUniquePending("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadableUniquePending))

				// Build data should NOT be cleared for READABLE_UNIQUE_PENDING
				rangeSet := NewIndexingRangeSet(store.subspace, idx)
				complete, err := rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IndexState string representation", func() {
		It("returns correct strings", func() {
			Expect(IndexStateReadable.String()).To(Equal("READABLE"))
			Expect(IndexStateWriteOnly.String()).To(Equal("WRITE_ONLY"))
			Expect(IndexStateDisabled.String()).To(Equal("DISABLED"))
			Expect(IndexStateReadableUniquePending.String()).To(Equal("READABLE_UNIQUE_PENDING"))
		})
	})

	Describe("OnlineIndexer READABLE_UNIQUE_PENDING end-to-end", func() {
		It("builds unique index with violations into READABLE_UNIQUE_PENDING", func() {
			ss := specSubspace()

			// Phase 1: Insert records with duplicate prices (no unique index yet).
			_, builder := func() (*RecordMetaData, *RecordMetaDataBuilder) {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				return nil, b
			}()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Two orders with same price = uniqueness violation
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				// One order with distinct price
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add unique index and build online.
			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			_, builder2 := func() (*RecordMetaData, *RecordMetaDataBuilder) {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				return nil, b
			}()
			builder2.AddIndex("Order", uniqueIdx)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(uniqueIdx).
				SetSubspace(ss).
				SetLimit(10).
				// READABLE_UNIQUE_PENDING is an OPT-IN outcome, not the default. Java
				// gates it on IndexingPolicy.shouldAllowUniquePendingState
				// (OnlineIndexer.java:1117), whose builder field defaults FALSE (:1220,
				// javadoc: "allow=false (default, backward compatible): throw an
				// exception"). Without this the build correctly FAILS instead.
				SetAllowUniquePendingState(true).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 3))

			// Phase 3: Verify index is READABLE_UNIQUE_PENDING (not READABLE).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}

				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadableUniquePending))
				Expect(store.IsIndexScannable("Order$unique_price")).To(BeTrue())

				// Index should be scannable even in READABLE_UNIQUE_PENDING.
				idx := mdWithIndex.GetIndex("Order$unique_price")
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// Violations should exist.
				violations, err := store.ScanUniquenessViolations(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).NotTo(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds unique index without violations into READABLE", func() {
			ss := specSubspace()

			// Phase 1: Insert records with distinct prices.
			_, builder := func() (*RecordMetaData, *RecordMetaDataBuilder) {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				return nil, b
			}()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build unique index online — no violations.
			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			_, builder2 := func() (*RecordMetaData, *RecordMetaDataBuilder) {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				return nil, b
			}()
			builder2.AddIndex("Order", uniqueIdx)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(uniqueIdx).
				SetSubspace(ss).
				SetLimit(10).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 3))

			// Phase 3: Verify index is READABLE (not READABLE_UNIQUE_PENDING).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}

				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadable))
				Expect(store.IsIndexReadable("Order$unique_price")).To(BeTrue())

				// Build data should be cleared for READABLE indexes.
				idx := mdWithIndex.GetIndex("Order$unique_price")
				rangeSet := NewIndexingRangeSet(store.subspace, idx)
				complete, err := rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete).To(BeFalse()) // cleared

				// No violations.
				violations, err := store.ScanUniquenessViolations(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("READABLE_UNIQUE_PENDING state persists across transactions", func() {
			ss := specSubspace()

			uniqueIdx := NewIndex("Order$unique_price", Field("price"))
			uniqueIdx.SetUnique()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			// Set up: WRITE_ONLY with violations, transition to READABLE_UNIQUE_PENDING
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexWriteOnly("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())

				idx := mdWithUnique.GetIndex("Order$unique_price")
				markRangeSetComplete(store, idx)
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(100)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				_, err = store.MarkIndexReadableOrUniquePending("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadableUniquePending))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify state persists in a new transaction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadableUniquePending))
				Expect(store.IsIndexScannable("Order$unique_price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$unique_price")).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("Replacement retirement", func() {
	var md *RecordMetaData
	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		original := NewIndex("original", Field("price"))
		original.Options = map[string]string{IndexOptionReplacedByPrefix + "0": "replacement1", IndexOptionReplacedByPrefix + "1": "replacement2"}
		builder.AddIndex("Order", original)
		builder.AddIndex("Order", NewIndex("replacement1", Field("price")))
		builder.AddIndex("Order", NewIndex("replacement2", Field("price")))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("stores full overlapping primary keys in violations and removes only resolved value groups", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("price"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		index := NewIndex("price", Field("price")).SetUnique()
		builder.AddIndex("Order", index)
		metadata, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(index.PrimaryKeyComponentPositions()).To(Equal([]int{0, -1}))
		_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexWriteOnly(index.Name)
			Expect(err).NotTo(HaveOccurred())
			for id, price := range []int32{100, 100, 100, 200, 200} {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(id + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}
			space := ss.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
			rows, err := rtx.Transaction().GetRange(space, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			var keys []tuple.Tuple
			for _, row := range rows {
				key, err := space.Unpack(row.Key)
				Expect(err).NotTo(HaveOccurred())
				keys = append(keys, key)
			}
			Expect(keys).To(ConsistOf(tuple.Tuple{int64(100), int64(100), int64(1)}, tuple.Tuple{int64(100), int64(100), int64(2)}, tuple.Tuple{int64(100), int64(100), int64(3)}, tuple.Tuple{int64(200), int64(200), int64(4)}, tuple.Tuple{int64(200), int64(200), int64(5)}))
			markRangeSetComplete(store, index)
			_, err = store.MarkIndexReadable(index.Name)
			var violation *RecordIndexUniquenessViolationError
			Expect(errors.As(err, &violation)).To(BeTrue())
			Expect(violation.IndexKey).To(Equal(tuple.Tuple{int64(100)}))
			Expect(violation.PrimaryKey).To(Equal(tuple.Tuple{int64(100), int64(1)}))
			Expect(violation.ExistingKey).To(BeElementOf(tuple.Tuple{int64(100), int64(2)}, tuple.Tuple{int64(100), int64(3)}))
			// The explicit low-level API removes just the named entry; ordinary
			// record deletion additionally clears the last remaining violation.
			Expect(store.ResolveUniquenessViolation(index, tuple.Tuple{int64(100)}, tuple.Tuple{int64(100), int64(3)})).To(Succeed())
			_, err = store.DeleteRecord(tuple.Tuple{int64(100), int64(3)})
			Expect(err).NotTo(HaveOccurred())
			violations, err := store.ScanUniquenessViolationsForValue(index, tuple.Tuple{int64(100)})
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(HaveLen(2))
			Expect(violations[0].PrimaryKey).To(Equal(tuple.Tuple{int64(100), int64(1)}))
			_, err = store.DeleteRecord(tuple.Tuple{int64(100), int64(2)})
			Expect(err).NotTo(HaveOccurred())
			violations, err = store.ScanUniquenessViolations(index)
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(HaveLen(2), "the separate value 200 must retain both violations")
			for _, violation := range violations {
				Expect(violation.IndexKey).To(Equal(tuple.Tuple{int64(200)}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("retains the original through real unique-pending replacement completion then retires after resolution", func() {
		md.GetIndex("replacement2").SetUnique()
		ss := specSubspace()
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{"replacement1", "replacement2"} {
				_, err := store.MarkIndexWriteOnly(name)
				Expect(err).NotTo(HaveOccurred())
			}
			for _, id := range []int64{1, 2} {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(store.RebuildIndex(md.GetIndex("original"))).To(Succeed())
			Expect(store.RebuildIndex(md.GetIndex("replacement1"))).To(Succeed())
			Expect(store.RebuildIndex(md.GetIndex("replacement2"))).To(Succeed())
			changed, err := store.MarkIndexReadableOrUniquePending("replacement2")
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			Expect(store.GetIndexState("replacement2")).To(Equal(IndexStateReadableUniquePending))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable))
			Expect(store.GetIndexState("replacement2")).To(Equal(IndexStateReadableUniquePending))
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			changed, err := store.MarkIndexReadable("replacement2")
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled))
			Expect(store.GetIndexState("replacement2")).To(Equal(IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, reverse := range []bool{false, true} {
		label := "old type"
		if reverse {
			label = "new type"
		}
		It("propagates state-read errors while partitioning cross-type indexes for the "+label, func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			rtx := NewFDBRecordContext(tx, nil)
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			saved, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			other := *saved
			other.RecordType = md.GetRecordType("Customer")
			tx.Cancel()
			if reverse {
				err = store.updateSecondaryIndexes(&other, saved)
			} else {
				err = store.updateSecondaryIndexes(saved, &other)
			}
			var canceled fdb.Error
			Expect(errors.As(err, &canceled)).To(BeTrue())
			Expect(canceled.Code).To(Equal(1025))
		})
	}

	It("uses transaction-visible write-only state for unique maintenance through another store object", func() {
		md.GetIndex("original").SetUnique()
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()
			first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{"replacement1", "replacement2"} {
				_, err := first.MarkIndexWriteOnly(name)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(first.RebuildIndex(md.GetIndex("original"))).To(Succeed())
			second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			_, err = first.MarkIndexWriteOnly("original")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.indexStates["original"]).To(Equal(IndexStateReadable), "the handle's open-time snapshot remains stale")
			for _, id := range []int64{1, 2} {
				_, err := second.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred(), "WRITE_ONLY records uniqueness violations instead of rejecting the write")
			}
			violations, err := second.ScanUniquenessViolations(md.GetIndex("original"))
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(HaveLen(2))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, lazy := range []bool{false, true} {
		for _, state := range []IndexState{IndexStateReadable, IndexStateWriteOnly, IndexStateDisabled} {
			It(fmt.Sprintf("build-state lookup propagates cancellation lazy=%v state=%s", lazy, state), func() {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := NewFDBRecordContext(tx, nil)
				builder := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace())
				store, err := builder.CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				store.setIndexState("original", state)
				if lazy {
					store, err = builder.Build()
					Expect(err).NotTo(HaveOccurred())
				}
				tx.Cancel()
				result, err := LoadIndexBuildState(store, md.GetIndex("original"))
				var canceled fdb.Error
				Expect(errors.As(err, &canceled)).To(BeTrue())
				Expect(canceled.Code).To(Equal(1025))
				Expect(result).To(BeNil())
			})
		}
	}

	for _, indexType := range []string{IndexTypeValue, IndexTypeRank, IndexTypeBitmapValue} {
		It(indexType+" maintainer propagates an index-state read error", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			rtx := NewFDBRecordContext(tx, nil)
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			index := NewIndex("original", Field("price"))
			index.Type = indexType
			maintainer, err := store.getIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			tx.Cancel()
			var canceled fdb.Error
			Expect(errors.As(maintainer.Update(nil, nil), &canceled)).To(BeTrue())
			Expect(canceled.Code).To(Equal(1025))
		})
	}

	for _, method := range []string{"value", "typed", "time-window", "vector-scan", "vector-search"} {
		scan := func(store *FDBRecordStore) RecordCursor[*IndexEntry] {
			index := md.GetIndex("original")
			switch method {
			case "vector-scan":
				return store.ScanVectorIndex(index, []float64{1, 0}, 1, 10, nil, ForwardScan())
			case "vector-search":
				_, err := store.SearchVectorIndex(index, []float64{1, 0}, 1, 10)
				if err != nil {
					return &errorCursor[*IndexEntry]{err: err}
				}
				return Empty[*IndexEntry]()
			case "typed":
				return store.ScanIndexByType(index, IndexScanByValue, TupleRangeAll, nil, ForwardScan())
			case "time-window":
				return store.ScanTimeWindowLeaderboard(index, IndexScanByRank, 0, 0, TupleRangeAll, nil, ForwardScan())
			default:
				return store.ScanIndex(index, TupleRangeAll, nil, ForwardScan())
			}
		}
		It(method+" scan checks transaction-visible state before dispatch", func() {
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				ss := specSubspace()
				first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				first.setIndexState("original", IndexStateReadable)
				second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				_, err = first.MarkIndexDisabled("original")
				Expect(err).NotTo(HaveOccurred())
				Expect(second.indexStates["original"]).To(Equal(IndexStateReadable), "the handle's open-time snapshot remains stale")
				_, err = AsList(context.Background(), scan(second))
				var unreadable *IndexNotReadableError
				Expect(errors.As(err, &unreadable)).To(BeTrue())
				Expect(unreadable.CurrentState).To(Equal(IndexStateDisabled))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
		It(method+" scan propagates index-state read failures", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			rtx := NewFDBRecordContext(tx, nil)
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			tx.Cancel()
			_, err = AsList(context.Background(), scan(store))
			var canceled fdb.Error
			Expect(errors.As(err, &canceled)).To(BeTrue())
			Expect(canceled.Code).To(Equal(1025))
		})
		It(method+" refused scan conflicts with a concurrently enabled index", func() {
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{"replacement1", "replacement2"} {
					_, err = store.MarkIndexWriteOnly(name)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			rtx := NewFDBRecordContext(tx, nil)
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			_, err = AsList(ctx, scan(store))
			var unreadable *IndexNotReadableError
			Expect(errors.As(err, &unreadable)).To(BeTrue())
			_, err = sharedDB.Run(ctx, func(builder *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(builder).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, store.RebuildIndex(md.GetIndex("original"))
			})
			Expect(err).NotTo(HaveOccurred())
			tx.Set(ss.Pack(tuple.Tuple{"refused-scan-sentinel"}), []byte("force commit conflict validation"))
			var conflict fdb.Error
			Expect(errors.As(rtx.Commit(), &conflict)).To(BeTrue())
			Expect(conflict.Code).To(Equal(1020))
		})
	}

	for _, addRelationship := range []bool{false, true} {
		label := "removing"
		if addRelationship {
			label = "adding"
		}
		It("honors "+label+" replacement options without changing the original index version", func() {
			metadata := func(replaced bool, version int) *RecordMetaData {
				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				original := NewIndex("original", Field("price"))
				if replaced {
					original.Options[IndexOptionReplacedByPrefix+"0"] = "replacement1"
					original.Options[IndexOptionReplacedByPrefix+"1"] = "replacement2"
				}
				builder.AddIndex("Order", original)
				builder.AddIndex("Order", NewIndex("replacement1", Field("price")))
				builder.AddIndex("Order", NewIndex("replacement2", Field("price")))
				builder.SetVersion(version)
				result, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				return result
			}
			before, after := metadata(!addRelationship, 3), metadata(addRelationship, 4)
			Expect(after.GetIndex("original").LastModifiedVersion).To(Equal(before.GetIndex("original").LastModifiedVersion))
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(before).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{"replacement1", "replacement2"} {
					_, err := store.MarkIndexWriteOnly(name)
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, store.RebuildIndex(before.GetIndex("original"))
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(after).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable), "option-only changes must not prematurely disable an established index")
				for _, name := range []string{"replacement1", "replacement2"} {
					Expect(store.RebuildIndex(after.GetIndex(name))).To(Succeed())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(after).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				want, count := IndexStateReadable, 1
				if addRelationship {
					want, count = IndexStateDisabled, 0
				}
				Expect(store.GetIndexState("original")).To(Equal(want))
				rows, err := reader.Transaction().GetRange(store.IndexSubspace(after.GetIndex("original")), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(count))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, completeWithOld := range []bool{false, true} {
		It(fmt.Sprintf("retirement uses canceled replacement metadata across one context, old completion=%t", completeWithOld), func() {
			p, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			after, err := RecordMetaDataFromProto(p)
			Expect(err).NotTo(HaveOccurred())
			after.version++
			delete(after.GetIndex("original").Options, IndexOptionReplacedByPrefix+"0")
			delete(after.GetIndex("original").Options, IndexOptionReplacedByPrefix+"1")
			Expect(after.GetIndex("original").GetReplacedByIndexNames()).To(BeEmpty())
			Expect(after.GetIndex("original").LastModifiedVersion).To(Equal(md.GetIndex("original").LastModifiedVersion))
			ss := specSubspace()
			ctx := context.Background()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{"replacement1", "replacement2"} {
					_, err := store.MarkIndexWriteOnly(name)
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, store.RebuildIndex(md.GetIndex("original"))
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				old, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(old.RebuildIndex(md.GetIndex("replacement1"))).To(Succeed())
				current, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(after).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				if completeWithOld {
					current = old
				}
				Expect(current.RebuildIndex(current.metaData.GetIndex("replacement2"))).To(Succeed())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(after).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable))
				rows, err := rtx.Transaction().GetRange(store.IndexSubspace(after.GetIndex("original")), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(1))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("retires the original when the last chunked online replacement completes", func() {
		ss := specSubspace()
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{"replacement1", "replacement2"} {
				_, err := store.ClearAndMarkIndexWriteOnly(name)
				Expect(err).NotTo(HaveOccurred())
			}
			for id := int64(1); id <= 5; id++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, store.RebuildIndex(md.GetIndex("original"))
		})
		Expect(err).NotTo(HaveOccurred())
		for i, name := range []string{"replacement1", "replacement2"} {
			indexer, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).
				SetIndex(md.GetIndex(name)).SetSubspace(ss).SetLimit(2).Build()
			Expect(err).NotTo(HaveOccurred())
			processed, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(processed).To(BeNumerically(">=", 5), "idempotent builds may rescan boundary rows")
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.storeHeader.GetMetaDataversion()).To(Equal(int32(md.Version())))
				want := IndexStateReadable
				if i == 1 {
					want = IndexStateDisabled
				}
				Expect(store.GetIndexState("original")).To(Equal(want))
				Expect(store.GetIndexState(name)).To(Equal(IndexStateReadable))
				entries, err := AsList(ctx, store.ScanIndex(md.GetIndex(name), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))
				for n, entry := range entries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((n + 1) * 100)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	for _, firstName := range []string{"replacement1", "replacement2"} {
		It("retries overlapping replacement completion when "+firstName+" commits first", func() {
			secondName := "replacement2"
			if firstName == secondName {
				secondName = "replacement1"
			}
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{firstName, secondName} {
					_, err := store.MarkIndexWriteOnly(name)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, store.RebuildIndex(md.GetIndex("original"))
			})
			Expect(err).NotTo(HaveOccurred())
			first, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer first.Cancel()
			second, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer second.Cancel()
			version, err := first.GetReadVersion().Get()
			Expect(err).NotTo(HaveOccurred())
			second.SetReadVersion(version)
			firstContext := NewFDBRecordContext(first, nil)
			secondContext := NewFDBRecordContext(second, nil)
			complete := func(rtx *FDBRecordContext, name string) error {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return err
				}
				markRangeSetComplete(store, md.GetIndex(name))
				_, err = store.MarkIndexReadable(name)
				return err
			}
			Expect(complete(firstContext, firstName)).To(Succeed())
			Expect(complete(secondContext, secondName)).To(Succeed())
			Expect(firstContext.Commit()).To(Succeed())
			var conflict fdb.Error
			Expect(errors.As(secondContext.Commit(), &conflict)).To(BeTrue())
			Expect(conflict.Code).To(Equal(1020))
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) { return nil, complete(rtx, secondName) })
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled))
				Expect(store.GetIndexState(firstName)).To(Equal(IndexStateReadable))
				Expect(store.GetIndexState(secondName)).To(Equal(IndexStateReadable))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, enable := range []bool{false, true} {
		for _, builderFirst := range []bool{false, true} {
			label := "retirement"
			if enable {
				label = "enabling a skipped index"
			}
			order := "writer first"
			if builderFirst {
				order = "builder first"
			}
			It("serializes stale writers against "+label+" with "+order, func() {
				ss := specSubspace()
				ctx := context.Background()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for _, name := range []string{"replacement1", "replacement2"} {
						_, err = store.MarkIndexWriteOnly(name)
						Expect(err).NotTo(HaveOccurred())
					}
					if !enable {
						return nil, store.RebuildIndex(md.GetIndex("original"))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				writer, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer writer.Cancel()
				builder, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer builder.Cancel()
				version, err := writer.GetReadVersion().Get()
				Expect(err).NotTo(HaveOccurred())
				builder.SetReadVersion(version)
				writeContext := NewFDBRecordContext(writer, nil)
				buildContext := NewFDBRecordContext(builder, nil)
				save := func(rtx *FDBRecordContext) error {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return err
					}
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
					return err
				}
				build := func(rtx *FDBRecordContext) error {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return err
					}
					if enable {
						return store.RebuildIndex(md.GetIndex("original"))
					}
					for _, name := range []string{"replacement1", "replacement2"} {
						markRangeSetComplete(store, md.GetIndex(name))
						_, err := store.MarkIndexReadable(name)
						if err != nil {
							return err
						}
					}
					return nil
				}
				Expect(save(writeContext)).To(Succeed())
				Expect(build(buildContext)).To(Succeed())
				var conflict fdb.Error
				if builderFirst {
					Expect(buildContext.Commit()).To(Succeed())
					Expect(errors.As(writeContext.Commit(), &conflict)).To(BeTrue())
					Expect(conflict.Code).To(Equal(1020))
					_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) { return nil, save(rtx) })
				} else {
					Expect(writeContext.Commit()).To(Succeed())
					if enable {
						Expect(errors.As(buildContext.Commit(), &conflict)).To(BeTrue())
						Expect(conflict.Code).To(Equal(1020))
						_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) { return nil, build(rtx) })
					} else {
						Expect(buildContext.Commit()).To(Succeed())
					}
				}
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
					for _, index := range md.GetAllIndexes() {
						rows, err := reader.Transaction().GetRange(ss.Sub(IndexKey, index.SubspaceTupleKey()), fdb.RangeOptions{}).GetSliceWithError()
						Expect(err).NotTo(HaveOccurred())
						want := 1
						if index.Name == "original" && !enable {
							want = 0
						}
						Expect(rows).To(HaveLen(want), index.Name)
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}

	It("clears former index state by name and leaves Java build-lock data intact", func() {
		ss := specSubspace()
		ctx := context.Background()
		former := &FormerIndex{FormerName: "original", SubspaceKey: int64(991)}
		dataKey := ss.Sub(IndexKey, former.SubspaceKey).Pack(tuple.Tuple{"obsolete"})
		lockKey := ss.Sub(IndexBuildSpaceKey, former.SubspaceKey, int64(0)).Pack(tuple.Tuple{"owner"})
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexWriteOnly(former.FormerName)
			Expect(err).NotTo(HaveOccurred())
			rtx.Transaction().Set(lockKey, []byte("java-builder"))
			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, dataKey, make([]byte, 14))
			Expect(store.removeFormerIndexData(former)).To(Succeed())
			Expect(store.GetIndexState(former.FormerName)).To(Equal(IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
			state, err := reader.Transaction().Get(ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{former.FormerName})).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(BeNil())
			data, err := reader.Transaction().Get(dataKey).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(data).To(BeNil())
			lock, err := reader.Transaction().Get(lockKey).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(lock).To(Equal([]byte("java-builder")))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, operation := range []string{"disable", "restart", "former"} {
		It(operation+" clears deferred index data with exact Java prefix boundaries", func() {
			ss := specSubspace()
			index := md.GetIndex("original")
			spaces := []int64{IndexKey, IndexSecondarySpaceKey, IndexSlidingWindowSpaceKey, IndexRangeSpaceKey, IndexUniquenessViolationsKey}
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				store.setIndexState(index.Name, IndexStateReadable)
				for _, space := range spaces {
					prefix := ss.Sub(space, index.SubspaceTupleKey())
					for _, key := range [][]byte{prefix.Bytes(), prefix.Pack(tuple.Tuple{"child"})} {
						rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, make([]byte, 14))
						rtx.AddToLocalVersionCache(key, 1)
					}
				}
				switch operation {
				case "disable":
					_, err = store.MarkIndexDisabled(index.Name)
				case "restart":
					_, err = store.ClearAndMarkIndexWriteOnly(index.Name)
				default:
					err = store.removeFormerIndexData(&FormerIndex{FormerName: index.Name, SubspaceKey: index.SubspaceTupleKey()})
				}
				Expect(err).NotTo(HaveOccurred())
				for _, space := range spaces {
					prefix := ss.Sub(space, index.SubspaceTupleKey())
					_, present := rtx.GetLocalVersion(prefix.Bytes())
					Expect(present).To(Equal(space != IndexKey))
					_, present = rtx.GetLocalVersion(prefix.Pack(tuple.Tuple{"child"}))
					Expect(present).To(BeFalse())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				for _, space := range spaces {
					prefix := ss.Sub(space, index.SubspaceTupleKey())
					root, err := reader.Transaction().Get(fdb.Key(prefix.Bytes())).Get()
					Expect(err).NotTo(HaveOccurred())
					if space == IndexKey {
						Expect(root).To(BeNil(), "ungrouped aggregate prefix is included")
					} else {
						Expect(root).To(HaveLen(10), "tuple-subspace ranges exclude their exact prefix")
					}
					child, err := reader.Transaction().Get(prefix.Pack(tuple.Tuple{"child"})).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(child).To(BeNil())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, operation := range []string{"disable", "restart", "erase-build-data"} {
		It(operation+" preserves build locks while clearing Java bookkeeping and deferred versions", func() {
			ss := specSubspace()
			index := md.GetIndex("original")
			buildSpace := ss.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey())
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				store.setIndexState(index.Name, IndexStateReadable)
				for part := int64(0); part <= 10; part++ {
					root := buildSpace.Pack(tuple.Tuple{part})
					child := buildSpace.Pack(tuple.Tuple{part, "child"})
					rtx.Transaction().Set(root, []byte("existing"))
					rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, child, make([]byte, 14))
					rtx.AddToLocalVersionCache(child, 1)
				}
				switch operation {
				case "disable":
					_, err = store.MarkIndexDisabled(index.Name)
				case "restart":
					_, err = store.ClearAndMarkIndexWriteOnly(index.Name)
				default:
					err = store.eraseAllIndexingDataButTheLockAndRangeSet(index)
				}
				Expect(err).NotTo(HaveOccurred())
				for part := int64(0); part <= 10; part++ {
					_, present := rtx.GetLocalVersion(buildSpace.Pack(tuple.Tuple{part, "child"}))
					Expect(present).To(Equal(part == 0 || part == 10), "only lock and unknown future subspaces survive")
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				for part := int64(0); part <= 10; part++ {
					root, err := reader.Transaction().Get(buildSpace.Pack(tuple.Tuple{part})).Get()
					Expect(err).NotTo(HaveOccurred())
					child, err := reader.Transaction().Get(buildSpace.Pack(tuple.Tuple{part, "child"})).Get()
					Expect(err).NotTo(HaveOccurred())
					if part == 0 || part == 10 {
						Expect(root).To(Equal([]byte("existing")))
						Expect(child).To(HaveLen(10))
					} else {
						Expect(root).To(BeNil())
						Expect(child).To(BeNil(), "a deferred version must not resurrect cleared bookkeeping")
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, createOnly := range []bool{false, true} {
		for _, desired := range []IndexState{IndexStateReadable, IndexStateWriteOnly, IndexStateDisabled} {
			label := "CreateOrOpen"
			if createOnly {
				label = "Create"
			}
			It(label+" initializes replaced originals as disabled before consulting "+desired.String()+" policy", func() {
				ss := specSubspace()
				var calls []string
				_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
					calls = nil
					builder := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
						SetIndexRebuildPolicy(func(index *Index, count int64, newTypes bool) IndexState {
							calls = append(calls, index.Name)
							Expect(count).To(BeZero())
							Expect(newTypes).To(BeTrue())
							return desired
						})
					var store *FDBRecordStore
					var err error
					if createOnly {
						store, err = builder.Create()
					} else {
						store, err = builder.CreateOrOpen()
					}
					Expect(err).NotTo(HaveOccurred())
					Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled))
					Expect(calls).To(ConsistOf("replacement1", "replacement2"))
					want := IndexStateReadable
					if desired == IndexStateDisabled {
						want = desired
					}
					for _, name := range []string{"replacement1", "replacement2"} {
						Expect(store.GetIndexState(name)).To(Equal(want), "new-store empty shortcut")
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}

	for _, changeOriginal := range []bool{false, true} {
		label := "retires an unchanged original immediately after metadata reconciliation"
		if changeOriginal {
			label = "disables a changed original before consulting reconciliation policy"
		}
		It(label, func() {
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				// Seed the pre-upgrade state: an established original still readable.
				store.setIndexState("original", IndexStateReadable)
				if changeOriginal {
					for _, name := range []string{"replacement1", "replacement2"} {
						_, err := store.MarkIndexWriteOnly(name)
						Expect(err).NotTo(HaveOccurred())
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			for _, index := range md.GetAllIndexes() {
				if changeOriginal && index.Name == "original" {
					changed := NewIndex("original", Field("price"))
					changed.Options = index.Options
					changed.AddedVersion = index.AddedVersion
					changed.LastModifiedVersion = md.Version() + 1
					builder.AddIndex("Order", changed)
				} else {
					builder.AddIndex("Order", index)
				}
			}
			if !changeOriginal {
				builder.AddIndex("Order", NewIndex("extra", Field("price")))
			}
			next, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				var calls []string
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(next).SetSubspace(ss).
					SetIndexRebuildPolicy(func(index *Index, _ int64, _ bool) IndexState {
						calls = append(calls, index.Name)
						return IndexStateWriteOnly
					}).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled), "must already be disabled before precommit")
				if changeOriginal {
					Expect(calls).To(BeEmpty())
					Expect(next.GetIndexesToBuildSince(md.Version())).To(BeEmpty())
				} else {
					Expect(calls).To(ConsistOf("extra"))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("excludes replaced originals from metadata and store build eligibility", func() {
		Expect(md.GetIndexesSince(-1)).To(ConsistOf(md.GetIndex("original"), md.GetIndex("replacement1"), md.GetIndex("replacement2")))
		Expect(md.GetIndexesSince(md.Version())).To(BeEmpty())
		Expect(md.GetIndexesToBuildSince(-1)).To(ConsistOf(md.GetIndex("replacement1"), md.GetIndex("replacement2")))
		Expect(md.GetIndexesToBuildSince(md.Version())).To(BeEmpty())
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexesToBuildSince(-1)).To(ConsistOf(md.GetIndex("replacement1"), md.GetIndex("replacement2")))
			for _, name := range []string{"replacement1", "replacement2"} {
				_, err := store.MarkIndexWriteOnly(name)
				Expect(err).NotTo(HaveOccurred())
			}
			eligible, err := store.GetIndexesToBuild()
			Expect(err).NotTo(HaveOccurred())
			Expect(eligible).To(ConsistOf(md.GetIndex("replacement1"), md.GetIndex("replacement2")))
			Expect(store.RebuildAllIndexes()).To(Succeed())
			eligible, err = store.GetIndexesToBuild()
			Expect(err).NotTo(HaveOccurred())
			Expect(eligible).To(BeEmpty())
			Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled), "build-all must not resurrect the original even before precommit")
			for _, name := range []string{"replacement1", "replacement2"} {
				Expect(store.GetIndexState(name)).To(Equal(IndexStateReadable))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("uses transaction-visible replacement states across store objects and deduplicates retirement", func() {
		ss := specSubspace()
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{"replacement1", "replacement2"} {
				_, err = first.MarkIndexWriteOnly(name)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(first.RebuildIndex(md.GetIndex("original"))).To(Succeed())
			second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(first.RebuildIndex(md.GetIndex("replacement1"))).To(Succeed())
			Expect(second.RebuildIndex(md.GetIndex("replacement2"))).To(Succeed())
			Expect(first.indexStates["replacement2"]).To(Equal(IndexStateWriteOnly), "the handle's open-time snapshot remains stale")
			Expect(first.GetIndexState("replacement2")).To(Equal(IndexStateReadable), "public state uses the context's current view")
			Expect(rtx.namedCommitChecks).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not treat a missing replacement as implicitly readable", func() {
		md.GetIndex("original").Options[IndexOptionReplacedByPrefix+"2"] = "not_defined"
		ss := specSubspace()
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			return nil, store.RebuildIndex(md.GetIndex("original"))
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, state := range []IndexState{IndexStateWriteOnly, IndexStateDisabled, IndexStateReadableUniquePending} {
		It("does not retire when a replacement is "+state.String(), func() {
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				store.setIndexState("replacement2", state)
				return nil, store.RebuildIndex(md.GetIndex("original"))
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable))
				Expect(store.GetIndexState("replacement2")).To(Equal(state))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, scheduleNew := range []bool{false, true} {
		label := "without new retirement"
		if scheduleNew {
			label = "with new replacement metadata"
		}
		It("recreates a deleted store "+label+" without reusing its old callback", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			original := NewIndex("original", Field("price"))
			if scheduleNew {
				original.Options[IndexOptionReplacedByPrefix+"0"] = "replacement2"
			}
			builder.AddIndex("Order", original)
			builder.AddIndex("Order", NewIndex("replacement1", Field("price")))
			builder.AddIndex("Order", NewIndex("replacement2", Field("price")))
			next, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			ss := specSubspace()
			ctx := context.Background()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				old, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				Expect(old.RebuildIndex(md.GetIndex("original"))).To(Succeed())
				Expect(DeleteStore(rtx, ss)).To(Succeed())
				fresh, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(next).SetSubspace(ss).Create()
				Expect(err).NotTo(HaveOccurred())
				if scheduleNew {
					_, err := fresh.MarkIndexWriteOnly("replacement1")
					Expect(err).NotTo(HaveOccurred())
					Expect(fresh.RebuildIndex(original)).To(Succeed())
				}
				_, err = fresh.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(next).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				want := IndexStateReadable
				rowsWanted := 1
				if scheduleNew {
					want, rowsWanted = IndexStateDisabled, 0
				}
				Expect(store.GetIndexState("original")).To(Equal(want))
				rows, err := reader.Transaction().GetRange(store.IndexSubspace(original), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(rowsWanted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("cancels retirement only for the deleted subspace", func() {
		ss := specSubspace()
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			for _, name := range []string{"removed", "retained"} {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss.Sub(name)).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.RebuildIndex(md.GetIndex("original"))).To(Succeed())
			}
			Expect(rtx.namedCommitChecks).To(HaveLen(2))
			Expect(DeleteStore(rtx, ss.Sub("removed"))).To(Succeed())
			Expect(rtx.namedCommitChecks).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
			rows, err := reader.Transaction().GetRange(ss.Sub("removed"), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss.Sub("retained")).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("original")).To(Equal(IndexStateDisabled))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, duringCommit := range []bool{false, true} {
		label := "before commit"
		if duringCommit {
			label = "from an earlier commit check"
		}
		It("does not resurrect deleted stores "+label, func() {
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				if duringCommit {
					rtx.AddCommitCheck(func() error { return DeleteStore(rtx, ss) })
				}
				Expect(store.RebuildIndex(md.GetIndex("original"))).To(Succeed())
				Expect(rtx.getCommitCheck(replacementRetirementCheckName(ss))).NotTo(BeNil())
				if !duringCommit {
					return nil, DeleteStore(rtx, ss)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
				rows, err := reader.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
				Expect(rows).To(BeEmpty(), "retirement must not restore a DISABLED state key after deletion")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, method := range []string{"plain", "hooks", "versionstamp"} {
		It(method+" retires only after the last replacement is readable, without a metadata change", func() {
			ss := specSubspace()
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{"replacement1", "replacement2"} {
					_, err = store.MarkIndexWriteOnly(name)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, store.RebuildIndex(md.GetIndex("original"))
			})
			Expect(err).NotTo(HaveOccurred())
			for i, name := range []string{"replacement1", "replacement2"} {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := NewFDBRecordContext(tx, nil)
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable))
				Expect(store.RebuildIndex(md.GetIndex(name))).To(Succeed())
				Expect(store.GetIndexState("original")).To(Equal(IndexStateReadable), "retirement is deferred to precommit")
				switch method {
				case "plain":
					err = rtx.Commit()
				case "hooks":
					err = rtx.CommitWithHooks()
				default:
					_, err = rtx.CommitWithVersionstamp()
				}
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(ss).Open()
					Expect(err).NotTo(HaveOccurred())
					want := IndexStateReadable
					if i == 1 {
						want = IndexStateDisabled
					}
					Expect(store.GetIndexState("original")).To(Equal(want))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	}
})

var _ = Describe("Transactional function selection", func() {
	for _, kind := range []string{"record", "aggregate"} {
		for _, explicit := range []bool{false, true} {
			for _, warm := range []bool{false, true} {
				It(fmt.Sprintf("refused %s explicit=%t warm=%t conflicts with enable", kind, explicit, warm), func() {
					ctx := context.Background()
					ss := specSubspace()
					b := baseBuilder()
					index := NewCountIndex("function", Ungrouped(EmptyKey()))
					if kind == "record" {
						index = NewRankIndex("function", GroupBy(Field("price")))
					}
					b.AddIndex("Order", index)
					md, err := b.Build()
					Expect(err).NotTo(HaveOccurred())
					cache := NewMetaDataVersionStampStoreStateCache()
					open := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
						return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetStoreStateCache(cache).Open()
					}
					_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
						Expect(err).NotTo(HaveOccurred())
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
						Expect(err).NotTo(HaveOccurred())
						_, err = store.MarkIndexDisabled(index.Name)
						Expect(err).NotTo(HaveOccurred())
						_, err = store.SetStateCacheability(warm)
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
					if warm {
						_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) { _, err := open(rtx); return nil, err })
						Expect(err).NotTo(HaveOccurred())
					}
					tx, err := sharedDB.CreateTransaction()
					Expect(err).NotTo(HaveOccurred())
					defer tx.Cancel()
					reader := sharedDB.NewRecordContext(tx)
					store, err := open(reader)
					Expect(err).NotTo(HaveOccurred())
					record, err := store.LoadRecord(tuple.Tuple{int64(1)})
					Expect(err).NotTo(HaveOccurred())
					Expect(record).NotTo(BeNil())
					name := ""
					if explicit {
						name = index.Name
					}
					if kind == "record" {
						_, err = store.EvaluateRecordFunction(&IndexRecordFunction{Name: FunctionNameRank, Operand: GroupBy(Field("price")), Index: name}, record)
					} else {
						_, err = store.EvaluateAggregateFunction(ctx, []string{"Order"}, &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey()), Index: name}, TupleRangeAll, SnapshotIsolation)
					}
					Expect(err).To(HaveOccurred(), "the only candidate is disabled")
					_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := open(rtx)
						Expect(err).NotTo(HaveOccurred())
						return nil, store.RebuildIndex(index)
					})
					Expect(err).NotTo(HaveOccurred())
					tx.Set(ss.Pack(tuple.Tuple{"selection-sentinel"}), []byte("force conflict validation"))
					var conflict fdb.Error
					Expect(errors.As(reader.Commit(), &conflict)).To(BeTrue())
					Expect(conflict.Code).To(Equal(1020))
				})
			}
			It(fmt.Sprintf("%s explicit=%t observes other handles and propagates cancellation", kind, explicit), func() {
				b := baseBuilder()
				var index *Index
				if kind == "record" {
					index = NewRankIndex("function", GroupBy(Field("price")))
				} else {
					index = NewCountIndex("function", Ungrouped(EmptyKey()))
				}
				b.AddIndex("Order", index)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := sharedDB.NewRecordContext(tx)
				first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				record, err := first.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).Open()
				Expect(err).NotTo(HaveOccurred())
				name := ""
				if explicit {
					name = index.Name
				}
				evaluate := func() error {
					if kind == "record" {
						value, err := second.EvaluateRecordFunction(&IndexRecordFunction{Name: FunctionNameRank, Operand: GroupBy(Field("price")), Index: name}, record)
						if err == nil {
							Expect(value).NotTo(BeNil())
							Expect(*value).To(Equal(int64(0)))
						}
						return err
					}
					value, err := second.EvaluateAggregateFunction(context.Background(), []string{"Order"}, &IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey()), Index: name}, TupleRangeAll, SnapshotIsolation)
					if err == nil {
						Expect(value).To(Equal(tuple.Tuple{int64(1)}))
					}
					return err
				}
				Expect(evaluate()).To(Succeed())
				_, err = first.MarkIndexDisabled(index.Name)
				Expect(err).NotTo(HaveOccurred())
				Expect(second.indexStates[index.Name]).To(Equal(IndexStateReadable), "open-time snapshot is stale")
				Expect(evaluate()).NotTo(Succeed(), "selection must reject the now-cleared index")
				tx.Cancel()
				var canceled fdb.Error
				Expect(errors.As(evaluate(), &canceled)).To(BeTrue())
				Expect(canceled.Code).To(Equal(1025))
			})
		}
	}
})

var _ = Describe("Bulk deletion state conflicts", func() {
	It("clears a rebuilt index through an older handle in the same context", func() {
		ctx := context.Background()
		ss := specSubspace()
		b := baseBuilder()
		index := NewIndex("aligned", Field("order_id"))
		b.AddIndex("Order", index)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			builder, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			_, err = builder.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = builder.MarkIndexDisabled(index.Name)
			Expect(err).NotTo(HaveOccurred())
			deleter, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(builder.RebuildIndex(index)).To(Succeed())
			Expect(deleter.indexStates[index.Name]).To(Equal(IndexStateDisabled))
			rows, err := rtx.Transaction().GetRange(builder.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			return nil, deleter.DeleteRecordsWhere(tuple.Tuple{int64(1)})
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			record, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(record).To(BeNil())
			rows, err := rtx.Transaction().GetRange(store.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	for _, warm := range []bool{false, true} {
		for _, rebuildFirst := range []bool{false, true} {
			It(fmt.Sprintf("serializes deletion against rebuild, warm=%t rebuildFirst=%t", warm, rebuildFirst), func() {
				b := baseBuilder()
				index := NewIndex("aligned", Field("order_id"))
				b.AddIndex("Order", index)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				ss := specSubspace()
				ctx := context.Background()
				cache := NewMetaDataVersionStampStoreStateCache()
				open := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
					builder := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss)
					if warm {
						builder.SetStoreStateCache(cache)
					}
					return builder.Open()
				}
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
					Expect(err).NotTo(HaveOccurred())
					_, err = store.MarkIndexDisabled(index.Name)
					Expect(err).NotTo(HaveOccurred())
					_, err = store.SetStateCacheability(warm)
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				if warm {
					_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) { _, err := open(rtx); return nil, err })
					Expect(err).NotTo(HaveOccurred())
				}
				deletion, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer deletion.Cancel()
				rebuild, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer rebuild.Cancel()
				version, err := deletion.GetReadVersion().Get()
				Expect(err).NotTo(HaveOccurred())
				rebuild.SetReadVersion(version)
				deleteContext, buildContext := sharedDB.NewRecordContext(deletion), sharedDB.NewRecordContext(rebuild)
				deleter, err := open(deleteContext)
				Expect(err).NotTo(HaveOccurred())
				builder, err := open(buildContext)
				Expect(err).NotTo(HaveOccurred())
				Expect(builder.RebuildIndex(index)).To(Succeed())
				Expect(deleter.DeleteRecordsWhere(tuple.Tuple{int64(1)})).To(Succeed())
				var conflict fdb.Error
				if rebuildFirst {
					Expect(buildContext.Commit()).To(Succeed())
					Expect(errors.As(deleteContext.Commit(), &conflict)).To(BeTrue())
				} else {
					Expect(deleteContext.Commit()).To(Succeed())
					Expect(errors.As(buildContext.Commit(), &conflict)).To(BeTrue())
				}
				Expect(conflict.Code).To(Equal(1020))
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := open(rtx)
					Expect(err).NotTo(HaveOccurred())
					record, err := store.LoadRecord(tuple.Tuple{int64(1)})
					Expect(err).NotTo(HaveOccurred())
					rows, err := rtx.Transaction().GetRange(store.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					if rebuildFirst {
						Expect(record).NotTo(BeNil())
						Expect(rows).To(HaveLen(1))
						Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
					} else {
						Expect(record).To(BeNil())
						Expect(rows).To(BeEmpty())
						Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateDisabled))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})

var _ = Describe("SPFresh transactional search state", func() {
	It("returns live results, rejects another handle's disable and propagates cancellation", func() {
		b := baseBuilder()
		index := NewIndex("spf_state", Concat(Field("price"), Field("quantity")))
		index.Type = IndexTypeVectorSPFresh
		index.Options = map[string]string{IndexOptionSPFreshNumDimensions: "2"}
		b.AddIndex("Order", index)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		rtx := sharedDB.NewRecordContext(tx)
		ss := specSubspace()
		first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
		Expect(err).NotTo(HaveOccurred())
		_, err = first.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(20)})
		Expect(err).NotTo(HaveOccurred())
		second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		Expect(err).NotTo(HaveOccurred())
		rows, err := SearchSPFreshIndex(second, index.Name, []float64{10, 20}, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
		_, err = first.MarkIndexDisabled(index.Name)
		Expect(err).NotTo(HaveOccurred())
		_, err = SearchSPFreshIndex(second, index.Name, []float64{10, 20}, 1)
		var unreadable *IndexNotReadableError
		Expect(errors.As(err, &unreadable)).To(BeTrue())
		Expect(unreadable.CurrentState).To(Equal(IndexStateDisabled))
		tx.Cancel()
		_, err = SearchSPFreshIndex(second, index.Name, []float64{10, 20}, 1)
		var canceled fdb.Error
		Expect(errors.As(err, &canceled)).To(BeTrue())
		Expect(canceled.Code).To(Equal(1025))
	})
})

// stateWriteBarrier delegates to real FDB and pauses one completed state write
// so the publication order can be exercised without scheduler timing guesses.
type stateWriteBarrier struct {
	fdb.WritableTransaction
	key     []byte
	paused  atomic.Bool
	written chan struct{}
	resume  chan struct{}
}

func (tx *stateWriteBarrier) Set(key fdb.KeyConvertible, value []byte) {
	tx.WritableTransaction.Set(key, value)
	if bytes.Equal(key.FDBKey(), tx.key) && tx.paused.CompareAndSwap(false, true) {
		close(tx.written)
		<-tx.resume
	}
}

func (tx *stateWriteBarrier) ClearRange(keyRange fdb.ExactRange) {
	tx.WritableTransaction.ClearRange(keyRange)
	begin, end := keyRange.FDBRangeKeys()
	if bytes.Compare(tx.key, begin.FDBKey()) >= 0 && bytes.Compare(tx.key, end.FDBKey()) < 0 && tx.paused.CompareAndSwap(false, true) {
		close(tx.written)
		<-tx.resume
	}
}

type maintenanceStateBarrier struct{ *stateWriteBarrier }

func (tx *maintenanceStateBarrier) Clear(key fdb.KeyConvertible) {
	tx.WritableTransaction.Clear(key)
	if bytes.Equal(key.FDBKey(), tx.key) && tx.paused.CompareAndSwap(false, true) {
		close(tx.written)
		<-tx.resume
	}
}

var _ = Describe("Shared index-state lifecycle coherence", func() {
	for _, twoHandles := range []bool{false, true} {
		It(fmt.Sprintf("holds the shared maintenance gate during DELETE_WHERE twoHandles=%t", twoHandles), func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			ss := specSubspace()
			barrier := &stateWriteBarrier{WritableTransaction: tx, written: make(chan struct{}), resume: make(chan struct{})}
			barrier.paused.Store(true)
			rtx := NewFDBRecordContext(barrier, nil)
			b := baseBuilder()
			index := NewIndex("state", Field("order_id"))
			b.AddIndex("Order", index)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			writer, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			for id := int64(1); id <= 2; id++ {
				_, err := writer.SaveRecord(&gen.Order{OrderId: proto.Int64(id)})
				Expect(err).NotTo(HaveOccurred())
			}
			setter := writer
			if twoHandles {
				setter, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
			}
			rows, err := tx.GetRange(writer.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(2))
			barrier.key = rows[0].Key
			barrier.paused.Store(false)
			defer func() {
				select {
				case <-barrier.resume:
				default:
					close(barrier.resume)
				}
			}()
			done := make(chan error, 1)
			go func() { done <- writer.DeleteRecordsWhere(tuple.Tuple{int64(1)}) }()
			Eventually(barrier.written, "5s").Should(BeClosed())
			unprotected := setter.indexStateView.maintenance.TryLock()
			if unprotected {
				setter.indexStateView.maintenance.Unlock()
			}
			transition := make(chan error, 1)
			go func() { _, err := setter.MarkIndexDisabled("state"); transition <- err }()
			close(barrier.resume)
			Eventually(done, "5s").Should(Receive(BeNil()))
			Eventually(transition, "5s").Should(Receive(BeNil()))
			Expect(unprotected).To(BeFalse())
			remaining, err := writer.LoadRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(remaining).NotTo(BeNil())
			removed, err := writer.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeNil())
		})
	}
	for _, pending := range []bool{false, true} {
		for _, twoHandles := range []bool{false, true} {
			It(fmt.Sprintf("holds the exclusive maintenance gate through readable publication pending=%t twoHandles=%t", pending, twoHandles), func() {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				ss := specSubspace()
				barrier := &maintenanceStateBarrier{&stateWriteBarrier{WritableTransaction: tx, key: ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"state"}), written: make(chan struct{}), resume: make(chan struct{})}}
				barrier.paused.Store(true)
				rtx := NewFDBRecordContext(barrier, nil)
				b := baseBuilder()
				index := NewIndex("state", Field("price")).SetUnique()
				b.AddIndex("Order", index)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				setter, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				Expect(err).NotTo(HaveOccurred())
				_, err = setter.MarkIndexWriteOnly("state")
				Expect(err).NotTo(HaveOccurred())
				if pending {
					Expect(setter.AddUniquenessViolation(index, tuple.Tuple{int64(10)}, tuple.Tuple{int64(99)})).To(Succeed())
				}
				writer := setter
				if twoHandles {
					writer, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(writer.ensureStoreStateLoadedErr()).To(Succeed())
				barrier.paused.Store(false)
				defer func() {
					select {
					case <-barrier.resume:
					default:
						close(barrier.resume)
					}
				}()
				transition := make(chan error, 1)
				go func() {
					if pending {
						_, err := setter.MarkIndexReadableOrUniquePending("state")
						transition <- err
					} else {
						_, err := setter.MarkIndexReadable("state")
						transition <- err
					}
				}()
				Eventually(barrier.written, "5s").Should(BeClosed())
				unprotected := writer.indexStateView.maintenance.TryRLock()
				if unprotected {
					writer.indexStateView.maintenance.RUnlock()
				}
				done := make(chan error, 1)
				go func() {
					_, err := writer.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(37)})
					done <- err
				}()
				close(barrier.resume)
				Eventually(transition, "5s").Should(Receive(BeNil()))
				Eventually(done, "5s").Should(Receive(BeNil()))
				Expect(unprotected).To(BeFalse(), "maintenance must not enter during checked state publication")
				want := IndexStateReadable
				if pending {
					want = IndexStateReadableUniquePending
				}
				Expect(writer.GetIndexState("state")).To(Equal(want))
				rows, err := tx.GetRange(writer.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(1))
			})
		}
	}
	for _, batch := range []bool{false, true} {
		for _, twoHandles := range []bool{false, true} {
			It(fmt.Sprintf("holds the shared maintenance gate during real index writes batch=%t twoHandles=%t", batch, twoHandles), func() {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				ss := specSubspace()
				barrier := &stateWriteBarrier{WritableTransaction: tx, key: ss.Sub(IndexKey, "state").Pack(tuple.Tuple{int64(37), int64(1)}), written: make(chan struct{}), resume: make(chan struct{})}
				barrier.paused.Store(true)
				rtx := NewFDBRecordContext(barrier, nil)
				b := baseBuilder()
				b.AddIndex("Order", NewIndex("state", Field("price")))
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				Expect(err).NotTo(HaveOccurred())
				second := first
				if twoHandles {
					second, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(first.ensureStoreStateLoadedErr()).To(Succeed())
				Expect(second.ensureStoreStateLoadedErr()).To(Succeed())
				Expect(second.indexStateView).To(BeIdenticalTo(first.indexStateView))
				barrier.paused.Store(false)
				done := make(chan error, 1)
				go func() {
					record := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(37)}
					if batch {
						_, err := first.SaveRecordBatch([]proto.Message{record})
						done <- err
					} else {
						_, err := first.SaveRecord(record)
						done <- err
					}
				}()
				defer func() {
					select {
					case <-barrier.resume:
					default:
						close(barrier.resume)
					}
				}()
				Eventually(barrier.written, "5s").Should(BeClosed())
				unprotected := second.indexStateView.maintenance.TryLock()
				if unprotected {
					second.indexStateView.maintenance.Unlock()
				}
				transition := make(chan error, 1)
				go func() { _, err := second.MarkIndexDisabled("state"); transition <- err }()
				close(barrier.resume)
				Eventually(done, "5s").Should(Receive(BeNil()))
				Eventually(transition, "5s").Should(Receive(BeNil()))
				Expect(unprotected).To(BeFalse(), "state transition must not overlap maintainer writes")
				Expect(first.GetIndexState("state")).To(Equal(IndexStateDisabled))
				rows, err := tx.GetRange(first.IndexSubspace(md.GetIndex("state")), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(BeEmpty())
			})
		}
	}
	It("keeps a same-context dropped and re-added index disabled on cold reopen", func() {
		ctx := context.Background()
		ss := specSubspace()
		b := baseBuilder()
		b.AddIndex("Order", NewIndex("reused", Field("price")).SetSubspaceKey(int64(1)))
		snapshot := func() *RecordMetaData {
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			p, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			copy, err := RecordMetaDataFromProto(p)
			Expect(err).NotTo(HaveOccurred())
			return copy
		}
		v1 := snapshot()
		b.RemoveIndex("reused")
		v2 := snapshot()
		b.AddIndex("Order", NewIndex("reused", Field("price")).SetSubspaceKey(int64(2)))
		v3 := snapshot()
		stateKey := ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"reused"})
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(v1).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexDisabled("reused")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(v1).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(first.GetIndexState("reused")).To(Equal(IndexStateDisabled))
			_, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(v2).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			raw, err := rtx.Transaction().Get(stateKey).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(BeNil())
			Expect(first.GetIndexState("reused")).To(Equal(IndexStateReadable), "former-index cleanup must update the existing shared view")
			third, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(v3).SetSubspace(ss).
				SetIndexRebuildPolicy(func(*Index, int64, bool) IndexState { return IndexStateDisabled }).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(third.GetIndexState("reused")).To(Equal(IndexStateDisabled))
			raw, err = rtx.Transaction().Get(stateKey).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(Equal(tuple.Tuple{int64(IndexStateDisabled)}.Pack()))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(v3).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState("reused")).To(Equal(IndexStateDisabled))
			record, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(record).NotTo(BeNil())
			_, err = AsList(ctx, store.ScanIndex(v3.GetIndex("reused"), TupleRangeAll, nil, ForwardScan()))
			var unreadable *IndexNotReadableError
			Expect(errors.As(err, &unreadable)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, rangeClear := range []bool{false, true} {
		It(fmt.Sprintf("serializes transaction state writes with publication across handles rangeClear=%t", rangeClear), func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			ss := specSubspace()
			barrier := &stateWriteBarrier{WritableTransaction: tx, key: ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"state"}), written: make(chan struct{}), resume: make(chan struct{})}
			rtx := NewFDBRecordContext(barrier, nil)
			b := baseBuilder()
			b.AddIndex("Order", NewIndex("state", Field("price")))
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(first.GetIndexState("state")).To(Equal(IndexStateReadable))
			Expect(second.GetIndexState("state")).To(Equal(IndexStateReadable))
			firstDone := make(chan struct{})
			go func() {
				defer close(firstDone)
				if rangeClear {
					rtx.ClearRange(first.indexStateSubspace())
				} else {
					first.setIndexState("state", IndexStateDisabled)
				}
			}()
			defer func() {
				select {
				case <-barrier.resume:
				default:
					close(barrier.resume)
				}
			}()
			Eventually(barrier.written, "5s").Should(BeClosed())
			// If publication is not locked across the FDB mutation, force the
			// second writer to finish before the first publishes its older value.
			unprotected := first.indexStateView.mu.TryLock()
			secondDone := make(chan struct{})
			if unprotected {
				first.indexStateView.mu.Unlock()
				second.setIndexState("state", IndexStateWriteOnly)
				close(secondDone)
			} else {
				go func() { defer close(secondDone); second.setIndexState("state", IndexStateWriteOnly) }()
			}
			close(barrier.resume)
			Eventually(firstDone, "5s").Should(BeClosed())
			Eventually(secondDone, "5s").Should(BeClosed())
			raw, err := tx.Get(fdb.Key(barrier.key)).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(Equal(tuple.Tuple{int64(IndexStateWriteOnly)}.Pack()))
			Expect(first.GetIndexState("state")).To(Equal(IndexStateWriteOnly))
			Expect(second.GetIndexState("state")).To(Equal(IndexStateWriteOnly))
			Expect(unprotected).To(BeFalse(), "the common lock must cover the transaction write as well as publication")
		})
	}
})

// stateLoadBarrier pauses a real range read before materialization or before
// returning its result. No storage results or transaction mutations are replaced.
type stateLoadBarrier struct {
	fdb.WritableTransaction
	beforeRead bool
	begin      []byte
	loaded     chan struct{}
	resume     chan struct{}
	paused     atomic.Bool
}

func (tx *stateLoadBarrier) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return (&stateLoadSnapshot{ReadTransaction: tx.WritableTransaction, barrier: tx}).GetRange(r, options)
}

func (tx *stateLoadBarrier) Snapshot() fdb.ReadTransaction {
	return &stateLoadSnapshot{ReadTransaction: tx.WritableTransaction.Snapshot(), barrier: tx}
}

type stateLoadSnapshot struct {
	fdb.ReadTransaction
	barrier *stateLoadBarrier
}

func (tx *stateLoadSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	result := tx.ReadTransaction.GetRange(r, options)
	begin, _ := r.FDBRangeKeySelectors()
	if bytes.Equal(begin.FDBKeySelector().Key.FDBKey(), tx.barrier.begin) {
		return &stateLoadRange{RangeResult: result, barrier: tx.barrier}
	}
	return result
}

type stateLoadRange struct {
	fdb.RangeResult
	barrier *stateLoadBarrier
}

func (r *stateLoadRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	if r.barrier.beforeRead && r.barrier.paused.CompareAndSwap(false, true) {
		close(r.barrier.loaded)
		<-r.barrier.resume
	}
	rows, err := r.RangeResult.GetSliceWithError()
	if !r.barrier.beforeRead && r.barrier.paused.CompareAndSwap(false, true) {
		close(r.barrier.loaded)
		<-r.barrier.resume
	}
	return rows, err
}

var _ = Describe("Index-state initialization versus context clearing", func() {
	for _, mode := range []string{"opened", "opening", "lazy", "reload"} {
		It("does not publish pre-clear state through "+mode+" handles", func() {
			ctx := context.Background()
			ss := specSubspace()
			b := baseBuilder()
			b.AddIndex("Order", NewIndex("state", Field("price")))
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("state")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			stateSS := ss.Sub(IndexStateSpaceKey)
			begin, _ := stateSS.FDBRangeKeys()
			barrier := &stateLoadBarrier{WritableTransaction: tx, begin: begin.FDBKey(), loaded: make(chan struct{}), resume: make(chan struct{})}
			rtx := NewFDBRecordContext(barrier, nil)
			builder := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss)
			var store *FDBRecordStore
			if mode == "reload" {
				barrier.paused.Store(true)
				store, err = builder.Open()
				Expect(err).NotTo(HaveOccurred())
				barrier.paused.Store(false)
			}
			if mode == "opened" {
				close(barrier.resume)
				store, err = builder.Open()
				Expect(err).NotTo(HaveOccurred())
				// Deliberately no state getter between Open and ClearRange.
				rtx.ClearRange(stateSS)
			} else {
				defer func() {
					select {
					case <-barrier.resume:
					default:
						close(barrier.resume)
					}
				}()
				loaded := make(chan struct{})
				go func() {
					defer close(loaded)
					if mode == "reload" {
						err = store.ReloadRecordStoreState()
					} else if mode == "lazy" {
						store, err = builder.Build()
						if err == nil {
							err = store.ensureStoreStateLoadedErr()
						}
					} else {
						store, err = builder.Open()
					}
				}()
				Eventually(barrier.loaded, "5s").Should(BeClosed())
				// Force the old implementation to clear before publishing its
				// captured range. With synchronization the clear follows publication.
				unprotected := rtx.indexStateMu.TryLock()
				cleared := make(chan struct{})
				if unprotected {
					rtx.indexStateMu.Unlock()
					rtx.ClearRange(stateSS)
					close(cleared)
				} else {
					go func() { defer close(cleared); rtx.ClearRange(stateSS) }()
				}
				close(barrier.resume)
				Eventually(loaded, "5s").Should(BeClosed())
				Eventually(cleared, "5s").Should(BeClosed())
				Expect(err).NotTo(HaveOccurred())
			}
			raw, err := tx.Get(stateSS.Pack(tuple.Tuple{"state"})).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(BeNil())
			changed, err := store.MarkIndexDisabled("state")
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue(), "the cleared state key is READABLE, not the pre-clear DISABLED snapshot")
			Expect(store.GetIndexState("state")).To(Equal(IndexStateDisabled))
			raw, err = tx.Get(stateSS.Pack(tuple.Tuple{"state"})).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(Equal(tuple.Tuple{int64(IndexStateDisabled)}.Pack()))
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())
			entries, err := tx.GetRange(store.IndexSubspace(md.GetIndex("state")), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "disabled state must suppress maintenance")
			Expect(rtx.Commit()).To(Succeed())
			_, err = sharedDB.Run(ctx, func(cold *FDBRecordContext) (any, error) {
				reopened, err := NewStoreBuilder().SetContext(cold).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(reopened.GetIndexState("state")).To(Equal(IndexStateDisabled))
				record, err := reopened.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(record).NotTo(BeNil())
				_, err = AsList(ctx, reopened.ScanIndex(md.GetIndex("state"), TupleRangeAll, nil, ForwardScan()))
				var unreadable *IndexNotReadableError
				Expect(errors.As(err, &unreadable)).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})

var _ = Describe("Shared index-state explicit reload", func() {
	It("publishes a reloaded state to already-bound handles", func() {
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		rtx := sharedDB.NewRecordContext(tx)
		ss := specSubspace()
		b := baseBuilder()
		b.AddIndex("Order", NewIndex("state", Field("price")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		first, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
		Expect(err).NotTo(HaveOccurred())
		second, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		Expect(err).NotTo(HaveOccurred())
		Expect(first.GetIndexState("state")).To(Equal(IndexStateReadable))
		Expect(second.GetIndexState("state")).To(Equal(IndexStateReadable))
		// Direct transaction writes require explicit reload; ordinary store
		// transitions publish their state as part of the mutation itself.
		tx.Set(ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"state"}), tuple.Tuple{int64(IndexStateDisabled)}.Pack())
		Expect(first.ReloadRecordStoreState()).To(Succeed())
		Expect(first.GetIndexState("state")).To(Equal(IndexStateDisabled))
		Expect(second.GetIndexState("state")).To(Equal(IndexStateDisabled))
		for _, handle := range []*FDBRecordStore{first, second} {
			Expect(handle.GetAllIndexStatesMap()).To(HaveKeyWithValue("state", IndexStateDisabled))
			Expect(handle.GetRecordStoreState().IndexStates).To(HaveKeyWithValue("state", IndexStateDisabled))
		}
		_, err = second.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
		Expect(err).NotTo(HaveOccurred())
		entries, err := tx.GetRange(second.IndexSubspace(md.GetIndex("state")), fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})
})

var _ = Describe("Index-state clear cache coherence", func() {
	for _, mode := range []string{"afterOpen", "beforeOpen", "clearOnly"} {
		It("does not reuse a pre-clear shared cache entry after commit "+mode, func() {
			beforeOpen := mode != "afterOpen"
			ctx := context.Background()
			ss := specSubspace()
			b := baseBuilder()
			b.AddIndex("Order", NewIndex("state", Field("price")))
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			cache := NewMetaDataVersionStampStoreStateCache()
			open := func(rtx *FDBRecordContext) *StoreBuilder {
				return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetStoreStateCache(cache)
			}
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := open(rtx).Create()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SetStateCacheability(true)
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("state")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := open(rtx).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState("state")).To(Equal(IndexStateDisabled))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			cache.mu.Lock()
			admitted := cache.getIfPresent(string(ss.Bytes()))
			cache.mu.Unlock()
			Expect(admitted).NotTo(BeNil())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.ClearRange(ss.Sub(RecordKey))
				Expect(rtx.HasDirtyStoreState()).To(BeFalse(), "ordinary record clears must not invalidate metadata caches")
				Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeFalse())
				if beforeOpen {
					rtx.ClearRange(ss.Sub(IndexStateSpaceKey))
				}
				if mode == "clearOnly" {
					return nil, nil // no store load in this transaction
				}
				store, err := open(rtx).Open()
				Expect(err).NotTo(HaveOccurred())
				if !beforeOpen {
					rtx.ClearRange(store.IndexStateSubspace())
				}
				Expect(store.GetIndexState("state")).To(Equal(IndexStateReadable))
				Expect(store.GetAllIndexStatesMap()).NotTo(HaveKey("state"))
				Expect(store.GetRecordStoreState().IndexStates).NotTo(HaveKey("state"))
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := open(rtx).Open()
				Expect(err).NotTo(HaveOccurred())
				raw, err := rtx.Transaction().Get(ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"state"})).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(raw).To(BeNil())
				stamp, err := rtx.GetMetaDataVersionStamp()
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(Equal(admitted.GetMetaDataVersionStamp()), "the clear must invalidate shared cache entries before it commits")
				Expect(store.GetIndexState("state")).To(Equal(IndexStateReadable))
				if mode == "clearOnly" {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
					Expect(err).NotTo(HaveOccurred())
				}
				entries, err := AsList(ctx, store.ScanIndex(md.GetIndex("state"), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				cold, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(cold.GetIndexState("state")).To(Equal(IndexStateReadable))
				entries, err := AsList(ctx, cold.ScanIndex(md.GetIndex("state"), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), "maintenance must survive commit and uncached reopening")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})

var _ = Describe("Reload and uniqueness cleanup lock ordering", func() {
	It("does not hold the registry while waiting for a maintenance reader", func() {
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		ss := specSubspace()
		idx := NewIndex("unique", Field("price"))
		idx.SetUnique()
		b := baseBuilder()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		violations := ss.Sub(IndexUniquenessViolationsKey, idx.SubspaceTupleKey()).Sub(int64(42))
		begin, _ := violations.FDBRangeKeys()
		barrier := &stateLoadBarrier{WritableTransaction: tx, beforeRead: true, begin: begin.FDBKey(), loaded: make(chan struct{}), resume: make(chan struct{})}
		// Setup reads must complete before the deletion-specific pause is armed.
		barrier.paused.Store(true)
		rtx := NewFDBRecordContext(barrier, nil)
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
		Expect(err).NotTo(HaveOccurred())
		_, err = store.MarkIndexWriteOnly(idx.Name)
		Expect(err).NotTo(HaveOccurred())
		for _, id := range []int64{1, 2} {
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())
		}
		rows, err := store.ScanUniquenessViolations(idx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(2))
		barrier.paused.Store(false)
		defer func() {
			select {
			case <-barrier.resume:
			default:
				close(barrier.resume)
			}
		}()
		deleted := make(chan error, 1)
		go func() { _, err := store.DeleteRecord(tuple.Tuple{int64(1)}); deleted <- err }()
		Eventually(barrier.loaded, "5s").Should(BeClosed())
		// Deletion already cleared its own violation and holds stateMu.RLock.
		remaining, err := tx.GetRange(violations, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		Expect(remaining).To(HaveLen(1))
		reloaded := make(chan error, 1)
		go func() { reloaded <- store.ReloadRecordStoreState() }()
		Eventually(func() bool {
			if store.stateMu.TryRLock() {
				store.stateMu.RUnlock()
				return false
			}
			return true // reload's writer is queued behind maintenance
		}, "5s").Should(BeTrue())
		registryFree := rtx.indexStateMu.TryLock()
		if registryFree {
			rtx.indexStateMu.Unlock()
		} else {
			// Do not strand a reproduced lock cycle: the paused range read
			// must fail on the real canceled transaction before reaching clear.
			tx.Cancel()
		}
		close(barrier.resume)
		var deleteErr, reloadErr error
		Eventually(deleted, "5s").Should(Receive(&deleteErr))
		Eventually(reloaded, "5s").Should(Receive(&reloadErr))
		Expect(registryFree).To(BeTrue(), "reload must not hold registry while maintenance holds stateMu")
		Expect(deleteErr).NotTo(HaveOccurred())
		Expect(reloadErr).NotTo(HaveOccurred())
		rows, err = store.ScanUniquenessViolations(idx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
		Expect(store.GetIndexState(idx.Name)).To(Equal(IndexStateWriteOnly))
		record, err := store.LoadRecord(tuple.Tuple{int64(2)})
		Expect(err).NotTo(HaveOccurred())
		Expect(record).NotTo(BeNil())
	})
})

var _ = Describe("Opened noncacheable store deletion stamp policy", func() {
	for _, recreate := range []bool{false, true} {
		It(fmt.Sprintf("does not bump the global stamp recreate=%t", recreate), func() {
			ctx := context.Background()
			ss := specSubspace()
			md, err := baseBuilder().Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				rtx.SetMetaDataVersionStamp()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			var before []byte
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsStateCacheable()).To(BeFalse())
				before, err = rtx.GetMetaDataVersionStamp()
				Expect(err).NotTo(HaveOccurred())
				Expect(before).NotTo(BeEmpty())
				Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeFalse())
				Expect(DeleteStore(rtx, ss)).To(Succeed())
				Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeFalse(), "opening a noncacheable store must not alter deletion's header policy")
				if recreate {
					_, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeFalse(), "clear history must not override the deletion policy during recreation")
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				after, err := rtx.GetMetaDataVersionStamp()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})

var _ = Describe("Context clear metadata classification", func() {
	for _, tc := range []struct {
		name      string
		opened    bool
		wantDirty bool
	}{
		{"records", false, false},
		{"records", true, false},
		{"partial records", false, false},
		{"partial records", true, false},
		{"index data", true, false},
		{"state", false, true},
		{"state", true, true},
		{"partial state", false, true},
		{"header", true, true},
		{"unknown", false, true},
		{"missing header records", false, true},
		{"malformed header records", false, true},
		{"empty", false, false},
	} {
		It(fmt.Sprintf("classifies %s opened=%t", tc.name, tc.opened), func() {
			ctx := context.Background()
			// The embedded numeric record-key bytes are not a tuple boundary.
			// Header validation must reject that candidate, not trust bytes alone.
			ss := specSubspace().Sub([]byte{0xfe, 0x15, 0x01, 0xfd})
			md, err := baseBuilder().Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			rtx := sharedDB.NewRecordContext(tx)
			if tc.opened {
				_, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
			}
			var keyRange fdb.ExactRange = ss.Sub(RecordKey)
			switch tc.name {
			case "partial records":
				key := ss.Sub(RecordKey).Pack(tuple.Tuple{int64(42)})
				keyRange = fdb.KeyRange{Begin: key, End: fdb.Key(append(bytes.Clone(key), 0))}
			case "index data":
				keyRange = ss.Sub(IndexKey)
			case "state":
				keyRange = ss.Sub(IndexStateSpaceKey)
			case "partial state":
				key := ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"one"})
				keyRange = fdb.KeyRange{Begin: key, End: fdb.Key(append(bytes.Clone(key), 0))}
			case "header":
				key := ss.Pack(tuple.Tuple{StoreInfoKey})
				keyRange = fdb.KeyRange{Begin: key, End: fdb.Key(append(bytes.Clone(key), 0))}
			case "unknown":
				keyRange = specSubspace()
			case "missing header records":
				tx.Clear(ss.Pack(tuple.Tuple{StoreInfoKey}))
			case "malformed header records":
				tx.Set(ss.Pack(tuple.Tuple{StoreInfoKey}), []byte{0xff})
			case "empty":
				key := ss.Pack(tuple.Tuple{StoreInfoKey})
				keyRange = fdb.KeyRange{Begin: key, End: key}
			}
			Expect(rtx.HasDirtyStoreState()).To(BeFalse())
			Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(BeFalse())
			rtx.ClearRange(keyRange)
			Expect(rtx.HasDirtyStoreState()).To(Equal(tc.wantDirty))
			Expect(rtx.dirtyMetaDataVersionStamp.Load()).To(Equal(tc.wantDirty))
		})
	}
})
