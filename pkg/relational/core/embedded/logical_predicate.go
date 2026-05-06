package embedded

// Catalog-aware logical-builder seam.
//
// logical_builder.go ports parse trees into LogicalOperator trees with
// WHERE clauses carried as canonical source text — adequate for the
// pre-cascades Explain output but blind to identifier resolution and
// type information.
//
// This file is the catalog-aware variant: when a *recordlayer.RecordMetaData
// is in scope, WHERE clauses walk through expr.WalkPredicate (via
// rlcatalog → semantic.Analyzer + Scope) and produce a real
// predicates.QueryPredicate tree on LogicalFilter.Predicate alongside
// the source text. Best-effort throughout — any walker error,
// catalog miss, ambiguous column ref, or shape outside the walker's
// support degrades to text fallback rather than failing the build.
//
// Wiring map (catalog-aware → text fallback):
//
//   buildLogicalPlanForSelectWithCatalog → buildLogicalPlanForSelect
//   buildLogicalPlanForDeleteWithCatalog → buildLogicalPlanForDelete
//   buildLogicalPlanForUpdateWithCatalog → buildLogicalPlanForUpdate
//   buildLogicalPlanForInsertWithCatalog → buildLogicalPlanForInsert
//   buildLogicalPlanForQueryWithCatalog (CTE/UNION/SELECT recursion)
//
// Predicate-extraction helpers:
//
//   buildWherePredicate          (selectQuery shape, dispatches)
//   buildWherePredicateForTable  (single source — primary table)
//   buildWherePredicateForJoins  (multi source — JOIN chain)
//
// Plumbed into naive_generator.ExplainFn via
// EmbeddedConnection.cachedMetaData() — when the session schema cache
// already holds the active schema, ExplainFn upgrades to predicate-tree
// rendering; cold cache stays on the text-builder path so EXPLAIN
// remains deterministic and IO-free.

import (
	"errors"
	"strings"

	recordlayer "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic/rlcatalog"
)

// buildWherePredicateForTable converts a WHERE expression context
// into a predicates.QueryPredicate using the expr walker, with a
// single-source scope over the named table. Returns (nil, false) on
// any shape the walker can't handle, on a catalog lookup miss, or
// when metadata is nil.
//
// The (pred, true) branch is what callers attach to a LogicalFilter;
// the (nil, false) branch is the signal to fall back to the
// canonical source text. Error discrimination is intentionally
// coarse — unsupported shape, catalog miss, nil metadata all land
// in the same (nil, false) bucket — because every error at this
// boundary has the same handling: use text.
//
// tableAlias may be empty; the table's own name fills in.
func buildWherePredicateForTable(
	md *recordlayer.RecordMetaData,
	tableName, tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if md == nil || tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	// Split on '.' so a schema-qualified table name like "schema.t"
	// reaches FromSegments as ["schema", "t"] rather than as a single
	// dotted segment that would never resolve in the catalog.
	tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
	if err != nil {
		return nil, false
	}
	alias := semantic.NewUnquoted(tableAlias)
	if tableAlias == "" {
		alias = semantic.NewUnquoted(tableName)
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(semantic.ScopeSource{
		Table:           tbl,
		Alias:           alias,
		CorrelationName: alias.Name(),
	}); err != nil {
		return nil, false
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	// Plan-time fold of constant Value sub-trees inside the predicate
	// (`name = 1+2` → `name = 3`). Best-effort — SimplifyPredicateValues
	// is pointer-stable when nothing folds.
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildWherePredicate is the selectQuery-shaped adapter over the
// walker. Single-table FROM uses buildWherePredicateForTable;
// JOIN-shape FROM (sq.joins non-empty) builds a multi-source scope
// — one ScopeSource per primary + JOIN. Derived-table FROM routes
// through buildWherePredicateForDerived which synthesises a virtual
// ScopeSource from the inner query's projection schema (basic
// shapes only — see buildDerivedTableSource).
func buildWherePredicate(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if sq == nil {
		return nil, false
	}
	if sq.derivedQuery != nil {
		return buildWherePredicateForDerived(md, sq, whereExpr)
	}
	if len(sq.joins) == 0 {
		return buildWherePredicateForTable(md, sq.tableName, sq.tableAlias, whereExpr)
	}
	return buildWherePredicateForJoins(md, sq, whereExpr)
}

// buildWherePredicateForDerived handles `FROM (SELECT ...) AS alias`.
// Synthesises a virtual ScopeSource from the inner query's projection
// schema (via buildDerivedTableSource — basic shapes only) and then
// walks the WHERE under that scope.
//
// Anything richer than `(SELECT col1, col2 FROM realtable) AS alias`
// — joins, derived-of-derived, SELECT *, aggregates, computed
// projections — declines and the caller falls back to the text
// builder. Phase 4.0 Type hierarchy port unlocks computed
// projections (the seed has no way to infer the projected
// expression's result type).
func buildWherePredicateForDerived(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if md == nil || sq == nil || sq.tableName == "" || sq.derivedQuery == nil ||
		whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	src, ok := buildDerivedTableSource(md, sq.tableName, sq.derivedQuery)
	if !ok {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(src); err != nil {
		return nil, false
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildDerivedTableSource synthesises a virtual ScopeSource for
// `FROM (SELECT col1, col2 FROM realtable) AS alias`. Walks the inner
// query's parse tree via extractFromQueryTerm, then builds a
// semantic.StaticTable whose columns inherit the inner-table column
// types. Anything outside the basic shape — derived-of-derived,
// joins, SELECT *, aggregates, computed projections, qualified-star
// projections — declines with (zero, false).
//
// alias is the outer FROM clause's alias for the derived table; the
// virtual table's name + visibility are bound to that alias.
func buildDerivedTableSource(
	md *recordlayer.RecordMetaData,
	alias string,
	inner antlrgen.IQueryContext,
) (semantic.ScopeSource, bool) {
	if md == nil || alias == "" || inner == nil {
		return semantic.ScopeSource{}, false
	}
	body, ok := inner.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return semantic.ScopeSource{}, false
	}
	innerSQ, err := extractFromQueryTerm(body)
	if err != nil || innerSQ == nil {
		return semantic.ScopeSource{}, false
	}
	// Derived-of-derived: recursively build the inner scope.
	if innerSQ.derivedQuery != nil {
		innerSrc, ok := buildDerivedTableSource(md, innerSQ.tableName, innerSQ.derivedQuery)
		if !ok {
			return semantic.ScopeSource{}, false
		}
		aliasID := semantic.NewUnquoted(alias)
		// Apply inner projection aliases if present.
		cols := innerSrc.Table.Columns()
		if innerSQ.projCols != nil {
			cols = make([]semantic.Column, 0, len(innerSQ.projCols))
			for i, col := range innerSQ.projCols {
				name := col
				if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
					name = innerSQ.projAliases[i]
				}
				cols = append(cols, semantic.Column{
					Id:       semantic.NewUnquoted(name),
					Type:     "UNKNOWN",
					Nullable: true,
				})
			}
		}
		virtualTable := &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{alias}, false),
			TableColumns: cols,
		}
		return semantic.ScopeSource{
			Table:           virtualTable,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}, true
	}
	if len(innerSQ.joins) > 0 ||
		len(innerSQ.aggCols) > 0 ||
		innerSQ.countStar ||
		innerSQ.tableName == "" {
		return semantic.ScopeSource{}, false
	}
	for _, e := range innerSQ.projExprs {
		if e != nil {
			// Computed expression — type unknown without Phase 4.0 Type
			// hierarchy. Decline so the caller falls back to text.
			return semantic.ScopeSource{}, false
		}
	}
	for _, qual := range innerSQ.projStarQualifiers {
		if qual != "" {
			return semantic.ScopeSource{}, false
		}
	}

	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	innerTbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(innerSQ.tableName, "."), false))
	if err != nil {
		return semantic.ScopeSource{}, false
	}

	columns := make([]semantic.Column, 0, len(innerSQ.projCols))
	var colAliasMap map[string]string
	for i, col := range innerSQ.projCols {
		bareName := col
		if dot := strings.LastIndex(col, "."); dot >= 0 {
			bareName = col[dot+1:]
		}
		innerCol, found := innerTbl.LookupColumn(semantic.NewUnquoted(bareName))
		if !found {
			return semantic.ScopeSource{}, false
		}
		outName := bareName
		if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
			outName = innerSQ.projAliases[i]
		}
		if !strings.EqualFold(outName, bareName) {
			if colAliasMap == nil {
				colAliasMap = make(map[string]string)
			}
			colAliasMap[strings.ToUpper(outName)] = strings.ToUpper(bareName)
		}
		columns = append(columns, semantic.Column{
			Id:       semantic.NewUnquoted(outName),
			Type:     innerCol.Type,
			Nullable: innerCol.Nullable,
		})
	}

	aliasID := semantic.NewUnquoted(alias)
	virtualTable := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{alias}, false),
		TableColumns: columns,
	}
	return semantic.ScopeSource{
		Table:           virtualTable,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
		ColumnAliasMap:  colAliasMap,
	}, true
}

