package embedded

import (
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// buildLogicalPlanForQuery is the outer-most builder entry point.
// It handles WITH (CTE) wrapping, then delegates the main query
// body to buildLogicalPlanForQueryBody. Each CTE in the WITH list
// wraps the result in a LogicalCTE; the outermost CTE ends up at
// the root.
func buildLogicalPlanForQuery(q antlrgen.IQueryContext) logical.LogicalOperator {
	if q == nil {
		return nil
	}
	main := buildLogicalPlanForQueryBody(q.QueryExpressionBody())
	if main == nil {
		return nil
	}
	ctesCtx := q.Ctes()
	if ctesCtx == nil {
		return main
	}
	recursive := ctesCtx.RECURSIVE() != nil
	// Wrap each named CTE around the accumulated main. Reverse
	// iteration so the first-declared CTE ends up at the root (read
	// top-down in Explain output, matching the SQL text order).
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			body = buildLogicalPlanForQueryBody(inner.QueryExpressionBody())
		}
		if body == nil {
			// CTE body out of builder scope — bail rather than emit
			// a partial tree.
			return nil
		}
		main = logical.NewCTE(name, body, main, recursive)
	}
	return main
}

// buildLogicalPlanForQueryBody is the general entry point that
// handles both simple SELECT and UNION shapes. It mirrors
// execQueryBodyRows' dispatch: QueryTermDefaultContext → simple
// SELECT; SetQueryContext → UNION (N-ary via nested SetQuery). Called
// from naive_generator's SELECT ExplainFn.
func buildLogicalPlanForQueryBody(body antlrgen.IQueryExpressionBodyContext) logical.LogicalOperator {
	if body == nil {
		return nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil
		}
		return buildLogicalPlanForSelect(sq)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnion(b)
	}
	return nil
}

// buildLogicalPlanForUnion walks a SetQueryContext (UNION ALL or
// UNION DISTINCT) recursively, producing a LogicalUnion whose
// Inputs hold the flattened left-and-right subtrees. The grammar
// nests SetQuery(SetQuery(A, B), C) for A UNION B UNION C; we
// flatten into [A, B, C] when all levels share the same quantifier
// — makes the Explain output read left-to-right. Mixed quantifiers
// keep the original nesting.
func buildLogicalPlanForUnion(setQ *antlrgen.SetQueryContext) logical.LogicalOperator {
	if setQ == nil {
		return nil
	}
	if q := setQ.GetQuantifier(); q == nil || !strings.EqualFold(q.GetText(), "ALL") {
		return nil
	}
	left := buildLogicalPlanForQueryBody(setQ.GetLeft())
	right := buildLogicalPlanForQueryBody(setQ.GetRight())
	if left == nil || right == nil {
		return nil
	}
	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	return logical.NewUnion(inputs, false)
}

// Phase 3 logical-plan builder — narrow-scope seed.
//
// Converts a parsed `*selectQuery` (the internal struct the embedded
// engine builds today) into a `logical.LogicalOperator` tree. This is
// the first bridge between the parse tree and the Phase 3 skeleton:
// `Plan.Explain()` on the naive Generator now returns a real plan
// tree instead of canonical SQL text, for the query shapes this
// builder recognises.
//
// **Scope.** SELECT (single-table / JOIN / aggregate+GROUP BY+HAVING
// / derived table / UNION) and all DML (INSERT VALUES / INSERT SELECT
// / UPDATE / DELETE). Returns nil for CTE (WITH …) and SELECT
// without FROM. Nil shapes fall through to the canonical-SQL explain
// (the pre-existing Phase 1a placeholder).
//
// Predicates + expressions are carried as canonical source text for
// now (LogicalFilter.PredicateText, LogicalProject.Projections).
// RFC-021 Phase 2 replaces those with real `Value` / `QueryPredicate`
// nodes from the cascades package — at which point the builder grows
// a translation pass from antlrgen.IExpressionContext to Value /
// QueryPredicate.
//
// Output tree shape (innermost to outermost):
//
//	LogicalScan (or derived subtree)
//	  → LogicalFilter    (if WHERE)
//	    → LogicalJoin*   (if joins; chained left-to-right)
//	      → LogicalAggregate (if GROUP BY / aggregates / HAVING)
//	        → LogicalSort    (if ORDER BY)
//	          → LogicalLimit (if LIMIT or OFFSET)
//	            → LogicalProject (unless SELECT *)

