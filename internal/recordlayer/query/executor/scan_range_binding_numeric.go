package executor

import (
	"math"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer"
)

// projectRuntimeFloatComparisonToIntegerDomain applies the inverse-domain rule
// only when both sides of the impedance mismatch are established at runtime:
// the physical key metadata is authoritative INT/LONG and the evaluated
// comparand is a native float32/float64. Ordinary integer parameters therefore
// retain their exact integer probe, and an Unknown physical key is never
// guessed to be integer merely from the RHS value.
func projectRuntimeFloatComparisonToIntegerDomain(
	value any,
	physicalType values.Type,
	comparisonType predicates.ComparisonType,
) (integerDomainProjection, bool) {
	switch comparisonType {
	case predicates.ComparisonEquals, predicates.ComparisonNotDistinctFrom,
		predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
		// These are exactly the operators whose inverse is one integer interval.
	default:
		return integerDomainProjection{}, false
	}

	var floating float64
	switch number := value.(type) {
	case float32:
		floating = float64(number)
	case float64:
		floating = number
	default:
		return integerDomainProjection{}, false
	}

	domainLow, domainHigh, integerPhysical := physicalIntegerDomain(physicalType)
	if !integerPhysical {
		return integerDomainProjection{}, false
	}
	return projectFloatComparisonToIntegerDomain(
		floating, domainLow, domainHigh, comparisonType,
	), true
}

func physicalIntegerDomain(physicalType values.Type) (int64, int64, bool) {
	if physicalType == nil {
		return 0, 0, false
	}
	switch physicalType.Code() {
	case values.TypeCodeInt, values.TypeCodeLong:
		// TypeCode selects the tuple carrier, not a recoverable protobuf
		// source domain. INT includes uint32/fixed32 fields, whose values above
		// MaxInt32 are stored as int64. LONG includes uint64/fixed64 fields;
		// extraction casts their raw bits to int64, so values above MaxInt64
		// occupy the negative half of the same signed tuple domain. With the
		// source field kind erased, the only sound authoritative domain for
		// either integer tuple carrier is therefore all int64 values.
		return math.MinInt64, math.MaxInt64, true
	default:
		return 0, 0, false
	}
}

// projectFloatComparisonToIntegerDomain returns the exact set of integers n in
// [domainLow, domainHigh] for which cmpAny(n, floating) satisfies
// comparisonType. cmpAny's mixed numeric path first converts n to float64,
// checks IEEE equality (so both zero signs equal integer zero), and otherwise
// falls back to values.CompareFloat64 (where NaN sorts greatest). That mapping
// is monotone, so two binary searches find the complete inverse without an
// imprecise float-to-int cast or an overflow-prone ceil/floor calculation.
//
// The helper intentionally accepts explicit domain bounds so its algebra can
// be exhaustively tested over small domains as well as at INT32/INT64 extrema.
func projectFloatComparisonToIntegerDomain(
	floating float64,
	domainLow int64,
	domainHigh int64,
	comparisonType predicates.ComparisonType,
) integerDomainProjection {
	if domainLow > domainHigh {
		return integerDomainProjection{empty: true}
	}

	firstGreaterOrEqual, hasGreaterOrEqual := firstIntegerInDomain(
		domainLow, domainHigh,
		func(integer int64) bool {
			return compareIntegerToFloat(integer, floating) >= 0
		},
	)
	firstGreater, hasGreater := firstIntegerInDomain(
		domainLow, domainHigh,
		func(integer int64) bool {
			return compareIntegerToFloat(integer, floating) > 0
		},
	)

	switch comparisonType {
	case predicates.ComparisonEquals, predicates.ComparisonNotDistinctFrom:
		if !hasGreaterOrEqual ||
			compareIntegerToFloat(firstGreaterOrEqual, floating) != 0 {
			return integerDomainProjection{empty: true}
		}
		high := domainHigh
		if hasGreater {
			high = firstGreater - 1
		}
		return integerDomainProjection{low: firstGreaterOrEqual, high: high}

	case predicates.ComparisonLessThan:
		if !hasGreaterOrEqual {
			return integerDomainProjection{low: domainLow, high: domainHigh}
		}
		if firstGreaterOrEqual == domainLow {
			return integerDomainProjection{empty: true}
		}
		return integerDomainProjection{low: domainLow, high: firstGreaterOrEqual - 1}

	case predicates.ComparisonLessThanOrEq:
		if !hasGreater {
			return integerDomainProjection{low: domainLow, high: domainHigh}
		}
		if firstGreater == domainLow {
			return integerDomainProjection{empty: true}
		}
		return integerDomainProjection{low: domainLow, high: firstGreater - 1}

	case predicates.ComparisonGreaterThan:
		if !hasGreater {
			return integerDomainProjection{empty: true}
		}
		return integerDomainProjection{low: firstGreater, high: domainHigh}

	case predicates.ComparisonGreaterThanEq:
		if !hasGreaterOrEqual {
			return integerDomainProjection{empty: true}
		}
		return integerDomainProjection{low: firstGreaterOrEqual, high: domainHigh}

	default:
		return integerDomainProjection{empty: true}
	}
}

