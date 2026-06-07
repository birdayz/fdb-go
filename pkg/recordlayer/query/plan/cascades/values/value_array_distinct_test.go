package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArrayDistinctValue_DedupesPreservingOrder(t *testing.T) {
	t.Parallel()
	in := LiteralValue([]any{int64(1), int64(2), int64(1), int64(3), int64(2)})
	v := NewArrayDistinctValue(in)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	out, ok := got.([]any)
	if !ok {
		t.Fatalf("Evaluate = %T, want []any", got)
	}
	want := []any{int64(1), int64(2), int64(3)}
	if len(out) != len(want) {
		t.Fatalf("len = %d, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("out[%d] = %v, want %v", i, out[i], want[i])
		}
	}
}

func TestArrayDistinctValue_NilInputReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewArrayDistinctValue(LiteralValue(nil))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("nil input = %v, want nil", got)
	}
}

func TestArrayDistinctValue_NonSliceReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewArrayDistinctValue(LiteralValue("not-a-list"))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("non-slice input = %v, want nil", got)
	}
}

func TestArrayDistinctValue_StringDedup(t *testing.T) {
	t.Parallel()
	v := NewArrayDistinctValue(LiteralValue([]any{"a", "b", "a", "c"}))
	tmpEv0, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	got, _ := tmpEv0.([]any)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestArrayDistinctValue_BytesDedup(t *testing.T) {
	t.Parallel()
	// []byte slices: == panics, so dedup must use bytes.Equal.
	a1 := []byte{1, 2, 3}
	a2 := []byte{1, 2, 3} // equal contents, different slice
	a3 := []byte{4, 5, 6}
	v := NewArrayDistinctValue(LiteralValue([]any{a1, a2, a3}))
	tmpEv0, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	got, _ := tmpEv0.([]any)
	if len(got) != 2 {
		t.Fatalf("got %d distinct, want 2 (a1==a2 by content)", len(got))
	}
}

func TestArrayDistinctValue_EmptyArray(t *testing.T) {
	t.Parallel()
	v := NewArrayDistinctValue(LiteralValue([]any{}))
	tmpEv0, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	got, ok := tmpEv0.([]any)
	if !ok || len(got) != 0 {
		t.Fatalf("empty array Evaluate = %v, want []any{}", got)
	}
}

func TestArrayDistinctValue_NilChildReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewArrayDistinctValue(nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("nil child = %v, want nil", got)
	}
}
