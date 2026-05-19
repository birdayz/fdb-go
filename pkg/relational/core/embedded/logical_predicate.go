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
	"fmt"
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
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CorrelatedExistsError is returned when buildCorrelatedExists fails.
// Detected via errors.As at the caller to propagate as
// ErrCodeUndefinedColumn for fallback to a richer outer scope.
type CorrelatedExistsError struct {
	Message string
	Cause   error
}

func (e *CorrelatedExistsError) Error() string { return e.Message }
func (e *CorrelatedExistsError) Unwrap() error { return e.Cause }

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
	if len(innerSQ.aggCols) > 0 || innerSQ.countStar {
		if len(innerSQ.joins) == 0 && innerSQ.tableName != "" {
			return buildDerivedTableSourceFromAgg(alias, innerSQ)
		}
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
	if len(innerSQ.joins) > 0 || innerSQ.tableName == "" {
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

	projCols := innerSQ.projCols
	if projCols == nil {
		// SELECT * — use all columns from the inner table in schema order.
		allCols := innerTbl.Columns()
		projCols = make([]string, len(allCols))
		for i, c := range allCols {
			projCols[i] = c.Id.Name()
		}
	}
	columns := make([]semantic.Column, 0, len(projCols))
	var colAliasMap map[string]string
	for i, col := range projCols {
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

func buildDerivedTableSourceFromAgg(alias string, sq *selectQuery) (semantic.ScopeSource, bool) {
	var columns []semantic.Column
	if sq.countStar {
		name := "COUNT(*)"
		if sq.countStarAlias != "" {
			name = sq.countStarAlias
		}
		columns = append(columns, semantic.Column{
			Id: semantic.NewUnquoted(name), Type: "BIGINT", Nullable: false,
		})
	}
	for _, ac := range sq.aggCols {
		name := ac.outName
		if name == "" {
			if ac.groupCol != "" {
				name = ac.groupCol
			} else {
				continue
			}
		}
		columns = append(columns, semantic.Column{
			Id: semantic.NewUnquoted(name), Type: "UNKNOWN", Nullable: true,
		})
	}
	if len(columns) == 0 {
		return semantic.ScopeSource{}, false
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
	}, true
}

// upgradeJoinOnPredicates walks the logical plan tree to find LogicalJoin
// nodes and upgrades their OnText to OnPredicate using the full join scope.
// The join nodes are created in order matching sq.joins, so we match
// them sequentially by walking the left-child spine (the builder chains
// joins left-to-right with op = NewJoin(op, right, ...)).
func upgradeJoinOnPredicates(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource) error {
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
		return nil
	}
	resolver := expr.New(analyzer, scope)

	// Match collected joins with sq.joins in reverse order.
	for i, j := range joins {
		sqIdx := len(sq.joins) - 1 - i
		if sqIdx < 0 || sqIdx >= len(sq.joins) {
			break
		}
		if sq.joins[sqIdx].onExpr != nil && j.OnPredicate == nil {
			pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr)
			if walkErr != nil {
				var srcNotFound *semantic.SourceNotFoundError
				if errors.As(walkErr, &srcNotFound) {
					return api.NewErrorf(api.ErrCodeUndefinedColumn,
						"no FROM source aliased as %s", srcNotFound.Alias.Name())
				}
				continue
			}
			j.OnPredicate = predicates.SimplifyPredicateValues(pred)
		}
	}
	return nil
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
	// The CTE body is either a simple QueryTermDefault (non-recursive) or a
	// SetQuery / UNION ALL (recursive). For recursive CTEs, derive the column
	// schema from the seed (left) branch of the UNION.
	var body *antlrgen.QueryTermDefaultContext
	switch b := cteQuery.QueryExpressionBody().(type) {
	case *antlrgen.QueryTermDefaultContext:
		body = b
	case *antlrgen.SetQueryContext:
		seed, ok := b.GetLeft().(*antlrgen.QueryTermDefaultContext)
		if !ok {
			return semantic.ScopeSource{}, false
		}
		body = seed
	default:
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
	if sq.derivedQuery != nil && md != nil && len(sq.joins) == 0 {
		innerOp, innerErr := buildLogicalPlanForQueryBodyWithCTECatalog(
			sq.derivedQuery.QueryExpressionBody(), md, cteScopes)
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
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

func buildLogicalPlanForSelectWithCTECatalog_postBuild(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource, cteBodies ...map[string]logical.LogicalOperator) (logical.LogicalOperator, error) {
	// Build the semantic scope once. All identifier resolution below
	// goes through this scope — same architecture as Java's
	// QueryVisitor holding a SemanticAnalyzer.
	resolver := buildSelectScope(sq, md, cteScopes)

	// Expand qualified stars (a.*) in the projection list. Replaces each
	// qualified-star slot with explicit column names from the source.
	// Matches Java's SemanticAnalyzer.expandStar.
	//
	// Two shapes:
	//  1. projQualifier != "" && projCols == nil — `SELECT a.*` alone.
	//     The parser sets projQualifier but leaves projCols nil (which
	//     buildLogicalPlanForSelect treats as SELECT *, emitting no
	//     LogicalProject). For JOINs this is wrong — it must project
	//     only the qualifier's columns. Expand into explicit projCols.
	//  2. projStarQualifiers slots — `SELECT a.*, b.label` mixed.
	//     Handled by expandQualifiedStars (rewrites star slots in-place).
	needRebuild := false
	if sq.projQualifier != "" && sq.projCols == nil {
		expandProjQualifier(sq, md)
		needRebuild = true
	}
	if hasAnyQualifiedStar(sq) {
		expandQualifiedStars(sq, md)
		needRebuild = true
	}
	if needRebuild {
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
				if proj != nil {
					if v, walkErr := resolver.WalkExpression(sq.projExprs[i]); walkErr == nil && v != nil {
						if proj.ProjectedValues == nil {
							proj.ProjectedValues = make([]values.Value, len(proj.Projections))
						}
						if i < len(proj.ProjectedValues) {
							proj.ProjectedValues[i] = v
						}
					}
				}
				continue
			}
			if err := resolveColumnName(resolver, col); err != nil {
				return nil, err
			}
			if strings.Contains(col, ".") && proj != nil {
				if proj.ProjectedValues == nil {
					proj.ProjectedValues = make([]values.Value, len(proj.Projections))
				}
				if len(sq.joins) > 0 {
					if i < len(proj.ProjectedValues) {
						proj.ProjectedValues[i] = &values.FieldValue{
							Field: strings.ToUpper(col),
							Typ:   values.UnknownType,
						}
					}
				} else {
					var qualifier semantic.Identifier
					id := semantic.NewUnquoted(col)
					if dot := strings.IndexByte(col, '.'); dot >= 0 {
						qualifier = semantic.NewUnquoted(col[:dot])
						id = semantic.NewUnquoted(col[dot+1:])
					}
					if v, err := resolver.ResolveIdentifier(qualifier, id); err == nil {
						if i < len(proj.ProjectedValues) {
							proj.ProjectedValues[i] = v
						}
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

	for _, ob := range sq.orderBy {
		if ob.rawExpr != nil {
			hasSubquery := false
			walkScalarSubqueries(ob.rawExpr, func(_ antlrgen.IQueryContext) {
				hasSubquery = true
			})
			if hasSubquery {
				return nil, api.NewError(api.ErrCodeUnsupportedSort,
					"ORDER BY with scalar subquery is not supported")
			}
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
						// The ORDER BY rawExpr may reference a GROUP BY
						// alias (`ORDER BY z` where `GROUP BY x.col1 AS
						// z`). classifySelectElements rewrites ob.colName
						// to the underlying column, so colName now differs
						// from the rawExpr text. Try resolving the
						// rewritten colName through the scope; if it
						// resolves, the reference is valid.
						if ob.colName != "" && resolveColumnName(resolver, ob.colName) == nil {
							continue
						}
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column %q does not exist", ob.colName)
					}
				}
			}
		}
	}

	if resolver != nil {
		for i, gb := range sq.groupBy {
			if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
				continue
			}
			if err := resolveColumnName(resolver, gb); err != nil {
				return nil, err
			}
		}
	}

	if resolver != nil {
		for _, ac := range sq.aggCols {
			if ac.aggArg != "" && ac.aggExpr == nil {
				if err := resolveColumnName(resolver, ac.aggArg); err != nil {
					return nil, err
				}
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
				var binErr *expr.InvalidBinaryLiteralError
				if errors.As(walkErr, &binErr) {
					return nil, api.NewError(api.ErrCodeInvalidBinaryRepresentation, binErr.Error())
				}
			}
		}
	}

	// NOTE: CTE column-alias rewriting is intentionally NOT applied here.
	// The CTE alias wrapper (translateCTE → NewProject(origCols, aliases))
	// stores values under BOTH the original key and the alias key in the
	// executor's datum map. FieldValues created from the user's alias
	// names (e.g. "d", "val") resolve correctly because the alias keys
	// are present. Rewriting projection names to the underlying table
	// columns (via ColumnAliasMap) is redundant for single-level CTEs
	// and actively breaks chained CTEs: when CTE B reads from CTE A's
	// aliased columns and CTE C reads from CTE B's aliased columns,
	// the rewrite maps through only one level of aliasing, producing
	// FieldValues that point to intermediate names absent from the
	// output datum.
	if sq.derivedQuery != nil {
		if src, ok := buildDerivedTableSource(md, sq.tableName, sq.derivedQuery); ok && src.ColumnAliasMap != nil {
			rewriteProjectionAliases(op, src.ColumnAliasMap)
		}
	}

	if len(sq.joins) > 0 {
		if err := upgradeJoinOnPredicates(op, sq, md, cteScopes); err != nil {
			return nil, err
		}
	}

	if len(sq.aggCols) > 0 {
		upgradeAggregateOperands(op, sq, md, cteScopes)
	}

	// Create a unified SubqueryPlanner early so both projection and
	// WHERE walks can build inner plans for EXISTS and scalar subqueries.
	var existsPlanner *existsSubqueryPlanner
	if md != nil {
		var bodies map[string]logical.LogicalOperator
		if len(cteBodies) > 0 {
			bodies = cteBodies[0]
		}
		existsPlanner = &existsSubqueryPlanner{
			md:          md,
			outerScopes: buildOuterScopeSources(sq, md),
			cteScopes:   cteScopes,
			cteBodies:   bodies,
		}
	}

	if len(sq.projExprs) > 0 || len(sq.postAggExprs) > 0 {
		if err := upgradeProjectionValues(op, sq, md, cteScopes, existsPlanner); err != nil {
			return nil, err
		}
	}

	// Attach scalar subqueries from projections to the LogicalProject.
	if existsPlanner != nil && len(existsPlanner.scalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.ScalarSubqueries = existsPlanner.scalarSubqueries
		}
		// Reset scalar subqueries so WHERE walk starts fresh.
		existsPlanner.scalarSubqueries = nil
	}

	if sq.havingExpr != nil {
		upgradeHavingPredicate(op, sq, md, cteScopes, existsPlanner)
	}

	upgradeSortKeyValues(op, sq, md, cteScopes)

	if sq.whereExpr == nil {
		return op, nil
	}

	// Install the SubqueryPlanner on the resolver so EXISTS and scalar
	// subqueries in WHERE clauses can be planned.
	if resolver != nil && existsPlanner != nil {
		resolver.SetSubqueryPlanner(existsPlanner)
	}

	// Walk WHERE expression through the resolver to catch ambiguous/
	// undefined column references before the predicate builder. The
	// predicate builder swallows errors into text fallback — this
	// check ensures semantic errors surface with correct SQLSTATE.
	//
	// When the walk succeeds AND the SubqueryPlanner collected EXISTS
	// subqueries, use the pre-walk predicate directly — the
	// buildWherePredicate functions don't have a SubqueryPlanner and
	// would decline the EXISTS shape, falling back to text.
	var preWalkPred predicates.QueryPredicate
	if resolver != nil && sq.whereExpr.Expression() != nil {
		walked, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
		if walkErr != nil {
			var ambigErr *semantic.AmbiguousColumnError
			if errors.As(walkErr, &ambigErr) {
				return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
					"column reference is ambiguous")
			}
			var inListNull *expr.InListNullError
			if errors.As(walkErr, &inListNull) {
				return nil, api.NewError(api.ErrCodeWrongObjectType,
					"NULL values are not allowed in the IN list")
			}
			var srcNotFound *semantic.SourceNotFoundError
			if errors.As(walkErr, &srcNotFound) {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"no FROM source aliased as %s", srcNotFound.Alias.Name())
			}
			var inColRef *expr.InColumnRefError
			if errors.As(walkErr, &inColRef) {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					inColRef.Error())
			}
			var binErr *expr.InvalidBinaryLiteralError
			if errors.As(walkErr, &binErr) {
				return nil, api.NewError(api.ErrCodeInvalidBinaryRepresentation, binErr.Error())
			}
			// When a nested correlated EXISTS fails because the outer
			// scope is insufficient (e.g. inner EXISTS references a
			// grandparent table), propagate as ErrCodeUndefinedColumn
			// so the caller's BuildExists can fall back to
			// buildCorrelatedExists with its richer outer scope.
			var corrExistsErr *CorrelatedExistsError
			if errors.As(walkErr, &corrExistsErr) {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"nested correlated EXISTS: %v", walkErr)
			}
		} else {
			preWalkPred = walked
		}
	}

	// When the pre-walk produced a subquery-bearing predicate (EXISTS
	// or scalar), use it directly. The buildWherePredicate functions
	// build their own resolvers without a SubqueryPlanner — they'd
	// decline these shapes and fall back to text, losing the plans.
	hasSubqueries := existsPlanner != nil && (len(existsPlanner.subqueries) > 0 || len(existsPlanner.scalarSubqueries) > 0)
	if hasSubqueries && preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)

		// Semi-join optimization: when the filter sits on a cross-join
		// and a correlated EXISTS references the same table as the
		// join's right side with the same equi-join column pair, the
		// cross-join is redundant — the EXISTS subsumes it. Drop the
		// cross-join, strip the join predicate from the WHERE, and keep
		// only the EXISTS. This matches Java's Cascades behavior.
		if len(sq.joins) > 0 && len(existsPlanner.subqueries) == 1 {
			esq := existsPlanner.subqueries[0]
			if esq.JoinPredicate != nil {
				if eliminated := eliminateRedundantCrossJoin(op, sq, pred, esq); eliminated {
					return op, nil
				}
			}
		}

		_ = upgradeFirstFilter(op, pred)
		if len(existsPlanner.subqueries) > 0 {
			upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries)
		}
		if len(existsPlanner.scalarSubqueries) > 0 {
			upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries)
		}
		return op, nil
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
				"column reference with qualifier %q cannot be resolved", srcNotFound.Alias.Name())
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

