package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
