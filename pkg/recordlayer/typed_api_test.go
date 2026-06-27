package recordlayer

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// testMetaData creates metadata with all record types having primary keys set.
func testMetaData(t testing.TB) *RecordMetaData {
	t.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("failed to build metadata: %v", err)
	}
	return md
}

// TestGetTypedRecordStore verifies the new Java-like API works
func TestGetTypedRecordStore(t *testing.T) {
	// Create base store
	metaData := testMetaData(t)

	baseStore := &FDBRecordStore{
		metaData: metaData,
	}

	// Create typed store using the new API - much cleaner!
	orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
	if err != nil {
		t.Fatalf("Failed to create typed store: %v", err)
	}

	// Verify it's the right type
	if orderStore == nil {
		t.Fatal("Typed store is nil")
	}

	if orderStore.recordType.Name != "Order" {
		t.Errorf("Expected record type 'Order', got %s", orderStore.recordType.Name)
	}

	// Test that wrap/unwrap functions work
	order := &gen.Order{
		OrderId: proto.Int64(123),
		Price:   proto.Int32(50),
	}

	// Test wrap function
	union, err := orderStore.wrapFunc(order)
	if err != nil {
		t.Fatalf("Wrap function failed: %v", err)
	}

	if union.XOrder == nil {
		t.Fatal("Union does not contain Order")
	}

	if *union.XOrder.OrderId != 123 {
		t.Errorf("Expected OrderId 123, got %d", *union.XOrder.OrderId)
	}

	// Test unwrap function
	unwrapped, err := orderStore.unwrapFunc(union)
	if err != nil {
		t.Fatalf("Unwrap function failed: %v", err)
	}

	if *unwrapped.OrderId != 123 {
		t.Errorf("Expected OrderId 123, got %d", *unwrapped.OrderId)
	}

	t.Logf("✓ New API works! Much cleaner than manual construction.")
}

// TestGetTypedRecordStore_InvalidType verifies error handling
func TestGetTypedRecordStore_InvalidType(t *testing.T) {
	metaData := testMetaData(t)

	baseStore := &FDBRecordStore{
		metaData: metaData,
	}

	// Try to get typed store for non-existent type
	_, err := GetTypedRecordStore[*gen.Order](baseStore, "NonExistentType")
	if err == nil {
		t.Fatal("Expected error for non-existent record type")
	}

	if err.Error() != "record type 'NonExistentType' not found in metadata" {
		t.Errorf("Unexpected error message: %v", err)
	}

	t.Logf("✓ Error handling works correctly")
}
