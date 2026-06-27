package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestValueSemanticEquals_PointerIdentity(t *testing.T) {
	t.Parallel()
	v := &values.FieldValue{Field: "x"}
	result := ValueSemanticEquals(v, v, EmptyValueEquivalence())
	if !result.IsTrue() {
		t.Fatal("same pointer should return AlwaysTrue")
	}
	if result.Constraint != nil {
		t.Fatal("pointer-identity result should have nil constraint")
	}
}

func TestValueSemanticEquals_NilValues(t *testing.T) {
	t.Parallel()
	fv := &values.FieldValue{Field: "x"}

	t.Run("both_nil", func(t *testing.T) {
		t.Parallel()
		// Both nil is pointer-equal (nil == nil), so returns true.
		result := ValueSemanticEquals(nil, nil, EmptyValueEquivalence())
		if !result.IsTrue() {
			t.Fatal("both nil (pointer identity) should return true")
		}
	})
	t.Run("left_nil", func(t *testing.T) {
		t.Parallel()
		result := ValueSemanticEquals(nil, fv, EmptyValueEquivalence())
		if !result.IsFalse() {
			t.Fatal("left nil should return false")
		}
	})
	t.Run("right_nil", func(t *testing.T) {
		t.Parallel()
		result := ValueSemanticEquals(fv, nil, EmptyValueEquivalence())
		if !result.IsFalse() {
			t.Fatal("right nil should return false")
		}
	})
}

func TestValueSemanticEquals_StructurallyEqual(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "col"}
	b := &values.FieldValue{Field: "col"}
	result := ValueSemanticEquals(a, b, EmptyValueEquivalence())
	if !result.IsTrue() {
		t.Fatal("structurally identical FieldValues should be semantically equal")
	}
}

func TestValueSemanticEquals_StructurallyDifferent(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "x"}
	b := &values.FieldValue{Field: "y"}
	result := ValueSemanticEquals(a, b, EmptyValueEquivalence())
	if !result.IsFalse() {
		t.Fatal("different FieldValues should not be semantically equal")
	}
}

func TestValueSemanticEquals_QOVWithEquivalence(t *testing.T) {
	t.Parallel()
	aliasA := values.NamedCorrelationIdentifier("a")
	aliasB := values.NamedCorrelationIdentifier("b")

	am := AliasMapOfAliases(aliasA, aliasB)
	veq := NewAliasMapValueEquivalence(am)

	va := values.NewQuantifiedObjectValue(aliasA)
	vb := values.NewQuantifiedObjectValue(aliasB)

	result := ValueSemanticEquals(va, vb, veq)
	if !result.IsTrue() {
		t.Fatal("QOVs with mapped aliases should be semantically equal")
	}
}

func TestValueSemanticEquals_QOVWithoutEquivalence(t *testing.T) {
	t.Parallel()
	aliasA := values.NamedCorrelationIdentifier("a")
	aliasB := values.NamedCorrelationIdentifier("b")

	va := values.NewQuantifiedObjectValue(aliasA)
	vb := values.NewQuantifiedObjectValue(aliasB)

	result := ValueSemanticEquals(va, vb, EmptyValueEquivalence())
	if !result.IsFalse() {
		t.Fatal("QOVs with different aliases and empty equivalence should not be equal")
	}
}

func TestValueSemanticEquals_CompositeWithMappedChildren(t *testing.T) {
	t.Parallel()
	aliasA := values.NamedCorrelationIdentifier("a")
	aliasB := values.NamedCorrelationIdentifier("b")

	am := AliasMapOfAliases(aliasA, aliasB)
	veq := NewAliasMapValueEquivalence(am)

	konst := &values.ConstantValue{Value: int64(42)}

	lhs := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  values.NewQuantifiedObjectValue(aliasA),
		Right: konst,
	}
	rhs := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  values.NewQuantifiedObjectValue(aliasB),
		Right: konst,
	}

	result := ValueSemanticEquals(lhs, rhs, veq)
	if !result.IsTrue() {
		t.Fatal("ArithmeticValues with mapped QOV children should be semantically equal")
	}
}

func TestValueSemanticEquals_DifferentTypes(t *testing.T) {
	t.Parallel()
	fv := &values.FieldValue{Field: "x"}
	cv := &values.ConstantValue{Value: int64(1)}

	result := ValueSemanticEquals(fv, cv, EmptyValueEquivalence())
	if !result.IsFalse() {
		t.Fatal("FieldValue vs ConstantValue should not be semantically equal")
	}
}
