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
	schemaName string,
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
	return buildWherePredicateForJoins(md, schemaName, sq, whereExpr)
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
		bareName := parseColRef(col).bare()
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
func upgradeJoinOnPredicates(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
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

	// Build the full scope for predicate resolution. A lateral array unnest
	// leg (`FROM T1 INNER JOIN U ON …, T1.ARR AS V`) is NOT a real table —
	// resolveTable("T1.ARR") fails. Without registering its virtual element/
	// ordinal source, the scope build would abort, the ON resolver would never
	// run, and the EXPLICIT JOIN's ON predicate (`U.ID = T1.ID`) would be silently
	// DROPPED → the T1/U join degrades to a CROSS join (silent-wrong). Register the
	// unnest leg via the SAME shared helpers every other scope builder uses so the
	// ON predicate still resolves against the real-table legs. RFC-142.
	scope := semantic.NewScope(nil)
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	addTableSource := func(tableName, alias string) bool {
		tbl := resolveTable(tableName)
		if tbl == nil {
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
	// A derived-table JOIN source (`... JOIN (SELECT ...) AS x ON ...`) is NOT a
	// real table — register its virtual column schema (derived from the
	// subquery body) so the ON predicate referencing `x.col` resolves. Without
	// this the scope build aborts, the ON resolver never runs, and the join's
	// ON predicate is silently DROPPED → the outer join degrades to a cartesian
	// product that still null-pads (a wrong result). Mirrors the lateral-unnest
	// leg registration above.
	addDerivedSource := func(j joinClause) bool {
		src, ok := buildDerivedTableSource(md, j.alias, j.derivedQuery)
		if !ok {
			return false
		}
		return scope.AddSource(src) == nil
	}
	var scopeOK bool
	if sq.derivedQuery != nil {
		// Primary FROM source is a derived table (`FROM (SELECT ...) x JOIN ...`).
		if src, ok := buildDerivedTableSource(md, sq.tableAlias, sq.derivedQuery); ok {
			scopeOK = scope.AddSource(src) == nil
		}
	} else {
		scopeOK = addTableSource(sq.tableName, sq.tableAlias)
	}
	for i, j := range sq.joins {
		if !scopeOK {
			break
		}
		if j.derivedQuery != nil {
			scopeOK = addDerivedSource(j)
			continue
		}
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			scopeOK = addUnnestSource(j)
			continue
		}
		scopeOK = addTableSource(j.tableName, j.alias)
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
			// RFC-141 R4: EXISTS in a JOIN ON clause is NOT a
			// directly-handled position. The ON resolver carries no
			// SubqueryPlanner, so WalkPredicate would error on the EXISTS and the
			// whole ON predicate would be silently DROPPED → every joined row
			// passes (a silent wrong result). Detect a contained EXISTS atom
			// structurally and reject cleanly rather than drop the condition.
			if expr.ContainsExistsAtom(sq.joins[sqIdx].onExpr) {
				return api.NewError(api.ErrCodeUnsupportedQuery,
					"EXISTS in a JOIN ON clause is not yet supported")
			}
			pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr)
			if walkErr != nil {
				var srcNotFound *semantic.SourceNotFoundError
				if errors.As(walkErr, &srcNotFound) {
					return api.NewErrorf(api.ErrCodeUndefinedColumn,
						"no FROM source aliased as %s", srcNotFound.Alias.Name())
				}
				// A structured api.Error (e.g. DATATYPE_MISMATCH from a
				// non-boolean bare ON predicate like `ON a.amount`, RFC-146) is a
				// real user error — surface it rather than dropping the ON
				// condition, which the translator would silently degrade to a
				// cross join (it ignores OnText once OnPredicate is nil).
				var apiErr *api.Error
				if errors.As(walkErr, &apiErr) {
					return apiErr
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
		innerSQ.tableName == "" {
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.aggCols) > 0 || innerSQ.countStar {
		src, ok := buildDerivedTableSourceFromAgg(cteName, innerSQ)
		if !ok {
			return semantic.ScopeSource{}, false
		}
		return src, true
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
			bareName := parseColRef(col).bare()
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
	schemaName string,
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
	// A lateral array unnest leg is not a real table / CTE — register its virtual
	// element/ordinal source via the SAME shared helpers buildWherePredicateForJoins
	// (the non-CTE twin) uses, so a CTE-bearing query with an unnest WHERE on the
	// element/ordinal resolves here instead of declining and degrading to text. RFC-142.
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if !addSource(sq.tableName, sq.tableAlias) {
		return nil, false
	}
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if !addUnnestSource(j) {
				return nil, false
			}
			continue
		}
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
	schemaName string,
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
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if !addSource(sq.tableName, sq.tableAlias) {
		return nil, false
	}
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if !addUnnestSource(j) {
				return nil, false
			}
			continue
		}
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

// isLateralUnnestJoin reports whether a joinClause should register a virtual
// unnest scope source in the WHERE/projection scope binding. It delegates to the
// SAME `unnestCandidateShape` predicate the logical lowering
// (lateralUnnestCandidate) uses, with ONE scope-only refinement: a
// schema-qualified TABLE source is NOT registered as an unnest source — it is a
// table cross join (or, with an AT alias, a WRONG_OBJECT_TYPE the demotion pass
// rejects). unnestCandidateShape keeps an AT-on-a-table source as a LogicalUnnest
// so the AT survives to that rejection, but the scope must resolve its columns as
// a table, never an unnest binding. RFC-142.
func isLateralUnnestJoin(j joinClause, visible map[string]struct{}, resolvesToTable tableResolver) bool {
	if j.derivedQuery != nil || j.catalogAwareInnerPlan != nil || j.onExpr != nil {
		return false
	}
	if schemaQualifiedTableUnnest(j, resolvesToTable) {
		return false
	}
	return unnestCandidateShape(j, visible, resolvesToTable)
}

// unnestVirtualScopeSource builds the VIRTUAL scope source for a lateral array
// unnest (`FROM t, t.arr AS x [AT ord]`): a Shadowing source exposing the AS
// alias (element) and AT alias (ordinal) as columns under the AS alias (else the
// AT alias) as correlation name. This is the SINGLE source of truth for the
// unnest binding — every scope/resolver that must see the unnest column (the
// SELECT scope via unnestScopeSourceAdder, AND a correlated subquery's outer
// scope via buildOuterScopeSources) derives it here so they cannot diverge. The
// translator rewrites these references to the inner Explode binding when lowering
// the unnest. ok=false when the source has neither an AS nor an AT alias. RFC-142.
func unnestVirtualScopeSource(j joinClause) (semantic.ScopeSource, bool) {
	// The (AS, AT) pair MUST come from the same normalization the logical
	// lowering uses (unnestAliases) — otherwise the WHERE/projection scope
	// binds the unnest column under the parser's DEFAULTED alias (the joined
	// segment name `T1.ARR1`) while the inner Explode quantifier is bound under
	// the real alias (the AT alias for the AT-only form), so a WHERE-on-ordinal
	// predicate never pushes into the inner Explode filter. RFC-142.
	asAlias, atAlias := unnestAliases(j)
	var cols []semantic.Column
	corr := asAlias
	if asAlias != "" {
		cols = append(cols, semantic.Column{Id: semantic.NewUnquoted(asAlias), Type: "UNKNOWN", Nullable: true})
	}
	if atAlias != "" {
		// The unnest WITH ORDINALITY ordinal is a 1-based, NON-NULL INT
		// (Java's Type.primitiveType(INT, false); the executor yields a 1-based
		// int per element). Register it with the recognized NON-NULL spelling so
		// sqlTypeToCascadesType resolves it to values.NotNullInt — matching the
		// translator's ordinal FieldValue type — and a PROJECT/COMPUTE over the AT
		// alias reports INT, not UNKNOWN. RFC-142.
		cols = append(cols, semantic.Column{Id: semantic.NewUnquoted(atAlias), Type: "INT NOT NULL", Nullable: false})
		if corr == "" {
			corr = atAlias
		}
	}
	if corr == "" {
		return semantic.ScopeSource{}, false
	}
	corrID := semantic.NewUnquoted(corr)
	virtual := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{corr}, false),
		TableColumns: cols,
	}
	return semantic.ScopeSource{
		Table:           virtual,
		Alias:           corrID,
		CorrelationName: corrID.Name(),
		// The unnest binding SHADOWS a same-named outer column (RFC-142).
		Shadowing: true,
	}, true
}

// unnestScopeSourceAdder returns a closure that registers the VIRTUAL scope
// source (unnestVirtualScopeSource) for a lateral array unnest into the SELECT
// scope so a WHERE / projection / ORDER BY reference to the AS/AT column
// resolves (RFC-142).
func unnestScopeSourceAdder(scope *semantic.Scope) func(j joinClause) bool {
	return func(j joinClause) bool {
		src, ok := unnestVirtualScopeSource(j)
		if !ok {
			return false
		}
		return scope.AddSource(src) == nil
	}
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
func buildLogicalPlanForSelectWithCatalog(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) (logical.LogicalOperator, error) {
	return buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, nil)
}

func buildLogicalPlanForSelectWithCTECatalog(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) (logical.LogicalOperator, error) {
	// For derived tables, build the inner plan through the catalog-aware
	// path so WHERE predicates get upgraded. Java's visitSubqueryTableItem
	// recursively visits through the same typed visitor.
	if sq.derivedQuery != nil && md != nil && len(sq.joins) == 0 {
		innerOp, innerErr := buildLogicalPlanForQueryBodyWithCTECatalog(
			sq.derivedQuery.QueryExpressionBody(), md, schemaName, cteScopes,
		)
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			op := buildOuterPlanOnDerived(sq, innerOp)
			if op == nil {
				return nil, nil
			}
			return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, schemaName, cteScopes)
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
			j.derivedQuery.QueryExpressionBody(), md, schemaName, cteScopes,
		)
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			j.catalogAwareInnerPlan = innerOp
		}
	}

	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	// Strip the session-schema qualifier off the parser's schema-qualified FROM
	// sources (`s.PB` → `PB`) BEFORE the logical tree is built. The semantic
	// analyzer's ResolveTable does not strip a schema qualifier, so without this a
	// schema-qualified table inside a SUBQUERY fails to register a scope source, the
	// projection resolver degrades to nil, and translation fails (the same class
	// demoteSchemaQualifiedUnnest / resolveQualifiedTableNames cover for the logical
	// tree). This is the catalog sub-build path (subqueries, derived tables) only —
	// the top-level query builds its scope through the PlanVisitor, untouched.
	//
	// Running BEFORE buildLogicalPlanForSelect (not after) is the ROOT fix for the
	// alias desync: a no-alias schema-qualified source `s.PB` parses with
	// alias == tableName == "S.PB", so the built LogicalScan would carry Alias
	// "S.PB" while normalize strips sq's source alias to "PB". The post-build SCOPE
	// (which reads the normalized sq) then resolves a predicate `PB.ID = PA.ID` to
	// QOV(PB) while the scan binds under "S.PB" → the predicate reads NULL and
	// misfilters rows. Normalizing FIRST makes the scan carry the SAME alias "PB"
	// the resolver uses, so resolver and scan never disagree. RFC-142.
	normalizeSchemaQualifiedSelectSources(sq, schemaName, md)

	op := buildLogicalPlanForSelect(sq)
	if op == nil || md == nil || sq == nil {
		return op, nil
	}
	// Java's generateAccess resolves a FROM identifier table-first at EVERY
	// FROM-source point. buildLogicalPlanForSelect (no metadata in scope) runs the
	// lateral-unnest classifier with a nil resolver, so a schema-qualified table
	// whose qualifier also names a prior alias (`FROM PA AS s, s.PB`) is tentatively
	// emitted as a LogicalUnnest. Demote it back to a Scan HERE — with metadata in
	// scope — BEFORE the post-build scope/projection-value resolution runs, so the
	// subquery's projections resolve against the correct table cross join rather than
	// degrading on the would-be unnest. This is the subquery analog of the
	// top-level demoteSchemaQualifiedUnnest pass (which mutates the logical tree only
	// — too late to recover the projection Values this nested build computes).
	// RFC-142 (P2: schema-qualified table inside a subquery).
	if err := demoteSchemaQualifiedUnnest(op, schemaName, md); err != nil {
		return nil, err
	}
	// Reject AT-ordinality on a TABLE / non-array source (`FROM t, U AT O`, a
	// present-scalar correlated field, …) HERE — at FROM-source analysis time,
	// before _postBuild resolves this (sub)query's WHERE / projection columns. This
	// is the catalog SELECT-build path's copy of the top-level PlanVisitor's early
	// pass (plan_visitor.go's rejectAtOrdinalityOnTableWithCTEs after visitFrom): a
	// subquery / derived-table / INSERT…SELECT body whose OWN predicate resolves
	// first masks the intended WRONG_OBJECT_TYPE (42809) with a scope-level
	// undefined-column (42703) — the AT source registers a virtual unnest binding
	// that SHADOWS the real table, so `U.ID` fails to resolve during _postBuild's
	// WalkPredicate. The post-attach backstop (cascades_generator.go) only walks an
	// already-attached subquery tree, so it never sees a subquery whose construction
	// fails first; running the same early rejection on the built FROM tree here, in
	// EVERY SELECT build path, surfaces 42809 regardless of which path plans the
	// SELECT. Reuses the same rejectAtOrdinalityOnTableWithCTEs helper, threading the
	// in-scope WITH-CTE names from cteScopes (a CTE source is the translator's
	// outerSourceIsCTE territory, never a base-table AT — same as the PlanVisitor
	// seeds from v.cteScopes). RFC-142.
	cteNames := make(map[string]struct{}, len(cteScopes))
	for name := range cteScopes {
		cteNames[strings.ToUpper(name)] = struct{}{}
	}
	if err := rejectAtOrdinalityOnTableWithCTEs(op, md, cteNames); err != nil {
		return nil, err
	}
	return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, schemaName, cteScopes)
}

// normalizeSchemaQualifiedSelectSources strips the session-schema qualifier off
// a selectQuery's primary + join FROM-source table names AND, in lockstep, off
// the matching join leg's un-flattened uid segments, when the source is a real
// schema-qualified table (`s.PB` where `s` is the session schema and `PB`
// resolves). It mirrors resolveQualifiedTableNames (which strips the logical
// scan's `schema.`), applied to the parser struct the scope builders AND the
// (metadata-less) rebuild classifier read. The segments MUST move with the
// tableName: the lateral-unnest classifier resolves segment 0 against the
// visible FROM aliases, and a leftover `[schema, table]` segment slice whose
// schema also happens to name a prior alias would mis-classify the real table
// as a correlated unnest on a later rebuild (`SELECT B.*` etc.).
// Sources that do not resolve to a schema-qualified table are left untouched —
// in particular a dotted reference whose qualifier is a prior FROM alias (a
// lateral unnest candidate) is NOT a `[schema, table]` pair the resolver
// matches, so its segments survive for the unnest classifier. RFC-142.
func normalizeSchemaQualifiedSelectSources(sq *selectQuery, schemaName string, md *recordlayer.RecordMetaData) {
	if sq == nil || md == nil {
		return
	}
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	strip := func(name string) string {
		segs := strings.Split(name, ".")
		if len(segs) == 2 && resolvesToTable(segs) {
			return segs[1]
		}
		return name
	}
	if sq.derivedQuery == nil {
		bare := strip(sq.tableName)
		if bare != sq.tableName {
			if sq.tableAlias == sq.tableName {
				sq.tableAlias = bare
			}
			sq.tableName = bare
		}
	}
	for i := range sq.joins {
		j := &sq.joins[i]
		if j.derivedQuery != nil || j.catalogAwareInnerPlan != nil {
			continue
		}
		bare := strip(j.tableName)
		if bare != j.tableName {
			if j.alias == j.tableName {
				j.alias = bare
			}
			j.tableName = bare
			// Drop the leading schema segment in LOCKSTEP with the tableName
			// strip. The lateral-unnest classifier reads j.segments — NOT
			// j.tableName — so leaving `['main','PB']` here while tableName
			// became the bare `PB` would let a later metadata-less REBUILD
			// (`buildLogicalPlanForSelect`, e.g. forced by `SELECT B.*`)
			// see segment 0 (`main`) as a visible FROM alias (the alias of
			// `PA AS main`) and reclassify the real schema-qualified table
			// `main.PB` as a correlated unnest of `MAIN.PB`. The strip ran
			// IFF the dotted name was `[schema, table]` (resolvesToTable),
			// so segment 0 is the schema qualifier, not a prior FROM alias:
			// dropping it yields the single-segment table name the rebuild
			// classifier reads as a plain table. A genuine lateral unnest
			// `alias.field` (segment 0 a prior FROM alias) does not resolve
			// to a table, never enters this branch, and keeps its segments.
			// RFC-142.
			if len(j.segments) == 2 && strings.EqualFold(j.segments[0], schemaName) {
				j.segments = j.segments[1:]
			}
		}
	}
}

