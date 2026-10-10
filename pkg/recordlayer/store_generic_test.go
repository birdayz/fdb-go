package recordlayer

import (
	"testing"

	"fdb.dev/gen"
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
	typedStore, err := NewTypedRecordStore[*gen.Order](baseStore, recordType)
	if err != nil {
		t.Fatalf("Failed to create typed store: %v", err)
	}

	if typedStore == nil {
		t.Fatal("Failed to create typed store")
	}

	if typedStore.recordType.Name != "Order" {
		t.Errorf("Expected record type 'Order', got %s", typedStore.recordType.Name)
	}
}
