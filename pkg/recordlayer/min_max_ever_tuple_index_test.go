package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("MaxEverTupleIndex", func() {
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

		maxIdx := NewMaxEverTupleIndex("max_price_tuple", Ungrouped(Field("price")))
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
			// Value is tuple-decoded: the max as packed tuple element
			Expect(entries[0].Value).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(BeNumerically("==", 300))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete does NOT revert max (_EVER semantics)", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverTupleIndex("max_price_tuple", Ungrouped(Field("price")))
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

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(BeNumerically("==", 500))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("accepts negative values (unlike _LONG variant)", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverTupleIndex("max_price_tuple", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", maxIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(-10)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(-5)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			// BYTE_MAX with tuple encoding handles negatives correctly
			Expect(entries[0].Value[0]).To(BeNumerically("==", -5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("skips null values", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverTupleIndex("max_price_tuple", Ungrouped(Field("price")))
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
			// No price (null)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(BeNumerically("==", 100))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuilds correctly", func() {
		ks := specSubspace()

		maxIdx := NewMaxEverTupleIndex("max_price_tuple", Ungrouped(Field("price")))
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
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			return nil, store.RebuildIndex(maxIdx)
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			entries, err := AsList(ctx, store.ScanIndex(maxIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(BeNumerically("==", 500))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("MinEverTupleIndex", func() {
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

		minIdx := NewMinEverTupleIndex("min_price_tuple", Ungrouped(Field("price")))
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
			Expect(entries[0].Value[0]).To(BeNumerically("==", 100))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete does NOT revert min (_EVER semantics)", func() {
		ks := specSubspace()

		minIdx := NewMinEverTupleIndex("min_price_tuple", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", minIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			entries, err := AsList(ctx, store.ScanIndex(minIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(BeNumerically("==", 50))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles negative values correctly", func() {
		ks := specSubspace()

		minIdx := NewMinEverTupleIndex("min_price_tuple", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", minIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(-20)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(minIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(BeNumerically("==", -20))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
