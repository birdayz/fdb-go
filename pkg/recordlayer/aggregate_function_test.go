package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("EvaluateAggregateFunction", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		return builder
	}

	Describe("COUNT aggregate", func() {
		It("evaluates ungrouped count", func() {
			ks := specSubspace()

			countIdx := NewCountIndex("count_all", Ungrouped(EmptyKey()))
			builder := baseMetaData()
			builder.AddIndex("Order", countIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := range 5 {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
				}

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(5)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("evaluates grouped count with range", func() {
			ks := specSubspace()

			countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", countIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// price=100: 3 orders, price=200: 2 orders
				for i, price := range []int32{100, 200, 100, 100, 200} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				// Total count across all groups
				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameCount, Operand: GroupAll(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(5)}))

				// Count for price=100 only
				result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameCount, Operand: GroupAll(Field("price"))},
					TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(3)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SUM aggregate", func() {
		It("evaluates ungrouped sum", func() {
			ks := specSubspace()

			sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", sumIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range []int32{100, 200, 300} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(600)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MAX_EVER aggregate", func() {
		It("evaluates ungrouped max_ever", func() {
			ks := specSubspace()

			maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", maxIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range []int32{100, 500, 200} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: Ungrouped(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(500)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MIN_EVER aggregate", func() {
		It("evaluates ungrouped min_ever", func() {
			ks := specSubspace()

			minIdx := NewMinEverLongIndex("min_price", Ungrouped(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", minIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range []int32{300, 50, 200} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMinEver, Operand: Ungrouped(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(50)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("explicit index selection", func() {
		It("uses explicitly named index", func() {
			ks := specSubspace()

			sumIdx := NewSumIndex("my_sum", Ungrouped(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", sumIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
				Expect(err).NotTo(HaveOccurred())

				result, err := store.EvaluateAggregateFunction(ctx, nil,
					&IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(Field("price")), Index: "my_sum"},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(42)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for missing index", func() {
			ks := specSubspace()

			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.EvaluateAggregateFunction(ctx, nil,
					&IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(Field("price")), Index: "nonexistent"},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not found"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("empty store", func() {
		It("returns identity for COUNT on empty store", func() {
			ks := specSubspace()

			countIdx := NewCountIndex("count_all", Ungrouped(EmptyKey()))
			builder := baseMetaData()
			builder.AddIndex("Order", countIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				// No entries → identity = {0}
				Expect(result).To(Equal(tuple.Tuple{int64(0)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for MAX_EVER on empty store", func() {
			ks := specSubspace()

			maxIdx := NewMaxEverLongIndex("max_price", Ungrouped(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", maxIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMaxEver, Operand: Ungrouped(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				// No entries → nil (no identity for MAX_EVER)
				Expect(result).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MIN aggregate via VALUE index", func() {
		It("evaluates ungrouped min", func() {
			ks := specSubspace()

			// VALUE index on price — can serve MIN by scanning 1 entry forward
			valueIdx := NewIndex("price_idx", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", valueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range []int32{300, 50, 200} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMin, Operand: Field("price")},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(50)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for empty store", func() {
			ks := specSubspace()

			valueIdx := NewIndex("price_idx", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", valueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMin, Operand: Field("price")},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MAX aggregate via VALUE index", func() {
		It("evaluates ungrouped max", func() {
			ks := specSubspace()

			valueIdx := NewIndex("price_idx", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", valueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range []int32{100, 500, 200} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMax, Operand: Field("price")},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(500)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reflects deletes (unlike MAX_EVER)", func() {
			ks := specSubspace()

			valueIdx := NewIndex("price_idx", Field("price"))
			builder := baseMetaData()
			builder.AddIndex("Order", valueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(500)})
				Expect(err).NotTo(HaveOccurred())

				// Delete the max record
				_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())

				// MAX now reflects the remaining record
				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameMax, Operand: Field("price")},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(100)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("auto-select index", func() {
		It("finds correct index among multiple", func() {
			ks := specSubspace()

			countIdx := NewCountIndex("count_all", Ungrouped(EmptyKey()))
			sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
			builder := baseMetaData()
			builder.AddIndex("Order", countIdx)
			builder.AddIndex("Order", sumIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range []int32{100, 200} {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}

				// Count auto-selects count index
				countResult, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(countResult).To(Equal(tuple.Tuple{int64(2)}))

				// Sum auto-selects sum index
				sumResult, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(sumResult).To(Equal(tuple.Tuple{int64(300)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error when no matching index exists", func() {
			ks := specSubspace()

			builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameSum, Operand: Ungrouped(Field("price"))},
					TupleRangeAll, IsolationLevelSerializable)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("no index found"))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
