package values

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/tupleordering"

	"github.com/stretchr/testify/require"
)

func TestOrderedBytesDirection_String(t *testing.T) {
	t.Parallel()
	cases := map[OrderedBytesDirection]string{
		OrderedBytesAscNullsFirst:  "ASC_NULLS_FIRST",
		OrderedBytesAscNullsLast:   "ASC_NULLS_LAST",
		OrderedBytesDescNullsFirst: "DESC_NULLS_FIRST",
		OrderedBytesDescNullsLast:  "DESC_NULLS_LAST",
		OrderedBytesDirection(99):  "INVALID",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("Direction(%d).String() = %q, want %q", d, got, want)
		}
	}
}

func TestOrderedBytesDirection_IsAscending(t *testing.T) {
	t.Parallel()
	cases := map[OrderedBytesDirection]bool{
		OrderedBytesAscNullsFirst:  true,
		OrderedBytesAscNullsLast:   true,
		OrderedBytesDescNullsFirst: false,
		OrderedBytesDescNullsLast:  false,
	}
	for d, want := range cases {
		if got := d.IsAscending(); got != want {
			t.Errorf("Direction(%v).IsAscending() = %v, want %v", d, got, want)
		}
	}
}

func TestToOrderedBytesValue_Type(t *testing.T) {
	t.Parallel()
	v := NewToOrderedBytesValue(LiteralValue(int64(7)), OrderedBytesAscNullsFirst)
	if !v.Type().Equals(NullableBytes) {
		t.Fatalf("Type = %v, want NullableBytes", v.Type())
	}
}

func TestToOrderedBytesValue_Name(t *testing.T) {
	t.Parallel()
	v := NewToOrderedBytesValue(LiteralValue(int64(7)), OrderedBytesAscNullsFirst)
	if got := v.Name(); got != "to_ordered_bytes" {
		t.Fatalf("Name = %q, want to_ordered_bytes", got)
	}
}

func TestToOrderedBytesValue_Children(t *testing.T) {
	t.Parallel()
	c := LiteralValue(int64(7))
	v := NewToOrderedBytesValue(c, OrderedBytesAscNullsFirst)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != c {
		t.Fatalf("Children = %v, want [c]", cs)
	}
}

func TestToOrderedBytesValue_NilChildEmptyChildren(t *testing.T) {
	t.Parallel()
	v := NewToOrderedBytesValue(nil, OrderedBytesAscNullsFirst)
	if got := v.Children(); len(got) != 0 {
		t.Fatalf("Children(nil child) = %v, want empty", got)
	}
}

// TestToOrderedBytesValue_EvaluatePacksTheScalar pins Java's eval:
// TupleOrdering.pack(Key.Evaluated.scalar(child).toTuple(), direction), the
// bytes an order-function index key holds.
func TestToOrderedBytesValue_EvaluatePacksTheScalar(t *testing.T) {
	t.Parallel()
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	for _, tc := range []struct {
		name  string
		child Value
		want  tuple.Tuple
	}{
		{"long", LiteralValue(int64(7)), tuple.Tuple{int64(7)}},
		{"string", LiteralValue("abc"), tuple.Tuple{"abc"}},
		{"null", NewNullValue(NullableLong), tuple.Tuple{nil}},
		{"float_narrowed", &ConstantValue{Value: float64(1.5), Typ: NotNullFloat}, tuple.Tuple{float32(1.5)}},
		{"double", &ConstantValue{Value: float64(1.5), Typ: NotNullDouble}, tuple.Tuple{float64(1.5)}},
		{"uuid", &ConstantValue{Value: uuid, Typ: NotNullUuid}, tuple.Tuple{tuple.UUID(uuid)}},
	} {
		for _, dir := range []OrderedBytesDirection{OrderedBytesAscNullsFirst, OrderedBytesAscNullsLast, OrderedBytesDescNullsFirst, OrderedBytesDescNullsLast} {
			got, err := NewToOrderedBytesValue(tc.child, dir).Evaluate(nil)
			require.NoError(t, err, "%s %s", tc.name, dir)
			require.Equal(t, tupleordering.Pack(tc.want, dir.tupleDirection()), got, "%s %s", tc.name, dir)
		}
	}
}

