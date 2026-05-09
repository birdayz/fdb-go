package embedded

import (
	"database/sql/driver"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// Parse-tree → selectQuery extraction.
//
// extractFromSimpleTable is the main entrypoint: an ANTLR
// SimpleTableContext walks out into a selectQuery describing every
// piece the executor needs (projection columns + aliases + computed
// expressions, FROM table / derived query, JOIN clauses, WHERE /
// HAVING predicates, GROUP BY keys + expressions, ORDER BY clauses
// including expression ORDER BY, LIMIT / OFFSET, DISTINCT, the
// count-star fast path, and aggregate columns — both bare in the
// SELECT list and harvested out of HAVING / ORDER BY).
//
// Supporting types + helpers live here too:
//   selectQuery / joinClause / orderByClause / aggSelectCol
//   extractSelectParts / extractFromQueryTerm
//   checkCountStar / extractAggFunc / extractAwfFields
//   columnNameFromExpr / selectExprToColumnName
//   exprReferencesColumn / harvestColumnRefs / harvestAggregates
//   aggColFromAwf / extractJoinClause / orderByLess
//
// Destined for pkg/relational/core/query/visitors/ per RFC 021
// Phase 1c. Phase 2 Cascades subsumes this into Logical* expression
// builders.

// countStarOutName returns the output column name for a COUNT(*)-only
// SELECT: the SELECT-list `AS alias` when present, otherwise the
// canonical reconstruction "COUNT(*)". Used at every emission site so
// derived tables, UNION arity, and caller projections see the aliased
// name instead of the canonical form.
func countStarOutName(sq *selectQuery) string {
	if sq.countStarAlias != "" {
		return sq.countStarAlias
	}
	return "COUNT(*)"
}

// selectQuery holds the parsed components of a SELECT statement.
type selectQuery struct {
	// selectClassification holds all SELECT-list, GROUP BY, HAVING,
	// ORDER BY, and aggregate classification fields. Embedded so that
	// sq.projCols, sq.aggCols, etc. continue to work as before.
	selectClassification

	tableName  string
	tableAlias string // alias or tableName if no alias given
	whereExpr  antlrgen.IWhereExprContext
	// limit < 0 means no limit.
	limit int64
	// offset >= 0 means skip that many rows after sort/group (OFFSET n).
	offset int64
	// joins describes JOIN clauses (nil = no joins).
	joins []joinClause
	// derivedQuery is non-nil when the FROM clause is a subquery (derived table).
	// When set, tableName holds the alias; the query is materialized at execution time.
	derivedQuery antlrgen.IQueryContext
	// projConstFolded is parallel to projExprs (populated lazily by
	// foldConstantProjections from execSelectQuery). A slot with
	// present=true means the expression was determined to be row-
	// context-independent at plan time; its precomputed Go value lives
	// in `value` and per-row consumers must use it instead of calling
	// evalExpr. Slots stay zero-valued for expressions that touched a
	// FieldValue, declined the walker, or weren't constant after
	// SimplifyValue. Empty slice = pass not run yet.
	projConstFolded []projectionFold
}

// joinClause describes a single JOIN part in a SELECT query.
type joinClause struct {
	tableName string
	joinType  string // "INNER", "LEFT", "RIGHT"
	alias     string
	onExpr    antlrgen.IExpressionContext
	// derivedQuery is set when the join's right-hand source is a
	// subquery (`... , (SELECT ...) AS x` or `INNER JOIN (SELECT ...)
	// AS x ON ...`). The dispatcher materializes the subquery as a
	// CTE keyed by `alias` before the join executor runs, mirroring
	// the first-source derived-table handling.
	derivedQuery antlrgen.IQueryContext
	// catalogAwareInnerPlan is set by the catalog-aware builder when
	// it pre-builds the derived table's inner plan with upgraded
	// predicates. When non-nil, buildLogicalPlanForSelect uses this
	// instead of calling buildLogicalPlanForSelect recursively.
	catalogAwareInnerPlan logical.LogicalOperator
}

type orderByClause struct {
	colName   string
	ascending bool
	// nullsFirst overrides the Java-default NULL ordering when the user
	// specifies NULLS FIRST / NULLS LAST explicitly. nil = use the
	// direction-implied default (ASC → NULLS FIRST, DESC → NULLS LAST,
	// per ParseHelpers.isNullsLast). true = NULLS FIRST, false =
	// NULLS LAST.
	nullsFirst *bool
	// expr is non-nil for ORDER BY on a non-trivial expression (e.g.
	// `ORDER BY UPPER(name)`, `ORDER BY price * qty`). When set, colName is
	// empty and the expression is evaluated per row at sort time. Only the
	// CTE and JOIN paths (which retain map rows) honor this; the proto /
	// single-table scan path still requires a column/aggregate name.
	expr antlrgen.IExpressionContext
	// rawExpr always holds the original IExpressionContext for the ORDER BY
	// item, even when colName is populated. Used by post-parse passes that
	// need to inspect the expression (e.g. harvesting aggregates from
	// `ORDER BY SUM(v)` where colName resolved to "SUM(v)" and expr was
	// left nil because the expression was a bare aggregate).
	rawExpr antlrgen.IExpressionContext
}

// orderByLess returns true iff value `a` sorts before value `b` under the
// given ORDER BY clause, honouring explicit NULLS FIRST / NULLS LAST and
// falling back to the direction-implied default when unspecified. Returns
// false for equal values — the caller's outer loop advances to the next
// sort key.
func orderByLess(a, b driver.Value, ob orderByClause) (less, equal bool) {
	if a == nil && b == nil {
		return false, true
	}
	if a == nil || b == nil {
		nullsFirst := ob.ascending // Default: ASC → NULLS FIRST, DESC → NULLS LAST.
		if ob.nullsFirst != nil {
			nullsFirst = *ob.nullsFirst
		}
		if a == nil {
			return nullsFirst, false
		}
		return !nullsFirst, false
	}
	cmp := functions.CompareValues(a, b)
	if cmp == 0 {
		return false, true
	}
	if ob.ascending {
		return cmp < 0, false
	}
	return cmp > 0, false
}

// aggSelectCol describes one column in a GROUP BY aggregate SELECT list.
type aggSelectCol struct {
	outName string // output column name
	// Exactly one of groupCol / aggFunc / outExpr is set (non-visible entries
	// harvested from HAVING/ORDER BY always have aggFunc set).
	groupCol string // plain group-by column reference
	aggFunc  string // COUNT/SUM/MIN/MAX/AVG
	aggArg   string // argument column name — set only when arg is a bare column; used for the proto-path FD fast path. Empty for COUNT(*) and for expression args.
	// aggExpr is the IExpressionContext of the aggregate's argument when it is not a bare
	// column reference (e.g. SUM(qty*price), AVG(CASE ... END)). Evaluated per input row.
	// nil for bare-column args and for COUNT(*).
	aggExpr     antlrgen.IExpressionContext
	aggDistinct bool // true when COUNT(DISTINCT col)
	// visible is true when the aggregate appears in the user's SELECT list.
	// Non-visible entries are harvested from HAVING or ORDER BY — they
	// contribute to accumulation/evaluation but are excluded from (or
	// stripped after) the projected output.
	visible bool
	// outExpr is a post-aggregation expression that references aggregate
	// outputs and/or group-by columns. Evaluated at emit time against a
	// rowMap that already contains all aggCols values. Used for SELECT-list
	// shapes like `SUM(a) + SUM(b)` or `COALESCE(SUM(v), 0)`. When set,
	// aggFunc / groupCol are empty and the row's value comes from evaluating
	// outExpr rather than reading an aggregator slot.
	outExpr antlrgen.IExpressionContext
}

// checkCountStar returns true if e is a bare COUNT(*) expression.
func checkCountStar(e *antlrgen.SelectExpressionElementContext) bool {
	pred, ok := e.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return false
	}
	fc, ok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !ok {
		return false
	}
	agg, ok := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
	if !ok {
		return false
	}
	awf, ok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
	if !ok {
		return false
	}
	return awf.COUNT() != nil && awf.STAR() != nil
}

// extractAggFunc attempts to parse an aggregate function (COUNT/SUM/MIN/MAX/AVG)
// from a SelectExpressionElementContext. Returns (funcName, argColName, argExpr, alias, distinct, ok).
// funcName is upper-case.
// argColName is non-empty when the argument is a bare column reference (enables the
// proto-path FD fast path). argExpr is non-nil when the argument is an arbitrary
// expression (e.g. SUM(qty*price)) — mutually exclusive with argColName.
// Both are empty/nil for COUNT(*).
//
// Shares the AggregateWindowedFunction → (funcName, argCol, argExpr, outName)
// extraction with aggColFromAwf via extractAwfFields; this wrapper adds the
// SELECT-list element unwrap + the alias-from-AS overlay.
func extractAggFunc(e *antlrgen.SelectExpressionElementContext) (funcName, argCol string, argExpr antlrgen.IExpressionContext, alias string, distinct, ok bool) {
	pred, pok := e.Expression().(*antlrgen.PredicatedExpressionContext)
	if !pok {
		return "", "", nil, "", false, false
	}
	fc, fcok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !fcok {
		return "", "", nil, "", false, false
	}
	agg, aggok := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
	if !aggok {
		return "", "", nil, "", false, false
	}
	awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
	if !awfok {
		return "", "", nil, "", false, false
	}
	fn, arg, aExpr, outName, isDistinct, fieldsOk := extractAwfFields(awf)
	if !fieldsOk {
		return "", "", nil, "", false, false
	}
	// SELECT-list-only overlay: an explicit `AS alias` on the SELECT element
	// wins over the reconstructed default ("SUM(v)") as the output column
	// name.
	if e.Uid() != nil {
		outName = functions.StripIdentifierQuotes(e.Uid().GetText())
	}
	return fn, arg, aExpr, outName, isDistinct, true
}

