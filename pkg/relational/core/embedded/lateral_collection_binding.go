// Portions derived from FoundationDB Record Layer (LogicalOperator.java,
// RecordMetaData.java, QueryVisitor.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"errors"
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// bindLateralCollections runs after the SELECT's final FROM-tree rebuild. Java's
// generateCorrelatedFieldAccess consumes resolveCorrelatedIdentifier's underlying
// Value: the collection's identity is settled before the Explode is constructed.
func bindLateralCollections(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, templateName string, cteScopes map[string]semantic.ScopeSource) error {
	if sq == nil {
		return nil
	}
	tables := newUnnestTableResolver(md, templateName)
	if err := bindPrimaryUnnest(op, sq, md, tables); err != nil {
		return err
	}
	lateral := make([]bool, len(sq.joins))
	anyLateral := false
	anyTableFunction := isTableFunctionItem(sq.inlineValues)
	for i, j := range sq.joins {
		lateral[i] = isLateralUnnestJoin(j, tables)
		anyLateral = anyLateral || lateral[i]
		anyTableFunction = anyTableFunction || isTableFunctionItem(j.inlineValues)
	}
	if !anyLateral && !anyTableFunction {
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
	if anyTableFunction {
		if err := bindTableFunctions(op, joins, sq, md, templateName, cteScopes); err != nil {
			return err
		}
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
		resolver, err := buildSelectScopeChecked(&prefix, md, templateName, cteScopes)
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			return err
		}
		if len(j.segments) == 1 && at != "" {
			// The verdict the early pass gives (atOnJoinSourceError), for a
			// build that reaches binding first: Java's CTE branch reads a WITH
			// CTE or an operator of this block's prefix or any enclosing
			// block (the prefix scope's chain).
			_, withCTE := cteScopes[strings.ToUpper(j.segments[0])]
			return singleSegmentAtError(j.segments[0], withCTE || scopeNamesSource(resolver.Scope(), j.segments[0]), md)
		}
		path, sourceQualified := unnestSemanticPath(resolver.Scope(), j)
		// A single source would otherwise resolve locally and lose its owner
		// correlation. The lateral inner scope sees the FROM prefix as a parent.
		inner := semantic.NewScope(resolver.Scope())
		pathResolver := expr.New(semantic.NewAnalyzer(rlcatalog.Wrap(md), false), inner)
		var bound values.Value
		if sourceQualified {
			bound, err = pathResolver.ResolveSourceQualifiedIdentifierPath(path)
		} else {
			bound, err = pathResolver.ResolveCorrelatedIdentifierPath(path)
		}
		if err != nil {
			// A path no reading resolves is Java's resolveIdentifier refusal,
			// UNDEFINED_COLUMN "Unknown reference <path>"; an ambiguous one
			// keeps its 42702.
			var notSource *semantic.SourceNotFoundError
			var notColumn *semantic.ColumnNotFoundError
			if errors.As(err, &notSource) || errors.As(err, &notColumn) {
				return functions.UnknownSourceReferenceError(j.segments)
			}
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			return err
		}
		array, ok := bound.Type().(*values.ArrayType)
		if !ok {
			return rowstruct.NonArrayCorrelationError(bound.Type())
		}
		if _, err := values.SnapshotExactType(array); err != nil {
			return api.NewErrorf(api.ErrCodeUnsupportedQuery, "lateral collection has no exact type: %v", err)
		}
		u.CorrelatedCollection = bound
		u.EnclosingOwner = !ownedByFromPrefix(bound, resolver.Scope())
		if err := checkUnnestUsingColumns(j, resolver.Scope()); err != nil {
			return err
		}
	}
	return nil
}

func isTableFunctionItem(item antlrgen.ITableSourceItemContext) bool {
	_, ok := item.(*antlrgen.TableValuedFunctionContext)
	return ok
}

