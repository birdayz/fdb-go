package executor

import (
	"math"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestQueryResult_FromStoredRecord_NilSafe(t *testing.T) {
	t.Parallel()

	m := protoToMap(nil)
	if m != nil {
		t.Fatalf("protoToMap(nil) = %v, want nil", m)
	}
}

// --- covering-read row-domain normalization (F10) ---

// tupleElementToRowValue must place index-entry elements in the SAME domain the
// base-record path produces: float32 (32-bit tuple float) widens to float64
// (values.ProtoScalarKindToRowValue widens FLOAT), tuple.UUID becomes the
// neutral [16]byte, everything else passes through untouched.
func TestTupleElementToRowValue(t *testing.T) {
	t.Parallel()
	if got := tupleElementToRowValue(float32(1.5)); got != float64(1.5) {
		t.Fatalf("float32 → %T:%v, want float64:1.5", got, got)
	}
	u := tuple.UUID{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00}
	if got := tupleElementToRowValue(u); got != [16]byte(u) {
		t.Fatalf("tuple.UUID → %T, want [16]byte", got)
	}
	for _, v := range []any{nil, int64(7), "s", true, float64(2.5)} {
		if got := tupleElementToRowValue(v); got != v {
			t.Fatalf("passthrough %T:%v changed to %T:%v", v, v, got, got)
		}
	}
	// []byte passes through by identity (interface compare would fail on slices).
	b := []byte{0x01}
	if got, ok := tupleElementToRowValue(b).([]byte); !ok || &got[0] != &b[0] {
		t.Fatalf("[]byte should pass through unchanged")
	}
}

// =============================================================================
// values.ProtoScalarKindToRowValue — exhaustive coverage of every protoreflect.Kind
// =============================================================================

func TestScalarProtoToGo_Bool(t *testing.T) {
	t.Parallel()
	got := values.ProtoScalarKindToRowValue(protoreflect.BoolKind, protoreflect.ValueOfBool(true))
	if got != true {
		t.Errorf("got %v, want true", got)
	}
	got = values.ProtoScalarKindToRowValue(protoreflect.BoolKind, protoreflect.ValueOfBool(false))
	if got != false {
		t.Errorf("got %v, want false", got)
	}
}

func TestScalarProtoToGo_Int32Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfInt32(42))
			if got != int64(42) {
				t.Errorf("got %v (%T), want int64(42)", got, got)
			}
			got = values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfInt32(-1))
			if got != int64(-1) {
				t.Errorf("got %v, want int64(-1)", got)
			}
		})
	}
}

func TestScalarProtoToGo_Int64Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfInt64(math.MaxInt64))
			if got != int64(math.MaxInt64) {
				t.Errorf("got %v, want MaxInt64", got)
			}
		})
	}
}

func TestScalarProtoToGo_Uint32Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			// protobuf-java's signed Integer: all 32 bits set is -1.
			got := values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfUint32(math.MaxUint32))
			if got != int64(-1) {
				t.Errorf("got %v (%T), want int64(-1)", got, got)
			}
			if got := values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfUint32(3000000000)); got != int64(-1294967296) {
				t.Errorf("3000000000: got %v (%T), want int64(-1294967296)", got, got)
			}
			if got := values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfUint32(42)); got != int64(42) {
				t.Errorf("42: got %v (%T), want int64(42)", got, got)
			}
		})
	}
}

func TestScalarProtoToGo_Uint64Kinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []protoreflect.Kind{
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			got := values.ProtoScalarKindToRowValue(kind, protoreflect.ValueOfUint64(12345))
			if got != int64(12345) {
				t.Errorf("got %v (%T), want int64(12345)", got, got)
			}
		})
	}
}

func TestScalarProtoToGo_Float(t *testing.T) {
	t.Parallel()
	got := values.ProtoScalarKindToRowValue(protoreflect.FloatKind, protoreflect.ValueOfFloat32(3.14))
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("got type %T, want float64", got)
	}
	if f < 3.13 || f > 3.15 {
		t.Errorf("got %f, want ~3.14", f)
	}
}

func TestScalarProtoToGo_Double(t *testing.T) {
	t.Parallel()
	got := values.ProtoScalarKindToRowValue(protoreflect.DoubleKind, protoreflect.ValueOfFloat64(2.71828))
	if got != float64(2.71828) {
		t.Errorf("got %v, want 2.71828", got)
	}
}

func TestScalarProtoToGo_String(t *testing.T) {
	t.Parallel()
	got := values.ProtoScalarKindToRowValue(protoreflect.StringKind, protoreflect.ValueOfString("hello"))
	if got != "hello" {
		t.Errorf("got %v, want hello", got)
	}
	got = values.ProtoScalarKindToRowValue(protoreflect.StringKind, protoreflect.ValueOfString(""))
	if got != "" {
		t.Errorf("got %v, want empty string", got)
	}
}

