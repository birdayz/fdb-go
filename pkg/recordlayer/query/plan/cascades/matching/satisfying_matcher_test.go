package matching

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSatisfyingMatcher_PredicateTrue pins the success path: type
// assertion succeeds AND the predicate returns true.
func TestSatisfyingMatcher_PredicateTrue(t *testing.T) {
	t.Parallel()
	m := NewSatisfyingMatcher[*values.FieldValue](
		"FieldNamedX",
		func(f *values.FieldValue) bool { return f.Field == "x" },
	)
	got := m.BindMatches(NewBindings(), &values.FieldValue{Field: "x", Typ: values.TypeInt})
	if len(got) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(got))
	}
}

// TestSatisfyingMatcher_PredicateFalse pins the failure path: type
// assertion succeeds but predicate returns false.
func TestSatisfyingMatcher_PredicateFalse(t *testing.T) {
	t.Parallel()
	m := NewSatisfyingMatcher[*values.FieldValue](
		"FieldNamedX",
		func(f *values.FieldValue) bool { return f.Field == "x" },
	)
	if got := m.BindMatches(NewBindings(), &values.FieldValue{Field: "y", Typ: values.TypeInt}); got != nil {
		t.Errorf("expected nil (predicate false), got %d bindings", len(got))
	}
}

// TestSatisfyingMatcher_TypeMismatch pins that wrong-type input
// short-circuits BEFORE the predicate runs (saves the predicate
// from doing its own nil-check / type assertion).
func TestSatisfyingMatcher_TypeMismatch(t *testing.T) {
	t.Parallel()
	predicateRan := false
	m := NewSatisfyingMatcher[*values.FieldValue](
		"FieldNamedX",
		func(f *values.FieldValue) bool {
			predicateRan = true
			return true
		},
	)
	if got := m.BindMatches(NewBindings(), &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}); got != nil {
		t.Errorf("expected nil for wrong-type input, got %d", len(got))
	}
	if predicateRan {
		t.Error("predicate ran on wrong-type input — should short-circuit on type assertion")
	}
}

// TestSatisfyingMatcher_OnAnyType pins that the matcher works for
// non-pointer / arbitrary types (int64, string, struct).
func TestSatisfyingMatcher_OnAnyType(t *testing.T) {
	t.Parallel()
	posInt := NewSatisfyingMatcher[int64]("PositiveInt", func(n int64) bool { return n > 0 })

	if got := posInt.BindMatches(NewBindings(), int64(5)); len(got) != 1 {
		t.Errorf("int64(5): got %d, want 1", len(got))
	}
	if got := posInt.BindMatches(NewBindings(), int64(-5)); got != nil {
		t.Errorf("int64(-5): got %d, want nil", len(got))
	}
	if got := posInt.BindMatches(NewBindings(), "wrong type"); got != nil {
		t.Errorf("wrong type: got %d, want nil", len(got))
	}
}

// TestSatisfyingMatcher_RejectsNilPredicate pins constructor panic.
func TestSatisfyingMatcher_RejectsNilPredicate(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil predicate")
		}
	}()
	_ = NewSatisfyingMatcher[*values.FieldValue]("X", nil)
}

// TestSatisfyingMatcher_DistinctIdentity pins distinct map-key
// identity for two NewSatisfyingMatcher instances.
func TestSatisfyingMatcher_DistinctIdentity(t *testing.T) {
	t.Parallel()
	a := NewSatisfyingMatcher[*values.FieldValue]("A", func(f *values.FieldValue) bool { return true })
	b := NewSatisfyingMatcher[*values.FieldValue]("B", func(f *values.FieldValue) bool { return true })
	in := &values.FieldValue{Field: "x", Typ: values.TypeInt}

	bindings := NewBindings()
	for _, partial := range a.BindMatches(bindings, in) {
		bindings = partial
	}
	for _, partial := range b.BindMatches(bindings, in) {
		bindings = partial
	}
	if bindings.Get(a) == nil || bindings.Get(b) == nil {
		t.Fatalf("each matcher should bind distinctly")
	}
}
