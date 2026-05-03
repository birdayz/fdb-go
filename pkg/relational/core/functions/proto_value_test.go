package functions

import (
	"math"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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
