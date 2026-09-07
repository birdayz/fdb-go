package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// TestAdjustMatches_LeafMatchDoesNotClimb pins a REFUSAL, with the message
// naming what re-arms.
//
// A scan group's partial match against a value-index candidate is seeded at
// the candidate's LEAF (its scan). Adjustment should climb it through the
// candidate's select to the MatchableSortExpression, whose adjustment mints
// the matched ordering parts an ordered full index scan is chosen by
// (adjustMatchForMatchableSort, ComputeMatchedOrderingParts — both ported).
// It does not climb, and ONE gate refuses it: matchWithCandidate refuses at
// correlatedToEquals, Go's stand-in for Java's
// `candidateExpression.getCorrelatedTo().equals(otherRangesOver.getCorrelatedTo())`,
// which demands ZERO node-local correlations on the candidate expression —
// while the candidate's own select reports two: the placeholder's parameter
// alias (Go's Placeholder.GetCorrelatedTo counts it; Java's
// PredicateWithValueAndRanges.getCorrelatedTo is value ∪ ranges only) and its
// own inner quantifier's alias (Java's getCorrelatedTo subtracts the aliases
// the expression owns). Admitting that gate under mutation, with the match
// seeded as the planner seeds it (matchLeafWithCandidate, which builds the
// MaxMatchMap every adjustment requires), the leaf match climbs through the
// select to the MatchableSortExpression and carries an ordering part — so
// this test goes RED under that mutation, which is what makes it a sentinel.
// (A seed without the MaxMatchMap stops at the select's nil-map check instead,
// which once read as a second gate; it is a fixture artifact, not a gate.)
// So a zero-prefix match carries no ordering parts,
// SatisfiesRequestedOrdering sees no order in any candidate, and an ordered
// full index scan exists only where a sort sits DIRECTLY over a scan
// (OrderedIndexScanRule, OrderedPrimaryScanRule). TODO.md, "Ordering through
// a projection reaches the child group but not the index".
//
// When the climb works — the gate ported — this test turns red: re-pin it to
// the adjusted twin and retire the two Go-only ordered-scan rules.
func TestAdjustMatches_LeafMatchDoesNotClimb(t *testing.T) {
	t.Parallel()

	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	traversal := cand.GetTraversal()
	leaves := traversal.GetLeafReferences()
	if len(leaves) != 1 {
		t.Fatalf("value-index candidate traversal has %d leaves, want the one scan", len(leaves))
	}
	leaf := leaves[0]

	// The refusal's premise: the leaf's parent in the candidate — its select
	// — reports node-local correlations, and one of them is the select's own
	// inner quantifier alias, which Java's getCorrelatedTo would not count.
	parents := traversal.GetParentRefPairs(leaf)
	if len(parents) == 0 {
		t.Fatal("the candidate's scan has no parent in its traversal")
	}
	for _, parent := range parents {
		nodeCorrs := parent.expr.GetCorrelatedToWithoutChildren()
		if len(nodeCorrs) == 0 {
			t.Fatalf("the candidate's %T reports no node-local correlations; correlatedToEquals now admits it and the leaf match can climb — re-pin this test to the adjusted twin", parent.expr)
		}
		ownsOne := false
		for _, q := range parent.expr.GetQuantifiers() {
			if _, owned := nodeCorrs[q.GetAlias()]; owned {
				ownsOne = true
			}
		}
		if !ownsOne {
			t.Fatalf("the candidate's %T no longer counts its own quantifier aliases among its node-local correlations %v; the refusal has moved", parent.expr, nodeCorrs)
		}
	}

	queryScan := referenceWinnerFullScan(t, "Order", "STATUS")
	queryRef := expressions.InitialOf(queryScan)
	// Seeded as MatchLeafRule seeds it — with the MaxMatchMap — so the only
	// thing between this match and the climb is the gate under test.
	seeds := matchLeafWithCandidate(queryScan, leaf.Get())
	if len(seeds) != 1 {
		t.Fatalf("matchLeafWithCandidate yielded %d seeds for the candidate's scan, want one", len(seeds))
	}
	seedPM := NewPartialMatch(seeds[0].boundAliasMap, cand, queryRef, queryScan, leaf, seeds[0].matchInfo)
	if !AddPartialMatchForCandidate(queryRef, cand, seedPM) {
		t.Fatal("seeding the leaf match was rejected")
	}
	AdjustMatches(queryRef)

	for _, pm := range GetPartialMatchesForCandidate(queryRef, cand) {
		if pm.GetCandidateRef() != leaf {
			t.Fatalf("the leaf match CLIMBED to candidate ref %p (%T): correlatedToEquals now admits the candidate's select — re-pin this test to the climb, then retire OrderedIndexScanRule and OrderedPrimaryScanRule and close the TODO entry", pm.GetCandidateRef(), pm.GetCandidateRef().Get())
		}
		if len(pm.GetMatchInfo().GetMatchedOrderingParts()) != 0 {
			t.Fatalf("the unadjusted leaf match carries %d matched ordering parts, want none", len(pm.GetMatchInfo().GetMatchedOrderingParts()))
		}
	}
}
