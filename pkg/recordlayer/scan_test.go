package recordlayer

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"google.golang.org/protobuf/proto"
)

func TestBasicScan(t *testing.T) {
	ctx := context.Background()
	
	// Start FoundationDB testcontainer
	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("basic_scan_test"),
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

	// Create metadata
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaData := builder.Build()

	_, err = fdbDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metaData).
			SetSubspace(subspace.FromBytes(tuple.Tuple{"scan_test"}.Pack())).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Save some test orders
		testOrders := []*gen.Order{
			{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(25),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			},
			{
				OrderId: proto.Int64(1002),
				Price:   proto.Int32(30),
				Flower:  &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_YELLOW.Enum()},
			},
			{
				OrderId: proto.Int64(1003),
				Price:   proto.Int32(35),
				Flower:  &gen.Flower{Type: proto.String("Lily"), Color: gen.Color_BLUE.Enum()},
			},
		}

		// Save all test orders
		for _, order := range testOrders {
			_, err := store.SaveRecord(order)
			if err != nil {
				t.Fatalf("Failed to save order %d: %v", *order.OrderId, err)
			}
		}

		// Scan all records
		cursor := store.ScanRecords(nil, ForwardScan)
		defer func() { _ = cursor.Close() }()

		var foundOrders []int64
		scanCtx := context.Background()

		for {
			result, err := cursor.OnNext(scanCtx)
			if err != nil {
				t.Fatalf("Scan error: %v", err)
			}

			if !result.HasNext() {
				break
			}

			record := result.GetValue()
			order, ok := record.Record.(*gen.Order)
			if !ok {
				t.Fatalf("Expected *gen.Order, got %T", record.Record)
			}

			foundOrders = append(foundOrders, *order.OrderId)
			t.Logf("Found order: ID=%d, Price=%d, Type=%s",
				*order.OrderId, *order.Price, *order.Flower.Type)
		}

		// Verify we found all orders
		if len(foundOrders) != len(testOrders) {
			t.Fatalf("Expected %d orders, found %d", len(testOrders), len(foundOrders))
		}

		// Verify order IDs (should be in key order: 1001, 1002, 1003)
		expectedIDs := []int64{1001, 1002, 1003}
		for i, expectedID := range expectedIDs {
			if foundOrders[i] != expectedID {
				t.Errorf("Expected order %d at position %d, got %d", expectedID, i, foundOrders[i])
			}
		}

		t.Logf("Scan test passed: found %d orders in correct order", len(foundOrders))
		return nil, nil
	})

	if err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}
}