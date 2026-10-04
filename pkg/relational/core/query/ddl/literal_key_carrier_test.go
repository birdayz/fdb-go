package ddl

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestLiteralKeyCarrier pins the stored width of a literal key expression
// against the literal's static type. Java stores Key.Expressions.value(<boxed
// literal>), so an INT literal is int_value and a FLOAT one float_value, while
// Go's query runtime carries every integer on int64 and every floating value on
// float64. The DDL path reaches the INT arm (every Java IndexTest literal and
// the bitmap entry size); the FLOAT arm is driven here directly so that a width
// the SQL goldens do not reach is still pinned.
func TestLiteralKeyCarrier(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   *values.ConstantValue
		want any
	}{
		{"INT narrows to int32", &values.ConstantValue{Value: int64(10000), Typ: values.NullableInt}, int32(10000)},
		{"INT negative", &values.ConstantValue{Value: int64(-7), Typ: values.NullableInt}, int32(-7)},
		{"LONG stays int64", &values.ConstantValue{Value: int64(3000000000), Typ: values.NullableLong}, int64(3000000000)},
		{"FLOAT narrows to float32", &values.ConstantValue{Value: float64(1.5), Typ: values.NullableFloat}, float32(1.5)},
		{"DOUBLE stays float64", &values.ConstantValue{Value: float64(1.5), Typ: values.NullableDouble}, float64(1.5)},
		{"STRING unchanged", &values.ConstantValue{Value: "x", Typ: values.TypeString}, "x"},
		{"NULL INT unchanged", &values.ConstantValue{Value: nil, Typ: values.NullableInt}, nil},
		// The carrier follows the static type whatever Go kind holds the value.
		{"INT held in a Go int", &values.ConstantValue{Value: int(10000), Typ: values.NullableInt}, int32(10000)},
		{"INT held in an int32", &values.ConstantValue{Value: int32(-7), Typ: values.NullableInt}, int32(-7)},
		{"INT held in a uint16", &values.ConstantValue{Value: uint16(9), Typ: values.NullableInt}, int32(9)},
		{"INT max held in a Go int", &values.ConstantValue{Value: int(2147483647), Typ: values.NullableInt}, int32(2147483647)},
		{"INT min held in a Go int", &values.ConstantValue{Value: int(-2147483648), Typ: values.NullableInt}, int32(-2147483648)},
		{"LONG held in a Go int", &values.ConstantValue{Value: int(2147483648), Typ: values.NullableLong}, int64(2147483648)},
		{"LONG held in an int32", &values.ConstantValue{Value: int32(5), Typ: values.NullableLong}, int64(5)},
		{"LONG held in a uint64", &values.ConstantValue{Value: uint64(3000000000), Typ: values.NullableLong}, int64(3000000000)},
		{"DOUBLE held in a float32", &values.ConstantValue{Value: float32(1.5), Typ: values.NullableDouble}, float64(1.5)},
		{"FLOAT held in a float32", &values.ConstantValue{Value: float32(1.5), Typ: values.NullableFloat}, float32(1.5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := literalKeyCarrier(tc.in)
			if err != nil {
				t.Fatalf("literalKeyCarrier(%v %v): %v", tc.in.Value, tc.in.Typ, err)
			}
			if got != tc.want {
				t.Fatalf("literalKeyCarrier(%v %v) = %v (%T), want %v (%T)", tc.in.Value, tc.in.Typ, got, got, tc.want, tc.want)
			}
		})
	}

	// The carrier is what reaches the wire: the Value proto variant is the
	// thing Java compares.
	for _, tc := range []struct {
		name string
		in   *values.ConstantValue
		want string
	}{
		{"INT", &values.ConstantValue{Value: int64(10000), Typ: values.NullableInt}, "int_value"},
		{"INT held in a Go int", &values.ConstantValue{Value: int(10000), Typ: values.NullableInt}, "int_value"},
		{"LONG", &values.ConstantValue{Value: int64(10000), Typ: values.NullableLong}, "long_value"},
		{"LONG held in a Go int", &values.ConstantValue{Value: int(10000), Typ: values.NullableLong}, "long_value"},
		{"FLOAT", &values.ConstantValue{Value: float64(0.5), Typ: values.NullableFloat}, "float_value"},
		{"DOUBLE", &values.ConstantValue{Value: float64(0.5), Typ: values.NullableDouble}, "double_value"},
	} {
		t.Run("wire "+tc.name, func(t *testing.T) {
			t.Parallel()
			carrier, err := literalKeyCarrier(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			kp := recordlayer.Literal(carrier).ToKeyExpression()
			if kp.GetValue() == nil {
				t.Fatalf("literal key expression has no Value: %v", kp)
			}
			var set []string
			kp.GetValue().ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
				set = append(set, string(fd.Name()))
				return true
			})
			if len(set) != 1 || set[0] != tc.want {
				t.Fatalf("stored Value fields = %v, want exactly [%s] (%v)", set, tc.want, kp)
			}
		})
	}
}

