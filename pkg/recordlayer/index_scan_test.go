package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("IndexScanning", func() {
	ctx := context.Background()

	buildMetaWithIndex := func(indexes ...*Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		for _, idx := range indexes {
			builder.AddIndex("Order", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// insertOrders creates n Order records with sequential IDs and prices.
	insertOrders := func(store *FDBRecordStore, count int) {
		for i := 1; i <= count; i++ {
			order := &gen.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i * 100))}
			_, err := store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	Describe("ScanIndex", func() {
		It("scans all entries in an index", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5)

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				// Entries should be in ascending price order (FDB tuple ordering)
				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 100)
					expectedPK := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{expectedPK}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scans index in reverse order", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 3)

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ReverseScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// Reverse: highest price first
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[2].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scans empty index", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scans with row limit", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5)

				scan := ForwardScan()
				scan.ExecuteProperties.ReturnedRowLimit = 2

				cursor := store.ScanIndex(priceIndex, TupleRangeAll, nil, scan)

				// First entry
				r1, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r1.HasNext()).To(BeTrue())
				Expect(r1.GetValue().IndexValues()).To(Equal(tuple.Tuple{int64(100)}))

				// Second entry
				r2, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r2.HasNext()).To(BeTrue())
				Expect(r2.GetValue().IndexValues()).To(Equal(tuple.Tuple{int64(200)}))

				// Should stop — limit reached with more data available
				r3, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r3.HasNext()).To(BeFalse())
				Expect(r3.GetNoNextReason()).To(Equal(ReturnLimitReached))
				contBytes, contErr := r3.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				Expect(contBytes).NotTo(BeNil())

				Expect(cursor.Close()).To(Succeed())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("supports continuation for paging", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5)

				// Page 1: first 2 entries
				scan := ForwardScan()
				scan.ExecuteProperties.ReturnedRowLimit = 2
				entries1, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, scan))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries1).To(HaveLen(2))
				Expect(entries1[0].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
				Expect(entries1[1].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))

				// Get continuation from the last entry (need OnNext for this)
				scan2 := ForwardScan()
				scan2.ExecuteProperties.ReturnedRowLimit = 2
				cursor := store.ScanIndex(priceIndex, TupleRangeAll, nil, scan2)
				var continuation []byte
				for {
					r, nextErr := cursor.OnNext(ctx)
					Expect(nextErr).NotTo(HaveOccurred())
					if !r.HasNext() {
						var contErr error
						continuation, contErr = r.GetContinuation().ToBytes()
						Expect(contErr).NotTo(HaveOccurred())
						break
					}
				}
				Expect(cursor.Close()).To(Succeed())
				Expect(continuation).NotTo(BeNil())

				// Page 2: next 2 entries using continuation
				scan3 := ForwardScan()
				scan3.ExecuteProperties.ReturnedRowLimit = 2
				cursor2 := store.ScanIndex(priceIndex, TupleRangeAll, continuation, scan3)
				var page2 []*IndexEntry
				for {
					r, nextErr := cursor2.OnNext(ctx)
					Expect(nextErr).NotTo(HaveOccurred())
					if !r.HasNext() {
						var contErr error
						continuation, contErr = r.GetContinuation().ToBytes()
						Expect(contErr).NotTo(HaveOccurred())
						break
					}
					page2 = append(page2, r.GetValue())
				}
				Expect(cursor2.Close()).To(Succeed())
				Expect(page2).To(HaveLen(2))
				Expect(page2[0].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))
				Expect(page2[1].IndexValues()).To(Equal(tuple.Tuple{int64(400)}))

				// Page 3: last entry
				scan4 := ForwardScan()
				scan4.ExecuteProperties.ReturnedRowLimit = 2
				page3, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, continuation, scan4))
				Expect(err).NotTo(HaveOccurred())
				Expect(page3).To(HaveLen(1))
				Expect(page3[0].IndexValues()).To(Equal(tuple.Tuple{int64(500)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("supports reverse continuation", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 4)

				// Page 1 reverse: 2 highest prices
				scan := ReverseScan()
				scan.ExecuteProperties.ReturnedRowLimit = 2
				cursor := store.ScanIndex(priceIndex, TupleRangeAll, nil, scan)
				var page1 []*IndexEntry
				var continuation []byte
				for {
					r, nextErr := cursor.OnNext(ctx)
					Expect(nextErr).NotTo(HaveOccurred())
					if !r.HasNext() {
						var contErr error
						continuation, contErr = r.GetContinuation().ToBytes()
						Expect(contErr).NotTo(HaveOccurred())
						break
					}
					page1 = append(page1, r.GetValue())
				}
				Expect(cursor.Close()).To(Succeed())
				Expect(page1).To(HaveLen(2))
				Expect(page1[0].IndexValues()).To(Equal(tuple.Tuple{int64(400)}))
				Expect(page1[1].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))

				// Page 2: remaining 2 in reverse
				scan2 := ReverseScan()
				scan2.ExecuteProperties.ReturnedRowLimit = 2
				page2, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, continuation, scan2))
				Expect(err).NotTo(HaveOccurred())
				Expect(page2).To(HaveLen(2))
				Expect(page2[0].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))
				Expect(page2[1].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("TupleRange prefix scan", func() {
		It("scans with TupleRangeAllOf for prefix match", func() {
			// Index on customer name — use Customer records for string index
			nameIndex := NewIndex("Customer$name", Field("name"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Customer", nameIndex)
			metaData, buildErr := builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert customers with different names
				for i, name := range []string{"Alice", "Alice", "Bob", "Charlie"} {
					customer := &gen.Customer{CustomerId: proto.Int64(int64(i + 1)), Name: proto.String(name)}
					_, err = store.SaveRecord(customer)
					Expect(err).NotTo(HaveOccurred())
				}

				// Scan for all "Alice" entries
				entries, err := AsList(ctx, store.ScanIndex(nameIndex, TupleRangeAllOf(tuple.Tuple{"Alice"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

				// Scan for "Bob"
				entries, err = AsList(ctx, store.ScanIndex(nameIndex, TupleRangeAllOf(tuple.Tuple{"Bob"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				// Scan for "Nobody" — should be empty
				entries, err = AsList(ctx, store.ScanIndex(nameIndex, TupleRangeAllOf(tuple.Tuple{"Nobody"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("TupleRange between", func() {
		It("scans with inclusive low, exclusive high", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5) // prices: 100, 200, 300, 400, 500

				// Between [200, 400) — should get prices 200, 300
				scanRange := TupleRangeBetween(tuple.Tuple{int64(200)}, tuple.Tuple{int64(400)})
				entries, err := AsList(ctx, store.ScanIndex(priceIndex, scanRange, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scans with both inclusive", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5) // prices: 100, 200, 300, 400, 500

				// BetweenInclusive [200, 400] — should get prices 200, 300, 400
				scanRange := TupleRangeBetweenInclusive(tuple.Tuple{int64(200)}, tuple.Tuple{int64(400)})
				entries, err := AsList(ctx, store.ScanIndex(priceIndex, scanRange, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))
				Expect(entries[2].IndexValues()).To(Equal(tuple.Tuple{int64(400)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scans with exclusive low", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5) // prices: 100, 200, 300, 400, 500

				// Exclusive low (200, 500] — should get 300, 400, 500
				scanRange := TupleRange{
					Low:          tuple.Tuple{int64(200)},
					High:         tuple.Tuple{int64(500)},
					LowEndpoint:  EndpointTypeRangeExclusive,
					HighEndpoint: EndpointTypeRangeInclusive,
				}
				entries, err := AsList(ctx, store.ScanIndex(priceIndex, scanRange, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(400)}))
				Expect(entries[2].IndexValues()).To(Equal(tuple.Tuple{int64(500)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Composite index scanning", func() {
		It("scans composite index and extracts primary key correctly", func() {
			// Composite index on (price, order_id) — 2 indexed columns
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))
			metaData := buildMetaWithIndex(compositeIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 3)

				entries, err := AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// Entry key: (price, order_id, pk=order_id) — 3 elements
				// IndexValues = (price, order_id) — 2 elements (composite column size)
				// PrimaryKey = (pk=order_id) — 1 element
				for i, entry := range entries {
					price := int64((i + 1) * 100)
					pk := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{price, pk}))
					Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{pk}))
					Expect(entry.Key).To(HaveLen(2)) // 2 indexed, PK deduplicated
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("supports prefix scan on composite index", func() {
			// Composite index on (price, order_id)
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))
			metaData := buildMetaWithIndex(compositeIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert orders: 3 with price=100, 2 with price=200
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

				// Prefix scan for price=100 — should get 3 entries
				entries, err := AsList(ctx, store.ScanIndex(compositeIndex,
					TupleRangeAllOf(tuple.Tuple{int64(100)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// Prefix scan for price=200 — should get 2 entries
				entries, err = AsList(ctx, store.ScanIndex(compositeIndex,
					TupleRangeAllOf(tuple.Tuple{int64(200)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IndexEntry methods", func() {
		It("returns correct column size for various expressions", func() {
			Expect(Field("x").ColumnSize()).To(Equal(1))
			Expect(Concat(Field("x"), Field("y")).ColumnSize()).To(Equal(2))
			Expect(Concat(Field("x"), Field("y"), Field("z")).ColumnSize()).To(Equal(3))
			Expect(EmptyKey().ColumnSize()).To(Equal(0))
			Expect(RecordTypeKey().ColumnSize()).To(Equal(1))
			Expect(RecordTypeKey().Nest(Field("x")).ColumnSize()).To(Equal(2))
		})
	})

	Describe("Seq iterators", func() {
		It("works with Seq", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 3)

				var prices []int64
				for entry, iterErr := range Seq2(store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
					Expect(iterErr).NotTo(HaveOccurred())
					prices = append(prices, entry.IndexValues()[0].(int64))
				}
				Expect(prices).To(Equal([]int64{100, 200, 300}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("works with Seq2", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 3)

				var count int
				for _, scanErr := range Seq2(store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
					Expect(scanErr).NotTo(HaveOccurred())
					count++
				}
				Expect(count).To(Equal(3))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetIndex metadata lookup", func() {
		It("returns nil for unknown index", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.GetIndex("nonexistent")).To(BeNil())
		})

		It("returns the correct index", func() {
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			idx := md.GetIndex("Order$price")
			Expect(idx).NotTo(BeNil())
			Expect(idx.Name).To(Equal("Order$price"))
		})
	})

	Describe("Repeated field fan-out", func() {
		It("creates multiple index entries for repeated field values", func() {
			tagIndex := NewIndex("Order$tags", FanOut("tags"))
			metaData := buildMetaWithIndex(tagIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Order with 3 tags → 3 index entries
				order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"urgent", "fragile", "gift"}}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// FDB tuple ordering: "fragile" < "gift" < "urgent"
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{"fragile"}))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{"gift"}))
				Expect(entries[2].IndexValues()).To(Equal(tuple.Tuple{"urgent"}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("creates no index entries for empty repeated field", func() {
			tagIndex := NewIndex("Order$tags", FanOut("tags"))
			metaData := buildMetaWithIndex(tagIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Order with no tags → no index entries
				order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles fan-out with prefix scan", func() {
			tagIndex := NewIndex("Order$tags", FanOut("tags"))
			metaData := buildMetaWithIndex(tagIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Order 1: tags ["urgent", "gift"]
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"urgent", "gift"}})
				Expect(err).NotTo(HaveOccurred())
				// Order 2: tags ["urgent", "fragile"]
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Tags: []string{"urgent", "fragile"}})
				Expect(err).NotTo(HaveOccurred())
				// Order 3: tags ["gift"]
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Tags: []string{"gift"}})
				Expect(err).NotTo(HaveOccurred())

				// Find all orders tagged "urgent" — should be PKs 1 and 2
				entries, err := AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAllOf(tuple.Tuple{"urgent"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

				// Find all orders tagged "gift" — should be PKs 1 and 3
				entries, err = AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAllOf(tuple.Tuple{"gift"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)}))

				// Find all orders tagged "fragile" — should be PK 2 only
				entries, err = AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAllOf(tuple.Tuple{"fragile"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("removes old fan-out entries on update", func() {
			tagIndex := NewIndex("Order$tags", FanOut("tags"))
			metaData := buildMetaWithIndex(tagIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save with tags ["urgent", "gift"]
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"urgent", "gift"}})
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// Update to tags ["fragile"] — "urgent" and "gift" entries should be removed
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"fragile"}})
				Expect(err).NotTo(HaveOccurred())

				entries, err = AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{"fragile"}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles composite index with fan-out", func() {
			// Index on (price, tags) — fan-out on tags creates entries for each (price, tag) pair
			priceTagIndex := NewIndex("Order$price_tags", Concat(Field("price"), FanOut("tags")))
			metaData := buildMetaWithIndex(priceTagIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"urgent", "gift"}}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceTagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2)) // (100,"gift") and (100,"urgent")

				// Entries are ordered by (price, tag) — "gift" < "urgent"
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100), "gift"}))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(100), "urgent"}))

				// Prefix scan for price=100 should get both
				entries, err = AsList(ctx, store.ScanIndex(priceTagIndex,
					TupleRangeAllOf(tuple.Tuple{int64(100)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes all fan-out entries on record delete", func() {
			tagIndex := NewIndex("Order$tags", FanOut("tags"))
			metaData := buildMetaWithIndex(tagIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"a", "b", "c"}})
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// Delete the record — all 3 index entries should be removed
				_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())

				entries, err = AsList(ctx, store.ScanIndex(tagIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("errors on FanTypeNone with repeated field", func() {
			// Using Field() (FanTypeNone) on a repeated field is caught at Build() time
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			badIndex := NewIndex("Order$tags_bad", Field("tags"))
			builder.AddIndex("Order", badIndex)
			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("repeated"))
		})
	})

	Describe("NestingKeyExpression", func() {
		It("indexes a field inside a nested message", func() {
			// Index on flower.type — navigate into Flower submessage
			flowerTypeIndex := NewIndex("Order$flower_type", Nest("flower", Field("type")))
			metaData := buildMetaWithIndex(flowerTypeIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				rose := &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()}
				tulip := &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_YELLOW.Enum()}

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Flower: rose})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Flower: tulip})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Flower: rose})
				Expect(err).NotTo(HaveOccurred())

				// Scan all — should have 3 entries ordered by flower type string
				entries, err := AsList(ctx, store.ScanIndex(flowerTypeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{"Rose"}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{"Rose"}))
				Expect(entries[2].IndexValues()).To(Equal(tuple.Tuple{"Tulip"}))

				// Prefix scan for "Rose" — PKs 1 and 3
				entries, err = AsList(ctx, store.ScanIndex(flowerTypeIndex,
					TupleRangeAllOf(tuple.Tuple{"Rose"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("computes correct column size for nested expressions", func() {
			// Nest("flower", Field("type")) → 1 column (child's column size)
			Expect(Nest("flower", Field("type")).ColumnSize()).To(Equal(1))
			// Nest("flower", Concat(Field("type"), Field("color"))) → 2 columns
			Expect(Nest("flower", Concat(Field("type"), Field("color"))).ColumnSize()).To(Equal(2))
		})

		It("indexes composite nested fields", func() {
			// Index on (flower.type, flower.color) via nesting
			flowerIndex := NewIndex("Order$flower", Nest("flower", Concat(Field("type"), Field("color"))))
			metaData := buildMetaWithIndex(flowerIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1), Price: proto.Int32(100),
					Flower: &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(2), Price: proto.Int32(200),
					Flower: &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_BLUE.Enum()},
				})
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(flowerIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// Both Roses, ordered by color enum value (BLUE=2, RED=1)
				// Proto enum values are int64 in FDB tuples
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{"Rose", int64(1)})) // RED=1
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{"Rose", int64(2)})) // BLUE=2

				// PK extraction with 2-column nested index
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("works with composite index mixing nested and top-level fields", func() {
			// Index on (price, flower.type) — composite of top-level + nested
			priceFlowerIndex := NewIndex("Order$price_flower",
				Concat(Field("price"), Nest("flower", Field("type"))))
			metaData := buildMetaWithIndex(priceFlowerIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1), Price: proto.Int32(100),
					Flower: &gen.Flower{Type: proto.String("Rose")},
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(2), Price: proto.Int32(100),
					Flower: &gen.Flower{Type: proto.String("Tulip")},
				})
				Expect(err).NotTo(HaveOccurred())

				// Prefix scan for price=100 — both flowers
				entries, err := AsList(ctx, store.ScanIndex(priceFlowerIndex,
					TupleRangeAllOf(tuple.Tuple{int64(100)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100), "Rose"}))
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(100), "Tulip"}))
				// Column size is 2 (price + flower.type), so PK is 3rd element
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ScanIndexRecords", func() {
		It("returns full records via index lookup", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 3)

				records, err := AsList(ctx, store.ScanIndexRecords("Order$price", TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(3))

				for i, rec := range records {
					expectedPK := int64(i + 1)
					expectedPrice := int32((i + 1) * 100)

					Expect(rec.IndexEntry).NotTo(BeNil())
					Expect(rec.IndexEntry.IndexValues()).To(Equal(tuple.Tuple{int64(expectedPrice)}))

					Expect(rec.Record).NotTo(BeNil())
					Expect(rec.Record.PrimaryKey).To(Equal(tuple.Tuple{expectedPK}))
					order := rec.Record.Record.(*gen.Order)
					Expect(order.GetOrderId()).To(Equal(expectedPK))
					Expect(order.GetPrice()).To(Equal(expectedPrice))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("respects TupleRange filtering", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5)

				records, err := AsList(ctx, store.ScanIndexRecords(
					"Order$price",
					TupleRangeAllOf(tuple.Tuple{int64(300)}),
					nil,
					ForwardScan(),
				))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(1))
				Expect(records[0].Record.Record.(*gen.Order).GetPrice()).To(Equal(int32(300)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for unknown index", func() {
			metaData := buildMetaWithIndex()
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				cursor := store.ScanIndexRecords("nonexistent", TupleRangeAll, nil, ForwardScan())
				_, scanErr := cursor.OnNext(ctx)
				Expect(scanErr).To(HaveOccurred())
				var notFound *IndexNotFoundError
				Expect(errors.As(scanErr, &notFound)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("supports row limits", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 5)

				scanProps := ForwardScan()
				scanProps.ExecuteProperties.ReturnedRowLimit = 2

				records, err := AsList(ctx, store.ScanIndexRecords("Order$price", TupleRangeAll, nil, scanProps))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(2))

				Expect(records[0].Record.Record.(*gen.Order).GetPrice()).To(Equal(int32(100)))
				Expect(records[1].Record.Record.(*gen.Order).GetPrice()).To(Equal(int32(200)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("supports reverse scan", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			metaData := buildMetaWithIndex(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				insertOrders(store, 3)

				records, err := AsList(ctx, store.ScanIndexRecords("Order$price", TupleRangeAll, nil, ReverseScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(3))

				Expect(records[0].Record.Record.(*gen.Order).GetPrice()).To(Equal(int32(300)))
				Expect(records[1].Record.Record.(*gen.Order).GetPrice()).To(Equal(int32(200)))
				Expect(records[2].Record.Record.(*gen.Order).GetPrice()).To(Equal(int32(100)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("TupleRange.ToFDBRange", func() {
		It("TupleRangeAll covers entire subspace range", func() {
			ss := subspace.Sub("test")

			kr := TupleRangeAll.ToFDBRange(ss)

			// Begin should be the subspace key itself (TreeStart).
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(ss.FDBKey())))

			// End should be the subspace end key from FDBRangeKeys.
			_, expectedEnd := ss.FDBRangeKeys()
			Expect([]byte(kr.End.(fdb.Key))).To(Equal([]byte(expectedEnd.(fdb.Key))))
		})

		It("TupleRangeAllOf covers prefix range", func() {
			ss := subspace.Sub("test")
			prefix := tuple.Tuple{"alice"}

			kr := TupleRangeAllOf(prefix).ToFDBRange(ss)

			// Low is inclusive: begin = ss.Pack(prefix)
			expectedBegin := ss.Pack(prefix)
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(expectedBegin)))

			// High is inclusive: end = ss.Pack(prefix) + 0xFF
			expectedEnd := append([]byte(nil), ss.Pack(prefix)...)
			expectedEnd = append(expectedEnd, 0xFF)
			Expect([]byte(kr.End.(fdb.Key))).To(Equal(expectedEnd))
		})

		It("TupleRangeBetween has inclusive low and exclusive high", func() {
			ss := subspace.Sub("test")
			low := tuple.Tuple{int64(100)}
			high := tuple.Tuple{int64(300)}

			kr := TupleRangeBetween(low, high).ToFDBRange(ss)

			// Low inclusive: begin = ss.Pack(low)
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(ss.Pack(low))))

			// High exclusive: end = ss.Pack(high) (exact, not +0xFF)
			Expect([]byte(kr.End.(fdb.Key))).To(Equal([]byte(ss.Pack(high))))
		})

		It("TupleRangeBetweenInclusive has both inclusive endpoints", func() {
			ss := subspace.Sub("test")
			low := tuple.Tuple{int64(100)}
			high := tuple.Tuple{int64(300)}

			kr := TupleRangeBetweenInclusive(low, high).ToFDBRange(ss)

			// Low inclusive: begin = ss.Pack(low)
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(ss.Pack(low))))

			// High inclusive: end = ss.Pack(high) + 0xFF
			expectedEnd := append([]byte(nil), ss.Pack(high)...)
			expectedEnd = append(expectedEnd, 0xFF)
			Expect([]byte(kr.End.(fdb.Key))).To(Equal(expectedEnd))
		})

		It("exclusive low uses strinc", func() {
			ss := subspace.Sub("test")
			low := tuple.Tuple{int64(200)}
			high := tuple.Tuple{int64(500)}

			r := TupleRange{
				Low:          low,
				High:         high,
				LowEndpoint:  EndpointTypeRangeExclusive,
				HighEndpoint: EndpointTypeRangeInclusive,
			}
			kr := r.ToFDBRange(ss)

			// Low exclusive: begin = strinc(ss.Pack(low))
			packed := ss.Pack(low)
			expectedBegin, err := fdb.Strinc(packed)
			Expect(err).NotTo(HaveOccurred())
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(expectedBegin)))

			// High inclusive: end = ss.Pack(high) + 0xFF
			expectedEnd := append([]byte(nil), ss.Pack(high)...)
			expectedEnd = append(expectedEnd, 0xFF)
			Expect([]byte(kr.End.(fdb.Key))).To(Equal(expectedEnd))
		})

		It("begin is before end for all range types", func() {
			ss := subspace.Sub("ordering")

			// TupleRangeAll
			kr := TupleRangeAll.ToFDBRange(ss)
			Expect(bytes.Compare(kr.Begin.(fdb.Key), kr.End.(fdb.Key))).To(BeNumerically("<", 0))

			// TupleRangeAllOf
			kr = TupleRangeAllOf(tuple.Tuple{"x"}).ToFDBRange(ss)
			Expect(bytes.Compare(kr.Begin.(fdb.Key), kr.End.(fdb.Key))).To(BeNumerically("<", 0))

			// TupleRangeBetween
			kr = TupleRangeBetween(tuple.Tuple{int64(1)}, tuple.Tuple{int64(100)}).ToFDBRange(ss)
			Expect(bytes.Compare(kr.Begin.(fdb.Key), kr.End.(fdb.Key))).To(BeNumerically("<", 0))

			// TupleRangeBetweenInclusive
			kr = TupleRangeBetweenInclusive(tuple.Tuple{int64(1)}, tuple.Tuple{int64(100)}).ToFDBRange(ss)
			Expect(bytes.Compare(kr.Begin.(fdb.Key), kr.End.(fdb.Key))).To(BeNumerically("<", 0))
		})

		It("all ranges are properly scoped to the subspace", func() {
			ss := subspace.Sub("scoped")
			ssPrefix := []byte(ss.FDBKey())

			ranges := []TupleRange{
				TupleRangeAll,
				TupleRangeAllOf(tuple.Tuple{"val"}),
				TupleRangeBetween(tuple.Tuple{int64(1)}, tuple.Tuple{int64(10)}),
				TupleRangeBetweenInclusive(tuple.Tuple{int64(1)}, tuple.Tuple{int64(10)}),
			}

			for _, r := range ranges {
				kr := r.ToFDBRange(ss)
				beginKey := []byte(kr.Begin.(fdb.Key))
				endKey := []byte(kr.End.(fdb.Key))

				Expect(bytes.HasPrefix(beginKey, ssPrefix)).To(BeTrue(),
					"begin key should be within subspace")
				Expect(bytes.HasPrefix(endKey, ssPrefix)).To(BeTrue(),
					"end key should be within subspace")
			}
		})
	})

	Describe("indexCursor ScannedRecordsLimit (regression: was missing)", func() {
		It("stops after scanning the configured number of entries", func() {
			priceIdx := NewIndex("Order$price_scanl", Field("price"))
			metaData := buildMetaWithIndex(priceIdx)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, saveErr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(saveErr).NotTo(HaveOccurred())
				}

				// Scan index with ScannedRecordsLimit=3
				scan := ForwardScan()
				scan.ExecuteProperties.ScannedRecordsLimit = 3
				cursor := store.ScanIndex(priceIdx, TupleRangeAll, nil, scan)

				var entries []*IndexEntry
				var lastResult RecordCursorResult[*IndexEntry]
				for {
					result, nextErr := cursor.OnNext(ctx)
					Expect(nextErr).NotTo(HaveOccurred())
					lastResult = result
					if !result.HasNext() {
						break
					}
					entries = append(entries, result.GetValue())
				}
				Expect(entries).To(HaveLen(3))
				Expect(lastResult.GetNoNextReason()).To(Equal(ScanLimitReached))
				Expect(lastResult.HasStoppedBeforeEnd()).To(BeTrue())

				// Resume from continuation — should get next 3.
				cont, contErr := lastResult.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				scan2 := ForwardScan()
				scan2.ExecuteProperties.ScannedRecordsLimit = 3
				cursor2 := store.ScanIndex(priceIdx, TupleRangeAll, cont, scan2)

				var entries2 []*IndexEntry
				for {
					result, nextErr := cursor2.OnNext(ctx)
					Expect(nextErr).NotTo(HaveOccurred())
					if !result.HasNext() {
						break
					}
					entries2 = append(entries2, result.GetValue())
				}
				Expect(entries2).To(HaveLen(3))

				// Verify no overlap between first and second batch.
				firstPKs := make(map[string]bool)
				for _, e := range entries {
					firstPKs[string(e.PrimaryKey().Pack())] = true
				}
				for _, e := range entries2 {
					Expect(firstPKs).NotTo(HaveKey(string(e.PrimaryKey().Pack())),
						"second batch should not contain entries from first batch")
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