// upgradeJoinOnPredicates walks the logical plan tree to find LogicalJoin
// nodes and upgrades their OnText to OnPredicate using the full join scope.
// The join nodes are created in order matching sq.joins, so we match
// them sequentially by walking the left-child spine (the builder chains
// joins left-to-right with op = NewJoin(op, right, ...)).
func upgradeJoinOnPredicates(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) {
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)

	resolveTable := func(tableName string) semantic.Table {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err == nil {
			return tbl
		}
		if cteScopes != nil {
			if src, found := cteScopes[strings.ToUpper(tableName)]; found {
				return src.Table
			}
		}
		return nil
	}

	// Collect all tables in the FROM clause for the join scope.
	type tableInfo struct {
		name  string
		alias string
	}
	tables := []tableInfo{{name: sq.tableName, alias: sq.tableAlias}}
	for _, j := range sq.joins {
		tables = append(tables, tableInfo{name: j.tableName, alias: j.alias})
	}

	// Collect LogicalJoin nodes from the left-child spine. The builder
	// chains joins left-to-right: Join(Join(Scan, R0), R1), so the
	// outermost join wraps the LAST sq.joins entry. We collect them
	// and then match in reverse.
	var joins []*logical.LogicalJoin
	for cur := op; cur != nil; {
		j, ok := cur.(*logical.LogicalJoin)
		if !ok {
			ch := cur.Children()
			if len(ch) > 0 {
				cur = ch[0]
				continue
			}
			break
		}
		joins = append(joins, j)
		cur = j.Left
	}

	// Build the full scope for predicate resolution.
	scope := semantic.NewScope(nil)
	scopeOK := true
	for _, ti := range tables {
		tbl := resolveTable(ti.name)
		if tbl == nil {
			scopeOK = false
			break
		}
		aliasID := semantic.NewUnquoted(ti.alias)
		if ti.alias == "" {
			aliasID = semantic.NewUnquoted(ti.name)
		}
		if err := scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}); err != nil {
			scopeOK = false
			break
		}
	}
	if !scopeOK {
		return
	}
	resolver := expr.New(analyzer, scope)

	// Match collected joins with sq.joins in reverse order.
	for i, j := range joins {
		sqIdx := len(sq.joins) - 1 - i
		if sqIdx < 0 || sqIdx >= len(sq.joins) {
			break
		}
		if sq.joins[sqIdx].onExpr != nil && j.OnPredicate == nil {
			if pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr); walkErr == nil {
				j.OnPredicate = predicates.SimplifyPredicateValues(pred)
			}
		}
	}
}

// buildWherePredicateFromCTEScope builds a predicate using a CTE-derived
// ScopeSource. Used when the main query's FROM references a CTE — the
// CTE's column schema was derived from its body's SELECT projection and
// the underlying table's metadata.
func buildWherePredicateFromCTEScope(
	src semantic.ScopeSource,
	tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
	md *recordlayer.RecordMetaData,
) (predicates.QueryPredicate, bool) {
	if whereExpr == nil || whereExpr.Expression() == nil || md == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)
	if tableAlias != "" {
		src.Alias = semantic.NewUnquoted(tableAlias)
		src.CorrelationName = tableAlias
	}
	if err := scope.AddSource(src); err != nil {
		return nil, false
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildCTEColumnSource derives a ScopeSource from a CTE body's query
// context. Extracts the projected column names and their types from the
// underlying real table's metadata. Declines on complex shapes (SELECT *,
// aggregates, computed expressions, derived tables, JOINs) — same
// restrictions as buildDerivedTableSource.
func buildCTEColumnSource(
	md *recordlayer.RecordMetaData,
	cteName string,
	cteQuery antlrgen.IQueryContext,
	priorCTEs map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	if md == nil || cteName == "" || cteQuery == nil {
		return semantic.ScopeSource{}, false
	}
	body, ok := cteQuery.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return semantic.ScopeSource{}, false
	}
	innerSQ, err := extractFromQueryTerm(body)
	if err != nil || innerSQ == nil {
		return semantic.ScopeSource{}, false
	}
	if innerSQ.derivedQuery != nil ||
		len(innerSQ.joins) > 0 ||
		len(innerSQ.aggCols) > 0 ||
		innerSQ.countStar ||
		innerSQ.tableName == "" {
		return semantic.ScopeSource{}, false
	}
	hasComputedExpr := false
	for _, e := range innerSQ.projExprs {
		if e != nil {
			hasComputedExpr = true
			break
		}
	}

	// Resolve the inner table: try metadata first, then prior CTE schemas.
	var innerTbl semantic.Table
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	tbl, resolveErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(innerSQ.tableName, "."), false))
	if resolveErr == nil {
		innerTbl = tbl
	} else if priorCTEs != nil {
		if src, found := priorCTEs[strings.ToUpper(innerSQ.tableName)]; found {
			innerTbl = src.Table
		}
	}
	if innerTbl == nil {
		return semantic.ScopeSource{}, false
	}

	var columns []semantic.Column
	var aliasMap map[string]string
	if innerSQ.projCols == nil {
		allCols := innerTbl.Columns()
		columns = make([]semantic.Column, len(allCols))
		copy(columns, allCols)
	} else {
		columns = make([]semantic.Column, 0, len(innerSQ.projCols))
		for i, col := range innerSQ.projCols {
			isComputed := i < len(innerSQ.projExprs) && innerSQ.projExprs[i] != nil
			bareName := col
			if dot := strings.LastIndex(col, "."); dot >= 0 {
				bareName = col[dot+1:]
			}
			outName := bareName
			if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
				outName = innerSQ.projAliases[i]
			}
			if isComputed {
				columns = append(columns, semantic.Column{
					Id:       semantic.NewUnquoted(outName),
					Type:     "UNKNOWN",
					Nullable: true,
				})
				continue
			}
			innerCol, found := innerTbl.LookupColumn(semantic.NewUnquoted(bareName))
			if !found {
				if hasComputedExpr {
					columns = append(columns, semantic.Column{
						Id:       semantic.NewUnquoted(outName),
						Type:     "UNKNOWN",
						Nullable: true,
					})
					continue
				}
				return semantic.ScopeSource{}, false
			}
			if !strings.EqualFold(outName, bareName) {
				if aliasMap == nil {
					aliasMap = make(map[string]string)
				}
				aliasMap[strings.ToUpper(outName)] = strings.ToUpper(bareName)
			}
			columns = append(columns, semantic.Column{
				Id:       semantic.NewUnquoted(outName),
				Type:     innerCol.Type,
				Nullable: innerCol.Nullable,
			})
		}
	}

	aliasID := semantic.NewUnquoted(cteName)
	virtualTable := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{cteName}, false),
		TableColumns: columns,
	}
	return semantic.ScopeSource{
		Table:           virtualTable,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
		ColumnAliasMap:  aliasMap,
	}, true
}

