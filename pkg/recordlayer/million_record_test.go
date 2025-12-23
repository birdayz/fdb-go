package recordlayer

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// TestMillionRecordScan tests scanning 1M records across multiple transactions
// This test verifies that our continuation mechanism works correctly for very large datasets
// that definitely exceed FDB's 5-second transaction limit
func TestMillionRecordScan(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping 1M record test in short mode")
	}

	if !fdb.IsAPIVersionSelected() {
		fdb.MustAPIVersion(630)
	}

	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)

	const numRecords = 1_000_000    // Full 1M record test
	const writeBatchSize = 2_000   // Reasonable batch size with proper retry logic
	const scanBatchSize = 10_000   // Records per scan batch

	ctx := context.Background()
	
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaData := builder.Build()
	testSubspace := subspace.Sub(fmt.Sprintf("million_test_%d", time.Now().UnixNano()))

	log.Printf("🚀 Starting 1M record test...")
	overallStart := time.Now()

	// Step 1: Write 1M records in batches to avoid transaction timeouts
	log.Printf("📝 Writing %d records in batches of %d...", numRecords, writeBatchSize)
	writeStart := time.Now()
	
	writtenRecords := 0
	for batch := 0; batch < numRecords/writeBatchSize; batch++ {
		batchStart := time.Now()
		
		_, err := fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
					Price:   proto.Int32(int32(i % 1000)), // Keep prices reasonable
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
		
		if err != nil {
			t.Fatalf("Failed to write batch %d: %v", batch, err)
		}
		
		batchRecords := writeBatchSize
		if batch == numRecords/writeBatchSize - 1 {
			batchRecords = numRecords % writeBatchSize
			if batchRecords == 0 {
				batchRecords = writeBatchSize
			}
		}
		
		writtenRecords += batchRecords
		batchTime := time.Since(batchStart)
		
		if batch%10 == 0 || batch == numRecords/writeBatchSize-1 {
			log.Printf("  Batch %d/%d: wrote %d records in %v (total: %d, %.1f%%)", 
				batch+1, numRecords/writeBatchSize, batchRecords, batchTime, 
				writtenRecords, float64(writtenRecords)*100/float64(numRecords))
		}
	}

	writeTime := time.Since(writeStart)
	log.Printf("✅ Finished writing %d records in %v (%.0f records/sec)", 
		writtenRecords, writeTime, float64(writtenRecords)/writeTime.Seconds())

	// Step 2: Read back all records using continuation-based scanning
	log.Printf("📖 Reading back all %d records using cursor scan...", numRecords)
	readStart := time.Now()
	
	var totalRecordsRead int
	var continuation []byte
	transactionCount := 0

	// Create scan properties with limits to force multiple transactions
	scanProps := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			ReturnedRowLimit: scanBatchSize,
			TimeLimit:        3 * time.Second, // Force transaction boundaries
		},
		Reverse:               false,
		CursorStreamingMode: StreamingModeSmall,
	}

	for {
		transactionCount++
		batchStart := time.Now()
		
		// Each batch runs in its own transaction
		batchResult, batchErr := fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			result := map[string]interface{}{
				"count":        batchRecordCount,
				"continuation": []byte(nil),
			}
			if lastContinuation != nil && !lastContinuation.IsEnd() {
				result["continuation"] = lastContinuation.ToBytes()
			}
			
			return result, nil
		})
		
		if batchErr != nil {
			t.Fatalf("Failed during read batch %d: %v", transactionCount, batchErr)
		}
		
		// Extract results
		resultMap := batchResult.(map[string]interface{})
		recordsThisBatch := resultMap["count"].(int)
		totalRecordsRead += recordsThisBatch
		continuation = resultMap["continuation"].([]byte)

		batchTime := time.Since(batchStart)
		
		if transactionCount%10 == 0 || recordsThisBatch == 0 {
			log.Printf("  Transaction %d: read %d records in %v (total: %d, %.1f%%)", 
				transactionCount, recordsThisBatch, batchTime, totalRecordsRead,
				float64(totalRecordsRead)*100/float64(numRecords))
		}

		// Check if we're done
		if recordsThisBatch == 0 || totalRecordsRead >= numRecords || len(continuation) == 0 {
			break
		}

		// Safety check to prevent infinite loops
		if transactionCount > (numRecords/scanBatchSize)+10 {
			t.Fatalf("Too many transactions, possible infinite loop (tx: %d)", transactionCount)
		}
	}

	readTime := time.Since(readStart)
	totalTime := time.Since(overallStart)

	// Final verification and statistics
	log.Printf("")
	log.Printf("🎯 MILLION RECORD TEST RESULTS:")
	log.Printf("  Total records written: %d", writtenRecords)
	log.Printf("  Total records read: %d", totalRecordsRead)
	log.Printf("  Write performance: %v (%.0f records/sec)", writeTime, float64(writtenRecords)/writeTime.Seconds())
	log.Printf("  Read performance: %v (%.0f records/sec)", readTime, float64(totalRecordsRead)/readTime.Seconds())
	log.Printf("  Total test time: %v", totalTime)
	log.Printf("  Write transactions: %d", numRecords/writeBatchSize)
	log.Printf("  Read transactions: %d", transactionCount)
	log.Printf("  Average records per read transaction: %.1f", float64(totalRecordsRead)/float64(transactionCount))

	// Assertions
	assert.Equal(t, numRecords, writtenRecords, "Should write all intended records")
	assert.Equal(t, numRecords, totalRecordsRead, "Should read back all written records")
	assert.Greater(t, transactionCount, 1, "Should require multiple read transactions for 1M records")
	assert.LessOrEqual(t, transactionCount, (numRecords/scanBatchSize)+10, "Should not require excessive transactions")
	
	log.Printf("✅ MILLION RECORD TEST PASSED - Continuation mechanism handles large datasets!")
}

