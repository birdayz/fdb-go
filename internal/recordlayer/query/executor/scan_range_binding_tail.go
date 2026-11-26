package executor

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func bindRangeTail(
	comparisons []*predicates.ComparisonRange,
	equalityCount int,
	keyTypes []values.Type,
	binder values.ParameterBinder,
) (boundRangeTail, error) {
	tail := boundRangeTail{kind: boundRangeTailAllOf}
	if equalityCount >= len(comparisons) {
		return tail, nil
	}
	nextRange := comparisons[equalityCount]
	if nextRange == nil || nextRange.IsEmpty() || !nextRange.IsInequality() {
		return tail, nil
	}

	inequalities := nextRange.GetInequalityComparisons()
	physicalType := scanPhysicalTypeAt(keyTypes, equalityCount)
	if err := validateBoundRangeTailComparisons(inequalities, equalityCount); err != nil {
		return boundRangeTail{}, err
	}
	if len(inequalities) == 1 && inequalities[0].Type == predicates.ComparisonStartsWith {
		return bindStartsWithRangeTail(inequalities[0], equalityCount, physicalType, binder)
	}

	tail.kind = boundRangeTailInequality
	for _, inequality := range inequalities {
		eval, err := evaluateRangeTailInequality(inequality, equalityCount, physicalType, binder, &tail)
		if err != nil {
			return boundRangeTail{}, err
		}
		if eval.empty {
			return boundRangeTail{kind: boundRangeTailEmpty}, nil
		}
		if eval.hasIntProjection {
			intersectIntegerProjection(&tail, eval.integerProjection)
			if rangeTailIsEmpty(tail) {
				return boundRangeTail{kind: boundRangeTailEmpty}, nil
			}
			continue
		}
		switch inequality.Type {
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			if eval.comparand == nil {
				return boundRangeTail{kind: boundRangeTailEmpty}, nil
			}
		}
		if err := applyRangeTailInequality(&tail, inequality.Type, eval.comparand, eval.projectionRelation); err != nil {
			return boundRangeTail{}, err
		}
		if rangeTailIsEmpty(tail) {
			return boundRangeTail{kind: boundRangeTailEmpty}, nil
		}
	}
	return tail, nil
}

func bindStartsWithRangeTail(
	comparison *predicates.Comparison,
	equalityCount int,
	physicalType values.Type,
	binder values.ParameterBinder,
) (boundRangeTail, error) {
	if comparison.Operand == nil {
		return boundRangeTail{}, fmt.Errorf(
			"scan STARTS_WITH component %d has no operand", equalityCount,
		)
	}
	value, err := comparison.Operand.Evaluate(binder)
	if err != nil {
		return boundRangeTail{}, err
	}
	if value == nil {
		return boundRangeTail{kind: boundRangeTailEmpty}, nil
	}
	if err := validateAuthoritativeScanPhysicalType(
		physicalType, equalityCount, comparison.Type,
	); err != nil {
		return boundRangeTail{}, err
	}
	if physicalType.Code() != values.TypeCodeString {
		return boundRangeTail{}, &UnsupportedPhysicalStartsWithError{
			Component: equalityCount, PhysicalType: physicalType,
		}
	}
	value = coerceTupleElementForKey(value, physicalType, nil)
	if err := validatePhysicalComparandCarrier(
		value, physicalType, equalityCount, comparison.Type,
	); err != nil {
		return boundRangeTail{}, err
	}
	if scanBoundUsesFloatWire(physicalType) &&
		isNaNFloatBound(value) {
		return boundRangeTail{}, &UnsupportedPhysicalFloatEquivalenceError{
			Component: equalityCount, Comparison: comparison.Type,
			PhysicalType: physicalType,
		}
	}
	return boundRangeTail{kind: boundRangeTailStartsWith, startsWith: value}, nil
}

type tailInequalityEval struct {
	comparand          any
	projectionRelation int
	integerProjection  integerDomainProjection
	hasIntProjection   bool
	empty              bool
}

