package recordlayer

import (
	"strings"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Test data - a serialized UnionDescriptor with an Order
var testUnionData []byte

// Large test data - Order with 50KB notes field
var testUnionDataLarge []byte

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

	// Large record with 50KB padding
	largeOrder := &gen.Order{
		OrderId: proto.Int64(2001),
		Price:   proto.Int32(999),
		Flower: &gen.Flower{
			Type:  proto.String(strings.Repeat("X", 50_000)),
			Color: gen.Color_BLUE.Enum(),
		},
	}
	largeUnion := &gen.UnionDescriptor{
		XOrder: largeOrder,
	}
	testUnionDataLarge, err = proto.Marshal(largeUnion)
	if err != nil {
		panic(err)
	}
}

// BenchmarkProtoMarshal_Small measures proto.Marshal of a small Order (~30 bytes).
func BenchmarkProtoMarshal_Small(b *testing.B) {
	order := &gen.Order{
		OrderId: proto.Int64(1001),
		Price:   proto.Int32(25),
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_RED.Enum(),
		},
	}
	union := &gen.UnionDescriptor{XOrder: order}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := proto.Marshal(union)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoMarshal_Large measures proto.Marshal of a 50KB Order.
func BenchmarkProtoMarshal_Large(b *testing.B) {
	order := &gen.Order{
		OrderId: proto.Int64(2001),
		Price:   proto.Int32(999),
		Flower: &gen.Flower{
			Type:  proto.String(strings.Repeat("X", 50_000)),
			Color: gen.Color_BLUE.Enum(),
		},
	}
	union := &gen.UnionDescriptor{XOrder: order}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := proto.Marshal(union)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoUnmarshal_Small measures proto.Unmarshal of a small Order (~30 bytes).
func BenchmarkProtoUnmarshal_Small(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		union := &gen.UnionDescriptor{}
		if err := proto.Unmarshal(testUnionData, union); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoUnmarshal_Large measures proto.Unmarshal of a 50KB Order.
func BenchmarkProtoUnmarshal_Large(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		union := &gen.UnionDescriptor{}
		if err := proto.Unmarshal(testUnionDataLarge, union); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeserializeAndDiscover_Small measures the full deserialize+discover
// path used during record scan (unmarshal + walk union fields to find record type).
func BenchmarkDeserializeAndDiscover_Small(b *testing.B) {
	metaData := testMetaData(b)
	store := &FDBRecordStore{metaData: metaData}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := store.deserializeAndDiscover(testUnionData)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeserializeAndDiscover_Large measures deserialize+discover for 50KB record.
func BenchmarkDeserializeAndDiscover_Large(b *testing.B) {
	metaData := testMetaData(b)
	store := &FDBRecordStore{metaData: metaData}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := store.deserializeAndDiscover(testUnionDataLarge)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoMarshalVT_Small measures vtprotobuf MarshalVT of a small Order.
func BenchmarkProtoMarshalVT_Small(b *testing.B) {
	order := &gen.Order{
		OrderId: proto.Int64(1001),
		Price:   proto.Int32(25),
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_RED.Enum(),
		},
	}
	union := &gen.UnionDescriptor{XOrder: order}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := union.MarshalVT()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoMarshalVT_Large measures vtprotobuf MarshalVT of a 50KB Order.
func BenchmarkProtoMarshalVT_Large(b *testing.B) {
	order := &gen.Order{
		OrderId: proto.Int64(2001),
		Price:   proto.Int32(999),
		Flower: &gen.Flower{
			Type:  proto.String(strings.Repeat("X", 50_000)),
			Color: gen.Color_BLUE.Enum(),
		},
	}
	union := &gen.UnionDescriptor{XOrder: order}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := union.MarshalVT()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoUnmarshalVT_Small measures vtprotobuf UnmarshalVT of a small Order.
func BenchmarkProtoUnmarshalVT_Small(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		union := &gen.UnionDescriptor{}
		if err := union.UnmarshalVT(testUnionData); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoUnmarshalVT_Large measures vtprotobuf UnmarshalVT of a 50KB Order.
func BenchmarkProtoUnmarshalVT_Large(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		union := &gen.UnionDescriptor{}
		if err := union.UnmarshalVT(testUnionDataLarge); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProtoUnmarshalVTPool_Small measures vtprotobuf UnmarshalVT with pool reuse.
func BenchmarkProtoUnmarshalVTPool_Small(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		union := gen.UnionDescriptorFromVTPool()
		if err := union.UnmarshalVT(testUnionData); err != nil {
			b.Fatal(err)
		}
		union.ReturnToVTPool()
	}
}

// BenchmarkProtoUnmarshalVTPool_Large measures vtprotobuf UnmarshalVT with pool for 50KB.
func BenchmarkProtoUnmarshalVTPool_Large(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		union := gen.UnionDescriptorFromVTPool()
		if err := union.UnmarshalVT(testUnionDataLarge); err != nil {
			b.Fatal(err)
		}
		union.ReturnToVTPool()
	}
}

func BenchmarkDeserializeRecord_Standard(b *testing.B) {
	metaData := testMetaData(b)

	store := &FDBRecordStore{
		metaData: metaData,
	}

	recordType := metaData.GetRecordType("Order")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := store.deserializeRecord(testUnionData, recordType)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// TestDeserializeWithUnknownFields is a regression test for the bug where
// deserializeAndDiscover assumed the first proto field was the record field.
// If unknown fields precede the record field (forward compat, extensions),
// the lookup would fail. The fix scans all fields until a known one is found.
func TestDeserializeWithUnknownFields(t *testing.T) {
	t.Parallel()
	metaData := testMetaData(t)
	store := &FDBRecordStore{metaData: metaData}

	// Build valid union data, then prepend an unknown field.
	// Unknown field: field number 999, wire type 2 (bytes), value "junk".
	unknownField := protowire.AppendTag(nil, 999, protowire.BytesType)
	unknownField = protowire.AppendBytes(unknownField, []byte("junk"))
	dataWithUnknown := append(unknownField, testUnionData...)

	// deserializeAndDiscover must skip the unknown field and find the Order
	rt, msg, err := store.deserializeAndDiscover(dataWithUnknown)
	if err != nil {
		t.Fatalf("deserializeAndDiscover with unknown field: %v", err)
	}
	if rt.Name != "Order" {
		t.Errorf("expected Order, got %s", rt.Name)
	}
	order := msg.(*gen.Order)
	if order.GetOrderId() != 1001 {
		t.Errorf("expected OrderId 1001, got %d", order.GetOrderId())
	}

	// deserializeRecord with known type must also skip unknown fields
	recordType := metaData.GetRecordType("Order")
	msg2, err := store.deserializeRecord(dataWithUnknown, recordType)
	if err != nil {
		t.Fatalf("deserializeRecord with unknown field: %v", err)
	}
	order2 := msg2.(*gen.Order)
	if order2.GetOrderId() != 1001 {
		t.Errorf("expected OrderId 1001, got %d", order2.GetOrderId())
	}
}

// Test basic deserialization works correctly
func TestDeserializationWorks(t *testing.T) {
	// Use proper builder to set up union field descriptors
	metaData := testMetaData(t)

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
