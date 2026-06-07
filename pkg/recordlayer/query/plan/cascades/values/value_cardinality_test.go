package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCardinalityValue_Counts(t *testing.T) {
	t.Parallel()
	v := NewCardinalityValue(LiteralValue([]any{int64(1), int64(2), int64(3)}))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != int64(3) {
		t.Fatalf("CARDINALITY([1,2,3]) = %v, want 3", got)
	}
}

func TestCardinalityValue_EmptyArray(t *testing.T) {
	t.Parallel()
	v := NewCardinalityValue(LiteralValue([]any{}))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != int64(0) {
		t.Fatalf("CARDINALITY([]) = %v, want 0", got)
	}
}

func TestCardinalityValue_NullInputReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewCardinalityValue(LiteralValue(nil))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("CARDINALITY(NULL) = %v, want nil", got)
	}
}

func TestCardinalityValue_NonSliceReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewCardinalityValue(LiteralValue("not-a-list"))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("CARDINALITY('not-a-list') = %v, want nil", got)
	}
}

func TestCardinalityValue_TypeIsNotNullLong(t *testing.T) {
	t.Parallel()
	v := NewCardinalityValue(LiteralValue([]any{}))
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}
