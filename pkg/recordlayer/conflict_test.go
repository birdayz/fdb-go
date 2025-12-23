package recordlayer_test

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// TestAddRecordReadConflict_CausesWriteConflict verifies that adding a read conflict
// on a record causes write conflicts in other transactions.
//
// Java equivalent: FDBRecordStore.addRecordReadConflict(Tuple primaryKey)
// Java behavior: Adds a read conflict range, causing writes in other transactions to conflict
func TestAddRecordReadConflict_CausesWriteConflict(t *testing.T) {
	ctx := context.Background()

	t.Log("DEBUG: Starting test container setup")
	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "read_conflict_causes_write_conflict_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)
	t.Log("DEBUG: Test container setup complete")

	// Create metadata
	t.Log("DEBUG: Creating metadata")
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
			OrderId: proto.Int64(3001),
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
		t.Fatalf("Failed to insert test record: %v", err)
	}

	// Get raw FDB database
	fdbDB, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Transaction 1: Add read conflict and hold transaction open
	tx1, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 1: %v", err)
	}
	defer tx1.Cancel()

	rtx1 := recordlayer.NewFDBRecordContext(tx1)
	store1, err := recordlayer.NewStoreBuilder().
		SetContext(rtx1).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx1: %v", err)
	}

	// Add read conflict on the record
	store1.AddRecordReadConflict(tuple.Tuple{int64(3001)})

	// Transaction 2: Try to write to the same record
	tx2, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 2: %v", err)
	}
	defer tx2.Cancel()

	rtx2 := recordlayer.NewFDBRecordContext(tx2)
	store2, err := recordlayer.NewStoreBuilder().
		SetContext(rtx2).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx2: %v", err)
	}

	// Modify the record in tx2
	order := &gen.Order{
		OrderId: proto.Int64(3001),
		Price:   proto.Int32(200), // Modified
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_BLUE.Enum(), // Modified
		},
	}
	_, err = store2.SaveRecord(order)
	if err != nil {
		t.Fatalf("SaveRecord failed in tx2: %v", err)
	}

	// Commit tx2 first
	err = tx2.Commit().Get()
	if err != nil {
		t.Fatalf("Transaction 2 commit failed: %v", err)
	}

	// Now try to commit tx1 - should fail due to write conflict
	err = tx1.Commit().Get()
	if err == nil {
		t.Fatal("Transaction 1 should have failed with conflict after tx2 wrote to the read-conflicted key")
	}

	// Verify it's a conflict error
	if fdbErr, ok := err.(fdb.Error); ok {
		if fdbErr.Code != 1020 { // FDB error code for not_committed
			t.Fatalf("Expected conflict error (1020), got: %v (code %d)", err, fdbErr.Code)
		}
	} else {
		t.Fatalf("Expected FDB error, got: %v", err)
	}
}

// TestAddRecordWriteConflict_CausesReadConflict verifies that adding a write conflict
// on a record causes read conflicts in other transactions.
//
// Java equivalent: FDBRecordStore.addRecordWriteConflict(Tuple primaryKey)
// Java behavior: Adds a write conflict range, causing reads in other transactions to conflict
func TestAddRecordWriteConflict_CausesReadConflict(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "write_conflict_causes_read_conflict_test")
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
			OrderId: proto.Int64(3002),
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
		t.Fatalf("Failed to insert test record: %v", err)
	}

	// Get raw FDB database
	fdbDB, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Transaction 1: Add write conflict and hold transaction open
	tx1, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 1: %v", err)
	}
	defer tx1.Cancel()

	rtx1 := recordlayer.NewFDBRecordContext(tx1)
	store1, err := recordlayer.NewStoreBuilder().
		SetContext(rtx1).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx1: %v", err)
	}

	// Add write conflict on the record
	store1.AddRecordWriteConflict(tuple.Tuple{int64(3002)})

	// Transaction 2: Try to read the same record with SERIALIZABLE isolation
	tx2, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 2: %v", err)
	}
	defer tx2.Cancel()

	rtx2 := recordlayer.NewFDBRecordContext(tx2)
	store2, err := recordlayer.NewStoreBuilder().
		SetContext(rtx2).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx2: %v", err)
	}

	// Read the record in tx2 (serializable - adds to conflict range)
	_, err = store2.RecordExists(tuple.Tuple{int64(3002)}, recordlayer.IsolationLevelSerializable)
	if err != nil {
		t.Fatalf("RecordExists failed in tx2: %v", err)
	}

	// Commit tx1 first
	err = tx1.Commit().Get()
	if err != nil {
		t.Fatalf("Transaction 1 commit failed: %v", err)
	}

	// Now try to commit tx2 - should fail due to read conflict
	err = tx2.Commit().Get()
	if err == nil {
		t.Fatal("Transaction 2 should have failed with conflict after tx1 added write conflict")
	}

	// Verify it's a conflict error
	if fdbErr, ok := err.(fdb.Error); ok {
		if fdbErr.Code != 1020 { // FDB error code for not_committed
			t.Fatalf("Expected conflict error (1020), got: %v (code %d)", err, fdbErr.Code)
		}
	} else {
		t.Fatalf("Expected FDB error, got: %v", err)
	}
}

