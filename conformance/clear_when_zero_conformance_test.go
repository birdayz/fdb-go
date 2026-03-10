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

var _ = Describe("CLEAR_WHEN_ZERO Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.ClearWhenZeroConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cwz_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewClearWhenZeroConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go insert+delete clears entry, Java confirms", func() {
		It("should have no index entries when all records are deleted", func() {
			// Go saves 2 orders with price=100
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify count=2
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			// Delete both — with CLEAR_WHEN_ZERO, the entry should be removed
			_, err = store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Go sees no entries
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty(), "Go should see no entries after CLEAR_WHEN_ZERO")

			// Java also sees no entries
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty(), "Java should see no entries after Go's CLEAR_WHEN_ZERO")
		})
	})

	Describe("Java insert+delete clears entry, Go confirms", func() {
		It("should have no index entries when all records are deleted by Java", func() {
			// Java saves 2 orders with price=200
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify count=2
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Count).To(Equal(int64(2)))

			// Java deletes both
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Java sees no entries
			javaEntries, err = store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty(), "Java should see no entries after CLEAR_WHEN_ZERO")

			// Go also sees no entries
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty(), "Go should see no entries after Java's CLEAR_WHEN_ZERO")
		})
	})

	Describe("Cross-platform: Go inserts, Java deletes all, entry cleared", func() {
		It("should clear entry when Java deletes Go-written records to zero", func() {
			// Go saves 1 order with price=300
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify count=1
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(1)))

			// Java deletes it
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should see no entries
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Partial delete leaves non-zero entry", func() {
		It("should keep entry when count is still > 0", func() {
			// Go saves 3 orders with price=400
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGoProto(ctx, i, 400)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete 1 — count should be 2
			_, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

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
})
