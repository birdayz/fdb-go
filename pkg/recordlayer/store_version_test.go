package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("Store version access", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("GetFormatVersion", func() {
		It("returns the current format version", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.GetFormatVersion()).To(Equal(int32(FormatVersionCurrent)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetUserVersion / SetUserVersion", func() {
		It("defaults to 0", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.GetUserVersion()).To(Equal(int32(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("persists across reopens", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetUserVersion(42)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.GetUserVersion()).To(Equal(int32(42)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetMetaDataVersion", func() {
		It("returns the metadata version from the header", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.GetMetaDataVersion()).To(Equal(int32(md.Version())))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
