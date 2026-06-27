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

var _ = Describe("Store state management", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("GetRecordStoreState", func() {
		It("returns store header and index states", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				state := store.GetRecordStoreState()
				Expect(state).NotTo(BeNil())
				Expect(state.StoreHeader).NotTo(BeNil())
				Expect(state.IndexStates).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SetStoreLockState", func() {
		It("locks store and prevents record updates", func() {
			ss := specSubspace()

			// Create store and lock it
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "test lock")
			})
			Expect(err).NotTo(HaveOccurred())

			// Try to save — should fail
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
			Expect(err).To(HaveOccurred())
			var lockErr *StoreIsLockedForRecordUpdatesError
			Expect(errors.As(err, &lockErr)).To(BeTrue())
		})

		It("unlocks store by clearing lock state", func() {
			ss := specSubspace()

			// Create and lock
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "")
			})
			Expect(err).NotTo(HaveOccurred())

			// Unlock
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, store.ClearStoreLockState()
			})
			Expect(err).NotTo(HaveOccurred())

			// Save should work now
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
		})
	})

	Describe("FULL_STORE lock state", func() {
		It("prevents Open", func() {
			ss := specSubspace()

			// Create store and set FULL_STORE lock
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FULL_STORE, "maintenance")
			})
			Expect(err).NotTo(HaveOccurred())

			// Try to Open — should get StoreIsFullyLockedError
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var fullErr *StoreIsFullyLockedError
			Expect(errors.As(err, &fullErr)).To(BeTrue(),
				"expected StoreIsFullyLockedError, got: %v", err)
		})

		It("prevents CreateOrOpen", func() {
			ss := specSubspace()

			// Create store and set FULL_STORE lock
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FULL_STORE, "maintenance")
			})
			Expect(err).NotTo(HaveOccurred())

			// Try to CreateOrOpen — should get StoreIsFullyLockedError
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var fullErr *StoreIsFullyLockedError
			Expect(errors.As(err, &fullErr)).To(BeTrue(),
				"expected StoreIsFullyLockedError, got: %v", err)
		})

		It("bypass with matching reason succeeds", func() {
			ss := specSubspace()

			// Create store and set FULL_STORE lock with reason "maintenance"
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FULL_STORE, "maintenance")
			})
			Expect(err).NotTo(HaveOccurred())

			// Open with matching bypass reason — should succeed
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					SetBypassFullStoreLockReason("maintenance").Open()
				if err != nil {
					return nil, err
				}
				Expect(store).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("bypass with wrong reason fails", func() {
			ss := specSubspace()

			// Create store and set FULL_STORE lock with reason "maintenance"
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FULL_STORE, "maintenance")
			})
			Expect(err).NotTo(HaveOccurred())

			// Open with wrong bypass reason — should get StoreIsFullyLockedError
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					SetBypassFullStoreLockReason("wrong-reason").Open()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var fullErr *StoreIsFullyLockedError
			Expect(errors.As(err, &fullErr)).To(BeTrue(),
				"expected StoreIsFullyLockedError, got: %v", err)
		})

		It("ClearStoreLockState removes FULL_STORE lock", func() {
			ss := specSubspace()

			// Create store and set FULL_STORE lock
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FULL_STORE, "maintenance")
			})
			Expect(err).NotTo(HaveOccurred())

			// Bypass-open, clear lock, commit
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					SetBypassFullStoreLockReason("maintenance").Open()
				if err != nil {
					return nil, err
				}
				return nil, store.ClearStoreLockState()
			})
			Expect(err).NotTo(HaveOccurred())

			// Open normally without bypass — should succeed
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				Expect(store).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("validateRecordUpdateAllowed error precedence", func() {
		It("existence error takes priority over lock error on save", func() {
			ss := specSubspace()

			// Create store and lock it.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "")
			})
			Expect(err).NotTo(HaveOccurred())

			// Try SaveRecordWithOptions(ERROR_IF_NOT_EXISTS) on locked store
			// for a record that doesn't exist.
			// Should get RecordDoesNotExistError, NOT StoreIsLockedForRecordUpdatesError.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordWithOptions(
					&gen.Order{OrderId: proto.Int64(999), Price: proto.Int32(100)},
					RecordExistenceCheckErrorIfNotExists,
				)
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var doesNotExist *RecordDoesNotExistError
			Expect(errors.As(err, &doesNotExist)).To(BeTrue(),
				"expected RecordDoesNotExistError, got: %v", err)
		})

		It("delete on non-existent record returns false without lock error", func() {
			ss := specSubspace()

			// Create store and lock it.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "")
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete non-existent record — should return (false, nil),
			// NOT StoreIsLockedForRecordUpdatesError.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(999)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ReloadRecordStoreState", func() {
		It("reloads state from FDB", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				err = store.ReloadRecordStoreState()
				Expect(err).NotTo(HaveOccurred())
				state := store.GetRecordStoreState()
				Expect(state.StoreHeader).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("EstimateStoreSize", func() {
		It("returns a non-negative estimate", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Save some data
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				size, err := store.EstimateStoreSize()
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("EstimateRecordsSize", func() {
		It("returns a non-negative estimate", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				size, err := store.EstimateRecordsSize()
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Uniqueness violation tracking", func() {
		It("records and scans violations", func() {
			priceIndex := NewIndex("Order$price", Field("price")).SetUnique()

			idxBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			idxBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			idxBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			idxBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idxBuilder.AddIndex("Order", priceIndex)
			idxMd, err := idxBuilder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(idxMd).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Manually add a violation
				Expect(store.AddUniquenessViolation(priceIndex, tuple.Tuple{int64(500)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(priceIndex, tuple.Tuple{int64(500)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				violations, err := store.ScanUniquenessViolations(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(2))
				Expect(violations[0].IndexName).To(Equal("Order$price"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("resolves violations", func() {
			priceIndex := NewIndex("Order$price", Field("price")).SetUnique()

			idxBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			idxBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			idxBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			idxBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idxBuilder.AddIndex("Order", priceIndex)
			idxMd, err := idxBuilder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(idxMd).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				Expect(store.AddUniquenessViolation(priceIndex, tuple.Tuple{int64(500)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())
				Expect(store.AddUniquenessViolation(priceIndex, tuple.Tuple{int64(500)}, tuple.Tuple{int64(2)})).NotTo(HaveOccurred())

				// Resolve one
				Expect(store.ResolveUniquenessViolation(priceIndex, tuple.Tuple{int64(500)}, tuple.Tuple{int64(1)})).NotTo(HaveOccurred())

				violations, err := store.ScanUniquenessViolations(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(1))
				Expect(violations[0].PrimaryKey).To(Equal(tuple.Tuple{int64(2)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty for no violations", func() {
			priceIndex := NewIndex("Order$price", Field("price")).SetUnique()

			idxBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			idxBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			idxBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			idxBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idxBuilder.AddIndex("Order", priceIndex)
			idxMd, err := idxBuilder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(idxMd).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				violations, err := store.ScanUniquenessViolations(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SetTransactionPriority", func() {
		It("sets batch priority without error", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, rtx.SetTransactionPriority(PriorityBatch)
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets default priority without error", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, rtx.SetTransactionPriority(PriorityDefault)
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