// applyCTEColumnAliases renames the columns of a CTE ScopeSource
// according to the explicit column alias list: WITH c1(x, y) AS (...).
// Matches Java's QueryVisitor.visitNamedQuery column-alias handling.
func applyCTEColumnAliases(src semantic.ScopeSource, colAliases antlrgen.IFullIdListContext) semantic.ScopeSource {
	list, ok := colAliases.(*antlrgen.FullIdListContext)
	if !ok || list == nil {
		return src
	}
	aliases := list.AllFullId()
	if len(aliases) == 0 {
		return src
	}
	tbl := src.Table
	if tbl == nil {
		return src
	}
	origCols := tbl.Columns()

	newCols := make([]semantic.Column, len(origCols))
	aliasMap := make(map[string]string)
	for i, col := range origCols {
		if i < len(aliases) {
			newName := functions.FullIdToName(aliases[i])
			aliasMap[strings.ToUpper(newName)] = strings.ToUpper(col.Id.Name())
			newCols[i] = semantic.Column{
				Id:       semantic.NewUnquoted(newName),
				Type:     col.Type,
				Nullable: col.Nullable,
			}
		} else {
			newCols[i] = col
		}
	}

	newTable := &semantic.StaticTable{
		TableName:    tbl.Name(),
		TableColumns: newCols,
	}
	return semantic.ScopeSource{
		Table:           newTable,
		Alias:           src.Alias,
		CorrelationName: src.CorrelationName,
		ColumnAliasMap:  aliasMap,
	}
}

// buildWherePredicateForJoinsWithCTEScopes is like
// buildWherePredicateForJoins but resolves CTE table references
// using pre-derived column schemas when metadata lookup fails.
func buildWherePredicateForJoinsWithCTEScopes(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
	cteScopes map[string]semantic.ScopeSource,
) (predicates.QueryPredicate, bool) {
	if md == nil || sq == nil || sq.tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)

	addSource := func(tableName, alias string) bool {
		aliasID := semantic.NewUnquoted(alias)
		if alias == "" {
			aliasID = semantic.NewUnquoted(tableName)
		}
		// Try metadata first, then CTE scopes.
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err == nil {
			return scope.AddSource(semantic.ScopeSource{
				Table:           tbl,
				Alias:           aliasID,
				CorrelationName: aliasID.Name(),
			}) == nil
		}
		if src, found := cteScopes[strings.ToUpper(tableName)]; found {
			src.Alias = aliasID
			src.CorrelationName = aliasID.Name()
			return scope.AddSource(src) == nil
		}
		return false
	}
	if !addSource(sq.tableName, sq.tableAlias) {
		return nil, false
	}
	for _, j := range sq.joins {
		if !addSource(j.tableName, j.alias) {
			return nil, false
		}
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildWherePredicateForJoins handles the JOIN case: builds a scope
// with one source per (primary table, joined tables) entry, then
// runs the walker. Bare columns ambiguous across sources fail at
// scope resolution → walker returns an error → fall back to text.
// Qualified columns (`Order.price`) resolve via ScopeSource alias.
//
// Each source needs a Table from the catalog. A miss on any one
// declines the whole predicate (the walker would have failed on
// the missing-table column ref anyway).
func buildWherePredicateForJoins(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if md == nil || sq == nil || sq.tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)

	addSource := func(tableName, alias string) bool {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		aliasID := semantic.NewUnquoted(alias)
		if alias == "" {
			aliasID = semantic.NewUnquoted(tableName)
		}
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}) == nil
	}
	if !addSource(sq.tableName, sq.tableAlias) {
		return nil, false
	}
	for _, j := range sq.joins {
		if !addSource(j.tableName, j.alias) {
			return nil, false
		}
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildLogicalPlanForSelectWithCatalog is the catalog-aware variant
// of buildLogicalPlanForSelect. It walks the WHERE predicate through
// the expr package and attaches a predicates.QueryPredicate tree to
// LogicalFilter when the walker succeeds; on any walker failure the
// filter falls back to the canonical source text (identical output
// to buildLogicalPlanForSelect for the WHERE shape alone).
//
// All non-WHERE operators (Scan / Join / Aggregate / Sort / Limit /
// Project) are identical to the text-only builder — only the
// LogicalFilter node differs when the walker succeeds. Passing md=nil
// is equivalent to calling buildLogicalPlanForSelect: every WHERE
// degrades to text.
//
// Follow-up wiring (not in this shift): plumb md into
// naive_generator's ExplainFn so Explain output shows simplified
// predicate trees when metadata is available.
func buildLogicalPlanForSelectWithCatalog(sq *selectQuery, md *recordlayer.RecordMetaData) (logical.LogicalOperator, error) {
	return buildLogicalPlanForSelectWithCTECatalog(sq, md, nil)
}

func buildLogicalPlanForSelectWithCTECatalog(sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) (logical.LogicalOperator, error) {
	// For derived tables, build the inner plan through the catalog-aware
	// path so WHERE predicates get upgraded. Java's visitSubqueryTableItem
	// recursively visits through the same typed visitor.
	if sq.derivedQuery != nil && md != nil {
		innerOp, innerErr := buildLogicalPlanForQueryBodyWithCTECatalog(
			sq.derivedQuery.QueryExpressionBody(), md, cteScopes)
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			// Build the outer plan (Sort, Limit, Project) on top of
			// the catalog-aware inner plan.
			op := buildOuterPlanOnDerived(sq, innerOp)
			if op == nil {
				return nil, nil
			}
			return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, cteScopes)
		}
	}

	// Pre-build derived table inner plans for JOIN sources through
	// the catalog-aware path (same as the primary source above).
	for i := range sq.joins {
		j := &sq.joins[i]
		if j.derivedQuery == nil {
			continue
		}
		innerOp, innerErr := buildLogicalPlanForQueryBodyWithCTECatalog(
			j.derivedQuery.QueryExpressionBody(), md, cteScopes)
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			j.catalogAwareInnerPlan = innerOp
		}
	}

	op := buildLogicalPlanForSelect(sq)
	if op == nil || md == nil || sq == nil {
		return op, nil
	}
	return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, cteScopes)
}

