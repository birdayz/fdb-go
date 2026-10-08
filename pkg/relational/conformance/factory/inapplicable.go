package factory

import "fmt"

// structuralInapplicability names a shape family where an oracle CANNOT apply,
// together with the committed pin that establishes why.
//
// "Cannot apply" and "did not apply" are different claims, and only the first
// one may weaken a blessing. A per-case skip is an observation about one run —
// the plans happened to match, the query happened not to reach an index — and
// treating it as licence to bless on fewer oracles would turn every accident
// into an exemption. A structural claim is about the SHAPE: it says the oracle
// can never apply here, for a reason someone measured and wrote down.
//
// That is why Pin is a required field rather than documentation. The gate below
// refuses to bless any family whose pin is empty, so the only way to add an
// exemption is to first commit a test that establishes it — and that test then
// fails the day the structure changes, which is exactly when the exemption
// should be withdrawn.
type structuralInapplicability struct {
	// Family is the reason class, reported in the manifest.
	Family string
	// Pin names the committed test establishing the inapplicability.
	Pin string
	// Upgrade names the oracle that would close the hole, so the exemption
	// carries its own expiry condition rather than becoming permanent by
	// default.
	Upgrade string
	// Applies reports whether a candidate belongs to the family. It is a
	// question about the SPEC, never about what a particular run observed.
	Applies func(Candidate) bool
}

// secondPlanInapplicable is the whole ledger of families that may bless on TLP
// alone. It is deliberately short and expensive to extend.
// Correlated EXISTS is no longer exempt: outer predicates now admit index
// matching, so the second-plan oracle can compare different access paths.
var secondPlanInapplicable = []structuralInapplicability{}

// secondPlanInapplicableFor returns the family covering a candidate, or nil.
//
// An entry with no pin is REFUSED rather than honoured: an exemption whose
// justification nobody committed is indistinguishable from an exemption
// somebody invented, and this is the one place in the pipeline where a wrong
// answer silently weakens every file it touches.
func secondPlanInapplicableFor(c Candidate) *structuralInapplicability {
	return inapplicableForIn(secondPlanInapplicable, c)
}

// inapplicableForIn is the decision procedure over an explicit ledger. The
// split exists so a test can probe the refusal rules against a ledger of its
// own instead of swapping the package variable — a parallel test mutating the
// global raced every other reader in the package, and while the swap was live
// the spec-matching test could observe a ledger that matches everything.
func inapplicableForIn(ledger []structuralInapplicability, c Candidate) *structuralInapplicability {
	for i := range ledger {
		e := &ledger[i]
		if e.Pin == "" || e.Applies == nil {
			continue
		}
		if e.Applies(c) {
			return e
		}
	}
	return nil
}

// InapplicabilityLedger renders the ledger for a run's manifest, so a batch
// says out loud which exemptions were available to it.
func InapplicabilityLedger() []string {
	out := make([]string, 0, len(secondPlanInapplicable))
	for _, e := range secondPlanInapplicable {
		out = append(out, fmt.Sprintf("%s (pin: %s; upgrade: %s)", e.Family, e.Pin, e.Upgrade))
	}
	return out
}
