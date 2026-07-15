package executor

import (
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// avgTestCursor builds a bare aggregateCursor with a single AVG aggregate over
// a baked-ordinal operand reading slot 0 ("V") — the minimal harness to drive
// accumulateRow/finalizeGroup directly (no inner cursor, no planner).
func avgTestCursor(t *testing.T, fieldType values.Type) *aggregateCursor {
	t.Helper()
	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "V", FieldType: fieldType, Ordinal: 0},
	})
	qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("T"), rowType)
	operand, err := values.NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	c := &aggregateCursor{
		aggregates: []expressions.AggregateSpec{{Function: expressions.AggAvg, Operand: operand}},
		scalarMode: true,
	}
	c.current = c.newGroupState()
	return c
}

// avgAccumulateAll feeds vals through accumulateRow and returns finalizeGroup's
// single AVG output slot.
func avgAccumulateAll(t *testing.T, c *aggregateCursor, vals []any) any {
	t.Helper()
	for _, v := range vals {
		if err := c.accumulateRow(dorder([]string{"V"}, []any{v})); err != nil {
			t.Fatalf("accumulateRow(%v): %v", v, err)
		}
	}
	out := c.finalizeGroup()
	got, ok := out.Positional.Get(0)
	if !ok {
		t.Fatalf("finalizeGroup emitted no slot 0: %+v", out.Positional)
	}
	return got
}

// AVG over BIGINT must divide the EXACT int64 running sum (converted to double
// once at finish), not an incrementally-accumulated float64 sum — Java
// AverageAccumulatorState.longAverageState (LongState SUM via Math.addExact,
// finish = total.doubleValue()/count) and Cascades AVG_L. Incremental float64
// addition loses low-order bits past 2^53: 2^53 + 1 + 1 accumulated as float64
// stays 2^53 (each +1 rounds away), yielding 3002399751580330.5 instead of the
// correct float64(2^53+2)/3 = 3002399751580331.5.
func TestAggregateCursorAvgBigintExactAbove2p53(t *testing.T) {
	t.Parallel()
	c := avgTestCursor(t, values.NotNullLong)
	got := avgAccumulateAll(t, c, []any{int64(1) << 53, int64(1), int64(1)})

	want := float64(int64(1)<<53+2) / 3 // 3002399751580331.5 — exact sum, ONE conversion
	broken := (float64(int64(1)<<53) + 1 + 1) / 3
	if broken == want {
		t.Fatalf("test values do not discriminate: broken==want==%v", want)
	}
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("AVG(BIGINT) = %T(%v), want float64", got, got)
	}
	if f != want {
		t.Errorf("AVG(BIGINT) above 2^53 = %v, want %v (exact int64 sum / count); the incremental-float64 value is %v", f, want, broken)
	}
}

// Control: an all-int AVG with a small exact sum — guards against
// over-correction (the exact-sum path must agree with the float path where
// both are exact).
func TestAggregateCursorAvgBigintSmallExact(t *testing.T) {
	t.Parallel()
	c := avgTestCursor(t, values.NotNullLong)
	got := avgAccumulateAll(t, c, []any{int64(2), int64(3), int64(4)})
	if f, ok := got.(float64); !ok || f != 3.0 {
		t.Errorf("AVG(2,3,4) = %T(%v), want float64(3.0)", got, got)
	}
}

// Control: a DOUBLE-operand AVG keeps the float64 accumulation path (Java
// doubleAverageState — double running sum). The int64 exact-sum path would
// return 0 here (sumsI never accumulates float64 operands), so a correct
// result proves the float path still serves non-all-int groups.
func TestAggregateCursorAvgDoubleUsesFloatPath(t *testing.T) {
	t.Parallel()
	c := avgTestCursor(t, values.NotNullDouble)
	got := avgAccumulateAll(t, c, []any{1.5, 2.5, 3.5})
	if f, ok := got.(float64); !ok || f != 2.5 {
		t.Errorf("AVG(1.5,2.5,3.5) = %T(%v), want float64(2.5)", got, got)
	}
}

// AVG shares SUM's int64 overflow guard (the accumulateRow arm is common to
// both): overflowing the exact int64 running sum raises SumOverflowError,
// matching Java's Math.addExact ArithmeticException in LongState/AVG_L.
func TestAggregateCursorAvgBigintOverflowGuard(t *testing.T) {
	t.Parallel()
	c := avgTestCursor(t, values.NotNullLong)
	if err := c.accumulateRow(dorder([]string{"V"}, []any{int64(math.MaxInt64)})); err != nil {
		t.Fatalf("accumulateRow(MaxInt64): %v", err)
	}
	err := c.accumulateRow(dorder([]string{"V"}, []any{int64(1)}))
	var overflow *SumOverflowError
	if !errors.As(err, &overflow) {
		t.Errorf("AVG int64 overflow: got err=%v, want SumOverflowError (Java Math.addExact)", err)
	}
}
