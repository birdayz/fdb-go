package recordlayer

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("LargeScanSequentialAccess", func() {
	It("reads 10K records in correct order", func() {
		ctx := context.Background()

		const numRecords = 10_000

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		testSubspace := specSubspace()

		GinkgoWriter.Printf("Testing sequential access order with %d records\n", numRecords)

		// Write records
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(testSubspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			for i := 0; i < numRecords; i++ {
				record := &gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i)),
					Flower: &gen.Flower{
						Type:  proto.String(fmt.Sprintf("sequential_%d", i)),
						Color: gen.Color_RED.Enum(),
					},
				}

				_, err := store.SaveRecord(record)
				if err != nil {
					return nil, fmt.Errorf("failed to save record %d: %w", i, err)
				}
			}

			GinkgoWriter.Printf("Successfully wrote %d records\n", numRecords)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Read back and verify order
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(testSubspace).
				Open()
			if err != nil {
				return nil, err
			}

			// Create scan properties with no limits
			unlimitedScan := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					ReturnedRowLimit: 0, // No limit
				},
				Reverse:             false,
				CursorStreamingMode: StreamingModeIterator,
			}
			cursor := store.ScanRecords(nil, unlimitedScan)
			defer func() { _ = cursor.Close() }()

			expectedID := int64(0)
			recordCount := 0
			for record, iterErr := range Seq2(cursor, ctx) {
				Expect(iterErr).NotTo(HaveOccurred())
				order, ok := record.Record.(*gen.Order)
				if !ok {
					return nil, fmt.Errorf("unexpected record type: %T", record.Record)
				}

				if order.GetOrderId() != expectedID {
					return nil, fmt.Errorf("records out of order: got %d, expected %d", order.GetOrderId(), expectedID)
				}

				expectedID++
				recordCount++
			}

			GinkgoWriter.Printf("Read %d records during scan\n", recordCount)

			Expect(expectedID).To(Equal(int64(numRecords)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		GinkgoWriter.Println("Sequential access test passed - records read in correct order")
	})
})

var _ = Describe("BasicContinuation", func() {
	It("reads 1K records in batches of 50", func() {
		ctx := context.Background()

		const numRecords = 1000
		const batchSize = 50

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		testSubspace := specSubspace()

		GinkgoWriter.Printf("Testing continuation mechanism with %d records\n", numRecords)

		// Write records
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(testSubspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			for i := 0; i < numRecords; i++ {
				record := &gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i * 10)),
					Flower: &gen.Flower{
						Type:  proto.String(fmt.Sprintf("flower_%d", i)),
						Color: gen.Color_BLUE.Enum(),
					},
				}

				_, err := store.SaveRecord(record)
				if err != nil {
					return nil, fmt.Errorf("failed to save record %d: %w", i, err)
				}
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Read back using limited batches across multiple transactions to test continuation
		var totalRecordsRead int
		var continuation []byte
		batchCount := 0

		// Create scan properties with row limit to force batching
		scanProps := ScanProperties{
			ExecuteProperties: ExecuteProperties{
				ReturnedRowLimit: batchSize,
			},
			Reverse:             false,
			CursorStreamingMode: StreamingModeSmall,
		}

		for {
			batchCount++

			// Each batch runs in its own transaction
			batchResult, batchErr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(metaData).
					SetSubspace(testSubspace).
					Open()
				if err != nil {
					return nil, err
				}

				cursor := store.ScanRecords(continuation, scanProps)
				defer func() { _ = cursor.Close() }()

				batchRecordCount := 0

				// Read this batch via AsListWithContinuation to get the final
				// (resumable) continuation bytes — nil when the source is exhausted.
				batch, contBytes, lErr := AsListWithContinuation(ctx, cursor)
				if lErr != nil {
					return nil, lErr
				}
				for _, record := range batch {
					order, ok := record.Record.(*gen.Order)
					if !ok {
						return nil, fmt.Errorf("unexpected record type: %T", record.Record)
					}

					expectedID := int64(totalRecordsRead + batchRecordCount)
					if order.GetOrderId() != expectedID {
						return nil, fmt.Errorf("record ID mismatch: got %d, expected %d", order.GetOrderId(), expectedID)
					}

					// Validate content
					expectedPrice := int32(expectedID * 10)
					if order.GetPrice() != expectedPrice {
						return nil, fmt.Errorf("price mismatch for record %d: got %d, expected %d", expectedID, order.GetPrice(), expectedPrice)
					}

					expectedFlowerType := fmt.Sprintf("flower_%d", expectedID)
					if order.GetFlower().GetType() != expectedFlowerType {
						return nil, fmt.Errorf("flower type mismatch for record %d: got %s, expected %s", expectedID, order.GetFlower().GetType(), expectedFlowerType)
					}

					if order.GetFlower().GetColor() != gen.Color_BLUE {
						return nil, fmt.Errorf("flower color mismatch for record %d: got %v, expected BLUE", expectedID, order.GetFlower().GetColor())
					}

					batchRecordCount++
				}

				// Return batch results as a map for easier type assertion
				result := map[string]any{
					"count":        batchRecordCount,
					"continuation": contBytes,
				}

				return result, nil
			})
			Expect(batchErr).NotTo(HaveOccurred())

			// Extract results
			resultMap := batchResult.(map[string]any)

			recordsThisBatch := resultMap["count"].(int)
			totalRecordsRead += recordsThisBatch
			continuation = resultMap["continuation"].([]byte)

			GinkgoWriter.Printf("Batch %d: read %d records (total: %d)\n", batchCount, recordsThisBatch, totalRecordsRead)

			// Check if we're done
			if recordsThisBatch == 0 || totalRecordsRead >= numRecords || len(continuation) == 0 {
				break
			}

			// Safety check to prevent infinite loops
			if batchCount > (numRecords/batchSize)+5 {
				Fail("Too many batches, possible infinite loop")
			}
		}

		Expect(totalRecordsRead).To(Equal(numRecords))
		Expect(batchCount).To(BeNumerically(">", 1))

		GinkgoWriter.Printf("Read %d records in %d batches\n", totalRecordsRead, batchCount)
	})
})

