package embedded

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"github.com/antlr4-go/antlr/v4"
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

// selectQuery holds the parsed components of a SELECT statement.
// groupKeyRef is one GROUP BY key as the parse tree stated it. A bare column
// carries bare/qualifier segments (qualification is FullId SEGMENT COUNT,
// never a dot scan of display); an expression key carries expr (evaluated per
// row) with display as its canonical rendering and empty bare.
type groupKeyRef struct {
	display   string // canonical rendering — output naming / diagnostics only
	bare      string // last segment of a bare column ref; "" for expressions
	qualifier string // leading segment(s) of a qualified bare ref; "" otherwise
	qualified bool   // parse-tree segment count > 1
	// segs is the FULL ordered segment list of the reference (`a.n.sk` ->
	// [A N SK]). It is what RESOLUTION consumes; qualifier is a rendering and
	// cannot express where one segment ends and the next begins.
	segs []string
	expr antlrgen.IExpressionContext
}

// groupKeyRefDisplays renders the parser keys' display names for name-only
// consumers (the ORDER-BY visitor's key membership).
func groupKeyRefDisplays(keys []groupKeyRef) []string {
	if keys == nil {
		return nil
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.display
	}
	return out
}

// logicalGroupKeys converts the parser's structured keys to the logical IR's
// — same segments, no re-parse anywhere in between.
func logicalGroupKeys(keys []groupKeyRef) []logical.GroupKey {
	if keys == nil {
		return nil
	}
	out := make([]logical.GroupKey, len(keys))
	for i, k := range keys {
		out[i] = logical.GroupKey{
			Display:   k.display,
			Bare:      k.bare,
			Qualifier: k.qualifier,
			Qualified: k.qualified,
			Segs:      k.segs,
		}
	}
	return out
}

// stripGroupKeyLeadingSegment rebuilds a group key whose DISPLAY has had a
// single-source table/alias prefix baked away, keeping the reference structured.
//
// The naive rebuild — `GroupKey{Display: stripped, Bare: stripped}` — makes one
// "bare" segment out of everything that survived the strip. For a flat key
// (`A.ID` -> `ID`) that is exact. For a key that DESCENDS into a struct
// (`A.R.V.Z` -> `R.V.Z`) it is a dotted string in the field RFC-197 exists to
// keep single-segment: resolution then asks for a column literally named
// "R.V.Z", and key identity (groupKeysEquivalent) compares the whole dotted
// blob, so `GROUP BY r.v.z, s.v.z` under two aliases would present as one
// repeated key and take a duplicate-key 42702 that Java does not take.
//
// So the strip is performed on the SEGMENTS when the reference has them and its
// leading segment is what the prefix named: the alias segment is dropped and the
// remaining segments re-derive Bare/Qualifier/Qualified. A key without segments,
// or whose leading segment is not the stripped prefix, keeps the flat rebuild.
func stripGroupKeyLeadingSegment(k logical.GroupKey, stripped string) logical.GroupKey {
	if len(k.Segs) > 1 && strings.EqualFold(strings.Join(k.Segs[1:], "."), stripped) {
		rest := k.Segs[1:]
		out := logical.GroupKey{
			Display:   stripped,
			Bare:      rest[len(rest)-1],
			Qualified: len(rest) > 1,
			Segs:      rest,
		}
		if len(rest) > 1 {
			out.Qualifier = strings.Join(rest[:len(rest)-1], ".")
		}
		return out
	}
	// The single-source prefix was baked away and the segments cannot account
	// for it: the key is BARE from here on — stale qualification segments would
	// chase a qualifier the runtime row no longer carries.
	return logical.GroupKey{Display: stripped, Bare: stripped}
}

type selectQuery struct {
	// selectClassification holds all SELECT-list, GROUP BY, HAVING,
	// ORDER BY, and aggregate classification fields. Embedded so that
	// sq.projCols, sq.aggCols, etc. continue to work as before.
	selectClassification

	tableName  string
	tableAlias string // alias or tableName if no alias given
	// sourceSegments preserves the primary FROM source's identifier segments.
	// Most primary sources are catalog tables, but an EXISTS subquery may use a
	// correlated array field (`FROM R.TAGS AS E`). Keeping the parse-tree
	// segments lets the catalog-aware planner distinguish that field access
	// from a schema-qualified table without re-splitting display text.
	sourceSegments []string
	whereExpr      antlrgen.IWhereExprContext
	// catalogAwareInnerPlan carries the PRIMARY derived source's catalog-aware
	// inner plan across the fromSource bridge — see fromSource's field.
	catalogAwareInnerPlan logical.LogicalOperator
	// limit < 0 means no limit.
	limit int64
	// offset >= 0 means skip that many rows after sort/group (OFFSET n).
	offset int64
	// joins describes JOIN clauses (nil = no joins).
	joins []joinClause
	// tableQualifierAliases is the exact, query-local exception to
	// alias-hides-table-name resolution. A key is a FROM correlation alias that
	// also spelled the active schema qualifier of a later real table
	// (`FROM PA AS s, s.PB AS B`). Java resolves the later source table-first and
	// still lets `PA.ID` address the earlier PA source. The marker is captured
	// before schema normalization erases `s.PB`'s leading segment.
	tableQualifierAliases map[string]bool
	// derivedQuery is non-nil when the FROM clause is a subquery (derived table).
	// When set, tableName holds the alias; the query is materialized at execution time.
	derivedQuery antlrgen.IQueryContext
	// inlineValues carries the parse-tree leaf for a literal VALUES table.
	// This parser layer does not assign logical semantics; it only preserves the
	// distinct source kind for the later builder.
	inlineValues *antlrgen.InlineTableItemContext
}

// joinType enumerates the join flavours; threaded through to the
// Cascades JOIN logical builder for LEFT/RIGHT/FULL null-padding.
type joinType int

const (
	joinTypeInner joinType = iota
	joinTypeLeft
	joinTypeRight
	joinTypeFull // FULL OUTER JOIN (Go-only extension)
)

// joinClause describes a single JOIN part in a SELECT query.
type joinClause struct {
	tableName string
	joinType  joinType
	alias     string
	onExpr    antlrgen.IExpressionContext
	// usingUids is the parsed `USING (col, ...)` column list, set when the
	// JOIN used USING-syntax instead of ON. The equivalent ON predicate is
	// synthesized in extractFromSimpleTable (after the left source alias is
	// known) into onExpr; usingUids is cleared once synthesized.
	usingUids antlrgen.IUidListContext
	// usingHiddenCols retains the USING column names (UPPER-folded,
	// quote-stripped) after the ON synthesis clears usingUids. This
	// join's RIGHT side hides these columns: Java marks the right copy
	// hidden (QueryVisitor.resolveJoinUsingClause → Expression.asHidden)
	// so star expansion drops it and an unqualified reference resolves
	// the left copy.
	usingHiddenCols []string
	// usingColTexts retains the USING columns as WRITTEN (quotes intact), in
	// order, so retargetUsingJoins can re-synthesize the ON predicate against
	// the correct owner. usingHiddenCols cannot serve: it is UPPER-folded and
	// quote-stripped, and splicing that back into SQL text would re-normalize a
	// quoted-DDL column.
	usingColTexts []string
	// segments preserves the un-flattened uid segments of the source's
	// dotted name (`FullId().AllUid()`, quote-stripped). For a single-name
	// table source it is `[name]`; for a correlated array source
	// `outer.arr` it is `["OUTER", "ARR"]`. The translator classifies a
	// comma source as a LATERAL UNNEST (vs a table) by resolving segment 0
	// against the in-scope source aliases — it MUST NOT re-split tableName,
	// which is a lossy `strings.Join` of these segments — no
	// text-heuristic re-split. RFC-142 R5.
	segments []string
	// atAlias is the `AT atAlias` ordinal alias of a lateral array unnest
	// (`FROM t, t.arr AS x AT ord`), empty when absent. Its presence makes
	// the Explode produce 1-based ordinals (WITH ORDINALITY). Carried
	// through for the translator to bind; the parser no longer rejects AT.
	atAlias string
	// fromComma is true when this entry is a COMMA-separated FROM source
	// (`FROM t, x`), false when it is produced by extractJoinClause from an
	// explicit JOIN part (`... INNER/LEFT/RIGHT/FULL JOIN x ...`). Only a
	// comma source may be a lateral array unnest: Java unnests via the
	// COMMA-syntax `FROM t, t.arr AS x` correlated-field path; an explicit
	// JOIN source is always resolved as a table/derived source, never a
	// lateral array unnest (the JOIN visitor adds it as a normal operator).
	// onExpr alone cannot distinguish them — an `INNER JOIN x` with no ON
	// also has onExpr == nil — so the unnest classifier gates on this flag.
	// RFC-142 R5.
	fromComma bool
	// derivedQuery is set when the join's right-hand source is a
	// subquery (`... , (SELECT ...) AS x` or `INNER JOIN (SELECT ...)
	// AS x ON ...`). The dispatcher materializes the subquery as a
	// CTE keyed by `alias` before the join executor runs, mirroring
	// the first-source derived-table handling.
	derivedQuery antlrgen.IQueryContext
	// inlineValues is the literal-table source for this comma FROM leg.
	inlineValues *antlrgen.InlineTableItemContext
	// bindingID is the leg's binding correlation name when its alias
	// DUPLICATES an earlier FROM leg's at the same level; empty when the
	// alias itself binds (every non-duplicate leg — zero change for
	// non-duplicate queries). Java mints CorrelationIdentifier.uniqueId for
	// EVERY quantifier and keeps the SQL alias as a display qualifier only
	// (LogicalOperator.newNamedOperator); Go mints only
	// where collision forces it. Minted DETERMINISTICALLY from the leg's
	// FROM position (`Q$DUPN`, N = the leg's 1-based join ordinal; fold-stable upper form) so two
	// plannings of the same query produce identical correlation ids (the
	// stablePlanHash determinism lesson — never an atomic counter).
	// Assigned ONCE by assignFromLegBindingIDs (the single mint authority);
	// consumers READ it, never re-derive it from the alias. The logical
	// builders carry it (LogicalScan/LogicalCTE/LogicalUnnest.Binding); the
	// semantic-scope builders read it too.
	bindingID string
	// catalogAwareInnerPlan is set by the catalog-aware builder when
	// it pre-builds the derived table's inner plan with upgraded
	// predicates. When non-nil, buildLogicalPlanForSelect uses this
	// instead of calling buildLogicalPlanForSelect recursively.
	catalogAwareInnerPlan logical.LogicalOperator
}

