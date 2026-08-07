package explaindiff_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// ProjectionMergeRule used to compose an outer projection slot by matching the
// outer read's DISPLAY NAME against the inner projection's slot names. That arm
// is gone (RFC-197, name-keyed): a name cannot select a slot, because two inner
// slots may share one output name and one slot may be addressed by two names.
//
// Removing it rather than converting it rests on a claim about the whole corpus —
// every outer read arriving at this rule carries its output ordinal, so the arm
// served nobody. That claim is counted here rather than argued, and it is counted
// here rather than beside the rule because this harness imports the planner, so a
// test inside the planner package could not import the harness back.
//
// It is counted rather than PANICKED for the reason the census header records at
// length: the debt entry this deletion retired said the arm was "HEAVILY LIVE"
// on the evidence of a panic reddening dozens of tests. The panic was real. What
// it proved was that the RULE is reachable, not that the ARM is — and the arm
// took zero of the rule's firings. A panic probe answers "is this point
// reachable at all", once, and destroys the run that would have answered
// anything else.
func TestProjectionMergeComposesOnlyOrdinalsOverTheCorpus(t *testing.T) {
	t.Parallel()

	cascades.ResetProjectionMergeCensus()

	if _, _, err := explaindiff.Collect(corpusDir); err != nil {
		t.Fatalf("collect corpus: %v", err)
	}

	c := cascades.ProjectionMergeCensusSnapshot()
	t.Logf("ProjectionMergeRule: firings=%d slots=%d baked=%d lazy=%d notComposable=%d",
		c.RuleFirings, c.SlotCompositions, c.BakedSingleAccessor,
		c.LazyOuterReads, c.DeclinedNotComposable)

	// The vacuity guards come FIRST. Every assertion below them is a zero, and a
	// zero over a population nothing reached is indistinguishable from a zero over
	// a population that was reached and stayed clean.
	if c.RuleFirings == 0 {
		t.Fatalf("ProjectionMergeRule fired ZERO times over the corpus.\n\n" +
			"This test is then vacuous and so is the lazy-read zero below it. " +
			"Either the census stopped being wired into the rule or the corpus " +
			"stopped producing a projection over a projection; find out which " +
			"before trusting the zero.")
	}
	if c.SlotCompositions == 0 {
		t.Fatalf("ProjectionMergeRule fired %d times and examined ZERO outer slots.\n\n"+
			"The slot loop is then unreached, so no arm below it was exercised.",
			c.RuleFirings)
	}
	if c.BakedSingleAccessor == 0 {
		t.Fatalf("ProjectionMergeRule examined %d slots and composed NONE by ordinal.\n\n"+
			"The ordinal arm is the only composing arm left. A corpus that never "+
			"takes it cannot say anything about whether the removed NAME arm was "+
			"needed, because it never merged a projection at all.",
			c.SlotCompositions)
	}

	// The claim.
	if c.LazyOuterReads != 0 {
		t.Errorf("ProjectionMergeRule saw %d LAZY outer reads over the corpus, want 0.\n\n"+
			"A lazy outer read is a childless FieldValue with no resolved path — a "+
			"display name and nothing else. The resolver is supposed to bake a "+
			"projection-output reference to its output ordinal before it reaches "+
			"this rule, and this count going nonzero means it stopped.\n\n"+
			"Nothing is silently wrong: the rule DECLINES such a read, so the cost "+
			"is a lost merge (an extra Projection operator), never a wrong column. "+
			"But the fix belongs at the resolver, not here — restoring a "+
			"name-matching arm at this rule is the RFC-197 defect, not its repair.",
			c.LazyOuterReads)
	}

	// The arms must account for every slot. Without this a slot that stopped being
	// classified would drain the lazy count to zero and read as good news.
	if sum := c.BakedSingleAccessor + c.LazyOuterReads + c.DeclinedNotComposable; sum != c.SlotCompositions {
		t.Errorf("arms sum to %d but %d slots were examined.\n\n"+
			"An unclassified slot is a hole in the instrument: the lazy-read zero "+
			"above is only meaningful if every slot lands in exactly one arm.",
			sum, c.SlotCompositions)
	}
}
