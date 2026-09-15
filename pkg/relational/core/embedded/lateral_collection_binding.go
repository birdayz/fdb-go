package embedded

import (
	"slices"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
)

// bindLateralCollections runs after the SELECT's final FROM-tree rebuild. Java's
// generateCorrelatedFieldAccess consumes resolveCorrelatedIdentifier's underlying
// Value: the collection's identity is settled before the Explode is constructed.
func bindLateralCollections(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	if sq == nil {
		return nil
	}
	tables := newUnnestTableResolver(md, schemaName)
	lateral := make([]bool, len(sq.joins))
	anyLateral := false
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], tables)
		lateral[i] = isLateralUnnestJoin(j, visible, tables)
		anyLateral = anyLateral || lateral[i]
	}
	if !anyLateral {
		return nil
	}
	var joins []*logical.LogicalJoin
	for cur := op; cur != nil; {
		switch node := cur.(type) {
		case *logical.LogicalJoin:
			joins = append(joins, node)
			cur = node.Left
		case *logical.LogicalCTE:
			cur = node.Main
		case *logical.LogicalProject, *logical.LogicalFilter, *logical.LogicalAggregate,
			*logical.LogicalSort, *logical.LogicalLimit, *logical.LogicalDistinct:
			cur = cur.Children()[0]
		default:
			cur = nil
		}
	}
	slices.Reverse(joins)
	if len(joins) != len(sq.joins) {
		return api.NewErrorf(api.ErrCodeInternalError, "lateral binding: parsed FROM has %d joins, logical FROM has %d", len(sq.joins), len(joins))
	}
	for i, isLateral := range lateral {
		if !isLateral {
			if _, unexpected := joins[i].Right.(*logical.LogicalUnnest); unexpected {
				return api.NewErrorf(api.ErrCodeInternalError, "lateral binding: unexpected unnest at FROM join %d", i)
			}
			continue
		}
		j := sq.joins[i]
		u, ok := joins[i].Right.(*logical.LogicalUnnest)
		as, at := unnestAliases(j)
		if !ok || !slices.Equal(u.Segments, j.segments) || u.Alias != as || u.AtAlias != at || u.Binding != j.bindingID {
			return api.NewErrorf(api.ErrCodeInternalError, "lateral binding: parsed/logical source mismatch at FROM join %d", i)
		}
		prefix := *sq
		prefix.joins = sq.joins[:i]
		resolver, err := buildSelectScopeChecked(&prefix, md, schemaName, cteScopes)
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			return err
		}
		if len(j.segments) < 2 && at != "" {
			return api.NewError(api.ErrCodeWrongObjectType, "AT ordinality is only valid on a correlated array source (FROM t, t.arr AS x AT ord)")
		}
		path := unnestSemanticPath(resolver.Scope(), j)
		// A single source would otherwise resolve locally and lose its owner
		// correlation. The lateral inner scope sees the FROM prefix as a parent.
		inner := semantic.NewScope(resolver.Scope())
		bound, err := expr.New(semantic.NewAnalyzer(rlcatalog.Wrap(md), false), inner).ResolveIdentifierPath(path)
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			return err
		}
		array, ok := bound.Type().(*values.ArrayType)
		if !ok {
			return api.NewError(api.ErrCodeInvalidColumnReference, "join correlation can occur only on a column of repeated (array) type")
		}
		if _, err := values.SnapshotExactType(array); err != nil {
			return api.NewErrorf(api.ErrCodeUnsupportedQuery, "lateral collection has no exact type: %v", err)
		}
		u.CorrelatedCollection = bound
	}
	return nil
}

// unnestSemanticPath expands a prior unnest's virtual whole-element column only
// when that source-qualified path actually resolves to a shadowing source. The
// original segments stay intact, including a dot inside one quoted identifier.
func unnestSemanticPath(scope *semantic.Scope, j joinClause) []semantic.Identifier {
	path := make([]semantic.Identifier, len(j.segments))
	for i, segment := range j.segments {
		path[i] = semantic.FromNormalized(segment)
	}
	if len(path) < 2 {
		return path
	}
	wrapped := make([]semantic.Identifier, 0, len(path)+1)
	wrapped = append(wrapped, path[0])
	wrapped = append(wrapped, path...)
	if _, src, _, err := scope.ResolveSourceQualifiedPath(wrapped); err == nil && src.Shadowing {
		return wrapped
	}
	return path
}