var _ = Describe("TimeLimitScan", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("stops record scan when time limit is reached", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save 100 records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 100; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				_, err = store.SaveRecord(order)
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Scan with a very short time limit (1 nanosecond).
		// This should return at least 1 record (free initial pass) then stop.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ForwardScan()
			props.ExecuteProperties.TimeLimit = 1 * time.Nanosecond
			cursor := store.ScanRecords(nil, props)
			defer cursor.Close()

			var count int
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(TimeLimitReached))
					break
				}
				count++
			}

			// Should have read at least 1 (free initial pass) but fewer than 100.
			Expect(count).To(BeNumerically(">=", 1))
			Expect(count).To(BeNumerically("<", 100))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("stops index scan when time limit is reached", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save 100 records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 100; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				_, err = store.SaveRecord(order)
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Scan index with 1ns time limit.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			props := ForwardScan()
			props.ExecuteProperties.TimeLimit = 1 * time.Nanosecond
			cursor := store.ScanIndex(priceIndex, TupleRangeAll, nil, props)
			defer cursor.Close()

			var count int
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(TimeLimitReached))
					break
				}
				count++
			}

			Expect(count).To(BeNumerically(">=", 1))
			Expect(count).To(BeNumerically("<", 100))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("allows resumption with continuation after time limit", func() {
		ks := specSubspace()

		builder := baseMetaData()
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save 50 records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 50; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				_, err = store.SaveRecord(order)
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Scan in multiple batches using time limit continuations.
		var totalRecords int
		var continuation []byte
		batches := 0

		for batches < 100 { // Safety limit
			var batchCount int
			var sourceExhausted bool

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				props := ForwardScan()
				props.ExecuteProperties.TimeLimit = 1 * time.Nanosecond
				cursor := store.ScanRecords(continuation, props)
				defer cursor.Close()

				for {
					result, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					if !result.HasNext() {
						if result.GetNoNextReason() == SourceExhausted {
							sourceExhausted = true
						}
						cont := result.GetContinuation()
						if cont != nil {
							var contErr error
							continuation, contErr = cont.ToBytes()
							Expect(contErr).NotTo(HaveOccurred())
						}
						break
					}
					batchCount++
					cont := result.GetContinuation()
					if cont != nil {
						var contErr error
						continuation, contErr = cont.ToBytes()
						Expect(contErr).NotTo(HaveOccurred())
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			totalRecords += batchCount
			batches++

			if sourceExhausted {
				break
			}
		}

		Expect(totalRecords).To(Equal(50))
		Expect(batches).To(BeNumerically(">", 1))
	})

	It("does not enforce time limit when TimeLimit is zero", func() {
		ks := specSubspace()

		builder := baseMetaData()
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 10; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				_, err = store.SaveRecord(order)
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Scan without time limit — should get all records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			Expect(entries).To(HaveLen(10))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
