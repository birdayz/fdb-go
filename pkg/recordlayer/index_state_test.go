package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

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
		builder.AddIndex("Order", priceIndex)
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("Default state", func() {
		It("all indexes default to READABLE", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
		It("sets write-only state", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
		It("transitions from disabled back to readable", func() {
			ss := specSubspace()

			// Disable
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("Order$price")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Re-enable
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
	})

	Describe("ClearAndMarkIndexWriteOnly", func() {
		It("clears index data and sets write-only", func() {
			ss := specSubspace()

			// Save a record so the index has entries
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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

			// Mark readable and verify index is empty (data was cleared)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
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
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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

			// Re-enable index and scan — should be empty (no entries were created)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
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
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
				Expect(err.Error()).To(ContainSubstring("not readable"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error when scanning write-only index", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
				Expect(err.Error()).To(ContainSubstring("not readable"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Unknown index errors", func() {
		It("returns error for non-existent index", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexDisabled("nonexistent")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not found"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MarkIndexReadableOrUniquePending", func() {
		It("marks non-unique index as READABLE", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Disable first, then mark readable-or-unique-pending
				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())
				changed, err := store.MarkIndexReadableOrUniquePending("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable))
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
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithUnique).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				// Manually add a uniqueness violation entry
				idx := mdWithUnique.GetIndex("Order$unique_price")
				store.AddUniquenessViolation(idx, tuple.Tuple{int64(100)}, tuple.Tuple{int64(2)})

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
			builder.AddIndex("Order", uniqueIdx)
			mdWithUnique, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
				changed, err := store.MarkIndexReadableOrUniquePending("Order$unique_price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.GetIndexState("Order$unique_price")).To(Equal(IndexStateReadable))
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
})