func buildLogicalPlanForSelectWithCTECatalog_postBuild(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) (logical.LogicalOperator, error) {
	// Build the semantic scope once. All identifier resolution below
	// goes through this scope — same architecture as Java's
	// QueryVisitor holding a SemanticAnalyzer.
	resolver := buildSelectScope(sq, md, cteScopes)

	// Expand qualified stars (a.*) in the projection list. Replaces each
	// qualified-star slot with explicit column names from the source.
	// Matches Java's SemanticAnalyzer.expandStar.
	if hasAnyQualifiedStar(sq) {
		expandQualifiedStars(sq, md)
		op = buildLogicalPlanForSelect(sq)
		if op == nil {
			return op, nil
		}
	}

	// Resolve projection columns through the scope. Only plain column
	// references (projExprs[i] == nil) are resolved — computed
	// expressions / literals have non-nil projExprs entries and go
	// through the expression walker instead. Skip aggregate queries
	// (aggCols / countStar) — their projection names are aggregate
	// output labels, not column references.
	if resolver != nil && sq.projCols != nil && len(sq.aggCols) == 0 && !sq.countStar {
		proj := findProjection(op)
		for i, col := range sq.projCols {
			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				continue
			}
			if err := resolveColumnName(resolver, col); err != nil {
				return nil, err
			}
			// For qualified column refs (d.id), resolve the Value so the
			// Cascades translator gets a FieldValue with the bare column
			// name, not the qualified "D.ID" text.
			if strings.Contains(col, ".") && proj != nil {
				var qualifier semantic.Identifier
				id := semantic.NewUnquoted(col)
				if dot := strings.IndexByte(col, '.'); dot >= 0 {
					qualifier = semantic.NewUnquoted(col[:dot])
					id = semantic.NewUnquoted(col[dot+1:])
				}
				if v, err := resolver.ResolveIdentifier(qualifier, id); err == nil {
					if proj.ProjectedValues == nil {
						proj.ProjectedValues = make([]values.Value, len(proj.Projections))
					}
					if i < len(proj.ProjectedValues) {
						proj.ProjectedValues[i] = v
					}
				}
			}
		}
	}

	// ORDER BY: Java's ExpressionVisitor.visitOrderByExpression walks each
	// ORDER BY expression through the expression visitor. Do the same —
	// the resolver detects ambiguous/undefined column references.
	// Build a set of projection aliases for ORDER BY resolution.
	projAliasSet := make(map[string]bool)
	if sq.projAliases != nil {
		for _, a := range sq.projAliases {
			if a != "" {
				projAliasSet[strings.ToUpper(a)] = true
			}
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" {
			projAliasSet[strings.ToUpper(ac.outName)] = true
		}
	}

	if resolver != nil {
		for _, ob := range sq.orderBy {
			if ob.rawExpr != nil {
				if _, walkErr := resolver.WalkExpression(ob.rawExpr); walkErr != nil {
					var ambigErr *semantic.AmbiguousColumnError
					if errors.As(walkErr, &ambigErr) {
						return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
							"column reference %q is ambiguous", ob.colName)
					}
					var notFoundErr *semantic.ColumnNotFoundError
					if errors.As(walkErr, &notFoundErr) {
						// Check if the ORDER BY name is a SELECT alias.
						if projAliasSet[strings.ToUpper(ob.colName)] {
							continue
						}
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column %q does not exist", ob.colName)
					}
				}
			}
		}
	}

	// Resolve GROUP BY columns through the scope.
	if resolver != nil && len(sq.aggCols) == 0 {
		for _, gb := range sq.groupBy {
			if err := resolveColumnName(resolver, gb); err != nil {
				return nil, err
			}
		}
	}

	if len(sq.groupBy) > 0 && !sq.countStar {
		if err := validateGroupByProjection(sq, md); err != nil {
			return nil, err
		}
	}

	// Detect overflow numeric literals in projection expressions.
	if resolver != nil && len(sq.projExprs) > 0 {
		for _, e := range sq.projExprs {
			if e == nil {
				continue
			}
			if _, walkErr := resolver.WalkExpressionForProjection(e); walkErr != nil {
				var overflow *expr.NumericOverflowLiteralError
				if errors.As(walkErr, &overflow) {
					return nil, api.NewError(api.ErrCodeNumericValueOutOfRange, overflow.Error())
				}
			}
		}
	}

	if cteScopes != nil && len(sq.joins) == 0 {
		if src, found := cteScopes[strings.ToUpper(sq.tableName)]; found && src.ColumnAliasMap != nil {
			rewriteProjectionAliases(op, src.ColumnAliasMap)
		}
	}
	if sq.derivedQuery != nil {
		if src, ok := buildDerivedTableSource(md, sq.tableName, sq.derivedQuery); ok && src.ColumnAliasMap != nil {
			rewriteProjectionAliases(op, src.ColumnAliasMap)
		}
	}

	if len(sq.joins) > 0 {
		upgradeJoinOnPredicates(op, sq, md, cteScopes)
	}

	if len(sq.aggCols) > 0 {
		upgradeAggregateOperands(op, sq, md, cteScopes)
	}

	if len(sq.projExprs) > 0 || len(sq.postAggExprs) > 0 {
		upgradeProjectionValues(op, sq, md, cteScopes)
	}

	if sq.havingExpr != nil {
		upgradeHavingPredicate(op, sq, md, cteScopes)
	}

	upgradeSortKeyValues(op, sq, md, cteScopes)

	if sq.whereExpr == nil {
		return op, nil
	}

	// Walk WHERE expression through the resolver to catch ambiguous/
	// undefined column references before the predicate builder. The
	// predicate builder swallows errors into text fallback — this
	// check ensures semantic errors surface with correct SQLSTATE.
	if resolver != nil && sq.whereExpr.Expression() != nil {
		if _, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression()); walkErr != nil {
			var ambigErr *semantic.AmbiguousColumnError
			if errors.As(walkErr, &ambigErr) {
				return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
					"column reference is ambiguous")
			}
			var inListNull *expr.InListNullError
			if errors.As(walkErr, &inListNull) {
				return nil, api.NewError(api.ErrCodeCannotConvertType,
					"IN-list contains NULL literal")
			}
		}
	}

	var pred predicates.QueryPredicate
	var ok bool
	if cteScopes != nil && len(sq.joins) == 0 {
		if src, found := cteScopes[strings.ToUpper(sq.tableName)]; found {
			pred, ok = buildWherePredicateFromCTEScope(src, sq.tableAlias, sq.whereExpr, md)
		}
	}
	if !ok && cteScopes != nil && len(sq.joins) > 0 {
		pred, ok = buildWherePredicateForJoinsWithCTEScopes(md, sq, sq.whereExpr, cteScopes)
	}
	if !ok {
		pred, ok = buildWherePredicate(md, sq, sq.whereExpr)
	}
	if !ok {
		return op, nil
	}
	_ = upgradeFirstFilter(op, pred)
	return op, nil
}

// buildSelectScope builds a semantic scope + resolver from the FROM
// clause of a selectQuery. This is the single point of scope
// construction — all identifier resolution (projection, ORDER BY,
// GROUP BY, WHERE, ON) goes through the returned resolver.
//
// Returns nil resolver when the scope can't be built (missing metadata,
// CTE-only sources without schema, etc.). Callers fall back to text.
func buildSelectScope(
	sq *selectQuery,
	md *recordlayer.RecordMetaData,
	cteScopes map[string]semantic.ScopeSource,
) *expr.Resolver {
	if sq == nil || md == nil || sq.tableName == "" {
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)

	addSource := func(tableName, alias string) bool {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil && cteScopes != nil {
			if src, found := cteScopes[strings.ToUpper(tableName)]; found {
				aliasID := semantic.NewUnquoted(alias)
				if alias == "" {
					aliasID = semantic.NewUnquoted(tableName)
				}
				return scope.AddSource(semantic.ScopeSource{
					Table:           src.Table,
					Alias:           aliasID,
					CorrelationName: aliasID.Name(),
					ColumnAliasMap:  src.ColumnAliasMap,
				}) == nil
			}
			return false
		}
		if err != nil {
			return false
		}
		aliasID := semantic.NewUnquoted(alias)
		if alias == "" {
			aliasID = semantic.NewUnquoted(tableName)
		}
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}) == nil
	}

	if sq.derivedQuery != nil {
		if src, ok := buildDerivedTableSource(md, sq.tableName, sq.derivedQuery); ok {
			if scope.AddSource(src) != nil {
				return nil
			}
		} else {
			return nil
		}
	} else if !addSource(sq.tableName, sq.tableAlias) {
		return nil
	}
	for _, j := range sq.joins {
		if j.derivedQuery != nil {
			if src, ok := buildDerivedTableSource(md, j.alias, j.derivedQuery); ok {
				if scope.AddSource(src) != nil {
					return nil
				}
				continue
			}
		}
		if !addSource(j.tableName, j.alias) {
			return nil
		}
	}
	return expr.New(analyzer, scope)
}