func buildLogicalPlanForSelectWithCTECatalog_postBuild(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteBodies ...map[string]logical.LogicalOperator) (logical.LogicalOperator, error) {
	// Build the semantic scope once. All identifier resolution below
	// goes through this scope — same architecture as Java's
	// QueryVisitor holding a SemanticAnalyzer.
	resolver := buildSelectScope(sq, md, schemaName, cteScopes)

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
		expandProjQualifier(sq, md, schemaName)
		needRebuild = true
	}
	if hasAnyQualifiedStar(sq) {
		expandQualifiedStars(sq, md, schemaName)
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
					v, walkErr := resolver.WalkExpression(sq.projExprs[i])
					if walkErr != nil {
						var corrErr *CorrelatedExistsError
						if errors.As(walkErr, &corrErr) {
							return nil, walkErr
						}
					}
					if walkErr == nil && v != nil {
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
			// A BARE column that binds to a lateral-unnest SHADOWING source
			// (`FROM t, t.arr AS v, …`) must be projected QUALIFIED to the unnest
			// correlation (`v.v`), not as a bare `v`. The unnest element flows the
			// merged row under both bare `v` and qualified `v.v`, but a LATER FROM
			// item with its own `v` overwrites the bare key last-leg-wins in
			// mergeRows; the qualified `v.v` survives (dotted keys preserved
			// verbatim). This is the SUBQUERY / DML / derived-table SELECT-build path,
			// the twin of the PlanVisitor's bare-projection step — both reuse
			// ResolveColumnShadowingQualified so the catalog and top-level paths shadow
			// identically. Without this a shadowed unnest projection inside a subquery
			// reads the wrong column (silent-wrong). RFC-142.
			if ref := parseColRef(col); !ref.isQualified() && proj != nil {
				id := semantic.NewUnquoted(ref.bare())
				if qv, ok, qerr := resolver.ResolveColumnShadowingQualified(semantic.Identifier{}, id); qerr == nil && ok {
					if proj.ProjectedValues == nil {
						proj.ProjectedValues = make([]values.Value, len(proj.Projections))
					}
					if i < len(proj.ProjectedValues) {
						proj.ProjectedValues[i] = qv
					}
				}
			}
			if parseColRef(col).isQualified() && proj != nil {
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
					ref := parseColRef(col)
					id := semantic.NewUnquoted(ref.bare())
					if ref.isQualified() {
						qualifier = semantic.NewUnquoted(ref.table)
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
			// RFC-141 R4: EXISTS in an ORDER BY key is NOT a
			// directly-handled position. The sort-key resolver carries no
			// SubqueryPlanner, so the EXISTS would fail to resolve, the key would
			// keep its raw text form, and the existential would never be
			// evaluated → a silent WRONG ORDERING (all rows tie on a constant).
			// Reject cleanly rather than mis-order (mirrors the scalar-subquery
			// rejection above).
			if expr.ContainsExistsAtom(ob.rawExpr) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"EXISTS in an ORDER BY clause is not yet supported")
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

	// Detect overflow numeric literals and correlated-subquery rejections
	// in projection expressions.
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
				var corrErr *CorrelatedExistsError
				if errors.As(walkErr, &corrErr) {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation, corrErr.Error())
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
		if err := upgradeJoinOnPredicates(op, sq, md, schemaName, cteScopes); err != nil {
			return nil, err
		}
	}

	if len(sq.aggCols) > 0 {
		upgradeAggregateOperands(op, sq, md, schemaName, cteScopes)
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
			schemaName:  schemaName,
			outerScopes: buildOuterScopeSources(sq, md, schemaName),
			cteScopes:   cteScopes,
			cteBodies:   bodies,
		}
	}

	if len(sq.projExprs) > 0 || len(sq.postAggExprs) > 0 {
		if err := upgradeProjectionValues(op, sq, md, schemaName, cteScopes, existsPlanner); err != nil {
			return nil, err
		}
	}

	// Attach scalar subqueries from projections to the LogicalProject.
	if existsPlanner != nil && len(existsPlanner.scalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.ScalarSubqueries = existsPlanner.scalarSubqueries
		}
		existsPlanner.scalarSubqueries = nil
	}
	if existsPlanner != nil && len(existsPlanner.correlatedScalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.CorrelatedScalarSubqueries = existsPlanner.correlatedScalarSubqueries
		}
		existsPlanner.correlatedScalarSubqueries = nil
	}

	if sq.havingExpr != nil {
		upgradeHavingPredicate(op, sq, md, schemaName, cteScopes, existsPlanner)
	}

	upgradeSortKeyValues(op, sq, md, schemaName, cteScopes)

	// A BARE ORDER BY sort key that binds to a lateral-unnest SHADOWING source
	// (`FROM t, t.arr AS v, …`) must sort by the key QUALIFIED to the unnest
	// correlation (`v.v`), exactly as the bare PROJECTION column above. A LATER FROM
	// item with its own `v` overwrites the bare sort key last-leg-wins in mergeRows;
	// the qualified `v.v` survives. Without this the SORT reads the clobbered bare
	// key → rows in the WRONG ORDER (silent-wrong). This is the SUBQUERY / DML /
	// derived-table SELECT-build twin of the PlanVisitor's step (15a), reusing the
	// SAME qualifyShadowedSortKeys / ResolveColumnShadowingQualified helpers so the
	// catalog and top-level paths shadow ORDER BY identically. RFC-142.
	if resolver != nil {
		qualifyShadowedSortKeys(op, resolver)
	}

	// RFC-141 Phase 2 (projected EXISTS, the hidden-blocker step): a projected
	// ExistsValue carries an existential alias but, unlike a WHERE-EXISTS, it is
	// not collected into the existential-subquery list the translator reads to
	// attach the NamedExistentialQuantifier. upgradeProjectionValues already ran
	// BuildExists for projected EXISTS (populating existsPlanner.subqueries via
	// the walk's walkExistsValue). When there is no WHERE clause to carry them,
	// synthesize a LogicalFilter (nil predicate) above the scan to hold the
	// projected-EXISTS subqueries so the translator attaches the existential
	// quantifier and builds the FlatMap; the existential boolean is then computed
	// by the projection's ExistsValue inside the SelectExpression's result value.
	if sq.whereExpr == nil && existsPlanner != nil && len(existsPlanner.subqueries) > 0 &&
		projectionHasExistsValue(op) {
		op = attachOrSynthesizeExistsFilter(op, existsPlanner.subqueries)
		existsPlanner.subqueries = nil
		// Fall through to QUALIFY handling below (the synthesized filter and a
		// QUALIFY predicate are independent).
	}

	if sq.whereExpr == nil {
		// No WHERE, but a QUALIFY filter (the vector K-NN ROW_NUMBER() <= K
		// predicate) must still be attached — synthesize a LogicalFilter above
		// the scan rather than dropping it (an unpartitioned KNN query has no
		// WHERE, so no filter was built upstream).
		qualPred, qErr := buildQualifyPredicate(md, schemaName, sq, cteScopes)
		if qErr != nil {
			return nil, qErr
		}
		if qualPred != nil {
			op = attachOrSynthesizeFilter(op, qualPred)
		}
		return op, nil
	}

	// RFC-141 R4: this select-build path (used for SUBQUERIES — scalar /
	// EXISTS / derived-table inner plans built via buildLogicalPlanForQueryWith*)
	// is a SECOND WHERE-build path, distinct from the PlanVisitor's
	// visitSelectQuery (which carries the same guard). An EXISTS buried in a SCALAR
	// expression in this subquery's WHERE (`(SELECT MAX(id) FROM t2 WHERE CASE WHEN
	// EXISTS(...) THEN 1 ELSE 0 END = 1)`) lowers to a constant-false Value with no
	// existential quantifier driving it — a silent wrong result for the subquery.
	// Detect it structurally and reject cleanly here too, so a nested subquery
	// behaves identically whether it runs standalone or embedded in an outer query
	// (the boundary stop makes the OUTER detector NOT pre-empt this — the
	// subquery owns its own clause; this guard is where that ownership is enforced).
	if sq.whereExpr.Expression() != nil && expr.WhereExistsInScalarPosition(sq.whereExpr.Expression()) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"EXISTS nested in a scalar expression is not yet supported")
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
			// A BARE column unresolvable in THIS (sub)query's own scope. For a
			// correlated subquery (`SELECT 1 FROM UV WHERE UV.V = VAL` where VAL is the
			// OUTER query's lateral-unnest element binding), the column is not local;
			// surface it as ErrCodeUndefinedColumn so BuildExists falls back to
			// buildCorrelatedExists, which resolves it against the richer outer scope
			// (outerScopes — including the unnest virtual source, RFC-142
			// P2c). This mirrors the qualified-outer-ref path above
			// (SourceNotFoundError for `T1.ID`): a bare outer ref must take the SAME
			// correlation fallback as a qualified one, never silently degrade to a
			// text WHERE the translator then rejects. Only the SUBQUERY / derived-table
			// inner-plan build reaches this walk (the main query plans via the
			// PlanVisitor and validates columns separately), so the main-query text
			// fallback is unaffected.
			var colNotFound *semantic.ColumnNotFoundError
			if errors.As(walkErr, &colNotFound) {
				name := colNotFound.Id.Name()
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q does not exist", name)
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
			// A structured *api.Error from the walk is a deliberate, already
			// SQLSTATE-classified rejection raised by a nested subquery's build
			// (e.g. an EXISTS subquery whose own WHERE buries a scalar EXISTS, which
			// BuildExists' postBuild guard rejects with ErrCodeUnsupportedQuery —
			// RFC-141 R4). It must surface VERBATIM, not fall through to
			// the text-fallback predicate builder below — which declines the EXISTS
			// shape and reports the generic "Cascades planner could not plan",
			// masking the precise reason. (The specific remappings above run first,
			// so a fallback-intended ErrCodeUndefinedColumn still takes precedence.)
			var apiErr *api.Error
			if errors.As(walkErr, &apiErr) {
				return nil, apiErr
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

		// EXISTS is lowered to a conjunctive semi-join; under an OR that loses
		// the disjunction and silently returns empty. Reject rather than
		// return wrong rows (RFC-082; inline-EXISTS-under-OR is future work).
		if existsUnderDisjunction(pred) {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"EXISTS within an OR (disjunction) is not supported")
		}

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
		pred, ok = buildWherePredicateForJoinsWithCTEScopes(md, schemaName, sq, sq.whereExpr, cteScopes)
	}
	if !ok {
		pred, ok = buildWherePredicate(md, schemaName, sq, sq.whereExpr)
	}
	// QUALIFY (vector K-NN ROW_NUMBER() filter) is AND-combined with the WHERE
	// predicate onto the same LogicalFilter — upgradeFirstFilter replaces, so
	// both must be attached together.
	qualPred, qErr := buildQualifyPredicate(md, schemaName, sq, cteScopes)
	if qErr != nil {
		return nil, qErr
	}
	if qualPred != nil {
		if ok {
			pred = predicates.NewAnd(pred, qualPred)
		} else {
			pred, ok = qualPred, true
		}
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
	schemaName string,
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
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	for i, j := range sq.joins {
		if j.derivedQuery != nil {
			if src, ok := buildDerivedTableSource(md, j.alias, j.derivedQuery); ok {
				if scope.AddSource(src) != nil {
					return nil
				}
				continue
			}
		}
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			// A lateral array unnest (`FROM t, t.arr AS x [AT ord]`) exposes its
			// AS/AT columns as a virtual scope source so projection / WHERE /
			// ORDER BY references resolve and column validation passes (RFC-142).
			if !addUnnestSource(j) {
				return nil
			}
			continue
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
	ref := parseColRef(col)
	id := semantic.NewUnquoted(ref.bare())
	if ref.isQualified() {
		qualifier = semantic.NewUnquoted(ref.table)
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
		addUnnestStarAlias(validSources, j)
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

// addUnnestStarAlias whitelists a lateral-unnest comma source's element (AS) and
// ordinal (AT) binding aliases — the SAME names unnestAliases derives — so a
// qualified star over them (`SELECT V.*` / aliasless `SELECT ARR.*`) passes
// validation and reaches the unnest-aware expansion (expandQualifiedStars /
// expandProjQualifier). For an aliasless unnest (`FROM t, t.arr`) the parser
// defaults j.alias to the flattened segment name (`T1.ARR`), while the element
// binds under the DEFAULT alias (the array field name `ARR`); the raw
// tableName/alias whitelist alone misses that default, so an aliasless `ARR.*`
// would be rejected 42F01 before the expansion could run. unnestAliases is the
// single source of truth shared with the expansion, so validator and expansion
// agree on the alias. RFC-142.
func addUnnestStarAlias(validSources map[string]bool, j joinClause) {
	asAlias, atAlias := unnestAliases(j)
	if asAlias != "" {
		validSources[strings.ToUpper(asAlias)] = true
	}
	if atAlias != "" {
		validSources[strings.ToUpper(atAlias)] = true
	}
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
		addUnnestStarAlias(validSources, j)
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
	return parseColRef(field).bare()
}

// splitNonExistsPredicatesFromWalked returns only the non-EXISTS parts
// of a walked predicate tree. EXISTS and NOT(EXISTS) nodes are dropped.
// Returns nil if there are no non-EXISTS predicates.
func splitNonExistsPredicatesFromWalked(p predicates.QueryPredicate) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(p); ok {
		return nil
	}
	if _, ok := predicates.IsNotExistentialPredicate(p); ok {
		return nil
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
	if _, ok := predicates.IsExistentialPredicate(p); ok {
		return p
	}
	if _, ok := predicates.IsNotExistentialPredicate(p); ok {
		return p
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

// existsUnderDisjunction reports whether an EXISTS / NOT EXISTS predicate is
// reachable through an OR in the predicate tree. Go lowers EXISTS predicates to
// conjunctive semi-joins (FlatMap), which is only correct under AND. Under an
// OR the EXISTS must instead be evaluated as an inline boolean (P OR EXISTS(Q)
// is true when P is true OR Q matches) — not yet supported. Callers reject with
// a clear error rather than returning wrong rows: the split helpers
// (stripNonExistsPredicates / splitNonExistsPredicatesFromWalked) only recurse
// through AND, so an EXISTS under OR is silently mis-extracted into an
// unconditional semi-join and the disjunction is lost (returns empty).
func existsUnderDisjunction(p predicates.QueryPredicate) bool {
	return existsReachableUnderOr(p, false)
}

func existsReachableUnderOr(p predicates.QueryPredicate, underOr bool) bool {
	if p == nil {
		return false
	}
	if _, ok := predicates.IsExistentialPredicate(p); ok {
		return underOr
	}
	if _, ok := p.(*predicates.OrPredicate); ok {
		underOr = true
	}
	for _, ch := range p.Children() {
		if existsReachableUnderOr(ch, underOr) {
			return true
		}
	}
	return false
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

// projectionHasExistsValue reports whether the LogicalProject on op's unary
// spine carries a projected ExistsValue (RFC-141 Phase 2). Walks the projected
// Value trees TYPED — no GetText / text matching — so `NOT EXISTS` (NotValue
// over ExistsValue), CASE branches, etc. are all detected structurally.
func projectionHasExistsValue(op logical.LogicalOperator) bool {
	proj := findProjection(op)
	if proj == nil {
		return false
	}
	for _, v := range proj.ProjectedValues {
		if v == nil {
			continue
		}
		found := false
		values.WalkValue(v, func(node values.Value) bool {
			if _, ok := node.(*values.ExistsValue); ok {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// attachOrSynthesizeExistsFilter attaches the projected-EXISTS subqueries to
// the first LogicalFilter on op's unary spine; if there is none (a projected
// EXISTS with no WHERE builds no filter), it synthesizes a LogicalFilter (nil
// predicate) directly above the base LogicalScan to hold them — the same
// position a WHERE filter occupies — so the translator attaches the existential
// quantifier and builds the FlatMap. Returns the (possibly new) plan root.
func attachOrSynthesizeExistsFilter(op logical.LogicalOperator, subqueries []logical.ExistsSubquery) logical.LogicalOperator {
	if upgradeFirstFilterExistsSubqueries(op, subqueries) {
		return op
	}
	if _, isScan := op.(*logical.LogicalScan); isScan {
		f := logical.NewFilterWithPredicate(op, nil, "")
		f.ExistsSubqueries = subqueries
		return f
	}
	// Walk the unary spine to the deepest unary operator and synthesize the
	// existential filter as its child — directly above the scan OR the join.
	// The filter MUST land UNDER the projection (between the last unary op and
	// the leaf/join), never above it: a filter above the projection runs the
	// projection — and its projected ExistsValue — BEFORE the FlatMap, where the
	// existential binding is dead (the join-from leak). With the filter
	// below the projection, translateProject's findExistsFilterUnderUnaryChain
	// reaches the existential filter and folds the projection into the
	// existential SelectExpression's result value (RFC-141), handling a JOIN
	// input via buildExistentialSelect's join flatten.
	for cur := op; cur != nil; {
		child, ok := unaryInput(cur)
		if !ok {
			// op itself is non-unary (e.g. a bare LogicalJoin with no project).
			// Wrap it directly; there is no projection above to displace.
			f := logical.NewFilterWithPredicate(op, nil, "")
			f.ExistsSubqueries = subqueries
			return f
		}
		// child is the deepest unary's input when it is a scan OR a join (any
		// non-unary). Either way the synthesized filter must sit on top of that
		// child, below cur, so the spine becomes ...cur -> Filter(child).
		if _, childUnary := unaryInput(child); !childUnary {
			f := logical.NewFilterWithPredicate(child, nil, "")
			f.ExistsSubqueries = subqueries
			setUnaryInput(cur, f)
			return op
		}
		cur = child
	}
	return op
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
func upgradeProjectionValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) error {
	proj := findProjection(op)
	if proj == nil {
		return nil
	}
	// Post-aggregation projections: walk through the Resolver using base
	// table scope, then rewrite AggregateValues to FieldValue references.
	if len(sq.postAggExprs) > 0 {
		resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
		if resolver == nil {
			resolver = buildSelectScope(sq, md, schemaName, cteScopes)
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
		aggSlots := make([]bool, len(proj.Projections))
		for i, e := range sq.postAggExprs {
			if i >= len(vals) || e == nil {
				continue
			}
			if groupKeyExplains != nil {
				projText := strings.ToUpper(strings.TrimSpace(canonicalTextOf(e)))
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
			aggSlots[i] = containsAggregate(v) // pre-rewrite: aggregate nodes still present
			v = rewriteAggregateValuesInTree(v)
			vals[i] = v
		}
		// Unifying post-aggregate rebase: a computed projection over
		// a grouped unnest key (`V + 1`) resolves `V` against the PRE-aggregate
		// Shadowing scope → qualified FieldValue(QOV(V), V) (explain `V.V`), but the
		// aggregate cursor outputs the group key under the BARE `V`. Rebase every
		// post-aggregate reference to a qualified grouped-unnest group key down to the
		// bare aggregate-output name — the SAME aggregateGroupKeyOutputName the
		// ORDER-BY rebase uses — so the projection reads the element, not a missing
		// `V.V` key (→ NULL). RFC-142.
		for i := range vals {
			vals[i] = rebasePostAggregateGroupKeyValue(vals[i], agg)
		}
		proj.ProjectedValues = vals
		proj.AggregateSlots = aggSlots
		return nil
	}

	// Regular projections.
	exprs := sq.projExprs
	if len(exprs) == 0 {
		return nil
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
	}
	if resolver == nil {
		return nil
	}
	if subqPlanner != nil {
		resolver.SetSubqueryPlanner(subqPlanner)
	}
	vals := make([]values.Value, len(proj.Projections))
	copy(vals, proj.ProjectedValues)
	aggSlots := make([]bool, len(proj.Projections))
	for i, e := range exprs {
		if i >= len(vals) {
			break
		}
		if e == nil {
			continue
		}
		v, err := resolver.WalkExpressionForProjection(e)
		if err != nil {
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				return err
			}
			var corrErr *CorrelatedExistsError
			if errors.As(err, &corrErr) {
				return api.NewError(api.ErrCodeUnsupportedOperation, corrErr.Error())
			}
			// RFC-141 R4 (P1b): a SELECT item with a NESTED EXISTS is not
			// foldable; reject cleanly rather than fall through to the text path
			// (which would evaluate the ExistsValue with a dead binding → wrong).
			var nestedExists *expr.NestedExistsProjectionError
			if errors.As(err, &nestedExists) {
				return api.NewError(api.ErrCodeUnsupportedQuery, nestedExists.Error())
			}
			continue
		}
		if !isCascadesSafeValue(v) {
			continue
		}
		aggSlots[i] = containsAggregate(v) // pre-rewrite: aggregate nodes still present
		v = rewriteAggregateValuesInTree(v)
		vals[i] = v
	}
	proj.ProjectedValues = vals
	proj.AggregateSlots = aggSlots
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

func upgradeAggregateOperands(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) {
	agg := findAggregate(op)
	if agg == nil {
		return
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
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

	// Resolve GROUP BY key Values. Two sources:
	//   - COMPUTED keys (`GROUP BY x.col1 + x.col2`) carry a parse expr in
	//     sq.groupByExprs[i] → resolve via WalkExpressionForProjection.
	//   - SIMPLE column keys (`GROUP BY V`) do NOT populate groupByExprs, so they
	//     fall through to the translator's bare FieldValue{V} fallback. For a bare
	//     key that binds to a lateral-unnest SHADOWING source (`FROM t, t.arr AS V,
	//     u` where a LATER FROM item u also has a column V), that bare FieldValue
	//     reads the merged row's bare `V` key — which mergeRows overwrites
	//     last-leg-wins with u.V — so grouping happens on the LATER table's column,
	//     not the shadowing unnest element (P2a, silent-wrong
	//     grouping). Route the simple key through ResolveColumnShadowingQualified —
	//     the SAME helper the projection (buildSelectShell) and ORDER-BY
	//     (qualifyShadowedSortKeys) paths use — so a key that binds to the unnest
	//     resolves to the QUALIFIED `V.V` (which mergeRows preserves verbatim),
	//     grouping on the unnest element. An explicitly-qualified `u.V` key binds to
	//     u's real source (not Shadowing) → ok=false → left for the bare fallback,
	//     so the control (group by the later column) is unaffected. RFC-142.
	keyValues := make([]values.Value, len(agg.GroupKeys))
	filled := false
	for i := range agg.GroupKeys {
		if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
			v, err := resolver.WalkExpressionForProjection(sq.groupByExprs[i])
			if err != nil {
				continue
			}
			keyValues[i] = v
			filled = true
			continue
		}
		ref := parseColRef(agg.GroupKeys[i])
		var qualID semantic.Identifier
		if ref.isQualified() {
			qualID = semantic.NewUnquoted(ref.table)
		}
		qv, ok, err := resolver.ResolveColumnShadowingQualified(qualID, semantic.NewUnquoted(ref.bare()))
		if err == nil && ok {
			keyValues[i] = qv
			filled = true
		}
	}
	if filled {
		agg.GroupKeyValues = keyValues
	}
}

func upgradeHavingPredicate(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) {
	agg := findAggregate(op)
	if agg == nil || sq.havingExpr == nil {
		return
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
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
	// Unifying post-aggregate rebase: a HAVING reference to a
	// grouped unnest key (`HAVING V > x`) resolves `V` against the PRE-aggregate
	// scope → qualified `V.V`. When the predicate stays ABOVE the aggregate (it
	// also references an aggregate, e.g. `V > x AND COUNT(*) > 1`), `V.V` reads the
	// MISSING key off the bare-V aggregate row → NULL → every group dropped; rebase
	// it to the bare aggregate-output name, the SAME aggregateGroupKeyOutputName the
	// projection + ORDER-BY post-aggregate paths use. A PURE group-key HAVING is
	// pushed BELOW the aggregate (PushFilterThroughGroupByRule) and MUST stay
	// qualified there (the pre-aggregate row binds `V.V`, the unnest element);
	// rebaseHavingGroupKeyPredicate keeps that case untouched. RFC-142.
	agg.HavingPredicate = rebaseHavingGroupKeyPredicate(
		rewriteAggregateRefsInPredicate(pred), agg)
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
		// Copy the whole Comparison and replace ONLY the rewritten RHS operand,
		// preserving Escape and every other Comparison field. A fresh
		// {Type, Operand} would drop the LIKE escape rune (and the parameter /
		// text / distance-rank metadata) and change comparison semantics. RFC-142.
		cmp := p.Comparison
		cmp.Operand = rewriteAggregateValuesInTree(p.Comparison.Operand)
		return predicates.NewComparisonPredicate(lhs, cmp)
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
	case *predicates.NotPredicate:
		return predicates.NewNot(rewriteAggregateRefsInPredicate(p.Child))
	}
	return pred
}

// containsAggregate reports whether v's value tree contains any
// *values.AggregateValue. Called PRE-rewrite (before
// rewriteAggregateValuesInTree replaces aggregates with typed FieldValue
// references) so the INSERT…SELECT promotion guard can mark which projection
// slots are aggregate-derived. Tree-walk, not a top-level type assert:
// `AVG(x)+1` is a top-level ArithmeticValue that still resolves to DOUBLE and
// must be guarded.
func containsAggregate(v values.Value) bool {
	found := false
	values.WalkValue(v, func(n values.Value) bool {
		if _, ok := n.(*values.AggregateValue); ok {
			found = true
			return false // stop descending
		}
		return !found
	})
	return found
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

// canonicalAggName is the single canonicaliser for an aggregate's result-row
// column name. Both the HAVING-predicate rewrite (rewriteAggregateValue) and the
// correlated-scalar-subquery aggregate builder name aggregates through it, so a
// HAVING reference always resolves against the materialised slot — they cannot
// drift. funcSymbol is the aggregate function symbol (e.g. "SUM", "COUNT", or
// the count-star op's "COUNT(*)"); operand is the (already-resolved) argument
// Value, or nil for a no-operand aggregate. The form mirrors what the executor's
// aggResultName produces: FN(<uppercased ExplainValue, spaces stripped, one
// outer-paren pair stripped>), with COUNT(*)/no-operand => "FN(*)".
func canonicalAggName(funcSymbol string, operand values.Value) string {
	fn := strings.ToUpper(funcSymbol)
	if fn == "COUNT(*)" {
		return "COUNT(*)"
	}
	inner := "*"
	if operand != nil {
		inner = strings.ToUpper(values.ExplainValue(operand))
		inner = strings.ReplaceAll(inner, " ", "")
		if len(inner) > 2 && inner[0] == '(' && inner[len(inner)-1] == ')' {
			inner = inner[1 : len(inner)-1]
		}
	}
	return fn + "(" + inner + ")"
}

func rewriteAggregateValue(v values.Value) values.Value {
	if v == nil {
		return nil
	}
	av, ok := v.(*values.AggregateValue)
	if !ok {
		return v
	}
	// Preserve the aggregate's result type on the reference — a reference must
	// report the type of its referent. (Previously discarded as UnknownType,
	// which left every downstream type query on a rewritten projection blind;
	// the INSERT…SELECT promotion guard relies on this carrying e.g. AVG→DOUBLE.)
	return &values.FieldValue{
		Field: canonicalAggName(av.Op.Symbol(), av.Operand),
		Typ:   av.Type(),
	}
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
	// RFC-141 §8 safety guard: GROUP BY on an EXISTS expression (e.g. `GROUP BY
	// id, EXISTS(...)` where the EXISTS column is the grouping key) cannot be
	// folded — the aggregate path has no SubqueryPlanner, so the existential
	// never resolves to a Value and the group key silently evaluates to a
	// constant. Reject cleanly rather than ship a constant-false grouped column.
	// Structural detection (typed ANTLR node), no text matching.
	for _, gbe := range sq.groupByExprs {
		if gbe != nil && expr.ContainsExistsAtom(gbe) {
			return api.NewError(api.ErrCodeUnsupportedQuery,
				"projected EXISTS in this query shape is not yet supported")
		}
	}

	groupBySet := make(map[string]bool, len(sq.groupBy))
	for _, gb := range sq.groupBy {
		ref := parseColRef(gb)
		groupBySet[strings.ToUpper(gb)] = true
		if ref.isQualified() {
			groupBySet[strings.ToUpper(ref.bare())] = true
		}
	}

	// Collect the field set from EVERY base-table source — the primary
	// table AND each join source — so a GROUP BY / projection key from a
	// joined table (`SELECT d.dname ... FROM emp e JOIN dept d ... GROUP BY
	// d.dname`) passes the existence check instead of falsely 42703-ing
	// because it isn't a column of the first table. If ANY source is a
	// derived table / CTE (no record type), its columns are unknowable, so
	// skip the existence check entirely (tableFields = nil) — conservative,
	// matching the pre-join behaviour for an unresolvable primary source.
	var tableFields map[string]bool
	allResolved := true
	if md != nil {
		collect := func(tableName string) {
			if tableName == "" {
				allResolved = false
				return
			}
			rt := md.GetRecordType(tableName)
			if rt == nil || rt.Descriptor == nil {
				allResolved = false
				return
			}
			if tableFields == nil {
				tableFields = make(map[string]bool)
			}
			fields := rt.Descriptor.Fields()
			for i := 0; i < fields.Len(); i++ {
				tableFields[strings.ToUpper(string(fields.Get(i).Name()))] = true
			}
		}
		collect(sq.tableName)
		for _, j := range sq.joins {
			collect(j.tableName)
		}
	}
	if !allResolved {
		tableFields = nil
	}

	// INVARIANT (load-bearing): this existence test compares only the BARE
	// name against the UNION of all source fields, so it is deliberately
	// qualifier-blind — `e.dname` (dname on the joined dept, not emp) would
	// pass here because DNAME is in the union. That coarse check is SAFE only
	// because EVERY call site is bracketed by a precise semantic resolver gate
	// that has the final say on a wrong-qualifier / genuinely-undefined key —
	// the union check never decides alone. The gate runs on DIFFERENT sides at
	// the two sites:
	//   - top-level GROUP BY: resolveColumnName(resolver, gb) (this file, ~L1002)
	//     runs BEFORE validateGroupByProjection (~L1019), so a wrong qualifier is
	//     rejected before it ever reaches this union check.
	//   - correlated scalar subquery: validateGroupByProjection (~L4414) runs
	//     FIRST and may pass a wrong-qualifier key, but resolveCorrelatedGroupKeyValues
	//     (~L4654, "resolve GROUP BY key: ... not found on table") runs AFTER and
	//     rejects it — the net protection still holds, via the later gate.
	// Both orderings are pinned by TestFDB_GroupByWrongQualifierRejected. The real
	// hazard is a NEW call site with NO resolver gate on either side; converging
	// the existence check onto resolver.ResolveIdentifier removes the coupling
	// entirely (TODO.md, RFC-088 follow-up).
	checkColumn := func(col string) error {
		upper := strings.ToUpper(col)
		bare := parseColRef(upper).bare()
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

func buildProjectionResolverWithCTEScopes(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) *expr.Resolver {
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
	// A lateral array unnest leg is not a real table — register its virtual
	// element/ordinal source via the SAME shared helper buildSelectScope uses, so
	// a projection / GROUP BY / HAVING / ORDER BY over an unnest column resolves
	// here directly (the callers' buildSelectScope fallback becomes belt-and-
	// suspenders, no longer load-bearing). RFC-142.
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if sq.tableName != "" {
		if sq.derivedQuery != nil {
			if !addDerived(sq.tableName, sq.derivedQuery) {
				return nil
			}
		} else if !addSource(sq.tableName, sq.tableAlias) {
			return nil
		}
	}
	for i, j := range sq.joins {
		if j.derivedQuery != nil {
			if !addDerived(j.alias, j.derivedQuery) {
				return nil
			}
			continue
		}
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if !addUnnestSource(j) {
				return nil
			}
			continue
		}
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
	schemaName string,
) (logical.LogicalOperator, error) {
	op := buildLogicalPlanForDelete(del)
	if op == nil || md == nil || del == nil {
		return op, nil
	}
	tableName := ""
	if tn := del.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	w := del.WhereExpr()
	if w == nil || tableName == "" {
		return op, nil
	}
	// A DML statement runs inside a single schema's store, so the record
	// type is the bare name; strip any schema qualifier before resolving
	// and aliasing the predicate, so refs bind to the resolved scan
	// (which resolveQualifiedTableNames also reduces to the bare name).
	bare := bareTableName(tableName)
	// Prefer the subquery-aware path so DELETE … WHERE EXISTS(…) plans
	// through Cascades; fall back to the plain predicate builder. A carried
	// SQLSTATE from a WHERE-EXISTS subquery plan failure (RFC-142: AT-on-a-table
	// → WRONG_OBJECT_TYPE) is surfaced rather than masked by the text fallback.
	if ok, carried := upgradeDMLWhereWithCatalog(op, md, bare, w, schemaName); ok {
		return op, nil
	} else if carried != nil {
		return nil, carried
	}
	pred, ok := buildWherePredicateForTable(md, bare, bare, w)
	if !ok {
		return op, nil
	}
	_ = upgradeFirstFilter(op, pred) // invariant: text builder always emits a Filter for a WHERE clause
	return op, nil
}

// bareTableName strips a leading schema qualifier ("s1.T" → "T"). Used so
// DML predicate resolution and correlation aliases match the resolved
// (bare) scan name.
func bareTableName(name string) string {
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

// upgradeDMLWhereWithCatalog upgrades the WHERE filter of a single-table
// DML plan (DELETE / UPDATE) to a real predicate with full EXISTS / scalar
// subquery support — the same machinery the SELECT path uses (an
// existsSubqueryPlanner installed on the resolver). This is what lets
// `DELETE … WHERE EXISTS(…)` plan through Cascades like SELECT; the plain
// buildWherePredicateForTable has no SubqueryPlanner and declines the
// EXISTS shape. Returns ok=false when the WHERE can't be resolved (caller
// falls back to the plain predicate builder).
//
// A WHERE-EXISTS subquery that fails to PLAN with a carried, specific SQLSTATE
// (e.g. RFC-142's AT-ordinality-on-a-table → WRONG_OBJECT_TYPE) must NOT be
// swallowed into a silent text fallback: the fallback can't plan the EXISTS
// either and the user sees a generic "DML Cascades translation failed" (0AF00)
// instead of the faithful diagnostic. Return that carried error so the DML
// builder surfaces it — the same precedence the SELECT path gives a
// translation error code. A non-specific resolver error (no api.Error in the
// chain) is the ordinary "WHERE not resolvable here" signal and still falls back.
func upgradeDMLWhereWithCatalog(
	op logical.LogicalOperator,
	md *recordlayer.RecordMetaData,
	tableName string,
	whereExpr antlrgen.IWhereExprContext,
	schemaName string,
) (ok bool, carried error) {
	if op == nil || md == nil || whereExpr == nil || whereExpr.Expression() == nil {
		return false, nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	sq := &selectQuery{tableName: tableName, tableAlias: tableName, limit: -1}
	resolver := buildSelectScope(sq, md, schemaName, nil)
	if resolver == nil {
		return false, nil
	}
	// schemaName threads onto the EXISTS-subquery planner so a schema-qualified
	// comma source inside a `DELETE/UPDATE … WHERE EXISTS (SELECT 1 FROM PA AS
	// main, main.PB AS B)` classifies main.PB as a schema-qualified TABLE against
	// the ACTIVE schema, not the hardcoded default. RFC-142.
	existsPlanner := &existsSubqueryPlanner{
		md:          md,
		schemaName:  schemaName,
		outerScopes: buildOuterScopeSources(sq, md, schemaName),
	}
	resolver.SetSubqueryPlanner(existsPlanner)
	walked, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil || walked == nil {
		// A specific carried SQLSTATE from a subquery PLAN failure (the EXISTS
		// inner build returned an *api.Error, e.g. WRONG_OBJECT_TYPE for an
		// AT-on-a-table source) takes precedence over the text fallback — surface
		// it so the DML rejection matches the SELECT path's faithful code. Gate on
		// the WHERE actually containing an EXISTS atom: that is the one shape the
		// plain text builder (buildWherePredicateForTable) cannot plan AT ALL, so
		// the catalog path's error is authoritative — for a plain comparison WHERE
		// the text fallback may still succeed, and swallowing here preserves that.
		var apiErr *api.Error
		if errors.As(err, &apiErr) && expr.ContainsExistsAtom(whereExpr.Expression()) {
			return false, apiErr
		}
		return false, nil
	}
	if !upgradeFirstFilter(op, predicates.SimplifyPredicateValues(walked)) {
		return false, nil
	}
	if len(existsPlanner.subqueries) > 0 {
		upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries)
	}
	if len(existsPlanner.scalarSubqueries) > 0 {
		upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries)
	}
	return true, nil
}

// buildLogicalPlanForUpdateWithCatalog is the catalog-aware variant
// of buildLogicalPlanForUpdate. Same shape as the Delete variant —
// walker failure falls back to text form on LogicalFilter.
func buildLogicalPlanForUpdateWithCatalog(
	upd antlrgen.IUpdateStatementContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
) (logical.LogicalOperator, error) {
	op := buildLogicalPlanForUpdate(upd)
	if op == nil || md == nil || upd == nil {
		return op, nil
	}
	updOp, ok := op.(*logical.LogicalUpdate)
	if !ok {
		return op, nil
	}
	tableName := ""
	if tn := upd.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	if tableName == "" {
		return op, nil
	}
	bare := bareTableName(tableName)

	// Resolve each SET RHS expression to a real Value against the target
	// table (e.g. `price / 2` → Divide(FieldValue(PRICE), 2)) so the
	// executor evaluates it per row instead of choking on raw text. The
	// iteration mirrors buildLogicalPlanForUpdate's append order/skip.
	if resolver := buildSelectScope(&selectQuery{tableName: bare, tableAlias: bare, limit: -1}, md, schemaName, nil); resolver != nil {
		idx := 0
		for _, el := range upd.AllUpdatedElement() {
			if el == nil || el.FullColumnName() == nil || el.Expression() == nil {
				continue
			}
			if idx < len(updOp.Sets) {
				if v, err := resolver.WalkExpression(el.Expression()); err == nil && v != nil {
					updOp.Sets[idx].Value = v
				}
			}
			idx++
		}
	}

	// Upgrade the WHERE filter with EXISTS/scalar subquery support; fall
	// back to the plain predicate builder. No WHERE → UPDATE all rows. A carried
	// SQLSTATE from a WHERE-EXISTS subquery plan failure (RFC-142: AT-on-a-table
	// → WRONG_OBJECT_TYPE) is surfaced rather than masked by the text fallback.
	if w := upd.WhereExpr(); w != nil {
		if ok, carried := upgradeDMLWhereWithCatalog(op, md, bare, w, schemaName); !ok {
			if carried != nil {
				return nil, carried
			}
			if pred, ok := buildWherePredicateForTable(md, bare, bare, w); ok {
				_ = upgradeFirstFilter(op, pred)
			}
		}
	}
	return op, nil
}

// buildLogicalPlanForInsertWithCatalog is the catalog-aware variant
// of buildLogicalPlanForInsert. INSERT VALUES has no nested query so
// it short-circuits to the text builder; INSERT … SELECT routes the
// inner SELECT through the catalog-aware Select path so its WHERE
// becomes a predicate tree when md is non-nil.
func buildLogicalPlanForInsertWithCatalog(
	ins antlrgen.IInsertStatementContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
) (logical.LogicalOperator, error) {
	if ins == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForInsert(ins), nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	op := buildLogicalPlanForInsert(ins)
	if op == nil {
		return op, nil
	}
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok || insertOp.Source == nil {
		// VALUES form (no Source) — nothing to upgrade.
		return op, nil
	}
	// Re-run the inner SELECT through the catalog-aware path. We
	// can't directly mutate the existing Source's filter without
	// re-walking the SELECT, so just rebuild Source.
	selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext)
	if !ok {
		return op, nil
	}
	body := selCtx.QueryExpressionBody()
	if body == nil {
		return op, nil
	}
	termDefault, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return op, nil
	}
	simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return op, nil
	}
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return op, nil
	}
	// Defensive: only swap Source when the catalog-aware build
	// produced a non-nil tree. Today buildLogicalPlanForSelectWithCatalog
	// can't return nil while buildLogicalPlanForSelect returned non-nil
	// (same ANTLR node, same extractFromSimpleTable contract), but
	// pinning the invariant in code instead of in the comment guards
	// against future divergence between the text and catalog paths.
	//
	// schemaName is the ACTIVE session schema, threaded so a schema-qualified
	// comma source in the INSERT … SELECT body (`INSERT INTO dst SELECT … FROM
	// PA AS main, main.PB AS B` in a session whose schema is `main`) classifies
	// main.PB as the schema-qualified TABLE against the active schema — the same
	// classification the top-level SELECT path performs. Hardcoding the default
	// would check it against `s`, leaving a LogicalUnnest the DML path's
	// resolveQualifiedTableNames cannot repair. RFC-142.
	//
	// A carried SQLSTATE from the SELECT-body build (RFC-142: an AT-on-a-table
	// comma source in the INSERT … SELECT FROM list → WRONG_OBJECT_TYPE) is
	// surfaced — not swallowed into the original (mis-classified unnest) source
	// the text path produced, which would later fail translation with a generic
	// "DML Cascades translation failed" instead of the faithful 42809.
	upgraded, selErr := buildLogicalPlanForSelectWithCatalog(sq, md, schemaName)
	if selErr != nil {
		return nil, selErr
	}
	if upgraded != nil {
		insertOp.Source = upgraded
	}
	wrapBareAggregateInsertSource(insertOp, sq)
	alignInsertSelectColumns(insertOp, md)
	return insertOp, nil
}

// wrapBareAggregateInsertSource fixes INSERT … SELECT … GROUP BY (RFC-084). A
// plain GROUP BY SELECT builds a bare LogicalAggregate with NO Project (standalone
// derives its schema from the physical plan), so its row datum is keyed by the
// aggregate's own canonical column names (e.g. "G", "SUM(V)"). buildInsertRecord
// maps by TARGET field name, finds none of them, and leaves every field unset —
// so each grouped row collapses to the same all-default record (unset PK) and the
// second group collides with the first → spurious 23505.
//
// Wrap the bare aggregate in the canonical post-aggregate Project (reusing the
// same buildPostAggregateProjection the standalone builders use): visible-only,
// canonical-named (matches the runtime datum key, not an alias), in SELECT order.
// alignInsertSelectColumns (called next) then sets the target column aliases
// positionally, so the projection re-keys the datum to the target names and
// buildInsertRecord finds every field. The Project-source case (findProjection
// non-nil) already aligns, so it's skipped here.
//
// Interim: RFC-079's builder unification moves this coercion into the Insert
// expression and DELETES this wrap (one query path), not a third parallel path.
func wrapBareAggregateInsertSource(insertOp *logical.LogicalInsert, sq *selectQuery) {
	if insertOp == nil || insertOp.Source == nil || sq == nil {
		return
	}
	if findProjection(insertOp.Source) != nil {
		return
	}
	agg := findAggregate(insertOp.Source)
	if agg == nil {
		return
	}
	// A QUALIFIED aggregate operand or group key (e.g. `SUM(s.v)`) is a known
	// SEPARATE defect on this insert-source path: the aggregate's operand is left
	// unresolved (nil), so the aggregate computes NULL. Wrapping would align the
	// (NULL) column and SILENTLY insert NULL; NOT wrapping leaves the original LOUD
	// failure (unset PK → 23505). Until the qualified-operand resolution is fixed
	// (follow-up), skip — a qualifier shows up as a '.' in the canonical
	// aggregate/group-key name.
	for _, a := range agg.Aggregates {
		if strings.Contains(a, ".") {
			return
		}
	}
	for _, k := range agg.GroupKeys {
		if strings.Contains(k, ".") {
			return
		}
	}
	// A sole `SELECT COUNT(*)` is tracked as sq.countStar (NOT an aggCol — the
	// parser sets the flag only when COUNT(*) is the single SELECT element), so it
	// is invisible to buildPostAggregateProjection. Synthesize its column (in
	// SELECT position, i.e. before any HAVING-only aggCols) so the bare COUNT(*)
	// insert is aligned too — else its row keys on "COUNT(*)" and buildInsertRecord
	// leaves the target unset (silently wrong, or a 23505 under GROUP BY).
	aggCols := sq.aggCols
	if sq.countStar {
		cs := aggSelectCol{aggFunc: "COUNT", aggArg: "*", outName: sq.countStarAlias, visible: true}
		aggCols = append([]aggSelectCol{cs}, aggCols...)
	}
	// Identity strip — matches the stripPrefix="" buildLogicalPlanForSelectWithCatalog
	// used to name this base-table aggregate's columns, so the Project's canonical
	// names match the runtime datum keys. (Qualified operands, where the runtime
	// key diverges from this naming, are filtered out above.)
	strip := func(s string) string { return s }
	proj, _ := buildPostAggregateProjection(insertOp.Source, aggCols, strip)
	if proj == nil {
		return
	}
	// A bare aggregate carries no post-aggregate antlr expressions, so
	// upgradeProjectionValues never fills these slots — fill them with canonical
	// FieldValue references (upper-cased to match the runtime upper-cased datum keys).
	proj.ProjectedValues = make([]values.Value, len(proj.Projections))
	for i, name := range proj.Projections {
		proj.ProjectedValues[i] = &values.FieldValue{Field: strings.ToUpper(name), Typ: values.UnknownType}
	}
	insertOp.Source = proj
}

// alignInsertSelectColumns sets the SELECT projection's output aliases to
// the INSERT target columns positionally. INSERT … SELECT is positional —
// the SELECT's i-th output feeds the target's i-th column regardless of
// the SELECT output's own name (e.g. `INSERT INTO t(id,total) SELECT id,
// price*qty`) — so the projected row datum ends up keyed by the target
// column names and executeInsert can build the target record by name.
func alignInsertSelectColumns(insertOp *logical.LogicalInsert, md *recordlayer.RecordMetaData) {
	proj := findProjection(insertOp.Source)
	if proj == nil || len(proj.Projections) == 0 {
		return
	}
	targetCols := insertOp.Columns
	if len(targetCols) == 0 {
		rt := md.GetRecordType(bareTableName(insertOp.Table))
		if rt == nil {
			return
		}
		fields := rt.Descriptor.Fields()
		targetCols = make([]string, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			targetCols[i] = string(fields.Get(i).Name())
		}
	}
	if proj.Aliases == nil {
		proj.Aliases = make([]string, len(proj.Projections))
	}
	for i := 0; i < len(proj.Projections) && i < len(targetCols); i++ {
		proj.Aliases[i] = targetCols[i]
	}
}

// protoKindToValueType maps a proto field kind to the cascades values.Type used
// for INSERT promotion checks. Nullability is irrelevant to IsPromotable, so the
// nullable singletons are returned. Returns nil for kinds outside the numeric
// promotion core; the caller skips those (the runtime converter handles them).
func protoKindToValueType(k protoreflect.Kind) values.Type {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return values.NullableInt
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return values.NullableLong
	case protoreflect.FloatKind:
		return values.NullableFloat
	case protoreflect.DoubleKind:
		return values.NullableDouble
	}
	return nil
}

// checkInsertSelectPromotable rejects an INSERT … SELECT whose projected
// AGGREGATE-result column cannot be promoted to its target column type — the
// plan-time, lattice-driven analogue of Java's PromoteValue assignability check.
// AVG(BIGINT) types DOUBLE (AggregateValue.Type()); DOUBLE→BIGINT has no edge in
// the promotion lattice, so the INSERT is rejected with SQLSTATE 22000 exactly
// like Java — and, because the verdict is purely IsPromotable over the
// structurally-derived type, independent of whether the source produces any rows
// (the empty-source axis).
//
// A projected aggregate appears in one of two shapes:
//   - a COMPUTED expression that CONTAINS an aggregate (e.g. AVG(v)+1) —
//     flagged by LogicalProject.AggregateSlots (provenance, captured pre-rewrite)
//     and reliably typed via the value's Type() (the aggregate reference carries
//     its result type, B′; ArithmeticValue propagates it). Provenance, NOT
//     type-presence: plain columns are concrete-typed too (ResolveIdentifier).
//   - a BARE aggregate (e.g. SELECT AVG(v)) — the projection slot carries a nil
//     ProjectedValue (the executor resolves it from the aggregate's output by
//     name), so its type comes from the producing LogicalAggregate, looked up by
//     the slot's canonical name.
//
// Plain-column narrowing (LONG→INT, DOUBLE-col→INT) is NOT checked here — it
// stays deferred to the runtime converter, pending the Java end-state
// (PromoteValue projection nodes) that dissolves this guard. INSERT … SELECT
// with an explicit column list is rejected upstream, so the projection maps
// positionally onto the target record's fields.
//
// Scope: covers sources with a LogicalProject (scalar aggregates, expressions
// over aggregates, plain projections). A bare GROUP BY whose Source is a
// LogicalAggregate DIRECTLY (no Project, e.g. INSERT … SELECT g, AVG(v) … GROUP
// BY g) has no projection to read column order from, so it is deferred to the
// PromoteValue follow-up (tracked in TODO.md) — its runtime converter still
// rejects the non-empty case.
func checkInsertSelectPromotable(insertOp *logical.LogicalInsert, md *recordlayer.RecordMetaData) error {
	proj := findProjection(insertOp.Source)
	if proj == nil {
		return nil
	}
	rt := md.GetRecordType(bareTableName(insertOp.Table))
	if rt == nil {
		return nil
	}
	// Canonical aggregate output name → reliable result type, for the bare-
	// aggregate case (nil ProjectedValue). Names match the projection text
	// verbatim (both produced by the same canonicaliser, e.g. "AVG(V)").
	aggTypes := map[string]values.Type{}
	if agg := findAggregate(insertOp.Source); agg != nil {
		for j, name := range agg.Aggregates {
			var operand values.Value
			if j < len(agg.AggregateOperands) {
				operand = agg.AggregateOperands[j]
			}
			if t := aggResultTypeFromName(name, operand); t != nil {
				aggTypes[strings.ToUpper(name)] = t
			}
		}
	}
	fields := rt.Descriptor.Fields()
	for i := 0; i < len(proj.Projections) && i < fields.Len(); i++ {
		var srcType values.Type
		if i < len(proj.AggregateSlots) && proj.AggregateSlots[i] &&
			i < len(proj.ProjectedValues) && proj.ProjectedValues[i] != nil {
			srcType = proj.ProjectedValues[i].Type()
		} else if t, ok := aggTypes[strings.ToUpper(proj.Projections[i])]; ok {
			srcType = t
		}
		if srcType == nil || srcType.Code() == values.TypeCodeUnknown {
			continue
		}
		targetType := protoKindToValueType(fields.Get(i).Kind())
		if targetType == nil {
			continue
		}
		if !values.IsPromotable(srcType, targetType) {
			return api.NewErrorf(api.ErrCodeCannotConvertType,
				"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.")
		}
	}
	return nil
}

// aggResultTypeFromName derives an aggregate's result type from its canonical
// output name (e.g. "AVG(V)", "SUM(PRICE)") and its resolved operand — the
// single source for the bare-aggregate projection case, where no ProjectedValue
// is present. AVG→DOUBLE and COUNT→LONG are function-determined; SUM/MIN/MAX
// inherit the operand type. The function prefix is read off the *internal*
// canonical name (the contract the executor's aggResultName also relies on), not
// user SQL text. Mirrors AggregateValue.Type() / Java's per-operator resultTypeCode
// — keep the two in sync until the PromoteValue follow-up (RFC-083) dissolves this
// function (it exists only for the nil-ProjectedValue bare-aggregate path).
func aggResultTypeFromName(name string, operand values.Value) values.Type {
	sym := name
	if idx := strings.IndexByte(name, '('); idx >= 0 {
		sym = name[:idx]
	}
	switch strings.ToUpper(strings.TrimSpace(sym)) {
	case "AVG":
		return values.NullableDouble
	case "COUNT":
		return values.NotNullLong
	case "SUM", "MIN", "MAX":
		if operand != nil {
			if t := operand.Type(); t != nil && t.Code() != values.TypeCodeUnknown {
				return t
			}
		}
		return values.NullableLong
	}
	return nil
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
	schemaName string,
	outerCTEScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	// Only short-circuit to the schema-less WithCatalog variant when the ACTIVE
	// schema IS the default — that variant hardcodes defaultEmbeddedSchema for the
	// schema-qualified-table demotion. For a NON-default session schema (e.g.
	// `main`), stay on this path so the threaded schemaName reaches
	// buildLogicalPlanForSelectWithCTECatalog's demotion/normalization (a
	// `main.PB`-in-a-subquery source resolves against the active schema, not `s`).
	// The own-CTE pre-scan below runs identically with an empty outer-scope map.
	// RFC-142 (P2b).
	if len(outerCTEScopes) == 0 && schemaName == defaultEmbeddedSchema {
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

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, schemaName, cteScopes)
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
			body, err = buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes)
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
	// This is the TOP-LEVEL (no external scope) variant — reached only from the
	// EXPLAIN-only generators and the WithCTECatalog default-schema short-circuit,
	// so it uses the default schema for the schema-qualified-table demotion. A
	// non-default session schema flows through the WithCTECatalog path instead.
	schemaName := defaultEmbeddedSchema
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

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, schemaName, cteScopes)
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
			body, err = buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes)
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
		// A parenthesized query operand — `(SELECT … LIMIT n)` as a UNION
		// branch — surfaces as a ParenthesisQueryContext. Recurse into the
		// inner query body so the branch's own clauses (notably LIMIT) are
		// built and not silently dropped (RFC-128 §4.7). Without this a
		// parenthesized branch fell through to nil here.
		if paren, ok := b.QueryTerm().(*antlrgen.ParenthesisQueryContext); ok {
			if inner := paren.Query(); inner != nil {
				return buildLogicalPlanForQueryBodyWithCatalog(inner.QueryExpressionBody(), md)
			}
			return nil, nil
		}
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
		return buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
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
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	// As in buildLogicalPlanForQueryWithCTECatalog: only short-circuit to the
	// schema-less variant when the active schema IS the default; a non-default
	// session schema must keep threading so the demotion uses the active schema.
	if len(cteScopes) == 0 && schemaName == defaultEmbeddedSchema {
		return buildLogicalPlanForQueryBodyWithCatalog(body, md)
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		// Parenthesized UNION branch — recurse into the inner query body so
		// the branch's LIMIT/clauses survive (RFC-128 §4.7); see the
		// non-CTE variant above.
		if paren, ok := b.QueryTerm().(*antlrgen.ParenthesisQueryContext); ok {
			if inner := paren.Query(); inner != nil {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes)
			}
			return nil, nil
		}
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
		return buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, cteScopes)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCTECatalog(b, md, schemaName, cteScopes, false)
	}
	return nil, nil
}

func buildLogicalPlanForUnionWithCTECatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	allowDistinct bool,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	distinct := false
	if setQ.ALL() == nil {
		if !allowDistinct {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "only UNION ALL is supported")
		}
		distinct = true
	}
	left, err := buildLogicalPlanForQueryBodyWithCTECatalog(setQ.GetLeft(), md, schemaName, cteScopes)
	if err != nil {
		return nil, err
	}

	// The grammar attaches a trailing ORDER BY / LIMIT / OFFSET to
	// the rightmost simpleTable. For a UNION, those clauses apply to
	// the combined result (SQL standard), NOT to the right branch
	// alone. Strip them from the right branch before building (so
	// column validation doesn't reject LEFT-branch column names
	// against the right table) and lift them to wrap the whole UNION.
	var lifted unionLiftedClauses
	var right logical.LogicalOperator
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, schemaName, cteScopes)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	// Legacy fallback: if the right branch's sort wasn't stripped at
	// the selectQuery level (e.g. nested UNION), peel it off the
	// logical plan tree.
	if len(lifted.sortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			lifted.sortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				lifted.sortKeys = s.Keys
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
	if len(lifted.sortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: lifted.sortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0]); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, distinct)
	if len(lifted.sortKeys) > 0 {
		result = logical.NewSort(result, lifted.sortKeys)
	}
	if lifted.limit >= 0 || lifted.offset > 0 {
		result = logical.NewLimit(result, lifted.limit, lifted.offset)
	}
	return result, nil
}

// unionLiftedClauses holds ORDER BY / LIMIT / OFFSET stripped from a
// UNION's right branch so the caller can re-attach them to the
// combined result.
type unionLiftedClauses struct {
	sortKeys []logical.SortKey
	limit    int64 // <0 means no limit
	offset   int64
}

// buildUnionRightBranchStrippingOrderBy builds the right branch of a
// UNION, stripping any trailing ORDER BY and LIMIT/OFFSET from the
// simpleTable before building the logical plan. Returns the built
// plan and the stripped clauses (empty if none). For non-simpleTable
// right branches (e.g. nested UNION), falls through to the normal
// builder and returns empty clauses.
func buildUnionRightBranchStrippingOrderBy(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, unionLiftedClauses, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	qtd, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, schemaName, cteScopes)
		return op, unionLiftedClauses{limit: -1}, err
	}
	simpleTable, ok := qtd.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, schemaName, cteScopes)
		return op, unionLiftedClauses{limit: -1}, err
	}
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return nil, unionLiftedClauses{limit: -1}, err
	}

	var lifted unionLiftedClauses
	lifted.limit = -1

	// Save and strip ORDER BY.
	if len(sq.orderBy) > 0 {
		for _, ob := range sq.orderBy {
			e := ob.colName
			if e == "" && ob.rawExpr != nil {
				e = canonicalTextOf(ob.rawExpr)
			}
			dir := logical.SortAsc
			if !ob.ascending {
				dir = logical.SortDesc
			}
			nullsFirst := ob.ascending
			if ob.nullsFirst != nil {
				nullsFirst = *ob.nullsFirst
			}
			lifted.sortKeys = append(lifted.sortKeys, logical.SortKey{Expr: e, Dir: dir, NullsFirst: nullsFirst})
		}
		sq.orderBy = nil
	}

	// Save and strip LIMIT/OFFSET. This is the rightmost simpleTable of an
	// UNPARENTHESIZED union (e.g. `… UNION ALL SELECT … ORDER BY id LIMIT n`),
	// whose trailing ORDER BY/LIMIT applies to the COMBINED result, NOT the
	// right branch alone (SQL standard). extractFromSimpleTable now populates
	// sq.limit/sq.offset, so we lift those and RESET them on the branch to
	// avoid double-applying the clause to the right branch (RFC-128). A
	// parenthesized right branch never reaches here — it is a
	// ParenthesisQueryContext, handled by the !ok path above, which keeps the
	// branch's own LIMIT inside.
	if sq.limit >= 0 || sq.offset > 0 {
		lifted.limit = sq.limit
		lifted.offset = sq.offset
		sq.limit = -1
		sq.offset = 0
	}

	if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
		return nil, lifted, api.NewError(api.ErrCodeUndefinedFunction, "Unsupported operator "+fn)
	}
	if err := validateQualifiedStarSources(sq, md); err != nil {
		return nil, lifted, err
	}
	op, err := buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, cteScopes)
	if err != nil {
		return nil, lifted, err
	}
	return op, lifted, nil
}

