package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("MaxEverLongIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("tracks max value ungrouped", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{100, 300, 200} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(HaveLen(0))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(300)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete does NOT revert max (_EVER semantics)", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(500)})
			Expect(err).NotTo(HaveOccurred())

			// Delete the max record
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Max should still be 500 (irreversible)
			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(500)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("tracks max grouped by field", func() {
		ks := specSubspace()

		// MAX order_id grouped by price
		maxIdx := NewMaxEverLongIndex("max_id_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// price=100: orders 1,3; price=200: order 2
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// price=100: max(order_id) = 3
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))

			// price=200: max(order_id) = 2
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects negative values", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(-100)})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("negative value"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update applies new value via MAX (never decreases)", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert with price 500
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
			Expect(err).NotTo(HaveOccurred())

			// Update price to 200 (lower)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Max should still be 500 (MAX never decreases)
			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(500)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles rebuild correctly", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save records
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{100, 500, 300} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Rebuild
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			err = store.RebuildIndex(maxIdx)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(500)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("MinEverLongIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("tracks min value ungrouped", func() {
		ks := specSubspace()

		minIdx := NewMinEverLongIndex("min_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", minIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{300, 100, 200} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(minIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(HaveLen(0))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(100)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete does NOT revert min (_EVER semantics)", func() {
		ks := specSubspace()

		minIdx := NewMinEverLongIndex("min_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", minIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Delete the min record
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Min should still be 100 (irreversible)
			entries, err := AsList(ctx, store.ScanIndex(minIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(100)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update applies new value via MIN (never increases)", func() {
		ks := specSubspace()

		minIdx := NewMinEverLongIndex("min_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", minIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert with price 100
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Update price to 500 (higher)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
			Expect(err).NotTo(HaveOccurred())

			// Min should still be 100 (MIN never increases)
			entries, err := AsList(ctx, store.ScanIndex(minIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(100)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles empty store (no records)", func() {
		ks := specSubspace()

		minIdx := NewMinEverLongIndex("min_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", minIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(minIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
