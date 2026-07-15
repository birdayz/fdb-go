package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAggregateNaming_StableUnderOrdinalBind pins that the canonical
// aggregate/group-key OUTPUT column names do not change when the operand
// references are bound to plan-time ordinals: the naming
// authority renders through values.ColumnNameValue, so a baked and a lazy
// instance of the same reference derive the same column name. Drift here
// broke the HAVING lockstep ("SUM(AMOUNT#2)" ref vs "SUM(AMOUNT)" output
// slot).
func TestAggregateNaming_StableUnderOrdinalBind(t *testing.T) {
	t.Parallel()

	lazyOp := values.NewFlatFieldValue("AMOUNT", values.UnknownType)
	bakedOp := values.NewFieldValueWithResolvedOrdinal("AMOUNT", 2, values.UnknownType)

	lazyName := AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: lazyOp})
	bakedName := AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: bakedOp})
	if lazyName != bakedName {
		t.Fatalf("aggregate name drift: lazy %q vs baked %q", lazyName, bakedName)
	}
	if lazyName != "SUM(AMOUNT)" {
		t.Fatalf("canonical name = %q, want SUM(AMOUNT)", lazyName)
	}

	// COMPUTED operand (no FieldValue shortcut — the ColumnNameValue arm).
	lazyExpr := &values.ArithmeticValue{Op: values.OpMul, Left: lazyOp, Right: values.NewFlatFieldValue("QTY", values.UnknownType)}
	bakedExpr := &values.ArithmeticValue{Op: values.OpMul, Left: bakedOp, Right: values.NewFieldValueWithResolvedOrdinal("QTY", 3, values.UnknownType)}
	if l, b := AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: lazyExpr}),
		AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: bakedExpr}); l != b {
		t.Fatalf("computed-operand aggregate name drift: lazy %q vs baked %q", l, b)
	}

	// Group-key naming: computed keys render ordinal-free too.
	if l, b := AggregateKeyColumnName(lazyExpr), AggregateKeyColumnName(bakedExpr); l != b {
		t.Fatalf("group-key name drift: lazy %q vs baked %q", l, b)
	}
}
