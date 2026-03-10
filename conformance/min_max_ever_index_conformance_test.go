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

var _ = Describe("MAX_EVER_LONG Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.MaxEverLongIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("maxever_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewMaxEverLongIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan MAX_EVER_LONG index", func() {
		It("should produce identical max entries visible to both Go and Java", func() {
			for i, price := range []int32{100, 300, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Value).To(Equal(int64(300)))
		})
	})

	Describe("Java writes, both scan MAX_EVER_LONG index", func() {
		It("should produce identical max entries visible to both Go and Java", func() {
			for i, price := range []int32{150, 450, 250} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Value).To(Equal(int64(450)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should track the global max across both writers", func() {
			// Go writes: max so far = 200
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes: max becomes 500
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes lower: max stays 500
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does NOT revert max (_EVER semantics)", func() {
		It("should preserve max after Go deletes the max record written by Java", func() {
			// Java saves: prices 100, 500
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify max = 500
			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			// Go deletes order 2 (the max record)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Max should STILL be 500 (_EVER = irreversible)
			goEntries, err = store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should preserve max after Java deletes the max record written by Go", func() {
			// Go saves: prices 200, 800
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(800),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2 (the max record)
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Max should STILL be 800
			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(800)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update never decreases max", func() {
		It("should keep max when Go updates to lower value", func() {
			// Save with price 700
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(700),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to lower price 200
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Max should still be 700
			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(700)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("MIN_EVER_LONG Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.MinEverLongIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("minever_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewMinEverLongIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan MIN_EVER_LONG index", func() {
		It("should produce identical min entries visible to both Go and Java", func() {
			for i, price := range []int32{300, 100, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Value).To(Equal(int64(100)))
		})
	})

	Describe("Java writes, both scan MIN_EVER_LONG index", func() {
		It("should produce identical min entries visible to both Go and Java", func() {
			for i, price := range []int32{350, 50, 250} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Value).To(Equal(int64(50)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should track the global min across both writers", func() {
			// Go writes: min so far = 300
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes lower: min becomes 50
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes higher: min stays 50
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(50)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does NOT revert min (_EVER semantics)", func() {
		It("should preserve min after Go deletes the min record written by Java", func() {
			// Java saves: prices 500, 100
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify min = 100
			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(100)))

			// Go deletes order 2 (the min record)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Min should STILL be 100 (_EVER = irreversible)
			goEntries, err = store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(100)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should preserve min after Java deletes the min record written by Go", func() {
			// Go saves: prices 400, 75
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(75),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2 (the min record)
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Min should STILL be 75
			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(75)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update never increases min", func() {
		It("should keep min when Go updates to higher value", func() {
			// Save with price 100
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to higher price 900
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(900),
			})
			Expect(err).NotTo(HaveOccurred())

			// Min should still be 100
			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(100)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = helpers.CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