// buildLogicalPlanForSelect returns a LogicalOperator tree for the
// parsed selectQuery, or nil when the shape is out of current scope
// (SELECT without FROM; derived-table builds that recursively fail).
func buildLogicalPlanForSelect(sq *selectQuery) logical.LogicalOperator {
	if sq == nil {
		return nil
	}
	if sq.tableName == "" && sq.derivedQuery == nil {
		// SELECT without FROM — emit LogicalValues (single-row
		// constant projection). Carries the projection expression
		// text per column (future: real Value nodes per RFC-021
		// Phase 2).
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
		return logical.NewValues(rows, aliases)
	}

	// Build the FROM-source subtree. Either a plain table scan or a
	// derived table (subquery in FROM). For derived tables we
	// recursively build the inner logical plan and wrap it as a CTE
	// so that the user-supplied alias (e.g. "sq1") is preserved in
	// the logical tree. The CTE wrapper ensures that sourceAlias
	// (used by the NLJ rule to set outerAlias/innerAlias on the
	// physical plan) returns the derived-table alias rather than the
	// underlying table name. Without this, column qualification in
	// mergeRows uses the wrong qualifier and projections like
	// "sq1.x" resolve to NULL.
	var op logical.LogicalOperator
	if sq.derivedQuery != nil {
		var innerOp logical.LogicalOperator
		body := sq.derivedQuery.QueryExpressionBody()
		if termDefault, ok := body.(*antlrgen.QueryTermDefaultContext); ok {
			if simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext); ok {
				if inner, err := extractFromSimpleTable(simpleTable); err == nil {
					innerOp = buildLogicalPlanForSelect(inner)
				}
			}
		}
		if innerOp == nil {
			// Derived query is out of the inner builder's scope — bail
			// rather than emit a misleading partial tree.
			return nil
		}
		// Wrap as CTE so the alias surfaces in the logical tree.
		// When there are no joins the outer operators (filter, sort,
		// project) can reference the inner plan directly — the CTE
		// wrapper would add an unnecessary indirection and change
		// datum key layout. Only wrap when joins are present.
		if len(sq.joins) > 0 {
			op = logical.NewCTE(sq.tableName, innerOp,
				logical.NewScan(sq.tableName, ""), false)
		} else {
			op = innerOp
		}
	} else {
		op = logical.NewScan(sq.tableName, sq.tableAlias)
	}

	// JOINs chain left-to-right from the primary scan. Each join wraps
	// the current op as Left and scans the joined table as Right.
	// Produces `InnerJoin(on ...) → LeftScan → RightScan` nested as
	// the logical operator tree expects.
	for _, j := range sq.joins {
		var right logical.LogicalOperator
		if j.catalogAwareInnerPlan != nil {
			// catalogAwareInnerPlan is the inner plan built through the
			// catalog-aware path. Wrap it in a CTE so the join alias
			// is preserved (same logic as the primary source above).
			if j.alias != "" {
				right = logical.NewCTE(j.alias, j.catalogAwareInnerPlan,
					logical.NewScan(j.alias, ""), false)
			} else {
				right = j.catalogAwareInnerPlan
			}
		} else if j.derivedQuery != nil {
			var innerRight logical.LogicalOperator
			body := j.derivedQuery.QueryExpressionBody()
			if termDefault, ok := body.(*antlrgen.QueryTermDefaultContext); ok {
				if simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext); ok {
					if inner, err := extractFromSimpleTable(simpleTable); err == nil {
						innerRight = buildLogicalPlanForSelect(inner)
					}
				}
			}
			if innerRight == nil {
				return nil
			}
			// Wrap as CTE so the alias surfaces in sourceAlias.
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

	if sq.whereExpr != nil {
		// Carry the canonical WHERE text — renders in Explain as
		// `Filter(<text>)`. Future: translate to QueryPredicate tree.
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
	}

	// Aggregate / GROUP BY. Three shapes collapse here:
	//   - Bare COUNT(*): no group keys, single COUNT(*) aggregate.
	//   - GROUP BY without aggregates: just the group keys.
	//   - Mixed: aggCols carries both group-col and agg-function
	//     entries with outName.
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		var aggs, aggAliases []string
		keys := append([]string{}, sq.groupBy...)
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
		// both aggregates and computed expressions / constants. When outExpr
		// entries exist, the projection must list ALL visible columns in
		// SELECT-list order — aggregate outputs as column references,
		// computed expressions as expressions to evaluate. Without this,
		// the outExpr-only projection would drop the aggregate columns.
		hasOutExpr := false
		for _, ac := range sq.aggCols {
			if ac.outExpr != nil && ac.aggFunc == "" && !ac.sortOnly && !ac.hidden {
				hasOutExpr = true
				break
			}
		}
		if hasOutExpr {
			var allProj []string
			var allAntlr []antlrgen.IExpressionContext
			for _, ac := range sq.aggCols {
				if ac.sortOnly || ac.hidden {
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
					allProj = append(allProj, ac.aggFunc+"("+arg+")")
					allAntlr = append(allAntlr, nil)
				} else if ac.groupCol != "" {
					allProj = append(allProj, ac.groupCol)
					allAntlr = append(allAntlr, nil)
				}
			}
			if len(allProj) > 0 {
				proj := logical.NewProject(op, allProj, nil)
				// Mark outExpr slots as computed (need Value resolution).
				// Aggregate output and groupCol slots are column references
				// to the aggregate's output — NOT computed.
				computed := make([]bool, len(allProj))
				for i, e := range allAntlr {
					computed[i] = e != nil
				}
				proj.IsComputed = computed
				op = proj
				sq.postAggExprs = allAntlr
			}
		} else if len(keys) > 0 {
			hasSortOnly := false
			for _, ac := range sq.aggCols {
				if ac.sortOnly {
					hasSortOnly = true
					break
				}
			}
			var visibleProj []string
			var visibleAliases []string
			for _, ac := range sq.aggCols {
				if ac.sortOnly || ac.hidden {
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
					canonical := ac.aggFunc + "(" + arg + ")"
					visibleProj = append(visibleProj, canonical)
					alias := ""
					if ac.outName != "" && !strings.EqualFold(ac.outName, canonical) {
						alias = ac.outName
					}
					visibleAliases = append(visibleAliases, alias)
				} else if ac.groupCol != "" {
					visibleProj = append(visibleProj, ac.groupCol)
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
				if a != "" && i < len(visibleProj) {
					upper := strings.ToUpper(visibleProj[i])
					if strings.Contains(upper, "(") {
						hasAggAlias = true
						break
					}
				}
			}
			needsStrip := len(visibleProj) < totalOutput || hasAggAlias || hasSortOnly
			if needsStrip {
				if hasSortOnly {
					sq.postSortStripProj = visibleProj
					sq.postSortStripAliases = visibleAliases
				} else {
					op = logical.NewProject(op, visibleProj, visibleAliases)
				}
			}
		}
	}

	if len(sq.orderBy) > 0 {
		keys := make([]logical.SortKey, 0, len(sq.orderBy))
		for _, ob := range sq.orderBy {
			dir := logical.SortAsc
			if !ob.ascending {
				dir = logical.SortDesc
			}
			expr := ob.colName
			if expr == "" && ob.rawExpr != nil {
				expr = ob.rawExpr.GetText()
			}
			nullsFirst := ob.ascending
			if ob.nullsFirst != nil {
				nullsFirst = *ob.nullsFirst
			}
			keys = append(keys, logical.SortKey{Expr: expr, Dir: dir, NullsFirst: nullsFirst})
		}
		op = logical.NewSort(op, keys)
	}

	if len(sq.postSortStripProj) > 0 {
		op = logical.NewProject(op, sq.postSortStripProj, sq.postSortStripAliases)
	}

	// LIMIT: sq.limit < 0 means "no limit". Offset alone (LIMIT -1
	// OFFSET N) renders via LogicalLimit's negative-limit Offset(N)
	// branch.
	if sq.limit >= 0 || sq.offset > 0 {
		op = logical.NewLimit(op, sq.limit, sq.offset)
	}

	// Projection: skip when the projection is SELECT * (projCols is
	// nil per the selectQuery doc).
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

// canonicalTextOf renders an antlr context as source text. When
// possible (ctx is a ParserRuleContext with resolvable token
// positions), returns the ORIGINAL source text with whitespace
// intact — `WHERE id > 5` stays as `id > 5`, not `id>5`. Falls back
// to `GetText()` (token concatenation, whitespace stripped) when
// the context doesn't expose its token range.
//
// Until Phase 4.0 ports real QueryPredicates this is the surface
// LogicalFilter.PredicateText etc. carry into the Explain tree.
func canonicalTextOf(ctx any) string {
	if ctx == nil {
		return ""
	}
	// Fast path: parser rule context with start/stop tokens.
	if prc, ok := ctx.(antlr.ParserRuleContext); ok {
		start := prc.GetStart()
		stop := prc.GetStop()
		if start != nil && stop != nil && start.GetInputStream() != nil {
			return start.GetInputStream().GetTextFromInterval(
				antlr.NewInterval(start.GetStart(), stop.GetStop()),
			)
		}
	}
	// Fallback: GetText() concatenates tokens (no whitespace).
	if gt, ok := ctx.(interface{ GetText() string }); ok {
		return gt.GetText()
	}
	return ""
}

// buildLogicalPlanForDelete returns a LogicalDelete-rooted tree for
// a DELETE statement. Input is the parse-tree context; output wraps
// a LogicalScan(table) with an optional LogicalFilter (the WHERE).
// Returns nil on a malformed parse (missing table).
func buildLogicalPlanForDelete(del antlrgen.IDeleteStatementContext) logical.LogicalOperator {
	if del == nil || del.TableName() == nil {
		return nil
	}
	tableName := functions.FullIdToName(del.TableName().FullId())
	var scan logical.LogicalOperator = logical.NewScan(tableName, "")
	if w := del.WhereExpr(); w != nil {
		scan = logical.NewFilter(scan, canonicalTextOf(w))
	}
	return logical.NewDelete(tableName, scan)
}

// buildLogicalPlanForInsert returns a LogicalInsert-rooted tree for
// an INSERT statement. Two INSERT shapes:
//
//  1. `INSERT INTO t VALUES (…)` — Source is nil (values are not
//     represented as operators today). The rendered plan is just
//     `Insert(t[(col, col, …)])`.
//  2. `INSERT INTO t SELECT …` — Source is the nested SELECT's
//     logical plan when buildLogicalPlanForSelect succeeds.
//     Complex SELECT shapes (JOIN / CTE / …) cause the Source to
//     be nil, but the Insert node itself still renders.
//
// Returns nil on a malformed parse.
func buildLogicalPlanForInsert(ins antlrgen.IInsertStatementContext) logical.LogicalOperator {
	if ins == nil || ins.TableName() == nil {
		return nil
	}
	tableName := functions.FullIdToName(ins.TableName().FullId())

	var cols []string
	if colCtx := ins.UidListWithNestingsInParens(); colCtx != nil {
		if ul := colCtx.UidListWithNestings(); ul != nil {
			for _, uw := range ul.AllUidWithNestings() {
				if uw == nil || uw.Uid() == nil {
					continue
				}
				cols = append(cols, functions.StripIdentifierQuotes(uw.Uid().GetText()))
			}
		}
	}

	// INSERT … SELECT: try to build the inner SELECT's logical plan.
	// If the SELECT shape is out of the builder's scope the inner
	// plan is nil and the Insert renders without a Source subtree
	// (same behaviour as VALUES).
	var source logical.LogicalOperator
	if selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext); ok {
		if body := selCtx.QueryExpressionBody(); body != nil {
			if termDefault, ok := body.(*antlrgen.QueryTermDefaultContext); ok {
				if simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext); ok {
					if sq, err := extractFromSimpleTable(simpleTable); err == nil {
						source = buildLogicalPlanForSelect(sq)
					}
				}
			}
		}
	}

	return logical.NewInsert(tableName, cols, source)
}

