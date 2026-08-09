package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("CountNotNullIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("counts only non-null entries (ungrouped)", func() {
		ks := specSubspace()

		// Ungrouped(Field("price")) = all columns are GROUPED → null check applies to price.
		// Java's COUNT_NOT_NULL.getMutationParam checks null on the GROUPED portion only.
		idx := NewCountNotNullIndex("count_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Order with price=100
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Order without price (nil) — should NOT be counted (null grouped column)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Order with price=200
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Only 1 ungrouped entry with count=2 (price=100 + price=200), null skipped
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("GroupAll counts ALL entries including null (Java compat)", func() {
		ks := specSubspace()

		// GroupAll(Field("price")) = all columns are GROUPING → grouped portion is empty.
		// Java's null check on empty grouped portion always passes → null prices ARE counted.
		// This matches Java: GroupAll + COUNT_NOT_NULL == GROUP BY with no null filtering.
		idx := NewCountNotNullIndex("count_price", GroupAll(Field("price")))
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
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// ALL 3 entries — null price IS counted (grouped portion is empty, no null check)
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("decrements on delete of non-null entry", func() {
		ks := specSubspace()

		idx := NewCountNotNullIndex("count_price", GroupAll(Field("price")))
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
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Delete one
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete of null-grouped-key entry is no-op (ungrouped)", func() {
		ks := specSubspace()

		// Ungrouped: null check applies to price (grouped portion)
		idx := NewCountNotNullIndex("count_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// One with price, one without
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Delete the null-price order — count should stay the same (null was never counted)
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update from null to non-null increments (ungrouped)", func() {
		ks := specSubspace()

		// Ungrouped: null check applies to price (grouped portion)
		idx := NewCountNotNullIndex("count_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save without price — null grouped column → not counted, no entry written
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			// Update to have price → now counted
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)})
			Expect(err).NotTo(HaveOccurred())

			entries, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ungrouped count skips null entries", func() {
		ks := specSubspace()

		// Ungrouped — counts all records with non-null price
		idx := NewCountNotNullIndex("count_nonnull", Ungrouped(Field("price")))
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
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)}) // no price
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)})) // only 2 non-null

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("evaluates aggregate function", func() {
		ks := specSubspace()

		idx := NewCountNotNullIndex("count_nonnull", Ungrouped(Field("price")))
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
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)}) // no price
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameCountNotNull, Operand: Ungrouped(Field("price"))},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A NULL GROUPING column must not suppress the row. Java splits the
	// evaluated entry and runs its null check on the GROUPED suffix only —
	// AtomicMutationIndexMaintainer.updateIndexKeys builds groupKey =
	// subTuple(key, 0, groupPrefixSize) and groupedValue = subKey(groupPrefixSize,
	// keySize), and passes only groupedValue to getMutationParam
	// (AtomicMutationIndexMaintainer.java:141-146), which is where
	// keyContainsNonUniqueNull runs (AtomicMutation.java:165-171). A null in the
	// GROUP BY column is a group, not a reason to drop the row.
	//
	// This is the dimension that was unprobed while the bug was live, and it
	// stayed unprobed after the fix. Every other COUNT_NOT_NULL case in this file
	// uses Ungrouped or GroupAll, i.e. groupingCount == 0, where "whole key" and
	// "grouped suffix" are the SAME set of columns — so all of them pass
	// identically whether the maintainer checks the whole key or only the
	// suffix, and none of them can tell the fixed behaviour from the bug. Only a
	// NON-EMPTY grouping prefix separates the two.
	It("counts a row whose GROUPING column is null when the grouped column is not", func() {
		ks := specSubspace()

		// wholeKey = Concat(quantity, price), groupedCount = 1: GROUP BY
		// quantity, COUNT_NOT_NULL(price).
		idx := NewCountNotNullIndex("count_price_by_quantity", GroupBy(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// quantity UNSET (null grouping column), price set. Java counts it
			// under the null group; a whole-key null check drops it silently.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// quantity set — the control group, which a whole-key check also counts.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Quantity: proto.Int32(7), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// price UNSET — dropped by BOTH readings, because price is the
			// grouped column. Present so the assertion below cannot pass by the
			// maintainer having simply stopped filtering nulls at all.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Quantity: proto.Int32(7)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			Expect(entries).To(HaveLen(2),
				"expected a null-quantity group AND a quantity=7 group; one entry means the null GROUPING "+
					"column suppressed its row, which is the whole-key null check Java does not perform")
			counts := map[any]tuple.Tuple{}
			for _, e := range entries {
				Expect(e.Key).To(HaveLen(1))
				counts[e.Key[0]] = e.Value
			}
			Expect(counts[nil]).To(Equal(tuple.Tuple{int64(1)}),
				"the row with a NULL quantity and a non-null price must be counted under the null group")
			Expect(counts[int64(7)]).To(Equal(tuple.Tuple{int64(1)}),
				"quantity=7 has one non-null price (order 2); order 3's null price is correctly excluded")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
