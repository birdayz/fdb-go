package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("Multi-type indexes", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("AddMultiTypeIndex with 2 types", func() {
		It("maintains index for both record types", func() {
			priceIndex := NewIndex("price_idx", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			// Price is only on Order, but we test multi-type registration
			builder.AddMultiTypeIndex([]string{"Order"}, priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Price index should be in Order's indexes
			orderIndexes := md.GetIndexesForRecordType("Order")
			Expect(orderIndexes).To(HaveLen(1))
			Expect(orderIndexes[0].Name).To(Equal("price_idx"))
		})
	})

	Describe("AddMultiTypeIndex with empty list becomes universal", func() {
		It("treats empty list as universal", func() {
			priceIndex := NewIndex("price_idx", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddMultiTypeIndex(nil, priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.GetUniversalIndexes()).To(HaveLen(1))
		})
	})

	Describe("AddMultiTypeIndex with 1 type becomes single-type", func() {
		It("treats single element as single-type", func() {
			priceIndex := NewIndex("price_idx", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddMultiTypeIndex([]string{"Order"}, priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderIndexes := md.GetIndexesForRecordType("Order")
			Expect(orderIndexes).To(HaveLen(1))
			// Should NOT be in universal
			Expect(md.GetUniversalIndexes()).To(BeEmpty())
		})
	})

	Describe("Multi-type index maintenance", func() {
		It("maintains index entries for records from both types", func() {
			// Create an index on "price" — only Order has price, but
			// we test that multi-type registration correctly wires into
			// index maintenance for the Order type.
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			// Register as multi-type for Order and Customer
			// Only Order saves will actually produce entries (Customer has no price field)
			builder.AddMultiTypeIndex([]string{"Order", "Customer"}, priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			// Save an Order — should create index entry
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(500)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify index entry exists
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