type orderByClause struct {
	colName   string
	ascending bool
	// bare/qualifier/qualified: parse-tree segments of a plain column
	// reference item (splitColumnRef); zero values for positional and
	// expression items. Qualification is FullId SEGMENT COUNT, never a dot
	// scan of colName.
	bare      string
	qualifier string
	qualified bool
	// segs is the FULL ordered segment list of the reference (`a.n.sk` ->
	// [A N SK]). It is what RESOLUTION consumes; qualifier is a rendering and
	// cannot express where one segment ends and the next begins.
	segs []string
	// bareRef: the ORDER BY item is a plain ONE-segment column reference —
	// the only shape SQL binds to an output alias. False for qualified
	// references (`d.x`), aggregates, and computed expressions, whose
	// canonical renderings may collide with a delimited alias; alias-
	// binding passes require bareRef.
	bareRef bool
	// pos is the 1-indexed SELECT-list position for a POSITIONAL key
	// (`ORDER BY 1`); 0 = not positional. A positional key IS an output
	// ordinal by SQL definition — carrying it (instead of only the
	// name-resolved text) lets a UNION lift bind the key to the union
	// OUTPUT slot: the text resolves against the RIGHT leg's spelling and
	// then fails name-validation against the LEFT leg when the legs spell
	// that position differently (RFC-180, union_with_aggregate).
	pos int
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
	// selectOrdinal is the immutable one-based position of a visible item in
	// the SQL SELECT list. Internal aggregates harvested from HAVING, ORDER BY,
	// or a wrapping expression keep zero. Reclassification may reorder
	// aggSelectCol storage, but it must never rewrite this identity.
	selectOrdinal int
	// Exactly one of groupCol / aggFunc / outExpr is set (non-visible entries
	// harvested from HAVING/ORDER BY always have aggFunc set).
	groupCol string // plain group-by column reference
	// groupColBare: the structural bare name of groupCol (parse-tree/derived
	// at set time) — consumers never dot-split groupCol.
	groupColBare string
	// groupColQualifier/groupColQualified: parse-tree qualification of the
	// groupCol reference (FullId segment count), for the scope validation
	// that rejects an AMBIGUOUS bare re-read (42702) instead of last-wins
	// binding one leg's key. Empty/false for expression-redirected entries.
	groupColQualifier string
	groupColQualified bool
	// groupColSegs is the FULL ordered segment list of the reference (`a.n.sk` ->
	// [A N SK]). It is what RESOLUTION consumes; qualifier is a rendering and
	// cannot express where one segment ends and the next begins.
	groupColSegs []string
	aggFunc      string // COUNT/SUM/MIN/MAX/AVG
	aggArg       string // argument column name — set only when arg is a bare column; used for the proto-path FD fast path. Empty for COUNT(*) and for expression args.
	// aggExpr is the IExpressionContext of the aggregate's argument when it is not a bare
	// column reference (e.g. SUM(qty*price), AVG(CASE ... END)). Evaluated per input row.
	// nil for bare-column args and for COUNT(*).
	aggExpr     antlrgen.IExpressionContext
	aggDistinct bool // true when COUNT(DISTINCT col)
	// aggArgQualified/aggArgBare: parse-tree qualification and LAST SEGMENT
	// of the bare-column arg (FullId) — never a dot scan of the rendered
	// name, which a delimited identifier containing a literal dot would
	// false-positive.
	aggArgQualified bool
	aggArgBare      string
	aggArgQualifier string
	// aggArgSegs is the FULL ordered segment list of the reference (`a.n.sk` ->
	// [A N SK]). It is what RESOLUTION consumes; qualifier is a rendering and
	// cannot express where one segment ends and the next begins.
	aggArgSegs []string
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
func extractAggFunc(e *antlrgen.SelectExpressionElementContext) (funcName, argCol string, argExpr antlrgen.IExpressionContext, alias string, distinct, argQualified bool, argBare, argQualifier string, argSegs []string, ok bool) {
	pred, pok := e.Expression().(*antlrgen.PredicatedExpressionContext)
	if !pok {
		return "", "", nil, "", false, false, "", "", nil, false
	}
	fc, fcok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !fcok {
		return "", "", nil, "", false, false, "", "", nil, false
	}
	agg, aggok := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
	if !aggok {
		return "", "", nil, "", false, false, "", "", nil, false
	}
	awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
	if !awfok {
		return "", "", nil, "", false, false, "", "", nil, false
	}
	fn, arg, aExpr, outName, isDistinct, argQual, argBare, argQualifier, argSegs, fieldsOk := extractAwfFields(awf)
	if !fieldsOk {
		return "", "", nil, "", false, false, "", "", nil, false
	}
	// SELECT-list-only overlay: an explicit `AS alias` on the SELECT element
	// wins over the reconstructed default ("SUM(v)") as the output column
	// name.
	if e.Uid() != nil {
		outName = functions.StripIdentifierQuotes(e.Uid().GetText())
	}
	return fn, arg, aExpr, outName, isDistinct, argQual, argBare, argQualifier, argSegs, true
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
func extractAwfFields(awf *antlrgen.AggregateWindowedFunctionContext) (funcName, argCol string, argExpr antlrgen.IExpressionContext, outName string, distinct, argQualified bool, argBare, argQualifier string, argSegs []string, ok bool) {
	distinct = awf.DISTINCT() != nil
	resolveArg := func(fa antlrgen.IFunctionArgContext) {
		if fa == nil {
			return
		}
		expr := fa.Expression()
		if pred, ok := expr.(*antlrgen.PredicatedExpressionContext); ok {
			if col, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
				fid := col.FullColumnName().FullId()
				argCol = functions.FullIdToName(fid)
				// Qualification and segments are PARSE-TREE truth, never a
				// dot scan of the rendered name — a delimited identifier
				// containing a literal dot ("a.b") is ONE unqualified
				// segment.
				uids := fid.AllUid()
				argQualified = len(uids) > 1
				argBare = functions.StripIdentifierQuotes(uids[len(uids)-1].GetText())
				// EVERY segment is carried, not just the leading ones joined:
				// an aggregate over a struct descent (`COUNT(a.n.sk)`) needs the
				// boundaries, and the joined form asks for a source "A.N".
				argSegs = make([]string, len(uids))
				for qi, u := range uids {
					argSegs[qi] = functions.StripIdentifierQuotes(u.GetText())
				}
				if argQualified {
					argQualifier = strings.Join(argSegs[:len(uids)-1], ".")
				}
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
	case awf.MAX_EVER() != nil:
		// MAX_EVER / MIN_EVER are index-only aggregates (Java's monotone
		// extremum family): the grammar admits them beside AVG/MAX/MIN/SUM
		// (RelationalParser.g4 aggregateWindowedFunction) and the AS-SELECT
		// index generator consumes them (RFC-202 S3). As QUERY aggregates the
		// translator declines them typed (aggregateFunctionByName), matching
		// Java where an extremum query is only ever served by its index.
		funcName = "MAX_EVER"
		resolveArg(awf.FunctionArg())
	case awf.MIN_EVER() != nil:
		funcName = "MIN_EVER"
		resolveArg(awf.FunctionArg())
	case awf.BITMAP_CONSTRUCT_AGG() != nil:
		// Own grammar alternative: BITMAP_CONSTRUCT_AGG '(' functionArg ')'
		// — no ALL/DISTINCT aggregator. Index-only, like the extremum family.
		funcName = "BITMAP_CONSTRUCT_AGG"
		resolveArg(awf.FunctionArg())
	default:
		return "", "", nil, "", false, false, "", "", nil, false
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
	return funcName, argCol, argExpr, outName, distinct, argQualified, argBare, argQualifier, argSegs, true
}

// columnNameFromExpr extracts a plain column name (or aggregate output name like
// "COUNT(*)") from an IExpressionContext.
// context is used in error messages (e.g. "SELECT expression", "ORDER BY expression").
// exprIsBareColumnRef reports whether expr is a plain column reference
// with exactly ONE identifier segment — the only shape SQL binds to an
// output alias. Everything else (qualified `d.x`, aggregate or computed
// expressions whose canonical rendering may collide with a delimited
// alias) must never alias-bind. The decision comes from the parse tree,
// never from the JOINED name text — a delimited identifier can itself
// contain a dot or parentheses (`AS "x.y"`, `AS "SUM(S)"`), and after
// FullIdToName strips the quotes the spellings are indistinguishable.
func exprIsBareColumnRef(expr antlrgen.IExpressionContext) bool {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok || pred.Predicate() != nil {
		return false
	}
	a, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return false
	}
	return len(a.FullColumnName().FullId().AllUid()) == 1
}

// splitColumnRef reads the parse-tree segments of a plain column-reference
// expression: bare = last FullId segment, qualifier = the joined leading
// segments, qualified = segment count > 1, segs = EVERY segment in order.
// Zero values when expr is not a plain column reference (the caller has
// already classified it via columnNameFromExpr).
//
// `segs` is the resolution-grade carrier and `qualifier` is only a rendering.
// Joining the leading segments loses the boundary between them, so a
// three-segment reference `a.n.sk` arrives at the scope as a lookup for a
// source or struct column literally named "A.N" — which exists nowhere, and
// the reference dies as UNDEFINED_COLUMN even though every segment resolves.
// Callers that RESOLVE must pass segs; callers that only display may keep
// using qualifier.
func splitColumnRef(expr antlrgen.IExpressionContext) (bare, qualifier string, qualified bool, segs []string) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok || pred.Predicate() != nil {
		return "", "", false, nil
	}
	atom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return "", "", false, nil
	}
	uids := atom.FullColumnName().FullId().AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		// StripIdentifierQuotes folds unquoted segments and preserves
		// quoted ones — the SQL binding semantics — but DISCARDS the
		// per-segment quoted flag, so downstream cannot tell `"ID"`
		// (quoted upper) from `id` (folded). Both bind the same column
		// today; the flag must be carried (semantic.Identifier per
		// segment) no later than WS-N Phase D, where case-faithful
		// registrations make the distinction observable.
		parts[i] = functions.StripIdentifierQuotes(u.GetText())
	}
	if len(parts) == 0 {
		return "", "", false, nil
	}
	bare = parts[len(parts)-1]
	if len(parts) > 1 {
		qualifier = strings.Join(parts[:len(parts)-1], ".")
		qualified = true
	}
	return bare, qualifier, qualified, parts
}

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
		_, _, _, outName, _, _, _, _, _, ok := extractAwfFields(awf)
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
// limit/offset fields.
//
// Embedded by selectQuery so the classification fields are promoted —
// code that reads sq.projCols, sq.aggCols, etc. works unchanged.
// PlanVisitor constructs a selectQuery directly from a
// selectClassification + fromSource without any bridge method.
// projCol is one projected column: the name (the load-bearing OUTPUT label —
// every naming consumer reads it) plus the parse-tree reference segments for
// plain column items (zero values for computed/sentinel entries, and cleared
// by any rebase that rewrites the name to internal text — the group-key
// rule). RFC-180 F-3: consumers never re-parse the name.
type projCol struct {
	name      string
	bare      string
	qualifier string
	qualified bool
	// segs is the FULL ordered segment list of the reference (`a.n.sk` ->
	// [A N SK]). It is what RESOLUTION consumes; qualifier is a rendering and
	// cannot express where one segment ends and the next begins.
	segs []string
	// selectOrdinal is the immutable one-based SQL SELECT-list position. It
	// follows this item when it is reclassified into aggSelectCol.
	selectOrdinal int
}

// projColRef is the parse-tree segment triple for one projected column, stated
// against the name the builder actually emitted.
//
// The reconciliation is the point rather than a formality. The emitted name is
// not always projCol.name — the derived-table shell strips a `X.` qualifier
// prefix off the rendering — and a segment triple that describes a DIFFERENT
// string than the one downstream carries is worse than no triple at all: it
// would authorize a qualified reading of a name whose qualifier is gone. So the
// triple is only claimed Present when it demonstrably spells the emitted name;
// anything else reads as "not captured", which every consumer must already
// handle.
func projColRef(col projCol, rendered string) logical.ColumnRef {
	return logical.ColumnRefFor(col.bare, col.qualifier, col.qualified, rendered)
}

// projColNames renders the name list for name-only consumers.
func projColNames(cols []projCol) []string {
	if cols == nil {
		return nil
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out
}

type selectClassification struct {
	projCols    []projCol // nil = SELECT * or SELECT <qualifier>.*; ignored when countStar or aggCols non-empty
	projAliases []string  // parallel to projCols; empty string = no alias (use column name)
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
	// groupBy holds the GROUP BY keys (nil = no GROUP BY), each captured
	// STRUCTURALLY from the parse tree — the display text is a rendering,
	// never re-parsed (RFC-180 F-3).
	groupBy []groupKeyRef
	// groupByAliases maps UPPERCASE `GROUP BY col AS alias` alias names to
	// their index in groupBy. Used at parse time to resolve SELECT-list
	// references to a GROUP BY alias (`SELECT x FROM t GROUP BY col1 AS x`)
	// — the SELECT-list column gets rewritten to the underlying group-by
	// name with the alias preserved as the output column name. Nil = no
	// aliased GROUP BY entries.
	groupByAliases map[string]int
	// havingExpr is the HAVING clause expression (nil = no HAVING).
	havingExpr antlrgen.IExpressionContext
	// qualifyExpr is the QUALIFY clause expression (nil = no QUALIFY).
	// QUALIFY filters on window-function results — in this port it carries
	// the vector K-NN ROW_NUMBER() OVER (... ORDER BY <distance>) <= K
	// predicate, which lowers to a DistanceRank comparison.
	qualifyExpr antlrgen.IExpressionContext
	// postAggExprs is populated by the visitor's visitSelectGroupBy when
	// post-aggregation computed projections are emitted.
	postAggExprs []antlrgen.IExpressionContext
	// postSortStripProj / postSortStripAliases are populated by the
	// visitor's visitSelectGroupBy when non-visible aggregate columns
	// need stripping after the Sort operator.
	postSortStripProj    []string
	postSortStripAliases []string
	// postSortAggregateOutputOrdinals / postSortIsComputed stay parallel to
	// postSortStripProj for the aggregate-output Project deliberately emitted
	// after ORDER BY. Direct slots address the aggregate's native output ABI;
	// negative slots are computed from postAggExprs.
	postSortAggregateOutputOrdinals []int
	postSortIsComputed              []bool
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
		sq.sourceSegments = fs.sourceSegments
		sq.joins = fs.joins
		sq.derivedQuery = fs.derivedQuery
		sq.inlineValues = fs.inlineValues
		sq.whereExpr = fs.whereExpr
		sq.catalogAwareInnerPlan = fs.catalogAwareInnerPlan
	}
	return sq
}

// classifySelectElements walks the SELECT, GROUP BY, HAVING, ORDER BY,
// and LIMIT clauses of a SimpleTableContext and returns a
// selectClassification. This is the pure parse-tree classification
// logic extracted from extractFromSimpleTable — it does NOT parse the
// FROM clause. Both extractFromSimpleTable (proto path) and
// PlanVisitor.VisitSimpleTable (Cascades path) delegate here.
// starExpander returns the columns a SELECT-list star expands to, in FROM
// order, for one qualifier ("" = the bare `*`). ok=false means the sources'
// columns are not knowable here — deliberately distinct from an empty slice,
// because "expands to nothing" and "cannot be expanded" need opposite handling
// and collapse onto the same value otherwise.
type starExpander func(qualifier string) ([]projCol, bool)