func evaluateRangeTailInequality(
	inequality *predicates.Comparison,
	equalityCount int,
	physicalType values.Type,
	binder values.ParameterBinder,
	tail *boundRangeTail,
) (tailInequalityEval, error) {
	var out tailInequalityEval
	if inequality.Type == predicates.ComparisonIsNotNull || inequality.Operand == nil {
		return out, nil
	}
	value, err := inequality.Operand.Evaluate(binder)
	if err != nil {
		return out, err
	}
	// Every supported binary range comparison with NULL is UNKNOWN and
	// therefore selects no key. This result does not depend on the
	// component's non-null tuple carrier.
	if value == nil {
		out.empty = true
		return out, nil
	}
	if err := validateAuthoritativeScanPhysicalType(
		physicalType, equalityCount, inequality.Type,
	); err != nil {
		return out, err
	}
	if projected, ok := projectRuntimeFloatComparisonToIntegerDomain(
		value, physicalType, inequality.Type,
	); ok {
		if projected.empty {
			out.empty = true
			return out, nil
		}
		out.integerProjection = projected
		out.hasIntProjection = true
		return out, nil
	}
	projection := projectScanComparandToKey(value, physicalType)
	comparand := projection.wireValue
	projectionRelation := projection.relationToLogical
	if err := validatePhysicalComparandCarrier(
		comparand, physicalType, equalityCount, inequality.Type,
	); err != nil {
		return out, err
	}
	if scanBoundUsesFloatWire(physicalType) &&
		isOrderedScanComparison(inequality.Type) {
		// Not a bail: the disagreement between physical tuple order
		// and the logical comparator is confined to the NaN blocks,
		// and decomposeOrderedFloatTail below represents it exactly
		// as one or two ranges. Withdrawing the access path here
		// would trade an index range for a full scan and fix
		// nothing.
		tail.floatOrdered = true
	}
	if scanBoundUsesFloatWire(physicalType) &&
		isNaNFloatBound(comparand) {
		return out, &UnsupportedPhysicalFloatEquivalenceError{
			Component: equalityCount, Comparison: inequality.Type,
			PhysicalType: physicalType,
		}
	}
	if scanBoundUsesFloatWire(physicalType) {
		comparand = canonicalizeProjectedZeroSignedBound(
			comparand, inequality.Type, projectionRelation,
		)
	}
	out.comparand = comparand
	out.projectionRelation = projectionRelation
	return out, nil
}

func applyRangeTailInequality(
	tail *boundRangeTail,
	comparisonType predicates.ComparisonType,
	comparand any,
	projectionRelation int,
) error {
	switch comparisonType {
	case predicates.ComparisonGreaterThan:
		endpoint := recordlayer.EndpointTypeRangeExclusive
		if projectionRelation > 0 {
			// The rounded physical bound is already strictly greater than
			// the logical comparand, so it is the first qualifying key.
			endpoint = recordlayer.EndpointTypeRangeInclusive
		}
		tightenRangeTailLow(tail, comparand, endpoint)
	case predicates.ComparisonGreaterThanEq:
		endpoint := recordlayer.EndpointTypeRangeInclusive
		if projectionRelation < 0 {
			// The rounded physical bound is below the logical comparand;
			// start at its successor.
			endpoint = recordlayer.EndpointTypeRangeExclusive
		}
		tightenRangeTailLow(tail, comparand, endpoint)
	case predicates.ComparisonLessThan:
		endpoint := recordlayer.EndpointTypeRangeExclusive
		if projectionRelation < 0 {
			// The rounded physical bound is already strictly less than the
			// logical comparand, so it remains a qualifying key.
			endpoint = recordlayer.EndpointTypeRangeInclusive
		}
		tightenRangeTailHigh(tail, comparand, endpoint)
		if !tail.hasLow {
			tail.hasLow = true
			tail.lowEndpoint = recordlayer.EndpointTypeRangeExclusive
			tail.lowIsNullBoundary = true
		}
	case predicates.ComparisonLessThanOrEq:
		endpoint := recordlayer.EndpointTypeRangeInclusive
		if projectionRelation > 0 {
			// The rounded physical bound is above the logical comparand;
			// stop before it.
			endpoint = recordlayer.EndpointTypeRangeExclusive
		}
		tightenRangeTailHigh(tail, comparand, endpoint)
		if !tail.hasLow {
			tail.hasLow = true
			tail.lowEndpoint = recordlayer.EndpointTypeRangeExclusive
			tail.lowIsNullBoundary = true
		}
	case predicates.ComparisonIsNotNull:
		if !tail.hasLow {
			tail.hasLow = true
			tail.lowEndpoint = recordlayer.EndpointTypeRangeExclusive
			tail.lowIsNullBoundary = true
		}
	default:
		return fmt.Errorf(
			"bindScanComparisonsToRangeSet: unexpected inequality comparison %v combined with another inequality on the same column (not a representable single scan range)",
			comparisonType,
		)
	}
	return nil
}

func validateBoundRangeTailComparisons(
	comparisons []*predicates.Comparison,
	component int,
) error {
	if len(comparisons) == 0 {
		return &InvalidScanComparisonShapeError{
			Component: component,
			Detail:    "inequality range has no comparisons",
		}
	}
	for _, comparison := range comparisons {
		if comparison == nil {
			return &InvalidScanComparisonShapeError{
				Component: component,
				Detail:    "inequality range contains a nil comparison",
			}
		}
		switch comparison.Type {
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			if comparison.Operand == nil {
				return &InvalidScanComparisonShapeError{
					Component: component,
					Detail: fmt.Sprintf(
						"ordered comparison %v has no operand", comparison.Type,
					),
				}
			}
		case predicates.ComparisonIsNotNull:
			if comparison.Operand != nil {
				return &InvalidScanComparisonShapeError{
					Component: component,
					Detail:    "IS NOT NULL unexpectedly has an operand",
				}
			}
		case predicates.ComparisonStartsWith:
			if len(comparisons) != 1 {
				return &InvalidScanComparisonShapeError{
					Component: component,
					Detail:    "STARTS_WITH cannot be combined with another range comparison",
				}
			}
			if comparison.Operand == nil {
				return &InvalidScanComparisonShapeError{
					Component: component,
					Detail:    "STARTS_WITH has no operand",
				}
			}
		default:
			return &InvalidScanComparisonShapeError{
				Component: component,
				Detail: fmt.Sprintf(
					"comparison %v is not a representable range-tail operator",
					comparison.Type,
				),
			}
		}
	}
	return nil
}

