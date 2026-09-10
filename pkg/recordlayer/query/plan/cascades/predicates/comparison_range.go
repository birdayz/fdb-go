package predicates

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
// Range type discipline (Java's ComparisonRange.merge, arm for arm):
//   - A NONE-type comparison (NOT_EQUALS, IN, LIKE, TEXT_*, …) never enters a
//     range; it comes back as a residual and the range is untouched.
//   - Adding any scan-range comparison to an Empty range produces a
//     non-empty range of the corresponding type.
//   - Adding an equality to an Equality range is a no-op when the two
//     are the same comparison, and leaves the incoming one as a residual
//     when they differ (the planner scans the first and re-checks the
//     second; a contradiction reads zero rows).
//   - Adding an inequality to an Equality range leaves it as a residual.
//   - Adding an inequality to an Inequality range appends it, unless it
//     is already present.
//   - Adding an equality to an Inequality range makes the equality the
//     range and every accumulated inequality a residual.
//
// The merge is TOTAL: it never fails. MergeResult carries the range plus
// the residual comparisons, and a caller that cannot carry residuals reads
// len(Residuals) > 0 as its rejection.
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

// MergeResult carries the outcome of a Merge call: the merged range and the
// comparisons that could not be pushed into it. Java's
// ComparisonRange.MergeResult.
type MergeResult struct {
	// Range is the merged range. Never nil.
	Range *ComparisonRange
	// Residuals are the comparisons the range could not carry, in the order
	// they were offered. Empty when everything merged.
	Residuals []*Comparison
}

// Complete reports whether every offered comparison merged into the range.
func (m MergeResult) Complete() bool { return len(m.Residuals) == 0 }

// GetComparisons returns every Comparison this range carries, whichever shape
// it has: the single equality, or all the inequalities. It returns an empty
// slice for an empty or nil range. The returned slice is read-only. Callers
// that need to inspect the range as a whole should use this rather than
// branching on the range type and risking one of the shapes going unhandled.
func (r *ComparisonRange) GetComparisons() []*Comparison {
	if r == nil {
		return nil
	}
	switch r.rangeType {
	case ComparisonRangeEquality:
		if r.equality == nil {
			return nil
		}
		return []*Comparison{r.equality}
	case ComparisonRangeInequality:
		return r.inequalities
	default:
		return nil
	}
}

// Merge adds a comparison to the range. Total: the outcome is always a
// range plus the residual comparisons that did not fit — see the type
// comment for the arm table (Java's ComparisonRange.merge(Comparison)).
func (r *ComparisonRange) Merge(c *Comparison) MergeResult {
	if c == nil {
		return MergeResult{Range: r}
	}
	kind := scanRangeComparisonType(c.Type)
	if kind == scanRangeNone {
		return MergeResult{Range: r, Residuals: []*Comparison{c}}
	}
	switch r.rangeType {
	case ComparisonRangeEmpty:
		if kind == scanRangeEquality {
			return MergeResult{Range: &ComparisonRange{
				rangeType: ComparisonRangeEquality,
				equality:  c,
			}}
		}
		return MergeResult{Range: &ComparisonRange{
			rangeType:    ComparisonRangeInequality,
			inequalities: []*Comparison{c},
		}}
	case ComparisonRangeEquality:
		if kind == scanRangeEquality && r.equality != nil && comparisonsEqualValue(r.equality, c) {
			return MergeResult{Range: r}
		}
		return MergeResult{Range: r, Residuals: []*Comparison{c}}
	case ComparisonRangeInequality:
		if kind == scanRangeEquality {
			return MergeResult{
				Range:     &ComparisonRange{rangeType: ComparisonRangeEquality, equality: c},
				Residuals: append([]*Comparison(nil), r.inequalities...),
			}
		}
		for _, existing := range r.inequalities {
			if comparisonsEqualValue(existing, c) {
				return MergeResult{Range: r}
			}
		}
		merged := &ComparisonRange{
			rangeType:    ComparisonRangeInequality,
			inequalities: append(append([]*Comparison(nil), r.inequalities...), c),
		}
		return MergeResult{Range: merged}
	}
	return MergeResult{Range: r, Residuals: []*Comparison{c}}
}

// MergeRange merges every comparison another range carries into this one,
// accumulating the residuals — Java's ComparisonRange.merge(ComparisonRange).
func (r *ComparisonRange) MergeRange(other *ComparisonRange) MergeResult {
	result := MergeResult{Range: r}
	if other == nil {
		return result
	}
	for _, c := range other.GetComparisons() {
		next := result.Range.Merge(c)
		result.Range = next.Range
		result.Residuals = append(result.Residuals, next.Residuals...)
	}
	return result
}

