package recordlayer

import (
	"context"
	"errors"

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
