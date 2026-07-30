package values

import "testing"

// The census is the instrument the leg-identity retyping rests on, so its own
// arithmetic is pinned here. An instrument that miscounts the FoldOnlyEqual
// population would report a vacuous zero and license the conversion on no
// evidence — which is the exact failure mode the census exists to prevent.
//
// This test does NOT call t.Parallel(): the counters are package-scoped and it
// asserts absolute totals, so it must own them for its duration. Every other test
// in this package leaves the census disabled (its default), so no sibling
// contributes; the FDB corpus pass that DOES enable it lives in another package
// and asserts only a zero, which extra traffic cannot falsely satisfy.
func TestLegIdentityCensus_ClassifiesExactFoldOnlyAndNeither(t *testing.T) {
	ResetLegIdentityCensus()
	SetLegIdentityCensusEnabled(true)
	t.Cleanup(func() {
		SetLegIdentityCensusEnabled(false)
		ResetLegIdentityCensus()
	})

	site := LegSiteRowLegsBinder
	// Exact.
	RecordLegIdentityComparison(site, "A", "A")
	RecordLegIdentityComparison(site, "q$5", "q$5")
	// Fold-only: the conversion delta. Both directions of the case disjointness
	// the planner deliberately maintains — a user alias upper-folded onto the
	// machine namespace, and the reverse.
	RecordLegIdentityComparison(site, "Q$5", "q$5")
	RecordLegIdentityComparison(site, "a", "A")
	// Neither.
	RecordLegIdentityComparison(site, "A", "B")

	c := LegIdentityCensusOf(site)
	if c.Total != 5 {
		t.Fatalf("Total = %d, want 5", c.Total)
	}
	if c.ExactEqual != 2 {
		t.Errorf("ExactEqual = %d, want 2", c.ExactEqual)
	}
	if c.FoldOnlyEqual != 2 {
		t.Errorf("FoldOnlyEqual = %d, want 2 — the population whose zero licenses the "+
			"folding-to-exact conversion must be counted, or its zero is vacuous", c.FoldOnlyEqual)
	}
	if c.Neither != 1 {
		t.Errorf("Neither = %d, want 1", c.Neither)
	}
	if len(c.FoldOnlySamples) != 2 {
		t.Fatalf("FoldOnlySamples = %v, want 2 witnesses — a nonzero count must NAME the "+
			"offending pair so the producer can be found", c.FoldOnlySamples)
	}
	want := map[string]bool{"Q$5 vs q$5": true, "a vs A": true}
	for _, s := range c.FoldOnlySamples {
		if !want[s] {
			t.Errorf("unexpected witness %q, want one of %v", s, want)
		}
	}

	// Sites are INDEPENDENT populations: a fold-only pair at one site must not
	// contaminate another's zero, or a per-site disposition could not be made.
	if other := LegIdentityCensusOf(LegSiteBuriedLegWindow); other.Total != 0 {
		t.Errorf("sibling site total = %d, want 0 — site populations must not merge", other.Total)
	}
}

// The disabled census must record NOTHING. The gate is what keeps an atomic
// increment out of the per-row executor path, so a leak here is both a
// measurement bug and a hot-path regression.
//
// Like its sibling above, this test does NOT call t.Parallel(), and for a
// sharper reason: it asserts the GATE's default state, which is a package-global
// that the sibling test deliberately flips. Running the two concurrently would
// make this one's first assertion a coin flip.
func TestLegIdentityCensus_DisabledRecordsNothing(t *testing.T) {
	ResetLegIdentityCensus()
	t.Cleanup(ResetLegIdentityCensus)

	if LegIdentityCensusEnabled() {
		t.Fatal("census must default to DISABLED — an always-on counter puts a " +
			"contended atomic in the row loop")
	}
	// Callers guard on LegIdentityCensusEnabled(); this asserts the guard's
	// premise, i.e. that the default really is off for every production path.
	for _, site := range LegIdentitySites() {
		if c := LegIdentityCensusOf(site); c.Total != 0 {
			t.Errorf("site %s recorded %d comparisons while disabled", site, c.Total)
		}
	}
}

