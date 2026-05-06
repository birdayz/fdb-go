package functions

import (
	"math"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

func typedFD(name string) protoreflect.FieldDescriptor {
	msg := &gen.TypedRecord{}
	return msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
}

// ----- ProtoValueToDriver ---------------------------------------------------

func TestProtoValueToDriver_Bool(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bool")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfBool(true))
	if got != true {
		t.Fatalf("got %v (%T), want true", got, got)
	}
}

func TestProtoValueToDriver_Int32(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int32")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfInt32(42))
	if got != int64(42) {
		t.Fatalf("got %v (%T), want int64(42)", got, got)
	}
}

func TestProtoValueToDriver_Int64(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfInt64(math.MaxInt64))
	if got != int64(math.MaxInt64) {
		t.Fatalf("got %v, want MaxInt64", got)
	}
}

func TestProtoValueToDriver_Float(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfFloat32(3.14))
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("got %T, want float64", got)
	}
	if math.Abs(f-3.14) > 0.01 {
		t.Fatalf("got %f, want ~3.14", f)
	}
}

func TestProtoValueToDriver_Double(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfFloat64(2.71828))
	if got != 2.71828 {
		t.Fatalf("got %v, want 2.71828", got)
	}
}

func TestProtoValueToDriver_String(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_string")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfString("hello"))
	if got != "hello" {
		t.Fatalf("got %v, want hello", got)
	}
}

func TestProtoValueToDriver_Bytes(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bytes")
	data := []byte{0xDE, 0xAD}
	got := ProtoValueToDriver(fd, protoreflect.ValueOfBytes(data))
	b, ok := got.([]byte)
	if !ok || len(b) != 2 || b[0] != 0xDE || b[1] != 0xAD {
		t.Fatalf("got %v, want [0xDE 0xAD]", got)
	}
}

func TestProtoValueToDriver_Sfixed32(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_sfixed32")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfInt32(-100))
	if got != int64(-100) {
		t.Fatalf("got %v (%T), want int64(-100)", got, got)
	}
}

func TestProtoValueToDriver_Sfixed64(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_sfixed64")
	got := ProtoValueToDriver(fd, protoreflect.ValueOfInt64(-999))
	if got != int64(-999) {
		t.Fatalf("got %v, want -999", got)
	}
}

// ----- ConvertToProtoValue --------------------------------------------------

func TestConvertToProtoValue_Bool(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bool")
	pv, err := ConvertToProtoValue(fd, true)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Bool() {
		t.Fatal("expected true")
	}
}

func TestConvertToProtoValue_BoolFromInt(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bool")
	pv, err := ConvertToProtoValue(fd, int64(0))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Bool() {
		t.Fatal("int64(0) → false")
	}
	pv, err = ConvertToProtoValue(fd, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Bool() {
		t.Fatal("int64(1) → true")
	}
}

func TestConvertToProtoValue_Int32(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int32")
	pv, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if int32(pv.Int()) != 42 {
		t.Fatalf("got %d, want 42", pv.Int())
	}
}

func TestConvertToProtoValue_Int32_Overflow(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int32")
	_, err := ConvertToProtoValue(fd, int64(math.MaxInt32+1))
	if err == nil {
		t.Fatal("expected overflow error")
	}
	_, err = ConvertToProtoValue(fd, int64(math.MinInt32-1))
	if err == nil {
		t.Fatal("expected underflow error")
	}
}

func TestConvertToProtoValue_Int64(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	pv, err := ConvertToProtoValue(fd, int64(math.MaxInt64))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Int() != math.MaxInt64 {
		t.Fatalf("got %d, want MaxInt64", pv.Int())
	}
}

func TestConvertToProtoValue_Int64_FromWholeFloat(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	pv, err := ConvertToProtoValue(fd, float64(42.0))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Int() != 42 {
		t.Fatalf("got %d, want 42", pv.Int())
	}
}

func TestConvertToProtoValue_Int64_FractionalFloatReject(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	_, err := ConvertToProtoValue(fd, float64(42.5))
	if err == nil {
		t.Fatal("fractional float → int64 should error")
	}
}

func TestConvertToProtoValue_Int64_NaNReject(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	_, err := ConvertToProtoValue(fd, math.NaN())
	if err == nil {
		t.Fatal("NaN → int64 should error")
	}
}

func TestConvertToProtoValue_Float(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	pv, err := ConvertToProtoValue(fd, float64(3.14))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(float32(pv.Float()))-3.14) > 0.01 {
		t.Fatalf("got %f, want ~3.14", pv.Float())
	}
}

