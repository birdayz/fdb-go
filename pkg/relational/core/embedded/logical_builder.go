package embedded

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// Phase 3 logical-plan builder — narrow-scope seed.
//
// Converts a parsed `*selectQuery` (the internal struct the embedded
// engine builds today) into a `logical.LogicalOperator` tree. This is
// the first bridge between the parse tree and the Phase 3 skeleton:
// `Plan.Explain()` on the naive Generator now returns a real plan
// tree instead of canonical SQL text, for the query shapes this
// builder recognises.
//
// **Scope (deliberately narrow):** single-table SELECT only. Returns
// nil for JOIN, derived-table, CTE, aggregate, GROUP BY, UNION, or
// DML. Those paths fall through to the canonical-SQL explain (the
// pre-existing Phase 1a placeholder). As the analyzer ports more
// shapes, this builder extends.
//
// Predicates + expressions are carried as canonical source text for
// now (LogicalFilter.PredicateText, LogicalProject.Projections).
// RFC-021 Phase 2 replaces those with real `Value` / `QueryPredicate`
// nodes from the cascades package — at which point the builder grows
// a translation pass from antlrgen.IExpressionContext to Value /
// QueryPredicate.
//
// Output tree shape (from innermost to outermost):
//
//	LogicalScan
//	  → LogicalFilter  (if WHERE)
//	    → LogicalSort  (if ORDER BY)
//	      → LogicalLimit (if LIMIT)
//	        → LogicalProject (unless SELECT *)

// buildLogicalPlanForSelect returns a LogicalOperator tree for the
// parsed selectQuery, or nil when the shape is out of current scope
// (JOIN / derived / CTE / aggregate / GROUP BY).
func buildLogicalPlanForSelect(sq *selectQuery) logical.LogicalOperator {
	if sq == nil {
		return nil
	}
	// Bail for shapes the builder doesn't yet handle. The caller falls
	// back to the canonical-SQL explain for these.
	if sq.derivedQuery != nil {
		return nil
	}
	if len(sq.joins) > 0 {
		return nil
	}
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return nil
	}
	if sq.tableName == "" {
		// SELECT without FROM — not a Scan; constant-row projection.
		// Future: a ValuesOperator. For now, bail.
		return nil
	}

	var op logical.LogicalOperator = logical.NewScan(sq.tableName, sq.tableAlias)

	if sq.whereExpr != nil {
		// Carry the canonical WHERE text — renders in Explain as
		// `Filter(<text>)`. Future: translate to QueryPredicate tree.
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
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
			keys = append(keys, logical.SortKey{Expr: expr, Dir: dir})
		}
		op = logical.NewSort(op, keys)
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
		for i, col := range sq.projCols {
			projs[i] = col
			if sq.projExprs != nil && i < len(sq.projExprs) && sq.projExprs[i] != nil {
				// Computed projection — render the expression text.
				projs[i] = strings.TrimSpace(sq.projExprs[i].GetText())
			}
			if sq.projAliases != nil && i < len(sq.projAliases) {
				aliases[i] = sq.projAliases[i]
			}
		}
		op = logical.NewProject(op, projs, aliases)
	}

	return op
}

// canonicalTextOf renders an antlr context as whitespace-collapsed
// source text. GetText() produces no-whitespace output (tokens
// concatenated); this is the seed's placeholder until Phase 4.0
// ports real QueryPredicates. Callers that need a more readable
// render can post-process.
func canonicalTextOf(ctx interface{ GetText() string }) string {
	if ctx == nil {
		return ""
	}
	return ctx.GetText()
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
