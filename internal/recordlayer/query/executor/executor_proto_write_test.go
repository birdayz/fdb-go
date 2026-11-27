package executor

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestGoToProtoValue_Int32(t *testing.T) {
	t.Parallel()
	order := (&demopb.Order{}).ProtoReflect().Descriptor()
	fd := order.Fields().ByName("price") // int32 field

	v, err := goToProtoValue(fd, int64(42))
	if err != nil {
		t.Fatalf("goToProtoValue int64→int32: %v", err)
	}
	if got := int32(v.Int()); got != 42 {
		t.Errorf("got %d, want 42", got)
	}

	v, err = goToProtoValue(fd, int32(99))
	if err != nil {
		t.Fatalf("goToProtoValue int32→int32: %v", err)
	}
	if got := int32(v.Int()); got != 99 {
		t.Errorf("got %d, want 99", got)
	}
}

func TestGoToProtoValue_Int64(t *testing.T) {
	t.Parallel()
	order := (&demopb.Order{}).ProtoReflect().Descriptor()
	fd := order.Fields().ByName("order_id") // int64 field

	v, err := goToProtoValue(fd, int64(123))
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if got := v.Int(); got != 123 {
		t.Errorf("got %d, want 123", got)
	}
}

// goToProtoValue must implement the promotable widenings Java's lattice allows
// (INT/LONG→FLOAT/DOUBLE), matching ConvertToProtoValue — a SUM(BIGINT) into a
// DOUBLE/FLOAT column flows an int64 and must widen, not error.
func TestGoToProtoValue_IntToDoubleWidens(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	v, err := goToProtoValue(typed.Fields().ByName("val_double"), int64(60))
	if err != nil {
		t.Fatalf("int64 → DOUBLE should widen, got: %v", err)
	}
	if got := v.Float(); got != 60.0 {
		t.Errorf("got %v, want 60.0", got)
	}
}

func TestGoToProtoValue_IntToFloatWidens(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	v, err := goToProtoValue(typed.Fields().ByName("val_float"), int64(7))
	if err != nil {
		t.Fatalf("int64 → FLOAT should widen, got: %v", err)
	}
	if got := v.Float(); got != 7.0 {
		t.Errorf("got %v, want 7.0", got)
	}
}

// A float64 (DOUBLE) into an integer column is NOT promotable (no DOUBLE→LONG
// edge); goToProtoValue's fallthrough must emit the verbatim 22000
// SemanticException, matching Java + the sibling ConvertToProtoValue — not a
// generic Go error.
func TestGoToProtoValue_DoubleToIntRejects22000(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	_, err := goToProtoValue(typed.Fields().ByName("val_int64"), float64(20.0))
	var mismatch *values.ScalarTypeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("float64 → BIGINT: want ScalarTypeMismatchError (22000), got %v", err)
	}
}

// The fallthrough is emergent: any genuinely incompatible assignment (e.g.
// string → integer) yields the same verbatim 22000, aligning with the sibling
// converter rather than the old generic fmt.Errorf.
func TestGoToProtoValue_StringToIntRejects22000(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	_, err := goToProtoValue(typed.Fields().ByName("val_int64"), "nope")
	var mismatch *values.ScalarTypeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("string → BIGINT: want ScalarTypeMismatchError (22000), got %v", err)
	}
}