func TestConvertToProtoValue_Float_FromInt(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	pv, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if float32(pv.Float()) != 42.0 {
		t.Fatalf("got %f, want 42.0", pv.Float())
	}
}

func TestConvertToProtoValue_Float_NaNReject(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	_, err := ConvertToProtoValue(fd, math.NaN())
	if err == nil {
		t.Fatal("NaN → float should error")
	}
}

func TestConvertToProtoValue_Float_Overflow(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	_, err := ConvertToProtoValue(fd, float64(math.MaxFloat64))
	if err == nil {
		t.Fatal("MaxFloat64 → float32 should overflow")
	}
}

func TestConvertToProtoValue_Double(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	pv, err := ConvertToProtoValue(fd, float64(2.71828))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Float() != 2.71828 {
		t.Fatalf("got %f, want 2.71828", pv.Float())
	}
}

func TestConvertToProtoValue_Double_FromInt(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	pv, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Float() != 42.0 {
		t.Fatalf("got %f, want 42.0", pv.Float())
	}
}

func TestConvertToProtoValue_Double_NaNReject(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	_, err := ConvertToProtoValue(fd, math.NaN())
	if err == nil {
		t.Fatal("NaN → double should error")
	}
}

func TestConvertToProtoValue_String(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_string")
	pv, err := ConvertToProtoValue(fd, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if pv.String() != "hello" {
		t.Fatalf("got %s, want hello", pv.String())
	}
}

func TestConvertToProtoValue_Bytes(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bytes")
	pv, err := ConvertToProtoValue(fd, []byte{0x01, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Bytes()) != 2 {
		t.Fatalf("got %d bytes, want 2", len(pv.Bytes()))
	}
}

func TestConvertToProtoValue_Bytes_FromString(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bytes")
	pv, err := ConvertToProtoValue(fd, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(pv.Bytes()) != "hello" {
		t.Fatalf("got %s, want hello", pv.Bytes())
	}
}

func TestConvertToProtoValue_TypeMismatch(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	_, err := ConvertToProtoValue(fd, "not-an-int")
	if err == nil {
		t.Fatal("string → int64 should error")
	}
}

// ----- LiteralMatchesPKKind -------------------------------------------------

func TestLiteralMatchesPKKind_IntegerKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind,
	} {
		if !LiteralMatchesPKKind(int64(42), kind) {
			t.Errorf("int64 should match %v", kind)
		}
		if LiteralMatchesPKKind("42", kind) {
			t.Errorf("string should not match %v", kind)
		}
	}
}

func TestLiteralMatchesPKKind_StringKind(t *testing.T) {
	t.Parallel()
	if !LiteralMatchesPKKind("hello", protoreflect.StringKind) {
		t.Fatal("string should match StringKind")
	}
	if LiteralMatchesPKKind(int64(42), protoreflect.StringKind) {
		t.Fatal("int64 should not match StringKind")
	}
}

func TestLiteralMatchesPKKind_BytesKind(t *testing.T) {
	t.Parallel()
	if !LiteralMatchesPKKind([]byte{1}, protoreflect.BytesKind) {
		t.Fatal("[]byte should match BytesKind")
	}
	if LiteralMatchesPKKind("hello", protoreflect.BytesKind) {
		t.Fatal("string should not match BytesKind")
	}
}

func TestLiteralMatchesPKKind_BoolKind(t *testing.T) {
	t.Parallel()
	if LiteralMatchesPKKind(true, protoreflect.BoolKind) {
		t.Fatal("bool should not match BoolKind (not a PK type)")
	}
}

// ----- Round-trip: ConvertToProtoValue → Set → Get → ProtoValueToDriver -----

func TestRoundTrip_Int32(t *testing.T) {
	t.Parallel()
	msg := &gen.TypedRecord{}
	refl := msg.ProtoReflect()
	fd := typedFD("val_int32")
	pv, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got := ProtoValueToDriver(fd, refl.Get(fd))
	if got != int64(42) {
		t.Fatalf("round-trip: got %v (%T), want int64(42)", got, got)
	}
}

func TestRoundTrip_String(t *testing.T) {
	t.Parallel()
	msg := &gen.TypedRecord{}
	refl := msg.ProtoReflect()
	fd := typedFD("val_string")
	pv, err := ConvertToProtoValue(fd, "hello")
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got := ProtoValueToDriver(fd, refl.Get(fd))
	if got != "hello" {
		t.Fatalf("round-trip: got %v, want hello", got)
	}
}

func TestRoundTrip_Double(t *testing.T) {
	t.Parallel()
	msg := &gen.TypedRecord{}
	refl := msg.ProtoReflect()
	fd := typedFD("val_double")
	pv, err := ConvertToProtoValue(fd, float64(3.14))
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got := ProtoValueToDriver(fd, refl.Get(fd))
	if got != 3.14 {
		t.Fatalf("round-trip: got %v, want 3.14", got)
	}
}

// ----- Benchmarks -----------------------------------------------------------

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

func BenchmarkProtoValueToDriver_Int64(b *testing.B) {
	fd := typedFD("val_int64")
	v := protoreflect.ValueOfInt64(42)
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkProtoValueToDriver_String(b *testing.B) {
	fd := typedFD("val_string")
	v := protoreflect.ValueOfString("hello world")
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkProtoValueToDriver_Float(b *testing.B) {
	fd := typedFD("val_float")
	v := protoreflect.ValueOfFloat32(3.14)
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkConvertToProtoValue_Int64(b *testing.B) {
	fd := typedFD("val_int64")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, int64(42))
	}
}

func BenchmarkConvertToProtoValue_String(b *testing.B) {
	fd := typedFD("val_string")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, "hello")
	}
}

