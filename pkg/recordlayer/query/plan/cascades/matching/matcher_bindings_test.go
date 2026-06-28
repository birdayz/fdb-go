package matching

// Deeper contract tests for PlannerBindings — the multimap that
// matchers return, where rule bodies fetch bound values. matcher_test.go
// covers the happy path; this file pins the contract corners that
// are easy to break under refactor:
//
//   - Bind is immutable (caller's Bindings never mutates).
//   - MergedWith concatenates per-matcher slices and is associative-ish
//     in the empty cases (used by combinators.AllOf — see RFC-023 §
//     Changes item 4).
//   - GetAll returns nil for unbound matchers and the full slice for
//     repeated binds.
//   - Get panics on 0 or >1 bindings.
//   - ArithmeticMatcher rejects op mismatch independently of child
//     matcher rejection.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPlannerBindings_BindIsImmutable: Bind must NOT mutate the
// receiver. Speculative matches in the rule engine retry many shapes
// against the same outer bindings; mutation would silently leak
// across attempts.
func TestPlannerBindings_BindIsImmutable(t *testing.T) {
	t.Parallel()
	m1 := NewConstantMatcher()
	m2 := NewConstantMatcher()
	cv1 := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	cv2 := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}

	original := NewBindings().Bind(m1, cv1)
	derived := original.Bind(m2, cv2)

	// original must NOT see m2.
	if got := original.GetAll(m2); len(got) != 0 {
		t.Fatalf("Bind mutated receiver: original.GetAll(m2)=%v, want empty", got)
	}
	// derived sees both.
	if got := derived.GetAll(m1); len(got) != 1 || got[0] != cv1 {
		t.Fatalf("derived.GetAll(m1)=%v, want [cv1]", got)
	}
	if got := derived.GetAll(m2); len(got) != 1 || got[0] != cv2 {
		t.Fatalf("derived.GetAll(m2)=%v, want [cv2]", got)
	}
}

// TestPlannerBindings_BindAccumulates: Bind with the SAME matcher
// twice produces a 2-element slice, addressable via GetAll. This is
// the contract AllOf relies on — multiple matches under one matcher
// identity surface as a list.
func TestPlannerBindings_BindAccumulates(t *testing.T) {
	t.Parallel()
	m := NewAnyValue()
	cv1 := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	cv2 := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	b := NewBindings().Bind(m, cv1).Bind(m, cv2)

	got := b.GetAll(m)
	if len(got) != 2 {
		t.Fatalf("GetAll returned %d entries, want 2", len(got))
	}
	if got[0] != cv1 || got[1] != cv2 {
		t.Fatalf("GetAll order/values wrong: got %v", got)
	}
}

// TestPlannerBindings_GetAll_UnboundReturnsNil pins the documented
// "empty means no match" — GetAll on an unknown matcher must return
// a nil slice, not panic.
func TestPlannerBindings_GetAll_UnboundReturnsNil(t *testing.T) {
	t.Parallel()
	m := NewConstantMatcher()
	b := NewBindings()
	if got := b.GetAll(m); got != nil {
		t.Fatalf("GetAll on unbound matcher returned %v, want nil", got)
	}
}

// TestPlannerBindings_Get_PanicsOnZero: Get expects exactly 1 binding.
// Zero is a programming error — the rule pattern said the matcher
// would bind, and it didn't.
func TestPlannerBindings_Get_PanicsOnZero(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Get with no bindings")
		}
	}()
	m := NewConstantMatcher()
	_ = NewBindings().Get(m)
}

// TestPlannerBindings_Get_PanicsOnMultiple: Get with >1 bindings
// also panics — the rule body picked the wrong API; it should be
// using GetAll.
func TestPlannerBindings_Get_PanicsOnMultiple(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Get with multiple bindings")
		}
	}()
	m := NewAnyValue()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	b := NewBindings().Bind(m, cv).Bind(m, cv)
	_ = b.Get(m)
}

// TestPlannerBindings_MergedWith_EmptyCases: empty + x = x,
// x + empty = x. Mirrors Java's PlannerBindings.empty() identity
// behaviour.
func TestPlannerBindings_MergedWith_EmptyCases(t *testing.T) {
	t.Parallel()
	m := NewConstantMatcher()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	withVal := NewBindings().Bind(m, cv)
	empty := NewBindings()

	if got := withVal.MergedWith(empty); got != withVal {
		t.Fatalf("withVal.MergedWith(empty) should return withVal, got new alloc")
	}
	if got := empty.MergedWith(withVal); got != withVal {
		t.Fatalf("empty.MergedWith(withVal) should return withVal, got new alloc")
	}
	if got := withVal.MergedWith(nil); got != withVal {
		t.Fatalf("withVal.MergedWith(nil) should return withVal")
	}
}

// TestPlannerBindings_MergedWith_ConcatenatesSameMatcher: when both
// sides bound a value under the same matcher, the merged result
// concatenates them (b's then other's). This is what AllOf relies
// on to fold a list of downstream matches into a single bindings.
func TestPlannerBindings_MergedWith_ConcatenatesSameMatcher(t *testing.T) {
	t.Parallel()
	m := NewAnyValue()
	cv1 := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	cv2 := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	left := NewBindings().Bind(m, cv1)
	right := NewBindings().Bind(m, cv2)

	merged := left.MergedWith(right)
	got := merged.GetAll(m)
	if len(got) != 2 {
		t.Fatalf("merged.GetAll(m) had %d entries, want 2", len(got))
	}
	if got[0] != cv1 || got[1] != cv2 {
		t.Fatalf("merge order wrong: got %v, want [cv1, cv2]", got)
	}
}

