package recordlayer

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Query Plan Execution", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", NewIndex("order_price", Field("price")))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	saveOrders := func(store *FDBRecordStore, orders ...*gen.Order) {
		for _, o := range orders {
			_, err := store.SaveRecord(o)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	Describe("ScanPlan", func() {
		It("scans all records", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)},
					&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
				)

				plan := &ScanPlan{}
				cursor := plan.Execute(store, nil, ForwardScan())
				var count int
				for result, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					Expect(result).NotTo(BeNil())
					count++
				}
				Expect(count).To(Equal(3))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scans filtered by type", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
				)
				_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(100), Name: proto.String("Alice")})
				if err != nil {
					return nil, err
				}

				plan := &ScanPlan{RecordTypeName: "Order"}
				cursor := plan.Execute(store, nil, ForwardScan())
				var count int
				for _, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					count++
				}
				Expect(count).To(Equal(1))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IndexPlan", func() {
		It("scans index and returns records", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)},
					&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
				)

				plan := &IndexPlan{
					IndexName: "order_price",
					Range:     TupleRangeAllOf(tuple.Tuple{int64(20)}),
				}
				cursor := plan.Execute(store, nil, ForwardScan())
				var records []*FDBStoredRecord[proto.Message]
				for result, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					records = append(records, result)
				}
				Expect(records).To(HaveLen(1))
				order := records[0].Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(2)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("FilterPlan", func() {
		It("filters results from child plan", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)},
					&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
				)

				plan := &FilterPlan{
					Child: &ScanPlan{RecordTypeName: "Order"},
					Predicate: func(r *FDBStoredRecord[proto.Message]) bool {
						order := r.Record.(*gen.Order)
						return order.GetPrice() > 15
					},
					Description: "price > 15",
				}
				cursor := plan.Execute(store, nil, ForwardScan())
				var count int
				for _, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					count++
				}
				Expect(count).To(Equal(2)) // orders 2 and 3
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("PrimaryKeyLookupPlan", func() {
		It("fetches a single record by PK", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(100)},
				)

				plan := &PrimaryKeyLookupPlan{PrimaryKey: tuple.Tuple{int64(42)}}
				cursor := plan.Execute(store, nil, ForwardScan())
				var records []*FDBStoredRecord[proto.Message]
				for result, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					records = append(records, result)
				}
				Expect(records).To(HaveLen(1))
				order := records[0].Record.(*gen.Order)
				Expect(order.GetPrice()).To(Equal(int32(100)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty for non-existent PK", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				plan := &PrimaryKeyLookupPlan{PrimaryKey: tuple.Tuple{int64(999)}}
				cursor := plan.Execute(store, nil, ForwardScan())
				var count int
				for _, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					count++
				}
				Expect(count).To(Equal(0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("UnionPlan", func() {
		It("merges results from two index scans", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)},
					&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
				)

				// Union of price=10 and price=30 — should return orders 1 and 3
				plan := &UnionPlan{
					Left:  &IndexPlan{IndexName: "order_price", Range: TupleRangeAllOf(tuple.Tuple{int64(10)})},
					Right: &IndexPlan{IndexName: "order_price", Range: TupleRangeAllOf(tuple.Tuple{int64(30)})},
				}
				cursor := plan.Execute(store, nil, ForwardScan())
				var ids []int64
				for result, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					order := result.Record.(*gen.Order)
					ids = append(ids, order.GetOrderId())
				}
				Expect(ids).To(HaveLen(2))
				Expect(ids).To(ContainElement(int64(1)))
				Expect(ids).To(ContainElement(int64(3)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("LimitPlan", func() {
		It("limits results from child plan", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				saveOrders(store,
					&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)},
					&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
					&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(40)},
					&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(50)},
				)

				plan := &LimitPlan{
					Child: &ScanPlan{RecordTypeName: "Order"},
					Limit: 2,
				}
				cursor := plan.Execute(store, nil, ForwardScan())
				var count int
				for _, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					count++
				}
				Expect(count).To(Equal(2))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Composability", func() {
		It("composes filter + limit + index scan", func() {
			ss := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 10; i++ {
					_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					if err != nil {
						return nil, err
					}
				}

				// "SELECT * FROM Order WHERE price > 50 LIMIT 3"
				plan := &LimitPlan{
					Limit: 3,
					Child: &FilterPlan{
						Child:       &ScanPlan{RecordTypeName: "Order"},
						Description: "price > 50",
						Predicate: func(r *FDBStoredRecord[proto.Message]) bool {
							return r.Record.(*gen.Order).GetPrice() > 50
						},
					},
				}
				cursor := plan.Execute(store, nil, ForwardScan())
				var prices []int32
				for result, err := range Seq2(cursor, ctx) {
					Expect(err).NotTo(HaveOccurred())
					prices = append(prices, result.Record.(*gen.Order).GetPrice())
				}
				Expect(prices).To(HaveLen(3))
				// First 3 orders with price > 50: 60, 70, 80
				Expect(prices).To(Equal([]int32{60, 70, 80}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Explain", func() {
		It("produces readable plan descriptions", func() {
			scan := &ScanPlan{RecordTypeName: "Order"}
			Expect(scan.Explain(0)).To(Equal("Scan(Order)"))

			idx := &IndexPlan{IndexName: "order_price", Range: TupleRangeAll}
			Expect(idx.Explain(0)).To(ContainSubstring("Index(order_price, ALL)"))

			filter := &FilterPlan{
				Child:       scan,
				Description: "price > 10",
			}
			explain := filter.Explain(0)
			Expect(explain).To(ContainSubstring("Filter(price > 10)"))
			Expect(explain).To(ContainSubstring("Scan(Order)"))

			lookup := &PrimaryKeyLookupPlan{PrimaryKey: tuple.Tuple{int64(42)}}
			Expect(lookup.Explain(0)).To(ContainSubstring("Lookup"))

			union := &UnionPlan{
				Left:  &ScanPlan{RecordTypeName: "Order"},
				Right: &ScanPlan{RecordTypeName: "Customer"},
			}
			unionExplain := union.Explain(0)
			Expect(unionExplain).To(ContainSubstring("Union"))
			Expect(unionExplain).To(ContainSubstring("Scan(Order)"))
			Expect(unionExplain).To(ContainSubstring("Scan(Customer)"))
		})
	})
})
