package recordlayer

import (
	"fmt"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// Test helper to create a typed Order store using the new API
func createTestOrderStore(baseStore *FDBRecordStore) *TypedFDBRecordStore[*gen.Order] {
	orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
	if err != nil {
		panic(err)
	}
	return orderStore
}

func TestTypedRecordStore_OrderOperations(t *testing.T) {
	// Create base store using proper builder
	metaData := testMetaData(t)

	baseStore := &FDBRecordStore{
		metaData: metaData,
	}

	// Create typed store for Orders
	orderStore := createTestOrderStore(baseStore)

	// Test that the typed store has the correct type
	if orderStore == nil {
		t.Fatal("Failed to create order store")
	}

	// Verify the store has the right record type
	if orderStore.recordType.Name != "Order" {
		t.Errorf("Expected record type 'Order', got %s", orderStore.recordType.Name)
	}
}

func TestTypedRecordStore_WrapUnwrapFunctions(t *testing.T) {
	// Create base store using proper builder
	metaData := testMetaData(t)

	baseStore := &FDBRecordStore{
		metaData: metaData,
	}

	// Create typed store
	orderStore := createTestOrderStore(baseStore)

	// Test wrap function
	order := &gen.Order{
		OrderId: proto.Int64(1001),
		Price:   proto.Int32(25),
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_RED.Enum(),
		},
	}

	union, err := orderStore.wrapFunc(order)
	if err != nil {
		t.Fatalf("Wrap function failed: %v", err)
	}

	if union.XOrder == nil {
		t.Fatal("Union does not contain Order")
	}

	if *union.XOrder.OrderId != 1001 {
		t.Errorf("Expected OrderId 1001, got %d", *union.XOrder.OrderId)
	}

	// Test unwrap function
	unwrappedOrder, err := orderStore.unwrapFunc(union)
	if err != nil {
		t.Fatalf("Unwrap function failed: %v", err)
	}

	if *unwrappedOrder.OrderId != 1001 {
		t.Errorf("Expected OrderId 1001, got %d", *unwrappedOrder.OrderId)
	}

	if *unwrappedOrder.Price != 25 {
		t.Errorf("Expected Price 25, got %d", *unwrappedOrder.Price)
	}

	if *unwrappedOrder.Flower.Type != "Rose" {
		t.Errorf("Expected Flower type 'Rose', got %s", *unwrappedOrder.Flower.Type)
	}
}

func TestGenericTypedRecordStore_Creation(t *testing.T) {
	// Create base store using proper builder
	metaData := testMetaData(t)

	baseStore := &FDBRecordStore{
		metaData: metaData,
	}

	recordType := metaData.GetRecordType("Order")
	if recordType == nil {
		t.Fatal("Order record type not found")
	}

	// Create typed store using the generic constructor
	typedStore := NewTypedRecordStore[*gen.Order](
		baseStore,
		recordType,
		func(union *gen.UnionDescriptor) (*gen.Order, error) {
			if union.XOrder == nil {
				return nil, fmt.Errorf("union descriptor does not contain Order record")
			}
			return union.XOrder, nil
		},
		func(order *gen.Order) (*gen.UnionDescriptor, error) {
			return &gen.UnionDescriptor{XOrder: order}, nil
		},
	)

	if typedStore == nil {
		t.Fatal("Failed to create typed store")
	}

	if typedStore.recordType.Name != "Order" {
		t.Errorf("Expected record type 'Order', got %s", typedStore.recordType.Name)
	}
}

// Benchmark to ensure generic wrapper doesn't add significant overhead
func BenchmarkTypedRecordStore_Overhead(b *testing.B) {
	// Setup using proper builder
	metaData := testMetaData(b)

	baseStore := &FDBRecordStore{
		metaData: metaData,
	}

	orderStore := createTestOrderStore(baseStore)

	order := &gen.Order{
		OrderId: proto.Int64(1001),
		Price:   proto.Int32(25),
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_RED.Enum(),
		},
	}

	// Benchmark wrap/unwrap operations
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		union, err := orderStore.wrapFunc(order)
		if err != nil {
			b.Fatal(err)
		}

		_, err = orderStore.unwrapFunc(union)
		if err != nil {
			b.Fatal(err)
		}
	}
}
