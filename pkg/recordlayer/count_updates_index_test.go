package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("CountUpdatesIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("counts inserts", func() {
		ks := specSubspace()

		idx := NewCountUpdatesIndex("update_count", Ungrouped(EmptyKey()))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := range 3 {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete does not decrement", func() {
		ks := specSubspace()

		idx := NewCountUpdatesIndex("update_count", Ungrouped(EmptyKey()))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Delete one — count should NOT decrease
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)})) // Still 2, not 1

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update increments even when key unchanged", func() {
		ks := specSubspace()

		// Grouped by price — re-saving with same price still increments
		idx := NewCountUpdatesIndex("update_count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert with price=100
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Update same record, same price — COUNT_UPDATES still increments
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)})) // 1 insert + 1 update

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update changes group key — increments new group only", func() {
		ks := specSubspace()

		idx := NewCountUpdatesIndex("update_count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert with price=100
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Update to price=200 — old group NOT decremented, new group incremented
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// price=100 still has count=1 (never decrements), price=200 has count=1
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("evaluates aggregate function", func() {
		ks := specSubspace()

		idx := NewCountUpdatesIndex("update_count", Ungrouped(EmptyKey()))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			// Update same record
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameCountUpdates, Operand: Ungrouped(EmptyKey())},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)})) // 1 insert + 1 update

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuild indexes correctly", func() {
		ks := specSubspace()

		idx := NewCountUpdatesIndex("update_count", Ungrouped(EmptyKey()))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 records
			for i := range 3 {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild
			err = store.RebuildIndex(idx)
			Expect(err).NotTo(HaveOccurred())

			// After rebuild: count = 3 (one per live record)
			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