func BenchmarkConvertToProtoValue_Float64(b *testing.B) {
	fd := typedFD("val_double")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, float64(3.14))
	}
}

func BenchmarkConvertToProtoValue_Int32_RangeCheck(b *testing.B) {
	fd := typedFD("val_int32")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, int64(42))
	}
}

func BenchmarkRoundTrip_Int64(b *testing.B) {
	msg := makeTypedRecord()
	refl := msg.ProtoReflect()
	fd := typedFD("val_int64")
	for b.Loop() {
		pv, _ := ConvertToProtoValue(fd, int64(42))
		refl.Set(fd, pv)
		_ = ProtoValueToDriver(fd, refl.Get(fd))
	}
}

func BenchmarkRoundTrip_String(b *testing.B) {
	msg := makeTypedRecord()
	refl := msg.ProtoReflect()
	fd := typedFD("val_string")
	for b.Loop() {
		pv, _ := ConvertToProtoValue(fd, "hello")
		refl.Set(fd, pv)
		_ = ProtoValueToDriver(fd, refl.Get(fd))
	}
}

// =============================================================================
// Helper: field descriptors for unsigned kinds (from SortSectionHeader)
// =============================================================================

func sortSectionFD(name string) protoreflect.FieldDescriptor {
	msg := &gen.SortSectionHeader{}
	return msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
}

// =============================================================================
// ProtoValueToDriver — unsigned kinds
// =============================================================================

func TestProtoValueToDriver_Uint32(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("section_number") // fixed32
	v := protoreflect.ValueOfUint32(math.MaxUint32)
	got := ProtoValueToDriver(fd, v)
	if got != int64(math.MaxUint32) {
		t.Errorf("got %v (%T), want int64(%d)", got, got, uint32(math.MaxUint32))
	}
}

func TestProtoValueToDriver_Uint64(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("number_of_bytes") // fixed64
	v := protoreflect.ValueOfUint64(12345)
	got := ProtoValueToDriver(fd, v)
	if got != int64(12345) {
		t.Errorf("got %v (%T), want int64(12345)", got, got)
	}
}

func TestProtoValueToDriver_Enum(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_enum")
	v := protoreflect.ValueOfEnum(protoreflect.EnumNumber(gen.Color_BLUE))
	got := ProtoValueToDriver(fd, v)
	// Enum → int64 via scalarProtoToGo path, but ProtoValueToDriver has
	// no explicit enum case — falls through to default v.Interface().
	// Verify we get something usable.
	if got == nil {
		t.Fatal("got nil for enum field")
	}
}

