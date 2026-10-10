package recordlayer

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/gen"
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

	// The metadata decodes Order as *gen.Order, so the union builder matches.
	if !orderStore.builderMatches {
		t.Error("Order store typed *gen.Order does not match the metadata's Order")
	}
	// Java's TypedRecordSerializer constructor refuses a getter of another
	// message than the field's (TypedRecordSerializer.java:77-80).
	_, err = GetTypedRecordStore[*gen.Customer](baseStore, "Order")
	var argErr *RecordCoreArgumentError
	if !errors.As(err, &argErr) || argErr.Message != "fieldDescriptor and getter do not match" {
		t.Errorf("Order store typed *gen.Customer: %v (%T), want fieldDescriptor and getter do not match", err, err)
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

	if err.Error() != "Unknown record type NonExistentType" {
		t.Errorf("Unexpected error message: %v", err)
	}

	t.Logf("✓ Error handling works correctly")
}

// NewTypedRecordStore resolves its record type in the store's own metadata:
// a nil, unknown or other metadata's record type is an error, not a store
// whose every check compares against a pointer the store never produces.
func TestNewTypedRecordStore_RecordTypeOutsideStoreMetaData(t *testing.T) {
	t.Parallel()
	baseStore := &FDBRecordStore{metaData: testMetaData(t)}
	catalog := NewRecordMetaDataBuilder().SetRecords(gen.File_catalog_data_proto)
	catalog.GetRecordType("Databases").SetPrimaryKey(Field("DATABASE_ID"))
	catalog.GetRecordType("Schemas").SetPrimaryKey(Field("DATABASE_ID"))
	catalog.GetRecordType("Templates").SetPrimaryKey(Field("TEMPLATE_NAME"))
	catalogMD, err := catalog.Build()
	if err != nil {
		t.Fatal(err)
	}
	newStore := func(rt *RecordType) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		_, err = NewTypedRecordStore[*gen.Order](baseStore, rt)
		return err
	}
	var argErr *RecordCoreArgumentError
	if err := newStore(nil); !errors.As(err, &argErr) {
		t.Errorf("nil record type: %v (%T), want a RecordCoreArgumentError", err, err)
	}
	var mdErr *MetaDataError
	if err := newStore(catalogMD.GetRecordType("Databases")); !errors.As(err, &mdErr) || mdErr.Message != "Unknown record type Databases" {
		t.Errorf("unknown record type: %v (%T), want Unknown record type Databases", err, err)
	}
	if err := newStore(testMetaData(t).GetRecordType("Order")); !errors.As(err, &argErr) || argErr.Message != "fieldDescriptor does not match recordType" {
		t.Errorf("other metadata's Order: %v (%T), want fieldDescriptor does not match recordType", err, err)
	}
	if err := newStore(baseStore.metaData.GetRecordType("Order")); err != nil {
		t.Errorf("the store's own Order: %v", err)
	}
}