func classifySelectElements(simpleTable *antlrgen.SimpleTableContext, expandStar starExpander) (*selectClassification, error) {
	// Parse SELECT list: either *, a list of column name expressions, COUNT(*), or
	// a GROUP BY aggregate list (mix of group-by columns + aggregate functions).
	selElems := simpleTable.SelectElements()
	var projCols []projCol                      // nil = SELECT * or SELECT <qualifier>.*
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
		for selectIdx, elem := range elems {
			selectOrdinal := selectIdx + 1
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
					projCols = append(projCols, projCol{selectOrdinal: selectOrdinal}) // sentinel; actual names resolved at execution
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
				} else if fn, argCol, argExpr, alias, isDistinct, argQual, argBare, argQualifier, argSegs, isAgg := extractAggFunc(e); isAgg {
					if containsNestedAggregateInSelectElement(e, argExpr) {
						return nil, api.NewError(api.ErrCodeUnsupportedOperation,
							"unsupported nested aggregate(s)")
					}
					aggCols = append(aggCols, aggSelectCol{outName: alias, selectOrdinal: selectOrdinal, aggFunc: fn, aggArg: argCol, aggExpr: argExpr, aggDistinct: isDistinct, aggArgQualified: argQual, aggArgBare: argBare, aggArgQualifier: argQualifier, aggArgSegs: argSegs, visible: true})
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
							aggCols = append(aggCols, aggSelectCol{outName: outName, selectOrdinal: selectOrdinal, outExpr: expr, visible: true})
						case expr != nil && !exprReferencesColumn(expr):
							aggCols = append(aggCols, aggSelectCol{outName: outName, selectOrdinal: selectOrdinal, outExpr: expr, visible: true})
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
							aggCols = append(aggCols, aggSelectCol{outName: outName, selectOrdinal: selectOrdinal, outExpr: expr, visible: true})
						default:
							gcBare, gcQual, gcQualified, gcSegs := splitColumnRef(e.Expression())
							if gcBare == "" {
								gcBare = colName
							}
							aggCols = append(aggCols, aggSelectCol{outName: outName, selectOrdinal: selectOrdinal, groupCol: colName, groupColBare: gcBare, groupColQualifier: gcQual, groupColQualified: gcQualified, groupColSegs: gcSegs, visible: true})
						}
					} else {
						pc := projCol{name: colName, selectOrdinal: selectOrdinal}
						if expr == nil {
							// Plain column reference: parse-tree segments.
							pc.bare, pc.qualifier, pc.qualified, pc.segs = splitColumnRef(e.Expression())
						}
						projCols = append(projCols, pc)
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
			var newProjCols []projCol
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
					outName = col.name
				}
				promoted = append(promoted, aggSelectCol{
					outName:       outName,
					selectOrdinal: col.selectOrdinal,
					outExpr:       projExprs[i],
					visible:       true,
				})
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
					out = c.name
				}
				var slotExpr antlrgen.IExpressionContext
				if i < len(projExprs) {
					slotExpr = projExprs[i]
				}
				switch {
				case slotExpr != nil && !exprReferencesColumn(slotExpr):
					extra[i] = aggSelectCol{outName: out, selectOrdinal: c.selectOrdinal, outExpr: slotExpr, visible: true}
				case slotExpr != nil:
					// Expression on group-by columns (no aggregates, no
					// constants-only). Java permits this when all referenced
					// columns are in GROUP BY. Route to outExpr — evaluated
					// post-aggregation against the rowMap holding group-by
					// values. Symmetric with the in-SELECT-loop case at the
					// mixed-agg classification site above.
					extra[i] = aggSelectCol{outName: out, selectOrdinal: c.selectOrdinal, outExpr: slotExpr, visible: true}
				default:
					extra[i] = aggSelectCol{outName: out, selectOrdinal: c.selectOrdinal, groupCol: c.name, groupColBare: colBareOrName(c), groupColQualifier: c.qualifier, groupColQualified: c.qualified, groupColSegs: c.segs, visible: true}
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

	// Parse QUALIFY clause (window-function filter; carries the vector
	// K-NN ROW_NUMBER() OVER (...) <= K predicate).
	if qc, ok := simpleTable.QualifyClause().(*antlrgen.QualifyClauseContext); ok && qc != nil {
		cls.qualifyExpr = qc.Expression()
	}

	// Parse ORDER BY clause.
	orderByClauseCtx := simpleTable.OrderByClause()
	if orderByClauseCtx != nil {
		// Java errors 42701 (COLUMN_ALREADY_EXISTS) on `ORDER BY b, b`
		// with the same column repeated. Stricter than Postgres, but
		// per the 100% Java-alignment principle we match.
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
			posName, pos, isPos, posErr := resolveSelectListPosition("ORDER BY", obExpr.Expression(), projColNames(projCols), projAliases, aggCols, countStar)
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
				cls.orderBy = append(cls.orderBy, orderByClause{colName: posName, pos: pos, ascending: ascending, nullsFirst: nullsFirst, rawExpr: obExpr.Expression()})
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
				kb, kq, kqf, ksegs := splitColumnRef(obExpr.Expression())
				cls.orderBy = append(cls.orderBy, orderByClause{colName: colName, ascending: ascending, nullsFirst: nullsFirst, rawExpr: obExpr.Expression(), bareRef: exprIsBareColumnRef(obExpr.Expression()), bare: kb, qualifier: kq, qualified: kqf, segs: ksegs})
			} else {
				cls.orderBy = append(cls.orderBy, orderByClause{ascending: ascending, nullsFirst: nullsFirst, expr: obExpr.Expression(), rawExpr: obExpr.Expression()})
			}
		}
	}

	// Go extension: LIMIT / OFFSET accepted (most-requested feature).
	// Java's fdb-relational 4.11.1.0 rejects them. Go applies them via a
	// uniform RecordQueryLimitPlan operator at the LIMIT's pipeline position
	// (RFC-128) — not post-execution.

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
			posName, _, isPos, posErr := resolveSelectListPosition("GROUP BY", item.Expression(), projColNames(projCols), projAliases, cls.aggCols, cls.countStar)
			if posErr != nil {
				return nil, posErr
			}
			if isPos {
				cls.groupBy = append(cls.groupBy, groupKeyRef{display: posName, bare: posName})
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
					cls.groupBy = append(cls.groupBy, groupKeyRef{
						display: canonicalTextOf(selectExprsSnapshot[i]),
						expr:    selectExprsSnapshot[i],
					})
					redirected = true
					break
				}
				if !redirected {
					bare, qualifier, qualified, segs := splitColumnRef(item.Expression())
					cls.groupBy = append(cls.groupBy, groupKeyRef{
						display:   colName,
						bare:      bare,
						qualifier: qualifier,
						qualified: qualified,
						segs:      segs,
					})
				}
			} else {
				// Synthesize a display name from the expression text; the
				// value used for grouping comes from evaluating the expr.
				cls.groupBy = append(cls.groupBy, groupKeyRef{
					display: canonicalTextOf(item.Expression()),
					expr:    item.Expression(),
				})
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
		// aliasResolves maps a GROUP BY alias to its underlying key —
		// the DISPLAY string for the name/datum channel AND the key's
		// structural segments, so rewrites keep both channels in sync
		// (a display like "X.COL1" is a rendered join, never a bare).
		aliasResolves := func(name string) (key groupKeyRef, outName string, ok bool) {
			idx, aliased := cls.groupByAliases[strings.ToUpper(name)]
			if !aliased {
				return groupKeyRef{}, "", false
			}
			if cls.groupBy[idx].expr != nil {
				return groupKeyRef{}, "", false
			}
			return cls.groupBy[idx], name, true
		}
		for i := range cls.projCols {
			if i < len(cls.projExprs) && cls.projExprs[i] != nil {
				continue
			}
			col := cls.projCols[i]
			if col.name == "" {
				continue
			}
			key, outName, ok := aliasResolves(col.name)
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
			// The rebased name is the underlying GROUP BY column text (the
			// datum channel); segments come from the KEY so a qualified
			// underlying keeps its real qualifier.
			cls.projCols[i] = projCol{name: key.display, bare: key.bare, qualifier: key.qualifier, qualified: key.qualified, selectOrdinal: col.selectOrdinal}
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
				if key, outName, ok := aliasResolves(ac.groupCol); ok {
					ac.groupCol = key.display
					ac.groupColBare = key.bare
					ac.groupColQualifier = key.qualifier
					ac.groupColQualified = key.qualified
					ac.groupColSegs = append([]string(nil), key.segs...)
					if ac.outName == "" {
						ac.outName = outName
					}
				}
			}
			if ac.aggFunc != "" && ac.aggArg != "" && ac.aggExpr == nil {
				// Rewrite arg only; aggregate's outName (e.g. `MAX(z)`)
				// is already set at parse time and shouldn't be
				// collapsed to the alias string.
				if key, _, ok := aliasResolves(ac.aggArg); ok {
					ac.aggArg = key.display
					ac.aggArgBare = key.bare
					ac.aggArgQualifier = key.qualifier
					ac.aggArgQualified = key.qualified
					ac.aggArgSegs = append([]string(nil), key.segs...)
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
			if key, _, ok := aliasResolves(ob.colName); ok {
				ob.colName = key.display
				// The structural segments must follow the rewrite — a
				// stale pre-rewrite bare would re-validate the ALIAS
				// against the FROM scope and 42703; a display copied
				// into bare would re-split a qualified key. Both
				// channels come from the group KEY.
				ob.bare = key.bare
				ob.qualifier = key.qualifier
				ob.qualified = key.qualified
				ob.segs = append([]string(nil), key.segs...)
			}
		}
	}

	// SQL §7.10 General Rule 1 / Java alignment: when GROUP BY is present,
	// every SELECT-list column reference must be in GROUP BY or wrapped in
	// an aggregate.
	//
	// THE STAR IS EXPANDED FIRST, AND ONLY THEN VALIDATED — that order is the
	// whole rule. Java expands against the FROM-side operators
	// (ExpressionVisitor.java:145-147 → SemanticAnalyzer.expandStar:321-368)
	// and validates each EXPANDED item against the grouping keys afterwards
	// (LogicalOperator.java:436-439, isComposableFrom). Go ran the two in the
	// opposite order: a blanket refusal here meant the star was NEVER expanded,
	// so `select * from (select col1 from T1) as X group by col1` — whose
	// expansion is exactly the grouping key, and which Java answers — was
	// refused as if it were `select * from T1 group by col1`, which genuinely
	// does expand to ungrouped columns. One shape's correct answer was standing
	// in for the other's.
	//
	// After expansion the star is an ordinary explicit column list, so the
	// per-column 42803 is minted where every other projection's is:
	// validateGroupByProjection. Nothing downstream needs to know a star was
	// ever here.
	if len(cls.groupBy) > 0 && len(projCols) == 0 && !countStar && len(cls.aggCols) == 0 {
		// projCols == nil + projQualifier == "" → SELECT *
		// projCols == nil + projQualifier != "" → SELECT qualifier.*
		var expanded []projCol
		var ok bool
		if expandStar != nil {
			expanded, ok = expandStar(projQualifier)
		}
		if !ok {
			// An ASSERTED refusal, never a silent fall-through: with the
			// sources' columns unknown there is no expansion to validate, and
			// continuing would drop the GROUP BY and emit every source row.
			// This is the pre-expansion behaviour, kept for the parse-only
			// callers that hold no metadata.
			return nil, api.NewError(api.ErrCodeGroupingError,
				"SELECT * cannot be used with GROUP BY (every column must be in GROUP BY or aggregated)")
		}
		// The expansion IS the SELECT list, so its positions are the SELECT-list
		// positions. selectOrdinal is one-based and load-bearing downstream: the
		// aggregate output contract asserts OutputSlots[i].SelectOrdinal == i+1
		// (validateExactAggregateProjectContract), so leaving it at the zero
		// value fails every expanded star with an internal 0AF00 rather than
		// answering it. The expander cannot set this itself — it does not know
		// whether it is filling the whole list or one slot of a mixed one.
		for i := range expanded {
			expanded[i].selectOrdinal = i + 1
		}
		projCols = expanded
		projAliases = make([]string, len(expanded))
		projExprs = make([]antlrgen.IExpressionContext, len(expanded))
		projStarQualifiers = make([]string, len(expanded))
		projQualifier = ""
		cls.projCols = projCols
		cls.projAliases = projAliases
		cls.projExprs = projExprs
		cls.projStarQualifiers = projStarQualifiers
		cls.projQualifier = ""
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
				// GROUP BY; Go matches (42803, not 0A000).
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
				out = c.name
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
				extra[i] = aggSelectCol{outName: out, selectOrdinal: c.selectOrdinal, outExpr: slotExpr, visible: true}
			default:
				extra[i] = aggSelectCol{outName: out, selectOrdinal: c.selectOrdinal, groupCol: c.name, groupColBare: colBareOrName(c), groupColQualifier: c.qualifier, groupColQualified: c.qualified, groupColSegs: c.segs, visible: true}
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
			for _, gn := range cls.groupBy {
				if gn.expr != nil && projText == gn.display {
					cls.aggCols[ai].groupCol = gn.display
					cls.aggCols[ai].groupColBare = gn.display
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
			for _, gn := range cls.groupBy {
				if gn.expr != nil && outExprText == gn.display {
					cls.aggCols[ai].outExpr = nil
					cls.aggCols[ai].groupCol = gn.display
					cls.aggCols[ai].groupColBare = gn.display
					break
				}
			}
		}
	}

	// countStar fast path assumes a single synthetic row. With GROUP BY
	// present we need a per-group COUNT(*), so demote to aggCols. The
	// alias (if any) propagates so `SELECT COUNT(*) AS n FROM t GROUP BY g`
	// emits the column as `n`. The visible aggCol is the complete public-output
	// contract: buildSelectShell records its native aggregate ordinal and builds
	// the one reshaping projection. Synthesizing a parallel projCols entry here
	// would add a second, ordinary projection without aggregate ordinals and
	// sever that exact slot contract (notably inside a UNION-derived table).
	if cls.countStar && len(cls.groupBy) > 0 {
		cls.countStar = false
		outName := "COUNT(*)"
		if cls.countStarAlias != "" {
			outName = cls.countStarAlias
		}
		cls.aggCols = append(cls.aggCols, aggSelectCol{outName: outName, selectOrdinal: 1, aggFunc: "COUNT", visible: true})
	}

	// Go extension: Java's fdb-relational 4.11.1.0 does not support ORDER BY on
	// aggregate expressions (e.g. ORDER BY SUM(v)); its planner never reaches this path.
	//
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
			if hasNestedAggregateInTree(hexpr) {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"unsupported nested aggregate(s)")
			}
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
		obAggIdx := 0
		for obIdx, ob := range cls.orderBy {
			if ob.expr == nil || len(harvestAggregates(ob.expr)) == 0 {
				continue
			}
			outName := ""
			if obAggIdx > 0 {
				outName = fmt.Sprintf("__ob_agg_%d__", obAggIdx)
			}
			newAggs = append(newAggs, aggSelectCol{
				outExpr: ob.expr,
				outName: outName,
			})
			cls.orderBy[obIdx].colName = outName
			// Synthetic aggregate output name — not a source column
			// reference; clear the segments so validation never resolves
			// it against the FROM scope.
			cls.orderBy[obIdx].bare = ""
			cls.orderBy[obIdx].qualifier = ""
			cls.orderBy[obIdx].qualified = false
			cls.orderBy[obIdx].expr = nil
			obAggIdx++
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
						out = c.name
					}
					gc := c.name
					gcBare := colBareOrName(c)
					gcQual, gcQualified := c.qualifier, c.qualified
					gcSegs := c.segs
					if i < len(projExprs) && projExprs[i] != nil {
						projText := canonicalTextOf(projExprs[i])
						for _, gn := range cls.groupBy {
							if gn.expr != nil && projText == gn.display {
								gc = gn.display
								gcBare = gn.display
								gcQual, gcQualified, gcSegs = "", false, nil
								break
							}
						}
					}
					prepended = append(prepended, aggSelectCol{outName: out, selectOrdinal: c.selectOrdinal, groupCol: gc, groupColBare: gcBare, groupColQualifier: gcQual, groupColQualified: gcQualified, groupColSegs: gcSegs, visible: true})
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
	return extractFromSimpleTableWithStar(simpleTable, nil)
}

