package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("COUNT_NOT_NULL Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.CountNotNullIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cnn_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewCountNotNullIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes with nulls, both scan", func() {
		It("should only count records with non-null price", func() {
			// Save 2 orders with price, 1 without (null)
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Order 3: no price (null) — should NOT be counted
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go scan — ungrouped, single entry with total count
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2))) // Only 2, not 3

			// Java scan — must see same result
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes with nulls, both scan", func() {
		It("should only count records with non-null price", func() {
			// Java saves 2 with price, 1 without
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(3),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go scan
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			// Java scan
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Mixed writes with nulls from both sides", func() {
		It("should produce identical count ignoring null records from both", func() {
			// Go saves with price
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go saves without price (null)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java saves with price
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java saves without price (null)
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(4),
			})
			Expect(err).NotTo(HaveOccurred())

			// Both should see count=2 (only the two records with non-null price)
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete of non-null record decrements cross-validated", func() {
		It("should decrement when Go deletes a Java-written non-null record", func() {
			// Java saves 3 orders with price
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(400 + i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			// Go deletes one
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should see count=2
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
