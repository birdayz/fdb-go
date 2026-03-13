package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
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
			idx3 := NewIndex("Order$flower_type", Field("flower.type"))

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
				store.AddUniquenessViolation(idx, tuple.Tuple{int64(10)}, tuple.Tuple{int64(2)})
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
			idx3 := NewIndex("Order$flower_type", Field("flower.type"))

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
			idx3 := NewIndex("Order$flower_type", Field("flower.type"))

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
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Verify build artifacts exist, then vacuum.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				// Build stamp should exist before vacuum.
				idx := mdWithIdx.GetIndex("Order$price")
				stamp, err := store.LoadIndexingTypeStamp(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())

				// Vacuum.
				store.VacuumReadableIndexesBuildData()

				// After vacuum, stamp and range data should be cleared.
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
				Expect(header.GetFormatVersion()).To(BeNumerically(">=", FormatVersionMinimum))
				Expect(header.GetFormatVersion()).To(BeNumerically("<=", FormatVersionCurrent))

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

		It("detects lock state without writing", func() {
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

			// DryRun should fail with lock error.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.DryRunSaveRecord(
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
					RecordExistenceCheckNone,
				)
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var lockErr *StoreIsLockedForRecordUpdatesError
			Expect(errors.As(err, &lockErr)).To(BeTrue())
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
})
