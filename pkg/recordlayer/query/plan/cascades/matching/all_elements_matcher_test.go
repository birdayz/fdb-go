package matching

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestAllElementsMatcher_AllConstants pins the canonical case: every
// element of the input matches the downstream Constant matcher.
func TestAllElementsMatcher_AllConstants(t *testing.T) {
	t.Parallel()
	m := NewAllElementsMatcher(NewConstantMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
}

// TestAllElementsMatcher_AnyMissDeclines pins the "all-or-nothing"
// contract: a single non-matching element collapses the whole match
// to nil.
func TestAllElementsMatcher_AnyMissDeclines(t *testing.T) {
	t.Parallel()
	m := NewAllElementsMatcher(NewConstantMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.FieldValue{Field: "x", Typ: values.TypeInt}, // not a Constant
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	if got := m.BindMatches(NewBindings(), in); got != nil {
		t.Fatalf("expected nil on partial match, got %d matches", len(got))
	}
}

// TestAllElementsMatcher_EmptyInputMatchesVacuously pins the SQL-ish
// "vacuous truth" — an empty collection matches because every (zero)
// element satisfies any predicate. Mirrors Java's AllMatcher.
func TestAllElementsMatcher_EmptyInputMatchesVacuously(t *testing.T) {
	t.Parallel()
	m := NewAllElementsMatcher(NewConstantMatcher())
	got := m.BindMatches(NewBindings(), []any{})
	if len(got) != 1 {
		t.Fatalf("empty input should match vacuously, got %d matches", len(got))
	}
	// Host-binding is the empty slice itself.
	host, ok := got[0].Get(m).([]any)
	if !ok || len(host) != 0 {
		t.Fatalf("matcher should bind empty slice, got %v (%T)", host, got[0].Get(m))
	}
}

// TestAllElementsMatcher_NonSliceInput_ReturnsEmpty pins the type-
// guard: non-[]any input declines cleanly without panicking.
func TestAllElementsMatcher_NonSliceInput_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	m := NewAllElementsMatcher(NewAnyValue())
	if got := m.BindMatches(NewBindings(), &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}); got != nil {
		t.Fatalf("Value input: expected nil, got %d matches", len(got))
	}
	if got := m.BindMatches(NewBindings(), nil); got != nil {
		t.Fatalf("nil input: expected nil")
	}
	if got := m.BindMatches(NewBindings(), 42); got != nil {
		t.Fatalf("int input: expected nil")
	}
}

// TestAllElementsMatcher_BindsHostNode pins that successful match
// binds the matcher itself to the input slice — rule bodies fetch
// the matched slice via Get[T](bindings, m).
func TestAllElementsMatcher_BindsHostNode(t *testing.T) {
	t.Parallel()
	m := NewAllElementsMatcher(NewAnyValue())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	host, ok := got[0].Get(m).([]any)
	if !ok || len(host) != 2 {
		t.Fatalf("matcher did not bind host slice (got %T)", got[0].Get(m))
	}
}

// TestAllElementsMatcher_CartesianProduct pins multi-match
// Cartesian-product accumulation. doubleMatcher (combinators_test.go)
// emits 2 bindings per input; with a 3-element input we get 2^3 = 8.
func TestAllElementsMatcher_CartesianProduct(t *testing.T) {
	t.Parallel()
	m := NewAllElementsMatcher(&doubleMatcher{})
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	// 2 × 2 × 2 = 8 product results.
	if len(got) != 8 {
		t.Fatalf("expected Cartesian 2^3 = 8 matches, got %d", len(got))
	}
}

// TestAllElementsMatcher_ThreadsOuterBindings pins outer-binding
// preservation across the all-elements scan. Symmetric to AllOf /
// ListMatcher tests.
func TestAllElementsMatcher_ThreadsOuterBindings(t *testing.T) {
	t.Parallel()
	outerMatcher := NewAnyValue()
	preset := &values.FieldValue{Field: "preset", Typ: values.TypeInt}
	outer := NewBindings().Bind(outerMatcher, preset)

	m := NewAllElementsMatcher(NewAnyValue())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	got := m.BindMatches(outer, in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	if got[0].Get(outerMatcher) != preset {
		t.Fatal("outer binding lost")
	}
}

// TestAllElementsMatcher_RootType pins the debug identifier.
func TestAllElementsMatcher_RootType(t *testing.T) {
	t.Parallel()
	if got := NewAllElementsMatcher(NewAnyValue()).RootType(); got != "AllElements" {
		t.Fatalf("RootType()=%q, want AllElements", got)
	}
}