// validateQualifiedStarSourcesFromClassification validates qualified
// star sources using the selectClassification (projection qualifiers)
// and fromSource (table names, aliases, join info). Used by the
// Cascades path which has these as separate objects.
func validateQualifiedStarSourcesFromClassification(cls *selectClassification, fs *fromSource, md *recordlayer.RecordMetaData) error {
	if cls == nil || fs == nil || md == nil {
		return nil
	}
	validSources := make(map[string]bool)
	if fs.tableName != "" {
		validSources[strings.ToUpper(fs.tableName)] = true
		if fs.tableAlias != "" {
			validSources[strings.ToUpper(fs.tableAlias)] = true
		}
	}
	for _, j := range fs.joins {
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
	if err := check(cls.projQualifier); err != nil {
		return err
	}
	// Detect duplicate qualifier-star references. `SELECT a.*, a.* FROM a`
	// would expand to duplicate columns (id, name, id, name). Java errors
	// 42702 at the outer SELECT referencing the ambiguous column; Go
	// surfaces 22023 here because expanding the same source twice produces
	// a column list the downstream materialiser/executor can't disambiguate.
	starSeen := make(map[string]bool, len(cls.projStarQualifiers))
	for _, q := range cls.projStarQualifiers {
		if err := check(q); err != nil {
			return err
		}
		if q != "" {
			up := strings.ToUpper(q)
			if starSeen[up] {
				return api.NewErrorf(api.ErrCodeInvalidParameter,
					"qualifier %q expanded more than once in SELECT list — duplicate columns", q)
			}
			starSeen[up] = true
		}
	}
	return nil
}

func cteAliasMapsCollide(maps []map[string]string) bool {
	if len(maps) <= 1 {
		return false
	}
	seen := make(map[string]struct{})
	for _, m := range maps {
		for _, target := range m {
			upper := strings.ToUpper(target)
			if _, exists := seen[upper]; exists {
				return true
			}
			seen[upper] = struct{}{}
		}
	}
	return false
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

// upgradeFirstFilterExistsSubqueries walks the single-child chain
// from op and, at the first LogicalFilter, attaches the EXISTS
// subquery plans. Returns true when a Filter was found.
// eliminateRedundantCrossJoin detects and eliminates a cross-join that is
// subsumed by a correlated EXISTS on the same table. When the cross-join's
// right table matches the EXISTS scan table and the filter's join predicate
// is equivalent to the EXISTS correlation, the cross-join is redundant —
// replacing it with a simple EXISTS semi-join on the left table avoids
// duplicate rows and matches Java's Cascades behavior.
//
// Returns true when the optimization fired (op modified in-place).
func eliminateRedundantCrossJoin(
	op logical.LogicalOperator,
	sq *selectQuery,
	pred predicates.QueryPredicate,
	esq logical.ExistsSubquery,
) bool {
	if len(sq.joins) != 1 {
		return false
	}
	joinTableName := sq.joins[0].tableName

	// Check if the EXISTS scan references the same table.
	existsTable := ""
	for cur := esq.Plan; cur != nil; {
		if s, ok := cur.(*logical.LogicalScan); ok {
			existsTable = s.Table
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if existsTable == "" || !strings.EqualFold(joinTableName, existsTable) {
		return false
	}

	// Check if the filter predicate contains a comparison between left
	// and right table columns that matches the EXISTS join predicate.
	filterComps := extractComparisonFieldPairs(pred)
	existsComps := extractComparisonFieldPairs(esq.JoinPredicate)
	if len(filterComps) == 0 || len(existsComps) == 0 {
		return false
	}
	subsumes := false
	for _, fc := range filterComps {
		for _, ec := range existsComps {
			fL := bareCol(fc[0])
			fR := bareCol(fc[1])
			eL := bareCol(ec[0])
			eR := bareCol(ec[1])
			if (fL == eL && fR == eR) || (fL == eR && fR == eL) {
				subsumes = true
			}
		}
	}
	if !subsumes {
		return false
	}

	// Strip the join predicate from the filter predicate — keep only
	// the EXISTS predicate.
	existsPred := stripNonExistsPredicates(pred)
	if existsPred == nil {
		return false
	}

	// Replace the LogicalJoin with just the left child (the main table
	// scan). Walk the operator chain to find the LogicalFilter and the
	// LogicalJoin beneath it.
	for cur := op; cur != nil; {
		f, ok := cur.(*logical.LogicalFilter)
		if !ok {
			ch := cur.Children()
			if len(ch) != 1 {
				return false
			}
			cur = ch[0]
			continue
		}
		join, joinOK := f.Input.(*logical.LogicalJoin)
		if !joinOK {
			return false
		}
		// Replace the join with just its left child.
		f.Input = join.Left
		f.Predicate = existsPred
		f.ExistsSubqueries = []logical.ExistsSubquery{esq}
		return true
	}
	return false
}

// extractComparisonFieldPairs extracts [left, right] field name pairs
// from comparison predicates in a predicate tree.
func extractComparisonFieldPairs(p predicates.QueryPredicate) [][2]string {
	if p == nil {
		return nil
	}
	var pairs [][2]string
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		cp, ok := qp.(*predicates.ComparisonPredicate)
		if !ok {
			return true
		}
		lFV, lOK := cp.Operand.(*values.FieldValue)
		if !lOK {
			return true
		}
		if cp.Comparison.Operand == nil {
			return true
		}
		rFV, rOK := cp.Comparison.Operand.(*values.FieldValue)
		if !rOK {
			return true
		}
		pairs = append(pairs, [2]string{
			strings.ToUpper(lFV.Field),
			strings.ToUpper(rFV.Field),
		})
		return true
	})
	return pairs
}

// bareCol extracts the unqualified column name from a potentially
// qualified field reference (e.g. "E.ID" → "ID").
func bareCol(field string) string {
	if dot := strings.LastIndexByte(field, '.'); dot >= 0 {
		return field[dot+1:]
	}
	return field
}

// splitNonExistsPredicatesFromWalked returns only the non-EXISTS parts
// of a walked predicate tree. EXISTS and NOT(EXISTS) nodes are dropped.
// Returns nil if there are no non-EXISTS predicates.
func splitNonExistsPredicatesFromWalked(p predicates.QueryPredicate) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	if _, ok := p.(*predicates.ExistsPredicate); ok {
		return nil
	}
	if not, ok := p.(*predicates.NotPredicate); ok {
		ch := not.Children()
		if len(ch) == 1 {
			if _, ok := ch[0].(*predicates.ExistsPredicate); ok {
				return nil
			}
		}
	}
	if and, ok := p.(*predicates.AndPredicate); ok {
		var nonExists []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			if ne := splitNonExistsPredicatesFromWalked(sub); ne != nil {
				nonExists = append(nonExists, ne)
			}
		}
		if len(nonExists) == 1 {
			return nonExists[0]
		}
		if len(nonExists) > 1 {
			return predicates.NewAnd(nonExists...)
		}
		return nil
	}
	return p
}