func TestGoToProtoValue_String(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_string")

	v, err := goToProtoValue(fd, "hello")
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if got := v.String(); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestGoToProtoValue_Bool(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_bool")

	v, err := goToProtoValue(fd, true)
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if !v.Bool() {
		t.Error("expected true")
	}
}

func TestGoToProtoValue_Double(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_double")

	v, err := goToProtoValue(fd, 3.14)
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	if got := v.Float(); got != 3.14 {
		t.Errorf("got %f, want 3.14", got)
	}
}

func TestGoToProtoValue_TypeError(t *testing.T) {
	t.Parallel()
	order := (&demopb.Order{}).ProtoReflect().Descriptor()
	fd := order.Fields().ByName("price")

	_, err := goToProtoValue(fd, "not_a_number")
	if err == nil {
		t.Fatal("expected error for string→int32, got nil")
	}
}

func TestGoToProtoValue_Float(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_float")

	v, err := goToProtoValue(fd, float64(2.5))
	if err != nil {
		t.Fatalf("goToProtoValue float64→float32: %v", err)
	}
	if got := float32(v.Float()); got != 2.5 {
		t.Errorf("got %f, want 2.5", got)
	}

	v, err = goToProtoValue(fd, float32(1.5))
	if err != nil {
		t.Fatalf("goToProtoValue float32→float32: %v", err)
	}
	if got := float32(v.Float()); got != 1.5 {
		t.Errorf("got %f, want 1.5", got)
	}
}

func TestGoToProtoValue_Overflow(t *testing.T) {
	t.Parallel()
	order := (&demopb.Order{}).ProtoReflect().Descriptor()
	int32Field := order.Fields().ByName("price")
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	float32Field := typed.Fields().ByName("val_float")

	t.Run("int32 overflow from int64", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(2147483648))
		if err == nil {
			t.Fatal("expected error for int64 value exceeding int32 max, got nil")
		}
		var overflowErr *NumericRangeOverflowError
		if !errors.As(err, &overflowErr) {
			t.Errorf("expected *NumericRangeOverflowError, got %T: %v", err, err)
		}
	})

	t.Run("int32 underflow from int64", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(-2147483649))
		if err == nil {
			t.Fatal("expected error for int64 value below int32 min, got nil")
		}
		var overflowErr *NumericRangeOverflowError
		if !errors.As(err, &overflowErr) {
			t.Errorf("expected *NumericRangeOverflowError, got %T: %v", err, err)
		}
	})

	t.Run("int32 boundary accept max", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(2147483647))
		if err != nil {
			t.Fatalf("expected no error for int32 max boundary, got: %v", err)
		}
	})

	t.Run("int32 boundary accept min", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(int32Field, int64(-2147483648))
		if err != nil {
			t.Fatalf("expected no error for int32 min boundary, got: %v", err)
		}
	})

	t.Run("float32 overflow from float64", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(float32Field, float64(math.MaxFloat32*2))
		if err == nil {
			t.Fatal("expected error for float64 value exceeding float32 range, got nil")
		}
		var overflowErr *NumericRangeOverflowError
		if !errors.As(err, &overflowErr) {
			t.Errorf("expected *NumericRangeOverflowError, got %T: %v", err, err)
		}
	})

	t.Run("float32 boundary accept max", func(t *testing.T) {
		t.Parallel()
		_, err := goToProtoValue(float32Field, float64(math.MaxFloat32))
		if err != nil {
			t.Fatalf("expected no error for float32 max boundary, got: %v", err)
		}
	})
}

