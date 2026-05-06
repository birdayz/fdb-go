package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// SELECT statement dispatchers.
//
// Three entry points — each routes to the appropriate executor:
//
//   execSelect         driver-visible entrypoint. Handles WITH /
//                      WITH RECURSIVE clauses (materialises the CTEs
//                      first, pushing the CTE scope for shadowing),
//                      SET-query (UNION) dispatch, or plain-SELECT
//                      dispatch.
//   execSelectQuery    accepts a parsed selectQuery (post-parse) and
//                      picks the executor: constants-only (no FROM),
//                      derived-table materialisation, CTE scan,
//                      INFORMATION_SCHEMA handler, or the full
//                      execSelectQueryFull. Used by execSelect +
//                      execQueryBodyRows + UNION right-side.
//   execQueryBodyRows  utility wrapper that returns (cols, rows) for
//                      either a QueryTermDefaultContext or a
//                      SetQueryContext. Lets the UNION executor
//                      consume either shape uniformly.
//
// Small helpers:
//   stripCTEColumnQualifiers — `SELECT d.id FROM t AS d` yields a
//                              CTE with column `id`, not `d.id`.
//   containsTableRef         — parse-subtree search used to decide
//                              whether RECURSIVE actually self-
//                              references (RECURSIVE is a scope
//                              enabler, not a requirement).

// execQueryBodyRows executes a queryExpressionBody and returns
// (colNames, colTypes, rows). colTypes is parallel to colNames, with
// "" entries meaning "type unknown" — callers that materialize the
// result as a CTE / derived table propagate the types so a downstream
// SELECT against the materialised relation can report typed metadata.
// Handles both simple queries (QueryTermDefaultContext) and UNION (SetQueryContext).
func (c *EmbeddedConnection) execQueryBodyRows(ctx context.Context, body antlrgen.IQueryExpressionBodyContext) ([]string, []string, [][]driver.Value, error) {
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		sq, err := extractFromQueryTerm(b)
		if err != nil {
			return nil, nil, nil, err
		}
		rows, err := c.execSelectQuery(ctx, sq)
		if err != nil {
			return nil, nil, nil, err
		}
		sr := rows.(*staticRows)
		return sr.cols, sr.colTypes, sr.rows, nil
	case *antlrgen.SetQueryContext:
		r, err := c.execUnion(ctx, b)
		if err != nil {
			return nil, nil, nil, err
		}
		sr := r.(*staticRows)
		return sr.cols, sr.colTypes, sr.rows, nil
	default:
		return nil, nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported query expression type %T", body)
	}
}

// stripCTEColumnQualifiers returns the column list with any leading
// `alias.` qualifier removed from each name (taking the text after
// the LAST dot). CTE output schemas expose bare column names —
// `WITH x AS (SELECT d.id FROM t AS d)` yields a CTE with column
// `id`, not `d.id`, matching Postgres / SQL standard. If the inner
// query has two qualified projections that collapse to the same
// bare name (`SELECT a.v, b.v FROM …`) both columns keep their
// suffix form and downstream queries must use aliases to
// disambiguate — consistent with how regular SQL handles ambiguous
// projection names.
func stripCTEColumnQualifiers(cols []string) []string {
	out := make([]string, len(cols))
	for i, col := range cols {
		if dot := strings.LastIndex(col, "."); dot >= 0 {
			out[i] = col[dot+1:]
		} else {
			out[i] = col
		}
	}
	return out
}

// containsTableRef reports whether the parse subtree references a
// table with the given uppercase name. Used by the recursive CTE
// evaluator to decide whether a CTE body actually self-references —
// the RECURSIVE keyword is a scope enabler (matches Postgres), so a
// non-self-referencing body is evaluated on the non-recursive path.
func containsTableRef(tree antlr.Tree, upperName string) bool {
	if tree == nil {
		return false
	}
	if tn, ok := tree.(antlrgen.ITableNameContext); ok {
		if strings.ToUpper(functions.FullIdToName(tn.FullId())) == upperName {
			return true
		}
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		if containsTableRef(tree.GetChild(i), upperName) {
			return true
		}
	}
	return false
}