// upgradeSortKeyValues walks the logical plan's LogicalSort and resolves
// sort key expressions through the expression walker. When an ORDER BY
// key is an aggregate expression (SUM(v)*2, COALESCE(SUM(v),0)), the
// walker produces a Value tree with AggregateValues rewritten to
// FieldValues referencing the aggregate output.
func upgradeSortKeyValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) {
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
			// The sort sits ABOVE the aggregate, so the group-key sort key must read
			// the AGGREGATE OUTPUT column name — what the executor (aggKeyName /
			// aggregateCursor.finalizeGroup) keys the group-key column by: a FieldValue
			// group key flows under its bare Field NAME (`V`), NOT its qualified explain
			// (`V.V`). A qualified group key value arises from a lateral-unnest SHADOWING
			// group key (`FROM t, t.arr AS V, u GROUP BY V`, resolved to FieldValue(QOV(V),
			// V) by upgradeAggregateOperands) — using the raw explain `V.V` here would key
			// the sort by a column the aggregate output does not carry → a no-op sort
			// (ORDER BY DESC silently ignored, P2b). Mirror aggKeyName:
			// the field name for a FieldValue, the explain for a computed key (whose
			// output column IS its explain). RFC-142.
			groupKeyExplainMap[strings.ToUpper(agg.GroupKeys[i])] = aggregateGroupKeyOutputName(gkv)
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

	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		// A lateral array unnest (`FROM t, t.arr AS v`) makes
		// buildProjectionResolverWithCTEScopes return nil: it tries to resolve the
		// dotted unnest source (`t.arr`) as a TABLE and fails, never registering the
		// unnest's AS/AT virtual columns. buildSelectScope is the single scope
		// builder that knows the unnest virtual source (unnestScopeSourceAdder), so
		// a COMPUTED ORDER BY over an unnest column (`ORDER BY v + 0 DESC`) can only
		// resolve there. Fall back to it; without this the sort key stays raw text
		// and the executor compares a non-existent field → a silent no-op sort.
		// RFC-142 (P2a).
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
		if resolver == nil {
			return
		}
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
		// A bare unnest sort key (`ORDER BY v`) resolves through the unnest's
		// Shadowing scope source to a qualified FieldValue over the unnest
		// correlation, which the P2a path already qualifies via
		// qualifyShadowedSortKeys. A COMPUTED key (`v + 0`) wraps that FieldValue in
		// an arithmetic Value; the qualification is intrinsic to the resolved tree
		// (the FieldValue carries its Child correlation), so the executor's ValueExpr
		// evaluates the qualified reference per row and the sort sorts for real.
		sort.Keys[i].Value = v
	}
}

