package embedded

// PlanVisitor walks ANTLR parse tree nodes and builds a
// logical.LogicalOperator tree for the Cascades planner path.
//
// Architecture: Java's QueryVisitor.visitSimpleTable builds the plan in
// this order: FROM -> WHERE -> GROUP BY + SELECT + HAVING -> ORDER BY ->
// LIMIT -> DISTINCT. Each step takes the current operator and wraps it.
//
// PlanVisitor mirrors this incremental wrapping: visitFrom builds the
// scan/join subtree, visitWhere wraps it with a filter, visitSelectGroupBy
// wraps it with aggregate/projection, visitOrderBy wraps it with sort,
// visitLimit wraps it with limit, and visitFinalProjection wraps it with
// the non-aggregate projection and DISTINCT.
//
// The complex aggregate classification (SELECT element parsing, GROUP BY
// interaction, HAVING harvesting — ~500 lines) still delegates to
// extractFromSimpleTable. Everything else walks the ANTLR tree directly.
//
// The proto/naive generator continues using the old pipeline directly.

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// PlanVisitor builds LogicalOperator trees from ANTLR parse nodes.
// It holds the metadata needed for catalog-aware resolution (predicate
// upgrade, column validation, sort-key resolution) and any CTE column
// schemas accumulated from WITH clause processing.
type PlanVisitor struct {
	md        *recordlayer.RecordMetaData
	cteScopes map[string]semantic.ScopeSource
}

// NewPlanVisitor creates a PlanVisitor with the given metadata.
// md may be nil; all catalog-aware upgrades degrade to text fallback.
func NewPlanVisitor(md *recordlayer.RecordMetaData) *PlanVisitor {
	return &PlanVisitor{md: md}
}

// VisitQuery is the top-level entry point. It handles WITH (CTE)
// wrapping and then delegates to VisitQueryBody for the main query.
//
// Mirrors buildLogicalPlanForQueryWithCatalog: pre-scans CTE
// definitions to extract column schemas, then recursively builds the
// main query body with CTE scopes in context.
func (v *PlanVisitor) VisitQuery(q antlrgen.IQueryContext) (logical.LogicalOperator, error) {
	if q == nil {
		return nil, nil
	}
	if v.md == nil {
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()

	// Pre-scan CTE definitions to extract column schemas. Process in
	// declaration order so CTE B can reference CTE A's derived schema.
	if ctesCtx != nil {
		if v.cteScopes == nil {
			v.cteScopes = make(map[string]semantic.ScopeSource)
		}
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if _, exists := v.cteScopes[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			if src, ok := buildCTEColumnSource(v.md, name, nq.Query(), v.cteScopes); ok {
				// Apply CTE column aliases: WITH c1(x, y) AS (...)
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
				v.cteScopes[upper] = src
			}
		}
	}

	main, err := v.VisitQueryBody(q.QueryExpressionBody())
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
			body, err = v.VisitQueryBody(inner.QueryExpressionBody())
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

// VisitQueryBody dispatches simple SELECT vs UNION, threading
// metadata and CTE scopes through both arms.
func (v *PlanVisitor) VisitQueryBody(body antlrgen.IQueryExpressionBodyContext) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		return v.VisitSimpleTable(b)
	case *antlrgen.SetQueryContext:
		return v.visitUnion(b)
	}
	return nil, nil
}

