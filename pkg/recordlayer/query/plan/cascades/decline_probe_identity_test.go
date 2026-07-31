package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// machineLegCorr is a planner mint: UniqueCorrelationIdentifier produces
// LOWERCASE by construction, and the semantic scope upper-folds user aliases at
// registration, so the two namespaces are deliberately case-DISJOINT. Every leg
// the real-FDB corpus drives through the two probes below is already upper —
// which is precisely why the corpus cannot detect a fold, and why both of these
// pins are unit-level rather than SQL.
//
// The population is not hypothetical: the leg-identity census reports FLOWED
// witnesses over machine-minted lowercase legs, so this shape exists in the tree
// today; it simply does not reach these two sites in the queries the suite runs.
func machineLegCorr() values.CorrelationIdentifier {
	return values.NamedCorrelationIdentifier("q$7")
}

// TestExistentialLegCorrelations_CarriesAMachineMintedLegAsItself pins the
// verification set that decides whether an existential subtree is rebased.
//
// legReferencesAny against this set is the switch: TRUE means rebase and then
// verify fail-closed, FALSE means ship the subtree untouched. Re-minting a
// window leg from the upper fold of its own identity is a no-op for every upper
// leg — the whole corpus — and turns a lowercase machine leg into one the set
// cannot match. The reference then answers FALSE, skips the rebase AND the
// fail-closed check, and an unbound leg-correlated reference ships. The guard
// waves through the exact failure it exists to catch.
func TestExistentialLegCorrelations_CarriesAMachineMintedLegAsItself(t *testing.T) {
	t.Parallel()
	machine := machineLegCorr()
	windows := map[values.CorrelationIdentifier]ordinalLegWindow{
		machine: {Offset: 0, Alias: machine, Typ: &values.RecordType{Fields: []values.Field{
			{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
		}}},
	}
	set := existentialLegCorrelations(
		values.NamedCorrelationIdentifier("Q0"),
		values.NamedCorrelationIdentifier("Q1"),
		windows, nil)

	if _, ok := set[machine]; !ok {
		folded := values.NamedCorrelationIdentifier(strings.ToUpper(machine.Name()))
		_, foldedPresent := set[folded]
		t.Fatalf("the verification set does NOT contain the machine-minted leg %q "+
			"(its upper fold %q present=%v).\n"+
			"  This set is what legReferencesAny asks. A leg missing from it makes the\n"+
			"  probe answer FALSE for a subtree that DOES reference the leg, so the\n"+
			"  subtree skips both the ordinal rebase and the fail-closed verification\n"+
			"  and ships a correlation the FlatMap never binds.\n"+
			"  Every leg the corpus produces here is already upper, so a fold is a\n"+
			"  no-op on it and the whole suite stays green — this shape is the only\n"+
			"  detector.", machine.Name(), folded.Name(), foldedPresent)
	}
	// The upper fold must NOT be in the set: its presence would mean the mint is
	// back, merely alongside the identity, and a quoted "Q$7" leg would then be
	// matched by a reference to the planner-minted q$7 — the collision SameLeg
	// exists to refuse.
	if _, ok := set[values.NamedCorrelationIdentifier("Q$7")]; ok {
		t.Fatal("the verification set carries the UPPER FOLD of a machine leg as well " +
			"as the leg. That re-merges the two deliberately case-disjoint alias " +
			"namespaces: a quoted \"Q$7\" and a planner-minted q$7 become one entry.")
	}
	// Control: the two join quantifiers are still there, so a set that simply
	// dropped everything would not pass.
	if _, ok := set[values.NamedCorrelationIdentifier("Q0")]; !ok {
		t.Fatal("control: the join quantifiers must be in the verification set")
	}
}

// TestRebaseOuterLegRefsOrdinal_DeclineProbeMatchesAMachineMintedLeg pins the
// SECOND decline probe, in the default arm of the predicate rebase.
//
// That arm handles predicate kinds the walk does not rewrite. Passing one
// through is safe ONLY if it carries no leg reference, so the arm probes the
// windows and declines when it finds one. The probe used to MINT a
// CorrelationIdentifier out of the map key's text while the window beside it
// already stated the identifier being manufactured — and the correlation set it
// asks is keyed by identity, so a lowercase machine leg produced a probe that
// could never match and the decline never fired.
//
// The shape here is an ExistentialValuePredicate, which reaches the default arm
// and carries its correlation through GetCorrelatedTo.
func TestRebaseOuterLegRefsOrdinal_DeclineProbeMatchesAMachineMintedLeg(t *testing.T) {
	t.Parallel()
	machine := machineLegCorr()
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
	}}
	windows := map[values.CorrelationIdentifier]ordinalLegWindow{
		machine: {Offset: 0, Alias: machine, Typ: legType},
	}
	mergedQOV := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("MERGED"), legType)

	// An unhandled predicate kind that DOES reference the leg.
	pred := predicates.MustNewExistentialValuePredicate(
		values.NewQuantifiedObjectValueOfType(machine, legType),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	if _, refs := predicates.GetCorrelatedToOfPredicate(pred)[machine]; !refs {
		t.Fatal("fixture: the probe predicate must report the leg correlation, or the " +
			"decline has nothing to find and this test proves nothing")
	}

	_, ok := rebaseOuterLegRefsOrdinal(pred, windows, mergedQOV)
	if ok {
		t.Fatalf("an unhandled predicate carrying a reference to the machine-minted leg "+
			"%q was PASSED THROUGH; it must DECLINE.\n"+
			"  This arm does not rewrite the predicate, so passing it on ships a\n"+
			"  leg correlation into a context that binds only the merged quantifier.\n"+
			"  A probe that upper-folds the window key before asking mints %q, which\n"+
			"  the identity-keyed correlation set never contains — so the decline\n"+
			"  cannot fire, silently, for exactly the namespace it was written to\n"+
			"  protect.", machine.Name(), strings.ToUpper(machine.Name()))
	}

	// CONTROL: the same predicate kind over a NON-leg correlation must still pass
	// through. Without this the assertion above is satisfied by an arm that
	// declines everything, which would turn the whole path off.
	free := predicates.MustNewExistentialValuePredicate(
		values.NewQuantifiedObjectValueOfType(
			values.NamedCorrelationIdentifier("SUBQ"), legType),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	if _, freeOK := rebaseOuterLegRefsOrdinal(free, windows, mergedQOV); !freeOK {
		t.Fatal("control: an unhandled predicate carrying NO leg reference must pass " +
			"through — a blanket decline here turns off every leg-independent EXISTS")
	}
}
