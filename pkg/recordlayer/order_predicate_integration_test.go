package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("Order function and predicate integration", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// Helper: build standard metadata with an Order index.
	buildMetadata := func(idx *Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// Helper: save orders and return the store for further assertions.
	saveOrders := func(store *FDBRecordStore, prices []*int32) {
		for i, p := range prices {
			id := int64(i + 1)
			order := &gen.Order{OrderId: &id}
			if p != nil {
				order.Price = p
			}
			_, err := store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	// ----------- Order function index tests -----------

	It("order_desc_nulls_last — forward scan returns descending order", func() {
		idx := NewIndex("Order$price_desc", FunctionExpr(OrderFuncDescNullsLast, Field("price")))
		md := buildMetadata(idx)
		ss := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			prices := []*int32{proto.Int32(100), proto.Int32(300), proto.Int32(500), proto.Int32(700), proto.Int32(900)}
			saveOrders(store, prices)

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			// DESC encoding inverts bytes — forward FDB scan yields descending price order.
			// Prices [100,300,500,700,900] have PKs [1,2,3,4,5].
			// DESC order: 900(pk=5), 700(pk=4), 500(pk=3), 300(pk=2), 100(pk=1).
			expectedPKs := []int64{5, 4, 3, 2, 1}
			for i, entry := range entries {
				Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{expectedPKs[i]}),
					"entry %d should have PK %d", i, expectedPKs[i])
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("order_desc_nulls_last — nulls sort last in forward scan", func() {
		idx := NewIndex("Order$price_desc_nl", FunctionExpr(OrderFuncDescNullsLast, Field("price")))
		md := buildMetadata(idx)
		ss := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Mix of set and nil prices.
			prices := []*int32{proto.Int32(100), nil, proto.Int32(500), nil, proto.Int32(900)}
			saveOrders(store, prices)

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			// DESC inverts: 900 first, then 500, 100. Nulls last.
			// Non-null PKs in descending price order: 5(900), 3(500), 1(100)
			// Null PKs: 2, 4 (order among nulls is unspecified, but both should come last)
			nonNullPKs := make([]int64, 0, 3)
			nullPKs := make([]int64, 0, 2)
			for i, entry := range entries {
				pk := entry.PrimaryKey()[0].(int64)
				if i < 3 {
					nonNullPKs = append(nonNullPKs, pk)
				} else {
					nullPKs = append(nullPKs, pk)
				}
			}
			Expect(nonNullPKs).To(Equal([]int64{5, 3, 1})) // 900, 500, 100
			Expect(nullPKs).To(ConsistOf(int64(2), int64(4)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("order_asc_nulls_last — nulls sort last, values ascending", func() {
		idx := NewIndex("Order$price_asc_nl", FunctionExpr(OrderFuncAscNullsLast, Field("price")))
		md := buildMetadata(idx)
		ss := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			prices := []*int32{proto.Int32(100), nil, proto.Int32(500)}
			saveOrders(store, prices)

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// ASC + NULLS_LAST: values in ascending order, then nulls.
			// PK 1 (100), PK 3 (500), PK 2 (nil)
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)}))
			Expect(entries[2].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("order function index — update preserves ordering", func() {
		idx := NewIndex("Order$price_desc_upd", FunctionExpr(OrderFuncDescNullsLast, Field("price")))
		md := buildMetadata(idx)
		ss := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save three orders: 100, 500, 900
			prices := []*int32{proto.Int32(100), proto.Int32(500), proto.Int32(900)}
			saveOrders(store, prices)

			// Update order 2 (price 500 → 200)
			id := int64(2)
			newPrice := int32(200)
			_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &newPrice})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// DESC: 900, 200, 100
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)})) // 900
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)})) // 200
			Expect(entries[2].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)})) // 100
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// ----------- Predicate from proto tests -----------

	It("predicate from proto — filters records during index maintenance", func() {
		priceIndex := NewIndex("Order$price_gt500", Field("price"))
		gt := gen.ComparisonType_GREATER_THAN
		err := priceIndex.SetPredicateProto(&gen.Predicate{
			ValuePredicate: &gen.ValuePredicate{
				Value: []string{"price"},
				Comparison: &gen.Comparison{
					SimpleComparison: &gen.SimpleComparison{
						Type:    &gt,
						Operand: &gen.Value{IntValue: proto.Int32(500)},
					},
				},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		md := buildMetadata(priceIndex)
		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			prices := []*int32{proto.Int32(100), proto.Int32(300), proto.Int32(500), proto.Int32(700), proto.Int32(900)}
			saveOrders(store, prices)

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(4)})) // price=700
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(5)})) // price=900
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("predicate from proto — delete of filtered record is no-op, indexed record is removed", func() {
		priceIndex := NewIndex("Order$price_gt500_del", Field("price"))
		gt := gen.ComparisonType_GREATER_THAN
		err := priceIndex.SetPredicateProto(&gen.Predicate{
			ValuePredicate: &gen.ValuePredicate{
				Value: []string{"price"},
				Comparison: &gen.Comparison{
					SimpleComparison: &gen.SimpleComparison{
						Type:    &gt,
						Operand: &gen.Value{IntValue: proto.Int32(500)},
					},
				},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		md := buildMetadata(priceIndex)
		ss := specSubspace()

		// Save records first.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			prices := []*int32{proto.Int32(100), proto.Int32(700), proto.Int32(900)}
			saveOrders(store, prices)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete record with price=100 (wasn't indexed) — should be fine.
		// Delete record with price=700 (was indexed) — should remove from index.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}

			// Delete non-indexed record (price=100, id=1)
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Delete indexed record (price=700, id=2)
			deleted, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Only price=900 (id=3) should remain in the index.
			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("predicate from proto — AND predicate filters correctly", func() {
		priceIndex := NewIndex("Order$price_and", Field("price"))
		gte := gen.ComparisonType_GREATER_THAN_OR_EQUALS
		lte := gen.ComparisonType_LESS_THAN_OR_EQUALS
		err := priceIndex.SetPredicateProto(&gen.Predicate{
			AndPredicate: &gen.AndPredicate{
				Children: []*gen.Predicate{
					{
						ValuePredicate: &gen.ValuePredicate{
							Value: []string{"price"},
							Comparison: &gen.Comparison{
								SimpleComparison: &gen.SimpleComparison{
									Type:    &gte,
									Operand: &gen.Value{IntValue: proto.Int32(200)},
								},
							},
						},
					},
					{
						ValuePredicate: &gen.ValuePredicate{
							Value: []string{"price"},
							Comparison: &gen.Comparison{
								SimpleComparison: &gen.SimpleComparison{
									Type:    &lte,
									Operand: &gen.Value{IntValue: proto.Int32(800)},
								},
							},
						},
					},
				},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		md := buildMetadata(priceIndex)
		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			prices := []*int32{proto.Int32(100), proto.Int32(200), proto.Int32(500), proto.Int32(800), proto.Int32(900)}
			saveOrders(store, prices)

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)})) // price=200
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(3)})) // price=500
			Expect(entries[2].PrimaryKey()).To(Equal(tuple.Tuple{int64(4)})) // price=800
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// ----------- Proto round-trip test -----------

	It("predicate proto round-trips through metadata serialization", func() {
		priceIndex := NewIndex("Order$price_rt", Field("price"))
		gt := gen.ComparisonType_GREATER_THAN
		predProto := &gen.Predicate{
			ValuePredicate: &gen.ValuePredicate{
				Value: []string{"price"},
				Comparison: &gen.Comparison{
					SimpleComparison: &gen.SimpleComparison{
						Type:    &gt,
						Operand: &gen.Value{IntValue: proto.Int32(500)},
					},
				},
			},
		}
		err := priceIndex.SetPredicateProto(predProto)
		Expect(err).NotTo(HaveOccurred())

		// Build original metadata.
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Serialize to proto.
		mdProto, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())

		// Deserialize back.
		md2, err := RecordMetaDataFromProto(mdProto)
		Expect(err).NotTo(HaveOccurred())

		// The deserialized index should have a working predicate and proto.
		roundTrippedIdx := md2.GetIndex("Order$price_rt")
		Expect(roundTrippedIdx).NotTo(BeNil())
		Expect(roundTrippedIdx.Predicate).NotTo(BeNil())
		Expect(roundTrippedIdx.GetPredicateProto()).NotTo(BeNil())

		// Verify the round-tripped proto matches the original.
		Expect(proto.Equal(roundTrippedIdx.GetPredicateProto(), predProto)).To(BeTrue(),
			"predicate proto should survive serialization round-trip")

		// Verify the rebuilt predicate evaluator works correctly on real messages.
		pred := roundTrippedIdx.Predicate
		Expect(pred(&gen.Order{Price: proto.Int32(100)})).To(BeFalse(), "100 should not match > 500")
		Expect(pred(&gen.Order{Price: proto.Int32(500)})).To(BeFalse(), "500 should not match > 500")
		Expect(pred(&gen.Order{Price: proto.Int32(700)})).To(BeTrue(), "700 should match > 500")
		Expect(pred(&gen.Order{Price: proto.Int32(900)})).To(BeTrue(), "900 should match > 500")
		Expect(pred(&gen.Order{})).To(BeFalse(), "unset price should not match > 500")
	})

	It("order function index -- unique enforcement on encoded bytes", func() {
		// Create a UNIQUE index with order_asc_nulls_first on price.
		// Two records with the same price should conflict.
		orderIdx := NewIndex("Order$orderPrice", FunctionExpr(OrderFuncAscNullsFirst, Field("price")))
		orderIdx.SetUnique()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", orderIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		// Save first record with price=500
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Save second record with DIFFERENT price — should succeed
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Save third record with SAME price as first — should fail uniqueness
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(500)})
			return nil, err
		})
		Expect(err).To(HaveOccurred())
		var uniqueErr *RecordIndexUniquenessViolationError
		Expect(err).To(BeAssignableToTypeOf(uniqueErr))
	})

	It("order function index -- scan with TupleRange filters correctly", func() {
		// Create index with order_asc_nulls_first so byte ordering = natural ordering.
		orderIdx := NewIndex("Order$ascPrice", FunctionExpr(OrderFuncAscNullsFirst, Field("price")))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", orderIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			for _, p := range []int32{100, 300, 500, 700, 900} {
				id := int64(p)
				price := p
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
			}

			// Scan with range: only the encoded bytes for price=300
			// Since ASC_NULLS_FIRST = standard tuple.Pack(), we can use AllOf.
			encoded300 := tupleOrderingPack(tuple.Tuple{int64(300)}, OrderAscNullsFirst)
			encoded700 := tupleOrderingPack(tuple.Tuple{int64(700)}, OrderAscNullsFirst)
			scanRange := TupleRangeBetween(
				tuple.Tuple{encoded300},
				tuple.Tuple{encoded700},
			)
			entries, err := AsList(ctx, store.ScanIndex(orderIdx, scanRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			// Should include 300 (inclusive low), 500, but NOT 700 (exclusive high)
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(300)}))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(500)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
