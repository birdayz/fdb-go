package factorycorpus_test

import (
	"strings"
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
		if len(f.Scenario.Setup) == 0 {
			t.Errorf("%s: no setup statements; the expectations would be frozen over an empty table", f.Path)
		}
		for i, tc := range f.Scenario.Tests {
			if tc.Query == "" {
				t.Errorf("%s: tests[%d] has no query", f.Path, i)
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

// corpusSample loads the committed corpus and returns it with the sample the
// per-PR tier would run, plus that sample's shape-family histogram.
func corpusSample(t *testing.T) (files, sample []*factorycorpus.File, hist map[string]int) {
	t.Helper()
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	n := factorycorpus.SampleSize
	if n > len(files) {
		n = len(files)
	}
	sample = factorycorpus.StratifiedSample(files, n)
	if len(sample) != n {
		t.Fatalf("sample size %d, want %d", len(sample), n)
	}
	hist = map[string]int{}
	for _, f := range sample {
		hist[factorycorpus.ShapeFamily(f.Header.FeatureVector)]++
	}
	return files, sample, hist
}

// Floors for the per-PR sample's shape-family spread, measured against the
// committed corpus rather than guessed.
//
// The corpus holds EIGHT shape families (single, join2/join3 × comma, inner,
// left-outer, and join2.right-outer), the smallest at 6% of the files, and the
// stratification key crosses those eight with the WHERE-clause class into
// thirty-odd strata. Spreading a hundred-file budget evenly over those strata
// gives every family several files and the widest family — `single`, which has
// the most distinct WHERE classes — just under a fifth of the sample.
//
// So minSampleFamilies is every family the corpus has: a sample that reaches
// only seven has silently stopped testing a whole query shape, and there is no
// budget pressure that would justify it while thirty-odd strata each fit
// several files. maxFamilyShare sits at a quarter, comfortably above the ~19%
// equal allocation actually produces and far under what a degenerate sample
// scores: a lexicographic prefix of the feature vectors scores 100%, and a
// size-proportional sample would put the widest family near its 21% corpus
// share and climbing as batches favour one generator shape.
const (
	minSampleFamilies = 8
	maxFamilyShare    = 0.25
)

// TestStratifiedSampleResistsFamilyCollapse is the per-PR sample's real gate:
// the budget must be spread over query families, not spent on one.
//
// It is deliberately NOT phrased as "the sample covers at least min(n, number
// of groups) groups". That form is a tautology whenever the corpus has more
// groups than the budget — which is the normal state, since the feature vector
// carries indexes, projections and ordering and is therefore nearly unique per
// file. Any n files with distinct vectors satisfy it, INCLUDING n files that
// are all the same query shape, so it stayed green through a sample that ran
// one query family a hundred times.
func TestStratifiedSampleResistsFamilyCollapse(t *testing.T) {
	t.Parallel()
	_, sample, hist := corpusSample(t)
	t.Logf("sample shape-family histogram (n=%d): %v", len(sample), hist)

	if len(hist) < minSampleFamilies {
		t.Errorf("the per-PR sample covers %d shape families, want at least %d: %v — "+
			"the budget has collapsed onto a subset of the query shapes and the families missing here "+
			"are untested on every PR", len(hist), minSampleFamilies, hist)
	}
	// The cap is relative to the family's share of STRATA, not a flat fraction.
	//
	// Equal allocation hands each stratum the same budget, so a family's share
	// of the sample is its share of the strata — which is a measure of
	// STRUCTURAL DIVERSITY, not of volume. A flat cap therefore punishes the
	// wrong thing: adding a real dimension (correlated sub-constructs) split
	// `single` into more strata and pushed it past a fixed quarter, even though
	// every one of those picks was a structurally distinct query. Capping at
	// twice the family's stratum share keeps the guard pointed at what it was
	// built for — one family eating a budget its diversity does not justify —
	// and lets legitimate diversity through.
	allFiles, _, _ := corpusSample(t)
	strata := map[string]map[string]bool{}
	for _, f := range allFiles {
		fam := factorycorpus.ShapeFamily(f.Header.FeatureVector)
		if strata[fam] == nil {
			strata[fam] = map[string]bool{}
		}
		strata[fam][factorycorpus.StratumKey(f.Header.FeatureVector)] = true
	}
	total := 0
	for _, s := range strata {
		total += len(s)
	}
	for family, n := range hist {
		share := float64(n) / float64(len(sample))
		stratumShare := float64(len(strata[family])) / float64(total)
		cap := 2 * stratumShare
		if cap < maxFamilyShare {
			cap = maxFamilyShare
		}
		if share > cap {
			t.Errorf("shape family %q takes %d of %d sampled scenarios (%.0f%%) but holds only %d of %d "+
				"strata (%.0f%%); cap is %.0f%% — one family eating a budget its structural diversity does "+
				"not justify is the volumetric fallacy the sample exists to avoid",
				family, n, len(sample), share*100, len(strata[family]), total, stratumShare*100, cap*100)
		}
	}
}

// TestStratifiedSampleTracksCorpusFamilyDistribution pins that the sample is
// REPRESENTATIVE of the corpus, not merely diverse.
//
// The collapse guard above bounds how much one family may take; this bounds
// the other direction. A family the corpus covers but the sample never reaches
// is a query shape whose regressions are invisible until the nightly, and the
// 1% floor exists so the claim survives the corpus growing a genuinely
// marginal family rather than being a restatement of today's file counts.
func TestStratifiedSampleTracksCorpusFamilyDistribution(t *testing.T) {
	t.Parallel()
	files, sample, hist := corpusSample(t)
	corpus := map[string]int{}
	for _, f := range files {
		corpus[factorycorpus.ShapeFamily(f.Header.FeatureVector)]++
	}
	t.Logf("corpus shape-family histogram (n=%d): %v", len(files), corpus)
	for family, n := range corpus {
		share := float64(n) / float64(len(files))
		if share < 0.01 {
			continue
		}
		if hist[family] == 0 {
			t.Errorf("shape family %q is %.1f%% of the corpus (%d files) but appears ZERO times in the "+
				"%d-scenario sample: the per-PR tier does not test it at all",
				family, share*100, n, len(sample))
		}
	}
}

// TestStratifiedSampleIsDeterministicAndSpendsOnDistinctVectors pins the two
// properties the walk order has to have.
//
// Stability, because a per-PR gate that runs different tests each time turns a
// real regression into an intermittent. Distinct feature vectors, because
// within a stratum the walk must reach a second VECTOR before it takes a
// second file of the first one — a stratum that spends its whole allocation on
// depth is a miniature of the collapse the outer round-robin fixes, and it
// would not move the family histogram at all.
func TestStratifiedSampleIsDeterministicAndSpendsOnDistinctVectors(t *testing.T) {
	t.Parallel()
	files, sample, _ := corpusSample(t)

	again := factorycorpus.StratifiedSample(files, len(sample))
	for i := range sample {
		if sample[i].Path != again[i].Path {
			t.Fatalf("sample is not deterministic at position %d: %s vs %s", i, sample[i].Path, again[i].Path)
		}
	}

	seen := map[string]string{}
	for _, f := range sample {
		if prev, dup := seen[f.Header.FeatureVector]; dup {
			t.Errorf("%s and %s are both sampled at feature vector %q, while %d vectors in the corpus "+
				"go unsampled: the within-stratum walk is taking depth before breadth",
				prev, f.Path, f.Header.FeatureVector, len(files))
		}
		seen[f.Header.FeatureVector] = f.Path
	}
}

// TestStratifiedSampleReachesConstructsOutsideTheStratumKey records the
// measurement that decided how coarse the stratification key should be.
//
// The key is shape × WHERE-class and nothing else, so every OTHER structural
// dimension the feature vector carries — the scalar-subquery flag is the
// thinnest one in the corpus at ~1% of files — is reached only because the
// within-stratum walk prefers unseen feature vectors, never because the key
// demands it. That is a real bet and it is measurable, so it is asserted
// rather than assumed: the sample currently carries several such files despite
// their corpus share.
//
// If this goes red the bet has stopped paying and the answer is to add the
// dimension to StratumKey (its cardinality still has room under the budget) —
// NOT to lower the expectation here.
func TestStratifiedSampleReachesConstructsOutsideTheStratumKey(t *testing.T) {
	t.Parallel()
	files, sample, _ := corpusSample(t)
	count := func(fs []*factorycorpus.File) int {
		n := 0
		for _, f := range fs {
			if strings.Contains(f.Header.FeatureVector, "scalarsub=") {
				n++
			}
		}
		return n
	}
	inCorpus, inSample := count(files), count(sample)
	t.Logf("scalar-subquery files: %d of %d in the corpus, %d of %d in the sample",
		inCorpus, len(files), inSample, len(sample))
	if inCorpus == 0 {
		t.Fatal("no committed file carries a scalar subquery; this measurement no longer means anything and the " +
			"generator has stopped emitting the construct")
	}
	if inSample == 0 {
		t.Errorf("the corpus holds %d scalar-subquery files and the %d-scenario sample reaches NONE of them: "+
			"a structural dimension outside the stratum key has fallen out of the per-PR tier, so it belongs "+
			"in StratumKey", inCorpus, len(sample))
	}
}

// TestStratumKeyCardinalityFitsTheBudget pins the property that makes the
// stratification a stratification at all.
//
// Round-robin over more strata than the budget never completes a single pass,
// so it degenerates into a lexicographic PREFIX of the sorted stratum list —
// and since a feature vector begins with its shape, that prefix is one query
// family. This is not hypothetical: stratifying on the full feature vector
// gave 894 strata against a budget of 100 and produced exactly that. The guard
// is that a pass must FIT, with room for the corpus to grow new strata.
func TestStratumKeyCardinalityFitsTheBudget(t *testing.T) {
	t.Parallel()
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	strata := map[string]bool{}
	vectors := map[string]bool{}
	for _, f := range files {
		strata[factorycorpus.StratumKey(f.Header.FeatureVector)] = true
		vectors[f.Header.FeatureVector] = true
	}
	t.Logf("%d files, %d feature vectors, %d strata, budget %d",
		len(files), len(vectors), len(strata), factorycorpus.SampleSize)
	if len(strata) > factorycorpus.SampleSize {
		t.Errorf("the stratification key has %d values against a budget of %d: round-robin cannot complete "+
			"one pass, so the sample is a lexicographic prefix of the stratum list rather than a stratified "+
			"sample. Coarsen the key or raise the budget", len(strata), factorycorpus.SampleSize)
	}
}
