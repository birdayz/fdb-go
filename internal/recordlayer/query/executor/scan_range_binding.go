package executor

import (
	"fmt"
	"math"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// bindScanComparisonsToRangeSet evaluates a comparison prefix once and returns
// a lazy physical range-set specification. keyTypes is aligned to comparisons;
// a missing/Unknown entry is never reconstructed from the operand declaration
// or evaluated runtime representation. Provably type-independent NULL cases
// remain executable; every non-null comparand fails before storage with
// UnknownPhysicalKeyTypeError.
func bindScanComparisonsToRangeSet(
	comparisons []*predicates.ComparisonRange,
	keyTypes []values.Type,
	binder values.ParameterBinder,
	reverse bool,
	fingerprintSalt string,
	compatibleFingerprintSalts ...string,
) (scanRangeSetSpec, error) {
	return bindScanComparisonsToRangeSetWithTerminalWideningAndCompatibility(
		comparisons, keyTypes, binder, reverse, fingerprintSalt,
		compatibleFingerprintSalts, true, false,
	)
}

// bindScanComparisonsToRangeSetWithNaNBlocks is bindScanComparisonsToRangeSet
// for a scan whose key entries are rows: a value-index or primary-key scan. A
// NaN equality there selects both NaN key blocks, every NaN the per-row `=`
// matches (RFC-257 WS-E 5.3). An aggregate-index scan keeps the refusal: each
// NaN payload is its own stored group, which a range would return as one
// group per payload.
func bindScanComparisonsToRangeSetWithNaNBlocks(
	comparisons []*predicates.ComparisonRange,
	keyTypes []values.Type,
	binder values.ParameterBinder,
	reverse bool,
	fingerprintSalt string,
	compatibleFingerprintSalts ...string,
) (scanRangeSetSpec, error) {
	return bindScanComparisonsToRangeSetWithTerminalWideningAndCompatibility(
		comparisons, keyTypes, binder, reverse, fingerprintSalt,
		compatibleFingerprintSalts, true, true,
	)
}

// bindScanComparisonsToRangeSetWithTerminalWidening is the common binder with
// an explicit terminal-zero policy. Ordinary tuple scans can cover the two
// adjacent terminal zero subtrees with one inclusive range. Vector partition
// prefixes cannot: each sign names a separate graph that must be opened as its
// own exact partition, so that caller passes false.
func bindScanComparisonsToRangeSetWithTerminalWidening(
	comparisons []*predicates.ComparisonRange,
	keyTypes []values.Type,
	binder values.ParameterBinder,
	reverse bool,
	fingerprintSalt string,
	allowTerminalZeroWidening bool,
) (scanRangeSetSpec, error) {
	return bindScanComparisonsToRangeSetWithTerminalWideningAndCompatibility(
		comparisons, keyTypes, binder, reverse, fingerprintSalt, nil,
		allowTerminalZeroWidening, false,
	)
}

func bindScanComparisonsToRangeSetWithTerminalWideningAndCompatibility(
	comparisons []*predicates.ComparisonRange,
	keyTypes []values.Type,
	binder values.ParameterBinder,
	reverse bool,
	fingerprintSalt string,
	compatibleFingerprintSalts []string,
	allowTerminalZeroWidening bool,
	allowNaNBlocks bool,
) (scanRangeSetSpec, error) {
	if err := validateScanComparisonShape(comparisons); err != nil {
		return scanRangeSetSpec{}, err
	}
	bound := &boundScanRangeSet{
		tails:                      []boundRangeTail{{kind: boundRangeTailAllOf}},
		terminalZeroIndex:          -1,
		reverse:                    reverse,
		fingerprintSalt:            fingerprintSalt,
		compatibleFingerprintSalts: append([]string(nil), compatibleFingerprintSalts...),
	}

	equalityCount := 0
	for i, comparisonRange := range comparisons {
		if comparisonRange == nil || !comparisonRange.IsEquality() {
			break
		}
		done, err := bindEqualityPrefixComponent(
			bound, comparisons, i, comparisonRange, keyTypes, binder,
			reverse, allowTerminalZeroWidening, allowNaNBlocks,
		)
		if err != nil {
			return scanRangeSetSpec{}, err
		}
		if done {
			return bound.spec(), nil
		}
		equalityCount++
	}

	tail, err := bindRangeTail(comparisons, equalityCount, keyTypes, binder)
	if err != nil {
		return scanRangeSetSpec{}, err
	}
	// An UNBOUND float coordinate is deliberately NOT split here.
	//
	// It used to be, into NULL / [-Inf..+Inf] / the two NaN blocks, so that a
	// single-column float ORDER BY could drop its sort. That bought nothing
	// measurable — it changed 0 of the 2489 plans in the plan-shape golden —
	// and it could not help the multi-column case it was introduced for,
	// because an ordering claim stops AT a coordinate whose physical key order
	// is not its logical order — the planner-side rule carried by
	// values.ColumnCanExtendOrderingClaim, with plans.EqualityPinsSinglePhysicalKey
	// as its narrower equality-bound companion. Against that it charged FOUR
	// range opens instead of one to every scan whose first unbound coordinate
	// is a float, ordering-consuming or not.
	//
	// A pure optimisation that no test can detect, paid for on every such scan,
	// is not worth its cost. Reinstating it needs a way to split only when the
	// ordering is actually consumed, plus a benefit someone can measure.
	bound.tails = decomposeOrderedFloatTail(
		tail, scanPhysicalTypeAt(keyTypes, equalityCount), reverse,
	)
	if tail.kind == boundRangeTailEmpty {
		bound.empty = true
	}
	return bound.spec(), nil
}

func bindEqualityPrefixComponent(
	bound *boundScanRangeSet,
	comparisons []*predicates.ComparisonRange,
	i int,
	comparisonRange *predicates.ComparisonRange,
	keyTypes []values.Type,
	binder values.ParameterBinder,
	reverse bool,
	allowTerminalZeroWidening bool,
	allowNaNBlocks bool,
) (bool, error) {
	comparison := comparisonRange.GetEqualityComparison()
	if comparison == nil {
		return false, fmt.Errorf("scan equality component %d has no comparison", i)
	}

	physicalType := scanPhysicalTypeAt(keyTypes, i)
	component := boundRangeComponent{physicalType: physicalType}
	if comparison.Type == predicates.ComparisonIsNull {
		component.alternatives = []any{nil}
		bound.components = append(bound.components, component)
		return false, nil
	}
	if comparison.Operand == nil {
		return false, fmt.Errorf(
			"scan equality component %d (%v) has no operand", i, comparison.Type,
		)
	}

	value, err := comparison.Operand.Evaluate(binder)
	if err != nil {
		return false, err
	}
	// NULL does not need a tuple-width decision. Ordinary equality is
	// UNKNOWN for every row; null-safe equality selects the one NULL tuple
	// key independently of the component's non-null carrier.
	if value == nil {
		if comparison.Type == predicates.ComparisonEquals {
			bound.empty = true
			return true, nil
		}
		if comparison.Type == predicates.ComparisonNotDistinctFrom {
			component.alternatives = []any{nil}
			bound.components = append(bound.components, component)
			return false, nil
		}
	}
	if err := validateAuthoritativeScanPhysicalType(
		physicalType, i, comparison.Type,
	); err != nil {
		return false, err
	}

	projection, empty, err := projectEqualityComparand(value, physicalType, i, comparison.Type)
	if err != nil {
		return false, err
	}
	if empty {
		bound.empty = true
		return true, nil
	}
	value = projection.wireValue
	if err := validatePhysicalComparandCarrier(
		value, physicalType, i, comparison.Type,
	); err != nil {
		return false, err
	}
	if scanBoundUsesFloatWire(physicalType) && isNaNFloatBound(value) {
		return true, bindEqualityNaNComponent(
			bound, comparisons, i, comparison.Type, physicalType,
			binder, reverse, allowNaNBlocks,
		)
	}

	// A FLOAT key contains only float32 values. If the evaluated predicate
	// comparand lies between two such values, no key can be logically equal
	// to it under cmpAny's float64 comparison domain. Probing the rounded key
	// would return a row that the same predicate rejects (for example,
	// FLOAT(16_777_216) = BIGINT(16_777_217)). Null-safe equality has the
	// same non-null numeric truth table, so it is empty as well.
	if projection.narrowedToFloat32 && projection.relationToLogical != 0 {
		bound.empty = true
		return true, nil
	}

	if scanBoundUsesFloatWire(physicalType) && isZeroFloatBound(value) {
		if allowTerminalZeroWidening && !anyLaterComparisonConstrains(comparisons, i) {
			// The two terminal zero subtrees are adjacent, so one inclusive
			// range is exact. Normalize the stored component to negative zero
			// so +0 and -0 bindings produce the same continuation fingerprint.
			component.alternatives = []any{negativeZeroLike(value)}
			bound.components = append(bound.components, component)
			bound.terminalZeroIndex = len(bound.components) - 1
			return true, nil
		}
		if reverse {
			component.alternatives = []any{positiveZeroLike(value), negativeZeroLike(value)}
		} else {
			component.alternatives = []any{negativeZeroLike(value), positiveZeroLike(value)}
		}
	} else {
		component.alternatives = []any{value}
	}
	bound.components = append(bound.components, component)
	return false, nil
}

func projectEqualityComparand(
	value any,
	physicalType values.Type,
	component int,
	comparisonType predicates.ComparisonType,
) (scanKeyComparandProjection, bool, error) {
	// The inverse of a runtime floating comparand over a physical INT/LONG
	// key is not necessarily one key. cmpAny converts the stored integer to
	// float64, so at and above 2^53 several adjacent integers can compare
	// equal to one float64. Preserve the planner's fixed-key proof by
	// accepting only empty or singleton inverse classes; a plural class is
	// correct-or-loud before any storage cursor opens.
	if integerProjection, projected := projectRuntimeFloatComparisonToIntegerDomain(
		value, physicalType, comparisonType,
	); projected {
		if integerProjection.empty {
			return scanKeyComparandProjection{}, true, nil
		}
		if integerProjection.low != integerProjection.high {
			return scanKeyComparandProjection{}, false, &UnsupportedPhysicalNumericProjectionError{
				Component:            component,
				Comparison:           comparisonType,
				PhysicalType:         physicalType,
				EquivalenceClassLow:  integerProjection.low,
				EquivalenceClassHigh: integerProjection.high,
			}
		}
		return scanKeyComparandProjection{wireValue: integerProjection.low}, false, nil
	}
	return projectScanComparandToKey(value, physicalType), false, nil
}

func bindEqualityNaNComponent(
	bound *boundScanRangeSet,
	comparisons []*predicates.ComparisonRange,
	i int,
	comparisonType predicates.ComparisonType,
	physicalType values.Type,
	binder values.ParameterBinder,
	reverse bool,
	allowNaNBlocks bool,
) error {
	// Every NaN is one logical value, but each sign and payload is its
	// own tuple key, in two blocks at the ends of the coordinate. With
	// no later component constrained, the two blocks over this prefix
	// are exactly the rows the per-row `=` matches, whatever payload
	// the comparand has. A later constrained component becomes a key
	// filter over the entries of both blocks.
	if !allowNaNBlocks {
		return &UnsupportedPhysicalFloatEquivalenceError{
			Component: i, Comparison: comparisonType, PhysicalType: physicalType,
		}
	}
	if anyLaterComparisonConstrains(comparisons, i) {
		filter, empty, err := bindScanKeyFilter(comparisons, i+1, binder)
		if err != nil {
			return err
		}
		if empty {
			bound.empty = true
			return nil
		}
		bound.keyFilter = filter
	}
	bound.tails = nanBlockTails(physicalType, reverse)
	return nil
}

func validateScanComparisonShape(comparisons []*predicates.ComparisonRange) error {
	endedContiguousPrefix := false
	tailSeen := false
	for i, comparisonRange := range comparisons {
		if comparisonRange == nil || comparisonRange.IsEmpty() {
			endedContiguousPrefix = true
			continue
		}
		if endedContiguousPrefix {
			return &InvalidScanComparisonShapeError{
				Component: i,
				Detail:    "constrained component follows an unconstrained gap or inequality tail",
			}
		}
		switch {
		case comparisonRange.IsEquality():
			// Equality components constitute the still-open contiguous prefix.
		case comparisonRange.IsInequality():
			if tailSeen {
				return &InvalidScanComparisonShapeError{
					Component: i,
					Detail:    "more than one inequality component",
				}
			}
			tailSeen = true
			endedContiguousPrefix = true
		default:
			return &InvalidScanComparisonShapeError{
				Component: i,
				Detail:    "component is neither equality, inequality, nor empty",
			}
		}
	}
	return nil
}

func scanPhysicalTypeAt(keyTypes []values.Type, i int) values.Type {
	if i < 0 || i >= len(keyTypes) {
		return nil
	}
	return keyTypes[i]
}

func validateAuthoritativeScanPhysicalType(
	physicalType values.Type,
	component int,
	comparisonType predicates.ComparisonType,
) error {
	if physicalType == nil || physicalType.Code() == values.TypeCodeUnknown {
		return &UnknownPhysicalKeyTypeError{
			Component: component, Comparison: comparisonType,
		}
	}
	if !physicalType.Code().IsPrimitive() {
		return &UnsupportedPhysicalKeyTypeError{
			Component: component, Comparison: comparisonType, PhysicalType: physicalType,
		}
	}
	return nil
}

// projectScanComparandToKey performs the physical wire conversion while also
// preserving enough information to make that conversion predicate-faithful.
// CockroachDB avoids this particular impedance mismatch by canonicalizing both
// signed zeros into one key and representing SQL floats as float64 keys. The
// FDB tuple format is Java-compatible and distinguishes FLOAT from DOUBLE (and
// both zero signs), so this binder must project explicitly rather than merely
// cast and hope the endpoint still means the same thing.
func projectScanComparandToKey(value any, physicalType values.Type) scanKeyComparandProjection {
	if physicalType != nil && physicalType.Code() == values.TypeCodeFloat {
		logicalValue, _, numeric := values.ToFloat64(value)
		if numeric {
			wireValue := float32(logicalValue)
			physicalValue := float64(wireValue)
			relation := 0
			// NaN is handled by the caller's correct-or-loud path. It has no
			// useful less/equal/greater projection relation.
			if !math.IsNaN(logicalValue) {
				switch {
				case physicalValue < logicalValue:
					relation = -1
				case physicalValue > logicalValue:
					relation = 1
				}
			}
			return scanKeyComparandProjection{
				wireValue:         wireValue,
				relationToLogical: relation,
				narrowedToFloat32: true,
			}
		}
	}
	return scanKeyComparandProjection{
		// The caller has established authoritative physical metadata. Never
		// pass the operand type into the legacy coercer's fallback path.
		wireValue: coerceTupleElementForKey(value, physicalType, nil),
	}
}

// validatePhysicalComparandCarrier verifies the projected Go value against the
// concrete carrier used when that physical key component was written. nil is
// admitted for every nullable key and handled by the comparison's SQL NULL
// semantics at the caller. The caller has already rejected unknown,
// placeholder, and structured physical metadata.
func validatePhysicalComparandCarrier(
	value any,
	physicalType values.Type,
	component int,
	comparisonType predicates.ComparisonType,
) error {
	if value == nil || physicalType == nil || physicalType.Code() == values.TypeCodeUnknown {
		return nil
	}

	compatible := true
	projectedType := fmt.Sprintf("%T", value)
	switch physicalType.Code() {
	case values.TypeCodeBoolean:
		_, compatible = value.(bool)
	case values.TypeCodeInt, values.TypeCodeLong:
		_, compatible = value.(int64)
	case values.TypeCodeFloat:
		_, compatible = value.(float32)
	case values.TypeCodeDouble:
		_, compatible = value.(float64)
	case values.TypeCodeString, values.TypeCodeDate, values.TypeCodeTimestamp:
		// DATE/TIMESTAMP key fields use the same canonical ISO string tuple
		// carrier as their backing protobuf string. A runtime time.Time remains
		// logically comparable in cmpAny, but has no identical tuple carrier;
		// until an explicit canonical temporal projection exists it is loud.
		_, compatible = value.(string)
	case values.TypeCodeBytes:
		_, compatible = value.([]byte)
	case values.TypeCodeUuid:
		_, compatible = value.(tuple.UUID)
	case values.TypeCodeVersion:
		versionstamp, ok := value.(tuple.Versionstamp)
		compatible = ok
		if ok {
			// Ordinary Tuple.Pack deliberately panics for an incomplete commit
			// versionstamp. Query bounds cannot meaningfully probe the commit-
			// version placeholder, so reject it before the range-set fingerprint
			// or materializer reaches Pack.
			incomplete, err := tuple.Tuple{versionstamp}.HasIncompleteVersionstamp()
			if err != nil || incomplete {
				compatible = false
				projectedType = "incomplete tuple.Versionstamp"
			}
		}
	default:
		// validateAuthoritativeScanPhysicalType rejects every non-scalar code
		// before projection. Retain a fail-closed default in case a new primitive
		// TypeCode is added without defining its tuple carrier here.
		compatible = false
	}
	if compatible {
		return nil
	}
	return &IncompatiblePhysicalComparandError{
		Component:     component,
		Comparison:    comparisonType,
		PhysicalType:  physicalType,
		ProjectedType: projectedType,
	}
}
