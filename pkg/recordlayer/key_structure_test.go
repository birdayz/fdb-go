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

// TestKeyStructure verifies the exact key structure for both modes
func TestKeyStructure(t *testing.T) {
	// Initialize FDB
	fdb.MustAPIVersion(630)
	db := fdb.MustOpenDefault()
	fdbDB := NewFDBDatabase(db)

	testCases := []struct {
		name           string
		primaryKeyExpr KeyExpression
		orderId        int64
		expectedKey    tuple.Tuple // What we expect in the key
	}{
		{
			name:           "JavaCompatibleKeyStructure",
			primaryKeyExpr: Field("order_id"),
			orderId:        100,
			expectedKey:    tuple.Tuple{int64(100), 0}, // Order ID, then record type 0 (like Java)
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
					SetSubspace(subspace.Sub("key_structure_test", tc.name)).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save a record
				order := &gen.Order{
					OrderId: proto.Int64(tc.orderId),
					Price:   proto.Int32(50),
				}

				_, err = store.SaveRecord(order)
				if err != nil {
					return nil, err
				}

				// Read the raw key from FDB
				recordsSubspace := store.Subspace().Sub(RecordKey)
				expectedFullKey := recordsSubspace.Pack(tc.expectedKey)
				
				// Try to read with expected key
				value := ctx.Transaction().Get(expectedFullKey).MustGet()
				if value == nil {
					t.Errorf("No value found at expected key")
					
					// Debug: scan all keys to see what's there
					t.Log("Scanning all keys in records subspace:")
					iter := ctx.Transaction().GetRange(recordsSubspace, fdb.RangeOptions{
						Limit: 10,
					}).Iterator()
					
					for iter.Advance() {
						kv := iter.MustGet()
						unpacked, err := recordsSubspace.Unpack(kv.Key)
						if err == nil {
							t.Logf("  Found key: %v", unpacked)
						}
					}
				} else {
					t.Logf("✓ Found record at expected key structure: %v", tc.expectedKey)
					t.Logf("  Key size: %d bytes", len(expectedFullKey))
					t.Logf("  Value size: %d bytes", len(value))
				}

				// Verify LoadRecord works with the right primary key
				loadKey := tuple.Tuple{tc.orderId}
				loaded, err := store.LoadRecord(loadKey)
				if err != nil {
					return nil, err
				}
				if loaded == nil {
					t.Errorf("Failed to load record with primary key %v", loadKey)
				} else {
					t.Logf("✓ LoadRecord successful with primary key: %v", loadKey)
				}

				return nil, nil
			})

			if err != nil {
				t.Fatal(err)
			}
		})
	}
}