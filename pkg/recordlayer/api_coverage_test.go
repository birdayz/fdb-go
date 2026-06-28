package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("API Coverage", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	saveOrder := func(store *FDBRecordStore, orderID int64) {
		order := &gen.Order{
			OrderId: proto.Int64(orderID),
			Price:   proto.Int32(100),
		}
		_, err := store.SaveRecord(order)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("AddRecordReadConflict", func() {
		It("succeeds with a valid primary key", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				saveOrder(store, 1)

				err = store.AddRecordReadConflict(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not panic with nil primary key", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// nil PK should not panic — may error or succeed, but must not crash
				Expect(func() {
					_ = store.AddRecordReadConflict(nil)
				}).NotTo(Panic())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("AddRecordWriteConflict", func() {
		It("succeeds with a valid primary key", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				saveOrder(store, 2)

				err = store.AddRecordWriteConflict(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not panic with nil primary key", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(func() {
					_ = store.AddRecordWriteConflict(nil)
				}).NotTo(Panic())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetIncarnation", func() {
		It("returns 0 for a newly created store", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetIncarnation()).To(Equal(int32(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns updated value after UpdateIncarnation", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.UpdateIncarnation(func(current int32) int32 { return current + 1 })
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetIncarnation()).To(Equal(int32(1)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("UpdateIncarnation", func() {
		It("increments incarnation and persists across reopen", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Set incarnation to 1
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.UpdateIncarnation(func(current int32) int32 { return current + 1 })
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Reopen and verify it persisted
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.GetIncarnation()).To(Equal(int32(1)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil updater", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.UpdateIncarnation(nil)
				Expect(err).To(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects non-increasing incarnation", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Same value (0 -> 0) should be rejected
				err = store.UpdateIncarnation(func(current int32) int32 { return current })
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("incarnation must increase"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("accumulates multiple increments", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int32(1); i <= 5; i++ {
					err = store.UpdateIncarnation(func(current int32) int32 { return current + 1 })
					Expect(err).NotTo(HaveOccurred())
				}

				Expect(store.GetIncarnation()).To(Equal(int32(5)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Nil index guards", func() {
		It("GetIndexMaintainer returns nil for nil index", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				m, mErr := store.GetIndexMaintainer(nil)
				Expect(mErr).NotTo(HaveOccurred())
				Expect(m).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("ScanUniquenessViolations returns error for nil index", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, scanErr := store.ScanUniquenessViolations(nil)
				Expect(scanErr).To(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RebuildIndex returns error for nil index", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				rebuildErr := store.RebuildIndex(nil)
				Expect(rebuildErr).To(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("DeleteIndexEntries does not panic for nil index", func() {
			ks := specSubspace()
			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(func() {
					store.DeleteIndexEntries(nil)
				}).NotTo(Panic())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
