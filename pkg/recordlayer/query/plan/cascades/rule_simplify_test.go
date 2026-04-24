package cascades

import "testing"

var (
	_ CascadesRule = (*AndConstantSimplifyRule)(nil)
	_ CascadesRule = (*OrConstantSimplifyRule)(nil)
)

// AndPredicate with all-TRUE children → TRUE.
func TestAndSimplify_AllTrueToConstant(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// AndPredicate with a FALSE child → FALSE (short-circuit).
func TestAndSimplify_FalseShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Drop TRUE children from an AND, leaving the non-trivial children.
func TestAndSimplify_DropTrueChildren(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	leaf := NewConstantPredicate(TriUnknown) // stands in for a non-constant predicate
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		leaf,
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	// Single non-constant child remains — rule yields it directly.
	if got[0] != QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No constant children → rule declines to yield (idempotent).
func TestAndSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	leaf := NewConstantPredicate(TriUnknown)
	and := NewAnd(leaf, leaf)
	got := FireRule(rule, and)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with a TRUE child → TRUE.
func TestOrSimplify_TrueShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := NewOr(
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// OrPredicate with all-FALSE children → FALSE.
func TestOrSimplify_AllFalseToConstant(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := NewOr(
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriFalse),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Rules do not fire when the input isn't the matcher's type.
func TestAndSimplify_WrongType(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	// Feed an OrPredicate — AND rule's matcher should bail.
	or := NewOr(NewConstantPredicate(TriTrue))
	if got := FireRule(rule, or); len(got) != 0 {
		t.Fatalf("expected AND rule to not fire on OR, got %d yields", len(got))
	}
}
