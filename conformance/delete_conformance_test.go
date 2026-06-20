//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Delete Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		// Generate unique tenant name using UUID
		tenantName := fmt.Sprintf("delete_%s", uuid.New().String())

		// Use shared container with tenant isolation
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		// Create conformance store for automatic Go/Java validation
		store = NewConformanceStoreWithTenant(env.RecordDB, env.MetaData, env.ClusterFile, env.TenantName)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx) // Deletes tenant only
		}
	})

	Describe("Delete Operations", func() {
		It("should delete existing records", func() {
			// Create a record (automatically validated with Java)
			order := StandardOrder(1001)
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
				order := StandardOrder(i)
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
				order := StandardOrder(i)
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
			order := StandardOrder(300)
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
			order1 := NewOrder(400).
				WithPrice(100).
				WithFlower("Rose", gen.Color_RED).
				Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update record
			order2 := NewOrder(400).
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

		It("should allow re-insert after delete", func() {
			// Create, delete, re-insert with different data
			order1 := NewOrder(600).
				WithPrice(100).
				WithFlower("Rose", gen.Color_RED).
				Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Delete it
			deleted, err := store.DeleteRecord(ctx, 600)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Re-insert with different data (validated with Java)
			order2 := NewOrder(600).
				WithPrice(999).
				WithFlower("Tulip", gen.Color_BLUE).
				Build()
			err = store.SaveRecord(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Verify the new data is what we get back (cross-checked with Java)
			loaded, err := store.LoadRecord(ctx, 600)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.GetPrice()).To(Equal(int32(999)))
			Expect(loaded.GetFlower().GetType()).To(Equal("Tulip"))
		})

		It("should handle deleting minimal vs full orders", func() {
			// Create and delete full order
			fullOrder := StandardOrder(500)
			err := store.SaveRecord(ctx, fullOrder)
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(ctx, 500)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Create and delete minimal order
			minOrder := MinimalOrder(501)
			err = store.SaveRecord(ctx, minOrder)
			Expect(err).NotTo(HaveOccurred())

			deleted, err = store.DeleteRecord(ctx, 501)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
		})
	})

	Describe("Cross-language Delete", func() {
		It("Go inserts, Java deletes, Go verifies gone", func() {
			// Go writes a record.
			order := StandardOrder(700)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Java deletes it.
			java := NewJavaInvoker()
			clusterFile, cfErr := sharedContainer.ClusterFile(ctx)
			Expect(cfErr).NotTo(HaveOccurred())
			var deleted bool
			err = java.InvokeAs(ctx, "deleteOrder", map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(env.Keyspace.Bytes()),
				"orderID":     int64(700),
				"tenantName":  env.TenantName,
			}, &deleted)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Go confirms it's gone.
			exists, err := store.RecordExists(ctx, 700)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})

		It("Java inserts, Go deletes, Java verifies gone", func() {
			// Java writes a record.
			java := NewJavaInvoker()
			clusterFile, cfErr := sharedContainer.ClusterFile(ctx)
			Expect(cfErr).NotTo(HaveOccurred())
			err := java.InvokeAs(ctx, "saveOrder", map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(env.Keyspace.Bytes()),
				"order": &gen.Order{
					OrderId: proto.Int64(701),
					Price:   proto.Int32(999),
				},
				"tenantName": env.TenantName,
			}, nil)
			Expect(err).NotTo(HaveOccurred())

			// Go deletes it.
			deleted, err := store.DeleteRecord(ctx, 701)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Java confirms it's gone.
			var exists bool
			err = java.InvokeAs(ctx, "recordExists", map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(env.Keyspace.Bytes()),
				"orderID":     int64(701),
				"tenantName":  env.TenantName,
			}, &exists)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})
	})
})
