package properties

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PhysicalEqualityComponentShape describes how one logical equality binds an
// FDB tuple-key component. In particular, PhysicallyFixed is deliberately
// distinct from ComparisonRange.IsEquality: IEEE equality binds both tuple
// encodings of signed zero for a FLOAT or DOUBLE key.
type PhysicalEqualityComponentShape struct {
	LogicalEquality     bool
	MayFanOut           bool
	PhysicallyFixed     bool
	UnsupportedKnownNaN bool
	Unsatisfiable       bool

	// SuccessfulSeekUpperBound is the maximum number of physical ranges this
	// component opens for a non-NaN execution. It is one or two. A terminal
	// signed-zero equality can still be non-fixed while needing only one seek,
	// because the adjacent zero subtrees can be covered by one inclusive range.
	SuccessfulSeekUpperBound int64

	// ProvenRowMultiplicity is the maximum number of distinct physical keys a
	// UNIQUE/primary-key equality can match. Dynamic FLOAT/DOUBLE and genuinely
	// Unknown values deliberately remain unknown because their domain includes
	// the unsupported NaN equivalence class.
	ProvenRowMultiplicity Cardinality
}

// PhysicalEqualityShape is the checked product of the leading equality
// components in a comparison prefix. Components is aligned with the consumed
// leading equalities; the first non-equality ends the shape.
type PhysicalEqualityShape struct {
	Components           []PhysicalEqualityComponentShape
	EqualityPrefixLength int
	MayFanOut            bool
	PhysicallyFixed      bool
	UnsupportedKnownNaN  bool
	Unsatisfiable        bool

	// SuccessfulSeekUpperBound saturates at math.MaxInt64 when its exact
	// product is not representable. SuccessfulSeekUpperBoundExact distinguishes
	// that conservative cost value from an exact execution-count proof.
	SuccessfulSeekUpperBound      int64
	SuccessfulSeekUpperBoundExact bool

	// ProvenRowMultiplicity is unknown if any component cannot prove a finite
	// physical multiplicity or if checked multiplication overflows.
	ProvenRowMultiplicity Cardinality
}

// PhysicalEqualityShapeForComparisons classifies the contiguous leading
// equality prefix using authoritative physical key types. A known physical
// type wins over the RHS declaration; Unknown stays unknown. The comparand's
// declaration or Go representation cannot resolve missing metadata or a
// physical-width disagreement in a shared index. This is the one planner-side
// authority shared by ordering, cardinality, and cost.
//
// allowTerminalWidening is true for ordinary tuple scans, whose final signed
// zero can use one inclusive [-0,+0] range, and false for vector partitions,
// where each sign names a distinct graph that must be opened independently.
func PhysicalEqualityShapeForComparisons(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	allowTerminalWidening bool,
) PhysicalEqualityShape {
	shape := PhysicalEqualityShape{
		PhysicallyFixed:               true,
		SuccessfulSeekUpperBound:      1,
		SuccessfulSeekUpperBoundExact: true,
		ProvenRowMultiplicity:         OfCardinality(1),
	}

	for i, cr := range comps {
		if cr == nil || !cr.IsEquality() {
			break
		}
		terminalWidening := allowTerminalWidening && !laterComparisonConstrains(comps, i)
		component := PhysicalEqualityShapeForComponent(
			cr, physicalEqualityTypeAt(physicalTypes, i), terminalWidening,
		)
		shape.Components = append(shape.Components, component)
		shape.EqualityPrefixLength++
		shape.MayFanOut = shape.MayFanOut || component.MayFanOut
		shape.PhysicallyFixed = shape.PhysicallyFixed && component.PhysicallyFixed
		shape.UnsupportedKnownNaN = shape.UnsupportedKnownNaN || component.UnsupportedKnownNaN
		shape.Unsatisfiable = shape.Unsatisfiable || component.Unsatisfiable

		if component.SuccessfulSeekUpperBound > 0 {
			if shape.SuccessfulSeekUpperBound > math.MaxInt64/component.SuccessfulSeekUpperBound {
				shape.SuccessfulSeekUpperBound = math.MaxInt64
				shape.SuccessfulSeekUpperBoundExact = false
			} else if shape.SuccessfulSeekUpperBoundExact {
				shape.SuccessfulSeekUpperBound *= component.SuccessfulSeekUpperBound
			}
		}
		shape.ProvenRowMultiplicity = shape.ProvenRowMultiplicity.Times(component.ProvenRowMultiplicity)
	}

	if shape.UnsupportedKnownNaN {
		shape.ProvenRowMultiplicity = UnknownCardinality()
	}
	if shape.Unsatisfiable && !shape.UnsupportedKnownNaN {
		shape.ProvenRowMultiplicity = OfCardinality(0)
		// The binder can determine an empty result before storage. Costing keeps
		// one setup unit so an empty plural access is not modeled as free.
		shape.SuccessfulSeekUpperBound = 1
		shape.SuccessfulSeekUpperBoundExact = true
	}
	return shape
}

