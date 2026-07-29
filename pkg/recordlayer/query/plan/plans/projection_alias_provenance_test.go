package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestProjectionPlan_AliasProvenanceExcludedFromIdentity pins the memo contract
// for the per-slot alias provenance: it records WHO named an output slot, not
// what the slot computes, so two projections differing only in it are the SAME
// memo member and must hash the same.
//
// This is the contract values.FieldPath.FrontierPinned already carries (values.go
// documents it: "an evaluation-contract marker, not a value distinction. Two
// references to the same column that arrived through different producers must
// still intern as one memo member"). Letting the marker into structuralKey would
// split a group in two and let extraction pick either half.
//
// The paired negative is deliberate: the alias STRING is still identity, so this
// test cannot pass by the whole output schema having dropped out of the key.
func TestProjectionPlan_AliasProvenanceExcludedFromIdentity(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan(nil, nil, false)
	v := values.NewFieldValueWithResolvedOrdinal("A.K", 0, values.NullableLong)

	minted := NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan).
		WithAliasProvenance([]bool{true})
	userWritten := NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan)

	if !minted.EqualsPlanWithoutChildren(userWritten) {
		t.Error("a projection differing only in alias provenance must be the same memo member")
	}
	if minted.HashCodeWithoutChildren() != userWritten.HashCodeWithoutChildren() {
		t.Error("equal plans must hash equal (memo invariant); provenance must not perturb the hash")
	}
	// Same in the other direction — an explicitly not-minted vector must not
	// differ from an absent one.
	notMinted := NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan).
		WithAliasProvenance([]bool{false})
	if !notMinted.EqualsPlanWithoutChildren(userWritten) ||
		notMinted.HashCodeWithoutChildren() != userWritten.HashCodeWithoutChildren() {
		t.Error("an all-false provenance vector must be identical to an absent one")
	}

	// The output NAME is still identity — otherwise the assertions above would
	// hold vacuously.
	renamed := NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"K"}, scan)
	if minted.EqualsPlanWithoutChildren(renamed) {
		t.Error("the alias STRING is still a memo discriminator")
	}
}

// TestProjectionPlan_AliasProvenanceSurvivesChildRewrite pins the carry-across
// contract on the copy paths the memo drives. WithQuantifiers / WithChildren
// hand back "the same projection over a new child"; a provenance dropped there
// silently relabels every machinery datum key as a user alias, and the only
// visible symptom is a leaked qualifier in ResultSet metadata far downstream.
func TestProjectionPlan_AliasProvenanceSurvivesChildRewrite(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan(nil, nil, false)
	other := NewRecordQueryScanPlan([]string{"U"}, nil, false)
	v := values.NewFieldValueWithResolvedOrdinal("A.K", 0, values.NullableLong)

	p := NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan).
		WithAliasProvenance([]bool{true})

	rewired := p.WithQuantifiers([]expressions.Quantifier{QuantifierOverPlan(other)})
	rp, ok := rewired.(*RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("WithQuantifiers returned %T, want *RecordQueryProjectionPlan", rewired)
	}
	if got := rp.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("WithQuantifiers dropped the alias provenance: got %v, want [true]", got)
	}

	withChild, err := p.WithChildren([]expressions.Quantifier{QuantifierOverPlan(other)})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	cp, ok := withChild.(*RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("WithChildren returned %T, want *RecordQueryProjectionPlan", withChild)
	}
	if got := cp.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("WithChildren dropped the alias provenance: got %v, want [true]", got)
	}

	// The original is untouched — these are copies, not mutations.
	if got := p.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("the source plan's provenance was mutated: got %v, want [true]", got)
	}
}