// stripNonExistsPredicates removes non-EXISTS predicates from an AND
// tree, returning only the EXISTS (or NOT EXISTS) predicate. Returns
// nil if no EXISTS predicate is found.
func stripNonExistsPredicates(p predicates.QueryPredicate) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	if _, ok := p.(*predicates.ExistsPredicate); ok {
		return p
	}
	if not, ok := p.(*predicates.NotPredicate); ok {
		ch := not.Children()
		if len(ch) == 1 {
			if _, ok := ch[0].(*predicates.ExistsPredicate); ok {
				return p
			}
		}
	}
	if and, ok := p.(*predicates.AndPredicate); ok {
		var existsPreds []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			if ep := stripNonExistsPredicates(sub); ep != nil {
				existsPreds = append(existsPreds, ep)
			}
		}
		if len(existsPreds) == 1 {
			return existsPreds[0]
		}
		if len(existsPreds) > 1 {
			return predicates.NewAnd(existsPreds...)
		}
	}
	return nil
}

func upgradeFirstFilterExistsSubqueries(op logical.LogicalOperator, subqueries []logical.ExistsSubquery) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.ExistsSubqueries = subqueries
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

// upgradeFirstFilterScalarSubqueries walks the single-child chain
// from op and, at the first LogicalFilter, attaches the scalar
// subquery plans. Returns true when a Filter was found.
func upgradeFirstFilterScalarSubqueries(op logical.LogicalOperator, subqueries []logical.ScalarSubquery) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.ScalarSubqueries = subqueries
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
func upgradeProjectionValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) error {
	proj := findProjection(op)
	if proj == nil {
		return nil
	}
	// Post-aggregation projections: walk through the Resolver using base
	// table scope, then rewrite AggregateValues to FieldValue references.
	if len(sq.postAggExprs) > 0 {
		resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
		if resolver == nil {
			resolver = buildSelectScope(sq, md, cteScopes)
		}
		if resolver == nil {
			return nil
		}
		if subqPlanner != nil {
			resolver.SetSubqueryPlanner(subqPlanner)
		}
		vals := make([]values.Value, len(proj.Projections))
		agg := findAggregate(op)
		var groupKeyExplains map[string]values.Value
		if agg != nil && len(agg.GroupKeyValues) > 0 {
			groupKeyExplains = make(map[string]values.Value, len(agg.GroupKeyValues))
			for i, gkv := range agg.GroupKeyValues {
				if gkv == nil {
					continue
				}
				explain := strings.ToUpper(values.ExplainValue(gkv))
				ref := &values.FieldValue{Field: explain, Typ: values.UnknownType}
				groupKeyExplains[explain] = ref
				if i < len(agg.GroupKeys) {
					groupKeyExplains[strings.ToUpper(agg.GroupKeys[i])] = ref
				}
			}
		}
		for i, e := range sq.postAggExprs {
			if i >= len(vals) || e == nil {
				continue
			}
			if groupKeyExplains != nil {
				projText := strings.ToUpper(strings.TrimSpace(e.GetText()))
				if ref, ok := groupKeyExplains[projText]; ok {
					vals[i] = ref
					continue
				}
			}
			v, err := resolver.WalkExpression(e)
			if err != nil {
				// Propagate real semantic errors (e.g. 42703 undefined
				// column from a correlated scalar subquery). Only
				// UnsupportedExpressionShapeError should be swallowed.
				var apiErr *api.Error
				if errors.As(err, &apiErr) {
					return err
				}
				continue
			}
			v = rewriteAggregateValuesInTree(v)
			vals[i] = v
		}
		proj.ProjectedValues = vals
		return nil
	}

	// Regular projections.
	exprs := sq.projExprs
	if len(exprs) == 0 {
		return nil
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, cteScopes)
	}
	if resolver == nil {
		return nil
	}
	if subqPlanner != nil {
		resolver.SetSubqueryPlanner(subqPlanner)
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
			// Propagate real semantic errors (e.g. 42703 undefined
			// column from a correlated scalar subquery). Only
			// UnsupportedExpressionShapeError should be swallowed.
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				return err
			}
			continue
		}
		if !isCascadesSafeValue(v) {
			continue
		}
		v = rewriteAggregateValuesInTree(v)
		vals[i] = v
	}
	proj.ProjectedValues = vals
	return nil
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
		resolver = buildSelectScope(sq, md, cteScopes)
	}
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

	if len(sq.groupByExprs) > 0 {
		keyValues := make([]values.Value, len(agg.GroupKeys))
		for i, gbe := range sq.groupByExprs {
			if gbe == nil || i >= len(keyValues) {
				continue
			}
			v, err := resolver.WalkExpressionForProjection(gbe)
			if err != nil {
				continue
			}
			keyValues[i] = v
		}
		agg.GroupKeyValues = keyValues
	}
}