// extractAwfFields classifies an AggregateWindowedFunction into the pieces
// every caller needs: the function name, the argument (bare column vs
// arbitrary expression), the DISTINCT flag, and the default output name
// used by both the SELECT-list alias path and the HAVING resolver's
// lookup name. Shared by extractAggFunc (SELECT-list aggregates) and
// aggColFromAwf (HAVING-harvested aggregates). Returns false when the
// AWF doesn't match any of the five supported aggregates.
//
// DISTINCT aggregates (`COUNT(DISTINCT col)`, `SUM(DISTINCT col)`,
// `MIN(DISTINCT col)`, `MAX(DISTINCT col)`, `AVG(DISTINCT col)`) are
// intentionally rejected via the parser path's distinct flag (caller
// raises ErrCodeUnsupportedOperation before any execution). fdb-
// relational 4.11.1.0's parser visitor NPEs on every aggregate with
// DISTINCT (`AggregateWindowedFunctionContext.ALL().getText()` is
// called unconditionally; ALL is null when DISTINCT is present, per
// CLAUDE.md gotcha "COUNT(DISTINCT col) NPEs in fdb-relational"). Go
// matches by surfacing distinct=true to callers, which then reject.
// Same architectural reason in both engines: visitor doesn't handle
// the DISTINCT case.
func extractAwfFields(awf *antlrgen.AggregateWindowedFunctionContext) (funcName, argCol string, argExpr antlrgen.IExpressionContext, outName string, distinct, ok bool) {
	distinct = awf.DISTINCT() != nil
	resolveArg := func(fa antlrgen.IFunctionArgContext) {
		if fa == nil {
			return
		}
		expr := fa.Expression()
		if pred, ok := expr.(*antlrgen.PredicatedExpressionContext); ok {
			if col, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
				argCol = functions.FullIdToName(col.FullColumnName().FullId())
				return
			}
		}
		argExpr = expr
	}
	switch {
	case awf.COUNT() != nil && awf.STAR() != nil:
		funcName = "COUNT"
	case awf.COUNT() != nil:
		funcName = "COUNT"
		if awf.FunctionArg() != nil {
			resolveArg(awf.FunctionArg())
		} else if awf.FunctionArgs() != nil && len(awf.FunctionArgs().AllFunctionArg()) > 0 {
			// COUNT(DISTINCT col|expr) — FunctionArgs variant
			resolveArg(awf.FunctionArgs().AllFunctionArg()[0])
		}
	case awf.SUM() != nil:
		funcName = "SUM"
		resolveArg(awf.FunctionArg())
	case awf.MIN() != nil:
		funcName = "MIN"
		resolveArg(awf.FunctionArg())
	case awf.MAX() != nil:
		funcName = "MAX"
		resolveArg(awf.FunctionArg())
	case awf.AVG() != nil:
		funcName = "AVG"
		resolveArg(awf.FunctionArg())
	default:
		return "", "", nil, "", false, false
	}
	display := argCol
	if display == "" && argExpr != nil {
		display = canonicalTextOf(argExpr)
	}
	switch {
	case display == "":
		outName = funcName + "(*)"
	case distinct:
		outName = funcName + "(DISTINCT " + display + ")"
	default:
		outName = funcName + "(" + display + ")"
	}
	return funcName, argCol, argExpr, outName, distinct, true
}

// columnNameFromExpr extracts a plain column name (or aggregate output name like
// "COUNT(*)") from an IExpressionContext.
// context is used in error messages (e.g. "SELECT expression", "ORDER BY expression").
func columnNameFromExpr(expr antlrgen.IExpressionContext, context string) (string, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s must be a column name, got %T", context, expr)
	}
	// `b IS TRUE`, `x IN (...)`, `s LIKE 'a%'`, `n BETWEEN 1 AND 10` all
	// parse as PredicatedExpression with both an atom AND a predicate.
	// These are NOT plain column references — the predicate transforms
	// the value. Force callers to take the expression-evaluation path
	// instead of treating it as a bare column lookup.
	if pred.Predicate() != nil {
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s contains a predicate, not a plain column", context)
	}
	switch a := pred.ExpressionAtom().(type) {
	case *antlrgen.FullColumnNameExpressionAtomContext:
		return functions.FullIdToName(a.FullColumnName().FullId()), nil
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Aggregate function in ORDER BY / HAVING — reuse extractAwfFields
		// so the canonical output name matches what aggCols registration
		// produces (column-ref args fold via FullIdToName; bare-expression
		// args use GetText). Without sharing the helper the two sides
		// drift on case-folding and the colIdx lookup misses on shapes
		// like `ORDER BY SUM(v)`.
		agg, aggok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
		if !aggok {
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"%s: unsupported function call %T", context, a.FunctionCall())
		}
		awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
		if !awfok {
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"%s: unsupported aggregate %T", context, agg.AggregateWindowedFunction())
		}
		_, _, _, outName, _, ok := extractAwfFields(awf)
		if !ok {
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation, "%s: unsupported aggregate function", context)
		}
		return outName, nil
	default:
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s must be a column name, got expression atom %T", context, pred.ExpressionAtom())
	}
}

// selectExprToColumnName extracts a plain column name and optional alias from a
// SelectExpressionElementContext. Returns (colName, alias, error).
func selectExprToColumnName(e *antlrgen.SelectExpressionElementContext) (string, string, error) {
	colName, err := columnNameFromExpr(e.Expression(), "SELECT expression")
	if err != nil {
		return "", "", err
	}
	alias := ""
	if e.Uid() != nil {
		alias = functions.StripIdentifierQuotes(e.Uid().GetText())
	}
	return colName, alias, nil
}

// extractSelectParts navigates the parse tree of a SELECT statement.
// Supports SELECT [* | col, ...] FROM <table> [WHERE col = val]
//
//	[ORDER BY col [ASC|DESC], ...] [LIMIT n].
//
// Joins, subqueries, aliases, GROUP BY, HAVING, etc. are not supported.
func extractSelectParts(sel antlrgen.ISelectStatementContext) (*selectQuery, error) {
	query := sel.Query()
	if query == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "malformed SELECT statement")
	}
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported SELECT form %T; only simple SELECT FROM <table> is supported",
			query.QueryExpressionBody())
	}
	return extractFromQueryTerm(body)
}

func extractFromQueryTerm(body *antlrgen.QueryTermDefaultContext) (*selectQuery, error) {
	simpleTable, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported query term %T; only simple SELECT FROM <table> is supported",
			body.QueryTerm())
	}
	return extractFromSimpleTable(simpleTable)
}

// selectClassification holds the classified SELECT-list elements,
// GROUP BY keys, HAVING expression, ORDER BY clauses, and all the
// reclassification/harvest results. It contains everything
// extractFromSimpleTable produces EXCEPT the FROM-derived fields
// (tableName, tableAlias, joins, derivedQuery, whereExpr) and the
// execution-only fields (projConstFolded, limit, offset).
//
// Embedded by selectQuery so the classification fields are promoted —
// code that reads sq.projCols, sq.aggCols, etc. works unchanged.
// PlanVisitor constructs a selectQuery directly from a
// selectClassification + fromSource without any bridge method.
type selectClassification struct {
	projCols    []string // nil = SELECT * or SELECT <qualifier>.*; ignored when countStar or aggCols non-empty
	projAliases []string // parallel to projCols; empty string = no alias (use column name)
	// projExprs holds computed projection expressions parallel to projCols.
	// Non-nil entry overrides the plain column lookup for that position.
	projExprs          []antlrgen.IExpressionContext
	projStarQualifiers []string
	// projQualifier is set when SELECT list is exactly `<qualifier>.*`.
	// Projection restricts to columns from the source whose alias (or
	// table name when no alias) equals projQualifier. Empty = SELECT *
	// (all sources) or explicit column list.
	projQualifier  string
	countStar      bool // true when SELECT list is exactly COUNT(*)
	countStarAlias string
	aggCols        []aggSelectCol
	distinct       bool // true when SELECT DISTINCT
	orderBy        []orderByClause
	// groupBy holds GROUP BY column names (nil = no GROUP BY). When an entry
	// is an expression (e.g. `GROUP BY amt + 1`), groupBy[i] holds the raw
	// expression text as a synthetic display key and groupByExprs[i] holds
	// the IExpressionContext evaluated per row to derive the group key value.
	groupBy []string
	// groupByExprs is parallel to groupBy. nil entry = bare column (fast path
	// via field-descriptor / map lookup); non-nil = evaluate per row/message.
	groupByExprs []antlrgen.IExpressionContext
	// groupByAliases maps UPPERCASE `GROUP BY col AS alias` alias names to
	// their index in groupBy. Used at parse time to resolve SELECT-list
	// references to a GROUP BY alias (`SELECT x FROM t GROUP BY col1 AS x`)
	// — the SELECT-list column gets rewritten to the underlying group-by
	// name with the alias preserved as the output column name. Nil = no
	// aliased GROUP BY entries.
	groupByAliases map[string]int
	// havingExpr is the HAVING clause expression (nil = no HAVING).
	havingExpr antlrgen.IExpressionContext
	// postAggExprs is populated by the visitor's visitSelectGroupBy when
	// post-aggregation computed projections are emitted.
	postAggExprs []antlrgen.IExpressionContext
	// postSortStripProj / postSortStripAliases are populated by the
	// visitor's visitSelectGroupBy when non-visible aggregate columns
	// need stripping after the Sort operator.
	postSortStripProj    []string
	postSortStripAliases []string
}

