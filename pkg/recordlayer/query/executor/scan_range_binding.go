package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// UnsupportedPhysicalFloatEquivalenceError reports a floating-point scan
// comparand whose logical equality class cannot yet be represented exactly by
// this physical range binder. FDB tuple keys preserve every NaN sign/payload,
// while the query value comparator treats NaNs as one logical value. Probing
// the evaluated payload would silently miss equal keys, so an already-chosen
// dynamic index access fails before opening storage.
type UnsupportedPhysicalFloatEquivalenceError struct {
	Component    int
	Comparison   predicates.ComparisonType
	PhysicalType values.Type
}

func (e *UnsupportedPhysicalFloatEquivalenceError) Error() string {
	typ := "UNKNOWN"
	if e.PhysicalType != nil {
		typ = e.PhysicalType.Code().String()
	}
	return fmt.Sprintf(
		"scan comparison %d (%v, physical %s) evaluates to NaN; exact indexed NaN equivalence is unsupported",
		e.Component, e.Comparison, typ,
	)
}

// UnsupportedPhysicalNumericProjectionError reports a floating-point
// comparand whose logical equality class contains more than one value in an
// authoritative physical integer key domain. The scan plan's fixed-key and
// one-row proofs are valid only when every successful equality binding selects
// at most one physical integer key. Returning this error before storage is
// therefore preferable to silently probing one representative of a larger
// cmpAny equivalence class.
//
// The class can contain multiple integers because cmpAny compares a mixed
// integer/float pair after converting the integer to float64. For example,
// both 2^53 and 2^53+1 compare equal to float64(2^53). Ordered comparisons do
// not use this error: their monotone inverse is projected to an exact inclusive
// integer interval by projectFloatComparisonToIntegerDomain.
type UnsupportedPhysicalNumericProjectionError struct {
	Component            int
	Comparison           predicates.ComparisonType
	PhysicalType         values.Type
	EquivalenceClassLow  int64
	EquivalenceClassHigh int64
}

func (e *UnsupportedPhysicalNumericProjectionError) Error() string {
	typ := "UNKNOWN"
	if e.PhysicalType != nil {
		typ = e.PhysicalType.Code().String()
	}
	return fmt.Sprintf(
		"scan comparison %d (%v, physical %s) has a floating comparand whose integer equality class spans [%d,%d]; exact fixed-key projection is unsupported",
		e.Component, e.Comparison, typ,
		e.EquivalenceClassLow, e.EquivalenceClassHigh,
	)
}

// IncompatiblePhysicalComparandError reports an evaluated scan comparand that
// cannot inhabit the authoritative physical key component's tuple carrier.
// UNKNOWN parameters deliberately pass planner type gates, so this check must
// happen after valid packing-boundary coercions (numeric width and UUID) but
// before a mismatched tuple type code can turn SQL UNKNOWN into arbitrary rows.
type IncompatiblePhysicalComparandError struct {
	Component     int
	Comparison    predicates.ComparisonType
	PhysicalType  values.Type
	ProjectedType string
}

func (e *IncompatiblePhysicalComparandError) Error() string {
	typ := "UNKNOWN"
	if e.PhysicalType != nil {
		typ = e.PhysicalType.Code().String()
	}
	return fmt.Sprintf(
		"scan comparison %d (%v, physical %s) projects to incompatible tuple carrier %s",
		e.Component, e.Comparison, typ, e.ProjectedType,
	)
}

// UnknownPhysicalKeyTypeError reports a non-null scan comparand for which the
// chosen physical plan did not carry an authoritative key-component type.
// Tuple encoding is selected by the Go runtime carrier, so guessing from an
// operand declaration or evaluated parameter can probe a different tuple type
// code than the stored key (most notably FLOAT versus DOUBLE). Type-independent
// NULL cases are handled before this error is considered.
type UnknownPhysicalKeyTypeError struct {
	Component  int
	Comparison predicates.ComparisonType
}

