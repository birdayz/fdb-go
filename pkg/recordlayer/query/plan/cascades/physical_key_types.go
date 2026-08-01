package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// matchCandidateBindingEligibility is an optional fail-closed gate for access
// paths that cannot compensate a matched predicate. Aggregate scans and
// self-limiting partitioned vector scans use it to decline visible constant
// NaN equalities rather than emit an incomplete exact-payload probe.
type matchCandidateBindingEligibility interface {
	bindingRangesEligible(map[values.CorrelationIdentifier]*predicates.ComparisonRange) bool
}

func candidateBindingRangesEligible(
	candidate MatchCandidate,
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) bool {
	eligible, ok := candidate.(matchCandidateBindingEligibility)
	return !ok || eligible.bindingRangesEligible(bindings)
}

func bindingsContainKnownConstantNaN(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	aliases []values.CorrelationIdentifier,
	physicalTypes []values.Type,
) bool {
	for i, alias := range aliases {
		physicalType := values.Type(values.UnknownType)
		if i < len(physicalTypes) && physicalTypes[i] != nil {
			physicalType = physicalTypes[i]
		}
		if properties.ComparisonRangeHasUnsupportedKnownNaN(bindings[alias], physicalType) {
			return true
		}
	}
	return false
}

func candidateRangeHasKnownConstantNaN(
	cr *predicates.ComparisonRange,
	physicalTypes []values.Type,
	position int,
) bool {
	physicalType := values.Type(values.UnknownType)
	if position >= 0 && position < len(physicalTypes) && physicalTypes[position] != nil {
		physicalType = physicalTypes[position]
	}
	return properties.ComparisonRangeHasUnsupportedKnownNaN(cr, physicalType)
}

// candidatePhysicalKeyTypeKnown reports whether metadata establishes the
// tuple-wire domain at one candidate coordinate. An Unknown component can
// mean that multiple record types sharing an index disagree (for example,
// FLOAT versus DOUBLE). In that case the RHS declaration is not a safe
// substitute: binding one convenient width would silently omit entries stored
// with the other width.
func candidatePhysicalKeyTypeKnown(physicalTypes []values.Type, position int) bool {
	return position >= 0 && position < len(physicalTypes) &&
		physicalTypes[position] != nil &&
		physicalTypes[position].Code() != values.TypeCodeUnknown
}

func bindingsUseUnknownPhysicalKeyType(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	aliases []values.CorrelationIdentifier,
	physicalTypes []values.Type,
) bool {
	for i, alias := range aliases {
		cr := bindings[alias]
		if cr == nil || cr.IsEmpty() {
			continue
		}
		if !candidatePhysicalKeyTypeKnown(physicalTypes, i) {
			return true
		}
	}
	return false
}

func candidateRangeHasUnsupportedPhysicalFloatOrdering(
	cr *predicates.ComparisonRange,
	physicalTypes []values.Type,
	position int,
) bool {
	if cr == nil || !cr.IsInequality() ||
		!candidatePhysicalKeyTypeKnown(physicalTypes, position) {
		return false
	}
	code := physicalTypes[position].Code()
	if code != values.TypeCodeFloat && code != values.TypeCodeDouble {
		return false
	}
	for _, comparison := range cr.GetInequalityComparisons() {
		if comparison == nil {
			continue
		}
		switch comparison.Type {
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			return true
		}
	}
	// IS NOT NULL is the exact exclusive-NULL boundary for every tuple carrier,
	// including all raw NaN encodings. STARTS_WITH is classified independently.
	return false
}

func bindingsContainUnsupportedPhysicalFloatOrdering(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	aliases []values.CorrelationIdentifier,
	physicalTypes []values.Type,
) bool {
	for i, alias := range aliases {
		if candidateRangeHasUnsupportedPhysicalFloatOrdering(bindings[alias], physicalTypes, i) {
			return true
		}
	}
	return false
}

// candidateRangeHasUnsupportedPhysicalStartsWith reports a range that would
// ask tuple PREFIX_STRING endpoints to implement STARTS_WITH over a non-STRING
// physical component. Comparison.Eval defines STARTS_WITH only for
// string/string operands; BYTES is deliberately not included, and the
// string-carrier implementation detail of DATE/TIMESTAMP does not make either
// one a logical string key. A mixed inequality range containing STARTS_WITH is
// also not one representable prefix-string range.
func candidateRangeHasUnsupportedPhysicalStartsWith(
	cr *predicates.ComparisonRange,
	physicalTypes []values.Type,
	position int,
) bool {
	if cr == nil || !cr.IsInequality() {
		return false
	}
	inequalities := cr.GetInequalityComparisons()
	hasStartsWith := false
	for _, comparison := range inequalities {
		if comparison != nil && comparison.Type == predicates.ComparisonStartsWith {
			hasStartsWith = true
			break
		}
	}
	if !hasStartsWith {
		return false
	}
	return len(inequalities) != 1 ||
		!candidatePhysicalKeyTypeKnown(physicalTypes, position) ||
		physicalTypes[position].Code() != values.TypeCodeString
}