// selectQueryFromClassification builds a selectQuery from the
// classification and the FROM-derived fields. The classification is
// embedded by value (slices share backing arrays).
func selectQueryFromClassification(cls *selectClassification, fs *fromSource) *selectQuery {
	sq := &selectQuery{
		selectClassification: *cls,
		limit:                -1,
	}
	if fs != nil {
		sq.tableName = fs.tableName
		sq.tableAlias = fs.tableAlias
		sq.joins = fs.joins
		sq.derivedQuery = fs.derivedQuery
		sq.whereExpr = fs.whereExpr
	}
	return sq
}

// classifySelectElements walks the SELECT, GROUP BY, HAVING, ORDER BY,
// and LIMIT clauses of a SimpleTableContext and returns a
// selectClassification. This is the pure parse-tree classification
// logic extracted from extractFromSimpleTable — it does NOT parse the
// FROM clause. Both extractFromSimpleTable (proto path) and
// PlanVisitor.VisitSimpleTable (Cascades path) delegate here.
func classifySelectElements(simpleTable *antlrgen.SimpleTableContext) (*selectClassification, error) {
	// Parse SELECT list: either *, a list of column name expressions, COUNT(*), or
	// a GROUP BY aggregate list (mix of group-by columns + aggregate functions).
	selElems := simpleTable.SelectElements()
	var projCols []string                       // nil = SELECT * or SELECT <qualifier>.*
	var projAliases []string                    // parallel to projCols
	var projExprs []antlrgen.IExpressionContext // parallel to projCols; nil entry = plain column
	var projStarQualifiers []string             // parallel to projCols; non-empty = <qualifier>.* slot
	var countStar bool
	var countStarAlias string
	var aggCols []aggSelectCol
	var projQualifier string // non-empty when SELECT list is *only* <qualifier>.*
	// Snapshots of projAliases / projExprs taken right after the SELECT
	// element loop, before any reclassification clears them. Downstream
	// GROUP BY / ORDER BY parsers consult these to resolve alias
	// references (e.g. `GROUP BY bucket` where bucket is `v/10 AS bucket`).
	var selectAliasesSnapshot []string
	var selectExprsSnapshot []antlrgen.IExpressionContext
	if selElems != nil {
		elems := selElems.AllSelectElement()
		for _, elem := range elems {
			switch e := elem.(type) {
			case *antlrgen.SelectStarElementContext:
				if len(elems) > 1 {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"cannot mix * with named columns in SELECT list")
				}
				// SELECT * — projCols stays nil
			case *antlrgen.SelectQualifierStarElementContext:
				// SELECT <qualifier>.* either alone or mixed with named
				// columns. Alone: use the legacy projQualifier / nil-projCols
				// path. Mixed: record as a star slot in projCols to be
				// expanded at execution time against the FROM sources.
				if e.Uid() == nil {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"SELECT <qualifier>.* missing qualifier")
				}
				qual := functions.StripIdentifierQuotes(e.Uid().GetText())
				if len(elems) == 1 {
					projQualifier = qual
				} else {
					projCols = append(projCols, "") // sentinel; actual names resolved at execution
					projAliases = append(projAliases, "")
					projExprs = append(projExprs, nil)
					projStarQualifiers = append(projStarQualifiers, qual)
				}
			case *antlrgen.SelectExpressionElementContext:
				if checkCountStar(e) && len(elems) == 1 {
					countStar = true
					if e.Uid() != nil {
						countStarAlias = functions.StripIdentifierQuotes(e.Uid().GetText())
					}
				} else if fn, argCol, argExpr, alias, isDistinct, isAgg := extractAggFunc(e); isAgg {
					aggCols = append(aggCols, aggSelectCol{outName: alias, aggFunc: fn, aggArg: argCol, aggExpr: argExpr, aggDistinct: isDistinct, visible: true})
				} else {
					colName, alias, nameErr := selectExprToColumnName(e)
					var expr antlrgen.IExpressionContext
					if nameErr != nil {
						// Not a plain column name — treat as a computed
						// expression. The internal column name uses
						// either the user-given AS alias (preserves the
						// user's chosen identifier) or the raw expression
						// text as a unique-per-slot internal token. Keep
						// `alias` empty when no user alias was provided
						// — downstream projection-binding distinguishes
						// "user gave an alias" from "we fabricated a
						// name" via this empty-string convention. The
						// JDBC name layer (jdbcColumnName) emits "_N"
						// for anonymous-computed slots.
						alias = ""
						if e.Uid() != nil {
							alias = functions.StripIdentifierQuotes(e.Uid().GetText())
						}
						if alias != "" {
							colName = alias
						} else {
							colName = canonicalTextOf(e.Expression())
						}
						expr = e.Expression()
					}
					if len(aggCols) > 0 {
						// Mixed aggregate query. Three classifications for
						// the trailing SELECT element based on what the
						// expression references:
						//   - wraps aggregates → harvest any novel inner
						//     aggregates (add as non-visible accumulators)
						//     and route the expression itself to outExpr.
						//   - constant-only (no columns) → outExpr so it's
						//     emitted once per group like SUM does.
						//   - bare column or column-only expression →
						//     group-by reference.
						outName := func() string {
							if alias != "" {
								return alias
							}
							return colName
						}()
						switch {
						case expr != nil && len(harvestAggregates(expr)) > 0:
							// Harvest aggregates that aren't already
							// accumulated. `SELECT SUM(a), SUM(b)+1`:
							// SUM(a) is already in aggCols (bare), SUM(b)
							// is novel — must be added as non-visible so
							// the rowMap at emit time has SUM(b) available
							// for outExpr evaluation. Dedup by outName.
							existingNames := make(map[string]struct{}, len(aggCols))
							for _, ac := range aggCols {
								existingNames[ac.outName] = struct{}{}
							}
							for _, h := range harvestAggregates(expr) {
								if _, seen := existingNames[h.outName]; seen {
									continue
								}
								// h.visible stays false — inner aggregate not in user's SELECT list.
								aggCols = append(aggCols, h)
								existingNames[h.outName] = struct{}{}
							}
							aggCols = append(aggCols, aggSelectCol{outName: outName, outExpr: expr, visible: true})
						case expr != nil && !exprReferencesColumn(expr):
							aggCols = append(aggCols, aggSelectCol{outName: outName, outExpr: expr, visible: true})
						case expr != nil:
							// Expression references columns but contains no
							// aggregates. Java permits this when the columns
							// are all in GROUP BY (the expression value is
							// constant per group, e.g. `SELECT a+b FROM t
							// GROUP BY a, b`). Route to outExpr so it's
							// evaluated post-aggregation against the rowMap
							// (which holds group-by column values). If the
							// expression touches a column NOT in GROUP BY,
							// the rowMap lookup errors at emit time with
							// "column not in row" — close to SQL standard's
							// 42803 grouping_error.
							aggCols = append(aggCols, aggSelectCol{outName: outName, outExpr: expr, visible: true})
						default:
							aggCols = append(aggCols, aggSelectCol{outName: outName, groupCol: colName, visible: true})
						}
					} else {
						projCols = append(projCols, colName)
						projAliases = append(projAliases, alias)
						projExprs = append(projExprs, expr) // nil when it's a plain column
						projStarQualifiers = append(projStarQualifiers, "")
					}
				}
			default:
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported SELECT element type %T", elem)
			}
		}
		// SELECT-list expressions that wrap aggregate function calls (e.g.
		// `SUM(a) + SUM(b)`, `COALESCE(SUM(v), 0)`, `CASE WHEN COUNT(*)>0
		// THEN 'yes' ELSE 'no' END`) don't match extractAggFunc at the
		// top level, so they land in projExprs with projCols[i] holding
		// the expression text. Promote each such slot to an aggSelectCol
		// with an outExpr (evaluated post-aggregation against the rowMap),
		// harvest the referenced aggregates as non-visible accumulators, and
		// drop the slot from projCols. Has to happen before the plain-col
		// reclassification below so those slots aren't treated as
		// group-by references.
		if len(projCols) > 0 {
			var newProjCols []string
			var newProjAliases []string
			var newProjExprs []antlrgen.IExpressionContext
			var newStarQualifiers []string
			var promoted []aggSelectCol
			existing := make(map[string]struct{}, len(aggCols))
			for _, ac := range aggCols {
				existing[ac.outName] = struct{}{}
			}
			for i, col := range projCols {
				if i >= len(projExprs) || projExprs[i] == nil {
					newProjCols = append(newProjCols, col)
					newProjAliases = append(newProjAliases, projAliases[i])
					newProjExprs = append(newProjExprs, projExprs[i])
					if i < len(projStarQualifiers) {
						newStarQualifiers = append(newStarQualifiers, projStarQualifiers[i])
					} else {
						newStarQualifiers = append(newStarQualifiers, "")
					}
					continue
				}
				harvested := harvestAggregates(projExprs[i])
				if len(harvested) == 0 {
					newProjCols = append(newProjCols, col)
					newProjAliases = append(newProjAliases, projAliases[i])
					newProjExprs = append(newProjExprs, projExprs[i])
					if i < len(projStarQualifiers) {
						newStarQualifiers = append(newStarQualifiers, projStarQualifiers[i])
					} else {
						newStarQualifiers = append(newStarQualifiers, "")
					}
					continue
				}
				for _, h := range harvested {
					if _, seen := existing[h.outName]; seen {
						continue
					}
					existing[h.outName] = struct{}{}
					// h.visible stays false — inner aggregate not in user's SELECT list.
					promoted = append(promoted, h)
				}
				outName := projAliases[i]
				if outName == "" {
					outName = col
				}
				promoted = append(promoted, aggSelectCol{outName: outName, outExpr: projExprs[i], visible: true})
			}
			if len(promoted) > 0 {
				projCols = newProjCols
				projAliases = newProjAliases
				projExprs = newProjExprs
				projStarQualifiers = newStarQualifiers
				aggCols = append(aggCols, promoted...)
			}
		}
		// Snapshot the original SELECT-list alias/expr arrays before any
		// reclassification clears them.
		selectAliasesSnapshot = append([]string(nil), projAliases...)
		selectExprsSnapshot = append([]antlrgen.IExpressionContext(nil), projExprs...)
		// If we found aggregate functions mixed with plain columns, the plain cols
		// that were added to projCols before the first aggregate need to be re-
		// classified. Bare columns become group-by references; expressions with
		// no column refs (literal constants like `SELECT 1, SUM(v)`) become
		// outExpr slots so they're emitted once per group without requiring
		// a GROUP BY clause or a field-descriptor lookup. Star slots can't be
		// demoted either way. Note: the GROUP BY / HAVING parsers haven't run
		// yet at this point, so we can't redirect groupCol to match a GROUP
		// BY expression here — that lookup happens in the HAVING-harvest
		// reclassification later when sq.groupBy is populated.
		if len(aggCols) > 0 && len(projCols) > 0 {
			for _, q := range projStarQualifiers {
				if q != "" {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"cannot mix qualifier.* with aggregate functions in SELECT list")
				}
			}
			extra := make([]aggSelectCol, len(projCols))
			for i, c := range projCols {
				out := projAliases[i]
				if out == "" {
					out = c
				}
				var slotExpr antlrgen.IExpressionContext
				if i < len(projExprs) {
					slotExpr = projExprs[i]
				}
				switch {
				case slotExpr != nil && !exprReferencesColumn(slotExpr):
					extra[i] = aggSelectCol{outName: out, outExpr: slotExpr, visible: true}
				case slotExpr != nil:
					// Expression on group-by columns (no aggregates, no
					// constants-only). Java permits this when all referenced
					// columns are in GROUP BY. Route to outExpr — evaluated
					// post-aggregation against the rowMap holding group-by
					// values. Symmetric with the in-SELECT-loop case at the
					// mixed-agg classification site above.
					extra[i] = aggSelectCol{outName: out, outExpr: slotExpr, visible: true}
				default:
					extra[i] = aggSelectCol{outName: out, groupCol: c, visible: true}
				}
			}
			aggCols = append(extra, aggCols...)
			projCols = nil
			projAliases = nil
			projExprs = nil
			projStarQualifiers = nil
		}
	}

	cls := &selectClassification{
		projCols:           projCols,
		projAliases:        projAliases,
		projExprs:          projExprs,
		projStarQualifiers: projStarQualifiers,
		projQualifier:      projQualifier,
		countStar:          countStar,
		countStarAlias:     countStarAlias,
		aggCols:            aggCols,
		distinct:           simpleTable.DISTINCT() != nil,
	}

	// Parse ORDER BY clause.
	orderByClauseCtx := simpleTable.OrderByClause()
	if orderByClauseCtx != nil {
		// Java errors 42701 (COLUMN_ALREADY_EXISTS) on `ORDER BY b, b`
		// with the same column repeated. Stricter than Postgres, but
		// per dayshift-40's 100% Java-alignment direction we match.
		// Expression entries (without a resolved colName) are not
		// deduped because two identical expressions are syntactically
		// distinct sort keys (e.g. `ORDER BY a+b, a+b` — Java accepts).
		seenOrderCols := make(map[string]bool)
		for _, obExpr := range orderByClauseCtx.AllOrderByExpression() {
			ascending := true
			var nullsFirst *bool
			if oc := obExpr.OrderClause(); oc != nil {
				if oc.DESC() != nil {
					ascending = false
				}
				// NULLS FIRST / NULLS LAST overrides the direction-implied
				// default. Grammar: orderClause: (ASC|DESC)? (NULLS (FIRST|LAST))?
				if oc.NULLS() != nil {
					f := oc.FIRST() != nil
					nullsFirst = &f
				}
			}
			// Handle positional references `ORDER BY N` (SQL-92): N is a
			// 1-indexed position into the SELECT list. Resolve to the
			// matching output column's name so the downstream colIdx
			// lookup in the sort path works uniformly.
			posName, isPos, posErr := resolveSelectListPosition("ORDER BY", obExpr.Expression(), projCols, projAliases, aggCols)
			if posErr != nil {
				return nil, posErr
			}
			if isPos {
				// Dedup key is case-folded (SQL identifiers are
				// case-insensitive): `ORDER BY 1, 1` is a dup regardless of
				// case in any resolved column name.
				key := strings.ToUpper(posName)
				if seenOrderCols[key] {
					return nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists,
						"duplicate column %q in ORDER BY", posName)
				}
				seenOrderCols[key] = true
				cls.orderBy = append(cls.orderBy, orderByClause{colName: posName, ascending: ascending, nullsFirst: nullsFirst, rawExpr: obExpr.Expression()})
				continue
			}
			// Prefer plain column / aggregate lookup (works in all sort paths,
			// including the proto single-table path). Fall back to storing the
			// expression for CTE / JOIN sort keys like `ORDER BY a + b`.
			colName, nameErr := columnNameFromExpr(obExpr.Expression(), "ORDER BY expression")
			if nameErr == nil {
				// SQL identifiers are case-insensitive, so `ORDER BY b, B`
				// is a dup. Dot-qualified names fold each segment the same
				// way — `ORDER BY t.x, T.X` dups as well. Unqualified-vs-
				// qualified (`ORDER BY t.x, x`) stay distinct because the
				// strings differ — that matches Java's behavior (requires
				// alias resolution for true dedup, which happens later).
				key := strings.ToUpper(colName)
				if seenOrderCols[key] {
					return nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists,
						"duplicate column %q in ORDER BY", colName)
				}
				seenOrderCols[key] = true
				cls.orderBy = append(cls.orderBy, orderByClause{colName: colName, ascending: ascending, nullsFirst: nullsFirst, rawExpr: obExpr.Expression()})
			} else {
				cls.orderBy = append(cls.orderBy, orderByClause{ascending: ascending, nullsFirst: nullsFirst, expr: obExpr.Expression(), rawExpr: obExpr.Expression()})
			}
		}
	}

	// Reject LIMIT / OFFSET at parse time — fdb-relational 4.11.1.0's
	// AstNormalizer.visitLimitClause throws UNSUPPORTED_QUERY for
	// either, with a fixed message per branch ("OFFSET clause is not
	// supported." / "LIMIT clause is not supported."). Pagination is
	// a JDBC-only knob exposed via Statement.setMaxRows; SQL-level
	// LIMIT N is not in the planner's repertoire. Java checks offset
	// first, so `LIMIT N OFFSET M` errors on OFFSET; mirror that
	// order so byte-equal alignment holds for the combined shape.
	if limitClauseCtx := simpleTable.LimitClause(); limitClauseCtx != nil {
		hasOffset := limitClauseCtx.GetOffset() != nil
		hasLimit := limitClauseCtx.GetLimit() != nil
		// MySQL "LIMIT offset, count" form: AllLimitClauseAtom() returns
		// two atoms, neither set as the named GetLimit/GetOffset. Java's
		// AstNormalizer doesn't special-case this — both atoms hit the
		// `ctx.limit != null` branch via the grammar's labeling. Treat
		// both atoms as a LIMIT presence for rejection.
		atoms := limitClauseCtx.AllLimitClauseAtom()
		if !hasLimit && !hasOffset && len(atoms) > 0 {
			hasLimit = true
		}
		if hasOffset {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"OFFSET clause is not supported.")
		}
		if hasLimit {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"LIMIT clause is not supported.")
		}
	}

	// Parse GROUP BY clause. Bare column references go through the
	// columnNameFromExpr fast path (used by the proto-scan field-descriptor
	// and the map-row name lookup); positional references `GROUP BY N`
	// resolve to the Nth SELECT-list output name; anything else is
	// captured as an IExpressionContext evaluated per row at aggregation
	// time.
	groupByCtx := simpleTable.GroupByClause()
	if groupByCtx != nil {
		// Java alignment: `GROUP BY col AS alias` is a syntactic
		// extension that assigns a name to the group key. Java errors
		// 42702 (ambiguous-column) when the same alias appears twice
		// (groupby-tests.yamsql: `group by col1 as x, col2 as x`).
		// Track aliases across all items and reject duplicates; the
		// alias itself is otherwise unused at evaluation time — the
		// group key comes from the expression.
		seenAliases := make(map[string]bool)
		for _, item := range groupByCtx.AllGroupByItem() {
			aliasName := ""
			if item.Uid() != nil {
				aliasName = functions.StripIdentifierQuotes(item.Uid().GetText())
				// SQL identifiers are case-insensitive, so `GROUP BY
				// col1 AS x, col2 AS X` must error 42702 even though
				// the two aliases differ only in case. groupByAliases
				// below uses uppercase keys for lookup; the dedup
				// check uses the same normalisation.
				aliasKey := strings.ToUpper(aliasName)
				if seenAliases[aliasKey] {
					return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"duplicate alias %q in GROUP BY", aliasName)
				}
				seenAliases[aliasKey] = true
			}
			posName, isPos, posErr := resolveSelectListPosition("GROUP BY", item.Expression(), projCols, projAliases, cls.aggCols)
			if posErr != nil {
				return nil, posErr
			}
			if isPos {
				cls.groupBy = append(cls.groupBy, posName)
				cls.groupByExprs = append(cls.groupByExprs, nil)
				if aliasName != "" {
					if cls.groupByAliases == nil {
						cls.groupByAliases = make(map[string]int)
					}
					cls.groupByAliases[strings.ToUpper(aliasName)] = len(cls.groupBy) - 1
				}
				continue
			}
			colName, nameErr := columnNameFromExpr(item.Expression(), "GROUP BY expression")
			if nameErr == nil {
				// Postgres / MySQL: GROUP BY may reference a SELECT-list
				// alias (e.g. `SELECT v/10 AS bucket FROM t GROUP BY
				// bucket`). When the bare-column path resolves to a name
				// that matches a SELECT-list alias whose projExpr is a
				// non-trivial expression, redirect to the underlying
				// expression so per-row evaluation derives the group key.
				// Uses the snapshot taken right after the SELECT loop —
				// reclassification may have cleared projAliases.
				redirected := false
				for i, alias := range selectAliasesSnapshot {
					if alias != colName {
						continue
					}
					if i >= len(selectExprsSnapshot) || selectExprsSnapshot[i] == nil {
						break
					}
					cls.groupBy = append(cls.groupBy, canonicalTextOf(selectExprsSnapshot[i]))
					cls.groupByExprs = append(cls.groupByExprs, selectExprsSnapshot[i])
					redirected = true
					break
				}
				if !redirected {
					cls.groupBy = append(cls.groupBy, colName)
					cls.groupByExprs = append(cls.groupByExprs, nil)
				}
			} else {
				// Synthesize a display name from the expression text; the
				// value used for grouping comes from evaluating the expr.
				cls.groupBy = append(cls.groupBy, canonicalTextOf(item.Expression()))
				cls.groupByExprs = append(cls.groupByExprs, item.Expression())
			}
			if aliasName != "" {
				if cls.groupByAliases == nil {
					cls.groupByAliases = make(map[string]int)
				}
				cls.groupByAliases[strings.ToUpper(aliasName)] = len(cls.groupBy) - 1
			}
		}

		// Java alignment (groupby-tests.yamsql): `SELECT x FROM t GROUP
		// BY col1 AS x` — the alias becomes a usable SELECT-list
		// reference. Rewrite any bare projection whose name matches a
		// GROUP BY alias to the underlying group-by column, preserving
		// the alias itself as the output column name. Only bare column
		// group-by items (groupByExprs[i] == nil) are handled;
		// expression group keys keep their synthetic display name.
		aliasResolves := func(name string) (underlying string, outName string, ok bool) {
			idx, aliased := cls.groupByAliases[strings.ToUpper(name)]
			if !aliased {
				return "", "", false
			}
			if idx < len(cls.groupByExprs) && cls.groupByExprs[idx] != nil {
				return "", "", false
			}
			return cls.groupBy[idx], name, true
		}
		for i := range cls.projCols {
			if i < len(cls.projExprs) && cls.projExprs[i] != nil {
				continue
			}
			col := cls.projCols[i]
			if col == "" {
				continue
			}
			underlying, outName, ok := aliasResolves(col)
			if !ok {
				continue
			}
			if i >= len(cls.projAliases) {
				padded := make([]string, i+1)
				copy(padded, cls.projAliases)
				cls.projAliases = padded
			}
			if cls.projAliases[i] == "" {
				cls.projAliases[i] = outName
			}
			cls.projCols[i] = underlying
		}
		// Also rewrite aggCols entries: when the SELECT list mixes
		// plain-col refs with aggregates, bare columns are classified
		// into aggCols with groupCol set rather than into projCols.
		// Also rewrite aggregate arguments — `MAX(z)` where z is a
		// GROUP BY alias needs the arg resolved to the underlying col
		// before per-row evaluation.
		for i := range cls.aggCols {
			ac := &cls.aggCols[i]
			if ac.outExpr != nil {
				continue
			}
			if ac.groupCol != "" {
				if underlying, outName, ok := aliasResolves(ac.groupCol); ok {
					ac.groupCol = underlying
					if ac.outName == "" {
						ac.outName = outName
					}
				}
			}
			if ac.aggFunc != "" && ac.aggArg != "" && ac.aggExpr == nil {
				// Rewrite arg only; aggregate's outName (e.g. `MAX(z)`)
				// is already set at parse time and shouldn't be
				// collapsed to the alias string.
				if underlying, _, ok := aliasResolves(ac.aggArg); ok {
					ac.aggArg = underlying
				}
			}
		}
		// Rewrite ORDER BY entries that reference a GROUP BY alias
		// (`ORDER BY z` where `GROUP BY x.col1 AS z`) to the underlying
		// column. Without this the Cascades sort key references a field
		// name that doesn't exist in the aggregate output schema.
		for i := range cls.orderBy {
			ob := &cls.orderBy[i]
			if ob.expr != nil || ob.colName == "" {
				continue
			}
			if underlying, _, ok := aliasResolves(ob.colName); ok {
				ob.colName = underlying
			}
		}
	}

	// SQL §7.10 General Rule 1 / Java alignment: when GROUP BY is present,
	// every SELECT-list column reference must be in GROUP BY or wrapped in
	// an aggregate. Both SELECT * and SELECT qualifier.* with GROUP BY
	// error 42803 because the star expansion includes all source columns,
	// which generally aren't all in GROUP BY.
	if len(cls.groupBy) > 0 && len(projCols) == 0 && !countStar && len(cls.aggCols) == 0 {
		// projCols == nil + projQualifier == "" → SELECT *
		// projCols == nil + projQualifier != "" → SELECT qualifier.*
		// Either way, the star expands to ungrouped columns. Java 42803.
		return nil, api.NewError(api.ErrCodeGroupingError,
			"SELECT * cannot be used with GROUP BY (every column must be in GROUP BY or aggregated)")
	}

	// GROUP BY without any aggregate function in the SELECT list (e.g.
	// `SELECT a, b, a+b FROM t GROUP BY a, b`). Java permits this — the
	// query is functionally a DISTINCT on (a, b) with optional projected
	// expressions on the group-by columns. Pre-fix the aggregate path
	// only fired when len(aggCols) > 0, so GROUP BY was silently ignored
	// here and every source row was emitted (no dedup). Now we
	// reclassify projCols into aggCols entries (groupCol for bare
	// columns, outExpr for expressions) so the aggregate pipeline
	// activates and emits one row per distinct group.
	if len(cls.groupBy) > 0 && len(cls.aggCols) == 0 && len(projCols) > 0 {
		for _, q := range projStarQualifiers {
			if q != "" {
				// Java errors 42803 (grouping error) for `SELECT a.* ...
				// GROUP BY a1` because the star expands to cols not in
				// GROUP BY. Pre-dayshift-40 Go emitted 0A000 (unsupported).
				return nil, api.NewError(api.ErrCodeGroupingError,
					"SELECT qualifier.* expands to columns not in GROUP BY")
			}
		}
		// Java 42803 validation per column: defer to runtime so that
		// undefined columns surface as 42703 first (Java's order). The
		// proto path's group-eval already handles unrecognized column
		// names; we don't reject at parse time without schema access.
		extra := make([]aggSelectCol, len(projCols))
		for i, c := range projCols {
			out := projAliases[i]
			if out == "" {
				out = c
			}
			var slotExpr antlrgen.IExpressionContext
			if i < len(projExprs) {
				slotExpr = projExprs[i]
			}
			switch {
			case slotExpr != nil:
				// Constant or column-referencing expression — both route
				// to outExpr and are evaluated post-aggregation against
				// the rowMap (which carries group-by column values).
				extra[i] = aggSelectCol{outName: out, outExpr: slotExpr, visible: true}
			default:
				extra[i] = aggSelectCol{outName: out, groupCol: c, visible: true}
			}
		}
		cls.aggCols = extra
		projCols = nil
		projAliases = nil
		projExprs = nil
		projStarQualifiers = nil
		cls.projCols = nil
		cls.projAliases = nil
		cls.projExprs = nil
		cls.projStarQualifiers = nil
	}

	// SQL §7.10 GR1: when a SELECT list contains aggregates, every
	// non-aggregate column reference must appear in GROUP BY. With no
	// GROUP BY at all, the query is implicitly one group and bare
	// column references violate the rule. Java errors 42803. Matches
	// Java's groupby-tests.yamsql 42803 pattern extended to the
	// no-GROUP-BY-at-all variant.
	//
	// The SELECT loop silently reclassifies a bare-column element as
	// `aggSelectCol{groupCol: ...}` when aggregates are in the list —
	// checking projCols alone misses those. Walk cls.aggCols for entries
	// that are neither aggregates nor outExprs (bare group column
	// references) and for outExprs that reference columns: both are GR1
	// violations when there's no GROUP BY.
	hasAggregates := cls.countStar
	for _, ac := range cls.aggCols {
		if ac.aggFunc != "" {
			hasAggregates = true
			break
		}
	}
	if hasAggregates && len(cls.groupBy) == 0 {
		for _, ac := range cls.aggCols {
			if ac.aggFunc != "" {
				continue // aggregate — fine
			}
			if ac.outExpr != nil {
				// Expression entries are fine if they either have no
				// column references (constants) or wrap aggregates (the
				// column refs are inside a SUM/MAX/... call). An outExpr
				// that references columns but contains no aggregates is a
				// bare-column expression (e.g. `v + 1`) and violates GR1.
				if !exprReferencesColumn(ac.outExpr) {
					continue
				}
				if len(harvestAggregates(ac.outExpr)) > 0 {
					continue
				}
			}
			// Bare column reference or column-referencing expression
			// without any aggregate — GR1 violation.
			offender := ac.groupCol
			if offender == "" {
				offender = ac.outName
			}
			return nil, api.NewErrorf(api.ErrCodeGroupingError,
				"column %q must appear in the GROUP BY clause or be used in an aggregate function", offender)
		}
	}

	// Parse HAVING clause (only meaningful with GROUP BY).
	havingCtx := simpleTable.HavingClause()
	if havingCtx != nil {
		cls.havingExpr = havingCtx.GetHavingExpr()
	}

	// Redirect aggCols groupCol entries that came from a SELECT-list
	// expression (`v/10 AS bucket`) to point at the matching GROUP BY
	// expression text, so the proto path's groupExprByName check fires
	// and skips the FD lookup. Walks selectExprsSnapshot to find the
	// original projExpr for each groupCol entry; matches against
	// cls.groupBy[] by GetText. Idempotent — runs once after both
	// SELECT-list reclassification (if any) and GROUP BY parsing.
	if len(cls.aggCols) > 0 && len(cls.groupBy) > 0 && len(selectExprsSnapshot) > 0 {
		for ai, ac := range cls.aggCols {
			if ac.groupCol == "" {
				continue
			}
			// Look up the original projExpr by alias / position in the snapshot.
			var origExpr antlrgen.IExpressionContext
			for si, alias := range selectAliasesSnapshot {
				if alias != ac.groupCol {
					continue
				}
				if si < len(selectExprsSnapshot) {
					origExpr = selectExprsSnapshot[si]
				}
				break
			}
			if origExpr == nil {
				continue
			}
			projText := canonicalTextOf(origExpr)
			for gi, gn := range cls.groupBy {
				if gi < len(cls.groupByExprs) && cls.groupByExprs[gi] != nil && projText == gn {
					cls.aggCols[ai].groupCol = gn
					break
				}
			}
		}
	}

	// Post-GROUP-BY: when a SELECT-list outExpr (an expression that
	// references columns but contains no aggregates) was routed to
	// outExpr by the SELECT-loop classification but its text matches a
	// GROUP BY entry exactly, switch back to a groupCol reference so
	// the groupExprByName mechanism evaluates it once per group from
	// gs.groupVals. Without this, expression-shaped GROUP BY keys
	// (e.g. SELECT CASE WHEN amt<200 THEN 'low' ELSE 'high' END FROM t
	// GROUP BY CASE WHEN amt<200 THEN 'low' ELSE 'high' END) would try
	// to evaluate the expression against a per-row map at outExpr emit
	// time — and the underlying column ('amt') is not in the rowMap
	// because GROUP BY summarized the rows. Symmetric with the alias
	// redirect just above.
	if len(cls.aggCols) > 0 && len(cls.groupBy) > 0 {
		for ai, ac := range cls.aggCols {
			if ac.outExpr == nil || ac.aggFunc != "" {
				continue
			}
			outExprText := canonicalTextOf(ac.outExpr)
			for gi, gn := range cls.groupBy {
				if gi < len(cls.groupByExprs) && cls.groupByExprs[gi] != nil && outExprText == gn {
					cls.aggCols[ai].outExpr = nil
					cls.aggCols[ai].groupCol = gn
					break
				}
			}
		}
	}

	// countStar fast path assumes a single synthetic row. With GROUP BY
	// present we need a per-group COUNT(*), so demote to aggCols. The
	// alias (if any) propagates so `SELECT COUNT(*) AS n FROM t GROUP BY g`
	// emits the column as `n`. Also set projCols so the downstream projection
	// narrows the aggregate output (keys+aggs) to just the COUNT column.
	if cls.countStar && len(cls.groupBy) > 0 {
		cls.countStar = false
		outName := "COUNT(*)"
		if cls.countStarAlias != "" {
			outName = cls.countStarAlias
		}
		cls.aggCols = append(cls.aggCols, aggSelectCol{outName: outName, aggFunc: "COUNT", visible: true})
		cls.projCols = []string{outName}
		cls.projAliases = []string{""}
		cls.projExprs = []antlrgen.IExpressionContext{nil}
		cls.projStarQualifiers = []string{""}
	}

	// Harvest aggregates referenced in HAVING and ORDER BY that aren't
	// already in aggCols. Otherwise queries like
	//   SELECT grp FROM t GROUP BY grp HAVING SUM(v) > 0
	//   SELECT grp FROM t GROUP BY grp ORDER BY SUM(v) DESC
	// have aggCols == nil -> the executor never runs the aggregate pipeline
	// -> GROUP BY is silently ignored. The HAVING / ORDER BY resolver already
	// looks up aggregates by their reconstructed output name ("COUNT(*)",
	// "SUM(v)"), so matching aggCols entries make the evaluation round-trip.
	// If projCols still holds plain columns at this point, reclassify them
	// as group-by references in aggCols (mirror of the SELECT-list-aggregate
	// path's existing reclassification).
	var harvestExprs []antlrgen.IExpressionContext
	if cls.havingExpr != nil {
		harvestExprs = append(harvestExprs, cls.havingExpr)
	}
	for _, ob := range cls.orderBy {
		if ob.rawExpr != nil {
			harvestExprs = append(harvestExprs, ob.rawExpr)
		}
	}
	if len(harvestExprs) > 0 {
		existing := make(map[string]struct{}, len(cls.aggCols))
		for _, ac := range cls.aggCols {
			existing[ac.outName] = struct{}{}
		}
		var newAggs []aggSelectCol
		for _, hexpr := range harvestExprs {
			for _, ac := range harvestAggregates(hexpr) {
				if _, ok := existing[ac.outName]; ok {
					continue
				}
				existing[ac.outName] = struct{}{}
				// ac.visible stays false — not in user's SELECT list.
				newAggs = append(newAggs, ac)
			}
		}
		// ORDER BY items that wrap aggregates in an expression (e.g.
		// `ORDER BY SUM(v) * 2`) get their own non-visible outExpr
		// aggCols entry. The proto sort path can then look up the entry
		// via colIdx and find a per-group value evaluated from the
		// wrapping expression. Inner aggregates were harvested above so
		// the rowMap at outExpr eval time has them available. Clear
		// colName so the Value-based sort resolver picks up rawExpr.
		for obIdx, ob := range cls.orderBy {
			if ob.expr == nil || len(harvestAggregates(ob.expr)) == 0 {
				continue
			}
			newAggs = append(newAggs, aggSelectCol{
				outExpr: ob.expr,
			})
			cls.orderBy[obIdx].colName = ""
			cls.orderBy[obIdx].expr = nil
		}
		if len(newAggs) > 0 {
			if len(cls.aggCols) == 0 && len(projCols) > 0 {
				// No SELECT-list aggregates yet; demote the plain projCols
				// to group-by references so the aggregate pipeline knows
				// how to surface them in each output row. When the projExpr
				// matches a GROUP BY expression by text (e.g. `SELECT v/10
				// AS bucket ... GROUP BY v/10`), point groupCol at the
				// matching groupBy[] string so the proto path's
				// groupExprByName check fires and skips the FD lookup.
				prepended := make([]aggSelectCol, 0, len(projCols)+len(cls.aggCols))
				for i, c := range projCols {
					out := projAliases[i]
					if out == "" {
						out = c
					}
					gc := c
					if i < len(projExprs) && projExprs[i] != nil {
						projText := canonicalTextOf(projExprs[i])
						for gi, gn := range cls.groupBy {
							if gi < len(cls.groupByExprs) && cls.groupByExprs[gi] != nil && projText == gn {
								gc = gn
								break
							}
						}
					}
					prepended = append(prepended, aggSelectCol{outName: out, groupCol: gc, visible: true})
				}
				cls.aggCols = append(prepended, cls.aggCols...)
				cls.projCols = nil
				cls.projAliases = nil
				cls.projExprs = nil
				cls.projStarQualifiers = nil
			}
			cls.aggCols = append(cls.aggCols, newAggs...)
		}
	}

	return cls, nil
}

