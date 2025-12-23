package recordlayer

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestLargeScanSequentialAccess tests that records are read in the correct order
func TestLargeScanSequentialAccess(t *testing.T) {
	ctx := context.Background()

	// Start FoundationDB testcontainer
	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("large_scan_sequential_test"),
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	// Initialize database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Get FDB database connection
	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	fdbDB := NewFDBDatabase(db)

	const numRecords = 10_000 // Reasonable test size
	
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaData := builder.Build()
	testSubspace := subspace.Sub(fmt.Sprintf("seq_test_%d", time.Now().UnixNano()))

	log.Printf("Testing sequential access order with %d records", numRecords)

	// Write records
	_, err = fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
		
		log.Printf("Successfully wrote %d records", numRecords)

		return nil, nil
	})
	
	if err != nil {
		t.Fatalf("Failed to write records: %v", err)
	}

	// Read back and verify order
	_, err = fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
			Reverse: false,
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
		
		log.Printf("Read %d records during scan", recordCount)

		assert.Equal(t, int64(numRecords), expectedID, "Should read all records in order")
		return nil, nil
	})

	if err != nil {
		t.Fatalf("Failed during sequential scan: %v", err)
	}

	log.Printf("✅ Sequential access test passed - records read in correct order!")
}

// TestBasicContinuation tests basic continuation mechanism with a smaller dataset
func TestBasicContinuation(t *testing.T) {
	ctx := context.Background()

	// Start FoundationDB testcontainer
	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("basic_continuation_test"),
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	// Initialize database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Get FDB database connection
	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	fdbDB := NewFDBDatabase(db)

	const numRecords = 1000
	const batchSize = 50
	
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaData := builder.Build()
	testSubspace := subspace.Sub(fmt.Sprintf("continuation_test_%d", time.Now().UnixNano()))

	log.Printf("Testing continuation mechanism with %d records", numRecords)

	// Write records
	_, err = fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
	
	if err != nil {
		t.Fatalf("Failed to write records: %v", err)
	}

	// Read back using limited batches across multiple transactions to test continuation
	var totalRecordsRead int
	var continuation []byte
	batchCount := 0

	// Create scan properties with row limit to force batching
	scanProps := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			ReturnedRowLimit: batchSize,
		},
		Reverse:               false,
		CursorStreamingMode: StreamingModeSmall,
	}

	for {
		batchCount++
		
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
		
		if batchErr != nil {
			t.Fatalf("Failed during batch %d: %v", batchCount, batchErr)
		}
		
		// Extract results
		resultMap := batchResult.(map[string]interface{})
		
		recordsThisBatch := resultMap["count"].(int)
		totalRecordsRead += recordsThisBatch
		continuation = resultMap["continuation"].([]byte)

		log.Printf("Batch %d: read %d records (total: %d)", batchCount, recordsThisBatch, totalRecordsRead)

		// Check if we're done
		if recordsThisBatch == 0 || totalRecordsRead >= numRecords || len(continuation) == 0 {
			break
		}

		// Safety check to prevent infinite loops
		if batchCount > (numRecords/batchSize)+5 {
			t.Fatalf("Too many batches, possible infinite loop")
		}
	}

	assert.Equal(t, numRecords, totalRecordsRead, "Should read back all written records")
	assert.Greater(t, batchCount, 1, "Should require multiple batches")
	
	log.Printf("✅ Read %d records in %d batches", totalRecordsRead, batchCount)

	if err != nil {
		t.Fatalf("Failed during continuation scan: %v", err)
	}

	log.Printf("✅ Basic continuation test passed!")
}