package recordlayer

import (
	"context"
	"errors"

	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("StaleMetaDataVersion", func() {
	ctx := context.Background()

	buildMD := func(version int) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetVersion(version)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	It("returns error when stored version is newer than local version", func() {
		ks := specSubspace()

		// Create store with version 5.
		md5 := buildMD(5)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md5).SetSubspace(ks).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to open with version 3 — should fail with StaleMetaDataVersionError.
		md3 := buildMD(3)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md3).SetSubspace(ks).Open()
			return nil, err
		})
		Expect(err).To(HaveOccurred())

		var staleErr *StaleMetaDataVersionError
		Expect(errors.As(err, &staleErr)).To(BeTrue(), "expected StaleMetaDataVersionError, got: %v", err)
		Expect(staleErr.LocalVersion).To(Equal(3))
		Expect(staleErr.StoredVersion).To(Equal(5))
	})

	It("succeeds when stored version equals local version", func() {
		ks := specSubspace()

		md := buildMD(5)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Re-open with same version — should succeed.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("succeeds when local version is newer (triggers rebuild)", func() {
		ks := specSubspace()

		md3 := buildMD(3)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md3).SetSubspace(ks).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Open with higher version — should succeed (checkPossiblyRebuild proceeds).
		md5 := buildMD(5)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md5).SetSubspace(ks).Open()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("error message includes both versions", func() {
		err := &StaleMetaDataVersionError{LocalVersion: 2, StoredVersion: 7}
		Expect(err.Error()).To(ContainSubstring("local 2"))
		Expect(err.Error()).To(ContainSubstring("stored 7"))
	})
})
