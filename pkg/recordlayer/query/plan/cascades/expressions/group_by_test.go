package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestIsCountStar pins the single-source-of-truth count-star classifier
// (RFC-164 WS-3), which the planner's aggregate-index candidate AND the
// executor's group cursors both consume. COUNT(*) and COUNT(<constant>) —
// COUNT(1), COUNT(NULL), COUNT(TRUE) — are count-star (a constant counts every
// row, matching the translator's normalization); COUNT(<column>) and non-COUNT
// aggregates are not. This is the regression that keeps the copies from drifting
// (the executor's prior narrow "constant is SQL NULL only" outlier is gone).
func TestIsCountStar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		agg  AggregateSpec
		want bool
	}{
		{"COUNT(*)", AggregateSpec{Function: AggCount, Operand: nil}, true},
		{"COUNT(1)", AggregateSpec{Function: AggCount, Operand: &values.ConstantValue{Value: int64(1)}}, true},
		{"COUNT(NULL)", AggregateSpec{Function: AggCount, Operand: &values.ConstantValue{Value: nil}}, true},
		{"COUNT(TRUE)", AggregateSpec{Function: AggCount, Operand: &values.ConstantValue{Value: true}}, true},
		{"COUNT(non-null pool)", AggregateSpec{Function: AggCount, Operand: values.NewConstantObjectValue(values.NamedCorrelationIdentifier("pool"), "0", values.NotNullLong)}, true},
		{"COUNT(nullable pool)", AggregateSpec{Function: AggCount, Operand: values.NewConstantObjectValue(values.NamedCorrelationIdentifier("pool"), "0", values.NullableLong)}, false},
		{"COUNT(col)", AggregateSpec{Function: AggCount, Operand: testField("id", values.NotNullLong)}, false},
		{"SUM(col)", AggregateSpec{Function: AggSum, Operand: testField("amount", values.NotNullLong)}, false},
		{"SUM(const) is not count-star", AggregateSpec{Function: AggSum, Operand: &values.ConstantValue{Value: int64(1)}}, false},
		{"MAX(*)-shape non-count", AggregateSpec{Function: AggMax, Operand: nil}, false},
	}
	for _, tc := range cases {
		if got := IsCountStar(tc.agg); got != tc.want {
			t.Errorf("%s: IsCountStar = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGroupByExpression_EqualsWithoutChildren_SameKeys(t *testing.T) {
	t.Parallel()
	a := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	b := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	if !a.EqualsWithoutChildren(b, nil) {
		t.Fatal("same GroupBy keys should be equal")
	}
}

func TestGroupByExpression_EqualsWithoutChildren_DifferentKeys(t *testing.T) {
	t.Parallel()
	a := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	b := mustExpression(NewGroupByExpression(
		[]values.Value{testField("city", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("different GroupBy keys should NOT be equal")
	}
}

func TestGroupByExpression_EqualsWithoutChildren_DifferentAggFunctions(t *testing.T) {
	t.Parallel()
	a := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	b := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggSum, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("different aggregate functions should NOT be equal")
	}
}

func TestGroupByExpression_EqualsWithoutChildren_DifferentAggOperands(t *testing.T) {
	t.Parallel()
	a := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	b := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("name", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("different aggregate operands should NOT be equal")
	}
}

func TestGroupByExpression_HashCodeWithoutChildren_Distinct(t *testing.T) {
	t.Parallel()
	a := mustExpression(NewGroupByExpression(
		[]values.Value{testField("region", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	b := mustExpression(NewGroupByExpression(
		[]values.Value{testField("city", values.NotNullLong)},
		[]AggregateSpec{{Function: AggCount, Operand: testField("id", values.NotNullLong)}},
		ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))))

	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different GroupBy keys should produce different hash codes (collision possible but unlikely with these inputs)")
	}
}

func TestAggregateFunction_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		f    AggregateFunction
		want string
	}{
		{AggCount, "COUNT"},
		{AggSum, "SUM"},
		{AggMin, "MIN"},
		{AggMax, "MAX"},
		{AggAvg, "AVG"},
		{AggregateFunction(99), "UNKNOWN"},
	}
	for _, tc := range tests {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("AggregateFunction(%d).String() = %q, want %q", tc.f, got, tc.want)
		}
	}
}

// TestGroupByExpression_IndexOnlyAndBitmapAggregates pins the values the three
// index aggregates stand for in a group by's row, Java's: MIN_EVER / MAX_EVER an
// IndexOnlyAggregateValue of the operand's type (nullable), BITMAP_CONSTRUCT_AGG
// an aggregate value of type BYTES over an INT or LONG operand only (Java's
// operator map), refused over anything else.
func TestGroupByExpression_IndexOnlyAndBitmapAggregates(t *testing.T) {
	t.Parallel()
	scanQ := ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))))
	operand := func(i int) values.Value {
		if i == 2 {
			return testField("s", values.NullableString)
		}
		return testField("x", values.NotNullLong)
	}
	for _, tc := range []struct {
		fn      AggregateFunction
		operand int
		check   func(values.Value) bool
		typ     values.TypeCode
	}{
		{AggMinEver, 1, func(v values.Value) bool {
			io, ok := v.(*values.IndexOnlyAggregateValue)
			return ok && io.Op == values.IndexOnlyMinEverLong
		}, values.TypeCodeLong},
		{AggMaxEver, 1, func(v values.Value) bool {
			io, ok := v.(*values.IndexOnlyAggregateValue)
			return ok && io.Op == values.IndexOnlyMaxEverLong
		}, values.TypeCodeLong},
		{AggMaxEver, 2, func(v values.Value) bool { _, ok := v.(*values.IndexOnlyAggregateValue); return ok }, values.TypeCodeString},
		{AggBitmapConstructAgg, 1, func(v values.Value) bool {
			av, ok := v.(*values.AggregateValue)
			return ok && av.Op == values.AggBitmapConstructAgg && av.GetIndexTypeName() == "bitmap_value"
		}, values.TypeCodeBytes},
	} {
		gb, err := NewGroupByExpression(nil, []AggregateSpec{{Function: tc.fn, Operand: operand(tc.operand)}}, scanQ)
		if err != nil {
			t.Fatalf("%v over column %d: %v", tc.fn, tc.operand, err)
		}
		rcv, ok := gb.GetResultValue().(*values.RecordConstructorValue)
		if !ok || len(rcv.Fields) != 1 {
			t.Fatalf("%v: row %v", tc.fn, gb.GetResultValue())
		}
		field := rcv.Fields[0].Value
		children := field.Children()
		if len(children) != 1 || !tc.check(children[0]) {
			t.Fatalf("%v: the row holds %T %v, not the aggregate Java builds", tc.fn, field, children)
		}
		if typ := field.Type(); typ.Code() != tc.typ || !typ.IsNullable() {
			t.Fatalf("%v: result type %v, want a nullable %v", tc.fn, typ, tc.typ)
		}
	}
	if _, err := NewGroupByExpression(nil, []AggregateSpec{{Function: AggBitmapConstructAgg, Operand: operand(2)}}, scanQ); err == nil {
		t.Fatal("BITMAP_CONSTRUCT_AGG over a STRING built; Java has no operator for it")
	}
}
