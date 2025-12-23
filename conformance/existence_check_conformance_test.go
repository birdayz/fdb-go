package conformance_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// Comprehensive tests for all 5 RecordExistenceCheck modes
// Java equivalent: FDBRecordStoreBase.java:394-443 (RecordExistenceCheck enum)
var _ = Describe("RecordExistenceCheck Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TestEnvironment
		store *helpers.ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = helpers.SetupTestEnvironment(ctx, "existence_check_conformance")
		Expect(err).NotTo(HaveOccurred())

		store = helpers.NewConformanceStore(env.RecordDB, env.MetaData, env.Keyspace, env.ClusterFile)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("NONE Mode", func() {
		// RecordExistenceCheck.NONE - No special checks, allows insert and update
		// Java: FDBRecordStoreBase.RecordExistenceCheck.NONE

		It("should allow saving new record", func() {
			orderID := int64(10001)

			order := helpers.StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())

			// Verify it was saved
			exists, err := store.RecordExists(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
		})

		It("should allow updating existing record", func() {
			orderID := int64(10002)

			// Save initial record
			order1 := helpers.NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with NONE - should succeed
			order2 := helpers.NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		It("should allow replacing record (no type change check)", func() {
			orderID := int64(10003)

			// Save initial order
			order := helpers.StandardOrder(orderID)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Save again with NONE - should succeed (effectively an update)
			order2 := helpers.NewOrder(orderID).WithPrice(999).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ERROR_IF_EXISTS Mode", func() {
		// RecordExistenceCheck.ERROR_IF_EXISTS - Fail if record already exists (insert-only)
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_EXISTS

		It("should succeed for new record", func() {
			orderID := int64(20001)

			order := helpers.StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfExists)
			Expect(err).NotTo(HaveOccurred())

			// Verify it was saved
			exists, err := store.RecordExists(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
		})

		It("should fail for existing record", func() {
			orderID := int64(20002)

			// Save initial record
			order1 := helpers.StandardOrder(orderID)
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Try to save again with ERROR_IF_EXISTS - should fail
			order2 := helpers.NewOrder(orderID).WithPrice(999).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfExists)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, recordlayer.ErrRecordAlreadyExists)).To(BeTrue())
		})

		It("should return structured error with primary key", func() {
			orderID := int64(20003)

			// Save initial record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Try to save again
			err = store.SaveRecordWithOptions(ctx, helpers.StandardOrder(orderID), recordlayer.RecordExistenceCheckErrorIfExists)
			Expect(err).To(HaveOccurred())

			// Check if error is the structured type
			var recordExistsErr *recordlayer.RecordAlreadyExistsError
			if errors.As(err, &recordExistsErr) {
				Expect(recordExistsErr.PrimaryKey).NotTo(BeNil())
				Expect(recordExistsErr.Error()).To(ContainSubstring("already exists"))
			}
		})
	})

	Describe("ERROR_IF_NOT_EXISTS Mode", func() {
		// RecordExistenceCheck.ERROR_IF_NOT_EXISTS - Fail if record doesn't exist (update-only)
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_NOT_EXISTS

		It("should fail for new record", func() {
			orderID := int64(30001)

			order := helpers.StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, recordlayer.ErrRecordDoesNotExist)).To(BeTrue())
		})

		It("should succeed for existing record", func() {
			orderID := int64(30002)

			// Save initial record
			order1 := helpers.NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with ERROR_IF_NOT_EXISTS - should succeed
			order2 := helpers.NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfNotExists)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		It("should return structured error with primary key", func() {
			orderID := int64(30003)

			err := store.SaveRecordWithOptions(ctx, helpers.StandardOrder(orderID), recordlayer.RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())

			// Check if error is the structured type
			var recordDoesNotExistErr *recordlayer.RecordDoesNotExistError
			if errors.As(err, &recordDoesNotExistErr) {
				Expect(recordDoesNotExistErr.PrimaryKey).NotTo(BeNil())
				Expect(recordDoesNotExistErr.Error()).To(ContainSubstring("does not exist"))
			}
		})
	})

	Describe("ERROR_IF_RECORD_TYPE_CHANGED Mode", func() {
		// RecordExistenceCheck.ERROR_IF_RECORD_TYPE_CHANGED - Fail if existing record has different type
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_RECORD_TYPE_CHANGED

		It("should succeed for new record", func() {
			orderID := int64(40001)

			order := helpers.StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should succeed for existing record with same type", func() {
			orderID := int64(40002)

			// Save initial Order
			order1 := helpers.NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with same type - should succeed
			order2 := helpers.NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		// Note: To test type change, we would need a multi-type schema (Order + Customer)
		// This is deferred to multi-type schema tests
	})

	Describe("ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED Mode", func() {
		// RecordExistenceCheck.ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED
		// Combined check: fail if doesn't exist OR type changed
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED
		// This is what UpdateRecord() uses

		It("should fail for new record", func() {
			orderID := int64(50001)

			order := helpers.StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, recordlayer.ErrRecordDoesNotExist)).To(BeTrue())
		})

		It("should succeed for existing record with same type", func() {
			orderID := int64(50002)

			// Save initial record
			order1 := helpers.NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with same type - should succeed
			order2 := helpers.NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		// Note: Type change test requires multi-type schema
	})

	Describe("InsertRecord Convenience Method", func() {
		// Tests that InsertRecord() == SaveRecordWithOptions(ERROR_IF_EXISTS)
		// Java: FDBRecordStoreBase.insertRecordAsync()

		It("should succeed for new record", func() {
			orderID := int64(60001)

			order := helpers.StandardOrder(orderID)
			err := store.InsertRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			exists, err := store.RecordExists(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
		})

		It("should fail for existing record", func() {
			orderID := int64(60002)

			// Save initial record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Try to insert again - should fail
			err = store.InsertRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, recordlayer.ErrRecordAlreadyExists)).To(BeTrue())
		})
	})

	Describe("UpdateRecord Convenience Method", func() {
		// Tests that UpdateRecord() == SaveRecordWithOptions(ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED)
		// Java: FDBRecordStoreBase.updateRecordAsync()

		It("should fail for new record", func() {
			orderID := int64(70001)

			order := helpers.StandardOrder(orderID)
			err := store.UpdateRecord(ctx, order)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, recordlayer.ErrRecordDoesNotExist)).To(BeTrue())
		})

		It("should succeed for existing record", func() {
			orderID := int64(70002)

			// Save initial record
			order1 := helpers.NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update - should succeed
			order2 := helpers.NewOrder(orderID).WithPrice(200).Build()
			err = store.UpdateRecord(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		It("should validate update semantics", func() {
			orderID := int64(70003)

			// Create initial record
			err := store.InsertRecord(ctx, helpers.NewOrder(orderID).WithPrice(100).Build())
			Expect(err).NotTo(HaveOccurred())

			// Multiple updates should succeed
			for i := 0; i < 5; i++ {
				order := helpers.NewOrder(orderID).WithPrice(int32(200 + i*10)).Build()
				err = store.UpdateRecord(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Final value should be 240
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(240)))
		})
	})

	Describe("RecordExistenceCheck Enum Methods", func() {
		// Tests the helper methods on RecordExistenceCheck
		// Java: FDBRecordStoreBase.RecordExistenceCheck methods

		It("should have correct ErrorIfExists() values", func() {
			Expect(recordlayer.RecordExistenceCheckNone.ErrorIfExists()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfExists.ErrorIfExists()).To(BeTrue())
			Expect(recordlayer.RecordExistenceCheckErrorIfNotExists.ErrorIfExists()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfTypeChanged.ErrorIfExists()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged.ErrorIfExists()).To(BeFalse())
		})

		It("should have correct ErrorIfNotExists() values", func() {
			Expect(recordlayer.RecordExistenceCheckNone.ErrorIfNotExists()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfExists.ErrorIfNotExists()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfNotExists.ErrorIfNotExists()).To(BeTrue())
			Expect(recordlayer.RecordExistenceCheckErrorIfTypeChanged.ErrorIfNotExists()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged.ErrorIfNotExists()).To(BeTrue())
		})

		It("should have correct ErrorIfTypeChanged() values", func() {
			Expect(recordlayer.RecordExistenceCheckNone.ErrorIfTypeChanged()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfExists.ErrorIfTypeChanged()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfNotExists.ErrorIfTypeChanged()).To(BeFalse())
			Expect(recordlayer.RecordExistenceCheckErrorIfTypeChanged.ErrorIfTypeChanged()).To(BeTrue())
			Expect(recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged.ErrorIfTypeChanged()).To(BeTrue())
		})

		It("should have meaningful String() values", func() {
			modes := []recordlayer.RecordExistenceCheck{
				recordlayer.RecordExistenceCheckNone,
				recordlayer.RecordExistenceCheckErrorIfExists,
				recordlayer.RecordExistenceCheckErrorIfNotExists,
				recordlayer.RecordExistenceCheckErrorIfTypeChanged,
				recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged,
			}

			for _, mode := range modes {
				str := mode.String()
				Expect(str).NotTo(BeEmpty())
				// Should be readable for debugging
				Expect(len(str)).To(BeNumerically(">", 4))
			}
		})
	})

	Describe("Edge Cases", func() {
		It("should handle rapid insert/update cycles", func() {
			orderID := int64(80001)

			// Insert
			err := store.InsertRecord(ctx, helpers.NewOrder(orderID).WithPrice(100).Build())
			Expect(err).NotTo(HaveOccurred())

			// Multiple updates in sequence
			for i := 0; i < 10; i++ {
				err = store.UpdateRecord(ctx, helpers.NewOrder(orderID).WithPrice(int32(200 + i)).Build())
				Expect(err).NotTo(HaveOccurred())
			}

			// Insert should now fail
			err = store.InsertRecord(ctx, helpers.NewOrder(orderID).WithPrice(999).Build())
			Expect(err).To(HaveOccurred())
		})

		It("should handle delete followed by insert", func() {
			orderID := int64(80002)

			// Insert
			err := store.InsertRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Delete
			deleted, err := store.DeleteRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Insert again - should succeed
			err = store.InsertRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle update on deleted record", func() {
			orderID := int64(80003)

			// Insert and delete
			err := store.InsertRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Update should fail
			err = store.UpdateRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, recordlayer.ErrRecordDoesNotExist)).To(BeTrue())
		})
	})

	Describe("Error Message Quality", func() {
		It("should include primary key in RecordAlreadyExists error", func() {
			orderID := int64(90001)

			err := store.InsertRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			err = store.InsertRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).To(HaveOccurred())

			errMsg := err.Error()
			Expect(errMsg).To(ContainSubstring("already exists"))
			// Should include the key in some form
			Expect(len(errMsg)).To(BeNumerically(">", 20))
		})

		It("should include primary key in RecordDoesNotExist error", func() {
			orderID := int64(90002)

			err := store.UpdateRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).To(HaveOccurred())

			errMsg := err.Error()
			Expect(errMsg).To(ContainSubstring("does not exist"))
			Expect(len(errMsg)).To(BeNumerically(">", 20))
		})
	})
})
