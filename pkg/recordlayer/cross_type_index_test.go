package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("CrossTypeIndexCleanup", func() {
	ctx := context.Background()

	// Metadata with type-specific indexes on both Order and Customer,
	// plus a universal index. Both types use the same PK field name
	// (order_id / customer_id are different fields but we use
	// RecordExistenceCheckNone to allow overwriting a record with a
	// different type at the same PK slot).
	buildMetaData := func() (*RecordMetaData, *Index, *Index, *Index) {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

		priceIdx := NewIndex("order_price_idx", Field("price"))
		builder.AddIndex("Order", priceIdx)

		emailIdx := NewIndex("customer_email_idx", Field("email"))
		builder.AddIndex("Customer", emailIdx)

		// Universal index — updated regardless of type change.
		universalIdx := NewIndex("type_idx", RecordTypeKey())
		builder.AddUniversalIndex(universalIdx)

		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, priceIdx, emailIdx, universalIdx
	}

	It("cleans up old type indexes on cross-type overwrite", func() {
		ks := specSubspace()
		md, priceIdx, emailIdx, universalIdx := buildMetaData()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save an Order with PK=1
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Verify: order_price_idx has 1 entry
			priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(priceEntries).To(HaveLen(1))
			Expect(priceEntries[0].Key).To(Equal(tuple.Tuple{int64(42), int64(1)}))

			// Verify: type_idx has 1 entry (Order's type key)
			typeEntries, err := AsList(ctx, store.ScanIndex(universalIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(typeEntries).To(HaveLen(1))

			// Verify: customer_email_idx is empty
			emailEntries, err := AsList(ctx, store.ScanIndex(emailIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(emailEntries).To(BeEmpty())

			// Now overwrite PK=1 with a Customer (cross-type overwrite)
			customer := &gen.Customer{CustomerId: proto.Int64(1), Email: proto.String("a@b.com")}
			_, err = store.SaveRecordWithOptions(customer, RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())

			// CRITICAL: order_price_idx must be empty (old type's index cleaned up)
			priceEntries, err = AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(priceEntries).To(BeEmpty(), "old type's index entries should be removed")

			// customer_email_idx should now have 1 entry
			emailEntries, err = AsList(ctx, store.ScanIndex(emailIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(emailEntries).To(HaveLen(1))
			Expect(emailEntries[0].Key).To(Equal(tuple.Tuple{"a@b.com", int64(1)}))

			// type_idx should still have 1 entry, but now the Customer type key
			typeEntries, err = AsList(ctx, store.ScanIndex(universalIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(typeEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles overwrite back to original type", func() {
		ks := specSubspace()
		md, priceIdx, emailIdx, _ := buildMetaData()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Order → Customer → Order at PK=1
			order1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			_, err = store.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			customer := &gen.Customer{CustomerId: proto.Int64(1), Email: proto.String("x@y.com")}
			_, err = store.SaveRecordWithOptions(customer, RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(99)}
			_, err = store.SaveRecordWithOptions(order2, RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())

			// price index: only the new order's entry
			priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(priceEntries).To(HaveLen(1))
			Expect(priceEntries[0].Key).To(Equal(tuple.Tuple{int64(99), int64(1)}))

			// email index: empty (customer's entry was cleaned up)
			emailEntries, err := AsList(ctx, store.ScanIndex(emailIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(emailEntries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("same-type overwrite still works normally", func() {
		ks := specSubspace()
		md, priceIdx, _, _ := buildMetaData()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			_, err = store.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(20)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			// Only the updated entry, not both
			priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(priceEntries).To(HaveLen(1))
			Expect(priceEntries[0].Key).To(Equal(tuple.Tuple{int64(20), int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cross-type delete only removes current type indexes", func() {
		ks := specSubspace()
		md, priceIdx, emailIdx, _ := buildMetaData()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save Order PK=1, Customer PK=2
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			customer := &gen.Customer{CustomerId: proto.Int64(2), Email: proto.String("z@w.com")}
			_, err = store.SaveRecord(customer)
			Expect(err).NotTo(HaveOccurred())

			// Delete the order
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			// price index empty, email index still has entry
			priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(priceEntries).To(BeEmpty())

			emailEntries, err := AsList(ctx, store.ScanIndex(emailIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(emailEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
