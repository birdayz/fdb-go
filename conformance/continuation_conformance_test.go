package conformance

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestContinuationConformance tests that Go and Java can exchange continuation tokens
// This is a critical test for cross-language compatibility
func TestContinuationConformance(t *testing.T) {
	if !fdb.IsAPIVersionSelected() {
		fdb.MustAPIVersion(630)
	}

	db := fdb.MustOpenDefault()
	fdbDB := recordlayer.NewFDBDatabase(db)
	
	// Use a unique subspace for this test
	testSubspace := subspace.Sub(fmt.Sprintf("continuation_conformance_%d", time.Now().UnixNano()))
	
	ctx := context.Background()
	
	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	metaData := builder.Build()

	// Step 1: Write records using Go (1000 records)
	t.Log("Step 1: Writing 1000 records using Go...")
	_, err := fdbDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metaData).
			SetSubspace(testSubspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		for i := 0; i < 1000; i++ {
			order := &gen.Order{
				OrderId: proto.Int64(int64(i)),
				Price:   proto.Int32(int32(i * 10 + i % 7)), // Unique pattern
				Flower: &gen.Flower{
					Type:  proto.String(fmt.Sprintf("flower_%04d", i)),
					Color: gen.Color(i%3 + 1).Enum(), // Rotate through RED, YELLOW, BLUE
				},
			}

			_, err := store.SaveRecord(order)
			if err != nil {
				return nil, fmt.Errorf("failed to save record %d: %w", i, err)
			}
		}

		return nil, nil
	})
	require.NoError(t, err, "Failed to write records with Go")

	// Step 2: Read first 300 records with Go and get continuation
	t.Log("Step 2: Reading first 300 records with Go...")
	var goContinuation1 []byte
	goRecords1 := make([]*gen.Order, 0, 300)
	
	_, err = fdbDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metaData).
			SetSubspace(testSubspace).
			Open()
		if err != nil {
			return nil, err
		}

		scanProps := recordlayer.ScanProperties{
			ExecuteProperties: recordlayer.ExecuteProperties{
				ReturnedRowLimit: 300,
			},
			Reverse:             false,
			CursorStreamingMode: recordlayer.StreamingModeSmall,
		}

		cursor := store.ScanRecords(nil, scanProps)
		defer cursor.Close()

		var lastContinuation recordlayer.RecordCursorContinuation
		for record, cont := range cursor.SeqWithContinuation(ctx) {
			order, ok := record.Record.(*gen.Order)
			if !ok {
				return nil, fmt.Errorf("unexpected record type: %T", record.Record)
			}
			goRecords1 = append(goRecords1, order)
			lastContinuation = cont
		}

		if lastContinuation != nil && !lastContinuation.IsEnd() {
			goContinuation1 = lastContinuation.ToBytes()
		}

		return nil, nil
	})
	require.NoError(t, err, "Failed to read first batch with Go")
	assert.Len(t, goRecords1, 300, "Should read exactly 300 records")
	assert.NotNil(t, goContinuation1, "Should have a continuation token")

	// Log the continuation for debugging
	t.Logf("Go continuation 1 (hex): %x", goContinuation1)
	if len(goContinuation1) > 0 {
		parsedTuple, err := tuple.Unpack(goContinuation1)
		if err == nil {
			t.Logf("Go continuation 1 (tuple): %v", parsedTuple)
		} else {
			t.Logf("Go continuation 1 (raw bytes): %v", goContinuation1)
		}
	}

	// Step 3: Continue reading next 300 records with Java
	t.Log("Step 3: Reading next 300 records (300-599) with Java using Go's continuation...")
	javaContinuation, javaRecordCount, err := runJavaContinuationRead(
		testSubspace.Bytes(),
		goContinuation1,
		300, // limit
		300, // expected start ID
		599, // expected end ID
	)
	require.NoError(t, err, "Failed to read with Java")
	assert.Equal(t, 300, javaRecordCount, "Java should read exactly 300 records")
	assert.NotNil(t, javaContinuation, "Java should return a continuation")

	t.Logf("Java continuation (hex): %x", javaContinuation)

	// Step 4: Continue reading next 300 records with Go using Java's continuation
	t.Log("Step 4: Reading next 300 records (600-899) with Go using Java's continuation...")
	var goContinuation2 []byte
	goRecords2 := make([]*gen.Order, 0, 300)
	
	_, err = fdbDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metaData).
			SetSubspace(testSubspace).
			Open()
		if err != nil {
			return nil, err
		}

		scanProps := recordlayer.ScanProperties{
			ExecuteProperties: recordlayer.ExecuteProperties{
				ReturnedRowLimit: 300,
			},
			Reverse:             false,
			CursorStreamingMode: recordlayer.StreamingModeSmall,
		}

		cursor := store.ScanRecords(javaContinuation, scanProps)
		defer cursor.Close()

		var lastContinuation recordlayer.RecordCursorContinuation
		for record, cont := range cursor.SeqWithContinuation(ctx) {
			order, ok := record.Record.(*gen.Order)
			if !ok {
				return nil, fmt.Errorf("unexpected record type: %T", record.Record)
			}
			goRecords2 = append(goRecords2, order)
			lastContinuation = cont
		}

		if lastContinuation != nil && !lastContinuation.IsEnd() {
			goContinuation2 = lastContinuation.ToBytes()
		}

		return nil, nil
	})
	require.NoError(t, err, "Failed to read second Go batch")
	assert.Len(t, goRecords2, 300, "Go should read exactly 300 records")
	assert.NotNil(t, goContinuation2, "Go should have another continuation")

	// Validate Go records 2
	for i, order := range goRecords2 {
		expectedID := int64(600 + i)
		assert.Equal(t, expectedID, order.GetOrderId(), "Record ID mismatch at index %d", i)
		validateOrderContent(t, order, expectedID)
	}

	// Step 5: Read final 100 records with Java using Go's continuation
	t.Log("Step 5: Reading final 100 records (900-999) with Java using Go's continuation...")
	finalJavaContinuation, finalJavaCount, err := runJavaContinuationRead(
		testSubspace.Bytes(),
		goContinuation2,
		100, // limit
		900, // expected start ID
		999, // expected end ID
	)
	require.NoError(t, err, "Failed to read final batch with Java")
	assert.Equal(t, 100, finalJavaCount, "Java should read exactly 100 records")
	// Test: If Java returned a continuation at the end, verify it returns no more records
	if finalJavaContinuation != nil {
		t.Logf("Testing Java continuation at end: %x", finalJavaContinuation)
		
		// Test with Java - should return 0 records or fail gracefully
		_, javaTestCount, err := runJavaContinuationRead(
			testSubspace.Bytes(),
			finalJavaContinuation,
			100, // limit 
			-1, // expected start (unused since no records expected)
			-1, // expected end (unused since no records expected)
		)
		// Java might fail with an error or return 0 records - both are acceptable
		if err != nil {
			t.Logf("Java failed when using end continuation (acceptable): %v", err)
		} else {
			assert.Equal(t, 0, javaTestCount, "Java should return 0 records when using end continuation")
			t.Logf("Java returned 0 records when using end continuation")
		}
		
		// Test with Go - should also return 0 records
		var goTestRecords []*gen.Order
		var goTestContinuation []byte
		_, err = fdbDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(testSubspace).
				Open()
			if err != nil {
				return nil, err
			}
			
			// Scan with continuation to test if it returns no records
			scanProps := recordlayer.ScanProperties{
				ExecuteProperties: recordlayer.ExecuteProperties{
					ReturnedRowLimit: 100,
				},
				Reverse:             false,
				CursorStreamingMode: recordlayer.StreamingModeIterator,
			}
			
			cursor := store.ScanRecords(finalJavaContinuation, scanProps)
			defer cursor.Close()
			
			var lastContinuation recordlayer.RecordCursorContinuation
			for record, cont := range cursor.SeqWithContinuation(ctx) {
				order, ok := record.Record.(*gen.Order)
				if !ok {
					return nil, fmt.Errorf("unexpected record type: %T", record.Record)
				}
				goTestRecords = append(goTestRecords, order)
				lastContinuation = cont
			}
			
			if lastContinuation != nil {
				goTestContinuation = lastContinuation.ToBytes()
			}
			return nil, err
		})
		require.NoError(t, err, "Go should handle end continuation gracefully")
		assert.Len(t, goTestRecords, 0, "Go should return 0 records when using end continuation")
		assert.Nil(t, goTestContinuation, "Go should return nil continuation when no more records")
		t.Logf("Go correctly returned %d records and continuation: %v", len(goTestRecords), goTestContinuation)
	}

	// Step 6: Validate all Go records we read
	t.Log("Step 6: Validating all records read by Go...")
	
	// Validate first Go batch (0-299)
	for i, order := range goRecords1 {
		expectedID := int64(i)
		assert.Equal(t, expectedID, order.GetOrderId(), "Record ID mismatch at index %d", i)
		validateOrderContent(t, order, expectedID)
	}

	// Step 7: Full scan to verify total count
	t.Log("Step 7: Full scan to verify all 1000 records are intact...")
	totalCount := 0
	_, err = fdbDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metaData).
			SetSubspace(testSubspace).
			Open()
		if err != nil {
			return nil, err
		}

		cursor := store.ScanRecords(nil, recordlayer.ScanProperties{
			ExecuteProperties: recordlayer.ExecuteProperties{
				ReturnedRowLimit: 0, // No limit
			},
			Reverse:             false,
			CursorStreamingMode: recordlayer.StreamingModeIterator,
		})
		defer cursor.Close()

		for record := range cursor.Seq(ctx) {
			order := record.Record.(*gen.Order)
			validateOrderContent(t, order, order.GetOrderId())
			totalCount++
		}

		return nil, nil
	})
	require.NoError(t, err, "Failed to do full scan")
	assert.Equal(t, 1000, totalCount, "Should have exactly 1000 records total")

	t.Log("✅ Continuation conformance test PASSED! Go and Java can exchange continuation tokens seamlessly.")
}