func extractFromSimpleTable(simpleTable *antlrgen.SimpleTableContext) (*selectQuery, error) {
	cls, err := classifySelectElements(simpleTable)
	if err != nil {
		return nil, err
	}

	// Parse FROM clause via the shared parseFromSource helper.
	fs, err := parseFromSource(simpleTable)
	if err != nil {
		return nil, err
	}

	return selectQueryFromClassification(cls, fs), nil
}

// exprReferencesColumn reports whether the expression tree contains any
// FullColumnName references. Used to distinguish constant expressions
// (SELECT 1, SUM(v) FROM t) from column-bearing expressions (SELECT grp,
// SUM(v) FROM t GROUP BY grp) in the mixed-aggregate classification —
// constants don't need to be group-by references and route through the
// outExpr path instead.
func exprReferencesColumn(expr antlrgen.IExpressionContext) bool {
	if expr == nil {
		return false
	}
	found := false
	var visit func(n antlr.Tree)
	visit = func(n antlr.Tree) {
		if n == nil || found {
			return
		}
		if _, ok := n.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
			found = true
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			visit(n.GetChild(i))
		}
	}
	visit(expr)
	return found
}

// harvestColumnRefs walks an expression tree and returns the set of column
// names (dot-separated) referenced outside of aggregate function calls.
// Used by aggregateMapRows's pre-check to detect ungrouped column
// references in outExpr projection entries (42803 vs 42703 distinction).
// Refs inside aggregate calls are correctly computed by the aggregate
// itself — walking into them would flag false positives.
func harvestColumnRefs(expr antlrgen.IExpressionContext) []string {
	if expr == nil {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	var visit func(n antlr.Tree)
	visit = func(n antlr.Tree) {
		if n == nil {
			return
		}
		// Don't recurse into aggregate function calls — the aggregate
		// resolves its own argument from the group's accumulator.
		if fc, ok := n.(*antlrgen.FunctionCallExpressionAtomContext); ok {
			if _, isAgg := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext); isAgg {
				return
			}
		}
		if c, ok := n.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
			name := functions.FullIdToName(c.FullColumnName().FullId())
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			visit(n.GetChild(i))
		}
	}
	visit(expr)
	return names
}

