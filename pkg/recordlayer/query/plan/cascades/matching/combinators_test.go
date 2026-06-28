package matching

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// AllOfMatcher: every downstream must match.
func TestAllOf_AllDownstreamsMustMatch(t *testing.T) {
	t.Parallel()

	// Setup: pattern matches "a ConstantValue AND anything".
	constMatcher := NewConstantMatcher()
	anyMatcher := NewAnyValue()
	pattern := NewAllOf("ConstantValue", constMatcher, anyMatcher)

	cv := &values.ConstantValue{Value: int64(7), Typ: values.TypeInt}
	got := pattern.BindMatches(NewBindings(), cv)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	b := got[0]
	// Both downstream matchers + the AllOf itself should be bound.
	if Get[*values.ConstantValue](b, constMatcher) != cv {
		t.Fatal("constMatcher binding wrong")
	}
	if Get[values.Value](b, anyMatcher) != values.Value(cv) {
		t.Fatal("anyMatcher binding wrong")
	}
	if Get[*values.ConstantValue](b, pattern) != cv {
		t.Fatal("allOf self-binding wrong")
	}
}

// AllOf collapses to empty when any single downstream fails.
func TestAllOf_AnyFailureCollapses(t *testing.T) {
	t.Parallel()

	// Expects ConstantValue AND FieldValue — input is ConstantValue,
	// fails the field matcher, AllOf returns empty.
	pattern := NewAllOf("Value", NewConstantMatcher(), NewFieldMatcher())

	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	if got := pattern.BindMatches(NewBindings(), cv); len(got) != 0 {
		t.Fatalf("expected 0 matches on AND failure, got %d", len(got))
	}
}

// AnyOfMatcher: at least one downstream matches.
func TestAnyOf_UnionOfMatches(t *testing.T) {
	t.Parallel()

	constMatcher := NewConstantMatcher()
	fieldMatcher := NewFieldMatcher()
	pattern := NewAnyOf("Value", constMatcher, fieldMatcher)

	// ConstantValue input: only constMatcher matches → 1 result.
	cv := &values.ConstantValue{Value: int64(3), Typ: values.TypeInt}
	got := pattern.BindMatches(NewBindings(), cv)
	if len(got) != 1 {
		t.Fatalf("ConstantValue input: expected 1 match, got %d", len(got))
	}
	// The AnyOf combinator itself is bound; the specific down-
	// stream that matched is also bound.
	if Get[*values.ConstantValue](got[0], constMatcher) != cv {
		t.Fatal("ConstantValue did not bind constMatcher")
	}

	// FieldValue input: only fieldMatcher matches → 1 result.
	fv := &values.FieldValue{Field: "name", Typ: values.TypeString}
	got = pattern.BindMatches(NewBindings(), fv)
	if len(got) != 1 {
		t.Fatalf("FieldValue input: expected 1 match, got %d", len(got))
	}
}

// AnyOf collapses to empty when no downstream matches.
func TestAnyOf_NoMatchCollapses(t *testing.T) {
	t.Parallel()
	pattern := NewAnyOf("Value", NewConstantMatcher(), NewFieldMatcher())
	av := &values.ArithmeticValue{Op: values.OpAdd}
	if got := pattern.BindMatches(NewBindings(), av); len(got) != 0 {
		t.Fatalf("expected 0 matches when no downstream fires, got %d", len(got))
	}
}

// AllOf + AnyOf composition — real rule patterns nest these.
func TestCombinators_Nested(t *testing.T) {
	t.Parallel()

	// Pattern: ArithmeticValue whose children include ANY of
	// (Constant, Field).
	constMatcher := NewConstantMatcher()
	fieldMatcher := NewFieldMatcher()
	inner := NewAnyOf("Value", constMatcher, fieldMatcher)
	pattern := &ArithmeticMatcher{
		Op:    values.OpAdd,
		Left:  inner,
		Right: inner,
	}

	// 5 + name → both sides match inner; AllOf-style composition of
	// ArithmeticMatcher's left×right bindings yields 1 result.
	expr := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		Right: &values.FieldValue{Field: "name", Typ: values.TypeString},
	}
	got := pattern.BindMatches(NewBindings(), expr)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
}

// PlannerBindings.MergedWith: nil-safe fast paths.
func TestMergedWith_NilSafety(t *testing.T) {
	t.Parallel()
	empty := NewBindings()
	m := NewConstantMatcher()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	nonEmpty := empty.Bind(m, cv)

	// empty.MergedWith(nonEmpty) → nonEmpty
	if got := empty.MergedWith(nonEmpty); got != nonEmpty {
		t.Fatal("empty merge should short-circuit to rhs")
	}
	// nonEmpty.MergedWith(empty) → nonEmpty
	if got := nonEmpty.MergedWith(empty); got != nonEmpty {
		t.Fatal("empty merge should short-circuit to lhs")
	}
	// nonEmpty.MergedWith(nil) → nonEmpty
	if got := nonEmpty.MergedWith(nil); got != nonEmpty {
		t.Fatal("nil merge should short-circuit to lhs")
	}
}

