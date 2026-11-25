package factory

import (
	"strings"
	"testing"
)

// TestEveryInapplicabilityEntryCarriesAPin is the gate on the gate.
//
// A structural-inapplicability entry is an EXEMPTION: it lets a shape family
// be blessed on fewer oracles than everything else. The only thing separating
// that from an excuse is the requirement that a committed test establish the
// inapplicability — so the ledger is worthless if an entry can be added
// without one, and this is the check that makes the requirement real rather
// than advisory.
//
// The upgrade field is required for the same reason: an exemption with no
// stated path out of it becomes permanent by default, and nobody revisits a
// weakening that never announced it was temporary.
func TestEveryInapplicabilityEntryCarriesAPin(t *testing.T) {
	t.Parallel()
	for _, e := range secondPlanInapplicable {
		if e.Family == "" {
			t.Error("an inapplicability entry has no family name; the manifest could not report it")
		}
		if e.Pin == "" {
			t.Errorf("family %q has no pin. An exemption nobody committed evidence for is indistinguishable "+
				"from one somebody invented, and this is where a wrong answer silently weakens every file it "+
				"touches", e.Family)
		}
		if e.Upgrade == "" {
			t.Errorf("family %q names no upgrade path; a weakening with no stated exit becomes permanent by "+
				"default", e.Family)
		}
		if e.Applies == nil {
			t.Errorf("family %q has no predicate and can never match", e.Family)
		}
	}
}

// TestUnpinnedFamiliesAreRefused pins that the requirement is ENFORCED at the
// decision point, not merely asserted by the test above.
//
// The two are different claims: the test above says the committed ledger is
// well-formed, this one says a malformed entry could not weaken a blessing
// even if it got in. Without it, the pin requirement is a convention that the
// next edit can quietly break.
// It probes the decision procedure with a LOCAL ledger, never by swapping the
// package variable: every test here is parallel, so a global swap-and-restore
// races the other readers (the race detector caught exactly that), and while
// the swap is live the spec-matching test can observe a ledger that matches
// everything. The production path goes through the same procedure
// (secondPlanInapplicableFor is a one-line delegation), so nothing is lost by
// probing the parameterised form.
func TestUnpinnedFamiliesAreRefused(t *testing.T) {
	t.Parallel()
	cand := Candidates(1)[0]

	unpinned := []structuralInapplicability{{
		Family:  "everything",
		Pin:     "", // the defect under test
		Upgrade: "someday",
		Applies: func(Candidate) bool { return true },
	}}
	if got := inapplicableForIn(unpinned, cand); got != nil {
		t.Fatalf("an entry with no pin was honoured (family %q); the exemption gate is decorative", got.Family)
	}

	predicateless := []structuralInapplicability{{
		Family:  "everything",
		Pin:     "TestSomething",
		Upgrade: "someday",
		Applies: nil, // cannot match, must not panic
	}}
	if got := inapplicableForIn(predicateless, cand); got != nil {
		t.Fatalf("an entry with no predicate matched (family %q)", got.Family)
	}

	// A local positive control keeps the admission arm covered even with
	// the production exemption ledger empty.
	pinned := []structuralInapplicability{{Family: "test", Pin: "TestUnpinnedFamiliesAreRefused", Upgrade: "test", Applies: func(Candidate) bool { return true }}}
	if got := inapplicableForIn(pinned, cand); got != &pinned[0] {
		t.Fatal("pinned local exemption was not admitted")
	}
	if got := secondPlanInapplicableFor(cand); got != nil {
		t.Fatal("production unexpectedly exempts the candidate")
	}
}

// EXISTS now responds to the second-plan perturbation. An unchanged plan
// on one query is not permission to restore a family-wide TLP-only exemption.
func TestCorrelatedExistsRequiresSecondPlanOracle(t *testing.T) {
	t.Parallel()
	withExists, withoutExists := 0, 0
	for seed := uint64(1); seed <= 80; seed++ {
		for _, c := range Candidates(seed) {
			if c.Query.Exists != nil {
				withExists++
			} else {
				withoutExists++
			}
			if got := secondPlanInapplicableFor(c); got != nil {
				t.Fatalf("candidate %s has obsolete exemption %s", c.Name(), got.Family)
			}
		}
	}
	if withExists == 0 || withoutExists == 0 {
		t.Fatalf("empty population: exists=%d other=%d", withExists, withoutExists)
	}
}

// TestInapplicabilityLedgerNamesPinsAndUpgrades pins that a run REPORTS which
// exemptions were available to it. An exemption that only exists in code is
// one a batch reader cannot see.
func TestInapplicabilityLedgerNamesPinsAndUpgrades(t *testing.T) {
	t.Parallel()
	ledger := InapplicabilityLedger()
	if len(ledger) != len(secondPlanInapplicable) {
		t.Fatalf("ledger reports %d entries, want %d", len(ledger), len(secondPlanInapplicable))
	}
	for i, line := range ledger {
		e := secondPlanInapplicable[i]
		for _, must := range []string{e.Family, e.Pin, e.Upgrade} {
			if must != "" && !strings.Contains(line, must) {
				t.Errorf("ledger line %q omits %q", line, must)
			}
		}
	}
}
