package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestLogicalProjection_AliasProvenanceExcludedFromIdentity is the logical-layer
// twin of the plan-level pin: the per-slot alias provenance records WHO named an
// output, not what it computes, so it stays out of EqualsWithoutChildren and
// HashCodeWithoutChildren. Same contract values.FieldPath.FrontierPinned carries.
//
// The paired negative keeps it honest: the alias STRING is still identity, so a
// pass here cannot come from the output schema having left the key entirely.
func TestLogicalProjection_AliasProvenanceExcludedFromIdentity(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	v := values.NewFieldValueWithResolvedOrdinal("A.K", 0, values.NullableLong)

	minted := NewLogicalProjectionExpressionWithAliasProvenance(
		[]values.Value{v}, []string{"A.K"}, []bool{true}, inner)
	userWritten := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{v}, []string{"A.K"}, inner)

	if !minted.EqualsWithoutChildren(userWritten, EmptyAliasMap()) {
		t.Error("a projection differing only in alias provenance must be the same memo member")
	}
	if minted.HashCodeWithoutChildren() != userWritten.HashCodeWithoutChildren() {
		t.Error("equal expressions must hash equal (memo invariant)")
	}

	renamed := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{v}, []string{"K"}, inner)
	if minted.EqualsWithoutChildren(renamed, EmptyAliasMap()) {
		t.Error("the alias STRING is still a memo discriminator")
	}
}

// TestLogicalProjection_AliasProvenanceSurvivesWithQuantifiers pins the copy path
// the memo drives on every child rewrite. This one is worth its own test because
// WithQuantifiers used to build a field-by-field struct literal: it listed the
// three fields that existed when it was written, so any field added afterwards
// was dropped on every rewrite, silently and everywhere.
func TestLogicalProjection_AliasProvenanceSurvivesWithQuantifiers(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	other := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	v := values.NewFieldValueWithResolvedOrdinal("A.K", 0, values.NullableLong)

	p := NewLogicalProjectionExpressionWithAliasProvenance(
		[]values.Value{v}, []string{"A.K"}, []bool{true}, inner)

	rewired, ok := p.WithQuantifiers([]Quantifier{other}).(*LogicalProjectionExpression)
	if !ok {
		t.Fatal("WithQuantifiers must return a *LogicalProjectionExpression")
	}
	if got := rewired.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("WithQuantifiers dropped the alias provenance: got %v, want [true]", got)
	}
	// The other name-carrying field must survive the same rewrite, so a
	// regression cannot hide by dropping both together.
	if got := rewired.GetAliases(); len(got) != 1 || got[0] != "A.K" {
		t.Errorf("WithQuantifiers dropped the aliases: got %v, want [A.K]", got)
	}
}
