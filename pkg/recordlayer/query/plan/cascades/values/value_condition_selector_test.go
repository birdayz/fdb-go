package values

import "testing"

func TestConditionSelectorValue_Type(t *testing.T) {
	t.Parallel()
	v := NewConditionSelectorValue(nil)
	if !v.Type().Equals(NotNullInt) {
		t.Fatalf("Type = %v, want NotNullInt", v.Type())
	}
}

func TestConditionSelectorValue_Name(t *testing.T) {
	t.Parallel()
	v := NewConditionSelectorValue(nil)
	if got := v.Name(); got != "ConditionSelector" {
		t.Fatalf("Name = %q, want ConditionSelector", got)
	}
}

func TestConditionSelectorValue_FirstMatchWins(t *testing.T) {
	t.Parallel()
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true), // first TRUE — index 1
		NewBooleanValue(true),
	})
	if got := mustEvalForTest(v, nil); got != int64(1) {
		t.Fatalf("Evaluate = %v, want 1 (first TRUE wins)", got)
	}
}

func TestConditionSelectorValue_AllFalseReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(false),
	})
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(all FALSE) = %v, want nil", got)
	}
}

func TestConditionSelectorValue_NullImplicationReturnsNil(t *testing.T) {
	t.Parallel()
	// Nil implication is treated as "no match" (matches Java: NULL is
	// not Boolean.TRUE, so it doesn't trigger the index return).
	v := NewConditionSelectorValue([]Value{
		LiteralValue(nil),
		LiteralValue(nil),
	})
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(all nil) = %v, want nil", got)
	}
}

func TestConditionSelectorValue_NonBoolImplicationDoesNotMatch(t *testing.T) {
	t.Parallel()
	// A non-bool implication value (string here) doesn't match.
	v := NewConditionSelectorValue([]Value{
		LiteralValue("not-a-bool"),
		NewBooleanValue(true), // TRUE — index 1
	})
	if got := mustEvalForTest(v, nil); got != int64(1) {
		t.Fatalf("Evaluate(non-bool, TRUE) = %v, want 1", got)
	}
}

func TestConditionSelectorValue_FalseDoesNotMatch(t *testing.T) {
	t.Parallel()
	// Boolean.FALSE specifically — Java's strict-TRUE check rejects.
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
	})
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(FALSE) = %v, want nil (only TRUE matches)", got)
	}
}

func TestConditionSelectorValue_EmptyImplicationsReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewConditionSelectorValue(nil)
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(empty) = %v, want nil", got)
	}
}

func TestConditionSelectorValue_ImplicitElseViaTrailingTrue(t *testing.T) {
	t.Parallel()
	// SQL CASE ELSE pattern — trailing TRUE captures unmatched cases.
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false), // c1 doesn't match
		NewBooleanValue(false), // c2 doesn't match
		NewBooleanValue(true),  // ELSE — always matches, index 2
	})
	if got := mustEvalForTest(v, nil); got != int64(2) {
		t.Fatalf("Evaluate = %v, want 2 (implicit ELSE)", got)
	}
}

func TestConditionSelectorValue_PickValueIntegration(t *testing.T) {
	t.Parallel()
	// Full SQL CASE lowering:
	//   CASE WHEN false THEN 'a'
	//        WHEN true THEN 'b'
	//        ELSE 'c' END
	// → PickValue(ConditionSelectorValue(false, true, true), [a, b, c])
	selector := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true),
		NewBooleanValue(true),
	})
	pick := NewPickValue(selector,
		[]Value{LiteralValue("a"), LiteralValue("b"), LiteralValue("c")},
		NotNullString)
	if got := mustEvalForTest(pick, nil); got != "b" {
		t.Fatalf("CASE result = %v, want 'b'", got)
	}
}

func TestConditionSelectorValue_Children(t *testing.T) {
	t.Parallel()
	a := NewBooleanValue(true)
	b := NewBooleanValue(false)
	v := NewConditionSelectorValue([]Value{a, b})
	cs := v.Children()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatalf("Children = %v, want [a, b]", cs)
	}
}

func TestConditionSelectorValue_DefensiveCopyOnConstruct(t *testing.T) {
	t.Parallel()
	impl := []Value{NewBooleanValue(true)}
	v := NewConditionSelectorValue(impl)
	impl[0] = NewBooleanValue(false) // mutate caller's slice
	if got := mustEvalForTest(v, nil); got != int64(0) {
		t.Fatalf("Evaluate after caller mutation = %v, want 0 (defensive copy)", got)
	}
}

func TestConditionSelectorValue_WithChildren(t *testing.T) {
	t.Parallel()
	original := NewConditionSelectorValue([]Value{NewBooleanValue(false)})
	rebuilt := original.WithChildren([]Value{NewBooleanValue(true)})
	if got := mustEvalForTest(rebuilt, nil); got != int64(0) {
		t.Fatalf("rebuilt.Evaluate = %v, want 0", got)
	}
}

// TestConditionSelectorValue_SimplifyConstantFold verifies that
// SimplifyValue folds an all-constant ConditionSelector into a
// literal int64 (the index of the first TRUE implication).
func TestConditionSelectorValue_SimplifyConstantFold(t *testing.T) {
	t.Parallel()
	v := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true), // first TRUE — index 1
		NewBooleanValue(false),
	})
	folded := SimplifyValue(v)
	if folded == v {
		t.Fatalf("SimplifyValue did NOT fold all-constant ConditionSelector")
	}
	if got := mustEvalForTest(folded, nil); got != int64(1) {
		t.Fatalf("folded.Evaluate = %v, want 1", got)
	}
}

// TestPickValue_SimplifyConstantFold verifies the full SQL-CASE
// constant-fold via PickValue + ConditionSelectorValue + ALL-
// CONSTANT alternatives.
func TestPickValue_SimplifyConstantFold(t *testing.T) {
	t.Parallel()
	// CASE WHEN false THEN 'a' WHEN true THEN 'b' ELSE 'c' END = 'b'
	selector := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true),
		NewBooleanValue(true),
	})
	pick := NewPickValue(selector,
		[]Value{LiteralValue("a"), LiteralValue("b"), LiteralValue("c")},
		NotNullString)
	folded := SimplifyValue(pick)
	if folded == pick {
		t.Fatalf("SimplifyValue did NOT fold all-constant CASE")
	}
	if got := mustEvalForTest(folded, nil); got != "b" {
		t.Fatalf("folded.Evaluate = %v, want 'b'", got)
	}
}
