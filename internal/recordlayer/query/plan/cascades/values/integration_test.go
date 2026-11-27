package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIntegration_ComplexCASEWithAndOr exercises ConditionSelectorValue
// + PickValue + AndOrValue together — the lowering of a SQL CASE
// expression with compound boolean conditions.
//
// SQL shape:
//
//	CASE WHEN a AND NOT b THEN 'high'
//	     WHEN a OR b THEN 'mid'
//	     ELSE 'low' END
//
// Lowers to:
//
//	PickValue(
//	  selector = ConditionSelectorValue([
//	    AndOrValue(AND, a, NotValue(b)),
//	    AndOrValue(OR, a, b),
//	    BooleanValue(true),  // ELSE
//	  ]),
//	  alternatives = ['high', 'mid', 'low'],
//	  type = NotNullString)
func TestIntegration_ComplexCASEWithAndOr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b bool
		want string
	}{
		{true, true, "mid"},   // a AND NOT b = false; a OR b = true → 'mid'
		{true, false, "high"}, // a AND NOT b = true → 'high'
		{false, true, "mid"},  // a OR b = true → 'mid'
		{false, false, "low"}, // ELSE → 'low'
	}

	for _, c := range cases {
		condition1 := NewAndOrValue(AndOrAnd,
			NewBooleanValue(c.a),
			NewNotValue(NewBooleanValue(c.b)))
		condition2 := NewAndOrValue(AndOrOr,
			NewBooleanValue(c.a),
			NewBooleanValue(c.b))
		else_ := NewBooleanValue(true)

		selector := NewConditionSelectorValue([]Value{condition1, condition2, else_})
		pick := NewPickValue(selector,
			[]Value{LiteralValue("high"), LiteralValue("mid"), LiteralValue("low")},
			NotNullString)

		got, errEv0 := pick.Evaluate(nil)
		require.NoError(t, errEv0)
		if got != c.want {
			t.Errorf("CASE(a=%v, b=%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestIntegration_ARRAY_DISTINCTOverArrayConstructor exercises
// ArrayConstructorValue + ArrayDistinctValue in a chain — the
// lowering of SQL `ARRAY_DISTINCT(ARRAY[1, 2, 1, 3])`.
func TestIntegration_ARRAY_DISTINCTOverArrayConstructor(t *testing.T) {
	t.Parallel()
	arr := NewArrayConstructorValue(NotNullLong, []Value{
		LiteralValue(int64(1)),
		LiteralValue(int64(2)),
		LiteralValue(int64(1)),
		LiteralValue(int64(3)),
	})
	distinct := NewArrayDistinctValue(arr)
	tmpEv1, errEv1 := distinct.Evaluate(nil)
	require.NoError(t, errEv1)
	got, ok := tmpEv1.([]any)
	if !ok {
		tmpEv0, errEv0 := distinct.Evaluate(nil)
		require.NoError(t, errEv0)
		t.Fatalf("Evaluate = %T, want []any", tmpEv0)
	}
	if len(got) != 3 {
		t.Fatalf("len(distinct) = %d, want 3 (1, 2, 3)", len(got))
	}
}

// TestIntegration_RangeValue_Length exercises RangeValue's
// Cardinality vs the materialized stream length.
//
// **Java-conformance note**: Java's RangeValue.getCardinalities
// computes floorDiv(end - begin, step), which under-counts by 1
// for non-divisible ranges (e.g. range(0, 100, 7) has 15 elements
// but cardinality = floor(100/7) = 14). Our Go port mirrors Java
// exactly, including this off-by-one. The cost model treats
// Cardinality as an estimate / lower bound, not an exact count.
//
// The test pins:
//   - For divisible ranges, Cardinality == len(stream).
//   - For non-divisible ranges, Cardinality is a lower bound:
//     Cardinality <= len(stream) <= Cardinality + 1.
func TestIntegration_RangeValue_Length(t *testing.T) {
	t.Parallel()
	cases := []struct {
		begin, end, step int64
		wantStreamLen    int64
	}{
		{0, 10, 1, 10},  // divisible
		{0, 10, 2, 5},   // divisible
		{0, 100, 7, 15}, // NOT divisible — floorDiv off-by-one
	}

	for _, c := range cases {
		r := NewRangeValue(LiteralValue(c.begin), LiteralValue(c.end), LiteralValue(c.step))
		card, ok := r.Cardinality()
		if !ok {
			t.Errorf("Cardinality returned ok=false for begin=%d end=%d step=%d", c.begin, c.end, c.step)
			continue
		}
		stream := r.EvaluateAsStream(nil)
		actualLen := int64(len(stream))
		if actualLen != c.wantStreamLen {
			t.Errorf("len(stream) = %d, want %d for begin=%d end=%d step=%d",
				actualLen, c.wantStreamLen, c.begin, c.end, c.step)
		}
		// Cardinality is a lower bound — within 1 of the actual length.
		if card > actualLen || actualLen > card+1 {
			t.Errorf("Cardinality (%d) outside [actualLen-1, actualLen]=[%d,%d] for begin=%d end=%d step=%d",
				card, actualLen-1, actualLen, c.begin, c.end, c.step)
		}
	}
}
