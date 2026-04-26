package matching

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestTypedMatcher_ExtractAndMatch pins the canonical case: input
// is an ArithmeticValue, extractor pulls .Left, downstream matches
// when Left is a ConstantValue.
func TestTypedMatcher_ExtractAndMatch(t *testing.T) {
	t.Parallel()
	m := NewTypedMatcher[*values.ArithmeticValue, any](
		"ArithLeft",
		func(av *values.ArithmeticValue) any { return av.Left },
		NewConstantMatcher(),
	)
	in := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		Right: &values.FieldValue{Field: "x", Typ: values.TypeInt},
	}
	got := m.BindMatches(NewBindings(), in)
	if len(got) != 1 {
		t.Fatalf("expected 1 match (Left is ConstantValue), got %d", len(got))
	}
}

// TestTypedMatcher_DownstreamFails pins the failure path: extractor
// runs cleanly but downstream rejects the extracted value (Left is
// a FieldValue, not a ConstantValue).
func TestTypedMatcher_DownstreamFails(t *testing.T) {
	t.Parallel()
	m := NewTypedMatcher[*values.ArithmeticValue, any](
		"ArithLeft",
		func(av *values.ArithmeticValue) any { return av.Left },
		NewConstantMatcher(),
	)
	in := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.FieldValue{Field: "x", Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
	}
	if got := m.BindMatches(NewBindings(), in); got != nil {
		t.Errorf("expected nil (Left is FieldValue), got %d bindings", len(got))
	}
}

// TestTypedMatcher_HostTypeMismatch pins the type-assertion failure:
// in is the wrong type, the extractor never runs.
func TestTypedMatcher_HostTypeMismatch(t *testing.T) {
	t.Parallel()
	extracted := false
	m := NewTypedMatcher[*values.ArithmeticValue, any](
		"ArithLeft",
		func(av *values.ArithmeticValue) any {
			extracted = true
			return av.Left
		},
		NewConstantMatcher(),
	)
	in := &values.ConstantValue{Value: int64(7), Typ: values.TypeInt}
	if got := m.BindMatches(NewBindings(), in); got != nil {
		t.Errorf("expected nil for wrong host type, got %d", len(got))
	}
	if extracted {
		t.Errorf("extractor ran on wrong host type — should short-circuit on type assertion")
	}
}

// TestTypedMatcher_RejectsNilExtract pins the constructor's nil-
// extract panic.
func TestTypedMatcher_RejectsNilExtract(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil extract")
		}
	}()
	_ = NewTypedMatcher[*values.ArithmeticValue, any]("X", nil, NewConstantMatcher())
}

// TestTypedMatcher_RejectsNilDownstream pins the constructor's nil-
// downstream panic.
func TestTypedMatcher_RejectsNilDownstream(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil downstream")
		}
	}()
	_ = NewTypedMatcher[*values.ArithmeticValue, any]("X",
		func(av *values.ArithmeticValue) any { return nil }, nil)
}

// TestTypedMatcher_DistinctIdentity pins distinct map-key identity
// for two NewTypedMatcher instances. Extractor returns the host
// itself so the downstream's Instance matcher succeeds (testing
// only the identity contract here, not the extract logic).
func TestTypedMatcher_DistinctIdentity(t *testing.T) {
	t.Parallel()
	a := NewTypedMatcher[*values.ConstantValue, any](
		"X", func(c *values.ConstantValue) any { return c }, NewConstantMatcher())
	b := NewTypedMatcher[*values.ConstantValue, any](
		"X", func(c *values.ConstantValue) any { return c }, NewConstantMatcher())
	in := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
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