// NewAllOf must panic if zero downstreams — a degenerate pattern that
// never composed into a useful rule, and silently changing semantics
// to "matches anything" would be worse than a panic at construction
// time.
func TestNewAllOf_ZeroDownstreamsPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewAllOf() with zero downstreams should panic")
		}
	}()
	_ = NewAllOf("Value")
}

// NewAnyOf — same shape: zero downstreams = always-fail; panic at
// construction time so the rule author sees the bug.
func TestNewAnyOf_ZeroDownstreamsPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewAnyOf() with zero downstreams should panic")
		}
	}()
	_ = NewAnyOf("Value")
}

// AllOf with a multi-match downstream multiplies the result count —
// the documented Cartesian-product semantics. AnyValue is the only
// generic multi-match shape we have today, so we use two layered
// bindings to force the multiplication.
//
// Scenario: outer bindings already have two values bound under
// matcher M (via AnyValue+repeated Bind). AllOf seeds its accumulator
// with `outer`, then each downstream sees that single seed. Multi-match
// only surfaces if a downstream returns >1 result for the same input;
// AnyValue always returns 1. To pin Cartesian behaviour we use a
// custom matcher that returns 2 results for a given input.
func TestAllOf_CartesianProduct(t *testing.T) {
	t.Parallel()
	multi := &doubleMatcher{}
	any := NewAnyValue()
	pattern := NewAllOf("Value", multi, any)

	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	got := pattern.BindMatches(NewBindings(), cv)
	// doubleMatcher returns 2 partial bindings; any then returns 1
	// each → 2 final results. AllOf's self-bind doesn't change count.
	if len(got) != 2 {
		t.Fatalf("expected Cartesian product of 2×1 = 2 matches, got %d", len(got))
	}
}

// AnyOf preserves downstream order: results from downstream[0] appear
// before downstream[1]'s. Java's stream-based impl has the same
// ordering guarantee; rule pattern matching depends on it for
// deterministic explain output.
func TestAnyOf_PreservesDownstreamOrder(t *testing.T) {
	t.Parallel()
	// Two distinct AnyValue matchers — one for "first", one for
	// "second" — so we can identify which downstream produced
	// each binding.
	first := NewAnyValue()
	second := NewAnyValue()
	pattern := NewAnyOf("Value", first, second)

	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	got := pattern.BindMatches(NewBindings(), cv)
	if len(got) != 2 {
		t.Fatalf("expected 2 matches (one per downstream), got %d", len(got))
	}
	// First result must be from `first`, second from `second`.
	if got[0].GetAll(first) == nil {
		t.Fatal("got[0] should carry the `first` matcher's binding")
	}
	if got[1].GetAll(second) == nil {
		t.Fatal("got[1] should carry the `second` matcher's binding")
	}
}

// AllOf threads outer bindings through every downstream — a value
// already bound in `outer` is visible in the final result.
func TestAllOf_ThreadsOuterBindings(t *testing.T) {
	t.Parallel()
	outerMatcher := NewAnyValue()
	preset := &values.FieldValue{Field: "preset", Typ: values.TypeInt}
	outer := NewBindings().Bind(outerMatcher, preset)

	pattern := NewAllOf("Value", NewConstantMatcher())
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	got := pattern.BindMatches(outer, cv)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	// The outer binding survives.
	if got[0].Get(outerMatcher) != preset {
		t.Fatalf("outer binding lost during AllOf threading")
	}
}

// doubleMatcher is a test-only matcher that always emits TWO
// successful PlannerBindings for any input — used to pin Cartesian
// behaviour where Go-side test fixtures don't have a natural
// multi-match shape today.
//
// The `_ bool` field forces a non-zero-size struct (1 byte) so two
// `&doubleMatcher{}` allocations land at distinct heap addresses.
// Per the zero-size-struct gotcha at AnyValue (matcher.go:130-136),
// pointers to distinct zero-size variables MAY share an address
// (Go's runtime parks them all at `runtime.zerobase`), which would
// silently collide in PlannerBindings's matcher → []any map.
//
// `_ [0]func()` is NOT the right idiom here — it has size 0 (the
// outer length is 0 regardless of the element type) and only makes
// the struct non-comparable, which is unrelated. `_ bool` is the
// minimal mechanism that actually forces non-zero size.
type doubleMatcher struct {
	_ bool
}

func (*doubleMatcher) RootType() string { return "Value" }
func (d *doubleMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	return []*PlannerBindings{outer.Bind(d, in), outer.Bind(d, in)}
}

// PlannerBindings.MergedWith: values from both inputs visible.
func TestMergedWith_BothBindingsVisible(t *testing.T) {
	t.Parallel()
	m1 := NewConstantMatcher()
	m2 := NewFieldMatcher()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	fv := &values.FieldValue{Field: "x", Typ: values.TypeString}
	b1 := NewBindings().Bind(m1, cv)
	b2 := NewBindings().Bind(m2, fv)

	merged := b1.MergedWith(b2)
	if Get[*values.ConstantValue](merged, m1) != cv {
		t.Fatal("merged: lhs binding lost")
	}
	if Get[*values.FieldValue](merged, m2) != fv {
		t.Fatal("merged: rhs binding lost")
	}
}