// VisitSimpleTable is the main SELECT visitor. It walks the ANTLR tree
// incrementally, building the LogicalOperator tree step by step in the
// same order as Java's QueryVisitor.visitSimpleTable:
//
//  1. FROM clause  → visitFrom     → scan/derived/join operator
//  2. WHERE clause → visitWhere    → wrap with filter
//  3. SELECT+GROUP BY+HAVING → visitSelectGroupBy → wrap with aggregate
//  4. ORDER BY     → visitOrderBy  → wrap with sort
//  5. LIMIT/OFFSET → visitLimit    → wrap with limit
//  6. Projection   → visitFinalProjection + DISTINCT
//
// Step 3 delegates to the existing extractFromSimpleTable pipeline for
// aggregate classification because that logic is ~500 lines of complex
// interaction between SELECT elements, GROUP BY, HAVING, and aggregate
// harvesting. Everything else walks the ANTLR tree directly.
func (v *PlanVisitor) VisitSimpleTable(termCtx *antlrgen.QueryTermDefaultContext) (logical.LogicalOperator, error) {
	if termCtx == nil {
		return nil, nil
	}
	simpleTable, ok := termCtx.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, nil
	}

	// Parse the full selectQuery for aggregate classification, validation,
	// and the complex GROUP BY / HAVING / ORDER BY interactions that are
	// too tangled to factor. The selectQuery is consumed only by
	// visitSelectGroupBy and the _postBuild catalog-aware upgrade pass.
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return nil, err
	}

	// Validate unsupported functions before building the plan.
	if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	// Validate qualified star sources.
	if err := validateQualifiedStarSources(sq, v.md); err != nil {
		return nil, err
	}

	// Step 1: FROM → build scan/derived/join operator from ANTLR.
	// Derived tables use recursive v.VisitQueryBody instead of
	// buildLogicalPlanForQueryBodyWithCTECatalog.
	op, err := v.visitFrom(sq)
	if err != nil {
		return nil, err
	}
	if op == nil {
		return nil, nil
	}

	// Step 2: WHERE → wrap with filter from ANTLR.
	if sq.whereExpr != nil {
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
	}

	// Step 3: SELECT + GROUP BY + HAVING → aggregate classification
	// and operator building. Delegates to the existing machinery via
	// buildSelectShell's aggregate section.
	op, stripPrefix := v.visitSelectGroupBy(op, sq)

	// Step 4: ORDER BY → wrap with sort from selectQuery.orderBy
	// (parsed by extractFromSimpleTable with dedup, positional refs,
	// alias resolution).
	op = v.visitOrderBy(op, sq, stripPrefix)

	// Post-sort strip projection: when hasSortOnly is true in the
	// aggregate path, the visible-only projection is deferred past
	// Sort so sort-key columns remain accessible.
	if len(sq.postSortStripProj) > 0 {
		op = logical.NewProject(op, sq.postSortStripProj, sq.postSortStripAliases)
	}

	// Step 5: LIMIT/OFFSET → wrap with limit.
	// extractFromSimpleTable rejects LIMIT/OFFSET for this codebase
	// (Java's AstNormalizer does the same), so sq.limit == -1 and
	// sq.offset == 0. The operator is only built when values are set.
	op = v.visitLimit(op, sq)

	// Step 6: Projection (non-aggregate) + DISTINCT.
	op = v.visitFinalProjection(op, sq, stripPrefix)
	if sq.distinct {
		op = logical.NewDistinct(op)
	}

	// Catalog-aware upgrades: run the _postBuild pass which handles
	// predicate resolution, sort key Value resolution, projection Value
	// resolution, aggregate operand resolution, qualified star expansion,
	// and all semantic validation (ambiguous columns, undefined columns,
	// GROUP BY projection validation, etc.).
	if v.md != nil {
		return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, v.md, v.cteScopes)
	}
	return op, nil
}