// TestOrderedBytesValues_RoundTrip: FromOrderedBytes inverts ToOrderedBytes
// in every direction, answering in the row domain (a FLOAT reads back as
// float64, a UUID as [16]byte).
func TestOrderedBytesValues_RoundTrip(t *testing.T) {
	t.Parallel()
	uuid := [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2, 3, 4, 5, 6}
	for _, tc := range []struct {
		name  string
		child Value
		typ   Type
		want  any
	}{
		{"long", LiteralValue(int64(-42)), NotNullLong, int64(-42)},
		{"string", LiteralValue("zz"), NotNullString, "zz"},
		{"bytes", LiteralValue([]byte{0, 1, 0xff}), NotNullBytes, []byte{0, 1, 0xff}},
		{"bool", LiteralValue(true), NotNullBoolean, true},
		{"null", NewNullValue(NullableString), NullableString, nil},
		{"float", &ConstantValue{Value: float64(2.25), Typ: NotNullFloat}, NotNullFloat, float64(2.25)},
		{"uuid", &ConstantValue{Value: uuid, Typ: NotNullUuid}, NotNullUuid, uuid},
	} {
		for _, dir := range []OrderedBytesDirection{OrderedBytesAscNullsFirst, OrderedBytesAscNullsLast, OrderedBytesDescNullsFirst, OrderedBytesDescNullsLast} {
			to := NewToOrderedBytesValue(tc.child, dir)
			got, err := to.CreateInverse(to, tc.typ).Evaluate(nil)
			require.NoError(t, err, "%s %s", tc.name, dir)
			require.Equal(t, tc.want, got, "%s %s", tc.name, dir)
		}
	}
}

func TestToOrderedBytesValue_CreateInverse(t *testing.T) {
	t.Parallel()
	original := NewToOrderedBytesValue(LiteralValue(int64(7)), OrderedBytesDescNullsLast)
	newChild := LiteralValue([]byte{0xff})
	inverse := original.CreateInverse(newChild, NotNullLong)
	if inverse == nil {
		t.Fatal("CreateInverse returned nil")
	}
	if inverse.Direction != OrderedBytesDescNullsLast {
		t.Fatalf("inverse.Direction = %v, want DESC_NULLS_LAST", inverse.Direction)
	}
	if !inverse.TargetType.Equals(NotNullLong) {
		t.Fatalf("inverse.TargetType = %v, want NotNullLong", inverse.TargetType)
	}
	if inverse.Child != newChild {
		t.Fatalf("inverse.Child mismatch")
	}
}

func TestFromOrderedBytesValue_Type(t *testing.T) {
	t.Parallel()
	v := NewFromOrderedBytesValue(LiteralValue([]byte{}), OrderedBytesAscNullsFirst, NotNullLong)
	got := v.Type()
	// Type should be the target type made nullable.
	if got.Code() != TypeCodeLong {
		t.Fatalf("Type = %v, want LONG-typed", got)
	}
	if !got.IsNullable() {
		t.Fatalf("Type.IsNullable = false, want true (decoded value is nullable)")
	}
}

func TestFromOrderedBytesValue_Name(t *testing.T) {
	t.Parallel()
	v := NewFromOrderedBytesValue(LiteralValue([]byte{}), OrderedBytesAscNullsFirst, NotNullLong)
	if got := v.Name(); got != "from_ordered_bytes" {
		t.Fatalf("Name = %q, want from_ordered_bytes", got)
	}
}

func TestFromOrderedBytesValue_NilTargetTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewFromOrderedBytesValue(LiteralValue([]byte{}), OrderedBytesAscNullsFirst, nil)
	if v.TargetType.Code() != TypeCodeUnknown {
		t.Fatalf("TargetType = %v, want UnknownType", v.TargetType)
	}
}

// TestFromOrderedBytesValue_EvaluateRefuses: Java requires non-null bytes
// (requireNonNull) holding at least one element (.get(0)); each failure is an
// error here, never a NULL that would read as data.
func TestFromOrderedBytesValue_EvaluateRefuses(t *testing.T) {
	t.Parallel()
	for name, child := range map[string]Value{
		"null_child":  NewNullValue(NullableBytes),
		"not_bytes":   LiteralValue(int64(3)),
		"empty":       LiteralValue([]byte{}),
		"not_ordered": LiteralValue([]byte{0x99}),
	} {
		_, err := NewFromOrderedBytesValue(child, OrderedBytesAscNullsFirst, NotNullLong).Evaluate(nil)
		require.Error(t, err, name)
	}
}

func TestFromOrderedBytesValue_Children(t *testing.T) {
	t.Parallel()
	c := LiteralValue([]byte{0xab, 0xcd})
	v := NewFromOrderedBytesValue(c, OrderedBytesAscNullsFirst, NotNullLong)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != c {
		t.Fatalf("Children = %v, want [c]", cs)
	}
}
