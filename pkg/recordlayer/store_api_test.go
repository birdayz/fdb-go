package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("FDBRecordStore API", func() {
	ctx := context.Background()

	// Helper: create metadata builder with Order/Customer/TypedRecord types set up.
	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	Describe("RecordsSubspace", func() {
		It("returns a subspace prefixed by the store subspace", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				recSub := store.RecordsSubspace()
				Expect(recSub).NotTo(BeNil())
				Expect(bytes.HasPrefix(recSub.Bytes(), ks.Bytes())).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IndexSubspace / IndexSecondarySubspace", func() {
		It("returns subspaces prefixed by the store subspace", func() {
			ks := specSubspace()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$price")
				Expect(idx).NotTo(BeNil())

				idxSub := store.IndexSubspace(idx)
				Expect(idxSub).NotTo(BeNil())
				Expect(bytes.HasPrefix(idxSub.Bytes(), ks.Bytes())).To(BeTrue())

				secSub := store.IndexSecondarySubspace(idx)
				Expect(secSub).NotTo(BeNil())
				Expect(bytes.HasPrefix(secSub.Bytes(), ks.Bytes())).To(BeTrue())

				// They should be different subspaces.
				Expect(idxSub.Bytes()).NotTo(Equal(secSub.Bytes()))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetReadableIndexes", func() {
		It("returns only indexes in READABLE or READABLE_UNIQUE_PENDING state", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))
			idx3 := NewIndex("Order$flower_type", Nest("flower", Field("type")))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			builder.AddIndex("Order", idx3)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// All start READABLE: 3 readable.
				readable := store.GetReadableIndexes()
				Expect(readable).To(HaveLen(3))

				// Mark one WRITE_ONLY, one DISABLED.
				_, err = store.MarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("Order$flower_type")
				Expect(err).NotTo(HaveOccurred())

				readable = store.GetReadableIndexes()
				Expect(readable).To(HaveLen(1))
				Expect(readable[0].Name).To(Equal("Order$price"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("includes READABLE_UNIQUE_PENDING indexes", func() {
			ks := specSubspace()

			priceIdx := NewIndex("Order$price", Field("price"))
			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save a record so the store has data.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(10)})
				Expect(err).NotTo(HaveOccurred())

				// Mark unique index WRITE_ONLY, add a uniqueness violation, mark complete.
				_, err = store.MarkIndexWriteOnly("Order$unique_qty")
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())
				markRangeSetComplete(store, idx)

				// Mark as READABLE_UNIQUE_PENDING.
				changed, err := store.MarkIndexReadableOrUniquePending("Order$unique_qty")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$unique_qty")).To(Equal(IndexStateReadableUniquePending))

				// GetReadableIndexes should include both READABLE and READABLE_UNIQUE_PENDING.
				readable := store.GetReadableIndexes()
				Expect(readable).To(HaveLen(2))

				names := map[string]bool{}
				for _, r := range readable {
					names[r.Name] = true
				}
				Expect(names).To(HaveKey("Order$price"))
				Expect(names).To(HaveKey("Order$unique_qty"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetEnabledIndexes", func() {
		It("returns READABLE, WRITE_ONLY, and READABLE_UNIQUE_PENDING but NOT DISABLED", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))
			idx3 := NewIndex("Order$flower_type", Nest("flower", Field("type")))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			builder.AddIndex("Order", idx3)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Mark one WRITE_ONLY, one DISABLED.
				_, err = store.MarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("Order$flower_type")
				Expect(err).NotTo(HaveOccurred())

				enabled := store.GetEnabledIndexes()
				Expect(enabled).To(HaveLen(2))

				names := map[string]bool{}
				for _, e := range enabled {
					names[e.Name] = true
				}
				Expect(names).To(HaveKey("Order$price"))
				Expect(names).To(HaveKey("Order$qty"))
				Expect(names).NotTo(HaveKey("Order$flower_type"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetAllIndexStates", func() {
		It("returns correct states for all indexes", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))
			idx3 := NewIndex("Order$flower_type", Nest("flower", Field("type")))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			builder.AddIndex("Order", idx3)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Mark one WRITE_ONLY, one DISABLED, one stays READABLE.
				_, err = store.MarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("Order$flower_type")
				Expect(err).NotTo(HaveOccurred())

				states := store.GetAllIndexStates()
				Expect(states).To(HaveLen(3))
				Expect(states["Order$price"]).To(Equal(IndexStateReadable))
				Expect(states["Order$qty"]).To(Equal(IndexStateWriteOnly))
				Expect(states["Order$flower_type"]).To(Equal(IndexStateDisabled))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("RebuildAllIndexes", func() {
		It("rebuilds all non-READABLE indexes", func() {
			ks := specSubspace()

			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))

			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store, insert records, mark both indexes WRITE_ONLY.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.ClearAndMarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: RebuildAllIndexes.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())
				Expect(store.IsIndexWriteOnly("Order$qty")).To(BeTrue())

				err = store.RebuildAllIndexes()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				// Verify entries exist.
				priceEntries, err := AsList(ctx, store.ScanIndex(md.GetIndex("Order$price"), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(5))

				qtyEntries, err := AsList(ctx, store.ScanIndex(md.GetIndex("Order$qty"), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(5))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("VacuumReadableIndexesBuildData", func() {
		It("clears build artifacts for READABLE indexes", func() {
			ks := specSubspace()

			priceIdx := NewIndex("Order$price", Field("price"))

			// Phase 1: Insert records without index.
			builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build the index online.
			builderWithIdx := baseMetaData()
			builderWithIdx.AddIndex("Order", priceIdx)
			mdWithIdx, err := builderWithIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				SetIndex(priceIdx).
				SetSubspace(ks).
				SetLimit(5).
				SetMarkReadable(false). // leave WRITE_ONLY so the type-stamp survives; we mark readable directly below.
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Verify build artifacts exist, then vacuum.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// The index was built with SetMarkReadable(false), so it is still
				// WRITE_ONLY and the type-stamp survives. Mark it readable directly:
				// MarkIndexReadableOrUniquePending clears the range set + heartbeats (via
				// clearReadableIndexBuildData) but deliberately does NOT erase the
				// type-stamp — leaving exactly the state this vacuum test needs: a
				// READABLE index that still carries a leftover type-stamp artifact.
				idx := mdWithIdx.GetIndex("Order$price")
				_, err = store.MarkIndexReadableOrUniquePending("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				// Build stamp should still exist before vacuum.
				stamp, err := store.LoadIndexingTypeStamp(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())

				// Vacuum.
				store.VacuumReadableIndexesBuildData()

				// After vacuum, the leftover type-stamp should be cleared.
				stamp, err = store.LoadIndexingTypeStamp(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil())

				firstMissing, err := store.FirstUnbuiltRange(idx)
				Expect(err).NotTo(HaveOccurred())
				// Range set was cleared, so everything is "missing" now.
				Expect(firstMissing).NotTo(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("DeleteStore", func() {
		It("removes all store data so Open fails", func() {
			ks := specSubspace()

			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store with records.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: DeleteStore.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, DeleteStore(rtx, ks)
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Open should fail with RecordStoreDoesNotExistError.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var storeErr *RecordStoreDoesNotExistError
			Expect(errors.As(err, &storeErr)).To(BeTrue())
		})
	})

	Describe("FirstUnbuiltRange", func() {
		It("returns nil when the range set is fully populated", func() {
			ks := specSubspace()

			priceIdx := NewIndex("Order$price", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Manually mark the range set as complete.
				idx := md.GetIndex("Order$price")
				markRangeSetComplete(store, idx)

				missing, err := store.FirstUnbuiltRange(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(missing).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns non-nil for a WRITE_ONLY index with empty range set", func() {
			ks := specSubspace()

			priceIdx := NewIndex("Order$price", Field("price"))

			// Phase 1: Insert records without index.
			builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add the index, mark WRITE_ONLY (no build yet).
			builderWithIdx := baseMetaData()
			builderWithIdx.AddIndex("Order", priceIdx)
			mdWithIdx, err := builderWithIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Range set has nothing inserted, so first unbuilt range should be non-nil.
				idx := mdWithIdx.GetIndex("Order$price")
				missing, err := store.FirstUnbuiltRange(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(missing).NotTo(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IsCacheable", func() {
		It("defaults to false and reflects SetStateCacheability", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsCacheable()).To(BeFalse())

				changed, err := store.SetStateCacheability(true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				Expect(store.IsCacheable()).To(BeTrue())

				// Setting again to same value returns false.
				changed, err = store.SetStateCacheability(true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetStoreHeader", func() {
		It("returns non-nil header with format version set", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				header := store.GetStoreHeader()
				Expect(header).NotTo(BeNil())
				Expect(header.GetFormatVersion()).To(BeNumerically(">=", formatVersionMinimum))
				Expect(header.GetFormatVersion()).To(BeNumerically("<=", formatVersionCurrent))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns a clone that does not affect the store", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				header1 := store.GetStoreHeader()
				originalVersion := header1.GetFormatVersion()

				// Mutate the clone.
				newVersion := int32(99)
				header1.FormatVersion = &newVersion

				// Get header again: should still have original version.
				header2 := store.GetStoreHeader()
				Expect(header2.GetFormatVersion()).To(Equal(originalVersion))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetAllIndexStatesMap", func() {
		It("returns only non-READABLE states", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// All default READABLE: raw map should be empty.
				rawStates := store.GetAllIndexStatesMap()
				Expect(rawStates).To(BeEmpty())

				// Mark one WRITE_ONLY.
				_, err = store.MarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())

				rawStates = store.GetAllIndexStatesMap()
				Expect(rawStates).To(HaveLen(1))
				Expect(rawStates["Order$qty"]).To(Equal(IndexStateWriteOnly))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetRecordMetaData / GetContext / GetSubspace", func() {
		It("returns the store's metadata, context, and subspace", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetRecordMetaData()).To(Equal(md))
				Expect(store.GetContext()).To(Equal(rtx))
				Expect(store.GetSubspace().Bytes()).To(Equal(ks.Bytes()))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("DryRunSaveRecord", func() {
		It("validates and returns computed record without writing", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// DryRun should succeed and return a record.
				order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
				result, err := store.DryRunSaveRecord(order, RecordExistenceCheckNone)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result.PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
				Expect(result.RecordType.Name).To(Equal("Order"))
				Expect(result.ValueSize).To(BeNumerically(">", 0))
				Expect(result.KeySize).To(BeNumerically(">", 0))

				// Verify no data was actually written.
				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("detects existence errors without writing", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// First, actually save a record.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// DryRun with ERROR_IF_EXISTS should fail.
				_, err = store.DryRunSaveRecord(
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)},
					RecordExistenceCheckErrorIfExists,
				)
				Expect(err).To(HaveOccurred())
				var existsErr *RecordAlreadyExistsError
				Expect(errors.As(err, &existsErr)).To(BeTrue())

				// DryRun with ERROR_IF_NOT_EXISTS on non-existent record should fail.
				_, err = store.DryRunSaveRecord(
					&gen.Order{OrderId: proto.Int64(999), Price: proto.Int32(200)},
					RecordExistenceCheckErrorIfNotExists,
				)
				Expect(err).To(HaveOccurred())
				var notExistsErr *RecordDoesNotExistError
				Expect(errors.As(err, &notExistsErr)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("previews on a locked store without checking lock state (Java-faithful)", func() {
			// RFC-158 / Graefe: Java's saveTypedRecord(isDryRun=true) early-returns at
			// FDBRecordStore.java:578, BEFORE validateRecordUpdateAllowed (line 584). So a
			// DRY RUN previews SUCCESS on a FORBID_RECORD_UPDATE-locked store — checking the
			// lock here made Go stricter than Java. (This test previously asserted the lock
			// error, pinning that divergence.) The real save IS still lock-rejected; see
			// store_state_test.go + the OverrideLockSaveRecord spec below.
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create and lock the store.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "test")
			})
			Expect(err).NotTo(HaveOccurred())

			// DryRun PREVIEWS success (no lock check), returning the would-be stored record.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stored, derr := store.DryRunSaveRecord(
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
					RecordExistenceCheckNone,
				)
				if derr != nil {
					return nil, derr
				}
				Expect(stored).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(),
				"DryRunSaveRecord must preview success on a locked store (Java early-returns before the lock check)")
		})
	})

	Describe("OverrideLockSaveRecord", func() {
		It("saves a record even when the store is locked for record updates", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store and lock it.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "test lock")
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Normal save should fail; OverrideLockSaveRecord should succeed.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// Normal save fails.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).To(HaveOccurred())
				var lockErr *StoreIsLockedForRecordUpdatesError
				Expect(errors.As(err, &lockErr)).To(BeTrue())

				// Override lock save succeeds.
				saved, err := store.OverrideLockSaveRecord(
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
					RecordExistenceCheckNone,
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(saved).NotTo(BeNil())
				Expect(saved.PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))

				// Normal save still fails (overrideLock flag was reset).
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(err).To(HaveOccurred())
				Expect(errors.As(err, &lockErr)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("passes through existence checks correctly", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store, insert a record, then lock.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "test lock")
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: OverrideLockSaveRecord with ERROR_IF_EXISTS should fail on duplicate.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.OverrideLockSaveRecord(
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)},
					RecordExistenceCheckErrorIfExists,
				)
				Expect(err).To(HaveOccurred())
				var existsErr *RecordAlreadyExistsError
				Expect(errors.As(err, &existsErr)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("DryRunDeleteRecord", func() {
		It("returns true for an existing record without deleting it", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save a record.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// DryRun should return true.
				exists, err := store.DryRunDeleteRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				// Record should still be there.
				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for a non-existent record", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				exists, err := store.DryRunDeleteRecord(tuple.Tuple{int64(999)})
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns true after save and false after real delete", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)})
				Expect(err).NotTo(HaveOccurred())

				exists, err := store.DryRunDeleteRecord(tuple.Tuple{int64(42)})
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				// Actually delete it.
				_, err = store.DeleteRecord(tuple.Tuple{int64(42)})
				Expect(err).NotTo(HaveOccurred())

				// Now DryRun should return false.
				exists, err = store.DryRunDeleteRecord(tuple.Tuple{int64(42)})
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("succeeds even when store is locked — matches Java dryRunDeleteRecordAsync", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store, save a record, then lock it.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "test lock")
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: DryRunDeleteRecord should succeed even when locked.
			// Java's dryRunDeleteRecordAsync just loads and returns existence,
			// it does NOT call validateRecordUpdateAllowed.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				exists, err := store.DryRunDeleteRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IsIndexReadableUniquePending", func() {
		It("returns false for a READABLE index", func() {
			ks := specSubspace()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Default state is READABLE.
				Expect(store.IsIndexReadableUniquePending("Order$price")).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns true for a READABLE_UNIQUE_PENDING index", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save a record so the index has data.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(10)})
				Expect(err).NotTo(HaveOccurred())

				// Mark WRITE_ONLY, add violation, mark complete, then READABLE_UNIQUE_PENDING.
				_, err = store.MarkIndexWriteOnly("Order$unique_qty")
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())
				markRangeSetComplete(store, idx)

				changed, err := store.MarkIndexReadableOrUniquePending("Order$unique_qty")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				Expect(store.IsIndexReadableUniquePending("Order$unique_qty")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for WRITE_ONLY and DISABLED indexes", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("Order$qty")
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadableUniquePending("Order$price")).To(BeFalse())
				Expect(store.IsIndexReadableUniquePending("Order$qty")).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetWriteOnlyIndexes / GetDisabledIndexes", func() {
		It("returns empty when all indexes are READABLE", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetWriteOnlyIndexes()).To(BeEmpty())
				Expect(store.GetDisabledIndexes()).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns correct indexes when states are mixed", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))
			idx3 := NewIndex("Order$flower_type", Nest("flower", Field("type")))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			builder.AddIndex("Order", idx3)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// price stays READABLE, qty → WRITE_ONLY, flower_type → DISABLED.
				_, err = store.MarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled("Order$flower_type")
				Expect(err).NotTo(HaveOccurred())

				writeOnly := store.GetWriteOnlyIndexes()
				Expect(writeOnly).To(HaveLen(1))
				Expect(writeOnly[0].Name).To(Equal("Order$qty"))

				disabled := store.GetDisabledIndexes()
				Expect(disabled).To(HaveLen(1))
				Expect(disabled[0].Name).To(Equal("Order$flower_type"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns multiple indexes in same state", func() {
			ks := specSubspace()

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$qty", Field("quantity"))
			idx3 := NewIndex("Order$flower_type", Nest("flower", Field("type")))

			builder := baseMetaData()
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			builder.AddIndex("Order", idx3)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Mark all three WRITE_ONLY.
				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexWriteOnly("Order$qty")
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexWriteOnly("Order$flower_type")
				Expect(err).NotTo(HaveOccurred())

				writeOnly := store.GetWriteOnlyIndexes()
				Expect(writeOnly).To(HaveLen(3))

				Expect(store.GetDisabledIndexes()).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetIndexesToBuildSince", func() {
		It("returns indexes added after given version", func() {
			ks := specSubspace()

			builder := baseMetaData()
			// Set explicit version=1 as baseline.
			builder.SetVersion(1)

			idx1 := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", idx1) // bumps to version=2, idx1.LastModifiedVersion=2

			idx2 := NewIndex("Order$qty", Field("quantity"))
			builder.AddIndex("Order", idx2) // bumps to version=3, idx2.LastModifiedVersion=3

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// All indexes added after version 1.
				toBuild := store.GetIndexesToBuildSince(1)
				Expect(toBuild).To(HaveLen(2))

				// Only idx2 was added after version 2.
				toBuild = store.GetIndexesToBuildSince(2)
				Expect(toBuild).To(HaveLen(1))
				Expect(toBuild[0].Name).To(Equal("Order$qty"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty when no indexes need building", func() {
			ks := specSubspace()

			builder := baseMetaData()
			idx1 := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", idx1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Use a version higher than any index's LastModifiedVersion.
				toBuild := store.GetIndexesToBuildSince(md.Version())
				Expect(toBuild).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty when there are no indexes at all", func() {
			ks := specSubspace()

			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				toBuild := store.GetIndexesToBuildSince(0)
				Expect(toBuild).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ScanRecordKeys", func() {
		It("returns primary keys for all records in forward order", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				keys, err := AsList(ctx, store.ScanRecordKeys(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(keys).To(HaveLen(5))

				for i, pk := range keys {
					Expect(pk).To(Equal(tuple.Tuple{int64(i + 1)}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty list for empty store", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				keys, err := AsList(ctx, store.ScanRecordKeys(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(keys).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("supports row limit with continuation", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// Scan with limit=2.
				scan := ForwardScan()
				scan.ExecuteProperties.ReturnedRowLimit = 2
				cursor := store.ScanRecordKeys(nil, scan)

				r1, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r1.HasNext()).To(BeTrue())
				Expect(r1.GetValue()).To(Equal(tuple.Tuple{int64(1)}))

				r2, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r2.HasNext()).To(BeTrue())
				Expect(r2.GetValue()).To(Equal(tuple.Tuple{int64(2)}))

				// Third call should hit the limit.
				r3, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r3.HasNext()).To(BeFalse())
				Expect(r3.GetNoNextReason()).To(Equal(ReturnLimitReached))

				// Extract continuation and resume.
				contBytes, err := r3.GetContinuation().ToBytes()
				Expect(err).NotTo(HaveOccurred())
				Expect(contBytes).NotTo(BeNil())

				scan2 := ForwardScan()
				scan2.ExecuteProperties.ReturnedRowLimit = 10
				cursor2 := store.ScanRecordKeys(contBytes, scan2)

				keys, err := AsList(ctx, cursor2)
				Expect(err).NotTo(HaveOccurred())
				Expect(keys).To(HaveLen(3))
				Expect(keys[0]).To(Equal(tuple.Tuple{int64(3)}))
				Expect(keys[1]).To(Equal(tuple.Tuple{int64(4)}))
				Expect(keys[2]).To(Equal(tuple.Tuple{int64(5)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles split records by deduplicating PKs", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetSplitLongRecords(true)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save a small record (unsplit, but still uses split format with suffix).
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// Save a large record that will be split into chunks.
				_, err = store.SaveRecord(makeLargeOrder(2, 250_000))
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())

				// ScanRecordKeys should return exactly 3 PKs despite split KV entries.
				keys, err := AsList(ctx, store.ScanRecordKeys(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(keys).To(HaveLen(3))
				Expect(keys[0]).To(Equal(tuple.Tuple{int64(1)}))
				Expect(keys[1]).To(Equal(tuple.Tuple{int64(2)}))
				Expect(keys[2]).To(Equal(tuple.Tuple{int64(3)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns keys for multiple record types", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(20), Name: proto.String("Alice")})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(30), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())

				keys, err := AsList(ctx, store.ScanRecordKeys(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(keys).To(HaveLen(3))
				// Keys are in order of their FDB key encoding, which is int64 order.
				Expect(keys[0]).To(Equal(tuple.Tuple{int64(10)}))
				Expect(keys[1]).To(Equal(tuple.Tuple{int64(20)}))
				Expect(keys[2]).To(Equal(tuple.Tuple{int64(30)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
		It("paginated reverse scan returns no duplicates (regression: continuation was adjusting begin instead of end)", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// Paginate reverse with limit=2 and collect all PKs.
				var allKeys []tuple.Tuple
				var cont []byte
				for {
					scan := ReverseScan()
					scan.ExecuteProperties.ReturnedRowLimit = 2
					cursor := store.ScanRecordKeys(cont, scan)
					keys, nextCont, err := collectPage(ctx, cursor)
					Expect(err).NotTo(HaveOccurred())
					allKeys = append(allKeys, keys...)
					if nextCont == nil {
						break
					}
					cont = nextCont
				}

				// Must be exactly 5 keys in reverse order, no duplicates.
				Expect(allKeys).To(HaveLen(5))
				Expect(allKeys[0]).To(Equal(tuple.Tuple{int64(5)}))
				Expect(allKeys[1]).To(Equal(tuple.Tuple{int64(4)}))
				Expect(allKeys[2]).To(Equal(tuple.Tuple{int64(3)}))
				Expect(allKeys[3]).To(Equal(tuple.Tuple{int64(2)}))
				Expect(allKeys[4]).To(Equal(tuple.Tuple{int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("paginated forward scan with split records returns no duplicates (regression: continuation pointed to first chunk, not past all chunks)", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetSplitLongRecords(true)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				// 250KB record — split into 3 chunks
				_, err = store.SaveRecord(makeLargeOrder(2, 250_000))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())

				// Paginate with limit=1 — forces continuation after each PK.
				var allKeys []tuple.Tuple
				var cont []byte
				for {
					scan := ForwardScan()
					scan.ExecuteProperties.ReturnedRowLimit = 1
					cursor := store.ScanRecordKeys(cont, scan)
					keys, nextCont, err := collectPage(ctx, cursor)
					Expect(err).NotTo(HaveOccurred())
					allKeys = append(allKeys, keys...)
					if nextCont == nil {
						break
					}
					cont = nextCont
				}

				// Must be exactly 3 keys, no duplicates from split chunks.
				Expect(allKeys).To(HaveLen(3))
				Expect(allKeys[0]).To(Equal(tuple.Tuple{int64(1)}))
				Expect(allKeys[1]).To(Equal(tuple.Tuple{int64(2)}))
				Expect(allKeys[2]).To(Equal(tuple.Tuple{int64(3)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ResolveUniquenessViolationByDeletion", func() {
		It("deletes all violating records except remainPrimaryKey", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save one record normally.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Quantity: proto.Int32(10), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// Mark index WRITE_ONLY so we can create violations.
				_, err = store.MarkIndexWriteOnly("Order$unique_qty")
				Expect(err).NotTo(HaveOccurred())

				// Save duplicate records (WRITE_ONLY allows duplicates for unique indexes).
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Quantity: proto.Int32(10), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Quantity: proto.Int32(10), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())

				// Manually record uniqueness violations.
				idx := md.GetIndex("Order$unique_qty")
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(3)})).NotTo(HaveOccurred())

				// Resolve: keep record with PK=1, delete PK=2 and PK=3.
				err = store.ResolveUniquenessViolationByDeletion(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())

				// PK=1 should still exist.
				rec1, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec1).NotTo(BeNil())

				// PK=2 and PK=3 should be deleted.
				rec2, err := store.LoadRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec2).To(BeNil())

				rec3, err := store.LoadRecord(tuple.Tuple{int64(3)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec3).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes all violating records when remainPrimaryKey is nil", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Quantity: proto.Int32(20), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				_, err = store.MarkIndexWriteOnly("Order$unique_qty")
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Quantity: proto.Int32(20), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(20)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(20)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				// Resolve with nil remainPrimaryKey: delete all.
				err = store.ResolveUniquenessViolationByDeletion(idx, tuple.Tuple{int64(20)}, nil)
				Expect(err).NotTo(HaveOccurred())

				rec1, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec1).To(BeNil())

				rec2, err := store.LoadRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec2).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("EstimateRecordsSizeInRange", func() {
		It("returns non-negative size for a store with records", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := store.EstimateRecordsSizeInRange(TupleRangeAll)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns non-negative size for a prefix range", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// Estimate for a specific PK prefix.
				size, err := store.EstimateRecordsSizeInRange(TupleRangeAllOf(tuple.Tuple{int64(3)}))
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns non-negative size for empty store", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				size, err := store.EstimateRecordsSizeInRange(TupleRangeAll)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("EstimateIndexSize", func() {
		It("returns non-negative size for a VALUE index with entries", func() {
			ks := specSubspace()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				idx := md.GetIndex("Order$price")
				size, err := store.EstimateIndexSize(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns non-negative size for an empty index", func() {
			ks := specSubspace()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$price")
				size, err := store.EstimateIndexSize(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ScanUniquenessViolationsForValue", func() {
		It("returns violations matching the given value key", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")

				// Add violations for two different value keys.
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(20)}, tuple.Tuple{int64(3)})).NotTo(HaveOccurred())

				// Scan for value=10: should find 2.
				violations, err := store.ScanUniquenessViolationsForValue(idx, tuple.Tuple{int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(2))

				pks := map[int64]bool{}
				for _, v := range violations {
					Expect(v.IndexName).To(Equal("Order$unique_qty"))
					Expect(v.IndexKey).To(Equal(tuple.Tuple{int64(10)}))
					pk := v.PrimaryKey[0].(int64)
					pks[pk] = true
				}
				Expect(pks).To(HaveKey(int64(1)))
				Expect(pks).To(HaveKey(int64(2)))

				// Scan for value=20: should find 1.
				violations, err = store.ScanUniquenessViolationsForValue(idx, tuple.Tuple{int64(20)})
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(1))
				Expect(violations[0].PrimaryKey).To(Equal(tuple.Tuple{int64(3)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty when no violations exist for the given value", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")

				// Add a violation for value=10 only.
				Expect(store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())

				// Scan for value=99: should be empty.
				violations, err := store.ScanUniquenessViolationsForValue(idx, tuple.Tuple{int64(99)})
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty when no violations exist at all", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")
				violations, err := store.ScanUniquenessViolationsForValue(idx, tuple.Tuple{int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("includes ExistingKey when stored with AddUniquenessViolationWithExisting", func() {
			ks := specSubspace()

			uniqueIdx := NewIndex("Order$unique_qty", Field("quantity"))
			uniqueIdx.SetUnique()

			builder := baseMetaData()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				idx := md.GetIndex("Order$unique_qty")
				Expect(store.AddUniquenessViolationWithExisting(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(2)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())

				violations, err := store.ScanUniquenessViolationsForValue(idx, tuple.Tuple{int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(1))
				Expect(violations[0].ExistingKey).To(Equal(tuple.Tuple{int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("AsBuilder and CopyBuilder", func() {
		It("AsBuilder creates a builder with the same config", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				ab := store.AsBuilder()
				Expect(ab).NotTo(BeNil())
				Expect(ab.metaData).To(Equal(md))
				Expect(ab.subspace).To(Equal(ks))
				Expect(ab.context).To(Equal(rtx))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("CopyBuilder creates a builder for a different context", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create the store first (committed transaction).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open in one transaction, then copy to another.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// Create a second context to copy to.
				_, err2 := sharedDB.Run(ctx, func(rtx2 *FDBRecordContext) (any, error) {
					cb := store.CopyBuilder(rtx2)
					Expect(cb).NotTo(BeNil())
					Expect(cb.metaData).To(Equal(md))
					Expect(cb.subspace).To(Equal(ks))
					Expect(cb.context).To(Equal(rtx2))
					Expect(cb.context).NotTo(Equal(rtx))

					// Open the store in the new context.
					store2, err := cb.Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store2).NotTo(BeNil())
					return nil, nil
				})
				Expect(err2).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IsVersionChanged", func() {
		It("returns false for initial open", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsVersionChanged()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns true when metadata version increases", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create the store initially.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Now open with a higher metadata version.
			builder2 := baseMetaData()
			builder2.SetVersion(md.Version() + 1)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsVersionChanged()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("error type formatting", func() {
		It("StoreIsLockedForRecordUpdatesError formats correctly", func() {
			e := &StoreIsLockedForRecordUpdatesError{Reason: "maintenance", Timestamp: 12345}
			Expect(e.Error()).To(ContainSubstring("maintenance"))
			Expect(e.Error()).To(ContainSubstring("12345"))
		})

		It("StoreIsFullyLockedError formats correctly", func() {
			e := &StoreIsFullyLockedError{Reason: "upgrade", Timestamp: 67890}
			Expect(e.Error()).To(ContainSubstring("upgrade"))
			Expect(e.Error()).To(ContainSubstring("67890"))
		})

		It("UnknownStoreLockStateError formats correctly", func() {
			e := &UnknownStoreLockStateError{LockStateValue: 99}
			Expect(e.Error()).To(ContainSubstring("99"))
		})

		It("StaleMetaDataVersionError formats correctly", func() {
			e := &StaleMetaDataVersionError{LocalVersion: 3, StoredVersion: 5}
			Expect(e.Error()).To(ContainSubstring("3"))
			Expect(e.Error()).To(ContainSubstring("5"))
		})
	})

	Describe("GetReadableUniversalIndexes and GetEnabledUniversalIndexes", func() {
		It("returns universal indexes in their respective states", func() {
			ks := specSubspace()
			builder := baseMetaData()
			idx := NewIndex("type_idx", RecordTypeKey())
			builder.AddUniversalIndex(idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				readable := store.GetReadableUniversalIndexes()
				Expect(readable).To(HaveLen(1))
				Expect(readable[0].Name).To(Equal("type_idx"))

				enabled := store.GetEnabledUniversalIndexes()
				Expect(enabled).To(HaveLen(1))
				Expect(enabled[0].Name).To(Equal("type_idx"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SetFormatVersion", func() {
		It("updates the store format version", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.SetFormatVersion(14)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IndexStateSubspace", func() {
		It("returns a subspace for index states", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				sub := store.IndexStateSubspace()
				Expect(sub).NotTo(BeNil())
				Expect(bytes.HasPrefix(sub.Bytes(), ks.Bytes())).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetAllIndexStates", func() {
		It("returns states for all indexes", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.AddIndex("Order", NewIndex("price_idx", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				states := store.GetAllIndexStates()
				Expect(states).NotTo(BeEmpty())
				// price_idx should be READABLE by default
				state, ok := states["price_idx"]
				Expect(ok).To(BeTrue())
				Expect(state).To(Equal(IndexStateReadable))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// collectPage drains a tuple cursor, returning all values and the raw
// continuation bytes (nil if source exhausted).
func collectPage(ctx context.Context, cursor RecordCursor[tuple.Tuple]) ([]tuple.Tuple, []byte, error) {
	var result []tuple.Tuple
	for {
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, nil, err
		}
		if !r.HasNext() {
			cont, contErr := r.GetContinuation().ToBytes()
			if contErr != nil {
				return nil, nil, contErr
			}
			return result, cont, nil
		}
		result = append(result, r.GetValue())
	}
}