func TestScalarProtoToGo_Bytes(t *testing.T) {
	t.Parallel()
	data := []byte{0xDE, 0xAD}
	got := values.ProtoScalarKindToRowValue(protoreflect.BytesKind, protoreflect.ValueOfBytes(data))
	b, ok := got.([]byte)
	if !ok {
		t.Fatalf("got type %T, want []byte", got)
	}
	if len(b) != 2 || b[0] != 0xDE || b[1] != 0xAD {
		t.Errorf("got %x, want DEAD", b)
	}
}

func TestScalarProtoToGo_Enum(t *testing.T) {
	t.Parallel()
	got := values.ProtoScalarKindToRowValue(protoreflect.EnumKind, protoreflect.ValueOfEnum(2))
	if got != int64(2) {
		t.Errorf("got %v (%T), want int64(2)", got, got)
	}
}

// =============================================================================
// protoFieldToGo — list, scalar, and message fields
// =============================================================================

func TestProtoFieldToGo_ScalarField(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{Price: proto.Int32(42)}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("price")
	got := protoFieldToGo(fd, refl.Get(fd))
	if got != int64(42) {
		t.Errorf("got %v (%T), want int64(42)", got, got)
	}
}

func TestProtoFieldToGo_RepeatedStringField(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{Tags: []string{"a", "b", "c"}}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("tags")
	got := protoFieldToGo(fd, refl.Get(fd))
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("got type %T, want []any", got)
	}
	if len(arr) != 3 {
		t.Fatalf("got len %d, want 3", len(arr))
	}
	if arr[0] != "a" || arr[1] != "b" || arr[2] != "c" {
		t.Errorf("got %v, want [a b c]", arr)
	}
}

func TestProtoFieldToGo_EmptyRepeated(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{Tags: []string{}}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("tags")
	if refl.Has(fd) {
		got := protoFieldToGo(fd, refl.Get(fd))
		arr := got.([]any)
		if len(arr) != 0 {
			t.Errorf("expected empty slice, got %v", arr)
		}
	}
}

func TestProtoFieldToGo_MessageField(t *testing.T) {
	t.Parallel()
	flower := &demopb.Flower{Type: proto.String("rose")}
	order := &demopb.Order{Flower: flower}
	refl := order.ProtoReflect()
	fd := refl.Descriptor().Fields().ByName("flower")
	got := protoFieldToGo(fd, refl.Get(fd))
	msg, ok := got.(*demopb.Flower)
	if !ok {
		t.Fatalf("got type %T, want *demopb.Flower", got)
	}
	if msg.GetType() != "rose" {
		t.Errorf("got %q, want rose", msg.GetType())
	}
}

func TestProtoToMap_AllSetFields(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{
		OrderId:  proto.Int64(42),
		Price:    proto.Int32(199),
		Quantity: proto.Int32(5),
	}
	m := protoToMap(order)

	if m["order_id"] != int64(42) {
		t.Errorf("order_id = %v, want 42", m["order_id"])
	}
	if m["price"] != int64(199) {
		t.Errorf("price = %v, want 199", m["price"])
	}
	if m["quantity"] != int64(5) {
		t.Errorf("quantity = %v, want 5", m["quantity"])
	}
}

func TestProtoToMap_UnsetFieldsOmitted(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{
		OrderId: proto.Int64(1),
	}
	m := protoToMap(order)

	if m["order_id"] != int64(1) {
		t.Errorf("order_id = %v, want 1", m["order_id"])
	}
	if _, exists := m["price"]; exists {
		t.Errorf("price should not be present for unset field, got %v", m["price"])
	}
	if _, exists := m["quantity"]; exists {
		t.Errorf("quantity should not be present for unset field, got %v", m["quantity"])
	}
}

func TestProtoToMap_NilMessage(t *testing.T) {
	t.Parallel()
	m := protoToMap(nil)
	if m != nil {
		t.Errorf("protoToMap(nil) = %v, want nil", m)
	}
}

// =============================================================================
// protoToMap — comprehensive field-type coverage
// =============================================================================

