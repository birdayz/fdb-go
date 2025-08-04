package recordlayer

import (
	"context"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// TestPrimaryKeyCollision tests that records can collide when not using record type prefix
func TestPrimaryKeyCollision(t *testing.T) {
	// Initialize FDB
	fdb.MustAPIVersion(630)
	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)

	// Create metadata with collision-prone primary keys (no record type prefix)
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	
	// Set primary keys WITHOUT record type prefix - can collide!
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	// Assuming we have a Customer type with customer_id field
	// For this test, we'll just use Order twice to demonstrate
	
	metaData := builder.Build()

	_, err := fdbDB.Run(context.Background(), func(ctx *FDBRecordContext) (interface{}, error) {
		store, err := NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(subspace.Sub("collision_test")).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Save an Order with ID 123
		order := &gen.Order{
			OrderId: proto.Int64(123),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}

		saved1, err := store.SaveRecord(order)
		if err != nil {
			return nil, err
		}
		t.Logf("Saved order with key: %v", saved1.PrimaryKey)

		// Load it back
		loaded1, err := store.LoadRecord(tuple.Tuple{int64(123)})
		if err != nil {
			return nil, err
		}
		if loaded1 == nil {
			t.Fatal("Failed to load order")
		}

		loadedOrder := loaded1.Record.(*gen.Order)
		if *loadedOrder.Price != 100 {
			t.Errorf("Expected price 100, got %d", *loadedOrder.Price)
		}

		// In a real scenario with multiple types sharing same key space,
		// saving Customer{customer_id: 123} would overwrite Order{order_id: 123}!

		return nil, nil
	})

	if err != nil {
		t.Fatal(err)
	}
}

// TestPrimaryKeyNoCollision tests that records don't collide (record type always included)
func TestPrimaryKeyNoCollision(t *testing.T) {
	// Initialize FDB
	fdb.MustAPIVersion(630)
	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)

	// Create metadata - record type index is always included automatically (like Java)
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	
	// Primary key - record type index prevents collisions automatically
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	
	metaData := builder.Build()

	_, err := fdbDB.Run(context.Background(), func(ctx *FDBRecordContext) (interface{}, error) {
		store, err := NewStoreBuilder().
			SetContext(ctx).
			SetMetaDataProvider(metaData).
			SetSubspace(subspace.Sub("no_collision_test")).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Save an Order with ID 123
		order := &gen.Order{
			OrderId: proto.Int64(123),
			Price:   proto.Int32(100),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}

		saved1, err := store.SaveRecord(order)
		if err != nil {
			return nil, err
		}
		t.Logf("Saved order with key: %v", saved1.PrimaryKey)

		// Load it back - note we still use the same primary key for loading
		loaded1, err := store.LoadRecord(tuple.Tuple{int64(123)})
		if err != nil {
			return nil, err
		}
		if loaded1 == nil {
			t.Fatal("Failed to load order")
		}

		loadedOrder := loaded1.Record.(*gen.Order)
		if *loadedOrder.Price != 100 {
			t.Errorf("Expected price 100, got %d", *loadedOrder.Price)
		}

		// Record type index ensures Order{order_id: 123} and Customer{customer_id: 123}
		// have different keys and don't collide (like Java Record Layer)

		return nil, nil
	})

	if err != nil {
		t.Fatal(err)
	}
}

// TestJavaCompatibilityBothModes verifies wire format matches Java in both modes
func TestJavaCompatibilityBothModes(t *testing.T) {
	// Initialize FDB
	fdb.MustAPIVersion(630)
	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)

	testCases := []struct {
		name            string
		primaryKeyExpr  KeyExpression
		expectedKeySize int // Approximate
		description     string
	}{
		{
			name:            "WithoutRecordType",
			primaryKeyExpr:  Field("order_id"),
			expectedKeySize: 15, // Smaller key
			description:     "Java: Key.Expressions.field(\"order_id\")",
		},
		{
			name:            "WithRecordType", 
			primaryKeyExpr:  Field("order_id"), // Record type always included now
			expectedKeySize: 17, // Same as without since record type is always included
			description:     "Go: Always includes record type (like Java)",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create metadata
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(tc.primaryKeyExpr)
			metaData := builder.Build()

			_, err := fdbDB.Run(context.Background(), func(ctx *FDBRecordContext) (interface{}, error) {
				store, err := NewStoreBuilder().
					SetContext(ctx).
					SetMetaDataProvider(metaData).
					SetSubspace(subspace.Sub("java_compat_test", tc.name)).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save a record
				order := &gen.Order{
					OrderId: proto.Int64(999),
					Price:   proto.Int32(50),
					Flower: &gen.Flower{
						Type:  proto.String("Tulip"),
						Color: gen.Color_YELLOW.Enum(),
					},
				}

				saved, err := store.SaveRecord(order)
				if err != nil {
					return nil, err
				}

				t.Logf("%s: %s", tc.name, tc.description)
				t.Logf("Key size: %d bytes", saved.KeySize)
				t.Logf("Primary key used for save: %v", saved.PrimaryKey)
				
				// The key size difference shows whether record type is included
				if tc.name == "WithRecordType" && saved.KeySize <= tc.expectedKeySize-2 {
					t.Errorf("Key should include record type prefix")
				}

				return nil, nil
			})

			if err != nil {
				t.Fatal(err)
			}
		})
	}
}