package matching

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestListMatcher_PairsByPosition pins the core ListMatcher contract:
// a 3-element input matched against [Constant, Field, Constant]
// downstreams binds each downstream to its positional element.
func TestListMatcher_PairsByPosition(t *testing.T) {
	t.Parallel()
	d0 := NewConstantMatcher()
	d1 := NewFieldMatcher()
	d2 := NewConstantMatcher()
	m := NewListMatcher(d0, d1, d2)

	cv0 := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	fv := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	cv2 := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	in := []any{cv0, fv, cv2}

	got := m.BindMatches(NewBindings(), in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	b := got[0]
	if Get[*values.ConstantValue](b, d0) != cv0 {
		t.Fatal("d0 didn't bind cv0")
	}
	if Get[*values.FieldValue](b, d1) != fv {
		t.Fatal("d1 didn't bind fv")
	}
	if Get[*values.ConstantValue](b, d2) != cv2 {
		t.Fatal("d2 didn't bind cv2")
	}
}

// TestListMatcher_LengthMismatch_ReturnsEmpty pins the documented
// length-discrepancy cutoff — neither too-short nor too-long inputs
// are matched. Both directions matter; one mismatch type covering
// both would break under refactor.
func TestListMatcher_LengthMismatch_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	m := NewListMatcher(NewAnyValue(), NewAnyValue())

	short := []any{&values.ConstantValue{Value: int64(1), Typ: values.TypeInt}}
	if got := m.BindMatches(NewBindings(), short); got != nil {
		t.Fatalf("too-short: expected nil, got %d matches", len(got))
	}

	long := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	if got := m.BindMatches(NewBindings(), long); got != nil {
		t.Fatalf("too-long: expected nil, got %d matches", len(got))
	}
}

// TestListMatcher_AnyDownstreamFailureCollapses pins the AND cutoff:
// if downstream[1] declines, the whole list-match returns nil even
// though downstream[0] would bind.
func TestListMatcher_AnyDownstreamFailureCollapses(t *testing.T) {
	t.Parallel()
	// Pattern: [Constant, Field] but input is [Constant, Constant]
	// — d1 (FieldMatcher) declines on the constant.
	m := NewListMatcher(NewConstantMatcher(), NewFieldMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	if got := m.BindMatches(NewBindings(), in); got != nil {
		t.Fatalf("expected nil on downstream failure, got %d matches", len(got))
	}
}

// TestListMatcher_NonSliceInput_ReturnsEmpty pins the type-guard:
// non-[]any inputs (e.g. a Value, a string, nil) decline cleanly
// rather than panicking.
func TestListMatcher_NonSliceInput_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	m := NewListMatcher(NewAnyValue())
	if got := m.BindMatches(NewBindings(), &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}); got != nil {
		t.Fatalf("Value input: expected nil, got %d matches", len(got))
	}
	if got := m.BindMatches(NewBindings(), nil); got != nil {
		t.Fatalf("nil input: expected nil, got %d matches", len(got))
	}
	if got := m.BindMatches(NewBindings(), "string"); got != nil {
		t.Fatalf("string input: expected nil, got %d matches", len(got))
	}
}

// TestListMatcher_BindsHostNode pins that successful match also
// binds the ListMatcher itself — rule bodies can fetch the whole
// matched slice via Get[T](bindings, listMatcher).
func TestListMatcher_BindsHostNode(t *testing.T) {
	t.Parallel()
	m := NewListMatcher(NewAnyValue(), NewAnyValue())
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
		t.Fatalf("matcher did not bind host slice: got %T", got[0].Get(m))
	}
}

// TestListMatcher_CartesianProduct pins the multi-match accumulator:
// when downstream positions return multiple matches, the result is
// the Cartesian product across positions. Uses doubleMatcher
// (declared in combinators_test.go) which emits 2 matches per call.
func TestListMatcher_CartesianProduct(t *testing.T) {
	t.Parallel()
	m := NewListMatcher(&doubleMatcher{}, &doubleMatcher{})
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	// 2 × 2 = 4 Cartesian-product results.
	if len(got) != 4 {
		t.Fatalf("expected Cartesian 2×2 = 4 matches, got %d", len(got))
	}
}

// TestNewListMatcher_ZeroDownstreamsPanics pins the construction-time
// rejection. Symmetric to NewAllOf / NewAnyOf.
func TestNewListMatcher_ZeroDownstreamsPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewListMatcher() with zero downstreams should panic")
		}
	}()
	_ = NewListMatcher()
}

// TestListMatcher_ThreadsOuterBindings pins that an outer binding
// (set up by an enclosing matcher) is preserved in the final result —
// every accumulated partial threads through outer's entries.
func TestListMatcher_ThreadsOuterBindings(t *testing.T) {
	t.Parallel()
	outerMatcher := NewAnyValue()
	preset := &values.FieldValue{Field: "preset", Typ: values.TypeInt}
	outer := NewBindings().Bind(outerMatcher, preset)

	m := NewListMatcher(NewAnyValue())
	in := []any{&values.ConstantValue{Value: int64(1), Typ: values.TypeInt}}
	got := m.BindMatches(outer, in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	if got[0].Get(outerMatcher) != preset {
		t.Fatal("outer binding lost during ListMatcher threading")
	}
}

// TestListMatcher_RootType pins the debug identifier. Used only by
// explain output, but matters for plan-diff render stability.
func TestListMatcher_RootType(t *testing.T) {
	t.Parallel()
	m := NewListMatcher(NewAnyValue())
	if got := m.RootType(); got != "List" {
		t.Fatalf("RootType()=%q, want List", got)
	}
}
