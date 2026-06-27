package predicates

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// ComparisonRange represents a contiguous range of values for a
// single column. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.ComparisonRange`.
//
// A range is one of three types:
//   - Empty: full universe (any value matches).
//   - Equality: a single equals comparison (col = X).
//   - Inequality: a set of one or more inequality comparisons
//     (col > X, col < Y, etc.) defining a contiguous range.
//
// Used by index-pushdown rules: when matching predicates against a
// candidate index, each indexed column gets a ComparisonRange
// derived from the predicate set. The planner then converts the
// list of per-column ranges into an index scan key range.
//
// Range type discipline (mirrors Java):
//   - Adding any comparison to an Empty range produces a non-empty
//     range of the corresponding type.
//   - Adding an equality to an Equality range only succeeds if the
//     two equality values are SAME (otherwise the merge is rejected
//     — the planner knows the predicate set is unsatisfiable).
//   - Adding an inequality to an Equality range is rejected (the
//     planner can't combine equality + inequality on the same col).
//   - Adding any comparison to an Inequality range merges into the
//     existing list.
//
// Returned MergeResult carries either the merged range or a "merge
// failed" indicator with the rejected comparison.
type ComparisonRange struct {
	// rangeType is empty / equality / inequality.
	rangeType ComparisonRangeType
	// equality holds the single equals Comparison when rangeType ==
	// ComparisonRangeEquality. Nil otherwise.
	equality *Comparison
	// inequalities holds the inequality comparisons when rangeType
	// == ComparisonRangeInequality. Nil/empty otherwise.
	inequalities []*Comparison
}

// ComparisonRangeType discriminates the three range shapes.
type ComparisonRangeType int

const (
	// ComparisonRangeEmpty is the universe range — any value matches.
	ComparisonRangeEmpty ComparisonRangeType = iota
	// ComparisonRangeEquality is a single = comparison.
	ComparisonRangeEquality
	// ComparisonRangeInequality is a set of inequalities.
	ComparisonRangeInequality
)

// EmptyComparisonRange returns the universe-range singleton. Each
// call returns a fresh struct so callers can safely mutate via Merge
// without aliasing.
func EmptyComparisonRange() *ComparisonRange {
	return &ComparisonRange{rangeType: ComparisonRangeEmpty}
}

// IsEmpty reports whether the range is the universe.
func (r *ComparisonRange) IsEmpty() bool { return r.rangeType == ComparisonRangeEmpty }

// IsEquality reports whether the range is a single = comparison.
func (r *ComparisonRange) IsEquality() bool { return r.rangeType == ComparisonRangeEquality }

// IsInequality reports whether the range is a set of inequalities.
func (r *ComparisonRange) IsInequality() bool { return r.rangeType == ComparisonRangeInequality }

// GetRangeType returns the discriminator.
func (r *ComparisonRange) GetRangeType() ComparisonRangeType { return r.rangeType }

// GetEqualityComparison returns the single equality comparison.
// Panics if the range isn't an equality range — callers should
// guard with IsEquality first.
func (r *ComparisonRange) GetEqualityComparison() *Comparison {
	if r.rangeType != ComparisonRangeEquality {
		panic("ComparisonRange.GetEqualityComparison: range is not equality")
	}
	return r.equality
}

// GetInequalityComparisons returns the inequality comparison list.
// Panics if the range isn't an inequality range — callers should
// guard with IsInequality first. Returns a read-only slice.
func (r *ComparisonRange) GetInequalityComparisons() []*Comparison {
	if r.rangeType != ComparisonRangeInequality {
		panic("ComparisonRange.GetInequalityComparisons: range is not inequality")
	}
	return r.inequalities
}

// MergeResult carries the outcome of a Merge call.
type MergeResult struct {
	// Range is the merged range when Ok is true.
	Range *ComparisonRange
	// Ok reports merge success.
	Ok bool
	// Residual is the comparison that couldn't be merged when Ok is
	// false. Nil otherwise.
	Residual *Comparison
}

// Merge attempts to add a comparison to the range. Returns a
// MergeResult capturing success / failure.
//
// Rules (per Java):
//   - Empty + EQUALS  → Equality
//   - Empty + INEQ    → Inequality
//   - Equality + EQUALS (same value) → Equality (idempotent)
//   - Equality + EQUALS (different value) → Failed (unsatisfiable)
//   - Equality + INEQ → Failed (planner doesn't combine = + range
//     on the same column)
//   - Inequality + INEQ → Inequality (extended)
//   - Inequality + EQUALS → Failed (similar reasoning)
func (r *ComparisonRange) Merge(c *Comparison) MergeResult {
	if c == nil {
		return MergeResult{Range: r, Ok: true}
	}
	switch r.rangeType {
	case ComparisonRangeEmpty:
		if c.Type == ComparisonEquals || c.Type == ComparisonIsNull {
			// IS NULL is an EQUALITY range on the NULL value, matching Java's
			// ScanComparisons.getComparisonType(IS_NULL) == EQUALITY: the index
			// scan seeks the single [null] key. (IS NOT NULL stays an
			// inequality — the (null, +inf) range — handled below.)
			return MergeResult{
				Range: &ComparisonRange{
					rangeType: ComparisonRangeEquality,
					equality:  c,
				},
				Ok: true,
			}
		}
		// All non-equals (including IsNotNull / Not) are inequalities
		// for range purposes — they restrict the universe.
		return MergeResult{
			Range: &ComparisonRange{
				rangeType:    ComparisonRangeInequality,
				inequalities: []*Comparison{c},
			},
			Ok: true,
		}
	case ComparisonRangeEquality:
		if c.Type != ComparisonEquals {
			return MergeResult{Ok: false, Residual: c}
		}
		// Two equality comparisons must agree. The seed compares via
		// the wrapped Operand's ExplainValue (structural string
		// match) — no alias context needed because operands at this
		// stage are typically literals.
		if r.equality == nil || !comparisonsEqualValue(r.equality, c) {
			return MergeResult{Ok: false, Residual: c}
		}
		return MergeResult{Range: r, Ok: true}
	case ComparisonRangeInequality:
		if c.Type == ComparisonEquals {
			return MergeResult{Ok: false, Residual: c}
		}
		merged := &ComparisonRange{
			rangeType:    ComparisonRangeInequality,
			inequalities: append([]*Comparison(nil), r.inequalities...),
		}
		merged.inequalities = append(merged.inequalities, c)
		return MergeResult{Range: merged, Ok: true}
	}
	return MergeResult{Ok: false, Residual: c}
}

// comparisonsEqualValue compares two equality Comparisons via their
// Operand's structural rendering (values.ExplainValue). Returns true
// if the values match. Used by Merge's equality-vs-equality check.
//
// Conformance trade-off: ExplainValue rendering is fragile for non-
// literal Operands (float formatting, alias names in sub-expressions
// can cause text drift between structurally-equal Values). The
// rendering-based comparison degrades to FALSE on uncertain
// equality — never to TRUE. That means Merge() rejects ambiguous
// equality merges (planner falls back to keeping both predicates as
// residual filters); it does NOT silently accept a wrong merge
// (which would drop a predicate). Sound for SQL semantics; only
// cost (extra residual filter) is at risk on non-literal Operand
// rendering drift. The seed's Merge callers pass literal Operands
// only, so the renderable-string contract is reliable in practice.
func comparisonsEqualValue(a, b *Comparison) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Type != b.Type {
		return false
	}
	return values.ValuesStructurallyEqual(a.Operand, b.Operand)
}
