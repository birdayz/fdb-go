package values

import (
	"errors"
	"strings"
	"testing"
)

// Baked-ordinal UNIFICATION pins. The join-seed's Resolved *ResolvedAccessor
// and the recursive-CTE wrap's ResolvedOrdinal/HasResolvedOrdinal twin fields
// were unified onto a single ResolvedAccessor with a FrontierPinned contract
// bit: the loudness contract must be a property of the VALUE, invariant
// under transformation, because pullup/pushdown passthrough copies strip
// Child while sharing the accessor pointer. These tests pin the exact seams
// that unification closed (a regression could silently reopen them).

func singleAccessorOf(fv *FieldValue) (ResolvedAccessor, bool) {
	if fv.Resolved == nil {
		return ResolvedAccessor{}, false
	}
	return fv.Resolved.Single()
}

func unifiedTestQOV(t *testing.T) (*QuantifiedObjectValue, *RecordType) {
	t.Helper()
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: NotNullLong, Ordinal: 1},
	})
	return NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rt), rt
}

// TestWrapNodeSurvivesPassthroughCopies pins the silent-drop hole this
// unification closed: before it, the passthrough copies rebuilt
// &FieldValue{Field, Typ, Resolved} and DROPPED the wrap mechanism's twin
// scalar fields — a wrap node pulled or pushed through a QOV/ObjectValue
// passthrough silently lost its ordinal and degraded to a first-match name
// read (the conflation hazard). The wrap constructor now produces a
// Resolved accessor, which the copies preserve by pointer.
func TestWrapNodeSurvivesPassthroughCopies(t *testing.T) {
	t.Parallel()
	wrap := NewFieldValueWithResolvedOrdinal("X", 1, UnknownType)

	for name, copied := range map[string]Value{
		"pullUpThroughPassthrough":   pullUpThroughPassthrough(wrap, NamedCorrelationIdentifier("up")),
		"pushDownThroughPassthrough": pushDownThroughPassthrough(wrap),
	} {
		fv, ok := copied.(*FieldValue)
		if !ok {
			t.Fatalf("%s returned %T, want *FieldValue", name, copied)
		}
		if acc, single := singleAccessorOf(fv); !single || acc.Ordinal != 1 {
			t.Fatalf("%s dropped the baked accessor (Resolved=%v) — the pre-unification silent-drop hole is back", name, fv.Resolved)
		}
		if fv.Resolved.FrontierPinned {
			t.Fatalf("%s invented a frontier pin on an unpinned wrap node", name)
		}
		if !EqualsWithoutChildren(wrap, fv) {
			t.Fatalf("%s copy is not identity-equal to the wrap node", name)
		}
	}
}

// TestPinnedStaysLoudThroughPassthrough pins the required property: a
// FRONTIER-PINNED seed node copied through the passthrough — which STRIPS
// Child while sharing the accessor pointer — must STAY LOUD on a name-keyed
// context. A child-presence guard alone would silently demote this copy to a
// quiet name read (the copy is childless); the FrontierPinned bit correctly
// keeps the contract traveling with the accessor regardless of Child.
func TestPinnedStaysLoudThroughPassthrough(t *testing.T) {
	t.Parallel()
	qov, _ := unifiedTestQOV(t)
	seed, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	copied := pullUpThroughPassthrough(seed, NamedCorrelationIdentifier("up"))
	fv, ok := copied.(*FieldValue)
	if !ok {
		t.Fatalf("passthrough returned %T, want *FieldValue", copied)
	}
	if fv.Child != nil {
		t.Fatalf("fixture invalid: the passthrough copy must strip Child (got %T) — the test exists because it does", fv.Child)
	}
	if fv.Resolved == nil || !fv.Resolved.FrontierPinned {
		t.Fatalf("passthrough copy lost the FrontierPinned contract (Resolved=%+v)", fv.Resolved)
	}
	_, evalErr := fv.Evaluate(map[string]any{"ID": int64(7)})
	var bnce *BakedNameContextError
	var uce *UnboundEvalContextError
	if !errors.As(evalErr, &bnce) && !errors.As(evalErr, &uce) {
		t.Fatalf("childless PINNED copy on a name-keyed row = %v, want loud *BakedNameContextError or *UnboundEvalContextError — the frontier contract must survive the child-stripping copy", evalErr)
	}
}

