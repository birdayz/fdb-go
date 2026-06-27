package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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
