package values

import (
	"errors"
	"strings"
	"testing"
)

// RFC-173 baked-ordinal UNIFICATION pins. Two mechanisms briefly coexisted —
// the gated-join seed's Resolved *ResolvedAccessor and the recursive-CTE
// wrap's ResolvedOrdinal/HasResolvedOrdinal twin fields (merge 0fbf20081) —
// unified onto ResolvedAccessor with a FrontierPinned contract bit (the
// unification review ruling: the loudness contract must be a property of the
// VALUE, invariant under transformation, because pullup/pushdown passthrough
// copies strip Child while sharing the accessor pointer). These tests pin the
// exact seams the fold closed or could have silently changed.

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

// TestRFC173Unified_WrapNodeSurvivesPassthroughCopies pins the silent-drop
// hole the fold closed: pre-unification, the passthrough copies rebuilt
// &FieldValue{Field, Typ, Resolved} and DROPPED the wrap mechanism's twin
// scalar fields — a wrap node pulled or pushed through a QOV/ObjectValue
// passthrough silently lost its ordinal and degraded to a first-match name
// read (§5 conflation hazard). Post-fold the wrap constructor produces a
// Resolved accessor, which the copies preserve by pointer.
func TestRFC173Unified_WrapNodeSurvivesPassthroughCopies(t *testing.T) {
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

// TestRFC173Unified_PinnedStaysLoudThroughPassthrough is the unification
// review's demanded pin: a FRONTIER-PINNED seed node copied through the
// passthrough — which STRIPS Child while sharing the accessor pointer — must
// STAY LOUD on a name-keyed context. Under a child-presence guard this copy
// silently demotes to a quiet name read (the copy is childless); under the
// FrontierPinned bit the contract travels with the accessor. Red under
// child-presence, green under the bit.
func TestRFC173Unified_PinnedStaysLoudThroughPassthrough(t *testing.T) {
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

// TestRFC173Unified_GuardDistinction pins the §F collapse of the old
// loud/quiet split. Pre-§F this test proved a distinction over the SAME
// non-positional context: the pinned seed was loud while the unpinned wrap was
// a quiet off-frontier NULL. The §F ruling RETIRES that split — a nothing-matched
// eval against a non-positional (non-OrdinalRow) context is UNIFORMLY loud, for
// BOTH pinned and unpinned nodes: there is no silent unpinned path left. The
// surviving pinned-vs-unpinned distinction now lives only at a MATCHED
// name-keyed binding (pinned → *BakedNameContextError, unpinned → raw value),
// covered elsewhere. Here, over a bare map nothing matches, so both tails are
// loud (in practice both hit the unbound-context tail → *UnboundEvalContextError).
func TestRFC173Unified_GuardDistinction(t *testing.T) {
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

	// The UNPINNED wrap node is now LOUD too: post-§F it carries no silent
	// off-frontier path — a nothing-matched eval over a non-positional context is
	// an *UnboundEvalContextError, never a quiet NULL. (Its MATCHED positive
	// half — resolving positionally over an ordinal row — is covered by
	// TestFieldValue_OrdinalEval_RFC173Slice1.)
	wrap := NewFieldValueWithResolvedOrdinal("X", 1, UnknownType)
	_, evalErr = wrap.Evaluate(nameRow)
	if !errors.As(evalErr, &bnce) && !errors.As(evalErr, &uce) {
		t.Fatalf("unpinned wrap node on a non-positional context = %v, want loud *BakedNameContextError or *UnboundEvalContextError (§F: no silent unpinned path)", evalErr)
	}
}

// TestRFC173Unified_SeedExplainRendersOrdinal pins the latent identity gap
// the fold closed: pre-unification only the wrap mechanism rendered
// "#ordinal" in ExplainValue, so two seed-baked reads of DUPLICATE-named leg
// columns differing only by ordinal rendered identically — and
// RecordQueryProjectionPlan identity is ExplainValue-string-keyed. Post-fold
// every baked node renders its ordinal; FrontierPinned does NOT render
// (evaluation contract, not identity).
func TestRFC173Unified_SeedExplainRendersOrdinal(t *testing.T) {
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