func upgradeHavingPredicate(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) {
	agg := findAggregate(op)
	if agg == nil || sq.havingExpr == nil {
		return
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, cteScopes)
	}
	if resolver == nil {
		return
	}
	// Install the SubqueryPlanner so EXISTS subqueries in HAVING can be planned.
	if subqPlanner != nil {
		// Reset subqueries so the HAVING walk starts fresh.
		subqPlanner.subqueries = nil
		subqPlanner.scalarSubqueries = nil
		resolver.SetSubqueryPlanner(subqPlanner)
	}
	pred, err := resolver.WalkPredicate(sq.havingExpr)
	if err != nil {
		return
	}
	agg.HavingPredicate = rewriteAggregateRefsInPredicate(pred)
	if subqPlanner != nil && len(subqPlanner.subqueries) > 0 {
		agg.HavingExistsSubqueries = subqPlanner.subqueries
		subqPlanner.subqueries = nil
	}
	if subqPlanner != nil && len(subqPlanner.scalarSubqueries) > 0 {
		agg.HavingScalarSubqueries = subqPlanner.scalarSubqueries
		subqPlanner.scalarSubqueries = nil
	}
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

func validateGroupByProjection(sq *selectQuery, md *recordlayer.RecordMetaData) error {
	groupBySet := make(map[string]bool, len(sq.groupBy))
	for _, gb := range sq.groupBy {
		groupBySet[strings.ToUpper(gb)] = true
		if dot := strings.LastIndex(gb, "."); dot >= 0 {
			groupBySet[strings.ToUpper(gb[dot+1:])] = true
		}
	}

	var tableFields map[string]bool
	if md != nil && sq.tableName != "" {
		rt := md.GetRecordType(sq.tableName)
		if rt != nil && rt.Descriptor != nil {
			fields := rt.Descriptor.Fields()
			tableFields = make(map[string]bool, fields.Len())
			for i := 0; i < fields.Len(); i++ {
				tableFields[strings.ToUpper(string(fields.Get(i).Name()))] = true
			}
		}
	}

	checkColumn := func(col string) error {
		upper := strings.ToUpper(col)
		bare := upper
		if dot := strings.LastIndex(bare, "."); dot >= 0 {
			bare = bare[dot+1:]
		}
		if tableFields != nil && !tableFields[bare] {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist", col)
		}
		if !groupBySet[bare] && !groupBySet[upper] {
			return api.NewErrorf(api.ErrCodeGroupingError,
				"column %q must appear in the GROUP BY clause or be used in an aggregate function", col)
		}
		return nil
	}

	groupByExprSet := make(map[string]bool)
	for i, gb := range sq.groupBy {
		if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
			groupByExprSet[strings.ToUpper(gb)] = true
		}
	}

	if len(sq.aggCols) > 0 {
		for _, ac := range sq.aggCols {
			if ac.aggFunc != "" || !ac.visible {
				continue
			}
			if ac.outExpr != nil {
				// Expression entry (e.g. `x.col1 + x.col2`). Walk the
				// expression tree for column references outside of
				// aggregate calls and verify each is in GROUP BY.
				// Expressions that are purely constant or only reference
				// aggregate results are fine.
				refs := harvestColumnRefs(ac.outExpr)
				for _, ref := range refs {
					if err := checkColumn(ref); err != nil {
						return err
					}
				}
				continue
			}
			col := ac.groupCol
			if col == "" {
				col = ac.outName
			}
			if groupByExprSet[strings.ToUpper(col)] {
				continue
			}
			if err := checkColumn(col); err != nil {
				return err
			}
		}
		return nil
	}

	for i, col := range sq.projCols {
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			continue
		}
		if err := checkColumn(col); err != nil {
			return err
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
	addDerived := func(alias string, derivedQuery antlrgen.IQueryContext) bool {
		if src, ok := buildDerivedTableSource(md, alias, derivedQuery); ok {
			return scope.AddSource(src) == nil
		}
		return false
	}
	if sq.tableName != "" {
		if sq.derivedQuery != nil {
			if !addDerived(sq.tableName, sq.derivedQuery) {
				return nil
			}
		} else if !addSource(sq.tableName, sq.tableAlias) {
			return nil
		}
	}
	for _, j := range sq.joins {
		if j.derivedQuery != nil {
			if !addDerived(j.alias, j.derivedQuery) {
				return nil
			}
		} else if !addSource(j.tableName, j.alias) {
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

// buildLogicalPlanForQueryWithCTECatalog is like
// buildLogicalPlanForQueryWithCatalog but accepts external CTE scopes
// from an enclosing WITH clause. Used by scalar subquery planning where
// the inner query (e.g. `SELECT MIN(v) FROM high`) references a CTE
// defined in the outer query's WITH clause. The outer scopes are merged
// with any CTEs the inner query itself defines (inner shadows outer on
// name collision, matching SQL scoping rules).
func buildLogicalPlanForQueryWithCTECatalog(
	q antlrgen.IQueryContext,
	md *recordlayer.RecordMetaData,
	outerCTEScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if len(outerCTEScopes) == 0 {
		return buildLogicalPlanForQueryWithCatalog(q, md)
	}
	if q == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()

	// Start with outer CTE scopes, then overlay any inner CTE defs
	// (inner shadows outer on name collision).
	cteScopes := make(map[string]semantic.ScopeSource, len(outerCTEScopes))
	for k, v := range outerCTEScopes {
		cteScopes[k] = v
	}
	if ctesCtx != nil {
		// Track inner CTE names to detect sibling duplicates.
		innerCTEs := make(map[string]bool)
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if innerCTEs[upper] {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			innerCTEs[upper] = true
			// Inner CTE shadowing an outer CTE is fine (SQL scoping).
			if src, ok := buildCTEColumnSource(md, name, nq.Query(), cteScopes); ok {
				if colAliases := nq.GetColumnAliases(); colAliases != nil {
					if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
						aliases := aliasList.AllFullId()
						if nAliases := len(aliases); nAliases > 0 && src.Table != nil {
							nCols := len(src.Table.Columns())
							if nAliases != nCols {
								return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
									"cte query has %d column(s), however %d aliases defined",
									nCols, nAliases)
							}
						}
					}
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
	traversalOrder := logical.TraversalLevelOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		if toc.PRE_ORDER() != nil {
			traversalOrder = logical.TraversalPreOrder
		} else if toc.POST_ORDER() != nil {
			traversalOrder = logical.TraversalPostOrder
		}
	}
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			if recursive {
				qeb := inner.QueryExpressionBody()
				if _, isSet := qeb.(*antlrgen.SetQueryContext); !isSet {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"recursive CTE requires UNION ALL body")
				}
			}
			body, err = buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, cteScopes)
			if err != nil {
				return nil, err
			}
		}
		if body == nil {
			return nil, nil
		}
		cte := logical.NewCTE(name, body, main, recursive)
		cte.TraversalOrder = traversalOrder
		if colAliases := nq.GetColumnAliases(); colAliases != nil {
			if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
				aliases := aliasList.AllFullId()
				names := make([]string, len(aliases))
				for j, fid := range aliases {
					names[j] = strings.ToUpper(functions.StripIdentifierQuotes(functions.FullIdToName(fid)))
				}
				cte.ColumnAliases = names
			}
		}
		main = cte
	}
	return main, nil
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
				// Java's SemanticAnalyzer.validateCteColumnAliases checks
				// that the alias count matches the CTE body column count.
				if colAliases := nq.GetColumnAliases(); colAliases != nil {
					if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
						aliases := aliasList.AllFullId()
						if nAliases := len(aliases); nAliases > 0 && src.Table != nil {
							nCols := len(src.Table.Columns())
							if nAliases != nCols {
								return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
									"cte query has %d column(s), however %d aliases defined",
									nCols, nAliases)
							}
						}
					}
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
	traversalOrder := logical.TraversalLevelOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		if toc.PRE_ORDER() != nil {
			traversalOrder = logical.TraversalPreOrder
		} else if toc.POST_ORDER() != nil {
			traversalOrder = logical.TraversalPostOrder
		}
	}
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			if recursive {
				qeb := inner.QueryExpressionBody()
				if _, isSet := qeb.(*antlrgen.SetQueryContext); !isSet {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"recursive CTE requires UNION ALL body")
				}
			}
			body, err = buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, cteScopes)
			if err != nil {
				return nil, err
			}
		}
		if body == nil {
			return nil, nil
		}
		cte := logical.NewCTE(name, body, main, recursive)
		cte.TraversalOrder = traversalOrder
		if colAliases := nq.GetColumnAliases(); colAliases != nil {
			if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
				aliases := aliasList.AllFullId()
				names := make([]string, len(aliases))
				for j, fid := range aliases {
					names[j] = strings.ToUpper(functions.StripIdentifierQuotes(functions.FullIdToName(fid)))
				}
				cte.ColumnAliases = names
			}
		}
		main = cte
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
		return buildLogicalPlanForUnionWithCTECatalog(b, md, cteScopes, false)
	}
	return nil, nil
}

func buildLogicalPlanForUnionWithCTECatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
	cteScopes map[string]semantic.ScopeSource,
	allowDistinct bool,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	distinct := false
	if setQ.ALL() == nil {
		if !allowDistinct {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "only UNION ALL is supported")
		}
		distinct = true
	}
	left, err := buildLogicalPlanForQueryBodyWithCTECatalog(setQ.GetLeft(), md, cteScopes)
	if err != nil {
		return nil, err
	}

	// The grammar attaches a trailing ORDER BY to the rightmost
	// simpleTable. For a UNION, that ORDER BY applies to the combined
	// result (SQL standard), NOT to the right branch alone. Strip it
	// from the right branch before building (so column validation
	// doesn't reject LEFT-branch column names against the right table)
	// and lift it to wrap the whole UNION.
	var liftedSortKeys []logical.SortKey
	var right logical.LogicalOperator
	right, liftedSortKeys, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, cteScopes)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	// Legacy fallback: if the right branch's sort wasn't stripped at
	// the selectQuery level (e.g. nested UNION), peel it off the
	// logical plan tree.
	if len(liftedSortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			liftedSortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				liftedSortKeys = s.Keys
				p.Input = s.Input
			}
		}
	}

	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	if err := validateUnionColumnTypes(inputs, md); err != nil {
		return nil, err
	}
	if len(liftedSortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: liftedSortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0]); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, distinct)
	if len(liftedSortKeys) > 0 {
		result = logical.NewSort(result, liftedSortKeys)
	}
	return result, nil
}