// visitFrom builds the FROM-source subtree from the selectQuery's parsed
// FROM metadata. For derived tables (subquery in FROM), it recursively
// calls v.VisitQueryBody to build the inner plan — this is the key
// structural improvement over the old pipeline which used
// buildLogicalPlanForQueryBodyWithCTECatalog for inner plans.
//
// Returns the built operator. The selectQuery carries tableName,
// tableAlias, joins, and derivedQuery that describe the FROM shape.
func (v *PlanVisitor) visitFrom(sq *selectQuery) (logical.LogicalOperator, error) {
	if sq == nil {
		return nil, nil
	}

	// No FROM: VALUES query (SELECT without FROM rejected by
	// extractFromSimpleTable for this codebase).
	if sq.tableName == "" && sq.derivedQuery == nil {
		rows := make([]string, len(sq.projCols))
		aliases := make([]string, len(sq.projCols))
		for i, col := range sq.projCols {
			expr := col
			if sq.projExprs != nil && i < len(sq.projExprs) && sq.projExprs[i] != nil {
				expr = strings.TrimSpace(sq.projExprs[i].GetText())
			}
			rows[i] = expr
			if sq.projAliases != nil && i < len(sq.projAliases) {
				aliases[i] = sq.projAliases[i]
			}
		}
		return logical.NewValues(rows, aliases), nil
	}

	var op logical.LogicalOperator
	if sq.derivedQuery != nil {
		// Derived table: recursively build inner plan via the visitor.
		// This is the key improvement: CTE scopes flow naturally through
		// the visitor instance, and inner plans get catalog-aware upgrades.
		innerOp, innerErr := v.VisitQueryBody(sq.derivedQuery.QueryExpressionBody())
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp == nil {
			return nil, nil
		}
		// For derived tables without joins, the _postBuild needRebuild
		// path (qualified star expansion) calls buildLogicalPlanForSelect(sq)
		// which re-builds the whole tree. That function uses
		// buildOuterPlanOnDerived for derived tables — it falls back to
		// extractFromSimpleTable for the inner plan, losing the visitor's
		// recursive CTE-scope-aware build. Short-circuit: when _postBuild
		// detects a derived table with no joins, it returns directly via
		// buildOuterPlanOnDerived(sq, innerOp). So the needRebuild path
		// only fires for the non-derived case. Safe.
		if len(sq.joins) > 0 {
			op = logical.NewCTE(sq.tableName, innerOp,
				logical.NewScan(sq.tableName, ""), false)
		} else {
			op = innerOp
		}
	} else {
		op = logical.NewScan(sq.tableName, sq.tableAlias)
	}

	// Pre-build derived table inner plans for JOIN sources through the
	// visitor. Store the result in j.catalogAwareInnerPlan so that if
	// _postBuild triggers a needRebuild (qualified star expansion),
	// buildLogicalPlanForSelect can use the already-built inner plan
	// rather than falling back to the old non-CTE-aware path.
	for i := range sq.joins {
		j := &sq.joins[i]
		if j.derivedQuery == nil {
			continue
		}
		innerOp, innerErr := v.VisitQueryBody(j.derivedQuery.QueryExpressionBody())
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			j.catalogAwareInnerPlan = innerOp
		}
	}

	// JOINs chain left-to-right from the primary scan. Each join wraps
	// the current op as Left and scans the joined table as Right.
	for _, j := range sq.joins {
		var right logical.LogicalOperator
		if j.catalogAwareInnerPlan != nil {
			// Use the pre-built inner plan from the visitor.
			if j.alias != "" {
				right = logical.NewCTE(j.alias, j.catalogAwareInnerPlan,
					logical.NewScan(j.alias, ""), false)
			} else {
				right = j.catalogAwareInnerPlan
			}
		} else if j.derivedQuery != nil {
			// Fallback: derived table without a pre-built inner plan
			// (shouldn't happen, but defensive).
			innerRight, innerErr := v.VisitQueryBody(j.derivedQuery.QueryExpressionBody())
			if innerErr != nil {
				return nil, innerErr
			}
			if innerRight == nil {
				return nil, nil
			}
			if j.alias != "" {
				right = logical.NewCTE(j.alias, innerRight,
					logical.NewScan(j.alias, ""), false)
			} else {
				right = innerRight
			}
		} else {
			right = logical.NewScan(j.tableName, j.alias)
		}
		var kind logical.JoinKind
		switch j.joinType {
		case "LEFT":
			kind = logical.JoinLeft
		case "RIGHT":
			kind = logical.JoinRight
		default:
			kind = logical.JoinInner
		}
		onText := ""
		if j.onExpr != nil {
			onText = canonicalTextOf(j.onExpr)
		}
		op = logical.NewJoin(op, right, kind, onText)
	}

	return op, nil
}

