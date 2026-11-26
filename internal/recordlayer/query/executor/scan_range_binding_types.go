package executor

import (
	"fmt"
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
	components                 []boundRangeComponent
	tails                      []boundRangeTail
	terminalZeroIndex          int
	reverse                    bool
	empty                      bool
	fingerprintSalt            string
	compatibleFingerprintSalts []string
	// keyFilter is set when a NaN equality is followed by constrained
	// components: the ranges are the NaN blocks over the prefix, and each
	// entry's later key components must satisfy their comparisons.
	keyFilter *scanKeyFilter
}

// scanKeyFilter is the per-entry predicate a NaN equality's later scan
// components become (RFC-257 WS-E 5.3). The two NaN key blocks hold every NaN
// payload, and a component after them is ordered within each payload only, so
// it cannot be a range: each entry read is tested on its decoded key component
// with the comparison's own semantics (NaN-equal and zero-widened, as every
// predicate). It runs below the continuation: a rejected entry is read,
// counted toward the scan's limits, and never re-read on resume.
type scanKeyFilter struct {
	components []scanKeyFilterComponent
}

type scanKeyFilterComponent struct {
	// position is the key component's index among the scan comparisons.
	position    int
	comparisons []predicates.Comparison
}

// admits reports whether the entry key passes every filtered component.
// offset is where the comparisons' components start in key (a primary key
// with a record-type prefix carries that prefix first).
func (f *scanKeyFilter) admits(key tuple.Tuple, offset int) (bool, error) {
	for _, component := range f.components {
		at := offset + component.position
		if at >= len(key) {
			return false, fmt.Errorf("scan key filter: key %v has no component %d", key, at)
		}
		value := values.TupleElementToRowValue(key[at])
		for _, comparison := range component.comparisons {
			result, err := comparison.Eval(value)
			if err != nil {
				return false, recordlayer.TranslateTextCmpError(err)
			}
			if result != predicates.TriTrue {
				return false, nil
			}
		}
	}
	return true, nil
}

// bindScanKeyFilter evaluates the comparisons after a NaN equality once, at
// binding, as the scan's other comparands are. A NULL comparand of an ordinary
// comparison admits no entry, so the scan is empty.
func bindScanKeyFilter(
	comparisons []*predicates.ComparisonRange,
	first int,
	binder values.ParameterBinder,
) (filter *scanKeyFilter, empty bool, err error) {
	filter = &scanKeyFilter{}
	for j := first; j < len(comparisons); j++ {
		cr := comparisons[j]
		if cr == nil || cr.IsEmpty() {
			continue
		}
		component := scanKeyFilterComponent{position: j}
		for _, comparison := range cr.GetComparisons() {
			if comparison == nil {
				return nil, false, fmt.Errorf("scan component %d has a nil comparison", j)
			}
			bound := *comparison
			if !comparison.Type.IsUnary() {
				if comparison.Operand == nil {
					return nil, false, fmt.Errorf("scan component %d (%v) has no operand", j, comparison.Type)
				}
				value, err := comparison.Operand.Evaluate(binder)
				if err != nil {
					return nil, false, err
				}
				if value == nil && comparison.Type != predicates.ComparisonNotDistinctFrom &&
					comparison.Type != predicates.ComparisonIsDistinctFrom {
					return nil, true, nil
				}
				bound.Operand = &values.ConstantValue{Value: value, Typ: comparison.Operand.Type()}
			}
			component.comparisons = append(component.comparisons, bound)
		}
		filter.components = append(filter.components, component)
	}
	return filter, false, nil
}

// keyFilterOperandIdentity is a filter comparand as the comparison sees it:
// every NaN is one value and both zeros are one, so a resume under an
// equivalent comparand is the same scan.
func keyFilterOperandIdentity(comparison predicates.Comparison) any {
	c, ok := comparison.Operand.(*values.ConstantValue)
	if !ok || c == nil {
		return nil
	}
	switch v := c.Value.(type) {
	case float64:
		if math.IsNaN(v) {
			return "NaN"
		}
		if v == 0 {
			return float64(0)
		}
	case float32:
		if math.IsNaN(float64(v)) {
			return "NaN"
		}
		if v == 0 {
			return float32(0)
		}
	}
	return c.Value
}