// aggregateGroupKeyOutputName returns the OUTPUT column name a group-key Value is
// keyed by in the aggregate's result row — the exact mirror of the executor's
// aggKeyName (executor.go): a FieldValue group key flows under its bare Field
// name (`V`), every other (computed) group key under its ExplainValue. The
// uppercase form matches the key the executor writes and the sort cursor reads.
// Load-bearing for a lateral-unnest SHADOWING group key, whose resolved Value is
// a QUALIFIED FieldValue(QOV(V), V): its bare field name `V` (not the explain
// `V.V`) is the aggregate output column. RFC-142.
func aggregateGroupKeyOutputName(gkv values.Value) string {
	if fv, ok := gkv.(*values.FieldValue); ok {
		return strings.ToUpper(fv.Field)
	}
	return strings.ToUpper(values.ExplainValue(gkv))
}

// rebasePostAggregateGroupKeyValue rewrites, inside a POST-aggregate value tree,
// every reference to a QUALIFIED grouped-unnest group key (e.g. FieldValue(QOV(V),
// V), explain `V.V`) down to the BARE aggregate-OUTPUT name the cursor keys the
// group-key column by (aggregateGroupKeyOutputName → `V`). This is the unifying
// twin of the ORDER-BY rebase (which rebuilt the sort key Value from the
// same aggregateGroupKeyOutputName): the sort sits ABOVE the aggregate, and so do
// the SELECT projection (computed `V + 1`) and HAVING — every post-aggregate
// consumer that references a grouped key must read the aggregate OUTPUT column,
// NOT the qualified PRE-aggregate value.
//
// Why qualified: the GROUP-BY-shadowing fix stores the QUALIFIED
// FieldValue(QOV(V), V) in GroupKeyValues so grouping is on the unnest ELEMENT
// (not a later same-named column merged last-leg-wins). The aggregate cursor
// outputs that key under aggKeyName = the bare field `V` (executor.go); a
// post-aggregate reference resolved against the PRE-aggregate FROM scope is the
// qualified `V.V`, which reads the MISSING `V.V` key off the bare-V aggregate row
// → NULL (silent-wrong computed projection / dropped HAVING
// groups). Only a QUALIFIED group key (a FieldValue carrying a Child correlation)
// needs the rewrite; a bare group key already keys under its own name, so its
// references read the aggregate output as-is and the structural match is a no-op.
// RFC-142.
func rebasePostAggregateGroupKeyValue(v values.Value, agg *logical.LogicalAggregate) values.Value {
	if v == nil || agg == nil || len(agg.GroupKeyValues) == 0 {
		return v
	}
	return values.MapFieldValues(v, func(fv *values.FieldValue) values.Value {
		for _, gkv := range agg.GroupKeyValues {
			qfv, ok := gkv.(*values.FieldValue)
			if !ok || qfv.Child == nil {
				continue // only qualified group keys carry the V.V mismatch
			}
			if values.ValuesStructurallyEqual(fv, qfv) {
				return &values.FieldValue{
					Field: aggregateGroupKeyOutputName(qfv),
					Typ:   fv.Typ,
				}
			}
		}
		return fv
	})
}

