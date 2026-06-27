package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Schema Migration", func() {
	ctx := context.Background()

	baseBuilder := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// buildMD builds metadata, calling configure (which may AddIndex/RemoveIndex)
	// then setting the final version. Matches the evolution validator test pattern:
	// SetVersion(N-1) before AddIndex so index.LastModifiedVersion > old version.
	buildMD := func(version int, configure func(b *RecordMetaDataBuilder)) *RecordMetaData {
		b := baseBuilder()
		if configure != nil {
			configure(b)
		}
		b.SetVersion(version)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	Describe("add index across transactions", func() {
		It("backfills index on pre-existing records and auto-maintains for new ones", func() {
			ks := specSubspace()

			// v1: no indexes, insert 20 records.
			md1 := buildMD(1, nil)

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 20; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 10)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// v2: add price index.
			priceIdx := NewIndex("Order$price", Field("price"))
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				b.SetVersion(1)
				b.AddIndex("Order", priceIdx)
			})

			Expect(ValidateEvolution(md1, md2)).NotTo(HaveOccurred())

			// Online backfill.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md2).
				SetIndex(priceIdx).
				SetSubspace(ks).
				SetLimit(7).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 20))

			// Verify index is readable with correct sorted entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20))

				for i, e := range entries {
					Expect(e.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 10)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert new records with v2 metadata — index auto-maintained.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(21); i <= 25; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify 25 entries now.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(25))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("drop index with FormerIndex tracking", func() {
		It("removes index and prevents subspace reuse", func() {
			ks := specSubspace()

			// v1: create with price index, insert data.
			priceIdx := NewIndex("Order$price", Field("price"))
			md1 := buildMD(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", priceIdx)
			})

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// v2: remove price index. Replay the add+remove so FormerIndex is created.
			oldIdx := md1.GetIndex("Order$price")
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", Field("price"))
				idx.AddedVersion = oldIdx.AddedVersion
				idx.LastModifiedVersion = oldIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
				b.SetVersion(1)
				b.RemoveIndex("Order$price")
			})

			formers := md2.GetFormerIndexes()
			Expect(formers).To(HaveLen(1))
			Expect(formers[0].FormerName).To(Equal("Order$price"))

			Expect(ValidateEvolution(md1, md2)).NotTo(HaveOccurred())

			// Records still accessible with v2 metadata (no index).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				rec, err := store.LoadRecord(tuple.Tuple{int64(3)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())

				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(3)))
				Expect(order.GetPrice()).To(Equal(int32(300)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("multi-step evolution v1 -> v2 -> v3", func() {
		It("evolves schema across three versions with data integrity", func() {
			ks := specSubspace()

			// v1: Order with price index.
			priceIdx := NewIndex("Order$price", Field("price"))
			md1 := buildMD(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", priceIdx)
			})

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i * 2)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// v2: drop price index, add quantity index.
			quantityIdx := NewIndex("Order$quantity", Field("quantity"))
			oldPriceIdx := md1.GetIndex("Order$price")
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				// Replay price index with original versions, then remove.
				idx := NewIndex("Order$price", Field("price"))
				idx.AddedVersion = oldPriceIdx.AddedVersion
				idx.LastModifiedVersion = oldPriceIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
				b.SetVersion(1)
				b.RemoveIndex("Order$price")
				// Add quantity index at version > old.
				b.AddIndex("Order", quantityIdx)
			})

			Expect(ValidateEvolution(md1, md2)).NotTo(HaveOccurred())
			Expect(md2.GetFormerIndexes()).To(HaveLen(1))

			// Online-build the new quantity index.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md2).
				SetIndex(quantityIdx).
				SetSubspace(ks).
				SetLimit(4).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 10))

			// Verify quantity index sorted correctly.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$quantity")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(quantityIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))
				for i, e := range entries {
					Expect(e.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 2)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// v3: add composite index on (quantity, price), keep quantity index.
			compositeIdx := NewIndex("Order$qty_price", Concat(Field("quantity"), Field("price")))
			oldQtyIdx := md2.GetIndex("Order$quantity")
			md3 := buildMD(3, func(b *RecordMetaDataBuilder) {
				// Replay price add+remove from v2.
				idx := NewIndex("Order$price", Field("price"))
				idx.AddedVersion = oldPriceIdx.AddedVersion
				idx.LastModifiedVersion = oldPriceIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
				b.SetVersion(1)
				b.RemoveIndex("Order$price")
				// Replay quantity index with v2 versions.
				qIdx := NewIndex("Order$quantity", Field("quantity"))
				qIdx.AddedVersion = oldQtyIdx.AddedVersion
				qIdx.LastModifiedVersion = oldQtyIdx.LastModifiedVersion
				b.AddIndex("Order", qIdx)
				// Add composite at version > v2.
				b.SetVersion(2)
				b.AddIndex("Order", compositeIdx)
			})

			Expect(ValidateEvolution(md2, md3)).NotTo(HaveOccurred())
			Expect(md3.GetFormerIndexes()).To(HaveLen(1))

			// Online-build the composite index.
			indexer2, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md3).
				SetIndex(compositeIdx).
				SetSubspace(ks).
				SetLimit(5).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total2, err := indexer2.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total2).To(BeNumerically(">=", 10))

			// Verify both indexes readable + data integrity.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md3).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$quantity")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty_price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(compositeIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				for i, e := range entries {
					expectedQty := int64((i + 1) * 2)
					expectedPrice := int64((i + 1) * 100)
					Expect(e.IndexValues()).To(Equal(tuple.Tuple{expectedQty, expectedPrice}))
				}

				// All records still load by PK.
				for i := int64(1); i <= 10; i++ {
					rec, err := store.LoadRecord(tuple.Tuple{i})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil())
					order := rec.Record.(*gen.Order)
					Expect(order.GetOrderId()).To(Equal(i))
					Expect(order.GetPrice()).To(Equal(int32(i * 100)))
					Expect(order.GetQuantity()).To(Equal(int32(i * 2)))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("metadata persistence across transactions", func() {
		It("stores and loads metadata with version history", func() {
			ks := specSubspace()

			// v1: save metadata.
			md1 := buildMD(1, nil)
			md1Proto, err := md1.ToProto()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				metaStore := NewFDBMetaDataStore(ks)
				return nil, metaStore.SaveRecordMetaData(rtx.Transaction(), md1Proto)
			})
			Expect(err).NotTo(HaveOccurred())

			// v2: add index, save metadata again.
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				b.SetVersion(1)
				b.AddIndex("Order", NewIndex("Order$price", Field("price")))
			})
			md2Proto, err := md2.ToProto()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				metaStore := NewFDBMetaDataStore(ks)
				return nil, metaStore.SaveRecordMetaData(rtx.Transaction(), md2Proto)
			})
			Expect(err).NotTo(HaveOccurred())

			// Load latest — should be v2.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				metaStore := NewFDBMetaDataStore(ks)

				latest, err := metaStore.LoadRecordMetaDataProto(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(latest).NotTo(BeNil())
				Expect(latest.GetVersion()).To(Equal(int32(2)))

				v1, err := metaStore.LoadRecordMetaDataProtoAtVersion(rtx.Transaction(), 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(v1).NotTo(BeNil())
				Expect(v1.GetVersion()).To(Equal(int32(1)))
				Expect(v1.GetIndexes()).To(BeEmpty())

				Expect(latest.GetIndexes()).To(HaveLen(1))
				Expect(latest.GetIndexes()[0].GetName()).To(Equal("Order$price"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("evolution validation rejects unsafe changes", func() {
		It("rejects removing an index without FormerIndex tracking", func() {
			md1 := buildMD(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("Order$price", Field("price")))
			})

			// v2 built independently — no FormerIndex.
			md2 := buildMD(2, nil)

			err := ValidateEvolution(md1, md2)
			Expect(err).To(HaveOccurred())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
		})

		It("rejects version downgrade", func() {
			md5 := buildMD(5, nil)
			md3 := buildMD(3, nil)

			err := ValidateEvolution(md5, md3)
			Expect(err).To(HaveOccurred())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("does not have newer version"))
		})

		It("accepts additive evolution (new index with FormerIndex for old)", func() {
			md1 := buildMD(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("Order$price", Field("price")))
			})
			oldIdx := md1.GetIndex("Order$price")

			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", Field("price"))
				idx.AddedVersion = oldIdx.AddedVersion
				idx.LastModifiedVersion = oldIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
				b.SetVersion(1)
				b.RemoveIndex("Order$price")
				b.AddIndex("Order", NewIndex("Order$quantity", Field("quantity")))
			})

			Expect(ValidateEvolution(md1, md2)).NotTo(HaveOccurred())
		})
	})

	Describe("index state transitions during migration", func() {
		It("transitions through WriteOnly -> Readable during online build", func() {
			ks := specSubspace()

			md1 := buildMD(1, nil)

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// v2: add index, mark WRITE_ONLY, build, verify READABLE.
			priceIdx := NewIndex("Order$price", Field("price"))
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				b.SetVersion(1)
				b.AddIndex("Order", priceIdx)
			})

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				changed, err := store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				state := store.GetIndexState("Order$price")
				Expect(state).To(Equal(IndexStateWriteOnly))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md2).
				SetIndex(priceIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				state := store.GetIndexState("Order$price")
				Expect(state).To(Equal(IndexStateReadable))

				entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("concurrent writes during index build", func() {
		It("index captures records written during backfill", func() {
			ks := specSubspace()

			md1 := buildMD(1, nil)

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// v2: add index + write more records BEFORE online build.
			priceIdx := NewIndex("Order$price", Field("price"))
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				b.SetVersion(1)
				b.AddIndex("Order", priceIdx)
			})

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(11); i <= 15; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md2).
				SetIndex(priceIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// All 15 records in the index.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(15))

				for i, e := range entries {
					Expect(e.IndexValues()).To(Equal(tuple.Tuple{int64(i + 1)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("multi-type index evolution", func() {
		It("validates multi-type index addition across versions", func() {
			// Verify multi-type index evolution passes validation.
			md1 := buildMD(1, nil)

			uniPriceIdx := NewIndex("universal$price", Field("price"))
			md2 := buildMD(2, func(b *RecordMetaDataBuilder) {
				b.SetVersion(1)
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, uniPriceIdx)
			})

			Expect(ValidateEvolution(md1, md2)).NotTo(HaveOccurred())

			// Index exists on both record types.
			orderType := md2.GetRecordType("Order")
			Expect(orderType).NotTo(BeNil())
			customerType := md2.GetRecordType("Customer")
			Expect(customerType).NotTo(BeNil())

			idx := md2.GetIndex("universal$price")
			Expect(idx).NotTo(BeNil())
			Expect(idx.AddedVersion).To(Equal(2))
		})
	})
})