// bindTableFunctions resolves each table function's arguments laterally: the
// primary source against the enclosing scope, a FROM leg against its prefix
// (Java's `FROM t AS a, range(a.id)`). The build pass left an unbound stream.
func bindTableFunctions(op logical.LogicalOperator, joins []*logical.LogicalJoin, sq *selectQuery, md *recordlayer.RecordMetaData, templateName string, cteScopes map[string]semantic.ScopeSource) error {
	bind := func(item antlrgen.ITableSourceItemContext, node logical.LogicalOperator, prefix selectQuery) error {
		tf, ok := item.(*antlrgen.TableValuedFunctionContext)
		if !ok {
			return nil
		}
		target, ok := node.(*logical.LogicalInlineValues)
		if !ok || target.StreamValue() == nil {
			return api.NewError(api.ErrCodeInternalError, "table function binding: parsed/logical source mismatch")
		}
		resolver, err := buildSelectScopeChecked(&prefix, md, templateName, cteScopes)
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			return err
		}
		bound, err := buildTableFunctionLogical(tf, target.Alias, target.Binding, resolver)
		if err != nil {
			return err
		}
		target.SetStream(bound.StreamValue())
		return nil
	}
	primary := *sq
	primary.tableName, primary.inlineValues, primary.derivedQuery, primary.joins = "", nil, nil, nil
	if err := bind(sq.inlineValues, primaryLeaf(op), primary); err != nil {
		return err
	}
	for i, j := range sq.joins {
		prefix := *sq
		prefix.joins = sq.joins[:i]
		if err := bind(j.inlineValues, joins[i].Right, prefix); err != nil {
			return err
		}
	}
	return nil
}

// checkUnnestUsingColumns resolves an INNER JOIN's USING columns on the right
// side as Java does, against the unnest's own operator alone
// (QueryVisitor.resolveJoinUsingClause: resolveIdentifier(uid, the right
// operator)): a record element's members without ordinality, the element
// itself (named by its alias) and the ordinal otherwise. Go synthesizes the
// USING as a qualified ON (`left.k = el.k`), whose struct-relative reading
// would also reach a member through the WITH ORDINALITY element, so a column
// the operator does not carry is refused here, "Unknown reference <col>"
// (measured: `kk JOIN kk.items AS i AT p USING (k)`, and `USING (id)` over a
// scalar element).
func checkUnnestUsingColumns(j joinClause, prefix *semantic.Scope) error {
	if len(j.usingColTexts) == 0 {
		return nil
	}
	element, typed := unnestElementColumn(prefix, j)
	var elementPtr *semantic.Column
	if typed {
		elementPtr = &element
	}
	src, ok := unnestVirtualScopeSourceWithElement(j, elementPtr)
	if !ok {
		return nil
	}
	for _, colText := range j.usingColTexts {
		id := usingColumnKey(colText)
		if _, found := src.Table.LookupColumn(id); !found {
			return api.NewErrorf(api.ErrCodeUndefinedColumn, "Unknown reference %s", id.Name())
		}
	}
	return nil
}

// ownedByFromPrefix reports whether a bound collection reads a source of the
// FROM prefix (the scope level built from the sources to the unnest's left),
// rather than of an enclosing query. The owner is the quantified object the
// collection's field path starts from.
func ownedByFromPrefix(collection values.Value, prefix *semantic.Scope) bool {
	if field, ok := values.AsFieldValue(collection); ok {
		collection = field.ChildValue()
	}
	owner, ok := values.AsQuantifiedObjectValue(collection)
	if !ok {
		return true
	}
	for _, src := range prefix.Sources() {
		if strings.EqualFold(src.CorrelationName, owner.Correlation().Name()) {
			return true
		}
	}
	return false
}