// MergeAll folds a list of comparisons into one range from the empty range,
// accumulating the residuals — Java's ComparisonRange.mergeAll.
func MergeAll(comparisons []*Comparison) MergeResult {
	result := MergeResult{Range: EmptyComparisonRange()}
	for _, c := range comparisons {
		next := result.Range.Merge(c)
		result.Range = next.Range
		result.Residuals = append(result.Residuals, next.Residuals...)
	}
	return result
}

// scanRangeComparisonKind is a comparison's role in a scan range: an exact
// key, an ordered bound, or neither. Java's ScanComparisons.ComparisonType.
type scanRangeComparisonKind int

const (
	scanRangeNone scanRangeComparisonKind = iota
	scanRangeEquality
	scanRangeInequality
)

// scanRangeComparisonType classifies a comparison type for range merging —
// Java's ScanComparisons.getComparisonType, with the two documented Go
// differences kept (see isScanRangeEqualityType): NOT_DISTINCT_FROM is an
// exact key, DISTANCE_RANK_EQUALS is an ordered bound. Everything Java's
// switch sends to `default: NONE` is none here too, so it never enters a
// range and always comes back as a residual.
func scanRangeComparisonType(t ComparisonType) scanRangeComparisonKind {
	if isScanRangeEqualityType(t) {
		return scanRangeEquality
	}
	switch t {
	case ComparisonLessThan, ComparisonLessThanOrEq,
		ComparisonGreaterThan, ComparisonGreaterThanEq,
		ComparisonStartsWith, ComparisonIsNotNull, ComparisonSort,
		ComparisonDistanceRankEquals, ComparisonDistanceRankLessThan,
		ComparisonDistanceRankLessThanOrEq:
		return scanRangeInequality
	default:
		return scanRangeNone
	}
}

// isScanRangeEqualityType reports whether a comparison binds an EXACT KEY in a
// scan range rather than an ordered bound. It corresponds to the EQUALITY arm of
// Java's ScanComparisons.getComparisonType, and DELIBERATELY DIFFERS FROM IT IN
// BOTH DIRECTIONS. Do not "align" it without reading why.
//
// Java's arm is {EQUALS, IS_NULL, DISTANCE_RANK_EQUALS}.
//
// DISTANCE_RANK_EQUALS is omitted here, and adding it is an ACTIVE REGRESSION,
// not a missing port. Java's scan machinery consumes vector comparisons through
// this classification; Go does not — a DistanceRank comparison is lowered to a
// RecordQueryVectorIndexPlan, and bindScanComparisonsToRangeSet binds TUPLE keys
// with no vector handling at all. So a DistanceRank comparison arriving in a
// ComparisonRange is malformed by construction, and the inequality
// classification is what makes the binder reject it as a malformed tail.
//
// Measured: classify it as an equality and the binder stops rejecting it. With a
// type-compatible operand (int64 against a LONG key) bindScanComparisonsToRangeSet
// returns a nil error AND a live materializer — a vector distance-rank bound as
// an ordinary exact tuple key, silently. With a float operand the rejection
// survives only incidentally, as a comparand-type error rather than a shape one.
// TestBindScanComparisonsToRangeSet_RejectsMalformedTailBeforeProjection in
// pkg/recordlayer/query/executor is what catches this; its skip list mirrors the
// set below, so the two must be changed together or not at all.
//
// NOT_DISTINCT_FROM is added here and is NOT a Java case — Java's switch has no
// arm for it, so it falls to `default: NONE`. Sound because a null-safe equality
// seeks one exact key: the value's, or the null key when the operand is null.
// See Merge's IS NULL comment.
func isScanRangeEqualityType(comparisonType ComparisonType) bool {
	switch comparisonType {
	case ComparisonEquals, ComparisonIsNull, ComparisonNotDistinctFrom:
		return true
	default:
		return false
	}
}

// comparisonsEqualValue reports whether two Comparisons are the same
// comparison — every identity-bearing field, through comparisonIdentityEqual,
// so a parameter-bound `= ?p` and a literal `= 7` over the same operand are
// different comparisons. Merge uses it for the two dedup arms —
// equality-vs-equality and an inequality already present — where Java uses
// Comparison.equals.
func comparisonsEqualValue(a, b *Comparison) bool {
	if a == nil || b == nil {
		return false
	}
	return comparisonIdentityEqual(*a, *b)
}
