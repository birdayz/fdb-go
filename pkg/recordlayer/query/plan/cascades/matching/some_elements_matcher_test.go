package matching

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSomeElementsMatcher_AnyMatchSucceeds pins the canonical
// success case: one of three elements matches the downstream → the
// matcher succeeds and binds the slice. Mirrors Java's
// MultiMatcher.SomeMatcher tests (`stream.anyMatch`).
func TestSomeElementsMatcher_AnyMatchSucceeds(t *testing.T) {
	t.Parallel()
	// Downstream matches only ConstantValue.
	m := NewSomeElementsMatcher(NewConstantMatcher())
	in := []any{
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(7), Typ: values.TypeInt},
		&values.FieldValue{Field: "y", Typ: values.TypeString},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	bound := Get[[]any](got[0], m)
	if len(bound) != len(in) {
		t.Errorf("bound slice has wrong length: got %d, want %d", len(bound), len(in))
	}
}

// TestSomeElementsMatcher_AllMatchProduceCartesian pins that when
// every element matches, the matcher yields one binding per
// matching element (not a single binding) so rule bodies that
// retrieve the downstream's binding via GetAll see every match.
func TestSomeElementsMatcher_AllMatchProduceCartesian(t *testing.T) {
	t.Parallel()
	m := NewSomeElementsMatcher(NewConstantMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 3 {
		t.Fatalf("expected 3 matches (one per matching element), got %d", len(got))
	}
}

// TestSomeElementsMatcher_NoMatch pins the failure case: when no
// element matches, the matcher returns nil. Distinct from
// AllElementsMatcher which would also fail here.
func TestSomeElementsMatcher_NoMatch(t *testing.T) {
	t.Parallel()
	m := NewSomeElementsMatcher(NewConstantMatcher())
	in := []any{
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		&values.FieldValue{Field: "y", Typ: values.TypeString},
	}
	if got := m.BindMatches(NewBindings(), in); got != nil {
		t.Fatalf("expected nil (no match), got %d bindings", len(got))
	}
}

// TestSomeElementsMatcher_EmptyInputFails pins the documented
// asymmetry vs AllElementsMatcher: empty input does NOT match
// SomeElementsMatcher (no element to bind). AllElementsMatcher's
// vacuous-true semantics are intentionally not mirrored here —
// matches Java's SomeMatcher.
func TestSomeElementsMatcher_EmptyInputFails(t *testing.T) {
	t.Parallel()
	m := NewSomeElementsMatcher(NewConstantMatcher())
	if got := m.BindMatches(NewBindings(), []any{}); got != nil {
		t.Errorf("empty input should NOT match (vs AllElementsMatcher's vacuous true): got %d bindings", len(got))
	}
}

// TestSomeElementsMatcher_NonSliceInput pins the type guard. The
// matcher only accepts []any; anything else (string, struct, nil)
// returns nil immediately.
func TestSomeElementsMatcher_NonSliceInput(t *testing.T) {
	t.Parallel()
	m := NewSomeElementsMatcher(NewConstantMatcher())
	for _, in := range []any{nil, "string", 42, &values.ConstantValue{}} {
		in := in
		if got := m.BindMatches(NewBindings(), in); got != nil {
			t.Errorf("non-slice %T: got %d bindings, want nil", in, len(got))
		}
	}
}

// TestCollectionMatcher_InterfaceImpls pins the marker-interface
// constraint: every collection-shaped matcher (AllElements / Some /
// AtLeast / List) satisfies CollectionMatcher; bare BindingMatchers
// (NewConstantMatcher, NewAnyValue) do not. Compile-time check via
// var assignment + a runtime sanity test.
func TestCollectionMatcher_InterfaceImpls(t *testing.T) {
	t.Parallel()
	// Compile-time interface conformance.
	var _ CollectionMatcher = (*AllElementsMatcher)(nil)
	var _ CollectionMatcher = (*SomeElementsMatcher)(nil)
	var _ CollectionMatcher = (*AtLeastElementsMatcher)(nil)
	var _ CollectionMatcher = (*ListMatcher)(nil)
	// Runtime sanity — the marker method is reachable.
	m := NewSomeElementsMatcher(NewConstantMatcher())
	var c CollectionMatcher = m
	if c.RootType() != "SomeElements" {
		t.Errorf("interface dispatch: got %q, want %q", c.RootType(), "SomeElements")
	}
}

// TestSomeElementsMatcher_DistinctIdentity pins that two
// SomeElementsMatcher instances bind to distinct identities in
// PlannerBindings — the downstream pointer field gives the struct
// non-zero size, so map-key identity comes from the allocation
// rather than the zero-size-struct collision (see AnyValue in
// matcher.go for the original gotcha).
func TestSomeElementsMatcher_DistinctIdentity(t *testing.T) {
	t.Parallel()
	a := NewSomeElementsMatcher(NewConstantMatcher())
	b := NewSomeElementsMatcher(NewConstantMatcher())
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
