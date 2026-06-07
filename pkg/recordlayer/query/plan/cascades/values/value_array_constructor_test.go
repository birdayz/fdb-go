package values

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArrayConstructorValue_Type(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, []Value{
		LiteralValue(int64(1)),
	})
	got := v.Type()
	at, ok := got.(*ArrayType)
	if !ok {
		t.Fatalf("Type = %T, want *ArrayType", got)
	}
	if !at.ElementType.Equals(NotNullLong) {
		t.Fatalf("ElementType = %v, want NotNullLong", at.ElementType)
	}
	if at.Nullable {
		t.Fatalf("Nullable = true, want false (constructor produces non-nullable arrays)")
	}
}

func TestArrayConstructorValue_NilElementTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(nil, nil)
	at := v.Type().(*ArrayType)
	if at.ElementType != UnknownType {
		t.Fatalf("ElementType = %v, want UnknownType", at.ElementType)
	}
}

func TestArrayConstructorValue_Name(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, nil)
	if got := v.Name(); got != "array" {
		t.Fatalf("Name = %q, want array", got)
	}
}

func TestArrayConstructorValue_Children(t *testing.T) {
	t.Parallel()
	a := LiteralValue(int64(1))
	b := LiteralValue(int64(2))
	v := NewArrayConstructorValue(NotNullLong, []Value{a, b})
	cs := v.Children()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatalf("Children = %v, want [a, b]", cs)
	}
}

func TestArrayConstructorValue_EvaluateConcreteValues(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, []Value{
		LiteralValue(int64(1)),
		LiteralValue(int64(2)),
		LiteralValue(int64(3)),
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(1), int64(2), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_EvaluateEmptyArray(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	gotSlice, ok := got.([]any)
	if !ok {
		t.Fatalf("Evaluate = %T, want []any", got)
	}
	if len(gotSlice) != 0 {
		t.Fatalf("Evaluate empty = %v, want empty slice", gotSlice)
	}
	if gotSlice == nil {
		t.Fatalf("Evaluate empty = nil — empty array must be non-nil to distinguish from NULL")
	}
}

func TestArrayConstructorValue_EvaluatePassesThroughNULLs(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NullableLong, []Value{
		LiteralValue(int64(1)),
		LiteralValue(nil), // SQL NULL
		LiteralValue(int64(3)),
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(1), nil, int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate w/ NULL = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_NilChildToleratedAsNil(t *testing.T) {
	t.Parallel()
	// A nil Value child (different from a Value evaluating to nil)
	// still slots into the result as a nil element — matches Java's
	// fault-tolerance where missing children don't crash eval.
	v := NewArrayConstructorValue(NullableLong, []Value{
		LiteralValue(int64(1)),
		nil,
		LiteralValue(int64(3)),
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(1), any(nil), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate w/ nil child = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_HeterogeneousElements(t *testing.T) {
	t.Parallel()
	// Element-type validation is the planner's responsibility — the
	// constructor doesn't reject mismatched children; each child's
	// evaluation flows through verbatim.
	v := NewArrayConstructorValue(NullableString, []Value{
		LiteralValue("hello"),
		LiteralValue(int64(42)), // int in a string-typed array
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{"hello", int64(42)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate hetero = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_DefensiveCopyOfElements(t *testing.T) {
	t.Parallel()
	original := []Value{LiteralValue(int64(1))}
	v := NewArrayConstructorValue(NotNullLong, original)
	original[0] = LiteralValue(int64(999))
	tmpEv0, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	got := tmpEv0.([]any)
	if got[0] == int64(999) {
		t.Fatalf("Elements aliased caller's slice — not defensively copied")
	}
}

func TestArrayConstructorValue_WithChildren(t *testing.T) {
	t.Parallel()
	original := NewArrayConstructorValue(NotNullLong, []Value{LiteralValue(int64(1))})
	rebuilt := original.WithChildren([]Value{
		LiteralValue(int64(10)),
		LiteralValue(int64(20)),
	})
	got, errEv0 := rebuilt.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(10), int64(20)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt.Evaluate = %v, want %v", got, want)
	}
	// Element type carries through.
	at := rebuilt.Type().(*ArrayType)
	if !at.ElementType.Equals(NotNullLong) {
		t.Fatalf("rebuilt.ElementType = %v, want NotNullLong (carried through)", at.ElementType)
	}
}
