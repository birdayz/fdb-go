package recordlayer

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// Test data - a serialized UnionDescriptor with an Order
var testUnionData []byte

func init() {
	// Create test data once
	order := &gen.Order{
		OrderId: proto.Int64(1001),
		Price:   proto.Int32(25),
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_RED.Enum(),
		},
	}
	
	union := &gen.UnionDescriptor{
		XOrder: order,
	}
	
	var err error
	testUnionData, err = proto.Marshal(union)
	if err != nil {
		panic(err)
	}
}

func BenchmarkDeserializeRecord_Standard(b *testing.B) {
	metaData := NewRecordMetaData(gen.File_record_layer_demo_proto)
	
	store := &FDBRecordStore{
		metaData: metaData,
	}
	
	recordType := metaData.GetRecordType("Order")
	
	// Warmup - run a few iterations to stabilize performance
	for i := 0; i < 1000; i++ {
		_, _ = store.deserializeRecord(testUnionData, recordType)
	}
	
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := store.deserializeRecord(testUnionData, recordType)
		if err != nil {
			b.Fatal(err)
		}
	}
}


// Test basic deserialization works correctly
func TestDeserializationWorks(t *testing.T) {
	// Use proper builder to set up union field descriptors
	metaData := NewRecordMetaData(gen.File_record_layer_demo_proto)
	
	store := &FDBRecordStore{
		metaData: metaData,
	}
	
	recordType := metaData.GetRecordType("Order")
	
	// Deserialize with standard method
	msg, err := store.deserializeRecord(testUnionData, recordType)
	if err != nil {
		t.Fatalf("Deserialization failed: %v", err)
	}
	
	// Verify the result
	order := msg.(*gen.Order)
	if *order.OrderId != 1001 {
		t.Errorf("Expected OrderId 1001, got %d", *order.OrderId)
	}
	if *order.Price != 25 {
		t.Errorf("Expected Price 25, got %d", *order.Price)
	}
	if *order.Flower.Type != "Rose" {
		t.Errorf("Expected Flower type 'Rose', got %s", *order.Flower.Type)
	}
}