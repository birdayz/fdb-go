package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSubscriptValue_OneBased(t *testing.T) {
	t.Parallel()
	// Java conformance: SQL standard 1-based indexing.
	source := LiteralValue([]any{int64(10), int64(20), int64(30)})
	v := NewSubscriptValue(source, LiteralValue(int64(1)), NotNullLong)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != int64(10) {
		t.Fatalf("arr[1] = %v, want 10 (first element, 1-based)", got)
	}
	v2 := NewSubscriptValue(source, LiteralValue(int64(3)), NotNullLong)
	got, errEv1 := v2.Evaluate(nil)
	require.NoError(t, errEv1)
	if got != int64(30) {
		t.Fatalf("arr[3] = %v, want 30", got)
	}
}

func TestSubscriptValue_OutOfBoundsReturnsNil(t *testing.T) {
	t.Parallel()
	// Java conformance: out-of-bounds returns NULL, doesn't error.
	source := LiteralValue([]any{int64(10), int64(20), int64(30)})
	v := NewSubscriptValue(source, LiteralValue(int64(99)), NotNullLong)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("arr[99] = %v, want nil (out of bounds)", got)
	}
	v2 := NewSubscriptValue(source, LiteralValue(int64(0)), NotNullLong)
	got, errEv1 := v2.Evaluate(nil)
	require.NoError(t, errEv1)
	if got != nil {
		t.Fatalf("arr[0] = %v, want nil (0 is below 1-based start)", got)
	}
	v3 := NewSubscriptValue(source, LiteralValue(int64(-1)), NotNullLong)
	got, errEv2 := v3.Evaluate(nil)
	require.NoError(t, errEv2)
	if got != nil {
		t.Fatalf("arr[-1] = %v, want nil (negative index)", got)
	}
}

func TestSubscriptValue_NullPropagation(t *testing.T) {
	t.Parallel()
	source := LiteralValue([]any{int64(10)})
	v1 := NewSubscriptValue(source, LiteralValue(nil), NotNullLong)
	got, errEv0 := v1.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("arr[NULL] = %v, want nil", got)
	}
	v2 := NewSubscriptValue(LiteralValue(nil), LiteralValue(int64(1)), NotNullLong)
	got, errEv1 := v2.Evaluate(nil)
	require.NoError(t, errEv1)
	if got != nil {
		t.Fatalf("NULL[1] = %v, want nil", got)
	}
}

func TestSubscriptValue_NonIntegerIndexReturnsNil(t *testing.T) {
	t.Parallel()
	source := LiteralValue([]any{int64(10)})
	v := NewSubscriptValue(source, LiteralValue("not-int"), NotNullLong)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("arr['x'] = %v, want nil (non-integer index)", got)
	}
}

func TestSubscriptValue_NonSliceSourceReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewSubscriptValue(LiteralValue("not-a-list"), LiteralValue(int64(1)), NotNullLong)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Subscript over non-slice = %v, want nil", got)
	}
}

func TestSubscriptValue_Children(t *testing.T) {
	t.Parallel()
	source := LiteralValue([]any{int64(1)})
	idx := LiteralValue(int64(1))
	v := NewSubscriptValue(source, idx, NotNullLong)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != source || cs[1] != idx {
		t.Fatalf("Children = %v, want [source, index]", cs)
	}
}

func TestSubscriptValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewSubscriptValue(LiteralValue([]any{}), LiteralValue(int64(1)), NotNullString)
	if !v.Type().Equals(NotNullString) {
		t.Fatalf("Type = %v, want NotNullString", v.Type())
	}
}