// PhysicalEqualityShapeForComponent classifies one ComparisonRange. The
// terminalWidening flag affects seek count only; a range spanning both signed
// zeros is still not physically fixed and cannot justify dropping that key
// from an ordering.
func PhysicalEqualityShapeForComponent(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
	terminalWidening bool,
) PhysicalEqualityComponentShape {
	unknown := PhysicalEqualityComponentShape{
		SuccessfulSeekUpperBound: 2,
		ProvenRowMultiplicity:    UnknownCardinality(),
	}
	if cr == nil || !cr.IsEquality() {
		return unknown
	}
	unknown.LogicalEquality = true
	cmp := cr.GetEqualityComparison()
	if cmp == nil {
		return unknown
	}
	if cmp.Type == predicates.ComparisonIsNull {
		return PhysicalEqualityComponentShape{
			LogicalEquality:          true,
			PhysicallyFixed:          true,
			SuccessfulSeekUpperBound: 1,
			ProvenRowMultiplicity:    OfCardinality(1),
		}
	}
	if cmp.Operand == nil {
		return unknown
	}

	constant, isConstant := values.EvaluateConstant(cmp.Operand)
	if isConstant && constant == nil {
		if cmp.Type == predicates.ComparisonEquals {
			return PhysicalEqualityComponentShape{
				LogicalEquality:          true,
				PhysicallyFixed:          true,
				Unsatisfiable:            true,
				SuccessfulSeekUpperBound: 1,
				ProvenRowMultiplicity:    OfCardinality(0),
			}
		}
		if cmp.Type == predicates.ComparisonNotDistinctFrom {
			return PhysicalEqualityComponentShape{
				LogicalEquality:          true,
				PhysicallyFixed:          true,
				SuccessfulSeekUpperBound: 1,
				ProvenRowMultiplicity:    OfCardinality(1),
			}
		}
	}
	if physicalEqualityTypeUnknown(physicalType) {
		// A non-null comparison without an authoritative physical domain is not
		// SARGable. Keep the shape non-fixed even when the RHS is a typed literal:
		// a shared FLOAT/DOUBLE index is the canonical counterexample.
		return unknown
	}
	effectiveType := physicalType

	usesFloatWire := physicalEqualityUsesFloatWire(effectiveType, constant, isConstant)
	if isConstant {
		if !usesFloatWire {
			return PhysicalEqualityComponentShape{
				LogicalEquality:          true,
				PhysicallyFixed:          true,
				SuccessfulSeekUpperBound: 1,
				ProvenRowMultiplicity:    OfCardinality(1),
			}
		}
		floating, ok := coercePhysicalEqualityFloat(constant, effectiveType)
		if !ok {
			// A constant that cannot be represented as the authoritative float
			// wire type is not a physical-fixedness proof. Type checking normally
			// prevents this shape; failing closed keeps hand-built plans sound.
			return unknown
		}
		if math.IsNaN(floating) {
			unknown.UnsupportedKnownNaN = true
			unknown.MayFanOut = true
			return unknown
		}
		if floating == 0 {
			seeks := int64(2)
			if terminalWidening {
				seeks = 1
			}
			return PhysicalEqualityComponentShape{
				LogicalEquality:          true,
				MayFanOut:                true,
				PhysicallyFixed:          false,
				SuccessfulSeekUpperBound: seeks,
				ProvenRowMultiplicity:    OfCardinality(2),
			}
		}
		// Finite nonzero values, subnormals, and infinities each have one
		// physical tuple representation after authoritative-width coercion.
		return PhysicalEqualityComponentShape{
			LogicalEquality:          true,
			PhysicallyFixed:          true,
			SuccessfulSeekUpperBound: 1,
			ProvenRowMultiplicity:    OfCardinality(1),
		}
	}

	if usesFloatWire || physicalEqualityTypeUnknown(effectiveType) {
		seeks := int64(2)
		if terminalWidening {
			seeks = 1
		}
		return PhysicalEqualityComponentShape{
			LogicalEquality:          true,
			MayFanOut:                true,
			PhysicallyFixed:          false,
			SuccessfulSeekUpperBound: seeks,
			ProvenRowMultiplicity:    UnknownCardinality(),
		}
	}

	// A dynamic value against an authoritative non-float key cannot acquire a
	// signed-zero branch. Its physical key remains fixed even when the RHS itself
	// was declared Unknown.
	return PhysicalEqualityComponentShape{
		LogicalEquality:          true,
		PhysicallyFixed:          true,
		SuccessfulSeekUpperBound: 1,
		ProvenRowMultiplicity:    OfCardinality(1),
	}
}

