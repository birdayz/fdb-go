package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// R2's SECOND route into a key column, and the one the strict-ordering consumer
// actually runs on.
//
// The filter route (null_rejecting_conjuncts.go) reads a predicate that will be
// EVALUATED per row, where three-valued semantics does all the work. An index
// scan's ComparisonRange is a different kind of object: it is a PHYSICAL KEY
// ENCLOSURE, chosen at plan time and turned into FDB key bounds by the binder.
// "This predicate rejects NULL" and "this range excludes the NULL boundary" are
// therefore separate claims, and reading the second off the first is exactly the
// mistake this file exists to not make — comparison_range.go:196-203 classifies
// ComparisonEquals, ComparisonIsNull and ComparisonNotDistinctFrom ALIKE as
// scan-range equality types, and two of those three enclose the NULL boundary
// (bindScanComparisonsToRangeSetWithTerminalWidening gives each of them
// `component.alternatives = []any{nil}` at scan_range_binding.go:303 and :327).
//
// The route matters because it is the only one that fires on the shape RFC-210
// §5.7 quotes. `ORDER BY email WHERE email IS NOT NULL` does not leave a
// residual filter: IS NOT NULL is admitted by isSargableComparisonForMatch
// (match_max_match_map.go:67-81), so the planner pushes it INTO the scan range
// and the filter route sees nothing. Without this file R2-for-ordering would be
// live code that no SQL query reaches.
//
// EVIDENCE THAT EACH ADMITTED KIND EXCLUDES THE NULL BOUNDARY. The binder is
// the authority; every citation is to pkg/recordlayer/query/executor/
// scan_range_binding.go, and each is pinned by a test in
// null_rejecting_scan_range_test.go so a change to the binder cannot silently
// invalidate the claim.
//
//   - EQUALITY over a NULL operand — a literal NULL, or a parameter bound to
//     NULL, which reaches the binder as an operand evaluating to nil. This is
//     the shape the hazard was recorded against, and it does NOT enclose the
//     NULL boundary: the binder returns `bound.empty = true` (:321-325), which
//     yields a zero-leaf cursor and reads nothing from FDB. Zero rows have no
//     ties, so the ordering claim over them is vacuously true. The planner
//     independently reaches the same conclusion — PhysicalEqualityShapeForComponent
//     returns Unsatisfiable for a constant-NULL Equals (physical_equality_shape.go:
//     345-354) — but the binder's guard is the load-bearing one, because a
//     parameter's value is not constant-foldable at plan time.
//   - EQUALITY over a non-NULL operand — a singleton range at that value, which
//     is above the NULL boundary in tuple order.
//   - GREATER_THAN / GREATER_THAN_OR_EQUALS — the low bound IS the comparand,
//     which is non-NULL (a NULL one returns boundRangeTailEmpty at :981-982).
//   - LESS_THAN / LESS_THAN_OR_EQUALS — the interesting one, because a bare
//     upper bound could plausibly start the range at the bottom of the key
//     space and sweep the NULL entries in. It does not: both arms explicitly
//     install an EXCLUSIVE low at the NULL boundary when none exists
//     (`tail.lowIsNullBoundary = true`, :1069-1073 and :1082-1086). That is
//     Java's Range.greaterThan(NULL_boundary) (RangeConstraints.java:650-653)
//     arrived at from the other side.
//   - IS_NOT_NULL — the same `lowIsNullBoundary` install (:1088-1093); the
//     direct form.
//   - STARTS_WITH — a STRING prefix range, above the NULL boundary, and empty
//     on a NULL operand (:936-938).
//
// IN, LIKE and NOT_EQUALS are on the filter route's allow-list but are not
// scan-range operators at all (they are in neither isScanRangeCompatible nor
// isSargableComparisonForMatch), so they can only ever arrive as residual
// filters. They are admitted here anyway rather than special-cased out: the
// enumeration's authority is nullRejectingComparison, and a kind that cannot
// reach a scan range contributes nothing by never appearing.
//
// The two REFUSED kinds are the whole point, and they are refused by
// nullRejectingComparison rather than by a second list here — IS NULL and
// IS NOT DISTINCT FROM are exactly the scan-range equality types that DO
// enclose the NULL boundary, and they are exactly the two the filter route
// already refuses. One allow-list, two consumers.
func nullRejectedByScanRange(
	comparisons []*predicates.ComparisonRange, keyColumnCount int,
) []bool {
	if keyColumnCount <= 0 {
		return nil
	}
	rejected := make([]bool, keyColumnCount)
	for i := 0; i < keyColumnCount; i++ {
		if i >= len(comparisons) {
			break
		}
		rejected[i] = comparisonRangeExcludesNull(comparisons[i])
	}
	return rejected
}

// comparisonRangeExcludesNull reports whether every key this range encloses is
// above the NULL boundary.
//
// An UNCONSTRAINED coordinate (nil, or the empty range every key column past
// the first inequality carries) says nothing and yields false — which is the
// fail-closed direction, because the caller's proof is a conjunction over key
// columns and a false position declines it.
//
// An inequality range holds SEVERAL comparisons on one coordinate, and the test
// is over ALL of them rather than over any one: a range is a conjunction, so an
// unrecognised member could only ever WIDEN what the range encloses. Requiring
// every member to be independently NULL-excluding is stricter than necessary
// (one NULL-excluding member already bounds the whole intersection from below)
// and is the direction that stays correct if a future member type is added that
// this file has not reasoned about.
func comparisonRangeExcludesNull(cr *predicates.ComparisonRange) bool {
	if cr == nil || cr.IsEmpty() {
		return false
	}
	if cr.IsEquality() {
		equality := cr.GetEqualityComparison()
		return equality != nil && nullRejectingComparison(equality.Type)
	}
	if !cr.IsInequality() {
		return false
	}
	inequalities := cr.GetInequalityComparisons()
	if len(inequalities) == 0 {
		return false
	}
	for _, inequality := range inequalities {
		if inequality == nil || !nullRejectingComparison(inequality.Type) {
			return false
		}
	}
	return true
}
