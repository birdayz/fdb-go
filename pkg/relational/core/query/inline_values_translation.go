package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// translateInlineValues lowers a literal VALUES table directly to the same
// Explode leaf Java uses. The logical source freezes the public row type while
// retaining the collection Value itself; verify those two authorities still
// agree before publishing the physical leaf so a later mutation of the
// collection's ordinary Type graph cannot split logical and physical schemas.
func (t *cascadesTranslator) translateInlineValues(source *logical.LogicalInlineValues) expressions.RelationalExpression {
	if source == nil || source.CollectionValue() == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"inline VALUES source has no exact literal collection"))
		return nil
	}
	logicalType, err := ExactLogicalResultType(source, nil)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"inline VALUES source has no exact result row: %v", err))
		return nil
	}
	array, ok := source.CollectionValue().Type().(*values.ArrayType)
	if !ok || array.ElementType == nil || !array.ElementType.Equals(logicalType) {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"inline VALUES collection row %v disagrees with logical row %v",
			arrayElementType(array), logicalType))
		return nil
	}
	explode, err := expressions.NewExplodeExpressionWithOrdinality(source.CollectionValue(), false)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"inline VALUES collection cannot be exploded exactly: %v", err))
		return nil
	}
	if !explode.GetExplodeResultType().Equals(logicalType) {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"inline VALUES physical row %v disagrees with logical row %v",
			explode.GetExplodeResultType(), logicalType))
		return nil
	}
	return explode
}

func arrayElementType(array *values.ArrayType) values.Type {
	if array == nil {
		return nil
	}
	return array.ElementType
}

// findInlineValuesOwner resolves one visible source alias in the current FROM
// scope. CTE bodies are separate scopes, so only a CTE's visible Main is
// traversed. A duplicate match declines rather than selecting by walk order.
func findInlineValuesOwner(op logical.LogicalOperator, alias string) *logical.LogicalInlineValues {
	return logical.FindOwnerInlineValues(op, alias)
}

// inlineValuesArrayElementType classifies an owner-relative array path against
// the inline source's frozen exact row. It is the literal-row counterpart of
// descriptor-backed unnestArrayElementType: no metadata, text fallback, or
// Unknown placeholder participates.
func inlineValuesArrayElementType(
	owner *logical.LogicalInlineValues,
	fieldSegments []string,
) (elementType values.Type, fieldName string, isArray, fieldPresent bool) {
	if owner == nil || len(fieldSegments) == 0 {
		return values.UnknownType, "", false, false
	}
	current, ok := owner.ResultType().(*values.RecordType)
	if !ok || current == nil {
		return values.UnknownType, "", false, false
	}
	for index, segment := range fieldSegments {
		matched := -1
		for ordinal, field := range current.Fields {
			if !strings.EqualFold(field.Name, segment) {
				continue
			}
			if matched >= 0 {
				return values.UnknownType, "", false, true
			}
			matched = ordinal
		}
		if matched < 0 {
			return values.UnknownType, "", false, false
		}
		field := current.Fields[matched]
		if index == len(fieldSegments)-1 {
			array, isExactArray := field.FieldType.(*values.ArrayType)
			if !isExactArray || array.ElementType == nil || values.IsUnresolved(array.ElementType) {
				return values.UnknownType, "", false, true
			}
			return array.ElementType, strings.ToUpper(field.Name), true, true
		}
		nested, isRecord := field.FieldType.(*values.RecordType)
		if !isRecord || nested == nil {
			return values.UnknownType, "", false, true
		}
		current = nested
	}
	return values.UnknownType, "", false, false
}
