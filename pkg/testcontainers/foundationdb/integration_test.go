package foundationdb_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/gen"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	"google.golang.org/protobuf/proto"
)

func TestGoWriteGoReadWithTestcontainer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("testcontainer_conformance"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer container.Terminate(ctx)

	path, err := container.ClusterFilePath(ctx)
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

	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	recordMetaData, err := metaDataBuilder.Build()
	if err != nil {
		t.Fatalf("Failed to build metadata: %v", err)
	}

	keyspace := subspace.FromBytes(tuple.Tuple{"testcontainer_conformance"}.Pack())

	// Write test data.
	_, err = recordDB.Run(ctx, func(ctx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

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
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	// Read test data back.
	result, err := recordDB.Run(ctx, func(ctx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{int64(3003)})
		if err != nil {
			return nil, err
		}
		if storedRecord == nil {
			return nil, nil
		}
		order, ok := storedRecord.Record.(*gen.Order)
		if !ok {
			return nil, fmt.Errorf("expected *gen.Order, got %T", storedRecord.Record)
		}
		return order, nil
	})
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}

	orderData := result.(*gen.Order)
	if orderData.OrderId == nil || *orderData.OrderId != 3003 {
		t.Fatalf("Expected order ID 3003, got %v", orderData.OrderId)
	}
	if orderData.Price == nil || *orderData.Price != 75 {
		t.Fatalf("Expected price 75, got %v", orderData.Price)
	}
	if orderData.Flower == nil || orderData.Flower.Type == nil || *orderData.Flower.Type != "Sunflower" {
		t.Fatalf("Expected flower 'Sunflower', got %v", orderData.Flower)
	}
}

func TestMultipleContainersIsolation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container1, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("test_db_1"),
	)
	if err != nil {
		t.Fatalf("Failed to start first container: %v", err)
	}
	defer container1.Terminate(ctx)

	container2, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("test_db_2"),
	)
	if err != nil {
		t.Fatalf("Failed to start second container: %v", err)
	}
	defer container2.Terminate(ctx)

	connStr1, err := container1.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString 1: %v", err)
	}
	connStr2, err := container2.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString 2: %v", err)
	}

	if connStr1 == connStr2 {
		t.Fatalf("Expected different connection strings, got same: %s", connStr1)
	}

	if container1.Database() == container2.Database() {
		t.Fatalf("Expected different database names")
	}
}

func TestFoundationDBContainerConfiguration(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDatabase("custom_test_db"),
		foundationdb.WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer container.Terminate(ctx)

	if container.Database() != "custom_test_db" {
		t.Fatalf("Expected database 'custom_test_db', got: %s", container.Database())
	}
	if container.APIVersion() != 720 {
		t.Fatalf("Expected API version 720, got: %d", container.APIVersion())
	}

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	if !strings.Contains(connStr, ":") {
		t.Fatalf("Expected connection string to contain port, got: %s", connStr)
	}
}
