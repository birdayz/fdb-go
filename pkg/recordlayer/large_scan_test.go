package recordlayer

import (
	"context"
	"fmt"

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
		metaData := builder.Build()
		testSubspace := specSubspace()

		GinkgoWriter.Printf("Testing sequential access order with %d records\n", numRecords)

		// Write records
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			for record := range cursor.Seq(ctx) {
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
		metaData := builder.Build()
		testSubspace := specSubspace()

		GinkgoWriter.Printf("Testing continuation mechanism with %d records\n", numRecords)

		// Write records
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			batchResult, batchErr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
				var lastContinuation RecordCursorContinuation

				// Read this batch using SeqWithContinuation to get proper continuation
				for record, cont := range cursor.SeqWithContinuation(ctx) {
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
					lastContinuation = cont
				}

				// Return batch results as a map for easier type assertion
				result := map[string]interface{}{
					"count":        batchRecordCount,
					"continuation": []byte(nil),
				}
				if lastContinuation != nil && !lastContinuation.IsEnd() {
					result["continuation"] = lastContinuation.ToBytes()
				}

				return result, nil
			})
			Expect(batchErr).NotTo(HaveOccurred())

			// Extract results
			resultMap := batchResult.(map[string]interface{})

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
