package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestScanRangeEqualityIsADeliberateDivergenceFromJava is a COMPLETE census of
// the exact-key classification, pinned against Java's
// ScanComparisons.getComparisonType and against the two places Go deliberately
// differs from it.
//
// Java's EQUALITY arm is {EQUALS, IS_NULL, DISTANCE_RANK_EQUALS}. Go's set omits
// DISTANCE_RANK_EQUALS and adds NOT_DISTINCT_FROM. Both differences are
// intentional and neither is obvious from reading the switch, which is the whole
// reason this census exists: the shape of "a Go table that is a subset of a
// cited Java table" is a defect ELSEWHERE in this package
// (ComparisonType.IsEquality had the mirror-image version, an extra IN), so the
// next reader comparing this one to Java will reach for the same fix.
//
// Adding DISTANCE_RANK_EQUALS is an active regression, measured rather than
// argued: with it classified as an equality, bindScanComparisonsToRangeSet
// accepts a distance-rank comparison against a LONG key with a type-compatible
// operand — nil error, live materializer — binding a vector distance rank as an
// ordinary exact tuple key. Go lowers DistanceRank to a RecordQueryVectorIndexPlan
// and its scan binder has no vector handling, so the inequality classification
// is what keeps that input rejected as a malformed tail.
func TestScanRangeEqualityIsADeliberateDivergenceFromJava(t *testing.T) {
	t.Parallel()

	// Keyed by type so a missing entry is a hole the exhaustiveness loop names
	// rather than a silently short list.
	want := map[ComparisonType]bool{
		// Shared with Java's EQUALITY arm.
		ComparisonEquals: true,
		ComparisonIsNull: true,
		// Go-only addition; Java has no NOT_DISTINCT_FROM arm at all.
		ComparisonNotDistinctFrom: true,
		// Java EQUALITY, deliberately NOT equality here — see the function's
		// comment and TestScanRangeClassification_DistanceRankEqualsStaysATail.
		ComparisonDistanceRankEquals: false,

		// Java's INEQUALITY arm.
		ComparisonLessThan:                 false,
		ComparisonLessThanOrEq:             false,
		ComparisonGreaterThan:              false,
		ComparisonGreaterThanEq:            false,
		ComparisonStartsWith:               false,
		ComparisonIsNotNull:                false,
		ComparisonSort:                     false,
		ComparisonDistanceRankLessThan:     false,
		ComparisonDistanceRankLessThanOrEq: false,

		// Java's NONE arm. Go's Merge has no NONE concept — every non-exact-key
		// type becomes an INEQUALITY rather than a residual — which is a separate
		// divergence recorded in TODO.md under the MergeResult residual-list
		// entry. This census covers the EQUALITY boundary only, and on that
		// boundary these agree with Java.
		ComparisonNotEquals:               false,
		ComparisonIn:                      false,
		ComparisonIsDistinctFrom:          false,
		ComparisonLike:                    false,
		ComparisonTextContainsAll:         false,
		ComparisonTextContainsAllWithin:   false,
		ComparisonTextContainsAny:         false,
		ComparisonTextContainsPhrase:      false,
		ComparisonTextContainsPrefix:      false,
		ComparisonTextContainsAllPrefixes: false,
		ComparisonTextContainsAnyPrefix:   false,
	}

	for c := ComparisonEquals; c <= ComparisonDistanceRankLessThanOrEq; c++ {
		if _, listed := want[c]; !listed {
			t.Errorf("ComparisonType %d (%s) is not in this census — classify it against "+
				"Java's ScanComparisons.getComparisonType, and say so here if Go differs",
				int(c), c.Symbol())
			continue
		}
		if got := isScanRangeEqualityType(c); got != want[c] {
			t.Errorf("isScanRangeEqualityType(%s) = %v, want %v", c.Symbol(), got, want[c])
		}
	}
	if len(want) != int(ComparisonDistanceRankLessThanOrEq)+1 {
		t.Fatalf("census holds %d types, the enum spans %d — the loop above cannot see a "+
			"type declared outside the iota run", len(want), int(ComparisonDistanceRankLessThanOrEq)+1)
	}

	// The size of the exact-key set, asserted independently. Every row above
	// compares the table against the code, so a change flipping both the same way
	// satisfies all 24; this does not move with the code under test.
	equalityCount := 0
	for _, isEquality := range want {
		if isEquality {
			equalityCount++
		}
	}
	if equalityCount != 3 {
		t.Errorf("census marks %d types as exact-key, want 3 — Java's EQUALS and IS_NULL "+
			"plus Go's NOT_DISTINCT_FROM extension, and NOT Java's DISTANCE_RANK_EQUALS",
			equalityCount)
	}
}

// TestScanRangeClassification_DistanceRankEqualsStaysATail is the behavioural
// half of the divergence above, stated at the range level so the reason is
// visible from this package rather than only from the executor test that
// actually breaks.
//
// A DistanceRank comparison in a ComparisonRange is malformed by construction in
// Go — DistanceRank is lowered to a RecordQueryVectorIndexPlan — and the
// inequality classification is what makes the scan binder reject it. The
// controls beside it are the two DISTANCE_RANK_LESS_THAN* spellings, which Java
// also classifies as inequalities: they make this an assertion about the
// EQUALITY spelling specifically, not about vector comparisons in general.
func TestScanRangeClassification_DistanceRankEqualsStaysATail(t *testing.T) {
	t.Parallel()

	for _, typ := range []ComparisonType{
		ComparisonDistanceRankEquals,
		ComparisonDistanceRankLessThan,
		ComparisonDistanceRankLessThanOrEq,
	} {
		c := Comparison{Type: typ, Operand: values.LiteralValue(int64(3))}
		res := EmptyComparisonRange().Merge(&c)
		if !res.Ok || res.Range == nil {
			t.Fatalf("%s: merging into an empty range must succeed, got ok=%v", typ.Symbol(), res.Ok)
		}
		if !res.Range.IsInequality() {
			t.Errorf("%s built a %v range, want an INEQUALITY. For DISTANCE_RANK_EQUALS this "+
				"looks like a Java-alignment fix and is a regression: as an equality, "+
				"bindScanComparisonsToRangeSet accepts it against a LONG key with a "+
				"type-compatible operand and returns a live materializer, binding a vector "+
				"distance rank as an ordinary exact tuple key.", typ.Symbol(), res.Range.GetRangeType())
		}
	}

	// Control: the shared-with-Java equality spellings DO build equality ranges,
	// so the assertions above are about these three types and not about Merge
	// having stopped producing equality ranges at all.
	for _, typ := range []ComparisonType{
		ComparisonEquals, ComparisonIsNull, ComparisonNotDistinctFrom,
	} {
		c := Comparison{Type: typ, Operand: values.LiteralValue(int64(3))}
		res := EmptyComparisonRange().Merge(&c)
		if !res.Ok || res.Range == nil || !res.Range.IsEquality() {
			t.Errorf("%s must build an EQUALITY range; ok=%v", typ.Symbol(), res.Ok)
		}
	}
}
