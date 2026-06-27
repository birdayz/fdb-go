package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// populate20Orders saves 20 orders (IDs 0-19) into the given subspace and returns the metadata.
func populate20Orders(ctx context.Context, metaData *RecordMetaData, ks subspace.Subspace) {
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := range int64(20) {
			order := &gen.Order{
				OrderId: proto.Int64(i),
				Price:   proto.Int32(int32(i * 10)),
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("ContinuationToken", func() {
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

	It("ResumeAfterExactlyN", func() {
		ks := specSubspace()
		populate20Orders(ctx, metaData, ks)

		// Scan 5 records, get continuation, resume and get the rest
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(5),
				CursorStreamingMode: StreamingModeIterator,
			}

			// First batch: records 0-4
			cursor := store.ScanRecords(nil, props)
			var firstBatch []int64
			var continuation RecordCursorContinuation
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					continuation = result.GetContinuation()
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				firstBatch = append(firstBatch, order.GetOrderId())
			}
			_ = cursor.Close()

			Expect(firstBatch).To(HaveLen(5))
			for i, id := range firstBatch {
				Expect(id).To(Equal(int64(i)))
			}

			Expect(continuation).NotTo(BeNil())
			Expect(continuation.IsEnd()).To(BeFalse())

			// Second batch: records 5-9
			contBytes, err := continuation.ToBytes()
			Expect(err).NotTo(HaveOccurred())
			cursor2 := store.ScanRecords(contBytes, props)
			var secondBatch []int64
			for {
				result, err := cursor2.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				secondBatch = append(secondBatch, order.GetOrderId())
			}
			_ = cursor2.Close()

			Expect(secondBatch).To(HaveLen(5))
			for i, id := range secondBatch {
				Expect(id).To(Equal(int64(i + 5)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContinuationAcrossTransactions", func() {
		ks := specSubspace()
		populate20Orders(ctx, metaData, ks)

		// Simulate cross-transaction continuation: scan 7 in tx1, resume in tx2
		type batchResult struct {
			ids          []int64
			continuation []byte
		}

		// Transaction 1: read first 7
		res1, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(7),
				CursorStreamingMode: StreamingModeIterator,
			}

			cursor := store.ScanRecords(nil, props)
			defer func() { _ = cursor.Close() }()

			var ids []int64
			var lastCont RecordCursorContinuation
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !result.HasNext() {
					lastCont = result.GetContinuation()
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				ids = append(ids, order.GetOrderId())
			}

			var contBytes []byte
			if lastCont != nil && !lastCont.IsEnd() {
				contBytes, err = lastCont.ToBytes()
				Expect(err).NotTo(HaveOccurred())
			}
			return &batchResult{ids: ids, continuation: contBytes}, nil
		})
		Expect(err).NotTo(HaveOccurred())

		batch1 := res1.(*batchResult)
		Expect(batch1.ids).To(HaveLen(7))
		Expect(batch1.continuation).NotTo(BeNil())

		// Transaction 2: resume with continuation from tx1
		res2, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Use unlimited scan to get all remaining
			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(0),
				CursorStreamingMode: StreamingModeIterator,
			}

			cursor := store.ScanRecords(batch1.continuation, props)
			defer func() { _ = cursor.Close() }()

			var ids []int64
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !result.HasNext() {
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				ids = append(ids, order.GetOrderId())
			}

			return &batchResult{ids: ids}, nil
		})
		Expect(err).NotTo(HaveOccurred())

		batch2 := res2.(*batchResult)
		Expect(batch2.ids).To(HaveLen(13))
		for i, id := range batch2.ids {
			Expect(id).To(Equal(int64(i + 7)))
		}
	})

	It("FullScanInBatches", func() {
		ks := specSubspace()
		populate20Orders(ctx, metaData, ks)

		// Scan all 20 records in batches of 3, verifying every ID
		var allIDs []int64
		var continuation []byte
		batchCount := 0

		for {
			batchCount++
			res, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				props := ScanProperties{
					ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(3),
					CursorStreamingMode: StreamingModeIterator,
				}

				cursor := store.ScanRecords(continuation, props)
				defer func() { _ = cursor.Close() }()

				var ids []int64
				var lastCont RecordCursorContinuation
				for {
					result, err := cursor.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !result.HasNext() {
						lastCont = result.GetContinuation()
						break
					}
					order := result.GetValue().Record.(*gen.Order)
					ids = append(ids, order.GetOrderId())
				}

				var contBytes []byte
				if lastCont != nil && !lastCont.IsEnd() {
					contBytes, err = lastCont.ToBytes()
					Expect(err).NotTo(HaveOccurred())
				}
				return &struct {
					ids  []int64
					cont []byte
				}{ids, contBytes}, nil
			})
			Expect(err).NotTo(HaveOccurred())

			batch := res.(*struct {
				ids  []int64
				cont []byte
			})
			allIDs = append(allIDs, batch.ids...)
			continuation = batch.cont

			if len(batch.ids) == 0 || continuation == nil {
				break
			}

			Expect(batchCount).To(BeNumerically("<=", 10), "Too many batches, possible infinite loop")
		}

		Expect(allIDs).To(HaveLen(20))
		for i, id := range allIDs {
			Expect(id).To(Equal(int64(i)))
		}
		// 20 records / 3 per batch = 7 batches (6 full + 1 with 2 records)
		Expect(batchCount).To(BeNumerically(">=", 7))
	})

	It("ContinuationAtStoreBoundary", func() {
		ks := specSubspace()
		populate20Orders(ctx, metaData, ks)

		// Set limit to exactly the total count -- continuation should indicate end
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(20),
				CursorStreamingMode: StreamingModeIterator,
			}

			cursor := store.ScanRecords(nil, props)
			defer func() { _ = cursor.Close() }()

			count := 0
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					// When source is exhausted, continuation should be end
					Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
					cont := result.GetContinuation()
					if cont != nil {
						Expect(cont.IsEnd()).To(BeTrue())
					}
					break
				}
				count++
			}

			Expect(count).To(Equal(20))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LimitReachedContinuationNotEnd", func() {
		ks := specSubspace()
		populate20Orders(ctx, metaData, ks)

		// When we hit the row limit (not source exhausted), continuation should NOT be end
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(10),
				CursorStreamingMode: StreamingModeIterator,
			}

			cursor := store.ScanRecords(nil, props)
			defer func() { _ = cursor.Close() }()

			count := 0
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(ReturnLimitReached))
					cont := result.GetContinuation()
					Expect(cont).NotTo(BeNil())
					Expect(cont.IsEnd()).To(BeFalse())
					break
				}
				count++
			}

			Expect(count).To(Equal(10))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContinuationRoundTrip", func() {
		ks := specSubspace()
		populate20Orders(ctx, metaData, ks)

		// Get a continuation, serialize to bytes, use it to resume -- basic round-trip
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ScanProperties{
				ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(1),
				CursorStreamingMode: StreamingModeIterator,
			}

			// Read exactly 1 record
			cursor := store.ScanRecords(nil, props)
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())

			firstID := result.GetValue().Record.(*gen.Order).GetOrderId()
			Expect(firstID).To(Equal(int64(0)))

			cont := result.GetContinuation()
			_ = cursor.Close()

			// Serialize continuation to bytes
			contBytes, err := cont.ToBytes()
			Expect(err).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeNil())

			// Resume with the raw bytes
			cursor2 := store.ScanRecords(contBytes, props)
			result2, err := cursor2.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result2.HasNext()).To(BeTrue())

			secondID := result2.GetValue().Record.(*gen.Order).GetOrderId()
			Expect(secondID).To(Equal(int64(1)))
			_ = cursor2.Close()

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("EmptyStoreContinuation", func() {
		// Scan an empty store -- should get source exhausted with end continuation
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			defer func() { _ = cursor.Close() }()

			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))

			cont := result.GetContinuation()
			if cont != nil {
				Expect(cont.IsEnd()).To(BeTrue())
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