// harvestAggregates walks an expression tree looking for aggregate function
// calls (COUNT/SUM/MIN/MAX/AVG). Returns a synthesized aggSelectCol per
// distinct aggregate found, with outName matching the HAVING resolver's
// reconstructed lookup name ("COUNT(*)", "SUM(v)", "AVG(price)", etc.).
// Used to back HAVING-only aggregates so the aggregate pipeline runs even
// when the SELECT list contains only plain columns.
func harvestAggregates(expr antlrgen.IExpressionContext) []aggSelectCol {
	if expr == nil {
		return nil
	}
	var out []aggSelectCol
	seen := make(map[string]struct{})
	visit := func(antlr.Tree) {}
	visit = func(n antlr.Tree) {
		if n == nil {
			return
		}
		// Stop at scalar subquery boundaries: aggregates inside a
		// subquery belong to the subquery, not the outer expression.
		// Without this guard `SELECT (SELECT MAX(v) FROM t) FROM t2`
		// would mis-promote the outer slot to an aggregate column,
		// dropping it from projCols entirely.
		if _, ok := n.(*antlrgen.SubqueryExpressionAtomContext); ok {
			return
		}
		if awf, ok := n.(*antlrgen.AggregateWindowedFunctionContext); ok {
			ac, ok := aggColFromAwf(awf)
			if ok {
				if _, dup := seen[ac.outName]; !dup {
					seen[ac.outName] = struct{}{}
					out = append(out, ac)
				}
			}
			// Do not recurse into the aggregate's argument — nested
			// aggregates aren't valid SQL and the outer evaluator
			// will reject them with a clearer error anyway.
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			visit(n.GetChild(i))
		}
	}
	visit(expr)
	return out
}