// TestMillionRecordPerformance is a lighter version that focuses on performance metrics
func TestMillionRecordPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	// This test is similar but focuses on performance benchmarking
	// It can be used to measure improvements over time
	
	const numRecords = 100_000 // Smaller for CI/quick runs
	
	if !fdb.IsAPIVersionSelected() {
		fdb.MustAPIVersion(630)
	}

	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)
	ctx := context.Background()
	
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaData := builder.Build()
	testSubspace := subspace.Sub(fmt.Sprintf("perf_test_%d", time.Now().UnixNano()))

	start := time.Now()

	// Write phase - use batching to avoid transaction limits
	writeStart := time.Now()
	batchSize := 1000 // Safe batch size
	
	for batch := 0; batch < numRecords/batchSize; batch++ {
		_, err := fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
		
		if err != nil {
			t.Fatalf("Failed to write batch %d: %v", batch, err)
		}
	}
	writeTime := time.Since(writeStart)

	// Read phase
	readStart := time.Now()
	readCount := 0
	_, err := fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
				ReturnedRowLimit: 0, // No limit
			},
			Reverse: false,
			CursorStreamingMode: StreamingModeIterator,
		}
		cursor := store.ScanRecords(nil, unlimitedScan)
		defer func() { _ = cursor.Close() }()

		for range cursor.Seq(ctx) {
			readCount++
		}

		return nil, nil
	})
	
	if err != nil {
		t.Fatalf("Failed to read records: %v", err)
	}
	readTime := time.Since(readStart)
	totalTime := time.Since(start)

	// Performance metrics
	writeRate := float64(numRecords) / writeTime.Seconds()
	readRate := float64(readCount) / readTime.Seconds()
	
	log.Printf("📊 PERFORMANCE TEST RESULTS (%d records):", numRecords)
	log.Printf("  Write: %v (%.0f records/sec)", writeTime, writeRate)
	log.Printf("  Read: %v (%.0f records/sec)", readTime, readRate)
	log.Printf("  Total: %v", totalTime)
	
	assert.Equal(t, numRecords, readCount, "Should read all records")
	
	// Performance assertions (adjust based on your environment)
	assert.Greater(t, writeRate, 1000.0, "Write rate should be > 1000 records/sec")
	assert.Greater(t, readRate, 5000.0, "Read rate should be > 5000 records/sec")
}