// resolveColumnName resolves a bare column name through the semantic
// scope. Returns an error for ambiguous (42702) or undefined (42703)
// columns. Returns nil for qualified names (contain ".") or when
// resolver is nil.
func resolveColumnName(resolver *expr.Resolver, col string) error {
	if resolver == nil || col == "" {
		return nil
	}
	var qualifier semantic.Identifier
	id := semantic.NewUnquoted(col)
	if dot := strings.IndexByte(col, '.'); dot >= 0 {
		qualifier = semantic.NewUnquoted(col[:dot])
		id = semantic.NewUnquoted(col[dot+1:])
	}
	_, err := resolver.ResolveIdentifier(qualifier, id)
	if err != nil {
		var ambigErr *semantic.AmbiguousColumnError
		if errors.As(err, &ambigErr) {
			return api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"column reference %q is ambiguous", col)
		}
		var notFoundErr *semantic.ColumnNotFoundError
		if errors.As(err, &notFoundErr) {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist", col)
		}
		var srcNotFound *semantic.SourceNotFoundError
		if errors.As(err, &srcNotFound) {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist", col)
		}
	}
	return nil
}

func validateQualifiedStarSources(sq *selectQuery, md *recordlayer.RecordMetaData) error {
	if sq == nil || md == nil {
		return nil
	}
	validSources := make(map[string]bool)
	if sq.tableName != "" {
		validSources[strings.ToUpper(sq.tableName)] = true
		if sq.tableAlias != "" {
			validSources[strings.ToUpper(sq.tableAlias)] = true
		}
	}
	for _, j := range sq.joins {
		if j.tableName != "" {
			validSources[strings.ToUpper(j.tableName)] = true
		}
		if j.alias != "" {
			validSources[strings.ToUpper(j.alias)] = true
		}
	}
	check := func(qual string) error {
		if qual == "" {
			return nil
		}
		if !validSources[strings.ToUpper(qual)] {
			return api.NewErrorf(api.ErrCodeUndefinedTable, "table %q does not exist", strings.ToUpper(qual))
		}
		return nil
	}
	if err := check(sq.projQualifier); err != nil {
		return err
	}
	for _, q := range sq.projStarQualifiers {
		if err := check(q); err != nil {
			return err
		}
	}
	return nil
}

func rewriteProjectionAliases(op logical.LogicalOperator, aliasMap map[string]string) {
	proj := findProjection(op)
	if proj == nil {
		return
	}
	for i, col := range proj.Projections {
		upper := strings.ToUpper(col)
		if real, ok := aliasMap[upper]; ok {
			proj.Projections[i] = real
			if i < len(proj.Aliases) && proj.Aliases[i] == "" {
				proj.Aliases[i] = col
			}
		}
	}
}

// upgradeFirstFilter walks the single-child chain from op and, at
// the first LogicalFilter, sets Predicate. Stops at the first
// non-unary node. Returns true when a Filter was found and upgraded.
func upgradeFirstFilter(op logical.LogicalOperator, pred predicates.QueryPredicate) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.Predicate = pred
			return true
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return false
		}
		cur = ch[0]
	}
	return false
}

// upgradeProjectionValues walks the unary spine from op to find the
// LogicalProject node, then attempts to resolve each projExpr through
// the expr.Resolver to produce a values.Value tree. Successful slots
// are stored in LogicalProject.ProjectedValues; failed slots remain nil
// (the Cascades translator treats nil as "plain column reference" when
// the text isn't a computed expression, or "cannot translate" otherwise).
func upgradeProjectionValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) {
	proj := findProjection(op)
	if proj == nil {
		return
	}
	// Post-aggregation projections: walk through the Resolver using base
	// table scope, then rewrite AggregateValues to FieldValue references.
	if len(sq.postAggExprs) > 0 {
		resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
		if resolver == nil {
			return
		}
		vals := make([]values.Value, len(proj.Projections))
		for i, e := range sq.postAggExprs {
			if i >= len(vals) || e == nil {
				continue
			}
			v, err := resolver.WalkExpression(e)
			if err != nil {
				continue
			}
			v = rewriteAggregateValuesInTree(v)
			vals[i] = v
		}
		proj.ProjectedValues = vals
		return
	}

	// Regular projections.
	exprs := sq.projExprs
	if len(exprs) == 0 {
		return
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
	if resolver == nil {
		return
	}
	vals := make([]values.Value, len(proj.Projections))
	copy(vals, proj.ProjectedValues)
	for i, e := range exprs {
		if i >= len(vals) {
			break
		}
		if e == nil {
			continue
		}
		v, err := resolver.WalkExpressionForProjection(e)
		if err != nil {
			continue
		}
		if !isCascadesSafeValue(v) {
			continue
		}
		v = rewriteAggregateValuesInTree(v)
		vals[i] = v
	}
	proj.ProjectedValues = vals
}

// isCascadesSafeValue checks whether v's tree contains only Value types
// that Java's Cascades planner supports. Rejects ScalarFunctionValue
// names not in the planner's function catalog (UPPER, SQRT, etc.).
func isCascadesSafeValue(v values.Value) bool {
	safe := true
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !cascadesSafeScalarFunction(sf.FuncName) {
				safe = false
				return false
			}
		}
		return true
	})
	return safe
}

func cascadesSafeScalarFunction(name string) bool {
	return values.IsCascadesSafeScalarFunction(name)
}