func (e *UnknownPhysicalKeyTypeError) Error() string {
	return fmt.Sprintf(
		"scan comparison %d (%v) has a non-null comparand but no authoritative physical key type",
		e.Component, e.Comparison,
	)
}

// UnsupportedPhysicalKeyTypeError reports physical metadata that names a
// placeholder or structured logical type rather than one concrete scalar FDB
// tuple carrier. Presence is not authority: ANY, NULL, NONE, RECORD, ARRAY,
// RELATION, and ENUM do not specify how a query comparand was encoded in the
// key. Production metadata maps enum fields to their LONG wire carrier; a
// hand-built/stale plan that carries ENUM itself must therefore fail closed.
type UnsupportedPhysicalKeyTypeError struct {
	Component    int
	Comparison   predicates.ComparisonType
	PhysicalType values.Type
}

func (e *UnsupportedPhysicalKeyTypeError) Error() string {
	typ := "UNKNOWN"
	if e.PhysicalType != nil {
		typ = e.PhysicalType.Code().String()
	}
	return fmt.Sprintf(
		"scan comparison %d (%v) has unsupported physical key type %s; a concrete scalar tuple carrier is required",
		e.Component, e.Comparison, typ,
	)
}

// UnsupportedPhysicalStartsWithError reports a STARTS_WITH comparison whose
// authoritative physical key component is not STRING. PREFIX_STRING tuple
// endpoints describe a byte prefix of a packed string element; applying them
// to another carrier (including BYTES, or the string-backed DATE/TIMESTAMP
// logical types) does not implement Comparison.Eval's string/string
// STARTS_WITH truth table. Such a plan must fail before opening storage rather
// than silently treating an operator/type mismatch as a compensated range.
type UnsupportedPhysicalStartsWithError struct {
	Component    int
	PhysicalType values.Type
}

func (e *UnsupportedPhysicalStartsWithError) Error() string {
	typ := "UNKNOWN"
	if e.PhysicalType != nil {
		typ = e.PhysicalType.Code().String()
	}
	return fmt.Sprintf(
		"scan comparison %d (STARTS_WITH, physical %s) requires an authoritative STRING key component",
		e.Component, typ,
	)
}

// InvalidScanComparisonShapeError reports a comparison slice that cannot be
// represented by one contiguous tuple-key prefix: zero or more equalities,
// optionally one inequality component, and then only nil/Empty components.
// Silently stopping at the first gap or tail would discard a later predicate
// while presenting the scan as fully SARGed.
type InvalidScanComparisonShapeError struct {
	Component int
	Detail    string
}

func (e *InvalidScanComparisonShapeError) Error() string {
	return fmt.Sprintf("scan comparison component %d has invalid physical prefix shape: %s",
		e.Component, e.Detail)
}

type boundRangeTailKind uint8

const (
	boundRangeTailAllOf boundRangeTailKind = iota
	boundRangeTailEmpty
	boundRangeTailStartsWith
	boundRangeTailInequality
)

type boundRangeTail struct {
	kind boundRangeTailKind

	// floatOrdered marks a tail built from an ordered comparison on a
	// FLOAT/DOUBLE key coordinate, whose exact physical form may be more than
	// one range. See decomposeOrderedFloatTail.
	floatOrdered bool

	startsWith any

	hasLow            bool
	lowItem           any
	lowEndpoint       recordlayer.EndpointType
	lowIsNullBoundary bool

	hasHigh      bool
	highItem     any
	highEndpoint recordlayer.EndpointType
	// highIsNullBoundary marks a high bound that is the NULL element itself,
	// which cannot be expressed by highItem: a nil highItem is indistinguishable
	// from "no high bound at all".
	highIsNullBoundary bool
}

type boundRangeComponent struct {
	physicalType values.Type
	alternatives []any
}

