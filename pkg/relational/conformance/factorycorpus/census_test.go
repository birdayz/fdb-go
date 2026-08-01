package factorycorpus_test

import (
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// TestCorpusLoads walks every committed file through the loader CI uses.
//
// It is the cheapest and most important gate in the package: a corpus file
// that does not load takes the whole suite red for a reason that has nothing
// to do with the engine, and it costs no container to notice.
func TestCorpusLoads(t *testing.T) {
	t.Parallel()
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	tests := 0
	for _, f := range files {
		if len(f.Doc.Setup) == 0 {
			t.Errorf("%s: %s has no setup statements; the expectations would be frozen over an empty table", f.Path, f.Header.Name)
		}
		for i, tc := range f.Doc.Tests {
			if tc.Query == "" {
				t.Errorf("%s: %s tests[%d] has no query", f.Path, f.Header.Name, i)
			}
			tests++
		}
	}
	t.Logf("committed corpus: %d scenarios, %d tests", len(files), tests)
}

// TestCensusRatchet is RFC-201 §8's growth gate: the committed corpus may grow
// and may never shrink, on any dimension.
//
// Componentwise, not aggregate. Totals rising while a feature vector empties
// out is exactly the failure a single count cannot see, and it is the
// realistic one — a regenerated batch that drops a query family still grows
// the total. The blessing dimension is ratcheted for the same reason: a rerun
// without a reachable Java server would otherwise silently re-bless
// cross-engine files as metamorphic, keeping every count intact while deleting
// the second engine from the corpus's authority.
func TestCensusRatchet(t *testing.T) {
	t.Parallel()
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	current := factorycorpus.ComputeCensus(files)
	baseline, err := factorycorpus.LoadCensus(factorycorpus.CensusBaselinePath)
	if err != nil {
		t.Fatalf("read census baseline: %v", err)
	}
	if shrinks := factorycorpus.CheckRatchet(baseline, current); len(shrinks) > 0 {
		t.Errorf("the committed corpus SHRANK — a factory batch adds coverage, it never removes it. " +
			"If a scenario was deliberately retired, the baseline must be lowered in the same commit with the reason:")
		for _, s := range shrinks {
			t.Errorf("  %s", s)
		}
	}
	t.Logf("census: %d scenarios (baseline %d), %d tests (baseline %d), %d feature vectors (baseline %d)",
		current.Scenarios, baseline.Scenarios, current.Tests, baseline.Tests,
		len(current.ByFeature), len(baseline.ByFeature))
}

// TestCensusBaselineIsMeasured pins that the committed baseline was produced
// from the committed files, not typed in.
//
// A ratchet whose baseline is lower than reality is a ratchet with slack in
// it: scenarios can be deleted up to the gap without the gate noticing, and
// the gap grows every time a batch lands without updating the baseline. The
// dimension audit has the same problem in a worse form, since a feature vector
// missing from the baseline is unratcheted entirely.
func TestCensusBaselineIsMeasured(t *testing.T) {
	t.Parallel()
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	current := factorycorpus.ComputeCensus(files)
	baseline, err := factorycorpus.LoadCensus(factorycorpus.CensusBaselinePath)
	if err != nil {
		t.Fatalf("read census baseline: %v", err)
	}
	if baseline.Scenarios != current.Scenarios || baseline.Tests != current.Tests {
		t.Errorf("baseline (%d scenarios, %d tests) does not equal the measured corpus (%d, %d): "+
			"run `go run ./cmd/factory-run` or regenerate the baseline, otherwise the ratchet has slack "+
			"and scenarios can be deleted up to the gap unnoticed",
			baseline.Scenarios, baseline.Tests, current.Scenarios, current.Tests)
	}
	for fv, n := range current.ByFeature {
		if baseline.ByFeature[fv] != n {
			t.Errorf("baseline feature vector %q counts %d, measured %d — an unratcheted dimension",
				fv, baseline.ByFeature[fv], n)
		}
	}
	for fv := range baseline.ByFeature {
		if _, ok := current.ByFeature[fv]; !ok {
			t.Errorf("baseline names feature vector %q that no committed file has", fv)
		}
	}
	// Blessing is ratcheted exactly like the feature vector, so it needs the
	// same baseline-is-measured guard. Checking only the feature dimension
	// leaves the blessing dimension unratcheted whenever its baseline is
	// absent or stale: CheckRatchet compares current against a key that is not
	// there, reads zero for the baseline, and passes — so the whole
	// cross-engine-to-metamorphic downgrade the blessing ratchet exists to
	// catch would land green.
	for b, n := range current.ByBlessing {
		if baseline.ByBlessing[b] != n {
			t.Errorf("baseline blessing %q counts %d, measured %d — an unratcheted dimension",
				b, baseline.ByBlessing[b], n)
		}
	}
	for b := range baseline.ByBlessing {
		if _, ok := current.ByBlessing[b]; !ok {
			t.Errorf("baseline names blessing %q that no committed file has", b)
		}
	}
	// Both maps partition the same scenarios, so both must sum to the scenario
	// count. A dimension map that does not is one whose keys stopped covering
	// every file, which makes every per-key number in it a claim about an
	// unknown subset.
	sum := func(m map[string]int) int {
		total := 0
		for _, n := range m {
			total += n
		}
		return total
	}
	if got := sum(baseline.ByFeature); got != baseline.Scenarios {
		t.Errorf("baseline by_feature sums to %d, not the %d scenarios it partitions", got, baseline.Scenarios)
	}
	if got := sum(baseline.ByBlessing); got != baseline.Scenarios {
		t.Errorf("baseline by_blessing sums to %d, not the %d scenarios it partitions", got, baseline.Scenarios)
	}
}

// TestRatchetDetectsEveryShrinkDirection is the detector for the ratchet.
//
// A gate is a claim about what cannot reach the tree, and a gate that matches
// nothing is green forever while the thing it guards rots. Each subtest
// removes ONE dimension's worth of coverage from a synthetic census and
// asserts CheckRatchet names it — because the failure mode is not "the ratchet
// is absent", it is "the ratchet checks the total and calls a per-dimension
// collapse healthy growth".
func TestRatchetDetectsEveryShrinkDirection(t *testing.T) {
	t.Parallel()
	base := factorycorpus.Census{
		Scenarios:  10,
		Tests:      40,
		ByFeature:  map[string]int{"shape=single;where=cmp.eq": 6, "shape=join2.inner;where=and(cmp.eq,cmp.lt)": 4},
		ByBlessing: map[string]int{"metamorphic": 7, "cross-engine": 3},
		ByKeyBlessing: map[string]string{
			"k1": "cross-engine", "k2": "metamorphic", "k3": "metamorphic-tlp-only",
		},
	}
	clone := func(mutate func(*factorycorpus.Census)) factorycorpus.Census {
		c := factorycorpus.Census{
			Scenarios:     base.Scenarios,
			Tests:         base.Tests,
			ByFeature:     map[string]int{},
			ByBlessing:    map[string]int{},
			ByKeyBlessing: map[string]string{},
		}
		for k, v := range base.ByFeature {
			c.ByFeature[k] = v
		}
		for k, v := range base.ByBlessing {
			c.ByBlessing[k] = v
		}
		for k, v := range base.ByKeyBlessing {
			c.ByKeyBlessing[k] = v
		}
		mutate(&c)
		return c
	}

	for _, tc := range []struct {
		name   string
		mutate func(*factorycorpus.Census)
	}{
		{"scenario deleted", func(c *factorycorpus.Census) { c.Scenarios--; c.Tests -= 4; c.ByFeature["shape=single;where=cmp.eq"]-- }},
		{"test stanza deleted", func(c *factorycorpus.Census) { c.Tests-- }},
		{"feature vector thinned", func(c *factorycorpus.Census) { c.ByFeature["shape=join2.inner;where=and(cmp.eq,cmp.lt)"] = 3 }},
		{"feature vector erased", func(c *factorycorpus.Census) { delete(c.ByFeature, "shape=join2.inner;where=and(cmp.eq,cmp.lt)") }},
		// Per FILE, not per label count: a count floor fires on PROMOTION and
		// misses the replacement downgrade, so the gate keys on the scenario.
		{"blessing downgraded", func(c *factorycorpus.Census) { c.ByKeyBlessing["k1"] = "metamorphic" }},
		{"blessing downgraded to tlp-only", func(c *factorycorpus.Census) { c.ByKeyBlessing["k2"] = "metamorphic-tlp-only" }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if shrinks := factorycorpus.CheckRatchet(base, clone(tc.mutate)); len(shrinks) == 0 {
				t.Fatalf("CheckRatchet reported no shrink for %q — the ratchet does not gate this direction, "+
					"so coverage can be removed on it without CI noticing", tc.name)
			}
		})
	}

	// Growth on every dimension must stay silent, or the gate is noise and the
	// next person turns it off.
	grown := clone(func(c *factorycorpus.Census) {
		c.Scenarios += 5
		c.Tests += 20
		c.ByFeature["shape=single;where=cmp.eq"] += 5
		c.ByFeature["shape=cte;where=or(cmp.gt,cmp.ne)"] = 3
		c.ByBlessing["cross-engine"] += 5
	})
	if shrinks := factorycorpus.CheckRatchet(base, grown); len(shrinks) != 0 {
		t.Fatalf("CheckRatchet reported a shrink for a corpus that only grew: %v", shrinks)
	}
}
