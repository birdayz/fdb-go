package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// Composite Index Conformance Tests
//
// Validates PK component deduplication: when an index key expression overlaps
// with the primary key, Java trims redundant PK components from the index entry.
// Go must produce identical wire format.
//
// Test: Index on (price, order_id), PK on (order_id).
// Java deduplicates order_id → entry key is (price, order_id) with 2 elements.
// Without dedup it would be (price, order_id, order_id) with 3 elements.
var _ = Describe("Composite Index Conformance (PK Dedup)", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.CompositeIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cidx_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewCompositeIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java scans composite index", func() {
		It("should produce identical deduplicated index entries", func() {
			// Save orders with Go
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderGo(ctx, order)
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

			// Compare entries
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify key structure: (price, order_id) with dedup
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				expectedPK := int64(i + 1)
				// Key should have 2 elements (deduplicated)
				Expect(entry.Key).To(HaveLen(2), "index entry key should be deduplicated to 2 elements")
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
				Expect(toInt64(entry.Key[1])).To(Equal(expectedPK))
				// PK should be reconstructed correctly
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
			}
		})
	})

	Describe("Java writes, Go scans composite index", func() {
		It("should produce identical deduplicated index entries", func() {
			// Save orders with Java
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
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

			// Compare entries — Go should read Java's deduplicated entries correctly
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// PK should be reconstructed correctly from deduplicated entries
			for i, entry := range goEntries {
				expectedPK := int64(i + 1)
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
			}
		})
	})

	Describe("Cross-write composite index", func() {
		It("Go and Java produce interchangeable composite index entries", func() {
			// Go writes odd orders
			for i := int64(1); i <= 5; i += 2 {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java writes even orders
			for i := int64(2); i <= 4; i += 2 {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Both Go and Java should see all 5 entries identically
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// toInt64 is defined in index_conformance_test.go
