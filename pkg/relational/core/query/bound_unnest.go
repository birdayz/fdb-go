package query

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// boundUnnestCollection reads the semantic binding, never the diagnostic source
// spelling. SQL FROM field resolution emits a fused field path over one owner.
func boundUnnestCollection(u *logical.LogicalUnnest) (values.QuantifiedObjectValue, []int, *values.ArrayType) {
	if u == nil || u.CorrelatedCollection == nil {
		return nil, nil, nil
	}
	array, ok := u.CorrelatedCollection.Type().(*values.ArrayType)
	if !ok || array.ElementType == nil {
		return nil, nil, nil
	}
	if field, ok := values.AsFieldValue(u.CorrelatedCollection); ok {
		owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
		if !ok {
			return nil, nil, nil
		}
		return owner, field.Path().Ordinals(), array
	}
	if owner, ok := values.AsQuantifiedObjectValue(u.CorrelatedCollection); ok {
		return owner, nil, array
	}
	return nil, nil, nil
}

// boundUnnestBindingError distinguishes an exact non-array value from missing
// resolver authority. Neither case may trigger descriptor or text recovery.
func boundUnnestBindingError(u *logical.LogicalUnnest) error {
	if u != nil && u.CorrelatedCollection != nil {
		collection := u.CorrelatedCollection
		if field, ok := values.AsFieldValue(collection); ok {
			collection = field.ChildValue()
		}
		if _, ok := values.AsQuantifiedObjectValue(collection); ok {
			if typ := u.CorrelatedCollection.Type(); typ != nil {
				if _, err := values.SnapshotExactType(typ); err == nil && typ.Code() != values.TypeCodeArray {
					return api.NewError(api.ErrCodeInvalidColumnReference, "join correlation can occur only on a column of repeated (array) type")
				}
			}
		}
	}
	return api.NewError(api.ErrCodeUnsupportedQuery, "lateral array unnest requires an exact semantic collection binding")
}

// boundUnnestOwner finds a prior element binding by its correlation identity.
// A derived definition is not visible in the enclosing FROM scope.
func boundUnnestOwner(outer logical.LogicalOperator, u *logical.LogicalUnnest) *logical.LogicalUnnest {
	owner, _, _ := boundUnnestCollection(u)
	if owner == nil {
		return nil
	}
	var walk func(logical.LogicalOperator) *logical.LogicalUnnest
	walk = func(node logical.LogicalOperator) *logical.LogicalUnnest {
		if prior, ok := node.(*logical.LogicalUnnest); ok {
			if unnestSourceCorrelation(prior) == owner.Correlation() {
				return prior
			}
			return nil
		}
		if cte, ok := node.(*logical.LogicalCTE); ok {
			return walk(cte.Main)
		}
		if node != nil {
			for _, child := range node.Children() {
				if prior := walk(child); prior != nil {
					return prior
				}
			}
		}
		return nil
	}
	return walk(outer)
}

// boundUnnestSingleSource reports whether the collection's owner is the whole
// visible outer source, rather than a window in a join. CTE Main is the visible
// reference; joins inside its definition do not add enclosing FROM sources.
func boundUnnestSingleSource(outer logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	owner, _, _ := boundUnnestCollection(u)
	if owner == nil || owner.Correlation().Name() != sourceBinding(outer) {
		return false
	}
	for outer != nil {
		switch o := outer.(type) {
		case *logical.LogicalScan, *logical.LogicalInlineValues:
			return true
		case *logical.LogicalJoin, *logical.LogicalUnnest:
			return false
		case *logical.LogicalCTE:
			outer = o.Main
		default:
			children := outer.Children()
			if len(children) != 1 {
				return false
			}
			outer = children[0]
		}
	}
	return false
}

// resolveBoundSeedCollection embeds a checked owner-relative path in an actual
// seed window. A whole-object element occupies one slot; a row owner occupies a
// flat run. Only the root gets a physical offset, never the nested accessors.
func resolveBoundSeedCollection(root values.Value, u *logical.LogicalUnnest, offset int, wholeObject bool) values.Value {
	owner, path, array := boundUnnestCollection(u)
	if owner == nil || offset < 0 {
		return nil
	}
	row, ok := root.Type().(*values.RecordType)
	if !ok {
		return nil
	}
	var ordinal int
	if wholeObject {
		if offset >= len(row.Fields) || !row.Fields[offset].FieldType.Equals(owner.FlowedType()) {
			return nil
		}
		ordinal = offset
	} else {
		source, ok := owner.FlowedType().(*values.RecordType)
		if !ok || len(path) == 0 || offset+len(source.Fields) > len(row.Fields) {
			return nil
		}
		for i, field := range source.Fields {
			if !row.Fields[offset+i].FieldType.Equals(field.FieldType) {
				return nil
			}
		}
		ordinal = offset + path[0]
		path = path[1:]
	}
	requests := make([]values.FieldRequest, len(path))
	for i, index := range path {
		request, err := values.FieldByOrdinal(index)
		if err != nil {
			return nil
		}
		requests[i] = request
	}
	collection, err := values.ResolveOrdinalSeedAccess(root, ordinal, requests)
	if err != nil || !collection.Type().Equals(array) {
		return nil
	}
	return collection
}

// boundUnnestLegColumns is the AS-then-AT row the lateral seed actually emits.
// A record element occupies one whole-object slot, not a flattened field run;
// AT-only drops that element slot from the joined output.
func boundUnnestLegColumns(u *logical.LogicalUnnest) []values.Field {
	if u == nil || (u.Alias == "" && u.AtAlias == "") {
		return nil
	}

	_, _, array := boundUnnestCollection(u)
	if array == nil {
		return nil
	}
	elementType := array.ElementType
	alias, ordinalAlias := u.Alias, u.AtAlias
	if u.AtAlias != "" {
		names := logical.UnnestOrdinalityNames(u.Alias, u.AtAlias)
		alias, ordinalAlias = names[0], names[1]
	}

	fields := make([]values.Field, 0, 2)
	if u.Alias != "" {
		fields = append(fields, values.Field{
			Name:      alias,
			FieldType: elementType,
			Ordinal:   len(fields),
		})
	}
	if u.AtAlias != "" {
		fields = append(fields, values.Field{
			Name:      ordinalAlias,
			FieldType: values.NotNullInt,
			Ordinal:   len(fields),
		})
	}
	return fields
}