// scanKeyComparandProjection records how an evaluated logical comparand maps
// onto the discrete physical key domain. relationToLogical compares the value
// represented by wireValue, after widening back into the predicate's numeric
// comparison domain, with the original evaluated comparand: -1 means the
// physical value rounded down, +1 rounded up, and 0 is exact. The relation is
// needed for FLOAT inequalities, where blindly retaining the logical endpoint
// kind after a float64-to-float32 narrowing can either admit a false positive
// or drop the first qualifying key.
type scanKeyComparandProjection struct {
	wireValue         any
	relationToLogical int
	narrowedToFloat32 bool
}

// integerDomainProjection is the exact inverse of one mixed integer/float
// predicate over an authoritative physical integer domain. Non-empty results
// are always one inclusive interval because int64 -> float64 is monotone
// (although not injective above the 53-bit precision cliff).
type integerDomainProjection struct {
	low   int64
	high  int64
	empty bool
}

// boundScanRangeSet is the evaluated, linear-size representation behind one
// scanRangeSetSpec. No tuple prefix is captured per decision and no Cartesian
// product is materialized. materialize builds only the currently selected
// physical TupleRange.
type boundScanRangeSet struct {
	components        []boundRangeComponent
	tails             []boundRangeTail
	terminalZeroIndex int
	reverse           bool
	empty             bool
	fingerprintSalt   string
}

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
) (scanRangeSetSpec, error) {
	return bindScanComparisonsToRangeSetWithTerminalWidening(
		comparisons, keyTypes, binder, reverse, fingerprintSalt, true,
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
	if err := validateScanComparisonShape(comparisons); err != nil {
		return scanRangeSetSpec{}, err
	}
	bound := &boundScanRangeSet{
		tails:             []boundRangeTail{{kind: boundRangeTailAllOf}},
		terminalZeroIndex: -1,
		reverse:           reverse,
		fingerprintSalt:   fingerprintSalt,
	}

	equalityCount := 0
	for i, comparisonRange := range comparisons {
		if comparisonRange == nil || !comparisonRange.IsEquality() {
			break
		}
		comparison := comparisonRange.GetEqualityComparison()
		if comparison == nil {
			return scanRangeSetSpec{}, fmt.Errorf("scan equality component %d has no comparison", i)
		}

		physicalType := scanPhysicalTypeAt(keyTypes, i)
		component := boundRangeComponent{physicalType: physicalType}
		if comparison.Type == predicates.ComparisonIsNull {
			component.alternatives = []any{nil}
			bound.components = append(bound.components, component)
			equalityCount++
			continue
		}
		if comparison.Operand == nil {
			return scanRangeSetSpec{}, fmt.Errorf(
				"scan equality component %d (%v) has no operand", i, comparison.Type)
		}

		value, err := comparison.Operand.Evaluate(binder)
		if err != nil {
			return scanRangeSetSpec{}, err
		}
		// NULL does not need a tuple-width decision. Ordinary equality is
		// UNKNOWN for every row; null-safe equality selects the one NULL tuple
		// key independently of the component's non-null carrier.
		if value == nil {
			if comparison.Type == predicates.ComparisonEquals {
				bound.empty = true
				return bound.spec(), nil
			}
			if comparison.Type == predicates.ComparisonNotDistinctFrom {
				component.alternatives = []any{nil}
				bound.components = append(bound.components, component)
				equalityCount++
				continue
			}
		}
		if err := validateAuthoritativeScanPhysicalType(
			physicalType, i, comparison.Type,
		); err != nil {
			return scanRangeSetSpec{}, err
		}

		// The inverse of a runtime floating comparand over a physical INT/LONG
		// key is not necessarily one key. cmpAny converts the stored integer to
		// float64, so at and above 2^53 several adjacent integers can compare
		// equal to one float64. Preserve the planner's fixed-key proof by
		// accepting only empty or singleton inverse classes; a plural class is
		// correct-or-loud before any storage cursor opens.
		var projection scanKeyComparandProjection
		if integerProjection, projected := projectRuntimeFloatComparisonToIntegerDomain(
			value, physicalType, comparison.Type,
		); projected {
			if integerProjection.empty {
				bound.empty = true
				return bound.spec(), nil
			}
			if integerProjection.low != integerProjection.high {
				return scanRangeSetSpec{}, &UnsupportedPhysicalNumericProjectionError{
					Component:            i,
					Comparison:           comparison.Type,
					PhysicalType:         physicalType,
					EquivalenceClassLow:  integerProjection.low,
					EquivalenceClassHigh: integerProjection.high,
				}
			}
			projection = scanKeyComparandProjection{wireValue: integerProjection.low}
		} else {
			projection = projectScanComparandToKey(value, physicalType)
		}
		value = projection.wireValue
		if err := validatePhysicalComparandCarrier(
			value, physicalType, i, comparison.Type,
		); err != nil {
			return scanRangeSetSpec{}, err
		}
		if scanBoundUsesFloatWire(physicalType) &&
			isNaNFloatBound(value) {
			return scanRangeSetSpec{}, &UnsupportedPhysicalFloatEquivalenceError{
				Component: i, Comparison: comparison.Type, PhysicalType: physicalType,
			}
		}

		// A FLOAT key contains only float32 values. If the evaluated predicate
		// comparand lies between two such values, no key can be logically equal
		// to it under cmpAny's float64 comparison domain. Probing the rounded key
		// would return a row that the same predicate rejects (for example,
		// FLOAT(16_777_216) = BIGINT(16_777_217)). Null-safe equality has the
		// same non-null numeric truth table, so it is empty as well.
		if projection.narrowedToFloat32 && projection.relationToLogical != 0 {
			bound.empty = true
			return bound.spec(), nil
		}

		if scanBoundUsesFloatWire(physicalType) &&
			isZeroFloatBound(value) {
			if allowTerminalZeroWidening && !anyLaterComparisonConstrains(comparisons, i) {
				// The two terminal zero subtrees are adjacent, so one inclusive
				// range is exact. Normalize the stored component to negative zero
				// so +0 and -0 bindings produce the same continuation fingerprint.
				component.alternatives = []any{negativeZeroLike(value)}
				bound.components = append(bound.components, component)
				bound.terminalZeroIndex = len(bound.components) - 1
				equalityCount++
				return bound.spec(), nil
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
		tail, scanPhysicalTypeAt(keyTypes, equalityCount), reverse)
	if tail.kind == boundRangeTailEmpty {
		bound.empty = true
	}
	return bound.spec(), nil
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
// floatWireLowestNaN and floatWireHighestNaN are the extreme encodings of the
// coordinate's carrier: every negative NaN lies in [lowest, -Inf) and every
// positive NaN in (+Inf, highest].
func floatWireLowestNaN(physicalType values.Type) any {
	if physicalType != nil && physicalType.Code() == values.TypeCodeFloat {
		return math.Float32frombits(0xFFFFFFFF)
	}
	return math.Float64frombits(0xFFFFFFFFFFFFFFFF)
}

func floatWireHighestNaN(physicalType values.Type) any {
	if physicalType != nil && physicalType.Code() == values.TypeCodeFloat {
		return math.Float32frombits(0x7FFFFFFF)
	}
	return math.Float64frombits(0x7FFFFFFFFFFFFFFF)
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
		comparison := inequalities[0]
		if comparison.Operand == nil {
			return boundRangeTail{}, fmt.Errorf(
				"scan STARTS_WITH component %d has no operand", equalityCount)
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

	tail.kind = boundRangeTailInequality
	for _, inequality := range inequalities {
		var comparand any
		projectionRelation := 0
		var integerProjection *integerDomainProjection
		if inequality.Type != predicates.ComparisonIsNotNull && inequality.Operand != nil {
			value, err := inequality.Operand.Evaluate(binder)
			if err != nil {
				return boundRangeTail{}, err
			}
			// Every supported binary range comparison with NULL is UNKNOWN and
			// therefore selects no key. This result does not depend on the
			// component's non-null tuple carrier.
			if value == nil {
				return boundRangeTail{kind: boundRangeTailEmpty}, nil
			}
			if err := validateAuthoritativeScanPhysicalType(
				physicalType, equalityCount, inequality.Type,
			); err != nil {
				return boundRangeTail{}, err
			}
			if projected, ok := projectRuntimeFloatComparisonToIntegerDomain(
				value, physicalType, inequality.Type,
			); ok {
				if projected.empty {
					return boundRangeTail{kind: boundRangeTailEmpty}, nil
				}
				integerProjection = &projected
			} else {
				projection := projectScanComparandToKey(value, physicalType)
				comparand = projection.wireValue
				projectionRelation = projection.relationToLogical
				if err := validatePhysicalComparandCarrier(
					comparand, physicalType, equalityCount, inequality.Type,
				); err != nil {
					return boundRangeTail{}, err
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
					return boundRangeTail{}, &UnsupportedPhysicalFloatEquivalenceError{
						Component: equalityCount, Comparison: inequality.Type,
						PhysicalType: physicalType,
					}
				}
				if scanBoundUsesFloatWire(physicalType) {
					comparand = canonicalizeProjectedZeroSignedBound(
						comparand, inequality.Type, projectionRelation)
				}
			}
		}

		if integerProjection != nil {
			intersectIntegerProjection(&tail, *integerProjection)
			if rangeTailIsEmpty(tail) {
				return boundRangeTail{kind: boundRangeTailEmpty}, nil
			}
			continue
		}

		switch inequality.Type {
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			if comparand == nil {
				return boundRangeTail{kind: boundRangeTailEmpty}, nil
			}
		}

		switch inequality.Type {
		case predicates.ComparisonGreaterThan:
			endpoint := recordlayer.EndpointTypeRangeExclusive
			if projectionRelation > 0 {
				// The rounded physical bound is already strictly greater than
				// the logical comparand, so it is the first qualifying key.
				endpoint = recordlayer.EndpointTypeRangeInclusive
			}
			tightenRangeTailLow(&tail, comparand, endpoint)
		case predicates.ComparisonGreaterThanEq:
			endpoint := recordlayer.EndpointTypeRangeInclusive
			if projectionRelation < 0 {
				// The rounded physical bound is below the logical comparand;
				// start at its successor.
				endpoint = recordlayer.EndpointTypeRangeExclusive
			}
			tightenRangeTailLow(&tail, comparand, endpoint)
		case predicates.ComparisonLessThan:
			endpoint := recordlayer.EndpointTypeRangeExclusive
			if projectionRelation < 0 {
				// The rounded physical bound is already strictly less than the
				// logical comparand, so it remains a qualifying key.
				endpoint = recordlayer.EndpointTypeRangeInclusive
			}
			tightenRangeTailHigh(&tail, comparand, endpoint)
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
			tightenRangeTailHigh(&tail, comparand, endpoint)
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
			return boundRangeTail{}, fmt.Errorf(
				"bindScanComparisonsToRangeSet: unexpected inequality comparison %v combined with another inequality on the same column (not a representable single scan range)",
				inequality.Type,
			)
		}
		if rangeTailIsEmpty(tail) {
			return boundRangeTail{kind: boundRangeTailEmpty}, nil
		}
	}
	return tail, nil
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
						"ordered comparison %v has no operand", comparison.Type),
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
					comparison.Type),
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

func (b *boundScanRangeSet) spec() scanRangeSetSpec {
	counts := make([]uint32, len(b.components))
	for i := range b.components {
		counts[i] = uint32(len(b.components[i].alternatives))
	}
	// A plural tail is one more odometer dimension, so the continuation stays
	// flat and O(k) and the cursor needs no new concept to resume inside it.
	if len(b.tails) > 1 {
		counts = append(counts, uint32(len(b.tails)))
	}
	if b.empty {
		return scanRangeSetSpec{
			fingerprint:       b.fingerprint(),
			alternativeCounts: counts,
			empty:             true,
		}
	}
	return scanRangeSetSpec{
		fingerprint:       b.fingerprint(),
		alternativeCounts: counts,
		materialize:       b.materialize,
	}
}

func (b *boundScanRangeSet) materialize(choices []uint32) (recordlayer.TupleRange, error) {
	if b.empty {
		return recordlayer.TupleRange{}, fmt.Errorf("cannot materialize an empty scan range set")
	}
	wantArity := len(b.components)
	if len(b.tails) > 1 {
		wantArity++
	}
	if len(choices) != wantArity {
		return recordlayer.TupleRange{}, fmt.Errorf(
			"scan range choices have arity %d, want %d", len(choices), wantArity)
	}
	tail := b.tails[0]
	if len(b.tails) > 1 {
		tailChoice := choices[len(b.components)]
		if tailChoice >= uint32(len(b.tails)) {
			return recordlayer.TupleRange{}, fmt.Errorf(
				"scan range tail choice %d is out of bounds (tails=%d)",
				tailChoice, len(b.tails))
		}
		tail = b.tails[tailChoice]
		choices = choices[:len(b.components)]
	}

	prefix := make(tuple.Tuple, len(b.components))
	for i, component := range b.components {
		choice := choices[i]
		if choice >= uint32(len(component.alternatives)) {
			return recordlayer.TupleRange{}, fmt.Errorf(
				"scan range choice %d for component %d is out of bounds (alternatives=%d)",
				choice, i, len(component.alternatives))
		}
		prefix[i] = component.alternatives[choice]
	}

	if b.terminalZeroIndex >= 0 {
		i := b.terminalZeroIndex
		base := append(tuple.Tuple(nil), prefix[:i]...)
		zero := prefix[i]
		low := append(append(tuple.Tuple(nil), base...), negativeZeroLike(zero))
		high := append(append(tuple.Tuple(nil), base...), positiveZeroLike(zero))
		return recordlayer.TupleRange{
			Low: low, High: high,
			LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
			HighEndpoint: recordlayer.EndpointTypeRangeInclusive,
		}, nil
	}

	switch tail.kind {
	case boundRangeTailAllOf:
		return recordlayer.TupleRangeAllOf(prefix), nil
	case boundRangeTailEmpty:
		return recordlayer.TupleRange{
			Low: prefix, High: prefix,
			LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
			HighEndpoint: recordlayer.EndpointTypeRangeExclusive,
		}, nil
	case boundRangeTailStartsWith:
		start := append(append(tuple.Tuple(nil), prefix...), tail.startsWith)
		return recordlayer.TupleRange{
			Low: start, High: start,
			LowEndpoint:  recordlayer.EndpointTypePrefixString,
			HighEndpoint: recordlayer.EndpointTypePrefixString,
		}, nil
	case boundRangeTailInequality:
		return b.materializeInequality(prefix, tail), nil
	default:
		return recordlayer.TupleRange{}, fmt.Errorf("unknown bound scan tail kind %d", tail.kind)
	}
}

func (b *boundScanRangeSet) materializeInequality(
	prefix tuple.Tuple,
	tail boundRangeTail,
) recordlayer.TupleRange {
	lowEndpoint := recordlayer.EndpointTypeTreeStart
	highEndpoint := recordlayer.EndpointTypeTreeEnd
	if len(prefix) > 0 {
		lowEndpoint = recordlayer.EndpointTypeRangeInclusive
		highEndpoint = recordlayer.EndpointTypeRangeInclusive
	}
	if tail.hasLow {
		lowEndpoint = tail.lowEndpoint
	}
	if tail.hasHigh {
		highEndpoint = tail.highEndpoint
	}

	var low, high tuple.Tuple
	switch {
	case tail.hasLow && tail.lowItem != nil:
		low = append(append(tuple.Tuple(nil), prefix...), tail.lowItem)
	case tail.hasLow && tail.lowIsNullBoundary:
		low = append(append(tuple.Tuple(nil), prefix...), nil)
	case len(prefix) > 0:
		low = append(tuple.Tuple(nil), prefix...)
	}
	if tail.hasHigh && tail.highItem != nil {
		high = append(append(tuple.Tuple(nil), prefix...), tail.highItem)
	} else if tail.hasHigh && tail.highIsNullBoundary {
		high = append(append(tuple.Tuple(nil), prefix...), nil)
	} else if len(prefix) > 0 {
		high = append(tuple.Tuple(nil), prefix...)
	}
	return recordlayer.TupleRange{
		Low: low, High: high,
		LowEndpoint: lowEndpoint, HighEndpoint: highEndpoint,
	}
}

func (b *boundScanRangeSet) fingerprint() []byte {
	h := sha256.New()
	writeFingerprintBytes(h, []byte("scan-range-set-v1"))
	writeFingerprintBytes(h, []byte(b.fingerprintSalt))
	if b.reverse {
		writeFingerprintBytes(h, []byte{1})
	} else {
		writeFingerprintBytes(h, []byte{0})
	}
	if b.empty {
		writeFingerprintBytes(h, []byte{1})
	} else {
		writeFingerprintBytes(h, []byte{0})
	}
	writeFingerprintUint32(h, uint32(b.terminalZeroIndex+1))
	writeFingerprintUint32(h, uint32(len(b.components)))
	for _, component := range b.components {
		code := values.TypeCodeUnknown
		if component.physicalType != nil {
			code = component.physicalType.Code()
		}
		writeFingerprintUint32(h, uint32(code))
		writeFingerprintUint32(h, uint32(len(component.alternatives)))
		for _, alternative := range component.alternatives {
			writeFingerprintBytes(h, tuple.Tuple{alternative}.Pack())
		}
	}
	writeFingerprintUint32(h, uint32(len(b.tails)))
	for _, t := range b.tails {
		writeFingerprintUint32(h, uint32(t.kind))
		writeFingerprintTailBounds(h, t)
	}
	if !b.empty {
		choices := make([]uint32, len(b.components))
		if len(b.tails) > 1 {
			choices = append(choices, 0)
		}
		if materialized, err := b.materialize(choices); err == nil {
			writeFingerprintBytes(h, materialized.Low.Pack())
			writeFingerprintBytes(h, materialized.High.Pack())
			writeFingerprintUint32(h, uint32(materialized.LowEndpoint))
			writeFingerprintUint32(h, uint32(materialized.HighEndpoint))
		}
	}
	return h.Sum(nil)
}

// writeFingerprintTailBounds folds one tail's endpoints into the continuation
// fingerprint. Every tail must contribute: a plural float tail differs from its
// siblings only in these bounds, so hashing the kind alone would let a resumed
// scan reopen a DIFFERENT range under a fingerprint it considers unchanged.
func writeFingerprintTailBounds(h hash.Hash, t boundRangeTail) {
	writeFingerprintBool(h, t.floatOrdered)
	writeFingerprintBool(h, t.hasLow)
	writeFingerprintBool(h, t.lowIsNullBoundary)
	writeFingerprintUint32(h, uint32(t.lowEndpoint))
	writeFingerprintBytes(h, tuple.Tuple{t.lowItem}.Pack())
	writeFingerprintBool(h, t.hasHigh)
	writeFingerprintBool(h, t.highIsNullBoundary)
	writeFingerprintUint32(h, uint32(t.highEndpoint))
	writeFingerprintBytes(h, tuple.Tuple{t.highItem}.Pack())
	writeFingerprintBytes(h, tuple.Tuple{t.startsWith}.Pack())
}

func writeFingerprintBool(h hash.Hash, value bool) {
	if value {
		writeFingerprintBytes(h, []byte{1})
		return
	}
	writeFingerprintBytes(h, []byte{0})
}

func writeFingerprintBytes(h hash.Hash, value []byte) {
	writeFingerprintUint32(h, uint32(len(value)))
	_, _ = h.Write(value)
}

func writeFingerprintUint32(h hash.Hash, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = h.Write(encoded[:])
}
