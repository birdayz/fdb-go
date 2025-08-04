package recordlayer

import (
	"context"
	"slices"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

func TestCursorSeqInterface(t *testing.T) {
	if !fdb.IsAPIVersionSelected() {
		fdb.MustAPIVersion(630)
	}
	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)

	// Create metadata
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaData := builder.Build()

	_, err := fdbDB.Run(context.Background(), func(ctx *FDBRecordContext) (interface{}, error) {
		store, err := NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(subspace.Sub("cursor_seq_test")).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Save test data
		testOrders := []*gen.Order{
			{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(10),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			},
			{
				OrderId: proto.Int64(1002),
				Price:   proto.Int32(25),
				Flower:  &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_YELLOW.Enum()},
			},
			{
				OrderId: proto.Int64(1003),
				Price:   proto.Int32(50),
				Flower:  &gen.Flower{Type: proto.String("Lily"), Color: gen.Color_BLUE.Enum()},
			},
		}

		// Save all orders
		for _, order := range testOrders {
			_, err := store.SaveRecord(order)
			if err != nil {
				t.Fatalf("Failed to save order: %v", err)
			}
		}

		scanCtx := context.Background()

		// Test 1: Basic Seq interface
		t.Run("BasicSeq", func(t *testing.T) {
			cursor := store.ScanRecords(nil, ForwardScan)
			
			var orderIDs []int64
			for record := range cursor.Seq(scanCtx) {
				order := record.Record.(*gen.Order)
				orderIDs = append(orderIDs, *order.OrderId)
			}
			
			if len(orderIDs) != 3 {
				t.Fatalf("Expected 3 orders, got %d", len(orderIDs))
			}
			
			expected := []int64{1001, 1002, 1003}
			if !slices.Equal(orderIDs, expected) {
				t.Errorf("Expected %v, got %v", expected, orderIDs)
			}
			
			t.Logf("✓ Basic Seq iteration found orders: %v", orderIDs)
		})

		// Test 2: Seq2 interface
		t.Run("Seq2WithErrors", func(t *testing.T) {
			cursor := store.ScanRecords(nil, ForwardScan)
			
			var orderIDs []int64
			for record, err := range cursor.Seq2(scanCtx) {
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				order := record.Record.(*gen.Order)
				orderIDs = append(orderIDs, *order.OrderId)
			}
			
			if len(orderIDs) != 3 {
				t.Fatalf("Expected 3 orders, got %d", len(orderIDs))
			}
			
			t.Logf("✓ Seq2 iteration found orders: %v", orderIDs)
		})

		// Test 3: Standard library integration
		t.Run("StdlibIntegration", func(t *testing.T) {
			cursor := store.ScanRecords(nil, ForwardScan)
			
			// Test slices.Collect (Go 1.23+)
			allRecords := slices.Collect(cursor.Seq(scanCtx))
			if len(allRecords) != 3 {
				t.Fatalf("slices.Collect: expected 3 records, got %d", len(allRecords))
			}
			
			// Test manual counting
			cursor2 := store.ScanRecords(nil, ForwardScan)
			count := 0
			for range cursor2.Seq(scanCtx) {
				count++
			}
			if count != 3 {
				t.Fatalf("manual count: expected 3, got %d", count)
			}
			
			// Test getting first record
			cursor3 := store.ScanRecords(nil, ForwardScan)
			var firstRecord *FDBStoredRecord[proto.Message]
			var found bool
			for record := range cursor3.Seq(scanCtx) {
				firstRecord = record
				found = true
				break
			}
			if !found {
				t.Fatal("no records found")
			}
			firstOrder := firstRecord.Record.(*gen.Order)
			if *firstOrder.OrderId != 1001 {
				t.Fatalf("expected order 1001, got %d", *firstOrder.OrderId)
			}
			
			t.Logf("✓ Standard library integration works: count=%d, first=%d", count, *firstOrder.OrderId)
		})

		// Test 4: Chaining operations
		t.Run("ChainingOperations", func(t *testing.T) {
			cursor := store.ScanRecords(nil, ForwardScan)
			
			// Use sequence transformations for filtering and mapping
			expensiveOrders := Filter(
				cursor.Seq(scanCtx),
				func(record *FDBStoredRecord[proto.Message]) bool {
					order := record.Record.(*gen.Order)
					return *order.Price > 20
				},
			)
			
			expensiveOrderIDs := slices.Collect(
				Map(expensiveOrders, func(record *FDBStoredRecord[proto.Message]) int64 {
					order := record.Record.(*gen.Order)
					return *order.OrderId
				}),
			)
			
			// Should find orders 1002 (25), 1003 (50)
			expected := []int64{1002, 1003}
			if !slices.Equal(expensiveOrderIDs, expected) {
				t.Errorf("Expected expensive orders %v, got %v", expected, expensiveOrderIDs)
			}
			
			t.Logf("✓ Chained filter+map found expensive orders: %v", expensiveOrderIDs)
		})

		// Test 5: Limit function
		t.Run("LimitFunction", func(t *testing.T) {
			cursor := store.ScanRecords(nil, ForwardScan)
			
			// Use Limit function to get first 2 records
			limitedOrders := slices.Collect(
				Limit(cursor.Seq(scanCtx), 2),
			)
			
			if len(limitedOrders) != 2 {
				t.Fatalf("LimitSeq: expected 2 records, got %d", len(limitedOrders))
			}
			
			firstOrder := limitedOrders[0].Record.(*gen.Order)
			secondOrder := limitedOrders[1].Record.(*gen.Order)
			
			if *firstOrder.OrderId != 1001 || *secondOrder.OrderId != 1002 {
				t.Errorf("LimitSeq: expected orders 1001,1002 got %d,%d", 
					*firstOrder.OrderId, *secondOrder.OrderId)
			}
			
			t.Logf("✓ LimitSeq correctly limited to first 2 orders: %d, %d", 
				*firstOrder.OrderId, *secondOrder.OrderId)
		})

		return nil, nil
	})

	if err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}
}