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

// KeyUniquenessSemantics states which storage invariant a full equality bind
// may rely on. Primary keys are unique over the complete packed tuple.
// Secondary UNIQUE indexes use SQL's default NULLS DISTINCT semantics: the
// uniqueness check is skipped when any indexed component is NULL, so one NULL
// prefix can have arbitrarily many entries distinguished by their appended PK.
type KeyUniquenessSemantics uint8

const (
	TupleKeyUnique KeyUniquenessSemantics = iota
	SecondaryUniqueNullsDistinct
)

// TupleKeyUniquenessMatchesLogicalEquality reports whether uniqueness of a raw
// FDB tuple key implies uniqueness under the engine's logical row-equality
// relation. The type vector must be authoritative and exactly aligned with the
// whole key. FLOAT/DOUBLE fail because FDB preserves distinct raw NaN
// sign/payload encodings while logical DISTINCT canonicalizes every NaN. (The
// logical row comparator does distinguish signed zero; predicate `=` is the
// operation that equates the zero signs.) Nullable components are safe here:
// a primary tuple key can contain only one raw nil value at a coordinate.
func TupleKeyUniquenessMatchesLogicalEquality(
	physicalTypes []values.Type,
	keyColumnCount int,
) bool {
	if keyColumnCount <= 0 || len(physicalTypes) != keyColumnCount {
		return false
	}
	for i := 0; i < keyColumnCount; i++ {
		if !ValueIdentityMatchesLogicalEquality(physicalTypes[i]) {
			return false
		}
	}
	return true
}

// ValueIdentityMatchesLogicalEquality reports whether the runtime's lossless
// physical identity for a value agrees with SQL row equality. It is recursive
// because ARRAY/STRUCT group keys and DISTINCT rows can contain floating values
// below the top level. Unknown/ANY types fail closed: a visible literal or Go
// carrier cannot substitute for missing authoritative metadata.
func ValueIdentityMatchesLogicalEquality(typ values.Type) bool {
	if typ == nil {
		return false
	}
	switch typed := typ.(type) {
	case *values.RecordType:
		for _, field := range typed.Fields {
			if !ValueIdentityMatchesLogicalEquality(field.FieldType) {
				return false
			}
		}
		return true
	case *values.ArrayType:
		return typed.ElementType != nil && ValueIdentityMatchesLogicalEquality(typed.ElementType)
	case *values.RelationType:
		// Relations are streams, not scalar key coordinates.
		return false
	}
	switch typ.Code() {
	case values.TypeCodeUnknown, values.TypeCodeAny,
		values.TypeCodeFloat, values.TypeCodeDouble,
		values.TypeCodeRecord, values.TypeCodeArray, values.TypeCodeRelation:
		return false
	default:
		return true
	}
}