// TestPlannerBindings_MergedWith_DisjointMatchers: distinct matchers
// merge without overlap.
func TestPlannerBindings_MergedWith_DisjointMatchers(t *testing.T) {
	t.Parallel()
	m1 := NewConstantMatcher()
	m2 := NewFieldMatcher()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	fv := &values.FieldValue{Field: "x", Typ: values.TypeInt}

	left := NewBindings().Bind(m1, cv)
	right := NewBindings().Bind(m2, fv)
	merged := left.MergedWith(right)

	if got := merged.Get(m1); got != cv {
		t.Fatalf("merged.Get(m1)=%v, want cv", got)
	}
	if got := merged.Get(m2); got != fv {
		t.Fatalf("merged.Get(m2)=%v, want fv", got)
	}
}

// TestPlannerBindings_MergedWith_ImmutableInputs: MergedWith must NOT
// mutate either side. Caller may reuse them in further matches.
func TestPlannerBindings_MergedWith_ImmutableInputs(t *testing.T) {
	t.Parallel()
	m := NewAnyValue()
	cv1 := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	cv2 := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	left := NewBindings().Bind(m, cv1)
	right := NewBindings().Bind(m, cv2)

	_ = left.MergedWith(right)

	if got := left.GetAll(m); len(got) != 1 || got[0] != cv1 {
		t.Fatalf("MergedWith mutated left: got %v", got)
	}
	if got := right.GetAll(m); len(got) != 1 || got[0] != cv2 {
		t.Fatalf("MergedWith mutated right: got %v", got)
	}
}

// TestArithmeticMatcher_OpMismatch: when the input is an
// ArithmeticValue but the operator doesn't match, return empty.
// Independent of child-matcher rejection.
func TestArithmeticMatcher_OpMismatch(t *testing.T) {
	t.Parallel()
	matcher := &ArithmeticMatcher{
		Op:    values.OpAdd,
		Left:  NewAnyValue(),
		Right: NewAnyValue(),
	}
	// Input has matching shape but wrong op.
	expr := &values.ArithmeticValue{
		Op:    values.OpSub,
		Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	if got := matcher.BindMatches(NewBindings(), expr); len(got) != 0 {
		t.Fatalf("op mismatch should return empty, got %d matches", len(got))
	}
}

// TestArithmeticMatcher_BindsHostNode: a successful match must bind
// the host ArithmeticValue under the matcher's identity, so the rule
// body can fetch it. (Easy to drop accidentally during refactor —
// the loop at the end of BindMatches.)
func TestArithmeticMatcher_BindsHostNode(t *testing.T) {
	t.Parallel()
	matcher := &ArithmeticMatcher{
		Op:    values.OpAdd,
		Left:  NewAnyValue(),
		Right: NewAnyValue(),
	}
	expr := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	got := matcher.BindMatches(NewBindings(), expr)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	host, ok := got[0].Get(matcher).(*values.ArithmeticValue)
	if !ok {
		t.Fatalf("matcher did not bind the host ArithmeticValue: %T", got[0].Get(matcher))
	}
	if host != expr {
		t.Fatalf("matcher bound a different node than the input expr")
	}
}

// TestAnyValue_RejectsNonValue: BindMatches contract — non-Value
// inputs return nil, not panic.
func TestAnyValue_RejectsNonValue(t *testing.T) {
	t.Parallel()
	m := NewAnyValue()
	if got := m.BindMatches(NewBindings(), 42); got != nil {
		t.Fatalf("AnyValue.BindMatches(42) should be nil, got %v", got)
	}
	if got := m.BindMatches(NewBindings(), "string"); got != nil {
		t.Fatalf("AnyValue.BindMatches(string) should be nil, got %v", got)
	}
	if got := m.BindMatches(NewBindings(), nil); got != nil {
		t.Fatalf("AnyValue.BindMatches(nil) should be nil, got %v", got)
	}
}

// TestInstance_NewConstantMatcher_RejectsField: NewConstantMatcher
// must reject *FieldValue (and any other Value subtype). Pinned
// because Instance's matches closure is the only thing keeping the
// type discipline.
func TestInstance_NewConstantMatcher_RejectsField(t *testing.T) {
	t.Parallel()
	m := NewConstantMatcher()
	fv := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	if got := m.BindMatches(NewBindings(), fv); got != nil {
		t.Fatalf("ConstantMatcher matched a FieldValue: %v", got)
	}
}

// TestInstance_NewFieldMatcher_RejectsConstant: symmetric.
func TestInstance_NewFieldMatcher_RejectsConstant(t *testing.T) {
	t.Parallel()
	m := NewFieldMatcher()
	cv := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	if got := m.BindMatches(NewBindings(), cv); got != nil {
		t.Fatalf("FieldMatcher matched a ConstantValue: %v", got)
	}
}

// TestInstance_RootType identifies which Go type each Instance
// matcher claims, used by combinator dispatch in shape (a).
func TestInstance_RootType(t *testing.T) {
	t.Parallel()
	if got := NewConstantMatcher().RootType(); got != "ConstantValue" {
		t.Fatalf("ConstantMatcher.RootType()=%q, want ConstantValue", got)
	}
	if got := NewFieldMatcher().RootType(); got != "FieldValue" {
		t.Fatalf("FieldMatcher.RootType()=%q, want FieldValue", got)
	}
	if got := NewAnyValue().RootType(); got != "Value" {
		t.Fatalf("AnyValue.RootType()=%q, want Value", got)
	}
	a := &ArithmeticMatcher{Op: values.OpAdd, Left: NewAnyValue(), Right: NewAnyValue()}
	if got := a.RootType(); got != "ArithmeticValue" {
		t.Fatalf("ArithmeticMatcher.RootType()=%q, want ArithmeticValue", got)
	}
}
