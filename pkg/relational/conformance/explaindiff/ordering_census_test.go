package explaindiff_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// The ordering-value comparators dispatch by VALUE TYPE: a pair of plain
// FieldValues is decided by column identity and by nothing else, with no
// fallthrough to the domain-blind structural comparison. That is only free if
// every FieldValue reaching either comparator has STATED an identity — a layout
// token and a non-negative ordinal — because a value that has not is declined
// outright.
//
// "Every one of them states an identity" is a claim about the whole corpus, so
// it is counted over the whole corpus. It lives HERE and not beside the
// comparators because this package's harness imports the planner, so a test
// inside the planner package cannot import the harness back.
//
// The counts are produced by pkg/recordlayer/query/plan/cascades'
// ordering_comparison_census.go, which classifies each pair independently of
// whichever arm the site ran, and derives what the RETIRED availability dispatch
// would have answered for the same pair. So the zero this test asserts is not
// "the new code is self-consistent" but "the new code and the old code agree
// pair-for-pair over 2481 queries, and the new one is additionally transitive".

// TestOrderingComparatorsDeclineNothingOnTheCorpus is the standing precondition
// for type dispatch at both comparators.
//
// The census is enabled for this test only. Counting is monotone, so a
// concurrently-running corpus pass in the same binary can only ADD comparisons —
// it can make a zero assertion fail, never falsely pass.
func TestOrderingComparatorsDeclineNothingOnTheCorpus(t *testing.T) {
	t.Parallel()

	cascades.ResetOrderingComparisonCensus()
	cascades.SetOrderingComparisonCensusEnabled(true)
	defer cascades.SetOrderingComparisonCensusEnabled(false)

	if _, _, err := explaindiff.Collect(corpusDir); err != nil {
		t.Fatalf("collect corpus: %v", err)
	}

	sites := []cascades.OrderingComparisonSite{
		cascades.OrderingSiteRequestedVsCandidate,
		cascades.OrderingSiteIntersectionKeys,
	}

	for _, site := range sites {
		c := cascades.OrderingComparisonCensusOf(site)

		t.Logf("%v: total=%d fieldPairs=%d decided=%d declineResidual=%d "+
			"residualLost=%d residualAgreed=%d nonFieldPairs=%d nonFieldBridgeOnly=%d",
			site, c.Total, c.FieldPairs, c.FieldPairsDecided, c.DeclineResidual,
			c.ResidualMatchesLost, c.ResidualWeakerArmsAgreed,
			c.NonFieldPairs, c.NonFieldBridgeOnly)

		if c.Total == 0 {
			t.Errorf("%v made NO comparisons over the corpus.\n\n"+
				"This test is then vacuous, and every zero below it is vacuous too. "+
				"Either the census stopped being wired into the comparator or the "+
				"corpus stopped reaching it; find out which before trusting any "+
				"other assertion in this file.", site)
			continue
		}

		if c.FieldPairs == 0 {
			t.Errorf("%v compared %d pairs and NONE of them were two plain "+
				"FieldValues.\n\n"+
				"The identity arm is then dead at this site, so the transitivity it "+
				"provides is untested by this corpus and the decline residual below "+
				"is a zero over an empty set.", site, c.Total)
		}

		if c.DeclineResidual != 0 {
			t.Errorf("%v: %d of %d FieldValue pairs have an operand that states NO "+
				"column identity, and %d of those would have MATCHED under the "+
				"retired availability dispatch.\n\n"+
				"Type dispatch declines them, so each one is a set-operation merge "+
				"lost. The fix is NOT to restore the fallthrough — that comparator was "+
				"intransitive, and an insertion-order-dependent ordering set is a "+
				"nondeterministic plan. The fix is at the PRODUCER that minted an "+
				"ordering key without the layout its ordinal indexes: find it (the "+
				"candidate mints route through bakeOrderingColumnIn, the requested "+
				"side through the translator's sort-key bakes) and give it the layout.",
				site, c.DeclineResidual, c.FieldPairs, c.ResidualMatchesLost)
		}
	}
}