// rebaseHavingGroupKeyPredicate rebases a HAVING predicate's grouped-unnest
// group-key references to the bare aggregate-OUTPUT name — but ONLY when the
// predicate will STAY ABOVE the aggregate. A HAVING that references an aggregate
// (e.g. `V > 0 AND COUNT(*) > 1`, or `COUNT(*) > 1`) cannot be pushed below the
// GroupBy, so it evaluates against the aggregate OUTPUT row (bare `V`), and a
// qualified `V.V` reference there reads the MISSING key → NULL.
//
// A PURE group-key HAVING (`V > 1`, a single ComparisonPredicate on a group key)
// is pushed BELOW the aggregate by PushFilterThroughGroupByRule, where it
// evaluates against the PRE-aggregate row and MUST keep the qualified `V.V`
// binding (the unnest element); rebasing it to the bare `V` there would read a
// LATER same-named column merged last-leg-wins (the shadowing trap). So
// the decision mirrors the push-down rule's predicateReferencesOnlyKeys EXACTLY:
// a top-level pushable group-key comparison is left untouched (it pushes down
// qualified); everything else is rebased (it stays above, reads bare). The two
// deciders cannot drift — both ask "is this a single group-key comparison?".
// RFC-142.
func rebaseHavingGroupKeyPredicate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) predicates.QueryPredicate {
	if pred == nil || agg == nil || len(agg.GroupKeyValues) == 0 {
		return pred
	}
	if havingPredicatePushesBelowAggregate(pred, agg) {
		return pred // stays qualified; PushFilterThroughGroupByRule pushes it pre-aggregate
	}
	return rebasePostAggregateGroupKeyPredicate(pred, agg)
}

