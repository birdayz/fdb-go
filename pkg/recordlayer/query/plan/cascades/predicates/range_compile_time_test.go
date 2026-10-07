package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestComparisonIsCompileTime_JavaTable pins the range builder's compile-time
// rule against Java's (RangeConstraints.Builder.isCompileTime,
// RangeConstraints.java:752-755): the comparison's type is one of the eight
// allowed (:727-734), and IndexComparison.isSupported holds
// (IndexComparison.java:86-93) — a simple or null comparison, or a value
// comparison whose comparand's every node is a Value.RangeMatchableValue
// (Cast, ConstantObject, EvaluatesTo, IndexEntryObject, Literal, OfType,
// Promote). A parameter comparison is neither.
//
// Go's NullValue is what LiteralValue(nil) builds, Java's LiteralValue(null),
// so it counts as range-matchable; Go's BooleanValue and ConstantValue are
// Java's LiteralValue.
func TestComparisonIsCompileTime_JavaTable(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q")
	row := mustQOV(t, alias)
	field := predicateTestField(t, "x", values.NullableLong)
	cov := values.NewConstantObjectValue(values.NamedCorrelationIdentifier("c"), "0", values.NullableLong)
	ieo, err := values.NewIndexEntryObjectValue(alias, values.TupleSourceKey, []int{0}, values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := values.NewArithmeticValue(values.OpAdd, values.LiteralValue(int64(1)), values.LiteralValue(int64(2)))
	if err != nil {
		t.Fatal(err)
	}

	operands := []struct {
		name      string
		v         values.Value
		matchable bool
	}{
		{"literal", values.LiteralValue(int64(5)), true},
		{"boolean literal", values.NewBooleanValue(true), true},
		{"null literal", values.LiteralValue(nil), true},
		{"constant object", cov, true},
		{"promote(literal)", values.NewPromoteValue(values.LiteralValue(int64(5)), values.NullableDouble), true},
		{"cast(constant object)", values.NewCastValue(cov, values.NullableString), true},
		{"evaluates-to(literal)", values.NewEvaluatesToValue(values.LiteralValue(true), values.EvaluatesToTrue), true},
		{"of-type(constant object)", values.NewOfTypeValue(cov, values.NullableLong), true},
		{"index entry object", ieo, true},
		{"field (row correlated)", field, false},
		{"quantified object", row, false},
		{"arithmetic over literals", sum, false},
		{"promote(field)", values.NewPromoteValue(field, values.NullableDouble), false},
	}
	allowed := map[ComparisonType]bool{
		ComparisonEquals: true, ComparisonLessThan: true, ComparisonLessThanOrEq: true,
		ComparisonGreaterThan: true, ComparisonGreaterThanEq: true, ComparisonNotDistinctFrom: true,
	}
	binary := []ComparisonType{
		ComparisonEquals, ComparisonNotEquals, ComparisonLessThan, ComparisonLessThanOrEq,
		ComparisonGreaterThan, ComparisonGreaterThanEq, ComparisonStartsWith, ComparisonIn,
		ComparisonIsDistinctFrom, ComparisonNotDistinctFrom,
	}
	for _, typ := range binary {
		for _, op := range operands {
			want := allowed[typ] && op.matchable
			c := Comparison{Type: typ, Operand: op.v}
			if got := comparisonIsCompileTime(c); got != want {
				t.Errorf("%v over %s: compile time = %v, want %v", typ, op.name, got, want)
			}
			// A parameter comparison is never IndexComparison-supported.
			c.ParameterName = "p"
			if comparisonIsCompileTime(c) {
				t.Errorf("%v over parameter (%s): compile time, want not", typ, op.name)
			}
		}
	}
	for _, typ := range []ComparisonType{ComparisonIsNull, ComparisonIsNotNull} {
		if !comparisonIsCompileTime(Comparison{Type: typ}) {
			t.Errorf("%v: a null comparison is compile time", typ)
		}
	}
}
