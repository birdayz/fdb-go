package recordlayer

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("MillionRecordScan", func() {
	It("scans 1M records across multiple transactions", Serial, Label("manual"), func() {
		if os.Getenv("RUN_MILLION_RECORD_TEST") == "" {
			Skip("set RUN_MILLION_RECORD_TEST=1 to run")
		}
		ctx := context.Background()

		const numRecords = 1_000_000
		const writeBatchSize = 2_000
		const scanBatchSize = 10_000

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		testSubspace := specSubspace()

		GinkgoWriter.Printf("Starting 1M record test...\n")
		overallStart := time.Now()

		// Step 1: Write 1M records in batches to avoid transaction timeouts
		GinkgoWriter.Printf("Writing %d records in batches of %d...\n", numRecords, writeBatchSize)
		writeStart := time.Now()

		writtenRecords := 0
		for batch := 0; batch < numRecords/writeBatchSize; batch++ {
			batchStart := time.Now()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(metaData).
					SetSubspace(testSubspace).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Write this batch
				startIdx := batch * writeBatchSize
				endIdx := (batch + 1) * writeBatchSize
				if endIdx > numRecords {
					endIdx = numRecords
				}

				for i := startIdx; i < endIdx; i++ {
					record := &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i % 1000)),
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

				return endIdx - startIdx, nil
			})
			Expect(err).NotTo(HaveOccurred())

			batchRecords := writeBatchSize
			if batch == numRecords/writeBatchSize-1 {
				batchRecords = numRecords % writeBatchSize
				if batchRecords == 0 {
					batchRecords = writeBatchSize
				}
			}

			writtenRecords += batchRecords
			batchTime := time.Since(batchStart)

			if batch%10 == 0 || batch == numRecords/writeBatchSize-1 {
				GinkgoWriter.Printf("  Batch %d/%d: wrote %d records in %v (total: %d, %.1f%%)\n",
					batch+1, numRecords/writeBatchSize, batchRecords, batchTime,
					writtenRecords, float64(writtenRecords)*100/float64(numRecords))
			}
		}

		writeTime := time.Since(writeStart)
		GinkgoWriter.Printf("Finished writing %d records in %v (%.0f records/sec)\n",
			writtenRecords, writeTime, float64(writtenRecords)/writeTime.Seconds())

		// Step 2: Read back all records using continuation-based scanning
		GinkgoWriter.Printf("Reading back all %d records using cursor scan...\n", numRecords)
		readStart := time.Now()

		var totalRecordsRead int
		var continuation []byte
		transactionCount := 0

		// Create scan properties with limits to force multiple transactions
		scanProps := ScanProperties{
			ExecuteProperties: ExecuteProperties{
				ReturnedRowLimit: scanBatchSize,
				TimeLimit:        3 * time.Second,
			},
			Reverse:             false,
			CursorStreamingMode: StreamingModeSmall,
		}

		for {
			transactionCount++
			batchStart := time.Now()

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
				var lastContinuation RecordCursorContinuation

				// Read this batch using SeqWithContinuation to get proper continuation
				for record, cont := range SeqWithContinuation(cursor, ctx) {
					order, ok := record.Record.(*gen.Order)
					if !ok {
						return nil, fmt.Errorf("unexpected record type: %T", record.Record)
					}

					expectedID := int64(totalRecordsRead + batchRecordCount)
					if order.GetOrderId() != expectedID {
						return nil, fmt.Errorf("record ID mismatch: got %d, expected %d", order.GetOrderId(), expectedID)
					}

					// Validate content
					expectedPrice := int32(expectedID % 1000)
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

				// Return batch results
				result := map[string]any{
					"count":        batchRecordCount,
					"continuation": []byte(nil),
				}
				if lastContinuation != nil && !lastContinuation.IsEnd() {
					contBytes, contErr := lastContinuation.ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					result["continuation"] = contBytes
				}

				return result, nil
			})
			Expect(batchErr).NotTo(HaveOccurred())

			// Extract results
			resultMap := batchResult.(map[string]any)
			recordsThisBatch := resultMap["count"].(int)
			totalRecordsRead += recordsThisBatch
			continuation = resultMap["continuation"].([]byte)

			batchTime := time.Since(batchStart)

			if transactionCount%10 == 0 || recordsThisBatch == 0 {
				GinkgoWriter.Printf("  Transaction %d: read %d records in %v (total: %d, %.1f%%)\n",
					transactionCount, recordsThisBatch, batchTime, totalRecordsRead,
					float64(totalRecordsRead)*100/float64(numRecords))
			}

			// Check if we're done
			if recordsThisBatch == 0 || totalRecordsRead >= numRecords || len(continuation) == 0 {
				break
			}

			// Safety check to prevent infinite loops
			if transactionCount > (numRecords/scanBatchSize)+10 {
				Fail(fmt.Sprintf("Too many transactions, possible infinite loop (tx: %d)", transactionCount))
			}
		}

		readTime := time.Since(readStart)
		totalTime := time.Since(overallStart)

		// Final verification and statistics
		GinkgoWriter.Printf("\nMILLION RECORD TEST RESULTS:\n")
		GinkgoWriter.Printf("  Total records written: %d\n", writtenRecords)
		GinkgoWriter.Printf("  Total records read: %d\n", totalRecordsRead)
		GinkgoWriter.Printf("  Write performance: %v (%.0f records/sec)\n", writeTime, float64(writtenRecords)/writeTime.Seconds())
		GinkgoWriter.Printf("  Read performance: %v (%.0f records/sec)\n", readTime, float64(totalRecordsRead)/readTime.Seconds())
		GinkgoWriter.Printf("  Total test time: %v\n", totalTime)
		GinkgoWriter.Printf("  Write transactions: %d\n", numRecords/writeBatchSize)
		GinkgoWriter.Printf("  Read transactions: %d\n", transactionCount)
		GinkgoWriter.Printf("  Average records per read transaction: %.1f\n", float64(totalRecordsRead)/float64(transactionCount))

		// Assertions
		Expect(writtenRecords).To(Equal(numRecords))
		Expect(totalRecordsRead).To(Equal(numRecords))
		Expect(transactionCount).To(BeNumerically(">", 1))
		Expect(transactionCount).To(BeNumerically("<=", (numRecords/scanBatchSize)+10))

		GinkgoWriter.Println("MILLION RECORD TEST PASSED - Continuation mechanism handles large datasets")
	})
})

