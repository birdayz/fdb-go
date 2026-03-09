package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("CountIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		return builder
	}

	It("counts records by grouping key", func() {
		ks := specSubspace()

		// COUNT index grouped by price
		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 5 orders: 3 with price=100, 2 with price=200
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan count index — should see 2 entries
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Price 100: count=3
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))

			// Price 200: count=2
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("decrements count on delete", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 orders with price=100
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete one
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Count should be 2
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("updates count when record changes grouping key", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 2 orders with price=100, 1 with price=200
			for i := int64(1); i <= 2; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			order := &gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Update order 1 to change price from 100 to 200
			updatedOrder := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			_, err = store.SaveRecord(updatedOrder)
			Expect(err).NotTo(HaveOccurred())

			// Price 100: count=1, Price 200: count=2
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles ungrouped count (total count)", func() {
		ks := specSubspace()

		// Ungrouped count — empty grouping key, counts all records
		countIdx := NewCountIndex("total_count", Ungrouped(EmptyKey()))
		builder := baseMetaData()
		builder.AddUniversalIndex(countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 4 orders
			for i := int64(1); i <= 4; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Single entry with empty key, count=4
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(HaveLen(0))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(4)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scans specific grouping key with TupleRangeAllOf", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert orders with prices 100, 200, 300
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			order := &gen.Order{OrderId: proto.Int64(6), Price: proto.Int32(300)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Query only price 200
			entries, err := AsList(ctx, store.ScanIndex(countIdx,
				TupleRangeAllOf(tuple.Tuple{int64(200)}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reverse scans count index", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Reverse scan
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ReverseScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Reverse order: 200 first, then 100
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
