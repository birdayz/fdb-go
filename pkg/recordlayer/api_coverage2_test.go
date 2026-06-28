package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("API Coverage 2", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	Describe("GetTypedRecordStore", func() {
		It("creates a typed store and saves/loads through it", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				typedStore, err := GetTypedRecordStore[*gen.Order](store, "Order")
				Expect(err).NotTo(HaveOccurred())
				Expect(typedStore).NotTo(BeNil())

				order := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)}
				saved, err := typedStore.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
				Expect(saved).NotTo(BeNil())

				loaded, err := typedStore.LoadRecord(tuple.Tuple{int64(42)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				Expect(loaded.Record.GetOrderId()).To(Equal(int64(42)))
				Expect(loaded.Record.GetPrice()).To(Equal(int32(999)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for non-existent record type", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, typedErr := GetTypedRecordStore[*gen.Order](store, "DoesNotExist")
				Expect(typedErr).To(HaveOccurred())
				var mdErr *MetaDataError
				Expect(errors.As(typedErr, &mdErr)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil store", func() {
			Expect(func() {
				_, _ = GetTypedRecordStore[*gen.Order](nil, "Order")
			}).To(Panic())
		})
	})

	Describe("ClearHeaderUserField", func() {
		It("sets then clears a user field", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.SetHeaderUserField("myKey", []byte("myValue"))
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetHeaderUserField("myKey")).To(Equal([]byte("myValue")))

				err = store.ClearHeaderUserField("myKey")
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetHeaderUserField("myKey")).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("persists clear across reopen", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Set the field.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.SetHeaderUserField("persistKey", []byte("hello"))
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Clear and reopen.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetHeaderUserField("persistKey")).To(Equal([]byte("hello")))
				err = store.ClearHeaderUserField("persistKey")
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Reopen and verify cleared.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetHeaderUserField("persistKey")).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clearing non-existent key is a no-op", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.ClearHeaderUserField("neverSet")
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetHeaderUserField("neverSet")).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetRangeSplitPoints", func() {
		It("returns no error on empty store", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				points, err := store.GetRangeSplitPoints(1000)
				Expect(err).NotTo(HaveOccurred())
				// Empty store may return empty or nil — just ensure no error.
				_ = points
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns no error on store with records", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 20; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}

				points, err := store.GetRangeSplitPoints(100)
				Expect(err).NotTo(HaveOccurred())
				// FDB may return empty for small data sets — just verify no error.
				_ = points
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetRecordMetaData", func() {
		It("returns the metadata we set", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				got := store.GetRecordMetaData()
				Expect(got).NotTo(BeNil())
				Expect(got).To(BeIdenticalTo(md))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("metadata contains expected record types", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				got := store.GetRecordMetaData()
				types := got.RecordTypes()
				Expect(types).To(HaveKey("Order"))
				Expect(types).To(HaveKey("Customer"))
				Expect(types).To(HaveKey("TypedRecord"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetContext", func() {
		It("returns the context we set", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				got := store.GetContext()
				Expect(got).NotTo(BeNil())
				Expect(got).To(BeIdenticalTo(rtx))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("context has a valid transaction", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				got := store.GetContext()
				// The transaction should be the same as the one from rtx.
				Expect(got.Transaction()).To(Equal(rtx.Transaction()))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetSubspace", func() {
		It("returns the subspace we set", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				got := store.GetSubspace()
				Expect(got).NotTo(BeNil())
				Expect(got.Bytes()).To(Equal(ks.Bytes()))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetStoreHeader", func() {
		It("returns non-nil header for opened store", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				header := store.GetStoreHeader()
				Expect(header).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns a clone — modifying it does not affect the store", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				header1 := store.GetStoreHeader()
				Expect(header1).NotTo(BeNil())

				// Mutate the returned header.
				header1.FormatVersion = proto.Int32(9999)

				// Re-read from store — should be unchanged.
				header2 := store.GetStoreHeader()
				Expect(header2.GetFormatVersion()).NotTo(Equal(int32(9999)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("header has format version set", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				header := store.GetStoreHeader()
				Expect(header.GetFormatVersion()).To(BeNumerically(">", 0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IsCacheable", func() {
		It("is false by default for new store", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsCacheable()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("becomes true after SetStateCacheability(true)", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				changed, err := store.SetStateCacheability(true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.IsCacheable()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("VacuumReadableIndexesBuildData", func() {
		It("clears manually-inserted range set data for a readable index", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price_vac", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store with index (auto-built inline since new store).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Manually insert range set data to simulate leftover build artifacts.
				rangeSet := NewIndexingRangeSet(ks, priceIdx)
				_, err = rangeSet.InsertRange(rtx.Transaction(), []byte{0x00}, []byte{0xff}, false)
				Expect(err).NotTo(HaveOccurred())

				// Verify range set is now complete.
				complete, err := rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete).To(BeTrue())

				// Index should be READABLE (auto-built).
				Expect(store.IsIndexReadable("Order$price_vac")).To(BeTrue())

				// Vacuum should clear it.
				store.VacuumReadableIndexesBuildData()

				// After vacuum, range set should be cleared.
				complete2, err := rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete2).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("is safe on store with no indexes", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Should not panic on store with no indexes.
				Expect(func() {
					store.VacuumReadableIndexesBuildData()
				}).NotTo(Panic())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not clear range set for non-readable indexes", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price_vac2", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Mark index as WRITE_ONLY.
				_, err = store.MarkIndexWriteOnly("Order$price_vac2")
				Expect(err).NotTo(HaveOccurred())

				// Manually insert range set data.
				rangeSet := NewIndexingRangeSet(ks, priceIdx)
				_, err = rangeSet.InsertRange(rtx.Transaction(), []byte{0x00}, []byte{0xff}, false)
				Expect(err).NotTo(HaveOccurred())

				// Vacuum should NOT clear it (index is WRITE_ONLY, not READABLE).
				store.VacuumReadableIndexesBuildData()

				// Range set should still be complete.
				complete, err := rangeSet.IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("DeleteStore", func() {
		It("removes all data in subspace", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store and save a record.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete the store.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, DeleteStore(rtx, ks)
			})
			Expect(err).NotTo(HaveOccurred())

			// Opening should fail (no header) or Create should work from scratch.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Opening a deleted store should fail (no store info).
				_, openErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(openErr).To(HaveOccurred())

				// But CreateOrOpen should succeed (re-creates).
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// The previously saved record should be gone.
				exists, err := store.RecordExists(tuple.Tuple{int64(1)}, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("is idempotent on already-empty subspace", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, DeleteStore(rtx, ks)
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetAllIndexStatesMap", func() {
		It("returns empty map when all indexes are READABLE", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price_states", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				states := store.GetAllIndexStatesMap()
				// All READABLE => non-READABLE map should be empty.
				Expect(states).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns non-empty map when an index is WRITE_ONLY", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price_states2", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				changed, err := store.MarkIndexWriteOnly("Order$price_states2")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				states := store.GetAllIndexStatesMap()
				Expect(states).To(HaveKey("Order$price_states2"))
				Expect(states["Order$price_states2"]).To(Equal(IndexStateWriteOnly))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns a copy — modifying it does not affect the store", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price_states3", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.MarkIndexWriteOnly("Order$price_states3")
				Expect(err).NotTo(HaveOccurred())

				states := store.GetAllIndexStatesMap()
				states["Order$price_states3"] = IndexStateDisabled // mutate the copy

				// Re-read from store — should still be WRITE_ONLY.
				states2 := store.GetAllIndexStatesMap()
				Expect(states2["Order$price_states3"]).To(Equal(IndexStateWriteOnly))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("NewScanProperties", func() {
		It("defaults to forward scan", func() {
			sp := NewScanProperties(DefaultExecuteProperties())
			Expect(sp.IsReverse()).To(BeFalse())
		})

		It("carries execute properties through", func() {
			ep := DefaultExecuteProperties().WithReturnedRowLimit(42)
			sp := NewScanProperties(ep)
			Expect(sp.GetExecuteProperties().ReturnedRowLimit).To(Equal(42))
		})

		It("can be overridden with reverse", func() {
			sp := NewScanProperties(DefaultExecuteProperties()).WithReverse(true)
			Expect(sp.IsReverse()).To(BeTrue())
		})
	})

	Describe("GetNestedExpression", func() {
		It("returns nil for non-RecordTypeKeyExpression", func() {
			field := Field("order_id")
			nested := GetNestedExpression(field)
			Expect(nested).To(BeNil())
		})

		It("returns nil for bare RecordTypeKeyExpression without nesting", func() {
			rtk := RecordTypeKey()
			nested := GetNestedExpression(rtk)
			Expect(nested).To(BeNil())
		})

		It("returns nested expression after Nest()", func() {
			inner := Field("order_id")
			rtk := RecordTypeKey()
			rtk.Nest(inner)

			nested := GetNestedExpression(rtk)
			Expect(nested).NotTo(BeNil())
			// The nested expression should be the same field expression we set.
			Expect(nested).To(BeIdenticalTo(inner))
		})
	})

	Describe("RecordMetaData accessors", func() {
		It("RecordTypes returns all defined types", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			types := md.RecordTypes()
			Expect(types).To(HaveKey("Order"))
			Expect(types).To(HaveKey("Customer"))
			Expect(types).To(HaveKey("TypedRecord"))
			Expect(types).To(HaveLen(3))
		})

		It("Version returns builder version", func() {
			builder := baseMetaData()
			builder.SetVersion(42)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.Version()).To(Equal(42))
		})

		It("GetRecordCountKey returns nil when not set", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetRecordCountKey()).To(BeNil())
		})

		It("GetRecordCountKey returns expression when set", func() {
			builder := baseMetaData()
			builder.SetRecordCountKey(EmptyKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetRecordCountKey()).NotTo(BeNil())
		})

		It("IsStoreRecordVersions defaults to false", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.IsStoreRecordVersions()).To(BeFalse())
		})

		It("IsStoreRecordVersions returns true when set", func() {
			builder := baseMetaData()
			builder.SetStoreRecordVersions(true)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.IsStoreRecordVersions()).To(BeTrue())
		})

		It("IsSplitLongRecords defaults to false", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.IsSplitLongRecords()).To(BeFalse())
		})

		It("IsSplitLongRecords returns true when set", func() {
			builder := baseMetaData()
			builder.SetSplitLongRecords(true)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.IsSplitLongRecords()).To(BeTrue())
		})

		It("GetUniversalIndexes returns empty when none added", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetUniversalIndexes()).To(BeEmpty())
		})

		It("GetUniversalIndexes returns added universal indexes", func() {
			builder := baseMetaData()
			uIdx := NewIndex("global_count", EmptyKey())
			uIdx.Type = "COUNT"
			builder.AddUniversalIndex(uIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			unis := md.GetUniversalIndexes()
			Expect(unis).To(HaveLen(1))
			Expect(unis[0].Name).To(Equal("global_count"))
		})

		It("HasIndexes returns false when no indexes", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.HasIndexes()).To(BeFalse())
		})

		It("HasIndexes returns true when indexes exist", func() {
			builder := baseMetaData()
			builder.AddIndex("Order", NewIndex("Order$price_has", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.HasIndexes()).To(BeTrue())
		})

		It("GetIndex returns index by name", func() {
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price_get", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			got := md.GetIndex("Order$price_get")
			Expect(got).NotTo(BeNil())
			Expect(got.Name).To(Equal("Order$price_get"))
		})

		It("GetIndex returns nil for unknown name", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetIndex("nonexistent")).To(BeNil())
		})

		It("GetAllIndexes returns all indexes", func() {
			builder := baseMetaData()
			builder.AddIndex("Order", NewIndex("idx_a", Field("price")))
			builder.AddUniversalIndex(NewIndex("idx_b", EmptyKey()))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			all := md.GetAllIndexes()
			Expect(all).To(HaveKey("idx_a"))
			Expect(all).To(HaveKey("idx_b"))
			Expect(all).To(HaveLen(2))
		})

		It("GetFormerIndexes returns empty when none removed", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetFormerIndexes()).To(BeEmpty())
		})

		It("GetFormerIndexes tracks removed indexes", func() {
			builder := baseMetaData()
			builder.AddIndex("Order", NewIndex("idx_removed", Field("price")))
			builder.RemoveIndex("idx_removed")
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			former := md.GetFormerIndexes()
			Expect(former).To(HaveLen(1))
			Expect(former[0].FormerName).To(Equal("idx_removed"))
		})

		It("GetIndexesForRecordType returns type-specific indexes", func() {
			builder := baseMetaData()
			builder.AddIndex("Order", NewIndex("idx_order_price", Field("price")))
			builder.AddIndex("Customer", NewIndex("idx_cust_name", Field("name")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderIndexes := md.GetIndexesForRecordType("Order")
			Expect(orderIndexes).To(HaveLen(1))
			Expect(orderIndexes[0].Name).To(Equal("idx_order_price"))

			custIndexes := md.GetIndexesForRecordType("Customer")
			Expect(custIndexes).To(HaveLen(1))
			Expect(custIndexes[0].Name).To(Equal("idx_cust_name"))
		})

		It("PrimaryKeyHasRecordTypePrefix returns false for simple PK", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.PrimaryKeyHasRecordTypePrefix()).To(BeFalse())
		})
	})

	Describe("RecordMetaDataBuilder", func() {
		It("SetRecords sets the proto file descriptor", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			// Verify we can get record types from it.
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.RecordTypes()).To(HaveLen(3))
		})

		It("EnableCounterBasedSubspaceKeys assigns int64 subspace keys", func() {
			builder := baseMetaData()
			builder.EnableCounterBasedSubspaceKeys()
			idx1 := NewIndex("idx_counter_a", Field("price"))
			idx2 := NewIndex("idx_counter_b", Field("order_id"))
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			a := md.GetIndex("idx_counter_a")
			b := md.GetIndex("idx_counter_b")
			Expect(a).NotTo(BeNil())
			Expect(b).NotTo(BeNil())

			// Counter-based keys should be int64, not strings.
			_, aIsInt := a.SubspaceTupleKey().(int64)
			_, bIsInt := b.SubspaceTupleKey().(int64)
			Expect(aIsInt).To(BeTrue(), "expected int64 subspace key for idx_counter_a, got %T", a.SubspaceTupleKey())
			Expect(bIsInt).To(BeTrue(), "expected int64 subspace key for idx_counter_b, got %T", b.SubspaceTupleKey())

			// They should be distinct.
			Expect(a.SubspaceTupleKey()).NotTo(Equal(b.SubspaceTupleKey()))
		})

		It("without EnableCounterBasedSubspaceKeys, indexes get string keys", func() {
			builder := baseMetaData()
			idx := NewIndex("idx_string_key", Field("price"))
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			got := md.GetIndex("idx_string_key")
			Expect(got).NotTo(BeNil())
			_, isString := got.SubspaceTupleKey().(string)
			Expect(isString).To(BeTrue(), "expected string subspace key, got %T", got.SubspaceTupleKey())
		})
	})
})