// The IDENTITY channel must classify the way SameLeg decides, because the whole
// point of it is that the census and the comparison cannot disagree.
//
// This is the defect that made the string-only census unusable at a converted
// site: the site compared identities and the instrument compared texts, so a leg
// whose Name is the UPPER fold of its own lowercase identity was recorded as an
// exact MATCH for a correlation the comparison DECLINED. Recording the pair the
// comparison evaluates is what makes the two agree by construction.
//
// Not parallel, for the same reason as its siblings: package-scoped counters and
// absolute assertions.
func TestLegIdentityCensus_IdentityChannelClassifiesAsSameLegDecides(t *testing.T) {
	ResetLegIdentityCensus()
	SetLegIdentityCensusEnabled(true)
	t.Cleanup(func() {
		SetLegIdentityCensusEnabled(false)
		ResetLegIdentityCensus()
	})

	site := LegSiteOrdinalSlotInLegWindow
	id := NamedCorrelationIdentifier
	// Exact — SameLeg true.
	RecordLegIdentityPair(site, id("A"), id("A"))
	// Fold-only — SameLeg FALSE. This is the pair the text channel called exact:
	// the leg's Name is "Q$5" while its identity is the minted "q$5".
	RecordLegIdentityPair(site, id("q$5"), id("Q$5"))
	// Neither.
	RecordLegIdentityPair(site, id("A"), id("B"))
	// Unstated, both sides. SameLeg declines an unstated identifier
	// unconditionally, so neither of these is a match and neither is an ordinary
	// miss: an omitted identity is its own population.
	RecordLegIdentityPair(site, CorrelationIdentifier{}, id("A"))
	RecordLegIdentityPair(site, id("A"), CorrelationIdentifier{})
	RecordLegIdentityPair(site, CorrelationIdentifier{}, CorrelationIdentifier{})

	c := LegIdentityCensusOf(site)
	if c.Channel != LegChannelIdentity {
		t.Errorf("Channel = %v, want identity — a site's channel is what says its "+
			"numbers describe its comparison", c.Channel)
	}
	if c.Total != 6 {
		t.Fatalf("Total = %d, want 6", c.Total)
	}
	if c.ExactEqual != 1 {
		t.Errorf("ExactEqual = %d, want 1", c.ExactEqual)
	}
	if c.FoldOnlyEqual != 1 {
		t.Errorf("FoldOnlyEqual = %d, want 1 — a minted lowercase leg read by its own "+
			"UPPER Name is the forgery the exact comparison exists to decline", c.FoldOnlyEqual)
	}
	if c.Neither != 1 {
		t.Errorf("Neither = %d, want 1", c.Neither)
	}
	if c.Unstated != 3 {
		t.Errorf("Unstated = %d, want 3 — SameLeg(zero, x), SameLeg(x, zero) and "+
			"SameLeg(zero, zero) all decline, so all three are unstated, not matches",
			c.Unstated)
	}
	// Every classification must be accounted for: a pair silently falling through
	// would make the totals lie.
	if sum := c.ExactEqual + c.FoldOnlyEqual + c.Neither + c.Unstated; sum != c.Total {
		t.Errorf("populations sum to %d, want Total %d", sum, c.Total)
	}
}