func TestProtoValueToDriver_FloatWidensToFloat64(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	v := protoreflect.ValueOfFloat32(1.5)
	got := ProtoValueToDriver(fd, v)
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("got type %T, want float64", got)
	}
	if f != 1.5 {
		t.Errorf("got %f, want 1.5", f)
	}
}

func TestProtoValueToDriver_ZeroValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fd   protoreflect.FieldDescriptor
		v    protoreflect.Value
		want any
	}{
		{"int32_zero", typedFD("val_int32"), protoreflect.ValueOfInt32(0), int64(0)},
		{"int64_zero", typedFD("val_int64"), protoreflect.ValueOfInt64(0), int64(0)},
		{"float_zero", typedFD("val_float"), protoreflect.ValueOfFloat32(0), float64(0)},
		{"double_zero", typedFD("val_double"), protoreflect.ValueOfFloat64(0), float64(0)},
		{"bool_false", typedFD("val_bool"), protoreflect.ValueOfBool(false), false},
		{"string_empty", typedFD("val_string"), protoreflect.ValueOfString(""), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ProtoValueToDriver(tc.fd, tc.v)
			if got != tc.want {
				t.Errorf("got %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}
}

// =============================================================================
// ConvertToProtoValue — unsigned kinds, range checks
// =============================================================================

func TestConvertToProtoValue_Uint32(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("section_number") // fixed32
	v, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if uint32(v.Uint()) != 42 {
		t.Errorf("got %d, want 42", v.Uint())
	}
}

func TestConvertToProtoValue_Uint32_MaxValue(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("section_number")
	v, err := ConvertToProtoValue(fd, int64(math.MaxUint32))
	if err != nil {
		t.Fatal(err)
	}
	if uint32(v.Uint()) != math.MaxUint32 {
		t.Errorf("got %d, want MaxUint32", v.Uint())
	}
}

func TestConvertToProtoValue_Uint32_Negative(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("section_number")
	_, err := ConvertToProtoValue(fd, int64(-1))
	if err == nil {
		t.Fatal("expected error for negative value in uint32 field")
	}
}

func TestConvertToProtoValue_Uint32_Overflow(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("section_number")
	_, err := ConvertToProtoValue(fd, int64(math.MaxUint32+1))
	if err == nil {
		t.Fatal("expected error for overflow in uint32 field")
	}
}

func TestConvertToProtoValue_Uint64(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("number_of_bytes") // fixed64
	v, err := ConvertToProtoValue(fd, int64(999))
	if err != nil {
		t.Fatal(err)
	}
	if v.Uint() != 999 {
		t.Errorf("got %d, want 999", v.Uint())
	}
}

func TestConvertToProtoValue_Uint64_Negative(t *testing.T) {
	t.Parallel()
	fd := sortSectionFD("number_of_bytes")
	_, err := ConvertToProtoValue(fd, int64(-1))
	if err == nil {
		t.Fatal("expected error for negative value in uint64 field")
	}
}

func TestConvertToProtoValue_Int64_FromInt(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	_, err := ConvertToProtoValue(fd, "not_a_number")
	if err == nil {
		t.Fatal("expected error for string → int64")
	}
}

func TestConvertToProtoValue_Float_FromInt64(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	v, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if float32(v.Float()) != 42.0 {
		t.Errorf("got %f, want 42.0", v.Float())
	}
}

func TestConvertToProtoValue_Double_FromInt64(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	v, err := ConvertToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if v.Float() != 42.0 {
		t.Errorf("got %f, want 42.0", v.Float())
	}
}

func TestConvertToProtoValue_Double_NaN(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	_, err := ConvertToProtoValue(fd, math.NaN())
	if err == nil {
		t.Fatal("expected error for NaN in double field")
	}
}

func TestConvertToProtoValue_Double_Inf(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_double")
	_, err := ConvertToProtoValue(fd, math.Inf(1))
	if err == nil {
		t.Fatal("expected error for +Inf in double field")
	}
	_, err = ConvertToProtoValue(fd, math.Inf(-1))
	if err == nil {
		t.Fatal("expected error for -Inf in double field")
	}
}

func TestConvertToProtoValue_Float_Inf(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	_, err := ConvertToProtoValue(fd, math.Inf(1))
	if err == nil {
		t.Fatal("expected error for +Inf in float field")
	}
}

func TestConvertToProtoValue_Int64_FromWholeFloat_Large(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	v, err := ConvertToProtoValue(fd, float64(1e15))
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != int64(1e15) {
		t.Errorf("got %d, want %d", v.Int(), int64(1e15))
	}
}

func TestConvertToProtoValue_Int64_FromInfFloat(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	_, err := ConvertToProtoValue(fd, math.Inf(1))
	if err == nil {
		t.Fatal("expected error for Inf → int64")
	}
}

// =============================================================================
// isUUIDMessageField
// =============================================================================

func TestIsUUIDMessageField_Nil(t *testing.T) {
	t.Parallel()
	if isUUIDMessageField(nil) {
		t.Error("nil should not be UUID")
	}
}

func TestIsUUIDMessageField_NonMessage(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_int64")
	if isUUIDMessageField(fd) {
		t.Error("int64 field should not be UUID")
	}
}

func TestIsUUIDMessageField_WrongMessage(t *testing.T) {
	t.Parallel()
	fd := (&gen.Order{}).ProtoReflect().Descriptor().Fields().ByName("flower")
	if isUUIDMessageField(fd) {
		t.Error("Flower field should not be UUID")
	}
}

// =============================================================================
// UUID round-trip: string → proto message → string
// =============================================================================

func TestUUIDRoundTrip(t *testing.T) {
	t.Parallel()
	uuidMsgDesc := (&gen.UUID{}).ProtoReflect().Descriptor()
	mostFD := uuidMsgDesc.Fields().ByName("most_significant_bits")
	leastFD := uuidMsgDesc.Fields().ByName("least_significant_bits")

	// Build a UUID message, read it back as string, verify it's valid.
	dynMsg := dynamicpb.NewMessage(uuidMsgDesc)
	dynMsg.Set(mostFD, protoreflect.ValueOfInt64(0x0102030405060708))
	dynMsg.Set(leastFD, protoreflect.ValueOfInt64(0x090a0b0c0d0e0f10))

	got := uuidProtoMessageToString(dynMsg)
	want := "01020304-0506-0708-090a-0b0c0d0e0f10"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUUIDProtoMessageToString_ZeroUUID(t *testing.T) {
	t.Parallel()
	uuidMsgDesc := (&gen.UUID{}).ProtoReflect().Descriptor()
	dynMsg := dynamicpb.NewMessage(uuidMsgDesc)
	mostFD := uuidMsgDesc.Fields().ByName("most_significant_bits")
	leastFD := uuidMsgDesc.Fields().ByName("least_significant_bits")
	dynMsg.Set(mostFD, protoreflect.ValueOfInt64(0))
	dynMsg.Set(leastFD, protoreflect.ValueOfInt64(0))
	got := uuidProtoMessageToString(dynMsg)
	if got != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("zero UUID: got %q", got)
	}
}

func TestUUIDProtoMessageToString_MaxBits(t *testing.T) {
	t.Parallel()
	uuidMsgDesc := (&gen.UUID{}).ProtoReflect().Descriptor()
	dynMsg := dynamicpb.NewMessage(uuidMsgDesc)
	mostFD := uuidMsgDesc.Fields().ByName("most_significant_bits")
	leastFD := uuidMsgDesc.Fields().ByName("least_significant_bits")
	dynMsg.Set(mostFD, protoreflect.ValueOfInt64(-1)) // 0xFFFFFFFFFFFFFFFF
	dynMsg.Set(leastFD, protoreflect.ValueOfInt64(-1))
	got := uuidProtoMessageToString(dynMsg)
	if got != "ffffffff-ffff-ffff-ffff-ffffffffffff" {
		t.Errorf("max UUID: got %q", got)
	}
}

// =============================================================================
// LiteralMatchesPKKind — extended unsigned kinds
// =============================================================================

func TestLiteralMatchesPKKind_UintKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			if !LiteralMatchesPKKind(int64(42), kind) {
				t.Error("int64 should match unsigned kind")
			}
			if !LiteralMatchesPKKind(uint32(42), kind) {
				t.Error("uint32 should match unsigned kind")
			}
			if !LiteralMatchesPKKind(uint64(42), kind) {
				t.Error("uint64 should match unsigned kind")
			}
			if LiteralMatchesPKKind("42", kind) {
				t.Error("string should not match unsigned kind")
			}
			if LiteralMatchesPKKind(true, kind) {
				t.Error("bool should not match unsigned kind")
			}
		})
	}
}