// TestGuardDistinction pins the collapse of the old loud/quiet split: a
// nothing-matched eval against a non-positional (non-OrdinalRow) context is
// UNIFORMLY loud, for BOTH pinned and unpinned nodes — there is no silent
// unpinned path. The surviving pinned-vs-unpinned distinction lives only at
// a MATCHED name-keyed binding (pinned → *BakedNameContextError, unpinned →
// raw value), covered elsewhere. Here, over a bare map nothing matches, so
// both tails are loud (in practice both hit the unbound-context tail →
// *UnboundEvalContextError).
func TestGuardDistinction(t *testing.T) {
	t.Parallel()
	qov, _ := unifiedTestQOV(t)
	nameRow := map[string]any{"ID": int64(7), "X": int64(9)}

	seed, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	_, evalErr := seed.Evaluate(nameRow)
	var bnce *BakedNameContextError
	var uce *UnboundEvalContextError
	if !errors.As(evalErr, &bnce) && !errors.As(evalErr, &uce) {
		t.Fatalf("pinned seed node on a non-positional context = %v, want loud *BakedNameContextError or *UnboundEvalContextError", evalErr)
	}

	// The UNPINNED wrap node is LOUD too: it carries no silent off-frontier
	// path — a nothing-matched eval over a non-positional context is an
	// *UnboundEvalContextError, never a quiet NULL. (Its MATCHED positive
	// half — resolving positionally over an ordinal row — is covered by
	// TestFieldValue_OrdinalEval.)
	wrap := NewFieldValueWithResolvedOrdinal("X", 1, UnknownType)
	_, evalErr = wrap.Evaluate(nameRow)
	if !errors.As(evalErr, &bnce) && !errors.As(evalErr, &uce) {
		t.Fatalf("unpinned wrap node on a non-positional context = %v, want loud *BakedNameContextError or *UnboundEvalContextError (no silent unpinned path)", evalErr)
	}
}

// TestSeedExplainRendersOrdinal pins the identity gap this unification
// closed: before it, only the wrap mechanism rendered "#ordinal" in
// ExplainValue, so two seed-baked reads of DUPLICATE-named leg columns
// differing only by ordinal rendered identically — and
// RecordQueryProjectionPlan identity is ExplainValue-string-keyed. Now every
// baked node renders its ordinal; FrontierPinned does NOT render (an
// evaluation contract, not identity).
func TestSeedExplainRendersOrdinal(t *testing.T) {
	t.Parallel()
	qov, _ := unifiedTestQOV(t)
	seed0, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(0): %v", err)
	}
	seed1, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(1): %v", err)
	}
	r0, r1 := ExplainValue(seed0), ExplainValue(seed1)
	if !strings.HasSuffix(r0, "#0") || !strings.HasSuffix(r1, "#1") {
		t.Fatalf("seed renders = %q, %q — every baked node must render its ordinal (ExplainValue-keyed plan identity)", r0, r1)
	}

	// Same (field, ordinal), pin differing: identical render, identical
	// identity, identical hash — the bit is contract, not identity.
	pinned := &FieldValue{Field: "X", Typ: UnknownType, Resolved: NewFieldPathOfSingle("X", 3, true)}
	unpinned := &FieldValue{Field: "X", Typ: UnknownType, Resolved: NewFieldPathOfSingle("X", 3, false)}
	if ExplainValue(pinned) != ExplainValue(unpinned) {
		t.Fatalf("FrontierPinned leaked into ExplainValue: %q vs %q", ExplainValue(pinned), ExplainValue(unpinned))
	}
	if !EqualsWithoutChildren(pinned, unpinned) {
		t.Fatal("FrontierPinned leaked into EqualsWithoutChildren — it is an evaluation-contract marker, not a value distinction")
	}
	if SemanticHashCode(pinned) != SemanticHashCode(unpinned) {
		t.Fatal("FrontierPinned leaked into SemanticHashCode")
	}
}