func upgradeAggregateOperands(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) {
	agg := findAggregate(op)
	if agg == nil {
		return
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
	if resolver == nil {
		return
	}
	operands := make([]values.Value, len(agg.Aggregates))
	for _, ac := range sq.aggCols {
		if ac.aggFunc == "" || ac.aggExpr == nil {
			continue
		}
		idx := -1
		for i, aggText := range agg.Aggregates {
			arg := ac.aggArg
			if arg == "" && ac.aggExpr != nil {
				arg = canonicalTextOf(ac.aggExpr)
			}
			if arg == "" {
				arg = "*"
			}
			distinctPfx := ""
			if ac.aggDistinct {
				distinctPfx = "DISTINCT "
			}
			expected := strings.ToUpper(ac.aggFunc + "(" + distinctPfx + arg + ")")
			if strings.ToUpper(aggText) == expected {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		v, err := resolver.WalkExpression(ac.aggExpr)
		if err != nil {
			continue
		}
		operands[idx] = v
	}
	agg.AggregateOperands = operands
}

func upgradeHavingPredicate(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) {
	agg := findAggregate(op)
	if agg == nil || sq.havingExpr == nil {
		return
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
	if resolver == nil {
		return
	}
	pred, err := resolver.WalkPredicate(sq.havingExpr)
	if err != nil {
		return
	}
	agg.HavingPredicate = rewriteAggregateRefsInPredicate(pred)
}

func rewriteAggregateRefsInPredicate(pred predicates.QueryPredicate) predicates.QueryPredicate {
	switch p := pred.(type) {
	case *predicates.ComparisonPredicate:
		lhs := rewriteAggregateValuesInTree(p.Operand)
		rhs := rewriteAggregateValuesInTree(p.Comparison.Operand)
		return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
			Type:    p.Comparison.Type,
			Operand: rhs,
		})
	case *predicates.AndPredicate:
		rewritten := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sub := range p.SubPredicates {
			rewritten[i] = rewriteAggregateRefsInPredicate(sub)
		}
		return predicates.NewAnd(rewritten...)
	case *predicates.OrPredicate:
		rewritten := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sub := range p.SubPredicates {
			rewritten[i] = rewriteAggregateRefsInPredicate(sub)
		}
		return predicates.NewOr(rewritten...)
	}
	return pred
}

func rewriteAggregateValuesInTree(v values.Value) values.Value {
	if v == nil {
		return nil
	}
	if _, ok := v.(*values.AggregateValue); ok {
		return rewriteAggregateValue(v)
	}
	if av, ok := v.(*values.ArithmeticValue); ok {
		return &values.ArithmeticValue{
			Op:    av.Op,
			Left:  rewriteAggregateValuesInTree(av.Left),
			Right: rewriteAggregateValuesInTree(av.Right),
		}
	}
	if sf, ok := v.(*values.ScalarFunctionValue); ok {
		args := make([]values.Value, len(sf.Args))
		for i, a := range sf.Args {
			args[i] = rewriteAggregateValuesInTree(a)
		}
		return values.NewScalarFunctionValue(sf.FuncName, sf.Typ, args...)
	}
	if cv, ok := v.(*values.CastValue); ok {
		return values.NewCastValue(rewriteAggregateValuesInTree(cv.Child), cv.Target)
	}
	if pv, ok := v.(*values.PickValue); ok {
		alts := make([]values.Value, len(pv.Alternatives))
		for i, a := range pv.Alternatives {
			alts[i] = rewriteAggregateValuesInTree(a)
		}
		return values.NewPickValue(rewriteAggregateValuesInTree(pv.Selector), alts, pv.Typ)
	}
	if cs, ok := v.(*values.ConditionSelectorValue); ok {
		impl := make([]values.Value, len(cs.Implications))
		for i, c := range cs.Implications {
			impl[i] = rewriteAggregateValuesInTree(c)
		}
		return values.NewConditionSelectorValue(impl)
	}
	if ph, ok := v.(expr.PredicateValueHolder); ok {
		rewritten := rewriteAggregateRefsInPredicate(ph.GetPredicate())
		ph.SetPredicate(rewritten)
		return ph
	}
	return v
}

func rewriteAggregateValue(v values.Value) values.Value {
	if v == nil {
		return nil
	}
	if _, ok := v.(*values.AggregateValue); ok {
		return &values.FieldValue{
			Field: strings.ToUpper(values.ExplainValue(v)),
			Typ:   values.UnknownType,
		}
	}
	return v
}

func findAggregate(op logical.LogicalOperator) *logical.LogicalAggregate {
	for cur := op; cur != nil; {
		if a, ok := cur.(*logical.LogicalAggregate); ok {
			return a
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return nil
		}
		cur = ch[0]
	}
	return nil
}

func findProjection(op logical.LogicalOperator) *logical.LogicalProject {
	for cur := op; cur != nil; {
		if p, ok := cur.(*logical.LogicalProject); ok {
			return p
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return nil
		}
		cur = ch[0]
	}
	return nil
}

func validateGroupByProjection(sq *selectQuery, _ *recordlayer.RecordMetaData) error {
	groupBySet := make(map[string]bool, len(sq.groupBy))
	for _, gb := range sq.groupBy {
		groupBySet[strings.ToUpper(gb)] = true
		if dot := strings.LastIndex(gb, "."); dot >= 0 {
			groupBySet[strings.ToUpper(gb[dot+1:])] = true
		}
	}

	if len(sq.aggCols) > 0 {
		for _, ac := range sq.aggCols {
			if ac.aggFunc != "" || ac.hidden || ac.sortOnly || ac.outExpr != nil {
				continue
			}
			col := ac.groupCol
			if col == "" {
				col = ac.outName
			}
			upper := strings.ToUpper(col)
			if dot := strings.LastIndex(upper, "."); dot >= 0 {
				upper = upper[dot+1:]
			}
			if !groupBySet[upper] && !groupBySet[strings.ToUpper(col)] {
				return api.NewErrorf(api.ErrCodeGroupingError,
					"column %q must appear in the GROUP BY clause or be used in an aggregate function", col)
			}
		}
		return nil
	}

	for i, col := range sq.projCols {
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			continue
		}
		upper := strings.ToUpper(col)
		if dot := strings.LastIndex(upper, "."); dot >= 0 {
			upper = upper[dot+1:]
		}
		if !groupBySet[upper] && !groupBySet[strings.ToUpper(col)] {
			return api.NewErrorf(api.ErrCodeGroupingError,
				"column %q must appear in the GROUP BY clause or be used in an aggregate function", col)
		}
	}
	return nil
}

func buildProjectionResolverWithCTEScopes(sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) *expr.Resolver {
	if sq.tableName == "" && len(cteScopes) == 0 {
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)
	addSource := func(tableName, alias string) bool {
		if src, ok := cteScopes[strings.ToUpper(tableName)]; ok {
			aliasID := semantic.NewUnquoted(alias)
			if alias == "" {
				aliasID = semantic.NewUnquoted(tableName)
			}
			src.Alias = aliasID
			src.CorrelationName = aliasID.Name()
			return scope.AddSource(src) == nil
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		aliasID := semantic.NewUnquoted(alias)
		if alias == "" {
			aliasID = semantic.NewUnquoted(tableName)
		}
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}) == nil
	}
	if sq.tableName != "" {
		if !addSource(sq.tableName, sq.tableAlias) {
			return nil
		}
	}
	for _, j := range sq.joins {
		if !addSource(j.tableName, j.alias) {
			return nil
		}
	}
	return expr.New(analyzer, scope)
}

// buildLogicalPlanForDeleteWithCatalog is the catalog-aware variant
// of buildLogicalPlanForDelete. If the WHERE walks cleanly through
// the expr resolver, the emitted LogicalFilter carries a
// QueryPredicate tree; otherwise the plan is identical to the
// text-only builder.
func buildLogicalPlanForDeleteWithCatalog(
	del antlrgen.IDeleteStatementContext,
	md *recordlayer.RecordMetaData,
) logical.LogicalOperator {
	op := buildLogicalPlanForDelete(del)
	if op == nil || md == nil || del == nil {
		return op
	}
	tableName := ""
	if tn := del.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	w := del.WhereExpr()
	if w == nil || tableName == "" {
		return op
	}
	pred, ok := buildWherePredicateForTable(md, tableName, "", w)
	if !ok {
		return op
	}
	_ = upgradeFirstFilter(op, pred) // invariant: text builder always emits a Filter for a WHERE clause
	return op
}

// buildLogicalPlanForUpdateWithCatalog is the catalog-aware variant
// of buildLogicalPlanForUpdate. Same shape as the Delete variant —
// walker failure falls back to text form on LogicalFilter.
func buildLogicalPlanForUpdateWithCatalog(
	upd antlrgen.IUpdateStatementContext,
	md *recordlayer.RecordMetaData,
) logical.LogicalOperator {
	op := buildLogicalPlanForUpdate(upd)
	if op == nil || md == nil || upd == nil {
		return op
	}
	tableName := ""
	if tn := upd.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	w := upd.WhereExpr()
	if w == nil || tableName == "" {
		return op
	}
	pred, ok := buildWherePredicateForTable(md, tableName, "", w)
	if !ok {
		return op
	}
	_ = upgradeFirstFilter(op, pred) // invariant: text builder always emits a Filter for a WHERE clause
	return op
}

