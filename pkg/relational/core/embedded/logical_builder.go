package embedded

import (
	"strings"

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