func TestLiteralMatchesPKKind_AllIntTypes(t *testing.T) {
	t.Parallel()
	intVals := []any{
		int(1), int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1),
	}
	for _, val := range intVals {
		if !LiteralMatchesPKKind(val, protoreflect.Int64Kind) {
			t.Errorf("%T should match Int64Kind", val)
		}
	}
}

func TestLiteralMatchesPKKind_FloatDoesNotMatch(t *testing.T) {
	t.Parallel()
	if LiteralMatchesPKKind(3.14, protoreflect.Int64Kind) {
		t.Error("float64 should not match Int64Kind")
	}
	if LiteralMatchesPKKind(float32(3.14), protoreflect.Int64Kind) {
		t.Error("float32 should not match Int64Kind")
	}
}

func TestLiteralMatchesPKKind_UnsupportedKind(t *testing.T) {
	t.Parallel()
	if LiteralMatchesPKKind(true, protoreflect.BoolKind) {
		t.Error("BoolKind should return false (not in scope)")
	}
	if LiteralMatchesPKKind(3.14, protoreflect.DoubleKind) {
		t.Error("DoubleKind should return false (not in scope)")
	}
}

// =============================================================================
// ConvertToProtoValue + ProtoValueToDriver round-trip — all signed kinds
// =============================================================================