func isOrderedScanComparison(comparisonType predicates.ComparisonType) bool {
	switch comparisonType {
	case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
		return true
	default:
		return false
	}
}

// intersectIntegerProjection tightens an existing physical-integer tail with
// one inclusive truth interval. It is order-independent: ComparisonRange keeps
// inequalities in predicate insertion order, so a projected upper bound must
// not erase a stricter lower bound that happened to be processed first (or
// vice versa).
func intersectIntegerProjection(
	tail *boundRangeTail,
	projection integerDomainProjection,
) {
	if projection.empty {
		tail.kind = boundRangeTailEmpty
		return
	}
	tightenRangeTailLow(tail, projection.low, recordlayer.EndpointTypeRangeInclusive)
	tightenRangeTailHigh(tail, projection.high, recordlayer.EndpointTypeRangeInclusive)
}

// tightenRangeTailLow intersects a new lower bound with the existing lower
// bound in exact FDB tuple order. Every non-null item has already been
// projected to, and validated against, one authoritative physical carrier, so
// comparing packed one-element tuples is the storage-domain comparison. In
// particular, this distinguishes FLOAT/DOUBLE -0 from +0 after the logical
// endpoint has been canonicalized to the correct physical sign.
func tightenRangeTailLow(
	tail *boundRangeTail,
	item any,
	endpoint recordlayer.EndpointType,
) {
	if !tail.hasLow {
		tail.hasLow = true
		tail.lowItem = item
		tail.lowEndpoint = endpoint
		tail.lowIsNullBoundary = false
		return
	}
	existing := tail.lowItem
	if tail.lowIsNullBoundary {
		existing = nil
	}
	comparison := compareTupleRangeItems(item, existing)
	if comparison > 0 ||
		(comparison == 0 && endpoint == recordlayer.EndpointTypeRangeExclusive &&
			tail.lowEndpoint == recordlayer.EndpointTypeRangeInclusive) {
		tail.lowItem = item
		tail.lowEndpoint = endpoint
		tail.lowIsNullBoundary = false
	}
}

// tightenRangeTailHigh is the upper-bound mirror of tightenRangeTailLow.
func tightenRangeTailHigh(
	tail *boundRangeTail,
	item any,
	endpoint recordlayer.EndpointType,
) {
	if !tail.hasHigh {
		tail.hasHigh = true
		tail.highItem = item
		tail.highEndpoint = endpoint
		return
	}
	comparison := compareTupleRangeItems(item, tail.highItem)
	if comparison < 0 ||
		(comparison == 0 && endpoint == recordlayer.EndpointTypeRangeExclusive &&
			tail.highEndpoint == recordlayer.EndpointTypeRangeInclusive) {
		tail.highItem = item
		tail.highEndpoint = endpoint
	}
}

func compareTupleRangeItems(left, right any) int {
	return bytes.Compare(tuple.Tuple{left}.Pack(), tuple.Tuple{right}.Pack())
}

func rangeTailIsEmpty(tail boundRangeTail) bool {
	if tail.kind == boundRangeTailEmpty || !tail.hasLow || !tail.hasHigh {
		return tail.kind == boundRangeTailEmpty
	}
	low := tail.lowItem
	if tail.lowIsNullBoundary {
		low = nil
	}
	comparison := compareTupleRangeItems(low, tail.highItem)
	if comparison > 0 {
		return true
	}
	return comparison == 0 &&
		(tail.lowEndpoint == recordlayer.EndpointTypeRangeExclusive ||
			tail.highEndpoint == recordlayer.EndpointTypeRangeExclusive)
}

// canonicalizeProjectedZeroSignedBound chooses the physical zero endpoint for
// an ordered predicate after numeric projection. When the logical comparand is
// exactly zero, the comparison type determines which of the adjacent -0/+0
// keys to use (canonicalizeZeroSignedBound). When a tiny non-zero value
// underflows to zero, the projection direction is authoritative instead:
// rounding down means the logical value is above both zero keys, so +0 is the
// boundary; rounding up means it is below both, so -0 is the boundary.
func canonicalizeProjectedZeroSignedBound(
	comparand any,
	comparisonType predicates.ComparisonType,
	projectionRelation int,
) any {
	if !isZeroFloatBound(comparand) {
		return comparand
	}
	switch {
	case projectionRelation < 0:
		return positiveZeroLike(comparand)
	case projectionRelation > 0:
		return negativeZeroLike(comparand)
	default:
		return canonicalizeZeroSignedBound(comparand, comparisonType)
	}
}
