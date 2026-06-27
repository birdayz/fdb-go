package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("PermutedMinMaxIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// Index expression: GroupBy(Field("order_id"), Field("price"))
	//   wholeKey = Concat(price, order_id)
	//   groupedCount = 1 (order_id is "grouped"/aggregated)
	//   grouping columns = [price] (first column)
	//   grouped (value) columns = [order_id] (trailing 1 column)
	//
	// With permutedSize=1, the last 1 grouping column (price) is permuted
	// after the value in the secondary subspace.
	//
	// Primary subspace entries: [price, order_id, pk...]
	// Secondary subspace entries: [order_id, price]
	//   (groupPrefix=[] empty since permutePosition=0, value=[order_id], groupSuffix=[price])
	//
	// For a cleaner multi-record-per-group scenario, we use price as the
	// grouping column. Multiple orders can share the same price, and we
	// find the max/min order_id per price group.

	// =========================================================================
	// 1. PERMUTED_MAX basic insert
	// =========================================================================
	It("PERMUTED_MAX basic insert: BY_VALUE returns all, BY_GROUP returns max per group", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100 group: order_ids 1, 3
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=200 group: order_id 2
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// BY_VALUE (primary): all 3 entries sorted by [price, order_id]
			byValue, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byValue).To(HaveLen(3))
			// (100,1), (100,3), (200,2)
			Expect(byValue[0].Key[0]).To(Equal(int64(100)))
			Expect(byValue[0].Key[1]).To(Equal(int64(1)))
			Expect(byValue[1].Key[0]).To(Equal(int64(100)))
			Expect(byValue[1].Key[1]).To(Equal(int64(3)))
			Expect(byValue[2].Key[0]).To(Equal(int64(200)))
			Expect(byValue[2].Key[1]).To(Equal(int64(2)))

			// BY_GROUP (secondary/permuted): one entry per group with max order_id.
			// Price=100 max order_id=3, Price=200 max order_id=2.
			// Permuted key: [order_id, price] → sorted by order_id.
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(2))
			// Sorted by [order_id, price]: (2, 200), (3, 100)
			Expect(byGroup[0].Key[0]).To(Equal(int64(2)))
			Expect(byGroup[0].Key[1]).To(Equal(int64(200)))
			Expect(byGroup[1].Key[0]).To(Equal(int64(3)))
			Expect(byGroup[1].Key[1]).To(Equal(int64(100)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 2. PERMUTED_MAX: delete record with max value, BY_GROUP shows new max
	// =========================================================================
	It("PERMUTED_MAX: delete record with max, BY_GROUP falls back to next", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100 group: order_ids 1, 5, 10
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Max order_id for price=100 is 10
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(10))) // max order_id

			// Delete order_id=10 (the max)
			_, err = store.DeleteRecord(tuple.Tuple{int64(10)})
			Expect(err).NotTo(HaveOccurred())

			// Now max should be 5
			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(5)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 3. PERMUTED_MIN basic: BY_GROUP returns min per group
	// =========================================================================
	It("PERMUTED_MIN basic: BY_GROUP returns min per group", func() {
		ks := specSubspace()

		idx := NewPermutedMinIndex("Order$minOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100 group: order_ids 3, 1, 7
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=200 group: order_ids 4, 2
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: min per group.
			// Price=100 min=1, Price=200 min=2.
			// Permuted key: [order_id, price] → sorted by order_id.
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(2))
			// (1, 100), (2, 200)
			Expect(byGroup[0].Key[0]).To(Equal(int64(1)))
			Expect(byGroup[0].Key[1]).To(Equal(int64(100)))
			Expect(byGroup[1].Key[0]).To(Equal(int64(2)))
			Expect(byGroup[1].Key[1]).To(Equal(int64(200)))

			// BY_VALUE: all 5 entries
			byValue, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byValue).To(HaveLen(5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 4. PERMUTED_MAX delete updates extremum chain
	// =========================================================================
	It("PERMUTED_MAX delete chain: successively removing max updates extremum", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100 group: order_ids 10, 20, 30
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(20), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(30), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Max is 30
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(30)))

			// Delete 30 → max becomes 20
			_, err = store.DeleteRecord(tuple.Tuple{int64(30)})
			Expect(err).NotTo(HaveOccurred())
			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(20)))

			// Delete 20 → max becomes 10
			_, err = store.DeleteRecord(tuple.Tuple{int64(20)})
			Expect(err).NotTo(HaveOccurred())
			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(10)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 5. PERMUTED_MAX delete last in group → group disappears
	// =========================================================================
	It("PERMUTED_MAX delete last in group removes group from BY_GROUP", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: one order
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			// Price=200: one order
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(2))

			// Delete the only record in price=100 group
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(2)))   // order_id
			Expect(byGroup[0].Key[1]).To(Equal(int64(200))) // price

			// Delete last remaining → empty
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 6. PERMUTED_MAX with multiple groups and AllOf range query
	// =========================================================================
	It("PERMUTED_MAX with multiple groups: AllOf range on BY_VALUE filters by price", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: orders 1, 5
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=200: orders 2, 8
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(8), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Price=300: order 4
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(300)})
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP (all): 3 groups
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(3))
			// Sorted by [order_id, price]: (4,300), (5,100), (8,200)
			Expect(byGroup[0].Key[0]).To(Equal(int64(4)))
			Expect(byGroup[1].Key[0]).To(Equal(int64(5)))
			Expect(byGroup[2].Key[0]).To(Equal(int64(8)))

			// BY_VALUE with AllOf(price=200): entries for price=200 only
			range200 := TupleRangeAllOf(tuple.Tuple{int64(200)})
			filtered, err := AsList(ctx, store.ScanIndex(idx, range200, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(filtered).To(HaveLen(2))
			// (200, 2), (200, 8)
			Expect(filtered[0].Key[0]).To(Equal(int64(200)))
			Expect(filtered[0].Key[1]).To(Equal(int64(2)))
			Expect(filtered[1].Key[0]).To(Equal(int64(200)))
			Expect(filtered[1].Key[1]).To(Equal(int64(8)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 7. PERMUTED_MAX aggregate function
	// =========================================================================
	It("PERMUTED_MAX aggregate: EvaluateAggregateFunction with max", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: orders 1, 5
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			// Price=200: order 3
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Max order_id across all groups = 5 (from price=100 group)
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMax,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(5)}))

			// Max order_id for price=100 group
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMax,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(5)}))

			// Max order_id for price=200 group
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMax,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAllOf(tuple.Tuple{int64(200)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 8. PERMUTED_MIN aggregate function
	// =========================================================================
	It("PERMUTED_MIN aggregate: EvaluateAggregateFunction with min", func() {
		ks := specSubspace()

		idx := NewPermutedMinIndex("Order$minOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: orders 3, 7
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			// Price=200: orders 1, 9
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(9), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Min order_id across all groups = 1 (from price=200 group)
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMin,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(1)}))

			// Min order_id for price=100 group
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMin,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(3)}))

			// Min order_id for price=200 group
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMin,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAllOf(tuple.Tuple{int64(200)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 9. PERMUTED_MAX: adding new record with higher value updates BY_GROUP
	// =========================================================================
	It("PERMUTED_MAX: new higher value in same group updates BY_GROUP", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: order 1
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Max for price=100 is 1
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(1)))

			// Add order 5 in same group → new max
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Max for price=100 is now 5
			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(5)))
			Expect(byGroup[0].Key[1]).To(Equal(int64(100)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 10. PERMUTED_MIN delete: removes min, BY_GROUP shows next lowest
	// =========================================================================
	It("PERMUTED_MIN delete: removes min, BY_GROUP shows next lowest", func() {
		ks := specSubspace()

		idx := NewPermutedMinIndex("Order$minOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: orders 5, 10, 15
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(15), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Min is 5
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(5)))

			// Delete order_id=5 → min becomes 10
			_, err = store.DeleteRecord(tuple.Tuple{int64(5)})
			Expect(err).NotTo(HaveOccurred())

			byGroup, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(10)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 11. PERMUTED_MAX: empty store aggregate returns nil
	// =========================================================================
	It("PERMUTED_MAX aggregate on empty store returns nil", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameMax,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 12. PERMUTED_MAX: lower value insert does not change BY_GROUP
	// =========================================================================
	It("PERMUTED_MAX: inserting lower value does not change BY_GROUP extremum", func() {
		ks := specSubspace()

		idx := NewPermutedMaxIndex("Order$maxOrderByPrice",
			GroupBy(Field("order_id"), Field("price")), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: order 50 (high order_id)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(50), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=100: order 10 (lower order_id)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP should still show 50 as max
			byGroup, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byGroup).To(HaveLen(1))
			Expect(byGroup[0].Key[0]).To(Equal(int64(50)))

			// BY_VALUE: both entries exist
			byValue, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(byValue).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