// aggColFromAwf reconstructs an aggSelectCol from an AggregateWindowedFunction
// context via the shared extractAwfFields helper. Output name matches the
// HAVING resolver's lookup name and the SELECT-list default alias
// ("COUNT(*)", "SUM(v)"). Returns false for unknown aggregate shapes.
func aggColFromAwf(awf *antlrgen.AggregateWindowedFunctionContext) (aggSelectCol, bool) {
	fn, argCol, argExpr, outName, isDistinct, ok := extractAwfFields(awf)
	if !ok {
		return aggSelectCol{}, false
	}
	return aggSelectCol{
		outName:     outName,
		aggFunc:     fn,
		aggArg:      argCol,
		aggExpr:     argExpr,
		aggDistinct: isDistinct,
	}, true
}

// fromSource holds the parsed FROM-clause metadata: table name, alias,
// derived-query reference, JOIN clauses, and WHERE expression. Extracted
// from ANTLR by parseFromSource so both extractFromSimpleTable (which
// needs it for selectQuery assembly) and PlanVisitor.visitFrom (which
// builds the operator tree directly from ANTLR) share a single parsing
// path.
type fromSource struct {
	tableName    string
	tableAlias   string
	derivedQuery antlrgen.IQueryContext
	joins        []joinClause
	whereExpr    antlrgen.IWhereExprContext
}

