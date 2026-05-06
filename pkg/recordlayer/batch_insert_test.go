package recordlayer

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("BatchInsert", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// Metadata with VALUE + COUNT + SUM indexes + record count.
	metaDataWithIndexes := func() *RecordMetaData {
		builder := baseMetaData()
		builder.AddIndex("Order", NewIndex("order_price_idx", Field("price")))
		builder.AddIndex("Order", NewCountIndex("order_count",
			GroupAll(Field("price"))))
		builder.AddIndex("Order", NewSumIndex("order_sum_price",
			Ungrouped(Field("price"))))
		builder.SetRecordCountKey(EmptyKey())
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	Describe("InsertBatch", func() {
		It("inserts 50 records and all are loadable with correct data", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			// Create store
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// InsertBatch 50 records
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 50)
				for i := 0; i < 50; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i * 10)),
					}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify all 50 records load back with correct data
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				for i := 0; i < 50; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "record %d should exist", i)

					order := rec.Record.(*gen.Order)
					Expect(order.GetOrderId()).To(Equal(int64(i)))
					Expect(order.GetPrice()).To(Equal(int32(i * 10)))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains correct record count for 50 records", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 50)
				for i := 0; i < 50; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(100)),
					}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(50)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains VALUE index entries for all inserted records", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()
			priceIdx := md.GetIndex("order_price_idx")

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert 50 records: 25 with price=100, 25 with price=200
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 50)
				for i := 0; i < 50; i++ {
					price := int32(100)
					if i >= 25 {
						price = 200
					}
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(price),
					}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(50))

				// First 25 should have price=100, next 25 price=200 (VALUE index sorts by price)
				for _, entry := range entries {
					price := entry.Key[0].(int64)
					Expect(price == 100 || price == 200).To(BeTrue(),
						"unexpected price in index: %d", price)
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains COUNT index with correct values", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()
			countIdx := md.GetIndex("order_count")

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// 10 records: 4 with price=100, 6 with price=200
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 10)
				for i := 0; i < 4; i++ {
					records[i] = &gen.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(100)}
				}
				for i := 4; i < 10; i++ {
					records[i] = &gen.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(200)}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// price=100 → count=4
				Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(4)}))

				// price=200 → count=6
				Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(6)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains SUM index with correct total", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()
			sumIdx := md.GetIndex("order_sum_price")

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// 5 records with prices: 10, 20, 30, 40, 50 → sum=150
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 5)
				for i := 0; i < 5; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32((i + 1) * 10)),
					}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Key).To(HaveLen(0))
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(150)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("then SaveRecord can update one of the batch-inserted records", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// InsertBatch 5 records
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 5)
				for i := 0; i < 5; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(100)),
					}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			// SaveRecord to update record 2 with a new price
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(2),
					Price:   proto.Int32(999),
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the update is visible
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				rec, err := store.LoadRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				order := rec.Record.(*gen.Order)
				Expect(order.GetPrice()).To(Equal(int32(999)))

				// Record count should still be 5 (update, not insert)
				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(5)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil error for empty slice", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				err = store.InsertBatch([]proto.Message{})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil record in batch", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				records := []proto.Message{
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
					nil,
					&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)},
				}
				err = store.InsertBatch(records)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nil"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil error for nil slice", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				err = store.InsertBatch(nil)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil record in InsertBatch", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				err = store.InsertBatch([]proto.Message{nil})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nil"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("single record batch works", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				return nil, store.InsertBatch([]proto.Message{
					&gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)},
				})
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				rec, err := store.LoadRecord(tuple.Tuple{int64(42)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(999)))

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(1)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SaveRecordBatch", func() {
		It("inserts 20 new records and returns stored results", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// SaveRecordBatch 20 records
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 20)
				for i := 0; i < 20; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i * 5)),
					}
				}
				results, err := store.SaveRecordBatch(records)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(20))

				// Verify returned stored records have correct PKs
				for i, r := range results {
					Expect(r.PrimaryKey).To(Equal(tuple.Tuple{int64(i)}))
					Expect(r.RecordType.Name).To(Equal("Order"))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify all 20 load back correctly
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				for i := 0; i < 20; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "record %d should exist", i)
					order := rec.Record.(*gen.Order)
					Expect(order.GetOrderId()).To(Equal(int64(i)))
					Expect(order.GetPrice()).To(Equal(int32(i * 5)))
				}

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(20)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("overwrites 5 of 20 records and updates index entries", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()
			sumIdx := md.GetIndex("order_sum_price")

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert 20 records with price=10 each → sum=200
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 20)
				for i := 0; i < 20; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(10),
					}
				}
				_, err = store.SaveRecordBatch(records)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Overwrite records 0-4 with price=100 (was 10).
			// New sum should be 5*100 + 15*10 = 500 + 150 = 650
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 5)
				for i := 0; i < 5; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(100),
					}
				}
				_, err = store.SaveRecordBatch(records)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				// Updated records should have new price
				for i := 0; i < 5; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil())
					Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(100)))
				}

				// Unchanged records keep old price
				for i := 5; i < 20; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil())
					Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(10)))
				}

				// Record count unchanged (20 — all updates, no new inserts)
				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(20)))

				// SUM index: 5*100 + 15*10 = 650
				entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(650)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles mixed record types with non-overlapping PKs", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetRecordCountKey(EmptyKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Use non-overlapping PKs: Order at PK=100, Customer at PK=200, Order at PK=300
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := []proto.Message{
					&gen.Order{OrderId: proto.Int64(100), Price: proto.Int32(50)},
					&gen.Customer{CustomerId: proto.Int64(200), Name: proto.String("Alice")},
					&gen.Order{OrderId: proto.Int64(300), Price: proto.Int32(75)},
				}
				results, err := store.SaveRecordBatch(records)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(3))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				// Order at PK=100
				rec, err := store.LoadRecord(tuple.Tuple{int64(100)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.RecordType.Name).To(Equal("Order"))
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(50)))

				// Customer at PK=200
				rec, err = store.LoadRecord(tuple.Tuple{int64(200)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.RecordType.Name).To(Equal("Customer"))
				Expect(rec.Record.(*gen.Customer).GetName()).To(Equal("Alice"))

				// Order at PK=300
				rec, err = store.LoadRecord(tuple.Tuple{int64(300)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.RecordType.Name).To(Equal("Order"))
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(75)))

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(3)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil record in batch", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordBatch([]proto.Message{nil})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nil"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for empty slice", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				results, err := store.SaveRecordBatch([]proto.Message{})
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SaveRecordBatchInsertOnly", func() {
		It("inserts 30 records and all are loadable", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 30)
				for i := 0; i < 30; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i + 1)),
					}
				}
				results, err := store.SaveRecordBatchInsertOnly(records)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(30))

				for i, r := range results {
					Expect(r.PrimaryKey).To(Equal(tuple.Tuple{int64(i)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify in a separate transaction
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				for i := 0; i < 30; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "record %d should exist", i)
					order := rec.Record.(*gen.Order)
					Expect(order.GetOrderId()).To(Equal(int64(i)))
					Expect(order.GetPrice()).To(Equal(int32(i + 1)))
				}

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(30)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains correct SUM index for 30 records", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()
			sumIdx := md.GetIndex("order_sum_price")

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// prices: 1+2+...+30 = 30*31/2 = 465
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 30)
				for i := 0; i < 30; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i + 1)),
					}
				}
				_, err = store.SaveRecordBatchInsertOnly(records)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(465)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for empty slice", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				results, err := store.SaveRecordBatchInsertOnly([]proto.Message{})
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil record in batch", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordBatchInsertOnly([]proto.Message{
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					nil,
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nil"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("corrupts data with mixed record types (compiled PK evaluator uses first record's field)", func() {
			// SaveRecordBatchInsertOnly compiles PK from records[0]'s type.
			// For Order, compiledPK looks up "order_id". When applied to Customer
			// (which has "customer_id", not "order_id"), the compiled evaluator
			// appends nil instead of the real PK value. The record is written at
			// PK=[nil] instead of PK=[200]. This documents the constraint:
			// SaveRecordBatchInsertOnly is for homogeneous batches only.
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetRecordCountKey(EmptyKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert mixed: Order(100) + Customer(200). No error returned.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := []proto.Message{
					&gen.Order{OrderId: proto.Int64(100), Price: proto.Int32(50)},
					&gen.Customer{CustomerId: proto.Int64(200), Name: proto.String("Bob")},
				}
				_, err = store.SaveRecordBatchInsertOnly(records)
				Expect(err).NotTo(HaveOccurred()) // silent success
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Order at PK=100 loads fine (first record, compiled PK correct)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				rec, err := store.LoadRecord(tuple.Tuple{int64(100)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.RecordType.Name).To(Equal("Order"))

				// Customer at PK=200 is NOT found — it was written at PK=[nil]
				// because the compiled PK evaluator resolved "order_id" on Customer → nil
				rec, err = store.LoadRecord(tuple.Tuple{int64(200)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).To(BeNil(), "Customer not at expected PK due to compiled PK mismatch")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("InsertBatch with UNIQUE index", func() {
		It("inserts records and UNIQUE index entries are scannable", func() {
			ks := specSubspace()
			builder := baseMetaData()
			uniqueIdx := NewIndex("order_price_unique", Field("price")).SetUnique()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// InsertBatch 50 records with unique prices
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 50)
				for i := 0; i < 50; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i * 100)), // unique prices
					}
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify all 50 UNIQUE index entries are scannable and records loadable
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				idx := md.GetIndex("order_price_unique")
				Expect(idx).NotTo(BeNil())
				entries, err := AsList(context.Background(),
					store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(50))

				// Verify each entry points to a valid record
				for _, entry := range entries {
					pk := entry.PrimaryKey()
					rec, err := store.LoadRecord(pk)
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("InsertBatch mixed types", func() {
		It("rejects mixed record types with explicit error", func() {
			// InsertBatch requires all records to be the same proto message type.
			// Mixing types is now rejected with a clear error message.
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetRecordCountKey(EmptyKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := []proto.Message{
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
					&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Alice")},
				}
				return nil, store.InsertBatch(records)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not match batch type"))
		})
	})

	Describe("SaveRecordBatch vs SaveRecord equivalence", func() {
		It("produces identical results to N sequential SaveRecord calls", func() {
			ks1 := specSubspace()
			// Use a completely separate subspace for the sequential path.
			// Must not be a prefix/suffix of ks1.
			ks2 := subspace.FromBytes(tuple.Tuple{"batch-equivalence-sequential-" + CurrentSpecReport().FullText()}.Pack())
			md := metaDataWithIndexes()
			sumIdx := md.GetIndex("order_sum_price")
			countIdx := md.GetIndex("order_count")

			// Create both stores
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks1).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks2).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Batch path
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks1).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 10)
				for i := 0; i < 10; i++ {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32((i + 1) * 10)),
					}
				}
				_, err = store.SaveRecordBatch(records)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Sequential path
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks2).Open()
				if err != nil {
					return nil, err
				}
				for i := 0; i < 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32((i + 1) * 10)),
					})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Compare: record count, SUM, COUNT, and all records should be identical
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				storeBatch, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks1).Open()
				if err != nil {
					return nil, err
				}
				storeSeq, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks2).Open()
				if err != nil {
					return nil, err
				}

				// Record counts
				countBatch, err := storeBatch.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				countSeq, err := storeSeq.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(countBatch).To(Equal(countSeq))

				// SUM index
				sumBatch, err := AsList(ctx, storeBatch.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				sumSeq, err := AsList(ctx, storeSeq.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(sumBatch).To(HaveLen(len(sumSeq)))
				for i := range sumBatch {
					Expect(sumBatch[i].Key).To(Equal(sumSeq[i].Key))
					Expect(sumBatch[i].Value).To(Equal(sumSeq[i].Value))
				}

				// COUNT index
				cntBatch, err := AsList(ctx, storeBatch.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				cntSeq, err := AsList(ctx, storeSeq.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(cntBatch).To(HaveLen(len(cntSeq)))
				for i := range cntBatch {
					Expect(cntBatch[i].Key).To(Equal(cntSeq[i].Key))
					Expect(cntBatch[i].Value).To(Equal(cntSeq[i].Value))
				}

				// All records
				for i := 0; i < 10; i++ {
					recBatch, err := storeBatch.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					recSeq, err := storeSeq.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(recBatch).NotTo(BeNil())
					Expect(recSeq).NotTo(BeNil())
					Expect(recBatch.Record.(*gen.Order).GetPrice()).
						To(Equal(recSeq.Record.(*gen.Order).GetPrice()))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SaveRecordBatchInsertOnly", func() {
		It("inserts records and returns correct results", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 20)
				for i := range 20 {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i * 10)),
					}
				}
				results, err := store.SaveRecordBatchInsertOnly(records)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(20))

				// Verify each result has correct PK and record type.
				for i, r := range results {
					Expect(r.PrimaryKey).To(Equal(tuple.Tuple{int64(i)}))
					Expect(r.RecordType.Name).To(Equal("Order"))
					Expect(r.Record.(*gen.Order).GetPrice()).To(Equal(int32(i * 10)))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify records are loadable in a new transaction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				for i := range 20 {
					rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "record %d not found", i)
					Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(i * 10)))
				}

				// Verify record count is correct.
				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(20)))

				// Verify VALUE index entries.
				priceIdx := md.GetIndex("order_price_idx")
				entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20))

				// Verify SUM index.
				sumIdx := md.GetIndex("order_sum_price")
				sumEntries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(sumEntries).To(HaveLen(1))
				// Sum of 0+10+20+...+190 = 10*(0+1+2+...+19) = 10*190 = 1900
				Expect(sumEntries[0].Value).To(Equal(tuple.Tuple{int64(1900)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for empty slice", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				results, err := store.SaveRecordBatchInsertOnly(nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for nil record in batch", func() {
			ks := specSubspace()
			md := metaDataWithIndexes()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordBatchInsertOnly([]proto.Message{nil})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nil"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saves record versions when versioning enabled", func() {
			builder := baseMetaData()
			builder.SetStoreRecordVersions(true)
			builder.SetRecordCountKey(EmptyKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				records := make([]proto.Message, 5)
				for i := range records {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i + 1)),
						Price:   proto.Int32(int32((i + 1) * 10)),
					}
				}
				results, err := store.SaveRecordBatchInsertOnly(records)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(5))

				for i, r := range results {
					Expect(r.Version).NotTo(BeNil(), "record %d should have version", i)
					Expect(r.Version.IsComplete()).To(BeFalse(), "version should be incomplete pre-commit")
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify versions are complete after commit.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 5; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{i})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil())
					Expect(rec.Version).NotTo(BeNil())
					Expect(rec.Version.IsComplete()).To(BeTrue())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("batch save with record versioning", func() {
		It("SaveRecordBatch assigns versions to new records", func() {
			builder := baseMetaData()
			builder.SetStoreRecordVersions(true)
			builder.SetRecordCountKey(EmptyKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()

			// Save via batch in one transaction, then load in another to get committed versions.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				records := make([]proto.Message, 5)
				for i := range records {
					records[i] = &gen.Order{
						OrderId: proto.Int64(int64(i + 1)),
						Price:   proto.Int32(int32((i + 1) * 10)),
					}
				}
				results, err := store.SaveRecordBatch(records)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(5))

				// Within the same tx, versions are incomplete (versionstamp placeholder).
				for i, r := range results {
					Expect(r.Version).NotTo(BeNil(), "record %d should have a version", i)
					Expect(r.Version.IsComplete()).To(BeFalse(), "version should be incomplete before commit")
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Load in a new transaction — versions should now be complete.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{i})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "record %d not found", i)
					Expect(rec.Version).NotTo(BeNil(), "record %d should have a version after load", i)
					Expect(rec.Version.IsComplete()).To(BeTrue(), "record %d version should be complete after commit", i)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