func TestProtoToMap_TypedRecord_AllKinds(t *testing.T) {
	t.Parallel()
	rec := &demopb.TypedRecord{
		Id:          proto.Int64(1),
		ValInt32:    proto.Int32(32),
		ValInt64:    proto.Int64(64),
		ValSint32:   proto.Int32(-32),
		ValSint64:   proto.Int64(-64),
		ValSfixed32: proto.Int32(320),
		ValSfixed64: proto.Int64(640),
		ValFloat:    proto.Float32(1.5),
		ValDouble:   proto.Float64(2.5),
		ValBool:     proto.Bool(true),
		ValString:   proto.String("test"),
		ValBytes:    []byte{0x01},
	}
	m := protoToMap(rec)

	checks := map[string]any{
		"id":           int64(1),
		"val_int32":    int64(32),
		"val_int64":    int64(64),
		"val_sint32":   int64(-32),
		"val_sint64":   int64(-64),
		"val_sfixed32": int64(320),
		"val_sfixed64": int64(640),
		"val_bool":     true,
		"val_string":   "test",
	}
	for key, want := range checks {
		got, ok := m[key]
		if !ok {
			t.Errorf("missing key %q", key)
			continue
		}
		if got != want {
			t.Errorf("%s: got %v (%T), want %v (%T)", key, got, got, want, want)
		}
	}

	if b, ok := m["val_bytes"].([]byte); !ok || len(b) != 1 || b[0] != 0x01 {
		t.Errorf("val_bytes: got %v, want [01]", m["val_bytes"])
	}

	// Float fields widen to float64.
	fv, ok := m["val_float"].(float64)
	if !ok {
		t.Fatalf("val_float type %T, want float64", m["val_float"])
	}
	if fv < 1.4 || fv > 1.6 {
		t.Errorf("val_float: got %f, want ~1.5", fv)
	}
	dv := m["val_double"].(float64)
	if dv != 2.5 {
		t.Errorf("val_double: got %f, want 2.5", dv)
	}
}

func TestProtoToMap_RepeatedField(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{"x", "y"},
	}
	m := protoToMap(order)
	tags, ok := m["tags"].([]any)
	if !ok {
		t.Fatalf("tags type %T, want []any", m["tags"])
	}
	if len(tags) != 2 || tags[0] != "x" || tags[1] != "y" {
		t.Errorf("tags = %v, want [x y]", tags)
	}
}

func TestProtoToMap_MessageField(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{
		OrderId: proto.Int64(1),
		Flower:  &demopb.Flower{Type: proto.String("tulip")},
	}
	m := protoToMap(order)
	flower, ok := m["flower"].(*demopb.Flower)
	if !ok {
		t.Fatalf("flower type %T, want *demopb.Flower", m["flower"])
	}
	if flower.GetType() != "tulip" {
		t.Errorf("got %q, want tulip", flower.GetType())
	}
}

func TestProtoToMap_EnumField(t *testing.T) {
	t.Parallel()
	blue := demopb.Color_BLUE
	rec := &demopb.TypedRecord{
		Id:      proto.Int64(1),
		ValEnum: &blue,
	}
	m := protoToMap(rec)
	got, ok := m["val_enum"]
	if !ok {
		t.Fatal("missing val_enum")
	}
	if got != int64(demopb.Color_BLUE) {
		t.Errorf("val_enum = %v, want %d", got, demopb.Color_BLUE)
	}
}

func TestProtoToMap_BytesField(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{
		OrderId:    proto.Int64(1),
		VectorData: []byte{0xCA, 0xFE},
	}
	m := protoToMap(order)
	b, ok := m["vector_data"].([]byte)
	if !ok {
		t.Fatalf("vector_data type %T, want []byte", m["vector_data"])
	}
	if len(b) != 2 || b[0] != 0xCA || b[1] != 0xFE {
		t.Errorf("vector_data = %x, want CAFE", b)
	}
}

// TestProtoToMap_KeysAreTheDescriptorSpelling pins the oracle's keying to the
// descriptor's own field names — the same spelling
// values.FieldNameForProtoField gives the corresponding positional slot.
//
// It used to assert the keys were UPPER, which was true only because every
// name-minting authority folded. Asserting the fold here made the oracle a
// second naming rule instead of an independent check of the first, so a fold
// that disappeared on one side would still have to be re-typed on the other
// before the shadow comparison could notice anything.
func TestProtoToMap_KeysAreTheDescriptorSpelling(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(99)}
	m := protoToMap(order)
	want := map[string]struct{}{"order_id": {}, "price": {}, "tags": {}}
	for key := range m {
		if _, ok := want[key]; !ok {
			t.Errorf("key %q is not a descriptor field name of Order", key)
		}
	}
	for key := range want {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

// =============================================================================
// FromStoredRecord — integration of protoToMap into QueryResult construction
// =============================================================================

func TestFromStoredRecord(t *testing.T) {
	t.Parallel()
	order := &demopb.Order{
		OrderId:  proto.Int64(42),
		Price:    proto.Int32(199),
		Quantity: proto.Int32(3),
	}
	rec := &recordlayer.FDBStoredRecord[proto.Message]{
		Record:     order,
		PrimaryKey: tuple.Tuple{int64(42)},
	}
	qr := FromStoredRecord(rec)

	m, ok := rowMapOK(qr)
	if !ok {
		t.Fatalf("Datum type %T, want map[string]any", qr.Positional)
	}
	if m["order_id"] != int64(42) {
		t.Errorf("order_id = %v, want 42", m["order_id"])
	}
	if m["price"] != int64(199) {
		t.Errorf("price = %v, want 199", m["price"])
	}
	if qr.PrimaryKey[0] != int64(42) {
		t.Errorf("PrimaryKey = %v, want [42]", qr.PrimaryKey)
	}
	if qr.Record != rec {
		t.Error("Record pointer mismatch")
	}
}
