package matching

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAtLeastElementsMatcher_AtLeastTwo pins the canonical case:
// the downstream matches 3 of 4 elements and the threshold is 2 —
// match succeeds. Mirrors Java's atLeastTwo(constantPredicate()).
func TestAtLeastElementsMatcher_AtLeastTwo(t *testing.T) {
	t.Parallel()
	m := NewAtLeastElementsMatcher(2, NewConstantMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 3 {
		t.Fatalf("expected 3 element-bindings (3 ConstantValues matched), got %d", len(got))
	}
}

// TestAtLeastElementsMatcher_FailsBelow pins the under-threshold
// path: only 1 element matches but threshold is 2 → nil.
func TestAtLeastElementsMatcher_FailsBelow(t *testing.T) {
	t.Parallel()
	m := NewAtLeastElementsMatcher(2, NewConstantMatcher())
	in := []any{
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.FieldValue{Field: "y", Typ: values.TypeInt},
	}
	if got := m.BindMatches(NewBindings(), in); got != nil {
		t.Errorf("expected nil (1 match < threshold 2), got %d bindings", len(got))
	}
}

// TestAtLeastElementsMatcher_AtLeastOne pins the threshold-1 case
// (alias for SomeElementsMatcher's behaviour).
func TestAtLeastElementsMatcher_AtLeastOne(t *testing.T) {
	t.Parallel()
	m := NewAtLeastElementsMatcher(1, NewConstantMatcher())
	in := []any{
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 1 {
		t.Fatalf("expected 1 binding (1 ConstantValue matched), got %d", len(got))
	}
}

// TestAtLeastElementsMatcher_ZeroThreshold_AlwaysMatches pins the
// vacuous-true case: minMatches=0 always matches. Empty input or
// no-element-matches both return a single binding (the matcher
// against the input).
func TestAtLeastElementsMatcher_ZeroThreshold_AlwaysMatches(t *testing.T) {
	t.Parallel()
	m := NewAtLeastElementsMatcher(0, NewConstantMatcher())

	t.Run("empty-input", func(t *testing.T) {
		t.Parallel()
		got := m.BindMatches(NewBindings(), []any{})
		if len(got) != 1 {
			t.Fatalf("expected 1 vacuous binding, got %d", len(got))
		}
	})
	t.Run("no-element-matches", func(t *testing.T) {
		t.Parallel()
		in := []any{
			&values.FieldValue{Field: "x", Typ: values.TypeInt},
			&values.FieldValue{Field: "y", Typ: values.TypeInt},
		}
		got := m.BindMatches(NewBindings(), in)
		if len(got) != 1 {
			t.Fatalf("expected 1 vacuous binding, got %d", len(got))
		}
	})
	t.Run("with-element-matches", func(t *testing.T) {
		t.Parallel()
		in := []any{
			&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
			&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		}
		got := m.BindMatches(NewBindings(), in)
		if len(got) != 2 {
			t.Fatalf("expected 2 element-bindings, got %d", len(got))
		}
	})
}

// TestAtLeastElementsMatcher_NonSliceInput pins the type-guard.
func TestAtLeastElementsMatcher_NonSliceInput(t *testing.T) {
	t.Parallel()
	m := NewAtLeastElementsMatcher(1, NewConstantMatcher())
	for _, in := range []any{nil, "string", 42} {
		in := in
		if got := m.BindMatches(NewBindings(), in); got != nil {
			t.Errorf("non-slice %T: got %d bindings, want nil", in, len(got))
		}
	}
}

// TestAtLeastElementsMatcher_RejectsNegative pins the constructor's
// negative-minMatches panic. Java's `Verify.verify(min >= 0)` has
// the same effect.
func TestAtLeastElementsMatcher_RejectsNegative(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on negative minMatches")
		}
	}()
	_ = NewAtLeastElementsMatcher(-1, NewConstantMatcher())
}

// TestAtLeastElementsMatcher_DistinctIdentity pins distinct map-key
// identity for two AtLeastElementsMatcher instances.
func TestAtLeastElementsMatcher_DistinctIdentity(t *testing.T) {
	t.Parallel()
	a := NewAtLeastElementsMatcher(1, NewConstantMatcher())
	b := NewAtLeastElementsMatcher(1, NewConstantMatcher())
	in := []any{&values.ConstantValue{Value: int64(1), Typ: values.TypeInt}}

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
