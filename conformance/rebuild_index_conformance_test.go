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

var _ = Describe("Index Rebuild Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.RebuildIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idxrebuild_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewRebuildIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go saves records, Go rebuilds index, Java scans", func() {
		It("should produce index entries readable by Java", func() {
			// Save 5 orders without index using Go
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index with Go
			err := store.RebuildIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			// Compare — entries must be byte-identical
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 100, 200, 300, 400, 500
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				Expect(idxToInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Go saves records, Java rebuilds index, Go scans", func() {
		It("should produce index entries readable by Go", func() {
			// Save 4 orders without index using Go
			for i := int64(1); i <= 4; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 200)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index with Java
			err := store.RebuildIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			// Compare
			err = helpers.CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 200, 400, 600, 800
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 200)
				Expect(idxToInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Java saves records, Go rebuilds index, Java scans", func() {
		It("should produce index entries readable by Java", func() {
			// Save 3 orders without index using Java
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 300)),
				}
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index with Go
			err := store.RebuildIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

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
		})
	})

	Describe("Cross-rebuild: both sides can rebuild the same data", func() {
		It("Go rebuild and Java rebuild produce identical results", func() {
			// Save 3 orders without index using Go
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 150)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild with Go first
			err := store.RebuildIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			goEntriesAfterGoRebuild, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntriesAfterGoRebuild).To(HaveLen(3))

			// Rebuild with Java (clears and rebuilds)
			err = store.RebuildIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			goEntriesAfterJavaRebuild, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntriesAfterJavaRebuild).To(HaveLen(3))

			// Both rebuilds should produce identical entries
			err = helpers.CompareIndexEntries(goEntriesAfterGoRebuild, goEntriesAfterJavaRebuild)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func idxToInt64(v interface{}) int64 {
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