// buildUnionRightBranchStrippingOrderBy builds the right branch of a
// UNION, stripping any trailing ORDER BY from the simpleTable before
// building the logical plan. Returns the built plan and the stripped
// sort keys (empty if there was no ORDER BY). For non-simpleTable
// right branches (e.g. nested UNION), falls through to the normal
// builder and returns empty sort keys.
func buildUnionRightBranchStrippingOrderBy(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
	cteScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, []logical.SortKey, error) {
	qtd, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, cteScopes)
		return op, nil, err
	}
	simpleTable, ok := qtd.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, cteScopes)
		return op, nil, err
	}
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return nil, nil, err
	}

	// Save and strip ORDER BY.
	var sortKeys []logical.SortKey
	if len(sq.orderBy) > 0 {
		for _, ob := range sq.orderBy {
			e := ob.colName
			if e == "" && ob.rawExpr != nil {
				e = ob.rawExpr.GetText()
			}
			dir := logical.SortAsc
			if !ob.ascending {
				dir = logical.SortDesc
			}
			nullsFirst := ob.ascending
			if ob.nullsFirst != nil {
				nullsFirst = *ob.nullsFirst
			}
			sortKeys = append(sortKeys, logical.SortKey{Expr: e, Dir: dir, NullsFirst: nullsFirst})
		}
		sq.orderBy = nil
	}

	if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
		return nil, nil, api.NewError(api.ErrCodeUndefinedFunction, "Unsupported operator "+fn)
	}
	if err := validateQualifiedStarSources(sq, md); err != nil {
		return nil, nil, err
	}
	op, err := buildLogicalPlanForSelectWithCTECatalog(sq, md, cteScopes)
	if err != nil {
		return nil, nil, err
	}
	return op, sortKeys, nil
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
	agg := findAggregate(op)
	var groupKeyExplainMap map[string]string
	if agg != nil && len(agg.GroupKeyValues) > 0 {
		groupKeyExplainMap = make(map[string]string)
		for i, gkv := range agg.GroupKeyValues {
			if gkv == nil || i >= len(agg.GroupKeys) {
				continue
			}
			groupKeyExplainMap[strings.ToUpper(agg.GroupKeys[i])] = strings.ToUpper(values.ExplainValue(gkv))
		}
	}
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
		if groupKeyExplainMap != nil {
			if explain, ok := groupKeyExplainMap[strings.ToUpper(sort.Keys[i].Expr)]; ok {
				sort.Keys[i].Value = &values.FieldValue{Field: explain, Typ: values.UnknownType}
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
		if ob.colName != "" {
			continue
		}
		if ob.rawExpr == nil {
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
			name = canonicalTextOf(ob.rawExpr)
		}
		if strings.EqualFold(name, keyExpr) {
			return ob
		}
	}
	return nil
}

// buildOuterPlanOnDerived builds the Aggregate/Sort/Limit/Project/Distinct
// shell from a selectQuery on top of an already-built inner plan (derived
// table). Delegates to buildSelectShell with the derived table qualifier
// as the strip prefix.
func buildOuterPlanOnDerived(sq *selectQuery, innerOp logical.LogicalOperator) logical.LogicalOperator {
	op := innerOp
	if sq.whereExpr != nil {
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
	}
	return buildSelectShell(op, sq, strings.ToUpper(sq.tableName)+".")
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

// expandProjQualifier handles `SELECT <qualifier>.*` when it is the
// only SELECT element (projQualifier set, projCols nil). Expands the
// qualifier into explicit projCols with qualified column names
// (`qualifier.COL`) so buildLogicalPlanForSelect emits a LogicalProject
// that restricts the output to that source's columns. Without this,
// JOIN queries with a lone qualified star would project all columns
// from all sources (the nil-projCols path in buildLogicalPlanForSelect
// skips the projection node entirely).
//
// For single-table queries `a.*` is equivalent to `*`, so the expansion
// is technically unnecessary but harmless — the resulting projection
// lists the same columns the scan produces.
func expandProjQualifier(sq *selectQuery, md *recordlayer.RecordMetaData) {
	if sq == nil || md == nil || sq.projQualifier == "" {
		return
	}
	qual := sq.projQualifier

	// Resolve which table the qualifier refers to.
	tableName := ""
	if strings.EqualFold(sq.tableAlias, qual) || (sq.tableAlias == "" && strings.EqualFold(sq.tableName, qual)) {
		tableName = sq.tableName
	}
	if tableName == "" {
		for _, j := range sq.joins {
			a := j.alias
			if a == "" {
				a = j.tableName
			}
			if strings.EqualFold(a, qual) {
				tableName = j.tableName
				break
			}
		}
	}
	if tableName == "" {
		return // unknown qualifier — validated elsewhere
	}

	rt := md.GetRecordType(tableName)
	if rt == nil || rt.Descriptor == nil {
		return
	}
	fields := rt.Descriptor.Fields()
	cols := make([]string, fields.Len())
	aliases := make([]string, fields.Len())
	exprs := make([]antlrgen.IExpressionContext, fields.Len())
	quals := make([]string, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		cols[i] = qual + "." + strings.ToUpper(string(fields.Get(i).Name()))
	}
	sq.projCols = cols
	sq.projAliases = aliases
	sq.projExprs = exprs
	sq.projStarQualifiers = quals
	// Clear projQualifier so downstream code doesn't treat this as the
	// legacy nil-projCols path.
	sq.projQualifier = ""
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

func validateUnionOrderByColumns(sort *logical.LogicalSort, leftBranch logical.LogicalOperator) error {
	leftProj := findProjection(leftBranch)
	if leftProj == nil {
		return nil
	}
	leftNames := make(map[string]bool, len(leftProj.Projections))
	for i, col := range leftProj.Projections {
		leftNames[strings.ToUpper(col)] = true
		if i < len(leftProj.Aliases) && leftProj.Aliases[i] != "" {
			leftNames[strings.ToUpper(leftProj.Aliases[i])] = true
		}
	}
	for _, k := range sort.Keys {
		if k.Expr != "" && !leftNames[strings.ToUpper(k.Expr)] {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q not found in UNION result columns", k.Expr)
		}
	}
	return nil
}

func validateUnionColumnTypes(inputs []logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	if md == nil || len(inputs) < 2 {
		return nil
	}
	firstTypes := resolveProjectionTypes(inputs[0], md)
	if firstTypes == nil {
		return nil
	}
	for i := 1; i < len(inputs); i++ {
		otherTypes := resolveProjectionTypes(inputs[i], md)
		if otherTypes == nil {
			continue
		}
		n := len(firstTypes)
		if len(otherTypes) < n {
			n = len(otherTypes)
		}
		for j := 0; j < n; j++ {
			if firstTypes[j] == 0 || otherTypes[j] == 0 {
				continue
			}
			lCat := unionTypeCategory(firstTypes[j])
			rCat := unionTypeCategory(otherTypes[j])
			if lCat == 0 || rCat == 0 {
				continue
			}
			if lCat != rCat {
				return api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
					"Incompatible column types in UNION legs")
			}
		}
	}
	return nil
}

func unionTypeCategory(k protoreflect.Kind) int {
	switch k {
	case protoreflect.BoolKind:
		return 1
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return 2
	case protoreflect.StringKind:
		return 3
	case protoreflect.BytesKind:
		return 4
	case protoreflect.EnumKind:
		return 5
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return 6
	}
	return 0
}

func findScanTable(op logical.LogicalOperator) string {
	for cur := op; cur != nil; {
		if scan, ok := cur.(*logical.LogicalScan); ok {
			return scan.Table
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return ""
		}
		cur = ch[0]
	}
	return ""
}

func resolveProjectionTypes(op logical.LogicalOperator, md *recordlayer.RecordMetaData) []protoreflect.Kind {
	proj := findProjection(op)
	if proj == nil {
		return nil
	}
	tableName := findScanTable(op)
	if tableName == "" {
		return nil
	}
	rt := md.GetRecordType(tableName)
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	desc := rt.Descriptor
	kinds := make([]protoreflect.Kind, len(proj.Projections))
	for i, col := range proj.Projections {
		if i < len(proj.IsComputed) && proj.IsComputed[i] {
			continue
		}
		bare := col
		if dot := strings.LastIndex(col, "."); dot >= 0 {
			bare = col[dot+1:]
		}
		fd := desc.Fields().ByName(protoreflect.Name(strings.ToLower(bare)))
		if fd == nil {
			fd = desc.Fields().ByName(protoreflect.Name(bare))
		}
		if fd != nil {
			kinds[i] = fd.Kind()
		}
	}
	return kinds
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
	if setQ.ALL() == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "only UNION ALL is supported")
	}
	left, err := buildLogicalPlanForQueryBodyWithCatalog(setQ.GetLeft(), md)
	if err != nil {
		return nil, err
	}

	// Same ORDER BY stripping as the CTE-catalog variant: strip the
	// trailing ORDER BY from the right branch before building so
	// column validation doesn't reject LEFT-branch names.
	var liftedSortKeys []logical.SortKey
	var right logical.LogicalOperator
	right, liftedSortKeys, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, nil)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	if len(liftedSortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			liftedSortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				liftedSortKeys = s.Keys
				p.Input = s.Input
			}
		}
	}

	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	if err := validateUnionColumnTypes(inputs, md); err != nil {
		return nil, err
	}
	if len(liftedSortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: liftedSortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0]); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, false)
	if len(liftedSortKeys) > 0 {
		result = logical.NewSort(result, liftedSortKeys)
	}
	return result, nil
}

