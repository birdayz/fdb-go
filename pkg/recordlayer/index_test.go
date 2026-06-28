package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		for _, idx := range indexes {
			builder.AddIndex("Order", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
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
			Expect(entryTuple[0]).To(Equal(int64(999))) // price (int32 → int64 for FDB tuple compat)
			Expect(entryTuple[1]).To(Equal(int64(42)))  // order_id (primary key)

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
			Expect(errors.As(err, &violation)).To(BeTrue())

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

			// Entry key: (price, order_id) — PK (order_id) is deduplicated since it
			// already appears in the index key. Matches Java's primaryKeyComponentPositions.
			idxSubspace := store.subspace.Sub(IndexKey, compositeIndex.SubspaceTupleKey())
			entryTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(entryTuple).To(HaveLen(2))
			Expect(entryTuple[0]).To(Equal(int64(999)))
			Expect(entryTuple[1]).To(Equal(int64(42)))

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
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		builder.AddIndex("Order", idIndex)
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

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

	It("UniqueIndexNullKeySkipsUniquenessCheck", func() {
		// Java's StandardIndexMaintainer skips uniqueness checks when the index key
		// contains a null component (NullStandin.NULL). Multiple records with null
		// index values should coexist without uniqueness violations.
		flowerTypeIndex := NewIndex("Order$flowerType", Nest("flower", Field("type"))).SetUnique()
		metaData := buildMetaWithIndex(flowerTypeIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Two orders WITHOUT flower set → index key is (nil, pk).
			// Both should succeed because null keys skip uniqueness checks.
			order1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)}
			_, err = store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			// Both entries should exist in the index
			kvs := scanIndexEntries(store, flowerTypeIndex)
			Expect(kvs).To(HaveLen(2))

			// But a UNIQUE non-null value should still be enforced
			order3 := &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(300),
				Flower:  &gen.Flower{Type: proto.String("Rose")},
			}
			_, err = store.SaveRecord(order3)
			Expect(err).NotTo(HaveOccurred())

			// Another record with the same non-null flower type → violation
			order4 := &gen.Order{
				OrderId: proto.Int64(4),
				Price:   proto.Int32(400),
				Flower:  &gen.Flower{Type: proto.String("Rose")},
			}
			_, err = store.SaveRecord(order4)
			Expect(err).To(HaveOccurred())
			var violation *RecordIndexUniquenessViolationError
			Expect(errors.As(err, &violation)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IndexWithRecordCounting", func() {
		priceIndex := NewIndex("Order$price", Field("price"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetRecordCountKey(EmptyKey())
		builder.AddIndex("Order", priceIndex)
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

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

	It("rejects index entry with key exceeding size limit", func() {
		ks := specSubspace()

		// Index on tags with fan-out — a single very long tag will produce a large key.
		tagIndex := NewIndex("Order$tags", FanOut("tags"))
		md := buildMetaWithIndex(tagIndex)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Create a tag string that will exceed keySizeLimit (10KB) once packed.
			longTag := make([]byte, keySizeLimit+1)
			for i := range longTag {
				longTag[i] = 'x'
			}
			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
				Tags:    []string{string(longTag)},
			}
			_, err = store.SaveRecord(order)
			Expect(err).To(HaveOccurred())
			var keySizeErr *IndexKeySizeError
			Expect(errors.As(err, &keySizeErr)).To(BeTrue(),
				"expected IndexKeySizeError, got: %v", err)
			Expect(keySizeErr.IndexName).To(Equal("Order$tags"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("KeyWithValueExpression covering indexes", func() {
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

	It("stores value columns in FDB value, key columns in FDB key", func() {
		// Inner: Concat(price, flower.type) = 2 columns. splitPoint=1: price in key, flower.type in value.
		coveringIndex := NewIndex("covering_price", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(42), Price: proto.Int32(999),
				Flower: &gen.Flower{Type: proto.String("Rose")},
			}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Verify index entry: key should have [price=999, pk=42], value should have [flower.type="Rose"]
			idxSubspace := store.subspace.Sub(IndexKey, coveringIndex.SubspaceTupleKey())
			begin, end := idxSubspace.FDBRangeKeys()
			kvs, err := rtx.Transaction().GetRange(
				fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{},
			).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(kvs).To(HaveLen(1))

			// Key: [price, order_id (pk)]
			keyTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(keyTuple).To(HaveLen(2)) // 1 key column + 1 PK column
			Expect(keyTuple[0]).To(Equal(int64(999)))
			Expect(keyTuple[1]).To(Equal(int64(42)))

			// Value: [flower.type="Rose"] (the value portion)
			valueTuple, err := tuple.Unpack(kvs[0].Value)
			Expect(err).NotTo(HaveOccurred())
			Expect(valueTuple).To(HaveLen(1))
			Expect(valueTuple[0]).To(Equal("Rose"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanIndex returns IndexEntry with both key and value", func() {
		coveringIndex := NewIndex("covering_price", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range []*gen.Order{
				{OrderId: proto.Int64(1), Price: proto.Int32(300), Flower: &gen.Flower{Type: proto.String("Rose")}},
				{OrderId: proto.Int64(2), Price: proto.Int32(100), Flower: &gen.Flower{Type: proto.String("Tulip")}},
				{OrderId: proto.Int64(3), Price: proto.Int32(200), Flower: &gen.Flower{Type: proto.String("Lily")}},
			} {
				_, err = store.SaveRecord(o)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan the index — entries should be sorted by price
			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(3))

			// Verify sorted by price ascending
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			// Verify value portion is populated
			Expect(entries[0].Value).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(Equal("Tulip")) // order_id=2 has price=100

			// Verify PrimaryKey extraction still works
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)}))
			Expect(entries[2].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete removes index entry and value", func() {
		coveringIndex := NewIndex("covering_price", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500), Flower: &gen.Flower{Type: proto.String("Rose")}})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(600), Flower: &gen.Flower{Type: proto.String("Tulip")}})
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
			Expect(entries[0].Value[0]).To(Equal("Tulip"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update changes value portion when indexed value changes", func() {
		// Concat(price, flower.type) split at 1: price in key, flower.type in value.
		coveringIndex := NewIndex("covering_price", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Flower: &gen.Flower{Type: proto.String("Rose")}})
			Expect(err).NotTo(HaveOccurred())

			// Update: price changes from 100 to 200, flower stays same
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200), Flower: &gen.Flower{Type: proto.String("Rose")}})
			Expect(err).NotTo(HaveOccurred())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(200))) // updated price
			Expect(entries[0].Value[0]).To(Equal("Rose"))   // flower type unchanged

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multi-column split: 2 key columns, 1 value column", func() {
		// Concat(price, order_id, flower.type) with splitPoint=2
		// Key: [price, order_id], Value: [flower.type]
		coveringIndex := NewIndex("covering_multi",
			KeyWithValue(Concat(Field("price"), Field("order_id"), Nest("flower", Field("type"))), 2))
		metaData := buildMetaWithIndex(coveringIndex)

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

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))
			// Key has 2 key columns + PK
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[0].Key[1]).To(Equal(int64(1)))
			// Value has flower type
			Expect(entries[0].Value).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(Equal("Rose"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("common entry skip works: unchanged value doesn't write", func() {
		// If both key and value are unchanged, the entry should be skipped.
		coveringIndex := NewIndex("covering_price", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save record
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Flower: &gen.Flower{Type: proto.String("Rose")}})
			Expect(err).NotTo(HaveOccurred())

			// Save exact same record again — should be a no-op for index
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Flower: &gen.Flower{Type: proto.String("Rose")}})
			Expect(err).NotTo(HaveOccurred())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ColumnSize returns splitPoint", func() {
		expr := KeyWithValue(Concat(Field("a"), Field("b"), Field("c")), 2)
		Expect(expr.ColumnSize()).To(Equal(2))
	})

	It("proto roundtrip preserves KeyWithValueExpression", func() {
		expr := KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1)
		p := expr.ToKeyExpression()

		restored, err := KeyExpressionFromProto(p)
		Expect(err).NotTo(HaveOccurred())

		kwv, ok := restored.(*KeyWithValueExpression)
		Expect(ok).To(BeTrue())
		Expect(kwv.SplitPoint()).To(Equal(1))

		// Inner key should be a CompositeKeyExpression with 2 children
		inner, ok := kwv.InnerKey().(*CompositeKeyExpression)
		Expect(ok).To(BeTrue())
		Expect(inner.expressions).To(HaveLen(2))
	})

	It("IndexValues returns only key columns for covering index", func() {
		coveringIndex := NewIndex("covering_price", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(300), Flower: &gen.Flower{Type: proto.String("Rose")}})
			Expect(err).NotTo(HaveOccurred())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))

			// IndexValues should return only key columns (1 = price), not PK
			indexVals := entries[0].IndexValues()
			Expect(indexVals).To(HaveLen(1))
			Expect(indexVals[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("PK dedup works with covering index when PK is in key portion", func() {
		// Index on (order_id, flower.type) with splitPoint=1. order_id overlaps with PK.
		// PK dedup should still work: order_id is in key[0], so PK is not appended.
		coveringIndex := NewIndex("covering_pk_dedup",
			KeyWithValue(Concat(Field("order_id"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(42), Price: proto.Int32(100),
				Flower: &gen.Flower{Type: proto.String("Rose")},
			})
			Expect(err).NotTo(HaveOccurred())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))

			// Key should be [order_id=42] only (PK deduplicated)
			Expect(entries[0].Key).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(42)))

			// Value should be [flower_type="Rose"]
			Expect(entries[0].Value).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(Equal("Rose"))

			// PrimaryKey should reconstruct correctly
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(42)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("splitPoint=0: all columns go to value, key is empty", func() {
		// splitPoint=0 means no columns in key, all in value. Only PK in the FDB key.
		coveringIndex := NewIndex("covering_all_value",
			KeyWithValue(Field("price"), 0))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
			Expect(err).NotTo(HaveOccurred())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(2))

			// Key has 0 index columns + PK only, sorted by PK (order_id)
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

			// Value has the price
			Expect(entries[0].Value).To(HaveLen(1))
			Expect(entries[0].Value[0]).To(Equal(int64(500)))
			Expect(entries[1].Value[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("splitPoint=len(inner): all columns in key, empty value", func() {
		// splitPoint equals number of inner columns — nothing goes to value.
		coveringIndex := NewIndex("covering_all_key",
			KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 2))
		metaData := buildMetaWithIndex(coveringIndex)

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

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			Expect(entries).To(HaveLen(1))

			// Key has both columns + PK
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[0].Key[1]).To(Equal("Rose"))

			// Value should be empty (splitPoint = len(inner))
			Expect(entries[0].Value).To(HaveLen(0))

			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("FanOut with covering index produces multiple entries with value", func() {
		// FanOut("tags") produces one entry per tag. With splitPoint=0, tags go to value.
		// Key portion is empty, so FDB key is just PK + tag value.
		coveringIndex := NewIndex("covering_fanout",
			KeyWithValue(Concat(FanOut("tags"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100),
				Tags:   []string{"premium", "gift"},
				Flower: &gen.Flower{Type: proto.String("Rose")},
			})
			Expect(err).NotTo(HaveOccurred())

			var entries []*IndexEntry
			for result, err := range Seq2(store.ScanIndex(coveringIndex, TupleRangeAll, nil, ForwardScan()), ctx) {
				Expect(err).NotTo(HaveOccurred())
				entries = append(entries, result)
			}
			// 2 entries: one per tag
			Expect(entries).To(HaveLen(2))

			// Sorted by tag (key[0]), both have flower type in value
			Expect(entries[0].Key[0]).To(Equal("gift"))
			Expect(entries[0].Value[0]).To(Equal("Rose"))
			Expect(entries[1].Key[0]).To(Equal("premium"))
			Expect(entries[1].Value[0]).To(Equal("Rose"))

			// Both entries point to same record
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("continuation token works with covering index", func() {
		coveringIndex := NewIndex("covering_price",
			KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))
		metaData := buildMetaWithIndex(coveringIndex)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)),
					Flower: &gen.Flower{Type: proto.String("Rose")},
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Page 1: scan with limit 2
			scan1 := ForwardScan()
			scan1.ExecuteProperties.ReturnedRowLimit = 2
			cursor := store.ScanIndex(coveringIndex, TupleRangeAll, nil, scan1)
			var continuation []byte
			var firstBatch []*IndexEntry
			for {
				r, nextErr := cursor.OnNext(ctx)
				Expect(nextErr).NotTo(HaveOccurred())
				if !r.HasNext() {
					var contErr error
					continuation, contErr = r.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					break
				}
				firstBatch = append(firstBatch, r.GetValue())
			}
			Expect(cursor.Close()).To(Succeed())
			Expect(firstBatch).To(HaveLen(2))
			Expect(firstBatch[0].Key[0]).To(Equal(int64(100)))
			Expect(firstBatch[1].Key[0]).To(Equal(int64(200)))
			Expect(firstBatch[0].Value[0]).To(Equal("Rose"))

			// Page 2: resume with continuation
			Expect(continuation).NotTo(BeNil())
			scan2 := ForwardScan()
			scan2.ExecuteProperties.ReturnedRowLimit = 2
			page2, err := AsList(ctx, store.ScanIndex(coveringIndex, TupleRangeAll, continuation, scan2))
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(2))
			Expect(page2[0].Key[0]).To(Equal(int64(300)))
			Expect(page2[1].Key[0]).To(Equal(int64(400)))

			// Value preserved across continuation
			Expect(page2[0].Value[0]).To(Equal("Rose"))
			Expect(page2[1].Value[0]).To(Equal("Rose"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