// TestLiteralKeyCarrierRefusesOutOfRangeInt pins that an INT-typed literal outside
// the int32 range is an error rather than a silent wrap into the stored index
// definition: int32(3000000000) would store int_value -1294967296.
func TestLiteralKeyCarrierRefusesOutOfRangeInt(t *testing.T) {
	t.Parallel()
	for _, v := range []int64{3000000000, -2147483649} {
		if got, err := literalKeyCarrier(&values.ConstantValue{Value: v, Typ: values.NullableInt}); err == nil {
			t.Fatalf("literalKeyCarrier(%d INT) = %v, want an error", v, got)
		}
	}
	for _, v := range []int64{2147483647, -2147483648} {
		got, err := literalKeyCarrier(&values.ConstantValue{Value: v, Typ: values.NullableInt})
		if err != nil || got != int32(v) {
			t.Fatalf("literalKeyCarrier(%d INT) = %v, %v; want int32(%d)", v, got, err, v)
		}
	}
	// The same refusal whatever Go kind holds the value.
	for _, v := range []any{int(3000000000), uint32(3000000000), int(-2147483649)} {
		if got, err := literalKeyCarrier(&values.ConstantValue{Value: v, Typ: values.NullableInt}); err == nil {
			t.Fatalf("literalKeyCarrier(%v %T INT) = %v, want an error", v, v, got)
		}
	}
	if got, err := literalKeyCarrier(&values.ConstantValue{Value: uint64(1) << 63, Typ: values.NullableLong}); err == nil {
		t.Fatalf("literalKeyCarrier(2^63 LONG) = %v, want an error", got)
	}
}

// TestLiteralKeyCarrierRefusesUnknownPairs pins that a (static type, Go kind)
// pair the carrier does not recognise is refused instead of reaching the wire
// with the Go kind's carrier, and that the recognised non-numeric pairs keep
// their value.
func TestLiteralKeyCarrierRefusesUnknownPairs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   *values.ConstantValue
	}{
		{"FLOAT held in an int64", &values.ConstantValue{Value: int64(1), Typ: values.NullableFloat}},
		{"DOUBLE held in an int", &values.ConstantValue{Value: int(1), Typ: values.NullableDouble}},
		{"INT held in a float64", &values.ConstantValue{Value: float64(1), Typ: values.NullableInt}},
		{"LONG held in a string", &values.ConstantValue{Value: "1", Typ: values.NullableLong}},
		{"STRING held in an int64", &values.ConstantValue{Value: int64(1), Typ: values.TypeString}},
		{"BOOLEAN held in a string", &values.ConstantValue{Value: "true", Typ: values.NullableBoolean}},
		{"BYTES held in a string", &values.ConstantValue{Value: "x", Typ: values.NullableBytes}},
		{"a type with no key carrier", &values.ConstantValue{Value: [16]byte{1}, Typ: values.NullableUuid}},
		{"a value with no static type", &values.ConstantValue{Value: int64(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := literalKeyCarrier(tc.in); err == nil {
				t.Fatalf("literalKeyCarrier(%v %T %v) = %v, want an error", tc.in.Value, tc.in.Value, tc.in.Typ, got)
			}
		})
	}
	for _, tc := range []struct {
		name string
		in   *values.ConstantValue
	}{
		{"BOOLEAN", &values.ConstantValue{Value: true, Typ: values.NullableBoolean}},
		{"BYTES", &values.ConstantValue{Value: []byte{1, 2}, Typ: values.NullableBytes}},
		{"STRING", &values.ConstantValue{Value: "abc", Typ: values.TypeString}},
		{"a NULL of any type", &values.ConstantValue{Value: nil, Typ: values.NullableLong}},
		{"a NULL with no static type", &values.ConstantValue{Value: nil}},
	} {
		t.Run("keeps "+tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := literalKeyCarrier(tc.in)
			if err != nil {
				t.Fatalf("literalKeyCarrier(%v): %v", tc.in.Value, err)
			}
			if b, ok := tc.in.Value.([]byte); ok {
				if gb, ok := got.([]byte); !ok || string(gb) != string(b) {
					t.Fatalf("literalKeyCarrier(%v) = %v (%T)", b, got, got)
				}
				return
			}
			if got != tc.in.Value {
				t.Fatalf("literalKeyCarrier(%v) = %v (%T)", tc.in.Value, got, got)
			}
		})
	}
}
