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
var secondPlanInapplicable = []structuralInapplicability{
	{
		Family: "correlated-exists",
		Pin:    "TestFDB_SecondPlanIsBlindToCorrelatedExists",
		// A perturbation the EXISTS plan actually responds to. Disabling
		// index matching cannot help: the outer leg of a correlated EXISTS is
		// a filtered scan in the baseline too, so both plans come out
		// byte-identical. A decorrelation-perturbing oracle — forcing the
		// semi-join to be answered by a different join strategy, or disabling
		// the EXISTS-to-FlatMap lowering — would give the comparison two
		// genuinely different plans and retire this entry.
		Upgrade: "a decorrelation-perturbing oracle (RFC-201 §5.5)",
		Applies: func(c Candidate) bool { return c.Query.Exists != nil },
	},
}

// secondPlanInapplicableFor returns the family covering a candidate, or nil.
//
// An entry with no pin is REFUSED rather than honoured: an exemption whose
// justification nobody committed is indistinguishable from an exemption
// somebody invented, and this is the one place in the pipeline where a wrong
// answer silently weakens every file it touches.
func secondPlanInapplicableFor(c Candidate) *structuralInapplicability {
	for i := range secondPlanInapplicable {
		e := &secondPlanInapplicable[i]
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