// existsSubqueryPlanner implements expr.SubqueryPlanner. It builds
// logical plans for EXISTS and scalar subqueries and collects the
// (alias, plan) pairs that the LogicalFilter/LogicalProject need to
// carry to the Cascades translator.
func buildOuterScopeSources(sq *selectQuery, md *recordlayer.RecordMetaData) map[string]semantic.ScopeSource {
	if sq == nil || md == nil || sq.tableName == "" {
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	sources := make(map[string]semantic.ScopeSource)
	addSrc := func(tableName, alias string) {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return
		}
		a := semantic.NewUnquoted(alias)
		if alias == "" {
			a = semantic.NewUnquoted(tableName)
		}
		sources[strings.ToUpper(a.Name())] = semantic.ScopeSource{
			Table: tbl, Alias: a, CorrelationName: a.Name(),
		}
	}
	addSrc(sq.tableName, sq.tableAlias)
	for _, j := range sq.joins {
		addSrc(j.tableName, j.alias)
	}
	return sources
}

type existsSubqueryPlanner struct {
	md                *recordlayer.RecordMetaData
	outerScopes       map[string]semantic.ScopeSource
	cteScopes         map[string]semantic.ScopeSource
	cteBodies         map[string]logical.LogicalOperator // CTE name → body plan, for wrapping scalar subquery plans
	subqueries        []logical.ExistsSubquery
	scalarSubqueries  []logical.ScalarSubquery
	lastJoinPredicate predicates.QueryPredicate
}

func (p *existsSubqueryPlanner) BuildExists(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: nil query context")
	}
	innerOp, err := buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.cteScopes)
	isUndefinedCol := false
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeUndefinedColumn {
			isUndefinedCol = true
		}
	}
	if err != nil && (!isUndefinedCol || len(p.outerScopes) == 0) {
		return values.CorrelationIdentifier{}, err
	}
	if isUndefinedCol {
		p.lastJoinPredicate = nil
		innerOp, err = p.buildCorrelatedExists(q)
		if err != nil {
			return values.CorrelationIdentifier{}, err
		}
	}
	if innerOp == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: inner query could not be planned")
	}
	alias := values.UniqueCorrelationIdentifier()
	p.subqueries = append(p.subqueries, logical.ExistsSubquery{
		Alias:         alias,
		Plan:          innerOp,
		JoinPredicate: p.lastJoinPredicate,
	})
	p.lastJoinPredicate = nil
	return alias, nil
}

