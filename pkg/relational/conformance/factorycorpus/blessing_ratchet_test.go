package factorycorpus_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// TestBlessingRatchetAllowsPromotionAndRefusesDowngrade is the authority
// ratchet's detector.
//
// The direction is the whole point and it is easy to get backwards. An
// aggregate count per label fires on PROMOTION — moving a file from
// metamorphic to cross-engine decrements `metamorphic`, which a count floor
// reads as loss — while staying silent on the downgrade that deletes a
// cross-engine file and adds an unrelated metamorphic one. Both mistakes look
// like a working ratchet from the outside, so each direction is asserted
// separately here.
func TestBlessingRatchetAllowsPromotionAndRefusesDowngrade(t *testing.T) {
	t.Parallel()

	census := func(byKey map[string]string) factorycorpus.Census {
		c := factorycorpus.Census{
			Scenarios:     len(byKey),
			Tests:         len(byKey) * 4,
			ByFeature:     map[string]int{"shape=single;where=cmp.eq": len(byKey)},
			ByBlessing:    map[string]int{},
			ByKeyBlessing: map[string]string{},
		}
		for k, v := range byKey {
			c.ByKeyBlessing[k] = v
			c.ByBlessing[v]++
		}
		return c
	}

	base := census(map[string]string{
		"k-tlp":   string(factorycorpus.BlessingMetamorphicTLPOnly),
		"k-meta":  string(factorycorpus.BlessingMetamorphic),
		"k-cross": string(factorycorpus.BlessingCrossEngine),
	})

	t.Run("promotion is silent", func(t *testing.T) {
		t.Parallel()
		// Every file gains authority. A count floor on `metamorphic` or on
		// `metamorphic-tlp-only` would fire here — both counts go to zero.
		promoted := census(map[string]string{
			"k-tlp":   string(factorycorpus.BlessingMetamorphic),
			"k-meta":  string(factorycorpus.BlessingCrossEngine),
			"k-cross": string(factorycorpus.BlessingCrossEngine),
		})
		if shrinks := factorycorpus.CheckRatchet(base, promoted); len(shrinks) != 0 {
			t.Fatalf("promoting every scenario was reported as a shrink: %v\n"+
				"the ratchet is pointing the wrong way — it fires on the improvement it exists to encourage", shrinks)
		}
	})

	for _, tc := range []struct{ name, key, to string }{
		{"cross-engine to metamorphic", "k-cross", string(factorycorpus.BlessingMetamorphic)},
		{"cross-engine to tlp-only", "k-cross", string(factorycorpus.BlessingMetamorphicTLPOnly)},
		{"metamorphic to tlp-only", "k-meta", string(factorycorpus.BlessingMetamorphicTLPOnly)},
	} {
		tc := tc
		t.Run("downgrade: "+tc.name, func(t *testing.T) {
			t.Parallel()
			byKey := map[string]string{
				"k-tlp":   string(factorycorpus.BlessingMetamorphicTLPOnly),
				"k-meta":  string(factorycorpus.BlessingMetamorphic),
				"k-cross": string(factorycorpus.BlessingCrossEngine),
			}
			byKey[tc.key] = tc.to
			shrinks := factorycorpus.CheckRatchet(base, census(byKey))
			if len(shrinks) == 0 {
				t.Fatalf("%s was accepted; a committed expectation may gain authority, never lose it", tc.name)
			}
			joined := strings.Join(shrinks, "\n")
			if !strings.Contains(joined, tc.key) {
				t.Errorf("the shrink does not name the scenario that lost authority: %s", joined)
			}
		})
	}
}

// TestBlessingRankOrdersEveryAuthority pins the scale the ratchet compares on.
// An unranked label silently ranks 0, which is BELOW tlp-only — so adding a
// blessing and forgetting it here would make every file carrying it read as a
// downgrade, and every downgrade TO it read as fine.
func TestBlessingRankOrdersEveryAuthority(t *testing.T) {
	t.Parallel()
	ranks := map[factorycorpus.Blessing]int{
		factorycorpus.BlessingMetamorphicTLPOnly: factorycorpus.BlessingRank(factorycorpus.BlessingMetamorphicTLPOnly),
		factorycorpus.BlessingMetamorphic:        factorycorpus.BlessingRank(factorycorpus.BlessingMetamorphic),
		factorycorpus.BlessingCrossEngine:        factorycorpus.BlessingRank(factorycorpus.BlessingCrossEngine),
	}
	for b, r := range ranks {
		if r == 0 {
			t.Errorf("blessing %q has no rank; it would compare below every other authority", b)
		}
	}
	if !(ranks[factorycorpus.BlessingMetamorphicTLPOnly] < ranks[factorycorpus.BlessingMetamorphic] &&
		ranks[factorycorpus.BlessingMetamorphic] < ranks[factorycorpus.BlessingCrossEngine]) {
		t.Fatalf("authorities are not strictly ordered weakest→strongest: %v", ranks)
	}
	if factorycorpus.BlessingRank("not-a-blessing") != 0 {
		t.Error("an unknown blessing must rank 0 so it can never masquerade as authority")
	}
}

// TestTLPOnlyIsADistinctCensusDimension pins that the weaker authority is
// counted separately. If it were folded into `metamorphic`, the census would
// report authority the corpus does not have, and the whole point of recording
// a blessing per file is that the number means something.
func TestTLPOnlyIsADistinctCensusDimension(t *testing.T) {
	t.Parallel()
	if factorycorpus.BlessingMetamorphicTLPOnly == factorycorpus.BlessingMetamorphic {
		t.Fatal("the TLP-only label is not distinct from the plain metamorphic one")
	}
	files := loadCorpus(t)
	c := factorycorpus.ComputeCensus(files)
	var sum int
	for _, n := range c.ByBlessing {
		sum += n
	}
	if sum != c.Scenarios {
		t.Errorf("by_blessing sums to %d over %d scenarios — a label is being dropped or double-counted",
			sum, c.Scenarios)
	}
	if len(c.ByKeyBlessing) != c.Scenarios {
		t.Errorf("by_key_blessing holds %d entries for %d scenarios; the per-file ratchet would skip the difference",
			len(c.ByKeyBlessing), c.Scenarios)
	}
	t.Logf("committed authority mix: %v", c.ByBlessing)
}
