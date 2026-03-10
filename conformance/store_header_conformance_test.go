package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
)

var _ = Describe("Store Header Format Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.StoreHeaderConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("header_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewStoreHeaderConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go creates store, Java reads raw header", func() {
		It("should produce a header Java can parse with correct fields", func() {
			// Go creates the store
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header (no store open, pure proto parse)
			javaHeader, err := store.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw header for comparison
			goHeader, err := store.GetStoreHeaderRawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Both must agree on all fields
			Expect(javaHeader.FormatVersion).To(Equal(goHeader.FormatVersion),
				"format version mismatch: go=%d java=%d", goHeader.FormatVersion, javaHeader.FormatVersion)
			Expect(javaHeader.MetaDataVersion).To(Equal(goHeader.MetaDataVersion),
				"metadata version mismatch: go=%d java=%d", goHeader.MetaDataVersion, javaHeader.MetaDataVersion)
			Expect(javaHeader.UserVersion).To(Equal(goHeader.UserVersion),
				"user version mismatch: go=%d java=%d", goHeader.UserVersion, javaHeader.UserVersion)

			// Go creates with format version 9, user version 0
			Expect(goHeader.FormatVersion).To(Equal(int32(9)))
			Expect(goHeader.UserVersion).To(Equal(int32(0)))
			Expect(goHeader.MetaDataVersion).To(BeNumerically(">=", 0))
		})
	})

	Describe("Java creates store, Go reads raw header", func() {
		It("should produce a header Go can parse with correct fields", func() {
			// Java creates the store
			err := store.CreateStoreJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw header
			goHeader, err := store.GetStoreHeaderRawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header for comparison
			javaHeader, err := store.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Both must agree
			Expect(goHeader.FormatVersion).To(Equal(javaHeader.FormatVersion))
			Expect(goHeader.MetaDataVersion).To(Equal(javaHeader.MetaDataVersion))
			Expect(goHeader.UserVersion).To(Equal(javaHeader.UserVersion))

			// Java default format version is 7 (CACHEABLE_STATE), user version 0
			Expect(goHeader.FormatVersion).To(Equal(int32(7)))
			Expect(goHeader.UserVersion).To(Equal(int32(0)))
			Expect(goHeader.MetaDataVersion).To(BeNumerically(">=", 0))
		})
	})

	Describe("User version cross-platform persistence", func() {
		It("Go sets user version, Java reads it", func() {
			// Go creates store and sets user version
			err := store.SetUserVersionGo(ctx, 42)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header
			javaHeader, err := store.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(javaHeader.UserVersion).To(Equal(int32(42)))
		})

		It("Java sets user version, Go reads it", func() {
			// Java creates store with user version 99
			err := store.CreateStoreJavaWithUserVersion(ctx, 99)
			Expect(err).NotTo(HaveOccurred())

			// Go reads via store open
			goHeader, err := store.GetStoreHeaderViaOpenGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(goHeader.UserVersion).To(Equal(int32(99)))
		})
	})
})
