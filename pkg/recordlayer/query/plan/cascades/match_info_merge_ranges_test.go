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
// Go's merge IS that total merge now (predicates.MergeResult carries the
// residual list, and the same-quantifier fold — foldPlaceholderBindings —
// carries those residuals as filter predicates). What remains divergent is
// THIS caller: mergeComparisonRanges merges the ranges two CHILD BRANCHES
// bound to one alias, and a PartialMatch has no channel for a residual that
// belongs to a sibling branch, so it fails closed on a non-empty residual list
// and tryMergeParameterBindings turns that into a lost match — the index
// candidate Java would keep (an equality seek plus a residual filter) is not
// produced for the cross-quantifier case. The arms below assert the rejection
// AND that the underlying merge produced exactly Java's range and residuals,
// so the rejection is visibly a carrying problem, not a merging one.
//
// WHEN THAT IS FIXED, this test must be REPLACED, not deleted: assert that the
// merge SUCCEEDS, that the surviving range is the EQUALITY, and that the
// inequality comes back as a residual carried by the match. A green from
// deleting it would mean the arm went untested again.
func TestMergeComparisonRanges_EqualityInequalityRejectsUnlikeJava(t *testing.T) {
	t.Parallel()

	eq7 := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	eq8 := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(8))
	gt5 := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))

	for _, tc := range []struct {
		name         string
		left, right  *predicates.ComparisonRange
		java         string
		wantRange    *predicates.Comparison
		wantResidual *predicates.Comparison
	}{
		{
			"equality then inequality", rangeOf(t, eq7), rangeOf(t, gt5),
			"keeps the equality range and residualises > 5", &eq7, &gt5,
		},
		{
			"inequality then equality", rangeOf(t, gt5), rangeOf(t, eq7),
			"replaces the range with the equality and residualises > 5", &eq7, &gt5,
		},
		{
			"conflicting equalities", rangeOf(t, eq7), rangeOf(t, eq8),
			"keeps the first equality range and residualises the second", &eq7, &eq8,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			merged, ok := mergeComparisonRanges(tc.left, tc.right)
			if ok {
				t.Fatalf("this arm is expected to REJECT today; it now returns ok=true with "+
					"range %v. If residuals are now carried across quantifier boundaries, "+
					"replace this test with one asserting the equality survives and the rest "+
					"comes back as a residual — do not delete it. Java %s.", merged, tc.java)
			}
			if merged != nil {
				t.Errorf("a rejecting merge must return a nil range, got %v", merged)
			}
			// The merge itself is Java's: the equality wins and the other
			// comparison is the residual this caller cannot carry.
			total := tc.left.MergeRange(tc.right)
			got := total.Range.GetComparisons()
			if !total.Range.IsEquality() || len(got) != 1 || !comparisonsEqual(got[0], tc.wantRange) {
				t.Fatalf("MergeRange range = %v, want the equality %v", got, tc.wantRange.Operand)
			}
			if len(total.Residuals) != 1 || !comparisonsEqual(total.Residuals[0], tc.wantResidual) {
				t.Fatalf("MergeRange residuals = %v, want exactly %v", total.Residuals, tc.wantResidual.Operand)
			}
		})
	}
}
