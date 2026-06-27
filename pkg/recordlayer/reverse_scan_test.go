package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// collectOrderIDs scans records and returns all order IDs in scan order.
func collectOrderIDs(store *FDBRecordStore, continuation []byte, props ScanProperties) []int64 {
	cursor := store.ScanRecords(continuation, props)
	defer func() { _ = cursor.Close() }()

	var ids []int64
	for {
		result, err := cursor.OnNext(context.Background())
		Expect(err).NotTo(HaveOccurred())
		if !result.HasNext() {
			break
		}
		order := result.GetValue().Record.(*gen.Order)
		ids = append(ids, *order.OrderId)
	}
	return ids
}

var _ = Describe("ReverseScan", func() {
	var (
		metaData *RecordMetaData
		ctx      context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		metaData, buildErr = builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("BasicReverseOrder", func() {
		ks := specSubspace()

		// Populate 5 orders
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			ids := collectOrderIDs(store, nil, ReverseScan())
			Expect(ids).To(Equal([]int64{5, 4, 3, 2, 1}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ReverseWithRowLimit", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(3),
				Reverse:             true,
				CursorStreamingMode: StreamingModeIterator,
			}
			ids := collectOrderIDs(store, nil, props)

			Expect(ids).To(HaveLen(3))
			Expect(ids).To(Equal([]int64{5, 4, 3}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ReverseWithRangeEndpoints", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Scan range [2, 4] inclusive in reverse
			cursor := store.ScanRecordsInRange(
				tuple.Tuple{int64(2)}, tuple.Tuple{int64(4)},
				EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
				nil, ReverseScan(),
			)
			defer func() { _ = cursor.Close() }()

			var ids []int64
			for {
				result, err := cursor.OnNext(context.Background())
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				ids = append(ids, *order.OrderId)
			}

			Expect(ids).To(Equal([]int64{4, 3, 2}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ForwardAndReverseSameData", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			fwd := collectOrderIDs(store, nil, ForwardScan())
			rev := collectOrderIDs(store, nil, ReverseScan())

			Expect(fwd).To(HaveLen(len(rev)))

			// Forward and reverse should be mirror images
			for i := range fwd {
				Expect(fwd[i]).To(Equal(rev[len(rev)-1-i]))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ReverseWithContinuation", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Scan 5 records in reverse in batches of 2, using continuation tokens.
		// Expected order: [5,4], [3,2], [1]
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			var allIDs []int64
			var continuation []byte

			for batch := 0; batch < 10; batch++ { // safety limit
				props := ScanProperties{
					ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(2),
					Reverse:             true,
					CursorStreamingMode: StreamingModeIterator,
				}

				cursor := store.ScanRecords(continuation, props)
				var batchIDs []int64

				for {
					result, err := cursor.OnNext(context.Background())
					Expect(err).NotTo(HaveOccurred())
					if !result.HasNext() {
						cont := result.GetContinuation()
						if cont.IsEnd() {
							continuation = nil
						} else {
							var contErr error
							continuation, contErr = cont.ToBytes()
							Expect(contErr).NotTo(HaveOccurred())
						}
						break
					}
					order := result.GetValue().Record.(*gen.Order)
					batchIDs = append(batchIDs, *order.OrderId)
				}
				_ = cursor.Close()

				allIDs = append(allIDs, batchIDs...)

				if continuation == nil {
					break
				}
			}

			Expect(allIDs).To(Equal([]int64{5, 4, 3, 2, 1}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ReverseEmptyRange", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Range [100, 200] -- no records exist here
			cursor := store.ScanRecordsInRange(
				tuple.Tuple{int64(100)}, tuple.Tuple{int64(200)},
				EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
				nil, ReverseScan(),
			)
			defer func() { _ = cursor.Close() }()

			result, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
