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

	var totalRootWildcardBridges int64

	for _, site := range sites {
		c := cascades.OrderingComparisonCensusOf(site)

		t.Logf("%v: total=%d fieldPairs=%d decided=%d declineResidual=%d "+
			"residualLost=%d residualAgreed=%d nonFieldPairs=%d nonFieldBridgeOnly=%d "+
			"rootWildcardBridges=%d rootWildcardNoContext=%d rootWildcardContextRooted=%d "+
			"rootWildcardMultiRoot=%d rootWildcardIntransitive=%d",
			site, c.Total, c.FieldPairs, c.FieldPairsDecided, c.DeclineResidual,
			c.ResidualMatchesLost, c.ResidualWeakerArmsAgreed,
			c.NonFieldPairs, c.NonFieldBridgeOnly,
			c.RootWildcardBridges, c.RootWildcardNoContext, c.RootWildcardContextRooted,
			c.RootWildcardMultiRoot, c.RootWildcardIntransitive)

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

		totalRootWildcardBridges += c.RootWildcardBridges

		if c.RootWildcardNoContext != 0 {
			t.Errorf("%v: %d of %d root-wildcard bridges were classified with NO "+
				"comparison context.\n\n"+
				"The two ambiguity zeros below are then zeros over a population the "+
				"instrument never looked at. A comparator call site was added without "+
				"threading the list it is scanning: use orderingValuesEqualIn / "+
				"intersectionValuesEqualIn and pass the haystack.",
				site, c.RootWildcardNoContext, c.RootWildcardBridges)
		}

		if c.RootWildcardMultiRoot != 0 || c.RootWildcardIntransitive != 0 {
			t.Errorf("%v: %d of %d root-wildcard bridges sit in a comparison context "+
				"holding a SECOND distinct quantifier root, and %d of those share the "+
				"childless key's column path.\n\n"+
				"values.SameOrderingColumn treats the ZERO correlation as a WILDCARD, "+
				"so a childless key bridges to every named quantifier while two named "+
				"quantifiers decline each other. With two of them in one list that is "+
				"the intransitive triple (childless ≡ o.A, childless ≡ i.A, "+
				"o.A ≢ i.A), and membership in the list then depends on the order it "+
				"was built in — a nondeterministic plan, not a lost merge.\n\n"+
				"The comparator behaviour is already pinned as a known defect by "+
				"ordering_comparator_dispatch_test.go's root-axis witness; this count "+
				"being zero is what keeps it UNREACHABLE. The fix is NOT to delete the "+
				"wildcard (876 corpus matches rest on it — a candidate mints its keys "+
				"childless while a scoped request does not). It is CQ-55-A2's "+
				"correlation-space translation: resolve the childless root to the "+
				"quantifier it actually reads, and the wildcard has nothing left to do.",
				site, c.RootWildcardMultiRoot, c.RootWildcardBridges,
				c.RootWildcardIntransitive)
		}

		if c.NonFieldBridgeOnly != 0 {
			t.Errorf("%v: the ordinal-free NAME bridge is the ONLY arm deciding %d of "+
				"%d non-FieldValue pairs EQUAL.\n\n"+
				"That arm is kept below the structural comparison on the grounds that "+
				"it is unexercised-but-reachable — the CardinalityValue wrapper is the "+
				"only population that can still cross it "+
				"(ordering_comparison_census_test.go pins that reachability). A "+
				"nonzero count means production now DEPENDS on an ordinal-free name "+
				"match at an ordering comparator, which is the comparison RFC-197 "+
				"exists to remove. Find the pair and give it a column identity rather "+
				"than accepting the name match.",
				site, c.NonFieldBridgeOnly, c.NonFieldPairs)
		}
	}

	// The root-axis zeros above are only evidence if the wildcard actually fires
	// somewhere in the corpus. It does — the requested side is scoped to its owning
	// quantifier while the candidate mints childless — and if it ever stops, the
	// three zeros become a zero over an empty population and say nothing.
	//
	// This is asserted across BOTH sites rather than per site on purpose: the
	// intersection site legitimately sees no cross-root pair at all (both of its
	// operands are candidate-side bakes), so a per-site nonzero would pin an
	// incidental fact instead of the instrument's liveness.
	if totalRootWildcardBridges == 0 {
		t.Errorf("neither comparator made a single root-wildcard bridge over the " +
			"corpus.\n\n" +
			"values.SameOrderingColumn's zero-correlation wildcard is then dead in " +
			"production, and every root-axis zero asserted above is a zero over an " +
			"empty set. Two possibilities, and they need different responses: the " +
			"childless root now carries a real correlation (CQ-55-A2 landed — then " +
			"DELETE the wildcard, delete the root-axis witness in " +
			"ordering_comparator_dispatch_test.go, and these assertions with it), or " +
			"the corpus stopped reaching the comparator at all.")
	}
}
