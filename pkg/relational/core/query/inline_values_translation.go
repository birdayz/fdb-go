package query

import (
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