// parseFromSource walks the FROM clause of a SimpleTableContext and
// returns the parsed source metadata. Returns an error for unsupported
// shapes (missing FROM, CROSS JOIN on extras, etc.). This is the
// single source of truth for FROM parsing — both extractFromSimpleTable
// and PlanVisitor.visitFrom delegate here.
func parseFromSource(simpleTable *antlrgen.SimpleTableContext) (*fromSource, error) {
	fromClause := simpleTable.FromClause()
	if fromClause == nil {
		// FROM-less SELECT: fdb-relational 4.11.1.0's QueryVisitor's
		// visitSimpleTable asserts simpleTableContext.fromClause() is
		// non-null with `Assert.notNullUnchecked(... ErrorCode.
		// UNSUPPORTED_QUERY, "query is not supported")`. The check
		// fires universally — including FROM-less SELECTs inside CTE
		// base cases (every SimpleTable visit hits the gate, no CTE-
		// context bypass). Match byte-equal. Standalone constant
		// projection like `SELECT 1+1` and CTE bases like
		// `WITH base AS (SELECT 1 AS n) ...` both reject.
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"query is not supported")
	}

	sources := fromClause.TableSources()
	if sources == nil || len(sources.AllTableSource()) == 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"FROM clause missing table source")
	}
	srcBase, ok := sources.AllTableSource()[0].(*antlrgen.TableSourceBaseContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported table source %T", sources.AllTableSource()[0])
	}
	// Additional comma-separated sources become implicit cross joins; the
	// WHERE clause supplies any join predicate.
	var extraCrossJoins []joinClause
	for _, extra := range sources.AllTableSource()[1:] {
		eb, isBase := extra.(*antlrgen.TableSourceBaseContext)
		if !isBase {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported extra table source %T", extra)
		}
		// Bare-source joins are not supported on extras (grammar quirk).
		if len(eb.AllJoinPart()) > 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"JOIN clauses on comma-separated FROM sources are not supported")
		}
		switch item := eb.TableSourceItem().(type) {
		case *antlrgen.AtomTableItemContext:
			uids := item.TableName().FullId().AllUid()
			parts := make([]string, len(uids))
			for i, u := range uids {
				parts[i] = functions.StripIdentifierQuotes(u.GetText())
			}
			tblName := strings.Join(parts, ".")
			alias := tblName
			// Use GetAlias() so implicit aliases (`FROM a, b alias`) parse.
			if item.GetAlias() != nil {
				alias = functions.StripIdentifierQuotes(item.GetAlias().GetText())
			}
			extraCrossJoins = append(extraCrossJoins, joinClause{
				tableName: tblName,
				joinType:  "INNER",
				alias:     alias,
				onExpr:    nil,
			})
		case *antlrgen.SubqueryTableItemContext:
			alias := ""
			if item.GetAlias() != nil {
				alias = functions.StripIdentifierQuotes(item.GetAlias().GetText())
			}
			if alias == "" {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"derived table in FROM must have an alias")
			}
			extraCrossJoins = append(extraCrossJoins, joinClause{
				tableName:    alias,
				joinType:     "INNER",
				alias:        alias,
				onExpr:       nil,
				derivedQuery: item.Query(),
			})
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"FROM: comma-separated sources must be plain table names, got %T",
				eb.TableSourceItem())
		}
	}
	// Resolve FROM source: derived table `FROM (SELECT ...) AS alias` or
	// a plain atom table.
	if subItem, isSub := srcBase.TableSourceItem().(*antlrgen.SubqueryTableItemContext); isSub {
		alias := ""
		if subItem.GetAlias() != nil {
			alias = functions.StripIdentifierQuotes(subItem.GetAlias().GetText())
		}
		if alias == "" {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation, "derived table in FROM must have an alias")
		}
		return &fromSource{
			tableName:    alias,
			tableAlias:   alias,
			joins:        extraCrossJoins,
			whereExpr:    fromClause.WhereExpr(),
			derivedQuery: subItem.Query(),
		}, nil
	}

	atomItem, ok := srcBase.TableSourceItem().(*antlrgen.AtomTableItemContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported table source item %T; only plain table names are supported",
			srcBase.TableSourceItem())
	}
	// Build table name from uid segments, stripping identifier quotes.
	// "INFORMATION_SCHEMA"."TABLES" → INFORMATION_SCHEMA.TABLES
	uids := atomItem.TableName().FullId().AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = functions.StripIdentifierQuotes(u.GetText())
	}
	// Only use Uid() as alias when AS is explicit. Without AS, the parser may
	// greedily consume a join keyword (LEFT, RIGHT, CROSS) as the table alias
	// due to grammar ambiguity — LEFT/RIGHT are in keywordsCanBeId.
	// When the mis-parsed "alias" is LEFT or RIGHT, we promote the first
	// InnerJoinContext to a LEFT/RIGHT join.
	leftAlias := ""
	promotedJoinType := ""
	// Grammar is `tableName (AS? alias=uid)?` — AS optional.
	// Pick up implicit aliases via GetAlias() (was previously
	// gated on AS being present, which lost `FROM Order o` etc).
	// Special case: a NO-AS, UNQUOTED bare-uid alias equal to
	// LEFT or RIGHT is the keywordsCanBeId grammar misparse for
	// `FROM a LEFT JOIN ...` — promote the first InnerJoin to
	// LEFT/RIGHT join instead of treating LEFT/RIGHT as the
	// alias. The AS form (`FROM a AS LEFT JOIN ...`) and the
	// quoted form (`FROM a "LEFT"`) both keep "LEFT" as the
	// literal alias.
	if atomItem.GetAlias() != nil {
		aliasRaw := atomItem.GetAlias().GetText()
		aliasTxt := functions.StripIdentifierQuotes(aliasRaw)
		// Structural quote-detection: post-case-fold,
		// `aliasRaw != aliasTxt` is also true whenever the
		// raw alias has any lowercase character (the helper
		// upper-cases unquoted text). Use a structural check
		// so a lowercased alias `from a hello` doesn't get
		// classified as quoted.
		isQuoted := len(aliasRaw) >= 2 &&
			((aliasRaw[0] == '"' && aliasRaw[len(aliasRaw)-1] == '"') ||
				(aliasRaw[0] == '`' && aliasRaw[len(aliasRaw)-1] == '`'))
		if atomItem.AS() == nil && !isQuoted {
			up := strings.ToUpper(aliasTxt)
			if up == "LEFT" || up == "RIGHT" {
				promotedJoinType = up
			} else {
				leftAlias = aliasTxt
			}
		} else {
			leftAlias = aliasTxt
		}
	}
	if leftAlias == "" {
		leftAlias = strings.Join(parts, ".")
	}

	// Parse JOIN clauses.
	var joins []joinClause
	for _, jp := range srcBase.AllJoinPart() {
		jc, jErr := extractJoinClause(jp)
		if jErr != nil {
			return nil, jErr
		}
		joins = append(joins, jc)
	}
	// If the first join was mis-parsed (LEFT/RIGHT consumed as alias), promote it.
	if promotedJoinType != "" && len(joins) > 0 && joins[0].joinType == "INNER" {
		joins[0].joinType = promotedJoinType
	}
	// Implicit cross joins from comma-separated FROM sources run last; the
	// WHERE predicate decides which combinations survive.
	joins = append(joins, extraCrossJoins...)

	return &fromSource{
		tableName:  strings.Join(parts, "."),
		tableAlias: leftAlias,
		joins:      joins,
		whereExpr:  fromClause.WhereExpr(),
	}, nil
}