// SecondaryUniqueKeyGloballyEnforced reports whether a secondary UNIQUE
// declaration is a true uniqueness invariant over every stored index entry.
// Under the engine's NULLS DISTINCT policy, that additionally requires
// authoritative NOT NULL metadata for every indexed key component.
// This stronger global proof is used for DISTINCT elimination and
// strict-ordering claims; point probes use ProvenFullEqualityMultiplicity,
// which can still prove individual supported probes on a nullable/floating
// unique index.
func SecondaryUniqueKeyGloballyEnforced(physicalTypes []values.Type, keyColumnCount int) bool {
	if !TupleKeyUniquenessMatchesLogicalEquality(physicalTypes, keyColumnCount) {
		return false
	}
	for i := 0; i < keyColumnCount; i++ {
		physicalType := physicalEqualityTypeAt(physicalTypes, i)
		if physicalType.IsNullable() {
			return false
		}
	}
	return true
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

// ProvenFullEqualityMultiplicity returns the finite physical-row upper bound
// established by a complete equality bind under the stated uniqueness
// semantics. It is the shared cardinality authority for primary-key and
// secondary-UNIQUE probes; ordering deliberately continues to use
// PhysicalEqualityShape because one NULL prefix is physically fixed even when
// it is not unique.
//
// For a NULLS DISTINCT secondary index, ordinary `= NULL` remains safely empty.
// IS NULL, static `IS NOT DISTINCT FROM NULL`, and a nullable dynamic null-safe
// equality decline when the physical key can contain NULL. A NOT NULL target
// makes the static NULL-only forms empty and keeps a dynamic null-safe equality
// safe: NULL yields no rows, while every non-NULL value is uniqueness-checked.
func ProvenFullEqualityMultiplicity(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	keyColumnCount int,
	semantics KeyUniquenessSemantics,
) Cardinality {
	if !EqualityBoundsCoverKey(comps, keyColumnCount) {
		return UnknownCardinality()
	}
	shape := PhysicalEqualityShapeForComparisons(comps, physicalTypes, true)
	if shape.UnsupportedKnownNaN || shape.ProvenRowMultiplicity.IsUnknown() {
		return UnknownCardinality()
	}
	if shape.Unsatisfiable {
		return OfCardinality(0)
	}
	if semantics != SecondaryUniqueNullsDistinct {
		return shape.ProvenRowMultiplicity
	}
	if len(physicalTypes) < keyColumnCount || len(comps) < keyColumnCount {
		return UnknownCardinality()
	}
	for i := 0; i < keyColumnCount; i++ {
		physicalType := physicalEqualityTypeAt(physicalTypes, i)
		if physicalEqualityTypeUnknown(physicalType) {
			return UnknownCardinality()
		}
		comparison := comps[i].GetEqualityComparison()
		if comparison == nil {
			return UnknownCardinality()
		}
		switch comparison.Type {
		case predicates.ComparisonIsNull:
			if physicalType.IsNullable() {
				return UnknownCardinality()
			}
			return OfCardinality(0)
		case predicates.ComparisonNotDistinctFrom:
			if comparison.Operand == nil {
				return UnknownCardinality()
			}
			constant, known := values.EvaluateConstant(comparison.Operand)
			if known {
				if constant == nil {
					if physicalType.IsNullable() {
						return UnknownCardinality()
					}
					return OfCardinality(0)
				}
				continue
			}
			operandType := comparison.Operand.Type()
			if physicalType.IsNullable() &&
				(physicalEqualityTypeUnknown(operandType) || operandType.IsNullable()) {
				return UnknownCardinality()
			}
		}
	}
	return shape.ProvenRowMultiplicity
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

// LogicalEqualityAtMostOnePhysicalKey reports whether evaluating cr as a
// row-level logical equality over a UNIQUE physical key can match at most one
// stored key. This is intentionally a different proof from
// PhysicalEqualityShapeForComponent.
//
// A scan range has an exact-or-loud binder: for example, a FLOAT/DOUBLE
// comparand over an integer key is rejected at runtime when its cmpAny
// equality class contains several integers. A residual filter or materialized
// nested-loop join has no such binder. It evaluates cmpAny against every row,
// so raw physical uniqueness is useful only when the complete logical
// equality class is provably a singleton (or empty).
//
// The two important non-singleton classes are:
//   - both signed-zero encodings and every raw NaN encoding in a physical
//     FLOAT/DOUBLE key; and
//   - adjacent physical integers that round to one FLOAT/DOUBLE comparand at
//     the binary64 precision cliff.
//
// Unknown physical metadata and dynamic shapes that can reach either class
// fail closed. A constant non-zero, non-NaN floating equality remains useful:
// at most one physical FLOAT/DOUBLE encoding has that logical value.
func LogicalEqualityAtMostOnePhysicalKey(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
) bool {
	if cr == nil || !cr.IsEquality() {
		return false
	}
	comparison := cr.GetEqualityComparison()
	if comparison == nil {
		return false
	}
	if comparison.Type == predicates.ComparisonIsNull {
		return true
	}
	if comparison.Type != predicates.ComparisonEquals &&
		comparison.Type != predicates.ComparisonNotDistinctFrom {
		return false
	}
	if comparison.Operand == nil {
		return false
	}

	constant, isConstant := values.EvaluateConstant(comparison.Operand)
	if isConstant && constant == nil {
		// Ordinary equality is unsatisfiable and null-safe equality selects the
		// one NULL tuple key. Neither needs a non-null carrier decision.
		return true
	}
	if physicalEqualityTypeUnknown(physicalType) {
		return false
	}

	physicalCode := physicalType.Code()
	if isConstant {
		floating, isFloating, isNumeric := values.ToFloat64(constant)
		switch physicalCode {
		case values.TypeCodeFloat, values.TypeCodeDouble:
			if !isNumeric {
				return false
			}
			// Every finite non-zero value has at most one physical float
			// representation in a fixed width. Both zero signs compare equal;
			// every NaN sign/payload compares equal under the logical total order.
			return floating != 0 && !math.IsNaN(floating)
		case values.TypeCodeInt, values.TypeCodeLong:
			if !isNumeric {
				return false
			}
			if !isFloating {
				return true
			}
			return integerEqualityClassAtMostOne(floating)
		default:
			operandType := comparison.Operand.Type()
			return operandType != nil &&
				operandType.Code() != values.TypeCodeUnknown &&
				operandType.Code() == physicalCode
		}
	}

	operandType := comparison.Operand.Type()
	if operandType == nil || operandType.Code() == values.TypeCodeUnknown {
		return false
	}
	operandCode := operandType.Code()
	switch physicalCode {
	case values.TypeCodeFloat, values.TypeCodeDouble:
		// Every numeric runtime domain contains zero; a physical floating key
		// can contain both signs. Non-numeric declarations are not used as a
		// type-mismatch proof here: a malformed hand-built plan fails closed.
		return false
	case values.TypeCodeInt, values.TypeCodeLong:
		// Pure integer comparison is exact. A dynamic floating comparand can
		// acquire a plural integer inverse at runtime.
		return operandCode == values.TypeCodeInt || operandCode == values.TypeCodeLong
	default:
		return operandCode == physicalCode
	}
}

// LogicalEqualityProjectionInjective reports whether distinct values of a
// source key are guaranteed to induce disjoint equality probes in the stated
// physical target domain. It is stronger than
// LogicalEqualityAtMostOnePhysicalKey: the latter bounds one probe's result;
// this one proves that the results of many correlated probes cannot overlap.
//
// Floating source keys always decline because distinct physical signed-zero
// and NaN encodings are already one logical equality value. Integer-to-FLOAT
// narrowing and LONG-to-DOUBLE conversion also decline because distinct
// integers can round to one target value. INT-to-DOUBLE is injective over the
// INT/uint32 domain; integer tuple carriers are mutually exact.
func LogicalEqualityProjectionInjective(sourceType, physicalTargetType values.Type) bool {
	if physicalEqualityTypeUnknown(sourceType) || physicalEqualityTypeUnknown(physicalTargetType) {
		return false
	}
	sourceCode := sourceType.Code()
	targetCode := physicalTargetType.Code()
	if sourceCode == values.TypeCodeFloat || sourceCode == values.TypeCodeDouble {
		return false
	}
	if targetCode == values.TypeCodeInt || targetCode == values.TypeCodeLong {
		return sourceCode == values.TypeCodeInt || sourceCode == values.TypeCodeLong
	}
	if targetCode == values.TypeCodeFloat || targetCode == values.TypeCodeDouble {
		return sourceCode == values.TypeCodeInt && targetCode == values.TypeCodeDouble
	}
	return sourceCode == targetCode
}

// integerEqualityClassAtMostOne is the exact inverse-size test for cmpAny's
// mixed integer/float equality over the full signed tuple-integer domain. The
// signed-to-unsigned rank transform lets the binary search cover MinInt64
// through MaxInt64 without overflowing a midpoint. Once the first equal
// integer is known, inspecting its successor is sufficient because the
// integer-to-float64 mapping is monotone.
func integerEqualityClassAtMostOne(floating float64) bool {
	const signBit = uint64(1) << 63
	domainLow, domainHigh := int64(math.MinInt64), int64(math.MaxInt64)
	lowRank := uint64(domainLow) ^ signBit
	highRank := uint64(domainHigh) ^ signBit
	predicate := func(integer int64) bool {
		return compareIntegerToLogicalFloat(integer, floating) >= 0
	}
	for lowRank < highRank {
		middleRank := lowRank + (highRank-lowRank)/2
		middle := int64(middleRank ^ signBit)
		if predicate(middle) {
			highRank = middleRank
		} else {
			lowRank = middleRank + 1
		}
	}
	first := int64(lowRank ^ signBit)
	if !predicate(first) || compareIntegerToLogicalFloat(first, floating) != 0 {
		return true // empty equality class
	}
	return first == math.MaxInt64 || compareIntegerToLogicalFloat(first+1, floating) != 0
}

func compareIntegerToLogicalFloat(integer int64, floating float64) int {
	integerAsFloat := float64(integer)
	if integerAsFloat == floating {
		return 0
	}
	return values.CompareFloat64(integerAsFloat, floating)
}

// keyOrderingCongruence answers two questions about one key coordinate, and the
// SECOND one is what a bare boolean gets wrong.
//
// Congruent says physical tuple order at this coordinate agrees with the query
// comparator for the rows the scan selects. Terminal says the agreement does
// NOT extend to the coordinates after it, so the ordering claim must stop here.
//
// Terminality exists because of the NaN tie class. Every NaN is ONE logical
// value, but it is spread over many distinct physical keys in two separate
// blocks. Within that tie class the next coordinate therefore comes back in
// NaN-PAYLOAD order rather than its own. Concretely, for an index on (v, w):
//
//	ORDER BY v, w  over rows with several NaN payloads
//	physical: nan(p1)/1 nan(p1)/2 nan(p2)/1 nan(p2)/2
//	logical:  nan/1     nan/1     nan/2     nan/2
//
// so claiming w is ordered under a NaN-relaxed v silently returns rows in the
// wrong order. The relaxation is sound FOR v and false for everything after it.
//
// This is corroborated by the binder: bindScanComparisonsToRangeSet splits
// exactly ONE coordinate, so binding a range set can never license a claim
// beyond the first non-equality coordinate.
type keyOrderingCongruence struct {
	Congruent bool
	Terminal  bool
}

// floatOrderedBounds classifies the ordered bounds on a float coordinate.
func floatOrderedBounds(cr *predicates.ComparisonRange) (hasLow, hasHigh bool) {
	if cr == nil {
		return false, false
	}
	for _, comparison := range cr.GetInequalityComparisons() {
		if comparison == nil {
			continue
		}
		switch comparison.Type {
		case predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			hasLow = true
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq:
			hasHigh = true
		}
	}
	return hasLow, hasHigh
}

func keyOrderingCongruenceAt(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
	rangeSetCapable bool,
) keyOrderingCongruence {
	if cr != nil && cr.IsEquality() {
		component := PhysicalEqualityShapeForComponent(cr, physicalType, false)
		congruent := component.LogicalEquality &&
			!component.UnsupportedKnownNaN &&
			(component.PhysicallyFixed || component.MayFanOut || component.Unsatisfiable)
		return keyOrderingCongruence{Congruent: congruent}
	}
	if physicalEqualityTypeUnknown(physicalType) {
		return keyOrderingCongruence{}
	}
	if physicalType.Code() != values.TypeCodeFloat &&
		physicalType.Code() != values.TypeCodeDouble {
		return keyOrderingCongruence{Congruent: true}
	}
	if cr != nil && cr.IsInequality() {
		if ComparisonRangeHasUnsupportedKnownNaN(cr, physicalType) {
			return keyOrderingCongruence{}
		}
		hasLow, hasHigh := floatOrderedBounds(cr)
		switch {
		case hasLow && hasHigh:
			// A two-sided finite range contains NO NaN at all, so there is no
			// tie class and physical order agrees with logical order for this
			// coordinate AND everything after it.
			return keyOrderingCongruence{Congruent: true}
		case hasLow || hasHigh:
			// One-sided: an upper bound is one range starting at -Inf; a lower
			// bound is the numeric range followed by the negative-NaN block.
			// Both are correct FOR THIS COORDINATE only.
			return keyOrderingCongruence{Congruent: true, Terminal: true}
		default:
			// IS NOT NULL alone. The binder emits ONE raw range for it — no
			// block split — so the negative NaNs come back first. Not congruent.
			return keyOrderingCongruence{}
		}
	}
	// UNBOUND. Only a leaf that enumerates a physical range set splits the
	// coordinate into NULL / numbers / the two NaN blocks and returns them in
	// logical order, and even then the claim stops here.
	if rangeSetCapable && (cr == nil || cr.IsEmpty()) {
		return keyOrderingCongruence{Congruent: true, Terminal: true}
	}
	return keyOrderingCongruence{}
}

// PhysicalKeyOrderingCongruent reports whether FDB tuple order at one key
// coordinate is congruent with the query comparator for the rows selected by
// cr. It does NOT say whether the following coordinates are also ordered; use
// PhysicalOrderingPrefixLength, which honours terminality.
func PhysicalKeyOrderingCongruent(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
) bool {
	return keyOrderingCongruenceAt(cr, physicalType, false).Congruent
}

// PhysicalKeyOrderingCongruentForRangeSet is PhysicalKeyOrderingCongruent for a
// leaf that binds through the physical RANGE SET, which is what a value index
// and a primary scan do.
//
// The single difference is the unbound float coordinate. Such a leaf splits it
// into NULL, the numbers, and the two NaN blocks and concatenates them in
// logical order, so the ordering it reports is real. A leaf that cannot
// enumerate several ranges must keep using PhysicalKeyOrderingCongruent, or it
// will promise an ORDER BY it delivers with the negative NaNs in the wrong
// place.
func PhysicalKeyOrderingCongruentForRangeSet(
	cr *predicates.ComparisonRange,
	physicalType values.Type,
) bool {
	return keyOrderingCongruenceAt(cr, physicalType, true).Congruent
}

// PhysicalOrderingPrefixLength returns the largest leading key prefix whose
// physical tuple order is a valid ORDER BY ordering. ORDER BY uses the total
// float comparator, which distinguishes -0 from +0 in the same order as tuple
// encoding; a signed-zero equality can therefore retain (v, suffix), while its
// non-fixed shape still prevents deleting v and claiming suffix-only order.
// PhysicalOrderingPrefixLengthForRangeSet is PhysicalOrderingPrefixLength for
// a plan that scans through the physical RANGE SET — a value index scan or a
// primary scan. Such a plan splits an unbound float coordinate into NULL, the
// numbers and the two NaN blocks and returns them in logical order, so that
// coordinate does not end its usable ordering prefix.
//
// An aggregate index plan reads a pre-aggregated stream and must keep using
// PhysicalOrderingPrefixLength, or it will advertise an ordering it cannot
// produce.
func PhysicalOrderingPrefixLengthForRangeSet(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	n int,
) int {
	return physicalOrderingPrefixLength(comps, physicalTypes, n, true)
}

func PhysicalOrderingPrefixLength(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	n int,
) int {
	return physicalOrderingPrefixLength(comps, physicalTypes, n, false)
}

// physicalOrderingPrefixLength walks the key and stops at the FIRST coordinate
// that is either incongruent (excluded) or terminal (included, but nothing
// after it). Returning n whenever every coordinate is congruent is the bug this
// replaces: a coordinate congruent only through the NaN tie class orders
// itself and scrambles its successors.
func physicalOrderingPrefixLength(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	n int,
	rangeSetCapable bool,
) int {
	for i := 0; i < n; i++ {
		physicalType := physicalEqualityTypeAt(physicalTypes, i)
		var cr *predicates.ComparisonRange
		if i < len(comps) {
			cr = comps[i]
		}
		congruence := keyOrderingCongruenceAt(cr, physicalType, rangeSetCapable)
		if !congruence.Congruent {
			return i
		}
		if congruence.Terminal {
			return i + 1
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