// havingPredicatePushesBelowAggregate mirrors the Cascades
// PushFilterThroughGroupByRule.predicateReferencesOnlyKeys decision: a single
// ComparisonPredicate whose operand is a FieldValue naming a grouping key (by its
// bare Field, the key the rule's group-key set is built on) is pushed below the
// GroupBy. Anything else (an aggregate reference, an AND/OR/NOT compound, a
// constant) stays above. The HAVING predicate is handed to the translator as ONE
// list entry, so the rule never splits a compound — the decision is binary at the
// top level. RFC-142.
func havingPredicatePushesBelowAggregate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) bool {
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		return false
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok {
		return false
	}
	for _, gkv := range agg.GroupKeyValues {
		if gfv, ok := gkv.(*values.FieldValue); ok && strings.EqualFold(gfv.Field, fv.Field) {
			return true
		}
	}
	return false
}

// rebasePostAggregateGroupKeyPredicate applies rebasePostAggregateGroupKeyValue to
// every Value operand of a post-aggregate predicate tree. Mirrors
// rewriteAggregateRefsInPredicate's tree walk so a grouped-unnest group-key
// reference reads the bare aggregate-output column, not the qualified pre-aggregate
// `V.V`. RFC-142.
func rebasePostAggregateGroupKeyPredicate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) predicates.QueryPredicate {
	if pred == nil || agg == nil || len(agg.GroupKeyValues) == 0 {
		return pred
	}
	switch p := pred.(type) {
	case *predicates.ComparisonPredicate:
		lhs := rebasePostAggregateGroupKeyValue(p.Operand, agg)
		// Copy the whole Comparison and replace ONLY the rebased RHS operand,
		// preserving Escape (the LIKE escape rune, e.g. `LIKE 'a!_%' ESCAPE '!'`)
		// and every other Comparison subclass field (ParameterName, the Text*
		// fields, the DistanceRank vector fields). Reconstructing a fresh
		// {Type, Operand} would silently drop them and change the comparison's
		// semantics — a LIKE pattern would evaluate with the wrong wildcard
		// meaning once its escape is lost. RFC-142.
		cmp := p.Comparison
		cmp.Operand = rebasePostAggregateGroupKeyValue(p.Comparison.Operand, agg)
		return predicates.NewComparisonPredicate(lhs, cmp)
	case *predicates.AndPredicate:
		subs := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, s := range p.SubPredicates {
			subs[i] = rebasePostAggregateGroupKeyPredicate(s, agg)
		}
		return predicates.NewAnd(subs...)
	case *predicates.OrPredicate:
		subs := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, s := range p.SubPredicates {
			subs[i] = rebasePostAggregateGroupKeyPredicate(s, agg)
		}
		return predicates.NewOr(subs...)
	case *predicates.NotPredicate:
		return predicates.NewNot(rebasePostAggregateGroupKeyPredicate(p.Child, agg))
	}
	return pred
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
//
// A `<qualifier>.*` over a lateral array unnest alias (`SELECT V.* FROM t,
// t.arr AS V`) is expanded to the unnest's element column(s) (and the ordinal
// under WITH ORDINALITY) via the SHARED unnest virtual source — NOT only real
// record types. Without this the star qualifier resolves to nothing and the
// query degrades to an UNQUALIFIED star → returns the ENTIRE FlatMap row (outer
// columns included) instead of just the unnest source's columns (silent-wrong).
// RFC-142.
func expandQualifiedStars(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) {
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
	resolvesToTable := newUnnestTableResolver(md, schemaName)
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
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if src, ok := unnestVirtualScopeSource(j); ok {
				cols := make([]string, 0, len(src.Table.Columns()))
				for _, c := range src.Table.Columns() {
					cols = append(cols, strings.ToUpper(c.Id.Name()))
				}
				sourceColumns[strings.ToUpper(src.CorrelationName)] = cols
			}
			continue
		}
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
// A lone `V.*` over a lateral array unnest alias (`SELECT V.* FROM t, t.arr AS
// V`) expands to the unnest's element column(s) (and the ordinal under WITH
// ORDINALITY) via the SHARED unnest virtual source — NOT only real record types.
// Without this the qualifier resolves to nothing and the query falls through to
// the nil-projCols path → returns the ENTIRE FlatMap row instead of just the
// unnest source's columns (silent-wrong). RFC-142.
func expandProjQualifier(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) {
	if sq == nil || md == nil || sq.projQualifier == "" {
		return
	}
	qual := sq.projQualifier

	// A lateral-unnest qualifier expands to the unnest's element/ordinal columns
	// (shared virtual source) before the real-table resolution below.
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if !isLateralUnnestJoin(j, visible, resolvesToTable) {
			continue
		}
		src, ok := unnestVirtualScopeSource(j)
		if !ok || !strings.EqualFold(src.CorrelationName, qual) {
			continue
		}
		srcCols := src.Table.Columns()
		cols := make([]string, len(srcCols))
		for k, c := range srcCols {
			cols[k] = qual + "." + strings.ToUpper(c.Id.Name())
		}
		sq.projCols = cols
		sq.projAliases = make([]string, len(srcCols))
		sq.projExprs = make([]antlrgen.IExpressionContext, len(srcCols))
		sq.projStarQualifiers = make([]string, len(srcCols))
		sq.projQualifier = ""
		return
	}

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
	leftNames := make(map[string]bool, len(leftProj.Projections)*2)
	for i, col := range leftProj.Projections {
		leftNames[strings.ToUpper(col)] = true
		leftNames[strings.ToUpper(parseColRef(col).bare())] = true
		if i < len(leftProj.Aliases) && leftProj.Aliases[i] != "" {
			leftNames[strings.ToUpper(leftProj.Aliases[i])] = true
		}
	}
	for _, k := range sort.Keys {
		if k.Expr == "" {
			continue
		}
		upper := strings.ToUpper(k.Expr)
		if !leftNames[upper] && !leftNames[strings.ToUpper(parseColRef(k.Expr).bare())] {
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
		bare := parseColRef(col).bare()
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
//
// Trailing ORDER BY: the ANTLR grammar greedily attaches a trailing
// ORDER BY to the rightmost SimpleTable, but SQL standard says it
// applies to the whole UNION result. Mirror the lift in execUnion
// (union.go): strip ORDER BY from the right branch's selectQuery
// before building it, then wrap the final LogicalUnion in a
// LogicalSort using the lifted keys.
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

	// Same ORDER BY / LIMIT stripping as the CTE-catalog variant.
	var lifted unionLiftedClauses
	var right logical.LogicalOperator
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, defaultEmbeddedSchema, nil)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	if len(lifted.sortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			lifted.sortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				lifted.sortKeys = s.Keys
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
	if len(lifted.sortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: lifted.sortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0]); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, false)
	if len(lifted.sortKeys) > 0 {
		result = logical.NewSort(result, lifted.sortKeys)
	}
	if lifted.limit >= 0 || lifted.offset > 0 {
		result = logical.NewLimit(result, lifted.limit, lifted.offset)
	}
	return result, nil
}

// existsSubqueryPlanner implements expr.SubqueryPlanner. It builds
// logical plans for EXISTS and scalar subqueries and collects the
// (alias, plan) pairs that the LogicalFilter/LogicalProject need to
// carry to the Cascades translator.
func buildOuterScopeSources(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) map[string]semantic.ScopeSource {
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
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	for i, j := range sq.joins {
		// A lateral array unnest leg (`FROM t, t.arr AS x [AT ord]`) is NOT a real
		// table; register its VIRTUAL Shadowing source (the SAME one the SELECT scope
		// uses, via unnestVirtualScopeSource) so a CORRELATED subquery referencing the
		// unnested element/ordinal resolves it. Without this the inner EXISTS / scalar
		// subquery's outer scope sees only the REAL tables and the correlated
		// reference (`WHERE U.V = VAL`) fails → a generic Cascades translation failure
		// (P2c). The existing EXISTS-over-unnest lowering binds it at
		// execution. RFC-142.
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if src, ok := unnestVirtualScopeSource(j); ok {
				sources[strings.ToUpper(src.CorrelationName)] = src
			}
			continue
		}
		addSrc(j.tableName, j.alias)
	}
	return sources
}

type existsSubqueryPlanner struct {
	md *recordlayer.RecordMetaData
	// schemaName is the ACTIVE session schema. EXISTS / scalar subquery plans are
	// built through buildLogicalPlanForQueryWithCTECatalog, which threads this into
	// the schema-qualified-table demotion (demoteSchemaQualifiedUnnest /
	// normalizeSchemaQualifiedSelectSources): a `… EXISTS (SELECT 1 FROM PA AS main,
	// main.PB AS B)` in a session whose schema is `main` resolves `main.PB` as the
	// schema-qualified TABLE against the ACTIVE schema, not the hardcoded default
	// `s`. Empty falls back to defaultEmbeddedSchema. RFC-142 (P2b).
	schemaName                 string
	outerScopes                map[string]semantic.ScopeSource
	cteScopes                  map[string]semantic.ScopeSource
	cteBodies                  map[string]logical.LogicalOperator // CTE name → body plan, for wrapping scalar subquery plans
	subqueries                 []logical.ExistsSubquery
	scalarSubqueries           []logical.ScalarSubquery
	correlatedScalarSubqueries []logical.CorrelatedScalarSubquery
	lastJoinPredicate          predicates.QueryPredicate
}

func (p *existsSubqueryPlanner) BuildExists(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: nil query context")
	}
	innerOp, err := buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.schemaName, p.cteScopes)
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

// correlatedSubqueryJoinRight builds the right child for a comma/JOIN FROM leg of
// a correlated EXISTS / scalar subquery whose inner FROM clause is rebuilt here
// (the fallback paths buildCorrelatedExists / buildCorrelatedScalar). It reuses
// the EXACT lateral-unnest classification the main FROM path uses
// (lateralUnnestCandidate over visibleFromAliases + newUnnestTableResolver): a
// `t.arr AS x [AT ord]` comma source resolves to a LogicalUnnest so the Cascades
// translator lowers it to FlatMap(Scan, Explode), instead of mis-scanning
// `t.arr` as a table name. Anything that is not a lateral-unnest candidate stays
// a plain table scan. RFC-142.
func (p *existsSubqueryPlanner) correlatedSubqueryJoinRight(j joinClause, primaryTable, primaryAlias string, priorJoins []joinClause) logical.LogicalOperator {
	resolvesToTable := newUnnestTableResolver(p.md, p.effectiveSchemaName())
	visible := visibleFromAliases(primaryTable, primaryAlias, priorJoins, resolvesToTable)
	if u := lateralUnnestCandidate(j, visible, resolvesToTable); u != nil {
		return u
	}
	return logical.NewScan(j.tableName, j.alias)
}

// addCorrelatedJoinScopeSource registers the inner-scope source for a comma/JOIN
// FROM leg of a correlated subquery so the inner WHERE / ON resolves its columns.
// A lateral-unnest leg registers the SAME virtual Shadowing source the main path
// uses (unnestVirtualScopeSource) — exposing the element/ordinal binding — rather
// than resolving `t.arr` as a table. A plain table leg resolves the table from
// metadata as before. Mirrors the main path's scope binding
// (unnestScopeSourceAdder / isLateralUnnestJoin). RFC-142.
func (p *existsSubqueryPlanner) addCorrelatedJoinScopeSource(innerScope *semantic.Scope, analyzer *semantic.Analyzer, j joinClause, primaryTable, primaryAlias string, priorJoins []joinClause) error {
	resolvesToTable := newUnnestTableResolver(p.md, p.effectiveSchemaName())
	visible := visibleFromAliases(primaryTable, primaryAlias, priorJoins, resolvesToTable)
	if isLateralUnnestJoin(j, visible, resolvesToTable) {
		if src, ok := unnestVirtualScopeSource(j); ok {
			_ = innerScope.AddSource(src)
		}
		return nil
	}
	jAlias := j.alias
	if jAlias == "" {
		jAlias = j.tableName
	}
	jTbl, jErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(j.tableName, "."), false))
	if jErr != nil {
		return jErr
	}
	jAliasID := semantic.NewUnquoted(jAlias)
	_ = innerScope.AddSource(semantic.ScopeSource{
		Table: jTbl, Alias: jAliasID, CorrelationName: jAliasID.Name(),
	})
	return nil
}

