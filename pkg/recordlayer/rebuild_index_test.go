package recordlayer

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("RebuildIndex", func() {
	ctx := context.Background()

	// Helper: create metadata with an Order type (PK on order_id).
	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		return builder
	}

	It("rebuilds a VALUE index within a single transaction", func() {
		ks := specSubspace()

		// Phase 1: Insert records with an index already defined.
		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 10; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Now rebuild the index inline (simulating re-indexing).
			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())

			// Verify index is READABLE.
			Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

			// Verify index entries are correct.
			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(10))

			for i, entry := range entries {
				expectedPrice := int64((i + 1) * 100)
				Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuilds an index with no records (empty store)", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears stale index entries before rebuilding", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Phase 1: Insert records with index.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Delete some records and rebuild.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			// Delete records 4 and 5.
			_, err = store.DeleteRecord(tuple.Tuple{int64(4)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteRecord(tuple.Tuple{int64(5)})
			Expect(err).NotTo(HaveOccurred())

			// Rebuild index — should only have entries for records 1-3.
			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("only indexes records of matching type", func() {
		ks := specSubspace()

		// Index only on Order, not Customer.
		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert Orders and Customers.
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(101); i <= 103; i++ {
				cust := &gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("cust")}
				_, err = store.SaveRecord(cust)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild should only index the 5 Orders.
			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets range set to complete after rebuild", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())

			// Verify the range set is complete.
			rangeSet := NewIndexingRangeSet(store.subspace, priceIndex)
			complete, err := rangeSet.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuilds a unique index", func() {
		ks := specSubspace()

		emailIndex := NewIndex("Customer$name", Field("name"))
		emailIndex.SetUnique()
		builder := baseMetaData()
		builder.AddIndex("Customer", emailIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				cust := &gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("cust" + string(rune('A'+i-1)))}
				_, err = store.SaveRecord(cust)
				Expect(err).NotTo(HaveOccurred())
			}

			err = store.RebuildIndex(emailIndex)
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable("Customer$name")).To(BeTrue())

			entries, err := AsList(ctx, store.ScanIndex(emailIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("tracks violations for unique index rebuild with duplicate values", func() {
		ks := specSubspace()

		// Phase 1: Insert records WITHOUT the unique index.
		builder1 := baseMetaData()
		mdNoIndex, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert two customers with same name — no index so no uniqueness check.
			cust1 := &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")}
			_, err = store.SaveRecord(cust1)
			Expect(err).NotTo(HaveOccurred())
			cust2 := &gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Alice")}
			_, err = store.SaveRecord(cust2)
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Add the unique index and rebuild — Java behavior: writes violation
		// entries to subspace 7 instead of throwing, transitions to READABLE_UNIQUE_PENDING.
		nameIndex := NewIndex("Customer$name", Field("name"))
		nameIndex.SetUnique()
		builder2 := baseMetaData()
		builder2.AddIndex("Customer", nameIndex)
		mdWithIndex, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			err = store.RebuildIndex(nameIndex)
			Expect(err).NotTo(HaveOccurred())

			// Index should be READABLE_UNIQUE_PENDING, not READABLE
			Expect(store.GetIndexState(nameIndex.Name)).To(Equal(IndexStateReadableUniquePending))

			// Should have violation entries
			violations, err := store.ScanUniquenessViolations(nameIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(violations)).To(BeNumerically(">=", 2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("index is maintained after rebuild when new records are added", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Phase 1: Insert records and rebuild index.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Add more records — index should be maintained since it's READABLE.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(4); i <= 6; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(6))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("addIndexCommon version tracking", func() {
		It("bumps version and sets LastModifiedVersion on AddIndex", func() {
			builder := baseMetaData()
			Expect(builder.version).To(Equal(0))

			idx1 := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", idx1)
			Expect(idx1.LastModifiedVersion).To(Equal(1))
			Expect(idx1.AddedVersion).To(Equal(1))
			Expect(builder.version).To(Equal(1))

			idx2 := NewIndex("Customer$name", Field("name"))
			builder.AddIndex("Customer", idx2)
			Expect(idx2.LastModifiedVersion).To(Equal(2))
			Expect(idx2.AddedVersion).To(Equal(2))
			Expect(builder.version).To(Equal(2))
		})

		It("preserves pre-set LastModifiedVersion", func() {
			builder := baseMetaData()
			idx := NewIndex("Order$price", Field("price"))
			idx.LastModifiedVersion = 5
			builder.AddIndex("Order", idx)

			// Should NOT bump version, should use existing value
			Expect(idx.LastModifiedVersion).To(Equal(5))
			Expect(idx.AddedVersion).To(Equal(5))
			Expect(builder.version).To(Equal(5))
		})
	})

	Describe("GetIndexesToBuildSince", func() {
		It("returns indexes added after the given version", func() {
			builder := baseMetaData()
			idx1 := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", idx1)
			// idx1 gets version 1

			idx2 := NewIndex("Customer$name", Field("name"))
			builder.AddIndex("Customer", idx2)
			// idx2 gets version 2

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Since version 0: both indexes
			since0 := md.GetIndexesToBuildSince(0)
			Expect(since0).To(HaveLen(2))

			// Since version 1: only idx2
			since1 := md.GetIndexesToBuildSince(1)
			Expect(since1).To(HaveLen(1))
			Expect(since1[0].Name).To(Equal("Customer$name"))

			// Since version 2: none
			since2 := md.GetIndexesToBuildSince(2)
			Expect(since2).To(BeEmpty())
		})
	})

	Describe("CreateOrOpen auto-rebuild on metadata version change", func() {
		It("rebuilds new indexes when metadata version increases", func() {
			ks := specSubspace()

			// Phase 1: Create store with no indexes, save some orders.
			builder1 := baseMetaData()
			md1, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 5; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Open with metadata that has a new price index.
			// This should auto-rebuild the index.
			priceIndex := NewIndex("Order$price", Field("price"))
			builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Index should be READABLE after auto-rebuild.
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				// Scan the index — should have all 5 entries.
				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(5))

				// Verify ordering: 100, 200, 300, 400, 500
				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 100)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not rebuild when metadata version is unchanged", func() {
			ks := specSubspace()

			// Create store with an index and save data.
			priceIndex := NewIndex("Order$price", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Re-open with same metadata — no rebuild should happen, index has 3 entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(3))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("updates store header metadata version after rebuild", func() {
			ks := specSubspace()

			// Phase 1: Create with version 0.
			builder1 := baseMetaData()
			md1, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Open with version 1 (new index).
			priceIndex := NewIndex("Order$price", Field("price"))
			builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Re-open with same md2 — should NOT rebuild again
			// (stored version now matches md2 version).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Verify the stored version was updated.
				Expect(store.storeHeader.GetMetaDataversion()).To(Equal(int32(md2.Version())))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rebuilds multiple new indexes at once", func() {
			ks := specSubspace()

			// Phase 1: Create store with no indexes and save data.
			builder1 := baseMetaData()
			md1, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					if err != nil {
						return nil, err
					}
				}
				for i := int64(101); i <= 102; i++ {
					cust := &gen.Customer{CustomerId: proto.Int64(i), Name: proto.String(fmt.Sprintf("cust%d", i))}
					_, err = store.SaveRecord(cust)
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Open with two new indexes.
			priceIdx := NewIndex("Order$price", Field("price"))
			nameIdx := NewIndex("Customer$name", Field("name"))
			builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Customer", nameIdx)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Both indexes should be READABLE.
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Customer$name")).To(BeTrue())

				// Price index should have 3 entries.
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(priceEntries).To(HaveLen(3))

				// Name index should have 2 entries.
				nameEntries, err := AsList(ctx, store.ScanIndex(nameIdx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(nameEntries).To(HaveLen(2))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("marks index DISABLED when DefaultIndexRebuildPolicy and too many records", func() {
			ks := specSubspace()

			// Phase 1: Create store with record counting, save >200 records.
			builder1 := baseMetaData()
			builder1.SetRecordCountKey(&EmptyKeyExpression{})
			md1, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 201; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Open with a new index. Default policy sees >200 records → DISABLED.
			priceIndex := NewIndex("Order$price", Field("price"))
			builder2 := baseMetaData()
			builder2.SetRecordCountKey(&EmptyKeyExpression{})
			builder2.AddIndex("Order", priceIndex)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Index should be DISABLED, not READABLE.
				Expect(store.IsIndexDisabled("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("AlwaysRebuildPolicy forces inline rebuild regardless of record count", func() {
			ks := specSubspace()

			// Phase 1: Create store with counting, save >200 records.
			builder1 := baseMetaData()
			builder1.SetRecordCountKey(&EmptyKeyExpression{})
			md1, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 201; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Open with AlwaysRebuildPolicy → forces READABLE.
			priceIndex := NewIndex("Order$price", Field("price"))
			builder2 := baseMetaData()
			builder2.SetRecordCountKey(&EmptyKeyExpression{})
			builder2.AddIndex("Order", priceIndex)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).
					SetIndexRebuildPolicy(AlwaysRebuildPolicy).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(201))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