// firstIntegerInDomain returns the first signed integer satisfying a monotone
// predicate. Flipping the sign bit maps signed order onto uint64 order, which
// lets the search cover the entire [MinInt64, MaxInt64] domain without an
// overflowing signed midpoint or a domain-size conversion to int.
func firstIntegerInDomain(
	domainLow int64,
	domainHigh int64,
	predicate func(int64) bool,
) (int64, bool) {
	const signBit = uint64(1) << 63
	lowRank := uint64(domainLow) ^ signBit
	highRank := uint64(domainHigh) ^ signBit
	for lowRank < highRank {
		middleRank := lowRank + (highRank-lowRank)/2
		middle := int64(middleRank ^ signBit)
		if predicate(middle) {
			highRank = middleRank
		} else {
			lowRank = middleRank + 1
		}
	}
	result := int64(lowRank ^ signBit)
	if !predicate(result) {
		return 0, false
	}
	return result, true
}

// compareIntegerToFloat is the mixed numeric arm of predicates.cmpAny for an
// integer row value and a floating comparand. Keep the native equality check:
// it is what makes integer zero equal both IEEE zero signs before the total
// ordering fallback distinguishes them.
func compareIntegerToFloat(integer int64, floating float64) int {
	integerAsFloat := float64(integer)
	if integerAsFloat == floating {
		return 0
	}
	return values.CompareFloat64(integerAsFloat, floating)
}

func isNaNFloatBound(value any) bool {
	switch number := value.(type) {
	case float32:
		return math.IsNaN(float64(number))
	case float64:
		return math.IsNaN(number)
	default:
		return false
	}
}

// Physical tuple order for a FLOAT/DOUBLE key coordinate is IEEE total order,
// measured against pkg/fdbgo/fdb/tuple and pinned by
// TestFloatKeyTupleOrderIsIEEETotalOrder:
//
//	negNaN < -Inf < negative finite < -0.0 < +0.0 < positive finite < +Inf < posNaN
//
// The LOGICAL comparator (values.CompareFloat64, and Java's) instead treats
// every NaN as one value that is GREATEST, and -0.0 as equal to +0.0. The two
// orders therefore disagree in exactly one place: the negative-NaN region,
// which sits below -Inf physically and above +Inf logically.
//
// That single disagreement is why an ordered comparison on a raw float key is
// not one contiguous physical range — but it is always a SMALL, exactly
// representable set of them, because the disagreeing region is a contiguous
// block at each end of the coordinate's domain.
//
// floatWireLowestNaN is the lowest encoding of the coordinate's carrier: every
// negative NaN lies in [lowest, -Inf), and every positive NaN symmetrically in
// (+Inf, highest].
//
// Only the LOW extreme needs naming. The two NaN blocks are not treated alike
// because the ranges that must reach them are not alike: the negative block is
// physically BELOW -Inf, so an open-low `> x` range cannot reach it and needs a
// second, explicit [lowestNaN, -Inf) range; the positive block sits at the very
// top of the coordinate's domain, which an open-high range already covers by
// running to the end of the subspace. A `floatWireHighestNaN` terminator would
// be redundant with that end, never an additional one, so the asymmetry here is
// the decomposition being exact rather than a missing case.
//
// That reachability is ASSERTED, not merely argued: TestOrderedFloatRangeSet-
// SelectsExactlyTheLogicalRows materializes the ranges and checks physical
// selection against the predicate evaluator over a domain carrying both
// positive-NaN encodings. Bounding the open-high range at +Inf makes it report
// "MISSES a qualifying key (silent data loss)" for every `>`/`>=` threshold. So
// if the encoding ever stops putting positive NaNs above +Inf, this asymmetry
// fails loudly there rather than silently dropping rows here.
func floatWireLowestNaN(physicalType values.Type) any {
	if physicalType != nil && physicalType.Code() == values.TypeCodeFloat {
		return math.Float32frombits(0xFFFFFFFF)
	}
	return math.Float64frombits(0xFFFFFFFFFFFFFFFF)
}

