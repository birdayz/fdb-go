package predicates

import "testing"

func TestIsTautology_ConstantTrue(t *testing.T) {
	t.Parallel()
	if !IsTautology(NewConstantPredicate(TriTrue)) {
		t.Fatal("ConstantPredicate(TRUE) should be tautology")
	}
}

func TestIsTautology_ConstantFalse(t *testing.T) {
	t.Parallel()
	if IsTautology(NewConstantPredicate(TriFalse)) {
		t.Fatal("ConstantPredicate(FALSE) should NOT be tautology")
	}
}

func TestIsTautology_ConstantUnknown(t *testing.T) {
	t.Parallel()
	if IsTautology(NewConstantPredicate(TriUnknown)) {
		t.Fatal("ConstantPredicate(UNKNOWN) should NOT be tautology")
	}
}

func TestIsTautology_ComparisonPredicate(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(nil, NewLiteralComparison(ComparisonEquals, 1))
	if IsTautology(pred) {
		t.Fatal("ComparisonPredicate should NOT be tautology")
	}
}

func TestIsContradiction_ConstantFalse(t *testing.T) {
	t.Parallel()
	if !IsContradiction(NewConstantPredicate(TriFalse)) {
		t.Fatal("ConstantPredicate(FALSE) should be contradiction")
	}
}

func TestIsContradiction_ConstantTrue(t *testing.T) {
	t.Parallel()
	if IsContradiction(NewConstantPredicate(TriTrue)) {
		t.Fatal("ConstantPredicate(TRUE) should NOT be contradiction")
	}
}

func TestIsContradiction_ComparisonPredicate(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(nil, NewLiteralComparison(ComparisonEquals, 1))
	if IsContradiction(pred) {
		t.Fatal("ComparisonPredicate should NOT be contradiction")
	}
}
