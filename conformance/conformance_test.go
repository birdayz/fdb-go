package conformance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

const (
	// Test subspace name - must match Java test
	testSubspaceName = "conformance_test"
)

func TestGoWriteJavaRead(t *testing.T) {
	// Skip if FDB is not available
	if os.Getenv("SKIP_FDB_TESTS") != "" {
		t.Skip("Skipping FDB conformance tests (SKIP_FDB_TESTS set)")
	}

	// Initialize FDB
	fdb.MustAPIVersion(720)
	db := fdb.MustOpenDefault()
	recordDB := recordlayer.NewFDBDatabase(db)

	// Setup metadata - same as our getting started example
	fileDesc := gen.File_record_layer_demo_proto
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	recordMetaData := metaDataBuilder.Build()

	// Create test subspace - must match Java's new Subspace(Tuple.from(SUBSPACE_NAME))
	keyspace := subspace.FromBytes(tuple.Tuple{testSubspaceName}.Pack())

	// Write test data with Go implementation
	err := writeTestDataWithGo(recordDB, recordMetaData, keyspace)
	if err != nil {
		t.Fatalf("Failed to write test data with Go: %v", err)
	}

	// Now call Java to read the data
	javaOutput, err := callJavaReader()
	if err != nil {
		t.Fatalf("Failed to call Java reader: %v", err)
	}

	// Verify Java could read the data correctly
	expectedOutput := "SUCCESS: Found order 1001 with price 25 and flower Rose (RED)"
	// Extract just the meaningful line from the Gradle output
	lines := strings.Split(javaOutput, "\n")
	var meaningfulOutput string
	for _, line := range lines {
		if strings.HasPrefix(line, "SUCCESS:") || strings.HasPrefix(line, "ERROR:") {
			meaningfulOutput = line
			break
		}
	}
	
	if meaningfulOutput != expectedOutput {
		t.Fatalf("Java reader output mismatch.\nExpected: %s\nGot: %s\nFull output: %s", expectedOutput, meaningfulOutput, javaOutput)
	}

	t.Logf("Conformance test passed! Java successfully read Go-written data: %s", javaOutput)
}

func TestJavaWriteGoRead(t *testing.T) {
	// Skip if FDB is not available
	if os.Getenv("SKIP_FDB_TESTS") != "" {
		t.Skip("Skipping FDB conformance tests (SKIP_FDB_TESTS set)")
	}

	// Call Java to write test data
	err := callJavaWriter()
	if err != nil {
		t.Fatalf("Failed to call Java writer: %v", err)
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

	// Create test subspace - must match Java's new Subspace(Tuple.from(SUBSPACE_NAME))
	keyspace := subspace.FromBytes(tuple.Tuple{testSubspaceName}.Pack())

	// Read test data with Go implementation
	orderData, err := readTestDataWithGo(recordDB, recordMetaData, keyspace)
	if err != nil {
		t.Fatalf("Failed to read test data with Go: %v", err)
	}

	// Verify the data matches what Java wrote
	if orderData.OrderId == nil || *orderData.OrderId != 2002 {
		t.Fatalf("Expected order ID 2002, got %v", orderData.OrderId)
	}
	if orderData.Price == nil || *orderData.Price != 50 {
		t.Fatalf("Expected price 50, got %v", orderData.Price)
	}
	if orderData.Flower == nil || orderData.Flower.Type == nil || *orderData.Flower.Type != "Tulip" {
		t.Fatalf("Expected flower type 'Tulip', got %v", orderData.Flower)
	}

	t.Logf("Conformance test passed! Go successfully read Java-written data: order_id=%d, price=%d, flower=%s",
		*orderData.OrderId, *orderData.Price, *orderData.Flower.Type)
}

func writeTestDataWithGo(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, keyspace subspace.Subspace) error {
	_, err := recordDB.Run(context.Background(), func(ctx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Create test order - same structure Java expects
		order := &gen.Order{
			OrderId: proto.Int64(1001),
			Price:   proto.Int32(25),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}

		_, err = store.SaveRecord(order)
		return nil, err
	})

	return err
}

func readTestDataWithGo(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, keyspace subspace.Subspace) (*gen.Order, error) {
	result, err := recordDB.Run(context.Background(), func(ctx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Try to load the record Java wrote (order ID 2002)
		primaryKey := tuple.Tuple{int64(2002)}
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

func callJavaReader() (string, error) {
	javaDir := filepath.Join(".", "java")
	cmd := exec.Command("./gradlew", "run", "--args=read")
	cmd.Dir = javaDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), err // Return output even on error for debugging
	}

	return string(output), nil
}

func callJavaWriter() error {
	javaDir := filepath.Join(".", "java")
	cmd := exec.Command("./gradlew", "run", "--args=write")
	cmd.Dir = javaDir

	return cmd.Run()
}