func (p *existsSubqueryPlanner) buildCorrelatedExists(q antlrgen.IQueryContext) (logical.LogicalOperator, error) {
	if q == nil {
		return nil, &CorrelatedExistsError{Message: "correlated EXISTS: nil query"}
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: unsupported query body shape %T", q.QueryExpressionBody())}
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: %v", err), Cause: err}
	}

	innerAlias := sq.tableAlias
	if innerAlias == "" {
		innerAlias = sq.tableName
	}
	op := logical.LogicalOperator(logical.NewScan(sq.tableName, innerAlias))

	// Build join tree from inner FROM clause (handles multi-table EXISTS).
	for _, j := range sq.joins {
		right := logical.LogicalOperator(logical.NewScan(j.tableName, j.alias))
		var kind logical.JoinKind
		switch j.joinType {
		case "LEFT":
			kind = logical.JoinLeft
		case "RIGHT":
			kind = logical.JoinRight
		default:
			kind = logical.JoinInner
		}
		op = logical.NewJoinWithPredicate(op, right, kind, nil)
	}

	if sq.whereExpr == nil || sq.whereExpr.Expression() == nil {
		return op, nil
	}

	cat := rlcatalog.Wrap(p.md)
	analyzer := semantic.NewAnalyzer(cat, false)

	outerScope := semantic.NewScope(nil)
	for _, src := range p.outerScopes {
		_ = outerScope.AddSource(src)
	}

	innerScope := semantic.NewScope(outerScope)
	tbl, tblErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(sq.tableName, "."), false))
	if tblErr != nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: resolve inner table %q: %v", sq.tableName, tblErr), Cause: tblErr}
	}
	aliasID := semantic.NewUnquoted(innerAlias)
	_ = innerScope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: aliasID, CorrelationName: aliasID.Name(),
	})

	// Add join tables to scope so the resolver can resolve their columns.
	for _, j := range sq.joins {
		jAlias := j.alias
		if jAlias == "" {
			jAlias = j.tableName
		}
		jTbl, jErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(j.tableName, "."), false))
		if jErr != nil {
			return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: resolve join table %q: %v", j.tableName, jErr), Cause: jErr}
		}
		jAliasID := semantic.NewUnquoted(jAlias)
		_ = innerScope.AddSource(semantic.ScopeSource{
			Table: jTbl, Alias: jAliasID, CorrelationName: jAliasID.Name(),
		})
	}

	resolver := expr.New(analyzer, innerScope)

	// Install a SubqueryPlanner on the resolver so that nested EXISTS
	// subqueries in the inner WHERE can be planned. The nested planner's
	// outer scopes include both the current planner's outer scopes and
	// the inner table — this enables correlation across multiple levels
	// (e.g. innermost EXISTS referencing outermost emp.id).
	nestedOuterScopes := make(map[string]semantic.ScopeSource, len(p.outerScopes)+1)
	for k, v := range p.outerScopes {
		nestedOuterScopes[k] = v
	}
	nestedOuterScopes[strings.ToUpper(aliasID.Name())] = semantic.ScopeSource{
		Table: tbl, Alias: aliasID, CorrelationName: aliasID.Name(),
	}
	nestedPlanner := &existsSubqueryPlanner{
		md:          p.md,
		outerScopes: nestedOuterScopes,
		cteScopes:   p.cteScopes,
	}
	resolver.SetSubqueryPlanner(nestedPlanner)

	pred, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
	if walkErr != nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: walk predicate: %v", walkErr), Cause: walkErr}
	}

	// If the nested planner collected EXISTS subqueries, check whether
	// the middle level has its own correlation predicate (non-EXISTS).
	if len(nestedPlanner.subqueries) > 0 {
		innerCorr := strings.ToUpper(aliasID.Name())
		nonExistsPred := splitNonExistsPredicatesFromWalked(pred)

		if nonExistsPred != nil {
			// Case 1: middle has BOTH correlation + nested EXISTS.
			// Build a proper LogicalFilter preserving the middle level.
			existsPred := stripNonExistsPredicates(pred)
			qualifyBareFields(nonExistsPred, innerCorr)
			p.lastJoinPredicate = predicates.SimplifyPredicateValues(nonExistsPred)
			filter := &logical.LogicalFilter{
				Input:            op,
				Predicate:        existsPred,
				ExistsSubqueries: nestedPlanner.subqueries,
			}
			return filter, nil
		}

		// Case 2: middle has ONLY EXISTS (no own correlation).
		// The inner correlation spans multiple levels (innermost →
		// outermost). Hoist the inner plan to this level so the
		// correlation binds against the outer row directly.
		innerESQ := nestedPlanner.subqueries[0]
		p.lastJoinPredicate = innerESQ.JoinPredicate
		return innerESQ.Plan, nil
	}

	// The predicate will be evaluated in a merged NLJ context where both
	// inner and outer columns coexist keyed by UPPER-CASE qualified names
	// (e.g. SUB.V, A.V). The resolver produced bare field names for inner
	// columns (e.g. "V") because the inner scope has only one source.
	// Qualify them with the inner correlation name so that merged-row
	// lookup finds the inner column, not the outer's value leaking
	// through when the inner row has a NULL (absent-from-map) field.
	innerCorr := strings.ToUpper(aliasID.Name())
	qualifyBareFields(pred, innerCorr)

	p.lastJoinPredicate = predicates.SimplifyPredicateValues(pred)
	return op, nil
}

// qualifyBareFields walks a predicate tree and prepends qualifier+"."
// to every FieldValue whose Field has no dot (i.e. was unqualified by
// the resolver because the inner scope had only one source). This is
// necessary for correlated EXISTS predicates that will be evaluated in
// a merged NLJ row where both outer and inner columns coexist.
func qualifyBareFields(p predicates.QueryPredicate, qualifier string) {
	if p == nil || qualifier == "" {
		return
	}
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			qualifyBareFieldValue(pred.Operand, qualifier)
			if pred.Comparison.Operand != nil {
				qualifyBareFieldValue(pred.Comparison.Operand, qualifier)
			}
		case *predicates.ValuePredicate:
			qualifyBareFieldValue(pred.Value, qualifier)
		}
		return true
	})
}

func qualifyBareFieldValue(v values.Value, qualifier string) {
	values.WalkValue(v, func(node values.Value) bool {
		if fv, ok := node.(*values.FieldValue); ok {
			if fv.Child != nil {
				return false
			}
			if !strings.Contains(fv.Field, ".") {
				fv.Field = qualifier + "." + fv.Field
			}
		}
		return true
	})
}

// NOTE: qualifyBareFieldValue mutates FieldValue.Field in place. This is
// safe because buildCorrelatedExists constructs a fresh predicate tree via
// resolver.WalkPredicate each call. Do NOT call on shared/memoized trees.

func (p *existsSubqueryPlanner) BuildScalar(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("scalar subquery: nil query context")
	}
	innerOp, err := buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.cteScopes)
	if err != nil {
		return values.CorrelationIdentifier{}, err
	}
	if innerOp == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("scalar subquery: inner query could not be planned")
	}
	// If the inner plan references outer CTEs (from a WITH clause on the
	// enclosing query), wrap it with LogicalCTE nodes so the Cascades
	// translator's cteScope can resolve the scan. Without this, a scan
	// on a CTE name (e.g. SELECT MIN(v) FROM high) would be translated
	// as a table scan on a nonexistent table.
	innerOp = p.wrapWithOuterCTEs(innerOp)
	alias := values.UniqueCorrelationIdentifier()
	p.scalarSubqueries = append(p.scalarSubqueries, logical.ScalarSubquery{
		Alias: alias,
		Plan:  innerOp,
	})
	return alias, nil
}

// wrapWithOuterCTEs wraps op with LogicalCTE nodes for every outer CTE
// whose name appears as a LogicalScan in the plan tree. This makes the
// plan self-contained so the Cascades translator can resolve CTE scan
// references without external scope.
func (p *existsSubqueryPlanner) wrapWithOuterCTEs(op logical.LogicalOperator) logical.LogicalOperator {
	if len(p.cteBodies) == 0 {
		return op
	}
	refs := collectScanTableNames(op)
	for name, body := range p.cteBodies {
		if refs[name] {
			op = logical.NewCTE(name, body, op, false)
		}
	}
	return op
}

// collectScanTableNames returns the set of UPPER-CASE table names
// referenced by LogicalScan nodes in the plan tree.
func collectScanTableNames(op logical.LogicalOperator) map[string]bool {
	names := make(map[string]bool)
	collectScanTableNamesInner(op, names)
	return names
}

func collectScanTableNamesInner(op logical.LogicalOperator, names map[string]bool) {
	if op == nil {
		return
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		names[strings.ToUpper(scan.Table)] = true
	}
	for _, ch := range op.Children() {
		collectScanTableNamesInner(ch, names)
	}
}
