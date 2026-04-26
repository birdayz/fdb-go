package matching

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestNegationMatcher_DownstreamFails_Negates pins the success
// case: downstream rejects the input → NegationMatcher succeeds.
func TestNegationMatcher_DownstreamFails_Negates(t *testing.T) {
	t.Parallel()
	// Downstream wants ConstantValue; input is FieldValue → fails.
	m := NewNegationMatcher(NewConstantMatcher())
	got := m.BindMatches(NewBindings(), &values.FieldValue{Field: "x", Typ: values.TypeInt})
	if len(got) != 1 {
		t.Fatalf("expected 1 binding (downstream rejected), got %d", len(got))
	}
}

// TestNegationMatcher_DownstreamMatches_Fails pins the failure
// case: downstream accepts the input → NegationMatcher returns nil.
func TestNegationMatcher_DownstreamMatches_Fails(t *testing.T) {
	t.Parallel()
	m := NewNegationMatcher(NewConstantMatcher())
	got := m.BindMatches(NewBindings(), &values.ConstantValue{Value: int64(7), Typ: values.TypeInt})
	if got != nil {
		t.Fatalf("expected nil (downstream matched), got %d bindings", len(got))
	}
}

// TestNegationMatcher_DoubleNegation pins behavior of nesting two
// NegationMatchers: NEG(NEG(downstream)) ≈ downstream (modulo
// what gets bound to whom). When the inner downstream matches,
// inner NEG fails, outer NEG succeeds — so the input matches.
func TestNegationMatcher_DoubleNegation(t *testing.T) {
	t.Parallel()
	inner := NewNegationMatcher(NewConstantMatcher())
	outer := NewNegationMatcher(inner)

	// ConstantValue: downstream matches → inner fails → outer matches.
	gotMatch := outer.BindMatches(NewBindings(), &values.ConstantValue{Value: int64(7), Typ: values.TypeInt})
	if len(gotMatch) != 1 {
		t.Fatalf("expected 1 binding (double-negation matches downstream), got %d", len(gotMatch))
	}
	// FieldValue: downstream fails → inner matches → outer fails.
	gotFail := outer.BindMatches(NewBindings(), &values.FieldValue{Field: "x", Typ: values.TypeInt})
	if gotFail != nil {
		t.Fatalf("expected nil (double-negation rejects what downstream rejects), got %d", len(gotFail))
	}
}

// TestNegationMatcher_RejectsNilDownstream pins constructor panic.
func TestNegationMatcher_RejectsNilDownstream(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil downstream")
		}
	}()
	_ = NewNegationMatcher(nil)
}

// TestNegationMatcher_DistinctIdentity pins distinct map-key
// identity for two NegationMatcher instances.
func TestNegationMatcher_DistinctIdentity(t *testing.T) {
	t.Parallel()
	a := NewNegationMatcher(NewConstantMatcher())
	b := NewNegationMatcher(NewConstantMatcher())
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