// buildLogicalPlanForInsertWithCatalog is the catalog-aware variant
// of buildLogicalPlanForInsert. INSERT VALUES has no nested query so
// it short-circuits to the text builder; INSERT … SELECT routes the
// inner SELECT through the catalog-aware Select path so its WHERE
// becomes a predicate tree when md is non-nil.
func buildLogicalPlanForInsertWithCatalog(
	ins antlrgen.IInsertStatementContext,
	md *recordlayer.RecordMetaData,
) logical.LogicalOperator {
	if ins == nil {
		return nil
	}
	if md == nil {
		return buildLogicalPlanForInsert(ins)
	}
	op := buildLogicalPlanForInsert(ins)
	if op == nil {
		return op
	}
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok || insertOp.Source == nil {
		// VALUES form (no Source) — nothing to upgrade.
		return op
	}
	// Re-run the inner SELECT through the catalog-aware path. We
	// can't directly mutate the existing Source's filter without
	// re-walking the SELECT, so just rebuild Source.
	selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext)
	if !ok {
		return op
	}
	body := selCtx.QueryExpressionBody()
	if body == nil {
		return op
	}
	termDefault, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return op
	}
	simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return op
	}
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return op
	}
	// Defensive: only swap Source when the catalog-aware build
	// produced a non-nil tree. Today buildLogicalPlanForSelectWithCatalog
	// can't return nil while buildLogicalPlanForSelect returned non-nil
	// (same ANTLR node, same extractFromSimpleTable contract), but
	// pinning the invariant in code instead of in the comment guards
	// against future divergence between the text and catalog paths.
	if upgraded, err := buildLogicalPlanForSelectWithCatalog(sq, md); err == nil && upgraded != nil {
		insertOp.Source = upgraded
	}
	return insertOp
}

// buildLogicalPlanForQueryWithCatalog is the catalog-aware variant
// of buildLogicalPlanForQuery. Recurses into CTE bodies and the
// query body so WHERE clauses anywhere in the tree pick up the
// metadata when available. md=nil collapses to the text builder.
func buildLogicalPlanForQueryWithCatalog(
	q antlrgen.IQueryContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if q == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()

	// Pre-scan CTE definitions to extract column schemas. Process in
	// declaration order so CTE B can reference CTE A's derived schema.
	var cteScopes map[string]semantic.ScopeSource
	if ctesCtx != nil {
		cteScopes = make(map[string]semantic.ScopeSource)
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if _, exists := cteScopes[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			if src, ok := buildCTEColumnSource(md, name, nq.Query(), cteScopes); ok {
				// Apply CTE column aliases: WITH c1(x, y) AS (...)
				if colAliases := nq.GetColumnAliases(); colAliases != nil {
					src = applyCTEColumnAliases(src, colAliases)
				}
				cteScopes[upper] = src
			}
		}
	}

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, cteScopes)
	if err != nil {
		return nil, err
	}
	if main == nil {
		return nil, nil
	}
	if ctesCtx == nil {
		return main, nil
	}
	recursive := ctesCtx.RECURSIVE() != nil
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			body, err = buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, cteScopes)
			if err != nil {
				return nil, err
			}
		}
		if body == nil {
			return nil, nil
		}
		main = logical.NewCTE(name, body, main, recursive)
	}
	return main, nil
}

// buildLogicalPlanForQueryBodyWithCatalog dispatches simple SELECT
// vs UNION, threading md through both arms. Mirrors the text
// builder's QueryTermDefault / SetQuery split.
func buildLogicalPlanForQueryBodyWithCatalog(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil, nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil, err
		}
		if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
			return nil, api.NewError(api.ErrCodeUndefinedFunction,
				"Unsupported operator "+fn)
		}
		if err := validateQualifiedStarSources(sq, md); err != nil {
			return nil, err
		}
		return buildLogicalPlanForSelectWithCatalog(sq, md)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCatalog(b, md)
	}
	return nil, nil
}

// buildLogicalPlanForQueryBodyWithCTECatalog is like
// buildLogicalPlanForQueryBodyWithCatalog but passes CTE-derived
// column schemas to the predicate builder so WHERE clauses on CTE
// references can produce real QueryPredicates.
func buildLogicalPlanForQueryBodyWithCTECatalog(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
	cteScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	if len(cteScopes) == 0 {
		return buildLogicalPlanForQueryBodyWithCatalog(body, md)
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil, nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil, err
		}
		if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
			return nil, api.NewError(api.ErrCodeUndefinedFunction,
				"Unsupported operator "+fn)
		}
		if err := validateQualifiedStarSources(sq, md); err != nil {
			return nil, err
		}
		return buildLogicalPlanForSelectWithCTECatalog(sq, md, cteScopes)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCTECatalog(b, md, cteScopes)
	}
	return nil, nil
}

func buildLogicalPlanForUnionWithCTECatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
	cteScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	distinct := true
	if q := setQ.GetQuantifier(); q != nil && strings.EqualFold(q.GetText(), "ALL") {
		distinct = false
	}
	left, err := buildLogicalPlanForQueryBodyWithCTECatalog(setQ.GetLeft(), md, cteScopes)
	if err != nil {
		return nil, err
	}
	right, err := buildLogicalPlanForQueryBodyWithCTECatalog(setQ.GetRight(), md, cteScopes)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}
	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && innerUnion.Distinct == distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	return logical.NewUnion(inputs, distinct), nil
}

// upgradeSortKeyValues walks the logical plan's LogicalSort and resolves
// sort key expressions through the expression walker. When an ORDER BY
// key is an aggregate expression (SUM(v)*2, COALESCE(SUM(v),0)), the
// walker produces a Value tree with AggregateValues rewritten to
// FieldValues referencing the aggregate output.
func upgradeSortKeyValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) {
	sort := findSort(op)
	if sort == nil || len(sort.Keys) == 0 {
		return
	}

	// Build alias→column mapping from projections.
	aliasToCol := make(map[string]string)
	aliasToIdx := make(map[string]int)
	if sq.projAliases != nil && sq.projCols != nil {
		for i, a := range sq.projAliases {
			if a != "" && i < len(sq.projCols) {
				aliasToCol[strings.ToUpper(a)] = sq.projCols[i]
				aliasToIdx[strings.ToUpper(a)] = i
			}
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" && ac.groupCol != "" {
			aliasToCol[strings.ToUpper(ac.outName)] = ac.groupCol
		}
	}

	// Resolve ORDER BY alias → underlying column or Value.
	// SQL standard (and Java): ORDER BY resolves to SELECT-list output
	// column names first, then table columns. Aliases take precedence.
	proj := findProjection(op)
	for i := range sort.Keys {
		upper := strings.ToUpper(sort.Keys[i].Expr)
		if real, ok := aliasToCol[upper]; ok {
			sort.Keys[i].Expr = real
		}
		if idx, ok := aliasToIdx[upper]; ok && proj != nil {
			if idx < len(proj.ProjectedValues) && proj.ProjectedValues[idx] != nil {
				sort.Keys[i].Value = proj.ProjectedValues[idx]
			}
		}
	}

	resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
	if resolver == nil {
		return
	}
	for i := range sort.Keys {
		ob := findOrderByForKey(sq, sort.Keys[i].Expr)
		if ob == nil || ob.rawExpr == nil {
			continue
		}
		if ob.colName != "" && !strings.HasPrefix(ob.colName, "__orderby_aggexpr_") {
			continue
		}
		v, err := resolver.WalkExpression(ob.rawExpr)
		if err != nil {
			continue
		}
		v = rewriteAggregateValuesInTree(v)
		sort.Keys[i].Value = v
	}
}