// The retired-verdict channel is the conversion's actual acceptance test: it
// counts pairs on which the predicate a site USED TO evaluate disagrees with the
// identity predicate it evaluates now.
//
// Both directions matter, and one of them is invisible to every other population.
// A retired MATCH that is now a decline shows up in FoldOnlyEqual too. A retired
// DECLINE that is now a match does not: a lowercase minted leg matches itself
// exactly, so the pair lands in ExactEqual and is indistinguishable from a pair
// both predicates accepted. That asymmetry is why the verdict is recorded rather
// than reasoned about.
//
// Not parallel: package-scoped counters, absolute assertions.
func TestLegIdentityCensus_RetiredVerdictCountsBothFlipDirections(t *testing.T) {
	ResetLegIdentityCensus()
	SetLegIdentityCensusEnabled(true)
	t.Cleanup(func() {
		SetLegIdentityCensusEnabled(false)
		ResetLegIdentityCensus()
	})

	site := LegSiteBuriedLegWindow
	id := NamedCorrelationIdentifier
	// AGREEMENT: both predicates match, and both decline. Neither is a divergence.
	RecordLegIdentityConversion(site, id("A"), id("A"), true)
	RecordLegIdentityConversion(site, id("A"), id("B"), false)
	// MATCH became DECLINE: the retired predicate accepted the forgery.
	RecordLegIdentityConversion(site, id("q$5"), id("Q$5"), true)
	// DECLINE became MATCH: an upper-folding retired predicate rejected a minted
	// leg read by its own exact spelling. Nothing but this counter sees it — the
	// pair is byte-equal, so it is ExactEqual.
	RecordLegIdentityConversion(site, id("q$7"), id("q$7"), false)

	c := LegIdentityCensusOf(site)
	if c.Total != 4 {
		t.Fatalf("Total = %d, want 4", c.Total)
	}
	if c.RetiredVerdictDivergent != 2 {
		t.Fatalf("RetiredVerdictDivergent = %d, want 2 — one flip in each direction",
			c.RetiredVerdictDivergent)
	}
	// The decline→match flip must be present, and it must be reported as such.
	// Asserting only the count would let the invisible direction go missing while
	// the visible one double-counted.
	want := map[string]bool{
		"q$5 vs Q$5 (retired=match)":   true,
		"q$7 vs q$7 (retired=decline)": true,
	}
	if len(c.RetiredVerdictSamples) != 2 {
		t.Fatalf("RetiredVerdictSamples = %v, want 2", c.RetiredVerdictSamples)
	}
	for _, s := range c.RetiredVerdictSamples {
		if !want[s] {
			t.Errorf("unexpected divergence witness %q, want one of %v", s, want)
		}
	}
	// The decline→match flip is byte-equal, so it lands in ExactEqual — the proof
	// that no other population could have caught it.
	if c.ExactEqual != 2 {
		t.Errorf("ExactEqual = %d, want 2 (the agreeing match plus the invisible "+
			"decline→match flip)", c.ExactEqual)
	}
}

// A site's CHANNEL is observed, not declared, so an instrument that drifts away
// from its comparison is reported instead of believed. That is the mechanism the
// string-only census lacked: the sites were DOCUMENTED as comparing identities
// while recording text, and nothing noticed for a whole review lap.
//
// Not parallel: package-scoped counters, absolute assertions.
func TestLegIdentityCensus_MixedChannelIsDetected(t *testing.T) {
	ResetLegIdentityCensus()
	SetLegIdentityCensusEnabled(true)
	t.Cleanup(func() {
		SetLegIdentityCensusEnabled(false)
		ResetLegIdentityCensus()
	})

	if got := LegIdentityCensusOf(LegSiteRowLegsBinder).Channel; got != LegChannelNone {
		t.Errorf("untouched site Channel = %v, want unrecorded", got)
	}
	RecordLegIdentityComparison(LegSiteRowLegsBinder, "A", "A")
	if got := LegIdentityCensusOf(LegSiteRowLegsBinder).Channel; got != LegChannelText {
		t.Fatalf("Channel after a text record = %v, want text", got)
	}
	RecordLegIdentityPair(LegSiteRowLegsBinder,
		NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("A"))
	if got := LegIdentityCensusOf(LegSiteRowLegsBinder).Channel; got != LegChannelMixed {
		t.Fatalf("Channel after both = %v, want MIXED — a site recording in two "+
			"namespaces has counts that describe neither comparison", got)
	}
	// A pure identity site stays pure: MIXED must mean both, not "identity seen".
	RecordLegIdentityPair(LegSiteBuriedLegWindow,
		NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("A"))
	if got := LegIdentityCensusOf(LegSiteBuriedLegWindow).Channel; got != LegChannelIdentity {
		t.Errorf("identity-only site Channel = %v, want identity", got)
	}
}
