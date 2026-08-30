package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// TestMergeComparisonRanges_AgreeingAndEmptyArms covers the cases where Go and
// Java agree. mergeComparisonRanges had NO test of any kind before this file —
// it is the intersection step tryMergeParameterBindings runs when two child
// match branches bind the same parameter alias, so an untested arm here is an
// untested decision about whether an index candidate survives.
func TestMergeComparisonRanges_AgreeingAndEmptyArms(t *testing.T) {
	t.Parallel()

	eq7 := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	gt5 := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))
	lt9 := predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(9))

	t.Run("nil operand rejects", func(t *testing.T) {
		t.Parallel()
		if _, ok := mergeComparisonRanges(nil, rangeOf(t, eq7)); ok {
			t.Error("a nil left range must reject rather than pass the right through")
		}
		if _, ok := mergeComparisonRanges(rangeOf(t, eq7), nil); ok {
			t.Error("a nil right range must reject")
		}
	})

	t.Run("identical ranges collapse", func(t *testing.T) {
		t.Parallel()
		merged, ok := mergeComparisonRanges(rangeOf(t, eq7), rangeOf(t, eq7))
		if !ok || merged == nil || !merged.IsEquality() {
			t.Fatalf("two identical equality ranges must merge to that equality, got ok=%v", ok)
		}
	})

	t.Run("empty is the identity on both sides", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name         string
			left, right  *predicates.ComparisonRange
			wantEquality bool
		}{
			{"empty left", predicates.EmptyComparisonRange(), rangeOf(t, eq7), true},
			{"empty right", rangeOf(t, eq7), predicates.EmptyComparisonRange(), true},
			{"empty left, inequality right", predicates.EmptyComparisonRange(), rangeOf(t, gt5), false},
		} {
			merged, ok := mergeComparisonRanges(tc.left, tc.right)
			if !ok || merged == nil {
				t.Errorf("%s: must merge, got ok=%v", tc.name, ok)
				continue
			}
			if merged.IsEquality() != tc.wantEquality {
				t.Errorf("%s: equality=%v, want %v", tc.name, merged.IsEquality(), tc.wantEquality)
			}
		}
	})

	t.Run("inequalities union and dedup", func(t *testing.T) {
		t.Parallel()
		merged, ok := mergeComparisonRanges(rangeOf(t, gt5), rangeOf(t, lt9))
		if !ok || merged == nil || !merged.IsInequality() {
			t.Fatalf("two inequality ranges must union, got ok=%v", ok)
		}
		if got := len(merged.GetInequalityComparisons()); got != 2 {
			t.Errorf("union holds %d comparisons, want 2 (> 5 and < 9)", got)
		}

		// The dedup lives in mergeComparisonRanges itself, NOT in
		// ComparisonRange.Merge — Merge appends unconditionally where Java's
		// merge checks `inequalityComparisons.contains(comparison)` first. This
		// arm is what keeps that difference invisible, so it is worth stating:
		// remove the dedup loop above mergeComparisonRanges' rebuild and this
		// count becomes 3.
		merged, ok = mergeComparisonRanges(rangeOf(t, gt5, lt9), rangeOf(t, gt5))
		if !ok || merged == nil {
			t.Fatalf("a range containing a duplicate must still merge, got ok=%v", ok)
		}
		if got := len(merged.GetInequalityComparisons()); got != 2 {
			t.Errorf("overlapping union holds %d comparisons, want 2 — the duplicate > 5 "+
				"must not be carried twice", got)
		}
	})
}

// TestMergeComparisonRanges_EqualityInequalityRejectsUnlikeJava pins a KNOWN
// DIVERGENCE, deliberately, so that closing it is a visible test change rather
// than a silent behaviour shift.
//
// Java's ComparisonRange.merge is TOTAL: it never fails. Given an inequality
// range and an incoming equality it returns `MergeResult.of(from(comparison),
// inequalityComparisons)` — the range becomes the EQUALITY and the inequalities
// come back as RESIDUAL comparisons for the caller to apply as a filter. The
// symmetric case keeps the equality range and residualises the inequality.
// Equality always wins, and nothing is ever dropped.
//
// Go cannot express that. predicates.MergeResult carries `Ok bool` and a single
// `Residual`, where Java carries a residual LIST, and no caller in the tree
// reads Residual at all. So mergeComparisonRanges rejects instead, and
// tryMergeParameterBindings turns the rejection into a lost match: the index
// candidate that Java would keep — an equality seek plus a residual filter —
// is not produced.
//
// mergeComparisonRanges says as much in its own comment ("equality/inequality
// is not representable by ComparisonRange without a residual"), which is the
// admission that the residual list is the real answer.
//
// WHEN THAT IS FIXED, this test must be REPLACED, not deleted: assert that the
// merge SUCCEEDS, that the surviving range is the EQUALITY, and that the
// inequality comes back as a residual. A green from deleting it would mean the
// arm went untested again.
func TestMergeComparisonRanges_EqualityInequalityRejectsUnlikeJava(t *testing.T) {
	t.Parallel()

	eq7 := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	eq8 := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(8))
	gt5 := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))

	for _, tc := range []struct {
		name        string
		left, right *predicates.ComparisonRange
		java        string
	}{
		{
			"equality then inequality", rangeOf(t, eq7), rangeOf(t, gt5),
			"keeps the equality range and residualises > 5",
		},
		{
			"inequality then equality", rangeOf(t, gt5), rangeOf(t, eq7),
			"replaces the range with the equality and residualises > 5",
		},
		{
			"conflicting equalities", rangeOf(t, eq7), rangeOf(t, eq8),
			"keeps the first equality range and residualises the second",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			merged, ok := mergeComparisonRanges(tc.left, tc.right)
			if ok {
				t.Fatalf("this arm is expected to REJECT today; it now returns ok=true with "+
					"range %v. If the residual port landed, replace this test with one asserting "+
					"the equality survives and the rest comes back as a residual — do not delete "+
					"it. Java %s.", merged, tc.java)
			}
			if merged != nil {
				t.Errorf("a rejecting merge must return a nil range, got %v", merged)
			}
		})
	}
}
