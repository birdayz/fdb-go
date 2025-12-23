package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("Delete Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		// Generate unique tenant name using UUID
		tenantName := fmt.Sprintf("delete_%s", uuid.New().String())

		// Use shared container with tenant isolation
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		// Create conformance store for automatic Go/Java validation
		store = helpers.NewConformanceStoreWithTenant(env.RecordDB, env.MetaData, env.ClusterFile, env.TenantName)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)  // Deletes tenant only
		}
	})

	Describe("Delete Operations", func() {
		It("should delete existing records", func() {
			// Create a record (automatically validated with Java)
			order := helpers.StandardOrder(1001)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify it exists (automatically checked in both Go and Java)
			exists, err := store.RecordExists(ctx, 1001)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue(), "Record should exist before deletion")

			// Delete the record (automatically validated with Java)
			deleted, err := store.DeleteRecord(ctx, 1001)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify it's deleted (automatically checked in both Go and Java)
			exists, err = store.RecordExists(ctx, 1001)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse(), "Record should not exist after deletion")
		})

		It("should handle deleting non-existent records", func() {
			// Try to delete record that doesn't exist (validated with both Go and Java)
			deleted, err := store.DeleteRecord(ctx, 9999)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse(), "Deleting non-existent record should return false")

			// Verify it doesn't exist (checked in both Go and Java)
			exists, err := store.RecordExists(ctx, 9999)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})

		It("should handle deleting multiple records", func() {
			// Create multiple records (each validated with Java)
			for i := int64(100); i < 110; i++ {
				order := helpers.StandardOrder(i)
				err := store.SaveRecord(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete every other record (each validated with Java)
			for i := int64(100); i < 110; i += 2 {
				deleted, err := store.DeleteRecord(ctx, i)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
			}

			// Verify deletion status for all records (each checked in both Go and Java)
			for i := int64(100); i < 110; i++ {
				exists, err := store.RecordExists(ctx, i)
				Expect(err).NotTo(HaveOccurred())

				if i%2 == 0 {
					Expect(exists).To(BeFalse(), "Even record should be deleted")
				} else {
					Expect(exists).To(BeTrue(), "Odd record should still exist")
				}
			}
		})

		It("should maintain consistency across multiple operations", func() {
			// Create records (validated with Java)
			for i := int64(200); i < 210; i++ {
				order := helpers.StandardOrder(i)
				err := store.SaveRecord(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete all records (validated with Java)
			for i := int64(200); i < 210; i++ {
				deleted, err := store.DeleteRecord(ctx, i)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
			}

			// Verify all are deleted (checked in both Go and Java)
			for i := int64(200); i < 210; i++ {
				exists, err := store.RecordExists(ctx, i)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())
			}
		})

		It("should handle deleting same record twice", func() {
			// Create record
			order := helpers.StandardOrder(300)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// First delete - should succeed
			deleted, err := store.DeleteRecord(ctx, 300)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Second delete - should return false
			deleted, err = store.DeleteRecord(ctx, 300)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse())
		})

		It("should handle delete after update", func() {
			// Create record
			order1 := helpers.NewOrder(400).
				WithPrice(100).
				WithFlower("Rose", gen.Color_RED).
				Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update record
			order2 := helpers.NewOrder(400).
				WithPrice(200).
				WithFlower("Tulip", gen.Color_BLUE).
				Build()
			err = store.SaveRecord(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Delete updated record
			deleted, err := store.DeleteRecord(ctx, 400)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify deletion
			exists, err := store.RecordExists(ctx, 400)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})

		It("should handle deleting minimal vs full orders", func() {
			// Create and delete full order
			fullOrder := helpers.StandardOrder(500)
			err := store.SaveRecord(ctx, fullOrder)
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(ctx, 500)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Create and delete minimal order
			minOrder := helpers.MinimalOrder(501)
			err = store.SaveRecord(ctx, minOrder)
			Expect(err).NotTo(HaveOccurred())

			deleted, err = store.DeleteRecord(ctx, 501)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
		})
	})
})