// extractFromSimpleTableWithStar is extractFromSimpleTable with a star
// expander. The parameterless form is the parse-only callers, which hold no
// metadata and therefore cannot expand a star under GROUP BY; they get the
// asserted refusal in classifySelectElements rather than a silent GROUP BY drop.
func extractFromSimpleTableWithStar(simpleTable *antlrgen.SimpleTableContext, expandStar starExpander) (*selectQuery, error) {
	// FROM is parsed FIRST because the star expander is derived from it: the
	// SELECT list cannot be classified until the star's columns are known.
	fs, err := parseFromSource(simpleTable)
	if err != nil {
		return nil, err
	}

	cls, err := classifySelectElements(simpleTable, expandStar)
	if err != nil {
		return nil, err
	}

	sq := selectQueryFromClassification(cls, fs)
	// Capture LIMIT/OFFSET. The selectQuery path (union branches, derived
	// tables) builds via buildLogicalPlanForSelect, which reads sq.limit/
	// sq.offset — unlike the live VisitSimpleTable path which reads the
	// clause directly. Without this a per-branch UNION LIMIT is silently
	// dropped (RFC-128 §4.7).
	var limitErr error
	if sq.limit, sq.offset, limitErr = parseLimitClause(simpleTable); limitErr != nil {
		return nil, limitErr
	}
	return sq, nil
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
// Used by the Cascades aggregate builder's pre-check to detect ungrouped
// column references in outExpr projection entries (42803 vs 42703
// distinction). Refs inside aggregate calls are correctly computed by the
// aggregate itself — walking into them would flag false positives.
func harvestColumnRefs(expr antlrgen.IExpressionContext) []string {
	return harvestColumnRefsImpl(expr, false)
}

// harvestColumnRefsOutsideSubqueries is harvestColumnRefs with the
// harvestAggregates nested-query-scope boundary: refs syntactically inside a
// subquery bind to THAT query block, not the enclosing SELECT. The CTE
// ON-only derivation validates its body's INPUT reads with this variant — a
// scalar-subquery item's local columns are not reads of the derived source
// (the subquery's own build resolves them in its own scope; a CORRELATED
// read into the derived source surfaces through that build, loud at
// translation). The GROUP-BY validator keeps the non-stopping walk: a
// correlated ref into the outer query DOES need the group-check there.
// harvestBareColumnRefsOutsideSubqueries is the STRUCTURAL variant: it
// returns each referenced column's bare LAST SEGMENT per the parse tree —
// never a dot split of the rendered name, which a delimited identifier
// containing a literal dot would corrupt. Same walk boundaries as the
// rendering variant.
func harvestBareColumnRefsOutsideSubqueries(expr antlrgen.IExpressionContext) []string {
	if expr == nil {
		return nil
	}
	var bares []string
	seen := map[string]bool{}
	var visit func(n antlr.Tree)
	visit = func(n antlr.Tree) {
		if n == nil {
			return
		}
		switch n.(type) {
		case *antlrgen.QueryContext, antlrgen.IQueryExpressionBodyContext:
			return
		}
		if fc, ok := n.(*antlrgen.FunctionCallExpressionAtomContext); ok {
			if _, isAgg := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext); isAgg {
				return
			}
		}
		if c, ok := n.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
			uids := c.FullColumnName().FullId().AllUid()
			bare := functions.StripIdentifierQuotes(uids[len(uids)-1].GetText())
			if !seen[bare] {
				seen[bare] = true
				bares = append(bares, bare)
			}
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			visit(n.GetChild(i))
		}
	}
	visit(expr)
	return bares
}

