package values

import "testing"

func TestIsFunctionallyDependentOn_SameCorrelation(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q")
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(alias),
		Right: &ConstantValue{Value: int64(1)},
	}
	other := NewQuantifiedObjectValue(alias)
	if !IsFunctionallyDependentOn(v, other) {
		t.Fatal("v references only q, should be functionally dependent on qov(q)")
	}
}

func TestIsFunctionallyDependentOn_DifferentCorrelation(t *testing.T) {
	t.Parallel()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(NamedCorrelationIdentifier("q1")),
		Right: NewQuantifiedObjectValue(NamedCorrelationIdentifier("q2")),
	}
	other := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q1"))
	if IsFunctionallyDependentOn(v, other) {
		t.Fatal("v references q1 AND q2, should NOT be functionally dependent on qov(q1) alone")
	}
}

func TestIsFunctionallyDependentOn_NonQOVOther(t *testing.T) {
	t.Parallel()
	v := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"))
	other := &FieldValue{Field: "x"}
	if IsFunctionallyDependentOn(v, other) {
		t.Fatal("non-QOV otherValue should return false")
	}
}

func TestIsFunctionallyDependentOn_ConstantValue(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: int64(42)}
	other := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"))
	if !IsFunctionallyDependentOn(v, other) {
		t.Fatal("constant with no QOV leaves is functionally dependent on anything")
	}
}