var _ = Describe("MillionRecordPerformance", func() {
	It("benchmarks 100K record write and read performance", Serial, Label("manual"), func() {
		const numRecords = 100_000

		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		testSubspace := specSubspace()

		start := time.Now()

		// Write phase - use batching to avoid transaction limits
		writeStart := time.Now()
		batchSize := 1000

		for batch := 0; batch < numRecords/batchSize; batch++ {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(metaData).
					SetSubspace(testSubspace).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}

				startIdx := batch * batchSize
				endIdx := (batch + 1) * batchSize
				if endIdx > numRecords {
					endIdx = numRecords
				}

				for i := startIdx; i < endIdx; i++ {
					record := &gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(int32(i % 100)),
						Flower: &gen.Flower{
							Type:  proto.String(fmt.Sprintf("perf_%d", i)),
							Color: gen.Color_RED.Enum(),
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
		}
		writeTime := time.Since(writeStart)

		// Read phase
		readStart := time.Now()
		readCount := 0
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(testSubspace).
				Open()
			if err != nil {
				return nil, err
			}

			unlimitedScan := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					ReturnedRowLimit: 0,
				},
				Reverse:             false,
				CursorStreamingMode: StreamingModeIterator,
			}
			cursor := store.ScanRecords(nil, unlimitedScan)
			defer func() { _ = cursor.Close() }()

			for _, iterErr := range Seq2(cursor, ctx) {
				Expect(iterErr).NotTo(HaveOccurred())
				readCount++
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		readTime := time.Since(readStart)
		totalTime := time.Since(start)

		// Performance metrics
		writeRate := float64(numRecords) / writeTime.Seconds()
		readRate := float64(readCount) / readTime.Seconds()

		GinkgoWriter.Printf("PERFORMANCE TEST RESULTS (%d records):\n", numRecords)
		GinkgoWriter.Printf("  Write: %v (%.0f records/sec)\n", writeTime, writeRate)
		GinkgoWriter.Printf("  Read: %v (%.0f records/sec)\n", readTime, readRate)
		GinkgoWriter.Printf("  Total: %v\n", totalTime)

		Expect(readCount).To(Equal(numRecords))

		// Performance assertions (adjust based on your environment)
		Expect(writeRate).To(BeNumerically(">", 1000.0))
		Expect(readRate).To(BeNumerically(">", 5000.0))
	})
})
