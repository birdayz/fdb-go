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

var _ = Describe("Fan-Out Index Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.FanOutIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("fanout_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewFanOutIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes with fan-out index, Java scans", func() {
		It("should produce one index entry per tag, visible to both Go and Java", func() {
			order := helpers.NewOrder(1).WithPrice(100).WithTags("urgent", "wholesale", "premium").Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// All entries should point to PK=1
			for _, entry := range goEntries {
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(int64(1)))
			}
		})
	})

	Describe("Java writes with fan-out index, Go scans", func() {
		It("should produce identical fan-out entries visible to Go", func() {
			order := helpers.NewOrder(2).WithPrice(200).WithTags("bulk", "discount").Build()
			err := store.SaveOrderJava(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Multiple records with fan-out", func() {
		It("should produce correct total entries across records", func() {
			// Record 1: 2 tags, Record 2: 3 tags, Record 3: 1 tag = 6 total entries
			orders := []*gen.Order{
				helpers.NewOrder(10).WithPrice(100).WithTags("a", "b").Build(),
				helpers.NewOrder(20).WithPrice(200).WithTags("c", "d", "e").Build(),
				helpers.NewOrder(30).WithPrice(300).WithTags("f").Build(),
			}

			for _, order := range orders {
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(6))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(6))

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Empty repeated field produces no entries", func() {
		It("should produce zero index entries for a record with no tags", func() {
			order := helpers.NewOrder(42).WithPrice(500).Build() // no tags
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Delete removes all fan-out entries", func() {
		It("should remove all index entries when record is deleted", func() {
			order := helpers.NewOrder(99).WithPrice(999).WithTags("x", "y", "z").Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify 3 entries exist
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Delete
			deleted, err := store.DeleteOrderGo(ctx, 99)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify all entries removed
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err = store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Update changes fan-out entries", func() {
		It("should update index entries when tags change", func() {
			// Save with 2 tags
			order := helpers.NewOrder(50).WithPrice(100).WithTags("old1", "old2").Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Update with 3 different tags
			order2 := helpers.NewOrder(50).WithPrice(100).WithTags("new1", "new2", "new3").Build()
			err = store.SaveOrderGo(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Should now have 3 entries (old 2 removed, new 3 added)
			goEntries, err = store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Cross-write fan-out", func() {
		It("should produce identical entries whether Go or Java writes", func() {
			// Go writes record with tags
			goOrder := helpers.NewOrder(100).WithPrice(100).WithTags("alpha", "beta").Build()
			err := store.SaveOrderGo(ctx, goOrder)
			Expect(err).NotTo(HaveOccurred())

			// Java writes record with tags
			javaOrder := helpers.NewOrder(200).WithPrice(200).WithTags("gamma", "delta").Build()
			err = store.SaveOrderJava(ctx, javaOrder)
			Expect(err).NotTo(HaveOccurred())

			// Total: 4 entries (2 from each record)
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
