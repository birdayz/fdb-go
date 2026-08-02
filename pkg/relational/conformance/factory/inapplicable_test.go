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
	if len(secondPlanInapplicable) == 0 {
		t.Skip("the ledger is empty; nothing to gate (this is the healthy state)")
	}
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

	// The gate the pipeline calls must be the SAME gate, or the two checks
	// above prove nothing about the execute path — they would pass just as
	// happily if secondPlanInapplicableFor had grown its own copy of the rules.
	//
	// Comparing on a candidate the ledger does not claim would agree vacuously,
	// both sides nil, so this insists on one it does. Seed 1's first candidate
	// is not such a candidate, which is exactly how a vacuous version of this
	// check reads as passing.
	var matching Candidate
	var found bool
	for seed := uint64(1); seed <= 80 && !found; seed++ {
		for _, c := range Candidates(seed) {
			if inapplicableForIn(secondPlanInapplicable, c) != nil {
				matching, found = c, true
				break
			}
		}
	}
	if !found {
		t.Fatal("no candidate in 80 seeds is claimed by the committed ledger; this check would have compared " +
			"two nils and proved nothing about the execute path")
	}
	if got, want := secondPlanInapplicableFor(matching), inapplicableForIn(secondPlanInapplicable, matching); got != want {
		t.Fatalf("secondPlanInapplicableFor(%s) = %v, but the gate under test says %v; the execute path does "+
			"not consult the ledger through the gate these checks exercise", matching.Name(), got, want)
	}
}

// TestInapplicabilityMatchesOnTheSpecNotTheRun pins that membership is decided
// by the candidate's SHAPE.
//
// This is the distinction the whole design rests on. "The oracle did not
// apply" is an observation about one execution — the plans happened to match,
// the data happened not to reach an index — and treating it as licence to
// bless on fewer oracles turns every accident into an exemption. "The oracle
// cannot apply" is a claim about the shape, and it is the only one that may
// weaken a blessing.
func TestInapplicabilityMatchesOnTheSpecNotTheRun(t *testing.T) {
	t.Parallel()
	var withExists, withoutExists int
	for seed := uint64(1); seed <= 80; seed++ {
		for _, c := range Candidates(seed) {
			got := secondPlanInapplicableFor(c)
			if c.Query.Exists != nil {
				withExists++
				if got == nil {
					t.Fatalf("seed %d %s carries an EXISTS but matched no family", seed, c.Name())
				}
				if got.Family != "correlated-exists" {
					t.Errorf("seed %d %s matched family %q, want correlated-exists", seed, c.Name(), got.Family)
				}
				continue
			}
			withoutExists++
			if got != nil {
				t.Fatalf("seed %d %s carries no EXISTS but matched family %q — the exemption is broader than "+
					"the structure that justifies it", seed, c.Name(), got.Family)
			}
		}
	}
	if withExists == 0 {
		t.Fatal("no EXISTS candidate was generated in 80 seeds; this gate is vacuous and the corpus cannot " +
			"be gaining EXISTS coverage either")
	}
	if withoutExists == 0 {
		t.Fatal("every candidate carried an EXISTS; the negative half of this check proved nothing")
	}
	t.Logf("matched %d EXISTS candidates, correctly declined %d others", withExists, withoutExists)
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
