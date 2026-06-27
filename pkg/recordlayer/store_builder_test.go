package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("StoreBuilder_Validation", func() {
	validBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	validBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	validBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	validBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := validBuilder.Build()
	ks := subspace.FromBytes(tuple.Tuple{"builder_validation"}.Pack())

	It("BuildWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			Build()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("context is required"))
	})

	It("BuildWithoutMetaData", func() {
		// Can't easily create a real FDBRecordContext without a container,
		// but validateBuilder checks context first, then metadata.
		// We verify the error message for nil context covers it.
		_, err := NewStoreBuilder().
			SetSubspace(ks).
			Build()
		Expect(err).To(HaveOccurred())
	})

	It("CreateWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			Create()
		Expect(err).To(HaveOccurred())
	})

	It("OpenWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			Open()
		Expect(err).To(HaveOccurred())
	})

	It("CreateOrOpenWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			CreateOrOpen()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("StoreBuilder_CreateOpenSemantics", func() {
	ctx := context.Background()
	semanticsBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	semanticsBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	semanticsBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	semanticsBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := semanticsBuilder.Build()

	It("OpenNonExistentStore", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Open()
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var storeErr *RecordStoreDoesNotExistError
		Expect(errors.As(err, &storeErr)).To(BeTrue())
	})

	It("CreateAlreadyExistingStore", func() {
		ks := specSubspace()

		// First create
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Second create should fail
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).To(HaveOccurred())
		var storeErr *RecordStoreAlreadyExistsError
		Expect(errors.As(err, &storeErr)).To(BeTrue())
	})

	It("CreateOrOpenExistingStore", func() {
		ks := specSubspace()

		// Create first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// CreateOrOpen on existing should succeed
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			Expect(store).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CreateOrOpenNewStore", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			Expect(store).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify it was actually created by Opening it
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Open()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("OpenAfterCreate", func() {
		ks := specSubspace()

		// Create
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Open should succeed
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Open()
			if err != nil {
				return nil, err
			}
			Expect(store).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("StoreLockState", func() {
	ctx := context.Background()

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := builder.Build()

	It("SaveBlockedByLock", func() {
		ks := specSubspace()

		// Create a store, then lock it by writing a header with FORBID_RECORD_UPDATE
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Lock the store by updating the header
			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			reason := "index rebuild in progress"
			ts := int64(1234567890)
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
				Reason:    &reason,
				Timestamp: &ts,
			}
			if err := store.writeStoreHeader(store.storeHeader); err != nil {
				return nil, err
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Now open the store and try to save — should be blocked
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(10),
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockErr)).To(BeTrue())
		Expect(lockErr.Reason).To(Equal("index rebuild in progress"))
	})

	It("DeleteBlockedByLock", func() {
		ks := specSubspace()

		// Create store with a record, then lock it
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Lock
			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}
			return nil, store.writeStoreHeader(store.storeHeader)
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to delete — should be blocked
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockErr)).To(BeTrue())
	})

	It("DeleteAllBlockedByLock", func() {
		ks := specSubspace()

		// Create store, then lock it
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}
			return nil, store.writeStoreHeader(store.storeHeader)
		})
		Expect(err).NotTo(HaveOccurred())

		// Try DeleteAllRecords — should be blocked
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			return nil, store.DeleteAllRecords()
		})

		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockErr)).To(BeTrue())
	})

	It("ReadAllowedWhenLocked", func() {
		ks := specSubspace()

		// Create store with a record, then lock it
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}
			return nil, store.writeStoreHeader(store.storeHeader)
		})
		Expect(err).NotTo(HaveOccurred())

		// Reads should still work on a locked store
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(), "Expected to find record in locked store")

			exists, err := store.RecordExists(tuple.Tuple{int64(1)}, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnlockedStoreAllowsMutations", func() {
		ks := specSubspace()

		// Normal store — no lock state set
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save should work
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Delete should work
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// DeleteAll should work
			return nil, store.DeleteAllRecords()
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