// visitSelectGroupBy builds the aggregate/GROUP BY/HAVING shell around
// the current operator. This delegates to the existing buildSelectShell
// aggregate section because the SELECT element classification, GROUP BY
// interaction, and HAVING harvesting involve ~500 lines of complex logic.
//
// Returns the wrapped operator and a stripPrefix (non-empty for derived
// table queries where column names need prefix stripping).
func (v *PlanVisitor) visitSelectGroupBy(op logical.LogicalOperator, sq *selectQuery) (logical.LogicalOperator, string) {
	if sq == nil {
		return op, ""
	}

	// Determine strip prefix for derived tables.
	stripPrefix := ""
	if sq.derivedQuery != nil {
		stripPrefix = strings.ToUpper(sq.tableName) + "."
	}

	strip := func(s string) string {
		if stripPrefix != "" && strings.HasPrefix(strings.ToUpper(s), stripPrefix) {
			return s[len(stripPrefix):]
		}
		return s
	}

	// Three aggregate shapes collapse here:
	//   - Bare COUNT(*): no group keys, single COUNT(*) aggregate.
	//   - GROUP BY without aggregates: just the group keys.
	//   - Mixed: aggCols carries both group-col and agg-function entries.
	if !sq.countStar && len(sq.aggCols) == 0 && len(sq.groupBy) == 0 {
		return op, stripPrefix
	}

	var aggs, aggAliases []string
	keys := make([]string, len(sq.groupBy))
	for i, k := range sq.groupBy {
		keys[i] = strip(k)
	}
	if sq.countStar {
		aggs = []string{"COUNT(*)"}
		aggAliases = []string{sq.countStarAlias}
	} else {
		for _, ac := range sq.aggCols {
			if ac.aggFunc != "" {
				arg := ac.aggArg
				if arg == "" && ac.aggExpr != nil {
					arg = canonicalTextOf(ac.aggExpr)
				}
				if arg == "" {
					arg = "*"
				}
				arg = strip(arg)
				distinctPfx := ""
				if ac.aggDistinct {
					distinctPfx = "DISTINCT "
				}
				aggs = append(aggs, ac.aggFunc+"("+distinctPfx+arg+")")
				aggAliases = append(aggAliases, ac.outName)
			}
		}
	}
	having := ""
	if sq.havingExpr != nil {
		having = canonicalTextOf(sq.havingExpr)
	}
	op = logical.NewAggregate(op, keys, aggs, aggAliases, having)

	// Post-aggregation projection for mixed SELECT lists that contain
	// both aggregates and computed expressions / constants.
	hasOutExpr := false
	for _, ac := range sq.aggCols {
		if ac.outExpr != nil && ac.aggFunc == "" && ac.visible {
			hasOutExpr = true
			break
		}
	}
	if hasOutExpr {
		var allProj []string
		var allAntlr []antlrgen.IExpressionContext
		for _, ac := range sq.aggCols {
			if !ac.visible {
				continue
			}
			if ac.outExpr != nil && ac.aggFunc == "" {
				allProj = append(allProj, strings.TrimSpace(ac.outExpr.GetText()))
				allAntlr = append(allAntlr, ac.outExpr)
			} else if ac.aggFunc != "" {
				arg := ac.aggArg
				if arg == "" && ac.aggExpr != nil {
					arg = canonicalTextOf(ac.aggExpr)
				}
				if arg == "" {
					arg = "*"
				}
				arg = strip(arg)
				allProj = append(allProj, ac.aggFunc+"("+arg+")")
				allAntlr = append(allAntlr, nil)
			} else if ac.groupCol != "" {
				allProj = append(allProj, strip(ac.groupCol))
				allAntlr = append(allAntlr, nil)
			}
		}
		if len(allProj) > 0 {
			proj := logical.NewProject(op, allProj, nil)
			computed := make([]bool, len(allProj))
			for i, e := range allAntlr {
				computed[i] = e != nil
			}
			proj.IsComputed = computed
			op = proj
			sq.postAggExprs = allAntlr
		}
	} else if len(keys) > 0 {
		hasNonVisible := false
		for _, ac := range sq.aggCols {
			if !ac.visible {
				hasNonVisible = true
				break
			}
		}
		var visibleProj []string
		var visibleAliases []string
		for _, ac := range sq.aggCols {
			if !ac.visible {
				continue
			}
			if ac.aggFunc != "" {
				arg := ac.aggArg
				if arg == "" && ac.aggExpr != nil {
					arg = canonicalTextOf(ac.aggExpr)
				}
				if arg == "" {
					arg = "*"
				}
				arg = strip(arg)
				canonical := ac.aggFunc + "(" + arg + ")"
				visibleProj = append(visibleProj, canonical)
				alias := ""
				if ac.outName != "" && !strings.EqualFold(ac.outName, canonical) {
					alias = ac.outName
				}
				visibleAliases = append(visibleAliases, alias)
			} else if ac.groupCol != "" {
				visibleProj = append(visibleProj, strip(ac.groupCol))
				alias := ""
				if ac.outName != "" && !strings.EqualFold(ac.outName, ac.groupCol) {
					alias = ac.outName
				}
				visibleAliases = append(visibleAliases, alias)
			}
		}
		totalOutput := len(keys) + len(aggs)
		hasAggAlias := false
		for i, a := range visibleAliases {
			if a != "" && i < len(visibleProj) && strings.Contains(strings.ToUpper(visibleProj[i]), "(") {
				hasAggAlias = true
				break
			}
		}
		needsStrip := len(visibleProj) < totalOutput || hasAggAlias || hasNonVisible
		if needsStrip {
			if hasNonVisible {
				sq.postSortStripProj = visibleProj
				sq.postSortStripAliases = visibleAliases
			} else {
				op = logical.NewProject(op, visibleProj, visibleAliases)
			}
		}
	}

	return op, stripPrefix
}

