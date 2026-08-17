package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestTableFunction_Construction(t *testing.T) {
	t.Parallel()
	r := values.NewRangeValue(
		values.LiteralValue(int64(0)),
		values.LiteralValue(int64(10)),
		values.LiteralValue(int64(1)))
	tf := mustExpression(NewTableFunctionExpression(r))
	if tf.GetValue() != r {
		t.Fatal("GetValue mismatch")
	}
	if got := tf.GetQuantifiers(); len(got) != 0 {
		t.Fatalf("GetQuantifiers = %v, want empty", got)
	}
	if tf.CanCorrelate() {
		t.Fatal("CanCorrelate = true, want false")
	}
}

func TestTableFunction_GetResultValue(t *testing.T) {
	t.Parallel()
	r := values.NewRangeValue(
		values.LiteralValue(int64(0)),
		values.LiteralValue(int64(10)),
		values.LiteralValue(int64(1)))
	tf := mustExpression(NewTableFunctionExpression(r))
	rv := tf.GetResultValue()
	// RangeValue's Type() is NotNullLong; QueriedValue typed at NotNullLong.
	if !rv.Type().Equals(values.NotNullLong) {
		t.Fatalf("ResultValue type = %v, want NotNullLong", rv.Type())
	}
}

func TestTableFunction_NilValueFallback(t *testing.T) {
	t.Parallel()
	tf, err := NewTableFunctionExpression(nil)
	if err == nil || tf != nil {
		t.Fatalf("nil stream returned (%v, %v), want nil expression and error", tf, err)
	}
}

func TestTableFunction_GetCorrelatedToFromValue(t *testing.T) {
	t.Parallel()
	r := values.NewRangeValue(
		values.LiteralValue(int64(0)),
		values.LiteralValue(int64(10)),
		values.LiteralValue(int64(1)))
	tf := mustExpression(NewTableFunctionExpression(r))
	if got := tf.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("GetCorrelatedTo over RangeValue with constant args = %v, want empty", got)
	}
}

func TestTableFunction_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	r := values.NewRangeValue(
		values.LiteralValue(int64(0)),
		values.LiteralValue(int64(10)),
		values.LiteralValue(int64(1)))
	tf1 := mustExpression(NewTableFunctionExpression(r))
	tf2 := mustExpression(NewTableFunctionExpression(r))
	if !tf1.EqualsWithoutChildren(tf2, nil) {
		t.Fatal("two TableFunctions over same Value should be EqualsWithoutChildren")
	}
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	if tf1.EqualsWithoutChildren(scan, nil) {
		t.Fatal("TableFunction should NOT equal Scan")
	}
}

func TestTableFunction_HashCodeStable(t *testing.T) {
	t.Parallel()
	r := values.NewRangeValue(
		values.LiteralValue(int64(0)),
		values.LiteralValue(int64(10)),
		values.LiteralValue(int64(1)))
	tf := mustExpression(NewTableFunctionExpression(r))
	h1 := tf.HashCodeWithoutChildren()
	h2 := tf.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren non-deterministic: %d vs %d", h1, h2)
	}
}

func TestTableFunction_DistinctFromExplodeHash(t *testing.T) {
	t.Parallel()
	// TableFunction(range(...)) and Explode(array) over different values
	// should hash differently — distinct class discriminators.
	r := values.NewRangeValue(
		values.LiteralValue(int64(0)),
		values.LiteralValue(int64(10)),
		values.LiteralValue(int64(1)))
	tf := mustExpression(NewTableFunctionExpression(r))
	arr := values.NewArrayConstructorValue(values.NotNullLong, nil)
	ex := mustExpression(NewExplodeExpression(arr))
	if tf.HashCodeWithoutChildren() == ex.HashCodeWithoutChildren() {
		t.Fatal("TableFunction and Explode should hash differently")
	}
}