func TestRoundTrip_AllSignedKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		fd    protoreflect.FieldDescriptor
		input any
		want  any
	}{
		{"int32", typedFD("val_int32"), int64(42), int64(42)},
		{"int32_negative", typedFD("val_int32"), int64(-100), int64(-100)},
		{"int64", typedFD("val_int64"), int64(math.MaxInt64), int64(math.MaxInt64)},
		{"sint32", typedFD("val_sint32"), int64(-1), int64(-1)},
		{"sint64", typedFD("val_sint64"), int64(math.MinInt64), int64(math.MinInt64)},
		{"sfixed32", typedFD("val_sfixed32"), int64(999), int64(999)},
		{"sfixed64", typedFD("val_sfixed64"), int64(-999), int64(-999)},
		{"bool_true", typedFD("val_bool"), true, true},
		{"bool_false", typedFD("val_bool"), false, false},
		{"string", typedFD("val_string"), "hello", "hello"},
		{"string_empty", typedFD("val_string"), "", ""},
		{"double", typedFD("val_double"), float64(math.E), float64(math.E)},
		{"double_zero", typedFD("val_double"), float64(0), float64(0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := &gen.TypedRecord{}
			refl := msg.ProtoReflect()
			pv, err := ConvertToProtoValue(tc.fd, tc.input)
			if err != nil {
				t.Fatalf("ConvertToProtoValue: %v", err)
			}
			refl.Set(tc.fd, pv)
			got := ProtoValueToDriver(tc.fd, refl.Get(tc.fd))
			if got != tc.want {
				t.Errorf("got %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}
}

func TestRoundTrip_UnsignedKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		fd    protoreflect.FieldDescriptor
		input int64
		want  int64
	}{
		{"fixed32_zero", sortSectionFD("section_number"), 0, 0},
		{"fixed32_max", sortSectionFD("section_number"), math.MaxUint32, math.MaxUint32},
		{"fixed64_42", sortSectionFD("number_of_bytes"), 42, 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pv, err := ConvertToProtoValue(tc.fd, tc.input)
			if err != nil {
				t.Fatalf("ConvertToProtoValue: %v", err)
			}
			got := ProtoValueToDriver(tc.fd, pv)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRoundTrip_Bytes(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bytes")
	msg := &gen.TypedRecord{}
	refl := msg.ProtoReflect()

	data := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	pv, err := ConvertToProtoValue(fd, data)
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got := ProtoValueToDriver(fd, refl.Get(fd))
	b, ok := got.([]byte)
	if !ok {
		t.Fatalf("got type %T, want []byte", got)
	}
	if len(b) != 4 || b[0] != 0xDE || b[3] != 0xEF {
		t.Errorf("got %x, want DEADBEEF", b)
	}
}

func TestRoundTrip_BytesFromString(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bytes")
	pv, err := ConvertToProtoValue(fd, "hello")
	if err != nil {
		t.Fatal(err)
	}
	got := ProtoValueToDriver(fd, pv)
	b, ok := got.([]byte)
	if !ok {
		t.Fatalf("got type %T, want []byte", got)
	}
	if string(b) != "hello" {
		t.Errorf("got %q, want hello", b)
	}
}

func TestRoundTrip_Float(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_float")
	msg := &gen.TypedRecord{}
	refl := msg.ProtoReflect()

	pv, err := ConvertToProtoValue(fd, float64(1.5))
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got := ProtoValueToDriver(fd, refl.Get(fd))
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("got type %T, want float64", got)
	}
	if f != 1.5 {
		t.Errorf("got %f, want 1.5", f)
	}
}

func TestRoundTrip_BoolFromInt(t *testing.T) {
	t.Parallel()
	fd := typedFD("val_bool")
	msg := &gen.TypedRecord{}
	refl := msg.ProtoReflect()

	pv, err := ConvertToProtoValue(fd, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got := ProtoValueToDriver(fd, refl.Get(fd))
	if got != true {
		t.Errorf("got %v, want true", got)
	}

	pv, err = ConvertToProtoValue(fd, int64(0))
	if err != nil {
		t.Fatal(err)
	}
	refl.Set(fd, pv)
	got = ProtoValueToDriver(fd, refl.Get(fd))
	if got != false {
		t.Errorf("got %v, want false", got)
	}
}

// =============================================================================
// Benchmarks — unsigned kinds + edge cases
// =============================================================================

func BenchmarkProtoValueToDriver_Uint32(b *testing.B) {
	fd := sortSectionFD("section_number")
	v := protoreflect.ValueOfUint32(42)
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkProtoValueToDriver_Uint64(b *testing.B) {
	fd := sortSectionFD("number_of_bytes")
	v := protoreflect.ValueOfUint64(12345)
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkProtoValueToDriver_Bool(b *testing.B) {
	fd := typedFD("val_bool")
	v := protoreflect.ValueOfBool(true)
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkProtoValueToDriver_Bytes(b *testing.B) {
	fd := typedFD("val_bytes")
	v := protoreflect.ValueOfBytes([]byte{0x01, 0x02, 0x03, 0x04})
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkProtoValueToDriver_Double(b *testing.B) {
	fd := typedFD("val_double")
	v := protoreflect.ValueOfFloat64(3.14159)
	for b.Loop() {
		_ = ProtoValueToDriver(fd, v)
	}
}

func BenchmarkConvertToProtoValue_Bool(b *testing.B) {
	fd := typedFD("val_bool")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, true)
	}
}

func BenchmarkConvertToProtoValue_Uint32(b *testing.B) {
	fd := sortSectionFD("section_number")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, int64(42))
	}
}

func BenchmarkConvertToProtoValue_Uint64(b *testing.B) {
	fd := sortSectionFD("number_of_bytes")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, int64(42))
	}
}

func BenchmarkConvertToProtoValue_Bytes(b *testing.B) {
	fd := typedFD("val_bytes")
	data := []byte{0x01, 0x02, 0x03, 0x04}
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, data)
	}
}

func BenchmarkConvertToProtoValue_Double(b *testing.B) {
	fd := typedFD("val_double")
	for b.Loop() {
		_, _ = ConvertToProtoValue(fd, float64(3.14159))
	}
}

func BenchmarkLiteralMatchesPKKind_Int64(b *testing.B) {
	for b.Loop() {
		_ = LiteralMatchesPKKind(int64(42), protoreflect.Int64Kind)
	}
}

func BenchmarkLiteralMatchesPKKind_String(b *testing.B) {
	for b.Loop() {
		_ = LiteralMatchesPKKind("hello", protoreflect.StringKind)
	}
}