func harvestColumnRefsImpl(expr antlrgen.IExpressionContext, stopAtNestedQuery bool) []string {
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
		if stopAtNestedQuery {
			// Same boundary as harvestAggregates: a `query` node (scalar
			// `(SELECT …)`, EXISTS) or a bare `queryExpressionBody` (IN
			// subquery) opens a nested scope; match the INTERFACE for the
			// body — the concrete node of the alternative-labelled rule is
			// never a bare *QueryExpressionBodyContext.
			switch n.(type) {
			case *antlrgen.QueryContext, antlrgen.IQueryExpressionBodyContext:
				return
			}
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
		// Stop at every NESTED QUERY SCOPE: an aggregate syntactically inside a
		// subquery belongs to THAT subquery's query block, not the enclosing
		// SELECT (same scoping Java's SemanticAnalyzer applies — an aggregate
		// binds to its innermost query scope). A subquery in an expression is
		// carried by a `query` node (scalar `(SELECT …)` and EXISTS both wrap
		// one) or, when there is no `query` wrapper, by a bare
		// `queryExpressionBody` (an IN subquery: `x IN (SELECT COUNT(*) FROM e)`
		// → InPredicate → InList → queryExpressionBody). `queryExpressionBody`
		// is an ALTERNATIVE-LABELLED rule, so the concrete node is
		// *QueryTermDefaultContext / *SetQueryContext (which EMBED
		// QueryExpressionBodyContext and implement IQueryExpressionBodyContext) —
		// never a bare *QueryExpressionBodyContext — so match the INTERFACE, not
		// that concrete type. Guarding the nested query node (not per-atom types)
		// is the complete boundary and cannot be out-enumerated by a new subquery
		// atom. Without it the nested aggregate leaks into the OUTER query's
		// aggregate set — mis-promoting the outer slot (dropping it from
		// projCols) or wrongly classifying the outer query as an aggregate
		// (spurious 42803 on its non-grouped columns). Guarding *QueryContext
		// (not only the body) also truncates a subquery's own `ctes?` — a WITH
		// aggregate belongs to the CTE, not the enclosing SELECT. A real outer
		// aggregate that merely CONTAINS a subquery (`HAVING COUNT(*) IN (…)`) is
		// harvested normally — it lives OUTSIDE the subquery's query node, which
		// the pre-order walk reaches first.
		switch n.(type) {
		case *antlrgen.QueryContext, antlrgen.IQueryExpressionBodyContext:
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

// queryInnerIsExactlyOneRowBeforePagination reports whether an EXISTS subquery
// body is a NON-GROUPED aggregate that produces EXACTLY ONE row before its
// LIMIT/OFFSET is applied. A non-grouped COUNT(*)/MAX/SUM yields one row even
// over an empty (post-WHERE) input (COUNT->0, MAX/SUM->NULL). GROUP BY and
// HAVING/QUALIFY can change that cardinality, while a WINDOWED aggregate is
// row-preserving (one output per input row), so those shapes are excluded.
//
// Pagination is deliberately not inspected here. The caller first establishes
// this one-row cardinality and only then applies LIMIT/OFFSET, matching SQL's
// operator order.
func queryInnerIsExactlyOneRowBeforePagination(q antlrgen.IQueryContext) bool {
	if q == nil {
		return false
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return false
	}
	if _, stOk := body.QueryTerm().(*antlrgen.SimpleTableContext); !stOk {
		return false
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil {
		return false
	}
	if len(sq.groupBy) > 0 || sq.havingExpr != nil || sq.qualifyExpr != nil {
		return false
	}
	hasRealAggregate := sq.countStar
	for i := range sq.aggCols {
		if sq.aggCols[i].aggFunc != "" {
			hasRealAggregate = true
			break
		}
	}
	if !hasRealAggregate {
		return false
	}
	return !queryScopeHasWindowedAggregate(body)
}

// queryScopeHasWindowedAggregate reports whether THIS query's own SELECT (not a
// nested subquery's) contains a windowed aggregate (`… OVER (…)`). Stops at
// nested query scopes — their window functions belong to them.
func queryScopeHasWindowedAggregate(n antlr.Tree) bool {
	if n == nil {
		return false
	}
	if awf, ok := n.(*antlrgen.AggregateWindowedFunctionContext); ok && awf.OverClause() != nil {
		return true
	}
	for i := 0; i < n.GetChildCount(); i++ {
		c := n.GetChild(i)
		switch c.(type) {
		case *antlrgen.QueryContext, antlrgen.IQueryExpressionBodyContext:
			continue // a nested subquery scope
		}
		if queryScopeHasWindowedAggregate(c) {
			return true
		}
	}
	return false
}

// aggColFromAwf reconstructs an aggSelectCol from an AggregateWindowedFunction
// context via the shared extractAwfFields helper. Output name matches the
// HAVING resolver's lookup name and the SELECT-list default alias
// ("COUNT(*)", "SUM(v)"). Returns false for unknown aggregate shapes.
func aggColFromAwf(awf *antlrgen.AggregateWindowedFunctionContext) (aggSelectCol, bool) {
	fn, argCol, argExpr, outName, isDistinct, argQual, argBare, argQualifier, argSegs, ok := extractAwfFields(awf)
	if !ok {
		return aggSelectCol{}, false
	}
	return aggSelectCol{
		outName:         outName,
		aggFunc:         fn,
		aggArg:          argCol,
		aggExpr:         argExpr,
		aggDistinct:     isDistinct,
		aggArgQualified: argQual,
		aggArgBare:      argBare,
		aggArgQualifier: argQualifier,
		aggArgSegs:      argSegs,
	}, true
}

// fromSource holds the parsed FROM-clause metadata: table name, alias,
// derived-query reference, JOIN clauses, and WHERE expression. Extracted
// from ANTLR by parseFromSource so both extractFromSimpleTable (which
// needs it for selectQuery assembly) and PlanVisitor.visitFrom (which
// builds the operator tree directly from ANTLR) share a single parsing
// path.
type fromSource struct {
	tableName      string
	tableAlias     string
	sourceSegments []string
	derivedQuery   antlrgen.IQueryContext
	inlineValues   *antlrgen.InlineTableItemContext
	joins          []joinClause
	whereExpr      antlrgen.IWhereExprContext
	// catalogAwareInnerPlan is the PRIMARY derived source's inner plan, built
	// through the catalog-aware path — the exact twin of
	// joinClause.catalogAwareInnerPlan, which has carried the same thing for
	// derived JOIN legs all along. Without it a rebuild (qualified/hidden star
	// expansion) re-entered the text-only builder for the primary source alone,
	// and the body came back with resolved Values on none of its projections.
	catalogAwareInnerPlan logical.LogicalOperator
}

// inlineValuesCarrierAlias returns the authored inline-table alias, or a
// deterministic private correlation when the optional definition is absent.
// It is parser metadata only: output column names remain in the carried parse
// node and are interpreted by the semantic builder in a later chunk.
func inlineValuesCarrierAlias(item *antlrgen.InlineTableItemContext, position int) string {
	if item != nil && item.InlineTableDefinition() != nil {
		definition := item.InlineTableDefinition()
		if definition.TableName() != nil {
			if alias := functions.FullIdToName(definition.TableName().FullId()); alias != "" {
				return alias
			}
		}
	}
	return fmt.Sprintf("Q$INLINE_VALUES%d", position)
}

// rejectAtOrdinality rejects an `AT atAlias` ordinality clause on a table
// source that can NEVER be a lateral array unnest — the PRIMARY FROM source
// and JOIN sources. Lateral unnest (`FROM t, t.arr AS x AT ord`) only
// occurs on a COMMA source whose dotted name resolves to a prior source's
// array field; those carry the AT alias through to the translator instead
// (it classifies and binds). AT on a genuine table/CTE/view is invalid —
// Java's WRONG_OBJECT_TYPE. R5 converges the rejection on the single
// ErrCodeWrongObjectType code so a rejection test is revert-proof
// (previously the parser threw ErrCodeUnsupportedQuery while scope_build
// threw UnsupportedFromShapeError — two errors for one shape). RFC-142.
func rejectAtOrdinality(item *antlrgen.AtomTableItemContext) error {
	if item != nil && item.GetAtAlias() != nil {
		return api.NewError(api.ErrCodeWrongObjectType,
			"AT ordinality is only valid on a correlated array source (FROM t, t.arr AS x AT ord), not on a table, CTE, or view")
	}
	return nil
}

// atAliasOf returns the quote-stripped `AT atAlias` ordinal alias of a
// table source, or "" when absent. Used for comma sources that may be a
// lateral array unnest (the translator binds the ordinal). RFC-142.
func atAliasOf(item *antlrgen.AtomTableItemContext) string {
	if item == nil || item.GetAtAlias() == nil {
		return ""
	}
	return functions.StripIdentifierQuotes(item.GetAtAlias().GetText())
}

// visibleFromAliases returns the set (upper-cased) of FROM-source aliases
// that are VISIBLE in the current query's FROM scope at the point of a comma
// source: the primary source's effective alias plus every PRIOR join's
// effective alias. A derived-table / CTE / subquery JOIN source contributes
// ONLY its outer alias (`d` in `(SELECT …) AS d`), never the table names
// hidden inside its body — those are out of scope here. This is the Go analog
// of Java's `currentPlanFragment.getLogicalOperatorsIncludingOuter()`: the
// visible in-scope quantifiers a correlated unnest may bind to.
//
// `priorJoins` is the slice of joins BEFORE the candidate comma source (the
// candidate may only correlate to a source to its left in the FROM list).
// RFC-142.
func visibleFromAliases(primaryTable, primaryAlias string, priorJoins []joinClause, resolvesToTable tableResolver) map[string]struct{} {
	set := make(map[string]struct{}, len(priorJoins)+1)
	add := func(table, alias string) {
		eff := alias
		if eff == "" {
			eff = table
		}
		if eff != "" {
			set[strings.ToUpper(eff)] = struct{}{}
		}
	}
	add(primaryTable, primaryAlias)
	for _, pj := range priorJoins {
		// A derived/CTE/subquery join exposes only its outer alias as a
		// visible source; the hidden body tables are out of scope.
		if pj.derivedQuery != nil || pj.catalogAwareInnerPlan != nil {
			if pj.alias != "" {
				set[strings.ToUpper(pj.alias)] = struct{}{}
			}
			continue
		}
		// Classify the prior leg against the set accumulated SO FAR (the aliases
		// visible to its left) using the SAME shape predicate the candidate uses.
		// A prior lateral-unnest leg exposes its element/ordinal binding alias,
		// not a real table; a prior table leg exposes its table alias. RFC-142.
		if pj.onExpr == nil && unnestCandidateShape(pj, set, resolvesToTable) {
			asAlias, atAlias := unnestAliases(pj)
			if asAlias != "" {
				set[strings.ToUpper(asAlias)] = struct{}{}
			}
			if atAlias != "" {
				set[strings.ToUpper(atAlias)] = struct{}{}
			}
			continue
		}
		add(pj.tableName, pj.alias)
	}
	return set
}

// lateralUnnestCandidate returns a LogicalUnnest for a comma FROM source
// that IS a lateral array unnest candidate (`FROM t, t.arr AS x [AT ord]`),
// else nil. The translator does the final array-vs-scalar / collision check.
//
// A source is a candidate IFF it is a plain comma source (no ON predicate, not
// a derived/CTE source) AND `unnestCandidateShape` holds. See that helper for
// the precise gate; in short:
//
//   - a DOTTED source (≥2 segments) is a candidate ONLY when segment 0 names a
//     VISIBLE in-scope FROM-source alias — NOT a schema-qualified table (`s.B`,
//     where `s` is a schema, not a source) and NOT a table hidden inside a
//     CTE/derived body (only the outer alias `d` is visible). This mirrors
//     Java's `generateAccess`, which resolves a FROM identifier as a
//     CTE/table/view/function FIRST and only falls through to
//     `resolveCorrelatedIdentifier` (an in-scope correlated field) otherwise.
//   - a source carrying an `AT` ordinal alias is ALWAYS a candidate, even when
//     segment 0 is not a visible source: AT explicitly requests ordinality,
//     which is valid ONLY on a correlated array, so the translator must reach
//     it and reject a non-array AT cleanly (WRONG_OBJECT_TYPE) rather than
//     silently dropping the AT and treating the source as a plain table.
//
// RFC-142.
func lateralUnnestCandidate(j joinClause, visible map[string]struct{}, resolvesToTable tableResolver) *logical.LogicalUnnest {
	if j.derivedQuery != nil || j.catalogAwareInnerPlan != nil || j.onExpr != nil {
		return nil
	}
	if !unnestCandidateShape(j, visible, resolvesToTable) {
		return nil
	}
	asAlias, atAlias := unnestAliases(j)
	return &logical.LogicalUnnest{
		Segments: j.segments,
		Alias:    asAlias,
		AtAlias:  atAlias,
		Binding:  j.bindingID,
	}
}

// tableResolver reports whether a dotted FROM-source name (its un-flattened uid
// segments) resolves to a real TABLE or CTE — i.e. Java's `tableExists` /
// `findCteMaybe`. When it returns true, `generateAccess`'s table/CTE branch wins
// and the source is NOT a correlated unnest. nil means "no metadata available
// here" (the parser-only path); the table-first demotion is then applied later
// by `demoteSchemaQualifiedUnnest` once metadata is in scope. RFC-142.
type tableResolver func(segments []string) bool

// unnestCandidateShape is the SINGLE classification predicate shared by the
// logical lowering (lateralUnnestCandidate) and the WHERE/projection scope
// binding (isLateralUnnestJoin). They MUST agree exactly or the scope source
// is registered for a shape the lowering treats as a table (or vice versa).
// `j` must already be known to be a plain comma source (no ON/derived).
//
// Mirrors Java's `LogicalOperator.generateAccess` resolution ORDER: a FROM
// identifier is a CTE/TABLE/view/function FIRST, and only falls through to a
// correlated array field otherwise. So a dotted source that `resolvesToTable`
// (a schema-qualified real table, e.g. `s.PB` where `s` is the schema name, or a
// CTE) is NOT an unnest — UNLESS it carries an AT ordinal alias, which Java
// rejects on a table with WRONG_OBJECT_TYPE; that AT case stays on the unnest
// path so the translator surfaces the faithful diagnostic. RFC-142.
func unnestCandidateShape(j joinClause, visible map[string]struct{}, resolvesToTable tableResolver) bool {
	// Only a COMMA-separated FROM source may be a lateral array unnest. Java
	// unnests via the comma-syntax `FROM t, t.arr AS x` correlated-field path
	// (generateCorrelatedFieldAccess); an explicit JOIN source — even an
	// `INNER JOIN t.arr AS x` with no ON clause — is always resolved as a
	// table/derived source (the JOIN visitor adds it as a normal operator),
	// never a lateral unnest. onExpr alone cannot distinguish the two (a
	// no-ON inner join also has onExpr == nil), so the origin flag gates here.
	// RFC-142 R5.
	if !j.fromComma {
		return false
	}
	// An AT ordinal alias forces the unnest LOGICAL node so the AT survives to the
	// translator / demoteSchemaQualifiedUnnest, which validate it (AT is valid only
	// on a correlated array; AT on a table → WRONG_OBJECT_TYPE). Even an
	// AT-on-a-schema-qualified-table stays a LogicalUnnest here so the AT is not
	// silently dropped into a plain table scan; the scope binding separately
	// declines to register a virtual unnest source for it (see schemaQualifiedTableUnnest).
	// RFC-142.
	if j.atAlias != "" {
		return true
	}
	// A dotted source is an unnest ONLY when segment 0 is a visible in-scope
	// FROM-source alias (a real correlated source). Otherwise it is a
	// (schema-qualified or unknown) table — the table path.
	if len(j.segments) < 2 {
		return false
	}
	if !isVisibleFromAlias(j.segments[0], visible) {
		return false
	}
	// Table-first (Java generateAccess): even though segment 0 names a visible
	// alias, if the WHOLE dotted name resolves to a real schema-qualified table
	// or CTE, the table branch wins — it is a cross join, not a correlated
	// unnest. (`FROM PA AS s, s.PB`: `s` is a visible alias AND the schema name,
	// but `s.PB` is the real table PB — a table, not an unnest of PA.) RFC-142.
	if resolvesToTable != nil && resolvesToTable(j.segments) {
		return false
	}
	return true
}

// rejectDuplicateUnnestAliasesInFrom rejects a lateral unnest AS/AT binding
// that collides with any other FROM source in the same query block, including
// a source appearing later in the comma list. The logical-tree backstop cannot
// inspect a subquery plan that construction discarded after its scope failed;
// this query-block check runs while the complete parsed FROM list is still in
// hand and therefore preserves DuplicateAlias precedence inside EXISTS too.
func rejectDuplicateUnnestAliasesInFrom(primaryTable, primaryAlias string, joins []joinClause, resolvesToTable tableResolver) error {
	type binding struct {
		name   string
		owner  int
		unnest bool
	}
	bindings := make([]binding, 0, 1+2*len(joins))
	primary := primaryAlias
	if primary == "" {
		primary = primaryTable
	}
	if primary != "" {
		bindings = append(bindings, binding{name: primary, owner: -1})
	}
	for i, j := range joins {
		visible := visibleFromAliases(primaryTable, primaryAlias, joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			asAlias, atAlias := unnestAliases(j)
			if asAlias != "" && atAlias != "" && strings.EqualFold(asAlias, atAlias) {
				return api.NewError(api.ErrCodeDuplicateAlias,
					"lateral unnest AS and AT aliases must be distinct")
			}
			for _, name := range []string{asAlias, atAlias} {
				if name != "" {
					bindings = append(bindings, binding{name: name, owner: i, unnest: true})
				}
			}
			continue
		}
		name := j.alias
		if name == "" {
			name = j.tableName
		}
		if name != "" {
			bindings = append(bindings, binding{name: name, owner: i})
		}
	}
	for i := range bindings {
		for j := i + 1; j < len(bindings); j++ {
			if bindings[i].owner == bindings[j].owner ||
				(!bindings[i].unnest && !bindings[j].unnest) ||
				!strings.EqualFold(bindings[i].name, bindings[j].name) {
				continue
			}
			return api.NewError(api.ErrCodeDuplicateAlias,
				"lateral unnest alias collides with another FROM-source alias; use a distinct AS/AT alias")
		}
	}
	return nil
}

// rememberSchemaAliasTableQualifiers records the table-first collision before
// normalizeSchemaQualifiedSelectSources removes the schema segment. Only a
// source alias that actually serves as the schema qualifier of a later real
// table is marked; ordinary `FROM PA AS x` keeps the alias-hides-table rule.
func rememberSchemaAliasTableQualifiers(sq *selectQuery, resolvesToTable tableResolver) {
	if sq == nil || resolvesToTable == nil {
		return
	}
	visible := visibleFromAliases(sq.tableName, sq.tableAlias, nil, resolvesToTable)
	for i, j := range sq.joins {
		if len(j.segments) == 2 && resolvesToTable(j.segments) {
			if _, collision := visible[strings.ToUpper(j.segments[0])]; collision {
				if sq.tableQualifierAliases == nil {
					sq.tableQualifierAliases = make(map[string]bool)
				}
				sq.tableQualifierAliases[strings.ToUpper(j.segments[0])] = true
			}
		}
		visible = visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i+1], resolvesToTable)
	}
}

// schemaQualifiedTableUnnest reports whether a comma source is a SCHEMA-QUALIFIED
// table (segments `[schema, table]` where the resolver confirms the dotted name
// is a real table/CTE). Used by the WHERE/projection scope binding to decline
// registering a virtual unnest scope source for such a source — it is a table,
// so the scope must resolve its columns as a table cross join, not an unnest.
// (Without this, an AT-on-a-table source like `FROM PA, s.PB AT a`, which
// unnestCandidateShape keeps as a LogicalUnnest so the AT survives to the
// WRONG_OBJECT_TYPE rejection, would also be mis-registered as an unnest source
// and shadow the real table resolution.) RFC-142.
func schemaQualifiedTableUnnest(j joinClause, resolvesToTable tableResolver) bool {
	return resolvesToTable != nil && len(j.segments) == 2 && resolvesToTable(j.segments)
}

// isVisibleFromAlias reports whether `seg` (case-insensitive) is a visible
// in-scope FROM-source alias. RFC-142.
func isVisibleFromAlias(seg string, visible map[string]struct{}) bool {
	_, ok := visible[strings.ToUpper(seg)]
	return ok
}

// unnestAliases extracts the EXPLICIT-or-defaulted AS alias and the AT (ordinal)
// alias of a lateral-unnest comma source. The parser defaults `j.alias` to the
// flattened segment name (`T1.ARR1`) when no `AS` was written, so an AS alias is
// "explicit" only when it differs from the joined segments.
//
// When no AS alias was written, the element's binding alias DEFAULTS to the LAST
// segment — the array field name (`ARR` for `t.ARR`) — mirroring Java's
// `QueryVisitor.visitAtomTableItem`, which defaults `tableAlias` to
// `visitTableName(...)` (the source name) when the `alias` token is absent. This
// guarantees the element is always referenceable by a non-empty name and that
// `unnestSourceCorrelation` never yields the zero CorrelationIdentifier (which
// would panic in NewQuantifiedObjectValue when planning `FROM t, t.arr` with
// neither AS nor AT).
//
// This is the SINGLE source of truth for the (AS, AT) pair so the logical
// lowering (lateralUnnestCandidate) and the WHERE/projection scope binding
// (unnestScopeSourceAdder) agree on the unnest's correlation name — they MUST,
// or a WHERE-on-element/ordinal predicate resolves to a correlation the inner
// Explode quantifier is not bound under and silently fails to push into the
// inner filter. RFC-142.
func unnestAliases(j joinClause) (asAlias, atAlias string) {
	if j.alias != "" && j.alias != strings.Join(j.segments, ".") {
		asAlias = j.alias
	} else if len(j.segments) > 0 {
		// No explicit AS: default the element binding to the last segment (the
		// array field name), Java's table-name-as-alias fallback.
		asAlias = j.segments[len(j.segments)-1]
	}
	return asAlias, j.atAlias
}

// uidSegments returns the quote-stripped uid segments of a table name's
// FullId. `"OUTER"."ARR"` → ["OUTER","ARR"]. Preserved un-flattened on the
// carried FROM source so the translator can resolve a comma source
// segment-by-segment against the scope without re-splitting a joined string
// — no text-heuristic re-split. RFC-142.
func uidSegments(tableName antlrgen.ITableNameContext) []string {
	uids := tableName.FullId().AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = functions.StripIdentifierQuotes(u.GetText())
	}
	return parts
}

// assignFromLegBindingIDs mints the per-leg binding correlation ids for
// duplicate FROM-source aliases.
// The FIRST leg under an alias binds the alias itself; each LATER duplicate
// mints the deterministic position-keyed `Q$DUPN` (N = the leg's 1-based
// position in the joins slice — the primary source is position 0 and, being
// first, never a duplicate). Aliases fold upper-case, matching the FROM-walk
// dup check's seen map. Comma sources that later classify as lateral unnests
// participate like any other leg: a duplicate unnest AS/AT alias is REJECTED
// outright (RFC-142), so a minted id for one is inert.
//
// A mint candidate is collision-checked against the FULL leg-key namespace:
// a QUOTED user alias can spell a mint-shaped name (`AS "Q$DUP1"` — the
// lexer admits `$` in quoted identifiers), and an unchecked mint would make
// two legs' correlations indistinguishable, the exact class the mint
// dissolves. Java's quantifier ids are unforgeable from SQL; the
// deterministic mint bumps with `$` suffixes instead (still a pure function
// of the query's leg keys — no randomness, no counter).
//
// This is the SINGLE mint authority; consumers read the minted id and never
// re-derive binding identity from the SQL alias (a re-derivation would
// re-collide the legs — the design ruling's carried-not-rederived
// condition). In this commit the LOGICAL builders carry it
// (LogicalScan/LogicalCTE/LogicalUnnest.Binding → sourceBinding); commit 2
// wires the semantic-scope builders to read it too.
func assignFromLegBindingIDs(fs *fromSource) {
	legKey := func(alias, table string) string {
		if alias != "" {
			return strings.ToUpper(alias)
		}
		return strings.ToUpper(table)
	}
	// The full alias namespace, pre-collected: mints must dodge LATER legs'
	// (possibly forged) aliases too, not just the prefix walked so far.
	taken := map[string]struct{}{legKey(fs.tableAlias, fs.tableName): {}}
	for i := range fs.joins {
		taken[legKey(fs.joins[i].alias, fs.joins[i].tableName)] = struct{}{}
	}
	seen := map[string]struct{}{legKey(fs.tableAlias, fs.tableName): {}}
	for i := range fs.joins {
		j := &fs.joins[i]
		k := legKey(j.alias, j.tableName)
		if _, dup := seen[k]; dup {
			// UPPER-case form on purpose: correlation keys are UPPER-folded
			// at several bake/lookup sites (bakeGatedJoinPredicates et al.),
			// and a fold-stable id behaves exactly like an alias under every
			// existing fold — no case sweep needed.
			id := fmt.Sprintf("Q$DUP%d", i+1)
			for {
				if _, forged := taken[id]; !forged {
					break
				}
				id += "$"
			}
			j.bindingID = id
			taken[id] = struct{}{}
			continue
		}
		seen[k] = struct{}{}
	}
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
			// A comma source may be a LATERAL ARRAY UNNEST (`FROM t, t.arr AS x
			// [AT ord]`) or a plain table cross-join. The parser carries the
			// un-flattened uid segments + the AT alias through; the translator
			// classifies (segment 0 = an in-scope source alias whose record type
			// has an array field named by the remaining segments → unnest, else
			// table). AT is NOT rejected here — the translator binds it for the
			// unnest case and rejects it (WRONG_OBJECT_TYPE) for the table case.
			// RFC-142.
			parts := uidSegments(item.TableName())
			tblName := strings.Join(parts, ".")
			alias := tblName
			// Use GetAlias() so implicit aliases (`FROM a, b alias`) parse.
			if item.GetAlias() != nil {
				alias = functions.StripIdentifierQuotes(item.GetAlias().GetText())
			}
			extraCrossJoins = append(extraCrossJoins, joinClause{
				tableName: tblName,
				joinType:  joinTypeInner,
				alias:     alias,
				onExpr:    nil,
				segments:  parts,
				atAlias:   atAliasOf(item),
				fromComma: true,
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
				joinType:     joinTypeInner,
				alias:        alias,
				onExpr:       nil,
				derivedQuery: item.Query(),
				fromComma:    true,
			})
		case *antlrgen.InlineTableItemContext:
			alias := inlineValuesCarrierAlias(item, len(extraCrossJoins)+1)
			extraCrossJoins = append(extraCrossJoins, joinClause{
				tableName:    alias,
				joinType:     joinTypeInner,
				alias:        alias,
				inlineValues: item,
				fromComma:    true,
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
		// A derived-table primary source can still be followed by explicit JOIN
		// clauses (`FROM (SELECT ...) x LEFT JOIN t ON ...`). Parse them here so
		// they are not silently dropped (no LEFT/RIGHT alias-promotion applies to
		// a derived primary).
		joins, jErr := parseJoinClauses(srcBase, alias, extraCrossJoins)
		if jErr != nil {
			return nil, jErr
		}
		fs := &fromSource{
			tableName:      alias,
			tableAlias:     alias,
			sourceSegments: []string{alias},
			joins:          joins,
			whereExpr:      fromClause.WhereExpr(),
			derivedQuery:   subItem.Query(),
		}
		assignFromLegBindingIDs(fs)
		return fs, nil
	}

	if inlineItem, isInline := srcBase.TableSourceItem().(*antlrgen.InlineTableItemContext); isInline {
		alias := inlineValuesCarrierAlias(inlineItem, 0)
		joins, jErr := parseJoinClauses(srcBase, alias, extraCrossJoins)
		if jErr != nil {
			return nil, jErr
		}
		fs := &fromSource{
			tableName:      alias,
			tableAlias:     alias,
			sourceSegments: []string{alias},
			inlineValues:   inlineItem,
			joins:          joins,
			whereExpr:      fromClause.WhereExpr(),
		}
		assignFromLegBindingIDs(fs)
		return fs, nil
	}

	atomItem, ok := srcBase.TableSourceItem().(*antlrgen.AtomTableItemContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported table source item %T; only plain table names are supported",
			srcBase.TableSourceItem())
	}
	// The PRIMARY FROM source can never be a lateral array unnest (no prior
	// scope to correlate to), so AT here is always invalid (WRONG_OBJECT_TYPE).
	if err := rejectAtOrdinality(atomItem); err != nil {
		return nil, err
	}
	// Build table name from uid segments, stripping identifier quotes.
	// "INFORMATION_SCHEMA"."TABLES" → INFORMATION_SCHEMA.TABLES
	parts := uidSegments(atomItem.TableName())
	// Grammar is `tableName (AS? alias=uid)?` — AS optional, so an implicit
	// alias (`FROM Order o`) is read from GetAlias() rather than gated on AS.
	//
	// There used to be a repair here for a grammar ambiguity: LEFT and RIGHT
	// were alias-eligible, so `FROM a LEFT JOIN b` parsed as `FROM a AS LEFT
	// JOIN b` and the first InnerJoin had to be PROMOTED back to an outer join.
	// It was removed once the ambiguity was fixed at the source — the census
	//
	//	awk '/^keywordsCanBeId/,/^    ;/' RelationalParser.g4 |
	//	  grep -cowE 'LEFT|RIGHT|FULL'
	//
	// reports 0, so the parser cannot hand this code an alias spelling any of
	// them and the promotion could never fire. Repairing a misparse downstream
	// is the wrong layer anyway: it can only recover the FIRST join, and it
	// cannot tell `FROM a LEFT JOIN b` from a table genuinely aliased LEFT.
	//
	// If a clause keyword is ever readmitted to keywordsCanBeId, the fix is
	// there and not here. TestFDB_ClauseKeywordsAreNotSwallowedAsAliases holds
	// that line by ASSERTING each join type still parses as its CLAUSE, with
	// and without OUTER, so a readmission reddens rather than silently changing
	// answers. (It does not execute the census above — that is prose in the
	// test's header, and re-running it is a reader's job, not the test's.)
	leftAlias := ""
	if atomItem.GetAlias() != nil {
		leftAlias = functions.StripIdentifierQuotes(atomItem.GetAlias().GetText())
	}
	if leftAlias == "" {
		leftAlias = strings.Join(parts, ".")
	}

	joins, jErr := parseJoinClauses(srcBase, leftAlias, extraCrossJoins)
	if jErr != nil {
		return nil, jErr
	}

	fs := &fromSource{
		tableName:      strings.Join(parts, "."),
		tableAlias:     leftAlias,
		sourceSegments: parts,
		joins:          joins,
		whereExpr:      fromClause.WhereExpr(),
	}
	assignFromLegBindingIDs(fs)
	return fs, nil
}

// parseJoinClauses parses every explicit JOIN part of a FROM clause into a
// joinClause slice, synthesizes ON predicates for USING-syntax joins (the
// left USING column qualified by its preceding source alias — leftAlias for
// the first join, the prior join's right alias otherwise), and appends the
// implicit comma-FROM cross joins last. It is
// shared by the atom-table primary path AND the derived-table primary path so a
// `FROM (SELECT ...) x JOIN t ON ...` does not silently drop its JOINs.
func parseJoinClauses(srcBase *antlrgen.TableSourceBaseContext, leftAlias string, extraCrossJoins []joinClause) ([]joinClause, error) {
	var joins []joinClause
	for _, jp := range srcBase.AllJoinPart() {
		jc, jErr := extractJoinClause(jp)
		if jErr != nil {
			return nil, jErr
		}
		joins = append(joins, jc)
	}
	// Synthesize ON predicates for any `USING (col, ...)` joins now that the
	// left source alias chain is known.
	for i := range joins {
		if joins[i].usingUids == nil {
			continue
		}
		leftSrcAlias := leftAlias
		if i > 0 {
			leftSrcAlias = joins[i-1].alias
		}
		synth, sErr := synthesizeUsingOnExpr(joins[i].usingUids, leftSrcAlias, joins[i].alias)
		if sErr != nil {
			return nil, sErr
		}
		// Retain the USING names before clearing: this leg's right side
		// hides them (star expansion + unqualified resolution).
		for _, u := range joins[i].usingUids.AllUid() {
			joins[i].usingHiddenCols = append(joins[i].usingHiddenCols,
				strings.ToUpper(functions.StripIdentifierQuotes(u.GetText())))
			joins[i].usingColTexts = append(joins[i].usingColTexts, u.GetText())
		}
		joins[i].onExpr = synth
		joins[i].usingUids = nil
	}
	// Implicit cross joins from comma-separated FROM sources run last; the
	// WHERE predicate decides which combinations survive.
	joins = append(joins, extraCrossJoins...)
	return joins, nil
}

// synthesizeUsingOnExpr lowers a `USING (col, col, ...)` join clause to the
// equivalent ON expression and parses it back into an IExpressionContext, so
// every downstream ON consumer (map-eval, cascades predicate resolution,
// explain) treats USING exactly like ON. Mirrors Java's
// QueryVisitor.resolveJoinUsingClause: for each column an equality
// `<leftAlias>.col = <rightAlias>.col` is built and the equalities are
// AND-conjoined. The left column is qualified by the preceding source so it
// resolves on the left side (Java resolves it against leftOperators — left
// here is unambiguous, whereas an unqualified col is ambiguous when both
// tables share the name, e.g. `id`); the right column is pinned to the joined
// source. The raw uid text is spliced verbatim so quoting/case-folding match
// how an explicit ON would resolve the same identifiers. (Go does not
// implement USING's right-column hiding for SELECT * — the join columns appear
// on both sides; the equality semantics, which is what USING is for, are
// faithful.)
func synthesizeUsingOnExpr(uidList antlrgen.IUidListContext, leftAlias, rightAlias string) (antlrgen.IExpressionContext, error) {
	uids := uidList.AllUid()
	if len(uids) == 0 {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError, "JOIN ... USING requires at least one column")
	}
	// The alias values are NORMALIZED (StripIdentifierQuotes: unquoted folded
	// UPPER, quoted verbatim with quotes removed). Splicing one back into SQL
	// text bare would re-normalize it — a quoted-DDL alias `"e"` (stored `e`)
	// would fold to `E` and resolve nothing (join-tests-outer.yamsql's USING
	// rows). Double-quoting the stored value round-trips exactly: `"e"` stays
	// `e`, an unquoted alias's stored `D` stays `D`.
	quoteAlias := func(alias string) string {
		if strings.Contains(alias, ".") {
			// A schema-qualified table name standing in for a missing alias
			// (`JOIN s.t USING (…)`) is a dotted PATH, not one identifier —
			// splice it as before; its segments were already normalized.
			return alias
		}
		return `"` + strings.ReplaceAll(alias, `"`, `""`) + `"`
	}
	terms := make([]string, len(uids))
	for i, u := range uids {
		col := u.GetText()
		terms[i] = fmt.Sprintf("%s.%s = %s.%s", quoteAlias(leftAlias), col, quoteAlias(rightAlias), col)
	}
	onText := strings.Join(terms, " AND ")
	onExpr, err := parser.ParseExpression(onText)
	if err != nil {
		return nil, err
	}
	return onExpr, nil
}