// execSelectQuery executes a parsed selectQuery and returns a driver.Rows.
// Extracted so execQueryBodyRows can call it without an ISelectStatementContext.
func (c *EmbeddedConnection) execSelectQuery(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	// Pre-evaluate every uncorrelated scalar subquery reachable from sq's
	// expressions BEFORE opening the outer FDB transaction. Each inner
	// subquery runs as its own top-level transaction; results are cached
	// and looked up per-row during the main scan. This avoids nested
	// FDB transactions (which misbehave — the outer cursor state gets
	// disturbed when the inner opens its own tx).
	if err := c.preEvaluateScalarSubqueries(ctx, sq); err != nil {
		return nil, err
	}

	// Plan-time constant fold of row-context-independent SELECT-list
	// expressions (`SELECT 1+2 FROM t`, `SELECT UPPER('hi'), price
	// FROM t`). Best-effort — slots that decline the walker or aren't
	// constant after cascades.values.SimplifyValue stay unset and fall through
	// to the per-row evaluator. Skipped for SELECT-without-FROM since
	// that path already evaluates each projExpr exactly once below.
	if sq.tableName != "" {
		foldConstantProjections(sq, c.cachedMetaData())
	}

	// SELECT without FROM: evaluate projExprs as constants and return one row.
	if sq.tableName == "" {
		cols := make([]string, len(sq.projCols))
		row := make([]driver.Value, len(sq.projCols))
		for i, col := range sq.projCols {
			name := sq.projAliases[i]
			if name == "" {
				name = col
			}
			cols[i] = name
			if sq.projExprs[i] != nil {
				v, err := evalExpr(ctx, c, nil, sq.projExprs[i])
				if err != nil {
					return nil, err
				}
				row[i] = v
			}
		}
		return &staticRows{cols: cols, rows: [][]driver.Value{row}}, nil
	}

	// Push a CTE scope when any derived-table aliases (first source OR
	// comma-joined / INNER-JOIN derived) need a sandboxed registry. The
	// scope keeps inner aliases from leaking out to the enclosing query
	// and ensures c.ctes is non-nil for the assignments below.
	hasJoinDerived := false
	for _, j := range sq.joins {
		if j.derivedQuery != nil {
			hasJoinDerived = true
			break
		}
	}
	if sq.derivedQuery != nil || hasJoinDerived {
		defer c.pushCTEScope()()
	}

	// Execute derived table query and register it as a temporary CTE.
	if sq.derivedQuery != nil {
		cols, colTypes, rows, err := c.execQueryBodyRows(ctx, sq.derivedQuery.QueryExpressionBody())
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidParameter,
				"derived table %q", sq.tableName)
		}
		// Reject duplicate output column names in the derived table's
		// projection (e.g. `SELECT a.*, a.* FROM a` which collapses
		// to id/name × 2). Java errors 42702 at the outer reference
		// because both sources of `id` are equally valid; Go surfaces
		// 22023 via the materialiser since the cte.cols list can't
		// disambiguate. Pinned by ambiguous_column.yaml.
		if len(cols) > 1 {
			seen := make(map[string]bool, len(cols))
			for _, col := range cols {
				key := col
				if dot := strings.LastIndex(col, "."); dot >= 0 {
					key = col[dot+1:]
				}
				key = strings.ToUpper(key)
				if seen[key] {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"derived table %q has duplicate column %q", sq.tableName, col)
				}
				seen[key] = true
			}
		}
		c.ctes[strings.ToUpper(sq.tableName)] = &cteData{cols: cols, colTypes: colTypes, rows: rows}
	}

	// Materialize comma-joined / INNER-JOINed derived sources the same
	// way as the first source. Each carries its own alias which becomes
	// the CTE key the join executor's scanTableToMaps already resolves.
	for _, j := range sq.joins {
		if j.derivedQuery == nil {
			continue
		}
		jcols, jcolTypes, jrows, jerr := c.execQueryBodyRows(ctx, j.derivedQuery.QueryExpressionBody())
		if jerr != nil {
			return nil, api.WrapErrorf(jerr, api.ErrCodeInvalidParameter,
				"derived table %q", j.alias)
		}
		c.ctes[strings.ToUpper(j.alias)] = &cteData{cols: jcols, colTypes: jcolTypes, rows: jrows}
	}

	// Check if the table name resolves to a CTE. Only route to the
	// CTE-only path when there are no joins — that path materialises
	// the one CTE's rows without looking at sq.joins, so a
	// comma-joined `SELECT ... FROM lo, hi` would drop the rhs. With
	// joins, fall through to execSelectQueryFull → execSelectJoin,
	// whose scanTableToMaps already resolves CTE names.
	if c.ctes != nil && len(sq.joins) == 0 {
		if cte, ok := c.ctes[strings.ToUpper(sq.tableName)]; ok {
			return c.execSelectFromCTE(ctx, sq, cte)
		}
	}

	// Route INFORMATION_SCHEMA.* queries to system table handlers.
	upper := strings.ToUpper(sq.tableName)
	if strings.HasPrefix(upper, "INFORMATION_SCHEMA.") {
		sysTable := upper[len("INFORMATION_SCHEMA."):]
		sysRows, sysErr := c.execSystemTable(ctx, sysTable, sq.whereExpr)
		if sysErr != nil {
			return nil, sysErr
		}
		return projectSystemRows(sysRows, sq)
	}

	if c.sess.Schema == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}
	// Delegate to the existing full implementation.
	return c.execSelectQueryFull(ctx, sq)
}