func bindingsContainUnsupportedPhysicalStartsWith(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	aliases []values.CorrelationIdentifier,
	physicalTypes []values.Type,
) bool {
	for i, alias := range aliases {
		if candidateRangeHasUnsupportedPhysicalStartsWith(bindings[alias], physicalTypes, i) {
			return true
		}
	}
	return false
}

// physicalEqualityShapeAt routes every candidate-side equality decision
// through the planner's shared physical-key classifier. A missing type is
// represented explicitly as Unknown and remains non-fixed even for a visible
// literal or statically typed parameter: comparand metadata cannot establish
// the tuple carrier of a shared or incompletely stamped physical key.
func physicalEqualityShapeAt(
	cr *predicates.ComparisonRange,
	physicalTypes []values.Type,
	position int,
) properties.PhysicalEqualityShape {
	physicalType := values.UnknownType
	if position >= 0 && position < len(physicalTypes) && physicalTypes[position] != nil {
		physicalType = physicalTypes[position]
	}
	return properties.PhysicalEqualityShapeForComparisons(
		[]*predicates.ComparisonRange{cr},
		[]values.Type{physicalType},
		false,
	)
}

// candidatePhysicalEqualityShape resolves a matched parameter back to the
// candidate key coordinate that owns it. Candidate metadata is authoritative:
// index/primary/aggregate candidates expose GetKeyComponentTypes, while vector
// candidates expose their shorter partition-key type vector. Hand-built or
// legacy candidates without aligned metadata fail through Unknown rather than
// guessing a neighboring key component.
func candidatePhysicalEqualityShape(
	candidate MatchCandidate,
	alias values.CorrelationIdentifier,
	cr *predicates.ComparisonRange,
) properties.PhysicalEqualityShape {
	if candidate == nil {
		return physicalEqualityShapeAt(cr, nil, -1)
	}
	aliases := candidate.GetSargableAliases()
	position := -1
	for i, candidateAlias := range aliases {
		if candidateAlias == alias {
			position = i
			break
		}
	}

	var physicalTypes []values.Type
	if typed, ok := candidate.(interface{ GetKeyComponentTypes() []values.Type }); ok {
		physicalTypes = typed.GetKeyComponentTypes()
	} else if typed, ok := candidate.(interface{ GetPartitionKeyComponentTypes() []values.Type }); ok {
		physicalTypes = typed.GetPartitionKeyComponentTypes()
	}
	return physicalEqualityShapeAt(cr, physicalTypes, position)
}

// normalizePhysicalKeyTypes returns an explicit, fixed-width type vector.
// Unknown metadata is represented by UnknownType rather than a missing entry,
// so every consumer indexes the same coordinate system as the comparisons.
func normalizePhysicalKeyTypes(types []values.Type, size int) []values.Type {
	if size <= 0 {
		return nil
	}
	result := make([]values.Type, size)
	for i := range result {
		result[i] = values.UnknownType
		if i < len(types) && types[i] != nil {
			result[i] = types[i]
		}
	}
	return result
}

// physicalTypesFromFlatRow is the compatibility derivation for hand-built
// candidates whose flowed row already carries concrete field types. Production
// metadata supplies key-expression-derived types explicitly; this fallback is
// deliberately unique-name-only and leaves ambiguous or expression keys
// Unknown instead of guessing from a display name.
func physicalTypesFromFlatRow(
	rowType values.Type,
	columnNames []string,
	columnFunctions []string,
) []values.Type {
	result := normalizePhysicalKeyTypes(nil, len(columnNames))
	recordType, ok := rowType.(*values.RecordType)
	if !ok {
		return result
	}
	for i, columnName := range columnNames {
		if i < len(columnFunctions) && columnFunctions[i] != "" {
			if columnFunctions[i] == FunctionKindCardinality {
				result[i] = values.NullableInt
			}
			continue
		}
		var found values.Type
		matches := 0
		for _, field := range recordType.Fields {
			if strings.EqualFold(field.Name, columnName) {
				found = field.FieldType
				matches++
			}
		}
		if matches == 1 && found != nil {
			result[i] = found
		}
	}
	return result
}