// extractJoinClause parses a single JOIN part (INNER JOIN, LEFT JOIN, etc.) from
// the grammar. INNER, LEFT/RIGHT/FULL OUTER joins are supported (ON or USING).
func extractJoinClause(jp antlrgen.IJoinPartContext) (joinClause, error) {
	switch j := jp.(type) {
	case *antlrgen.InnerJoinContext:
		// A CONDITIONLESS INNER JOIN IS SUPPORTED, and deliberately so.
		//
		// `a JOIN b`, `a INNER JOIN b` and `a CROSS JOIN b` all reach this arm
		// with a null `(ON expression | USING '(' uidList ')')` group, and all
		// three mean the cartesian product. fdb-relational 4.11.1.0 cannot
		// answer any of them — its visitor calls `accept(...)` on the ON-clause
		// expression unconditionally and NPEs on the null — so Go once refused
		// them too, matching the failure.
		//
		// That alignment was the wrong call. The rows Go produces are correct,
		// the syntax is ordinary SQL that every other engine accepts, and
		// nothing here touches the wire: it is a read-side capability Java
		// lacks, which this project allows Go to have. Refusing a query we can
		// answer correctly, in order to reproduce someone else's crash, buys
		// nothing and costs every user who writes CROSS JOIN.
		//
		// The conformance principle still binds where both engines RUN a query.
		// It does not oblige Go to inherit a null-dereference.
		//
		// One consequence worth stating because it is easy to misread later:
		// `a JOIN missing_table` reports the unknown TABLE, not a join error,
		// which is also what Java reports — there is no gate here to preempt
		// source resolution.
		// A derived table (subquery) on the right of an explicit JOIN —
		// `JOIN (SELECT ...) AS x ON ...`. Mirrors the comma-FROM derived path.
		if subItem, isSub := j.TableSourceItem().(*antlrgen.SubqueryTableItemContext); isSub {
			jc, sErr := joinClauseForSubquerySource(subItem, joinTypeInner)
			if sErr != nil {
				return joinClause{}, sErr
			}
			jc.onExpr, jc.usingUids = joinOnOrUsing(j.Expression(), j.USING(), j.UidList())
			return jc, nil
		}
		atomItem, ok := j.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"JOIN: unsupported table source item %T", j.TableSourceItem())
		}
		// A JOIN source is never a lateral array unnest (Java only unnests via
		// the comma FROM list), so AT here is invalid (WRONG_OBJECT_TYPE).
		if err := rejectAtOrdinality(atomItem); err != nil {
			return joinClause{}, err
		}
		parts := uidSegments(atomItem.TableName())
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
		onExpr, usingUids := joinOnOrUsing(j.Expression(), j.USING(), j.UidList())
		return joinClause{tableName: tblName, joinType: joinTypeInner, alias: alias, onExpr: onExpr, usingUids: usingUids, segments: parts}, nil

	case *antlrgen.OuterJoinContext:
		jt := joinTypeLeft
		if j.RIGHT() != nil {
			jt = joinTypeRight
		} else if j.FULL() != nil {
			jt = joinTypeFull
		}
		// A derived table (subquery) on the right of an outer JOIN —
		// `LEFT JOIN (SELECT ...) AS x ON ...`.
		if subItem, isSub := j.TableSourceItem().(*antlrgen.SubqueryTableItemContext); isSub {
			jc, sErr := joinClauseForSubquerySource(subItem, jt)
			if sErr != nil {
				return joinClause{}, sErr
			}
			jc.onExpr, jc.usingUids = joinOnOrUsing(j.Expression(), j.USING(), j.UidList())
			return jc, nil
		}
		atomItem, ok := j.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"JOIN: unsupported table source item %T", j.TableSourceItem())
		}
		// A JOIN source is never a lateral array unnest, so AT is invalid here.
		if err := rejectAtOrdinality(atomItem); err != nil {
			return joinClause{}, err
		}
		parts := uidSegments(atomItem.TableName())
		tblName := strings.Join(parts, ".")
		alias := tblName
		// Same implicit-alias note as InnerJoin.
		if atomItem.GetAlias() != nil {
			alias = functions.StripIdentifierQuotes(atomItem.GetAlias().GetText())
		}
		onExpr, usingUids := joinOnOrUsing(j.Expression(), j.USING(), j.UidList())
		return joinClause{tableName: tblName, joinType: jt, alias: alias, onExpr: onExpr, usingUids: usingUids, segments: parts}, nil

	default:
		return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported JOIN type %T; only INNER JOIN and LEFT/RIGHT/FULL OUTER JOIN are supported", jp)
	}
}