// validateOrderContent validates that an order has the expected content based on its ID
func validateOrderContent(t *testing.T, order *gen.Order, expectedID int64) {
	expectedPrice := int32(expectedID*10 + expectedID%7)
	assert.Equal(t, expectedPrice, order.GetPrice(), "Price mismatch for record %d", expectedID)
	
	expectedFlowerType := fmt.Sprintf("flower_%04d", expectedID)
	assert.Equal(t, expectedFlowerType, order.GetFlower().GetType(), "Flower type mismatch for record %d", expectedID)
	
	expectedColor := gen.Color(expectedID%3 + 1) // RED, YELLOW, BLUE rotation
	assert.Equal(t, expectedColor, order.GetFlower().GetColor(), "Flower color mismatch for record %d", expectedID)
}

// runJavaContinuationRead executes Java code to read records with a continuation token
func runJavaContinuationRead(subspaceBytes []byte, continuation []byte, limit int, expectedStartID, expectedEndID int64) ([]byte, int, error) {
	// Convert bytes to hex strings for passing to Java
	subspaceHex := fmt.Sprintf("%x", subspaceBytes)
	continuationHex := ""
	if continuation != nil {
		continuationHex = fmt.Sprintf("%x", continuation)
	}

	// Run Java test that reads with continuation and validates content using gradle
	cmd := exec.Command(
		"./gradlew", "runConformance", "--no-daemon", "-q",
		"--args", fmt.Sprintf("%s %s %s %s %s",
			subspaceHex,
			continuationHex,
			strconv.Itoa(limit),
			strconv.FormatInt(expectedStartID, 10),
			strconv.FormatInt(expectedEndID, 10)),
	)
	// Set working directory to the Java conformance directory
	cmd.Dir = "java"

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, 0, fmt.Errorf("Java execution failed: %v\nOutput: %s", err, output)
	}

	// Parse output
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return nil, 0, fmt.Errorf("unexpected Java output: %s", output)
	}

	// Last line should be "RECORDS_READ: <count>"
	// Second to last line should be "CONTINUATION: <hex>" or "CONTINUATION: null"
	recordsLine := lines[len(lines)-1]
	contLine := lines[len(lines)-2]

	// Parse record count
	if !strings.HasPrefix(recordsLine, "RECORDS_READ: ") {
		return nil, 0, fmt.Errorf("expected RECORDS_READ line, got: %s", recordsLine)
	}
	count, err := strconv.Atoi(strings.TrimPrefix(recordsLine, "RECORDS_READ: "))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse record count: %v", err)
	}

	// Parse continuation
	if !strings.HasPrefix(contLine, "CONTINUATION: ") {
		return nil, 0, fmt.Errorf("expected CONTINUATION line, got: %s", contLine)
	}
	contHex := strings.TrimPrefix(contLine, "CONTINUATION: ")
	if contHex == "null" {
		return nil, count, nil
	}

	contBytes := make([]byte, len(contHex)/2)
	_, err = fmt.Sscanf(contHex, "%x", &contBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse continuation hex: %v", err)
	}

	return contBytes, count, nil
}