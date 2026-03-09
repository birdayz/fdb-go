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

var _ = Describe("COUNT Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.CountIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cnt_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewCountIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan COUNT index", func() {
		It("should produce identical count entries visible to both Go and Java", func() {
			// Save 3 orders with price=100, 2 with price=200
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(200),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Scan with Java
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// Compare
			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify values: price=100 count=3, price=200 count=2
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(goEntries[0].Count).To(Equal(int64(3)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(goEntries[1].Count).To(Equal(int64(2)))
		})
	})

	Describe("Java writes, both scan COUNT index", func() {
		It("should produce identical count entries visible to both Go and Java", func() {
			// Java saves 2 orders with price=300, 1 with price=400
			for i := int64(1); i <= 2; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(300),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan with Go
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Scan with Java
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// Compare
			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(300)))
			Expect(goEntries[0].Count).To(Equal(int64(2)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(400)))
			Expect(goEntries[1].Count).To(Equal(int64(1)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce correct combined counts", func() {
			// Go saves 2 orders with price=500
			for i := int64(1); i <= 2; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(500),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java saves 3 more orders with price=500
			for i := int64(3); i <= 5; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(500),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Both should see count=5 for price=500
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(500)))
			Expect(goEntries[0].Count).To(Equal(int64(5)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete decrements count cross-validated", func() {
		It("should decrement when Go deletes a Java-written record", func() {
			// Java saves 3 orders with price=600
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(600),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			// Go deletes one record
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
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should decrement when Java deletes a Go-written record", func() {
			// Go saves 3 orders with price=700
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(700),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Count).To(Equal(int64(3)))

			// Java deletes one
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should see count=2
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err = store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update changes count correctly", func() {
		It("should move counts when price changes via Go update", func() {
			// Save 2 orders: price=800 and price=900
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(800),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(900),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify: 800=1, 900=1
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Update order 1: price 800 → 900
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(900),
			})
			Expect(err).NotTo(HaveOccurred())

			// Now: 800=0 (gone), 900=2
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Should have at least the 900=2 entry
			// Note: COUNT index may or may not retain 0-count entries depending
			// on implementation. Both Go and Java use atomic ADD so a 0-count
			// entry may still exist as a key with value 0.
			found900 := false
			for _, e := range goEntries {
				if toInt64(e.Key[0]) == int64(900) {
					Expect(e.Count).To(Equal(int64(2)))
					found900 = true
				}
			}
			Expect(found900).To(BeTrue(), "expected entry for price=900 with count=2")
		})
	})
})
