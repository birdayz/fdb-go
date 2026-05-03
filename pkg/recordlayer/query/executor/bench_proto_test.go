package executor

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// Benchmarks for the protoreflect hot path: protoToMap, scalarProtoToGo,
// goToProtoValue. These run on every record scan and every UPDATE/INSERT
// write — the dominant cost in query execution after FDB round-trips.

func makeOrder(id int64, price int32, qty int32) *gen.Order {
	return &gen.Order{
		OrderId:    proto.Int64(id),
		Price:      proto.Int32(price),
		Quantity:   proto.Int32(qty),
		Tags:       []string{"bulk", "priority"},
		VectorData: []byte{0x01, 0x02, 0x03, 0x04},
	}
}

func makeTypedRecord() *gen.TypedRecord {
	return &gen.TypedRecord{
		Id:        proto.Int64(42),
		ValInt32:  proto.Int32(100),
		ValInt64:  proto.Int64(999),
		ValFloat:  proto.Float32(3.14),
		ValDouble: proto.Float64(2.71828),
		ValBool:   proto.Bool(true),
		ValString: proto.String("hello world"),
		ValBytes:  []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
}

// BenchmarkProtoToMap_Order measures protoToMap on a typical 5-field Order.
func BenchmarkProtoToMap_Order(b *testing.B) {
	msg := makeOrder(1, 100, 5)
	b.ResetTimer()
	for b.Loop() {
		_ = protoToMap(msg)
	}
}

// BenchmarkProtoToMap_TypedRecord measures protoToMap on a wider 8-field record
// exercising all scalar kinds (int32/int64/float/double/bool/string/bytes).
func BenchmarkProtoToMap_TypedRecord(b *testing.B) {
	msg := makeTypedRecord()
	b.ResetTimer()
	for b.Loop() {
		_ = protoToMap(msg)
	}
}

// BenchmarkProtoToMap_SparseRecord measures protoToMap on a record where
// most fields are unset (the Has() check should skip quickly).
func BenchmarkProtoToMap_SparseRecord(b *testing.B) {
	msg := &gen.TypedRecord{Id: proto.Int64(1)}
	b.ResetTimer()
	for b.Loop() {
		_ = protoToMap(msg)
	}
}

// BenchmarkScalarProtoToGo_Int64 measures the fast path for int64 conversion.
func BenchmarkScalarProtoToGo_Int64(b *testing.B) {
	v := protoreflect.ValueOfInt64(42)
	b.ResetTimer()
	for b.Loop() {
		_ = scalarProtoToGo(protoreflect.Int64Kind, v)
	}
}

// BenchmarkScalarProtoToGo_String measures string conversion.
func BenchmarkScalarProtoToGo_String(b *testing.B) {
	v := protoreflect.ValueOfString("hello world")
	b.ResetTimer()
	for b.Loop() {
		_ = scalarProtoToGo(protoreflect.StringKind, v)
	}
}

// BenchmarkScalarProtoToGo_Float64 measures double conversion.
func BenchmarkScalarProtoToGo_Float64(b *testing.B) {
	v := protoreflect.ValueOfFloat64(3.14159)
	b.ResetTimer()
	for b.Loop() {
		_ = scalarProtoToGo(protoreflect.DoubleKind, v)
	}
}

// BenchmarkGoToProtoValue_Int64 measures the reverse path: Go int64 → proto.
func BenchmarkGoToProtoValue_Int64(b *testing.B) {
	msg := &gen.TypedRecord{}
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("val_int64")
	b.ResetTimer()
	for b.Loop() {
		_, _ = goToProtoValue(fd, int64(42))
	}
}

// BenchmarkGoToProtoValue_String measures Go string → proto.
func BenchmarkGoToProtoValue_String(b *testing.B) {
	msg := &gen.TypedRecord{}
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("val_string")
	b.ResetTimer()
	for b.Loop() {
		_, _ = goToProtoValue(fd, "hello")
	}
}

// BenchmarkGoToProtoValue_Bool measures Go bool → proto.
func BenchmarkGoToProtoValue_Bool(b *testing.B) {
	msg := &gen.TypedRecord{}
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("val_bool")
	b.ResetTimer()
	for b.Loop() {
		_, _ = goToProtoValue(fd, true)
	}
}

// BenchmarkProtoReflect_DescriptorLookup measures the cost of descriptor
// field lookup by name — done once per column per query but worth
// understanding.
func BenchmarkProtoReflect_DescriptorLookup(b *testing.B) {
	msg := &gen.TypedRecord{}
	desc := msg.ProtoReflect().Descriptor().Fields()
	b.ResetTimer()
	for b.Loop() {
		_ = desc.ByName("val_string")
	}
}

// BenchmarkProtoReflect_HasField measures the Has() check used by
// protoToMap to skip unset fields.
func BenchmarkProtoReflect_HasField(b *testing.B) {
	msg := makeTypedRecord()
	refl := msg.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("val_int64")
	b.ResetTimer()
	for b.Loop() {
		_ = refl.Has(fd)
	}
}

// BenchmarkProtoReflect_GetField measures Get() on a set field.
func BenchmarkProtoReflect_GetField(b *testing.B) {
	msg := makeTypedRecord()
	refl := msg.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("val_int64")
	b.ResetTimer()
	for b.Loop() {
		_ = refl.Get(fd)
	}
}

// BenchmarkProtoReflect_SetField measures Set() — the write path for
// UPDATE plans.
func BenchmarkProtoReflect_SetField(b *testing.B) {
	msg := makeTypedRecord()
	refl := msg.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("val_int64")
	pv := protoreflect.ValueOfInt64(99)
	b.ResetTimer()
	for b.Loop() {
		refl.Set(fd, pv)
	}
}

// BenchmarkProtoReflect_FullRoundTrip measures the complete read→map→write
// cycle: protoToMap (scan side) then goToProtoValue + Set (update side).
func BenchmarkProtoReflect_FullRoundTrip(b *testing.B) {
	msg := makeOrder(1, 100, 5)
	refl := msg.ProtoReflect()
	priceFD := refl.Descriptor().Fields().ByName("price")
	b.ResetTimer()
	for b.Loop() {
		m := protoToMap(msg)
		_ = m
		pv, _ := goToProtoValue(priceFD, int64(200))
		refl.Set(priceFD, pv)
	}
}
