package recordlayer

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// TranslatePrimaryKeyToValues translates a common primary-key KeyExpression into
// a flat []values.Value that encodes STRUCTURE — record-type-key prefixes
// (RecordTypeValue) and nesting (FieldValue.Child chains) — the Go analog of
// Java's ScalarTranslationVisitor.translateKeyExpression used by
// PrimaryKeyProperty.visitIndexPlan (RFC-189 B3).
//
// PK identity must compare by STRUCTURE, not by bare column names: Field("ID")
// and Concat(RecordTypeKey(), Field("ID")) share the flat name list ["ID"] but
// are DIFFERENT primary keys, and conflating them lets ImplementDistinctUnionRule
// dedup two union legs that must both survive (dropped rows — the exact hazard
// the by-name M5 was reverted for). The structural translation makes them
// unequal under values.ValuesStructurallyEqual.
//
// Returns nil if any component is not translatable (fan-out, version, function,
// or any unrecognized key) — fail-safe: the property then abstains, disabling
// the dedup optimization rather than risking a wrong one. The base of every
// leaf is nil (the flat field-reference model), so the translation is
// self-consistent and independent of any quantifier alias.
func TranslatePrimaryKeyToValues(pk KeyExpression) []values.Value {
	if pk == nil {
		return nil
	}
	normalized := normalizeKeyForPositions(pk)
	if len(normalized) == 0 {
		return nil
	}
	result := make([]values.Value, 0, len(normalized))
	for _, ke := range normalized {
		v := translateKeyComponent(ke, nil)
		if v == nil {
			return nil
		}
		result = append(result, v)
	}
	return result
}

// translateKeyComponent maps a single normalized key-position expression to a
// structure-encoding Value over `base` (nil = the record root). Returns nil for
// any component whose scan identity is not a plain structural field path.
func translateKeyComponent(ke KeyExpression, base values.Value) values.Value {
	switch e := ke.(type) {
	case *FieldKeyExpression:
		if e.fanType != FanTypeNone {
			return nil
		}
		return &values.FieldValue{Field: e.fieldName, Typ: values.UnknownType, Child: base}
	case *RecordTypeKeyExpression:
		return values.NewRecordTypeValue(base)
	case *NestingKeyExpression:
		if e.fanType != FanTypeNone {
			return nil
		}
		nestedBase := &values.FieldValue{Field: e.parentField, Typ: values.UnknownType, Child: base}
		return translateKeyComponent(e.child, nestedBase)
	default:
		return nil
	}
}
