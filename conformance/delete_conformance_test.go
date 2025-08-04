package conformance

import (
	"context"
	"os"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func TestDeleteRecordConformance(t *testing.T) {
	// Skip if FDB is not available
	if os.Getenv("SKIP_FDB_TESTS") != "" {
		t.Skip("Skipping FDB conformance tests (SKIP_FDB_TESTS set)")
	}

	// Initialize FDB
	fdb.MustAPIVersion(720)
	db := fdb.MustOpenDefault()
	recordDB := recordlayer.NewFDBDatabase(db)

	// Setup metadata
	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := metaDataBuilder.Build()

	// Create test subspace
	keyspace := subspace.FromBytes(tuple.Tuple{"delete_conformance_test"}.Pack())

	result, err := recordDB.Run(context.Background(), func(ctx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Test 1: Create and delete a record
		order := &gen.Order{
			OrderId: proto.Int64(9999),
			Price:   proto.Int32(99),
			Flower: &gen.Flower{
				Type:  proto.String("TestFlower"),
				Color: gen.Color_BLUE.Enum(),
			},
		}

		// Save the record
		savedRecord, err := store.SaveRecord(order)
		if err != nil {
			return nil, err
		}
		t.Logf("Saved record: key=%v, size=%d bytes", savedRecord.PrimaryKey, savedRecord.ValueSize)

		// Verify it exists
		primaryKey := tuple.Tuple{int64(9999)}
		loadedRecord, err := store.LoadRecord(primaryKey)
		if err != nil {
			return nil, err
		}
		if loadedRecord == nil {
			t.Errorf("Record should exist after save")
			return nil, nil
		}

		// Delete the record
		deleted, err := store.DeleteRecord(primaryKey)
		if err != nil {
			return nil, err
		}
		if !deleted {
			t.Errorf("DeleteRecord should return true when record exists")
		}
		t.Logf("Successfully deleted record with key %v", primaryKey)

		// Verify it no longer exists
		loadedRecord, err = store.LoadRecord(primaryKey)
		if err != nil {
			return nil, err
		}
		if loadedRecord != nil {
			t.Errorf("Record should not exist after delete")
		}

		// Test 2: Try to delete non-existent record
		nonExistentKey := tuple.Tuple{int64(1111111)}
		deleted, err = store.DeleteRecord(nonExistentKey)
		if err != nil {
			return nil, err
		}
		if deleted {
			t.Errorf("DeleteRecord should return false when record doesn't exist")
		}
		t.Logf("Correctly returned false for non-existent record with key %v", nonExistentKey)

		return "delete conformance test completed", nil
	})

	if err != nil {
		t.Fatalf("Delete conformance test failed: %v", err)
	}

	t.Logf("Delete conformance test passed: %v", result)
}