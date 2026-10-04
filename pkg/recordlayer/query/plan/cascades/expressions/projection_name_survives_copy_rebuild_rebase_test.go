package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestProjectionColumnNameExactRequestsCanonicalize(t *testing.T) {
	t.Parallel()

	byName := &values.ArithmeticValue{
		Left:  testField("N", values.NotNullLong),
		Right: &values.ConstantValue{Value: int64(1)},
		Op:    values.OpAdd,
	}
	byOrdinal := &values.ArithmeticValue{
		Left:  testFieldAt("N", 0, values.NotNullLong),
		Right: &values.ConstantValue{Value: int64(1)},
		Op:    values.OpAdd,
	}
	if a, b := values.OutputColumnName(byName, ""), values.OutputColumnName(byOrdinal, ""); a != b {
		t.Fatalf("equivalent exact field resolutions produced names %q and %q", a, b)
	}
	if !values.SemanticEqualsUnderAliasMap(byName, byOrdinal, values.EmptyAliasMap()) {
		t.Fatal("name and ordinal requests against one exact root did not canonicalize")
	}
}