// buildLogicalPlanForUpdate returns a LogicalUpdate-rooted tree for
// an UPDATE statement. SET assignments render as (col, expr-text)
// pairs; WHERE wraps the scan in a LogicalFilter. Returns nil on a
// malformed parse.
func buildLogicalPlanForUpdate(upd antlrgen.IUpdateStatementContext) logical.LogicalOperator {
	if upd == nil || upd.TableName() == nil {
		return nil
	}
	tableName := functions.FullIdToName(upd.TableName().FullId())
	var scan logical.LogicalOperator = logical.NewScan(tableName, "")
	if w := upd.WhereExpr(); w != nil {
		scan = logical.NewFilter(scan, canonicalTextOf(w))
	}
	var sets []logical.Assignment
	for _, el := range upd.AllUpdatedElement() {
		if el == nil || el.FullColumnName() == nil || el.Expression() == nil {
			continue
		}
		col := functions.FullIdToName(el.FullColumnName().FullId())
		// Strip the table-qualifier if present — UPDATE SET uses bare
		// col names at the logical level.
		if dot := strings.LastIndex(col, "."); dot >= 0 {
			col = col[dot+1:]
		}
		sets = append(sets, logical.Assignment{
			Column: col,
			Expr:   strings.TrimSpace(el.Expression().GetText()),
		})
	}
	return logical.NewUpdate(tableName, sets, scan)
}