// nanBlockTails are the two key ranges that hold every NaN of a float
// coordinate: the negative NaNs, [lowest, -Inf), and the positive NaNs,
// (+Inf, end]. Neither bound depends on the probing NaN's payload, so a
// resume under another payload is the same scan. Reverse scans read the
// positive block first, in physical order.
func nanBlockTails(physicalType values.Type, reverse bool) []boundRangeTail {
	negative := boundRangeTail{
		kind:         boundRangeTailInequality,
		floatOrdered: true,
		hasLow:       true,
		lowItem:      floatWireLowestNaN(physicalType),
		lowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
		hasHigh:      true,
		highItem:     floatWireInfinity(physicalType, -1),
		highEndpoint: recordlayer.EndpointTypeRangeExclusive,
	}
	positive := boundRangeTail{
		kind:         boundRangeTailInequality,
		floatOrdered: true,
		hasLow:       true,
		lowItem:      floatWireInfinity(physicalType, 1),
		lowEndpoint:  recordlayer.EndpointTypeRangeExclusive,
	}
	if reverse {
		return []boundRangeTail{positive, negative}
	}
	return []boundRangeTail{negative, positive}
}

func floatWireInfinity(physicalType values.Type, sign int) any {
	if physicalType != nil && physicalType.Code() == values.TypeCodeFloat {
		return float32(math.Inf(sign))
	}
	return math.Inf(sign)
}

// decomposeOrderedFloatTail turns one logical ordered comparison on a float key
// into the ordered, disjoint physical ranges that select EXACTLY its logical
// row set. It is the access-path half of RFC-208: the same range-set machinery
// that lets a zero equality keep its suffix lets an inequality keep its index.
//
// The returned ranges are in LOGICAL order for the scan direction, because the
// range-set cursor concatenates them in the order given and a consumer may be
// relying on the scan's ordering.
//
//   - `< x` / `<= x`: ONE range. No NaN satisfies it (NaN is logically
//     greatest), and every non-NaN value from -Inf up is physically contiguous.
//     The only correction needed is the LOW endpoint: it must start at -Inf,
//     not at the coordinate's type start, or the range would sweep in the
//     negative-NaN block that sorts below -Inf.
//   - `> x` / `>= x`: TWO ranges. (x, END] already covers the finite tail,
//     +Inf and the positive NaNs; the negative NaNs qualify logically (all NaNs
//     are greatest) but sit at the very bottom physically, so they need their
//     own range.
//   - two-sided (`BETWEEN`, or both bounds present): ONE range, unchanged. No
//     NaN is logically inside a finite two-sided interval, and the range as
//     built already excludes both NaN blocks.
//
// A tail whose only bound is the exclusive-NULL boundary (IS NOT NULL) is
// likewise unchanged: it is the exact non-null set for every carrier, NaNs
// included.
func decomposeOrderedFloatTail(
	tail boundRangeTail,
	physicalType values.Type,
	reverse bool,
) []boundRangeTail {
	if tail.kind != boundRangeTailInequality || !tail.floatOrdered ||
		!scanBoundUsesFloatWire(physicalType) {
		return []boundRangeTail{tail}
	}
	hasRealLow := tail.hasLow && tail.lowItem != nil
	hasRealHigh := tail.hasHigh && tail.highItem != nil
	switch {
	case hasRealHigh && !hasRealLow:
		bounded := tail
		bounded.hasLow = true
		bounded.lowItem = floatWireInfinity(physicalType, -1)
		bounded.lowEndpoint = recordlayer.EndpointTypeRangeInclusive
		bounded.lowIsNullBoundary = false
		return []boundRangeTail{bounded}
	case hasRealLow && !hasRealHigh:
		negativeNaNs := boundRangeTail{
			kind:         boundRangeTailInequality,
			floatOrdered: true,
			hasLow:       true,
			lowItem:      floatWireLowestNaN(physicalType),
			lowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
			hasHigh:      true,
			highItem:     floatWireInfinity(physicalType, -1),
			highEndpoint: recordlayer.EndpointTypeRangeExclusive,
		}
		if reverse {
			// Descending, the logically greatest rows come first, and every
			// NaN is logically greater than every number.
			return []boundRangeTail{negativeNaNs, tail}
		}
		return []boundRangeTail{tail, negativeNaNs}
	default:
		return []boundRangeTail{tail}
	}
}

func scanBoundUsesFloatWire(physicalType values.Type) bool {
	return physicalType != nil &&
		(physicalType.Code() == values.TypeCodeFloat ||
			physicalType.Code() == values.TypeCodeDouble)
}