// unnestSemanticPath expands a prior unnest's virtual whole-element column only
// when that source-qualified path actually resolves to a shadowing source. The
// original segments stay intact, including a dot inside one quoted identifier.
//
// sourceQualified reports the expansion: the result is then Go's spelling of
// the path through the whole-element column, which the caller resolves
// source-qualified (Scope.ResolveSourceQualifiedPath), never as an ordinary
// expression. Spelled by a user, `item.item.m` also has Java's doubled reading
// of member m and is ambiguous (measured); the expansion of `item.m` is not
// that expression.
func unnestSemanticPath(scope *semantic.Scope, j joinClause) (path []semantic.Identifier, sourceQualified bool) {
	path = make([]semantic.Identifier, len(j.segments))
	for i, segment := range j.segments {
		path[i] = semantic.FromNormalized(segment)
	}
	if len(path) < 2 {
		return path, false
	}
	wrapped := make([]semantic.Identifier, 0, len(path)+1)
	wrapped = append(wrapped, path[0])
	wrapped = append(wrapped, path...)
	if _, src, _, err := scope.ResolveSourceQualifiedPath(wrapped); err == nil && src.Shadowing {
		return wrapped, true
	}
	return path, false
}

// bindPrimaryUnnest binds a query block's PRIMARY FROM source when it is a
// correlated array unnest (primaryIsCorrelatedUnnest): its path resolves
// against the enclosing scopes alone, the operators Java's
// resolveCorrelatedIdentifier sees for a block's first FROM item, so its owner
// is always an enclosing query's source.
func bindPrimaryUnnest(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, tables tableResolver) error {
	clause := primaryUnnestClause(sq.tableName, sq.tableAlias, sq.tableAliasExplicit, sq.tableAtAlias, sq.sourceSegments, sq.bindingID)
	if sq.derivedQuery != nil || sq.inlineValues != nil || !primaryIsCorrelatedUnnest(sq.enclosingScope, clause, tables) {
		return nil
	}
	u, ok := primaryLeaf(op).(*logical.LogicalUnnest)
	as, at := unnestAliases(clause)
	if !ok || !slices.Equal(u.Segments, clause.segments) || u.Alias != as || u.AtAlias != at {
		return api.NewError(api.ErrCodeInternalError, "lateral binding: parsed/logical primary source mismatch")
	}
	path := make([]semantic.Identifier, len(clause.segments))
	for i, segment := range clause.segments {
		path[i] = semantic.FromNormalized(segment)
	}
	resolver := expr.New(semantic.NewAnalyzer(rlcatalog.Wrap(md), false), semantic.NewScope(sq.enclosingScope))
	bound, err := resolver.ResolveCorrelatedIdentifierPath(path)
	if err != nil {
		var notSource *semantic.SourceNotFoundError
		var notColumn *semantic.ColumnNotFoundError
		if errors.As(err, &notSource) || errors.As(err, &notColumn) {
			return functions.UnknownSourceReferenceError(clause.segments)
		}
		if mapped := mapPredicateWalkError(err); mapped != nil {
			return mapped
		}
		return err
	}
	array, ok := bound.Type().(*values.ArrayType)
	if !ok {
		return rowstruct.NonArrayCorrelationError(bound.Type())
	}
	if _, err := values.SnapshotExactType(array); err != nil {
		return api.NewErrorf(api.ErrCodeUnsupportedQuery, "lateral collection has no exact type: %v", err)
	}
	u.CorrelatedCollection = bound
	u.EnclosingOwner = true
	return nil
}

// primaryLeaf is the leaf a query block's operator tree starts from: the
// leftmost input below its joins and row-shaping operators.
func primaryLeaf(op logical.LogicalOperator) logical.LogicalOperator {
	for op != nil {
		switch node := op.(type) {
		case *logical.LogicalJoin:
			op = node.Left
		case *logical.LogicalCTE:
			op = node.Main
		case *logical.LogicalProject, *logical.LogicalFilter, *logical.LogicalAggregate,
			*logical.LogicalSort, *logical.LogicalLimit, *logical.LogicalDistinct:
			op = node.Children()[0]
		default:
			return op
		}
	}
	return nil
}
