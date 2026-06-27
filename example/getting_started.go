package main

import (
	"context"
	"fmt"
	"log"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func main() {
	// Initialize FDB - try the version the Go bindings expect
	fdb.MustAPIVersion(720)
	db := fdb.MustOpenDefault()

	// Create RecordLayer database wrapper
	recordDB := recordlayer.NewFDBDatabase(db)

	// Get the protobuf file descriptor for our demo schema
	// This is equivalent to Java's RecordLayerDemoProto.getDescriptor()
	fileDesc := gen.File_record_layer_demo_proto

	// Create metadata - equivalent to Java's RecordMetaData.newBuilder().setRecords(...)
	metaDataBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fileDesc)

	// Set primary keys for record types - equivalent to Java's setPrimaryKey(Key.Expressions.field("order_id"))
	metaDataBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))

	// Build the metadata
	recordMetaData, err := metaDataBuilder.Build()
	if err != nil {
		log.Fatalf("Failed to build metadata: %v", err)
	}

	// Create a keyspace path (simplified for now)
	keyspace := subspace.FromBytes([]byte("record_layer_demo"))

	fmt.Println("Setting up Record Store...")

	// Example of the transaction pattern - equivalent to Java's db.run(context -> { ... })
	result, err := recordDB.Run(context.Background(), func(ctx *recordlayer.FDBRecordContext) (any, error) {
		// Create record store - equivalent to Java's FDBRecordStore.newBuilder()...
		store, err := recordlayer.NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(recordMetaData).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, fmt.Errorf("failed to open store: %w", err)
		}

		fmt.Println("Record Store opened successfully!")
		fmt.Printf("Store subspace: %v\n", store.Subspace())
		fmt.Printf("Metadata version: %d\n", recordMetaData.Version())

		// Create and save a sample Order record
		order := &gen.Order{
			OrderId: proto.Int64(1001),
			Price:   proto.Int32(25),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}

		fmt.Printf("Saving order: %v\n", order)
		savedRecord, err := store.SaveRecord(order)
		if err != nil {
			return nil, fmt.Errorf("failed to save record: %w", err)
		}

		fmt.Printf("Record saved successfully! Key: %v, Size: %d bytes\n", savedRecord.PrimaryKey, savedRecord.ValueSize)

		// Now try to load the record back
		primaryKey := tuple.Tuple{int64(1001)}
		storedRecord, err := store.LoadRecord(primaryKey)
		if err != nil {
			return nil, fmt.Errorf("failed to load record: %w", err)
		}

		if storedRecord == nil {
			fmt.Printf("No record found with key %v\n", primaryKey)
		} else {
			fmt.Printf("Found record: Key=%v, Size=%d bytes\n", storedRecord.PrimaryKey, storedRecord.ValueSize)
		}

		return "Read/Write cycle completed successfully", nil
	})

	if err != nil {
		log.Printf("Transaction failed: %v", err)
		// This is expected since we don't have FDB running
		fmt.Println("Note: This error is expected without a running FoundationDB cluster")
	} else {
		fmt.Printf("Transaction completed successfully: %v\n", result)
	}

	fmt.Println("Getting Started example completed!")
}