// extractJoinClause parses a single JOIN part (INNER JOIN, LEFT JOIN, etc.) from
// the grammar. Only INNER JOIN and LEFT OUTER JOIN are implemented.
func extractJoinClause(jp antlrgen.IJoinPartContext) (joinClause, error) {
	switch j := jp.(type) {
	case *antlrgen.InnerJoinContext:
		// Explicit `CROSS JOIN` syntax — reject. fdb-relational
		// 4.11.1.0 NPEs on `a CROSS JOIN b` because its visitor
		// unconditionally calls `accept(...)` on the ON-clause
		// expression which is null for CROSS JOIN (CLAUDE.md gotcha).
		// Go's embedded engine matches by rejecting at parse time —
		// same architectural reason: the visitor's CROSS-JOIN code
		// path doesn't exist. Workaround: comma-join `FROM a, b`.
		// Per project conformance principle: doesn't work in Java →
		// doesn't work in Go.
		if j.CROSS() != nil {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"explicit CROSS JOIN syntax is not supported; use comma-join `FROM a, b` for cartesian products")
		}
		atomItem, ok := j.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"JOIN: unsupported table source item %T", j.TableSourceItem())
		}
		uids := atomItem.TableName().FullId().AllUid()
		parts := make([]string, len(uids))
		for i, u := range uids {
			parts[i] = functions.StripIdentifierQuotes(u.GetText())
		}
		tblName := strings.Join(parts, ".")
		alias := tblName
		// Grammar is `tableName (AS? alias=uid)?` — AS is optional.
		// `atom.AS()` being nil does NOT mean no alias; check
		// `GetAlias() != nil` so implicit aliases like
		// `JOIN Customer c` are picked up. Mirrors the FROM-clause
		// path in semantic.BuildScopeFromFromClause.
		if atomItem.GetAlias() != nil {
			alias = functions.StripIdentifierQuotes(atomItem.GetAlias().GetText())
		}
		var onExpr antlrgen.IExpressionContext
		if j.Expression() != nil {
			onExpr = j.Expression()
		}
		return joinClause{tableName: tblName, joinType: "INNER", alias: alias, onExpr: onExpr}, nil

	case *antlrgen.OuterJoinContext:
		atomItem, ok := j.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"JOIN: unsupported table source item %T", j.TableSourceItem())
		}
		uids := atomItem.TableName().FullId().AllUid()
		parts := make([]string, len(uids))
		for i, u := range uids {
			parts[i] = functions.StripIdentifierQuotes(u.GetText())
		}
		tblName := strings.Join(parts, ".")
		alias := tblName
		// Same implicit-alias note as InnerJoin.
		if atomItem.GetAlias() != nil {
			alias = functions.StripIdentifierQuotes(atomItem.GetAlias().GetText())
		}
		jt := "LEFT"
		if j.RIGHT() != nil {
			jt = "RIGHT"
		}
		var onExpr antlrgen.IExpressionContext
		if j.Expression() != nil {
			onExpr = j.Expression()
		}
		return joinClause{tableName: tblName, joinType: jt, alias: alias, onExpr: onExpr}, nil

	default:
		return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported JOIN type %T; only INNER JOIN and LEFT/RIGHT OUTER JOIN are supported", jp)
	}
}
