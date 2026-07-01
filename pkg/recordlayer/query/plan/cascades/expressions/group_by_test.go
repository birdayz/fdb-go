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
		{"COUNT(col)", AggregateSpec{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}, false},
		{"SUM(col)", AggregateSpec{Function: AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}}, false},
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
	a := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	b := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	if !a.EqualsWithoutChildren(b, nil) {
		t.Fatal("same GroupBy keys should be equal")
	}
}

func TestGroupByExpression_EqualsWithoutChildren_DifferentKeys(t *testing.T) {
	t.Parallel()
	a := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	b := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "city", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("different GroupBy keys should NOT be equal")
	}
}

func TestGroupByExpression_EqualsWithoutChildren_DifferentAggFunctions(t *testing.T) {
	t.Parallel()
	a := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	b := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggSum, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("different aggregate functions should NOT be equal")
	}
}

func TestGroupByExpression_EqualsWithoutChildren_DifferentAggOperands(t *testing.T) {
	t.Parallel()
	a := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	b := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "name", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("different aggregate operands should NOT be equal")
	}
}

func TestGroupByExpression_HashCodeWithoutChildren_Distinct(t *testing.T) {
	t.Parallel()
	a := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	b := NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "city", Typ: values.UnknownType}},
		[]AggregateSpec{{Function: AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		ForEachQuantifier(InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
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
