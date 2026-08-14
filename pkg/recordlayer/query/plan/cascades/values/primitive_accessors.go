package values

import "fmt"

// IncompatibleOrderingTypeError mirrors Java's
// SemanticException.ErrorCode.ORDERING_IS_OF_INCOMPATIBLE_TYPE: the type
// cannot participate in an ordering (and therefore cannot be a grouping
// requirement) even after record flattening — arrays and relations have no
// primitive leaf decomposition.
type IncompatibleOrderingTypeError struct {
	Typ Type
}

func (e *IncompatibleOrderingTypeError) Error() string {
	return fmt.Sprintf("ordering is of incompatible type %v", e.Typ)
}

// PrimitiveAccessorsForType ports Values.primitiveAccessorsForType
// (Values.java:99-121): recurse a RECORD type into accessors for its
// primitive leaf elements, in declared field order. For the flowed record
// ((a, b) as x, c as y) with primitive a, b, c it returns {_.x.a, _.x.b,
// _.y}. A non-record type yields the base value itself — Java asserts
// isPrimitive()||isEnum() there and throws
// ORDERING_IS_OF_INCOMPATIBLE_TYPE otherwise; Go rejects the two codes
// with no leaf decomposition (ARRAY, RELATION) and otherwise admits,
// including UNKNOWN: an untyped key (a bound parameter, an internal
// expression) is not evidence of an unorderable one, the same leniency
// sortKeysAreOrderable applies on the ORDER BY path.
//
// This flattening is what makes GROUP BY <struct> answerable: the grouping
// path expresses its pre-aggregate ordering requirement over these
// primitive leaves (ImplementStreamingAggregationRule.java:111,
// GroupByExpression.java:434,
// PushRequestedOrderingThroughGroupByRule.java:141), so no comparator ever
// orders whole record values.
//
// base is a supplier, exactly like Java's baseValueSupplier: each leaf
// accessor chain is built over a fresh base invocation. Go Values are
// immutable and structurally shared, so suppliers returning the same node
// are fine.
func PrimitiveAccessorsForType(typ Type, base func() Value) ([]Value, error) {
	if _, erasedRecord := typ.(anyRecordType); erasedRecord {
		return nil, &IncompatibleOrderingTypeError{Typ: typ}
	}
	rt, isRecord := typ.(*RecordType)
	if !isRecord {
		if typ != nil {
			switch typ.Code() {
			case TypeCodeArray, TypeCodeRelation:
				return nil, &IncompatibleOrderingTypeError{Typ: typ}
			}
		}
		return []Value{base()}, nil
	}
	out := make([]Value, 0, len(rt.Fields))
	for i := range rt.Fields {
		request, err := FieldByOrdinal(i)
		if err != nil {
			return nil, err
		}
		// Resolve the ordinal against the supplied base's exact type. The
		// recursive call accepts the admitted FieldValue and the public resolver
		// fuses the next step onto its QOV root.
		fv, err := ResolveFieldAccess(base(), []FieldRequest{request})
		if err != nil {
			return nil, err
		}
		sub, err := PrimitiveAccessorsForType(rt.Fields[i].FieldType, func() Value { return fv })
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}