func TestGoToProtoValue_ScalarConversions(t *testing.T) {
	t.Parallel()

	orderDesc := (&demopb.Order{}).ProtoReflect().Descriptor()
	typedDesc := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()

	cases := []struct {
		name string
		fd   protoreflect.FieldDescriptor
		val  any
		want protoreflect.Value
	}{
		{"int64_to_int32", orderDesc.Fields().ByName("price"), int64(42), protoreflect.ValueOfInt32(42)},
		{"int64_to_int64", orderDesc.Fields().ByName("order_id"), int64(42), protoreflect.ValueOfInt64(42)},
		{"float64_to_float", typedDesc.Fields().ByName("val_float"), float64(2.5), protoreflect.ValueOfFloat32(2.5)},
		{"float64_to_double", typedDesc.Fields().ByName("val_double"), float64(3.14), protoreflect.ValueOfFloat64(3.14)},
		{"string_to_string", typedDesc.Fields().ByName("val_string"), "hello", protoreflect.ValueOfString("hello")},
		{"bool_to_bool", typedDesc.Fields().ByName("val_bool"), true, protoreflect.ValueOfBool(true)},
		{"bytes_to_bytes", typedDesc.Fields().ByName("val_bytes"), []byte{1, 2, 3}, protoreflect.ValueOfBytes([]byte{1, 2, 3})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := goToProtoValue(tc.fd, tc.val)
			if err != nil {
				t.Fatalf("goToProtoValue: %v", err)
			}
			// Bytes fields return []byte which is not comparable via !=; handle separately.
			if tc.fd.Kind() == protoreflect.BytesKind {
				if !bytes.Equal(got.Bytes(), tc.want.Bytes()) {
					t.Errorf("goToProtoValue = %v, want %v", got, tc.want)
				}
			} else if got.Interface() != tc.want.Interface() {
				t.Errorf("goToProtoValue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGoToProtoValue_Bytes(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	fd := typed.Fields().ByName("val_bytes")

	data := []byte{0x01, 0x02, 0x03}
	v, err := goToProtoValue(fd, data)
	if err != nil {
		t.Fatalf("goToProtoValue: %v", err)
	}
	got := v.Bytes()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("got %v, want [1 2 3]", got)
	}
}

// ----- goToProtoValue (enum — extends existing int/string/bool/float/double/bytes tests) ---

func TestGoToProtoValue_EnumField(t *testing.T) {
	t.Parallel()
	msg := &demopb.TypedRecord{}
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("val_enum")
	pv, err := goToProtoValue(fd, int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if int64(pv.Enum()) != 2 {
		t.Fatalf("expected enum 2, got %d", pv.Enum())
	}
}

// =============================================================================
// goToProtoValue — gap coverage: uint32, uint64, int→int64
// =============================================================================

func TestGoToProtoValue_Int32FromInt(t *testing.T) {
	t.Parallel()
	fd := (&demopb.Order{}).ProtoReflect().Descriptor().Fields().ByName("price")
	v, err := goToProtoValue(fd, int(77))
	if err != nil {
		t.Fatal(err)
	}
	if int32(v.Int()) != 77 {
		t.Errorf("got %d, want 77", v.Int())
	}
}

func TestGoToProtoValue_Int64FromInt(t *testing.T) {
	t.Parallel()
	fd := (&demopb.Order{}).ProtoReflect().Descriptor().Fields().ByName("order_id")
	v, err := goToProtoValue(fd, int(999))
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != 999 {
		t.Errorf("got %d, want 999", v.Int())
	}
}

func TestGoToProtoValue_TypeErrors(t *testing.T) {
	t.Parallel()
	typed := (&demopb.TypedRecord{}).ProtoReflect().Descriptor()
	tests := []struct {
		name  string
		field string
		val   any
	}{
		{"bool_from_string", "val_bool", "true"},
		{"int32_from_bool", "val_int32", true},
		{"int64_from_string", "val_int64", "42"},
		{"float_from_string", "val_float", "3.14"},
		{"double_from_bool", "val_double", false},
		{"string_from_int", "val_string", 42},
		{"bytes_from_int", "val_bytes", 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fd := typed.Fields().ByName(protoreflect.Name(tc.field))
			_, err := goToProtoValue(fd, tc.val)
			if err == nil {
				t.Errorf("expected error for %T → %s", tc.val, tc.field)
			}
		})
	}
}

// The executor's array write backstop raises the same 0A000 error as array
// construction.
func TestGoToProtoValue_NullArrayElementIsJavas(t *testing.T) {
	t.Parallel()
	fd := (&metadatapb.Index{}).ProtoReflect().Descriptor().Fields().ByName("record_type")
	_, err := goToProtoValue(fd, []any{"a", nil})
	var nullElem *values.NullArrayElementError
	if !errors.As(err, &nullElem) {
		t.Fatalf("err = %v (%T), want NullArrayElementError", err, err)
	}
}

// =============================================================================
// protoToMap + goToProtoValue round-trip: read → map → write → read
// =============================================================================

func TestProtoRoundTrip_AllScalarKinds(t *testing.T) {
	t.Parallel()
	rec := &demopb.TypedRecord{
		Id:          proto.Int64(7),
		ValInt32:    proto.Int32(-42),
		ValInt64:    proto.Int64(math.MaxInt64),
		ValSint32:   proto.Int32(math.MinInt32),
		ValSint64:   proto.Int64(math.MinInt64),
		ValSfixed32: proto.Int32(12345),
		ValSfixed64: proto.Int64(-99999),
		ValFloat:    proto.Float32(1.5),
		ValDouble:   proto.Float64(math.Pi),
		ValBool:     proto.Bool(true),
		ValString:   proto.String("round-trip"),
		ValBytes:    []byte{0xAB, 0xCD},
	}

	m := protoToMap(rec)

	dst := &demopb.TypedRecord{}
	refl := dst.ProtoReflect()
	desc := refl.Descriptor()

	fieldMap := map[string]any{
		"id":           m["id"],
		"val_int32":    m["val_int32"],
		"val_int64":    m["val_int64"],
		"val_sint32":   m["val_sint32"],
		"val_sint64":   m["val_sint64"],
		"val_sfixed32": m["val_sfixed32"],
		"val_sfixed64": m["val_sfixed64"],
		"val_float":    m["val_float"],
		"val_double":   m["val_double"],
		"val_bool":     m["val_bool"],
		"val_string":   m["val_string"],
		"val_bytes":    m["val_bytes"],
	}

	for name, val := range fieldMap {
		fd := desc.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("no field %q", name)
		}
		pv, err := goToProtoValue(fd, val)
		if err != nil {
			t.Fatalf("goToProtoValue(%s, %v): %v", name, val, err)
		}
		refl.Set(fd, pv)
	}

	if dst.GetId() != 7 {
		t.Errorf("Id: got %d, want 7", dst.GetId())
	}
	if dst.GetValInt32() != -42 {
		t.Errorf("ValInt32: got %d, want -42", dst.GetValInt32())
	}
	if dst.GetValInt64() != math.MaxInt64 {
		t.Errorf("ValInt64: got %d, want MaxInt64", dst.GetValInt64())
	}
	if dst.GetValSint32() != math.MinInt32 {
		t.Errorf("ValSint32: got %d, want MinInt32", dst.GetValSint32())
	}
	if dst.GetValSint64() != math.MinInt64 {
		t.Errorf("ValSint64: got %d, want MinInt64", dst.GetValSint64())
	}
	if dst.GetValBool() != true {
		t.Error("ValBool: got false, want true")
	}
	if dst.GetValString() != "round-trip" {
		t.Errorf("ValString: got %q, want round-trip", dst.GetValString())
	}
	if dst.GetValDouble() != math.Pi {
		t.Errorf("ValDouble: got %f, want Pi", dst.GetValDouble())
	}
}
