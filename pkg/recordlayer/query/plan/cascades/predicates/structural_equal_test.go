package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestStructurallyEqual_SamePointer(t *testing.T) {
	t.Parallel()
	p := NewConstantPredicate(TriTrue)
	if !StructurallyEqual(p, p) {
		t.Fatal("same pointer should be equal")
	}
}

func TestStructurallyEqual_Nil(t *testing.T) {
	t.Parallel()
	if !StructurallyEqual(nil, nil) {
		t.Fatal("nil == nil should be true")
	}
	if StructurallyEqual(nil, NewConstantPredicate(TriTrue)) {
		t.Fatal("nil != non-nil should be false")
	}
}

func TestStructurallyEqual_Constant(t *testing.T) {
	t.Parallel()
	a := NewConstantPredicate(TriTrue)
	b := NewConstantPredicate(TriTrue)
	if !StructurallyEqual(a, b) {
		t.Fatal("two TRUE constants should be equal")
	}
	c := NewConstantPredicate(TriFalse)
	if StructurallyEqual(a, c) {
		t.Fatal("TRUE != FALSE")
	}
}

func TestStructurallyEqual_Comparison(t *testing.T) {
	t.Parallel()
	a := &ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "X"},
		Comparison: Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	b := &ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "X"},
		Comparison: Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	if !StructurallyEqual(a, b) {
		t.Fatal("identical comparisons should be equal")
	}
	c := &ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "Y"},
		Comparison: Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	if StructurallyEqual(a, c) {
		t.Fatal("different operands should not be equal")
	}
}

func TestStructurallyEqual_And(t *testing.T) {
	t.Parallel()
	p1 := NewConstantPredicate(TriTrue)
	p2 := NewConstantPredicate(TriFalse)
	a := NewAnd(p1, p2)
	b := NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))
	if !StructurallyEqual(a, b) {
		t.Fatal("identical AND should be equal")
	}
}

func TestStructurallyEqual_DifferentTypes(t *testing.T) {
	t.Parallel()
	a := NewConstantPredicate(TriTrue)
	b := &ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "X"},
		Comparison: Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	if StructurallyEqual(a, b) {
		t.Fatal("different types should not be equal")
	}
}

func TestStructurallyEqual_Exists(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q")
	a := NewExistentialAlias(alias)
	b := NewExistentialAlias(alias)
	if !StructurallyEqual(a, b) {
		t.Fatal("same alias EXISTS should be equal")
	}
	c := NewExistentialAlias(values.NamedCorrelationIdentifier("other"))
	if StructurallyEqual(a, c) {
		t.Fatal("different alias EXISTS should not be equal")
	}
}
