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

var _ = Describe("COUNT_UPDATES Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.CountUpdatesIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cup_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewCountUpdatesIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan", func() {
		It("should count inserts via both Go and Java scan", func() {
			// Save 3 orders with price=100
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, both scan", func() {
		It("should count inserts via both Go and Java scan", func() {
			for i := int64(1); i <= 2; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(200),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(200)))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does not decrement", func() {
		It("should keep count unchanged when Go deletes a Java-written record", func() {
			// Java saves 3 orders
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(300),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			// Go deletes one — count should NOT change
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should still see count=3
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should keep count unchanged when Java deletes a Go-written record", func() {
			// Go saves 3 orders
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(400),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Count).To(Equal(int64(3)))

			// Java deletes one — count should NOT change
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should still see count=3
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			javaEntries, err = store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update always increments", func() {
		It("should increment count on update even when key unchanged", func() {
			// Save order with price=500
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// count=1
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(1)))

			// Update same order, same price — count should STILL increment
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

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
