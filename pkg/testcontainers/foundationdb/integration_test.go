package foundationdb_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/testcontainers/testcontainers-go"

	"github.com/birdayz/fdb-record-layer-go/gen"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"google.golang.org/protobuf/proto"
)

func TestGoWriteGoReadWithTestcontainer(t *testing.T) {
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()

	// Start FoundationDB testcontainer
	container, err := foundationdb.Run(setupCtx, "",
		foundationdb.WithDatabase("testcontainer_conformance"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start FoundationDB container: %v", err)
	}
	defer func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	// Initialize database
	err = container.InitializeDatabase(setupCtx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Test socat proxy setup
	t.Logf("Testing FoundationDB with socat proxy...")

	path, err := container.ClusterFilePath(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	gofdb.MustAPIVersion(730)
	db, err := gofdb.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	recordDB := recordlayer.NewFDBDatabase(db)

	// Setup metadata
	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	recordMetaData, err := metaDataBuilder.Build()
	if err != nil {
		t.Fatalf("Failed to build metadata: %v", err)
	}

	// Create test subspace
	keyspace := subspace.FromBytes(tuple.Tuple{"testcontainer_conformance"}.Pack())

	// Write test data
	err = writeTestDataWithGoContainer(recordDB, recordMetaData, keyspace)
	if err != nil {
		t.Fatalf("Failed to write test data: %v", err)
	}

	// Read test data back
	orderData, err := readTestDataWithGoContainer(recordDB, recordMetaData, keyspace, 3003)
	if err != nil {
		t.Fatalf("Failed to read test data: %v", err)
	}

	// Verify the data
	if orderData.OrderId == nil || *orderData.OrderId != 3003 {
		t.Fatalf("Expected order ID 3003, got %v", orderData.OrderId)
	}
	if orderData.Price == nil || *orderData.Price != 75 {
		t.Fatalf("Expected price 75, got %v", orderData.Price)
	}
	if orderData.Flower == nil || orderData.Flower.Type == nil || *orderData.Flower.Type != "Sunflower" {
		t.Fatalf("Expected flower type 'Sunflower', got %v", orderData.Flower)
	}

	t.Logf("Testcontainer conformance test passed! Go write/read cycle successful: order_id=%d, price=%d, flower=%s",
		*orderData.OrderId, *orderData.Price, *orderData.Flower.Type)
}

func TestMultipleContainersIsolation(t *testing.T) {
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()

	// Start two separate FoundationDB containers
	container1, err := foundationdb.Run(setupCtx, "",
		foundationdb.WithDatabase("test_db_1"),
	)
	if err != nil {
		t.Fatalf("Failed to start first container: %v", err)
	}
	defer func() {
		if err := container1.Terminate(context.Background()); err != nil {
			t.Logf("Failed to terminate container1: %v", err)
		}
	}()

	container2, err := foundationdb.Run(setupCtx, "",
		foundationdb.WithDatabase("test_db_2"),
	)
	if err != nil {
		t.Fatalf("Failed to start second container: %v", err)
	}
	defer func() {
		if err := container2.Terminate(context.Background()); err != nil {
			t.Logf("Failed to terminate container2: %v", err)
		}
	}()

	// Verify containers are isolated
	connStr1, err := container1.ConnectionString(setupCtx)
	if err != nil {
		t.Fatalf("Failed to get connection string 1: %v", err)
	}

	connStr2, err := container2.ConnectionString(setupCtx)
	if err != nil {
		t.Fatalf("Failed to get connection string 2: %v", err)
	}

	if connStr1 == connStr2 {
		t.Fatalf("Expected different connection strings, got same: %s", connStr1)
	}

	// Verify different database names
	if container1.Database() == container2.Database() {
		t.Fatalf("Expected different database names")
	}

	t.Logf("Container isolation test passed: %s != %s", connStr1, connStr2)
}

func writeTestDataWithGoContainer(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, keyspace subspace.Subspace) error {
	_, err := recordDB.Run(context.Background(), func(ctx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Create test order with different data than the system FDB tests
		order := &gen.Order{
			OrderId: proto.Int64(3003),
			Price:   proto.Int32(75),
			Flower: &gen.Flower{
				Type:  proto.String("Sunflower"),
				Color: gen.Color_YELLOW.Enum(),
			},
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})

	return err
}

func readTestDataWithGoContainer(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, keyspace subspace.Subspace, orderID int64) (*gen.Order, error) {
	result, err := recordDB.Run(context.Background(), func(ctx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Try to load the record we just wrote
		primaryKey := tuple.Tuple{orderID}
		storedRecord, err := store.LoadRecord(primaryKey)
		if err != nil {
			return nil, err
		}

		if storedRecord == nil {
			return nil, nil
		}

		// Extract the actual deserialized Order from the stored record
		order, ok := storedRecord.Record.(*gen.Order)
		if !ok {
			return nil, fmt.Errorf("expected *gen.Order, got %T", storedRecord.Record)
		}

		return order, nil
	})
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	return result.(*gen.Order), nil
}

func TestFoundationDBContainerConfiguration(t *testing.T) {
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()

	// Test with custom configuration
	container, err := foundationdb.Run(setupCtx, "",
		foundationdb.WithDatabase("custom_test_db"),
		foundationdb.WithAPIVersion(720),
		foundationdb.WithMemory("2GB"),
		testcontainers.WithEnv(map[string]string{
			"CUSTOM_ENV": "test_value",
		}),
	)
	if err != nil {
		t.Fatalf("Failed to start container with custom config: %v", err)
	}
	defer func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("Failed to terminate container: %v", err)
		}
	}()

	// Verify configuration
	if container.Database() != "custom_test_db" {
		t.Fatalf("Expected database 'custom_test_db', got: %s", container.Database())
	}
	if container.APIVersion() != 720 {
		t.Fatalf("Expected API version 720, got: %d", container.APIVersion())
	}

	// Verify connection works
	connStr, err := container.ConnectionString(setupCtx)
	if err != nil {
		t.Fatalf("Failed to get connection string: %v", err)
	}

	if !strings.Contains(connStr, ":") {
		t.Fatalf("Expected connection string to contain port, got: %s", connStr)
	}

	t.Logf("Configuration test passed with connection: %s", connStr)
}
