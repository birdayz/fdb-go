package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("PrimaryKeyComponentDeduplication", func() {
	ctx := context.Background()

	Describe("buildPrimaryKeyComponentPositions", func() {
		It("returns nil when no overlap", func() {
			// Index on price, PK on order_id — no overlap
			positions := buildPrimaryKeyComponentPositions(Field("price"), Field("order_id"))
			Expect(positions).To(BeNil())
		})

		It("detects full overlap for single field", func() {
			// Index on order_id, PK on order_id — full overlap
			positions := buildPrimaryKeyComponentPositions(Field("order_id"), Field("order_id"))
			Expect(positions).To(Equal([]int{0}))
		})

		It("detects partial overlap in composite", func() {
			// Index on (price, order_id), PK on order_id
			// order_id is at index position 1
			positions := buildPrimaryKeyComponentPositions(
				Concat(Field("price"), Field("order_id")),
				Field("order_id"),
			)
			Expect(positions).To(Equal([]int{1}))
		})

		It("handles composite PK with partial overlap", func() {
			// Index on Field("name"), PK on Concat(RecordTypeKey(), Field("name"))
			// RecordTypeKey is not in index (-1), name is at position 0
			positions := buildPrimaryKeyComponentPositions(
				Field("name"),
				Concat(RecordTypeKey(), Field("name")),
			)
			Expect(positions).To(Equal([]int{-1, 0}))
		})

		It("handles full composite overlap", func() {
			// Index on (price, order_id), PK on (price, order_id)
			positions := buildPrimaryKeyComponentPositions(
				Concat(Field("price"), Field("order_id")),
				Concat(Field("price"), Field("order_id")),
			)
			Expect(positions).To(Equal([]int{0, 1}))
		})

		It("handles nested expression overlap", func() {
			// Index on Nest("flower", Field("type")), PK on Field("order_id") — no overlap
			positions := buildPrimaryKeyComponentPositions(
				Nest("flower", Field("type")),
				Field("order_id"),
			)
			Expect(positions).To(BeNil())
		})
	})

	Describe("normalizeKeyForPositions", func() {
		It("flattens composite expression", func() {
			expr := Concat(Field("a"), Field("b"), Field("c"))
			norms := normalizeKeyForPositions(expr)
			Expect(norms).To(HaveLen(3))
		})

		It("handles nested composite", func() {
			expr := Concat(Concat(Field("a"), Field("b")), Field("c"))
			norms := normalizeKeyForPositions(expr)
			Expect(norms).To(HaveLen(3))
		})

		It("returns singleton for field", func() {
			norms := normalizeKeyForPositions(Field("x"))
			Expect(norms).To(HaveLen(1))
		})

		It("re-wraps nesting children", func() {
			// Nest("parent", Concat(Field("a"), Field("b"))) →
			//   [Nest("parent", Field("a")), Nest("parent", Field("b"))]
			expr := Nest("parent", Concat(Field("a"), Field("b")))
			norms := normalizeKeyForPositions(expr)
			Expect(norms).To(HaveLen(2))
			// Both should be NestingKeyExpressions
			for _, n := range norms {
				_, ok := n.(*NestingKeyExpression)
				Expect(ok).To(BeTrue())
			}
		})
	})

	Describe("keyExpressionEquals", func() {
		It("matches identical fields", func() {
			Expect(keyExpressionEquals(Field("x"), Field("x"))).To(BeTrue())
		})

		It("rejects different fields", func() {
			Expect(keyExpressionEquals(Field("x"), Field("y"))).To(BeFalse())
		})

		It("matches RecordTypeKey expressions", func() {
			Expect(keyExpressionEquals(RecordTypeKey(), RecordTypeKey())).To(BeTrue())
		})

		It("rejects different types", func() {
			Expect(keyExpressionEquals(Field("x"), RecordTypeKey())).To(BeFalse())
		})

		It("matches nested expressions", func() {
			a := Nest("parent", Field("child"))
			b := Nest("parent", Field("child"))
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("rejects different nested parents", func() {
			a := Nest("parent1", Field("child"))
			b := Nest("parent2", Field("child"))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})
	})

	Describe("trimPrimaryKey and getEntryPrimaryKey", func() {
		It("no-op when positions is nil", func() {
			idx := NewIndex("test", Field("price"))
			pk := tuple.Tuple{int64(42)}
			trimmed, err := idx.TrimPrimaryKey(pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(trimmed).To(Equal(pk))
			// getEntryPrimaryKey should return the tail
			entry := tuple.Tuple{int64(100), int64(42)} // (price, pk)
			Expect(idx.getEntryPrimaryKey(entry)).To(Equal(tuple.Tuple{int64(42)}))
		})

		It("trims fully overlapping PK", func() {
			idx := NewIndex("test", Concat(Field("price"), Field("order_id")))
			idx.primaryKeyComponentPositions = []int{1} // order_id is at index position 1
			pk := tuple.Tuple{int64(42)}
			trimmed, err := idx.TrimPrimaryKey(pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(trimmed).To(HaveLen(0))
		})

		It("trims partial overlap", func() {
			idx := NewIndex("test", Field("name"))
			// PK is (RecordType, name). RecordType not in index (-1), name at position 0
			idx.primaryKeyComponentPositions = []int{-1, 0}
			pk := tuple.Tuple{int64(1), "Alice"}
			trimmed, err := idx.TrimPrimaryKey(pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(trimmed).To(Equal(tuple.Tuple{int64(1)})) // Only RecordType remains
		})

		It("reconstructs PK from partial overlap", func() {
			idx := NewIndex("test", Field("name"))
			idx.primaryKeyComponentPositions = []int{-1, 0}
			// Entry: (name_value, record_type) — name is index col, record_type is appended
			entry := tuple.Tuple{"Alice", int64(1)}
			pk := idx.getEntryPrimaryKey(entry)
			// PK should be (record_type, name) — reconstructed in original order
			Expect(pk).To(Equal(tuple.Tuple{int64(1), "Alice"}))
		})
	})

	Describe("end-to-end index deduplication", func() {
		It("deduplicates PK when index key contains PK field", func() {
			// Index on (price, order_id), PK on order_id.
			// order_id appears in both → should be deduplicated.
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", compositeIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				order := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				// Raw FDB key should have only 2 elements (price, order_id)
				// NOT 3 (price, order_id, order_id)
				idxSubspace := store.subspace.Sub(IndexKey, compositeIndex.SubspaceTupleKey())
				begin, end := idxSubspace.FDBRangeKeys()
				kvs, kvErr := rtx.Transaction().GetRange(
					fdb.KeyRange{Begin: begin, End: end},
					fdb.RangeOptions{},
				).GetSliceWithError()
				Expect(kvErr).NotTo(HaveOccurred())
				Expect(kvs).To(HaveLen(1))

				entryTuple, unpackErr := idxSubspace.Unpack(kvs[0].Key)
				Expect(unpackErr).NotTo(HaveOccurred())
				Expect(entryTuple).To(HaveLen(2)) // Deduplicated!
				Expect(entryTuple[0]).To(Equal(int64(999)))
				Expect(entryTuple[1]).To(Equal(int64(42)))

				// ScanIndex should still reconstruct PK correctly
				entries, scanErr := AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(scanErr).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(42)}))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(999), int64(42)}))

				// ScanIndexRecords should fetch the actual record
				records, recErr := AsList(ctx, store.ScanIndexRecords("Order$price_id", TupleRangeAll, nil, ForwardScan()))
				Expect(recErr).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(1))
				Expect(records[0].Record.Record.(*gen.Order).GetOrderId()).To(Equal(int64(42)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("no dedup when index and PK don't overlap", func() {
			// Index on price, PK on order_id — no overlap, PK appended as-is
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				order := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				// Entry should have 2 elements: (price, order_id) — PK not in index, appended
				idxSubspace := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				begin, end := idxSubspace.FDBRangeKeys()
				kvs, kvErr := rtx.Transaction().GetRange(
					fdb.KeyRange{Begin: begin, End: end},
					fdb.RangeOptions{},
				).GetSliceWithError()
				Expect(kvErr).NotTo(HaveOccurred())
				Expect(kvs).To(HaveLen(1))

				entryTuple, unpackErr := idxSubspace.Unpack(kvs[0].Key)
				Expect(unpackErr).NotTo(HaveOccurred())
				Expect(entryTuple).To(HaveLen(2)) // (price, order_id) — no dedup needed
				Expect(entryTuple[0]).To(Equal(int64(999)))
				Expect(entryTuple[1]).To(Equal(int64(42)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("CRUD works with deduplication", func() {
			// Full lifecycle: insert, update, delete with dedup active
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", compositeIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert
				order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				entries, _ := AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

				// Update price
				order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order2)
				Expect(err).NotTo(HaveOccurred())

				entries, _ = AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(200), int64(1)}))

				// Delete
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				entries, _ = AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(entries).To(HaveLen(0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("unique index with dedup works", func() {
			// Unique composite index where PK is part of index key
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id"))).SetUnique()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", compositeIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Different orders with same price — should work because order_id differs
				for i := int64(1); i <= 3; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}

				entries, _ := AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(entries).To(HaveLen(3))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("multi-type index PK dedup", func() {
		// Regression test: Build() previously skipped rt.multiTypeIndexes
		// when computing primaryKeyComponentPositions. Multi-type index
		// entries had full redundant PKs instead of trimmed PKs.
		It("computes primaryKeyComponentPositions for multi-type indexes", func() {
			// Create a multi-type index on (order_id, price) spanning Order+Customer.
			// order_id overlaps with Order's PK — should be deduped at position 0.
			compositeIdx := NewIndex("multi_order_id_price", Concat(Field("order_id"), Field("price")))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddMultiTypeIndex([]string{"Order", "Customer"}, compositeIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Verify primaryKeyComponentPositions was computed.
			idx := md.GetIndex("multi_order_id_price")
			Expect(idx).NotTo(BeNil())
			Expect(idx.HasPrimaryKeyComponentPositions()).To(BeTrue(),
				"multi-type index should have primaryKeyComponentPositions computed")
		})

		It("multi-type index entry has trimmed PK via dedup", func() {
			ks := specSubspace()

			// Index on (order_id, price) with PK = order_id.
			// order_id at position 0 in index key matches PK → dedup.
			// Without the fix: index entry key = [order_id, price, order_id] (redundant)
			// With the fix:    index entry key = [order_id, price] (trimmed)
			compositeIdx := NewIndex("multi_oid_price", Concat(Field("order_id"), Field("price")))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddMultiTypeIndex([]string{"Order", "Customer"}, compositeIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(42),
					Price:   proto.Int32(999),
				})
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(compositeIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				entry := entries[0]
				// With PK dedup: key = [42, 999], PK reconstructed to [42]
				// Without dedup: key = [42, 999, 42], PK = last element = [42]
				// The entry key length tells us if dedup happened.
				Expect(entry.Key).To(HaveLen(2),
					"index entry key should be [order_id, price] (2 elements, PK deduped), got %v", entry.Key)
				Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{int64(42)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
