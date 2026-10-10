package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A baked aggregate operand over a layout-less row must still see the
// statement's constant pool: SUM(x + 1) once summed x + NULL.
func TestAggregateEvalArg_BakedOperandReadsConstantPool(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("agg_pool_input", false, []values.Field{{Name: "X", FieldType: values.NotNullLong, Ordinal: 0}})
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q"), rowType)
	if err != nil {
		t.Fatal(err)
	}
	x, err := values.ResolveOrdinalSeedField(qov, 0)
	if err != nil {
		t.Fatal(err)
	}
	pool := values.NamedCorrelationIdentifier("pool")
	operand := &values.ArithmeticValue{Op: values.OpAdd, Left: x, Right: values.NewConstantObjectValue(pool, "0", values.NotNullLong)}
	if !valueReadsBakedOrdinal(operand) {
		t.Fatal("operand does not take the baked-ordinal path")
	}
	c := &aggregateCursor{evalCtx: EmptyEvaluationContext().WithConstants(pool, map[string]any{"0": int64(1)})}
	row := QueryResult{Positional: &PositionalRow{Type: rowType, Slots: []any{int64(41)}}}
	arg, err := c.aggregateEvalArg(operand, row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := operand.Evaluate(arg)
	if err != nil || got != int64(42) {
		t.Fatalf("x + @0 = %v, %v; want 42", got, err)
	}
}
