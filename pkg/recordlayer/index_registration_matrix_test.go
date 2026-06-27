package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("IndexRegistrationMatrix", func() {
	ctx := context.Background()

	type registrationCase struct {
		name        string
		register    func(builder *RecordMetaDataBuilder, idx *Index)
		isUniversal bool
	}

	cases := []registrationCase{
		{"SingleType", func(b *RecordMetaDataBuilder, idx *Index) { b.AddIndex("Order", idx) }, false},
		{"MultiType", func(b *RecordMetaDataBuilder, idx *Index) { b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx) }, false},
		{"Universal", func(b *RecordMetaDataBuilder, idx *Index) { b.AddUniversalIndex(idx) }, true},
	}

	for _, rc := range cases {
		rc := rc
		Describe(rc.name, func() {
			// ----------------------------------------------------------
			// 1. PK dedup
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: PK dedup", rc.name), func() {
				if rc.isUniversal {
					Skip("Universal indexes cannot use type-specific PK fields (order_id not on Customer)")
				}
				ks := specSubspace()

				// Index on (order_id, price). order_id overlaps with Order PK.
				pkDedupIdx := NewIndex("pkdedup_idx", Concat(Field("order_id"), Field("price")))

				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				rc.register(builder, pkDedupIdx)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				// Verify PK component positions were computed.
				idx := md.GetIndex("pkdedup_idx")
				Expect(idx).NotTo(BeNil())
				Expect(idx.HasPrimaryKeyComponentPositions()).To(BeTrue(),
					"index registered via %s should have primaryKeyComponentPositions computed", rc.name)

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())

					entries, err := AsList(ctx, store.ScanIndex(pkDedupIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(1))
					// With dedup: key = [order_id, price] (2 elements, PK not appended).
					Expect(entries[0].Key).To(HaveLen(2),
						"entry key should be [order_id, price] with PK deduped")
					Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			// ----------------------------------------------------------
			// 2. Scan
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: Scan", rc.name), func() {
				ks := specSubspace()

				priceIdx := NewIndex("price_idx", Field("price"))

				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				rc.register(builder, priceIdx)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					for i := int64(1); i <= 3; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					if !rc.isUniversal {
						// Multi-type: also save a Customer (has price field).
						if rc.name == "MultiType" {
							_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(100), Name: proto.String("Alice"), Price: proto.Int32(999)})
							Expect(err).NotTo(HaveOccurred())
						}
					} else {
						// Universal: save a Customer too.
						_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(100), Name: proto.String("Alice"), Price: proto.Int32(999)})
						Expect(err).NotTo(HaveOccurred())
					}

					entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())

					if rc.name == "SingleType" {
						Expect(entries).To(HaveLen(3), "single-type index should only have Order entries")
					} else {
						Expect(entries).To(HaveLen(4), "%s index should have Order+Customer entries", rc.name)
					}

					// Verify sorted order (forward scan).
					for i := 1; i < len(entries); i++ {
						prev := entries[i-1].IndexValues()[0].(int64)
						curr := entries[i].IndexValues()[0].(int64)
						Expect(curr).To(BeNumerically(">=", prev), "entries should be in ascending order")
					}

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			// ----------------------------------------------------------
			// 3. Save/Delete
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: Save and Delete", rc.name), func() {
				ks := specSubspace()

				priceIdx := NewIndex("price_idx", Field("price"))

				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				rc.register(builder, priceIdx)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Save two orders.
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
					Expect(err).NotTo(HaveOccurred())

					entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(2))

					// Delete one.
					deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
					Expect(err).NotTo(HaveOccurred())
					Expect(deleted).To(BeTrue())

					entries, err = AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(1))
					Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))

					// Update remaining record's price.
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
					Expect(err).NotTo(HaveOccurred())

					entries, err = AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(1))
					Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(300)}))

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			// ----------------------------------------------------------
			// 4. RebuildIndex
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: RebuildIndex", rc.name), func() {
				ks := specSubspace()

				priceIdx := NewIndex("price_idx", Field("price"))

				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				rc.register(builder, priceIdx)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					for i := int64(1); i <= 5; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					if rc.name != "SingleType" {
						_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(100), Name: proto.String("Bob"), Price: proto.Int32(555)})
						Expect(err).NotTo(HaveOccurred())
					}

					// Rebuild the index.
					Expect(store.RebuildIndex(priceIdx)).To(Succeed())

					entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())

					if rc.name == "SingleType" {
						Expect(entries).To(HaveLen(5))
					} else {
						Expect(entries).To(HaveLen(6))
					}

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			// ----------------------------------------------------------
			// 5. OnlineIndexer
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: OnlineIndexer", rc.name), func() {
				ks := specSubspace()

				// Phase 1: save records WITHOUT index.
				builderNoIdx := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builderNoIdx.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builderNoIdx.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builderNoIdx.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				mdNoIdx, err := builderNoIdx.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					for i := int64(1); i <= 5; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					if rc.name != "SingleType" {
						_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(100), Name: proto.String("Carol"), Price: proto.Int32(777)})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Phase 2: build index with OnlineIndexer.
				priceIdx := NewIndex("price_idx", Field("price"))

				builderWithIdx := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builderWithIdx.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builderWithIdx.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builderWithIdx.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				rc.register(builderWithIdx, priceIdx)
				mdWithIdx, err := builderWithIdx.Build()
				Expect(err).NotTo(HaveOccurred())

				indexer, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIdx).
					SetIndex(priceIdx).
					SetSubspace(ks).
					SetLimit(3).
					Build()
				Expect(err).NotTo(HaveOccurred())

				total, err := indexer.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(BeNumerically(">=", 5), "should have indexed at least all 5 orders")

				// Phase 3: verify.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())

					Expect(store.IsIndexReadable("price_idx")).To(BeTrue())

					entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())

					if rc.name == "SingleType" {
						Expect(entries).To(HaveLen(5))
					} else {
						Expect(entries).To(HaveLen(6))
					}

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			// ----------------------------------------------------------
			// 6. DeleteAllRecords
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: DeleteAllRecords", rc.name), func() {
				ks := specSubspace()

				priceIdx := NewIndex("price_idx", Field("price"))

				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				rc.register(builder, priceIdx)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				// Save records.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					for i := int64(1); i <= 3; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					if rc.name != "SingleType" {
						_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(100), Name: proto.String("Dave"), Price: proto.Int32(888)})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// DeleteAllRecords.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())

					return nil, store.DeleteAllRecords()
				})
				Expect(err).NotTo(HaveOccurred())

				// Verify everything is gone.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())

					entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(BeEmpty(), "index should be empty after DeleteAllRecords")

					records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(records).To(BeEmpty(), "records should be empty after DeleteAllRecords")

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			// ----------------------------------------------------------
			// 7. DeleteRecordsWhere
			// ----------------------------------------------------------
			It(fmt.Sprintf("%s: DeleteRecordsWhere", rc.name), func() {
				ks := specSubspace()

				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				// PKs must start with RecordTypeKey for DeleteRecordsWhere.
				builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
				builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))

				var priceIdx *Index
				if rc.isUniversal || rc.name == "MultiType" {
					// Universal/multi-type index: leading expression must match PK prefix
					// so that DeleteRecordsWhere can scope the clear by type.
					priceIdx = NewIndex("price_idx", Concat(RecordTypeKey(), Field("price")))
				} else {
					priceIdx = NewIndex("price_idx", Field("price"))
				}
				rc.register(builder, priceIdx)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()
				customerTypeKey := md.GetRecordType("Customer").GetRecordTypeKey()

				// Save records of both types.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					for i := int64(1); i <= 3; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					for i := int64(1); i <= 2; i++ {
						_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Cust"), Price: proto.Int32(int32(i * 100))})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Delete Order records only.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())

					return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
				})
				Expect(err).NotTo(HaveOccurred())

				// Verify: Orders gone, Customers remain.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())

					// Orders should be gone.
					for i := int64(1); i <= 3; i++ {
						rec, err := store.LoadRecord(tuple.Tuple{orderTypeKey, i})
						Expect(err).NotTo(HaveOccurred())
						Expect(rec).To(BeNil(), "Order %d should be deleted", i)
					}

					// Customers should remain.
					for i := int64(1); i <= 2; i++ {
						rec, err := store.LoadRecord(tuple.Tuple{customerTypeKey, i})
						Expect(err).NotTo(HaveOccurred())
						Expect(rec).NotTo(BeNil(), "Customer %d should remain", i)
					}

					// Check index entries.
					entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())

					if rc.name == "SingleType" {
						// Single-type Order index: fully cleared.
						Expect(entries).To(BeEmpty(), "single-type Order index should be fully cleared")
					} else {
						// Multi-type and universal indexes with RecordTypeKey prefix:
						// Only Order entries cleared, Customer entries remain.
						Expect(entries).To(HaveLen(2), "%s index should retain Customer entries", rc.name)
					}

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})
	}
})
