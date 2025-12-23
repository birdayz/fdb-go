package recordlayer

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// TestRecordExists_SnapshotIsolation verifies that snapshot isolation sees a consistent view
// of the database at transaction start time, even if another transaction modifies data.
//
// Java behavior: Snapshot reads do not participate in conflict detection and see the database
// state as it was when the transaction started.
func TestRecordExists_SnapshotIsolation(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "snapshot_isolation_test")
	if err != nil {
		t.Fatalf("Failed to setup test container: %v", err)
	}
	defer container.Terminate(ctx)

	// Create metadata
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := builder.Build()

	// Insert a record that both transactions will work with
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

	// Get raw FDB database for concurrent transaction testing
	fdbDB, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Start Transaction 1 (will delete the record but not commit yet)
	tx1, err := fdbDB.CreateTransaction()
	if err != nil {
		t.Fatalf("Failed to create transaction 1: %v", err)
	}
	defer tx1.Cancel()

	// Transaction 1: Delete the record (but don't commit)
	rtx1 := recordlayer.NewFDBRecordContext(tx1)
	store1, err := recordlayer.NewStoreBuilder().
		SetContext(rtx1).
		SetMetaDataProvider(recordMetaData).
		SetSubspace(keyspace).
		CreateOrOpen()
	if err != nil {
		t.Fatalf("Failed to open store in tx1: %v", err)
	}

	deleted, err := store1.DeleteRecord(tuple.Tuple{int64(2001)})
	if err != nil {
		t.Fatalf("Failed to delete record in tx1: %v", err)
	}
	if !deleted {
		t.Fatal("Expected delete to succeed in tx1")
	}

	// Transaction 2: Check existence with SNAPSHOT isolation (should see old state)
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

	// CRITICAL: Snapshot read should see the record even though tx1 deleted it
	exists, err := store2.RecordExists(tuple.Tuple{int64(2001)}, recordlayer.IsolationLevelSnapshot)
	if err != nil {
		t.Fatalf("RecordExists failed in tx2: %v", err)
	}

	if !exists {
		t.Fatal("SNAPSHOT isolation should see record (old state before tx1's delete)")
	}

	// Clean up
	tx1.Cancel()
	tx2.Cancel()
}

// TestRecordExists_SerializableIsolation verifies that serializable isolation participates
// in conflict detection and sees concurrent modifications.
//
// Java behavior: Serializable reads participate in conflict detection. If another transaction
// modifies data, serializable reads will conflict.
func TestRecordExists_SerializableIsolation(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "serializable_isolation_test")
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
			OrderId: proto.Int64(2002),
			Price:   proto.Int32(200),
			Flower: &gen.Flower{
				Type:  proto.String("Lily"),
				Color: gen.Color_BLUE.Enum(),
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

	// Transaction 1: Read with SERIALIZABLE, then try to commit after tx2 modifies
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

	// SERIALIZABLE read in tx1
	exists1, err := store1.RecordExists(tuple.Tuple{int64(2002)}, recordlayer.IsolationLevelSerializable)
	if err != nil {
		t.Fatalf("RecordExists failed in tx1: %v", err)
	}
	if !exists1 {
		t.Fatal("Record should exist before modification")
	}

	// Transaction 2: Modify the record and commit
	_, err = recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Update the record
		order := &gen.Order{
			OrderId: proto.Int64(2002),
			Price:   proto.Int32(300), // Changed price
			Flower: &gen.Flower{
				Type:  proto.String("Lily"),
				Color: gen.Color_RED.Enum(), // Changed color
			},
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to update record in tx2: %v", err)
	}

	// Try to commit tx1 - should fail due to conflict
	err = tx1.Commit().Get()
	if err == nil {
		t.Fatal("Transaction 1 should have failed with conflict after tx2 committed")
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

// TestSaveRecord_ConflictDetection_SnapshotRead verifies that snapshot reads
// do NOT cause write conflicts.
//
// Java behavior: Snapshot reads do not add to conflict ranges, so subsequent writes
// in other transactions will not conflict with them.
func TestSaveRecord_ConflictDetection_SnapshotRead(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "snapshot_read_no_conflict_test")
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
			OrderId: proto.Int64(2003),
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
		t.Fatalf("Failed to insert test record: %v", err)
	}

	// Get raw FDB database
	fdbDB, err := container.GetFDBDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to get FDB database: %v", err)
	}

	// Transaction 1: Read with SNAPSHOT isolation
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

	// SNAPSHOT read (does not add to conflict ranges)
	_, err = store1.RecordExists(tuple.Tuple{int64(2003)}, recordlayer.IsolationLevelSnapshot)
	if err != nil {
		t.Fatalf("RecordExists failed in tx1: %v", err)
	}

	// Transaction 2: Modify the same record and commit
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
			OrderId: proto.Int64(2003),
			Price:   proto.Int32(200), // Modified
			Flower: &gen.Flower{
				Type:  proto.String("Daisy"),
				Color: gen.Color_PINK.Enum(), // Modified
			},
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to update record in tx2: %v", err)
	}

	// Transaction 1 should still be able to commit (no conflict from snapshot read)
	err = tx1.Commit().Get()
	if err != nil {
		t.Fatalf("Transaction 1 should succeed (snapshot read doesn't cause conflicts): %v", err)
	}
}

// TestSaveRecord_ConflictDetection_SerializableRead verifies that serializable reads
// DO cause write conflicts.
//
// Java behavior: Serializable reads add to conflict ranges, so subsequent writes
// in other transactions will conflict.
func TestSaveRecord_ConflictDetection_SerializableRead(t *testing.T) {
	ctx := context.Background()

	// Start test container
	container, keyspace, recordDB, err := setupSharedTestContainer(ctx, "serializable_read_conflict_test")
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
			OrderId: proto.Int64(2004),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Tulip"),
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

	// Transaction 1: Read with SERIALIZABLE isolation
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

	// SERIALIZABLE read (adds to conflict ranges)
	_, err = store1.RecordExists(tuple.Tuple{int64(2004)}, recordlayer.IsolationLevelSerializable)
	if err != nil {
		t.Fatalf("RecordExists failed in tx1: %v", err)
	}

	// Transaction 2: Modify the same record and commit
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
			OrderId: proto.Int64(2004),
			Price:   proto.Int32(200), // Modified
			Flower: &gen.Flower{
				Type:  proto.String("Tulip"),
				Color: gen.Color_BLUE.Enum(), // Modified
			},
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to update record in tx2: %v", err)
	}

	// Transaction 1 should fail on commit (conflict from serializable read)
	err = tx1.Commit().Get()
	if err == nil {
		t.Fatal("Transaction 1 should have failed with conflict after tx2 wrote to the same key")
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
