package recordlayer_test

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestRecordExists_BasicFunctionality tests the RecordExists method
func TestRecordExists_BasicFunctionality(t *testing.T) {
	ctx := context.Background()

	// Start FoundationDB testcontainer
	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("record_exists_test"),
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

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("existence_check_error_if_exists"),
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

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("insert_record_test"),
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

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("update_record_test"),
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
