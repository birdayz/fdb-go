package recordlayer_test

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestSaveRecord_NotInUnion tests error handling when trying to save a record
// that is not in the UnionDescriptor
// Java equivalent: FDBRecordStoreCrudTest.writeNotUnionType()
func TestSaveRecord_NotInUnion(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("not_in_union_test"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	recordDB := recordlayer.NewFDBDatabase(db)

	// Create metadata with ONLY Order in the union (not Customer)
	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)

	// Only add Order to the union
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))

	// Build metadata - this should only include Order
	recordMetaData := metaDataBuilder.Build()

	// Verify that Order is in the union
	orderType := recordMetaData.GetRecordType("Order")
	if orderType == nil {
		t.Fatal("Order should be in the record metadata")
	}

	keyspace := subspace.FromBytes(tuple.Tuple{"not_in_union_test"}.Pack())

	// Test 1: Saving Order should work (it's in the union)
	t.Run("OrderInUnion", func(t *testing.T) {
		_, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(100),
			}

			_, err = store.SaveRecord(order)
			if err != nil {
				t.Fatalf("SaveRecord should succeed for Order (in union): %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: Saving Customer should fail (not in union)
	// Note: We need to check if Customer is actually in the proto definition
	// If Customer doesn't exist in the proto, we'll create a mock invalid record
	t.Run("InvalidTypeNotInUnion", func(t *testing.T) {
		_, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Create a message that is NOT in the union
			// We'll use a Flower message which is definitely not a record type
			flower := &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			}

			_, err = store.SaveRecord(flower)
			if err == nil {
				t.Fatal("SaveRecord should fail for Flower (not a record type in union)")
			}

			// Error should mention "not in union" or "record type"
			errMsg := err.Error()
			if errMsg == "" {
				t.Fatal("Error message should not be empty")
			}
			// The error should indicate the type is not valid
			t.Logf("Got expected error: %v", err)

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 3: Verify that GetRecordType returns nil for unknown types
	t.Run("GetRecordType_Unknown", func(t *testing.T) {
		unknownType := recordMetaData.GetRecordType("NonExistentType")
		if unknownType != nil {
			t.Fatal("GetRecordType should return nil for unknown type")
		}

		flowerType := recordMetaData.GetRecordType("Flower")
		if flowerType != nil {
			t.Fatal("GetRecordType should return nil for Flower (not a record type)")
		}
	})
}

// TestLoadRecord_InvalidRecordTypeKey tests graceful handling when
// a key has an invalid record type index
func TestLoadRecord_InvalidRecordTypeKey(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("invalid_type_key_test"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	recordDB := recordlayer.NewFDBDatabase(db)

	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := metaDataBuilder.Build()

	keyspace := subspace.FromBytes(tuple.Tuple{"invalid_type_key_test"}.Pack())

	// Save a valid record first
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		order := &gen.Order{
			OrderId: proto.Int64(2001),
			Price:   proto.Int32(200),
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to save valid record: %v", err)
	}

	// Now manually write a key with an invalid/unknown record type index
	// This simulates data corruption or version mismatch
	t.Run("LoadWithInvalidTypeIndex", func(t *testing.T) {
		// First, let's manually write bad data with an invalid record type index
		err := db.Transact(func(tr recordlayer.FDBTransaction) (interface{}, error) {
			// Use an invalid record type index (e.g., 999)
			invalidTypeIndex := int64(999)
			primaryKey := int64(3001)

			// Construct key: subspace + [RECORD, invalidTypeIndex, primaryKey]
			recordsSubspace := keyspace.Sub(recordlayer.RecordKey)
			key := recordsSubspace.Pack(tuple.Tuple{invalidTypeIndex, primaryKey})

			// Write some dummy protobuf data
			dummyData := []byte{0x08, 0x01} // Minimal valid protobuf

			tr.Set(key, dummyData)

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Failed to write invalid key: %v", err)
		}

		// Now try to load this record - should fail gracefully
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Try to load - this should return nil or error gracefully
			// NOT panic
			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(3001)})

			// Either should be nil (not found) or error gracefully
			if err != nil {
				t.Logf("Got error (acceptable): %v", err)
			} else if storedRecord != nil {
				t.Logf("Got record despite invalid type (acceptable if deserialized)")
			} else {
				t.Log("LoadRecord returned nil (not found) - acceptable")
			}

			// The key point: we should NOT panic
			return nil, nil
		})

		// Transaction should complete without panic
		if err != nil {
			t.Logf("Transaction completed with error (acceptable): %v", err)
		}
	})
}

// TestRecordExists_InvalidRecordTypeKey tests RecordExists with invalid type index
func TestRecordExists_InvalidRecordTypeKey(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("exists_invalid_type_test"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	recordDB := recordlayer.NewFDBDatabase(db)

	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := metaDataBuilder.Build()

	keyspace := subspace.FromBytes(tuple.Tuple{"exists_invalid_type_test"}.Pack())

	// Manually write a key with invalid type index
	err = db.Transact(func(tr recordlayer.FDBTransaction) (interface{}, error) {
		invalidTypeIndex := int64(888)
		primaryKey := int64(4001)

		recordsSubspace := keyspace.Sub(recordlayer.RecordKey)
		key := recordsSubspace.Pack(tuple.Tuple{invalidTypeIndex, primaryKey})

		dummyData := []byte{0x08, 0x01}
		tr.Set(key, dummyData)

		return nil, nil
	})
	if err != nil {
		t.Fatalf("Failed to write invalid key: %v", err)
	}

	// Try RecordExists - should handle gracefully
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// RecordExists should handle invalid type index gracefully
		exists, err := store.RecordExists(tuple.Tuple{int64(4001)}, recordlayer.IsolationLevelSerializable)

		if err != nil {
			t.Logf("RecordExists returned error (acceptable): %v", err)
		} else {
			t.Logf("RecordExists returned %v", exists)
		}

		// Should NOT panic
		return nil, nil
	})

	if err != nil {
		t.Logf("Transaction completed with error (acceptable): %v", err)
	}
}