// joinOnOrUsing extracts a JOIN's join condition: an explicit ON expression
// (returned as onExpr) or a `USING (col, ...)` column list (returned as
// usingUids, synthesized into an ON later in extractFromSimpleTable once the
// left source alias is known). At most one is non-nil; a join with neither is
// an unconstrained cross-join (onExpr == nil).
func joinOnOrUsing(onCtx antlrgen.IExpressionContext, using antlr.TerminalNode, uidList antlrgen.IUidListContext) (antlrgen.IExpressionContext, antlrgen.IUidListContext) {
	if onCtx != nil {
		return onCtx, nil
	}
	if using != nil && uidList != nil {
		return nil, uidList
	}
	return nil, nil
}

// joinClauseForSubquerySource builds a joinClause for a derived-table (subquery)
// JOIN source `JOIN (SELECT ...) AS alias`. Mirrors the comma-FROM derived-table
// path (which sets derivedQuery so the dispatcher materializes the subquery as a
// CTE keyed by alias before the join executor runs).
func joinClauseForSubquerySource(subItem *antlrgen.SubqueryTableItemContext, jt joinType) (joinClause, error) {
	alias := ""
	if subItem.GetAlias() != nil {
		alias = functions.StripIdentifierQuotes(subItem.GetAlias().GetText())
	}
	if alias == "" {
		return joinClause{}, api.NewError(api.ErrCodeUnsupportedOperation,
			"derived table in JOIN must have an alias")
	}
	return joinClause{
		tableName:    alias,
		joinType:     jt,
		alias:        alias,
		derivedQuery: subItem.Query(),
		segments:     []string{alias},
	}, nil
}

func containsNestedAggregateInSelectElement(e *antlrgen.SelectExpressionElementContext, argExpr antlrgen.IExpressionContext) bool {
	if argExpr != nil {
		return containsNestedAggregate(argExpr)
	}
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
	if fa := awf.FunctionArgs(); fa != nil {
		for _, arg := range fa.AllFunctionArg() {
			if containsNestedAggregate(arg) {
				return true
			}
		}
	}
	if fa := awf.FunctionArg(); fa != nil {
		return containsNestedAggregate(fa)
	}
	return false
}

func hasNestedAggregateInTree(tree antlr.Tree) bool {
	if tree == nil {
		return false
	}
	if agg, ok := tree.(*antlrgen.AggregateFunctionCallContext); ok {
		for i := 0; i < agg.GetChildCount(); i++ {
			if containsNestedAggregate(agg.GetChild(i)) {
				return true
			}
		}
		return false
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		if hasNestedAggregateInTree(tree.GetChild(i)) {
			return true
		}
	}
	return false
}

func containsNestedAggregate(tree antlr.Tree) bool {
	if tree == nil {
		return false
	}
	if _, ok := tree.(*antlrgen.AggregateFunctionCallContext); ok {
		return true
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		if containsNestedAggregate(tree.GetChild(i)) {
			return true
		}
	}
	return false
}

// usingSource is one candidate left source for a chained USING's column
// lookup: the alias the predicate must qualify with, and the base table whose
// columns decide whether it owns the column at all.
// cteNamePredicate answers "is this FROM source a CTE?" for a scope map, which
// retargetUsingJoins needs because a CTE is referenced by a bare name that no
// structural flag on the join distinguishes from a table. A nil or empty map
// yields nil — correct, since no CTEs are then in scope.
func cteNamePredicate(cteScopes map[string]semantic.ScopeSource) func(string) bool {
	if len(cteScopes) == 0 {
		return nil
	}
	return func(name string) bool {
		_, ok := cteScopes[strings.ToUpper(functions.StripIdentifierQuotes(name))]
		return ok
	}
}

type usingSource struct {
	alias string
	table string
	// base records whether `table` is a real table REFERENCE rather than a
	// stand-in. It cannot be inferred from the name: a derived table and an
	// inline VALUES source both carry their ALIAS in tableName
	// (joinClauseForSubquerySource, inlineValuesCarrierAlias), and a CTE is
	// referenced by a name that is not in the record metadata at all. So an
	// alias that happens to collide with a record type would otherwise resolve
	// ownership against a descriptor belonging to a completely different
	// relation — the wrong owner, silently, with a plausible predicate built on
	// it. Only a source flagged base may be looked up.
	base bool
	// cols answers "does this source export this column?" for a source that is
	// NOT a base table — a derived table or a CTE, whose columns come from its
	// projection rather than from a record descriptor. Nil for base sources,
	// which are answered from the descriptor, and nil when a non-base source's
	// schema could not be derived, which is what makes the scope decline.
	cols semantic.Table
}

// owns reports whether this source exports `col`, from whichever authority
// describes it: a record descriptor for a base table, the derived schema for a
// subquery or CTE. A source with neither — one whose schema could not be
// derived — owns nothing, and the caller's resolvable gate has already declined
// the whole join in that case rather than letting it read as "no owner".
//
// There is no name-based fallback: a source is described by its cols surface or
// it is not described at all. That is what makes a derived table unable to
// resolve against a same-named record type even if the caller-side gate is ever
// loosened — there is no descriptor path left for it to reach.
func (s usingSource) owns(id semantic.Identifier) bool {
	if s.cols == nil {
		// Nothing describes this source — a name that did not resolve, or a
		// derived schema that could not be derived. The caller's resolvable
		// gate has already declined the whole join on that basis, so reaching
		// here at all means the gate was loosened; owning nothing is the safe
		// answer either way.
		return false
	}
	_, ok := s.cols.LookupColumn(id)
	return ok
}

// usingOwnerOf returns the single left source that owns `col`, applying Java's
// hiding rule: a column consumed by an EARLIER USING is hidden on that join's
// RIGHT side, so only the left copy remains visible.
//
// Two owners is AMBIGUOUS and is raised here, because nothing downstream can
// detect it — a predicate built against either candidate plans and answers.
//
// NO owner returns ("", nil) — a DECLINE, not an error. A source that nothing
// describes owns nothing, and this layer cannot tell "the column is absent"
// from "the scope could not be read"; declining leaves the parse-time predicate
// in place so ordinary column resolution reports a genuine typo downstream,
// from the layer that can tell those apart.
//
// `__ROW_VERSION` used to reach this decline and no longer does, which is the
// improvement rather than a regression: ownership now reads the CATALOG, which
// appends the pseudo-column when the store keeps row versions, so it resolves
// like any other column. Measured against the live JVM
// (JoinUsingRowVersionJavaProbe): a single join and a chained USING both answer
// in both engines, and the shape where nothing hides a copy — an ON join
// putting two row-versioned sources in scope before a USING names it — is
// AMBIGUOUS in both. Two owners really is two owners here.
func usingOwnerOf(colText string, sources []usingSource, hidden map[string]map[string]bool) (string, error) {
	id := semantic.NewUnquoted(colText)
	var owners []string
	for _, s := range sources {
		if hidden[s.alias][id.Name()] {
			continue
		}
		if !s.owns(id) {
			continue
		}
		owners = append(owners, s.alias)
	}
	switch len(owners) {
	case 1:
		return owners[0], nil
	case 0:
		return "", nil
	default:
		return "", api.NewErrorf(api.ErrCodeAmbiguousColumn, "Ambiguous reference %s", id.Name())
	}
}

