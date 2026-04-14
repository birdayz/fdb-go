package recordlayer

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("QueryBuilder", func() {
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

	It("builds scan + filter + limit", func() {
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

			// SELECT * FROM Order WHERE price > 50 LIMIT 3
			plan := NewQueryFrom("Order").
				Filter("price > 50", func(r *FDBStoredRecord[proto.Message]) bool {
					return r.Record.(*gen.Order).GetPrice() > 50
				}).
				Limit(3).
				Build()

			results, err := ExecuteAndCollect(ctx, store, plan)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("builds index scan", func() {
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
				if err != nil {
					return nil, err
				}
			}

			// SELECT * FROM Order WHERE price = 30
			plan := NewQueryFromIndex("order_price", TupleRangeAllOf(tuple.Tuple{int64(30)})).Build()
			results, err := ExecuteAndCollect(ctx, store, plan)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Record.(*gen.Order).GetOrderId()).To(Equal(int64(3)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("builds reverse scan", func() {
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
				if err != nil {
					return nil, err
				}
			}

			// SELECT * FROM Order ORDER BY order_id DESC LIMIT 1
			plan := NewQueryFrom("Order").Reverse().Limit(1).Build()
			first, err := ExecuteFirst(ctx, store, plan)
			Expect(err).NotTo(HaveOccurred())
			Expect(first).NotTo(BeNil())
			Expect(first.Record.(*gen.Order).GetOrderId()).To(Equal(int64(5)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("explains built plan", func() {
		plan := NewQueryFrom("Order").
			Filter("price > 50", nil).
			Limit(3)

		explain := plan.Explain()
		Expect(explain).To(ContainSubstring("Limit(3)"))
		Expect(explain).To(ContainSubstring("Filter(price > 50)"))
		Expect(explain).To(ContainSubstring("Scan(Order)"))
	})
})
