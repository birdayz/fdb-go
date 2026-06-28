//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CRUD Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		// Generate unique tenant name using UUID
		tenantName := fmt.Sprintf("crud_%s", uuid.New().String())

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

	Describe("Basic Write/Read Operations", func() {
		It("should save and load standard orders", func() {
			// SaveRecord automatically validates with both Go and Java
			order := StandardOrder(1001)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// LoadRecord automatically cross-checks Go and Java
			loaded, err := store.LoadRecord(ctx, 1001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(int64(1001)))
			Expect(*loaded.Price).To(Equal(int32(10010)))
			Expect(*loaded.Flower.Type).To(Equal("Rose_1001"))
		})

		It("should handle orders with different prices", func() {
			order := NewOrder(2002).
				WithPrice(50).
				WithFlower("Tulip", gen.Color_BLUE).
				Build()

			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, 2002)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(50)))
			Expect(*loaded.Flower.Type).To(Equal("Tulip"))
			Expect(*loaded.Flower.Color).To(Equal(gen.Color_BLUE))
		})

		It("should handle minimal orders", func() {
			order := MinimalOrder(3003)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, 3003)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(int64(3003)))
		})
	})

	DescribeTable("Round-trip compatibility with various order types",
		func(orderID int64, price int32, flowerType string, color gen.Color) {
			// Create and save order (automatically validated with Java)
			order := NewOrder(orderID).
				WithPrice(price).
				WithFlower(flowerType, color).
				Build()
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Load and verify (automatically cross-checked with Java)
			loaded, err := store.LoadRecord(ctx, orderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(price))
			Expect(*loaded.Flower.Type).To(Equal(flowerType))
			Expect(*loaded.Flower.Color).To(Equal(color))
		},
		Entry("Small order", int64(100), int32(50), "Daisy", gen.Color_YELLOW),
		Entry("Large order", int64(99999), int32(12345), "Orchid", gen.Color_PINK),
		Entry("Zero price", int64(500), int32(0), "Tulip", gen.Color_BLUE),
		Entry("Red rose", int64(1001), int32(10010), "Rose_1001", gen.Color_RED),
	)

	Describe("Error Handling", func() {
		It("should handle loading non-existent records", func() {
			loaded, err := store.LoadRecord(ctx, 99999999)
			Expect(err).To(HaveOccurred())
			Expect(loaded).To(BeNil())
		})

		It("should verify non-existent record doesn't exist", func() {
			exists, err := store.RecordExists(ctx, 88888888)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})
	})

	Describe("Update Operations", func() {
		It("should allow overwriting existing records", func() {
			// Save initial order
			order1 := NewOrder(5001).
				WithPrice(100).
				WithFlower("Rose", gen.Color_RED).
				Build()
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Overwrite with different data
			order2 := NewOrder(5001).
				WithPrice(200).
				WithFlower("Tulip", gen.Color_BLUE).
				Build()
			err = store.SaveRecord(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Verify updated values
			loaded, err := store.LoadRecord(ctx, 5001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(int32(200)))
			Expect(*loaded.Flower.Type).To(Equal("Tulip"))
			Expect(*loaded.Flower.Color).To(Equal(gen.Color_BLUE))
		})

		It("should handle updating from full to minimal", func() {
			// Save full order
			order1 := StandardOrder(6001)
			err := store.SaveRecord(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Update to minimal
			order2 := MinimalOrder(6001)
			err = store.SaveRecord(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Verify minimal order
			loaded, err := store.LoadRecord(ctx, 6001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(int64(6001)))
			Expect(loaded.Flower).To(BeNil())
			Expect(loaded.Price).To(BeNil())
		})
	})

	Describe("Boundary Values", func() {
		It("should handle order ID of 1", func() {
			order := StandardOrder(1)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(int64(1)))
		})

		It("should handle large order IDs", func() {
			largeID := int64(9223372036854775000) // Close to max int64
			order := NewOrder(largeID).
				WithPrice(999).
				WithFlower("Rare", gen.Color_PINK).
				Build()
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, largeID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(largeID))
		})

		It("should handle max int32 price", func() {
			maxPrice := int32(2147483647) // Max int32
			order := NewOrder(7001).
				WithPrice(maxPrice).
				WithFlower("Diamond", gen.Color_PINK).
				Build()
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, 7001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Price).To(Equal(maxPrice))
		})

		It("should handle order ID of 0", func() {
			order := NewOrder(0).
				WithPrice(42).
				WithFlower("Lily", gen.Color_YELLOW).
				Build()
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(int64(0)))
			Expect(*loaded.Price).To(Equal(int32(42)))
			Expect(*loaded.Flower.Type).To(Equal("Lily"))
		})

		It("should handle negative order IDs", func() {
			order := NewOrder(-1).
				WithPrice(77).
				WithFlower("Cactus", gen.Color_RED).
				Build()
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(ctx, -1)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.OrderId).To(Equal(int64(-1)))
			Expect(*loaded.Price).To(Equal(int32(77)))
			Expect(*loaded.Flower.Type).To(Equal("Cactus"))

			// Also test a large negative close to min int64
			largeNeg := int64(-9223372036854775000)
			order2 := NewOrder(largeNeg).
				WithPrice(1).
				WithFlower("Edelweiss", gen.Color_BLUE).
				Build()
			err = store.SaveRecord(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			loaded2, err := store.LoadRecord(ctx, largeNeg)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded2.OrderId).To(Equal(largeNeg))
			Expect(*loaded2.Price).To(Equal(int32(1)))
			Expect(*loaded2.Flower.Type).To(Equal("Edelweiss"))
		})
	})

	Describe("Existence Checks", func() {
		It("should correctly report existence after save", func() {
			order := StandardOrder(8001)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			exists, err := store.RecordExists(ctx, 8001)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
		})

		It("should report non-existence before save", func() {
			exists, err := store.RecordExists(ctx, 9001)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})
	})

	Describe("All Color Variants", func() {
		DescribeTable("should handle all color types",
			func(orderID int64, color gen.Color) {
				order := NewOrder(orderID).
					WithPrice(100).
					WithFlower("TestFlower", color).
					Build()
				err := store.SaveRecord(ctx, order)
				Expect(err).NotTo(HaveOccurred())

				loaded, err := store.LoadRecord(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(*loaded.Flower.Color).To(Equal(color))
			},
			Entry("RED", int64(10001), gen.Color_RED),
			Entry("BLUE", int64(10002), gen.Color_BLUE),
			Entry("YELLOW", int64(10003), gen.Color_YELLOW),
			Entry("PINK", int64(10004), gen.Color_PINK),
		)
	})
})
