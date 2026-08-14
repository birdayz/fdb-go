package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAliasOnlyDifferenceSplitsTheMemoDeliberately pins that an ALIAS-ONLY
// difference between two otherwise identical projections keeps them APART in
// the memo — and that this is the intended contract, not an accident.
//
// `SELECT k AS a` and `SELECT k AS b` build structurally identical projections
// over one value. They compare unequal and hash unequal because
// ProjectionOutputIdentityKey is folded into EqualsWithoutChildren and
// HashCodeWithoutChildren here, and into structuralKey on the physical side.
// The design note at the logical projection states it outright — output names
// belong in memo identity — and forecloses the other direction, because folding
// the minted alias in the other way trades a wrong label for a duplicated memo
// group.
//
// This test exists because an RFC revision proposed the OPPOSITE as a hazard to
// be prevented: that a name reaching EqualsWithoutChildren would newly split
// alias-variant plans and cost duplicated search. That premise was false — the
// split is already there and is deliberate — and implementing it literally would
// have removed the alias from projection memo identity, a query-engine
// behaviour change reversing two prior RFCs, arriving disguised as a naming
// refactor.
//
// So the direction of this guard is the point. If a future change drops the
// name out of identity, the first assertion below fails and says so, instead of
// the change shipping silently as a "cleanup".
//
// The control is what keeps the guard honest: two projections with NO aliases
// must compare EQUAL and hash equal. Without it, an implementation that made
// every projection unequal — the trivial way to pass the first half — would
// look correct here while destroying interning across the board.
func TestAliasOnlyDifferenceSplitsTheMemoDeliberately(t *testing.T) {
	t.Parallel()

	k := testField("K", values.NotNullLong)
	a := mustExpression(NewLogicalProjectionExpressionWithAliases([]values.Value{k}, []string{"A"}, Quantifier{}))
	b := mustExpression(NewLogicalProjectionExpressionWithAliases([]values.Value{k}, []string{"B"}, Quantifier{}))

	if a.EqualsWithoutChildren(b, &AliasMap{}) {
		t.Fatal("two projections differing ONLY in output alias compared EQUAL.\n" +
			"  The output name has left memo identity. `SELECT k AS a` and " +
			"`SELECT k AS b` would now intern to one group and one of the two " +
			"labels would be wrong on the way out.\n" +
			"  This is the contract the logical projection documents — output names " +
			"belong in memo identity — so if it was removed deliberately it needs an " +
			"RFC and a Graefe ACK, not a passing test.")
	}
	if ha, hb := a.HashCodeWithoutChildren(), b.HashCodeWithoutChildren(); ha == hb {
		t.Fatalf("alias-only variants hashed EQUAL (%d): the alias is out of the "+
			"hash while still in Equals, so they land in one bucket and are then "+
			"separated by a full comparison — a silent cost, and a sign the two "+
			"halves of identity have drifted apart", ha)
	}

	// THE CONTROL. Without aliases the two are the same projection and MUST
	// intern. An implementation that passed the assertions above by making every
	// projection distinct would fail here, which is the only thing separating
	// "the alias is in identity" from "identity is broken".
	none1 := mustExpression(NewLogicalProjectionExpression([]values.Value{k}, Quantifier{}))
	none2 := mustExpression(NewLogicalProjectionExpression([]values.Value{k}, Quantifier{}))
	if !none1.EqualsWithoutChildren(none2, &AliasMap{}) {
		t.Fatal("two identical UNALIASED projections compared unequal — interning is " +
			"broken, and the alias assertions above are passing for the wrong reason")
	}
	if h1, h2 := none1.HashCodeWithoutChildren(), none2.HashCodeWithoutChildren(); h1 != h2 {
		t.Fatalf("identical unaliased projections hashed %d vs %d — equal values must "+
			"hash equal or the memo cannot find them", h1, h2)
	}
}

func TestFrozenOutputNameDifferenceSplitsTheMemo(t *testing.T) {
	t.Parallel()

	k := testField("K", values.NotNullLong)
	bare := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{k}, nil, nil, []string{"ID"}, Quantifier{}))
	qualified := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{k}, nil, nil, []string{"S.ID"}, Quantifier{}))

	if bare.EqualsWithoutChildren(qualified, &AliasMap{}) {
		t.Fatal("same Value/aliases with different frozen output names compared equal")
	}
	if bare.HashCodeWithoutChildren() == qualified.HashCodeWithoutChildren() {
		t.Fatal("different frozen output names hashed equal")
	}

	same := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{k}, nil, nil, []string{"ID"}, Quantifier{}))
	if !bare.EqualsWithoutChildren(same, &AliasMap{}) ||
		bare.HashCodeWithoutChildren() != same.HashCodeWithoutChildren() {
		t.Fatal("identical frozen output schemas must remain equal and hash-equal")
	}
}

func TestAuthoredSourceIdentityDoesNotLeakIntoProjectionSchema(t *testing.T) {
	t.Parallel()

	k := testField("K", values.NotNullLong)
	bare := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{k}, nil, nil, []string{"ID"}, Quantifier{}))
	qualified := mustExpression(bare.WithAuthoredOutputIdentity([]string{"S.ID"}))

	if got := bare.GetOutputNames(); len(got) != 1 || got[0] != "ID" {
		t.Fatalf("bare output schema = %v, want [ID]", got)
	}
	if got := qualified.GetOutputNames(); len(got) != 1 || got[0] != "ID" {
		t.Fatalf("authored source identity leaked into output schema: got %v, want [ID]", got)
	}
	if bare.EqualsWithoutChildren(qualified, &AliasMap{}) {
		t.Fatal("authored S.ID identity coalesced with the bare projection")
	}
	if bare.HashCodeWithoutChildren() == qualified.HashCodeWithoutChildren() {
		t.Fatal("authored S.ID identity did not participate in the projection hash")
	}

	rebuilt := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{k}, nil, nil, []string{"ID"}, Quantifier{})).
		WithInheritedOutputIdentity(qualified)
	if !qualified.EqualsWithoutChildren(rebuilt, &AliasMap{}) ||
		qualified.HashCodeWithoutChildren() != rebuilt.HashCodeWithoutChildren() {
		t.Fatal("row-program-preserving rebuild dropped the authored output identity")
	}
}
