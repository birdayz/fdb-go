package recordlayer

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

func TestBasicScan(t *testing.T) {
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
			SetSubspace(subspace.Sub("scan_test")).
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
		defer cursor.Close()

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