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

// CARDINALITY's result type is Java's Type.primitiveType(INT), nullable
// (a NULL array → NULL). The metadata layer reports this as INTEGER, not
// BIGINT — pinning the type code to INT (not LONG) is the revert-proof
// guard for that divergence fix.
func TestCardinalityValue_TypeIsNullableInt(t *testing.T) {
	t.Parallel()
	v := NewCardinalityValue(LiteralValue([]any{}))
	if !v.Type().Equals(NullableInt) {
		t.Fatalf("Type = %v, want NullableInt", v.Type())
	}
	if v.Type().Code() != TypeCodeInt {
		t.Fatalf("Type code = %v, want TypeCodeInt (→ INTEGER metadata)", v.Type().Code())
	}
	if !v.Type().IsNullable() {
		t.Fatalf("Type should be nullable (NULL array → NULL)")
	}
}

// ExplainValue must render cardinality(<child>) — matching the yamsql
// EXPLAIN strings (`cardinality(_.int_arr)`), not a bare `cardinality`.
func TestCardinalityValue_ExplainRendersChild(t *testing.T) {
	t.Parallel()
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("_"), UnknownType)
	v := NewCardinalityValue(NewFieldValue(qov, "int_arr", NullableInt))
	got := ExplainValue(v)
	const want = "cardinality(_.int_arr)"
	if got != want {
		t.Fatalf("ExplainValue = %q, want %q", got, want)
	}
}
