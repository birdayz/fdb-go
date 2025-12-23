package recordlayer

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestRecordExists_BasicFunctionality tests the RecordExists method
func TestRecordExists_BasicFunctionality(t *testing.T) {
	ctx := context.Background()

	// Start FoundationDB testcontainer
	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("record_exists_test"),
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer container.Terminate(ctx)

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

	recordDB := recordlayer.NewFDBDatabase(db)

	// Setup metadata
	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := metaDataBuilder.Build()

	// Create test subspace
	keyspace := subspace.FromBytes(tuple.Tuple{"record_exists_test"}.Pack())

	// Test 1: RecordExists should return false for non-existent record
	t.Run("NonExistentRecord", func(t *testing.T) {
		_, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			exists, err := store.RecordExists(tuple.Tuple{int64(99999)}, recordlayer.IsolationLevelSerializable)
			if err != nil {
				t.Fatalf("RecordExists failed: %v", err)
			}
			if exists {
				t.Fatal("Expected RecordExists to return false for non-existent record")
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: RecordExists should return true for existing record
	t.Run("ExistingRecord", func(t *testing.T) {
		// First, save a record
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
				Price:   proto.Int32(50),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			_, err = store.SaveRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to save record: %v", err)
		}

		// Now check if it exists
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			exists, err := store.RecordExists(tuple.Tuple{int64(1001)}, recordlayer.IsolationLevelSerializable)
			if err != nil {
				t.Fatalf("RecordExists failed: %v", err)
			}
			if !exists {
				t.Fatal("Expected RecordExists to return true for existing record")
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 3: RecordExists should return false after record is deleted
	t.Run("DeletedRecord", func(t *testing.T) {
		// Save a record
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
				OrderId: proto.Int64(1002),
				Price:   proto.Int32(75),
				Flower: &gen.Flower{
					Type:  proto.String("Tulip"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.SaveRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to save record: %v", err)
		}

		// Delete the record
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1002)})
			if err != nil {
				t.Fatalf("DeleteRecord failed: %v", err)
			}
			if !deleted {
				t.Fatal("Expected DeleteRecord to return true")
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Delete transaction failed: %v", err)
		}

		// Check if it still exists
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			exists, err := store.RecordExists(tuple.Tuple{int64(1002)}, recordlayer.IsolationLevelSerializable)
			if err != nil {
				t.Fatalf("RecordExists failed: %v", err)
			}
			if exists {
				t.Fatal("Expected RecordExists to return false after record deletion")
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestRecordExistenceCheck_ErrorIfExists tests the ERROR_IF_EXISTS check
func TestRecordExistenceCheck_ErrorIfExists(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("existence_check_error_if_exists"),
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer container.Terminate(ctx)

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

	keyspace := subspace.FromBytes(tuple.Tuple{"error_if_exists_test"}.Pack())

	// Test 1: Should succeed on new record
	t.Run("NewRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(2001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Daisy"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfExists)
			if err != nil {
				t.Fatalf("Expected SaveRecordWithOptions to succeed for new record, got error: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: Should fail on existing record
	t.Run("ExistingRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(2001),
				Price:   proto.Int32(200),
				Flower: &gen.Flower{
					Type:  proto.String("Lily"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfExists)
			if err == nil {
				t.Fatal("Expected SaveRecordWithOptions to fail for existing record")
			}
			if err != recordlayer.ErrRecordAlreadyExists {
				t.Fatalf("Expected ErrRecordAlreadyExists, got: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestInsertRecord tests the InsertRecord convenience method
func TestInsertRecord(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("insert_record_test"),
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer container.Terminate(ctx)

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

	keyspace := subspace.FromBytes(tuple.Tuple{"insert_record_test"}.Pack())

	// Test 1: InsertRecord should succeed for new record
	t.Run("NewRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(3001),
				Price:   proto.Int32(150),
				Flower: &gen.Flower{
					Type:  proto.String("Orchid"),
					Color: gen.Color_PINK.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			if err != nil {
				t.Fatalf("InsertRecord failed for new record: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: InsertRecord should fail for existing record
	t.Run("ExistingRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(3001),
				Price:   proto.Int32(250),
				Flower: &gen.Flower{
					Type:  proto.String("Carnation"),
					Color: gen.Color_RED.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			if err == nil {
				t.Fatal("InsertRecord should fail for existing record")
			}
			if err != recordlayer.ErrRecordAlreadyExists {
				t.Fatalf("Expected ErrRecordAlreadyExists, got: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestUpdateRecord tests the UpdateRecord convenience method
func TestUpdateRecord(t *testing.T) {
	ctx := context.Background()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithDatabase("update_record_test"),
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer container.Terminate(ctx)

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

	keyspace := subspace.FromBytes(tuple.Tuple{"update_record_test"}.Pack())

	// Test 1: UpdateRecord should fail for non-existent record
	t.Run("NonExistentRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(4001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Iris"),
					Color: gen.Color_BLUE.Enum(),
				},
			}

			_, err = store.UpdateRecord(order)
			if err == nil {
				t.Fatal("UpdateRecord should fail for non-existent record")
			}
			if err != recordlayer.ErrRecordDoesNotExist {
				t.Fatalf("Expected ErrRecordDoesNotExist, got: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: UpdateRecord should succeed for existing record
	t.Run("ExistingRecord", func(t *testing.T) {
		// First insert a record
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
				OrderId: proto.Int64(4002),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Peony"),
					Color: gen.Color_PINK.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to insert record: %v", err)
		}

		// Now update it
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
				OrderId: proto.Int64(4002),
				Price:   proto.Int32(200), // Updated price
				Flower: &gen.Flower{
					Type:  proto.String("Peony"),
					Color: gen.Color_RED.Enum(), // Updated color
				},
			}

			_, err = store.UpdateRecord(order)
			if err != nil {
				t.Fatalf("UpdateRecord failed for existing record: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}

		// Verify the update
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(4002)})
			if err != nil {
				t.Fatalf("LoadRecord failed: %v", err)
			}
			if storedRecord == nil {
				t.Fatal("Expected to find updated record")
			}

			order := storedRecord.Record.(*gen.Order)
			if *order.Price != 200 {
				t.Fatalf("Expected updated price 200, got %d", *order.Price)
			}
			if *order.Flower.Color != gen.Color_RED {
				t.Fatalf("Expected updated color RED, got %v", *order.Flower.Color)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestRecordExistenceCheck_ErrorIfTypeChanged tests the ERROR_IF_RECORD_TYPE_CHANGED check
// This test requires multiple record types in the schema (Order and Customer)
func TestRecordExistenceCheck_ErrorIfTypeChanged(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "error_if_type_changed_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata with both Order and Customer types
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	recordMetaData := builder.Build()

	// Test 1: ERROR_IF_RECORD_TYPE_CHANGED should fail when trying to overwrite with different type
	t.Run("DifferentType", func(t *testing.T) {
		// First insert an Order
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
				OrderId: proto.Int64(5001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Lily"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to insert order: %v", err)
		}

		// Now try to save a Customer with same primary key and ERROR_IF_RECORD_TYPE_CHANGED
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			customer := &gen.Customer{
				CustomerId: proto.Int64(5001), // Same ID as the Order!
				Name:       proto.String("John Doe"),
				Email:      proto.String("john@example.com"),
			}

			_, err = store.SaveRecordWithOptions(customer, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			if err == nil {
				t.Fatal("SaveRecord should fail when type changed")
			}
			if err != recordlayer.ErrRecordTypeChanged {
				t.Fatalf("Expected ErrRecordTypeChanged, got: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: ERROR_IF_RECORD_TYPE_CHANGED should succeed when types match
	t.Run("SameType", func(t *testing.T) {
		// First insert an Order
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
				OrderId: proto.Int64(5002),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Daisy"),
					Color: gen.Color_BLUE.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to insert order: %v", err)
		}

		// Now update with same type and ERROR_IF_RECORD_TYPE_CHANGED - should succeed
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
				OrderId: proto.Int64(5002), // Same ID and same type
				Price:   proto.Int32(200),  // Updated price
				Flower: &gen.Flower{
					Type:  proto.String("Daisy"),
					Color: gen.Color_RED.Enum(), // Updated color
				},
			}

			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			if err != nil {
				t.Fatalf("SaveRecord should succeed when types match: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}

		// Verify the update succeeded
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(5002)})
			if err != nil {
				t.Fatalf("LoadRecord failed: %v", err)
			}
			if storedRecord == nil {
				t.Fatal("Expected to find updated record")
			}

			order := storedRecord.Record.(*gen.Order)
			if *order.Price != 200 {
				t.Fatalf("Expected updated price 200, got %d", *order.Price)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 3: ERROR_IF_RECORD_TYPE_CHANGED should allow new record
	t.Run("NewRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(5003),
				Price:   proto.Int32(300),
				Flower: &gen.Flower{
					Type:  proto.String("Tulip"),
					Color: gen.Color_PINK.Enum(),
				},
			}

			// Should succeed because record doesn't exist yet
			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
			if err != nil {
				t.Fatalf("SaveRecord should succeed for new record: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestRecordExistenceCheck_None tests the NONE existence check mode
// which allows both inserts and updates without restrictions.
//
// Java equivalent: RecordExistenceCheck.NONE
func TestRecordExistenceCheck_None(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "existence_check_none_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata with both Order and Customer types
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	recordMetaData := builder.Build()

	// Test 1: NONE mode allows inserting new record
	t.Run("NewRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(6001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			// NONE mode should allow insert
			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckNone)
			if err != nil {
				t.Fatalf("SaveRecordWithOptions(NONE) should succeed for new record: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: NONE mode allows updating existing record
	t.Run("ExistingRecord", func(t *testing.T) {
		// First insert a record (using Test 1's record)
		_, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Update the existing record
			order := &gen.Order{
				OrderId: proto.Int64(6001), // Same ID
				Price:   proto.Int32(200),  // Modified price
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_BLUE.Enum(), // Modified color
				},
			}

			// NONE mode should allow update
			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckNone)
			if err != nil {
				t.Fatalf("SaveRecordWithOptions(NONE) should succeed for existing record: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}

		// Verify the update
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(6001)})
			if err != nil {
				t.Fatalf("LoadRecord failed: %v", err)
			}
			if storedRecord == nil {
				t.Fatal("Expected to find updated record")
			}

			order := storedRecord.Record.(*gen.Order)
			if *order.Price != 200 {
				t.Fatalf("Expected updated price 200, got %d", *order.Price)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 3: NONE mode allows type changes (overwrites)
	t.Run("TypeChange", func(t *testing.T) {
		// First insert an Order
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
				OrderId: proto.Int64(6002),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Lily"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to insert order: %v", err)
		}

		// Now save a Customer with same primary key using NONE mode (should overwrite)
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			customer := &gen.Customer{
				CustomerId: proto.Int64(6002), // Same ID as Order
				Name:       proto.String("John Doe"),
				Email:      proto.String("john@example.com"),
			}

			// NONE mode should allow type change (silent overwrite)
			_, err = store.SaveRecordWithOptions(customer, recordlayer.RecordExistenceCheckNone)
			if err != nil {
				t.Fatalf("SaveRecordWithOptions(NONE) should succeed for type change: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}

		// Verify Customer now exists and Order is gone
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(6002)})
			if err != nil {
				t.Fatalf("LoadRecord failed: %v", err)
			}
			if storedRecord == nil {
				t.Fatal("Expected to find record")
			}

			// Should be Customer now, not Order
			if _, ok := storedRecord.Record.(*gen.Customer); !ok {
				t.Fatalf("Expected Customer type, got %T", storedRecord.Record)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestRecordExistenceCheck_ErrorIfNotExists tests the ERROR_IF_NOT_EXISTS mode
// which requires the record to already exist.
//
// Java equivalent: RecordExistenceCheck.ERROR_IF_NOT_EXISTS
func TestRecordExistenceCheck_ErrorIfNotExists(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "error_if_not_exists_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Test 1: ERROR_IF_NOT_EXISTS fails for non-existent record
	t.Run("NonExistentRecord", func(t *testing.T) {
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
				OrderId: proto.Int64(7001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			// ERROR_IF_NOT_EXISTS should fail for new record
			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfNotExists)
			if err == nil {
				t.Fatal("SaveRecordWithOptions(ERROR_IF_NOT_EXISTS) should fail for non-existent record")
			}

			if err != recordlayer.ErrRecordDoesNotExist {
				t.Fatalf("Expected ErrRecordDoesNotExist, got: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})

	// Test 2: ERROR_IF_NOT_EXISTS succeeds for existing record
	t.Run("ExistingRecord", func(t *testing.T) {
		// First insert a record
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
				OrderId: proto.Int64(7002),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Lily"),
					Color: gen.Color_BLUE.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			return nil, err
		})
		if err != nil {
			t.Fatalf("Failed to insert record: %v", err)
		}

		// Now update it with ERROR_IF_NOT_EXISTS (should succeed)
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
				OrderId: proto.Int64(7002),
				Price:   proto.Int32(200), // Updated
				Flower: &gen.Flower{
					Type:  proto.String("Lily"),
					Color: gen.Color_RED.Enum(), // Updated
				},
			}

			// ERROR_IF_NOT_EXISTS should succeed for existing record
			_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfNotExists)
			if err != nil {
				t.Fatalf("SaveRecordWithOptions(ERROR_IF_NOT_EXISTS) should succeed for existing record: %v", err)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}

		// Verify the update
		_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(7002)})
			if err != nil {
				t.Fatalf("LoadRecord failed: %v", err)
			}
			if storedRecord == nil {
				t.Fatal("Expected to find updated record")
			}

			order := storedRecord.Record.(*gen.Order)
			if *order.Price != 200 {
				t.Fatalf("Expected updated price 200, got %d", *order.Price)
			}

			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed: %v", err)
		}
	})
}

// TestErrorMetadata_RecordAlreadyExists verifies that RecordAlreadyExistsError
// includes the primary key in its structured context.
//
// Java equivalent: RecordAlreadyExistsException with LogMessageKeys.PRIMARY_KEY
func TestErrorMetadata_RecordAlreadyExists(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "error_metadata_already_exists_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Insert a record
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
			OrderId: proto.Int64(8001),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}

		_, err = store.InsertRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to insert record: %v", err)
	}

	// Try to insert duplicate - should get structured error
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
			OrderId: proto.Int64(8001), // Duplicate
			Price:   proto.Int32(200),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_BLUE.Enum(),
			},
		}

		_, err = store.InsertRecord(order)
		return nil, err
	})

	// Verify error structure
	if err == nil {
		t.Fatal("Expected RecordAlreadyExistsError")
	}

	if recordErr, ok := err.(*recordlayer.RecordAlreadyExistsError); ok {
		// Verify PrimaryKey is populated
		if recordErr.PrimaryKey == nil {
			t.Fatal("RecordAlreadyExistsError.PrimaryKey should be populated")
		}

		// Verify it's the correct primary key
		if pkTuple, ok := recordErr.PrimaryKey.(tuple.Tuple); ok {
			if len(pkTuple) != 1 || pkTuple[0] != int64(8001) {
				t.Fatalf("Expected primary key Tuple{8001}, got %v", pkTuple)
			}
		} else {
			t.Fatalf("Expected PrimaryKey to be tuple.Tuple, got %T", recordErr.PrimaryKey)
		}

		// Verify message is not empty
		if recordErr.Message == "" {
			t.Fatal("RecordAlreadyExistsError.Message should not be empty")
		}
	} else {
		t.Fatalf("Expected *RecordAlreadyExistsError, got %T: %v", err, err)
	}
}

// TestErrorMetadata_RecordDoesNotExist verifies that RecordDoesNotExistError
// includes the primary key in its structured context.
//
// Java equivalent: RecordDoesNotExistException with LogMessageKeys.PRIMARY_KEY
func TestErrorMetadata_RecordDoesNotExist(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "error_metadata_does_not_exist_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Try to update non-existent record - should get structured error
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
			OrderId: proto.Int64(8002),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Lily"),
				Color: gen.Color_BLUE.Enum(),
			},
		}

		_, err = store.UpdateRecord(order)
		return nil, err
	})

	// Verify error structure
	if err == nil {
		t.Fatal("Expected RecordDoesNotExistError")
	}

	if recordErr, ok := err.(*recordlayer.RecordDoesNotExistError); ok {
		// Verify PrimaryKey is populated
		if recordErr.PrimaryKey == nil {
			t.Fatal("RecordDoesNotExistError.PrimaryKey should be populated")
		}

		// Verify it's the correct primary key
		if pkTuple, ok := recordErr.PrimaryKey.(tuple.Tuple); ok {
			if len(pkTuple) != 1 || pkTuple[0] != int64(8002) {
				t.Fatalf("Expected primary key Tuple{8002}, got %v", pkTuple)
			}
		} else {
			t.Fatalf("Expected PrimaryKey to be tuple.Tuple, got %T", recordErr.PrimaryKey)
		}

		// Verify message is not empty
		if recordErr.Message == "" {
			t.Fatal("RecordDoesNotExistError.Message should not be empty")
		}
	} else {
		t.Fatalf("Expected *RecordDoesNotExistError, got %T: %v", err, err)
	}
}

// TestErrorMetadata_RecordTypeChanged verifies that RecordTypeChangedError
// includes the primary key and both type names in its structured context.
//
// Java equivalent: RecordTypeChangedException with LogMessageKeys.PRIMARY_KEY,
// LogMessageKeys.ACTUAL_TYPE, LogMessageKeys.EXPECTED_TYPE
func TestErrorMetadata_RecordTypeChanged(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "error_metadata_type_changed_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata with multiple types
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	recordMetaData := builder.Build()

	// Insert an Order
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
			OrderId: proto.Int64(8003),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Daisy"),
				Color: gen.Color_YELLOW.Enum(),
			},
		}

		_, err = store.InsertRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to insert order: %v", err)
	}

	// Try to update with Customer (wrong type) - should get structured error
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		customer := &gen.Customer{
			CustomerId: proto.Int64(8003), // Same ID as Order
			Name:       proto.String("John Doe"),
			Email:      proto.String("john@example.com"),
		}

		_, err = store.SaveRecordWithOptions(customer, recordlayer.RecordExistenceCheckErrorIfTypeChanged)
		return nil, err
	})

	// Verify error structure
	if err == nil {
		t.Fatal("Expected RecordTypeChangedError")
	}

	if recordErr, ok := err.(*recordlayer.RecordTypeChangedError); ok {
		// Verify PrimaryKey is populated
		if recordErr.PrimaryKey == nil {
			t.Fatal("RecordTypeChangedError.PrimaryKey should be populated")
		}

		// Verify it's the correct primary key
		if pkTuple, ok := recordErr.PrimaryKey.(tuple.Tuple); ok {
			if len(pkTuple) != 1 || pkTuple[0] != int64(8003) {
				t.Fatalf("Expected primary key Tuple{8003}, got %v", pkTuple)
			}
		} else {
			t.Fatalf("Expected PrimaryKey to be tuple.Tuple, got %T", recordErr.PrimaryKey)
		}

		// Verify ActualType is populated (should be "Order")
		if recordErr.ActualType == "" {
			t.Fatal("RecordTypeChangedError.ActualType should be populated")
		}
		if recordErr.ActualType != "Order" {
			t.Fatalf("Expected ActualType 'Order', got '%s'", recordErr.ActualType)
		}

		// Verify ExpectedType is populated (should be "Customer")
		if recordErr.ExpectedType == "" {
			t.Fatal("RecordTypeChangedError.ExpectedType should be populated")
		}
		if recordErr.ExpectedType != "Customer" {
			t.Fatalf("Expected ExpectedType 'Customer', got '%s'", recordErr.ExpectedType)
		}

		// Verify message is not empty
		if recordErr.Message == "" {
			t.Fatal("RecordTypeChangedError.Message should not be empty")
		}
	} else {
		t.Fatalf("Expected *RecordTypeChangedError, got %T: %v", err, err)
	}
}
