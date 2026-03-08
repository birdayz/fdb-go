package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SecondaryIndexes", func() {
	ctx := context.Background()

	buildMetaWithIndex := func(indexes ...*Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		for _, idx := range indexes {
			builder.AddIndex("Order", idx)
		}
		return builder.Build()
	}

	// scanIndexEntries reads all raw KV pairs in the index subspace for verification.
	scanIndexEntries := func(store *FDBRecordStore, index *Index) []fdb.KeyValue {
		idxSubspace := store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
		begin, end := idxSubspace.FDBRangeKeys()
		kvs, err := store.context.Transaction().GetRange(
			fdb.KeyRange{Begin: begin, End: end},
			fdb.RangeOptions{},
		).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		return kvs
	}

	It("InsertCreatesIndexEntry", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Verify index entry exists
			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(1))

			// Unpack and verify the entry key: (price, order_id)
			idxSubspace := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
			entryTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(entryTuple).To(HaveLen(2))
			Expect(entryTuple[0]).To(Equal(int64(999)))  // price (int32 → int64 for FDB tuple compat)
			Expect(entryTuple[1]).To(Equal(int64(42)))    // order_id (primary key)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteRemovesIndexEntry", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Verify entry exists
			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(1))

			// Delete the record
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Index entry should be gone
			kvs = scanIndexEntries(store, priceIndex)
			Expect(kvs).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateChangesIndexEntry", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save with price=100
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Update to price=200
			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			// Should have exactly 1 entry (old removed, new added)
			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(1))

			// Verify it's the new price
			idxSubspace := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
			entryTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(entryTuple[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateUnchangedValueSkipsIndexWrite", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save with price=100
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Update with same price — index entry should be unchanged
			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MultipleRecordsSameIndex", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 records with different prices
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(3))

			// Delete middle record
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			kvs = scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UniqueIndexBlocksDuplicate", func() {
		priceIndex := NewIndex("Order$price", Field("price")).SetUnique()
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// First record with price=100
			order1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			// Second record with same price=100 — should fail
			order2 := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order2)
			Expect(err).To(HaveOccurred())

			var violation *RecordIndexUniquenessViolationError
			Expect(err).To(BeAssignableToTypeOf(violation))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UniqueIndexAllowsDifferentValues", func() {
		priceIndex := NewIndex("Order$price", Field("price")).SetUnique()
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UniqueIndexAllowsUpdateSameRecord", func() {
		priceIndex := NewIndex("Order$price", Field("price")).SetUnique()
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save with price=100
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Update same record to price=200 — should succeed
			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CompositeIndex", func() {
		// Composite index on (price, order_id) — tests multi-field key expressions
		compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))
		metaData := buildMetaWithIndex(compositeIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, compositeIndex)
			Expect(kvs).To(HaveLen(1))

			// Entry key: (price, order_id, primary_key=order_id)
			idxSubspace := store.subspace.Sub(IndexKey, compositeIndex.SubspaceTupleKey())
			entryTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			// Composite: (999, 42) + PK (42) = (999, 42, 42)
			Expect(entryTuple).To(HaveLen(3))
			Expect(entryTuple[0]).To(Equal(int64(999)))
			Expect(entryTuple[1]).To(Equal(int64(42)))
			Expect(entryTuple[2]).To(Equal(int64(42)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteAllRecordsClearsIndexes", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(5))

			Expect(store.DeleteAllRecords()).To(Succeed())

			kvs = scanIndexEntries(store, priceIndex)
			Expect(kvs).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MultipleIndexesOnSameType", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		// order_id is both PK and indexed — tests that PK is appended to index entry
		idIndex := NewIndex("Order$order_id", Field("order_id"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.AddIndex("Order", priceIndex)
		builder.AddIndex("Order", idIndex)
		metaData := builder.Build()

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(500)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Both indexes should have entries
			Expect(scanIndexEntries(store, priceIndex)).To(HaveLen(1))
			Expect(scanIndexEntries(store, idIndex)).To(HaveLen(1))

			// Delete should remove from both
			_, err = store.DeleteRecord(tuple.Tuple{int64(7)})
			Expect(err).NotTo(HaveOccurred())

			Expect(scanIndexEntries(store, priceIndex)).To(BeEmpty())
			Expect(scanIndexEntries(store, idIndex)).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IndexOnStringField", func() {
		nameIndex := NewIndex("Customer$name", Field("name"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.AddIndex("Customer", nameIndex)
		metaData := builder.Build()

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			customer := &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")}
			_, err = store.SaveRecord(customer)
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, nameIndex)
			Expect(kvs).To(HaveLen(1))

			idxSubspace := store.subspace.Sub(IndexKey, nameIndex.SubspaceTupleKey())
			entryTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(entryTuple[0]).To(Equal("Alice"))
			Expect(entryTuple[1]).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UniqueIndexAfterDeleteAllowsReinsert", func() {
		priceIndex := NewIndex("Order$price", Field("price")).SetUnique()
		metaData := buildMetaWithIndex(priceIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert with price=100
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Delete it
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			// Insert different record with same price — should succeed
			order2 := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IndexWithRecordCounting", func() {
		priceIndex := NewIndex("Order$price", Field("price"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.SetRecordCountKey(EmptyKey())
		builder.AddIndex("Order", priceIndex)
		metaData := builder.Build()

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 records
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify both count and index work together
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))

			kvs := scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(3))

			// Delete one — both count and index should update
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			count, err = store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(2)))

			kvs = scanIndexEntries(store, priceIndex)
			Expect(kvs).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
