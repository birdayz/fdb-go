package values

import (
	"testing"

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
	if !v.Type().Equals(NotNullBytes) {
		t.Fatalf("Type = %v, want NotNullBytes", v.Type())
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

func TestToOrderedBytesValue_EvaluateIsPlaceholder(t *testing.T) {
	t.Parallel()
	v := NewToOrderedBytesValue(LiteralValue(int64(7)), OrderedBytesAscNullsFirst)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate = %v, want nil (placeholder)", got)
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

func TestFromOrderedBytesValue_EvaluateIsPlaceholder(t *testing.T) {
	t.Parallel()
	v := NewFromOrderedBytesValue(LiteralValue([]byte{}), OrderedBytesAscNullsFirst, NotNullLong)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate = %v, want nil (placeholder)", got)
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