// execSelect executes a SELECT statement. Supports single-table and multi-table
// (INNER/LEFT JOIN) queries, WHERE, ORDER BY, GROUP BY, HAVING, LIMIT/OFFSET,
// aggregate functions, and INFORMATION_SCHEMA system tables.
func (c *EmbeddedConnection) execSelect(ctx context.Context, sel antlrgen.ISelectStatementContext) (driver.Rows, error) {
	query := sel.Query()
	if query == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "malformed SELECT statement")
	}

	// Materialize CTEs before routing the main query. Each WITH clause pushes
	// a CTE scope so inner nested queries with their own WITH do not clobber
	// the outer names, and outer scopes never see inner CTE names after the
	// nested query returns.
	if ctesCtx := query.Ctes(); ctesCtx != nil {
		defer c.pushCTEScope()()
		// Java's recursive-cte.yamsql accepts a trailing `TRAVERSAL ORDER
		// {pre_order | level_order | post_order}` clause. The default
		// (unspecified) is level_order — matches Java pre-4.7.1.0
		// behaviour. PRE_ORDER / POST_ORDER use DFS (Java 4.7.1.0+).
		traversalOrder := traversalLevelOrder
		if toc := ctesCtx.TraversalOrderClause(); toc != nil {
			switch {
			case toc.PRE_ORDER() != nil:
				traversalOrder = traversalPreOrder
			case toc.POST_ORDER() != nil:
				traversalOrder = traversalPostOrder
			case toc.LEVEL_ORDER() != nil:
				traversalOrder = traversalLevelOrder
			}
		}
		recursiveKeyword := ctesCtx.RECURSIVE() != nil
		for _, nq := range ctesCtx.AllNamedQuery() {
			cteName := strings.ToUpper(functions.FullIdToName(nq.GetName()))
			// Java alignment: duplicate CTE names in the same WITH list
			// error 42712 (DUPLICATE_ALIAS) per cte.yamsql. Detect before
			// overwriting so the error points at the second occurrence.
			if _, dup := c.ctes[cteName]; dup {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"duplicate CTE name %q in WITH clause", cteName)
			}
			// Column-rename list (`WITH name(c1, c2, ...) AS ...`) is
			// resolved once up-front so both the recursive and
			// non-recursive paths can apply it consistently. Recursive
			// CTEs need the renamed names INSIDE the iteration so the
			// recursive branch can reference the renamed columns
			// (e.g. `WITH RECURSIVE t(node, up) ... SELECT b.id, b.parent
			// FROM t AS a ... WHERE b.id = a.up`).
			var renameList []string
			if aliases := nq.GetColumnAliases(); aliases != nil {
				list := aliases.AllFullId()
				renameList = make([]string, len(list))
				for i, fid := range list {
					renameList[i] = functions.StripIdentifierQuotes(functions.FullIdToName(fid))
				}
			}
			var cteCols []string
			var cteRows [][]driver.Value
			var cteErr error
			body := nq.Query().QueryExpressionBody()
			// fdb-relational requires WITH RECURSIVE to actually
			// self-reference — non-self-referencing bodies under
			// RECURSIVE are rejected. SQL spec / Postgres treat
			// RECURSIVE as a scope enabler not a requirement, but per
			// the project conformance principle (doesn't work in Java
			// → doesn't work in Go), we reject too with the verbatim
			// Java message ("condition is not met!") so cross-engine
			// ExpectErrorMessage stays byte-equal across engines.
			var cteColTypes []string
			if recursiveKeyword && !containsTableRef(body, cteName) {
				// Java alignment: fdb-relational's SemanticAnalyzer
				// raises `condition is not met!` for any CTE under
				// RECURSIVE that doesn't actually self-reference (both
				// single-CTE and multi-CTE forms — verified via direct
				// probe against the Java conformance server, ,
				// ~1.2s response time per query when server is fresh).
				// SQL spec / Postgres treat RECURSIVE as a scope enabler
				// and would silently fall back to non-recursive
				// evaluation; per the project conformance principle we
				// reject too with the verbatim Java message.
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"condition is not met!")
			}
			if recursiveKeyword {
				cteCols, cteRows, cteErr = c.materializeRecursiveCTE(ctx, body, cteName, renameList, traversalOrder)
			} else {
				cteCols, cteColTypes, cteRows, cteErr = c.execQueryBodyRows(ctx, body)
				// Apply non-recursive rename here; the recursive path
				// handled it internally.
				if cteErr == nil && renameList != nil {
					if len(renameList) != len(cteCols) {
						return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
							"CTE %q column-rename has %d names but inner query has %d columns",
							cteName, len(renameList), len(cteCols))
					}
					cteCols = renameList
				} else if cteErr == nil {
					// Strip projection qualifiers from CTE output column
					// names: `SELECT d.id FROM t AS d` materialises a CTE
					// whose column is `id`, not `d.id`. Matches Postgres /
					// SQL standard where the CTE's output schema exposes
					// the bare column name (the inner alias is an internal
					// detail). Without this, `WITH x AS (SELECT d.id FROM
					// t AS d) SELECT id FROM x` errored 42703.
					cteCols = stripCTEColumnQualifiers(cteCols)
				}
			}
			if cteErr != nil {
				// Preserve the inner SQLSTATE (e.g. 42703 from a missing
				// column reference in a renamed outer CTE); otherwise
				// well-typed inner errors get masked as generic 22023.
				innerCode := api.ErrCodeInvalidParameter
				var apiErr *api.Error
				if errors.As(cteErr, &apiErr) {
					innerCode = apiErr.Code
				}
				return nil, api.WrapErrorf(cteErr, innerCode, "CTE %q", cteName)
			}
			c.ctes[cteName] = &cteData{cols: cteCols, colTypes: cteColTypes, rows: cteRows}
		}
	}

	if setQ, ok := query.QueryExpressionBody().(*antlrgen.SetQueryContext); ok {
		return c.execUnion(ctx, setQ)
	}
	sq, err := extractSelectParts(sel)
	if err != nil {
		return nil, err
	}
	return c.execSelectQuery(ctx, sq)
}