func findSort(op logical.LogicalOperator) *logical.LogicalSort {
	if op == nil {
		return nil
	}
	if s, ok := op.(*logical.LogicalSort); ok {
		return s
	}
	for _, ch := range op.Children() {
		if s := findSort(ch); s != nil {
			return s
		}
	}
	return nil
}

func findOrderByForKey(sq *selectQuery, keyExpr string) *orderByClause {
	if sq == nil {
		return nil
	}
	for i := range sq.orderBy {
		ob := &sq.orderBy[i]
		name := ob.colName
		if name == "" && ob.rawExpr != nil {
			name = ob.rawExpr.GetText()
		}
		if strings.EqualFold(name, keyExpr) {
			return ob
		}
	}
	return nil
}

// buildOuterPlanOnDerived builds the Sort/Limit/Project/Distinct shell
// from a selectQuery on top of an already-built inner plan (derived table).
// Mirrors buildLogicalPlanForSelect's post-FROM logic but skips the
// FROM-source and aggregate sections (those are in the inner plan).
func buildOuterPlanOnDerived(sq *selectQuery, innerOp logical.LogicalOperator) logical.LogicalOperator {
	op := innerOp

	if sq.whereExpr != nil {
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
	}

	if len(sq.orderBy) > 0 {
		keys := make([]logical.SortKey, 0, len(sq.orderBy))
		for _, ob := range sq.orderBy {
			dir := logical.SortAsc
			if !ob.ascending {
				dir = logical.SortDesc
			}
			e := ob.colName
			if e == "" && ob.rawExpr != nil {
				e = ob.rawExpr.GetText()
			}
			nullsFirst := ob.ascending
			if ob.nullsFirst != nil {
				nullsFirst = *ob.nullsFirst
			}
			keys = append(keys, logical.SortKey{Expr: e, Dir: dir, NullsFirst: nullsFirst})
		}
		op = logical.NewSort(op, keys)
	}

	if sq.limit >= 0 || sq.offset > 0 {
		op = logical.NewLimit(op, sq.limit, sq.offset)
	}

	if len(sq.projCols) > 0 {
		projs := make([]string, len(sq.projCols))
		aliases := make([]string, len(sq.projCols))
		computed := make([]bool, len(sq.projCols))
		for i, col := range sq.projCols {
			projs[i] = col
			if sq.projExprs != nil && i < len(sq.projExprs) && sq.projExprs[i] != nil {
				projs[i] = strings.TrimSpace(sq.projExprs[i].GetText())
				computed[i] = true
			}
			if sq.projAliases != nil && i < len(sq.projAliases) {
				aliases[i] = sq.projAliases[i]
			}
		}
		proj := logical.NewProject(op, projs, aliases)
		proj.IsComputed = computed
		op = proj
	}

	if sq.distinct {
		op = logical.NewDistinct(op)
	}

	return op
}

func hasAnyQualifiedStar(sq *selectQuery) bool {
	if sq == nil || sq.projStarQualifiers == nil {
		return false
	}
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			return true
		}
	}
	return false
}

// expandQualifiedStars replaces qualified-star projection slots (a.*)
// with explicit column names from the matching source table. Modifies
// sq.projCols, sq.projAliases, sq.projExprs, sq.projStarQualifiers in place.
func expandQualifiedStars(sq *selectQuery, md *recordlayer.RecordMetaData) {
	if sq == nil || sq.projCols == nil || sq.projStarQualifiers == nil {
		return
	}
	hasQualifiedStar := false
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			hasQualifiedStar = true
			break
		}
	}
	if !hasQualifiedStar {
		return
	}

	// Build a map of source alias → table columns.
	sourceColumns := make(map[string][]string)
	addSource := func(tableName, alias string) {
		rt := md.GetRecordType(tableName)
		if rt == nil || rt.Descriptor == nil {
			return
		}
		key := strings.ToUpper(alias)
		if key == "" {
			key = strings.ToUpper(tableName)
		}
		fields := rt.Descriptor.Fields()
		cols := make([]string, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			cols[i] = strings.ToUpper(string(fields.Get(i).Name()))
		}
		sourceColumns[key] = cols
	}
	if sq.tableName != "" {
		alias := sq.tableAlias
		if alias == "" {
			alias = sq.tableName
		}
		addSource(sq.tableName, alias)
	}
	for _, j := range sq.joins {
		alias := j.alias
		if alias == "" {
			alias = j.tableName
		}
		addSource(j.tableName, alias)
	}

	var newCols, newAliases, newQuals []string
	var newExprs []antlrgen.IExpressionContext
	for i, col := range sq.projCols {
		qual := ""
		if i < len(sq.projStarQualifiers) {
			qual = sq.projStarQualifiers[i]
		}
		if qual == "" {
			newCols = append(newCols, col)
			alias := ""
			if i < len(sq.projAliases) {
				alias = sq.projAliases[i]
			}
			newAliases = append(newAliases, alias)
			var expr antlrgen.IExpressionContext
			if i < len(sq.projExprs) {
				expr = sq.projExprs[i]
			}
			newExprs = append(newExprs, expr)
			newQuals = append(newQuals, "")
			continue
		}
		// Qualified star — expand to individual columns.
		cols, ok := sourceColumns[strings.ToUpper(qual)]
		if !ok {
			newCols = append(newCols, col)
			newAliases = append(newAliases, "")
			newExprs = append(newExprs, nil)
			newQuals = append(newQuals, qual)
			continue
		}
		for _, c := range cols {
			newCols = append(newCols, qual+"."+c)
			newAliases = append(newAliases, "")
			newExprs = append(newExprs, nil)
			newQuals = append(newQuals, "")
		}
	}
	sq.projCols = newCols
	sq.projAliases = newAliases
	sq.projExprs = newExprs
	sq.projStarQualifiers = newQuals
}

// validateUnionColumnCounts checks that all UNION branches project the
// same number of columns. Matches Java's SemanticAnalyzer.validateUnionTypes
// column-count check (ErrorCode.UNION_INCORRECT_COLUMN_COUNT / 42F64).
func validateUnionColumnCounts(inputs []logical.LogicalOperator) error {
	if len(inputs) < 2 {
		return nil
	}
	firstCount := countProjectionColumns(inputs[0])
	if firstCount < 0 {
		return nil
	}
	for i := 1; i < len(inputs); i++ {
		c := countProjectionColumns(inputs[i])
		if c < 0 {
			continue
		}
		if c != firstCount {
			return api.NewErrorf(api.ErrCodeUnionIncorrectColumnCount,
				"UNION legs do not have the same number of columns")
		}
	}
	return nil
}

func countProjectionColumns(op logical.LogicalOperator) int {
	if op == nil {
		return -1
	}
	if proj, ok := op.(*logical.LogicalProject); ok {
		return len(proj.Projections)
	}
	for _, ch := range op.Children() {
		if n := countProjectionColumns(ch); n >= 0 {
			return n
		}
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		_ = scan
		return -1
	}
	return -1
}

// buildLogicalPlanForUnionWithCatalog mirrors buildLogicalPlanForUnion
// — same flattening logic, threads md to each branch.
func buildLogicalPlanForUnionWithCatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	distinct := true
	if q := setQ.GetQuantifier(); q != nil && strings.EqualFold(q.GetText(), "ALL") {
		distinct = false
	}
	left, err := buildLogicalPlanForQueryBodyWithCatalog(setQ.GetLeft(), md)
	if err != nil {
		return nil, err
	}
	right, err := buildLogicalPlanForQueryBodyWithCatalog(setQ.GetRight(), md)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}
	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && innerUnion.Distinct == distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	return logical.NewUnion(inputs, distinct), nil
}