// PhysicalKeyOrderingCongruent reports whether FDB tuple order at one key
// coordinate is congruent with the query comparator for the rows selected by
// cr. Raw FLOAT/DOUBLE keys are not generally congruent: FDB preserves NaN
// sign/payload regions while the logical comparator canonicalizes all NaNs.
// A supported equality is safe because it restricts the coordinate to one
// logical value (including both signed-zero encodings); an unbound or ordered
// floating coordinate is not. Unknown non-null metadata also fails closed.
func PhysicalKeyOrderingCongruent(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
) bool {
	if cr != nil && cr.IsEquality() {
		component := PhysicalEqualityShapeForComponent(cr, physicalType, false)
		return component.LogicalEquality &&
			!component.UnsupportedKnownNaN &&
			(component.PhysicallyFixed || component.MayFanOut || component.Unsatisfiable)
	}
	if physicalEqualityTypeUnknown(physicalType) {
		return false
	}
	return physicalType.Code() != values.TypeCodeFloat &&
		physicalType.Code() != values.TypeCodeDouble
}

// PhysicalOrderingPrefixLength returns the largest leading key prefix whose
// physical tuple order is a valid ORDER BY ordering. ORDER BY uses the total
// float comparator, which distinguishes -0 from +0 in the same order as tuple
// encoding; a signed-zero equality can therefore retain (v, suffix), while its
// non-fixed shape still prevents deleting v and claiming suffix-only order.
func PhysicalOrderingPrefixLength(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	n int,
) int {
	for i := 0; i < n; i++ {
		physicalType := physicalEqualityTypeAt(physicalTypes, i)
		var cr *predicates.ComparisonRange
		if i < len(comps) {
			cr = comps[i]
		}
		if !PhysicalKeyOrderingCongruent(cr, physicalType) {
			return i
		}
	}
	return n
}

// ComparisonRangeHasUnsupportedKnownNaN reports whether a scan comparison
// contains a plan-time-known NaN that would be encoded on a FLOAT or DOUBLE
// physical key. This deliberately covers ordered comparisons as well as
// equality. FDB tuple order preserves NaN sign and payload regions, while the
// predicate comparator treats all NaNs as one total-order value, so no single
// raw-payload endpoint is an exact logical bound.
//
// Ordinary value/primary candidates use this result to leave the comparison
// as residual compensation. Physical leaves that cannot compensate (aggregate
// indexes and self-limiting vector partitions) decline the candidate entirely.
func ComparisonRangeHasUnsupportedKnownNaN(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
) bool {
	if cr == nil {
		return false
	}
	if cr.IsEquality() {
		return comparisonHasUnsupportedKnownNaN(cr.GetEqualityComparison(), physicalType)
	}
	if cr.IsInequality() {
		for _, comparison := range cr.GetInequalityComparisons() {
			if comparisonHasUnsupportedKnownNaN(comparison, physicalType) {
				return true
			}
		}
	}
	return false
}

func comparisonHasUnsupportedKnownNaN(
	comparison *predicates.Comparison,
	physicalType values.Type,
) bool {
	if comparison == nil || comparison.Operand == nil {
		return false
	}
	constant, known := values.EvaluateConstant(comparison.Operand)
	if !known {
		return false
	}
	effectiveType := effectivePhysicalEqualityType(physicalType, comparison.Operand.Type())
	if !physicalEqualityUsesFloatWire(effectiveType, constant, true) {
		return false
	}
	floating, ok := coercePhysicalEqualityFloat(constant, effectiveType)
	return ok && math.IsNaN(floating)
}

func physicalEqualityTypeAt(types []values.Type, i int) values.Type {
	if i < 0 || i >= len(types) {
		return values.UnknownType
	}
	if types[i] == nil {
		return values.UnknownType
	}
	return types[i]
}

func effectivePhysicalEqualityType(physicalType, operandType values.Type) values.Type {
	if !physicalEqualityTypeUnknown(physicalType) {
		return physicalType
	}
	if operandType == nil {
		return values.UnknownType
	}
	return operandType
}

func physicalEqualityTypeUnknown(t values.Type) bool {
	return t == nil || t.Code() == values.TypeCodeUnknown
}

func physicalEqualityUsesFloatWire(t values.Type, constant any, known bool) bool {
	if !physicalEqualityTypeUnknown(t) {
		return t.Code() == values.TypeCodeFloat || t.Code() == values.TypeCodeDouble
	}
	if !known {
		return false
	}
	switch constant.(type) {
	case float32, float64:
		return true
	default:
		return false
	}
}

func coercePhysicalEqualityFloat(v any, typ values.Type) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint:
		f = float64(n)
	case uint64:
		f = float64(n)
	default:
		return 0, false
	}
	if typ != nil && typ.Code() == values.TypeCodeFloat {
		return float64(float32(f)), true
	}
	return f, true
}

func laterComparisonConstrains(comps []*predicates.ComparisonRange, i int) bool {
	for _, cr := range comps[i+1:] {
		if cr != nil && !cr.IsEmpty() {
			return true
		}
	}
	return false
}