// TestConflictRange_CoversAllRecordTypes verifies that conflict ranges cover
// all record type variants for a given primary key.
//
// Java behavior: getRangeForRecord uses TupleRange.allOf() which creates a range
// covering all possible record type suffixes for the primary key.
func TestConflictRange_CoversAllRecordTypes(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "conflict_range_all_types_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata with multiple record types
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	recordMetaData := builder.Build()

	// Insert both Order and Customer with same primary key value
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Insert Order with key 3003
		order := &gen.Order{
			OrderId: proto.Int64(3003),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Daisy"),
				Color: gen.Color_PINK.Enum(),
			},
		}
		_, err = store.InsertRecord(order)
		if err != nil {
			return nil, err
		}

		// Insert Customer with key 3003 (same key value, different type)
		customer := &gen.Customer{
			CustomerId: proto.Int64(3003),
			Name:       proto.String("John Doe"),
			Email:      proto.String("john@example.com"),
		}
		_, err = store.InsertRecord(customer)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to insert test records: %v", err)
	}

	// Get raw FDB database
	fdbDB, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Transaction 1: Add read conflict on primary key 3003
	tx1, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 1: %v", err)
	}
	defer tx1.Cancel()

	rtx1 := recordlayer.NewFDBRecordContext(tx1)
	store1, err := recordlayer.NewStoreBuilder().
		SetContext(rtx1).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx1: %v", err)
	}

	// Add read conflict for key 3003 (should cover BOTH Order and Customer)
	store1.AddRecordReadConflict(tuple.Tuple{int64(3003)})

	// Transaction 2: Try to modify the Order
	tx2, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 2: %v", err)
	}
	defer tx2.Cancel()

	rtx2 := recordlayer.NewFDBRecordContext(tx2)
	store2, err := recordlayer.NewStoreBuilder().
		SetContext(rtx2).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx2: %v", err)
	}

	// Modify Order in tx2
	order := &gen.Order{
		OrderId: proto.Int64(3003),
		Price:   proto.Int32(200), // Modified
		Flower: &gen.Flower{
			Type:  proto.String("Daisy"),
			Color: gen.Color_RED.Enum(), // Modified
		},
	}
	_, err = store2.SaveRecord(order)
	if err != nil {
		t.Fatalf("SaveRecord failed in tx2: %v", err)
	}

	// Commit tx2
	err = tx2.Commit().Get()
	if err != nil {
		t.Fatalf("Transaction 2 commit failed: %v", err)
	}

	// tx1 should fail (conflict on Order modification)
	err = tx1.Commit().Get()
	if err == nil {
		t.Fatal("Transaction 1 should fail - conflict range should cover all record types")
	}

	// Verify it's a conflict error
	if fdbErr, ok := err.(fdb.Error); ok {
		if fdbErr.Code != 1020 {
			t.Fatalf("Expected conflict error (1020), got: %v (code %d)", err, fdbErr.Code)
		}
	}

	// Now test modifying Customer also conflicts
	tx3, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 3: %v", err)
	}
	defer tx3.Cancel()

	rtx3 := recordlayer.NewFDBRecordContext(tx3)
	store3, err := recordlayer.NewStoreBuilder().
		SetContext(rtx3).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx3: %v", err)
	}

	// Add read conflict again
	store3.AddRecordReadConflict(tuple.Tuple{int64(3003)})

	// Transaction 4: Modify Customer
	tx4, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 4: %v", err)
	}
	defer tx4.Cancel()

	rtx4 := recordlayer.NewFDBRecordContext(tx4)
	store4, err := recordlayer.NewStoreBuilder().
		SetContext(rtx4).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx4: %v", err)
	}

	customer := &gen.Customer{
		CustomerId: proto.Int64(3003),
		Name:       proto.String("Jane Doe"), // Modified
		Email:      proto.String("jane@example.com"), // Modified
	}
	_, err = store4.SaveRecord(customer)
	if err != nil {
		t.Fatalf("SaveRecord failed in tx4: %v", err)
	}

	err = tx4.Commit().Get()
	if err != nil {
		t.Fatalf("Transaction 4 commit failed: %v", err)
	}

	// tx3 should also fail (conflict on Customer modification)
	err = tx3.Commit().Get()
	if err == nil {
		t.Fatal("Transaction 3 should fail - conflict range should cover Customer type too")
	}

	if fdbErr, ok := err.(fdb.Error); ok {
		if fdbErr.Code != 1020 {
			t.Fatalf("Expected conflict error (1020), got: %v (code %d)", err, fdbErr.Code)
		}
	}
}