// effectiveSchemaName is the planner's active session schema, falling back to
// defaultEmbeddedSchema when unset — matching how buildLogicalPlanForQueryWithCTECatalog
// resolves p.schemaName so the unnest table resolver classifies a
// schema-qualified table source against the same schema. RFC-142.
func (p *existsSubqueryPlanner) effectiveSchemaName() string {
	if p.schemaName == "" {
		return defaultEmbeddedSchema
	}
	return p.schemaName
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

	// Strip the session-schema qualifier off a schema-qualified table source
	// (`s.PB` → `PB` when `s` is the active schema and PB exists) BEFORE building
	// the scan/join tree and resolving the join sources. The normal catalog-aware
	// SELECT path runs the same pass (buildLogicalPlanForSelectWithCTECatalog), but
	// this correlated fallback rebuilds the inner FROM clause itself and would hand
	// the raw `s.PB` straight to Analyzer.ResolveTable (which does NOT strip a
	// schema qualifier) → `table not found: S.PB`, rejecting a valid correlated
	// subquery. Java's generateAccess resolves the table first at every FROM-source
	// point; this matches it. A dotted reference whose qualifier is a prior FROM
	// alias (a genuine lateral unnest) is NOT a schema-qualified-table pair, so its
	// segments survive for the unnest classifier. RFC-142.
	normalizeSchemaQualifiedSelectSources(sq, p.effectiveSchemaName(), p.md)

	innerAlias := sq.tableAlias
	if innerAlias == "" {
		innerAlias = sq.tableName
	}
	op := logical.LogicalOperator(logical.NewScan(sq.tableName, innerAlias))

	// Build join tree from inner FROM clause (handles multi-table EXISTS).
	// A `t.arr AS x [AT ord]` comma source is a lateral array unnest, not a
	// table — classify it via the SAME helper the main FROM path uses so the
	// Cascades translator lowers it to FlatMap(Scan, Explode). RFC-142.
	for i, j := range sq.joins {
		right := p.correlatedSubqueryJoinRight(j, sq.tableName, innerAlias, sq.joins[:i])
		var kind logical.JoinKind
		switch j.joinType {
		case joinTypeLeft:
			kind = logical.JoinLeft
		case joinTypeRight:
			kind = logical.JoinRight
		case joinTypeFull:
			kind = logical.JoinFull
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

	// Add join sources to scope so the resolver can resolve their columns. A
	// lateral-unnest leg registers the same virtual Shadowing source the main
	// path uses (exposing the element/ordinal binding) instead of resolving
	// `t.arr` as a table — so the inner WHERE on the unnest column resolves and
	// a correlated reference to the outer query binds. RFC-142.
	for i, j := range sq.joins {
		if jErr := p.addCorrelatedJoinScopeSource(innerScope, analyzer, j, sq.tableName, innerAlias, sq.joins[:i]); jErr != nil {
			return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: resolve join table %q: %v", j.tableName, jErr), Cause: jErr}
		}
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
		schemaName:  p.schemaName,
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

// qualifyBareFieldValue mutates FieldValue nodes in place, setting
// Child to a QOV. Safe because buildCorrelatedExists constructs a
// fresh predicate tree via resolver.WalkPredicate for each call —
// these FieldValues are never shared or memoized.
func qualifyBareFieldValue(v values.Value, qualifier string) {
	corr := values.NamedCorrelationIdentifier(qualifier)
	values.WalkValue(v, func(node values.Value) bool {
		if fv, ok := node.(*values.FieldValue); ok {
			if fv.Child != nil {
				return false
			}
			ref := parseColRef(fv.Field)
			if !ref.isQualified() {
				fv.Child = values.NewQuantifiedObjectValue(corr)
			} else {
				fv.Field = ref.col
				fv.Child = values.NewQuantifiedObjectValue(
					values.NamedCorrelationIdentifier(ref.table),
				)
			}
		}
		return true
	})
}

func (p *existsSubqueryPlanner) BuildScalar(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("scalar subquery: nil query context")
	}
	innerOp, err := buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.schemaName, p.cteScopes)

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
		return p.buildCorrelatedScalar(q)
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

// resolveCorrelatedColumnValue resolves a (possibly alias-qualified) column
// name to a Value through the semantic scope — the same resolution the
// correlated WHERE clause uses. With a single inner source the scope returns a
// bare FieldValue (matching the bare keys a single scan flows); with joins the
// merged rows carry alias-qualified keys, so use the qualified FieldValue, as
// the top-level projection path does (logical_predicate.go ResolveIdentifier
// branch). A genuinely unresolvable column (single-source path) returns the
// resolver error so the caller can reject — silently falling back to a raw
// FieldValue would group every row under a null key (wrong results).
func resolveCorrelatedColumnValue(resolver *expr.Resolver, col string, hasJoins bool) (values.Value, error) {
	if hasJoins {
		// Merged join rows carry alias-qualified keys; use the qualified name
		// directly (ResolveIdentifier would yield a QOV-anchored value that does
		// not match the flat merged row). Existence is not validated here — the
		// same limitation as the top-level join GROUP BY path.
		return &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}, nil
	}
	ref := parseColRef(col)
	var qualifier semantic.Identifier
	id := semantic.NewUnquoted(ref.bare())
	if ref.isQualified() {
		qualifier = semantic.NewUnquoted(ref.table)
	}
	return resolver.ResolveIdentifier(qualifier, id)
}

// resolveCorrelatedGroupKeyValues resolves the GROUP BY keys of a correlated
// scalar subquery's inner aggregate to Value trees. The builder stores group
// keys as raw (often qualified) column-name strings with no expression context
// (groupByExprs nil), so resolve each name through the semantic scope rather
// than walking a parse node. An expression key (e.g. GROUP BY o.a + o.b) that
// fails to resolve is returned as an error — matching the top-level path
// (upgradeAggregate) — rather than silently falling back to an unresolvable
// raw FieldValue that would group every row under a null key.
func resolveCorrelatedGroupKeyValues(agg *logical.LogicalAggregate, sq *selectQuery, resolver *expr.Resolver, hasJoins bool) error {
	if agg == nil || len(agg.GroupKeys) == 0 {
		return nil
	}
	keyValues := make([]values.Value, len(agg.GroupKeys))
	for i, key := range agg.GroupKeys {
		if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
			v, err := resolver.WalkExpressionForProjection(sq.groupByExprs[i])
			if err != nil {
				return err
			}
			keyValues[i] = v
			continue
		}
		v, err := resolveCorrelatedColumnValue(resolver, key, hasJoins)
		if err != nil {
			return err
		}
		keyValues[i] = v
	}
	agg.GroupKeyValues = keyValues
	return nil
}

// aggColRefFromExpr inspects an ORDER BY expression for a column-argument
// aggregate function — `SUM(o.amount)` → ("SUM", "o.amount", true),
// `COUNT(*)` → ("COUNT", "", true) — by walking the parse tree (NOT text
// matching). Returns isAgg=false for non-aggregate refs and for
// expression-argument aggregates (`SUM(a*b)`), which have no bare column name
// to form the producer's stable FN(BAREARG) key.
func aggColRefFromExpr(expr antlrgen.IExpressionContext) (fn, argCol string, isAgg bool) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok || pred.Predicate() != nil {
		return "", "", false
	}
	atom, ok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !ok {
		return "", "", false
	}
	agg, ok := atom.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
	if !ok {
		return "", "", false
	}
	awf, ok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
	if !ok {
		return "", "", false
	}
	f, a, aExpr, _, _, ok := extractAwfFields(awf)
	if !ok || aExpr != nil {
		return "", "", false
	}
	return f, a, true
}

// groupedScalarSortKeys builds the ORDER BY sort keys for a correlated scalar
// subquery's grouped output (RFC-085). Each key's .Value is a FieldValue keyed by
// the EXACT datum key the aggregate cursor emits — a group key as
// strings.ToUpper(bare(col)) (the same derivation scalarCol uses, :4317), or a
// visible aggregate via aggDatumKey (its materialised name, not a recomputed
// canonical). The executor's sort does an exact-case datum lookup (values.go), so a
// mismatched key returns nil and sorts every row equal (nondeterministic — the bug
// this fixes). Hence any ORDER BY ref resolving to neither a group key nor a visible
// aggregate — including an ORDER-BY-only (not selected) aggregate or an expression
// key — is REJECTED loudly (SQL grouping semantics), never silently dropped. Setting
// .Value (which translateSort prefers over .Expr) bypasses the raw-text lookup.
func groupedScalarSortKeys(sq *selectQuery, aggDatumKey map[string]string) ([]logical.SortKey, error) {
	keys := make([]logical.SortKey, 0, len(sq.orderBy))
	for _, ob := range sq.orderBy {
		bare := strings.ToUpper(parseColRef(ob.colName).bare())
		dk := ""
		for _, k := range sq.groupBy {
			if gkdk := strings.ToUpper(parseColRef(k).bare()); gkdk == bare && bare != "" {
				dk = gkdk
				break
			}
		}
		if dk == "" {
			if v, ok := aggDatumKey[strings.ToUpper(ob.colName)]; ok {
				dk = v
			} else if v, ok := aggDatumKey[bare]; ok {
				dk = v
			}
		}
		// A selected aggregate spelled differently in ORDER BY than in SELECT
		// (`SELECT SUM(amount) … ORDER BY SUM(o.amount)`): the raw-text forms
		// above miss (parseColRef on `SUM(o.amount)` splits at the inner dot).
		// Recover the producer's stable FN(BAREARG) key from the parse tree so a
		// genuinely-selected aggregate resolves regardless of operand qualifier.
		if dk == "" && ob.rawExpr != nil {
			if fn, argCol, isAgg := aggColRefFromExpr(ob.rawExpr); isAgg {
				canonKey := strings.ToUpper(fn) + "(*)"
				if b := strings.ToUpper(parseColRef(argCol).bare()); b != "" {
					canonKey = strings.ToUpper(fn) + "(" + b + ")"
				}
				if v, ok := aggDatumKey[canonKey]; ok {
					dk = v
				}
			}
		}
		if dk == "" {
			return nil, api.NewErrorf(api.ErrCodeGroupingError,
				"ORDER BY %q must reference a grouping column or a selected aggregate in a grouped correlated scalar subquery", ob.colName)
		}
		dir := logical.SortAsc
		if !ob.ascending {
			dir = logical.SortDesc
		}
		sk := logical.SortKey{Value: &values.FieldValue{Field: dk}, Expr: dk, Dir: dir}
		if ob.nullsFirst != nil {
			sk.NullsFirst = *ob.nullsFirst
		}
		keys = append(keys, sk)
	}
	return keys, nil
}

