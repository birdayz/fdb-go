package values

import "testing"

// TestSourceRelativeBaked pins what this predicate ANSWERS, and — since the two
// were conflated and that conflation shipped a silent bug — separately pins what
// its answer does NOT mean.
//
// It reports a SINGLE-accessor UNPINNED path: the resolver's construction-time
// bind against the reference's own source row. Such a node must be treated like
// a lazy reference by the translator's rebase/collection/safety-net walks —
// rebind/count/flag it.
//
// WHAT THIS TEST USED TO ASSERT, AND WHY THAT WAS WRONG: that machinery-owned
// nodes are "FrontierPinned box ofOrdinals, multi-accessor paths", i.e. that a
// multi-accessor path is final. Machinery-ownership is the FRONTIER PIN ALONE;
// arity is orthogonal to it. An UNPINNED multi-accessor path — a user-written
// nested descent — is still leg-relative and still has to be rebound, yet this
// predicate answers false for it exactly as it does for genuinely-final
// machinery output. A walk selecting candidates with SourceRelativeBaked
// therefore SKIPS it silently, which is how an element MEMBER reference
// mis-resolved over a composed row and made EXISTS drop every row with no error.
//
// So the multi-accessor case below keeps its assertion and loses its reason, and
// the pairing beside it is the point: the SAME node that is not
// source-relative-baked IS root-leg-relative-unpinned. Asserting only the first
// is what let "false here" be read as "final".
func TestSourceRelativeBaked(t *testing.T) {
	t.Parallel()

	if (&FieldValue{Field: "A"}).SourceRelativeBaked() {
		t.Fatal("lazy node must NOT be source-relative baked (it is not baked at all)")
	}
	if !NewFieldValueWithResolvedOrdinal("A", 1, UnknownType).SourceRelativeBaked() {
		t.Fatal("unpinned single-accessor bake IS source-relative")
	}
	pinned := &FieldValue{Field: "A", Resolved: NewFieldPathOfSingle("A", 1, true)}
	if pinned.SourceRelativeBaked() {
		t.Fatal("FrontierPinned node is machinery-owned, never source-relative")
	}
	// A PINNED node is machinery-owned AND final — both predicates agree, and
	// that agreement is what makes the unpinned case below discriminating.
	if pinned.RootIsLegRelativeUnpinned() {
		t.Fatal("FrontierPinned node must NOT be leg-relative-unpinned — it is machinery-owned and final")
	}

	// An UNPINNED multi-accessor path: a user-written nested descent.
	multi := &FieldValue{Field: "B", Resolved: &FieldPath{Accessors: []ResolvedAccessor{
		{Field: "A", Ordinal: 0}, {Field: "B", Ordinal: 1},
	}}}
	if multi.SourceRelativeBaked() {
		t.Fatal("multi-accessor path is not SINGLE-accessor, so this predicate must answer false")
	}
	// THE CORRECTION, asserted rather than described. The node above is NOT
	// final: its root is still leg-relative and a rebase/collection walk must
	// still rebind and count it. Without this assertion the `false` on the line
	// above is indistinguishable from the `false` a genuinely machinery-owned
	// node gives, which is precisely the reading that shipped a silent 0-row
	// EXISTS. If this ever fails, every walk keyed on RootIsLegRelativeUnpinned
	// has stopped seeing nested descents.
	if !multi.RootIsLegRelativeUnpinned() {
		t.Fatal("an UNPINNED multi-accessor path IS still leg-relative — it is not machinery-owned, " +
			"and treating this predicate's false as 'final' is what silently dropped rows")
	}
}