// TestMultipleConflicts_SameKey_Idempotent verifies that adding multiple
// conflicts on the same key is idempotent.
//
// Java behavior: Multiple conflict additions should not cause issues
func TestMultipleConflicts_SameKey_Idempotent(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "multiple_conflicts_idempotent_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Run transaction with multiple conflict additions
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Add multiple read conflicts on same key (should be idempotent)
		store.AddRecordReadConflict(tuple.Tuple{int64(3004)})
		store.AddRecordReadConflict(tuple.Tuple{int64(3004)})
		store.AddRecordReadConflict(tuple.Tuple{int64(3004)})

		// Add multiple write conflicts on same key (should be idempotent)
		store.AddRecordWriteConflict(tuple.Tuple{int64(3004)})
		store.AddRecordWriteConflict(tuple.Tuple{int64(3004)})
		store.AddRecordWriteConflict(tuple.Tuple{int64(3004)})

		// Should commit successfully (idempotent operations)
		return nil, nil
	})

	if err != nil {
		t.Fatalf("Transaction should succeed with multiple conflict additions: %v", err)
	}
}

// TestConflictRange_DifferentKeys_Independent verifies that conflict ranges
// for different keys are independent.
//
// Java behavior: Each primary key has its own conflict range
func TestConflictRange_DifferentKeys_Independent(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "conflict_range_independent_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Insert two records
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		order1 := &gen.Order{
			OrderId: proto.Int64(3005),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}
		_, err = store.InsertRecord(order1)
		if err != nil {
			return nil, err
		}

		order2 := &gen.Order{
			OrderId: proto.Int64(3006),
			Price:   proto.Int32(200),
			Flower: &gen.Flower{
				Type:  proto.String("Lily"),
				Color: gen.Color_BLUE.Enum(),
			},
		}
		_, err = store.InsertRecord(order2)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to insert test records: %v", err)
	}

	// Get raw FDB database
	fdbDB, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Transaction 1: Add read conflict on key 3005
	tx1, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 1: %v", err)
	}
	defer tx1.Cancel()

	rtx1 := recordlayer.NewFDBRecordContext(tx1)
	store1, err := recordlayer.NewStoreBuilder().
		SetContext(rtx1).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx1: %v", err)
	}

	// Add read conflict only on key 3005
	store1.AddRecordReadConflict(tuple.Tuple{int64(3005)})

	// Transaction 2: Modify key 3006 (different key - should not conflict)
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
			OrderId: proto.Int64(3006), // Different key
			Price:   proto.Int32(300), // Modified
			Flower: &gen.Flower{
				Type:  proto.String("Lily"),
				Color: gen.Color_YELLOW.Enum(), // Modified
			},
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Transaction 2 failed: %v", err)
	}

	// tx1 should commit successfully (no conflict on different key)
	err = tx1.Commit().Get()
	if err != nil {
		t.Fatalf("Transaction 1 should succeed - conflict on different key should not affect it: %v", err)
	}
}

// TestAddRecordWriteConflict_SelfConsistent verifies that adding a write conflict
// and then writing in the same transaction doesn't cause self-conflict.
//
// Java behavior: Conflicts only affect other transactions, not the current one
func TestAddRecordWriteConflict_SelfConsistent(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "write_conflict_self_consistent_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Transaction: Add write conflict and then write in same transaction
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Add write conflict
		store.AddRecordWriteConflict(tuple.Tuple{int64(3007)})

		// Now write to the same key in the same transaction
		order := &gen.Order{
			OrderId: proto.Int64(3007),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Tulip"),
				Color: gen.Color_PINK.Enum(),
			},
		}

		// Should succeed (no self-conflict)
		_, err = store.InsertRecord(order)
		return nil, err
	})

	if err != nil {
		t.Fatalf("Transaction should succeed - no self-conflict: %v", err)
	}
}
