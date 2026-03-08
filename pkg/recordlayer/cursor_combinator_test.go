package recordlayer

import (
	"context"
	"slices"

	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// populate10Orders saves 10 orders (IDs 1-10, prices 10-100) into the given subspace.
func populate10Orders(ctx context.Context, metaData *RecordMetaData) {
	ks := specSubspace()
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := range int64(10) {
			order := &gen.Order{
				OrderId: proto.Int64(i + 1),
				Price:   proto.Int32(int32((i + 1) * 10)),
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("CursorCombinators", func() {
	var (
		metaData *RecordMetaData
		ctx      context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		var buildErr error
		metaData, buildErr = builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("FilterEliminatesAll", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			// Filter where price > 1000 -- no such records
			filtered := Filter(
				cursor.Seq(ctx),
				func(rec *FDBStoredRecord[proto.Message]) bool {
					return rec.Record.(*gen.Order).GetPrice() > 1000
				},
			)

			results := slices.Collect(filtered)
			Expect(results).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ChainedFilterMapLimit", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())

			// Filter: price > 30 (orders 4-10)
			// Map: extract order ID
			// Limit: take 3
			chain := Limit(
				Map(
					Filter(
						cursor.Seq(ctx),
						func(rec *FDBStoredRecord[proto.Message]) bool {
							return rec.Record.(*gen.Order).GetPrice() > 30
						},
					),
					func(rec *FDBStoredRecord[proto.Message]) int64 {
						return rec.Record.(*gen.Order).GetOrderId()
					},
				),
				3,
			)

			ids := slices.Collect(chain)
			Expect(ids).To(Equal([]int64{4, 5, 6}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LimitZero", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			limited := Limit(cursor.Seq(ctx), 0)
			results := slices.Collect(limited)
			Expect(results).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Filter2EmptyResult", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			filtered := Filter2(
				cursor.Seq2(ctx),
				func(rec *FDBStoredRecord[proto.Message]) bool {
					return false // eliminate everything
				},
			)

			count := 0
			for _, err := range filtered {
				Expect(err).NotTo(HaveOccurred())
				count++
			}
			Expect(count).To(Equal(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ForEachAndAsList", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Test AsList
			cursor := store.ScanRecords(nil, ForwardScan())
			records, err := AsList(ctx, cursor)
			if err != nil {
				return nil, err
			}
			Expect(records).To(HaveLen(10))

			// Test ForEach
			cursor2 := store.ScanRecords(nil, ForwardScan())
			var sum int32
			err = ForEach(ctx, cursor2, func(rec *FDBStoredRecord[proto.Message]) error {
				sum += rec.Record.(*gen.Order).GetPrice()
				return nil
			})
			if err != nil {
				return nil, err
			}
			// Sum of 10+20+...+100 = 550
			Expect(sum).To(Equal(int32(550)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
