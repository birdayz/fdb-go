package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// TestAdjustMatches_LeafMatchClimbsToTheCandidateRoot pins Java's
// AdjustMatchRule over a value-index candidate. A scan group's partial match
// is seeded at the candidate's leaf (its scan); adjustment absorbs the
// candidate's Select (whose predicates are unconstrained placeholders,
// SelectExpression.adjustMatch) and then its MatchableSortExpression, which
// mints the matched ordering parts. The climb is what gives a bare join leg
// (a type filter with no Select of its own) Java's index access: the match
// completes on the leg's own reference (measured on the JVM: the data-access
// rule fires on a LogicalTypeFilterExpression and yields ISCAN(I1 <,>)).
//
// The gate is correlatedToEquals, Java's
// candidateExpression.getCorrelatedTo().equals(child.getCorrelatedTo()): the
// candidate's own correlations exclude the aliases it owns and a placeholder's
// parameter alias. Restoring Go's former zero-correlation reading reddens this.
func TestAdjustMatches_LeafMatchClimbsToTheCandidateRoot(t *testing.T) {
	t.Parallel()

	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	traversal := cand.GetTraversal()
	leaves := traversal.GetLeafReferences()
	if len(leaves) != 1 {
		t.Fatalf("value-index candidate traversal has %d leaves, want the one scan", len(leaves))
	}
	leaf := leaves[0]

	queryScan := referenceWinnerFullScan(t, "Order", "STATUS")
	queryRef := expressions.InitialOf(queryScan)
	seeds := matchLeafWithCandidate(queryScan, leaf.Get())
	if len(seeds) != 1 {
		t.Fatalf("matchLeafWithCandidate yielded %d seeds for the candidate's scan, want one", len(seeds))
	}
	seedPM := NewPartialMatch(seeds[0].boundAliasMap, cand, queryRef, queryScan, leaf, seeds[0].matchInfo)
	if !AddPartialMatchForCandidate(queryRef, cand, seedPM) {
		t.Fatal("seeding the leaf match was rejected")
	}
	AdjustMatches(queryRef)

	root := traversal.GetRootReference()
	var complete PartialMatch
	for _, pm := range GetPartialMatchesForCandidate(queryRef, cand) {
		if pm.GetCandidateRef() == root {
			complete = pm
		}
	}
	if complete == nil {
		t.Fatal("the leaf match did not climb to the candidate's root (MatchableSortExpression)")
	}
	if _, ok := root.Get().(*expressions.MatchableSortExpression); !ok {
		t.Fatalf("candidate root is %T, want the MatchableSortExpression", root.Get())
	}
	if len(complete.GetMatchInfo().GetMatchedOrderingParts()) == 0 {
		t.Fatal("the complete match carries no matched ordering parts; the MatchableSortExpression adjustment did not run")
	}
}