// retargetUsingJoins re-qualifies each `USING (cols)` join's synthesized ON
// predicate against the source that actually OWNS each column, and raises
// Java's errors when no source owns it or more than one does.
//
// WHAT IT REPLACES. The parse-time synthesis qualifies a USING column by
// POSITION — the prior join's right alias — which is right only by luck. Two
// shapes, both measured against fdb-relational 4.12.11.0, show it failing in
// opposite directions:
//
//	a JOIN b USING (id) JOIN c USING (id, k)   java: Ambiguous reference K
//	                                           go  : silently picked b.k
//	a JOIN b USING (id) JOIN c USING (j)       java: answers, j lives on a
//	                                           go  : 42703 column "J" does not exist
//
// The second is the one that matters most: a legitimate query Go refused.
//
// WHY POSITION IS NOT A SUBSTITUTE FOR OWNERSHIP. `USING` names a column, not a
// side. Java resolves it against every visible left operator, having hidden the
// RIGHT copy of each column an earlier USING already consumed — which is what
// makes `USING (id, k) … USING (id, k)` legal (b.k is hidden, so a.k is the
// only candidate) while `USING (id) … USING (id, k)` is ambiguous (nothing hid
// b.k). Position cannot express that: it picks the same operator either way.
//
// IT DECLINES RATHER THAN GUESSES. Column ownership is decided from the record
// metadata, so a derived table, a CTE or any source it cannot resolve makes the
// answer unknowable — and a wrong "no owner" would reject a working query. When
// any in-scope source is unresolvable the correction is skipped and the
// parse-time predicate stands, which is exactly today's behaviour for those
// shapes and never worse.
//
// "UNRESOLVABLE" IS DECIDED STRUCTURALLY, NOT BY NAME LOOKUP. A derived table
// and an inline VALUES source carry their ALIAS in tableName, and a CTE is
// referenced by a name absent from the metadata — so asking "is this name a
// record type?" answers YES for a derived table whose alias collides with a
// real one, and ownership is then read off an unrelated descriptor. Each source
// therefore carries a `base` flag set from what it structurally IS, and only a
// base source is ever looked up.
func retargetUsingJoins(primaryTable, primaryAlias string, primaryIsBase bool,
	primaryDerived antlrgen.IQueryContext, joins []joinClause,
	md *recordlayer.RecordMetaData, schemaName string,
	isCTE func(string) bool, cteScopes map[string]semantic.ScopeSource,
) error {
	primary := primaryAlias
	if primary == "" {
		primary = primaryTable
	}
	// isBase reports whether a join leg is a plain table reference: not a
	// derived table, not an inline VALUES carrier, not a CTE.
	isBase := func(j *joinClause) bool {
		return j.derivedQuery == nil && j.inlineValues == nil &&
			j.tableName != "" && !(isCTE != nil && isCTE(j.tableName))
	}
	// legSource describes one FROM leg for ownership purposes. A base table is
	// answered from its descriptor; a derived table or CTE has its projection
	// derived instead, so it participates in ambiguity rather than silently
	// declining the join — measured against the live JVM, a derived source to
	// the LEFT of a second USING makes the column AMBIGUOUS, so treating it as
	// unknowable answered a query Java refuses.
	legSource := func(alias, table string, derived antlrgen.IQueryContext, base bool) usingSource {
		s := usingSource{alias: alias, table: table, base: base}
		if base {
			s.cols = baseTableColumns(table, md, schemaName)
			return s
		}
		if derived != nil {
			if src, ok, err := buildCTEColumnSource(md, alias, derived, cteScopes); err == nil && ok {
				s.cols = src.Table
			}
			return s
		}
		if cteScopes != nil {
			if src, ok := cteScopes[strings.ToUpper(functions.StripIdentifierQuotes(table))]; ok {
				s.cols = src.Table
			}
		}
		return s
	}
	// hidden[alias][COL] — the right copy an earlier USING consumed.
	hidden := map[string]map[string]bool{}
	sources := []usingSource{
		legSource(primary, primaryTable, primaryDerived, primaryIsBase),
	}

	for i := range joins {
		j := &joins[i]
		if len(j.usingColTexts) == 0 {
			// Not a USING join; it still contributes a source to later ones.
			sources = append(sources, legSource(j.alias, j.tableName, j.derivedQuery, isBase(j)))
			continue
		}
		// Every left source must be resolvable, or ownership is unknowable and
		// the parse-time predicate stands. Checked over the whole scope rather
		// than per column: one unresolvable source can hide the second owner
		// that would have made a column ambiguous.
		// The RIGHT source must be DESCRIBABLE, by whichever authority applies —
		// a base table's catalog entry or a derived/CTE projection. Requiring it
		// to be BASE was an error order bug: a derived right leg then skipped
		// its own missing-column check while still entering the scope for later
		// joins, so `a JOIN (SELECT id FROM c) d USING (k) JOIN b USING (id)`
		// reported the SECOND join's ambiguity when the real, earlier fault is
		// that the subquery has no `k`. Measured against the live JVM, which
		// reports the missing column.
		// Derived ONCE and reused as this join.s scope entry at the end of the
		// loop: for a computed or join-bodied subquery legSource builds the
		// inner logical plan, so calling it twice pays that cost twice and
		// nested shapes compound it.
		rightLeg := legSource(j.alias, j.tableName, j.derivedQuery, isBase(j))
		rightCols := rightLeg.cols
		resolvable := rightCols != nil
		for _, s := range sources {
			// A source is describable either as a base table (descriptor) or as
			// a derived/CTE projection (cols). Neither means the scope cannot be
			// read at all, and one unreadable source can hide the second owner
			// that would have made a column ambiguous — so the whole join
			// declines rather than resolving against a partial view.
			if s.cols != nil {
				continue
			}
			if !s.base || s.table == "" || !sourceResolves(s.table, md, schemaName) {
				resolvable = false
				break
			}
		}
		if resolvable {
			// Owners are resolved for ALL columns before anything is rewritten:
			// one unownable column declines the whole join, so a half-retargeted
			// predicate mixing the two rules can never be built.
			owners := make([]string, 0, len(j.usingColTexts))
			for _, colText := range j.usingColTexts {
				// The name as the catalog knows it: unquoted folds, quoted keeps
				// its case. Used for the messages too, so a quoted "k" is not
				// reported as K.
				col := semantic.NewUnquoted(colText).Name()
				owner, err := usingOwnerOf(colText, sources, hidden)
				if err != nil {
					return err
				}
				if owner == "" {
					// No descriptor field owns it — a pseudo-column such as
					// `__ROW_VERSION`, or a name that does not exist. This layer
					// cannot tell those apart, so it declines and lets the
					// parse-time predicate stand; ordinary column resolution
					// reports the second case downstream.
					owners = nil
					break
				}
				// THE RIGHT SIDE IS CHECKED HERE, AT THIS JOIN, and that is
				// about ERROR ORDER rather than about detection.
				//
				// A USING column missing from the right source is reported
				// downstream either way. But this pre-pass walks every join
				// before the FROM clause is visited, so a LATER join's
				// ambiguity would otherwise be raised first and mask it:
				// `a JOIN b USING (j) JOIN c USING (id)` reported 42702 for ID
				// when the real, earlier fault is that b has no j. Java visits
				// and resolves each right source left-to-right, so a fault in
				// the first join must win.
				//
				// Resolved through the SAME column surface as the owner — the
				// semantic catalog, not the raw descriptor. Mixing the two is
				// how a pseudo-column becomes "missing": the catalog appends
				// `__ROW_VERSION` when the store keeps row versions and a
				// descriptor never carries it, so a left owner found on one
				// surface could be declared absent on the other.
				// COUNTED, not looked up. LookupColumn answers with its FIRST
				// match, so it cannot see a right leg that exports the same
				// name twice — `(SELECT id, id FROM c)` — and a USING naming
				// that column is ambiguous ON THE RIGHT. Measured: Java reports
				// `Ambiguous reference ID`; a first-match check passed `id` and
				// went on to report a LATER column's absence instead, which is
				// both the wrong fault and the wrong column.
				switch n := rightColumnCount(rightCols, colText); {
				case n == 0:
					return api.NewErrorf(api.ErrCodeUndefinedColumn,
						"column %q does not exist", col)
				case n > 1:
					return api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"Ambiguous reference %s", col)
				}
				owners = append(owners, owner)
			}
			if len(owners) == len(j.usingColTexts) {
				terms := make([]string, len(owners))
				for n, colText := range j.usingColTexts {
					terms[n] = fmt.Sprintf("%s.%s = %s.%s",
						quoteUsingAlias(owners[n]), colText,
						quoteUsingAlias(j.alias), colText)
				}
				onExpr, err := parser.ParseExpression(strings.Join(terms, " AND "))
				if err != nil {
					return err
				}
				j.onExpr = onExpr
			}
		}
		// This join's RIGHT copy of each USING column is now hidden, whether or
		// not the predicate was retargeted — the hiding is Java's rule about
		// visibility, not about which predicate Go built.
		//
		// KEYED THE SAME WAY OWNERSHIP IS — by the quote-aware normalized
		// identifier, from the column text as WRITTEN. `usingHiddenCols` is
		// upper-folded for its own consumers (star expansion, unqualified
		// resolution), and reusing it here would collapse a quoted `"k"` and an
		// unquoted `K` into one key: a first `USING("k")` would then hide `K`
		// from the second join. That is the same case-folding defect the
		// ownership lookup was just repaired for, surviving in the sibling map.
		for _, colText := range j.usingColTexts {
			if hidden[j.alias] == nil {
				hidden[j.alias] = map[string]bool{}
			}
			hidden[j.alias][semantic.NewUnquoted(colText).Name()] = true
		}
		sources = append(sources, rightLeg)
	}
	return nil
}

// sourceResolves reports whether a name refers to a base record type.
func sourceResolves(table string, md *recordlayer.RecordMetaData, schemaName string) bool {
	if md == nil {
		return false
	}
	resolved, err := functions.ResolveQualifiedTableName(table, schemaName)
	if err != nil {
		return false
	}
	return recordTypeCI(md, resolved) != nil
}

// quoteUsingAlias mirrors synthesizeUsingOnExpr's aliasing rule: a stored alias
// spliced back into SQL text bare would RE-normalize, folding a quoted-DDL
// alias `"e"` to `E` and resolving nothing. Double-quoting round-trips it.
func quoteUsingAlias(alias string) string {
	if strings.Contains(alias, ".") {
		// A schema-qualified table name standing in for a missing alias is a
		// dotted PATH, not one identifier; its segments are already normalized.
		return alias
	}
	return `"` + strings.ReplaceAll(alias, `"`, `""`) + `"`
}

// baseTableColumns resolves a base source's column surface through the SEMANTIC
// CATALOG rather than straight off the record descriptor, so both sides of a
// USING see the same set of columns.
//
// The difference is not cosmetic. The catalog appends `__ROW_VERSION` as an
// ephemeral pseudo-column when the store keeps row versions (rlcatalog, mirroring
// Java's Type.Record.addPseudoFields) and a descriptor never carries it. Reading
// the owner from one surface and checking the right-hand side against the other
// would let a column be found as an owner and then declared missing on the right
// — 42703 on a query that is perfectly well-formed.
//
// Returns nil when the name does not resolve, which the caller reads as "this
// scope cannot be described" and declines on, rather than as "owns nothing".
func baseTableColumns(table string, md *recordlayer.RecordMetaData, schemaName string) semantic.Table {
	if md == nil || table == "" {
		return nil
	}
	resolved, err := functions.ResolveQualifiedTableName(table, schemaName)
	if err != nil {
		return nil
	}
	tbl, ok := rlcatalog.Wrap(md).LookupTable(
		semantic.FromSegments(strings.Split(resolved, "."), false))
	if !ok {
		return nil
	}
	return tbl
}

// rightColumnCount reports how many columns a source exports under `colText`,
// quote-aware.
//
// It counts rather than looking up because a lookup returns its FIRST match,
// which cannot distinguish "exports it once" from "exports it twice" — and a
// USING column the right side exports twice is AMBIGUOUS there, which Java
// reports in preference to any later column's absence.
func rightColumnCount(cols semantic.Table, colText string) int {
	if cols == nil {
		return 0
	}
	want := semantic.NewUnquoted(colText)
	n := 0
	for _, c := range cols.Columns() {
		if c.Id.EqualsIgnoreQuoting(want) {
			n++
		}
	}
	// A source whose Columns() view is empty but which resolves the name — a
	// catalog pseudo-column is registered in the index rather than the ordered
	// list on some paths — still owns it once.
	if n == 0 {
		if _, ok := cols.LookupColumn(want); ok {
			return 1
		}
	}
	return n
}