// visitOrderBy builds the LogicalSort operator from the selectQuery's
// parsed ORDER BY clauses. The ORDER BY parsing (dedup, positional
// reference resolution, alias resolution) was done by
// extractFromSimpleTable; this method builds the sort keys.
func (v *PlanVisitor) visitOrderBy(op logical.LogicalOperator, sq *selectQuery, stripPrefix string) logical.LogicalOperator {
	if len(sq.orderBy) == 0 {
		return op
	}

	strip := func(s string) string {
		if stripPrefix != "" && strings.HasPrefix(strings.ToUpper(s), stripPrefix) {
			return s[len(stripPrefix):]
		}
		return s
	}

	keys := make([]logical.SortKey, 0, len(sq.orderBy))
	for _, ob := range sq.orderBy {
		dir := logical.SortAsc
		if !ob.ascending {
			dir = logical.SortDesc
		}
		expr := strip(ob.colName)
		if expr == "" && ob.rawExpr != nil {
			expr = ob.rawExpr.GetText()
		}
		nullsFirst := ob.ascending
		if ob.nullsFirst != nil {
			nullsFirst = *ob.nullsFirst
		}
		keys = append(keys, logical.SortKey{Expr: expr, Dir: dir, NullsFirst: nullsFirst})
	}
	return logical.NewSort(op, keys)
}

// visitLimit builds the LogicalLimit operator. For this codebase,
// LIMIT/OFFSET are rejected at parse time by extractFromSimpleTable
// (matching Java's AstNormalizer), so sq.limit is always -1 and
// sq.offset is always 0. The operator is only built when values are set
// (future-proofing for when LIMIT support is added).
func (v *PlanVisitor) visitLimit(op logical.LogicalOperator, sq *selectQuery) logical.LogicalOperator {
	if sq.limit >= 0 || sq.offset > 0 {
		return logical.NewLimit(op, sq.limit, sq.offset)
	}
	return op
}

// visitFinalProjection builds the non-aggregate projection.
// Aggregate queries have their projection handled in visitSelectGroupBy;
// this handles the plain SELECT column list case.
func (v *PlanVisitor) visitFinalProjection(op logical.LogicalOperator, sq *selectQuery, stripPrefix string) logical.LogicalOperator {
	if len(sq.projCols) == 0 {
		return op
	}

	strip := func(s string) string {
		if stripPrefix != "" && strings.HasPrefix(strings.ToUpper(s), stripPrefix) {
			return s[len(stripPrefix):]
		}
		return s
	}

	projs := make([]string, len(sq.projCols))
	aliases := make([]string, len(sq.projCols))
	computed := make([]bool, len(sq.projCols))
	for i, col := range sq.projCols {
		projs[i] = strip(col)
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
	return proj
}

// visitUnion handles UNION ALL queries, threading CTE scopes through
// both branches. Mirrors buildLogicalPlanForUnionWithCTECatalog.
func (v *PlanVisitor) visitUnion(setQ *antlrgen.SetQueryContext) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	if len(v.cteScopes) == 0 {
		return buildLogicalPlanForUnionWithCatalog(setQ, v.md)
	}
	return buildLogicalPlanForUnionWithCTECatalog(setQ, v.md, v.cteScopes)
}
