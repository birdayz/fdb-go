package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
)

var _ = Describe("Index State Persistence Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.IndexStateConformanceStore
	)

	const indexName = "Order$price"

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idxstate_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewIndexStateConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())

		// Create the store first so both sides can open it
		err = store.CreateStoreGo(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go marks WRITE_ONLY, Java reads raw state", func() {
		It("should persist WRITE_ONLY state readable by Java", func() {
			err := store.MarkIndexWriteOnlyGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw state
			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			// Go reads raw state for comparison
			goState, err := store.GetIndexStateRawGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Java marks WRITE_ONLY, Go reads state", func() {
		It("should persist WRITE_ONLY state readable by Go", func() {
			err := store.MarkIndexWriteOnlyJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw state
			goState, err := store.GetIndexStateRawGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))

			// Go opens store and reads state through API
			openState, err := store.GetIndexStateViaOpenGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(openState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Go marks DISABLED, Java reads raw state", func() {
		It("should persist DISABLED state readable by Java", func() {
			err := store.MarkIndexDisabledGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("DISABLED"))
		})
	})

	Describe("Go marks WRITE_ONLY then READABLE, Java reads default", func() {
		It("should clear state entry when returning to READABLE", func() {
			// Mark WRITE_ONLY first
			err := store.MarkIndexWriteOnlyGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Verify it's WRITE_ONLY
			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			// Mark READABLE (clears the key)
			err = store.MarkIndexReadableGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Should be READABLE (no key in FDB)
			javaState, err = store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("READABLE"))
		})
	})
})
