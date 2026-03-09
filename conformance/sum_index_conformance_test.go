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

var _ = Describe("SUM Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.SumIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("sum_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewSumIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan SUM index", func() {
		It("should produce identical sum entries visible to both Go and Java", func() {
			// Save orders with known prices: 100 + 200 + 300 = 600
			for i, price := range []int32{100, 200, 300} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			// Scan with Java
			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Compare
			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify: ungrouped total sum = 600
			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Sum).To(Equal(int64(600)))
		})
	})

	Describe("Java writes, both scan SUM index", func() {
		It("should produce identical sum entries visible to both Go and Java", func() {
			// Java saves orders: 150 + 250 + 350 = 750
			for i, price := range []int32{150, 250, 350} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			// Scan with Java
			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Compare
			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify: total = 750
			Expect(goEntries[0].Sum).To(Equal(int64(750)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce correct combined sum", func() {
			// Go saves: 100 + 200 = 300
			for i, price := range []int32{100, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java saves: 300 + 400 = 700
			for i, price := range []int32{300, 400} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 3)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Both should see sum = 1000
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(1000)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete decrements sum cross-validated", func() {
		It("should decrement when Go deletes a Java-written record", func() {
			// Java saves: 100 + 200 + 300 = 600
			for i, price := range []int32{100, 200, 300} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify sum = 600
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(600)))

			// Go deletes order 2 (price=200)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should see sum = 400
			goEntries, err = store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(400)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should decrement when Java deletes a Go-written record", func() {
			// Go saves: 500 + 600 + 700 = 1800
			for i, price := range []int32{500, 600, 700} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify sum = 1800
			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Sum).To(Equal(int64(1800)))

			// Java deletes order 1 (price=500)
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should see sum = 1300
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(1300)))

			javaEntries, err = store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update changes sum correctly", func() {
		It("should adjust sum when price changes via Go update", func() {
			// Save: 100 + 200 = 300
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

			// Verify sum = 300
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(300)))

			// Update order 1: price 100 → 500 (net change +400)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Both should see sum = 700
			goEntries, err = store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(700)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should adjust sum when price changes via Java update", func() {
			// Java saves: 300 + 400 = 700
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java updates order 2: price 400 → 100 (net change -300)
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Both should see sum = 400
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(400)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