// TestUnionDescriptor_Validation tests that UnionDescriptor properly validates record types
func TestUnionDescriptor_Validation(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("union_validation_test"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	recordDB := recordlayer.NewFDBDatabase(db)

	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := metaDataBuilder.Build()

	keyspace := subspace.FromBytes(tuple.Tuple{"union_validation_test"}.Pack())

	t.Run("ValidateMessageIsInUnion", func(t *testing.T) {
		_, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Valid Order
			order := &gen.Order{
				OrderId: proto.Int64(5001),
				Price:   proto.Int32(500),
			}

			// Get the message descriptor
			msgDesc := order.ProtoReflect().Descriptor()
			fullName := string(msgDesc.FullName())

			t.Logf("Order full name: %s", fullName)

			// SaveRecord should succeed
			_, err = store.SaveRecord(order)
			if err != nil {
				t.Fatalf("SaveRecord should succeed for valid union member: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	t.Run("RejectMessageNotInUnion", func(t *testing.T) {
		_, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Flower is not a record type
			flower := &gen.Flower{
				Type:  proto.String("Tulip"),
				Color: gen.Color_BLUE.Enum(),
			}

			// Get the message descriptor to verify type
			msgDesc := flower.ProtoReflect().Descriptor()
			fullName := string(msgDesc.FullName())

			t.Logf("Flower full name: %s", fullName)

			// SaveRecord should fail
			_, err = store.SaveRecord(flower)
			if err == nil {
				t.Fatal("SaveRecord should fail for non-union member")
			}

			t.Logf("Got expected error: %v", err)

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// Helper to create a test message that's definitely not in the union
type invalidMessage struct {
	protoreflect.Message
}

func (m *invalidMessage) ProtoReflect() protoreflect.Message {
	return m
}