func (p *existsSubqueryPlanner) buildCorrelatedScalar(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{Message: "correlated scalar subquery: nil query"}
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated scalar subquery: unsupported query body shape %T", q.QueryExpressionBody()),
		}
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated scalar subquery: %v", err), Cause: err,
		}
	}

	// Strip the session-schema qualifier off a schema-qualified table source
	// (`s.PB` → `PB`) BEFORE building the scan/join tree and resolving join sources
	// — the same normalization the EXISTS fallback (buildCorrelatedExists) and the
	// normal catalog-aware SELECT path run. Without it the raw `s.PB` reaches
	// Analyzer.ResolveTable (which does not strip a schema qualifier) and a valid
	// correlated scalar subquery over a schema-qualified source is rejected. RFC-142.
	normalizeSchemaQualifiedSelectSources(sq, p.effectiveSchemaName(), p.md)

	if sq.whereExpr == nil || sq.whereExpr.Expression() == nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: "correlated scalar subquery: WHERE clause required for correlation",
		}
	}

	innerAlias := sq.tableAlias
	if innerAlias == "" {
		innerAlias = sq.tableName
	}

	// Build scope first so the resolver can walk ON clauses.
	cat := rlcatalog.Wrap(p.md)
	analyzer := semantic.NewAnalyzer(cat, false)

	outerScope := semantic.NewScope(nil)
	for _, src := range p.outerScopes {
		_ = outerScope.AddSource(src)
	}

	innerScope := semantic.NewScope(outerScope)
	tbl, tblErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(sq.tableName, "."), false))
	if tblErr != nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated scalar subquery: resolve table %q: %v", sq.tableName, tblErr), Cause: tblErr,
		}
	}
	aliasID := semantic.NewUnquoted(innerAlias)
	_ = innerScope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: aliasID, CorrelationName: aliasID.Name(),
	})

	// Add join sources to scope so the resolver can resolve their columns. A
	// `t.arr AS x [AT ord]` comma source registers the same virtual Shadowing
	// unnest source the main path uses (exposing the element/ordinal binding)
	// instead of resolving `t.arr` as a table. Mirrors buildCorrelatedExists /
	// the main FROM path. RFC-142.
	for i, j := range sq.joins {
		if jErr := p.addCorrelatedJoinScopeSource(innerScope, analyzer, j, sq.tableName, innerAlias, sq.joins[:i]); jErr != nil {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: fmt.Sprintf("correlated scalar subquery: resolve join table %q: %v", j.tableName, jErr), Cause: jErr,
			}
		}
	}

	resolver := expr.New(analyzer, innerScope)

	// Build the scan + join tree. A lateral array unnest comma source lowers to
	// a LogicalUnnest (FlatMap-over-Explode in the translator) via the SAME
	// classification the main FROM path uses, not a plain table scan. Walk each
	// real join's ON clause with the resolver so the join predicate is attached.
	op := logical.LogicalOperator(logical.NewScan(sq.tableName, innerAlias))
	for i, j := range sq.joins {
		right := p.correlatedSubqueryJoinRight(j, sq.tableName, innerAlias, sq.joins[:i])
		var kind logical.JoinKind
		switch j.joinType {
		case joinTypeLeft:
			kind = logical.JoinLeft
		case joinTypeRight:
			kind = logical.JoinRight
		default:
			kind = logical.JoinInner
		}
		var joinPred predicates.QueryPredicate
		if j.onExpr != nil {
			walked, wErr := resolver.WalkPredicate(j.onExpr)
			if wErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: walk ON clause: %v", wErr), Cause: wErr,
				}
			}
			joinPred = walked
		}
		op = logical.NewJoinWithPredicate(op, right, kind, joinPred)
	}

	// Walk WHERE with outer+inner scope.
	pred, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
	if walkErr != nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated scalar subquery: walk predicate: %v", walkErr), Cause: walkErr,
		}
	}

	innerCorr := strings.ToUpper(aliasID.Name())
	qualifyBareFields(pred, innerCorr)
	pred = predicates.SimplifyPredicateValues(pred)

	// Build Filter(correlated_pred, JoinTree) — predicate INSIDE inner plan.
	var innerOp logical.LogicalOperator = op
	if pred != nil {
		innerOp = logical.NewFilterWithPredicate(op, pred, "")
	}

	// Validate the grouped projection (42803 / undefined column) with the
	// exact helper the top-level GROUP BY path runs — buildCorrelatedScalar
	// holds p.md and sq in scope. Catches `SELECT amount ... GROUP BY status`
	// (amount neither grouped nor aggregated).
	if len(sq.groupBy) > 0 {
		if vErr := validateGroupByProjection(sq, p.md); vErr != nil {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: fmt.Sprintf("correlated scalar subquery: %v", vErr), Cause: vErr,
			}
		}
		// ORDER BY over grouped output (ordering the groups so the LIMIT-1
		// FirstOrDefault picks a deterministic group) is wired below — a sort over
		// the post-aggregate row whose keys are canonicalised to the exact datum
		// keys the aggregate cursor emits (see groupedScalarSortKeys). RFC-085.
	}

	// A real aggregate function (COUNT/SUM/MIN/MAX/AVG) is present iff
	// countStar is set or some aggCol carries an aggFunc. Under a GROUP BY a
	// bare group-key projection is ALSO stored as a (visible, empty-aggFunc)
	// aggCol, so len(aggCols)>0 does not by itself mean "aggregate" — route on
	// the presence of a real aggregate function.
	hasRealAgg := sq.countStar
	for i := range sq.aggCols {
		if sq.aggCols[i].aggFunc != "" {
			hasRealAgg = true
			break
		}
	}

	// A scalar subquery must produce exactly one output column. Count the
	// visible SELECT items: under a GROUP BY each item is a visible aggCol (an
	// aggregate or a bare group-key projection), while a sole COUNT(*) is also
	// echoed into projCols — so count visible aggCols plus only those projCols
	// NOT already represented as a visible aggregate (the COUNT(*) echo).
	// Without aggregation the items are plain projCols; a no-GROUP-BY sole
	// COUNT(*) is the countStar case. Counting items (not distinct names) is
	// load-bearing: two items sharing an alias are still two columns.
	visAggNames := make(map[string]struct{}, len(sq.aggCols))
	outCount := 0
	for _, ac := range sq.aggCols {
		if ac.visible {
			outCount++
			visAggNames[strings.ToUpper(ac.outName)] = struct{}{}
		}
	}
	for _, pc := range sq.projCols {
		if _, echo := visAggNames[strings.ToUpper(pc)]; !echo {
			outCount++
		}
	}
	if outCount == 0 && sq.countStar {
		outCount = 1
	}
	if outCount > 1 {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: fmt.Sprintf("scalar subquery must return exactly one column, got %d", outCount),
		}
	}

	var scalarCol string
	if hasRealAgg {
		// Build the aggregate over the correlated filter. With GROUP BY the
		// aggregate may emit more than one group; the scalar contract is then
		// enforced by FirstOrDefault — the LIMIT 1 below plus the LEFT-OUTER
		// NULL-on-empty wrap in the translator — not a runtime cardinality
		// assertion (which would need look-ahead and breaks continuation-based
		// pagination). Empty input => zero groups => NULL falls out naturally,
		// whereas the no-GROUP-BY scalar aggregate emits one row (e.g. COUNT=0).
		//
		// Compute EVERY aggregate the query needs — the single visible one (the
		// scalar's value) AND any non-visible ones the parser harvested for
		// HAVING — so a HAVING that references a different aggregate than the
		// projection (e.g. `SELECT SUM(x) ... HAVING COUNT(*) > 1`) is evaluated
		// correctly. Aggregate output names use the BARE operand: a qualified
		// arg (`o.amount`) would embed a '.' in "SUM(O.AMOUNT)" that the
		// join-merge resolver mis-parses as a qualifier separator; the operand
		// itself is resolved separately so the qualifier still binds.
		singleSource := len(sq.joins) == 0
		var aggTexts, aggAliases []string
		var aggOperands []values.Value
		aggSeen := make(map[string]struct{})
		exprAggNames := make(map[string]struct{}) // join-path collision tracking only
		// Match-name (uppercased: SELECT alias, source FN(bareArg), canonical) →
		// the EXACT datum key the aggregate cursor emits (addAgg's returned name),
		// for resolving an ORDER BY ref over the grouped output (RFC-085).
		aggDatumKey := make(map[string]string)
		addAgg := func(fn, arg string, e antlrgen.IExpressionContext, distinct bool) (string, error) {
			// An expression argument has no bare column name, so it collapses to
			// FN(*). Two DISTINCT expression aggregates (e.g. SUM(a+b) projected
			// and SUM(c*d) in HAVING) would both synthesize "SUM(*)" and the
			// second would silently overwrite the first — so the HAVING would
			// read the projected aggregate's value. We cannot disambiguate them
			// by name, so reject rather than return wrong rows.
			bareArg := parseColRef(arg).bare()
			if bareArg == "" {
				bareArg = "*"
			}
			name := strings.ToUpper(fn) + "(" + strings.ToUpper(bareArg) + ")"
			// Resolve the operand first so we can recognise COUNT(<non-null
			// constant>) — e.g. COUNT(1) — which is exactly COUNT(*): it counts
			// every row, so it can safely share the COUNT(*) slot rather than
			// being treated as an opaque, collision-prone expression aggregate.
			var opVal values.Value
			if e != nil {
				v, err := resolver.WalkExpression(e)
				if err != nil {
					return "", err
				}
				opVal = v
			} else if arg != "" {
				v, err := resolveCorrelatedColumnValue(resolver, arg, len(sq.joins) > 0)
				if err != nil {
					return "", err
				}
				opVal = v
			}
			// DISTINCT aggregates are unsupported here (aggDistinct is not threaded
			// into the materialised slot, and COUNT(DISTINCT 1) != COUNT(*)). Reject
			// explicitly rather than rely on a name-prefix check.
			if distinct {
				return "", fmt.Errorf("DISTINCT aggregate not supported in a correlated scalar subquery")
			}
			if singleSource {
				// Single-source inner: materialise under the canonical name the
				// HAVING rewrite resolves by (canonicalAggName, shared with
				// rewriteAggregateValue). The name is dot-free (safe scalarCol) and
				// distinct expressions get distinct slots, so a HAVING referencing
				// any aggregate resolves in either direction; identical func+operand
				// reuses one slot.
				cname := canonicalAggName(fn, opVal)
				if _, dup := aggSeen[cname]; dup {
					return cname, nil
				}
				aggSeen[cname] = struct{}{}
				aggTexts = append(aggTexts, cname)
				aggAliases = append(aggAliases, cname)
				aggOperands = append(aggOperands, opVal)
				return cname, nil
			}
			// Join path: an expression/constant argument has no bare column name, so it
			// collapses to FN(*) here — but the HAVING rewrite
			// (rewriteAggregateValue) names an aggregate by the operand's
			// *explain* (COUNT(1), SUM(A+B)), which FN(*) does not match. Any
			// such aggregate is therefore "opaque": it cannot be safely shared
			// with, or referenced by, a differently-named aggregate. We do NOT
			// special-case COUNT(<const>)≡COUNT(*): although equal in value, the
			// reuse repeatedly opened silent-wrong corners (HAVING COUNT(*) vs a
			// projected COUNT(1), COUNT(DISTINCT 1), a HAVING that repeats the
			// visible constant aggregate) because the two name schemes still
			// diverge. Treat every expression/constant arg as opaque and reject
			// collisions fail-safe; full support needs the materialised names
			// aligned with the HAVING rewrite (tracked follow-up).
			opaqueExpr := e != nil
			if _, dup := aggSeen[name]; dup {
				_, priorExpr := exprAggNames[name]
				if opaqueExpr || priorExpr {
					return "", fmt.Errorf("an expression-argument aggregate (e.g. SUM(<expr>)) collides with another aggregate named %q; not supported in a correlated scalar subquery", name)
				}
				// Identical bare-column / star aggregate referenced twice (e.g.
				// COUNT(*) in both SELECT and HAVING) — safe to reuse the slot.
				// (Any expression/constant arg is opaque and exited above, so
				// this dup is always a non-opaque, identically-named aggregate.)
				return name, nil
			}
			aggSeen[name] = struct{}{}
			if opaqueExpr {
				exprAggNames[name] = struct{}{}
			}
			aggTexts = append(aggTexts, fn+"("+bareArg+")")
			aggAliases = append(aggAliases, name)
			aggOperands = append(aggOperands, opVal)
			return name, nil
		}
		for i := range sq.aggCols {
			ac := &sq.aggCols[i]
			if ac.aggFunc == "" {
				continue // bare group-key projection — handled as scalarCol below
			}
			// A HAVING-only (non-visible) aggregate over an expression/constant
			// argument cannot be resolved: addAgg materialises it under the bare
			// FN(*) name, but the HAVING-predicate rewrite looks it up by operand
			// explain (e.g. COUNT(1), SUM(A*3)) -- a name never exposed, so the
			// predicate reads NULL and silently drops valid groups. Reject it. A
			// visible expression aggregate is fine (its scalarCol uses the same
			// FN(*) name); a HAVING COUNT(*)/bare-column aggregate names
			// identically in both schemes, so COUNT(1) projected + HAVING COUNT(*)
			// still works.
			if !singleSource && !ac.visible && ac.aggExpr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: "correlated scalar subquery over a join: HAVING references an expression/constant-argument aggregate (e.g. COUNT(1), SUM(<expr>)) that cannot be resolved against the grouped output",
				}
			}
			name, err := addAgg(ac.aggFunc, ac.aggArg, ac.aggExpr, ac.aggDistinct)
			if err != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: resolve aggregate argument: %v", err), Cause: err,
				}
			}
			if ac.visible {
				scalarCol = name
				// Record the datum key under every form an ORDER BY might name it:
				// its SELECT alias, its source FN(bareArg) form, and the materialised
				// canonical name itself.
				aggDatumKey[strings.ToUpper(name)] = name
				if ac.outName != "" {
					aggDatumKey[strings.ToUpper(ac.outName)] = name
				}
				if bareArg := parseColRef(ac.aggArg).bare(); bareArg != "" {
					aggDatumKey[strings.ToUpper(ac.aggFunc+"("+bareArg+")")] = name
				}
			}
		}
		// A sole COUNT(*) the parser flagged via countStar (no aggCol entry).
		if sq.countStar {
			name, _ := addAgg("COUNT", "", nil, false) // -> COUNT(*)
			scalarCol = name
			aggDatumKey[strings.ToUpper(name)] = name
			if sq.countStarAlias != "" {
				aggDatumKey[strings.ToUpper(sq.countStarAlias)] = name
			}
		}
		// If the single visible output is a bare group-key projection (e.g.
		// `SELECT status ... GROUP BY status HAVING COUNT(*) > 1`), the scalar
		// value is the group key, not an aggregate. Match ONLY a real group-key
		// entry (groupCol set) — NOT a post-aggregation expression such as
		// `SUM(x) + 1` (visible aggCol with aggFunc=="" but outExpr!=nil), whose
		// value the aggregate row never materializes; those fall through to the
		// error below rather than silently resolving to NULL. Use the grouping
		// column (qualifier stripped) so the name matches the grouped row key
		// (and replaceScalarSubqueryRef does not double-prefix `O.O.STATUS`).
		if scalarCol == "" {
			for i := range sq.aggCols {
				if sq.aggCols[i].visible && sq.aggCols[i].aggFunc == "" && sq.aggCols[i].groupCol != "" {
					scalarCol = strings.ToUpper(parseColRef(sq.aggCols[i].groupCol).bare())
					break
				}
			}
		}
		if scalarCol == "" {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: "correlated scalar subquery: expected an aggregate function or grouping-key projection",
			}
		}
		aggOp := logical.NewAggregate(innerOp, sq.groupBy, aggTexts, aggAliases, "")
		aggOp.AggregateOperands = aggOperands
		if gkErr := resolveCorrelatedGroupKeyValues(aggOp, sq, resolver, len(sq.joins) > 0); gkErr != nil {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: fmt.Sprintf("correlated scalar subquery: resolve GROUP BY key: %v", gkErr), Cause: gkErr,
			}
		}
		if sq.havingExpr != nil {
			havingPred, hErr := resolver.WalkPredicate(sq.havingExpr)
			if hErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: walk HAVING: %v", hErr), Cause: hErr,
				}
			}
			aggOp.HavingPredicate = rewriteAggregateRefsInPredicate(havingPred)
		}
		innerOp = aggOp
		// ORDER BY over the grouped output: sort the groups BEFORE the LIMIT 1 so
		// FirstOrDefault picks the ordered-first group deterministically. Keys are
		// canonicalised to the exact post-aggregate datum keys (RFC-085).
		if len(sq.orderBy) > 0 {
			sortKeys, skErr := groupedScalarSortKeys(sq, aggDatumKey)
			if skErr != nil {
				return values.CorrelationIdentifier{}, skErr
			}
			innerOp = logical.NewSort(innerOp, sortKeys)
		}
		// GROUP BY may yield many groups; HAVING may filter the single group
		// to none. Cap at the first group (FirstOrDefault) for the scalar
		// contract. The plain scalar aggregate (no GROUP BY, no HAVING) already
		// emits exactly one row, so it is left uncapped.
		if len(sq.groupBy) > 0 || sq.havingExpr != nil {
			innerOp = logical.NewLimit(innerOp, 1, 0)
		}
	} else {
		// Non-aggregate correlated scalar subquery. The single output column is
		// either a plain projected column or, under a GROUP BY, a bare
		// group-key projection stored as a visible aggCol (DISTINCT-of-key).
		switch {
		case len(sq.projCols) == 1:
			// A qualified projection (`SELECT o.amount`) must resolve to the
			// bare datum key the inner row carries. For a single inner table the
			// row is keyed bare (`AMOUNT`) and replaceScalarSubqueryRef
			// re-qualifies under the inner alias (`O.AMOUNT`) at read time — a
			// scalarCol that kept the `o.` qualifier would double-prefix to
			// `O.O.AMOUNT` and resolve to NULL (same failure mode the bare
			// group-key case below guards). For a join the row is keyed
			// qualified (see :910), so keep the qualifier there.
			if len(sq.joins) > 0 {
				scalarCol = strings.ToUpper(sq.projCols[0])
			} else {
				scalarCol = strings.ToUpper(parseColRef(sq.projCols[0]).bare())
			}
		case len(sq.projCols) == 0 && len(sq.groupBy) > 0:
			// The output is the bare group-key projection (stored as a visible
			// aggCol with groupCol set). Use the grouping column (qualifier
			// stripped) so the name matches the grouped row key — otherwise
			// replaceScalarSubqueryRef double-prefixes the inner alias
			// (`O.O.STATUS`) and the scalar resolves to NULL. A visible
			// expression-of-group-keys (outExpr, groupCol=="") is NOT a plain
			// key — the aggregate row never materializes it — so it falls through
			// to the error rather than silently resolving to NULL.
			for i := range sq.aggCols {
				if sq.aggCols[i].visible && sq.aggCols[i].groupCol != "" {
					scalarCol = strings.ToUpper(parseColRef(sq.aggCols[i].groupCol).bare())
					break
				}
			}
			if scalarCol == "" {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: "correlated scalar subquery: non-aggregate subquery must have explicit projection",
				}
			}
		default:
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: "correlated scalar subquery: non-aggregate subquery must have explicit projection",
			}
		}

		// Non-aggregate GROUP BY (`SELECT status ... GROUP BY status`): zero
		// aggregate functions, projecting a grouping key (DISTINCT-of-key).
		// validateGroupByProjection above already confirmed the projected
		// column is a grouping key. Build the GroupBy below the optional
		// ORDER BY so the sort runs over the grouped output.
		if len(sq.groupBy) > 0 {
			aggOp := logical.NewAggregate(innerOp, sq.groupBy, nil, nil, "")
			if gkErr := resolveCorrelatedGroupKeyValues(aggOp, sq, resolver, len(sq.joins) > 0); gkErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: resolve GROUP BY key: %v", gkErr), Cause: gkErr,
				}
			}
			innerOp = aggOp
		}

		// Add ORDER BY if present.
		if len(sq.orderBy) > 0 {
			if len(sq.groupBy) > 0 {
				// GROUP BY (group keys only): the sort runs over the POST-aggregate
				// row, whose keys are bare-uppercased — raw ob.colName (original case,
				// possibly qualified) would miss and sort every row equal. Canonicalise
				// to the exact group-key datum keys (RFC-085). No aggregates here.
				sortKeys, skErr := groupedScalarSortKeys(sq, nil)
				if skErr != nil {
					return values.CorrelationIdentifier{}, skErr
				}
				innerOp = logical.NewSort(innerOp, sortKeys)
			} else {
				// No GROUP BY: the sort runs over the raw scan rows before LIMIT 1.
				// For a single inner table that row is keyed by the bare column
				// name, so a qualified ORDER BY key (`ORDER BY o.amount`) would
				// miss and sort every row equal — strip the qualifier to the bare
				// key (preserving the written case, which reproduces the working
				// unqualified form). A join row is keyed qualified, so leave it.
				keys := make([]logical.SortKey, len(sq.orderBy))
				for i, ob := range sq.orderBy {
					dir := logical.SortAsc
					if !ob.ascending {
						dir = logical.SortDesc
					}
					keyExpr := ob.colName
					if len(sq.joins) == 0 {
						keyExpr = parseColRef(ob.colName).bare()
					}
					keys[i] = logical.SortKey{Expr: keyExpr, Dir: dir}
					if ob.nullsFirst != nil {
						keys[i].NullsFirst = *ob.nullsFirst
					}
				}
				innerOp = logical.NewSort(innerOp, keys)
			}
		}

		// SQL standard: scalar subquery must return at most 1 row.
		// Use the user's LIMIT if specified (limit < 0 = no limit),
		// otherwise enforce LIMIT 1.
		if sq.limit >= 0 {
			innerOp = logical.NewLimit(innerOp, sq.limit, sq.offset)
		} else {
			innerOp = logical.NewLimit(innerOp, 1, 0)
		}
	}

	alias := values.UniqueCorrelationIdentifier()
	p.correlatedScalarSubqueries = append(p.correlatedScalarSubqueries, logical.CorrelatedScalarSubquery{
		Alias:      alias,
		InnerPlan:  innerOp,
		InnerAlias: strings.ToUpper(innerAlias),
		ScalarCol:  scalarCol,
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
