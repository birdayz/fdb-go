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

var _ = Describe("Index Entry Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.IndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idx_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes with index, Java scans index", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			// Save 5 orders with Go — each gets an index entry on price
			for i := int64(1); i <= 5; i++ {
				order := helpers.NewOrder(i).WithPrice(int32(i * 100)).WithFlower("Rose", gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			// Scan index with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			// Compare: entries should be identical
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify ordering: ascending by price (100, 200, 300, 400, 500)
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				expectedPK := int64(i + 1)
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
				// Key = (price, pk)
				Expect(entry.Key).To(HaveLen(2))
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Java writes with index, Go scans index", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			// Save 3 orders with Java
			for i := int64(1); i <= 3; i++ {
				order := helpers.NewOrder(i).WithPrice(int32(i * 200)).WithFlower("Tulip", gen.Color_BLUE).Build()
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify ordering: ascending by price (200, 400, 600)
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 200)
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Delete removes index entry", func() {
		It("should remove the index entry when Go deletes a record", func() {
			// Save with Go
			order := helpers.NewOrder(42).WithPrice(999).WithFlower("Orchid", gen.Color_PINK).Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify index has 1 entry (Java sees it)
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Delete with Go
			deleted, err := store.DeleteOrderGo(ctx, 42)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify index is empty (Java sees no entries)
			javaEntries, err = store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())

			// Go also sees no entries
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())
		})
	})

	Describe("Update changes index entry", func() {
		It("should update the index entry when price changes", func() {
			// Save with Go, price=100
			order := helpers.NewOrder(77).WithPrice(100).WithFlower("Daisy", gen.Color_YELLOW).Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify Java sees price=100 in index
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(100)))

			// Update with Go, price=500
			order2 := helpers.NewOrder(77).WithPrice(500).WithFlower("Daisy", gen.Color_YELLOW).Build()
			err = store.SaveOrderGo(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Verify Java sees price=500 (old entry removed, new entry added)
			javaEntries, err = store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(500)))

			// Go also sees price=500
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(500)))

			// Cross-validate
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Multiple records sorted by price", func() {
		It("should produce identically sorted entries in both Go and Java", func() {
			// Insert orders with non-sequential prices to verify sort order
			prices := []int32{500, 100, 300, 200, 400}
			for i, price := range prices {
				order := helpers.NewOrder(int64(i + 1)).WithPrice(price).WithFlower("Mix", gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			// Both should be sorted by price ascending: 100, 200, 300, 400, 500
			expectedPrices := []int64{100, 200, 300, 400, 500}
			// PK order for these prices: 2(100), 4(200), 3(300), 5(400), 1(500)
			expectedPKs := []int64{2, 4, 3, 5, 1}

			for i := range goEntries {
				Expect(toInt64(goEntries[i].Key[0])).To(Equal(expectedPrices[i]),
					"Go entry %d price mismatch", i)
				Expect(toInt64(goEntries[i].PrimaryKey[0])).To(Equal(expectedPKs[i]),
					"Go entry %d PK mismatch", i)
			}

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// toInt64 normalizes numeric values to int64 for comparison.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int32:
		return int64(n)
	default:
		return -1
	}
}
