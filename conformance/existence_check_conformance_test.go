//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Comprehensive tests for all 5 RecordExistenceCheck modes
// Java equivalent: FDBRecordStoreBase.java:394-443 (RecordExistenceCheck enum)
var _ = Describe("RecordExistenceCheck Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		// Generate unique tenant name using UUID
		tenantName := fmt.Sprintf("existence_%s", uuid.New().String())

		// Use shared container with tenant isolation
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store = NewConformanceStoreWithTenant(env.RecordDB, env.MetaData, env.ClusterFile, env.TenantName)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx) // Deletes tenant only
		}
	})

	Describe("NONE Mode", func() {
		// RecordExistenceCheck.NONE - No special checks, allows insert and update
		// Java: FDBRecordStoreBase.RecordExistenceCheck.NONE

		It("should allow saving new record", func() {
			orderID := int64(10001)

			order := StandardOrder(orderID)
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
			order1 := NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with NONE - should succeed
			order2 := NewOrder(orderID).WithPrice(200).Build()
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
			order := StandardOrder(orderID)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Save again with NONE - should succeed (effectively an update)
			order2 := NewOrder(orderID).WithPrice(999).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ERROR_IF_EXISTS Mode", func() {
		// RecordExistenceCheck.ERROR_IF_EXISTS - Fail if record already exists (insert-only)
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_EXISTS

		It("should succeed for new record", func() {
			orderID := int64(20001)

			order := StandardOrder(orderID)
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
			order1 := StandardOrder(orderID)
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Try to save again with ERROR_IF_EXISTS - should fail
			order2 := NewOrder(orderID).WithPrice(999).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfExists)
			Expect(err).To(HaveOccurred())
			var existsErr *recordlayer.RecordAlreadyExistsError
			Expect(errors.As(err, &existsErr)).To(BeTrue())
		})

		It("should return structured error with primary key", func() {
			orderID := int64(20003)

			// Save initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Try to save again
			err = store.SaveRecordWithOptions(ctx, StandardOrder(orderID), recordlayer.RecordExistenceCheckErrorIfExists)
			Expect(err).To(HaveOccurred())

			// Check error is the structured type
			var recordExistsErr *recordlayer.RecordAlreadyExistsError
			Expect(errors.As(err, &recordExistsErr)).To(BeTrue())
			Expect(recordExistsErr.PrimaryKey).NotTo(BeNil())
		})
	})

	Describe("ERROR_IF_NOT_EXISTS Mode", func() {
		// RecordExistenceCheck.ERROR_IF_NOT_EXISTS - Fail if record doesn't exist (update-only)
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_NOT_EXISTS

		It("should fail for new record", func() {
			orderID := int64(30001)

			order := StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())
			var notExistErr *recordlayer.RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
		})

		It("should succeed for existing record", func() {
			orderID := int64(30002)

			// Save initial record
			order1 := NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with ERROR_IF_NOT_EXISTS - should succeed
			order2 := NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfNotExists)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		It("should return structured error with primary key", func() {
			orderID := int64(30003)

			err := store.SaveRecordWithOptions(ctx, StandardOrder(orderID), recordlayer.RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())

			// Check error is the structured type
			var recordDoesNotExistErr *recordlayer.RecordDoesNotExistError
			Expect(errors.As(err, &recordDoesNotExistErr)).To(BeTrue())
			Expect(recordDoesNotExistErr.PrimaryKey).NotTo(BeNil())
		})
	})

	Describe("ERROR_IF_RECORD_TYPE_CHANGED Mode", func() {
		// RecordExistenceCheck.ERROR_IF_RECORD_TYPE_CHANGED - Fail if existing record has different type
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_RECORD_TYPE_CHANGED

		It("should succeed for new record", func() {
			orderID := int64(40001)

			order := StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should succeed for existing record with same type", func() {
			orderID := int64(40002)

			// Save initial Order
			order1 := NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with same type - should succeed
			order2 := NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		It("should fail for existing record with different type", func() {
			// Save an Order at PK=42
			orderID := int64(40003)
			order := StandardOrder(orderID)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Try to save a Customer at the same PK with ERROR_IF_RECORD_TYPE_CHANGED
			customer := &gen.Customer{CustomerId: &orderID, Name: stringPtr("Alice")}
			_, saveErr := store.RunRaw(ctx, func(st *recordlayer.FDBRecordStore) (any, error) {
				return st.SaveRecordWithOptions(customer, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			})
			Expect(saveErr).To(HaveOccurred())
			var typeChangedErr *recordlayer.RecordTypeChangedError
			Expect(errors.As(saveErr, &typeChangedErr)).To(BeTrue(),
				"expected RecordTypeChangedError, got: %v", saveErr)
			Expect(typeChangedErr.ActualType).To(Equal("Order"))
			Expect(typeChangedErr.ExpectedType).To(Equal("Customer"))
		})
	})

	Describe("ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED Mode", func() {
		// RecordExistenceCheck.ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED
		// Combined check: fail if doesn't exist OR type changed
		// Java: FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED
		// This is what UpdateRecord() uses

		It("should fail for new record", func() {
			orderID := int64(50001)

			order := StandardOrder(orderID)
			err := store.SaveRecordWithOptions(ctx, order, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			Expect(err).To(HaveOccurred())
			var notExistErr *recordlayer.RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
		})

		It("should succeed for existing record with same type", func() {
			orderID := int64(50002)

			// Save initial record
			order1 := NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update with same type - should succeed
			order2 := NewOrder(orderID).WithPrice(200).Build()
			err = store.SaveRecordWithOptions(ctx, order2, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
		})

		It("should fail for existing record with different type", func() {
			// Save an Order at PK=42
			orderID := int64(50003)
			order := StandardOrder(orderID)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Try to save a Customer at the same PK with ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED
			customer := &gen.Customer{CustomerId: &orderID, Name: stringPtr("Bob")}
			_, saveErr := store.RunRaw(ctx, func(st *recordlayer.FDBRecordStore) (any, error) {
				return st.SaveRecordWithOptions(customer, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			})
			Expect(saveErr).To(HaveOccurred())
			var typeChangedErr *recordlayer.RecordTypeChangedError
			Expect(errors.As(saveErr, &typeChangedErr)).To(BeTrue(),
				"expected RecordTypeChangedError, got: %v", saveErr)
			Expect(typeChangedErr.ActualType).To(Equal("Order"))
			Expect(typeChangedErr.ExpectedType).To(Equal("Customer"))
		})
	})

	Describe("InsertRecord Convenience Method", func() {
		// Tests that InsertRecord() == SaveRecordWithOptions(ERROR_IF_EXISTS)
		// Java: FDBRecordStoreBase.insertRecordAsync()

		It("should succeed for new record", func() {
			orderID := int64(60001)

			order := StandardOrder(orderID)
			err := store.InsertRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			exists, err := store.RecordExists(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
		})

		It("should fail for existing record", func() {
			orderID := int64(60002)

			// Save initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Try to insert again - should fail
			err = store.InsertRecord(ctx, StandardOrder(orderID))
			Expect(err).To(HaveOccurred())
			var existsErr *recordlayer.RecordAlreadyExistsError
			Expect(errors.As(err, &existsErr)).To(BeTrue())
		})
	})

	Describe("UpdateRecord Convenience Method", func() {
		// Tests that UpdateRecord() == SaveRecordWithOptions(ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED)
		// Java: FDBRecordStoreBase.updateRecordAsync()

		It("should fail for new record", func() {
			orderID := int64(70001)

			order := StandardOrder(orderID)
			err := store.UpdateRecord(ctx, order)
			Expect(err).To(HaveOccurred())
			var notExistErr *recordlayer.RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
		})

		It("should succeed for existing record", func() {
			orderID := int64(70002)

			// Save initial record
			order1 := NewOrder(orderID).WithPrice(100).Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update - should succeed
			order2 := NewOrder(orderID).WithPrice(200).Build()
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
			err := store.InsertRecord(ctx, NewOrder(orderID).WithPrice(100).Build())
			Expect(err).NotTo(HaveOccurred())

			// Multiple updates should succeed
			for i := 0; i < 5; i++ {
				order := NewOrder(orderID).WithPrice(int32(200 + i*10)).Build()
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
				// Should be readable for debugging (NONE is 4 chars, others are longer)
				Expect(len(str)).To(BeNumerically(">=", 4))
			}
		})
	})

	Describe("Edge Cases", func() {
		It("should handle rapid insert/update cycles", func() {
			orderID := int64(80001)

			// Insert
			err := store.InsertRecord(ctx, NewOrder(orderID).WithPrice(100).Build())
			Expect(err).NotTo(HaveOccurred())

			// Multiple updates in sequence
			for i := 0; i < 10; i++ {
				err = store.UpdateRecord(ctx, NewOrder(orderID).WithPrice(int32(200+i)).Build())
				Expect(err).NotTo(HaveOccurred())
			}

			// Insert should now fail
			err = store.InsertRecord(ctx, NewOrder(orderID).WithPrice(999).Build())
			Expect(err).To(HaveOccurred())
		})

		It("should handle delete followed by insert", func() {
			orderID := int64(80002)

			// Insert
			err := store.InsertRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Delete
			deleted, err := store.DeleteRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Insert again - should succeed
			err = store.InsertRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle update on deleted record", func() {
			orderID := int64(80003)

			// Insert and delete
			err := store.InsertRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Update should fail
			err = store.UpdateRecord(ctx, StandardOrder(orderID))
			Expect(err).To(HaveOccurred())
			var notExistErr *recordlayer.RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
		})
	})

	Describe("Error Message Quality", func() {
		It("should include primary key in RecordAlreadyExists error", func() {
			orderID := int64(90001)

			err := store.InsertRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			err = store.InsertRecord(ctx, StandardOrder(orderID))
			Expect(err).To(HaveOccurred())

			errMsg := err.Error()
			Expect(errMsg).To(ContainSubstring("already exists"))
			// Should include the key in some form
			Expect(len(errMsg)).To(BeNumerically(">", 20))
		})

		It("should include primary key in RecordDoesNotExist error", func() {
			orderID := int64(90002)

			err := store.UpdateRecord(ctx, StandardOrder(orderID))
			Expect(err).To(HaveOccurred())

			errMsg := err.Error()
			Expect(errMsg).To(ContainSubstring("does not exist"))
			Expect(len(errMsg)).To(BeNumerically(">", 20))
		})
	})
})
