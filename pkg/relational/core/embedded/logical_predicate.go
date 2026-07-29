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
// Plumbed into the connection's Explain path via
// EmbeddedConnection.cachedMetaData() — when the session schema cache
// already holds the active schema, Explain upgrades to predicate-tree
// rendering; cold cache stays on the text-builder path so EXPLAIN
// remains deterministic and IO-free.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	recordlayer "fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CorrelatedExistsError is returned when buildCorrelatedExists fails.
// Detected via errors.As at the caller to propagate as
// ErrCodeUndefinedColumn for fallback to a richer outer scope.
//
// Unsupported distinguishes a DELIBERATE decline of an unsupported correlated-
// EXISTS shape (an intentional CORRECT-or-CONSERVATIVE rejection — surfaced as
// 0A000 unsupported-operation) from a resolution failure that should read as an
// undefined column (42703). The WHERE-EXISTS and projected paths both key on
// this so a decline reports the same 0A000 in either position.
type CorrelatedExistsError struct {
	Message     string
	Cause       error
	Unsupported bool
}

func (e *CorrelatedExistsError) Error() string { return e.Message }
func (e *CorrelatedExistsError) Unwrap() error { return e.Cause }

// wrapCorrelatedExistsWalkErr wraps a predicate-walk failure in a
// CorrelatedExistsError, PROPAGATING the Unsupported flag when the wrapped error
// is itself an Unsupported decline (e.g. a NESTED correlated EXISTS whose JOIN ON
// hit the RIGHT/FULL / nested-subquery decline). Without this, the outer wrapper
// defaults Unsupported=false, so mapPredicateWalkError matches it first and
// reports 42703 (undefined-column) instead of the intended 0A000
// (unsupported-operation) for the deliberate decline.
func wrapCorrelatedExistsWalkErr(msg string, err error) *CorrelatedExistsError {
	unsupported := false
	var inner *CorrelatedExistsError
	if errors.As(err, &inner) {
		unsupported = inner.Unsupported
	}
	return &CorrelatedExistsError{Message: msg, Cause: err, Unsupported: unsupported}
}

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
	pred, ok, _ := buildWherePredicateForTableE(md, tableName, tableAlias, whereExpr)
	return pred, ok
}

// buildWherePredicateForTableE is buildWherePredicateForTable that also carries
// a structured *api.Error from the predicate walk (e.g. DATATYPE_MISMATCH from a
// bare non-boolean WHERE, RFC-146). The DML (DELETE/UPDATE) paths use it so
// `DELETE FROM t WHERE <non-boolean>` surfaces 42804 — the same SQLSTATE the
// SELECT/ON paths give — instead of swallowing it into a generic DML translation
// error. A non-api walk failure (an unhandled shape that should soft-fall-back
// to the text builder) returns a nil error, preserving existing behaviour.
func buildWherePredicateForTableE(
	md *recordlayer.RecordMetaData,
	tableName, tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool, error) {
	if md == nil || tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false, nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	// Split on '.' so a schema-qualified table name like "schema.t"
	// reaches FromSegments as ["schema", "t"] rather than as a single
	// dotted segment that would never resolve in the catalog.
	tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
	if err != nil {
		return nil, false, nil
	}
	alias := semantic.FromNormalized(tableAlias)
	if tableAlias == "" {
		alias = semantic.FromNormalized(tableName)
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(semantic.ScopeSource{
		Table:           tbl,
		Alias:           alias,
		CorrelationName: alias.Name(),
	}); err != nil {
		return nil, false, nil
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		// Classify with the shared mapper (the same the SELECT WHERE / JOIN-ON paths
		// use) so a bare semantic failure — undefined column (ColumnNotFoundError →
		// 42703), ambiguous column, bad source — surfaces the specific SQLSTATE the
		// SELECT path gives, plus structured *api.Error (e.g. 42804 DATATYPE_MISMATCH).
		// The DML caller returns it instead of swallowing it into a generic 0AF00 "DML
		// Cascades translation failed". An unclassifiable shape failure (mapped == nil)
		// still soft-falls-back, preserving existing behaviour.
		if mapped := mapPredicateWalkError(err); mapped != nil {
			return nil, false, mapped
		}
		return nil, false, nil
	}
	// Plan-time fold of constant Value sub-trees inside the predicate
	// (`name = 1+2` → `name = 3`). Best-effort — SimplifyPredicateValues
	// is pointer-stable when nothing folds.
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true, nil
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
		aliasID := semantic.FromNormalized(alias)
		// Apply inner projection aliases if present.
		cols := innerSrc.Table.Columns()
		if innerSQ.projCols != nil {
			cols = make([]semantic.Column, 0, len(innerSQ.projCols))
			for i, col := range innerSQ.projCols {
				name := col.name
				if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
					name = innerSQ.projAliases[i]
				}
				cols = append(cols, semantic.Column{
					Id:       semantic.FromNormalized(name),
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
		projCols = make([]projCol, len(allCols))
		for i, c := range allCols {
			projCols[i] = projCol{name: c.Id.Name(), bare: c.Id.Name()}
		}
	}
	columns := make([]semantic.Column, 0, len(projCols))
	for i, col := range projCols {
		// Structured segments; a rebased/computed name is one opaque label.
		bareName := col.bare
		if bareName == "" {
			bareName = col.name
		}
		innerCol, found := innerTbl.LookupColumn(semantic.FromNormalized(bareName))
		if !found {
			return semantic.ScopeSource{}, false
		}
		outName := bareName
		if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
			outName = innerSQ.projAliases[i]
		}
		// The virtual column carries the OUTPUT name the derived-table
		// projection emits (Java resolves references to the output column
		// verbatim — no reverse-map to the underlying source column).
		columns = append(columns, semantic.Column{
			Id:       semantic.FromNormalized(outName),
			Type:     innerCol.Type,
			Nullable: innerCol.Nullable,
		})
	}

	aliasID := semantic.FromNormalized(alias)
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

// aggOutputCol is one VISIBLE output column of an aggregate SELECT body.
type aggOutputCol struct {
	name     string
	typ      string
	nullable bool
}

// aggOutputCols returns the aggregate body's VISIBLE output columns in install
// order (the SELECT-list COUNT(*) first, then the aggregate/group columns) — the
// SINGLE authority both buildDerivedTableSourceFromAgg (to build the schema) and
// the ON-only complete-or-decline gate (to dedup) consume, so the derivation is
// never twice-written. HIDDEN aggregates (a HAVING/ORDER-BY COUNT(*) harvested
// into aggCols with visible=false) are NOT output columns — they must not be
// advertised in the schema nor counted by the dup gate (else e.g.
// `SELECT COUNT(*) … HAVING COUNT(*) > 0` false-collides its lone output).
// countStar is set only for a SELECT-list COUNT(*), so it is always visible.
func aggOutputCols(sq *selectQuery) []aggOutputCol {
	var out []aggOutputCol
	if sq.countStar {
		name := "COUNT(*)"
		if sq.countStarAlias != "" {
			name = sq.countStarAlias
		}
		out = append(out, aggOutputCol{name: name, typ: "BIGINT", nullable: false})
	}
	for _, ac := range sq.aggCols {
		if !ac.visible {
			continue
		}
		name := ac.outName
		if name == "" {
			if ac.groupCol != "" {
				name = ac.groupCol
			} else {
				continue
			}
		}
		out = append(out, aggOutputCol{name: name, typ: "UNKNOWN", nullable: true})
	}
	return out
}

func buildDerivedTableSourceFromAgg(alias string, sq *selectQuery) (semantic.ScopeSource, bool) {
	cols := aggOutputCols(sq)
	if len(cols) == 0 {
		return semantic.ScopeSource{}, false
	}
	columns := make([]semantic.Column, len(cols))
	for i, c := range cols {
		columns[i] = semantic.Column{Id: semantic.FromNormalized(c.name), Type: c.typ, Nullable: c.nullable}
	}
	aliasID := semantic.FromNormalized(alias)
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

// mapPredicateWalkError converts a resolver.WalkPredicate failure into the
// SQLSTATE-classified *api.Error it should surface as, or nil when the error is
// not one of the recognized semantic / IN-shape errors (the caller then decides
// whether to fall back to a text predicate or fail closed). Shared by the
// WHERE-clause and JOIN-ON resolution paths so both classify column, ambiguity,
// source, and IN-shape failures identically — and a structured *api.Error from a
// nested subquery build surfaces verbatim.
//
// A bare ColumnNotFoundError maps to ErrCodeUndefinedColumn so a WHERE-clause
// correlated subquery's BuildExists can fall back to buildCorrelatedExists with
// its richer outer scope (RFC-141/RFC-142); in the JOIN-ON path the same mapping
// is simply the correct 42703 for an ON column that does not exist.
func mapPredicateWalkError(walkErr error) *api.Error {
	var ambigErr *semantic.AmbiguousColumnError
	if errors.As(walkErr, &ambigErr) {
		// Java's exact SemanticAnalyzer text, from the reference as written.
		return api.NewErrorf(api.ErrCodeAmbiguousColumn, "Ambiguous reference %s", ambigErr.Reference())
	}
	var inListNull *expr.InListNullError
	if errors.As(walkErr, &inListNull) {
		return api.NewError(api.ErrCodeWrongObjectType, "NULL values are not allowed in the IN list")
	}
	var srcNotFound *semantic.SourceNotFoundError
	if errors.As(walkErr, &srcNotFound) {
		return api.NewErrorf(api.ErrCodeUndefinedColumn, "no FROM source aliased as %s", srcNotFound.Alias.Name())
	}
	var colNotFound *semantic.ColumnNotFoundError
	if errors.As(walkErr, &colNotFound) {
		return api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q does not exist", colNotFound.Id.Name())
	}
	var shadowErr *semantic.CorrelatedShadowError
	if errors.As(walkErr, &shadowErr) {
		// A correlated reference shadowed by a same-named FROM source that lacks the
		// column is a RESOLUTION failure (undefined column in the bound scope) →
		// 42703, recognized by type BEFORE the CorrelatedExistsError fallback.
		return api.NewError(api.ErrCodeUndefinedColumn, shadowErr.Error())
	}
	var inColRef *expr.InColumnRefError
	if errors.As(walkErr, &inColRef) {
		return api.NewError(api.ErrCodeUnsupportedOperation, inColRef.Error())
	}
	var binErr *expr.InvalidBinaryLiteralError
	if errors.As(walkErr, &binErr) {
		return api.NewError(api.ErrCodeInvalidBinaryRepresentation, binErr.Error())
	}
	// A nested planner may deliberately carry the more specific
	// UnsupportedQuery (0AF00) classification through a correlation wrapper.
	// Preserve it before the generic CorrelatedExistsError fallback maps
	// message-only unsupported shapes to UnsupportedOperation (0A000).
	var carriedAPI *api.Error
	if errors.As(walkErr, &carriedAPI) && carriedAPI.Code == api.ErrCodeUnsupportedQuery {
		return carriedAPI
	}
	var corrExistsErr *CorrelatedExistsError
	if errors.As(walkErr, &corrExistsErr) {
		// Every GENUINE semantic resolution error (Ambiguous / ColumnNotFound /
		// SourceNotFound / …) is mapped to 42703/42702 ABOVE via the cause chain.
		// A CorrelatedExistsError reaching HERE therefore wraps NO recognized
		// resolution error — it is a deliberate unsupported-shape decline (a
		// Message-only rejection, an Unsupported=true decline, or one wrapping a
		// NON-semantic unsupported cause like COUNT(DISTINCT …)) → 0A000. Classifying
		// by the recognized-cause TYPE (not the Unsupported flag or a Cause==nil
		// heuristic) is what keeps every path's SQLSTATE consistent.
		return api.NewError(api.ErrCodeUnsupportedOperation, corrExistsErr.Error())
	}
	var apiErr *api.Error
	if errors.As(walkErr, &apiErr) {
		return apiErr
	}
	return nil
}

// bindingOrAlias resolves a FROM leg's binding correlation name: the
// parser-minted duplicate-leg id when present, else the alias. The single
// mint authority (assignFromLegBindingIDs)
// sets bindingID only on LATER duplicate legs; every non-duplicate leg keeps
// its alias as the correlation, so the resolver emits QOV(binding) addressing
// the leg's own quantifier — never the colliding alias namespace. Every scope
// builder reads THIS one helper so no site re-derives the fallback.
func bindingOrAlias(bindingID string, aliasID semantic.Identifier) string {
	if bindingID != "" {
		return bindingID
	}
	return aliasID.Name()
}

// upgradeJoinOnPredicates walks the logical plan tree to find LogicalJoin
// nodes and upgrades their OnText to OnPredicate using the full join scope.
// The join nodes are created in order matching sq.joins, so we match
// them sequentially by walking the left-child spine (the builder chains
// joins left-to-right with op = NewJoin(op, right, ...)).
func upgradeJoinOnPredicates(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource) error {
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)

	// isDeclaredCTE: the name IS a WITH-declared CTE, even when its
	// column-schema derivation declined and cteScopes has no entry (every
	// declared CTE not in cteScopes gets a cteOnScopes entry at WITH
	// registration — a derived source or a nil-Table marker). The distinction
	// is load-bearing for the drop-risk taxonomy below: an unresolvable REAL
	// table errors precisely downstream, but a declared CTE resolves fine at
	// translation — nothing downstream errors, so a silent scope decline here
	// silently DROPS the join's ON and the query returns cross-product rows.
	isDeclaredCTE := func(tableName string) bool {
		key := strings.ToUpper(tableName)
		if _, ok := cteOnScopes[key]; ok {
			return true
		}
		_, ok := cteScopes[key]
		return ok
	}

	resolveTable := func(tableName string) semantic.Table {
		// CTE-FIRST (execution's shadowing order — the same ordering
		// cteLegKind and buildSelectScope apply): a declared CTE shadows a
		// same-named catalog table; the prior analyzer-first order resolved
		// an ON through a shadowing CTE against the TABLE's schema —
		// over-declining valid ONs (42703 on the CTE's own columns) and, for
		// an ON naming a table-only column, ADMITTING the upgrade and moving
		// the failure to a runtime malformed plan (review-caught). The
		// ON-ONLY scope (join/unnest bodies kept out of the GLOBAL cteScopes
		// — the flatten-evasion class) resolves here so the enclosing join's
		// ON is never silently dropped; a marker entry (nil Table) falls
		// through to the loud drop-risk arm in addTableSource.
		if src, found := cteOnScopes[strings.ToUpper(tableName)]; found {
			return src.Table
		}
		if cteScopes != nil {
			if src, found := cteScopes[strings.ToUpper(tableName)]; found {
				return src.Table
			}
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err == nil {
			return tbl
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
	addUnnestSourceRaw := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	// scopeDropRisk marks scope failures where the query could still PLAN and
	// return silently-wrong cross-product rows if we fall through: a
	// resolvable-but-unscopable source (derived-table decline, duplicate
	// alias). An UNRESOLVABLE table (resolveTable nil) is NOT a drop risk —
	// the downstream scan produces its precise UndefinedDatabase/Table error,
	// which the fail-closed check below must not preempt with a generic one.
	var scopeDropRisk bool
	addTableSource := func(tableName, alias, bindingID string) bool {
		// ACTIVE-SCHEMA-QUALIFIED source (`"s"."LA"`): the visitor path's sq
		// keeps the dotted spelling (normalizeSchemaQualifiedSelectSources
		// runs only on the catalog sub-build path), so resolveTable failed
		// and the silent unresolvable-table decline below dropped the ON —
		// but the downstream scan SUCCEEDS after the tree-side demotion, so
		// the "unresolvable table errors precisely downstream" assumption
		// that keeps the decline silent is FALSE for this form: every
		// explicit join with a schema-qualified leg silently cross-producted
		// (review-caught by the Q37 pin). Strip the schema segment the same
		// way the normalizer does, keeping a defaulted alias in lockstep.
		if segs := strings.Split(tableName, "."); len(segs) == 2 && resolvesToTable(segs) {
			if alias == tableName {
				alias = segs[1]
			}
			tableName = segs[1]
		}
		tbl := resolveTable(tableName)
		if tbl == nil {
			// A DECLARED CTE whose schema derivation declined (join/unnest
			// body the deriver cannot type) is resolvable-but-unscopable —
			// the same drop-risk class as a JOIN-bodied derived table: the
			// query still PLANS (translation resolves the CTE body), no
			// downstream error fires, and the dropped ON turns the join into
			// a silent cross product. An unresolvable REAL table stays a
			// silent decline (the downstream scan raises the precise
			// UndefinedTable error this generic one must not preempt).
			if isDeclaredCTE(tableName) {
				scopeDropRisk = true
			}
			return false
		}
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		// The binding correlation: the parser-minted duplicate-leg id when
		// present, else the alias. Duplicate
		// PLAIN aliases REGISTER (per-attribute resolution owns the
		// ambiguity); only a shadowing (unnest) duplicate still errors, and
		// that keeps the drop-risk taxonomy exactly as before for the class
		// AddSource can still reject.
		binding := bindingOrAlias(bindingID, aliasID)
		if scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: binding,
		}) != nil {
			scopeDropRisk = true // shadowing-duplicate alias: resolvable, unscopable
			return false
		}
		return true
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
			scopeDropRisk = true // join-bodied derived decline: plans, then cross-products
			return false
		}
		if j.bindingID != "" {
			src.CorrelationName = j.bindingID
		}
		if scope.AddSource(src) != nil {
			scopeDropRisk = true
			return false
		}
		return true
	}
	// A shape that RESOLVED as a lateral unnest but cannot be scoped (its
	// AS/AT alias collides with an existing source — AddSource duplicate) is
	// resolvable-but-unscopable: a drop risk by the same taxonomy (review
	// catch: the shared adder closure predates the flag and its dup-alias arm
	// escaped it — `FROM t AS x, t.arr AS x JOIN u ON …` silently dropped the
	// ON).
	addUnnestSource := func(j joinClause) bool {
		if !addUnnestSourceRaw(j) {
			scopeDropRisk = true
			return false
		}
		return true
	}
	var scopeOK bool
	if sq.derivedQuery != nil {
		// Primary FROM source is a derived table (`FROM (SELECT ...) x JOIN ...`).
		if src, ok := buildDerivedTableSource(md, sq.tableAlias, sq.derivedQuery); ok {
			scopeOK = scope.AddSource(src) == nil
			if !scopeOK {
				scopeDropRisk = true
			}
		} else {
			scopeDropRisk = true
		}
	} else {
		scopeOK = addTableSource(sq.tableName, sq.tableAlias, "")
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
		scopeOK = addTableSource(j.tableName, j.alias, j.bindingID)
	}
	if !scopeOK {
		// FAIL-CLOSED (guards a silent-wrong-rows bug class): the scope could
		// not be built, so no
		// ON predicate can be resolved. When the failure is a DROP RISK — a
		// resolvable-but-unscopable source (JOIN-bodied derived-table
		// decline, duplicate alias) — returning nil here would leave
		// OnPredicate nil and the translator silently degrades the join to a
		// CROSS PRODUCT (it never reads OnText for predicates): the same
		// failure class as the fixed subquery-in-ON bug and this function's
		// own fail-closed backstop below, which this early return used to
		// bypass. ON-less joins (comma cross joins, lateral unnest legs)
		// have nothing to drop, and UNRESOLVABLE-table failures keep the
		// silent decline — the downstream scan raises the precise
		// UndefinedDatabase/Table error this generic one must not preempt.
		if scopeDropRisk {
			for _, j := range sq.joins {
				if j.onExpr != nil {
					return api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"unsupported FROM shape: cannot resolve the join's sources for its ON clause (e.g. a JOIN-bodied derived table or duplicate unaliased source); dropping the ON condition would return cross-product rows")
				}
			}
		}
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
			// EXISTS in a JOIN ON clause (RFC-154 §5, Java parity). For an INNER
			// join this is equivalent to EXISTS in WHERE (no null-extension):
			// install a SubqueryPlanner so WalkPredicate builds the ON predicate's
			// ExistentialValuePredicate, then carry the collected EXISTS subqueries
			// on the join so translateJoin attaches the existential quantifier and
			// the NLJ rule's implementJoinWithExistential path builds the semi-join.
			//
			// OUTER joins are deferred (RFC-154 §5.2b): the ON-EXISTS is correlated
			// to the PRESERVED side and gates null-extension, which the semi-join
			// shape cannot express (implementJoinWithExistential would drop preserved
			// rows whose EXISTS is false instead of null-extending). Reject
			// fail-closed so OUTER EXISTS-in-ON never returns wrong rows.
			if expr.ContainsExistsAtom(sq.joins[sqIdx].onExpr) {
				if j.Kind != logical.JoinInner {
					return api.NewError(api.ErrCodeUnsupportedQuery,
						"EXISTS in an OUTER JOIN ON clause is not yet supported")
				}
				onPlanner := &existsSubqueryPlanner{
					md:          md,
					schemaName:  schemaName,
					outerScopes: buildOuterScopeSources(sq, md, schemaName),
					cteScopes:   cteScopes,
					cteOnScopes: cteOnScopes,
				}
				resolver.SetSubqueryPlanner(onPlanner)
				pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr)
				resolver.SetSubqueryPlanner(nil) // don't leak into the next join's walk
				if walkErr != nil {
					if apiErr := mapPredicateWalkError(walkErr); apiErr != nil {
						return apiErr
					}
					return api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"unsupported EXISTS in JOIN ON clause: %v", walkErr)
				}
				// The NLJ rule's implementJoinWithExistential handles exactly ONE
				// existential quantifier on a binary join (a 2-ForEach + 1-Existential
				// select); a join with two+ existentials falls through unplanned. That
				// is a pre-existing limitation shared with WHERE EXISTS over a join —
				// reject MULTIPLE EXISTS-in-ON cleanly here rather than let it surface
				// as the opaque "Cascades planner could not plan query" (RFC-154 §5;
				// single EXISTS-in-ON is the supported shape).
				if len(onPlanner.subqueries) > 1 {
					return api.NewError(api.ErrCodeUnsupportedQuery,
						"multiple EXISTS in a JOIN ON clause is not yet supported")
				}
				j.OnPredicate = predicates.SimplifyPredicateValues(pred)
				j.OnExistsSubqueries = onPlanner.subqueries
				continue
			}
			// A scalar `(SELECT ...)` or `x IN (SELECT ...)` subquery in the ON
			// clause: Go (like Java) does not support correlated scalar subqueries
			// or IN-subqueries anywhere. The ON resolver installs no SubqueryPlanner,
			// so WalkPredicate would decline with UnsupportedExpressionShapeError and
			// the fail-closed backstop below would surface it — but detect it
			// structurally first to emit a clear, position-specific message (mirroring
			// the EXISTS-in-ON rejection above) rather than leaking the resolver's
			// internal shape string.
			if expr.ContainsSubqueryAtom(sq.joins[sqIdx].onExpr) {
				return api.NewError(api.ErrCodeUnsupportedQuery,
					"subquery in a JOIN ON clause is not supported")
			}
			pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr)
			if walkErr != nil {
				// A recognized semantic / shape error (undefined or ambiguous column,
				// unknown source, bad IN list, a structured api.Error from a non-boolean
				// bare ON predicate like `ON a.amount`, RFC-146) is a real user error —
				// surface it with its correct SQLSTATE rather than dropping the ON
				// condition, which the translator silently degrades to a cross join (it
				// ignores OnText once OnPredicate is nil).
				if apiErr := mapPredicateWalkError(walkErr); apiErr != nil {
					return apiErr
				}
				// FAIL-CLOSED: any other resolver failure means we could not build this
				// ON predicate (e.g. an UnsupportedExpressionShapeError from a shape this
				// resolver has no planner for). Dropping it is NEVER safe — it degrades
				// the join to a CROSS PRODUCT (silent wrong rows, the pre-existing bug in
				// TODO.md "Known gaps"). Surface a clean error instead of the historical
				// silent `continue`.
				return api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"unsupported expression in JOIN ON clause: %v", walkErr)
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
		src.Alias = semantic.FromNormalized(tableAlias)
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
	// A NESTED WITH on the body (`c2 AS (WITH c3 … SELECT … FROM c3)`): the
	// body's FROM names resolve against the nested CTEs FIRST (lexical
	// scoping — the same shadowing the plan build applies via
	// buildCTEBodyQuery). Derive each nested CTE's schema recursively into a
	// SCOPED extension of priorCTEs (declaration order, so a later nested CTE
	// sees an earlier one) and resolve the body against that. Without this the
	// registration declined (body table `c3` unknown), the enclosing CTE fell
	// to the ON-only class, and every later NAMED read of it failed to plan.
	if ctes := cteQuery.Ctes(); ctes != nil {
		scoped := make(map[string]semantic.ScopeSource, len(priorCTEs)+2)
		for k, vv := range priorCTEs {
			scoped[k] = vv
		}
		for _, nq := range ctes.AllNamedQuery() {
			nname := functions.FullIdToName(nq.GetName())
			if src, ok := buildCTEColumnSource(md, nname, nq.Query(), scoped); ok {
				scoped[strings.ToUpper(nname)] = applyCTEColumnAliases(src, nq.GetColumnAliases())
			} else {
				// A DECLARED nested name SHADOWS an outer same-name CTE even
				// when its schema is not derivable (join-shaped body):
				// leaving the cloned outer entry in place validated the
				// enclosing body against the OUTER schema and baked its
				// ordinals over the inner's row — silent wrong slot. A
				// TOMBSTONE (nil Table), not deletion: absence falls back to
				// the CATALOG, and a same-named base table would bind its
				// ordinals onto the CTE's rows just as silently. The
				// tombstone hard-declines both resolution paths.
				scoped[strings.ToUpper(nname)] = semantic.ScopeSource{}
			}
		}
		priorCTEs = scoped
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
		// A JOIN/lateral-unnest-legged body stays OUT of the global cteScopes:
		// registering a projection-derived schema here widened WHERE/projection
		// resolution across comma-joined multi-leg CTEs into execution paths
		// that answer silent-wrong rows (the flatten-evasion gate pin's class).
		// The ONE consumer that must resolve such a CTE — an enclosing explicit
		// join's ON clause — reads the separate cteOnScopes map instead,
		// populated at WITH registration (registerCTEOnOnlyScope →
		// buildCTEOnOnlySource) and consumed only by upgradeJoinOnPredicates.
		// Underivable declared CTEs register a marker there and go LOUD (drop
		// risk), never a silent ON drop.
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
	// CTE-FIRST (execution's shadowing order, like every other resolution
	// consumer in this family): a prior CTE shadowing a same-named table
	// must supply the body's schema — metadata-first derived a shadowing
	// body's columns from the TABLE, declined on the CTE-only column, and
	// dumped the CTE into the ON-only marker path (review-caught: the
	// nested-shadow pin was green only while a stale outer entry happened
	// to carry the same column name).
	var innerTbl semantic.Table
	if priorCTEs != nil {
		if src, found := priorCTEs[strings.ToUpper(innerSQ.tableName)]; found {
			// A TOMBSTONE (declared CTE, schema underivable) hard-declines:
			// falling through to the catalog would derive the enclosing
			// schema from a same-named BASE TABLE and bake its ordinals
			// onto the CTE's rows — silent wrong slots.
			if src.Table == nil {
				return semantic.ScopeSource{}, false
			}
			innerTbl = src.Table
		}
	}
	if innerTbl == nil {
		cat := rlcatalog.Wrap(md)
		analyzer := semantic.NewAnalyzer(cat, false)
		if tbl, resolveErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(innerSQ.tableName, "."), false)); resolveErr == nil {
			innerTbl = tbl
		}
	}
	if innerTbl == nil {
		return semantic.ScopeSource{}, false
	}

	var columns []semantic.Column
	if innerSQ.projCols == nil {
		allCols := innerTbl.Columns()
		columns = make([]semantic.Column, len(allCols))
		copy(columns, allCols)
	} else {
		columns = make([]semantic.Column, 0, len(innerSQ.projCols))
		for i, col := range innerSQ.projCols {
			isComputed := i < len(innerSQ.projExprs) && innerSQ.projExprs[i] != nil
			bareName := col.bare
			if bareName == "" {
				bareName = col.name
			}
			outName := bareName
			if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
				outName = innerSQ.projAliases[i]
			}
			if isComputed {
				columns = append(columns, semantic.Column{
					Id:       semantic.FromNormalized(outName),
					Type:     "UNKNOWN",
					Nullable: true,
				})
				continue
			}
			innerCol, found := innerTbl.LookupColumn(semantic.FromNormalized(bareName))
			if !found {
				if hasComputedExpr {
					columns = append(columns, semantic.Column{
						Id:       semantic.FromNormalized(outName),
						Type:     "UNKNOWN",
						Nullable: true,
					})
					continue
				}
				return semantic.ScopeSource{}, false
			}
			// The virtual column carries the OUTPUT name the CTE body
			// projection emits — references resolve to it verbatim.
			columns = append(columns, semantic.Column{
				Id:       semantic.FromNormalized(outName),
				Type:     innerCol.Type,
				Nullable: innerCol.Nullable,
			})
		}
	}

	aliasID := semantic.FromNormalized(cteName)
	virtualTable := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{cteName}, false),
		TableColumns: columns,
	}
	return semantic.ScopeSource{
		Table:           virtualTable,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}, true
}

// buildCTEOnOnlySource derives the ON-RESOLUTION-ONLY ScopeSource for a
// declared CTE that buildCTEColumnSource keeps OUT of the global cteScopes (a
// join/lateral-unnest-legged body — see the decline comment there). It is
// registered in the separate cteOnScopes map at WITH registration, whose ONLY
// reader is upgradeJoinOnPredicates: an enclosing explicit join's ON resolves
// against it instead of being silently DROPPED (cross-product rows), while
// WHERE/projection resolution over comma-joined multi-leg CTEs keeps its clean
// decline (the flatten-evasion class).
//
// Output-name authority (must match what execution actually EMITS, or the
// fabricated "CTE.col" merge keys miss): an explicit projection alias
// (executeProjection always writes the alias key) or a BARE unqualified
// non-computed reference (the runtime key mirrors the SQL spelling — a bare
// ref plans as Project([AID],…) and keys bare). The bare-ref arm additionally
// requires every FROM leg to be ENUMERABLE — a base table, a DERIVABLE CTE
// (the resolver sees those via addSource's cteScopes fallback), or a lateral
// unnest leg (binds one alias via the unnest source adder): the ambiguity
// backstop (the body build 42702s an ambiguous bare ref before it can
// execute) only holds when the resolver can see every leg's columns. A
// derived-table leg among several hides its columns from that check, so a
// textually-bare-but-ambiguous ref would silently resolve against the wrong
// leg (review-caught, pinned by Q18) — and an ON-ONLY CTE leg is worse:
// buildSelectScope hands the body a NIL resolver, which kills the 42703
// unknown-column gate along with the ambiguity gate (review-caught,
// Q27/Q28). Bodies whose single source IS a derived
// table stay derivable, but every projection/aggregate INPUT read must
// resolve in the derived source's provably-readable name set
// (derivedEmittedBareNames): a join-shaped derived row keys by the INNER
// spelling, so an inner qualified-spelled item makes an outer `D.col` read a
// runtime malformed-plan failure (and an aggregate over it a silent NULL) —
// decline to the plan-time marker instead (Q19/Q20). A single-BASE-TABLE
// inner stays on the POSITIONAL frontier, where qualified items are readable
// by last segment (review-caught over-decline, Q33). Everything else DECLINES
// to the loud marker:
//   - an unaliased QUALIFIED reference resolves to a FieldValue whose Field
//     is the dotted source name ("D.ID" — see values.ProjectionColumnName),
//     so the row carries no bare key and an advertised bare name would read
//     a column the merged row never has;
//   - `WITH c(x, y)` column aliases rename the SCOPE view only — the runtime
//     row still keys by the body's own output names, so resolving `c.x` here
//     would turn today's loud 42703 into a silent runtime miss (worse);
//   - computed items without an alias key by their explain rendering.
//
// Aggregate bodies derive via buildDerivedTableSourceFromAgg (agg outputs key
// by their canonical names at runtime — the existing derived-table pathway).
// Columns type UNKNOWN/nullable (the same precedent — the scope needs NAMES,
// not exact types). A false return means the caller registers a nil-Table
// MARKER instead: the declared name still routes to the loud drop-risk 0AF00,
// never a silent ON drop. Widening the derivable set (qualified/renamed
// output schemas) is booked with the derived-table-twin item.
// cteScopePreState snapshots a name's scope-map state as it was BEFORE the
// CTE's own registration — what SQL scoping says the body sees: outer
// scopes and earlier siblings, never itself. had=false is the common case
// (the name was absent); the preserved VALUE is the nested-shadowing case —
// a subquery WITH reusing an OUTER CTE's name overwrites the level map's
// outer entry at registration, and a plain self-DELETE then lost BOTH
// bindings, sending the inner body's reads to the base table (42703 on the
// outer CTE's own column, review-caught).
type cteScopePreState struct {
	scopeVal semantic.ScopeSource
	scopeHad bool
	onVal    semantic.ScopeSource
	onHad    bool
}

// buildCTEBodySelfHidden runs a CTE body build with the CTE's name mapped
// to its PRE-REGISTRATION state in both scope maps: non-recursive SQL
// scoping makes `FROM <own-name>` inside the body the outer binding (an
// enclosing CTE) or the TABLE — never the CTE being defined. With CTE-FIRST
// scope resolution a visible self entry resolves the body against its own
// OUTPUT schema — on the chain paths that surfaced as a bogus
// correlated-fallback misroute AND a silent base-table value substitution
// through BuildScalar's 42703 arm; on the visitor path the R5a shadow pin
// caught it (review-caught on all three, one shared helper so the pipelines
// cannot diverge again). pre carries the pre-registration snapshots (nil ⇒
// absent for every name — the top-level visitor case). Recursive bodies
// keep self visible — their union machinery consumes the self-reference.
// Restores are deferred (error-path safe).
func buildCTEBodySelfHidden(
	cteScopes, cteOnScopes map[string]semantic.ScopeSource,
	upper string,
	pre map[string]cteScopePreState,
	recursive bool,
	build func() (logical.LogicalOperator, error),
) (logical.LogicalOperator, error) {
	if !recursive {
		st := pre[upper] // zero value: absent in both maps pre-registration
		if cur, ok := cteScopes[upper]; ok || st.scopeHad {
			if st.scopeHad {
				cteScopes[upper] = st.scopeVal
			} else {
				delete(cteScopes, upper)
			}
			defer func() {
				if ok {
					cteScopes[upper] = cur
				} else {
					delete(cteScopes, upper)
				}
			}()
		}
		if cteOnScopes != nil {
			if cur, ok := cteOnScopes[upper]; ok || st.onHad {
				if st.onHad {
					cteOnScopes[upper] = st.onVal
				} else {
					delete(cteOnScopes, upper)
				}
				defer func() {
					if ok {
						cteOnScopes[upper] = cur
					} else {
						delete(cteOnScopes, upper)
					}
				}()
			}
		}
	}
	return build()
}

// cteLegKind classifies a NAMED FROM leg of a CTE ON-only body by what
// EXECUTION will resolve it to. Declared CTE names come FIRST — a CTE
// shadows a same-named catalog table (review-caught: a metadata-first lookup
// classified a shadowed leg by the TABLE's schema while runtime rows came
// from the CTE). cteLegOpaque: an ON-ONLY CTE name (or unknown) — addSource
// returns false and buildSelectScope hands the body a NIL resolver, which
// skips BOTH the 42702 ambiguity gate and the 42703 unknown-column gate for
// the WHOLE body (the backstop every bare-ref admission rests on).
// cteLegDerivableCTE: a DERIVABLE CTE — addSource falls back to cteScopes,
// so the resolver still sees its columns. cteLegBase: a base table — the
// analyzer resolves it (the same ResolveTable call addSource makes), or the
// active-schema-qualified form of one (this derivation runs at WITH
// registration, BEFORE normalizeSchemaQualifiedSelectSources strips the
// schema segment — mirror that strip or valid "s"."T" legs classify opaque,
// review-caught).
type cteLegKindT int

const (
	cteLegOpaque cteLegKindT = iota
	cteLegBase
	cteLegDerivableCTE
)

func cteLegKind(md *recordlayer.RecordMetaData, schemaName string, cteScopes, cteOnScopes map[string]semantic.ScopeSource, name string) cteLegKindT {
	if name == "" || md == nil {
		return cteLegOpaque
	}
	upper := strings.ToUpper(name)
	if _, on := cteOnScopes[upper]; on {
		return cteLegOpaque
	}
	if _, ok := cteScopes[upper]; ok {
		return cteLegDerivableCTE
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	if _, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(name, "."), false)); err == nil {
		return cteLegBase
	}
	if segs := strings.Split(name, "."); len(segs) == 2 && newUnnestTableResolver(md, schemaName)(segs) {
		return cteLegBase
	}
	return cteLegOpaque
}

// cteBodyLegsEnumerable reports whether every named FROM leg of a multi-leg
// body is visible to the resolver (base table or derivable CTE) — the
// precondition for the 42702/42703 backstop the bare-ref admission relies
// on. Comma legs classified as lateral unnests (segments[0] names a prior
// source alias — RFC-142 R5: typed segments, never a tableName re-split) are
// enumerable by construction: the element alias binds one name and
// buildSelectScope adds it via the unnest source adder. An unnest leg's
// binding name is its EFFECTIVE alias (unnestAliases: the explicit AS, else
// the last segment) — recording the flattened dotted name instead broke
// chained no-AS unnests (`FROM T4, T4.SARR, SARR.SUB AS Y`: the scope
// exposes SARR, review-caught). Derived legs are the caller's decline, not
// this check's.
func cteBodyLegsEnumerable(md *recordlayer.RecordMetaData, schemaName string, cteScopes, cteOnScopes map[string]semantic.ScopeSource, sq *selectQuery) bool {
	if sq.derivedQuery == nil && cteLegKind(md, schemaName, cteScopes, cteOnScopes, sq.tableName) == cteLegOpaque {
		return false
	}
	tableFirst := newUnnestTableResolver(md, schemaName)
	prior := map[string]bool{strings.ToUpper(sq.tableAlias): true}
	for _, jc := range sq.joins {
		bind := jc.alias
		if bind == "" {
			bind = jc.tableName
		}
		if jc.derivedQuery == nil {
			priorHit := len(jc.segments) > 1 && prior[strings.ToUpper(jc.segments[0])]
			switch {
			case priorHit && len(jc.segments) == 2 && tableFirst(jc.segments):
				// ALIAS-EQUALS-SCHEMA collision: buildSelectScope keeps its
				// nil-resolver leniency for this class (the R5b Java-parity
				// pins), so the 42702/42703 backstop is DEAD for the body —
				// the enumerability premise fails; decline to the marker.
				return false
			case jc.fromComma && priorHit:
				// genuine lateral unnest: binds its effective alias
				if as, _ := unnestAliases(jc); as != "" {
					bind = as
				}
			case cteLegKind(md, schemaName, cteScopes, cteOnScopes, jc.tableName) == cteLegOpaque:
				return false
			}
		}
		prior[strings.ToUpper(bind)] = true
	}
	return true
}

// derivedEmittedBareNames computes the set of names a derived source's
// runtime row provably answers reads for — the read-authority for a CTE
// ON-only body whose single FROM source is that derived table. ok=false
// means the set is not statically closed and the caller must decline to the
// loud marker: SELECT * (names unknown here — no catalog access),
// aggregate/set-query bodies (their materialized-row keying is unverified on
// this path), a derived leg among multiple legs (the same ambiguity-backstop
// hole as the caller's own arm, one level down), any OPAQUE leg (an ON-only
// CTE gives the body build a NIL resolver — no 42702/42703 backstop), or an
// opaque/ON-only SINGLE source. The per-item rules mirror the caller's
// admission loop: an explicit alias is always emitted (executeProjection
// writes the alias key); a bare unqualified non-computed ref keys by its
// spelling; a QUALIFIED-spelled item over a single-BASE-TABLE body is
// readable by its LAST SEGMENT — and not merely because that body's
// projection row stays positional: the resolver's SINGLE-SOURCE resolution
// rewrites the projected FieldValue's Field to the BARE name at build time
// (expr.go ResolveIdentifier, needsQualification = len(sources) > 1; pinned
// by TestWalkExpression_SingleVsMultiSourceFieldQualification), so the key
// is bare in BOTH representations — the positional row AND the name-keyed
// Datum — which is what lets the claim survive a sort-continuation resume
// that rebuilds rows without positional state (review-verified: only
// join/merge-shaped inner rows are name-keyed; declining qualified items
// here over-declined the positional class). Computed unaliased items key by
// their explain rendering — nothing readable. Input reads recurse: when this
// level's single source is itself derived, every item's read target must
// resolve in the deeper set, else the body can never execute (a decline
// beats the runtime malformed-plan error it would otherwise be); a
// scalar-subquery's LOCAL refs are excluded from that check
// (harvestColumnRefsOutsideSubqueries) — its own build resolves them in its
// own scope, and a correlated read into the derived source surfaces loud at
// translation.
func derivedEmittedBareNames(md *recordlayer.RecordMetaData, schemaName string, cteScopes, cteOnScopes map[string]semantic.ScopeSource, q antlrgen.IQueryContext) (map[string]bool, bool) {
	if q == nil {
		return nil, false
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, false
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil {
		return nil, false
	}
	if len(sq.aggCols) > 0 || sq.countStar || sq.projCols == nil {
		return nil, false
	}
	legDerived := sq.derivedQuery != nil
	for _, jc := range sq.joins {
		if jc.derivedQuery != nil {
			legDerived = true
		}
	}
	if len(sq.joins) > 0 && (legDerived || !cteBodyLegsEnumerable(md, schemaName, cteScopes, cteOnScopes, sq)) {
		return nil, false
	}
	positionalFrontier := false
	if sq.derivedQuery == nil && len(sq.joins) == 0 {
		switch cteLegKind(md, schemaName, cteScopes, cteOnScopes, sq.tableName) {
		case cteLegBase:
			positionalFrontier = true
		case cteLegDerivableCTE:
			// derivable CTE rows answer alias/bare-spelling reads (the same
			// contract this function claims); NOT known-positional.
		default:
			return nil, false // ON-only CTE or unknown single source: opaque
		}
	}
	var deeper map[string]bool
	if sq.derivedQuery != nil {
		if deeper, ok = derivedEmittedBareNames(md, schemaName, cteScopes, cteOnScopes, sq.derivedQuery); !ok {
			return nil, false
		}
	}
	set := make(map[string]bool, len(sq.projCols))
	for i, col := range sq.projCols {
		isComputed := i < len(sq.projExprs) && sq.projExprs[i] != nil
		if deeper != nil {
			if isComputed {
				for _, r := range harvestBareColumnRefsOutsideSubqueries(sq.projExprs[i]) {
					if !deeper[r] {
						return nil, false
					}
				}
			} else if !deeper[colBareOrName(col)] {
				return nil, false
			}
		}
		switch {
		case i < len(sq.projAliases) && sq.projAliases[i] != "":
			set[sq.projAliases[i]] = true
		case isComputed:
			// unaliased computed: keys by its rendering — nothing readable
		case !col.qualified && col.bare != "":
			// the "" guard: a mixed-star sentinel slot (name=="") must
			// not deposit a junk claim in a soundness-critical set
			set[col.bare] = true
		case col.qualified && positionalFrontier:
			set[col.bare] = true
		}
	}
	return set, true
}

// cteBodyReadsResolvable reports whether every projection and aggregate INPUT
// read of a single-derived-source CTE body resolves in the derived source's
// emitted bare-key set. Aggregate outExpr entries are skipped: their refs
// read the POST-aggregation rowMap (agg outputs and group columns), not the
// input row — the group/arg inputs they depend on arrive via sibling aggCols
// entries, which ARE checked.
func cteBodyReadsResolvable(sq *selectQuery, emitted map[string]bool) bool {
	for _, ac := range sq.aggCols {
		if ac.groupCol != "" {
			bare := ac.groupColBare
			if bare == "" {
				bare = ac.groupCol
			}
			if !emitted[bare] {
				return false
			}
		}
		if ac.aggArg != "" {
			bare := ac.aggArgBare
			if bare == "" {
				bare = ac.aggArg
			}
			if !emitted[bare] {
				return false
			}
		}
		for _, r := range harvestBareColumnRefsOutsideSubqueries(ac.aggExpr) {
			if !emitted[r] {
				return false
			}
		}
	}
	for i, col := range sq.projCols {
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			for _, r := range harvestBareColumnRefsOutsideSubqueries(sq.projExprs[i]) {
				if !emitted[r] {
					return false
				}
			}
			continue
		}
		if !emitted[colBareOrName(col)] {
			return false
		}
	}
	return true
}

// cteBodyAliasQuoted returns, per SELECT element of a plain (non-star,
// non-aggregate) CTE body, whether its AS-alias was written QUOTED — aligned
// 1:1 with projCols for the bodies the projection path handles (star bodies
// return nil projCols, aggregate bodies branch to buildDerivedTableSourceFromAgg
// earlier — each guarded on its own path). Typed parse-tree read of the leading
// quote char on the raw Uid text, never a GetText string-match.
func cteBodyAliasQuoted(body *antlrgen.QueryTermDefaultContext) []bool {
	st, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok || st.SelectElements() == nil {
		return nil
	}
	elems := st.SelectElements().AllSelectElement()
	out := make([]bool, len(elems))
	for i, elem := range elems {
		if e, isExpr := elem.(*antlrgen.SelectExpressionElementContext); isExpr && e.Uid() != nil {
			out[i] = strings.HasPrefix(e.Uid().GetText(), `"`)
		}
	}
	return out
}

// cteBodyAllAliasesCaseSafe reports whether every QUOTED alias in a plain SELECT
// body is case-safe — equals its uppercase fold. It is the case-sensitivity half
// of the ON-only schema's complete-or-decline gate for the AGGREGATE path, whose
// extracted output names (aggCols[].outName / countStarAlias) are already
// StripIdentifierQuotes'd — quote-stripped, so the quoted flag is lost and a
// case-sensitive `AS "x"` can no longer be told from an unquoted `AS x` (both
// yield "x"; only the quoted one mis-resolves, since execution folds unquoted
// keys anyway). Reads the leading-quote off each Uid terminal (typed, not a
// GetText feature-match). An unquoted alias folds consistently → safe.
func cteBodyAllAliasesCaseSafe(body *antlrgen.QueryTermDefaultContext) bool {
	st, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok || st.SelectElements() == nil {
		return true
	}
	for _, elem := range st.SelectElements().AllSelectElement() {
		e, isExpr := elem.(*antlrgen.SelectExpressionElementContext)
		if !isExpr || e.Uid() == nil {
			continue
		}
		raw := e.Uid().GetText()
		if !strings.HasPrefix(raw, `"`) {
			continue // unquoted → folds consistently through execution, case-safe
		}
		name := functions.StripIdentifierQuotes(raw)
		if name != strings.ToUpper(name) {
			return false // quoted case-sensitive: runtime key uppercased, unnamable
		}
	}
	return true
}

func buildCTEOnOnlySource(
	cteName string,
	cteQuery antlrgen.IQueryContext,
	colAliases antlrgen.IFullIdListContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	if cteName == "" || cteQuery == nil {
		return semantic.ScopeSource{}, false
	}
	if colAliases != nil {
		// WITH c(x, y) renames are scope-level only; the runtime row keeps the
		// body's keys — decline to the loud marker rather than resolve names
		// the merged row will never carry.
		return semantic.ScopeSource{}, false
	}
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
	if len(innerSQ.joins) == 0 && innerSQ.derivedQuery == nil {
		// Plain single-table bodies are buildCTEColumnSource territory; if
		// THAT declined, this name-only derivation has nothing better to
		// offer. Derived-source bodies (`FROM (SELECT …) d` — zero joins but
		// declined globally for the derivedQuery reason) DO derive here: their
		// projection names key the runtime row the same way.
		return semantic.ScopeSource{}, false
	}
	legDerived := innerSQ.derivedQuery != nil
	for _, jc := range innerSQ.joins {
		if jc.derivedQuery != nil {
			legDerived = true
		}
	}
	if len(innerSQ.joins) > 0 && legDerived {
		// A derived-table leg among MULTIPLE legs: the resolver cannot
		// enumerate its columns, so the 42702 ambiguity backstop the bare-ref
		// arm rests on does not run — a textually-bare-but-ambiguous ref
		// silently resolves against the wrong leg (Q18). Decline the whole
		// body to the loud marker.
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.joins) > 0 && !cteBodyLegsEnumerable(md, schemaName, cteScopes, cteOnScopes, innerSQ) {
		// An OPAQUE leg — an ON-ONLY CTE name — is worse than a derived leg:
		// buildSelectScope's addSource knows base tables and cteScopes only,
		// so the body gets a NIL resolver and BOTH the 42702 ambiguity gate
		// and the 42703 unknown-column gate are skipped for the whole body
		// (review-caught: an ambiguous bare ref AND a nonexistent column both
		// planned fine). Decline — covers the aggregate arm below too (Q27,
		// Q28).
		return semantic.ScopeSource{}, false
	}
	var innerEmitted map[string]bool
	if innerSQ.derivedQuery != nil {
		// Single derived source: the runtime row keys by the INNER spelling.
		// Every input read below must resolve in its provably-emitted set —
		// a miss is a runtime malformed plan (projection read, Q19) or a
		// silent NULL (aggregate arg, Q20) if admitted.
		var ok bool
		if innerEmitted, ok = derivedEmittedBareNames(md, schemaName, cteScopes, cteOnScopes, innerSQ.derivedQuery); !ok {
			return semantic.ScopeSource{}, false
		}
	}
	if innerEmitted != nil && !cteBodyReadsResolvable(innerSQ, innerEmitted) {
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.aggCols) > 0 || innerSQ.countStar {
		// COMPLETE-SCHEMA-OR-DECLINE applies here too: buildDerivedTableSourceFromAgg
		// folds every output via NewUnquoted with NO validation, so a quoted
		// case-sensitive alias or a duplicate output name would silently
		// mis-resolve an enclosing ON ref (wrong-case resolves; dup first-matches)
		// — review-caught. Decline the whole source on either obstruction, keyed
		// by the RUNTIME-emitted (uppercased) name, exactly like the projection
		// path below. The dup check consumes aggOutputCols — the SAME visible-only
		// authority buildDerivedTableSourceFromAgg builds from — so it counts
		// exactly the names installed (a hidden HAVING aggregate is neither
		// advertised nor counted).
		if !cteBodyAllAliasesCaseSafe(body) {
			return semantic.ScopeSource{}, false
		}
		aggSeen := make(map[string]int)
		for _, c := range aggOutputCols(innerSQ) {
			aggSeen[strings.ToUpper(c.name)]++
		}
		for _, n := range aggSeen {
			if n > 1 {
				return semantic.ScopeSource{}, false
			}
		}
		return buildDerivedTableSourceFromAgg(cteName, innerSQ)
	}
	if innerSQ.projCols == nil {
		return semantic.ScopeSource{}, false // SELECT * over a multi-leg/derived body: no name authority
	}
	// COMPLETE-SCHEMA-OR-DECLINE. This schema is installed as ONE source of the
	// enclosing join; the resolver decides bare-ref ambiguity by which SOURCES
	// carry a name (scope.ResolveColumn). A PARTIAL install — advertising some
	// runtime columns and dropping others — is therefore UNSOUND: a dropped
	// column whose runtime key another enclosing source ALSO carries would let a
	// bare ref bind silently to that other source (the ref should be ambiguous),
	// and this function cannot see the enclosing scope to know. So we install
	// ONLY when every runtime column is advertised correctly and unambiguously;
	// any obstruction declines the WHOLE source (caller's loud 0AF00), never a
	// partial table. Two obstructions, each keyed by the RUNTIME-emitted name
	// (executeProjection uppercases every output key):
	//   (1) a quoted CASE-SENSITIVE alias (`AS "x"`, outName != its fold): the
	//       runtime key is "X" but no correct-case ref can name it (a `C."x"`
	//       plans then runtime-fails against the uppercased row; a `C."X"`
	//       silently resolves the wrong case). Can't advertise it truthfully.
	//   (2) a DUPLICATE runtime name (`… AS X, … AS X`, or `AS "x", AS "X"` —
	//       both emit "X"): the schema is AMBIGUOUS on that name; advertising one
	//       column silently joins on an arbitrary one and, when dropped, rebinds.
	// (A partial "keep the unique columns, drop the bad one" was tried and is
	// unsound for the rebind reason above — review-caught. The full-reach fix —
	// keep unique columns AND make the bad name resolve ambiguous via a
	// per-source poison marker in the resolver — is a booked conformance slice;
	// until then a body with ANY obstruction declines wholesale, correct-or-loud.)
	aliasQuoted := cteBodyAliasQuoted(body)
	columns := make([]semantic.Column, 0, len(innerSQ.projCols))
	seen := make(map[string]int, len(innerSQ.projCols))
	for i, col := range innerSQ.projCols {
		// The output name must be one execution PROVABLY emits: the explicit
		// alias (executeProjection always writes the alias key), or a BARE
		// unqualified non-computed reference (the runtime key mirrors the SQL
		// spelling — a bare ref keys bare, verified by plan shape
		// Project([AID],…)). The bare arm is sound here because every leg is
		// enumerable at this point — multi-leg bodies with a derived leg
		// declined above, so an ambiguous bare ref never EXECUTES: the body
		// build 42702s it and the wrap rebuild re-raises the swallowed error.
		// A QUALIFIED unaliased ref keys by its dotted source name and a
		// computed item by its explain rendering — both decline (no bare key
		// on the runtime row).
		outName := ""
		quoted := false
		if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
			outName = innerSQ.projAliases[i]
			quoted = i < len(aliasQuoted) && aliasQuoted[i]
		} else {
			isComputed := i < len(innerSQ.projExprs) && innerSQ.projExprs[i] != nil
			if !isComputed && !col.qualified && col.bare != "" {
				outName = col.bare
			}
		}
		if outName == "" {
			return semantic.ScopeSource{}, false
		}
		runtimeName := strings.ToUpper(outName)
		if quoted && outName != runtimeName { // obstruction (1): case-sensitive
			return semantic.ScopeSource{}, false
		}
		seen[runtimeName]++
		columns = append(columns, semantic.Column{
			Id:       semantic.FromNormalized(runtimeName),
			Type:     "UNKNOWN",
			Nullable: true,
		})
	}
	for _, n := range seen {
		if n > 1 { // obstruction (2): duplicate runtime name
			return semantic.ScopeSource{}, false
		}
	}
	if len(columns) == 0 {
		return semantic.ScopeSource{}, false
	}
	aliasID := semantic.FromNormalized(cteName)
	return semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{cteName}, false),
			TableColumns: columns,
		},
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}, true
}

// registerCTEOnOnlyScope stores the ON-only source (or the nil-Table marker)
// for a declared CTE that did NOT make it into the global cteScopes — the ONE
// registration authority both build pipelines (the plan visitor and the
// CTECatalog chain) share, so a declared CTE can never reach
// upgradeJoinOnPredicates untracked (the silent ON-drop class).
func registerCTEOnOnlyScope(dst map[string]semantic.ScopeSource, upperName string, cteQuery antlrgen.IQueryContext, colAliases antlrgen.IFullIdListContext, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	// Column-alias arity for underivable bodies is validated at the POINT OF
	// TRUTH instead of here: translateCTE checks the BUILT body's real output
	// width against the alias list (42F10) — a static width predictor at
	// registration kept re-implementing source resolution (stars, shadowing,
	// unnest, nested WITH) and drifting from the real resolver, the exact
	// two-authorities anti-pattern (review rounds 3-7).
	if src, ok := buildCTEOnOnlySource(upperName, cteQuery, colAliases, md, schemaName, cteScopes, dst); ok {
		dst[upperName] = src
		return nil
	}
	dst[upperName] = semantic.ScopeSource{} // marker: declared, underivable → loud drop risk
	return nil
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
	for i, col := range origCols {
		if i < len(aliases) {
			// The renamed column exposes the explicit CTE column alias as its
			// OUTPUT name — references (a.node) resolve to it verbatim.
			newName := functions.FullIdToName(aliases[i])
			newCols[i] = semantic.Column{
				Id:       semantic.FromNormalized(newName),
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

	addSource := func(tableName, alias, bindingID string) bool {
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		// The binding correlation: the parser-minted duplicate-leg id when
		// present, else the alias.
		binding := bindingOrAlias(bindingID, aliasID)
		// Try metadata first, then CTE scopes.
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err == nil {
			return scope.AddSource(semantic.ScopeSource{
				Table:           tbl,
				Alias:           aliasID,
				CorrelationName: binding,
			}) == nil
		}
		if src, found := cteScopes[strings.ToUpper(tableName)]; found && src.Table != nil {
			// found-with-nil-Table is a TOMBSTONE (declared CTE, schema
			// underivable) — decline instead of AddSource(nil) nil-deref.
			src.Alias = aliasID
			src.CorrelationName = binding
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
	if !addSource(sq.tableName, sq.tableAlias, "") {
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
		if !addSource(j.tableName, j.alias, j.bindingID) {
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

	addSource := func(tableName, alias, bindingID string) bool {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		binding := bindingOrAlias(bindingID, aliasID)
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: binding,
		}) == nil
	}
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if !addSource(sq.tableName, sq.tableAlias, "") {
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
		if !addSource(j.tableName, j.alias, j.bindingID) {
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
		cols = append(cols, semantic.Column{Id: semantic.FromNormalized(asAlias), Type: "UNKNOWN", Nullable: true})
	}
	if atAlias != "" {
		// The unnest WITH ORDINALITY ordinal is a 1-based, NON-NULL INT
		// (Java's Type.primitiveType(INT, false); the executor yields a 1-based
		// int per element). Register it with the recognized NON-NULL spelling so
		// sqlTypeToCascadesType resolves it to values.NotNullInt — matching the
		// translator's ordinal FieldValue type — and a PROJECT/COMPUTE over the AT
		// alias reports INT, not UNKNOWN. RFC-142.
		cols = append(cols, semantic.Column{Id: semantic.FromNormalized(atAlias), Type: "INT NOT NULL", Nullable: false})
		if corr == "" {
			corr = atAlias
		}
	}
	if corr == "" {
		return semantic.ScopeSource{}, false
	}
	corrID := semantic.FromNormalized(corr)
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
func buildLogicalPlanForSelectWithCatalog(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) (logical.LogicalOperator, error) {
	return buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, nil, nil)
}

func buildLogicalPlanForSelectWithCTECatalog(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource) (logical.LogicalOperator, error) {
	// For derived tables, build the inner plan through the catalog-aware
	// path so WHERE predicates get upgraded. Java's visitSubqueryTableItem
	// recursively visits through the same typed visitor.
	if sq.derivedQuery != nil && md != nil && len(sq.joins) == 0 {
		innerOp, innerErr := buildLogicalPlanForQueryBodyWithCTECatalog(
			sq.derivedQuery.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes,
		)
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			op := buildOuterPlanOnDerived(sq, innerOp)
			if op == nil {
				return nil, nil
			}
			return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, schemaName, cteScopes, cteOnScopes)
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
			j.derivedQuery.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes,
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
	return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, schemaName, cteScopes, cteOnScopes)
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
			if len(sq.sourceSegments) == 2 && strings.EqualFold(sq.sourceSegments[0], schemaName) {
				sq.sourceSegments = sq.sourceSegments[1:]
			}
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

func buildLogicalPlanForSelectWithCTECatalog_postBuild(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource, cteBodies ...map[string]logical.LogicalOperator) (logical.LogicalOperator, error) {
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
		if starErr := expandQualifiedStars(sq, md, schemaName, cteScopes); starErr != nil {
			return nil, starErr
		}
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
			if col.bare != "" {
				if err := resolveColumnRefStructural(resolver, col.bare, col.qualifier, col.qualified); err != nil {
					return nil, err
				}
			} else if err := resolveColumnName(resolver, col.name); err != nil {
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
			if !col.qualified && col.bare != "" && proj != nil {
				id := semantic.FromNormalized(col.bare)
				qv, ok, qerr := resolver.ResolveColumnShadowingQualified(semantic.Identifier{}, id)
				if qerr == nil && ok {
					if proj.ProjectedValues == nil {
						proj.ProjectedValues = make([]values.Value, len(proj.Projections))
					}
					if i < len(proj.ProjectedValues) {
						proj.ProjectedValues[i] = qv
					}
				}
				var unresShadow *expr.UnresolvableOrdinalError
				if errors.As(qerr, &unresShadow) {
					// Born-baked (slice 2): the scope bound the name but the
					// source cannot answer a plan-time ordinal — never fall
					// through to the name channel.
					return nil, unresShadow
				}
				// A BARE non-shadowed column resolves through the
				// scope so the projection carries the construction-bound ordinal
				// (a childless source-relative baked FieldValue — the resolver's
				// single-source bind). Anything else — a multi-source
				// QOV-correlated resolution, an unresolvable name, a lazy result —
				// keeps the translator's name emission unchanged. Twin of the
				// PlanVisitor's bare-projection bind.
				if proj.ProjectedValues == nil || (i < len(proj.ProjectedValues) && proj.ProjectedValues[i] == nil) {
					rv, rerr := resolver.ResolveIdentifier(semantic.Identifier{}, id)
					if rerr == nil {
						if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child == nil && fv.SourceRelativeBaked() {
							if proj.ProjectedValues == nil {
								proj.ProjectedValues = make([]values.Value, len(proj.Projections))
							}
							if i < len(proj.ProjectedValues) {
								proj.ProjectedValues[i] = fv
							}
						}
					}
					var unresIdent *expr.UnresolvableOrdinalError
					if errors.As(rerr, &unresIdent) {
						return nil, unresIdent
					}
				}
			}
			if col.qualified && proj != nil {
				if proj.ProjectedValues == nil {
					proj.ProjectedValues = make([]values.Value, len(proj.Projections))
				}
				if len(sq.joins) > 0 {
					if i < len(proj.ProjectedValues) {
						// A qualified projection over a join
						// emits the resolver's QUANTIFIER-ADDRESSED
						// source-relative baked reference when resolvable (the
						// executor binds the leg window off the merged row's
						// own leg boundaries — rowLegsBinder); a flat dotted
						// "ALIAS.COL" name lookup does not run this path.
						// Twin of the PlanVisitor's qualified-projection bind
						// (incl. the DUPLICATED-bare-leaf qualified output pin).
						cr := colRef{table: col.qualifier, col: col.bare}
						if bv := resolveQualifiedBaked(resolver, cr); bv != nil {
							// A qualified projection's structural bake —
							// duplicated qualifiers included (per-attribute
							// resolution addresses one leg by its binding;
							// the display-keyed carve-out this arm once
							// deferred to is retired).
							proj.ProjectedValues[i] = bv
							if bareLeafDuplicated(sq.projCols, sq.projAliases, i) {
								if proj.Aliases == nil {
									proj.Aliases = make([]string, len(proj.Projections))
								}
								if i < len(proj.Aliases) && proj.Aliases[i] == "" {
									proj.Aliases[i] = strings.ToUpper(col.name)
								}
							}
						} else {
							// Born-baked (slice 3; the dup-alias flat-name
							// carve-out is RETIRED — a duplicated qualifier
							// bakes QOV(binding) per-attribute above, first
							// leg included, since only later duplicates were
							// renamed and QOV(alias) addresses exactly one
							// leg; ambiguous dup reads die 42702 upstream):
							// a validated qualified projection that cannot
							// bake a leg-window ordinal must fail the plan,
							// never mint a lazy name read.
							return nil, &expr.UnresolvableOrdinalError{Field: cr.col, Source: cr.table}
						}
					}
				} else {
					var qualifier semantic.Identifier
					id := semantic.FromNormalized(colBareOrName(col))
					if col.qualified {
						qualifier = semantic.FromNormalized(col.qualifier)
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
						// Output-alias precedence, mirrored from the visitor
						// path's arm (review-caught: this twin was missed, so
						// the SAME query 42702'd inside a subquery while the
						// top level answered): a BARE key binding exactly ONE
						// output alias wins over FROM-scope ambiguity.
						if bare, n := orderByOutputAliasBinding(ob.rawExpr, ob.colName, sq); bare && n == 1 {
							continue
						}
						// Java's exact text, from the reference as written (M5).
						return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
							"Ambiguous reference %s", ambigErr.Reference())
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
						if ob.bare != "" {
							if resolveColumnRefStructural(resolver, ob.bare, ob.qualifier, ob.qualified) == nil {
								continue
							}
						} else if ob.colName != "" && resolveColumnName(resolver, ob.colName) == nil {
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
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				continue
			}
			if gb.bare != "" {
				if err := resolveColumnRefStructural(resolver, gb.bare, gb.qualifier, gb.qualified); err != nil {
					return nil, err
				}
			} else if err := resolveColumnName(resolver, gb.display); err != nil {
				return nil, err
			}
		}
	}

	if resolver != nil {
		for _, ac := range sq.aggCols {
			if ac.aggArg != "" && ac.aggExpr == nil {
				if ac.aggArgBare != "" {
					if err := resolveColumnRefStructural(resolver, ac.aggArgBare, ac.aggArgQualifier, ac.aggArgQualified); err != nil {
						return nil, err
					}
				} else if err := resolveColumnName(resolver, ac.aggArg); err != nil {
					return nil, err
				}
			}
		}
	}

	// (5b) Validate SELECT-list group-column re-reads through the scope: a
	// BARE re-read that is ambiguous across sources (GROUP BY po.id, pi.id
	// re-read as `id`) is 42702 (Java AMBIGUOUS_COLUMN) — the aggregate
	// output-name table matches keys qualifier-stripped, so an unvalidated
	// bare re-read would silently bind ONE leg's key last-wins.
	// Expression-redirected entries (groupCol = the GROUP BY expression's
	// display) carry no column reference and are skipped.
	if resolver != nil {
		exprKeyDisplays := map[string]bool{}
		for _, gn := range sq.groupBy {
			if gn.expr != nil {
				exprKeyDisplays[gn.display] = true
			}
		}
		for _, ac := range sq.aggCols {
			if ac.groupCol == "" || ac.groupColBare == "" || exprKeyDisplays[ac.groupCol] {
				continue
			}
			if err := resolveColumnRefStructural(resolver, ac.groupColBare, ac.groupColQualifier, ac.groupColQualified); err != nil {
				return nil, err
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
					// Consistent SQLSTATE with the WHERE-EXISTS path (genuine
					// resolution failure → 42703/42702; Unsupported decline → 0A000).
					if mapped := mapPredicateWalkError(walkErr); mapped != nil {
						return nil, mapped
					}
					return nil, api.NewError(api.ErrCodeUnsupportedOperation, corrErr.Error())
				}
			}
		}
	}

	// Derived-table/CTE references resolve to the OUTPUT column name verbatim
	// (Java semantics); the projection already emits under that name, so there
	// is nothing to rewrite back to a source column.

	if len(sq.joins) > 0 {
		if err := upgradeJoinOnPredicates(op, sq, md, schemaName, cteScopes, cteOnScopes); err != nil {
			return nil, err
		}
	}

	if len(sq.aggCols) > 0 {
		if uerr := upgradeAggregateOperands(op, sq, md, schemaName, cteScopes); uerr != nil {
			return nil, uerr
		}
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
			cteOnScopes: cteOnScopes,
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
		if herr := upgradeHavingPredicate(op, sq, md, schemaName, cteScopes, existsPlanner); herr != nil {
			return nil, herr
		}
		// HAVING has no per-group correlated-scalar quantifier lowering yet.
		// Never let the freshly minted alias escape unattached into runtime
		// evaluation (an UnboundScalarSubqueryError on valid SQL).
		if len(existsPlanner.correlatedScalarSubqueries) > 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"correlated scalar subquery in a HAVING predicate is not supported")
		}
	}

	if err := upgradeSortKeyValues(op, sq, md, schemaName, cteScopes); err != nil {
		return nil, err
	}

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
		if qerr := qualifyShadowedSortKeys(op, resolver); qerr != nil {
			return nil, qerr
		}
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
			op = wrapGlobalRankVectorLimit(op, qualPred)
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
			// Classify the failure with its correct SQLSTATE (shared with the
			// JOIN-ON path). A bare ColumnNotFoundError maps to
			// ErrCodeUndefinedColumn so a correlated subquery's BuildExists falls
			// back to buildCorrelatedExists with its richer outer scope (RFC-142
			// P2c); a structured *api.Error from a nested subquery build (e.g.
			// RFC-141 R4's buried-scalar-EXISTS rejection) surfaces VERBATIM rather
			// than degrading to the text-fallback builder below (which would mask the
			// precise reason). An UNrecognized error falls through to that text
			// fallback, preserving the historical WHERE behavior.
			if apiErr := mapPredicateWalkError(walkErr); apiErr != nil {
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
	hasSubqueries := existsPlanner != nil &&
		(len(existsPlanner.subqueries) > 0 ||
			len(existsPlanner.scalarSubqueries) > 0 ||
			len(existsPlanner.correlatedScalarSubqueries) > 0)
	if hasSubqueries && preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)

		// EXISTS is lowered to a conjunctive semi-join; under an OR that loses
		// the disjunction and silently returns empty. Reject rather than
		// return wrong rows (RFC-082; inline-EXISTS-under-OR is future work).
		if existsUnderDisjunction(pred) {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"EXISTS within an OR (disjunction) is not supported")
		}

		if installErr := installFirstWherePredicate(op, pred); installErr != nil {
			return nil, installErr
		}
		if len(existsPlanner.subqueries) > 0 {
			if !upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE subqueries could not be installed on the logical plan")
			}
		}
		if len(existsPlanner.scalarSubqueries) > 0 {
			if !upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE scalar subqueries could not be installed on the logical plan")
			}
		}
		if len(existsPlanner.correlatedScalarSubqueries) > 0 {
			if !upgradeFirstFilterCorrelatedScalarSubqueries(op, existsPlanner.correlatedScalarSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE correlated scalar subqueries could not be installed on the logical plan")
			}
		}
		return op, nil
	}

	var pred predicates.QueryPredicate
	var ok bool
	if cteScopes != nil && len(sq.joins) == 0 {
		if src, found := cteScopes[strings.ToUpper(sq.tableName)]; found && src.Table != nil {
			// A TOMBSTONE entry (nil Table: declared CTE, schema underivable)
			// must not reach scope construction — ResolveColumn nil-derefs.
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
	if installErr := installFirstWherePredicate(op, pred); installErr != nil {
		return nil, installErr
	}
	op = wrapGlobalRankVectorLimit(op, pred)
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
	schemaStrip := newUnnestTableResolver(md, schemaName)

	addSource := func(tableName, alias, bindingID string) bool {
		// ACTIVE-SCHEMA-QUALIFIED source (`"s"."LA"`): on the visitor path
		// sq keeps the dotted spelling (normalizeSchemaQualifiedSelectSources
		// runs only on the catalog sub-build path), and a raw ResolveTable
		// miss here NIL'd the whole resolver — killing the 42702/42703 gates
		// (an ambiguous bare ref over `"s"."LA", LB` executed silently,
		// review-caught) and every WHERE/ORDER BY resolution over
		// schema-qualified explicit joins. Strip the schema segment with a
		// defaulted alias in lockstep, mirroring the ON-upgrade scope build
		// and the normalizer.
		if segs := strings.Split(tableName, "."); len(segs) == 2 && schemaStrip(segs) {
			if alias == tableName {
				alias = segs[1]
			}
			tableName = segs[1]
		}
		// CTE-FIRST: a declared CTE shadows a same-named catalog table
		// (execution's translateScan contract; the same ordering cteLegKind
		// applies). The prior catalog-first order analyzed the TABLE's
		// schema for reads that execute against the CTE — 42703 on the
		// CTE's own columns (review-caught; the plain-body variant of the
		// shape was broken this way all along, masked only for
		// schema-qualified bodies by the pre-round-9 nil resolver).
		if cteScopes != nil {
			if src, found := cteScopes[strings.ToUpper(tableName)]; found {
				// TOMBSTONE (nil Table): a DECLARED CTE whose schema is not
				// derivable in this context (underivable nested shadow). It
				// must NOT fall through to the catalog — a same-named base
				// table would bind ITS ordinals onto the CTE's rows (silent
				// wrong slots). Declining the scope add keeps resolution
				// loud downstream.
				if src.Table == nil {
					return false
				}
				aliasID := semantic.FromNormalized(alias)
				if alias == "" {
					aliasID = semantic.FromNormalized(tableName)
				}
				return scope.AddSource(semantic.ScopeSource{
					Table:           src.Table,
					Alias:           aliasID,
					CorrelationName: bindingOrAlias(bindingID, aliasID),
				}) == nil
			}
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: bindingOrAlias(bindingID, aliasID),
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
	} else if !addSource(sq.tableName, sq.tableAlias, "") {
		return nil
	}
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	for i, j := range sq.joins {
		if j.derivedQuery != nil {
			if src, ok := buildDerivedTableSource(md, j.alias, j.derivedQuery); ok {
				if j.bindingID != "" {
					src.CorrelationName = j.bindingID
				}
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
		if segs := strings.Split(j.tableName, "."); len(segs) == 2 && resolvesToTable(segs) {
			if _, collides := visible[strings.ToUpper(segs[0])]; collides {
				// ALIAS-EQUALS-SCHEMA collision (`FROM PA AS "s", "s"."PB"`):
				// Java's table-first rule classifies the LEG as the real
				// table (isLateralUnnestJoin above already declined the
				// unnest reading), but a STRICT scope over this FROM would
				// 42703 references the R5b Java-parity pins answer (the
				// query reads PA.* while PA is aliased "s"). Keep the
				// pre-existing nil-resolver leniency for exactly this
				// collision class; the plain schema-qualified form (no
				// collision) resolves strictly via the addSource strip.
				return nil
			}
		}
		if !addSource(j.tableName, j.alias, j.bindingID) {
			return nil
		}
	}
	return expr.New(analyzer, scope)
}

// orderByOutputAliasBinding classifies an ORDER BY key for the
// output-alias-precedence rule: bareIdent is true only when the RAW key is
// exactly a single-segment column reference (typed walk — columnNameFromExpr
// canonicalizes aggregate calls too, so `ORDER BY SUM(K)` must NOT match a
// quoted "SUM(K)" alias and thereby suppress a genuine ambiguity inside the
// aggregate's argument, review-caught); matches counts how many projection
// output columns bind the name (a presence-only check let duplicate aliases
// K, K bypass 42702 and silently sort by whichever the alias map kept last,
// review-caught). The precedence applies only when bareIdent && matches==1.
func orderByOutputAliasBinding(rawExpr antlrgen.IExpressionContext, colName string, sq *selectQuery) (bareIdent bool, matches int) {
	if colName == "" || !isBareIdentifierExpr(rawExpr) {
		return false, 0
	}
	upper := strings.ToUpper(colName)
	for _, a := range sq.projAliases {
		if a != "" && strings.ToUpper(a) == upper {
			matches++
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" && strings.ToUpper(ac.outName) == upper {
			matches++
		}
	}
	return true, matches
}

// isBareIdentifierExpr reports whether the expression is exactly a
// single-segment column reference — descending only single-child wrapper
// nodes to the atom (typed nodes, never text).
func isBareIdentifierExpr(e antlrgen.IExpressionContext) bool {
	var n antlr.Tree = e
	for n != nil {
		if c, ok := n.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
			return len(c.FullColumnName().FullId().AllUid()) == 1
		}
		if n.GetChildCount() != 1 {
			return false
		}
		n = n.GetChild(0)
	}
	return false
}

// resolveColumnRefStructural resolves a column reference from its
// parse-tree SEGMENTS — never a dotted re-split of a rendered string,
// so a derived column or alias whose NAME contains a dot ("A.ID")
// resolves as itself instead of being torn at the last dot into a
// phantom qualifier (WS-N Phase A slice 1; the segments arrive
// quote-stripped with quoted case preserved, so identifiers are built
// case-sensitively — no re-fold).
func resolveColumnRefStructural(resolver *expr.Resolver, bare, qualifier string, qualified bool) error {
	if resolver == nil || bare == "" {
		return nil
	}
	var qual semantic.Identifier
	display := bare
	if qualified {
		// The qualifier FOLDS: source aliases are registered through the
		// folded namespace (a quoted lowercase alias "q" registers as
		// "Q"), so the lookup must fold identically. Alias-namespace
		// case fidelity is Phase B's scope; the COLUMN side below stays
		// verbatim — that is where dotted/quoted-case names live.
		qual = semantic.FromNormalized(qualifier)
		display = qualifier + "." + bare
	}
	_, err := resolver.ResolveIdentifier(qual, semantic.New(bare, true))
	if err != nil {
		var notFound *semantic.ColumnNotFoundError
		if errors.As(err, &notFound) {
			// Folded retry: derived/virtual schemas still REGISTER their
			// columns folded (an alias "id" registers as "ID"), so a
			// verbatim miss re-tries the folded spelling. This retry MUST
			// fold — NewUnquoted on purpose, the one deliberate
			// re-normalization: FromNormalized would re-issue the
			// identical verbatim spelling and the exact-case lookup would
			// miss again (a quoted-lowercase reference over a derived
			// table's folded registration → spurious 42703).
			// Verbatim-first keeps case-significant names ("A.ID", quoted
			// lowercase stored columns) winning; Phase D makes
			// registrations case-faithful and retires this retry.
			if _, retryErr := resolver.ResolveIdentifier(qual, semantic.NewUnquoted(bare)); retryErr == nil {
				return nil
			}
		}
	}
	return mapColumnResolveError(err, display)
}

// resolveColumnName is the RENDERED-STRING arm for call sites whose
// carrier predates structural segment capture: it re-splits at the last
// dot (parseColRef), which mis-tears dotted display names — every
// caller with parse-tree segments must use resolveColumnRefStructural.
func resolveColumnName(resolver *expr.Resolver, col string) error {
	if resolver == nil || col == "" {
		return nil
	}
	var qualifier semantic.Identifier
	ref := parseColRef(col)
	id := semantic.FromNormalized(ref.bare())
	if ref.isQualified() {
		qualifier = semantic.FromNormalized(ref.table)
	}
	_, err := resolver.ResolveIdentifier(qualifier, id)
	return mapColumnResolveError(err, col)
}

// mapColumnResolveError classifies a ResolveIdentifier failure into its
// SQLSTATE (42702 ambiguous / 42703 undefined), shared by the
// structural and rendered-string arms.
func mapColumnResolveError(err error, display string) error {
	if err != nil {
		var ambigErr *semantic.AmbiguousColumnError
		if errors.As(err, &ambigErr) {
			// Java's exact SemanticAnalyzer text, from the reference as
			// written — verified against both duplicate and distinct
			// aliases, bare and qualified.
			return api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"Ambiguous reference %s", ambigErr.Reference())
		}
		var notFoundErr *semantic.ColumnNotFoundError
		if errors.As(err, &notFoundErr) {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist", display)
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

// upgradeFirstFilterExistsSubqueries walks the single-child chain from op and,
// at the first LogicalFilter, attaches the EXISTS subquery plans. Returns true
// when a Filter was found.
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

// upgradeFirstFilterCorrelatedScalarSubqueries attaches correlated scalar
// plans to the WHERE filter that owns their ScalarSubqueryValue references.
// Unlike uncorrelated scalars these plans are not pre-evaluated: the Cascades
// translator materializes each scalar per outer row through the strict
// LEFT-scalar join lowering.
func upgradeFirstFilterCorrelatedScalarSubqueries(op logical.LogicalOperator, subqueries []logical.CorrelatedScalarSubquery) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.CorrelatedScalarSubqueries = subqueries
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

// installFirstWherePredicate is the checked form of upgradeFirstFilter for
// WHERE-bearing production builders. Those builders create a LogicalFilter
// before adding unary SELECT shells; if that structural invariant ever drifts,
// fail closed with a typed SQL error instead of returning a logical tree whose
// text-only filter cannot be translated.
func installFirstWherePredicate(op logical.LogicalOperator, pred predicates.QueryPredicate) error {
	if pred == nil {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"WHERE predicate could not be constructed")
	}
	if !upgradeFirstFilter(op, pred) {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"WHERE predicate could not be installed on the logical plan")
	}
	return nil
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
		copy(vals, proj.ProjectedValues)
		agg := findAggregate(op)
		var groupKeyExplains map[string]values.Value
		if agg != nil && len(agg.GroupKeys) > 0 {
			groupKeyExplains = make(map[string]values.Value, len(agg.GroupKeys))
			for _, gk := range agg.GroupKeys {
				if gk.Value == nil {
					continue
				}
				explain := strings.ToUpper(values.ColumnNameValue(gk.Value))
				ref := &values.FieldValue{Field: explain, Typ: values.UnknownType}
				groupKeyExplains[explain] = ref
				groupKeyExplains[strings.ToUpper(gk.Display)] = ref
			}
		}
		aggSlots := make([]bool, len(proj.Projections))
		if agg != nil && len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
			for i, ordinal := range proj.AggregateOutputOrdinals {
				aggSlots[i] = ordinal >= len(agg.GroupKeys)
				if ordinal >= 0 {
					typ := aggregateNativeOutputType(agg, ordinal)
					// The slot is RECORDED, not derived: proj.AggregateOutputOrdinals[i]
					// was fixed by buildAggregateOutputSlots while the parser's
					// structural key/call identities were still in hand, and it
					// addresses the aggregate's [keys..., calls...] output row —
					// the executor's assembled frontier, not this reference's
					// source. PINNED for exactly that reason: an unpinned ordinal
					// invites the downstream binder (groupByOutputBaker) to discard
					// it and RECOVER a slot from a map keyed by the rendered output
					// name, which is last-wins on any duplicated label. The name
					// here is the display label only.
					vals[i] = values.NewFieldValueWithPinnedOrdinal(
						aggregateNativeOutputName(agg, ordinal), ordinal, typ)
				}
			}
		}
		for i, e := range sq.postAggExprs {
			if i >= len(vals) || e == nil {
				continue
			}
			if groupKeyExplains != nil && len(proj.AggregateOutputOrdinals) != len(proj.Projections) {
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
			if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
				v, err = bindPostAggregateValueToNativeOrdinals(v, agg)
				if err != nil {
					return err
				}
			} else {
				v = rewriteAggregateValuesInTree(v, agg)
			}
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
		if len(proj.AggregateOutputOrdinals) != len(proj.Projections) {
			for i := range vals {
				vals[i] = rebasePostAggregateGroupKeyValue(vals[i], agg)
			}
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
				// Route through the SAME classifier the WHERE-EXISTS path uses so a
				// GENUINE resolution failure in the ON (missing column/source) reports
				// 42703/42702 and only a DELIBERATE Unsupported decline reports 0A000 —
				// the projected and WHERE forms then agree on the SQLSTATE.
				if mapped := mapPredicateWalkError(err); mapped != nil {
					return mapped
				}
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
		// The regular-projection path has no aggregate above it, so there is no
		// output row whose slots could be recorded; the reference keeps its
		// name-only form.
		v = rewriteAggregateValuesInTree(v, nil)
		vals[i] = v
	}
	proj.ProjectedValues = vals
	proj.AggregateSlots = aggSlots
	return nil
}

func aggregateNativeOutputType(agg *logical.LogicalAggregate, ordinal int) values.Type {
	if agg == nil || ordinal < 0 {
		return values.UnknownType
	}
	if ordinal < len(agg.GroupKeys) {
		if v := agg.GroupKeys[ordinal].Value; v != nil && v.Type() != nil {
			return v.Type()
		}
		return values.UnknownType
	}
	callIdx := ordinal - len(agg.GroupKeys)
	if callIdx < 0 || callIdx >= len(agg.Calls) {
		return values.UnknownType
	}
	var operand []values.Value
	if callIdx < len(agg.AggregateOperands) {
		operand = []values.Value{agg.AggregateOperands[callIdx]}
	}
	return aggregateCallOutputType(agg.Calls[callIdx], operand)
}

func aggregateNativeOutputName(agg *logical.LogicalAggregate, ordinal int) string {
	if agg == nil || ordinal < 0 {
		return ""
	}
	if ordinal < len(agg.GroupKeys) {
		key := agg.GroupKeys[ordinal]
		if key.Value != nil {
			return aggregateGroupKeyOutputName(key.Value)
		}
		if key.Bare != "" {
			return strings.ToUpper(key.Bare)
		}
		return strings.ToUpper(key.Display)
	}
	callIdx := ordinal - len(agg.GroupKeys)
	if callIdx < 0 || callIdx >= len(agg.Calls) {
		return ""
	}
	return strings.ToUpper(agg.Calls[callIdx].CanonicalName())
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

func upgradeAggregateOperands(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	agg := findAggregate(op)
	if agg == nil {
		return nil
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
	}
	if resolver == nil {
		return nil
	}
	operands := make([]values.Value, len(agg.Calls))
	for _, ac := range sq.aggCols {
		// A PLAIN-column aggregate arg (`MIN(pid)`, `MIN(c2.pid)`) carries
		// aggArg only — the parser's resolveArg captures no aggExpr for a bare
		// FullColumnName. It must STILL resolve here: the translator's
		// bare-column lazy read keeps a qualified arg as ONE opaque dotted
		// FieldValue{"C2.PID"}, which key-misses the scan row's bare "PID" at
		// accumulation and silently aggregates NULL (a sub-planned scalar
		// subquery's rows carry bare keys only). COUNT(*) has neither and skips.
		if ac.aggFunc == "" || (ac.aggExpr == nil && ac.aggArg == "") {
			continue
		}
		// Collect EVERY matching aggregate slot — a HAVING that repeats a
		// SELECT-list aggregate (`SELECT SUM(x) … HAVING SUM(x) > k`) creates a
		// SECOND slot with the same call shape; leaving it unresolved makes
		// the translator fall back to the lazy bare-column read, whose flat
		// dotted operand refs the ordinal frontier cannot resolve.
		arg := ac.aggArg
		if arg == "" && ac.aggExpr != nil {
			arg = canonicalTextOf(ac.aggExpr)
		}
		if arg == "" {
			arg = "*"
		}
		// The aggregate slot may carry the BARE column while the parsed arg
		// is qualified (`MIN(PID)` vs aggArg `C.PID`) — the main builder
		// strips a same-source qualifier when naming the slot. Match the
		// bare form too so the resolved-operand path engages for it.
		bare := ""
		if ac.aggArgQualified {
			bare = ac.aggArgBare
		}
		wantFunc := strings.ToUpper(ac.aggFunc)
		var idxs []int
		for i, call := range agg.Calls {
			if call.Func != wantFunc || call.Distinct != ac.aggDistinct {
				continue
			}
			if strings.EqualFold(call.Operand, arg) || (bare != "" && strings.EqualFold(call.Operand, bare)) {
				idxs = append(idxs, i)
			}
		}
		if len(idxs) == 0 {
			continue
		}
		var v values.Value
		if ac.aggExpr != nil {
			walked, err := resolver.WalkExpression(ac.aggExpr)
			if err != nil {
				continue
			}
			v = walked
		} else {
			// Plain-column arg: resolve through the semantic scope
			// (ResolveIdentifier — the same resolution a WHERE reference gets),
			// so a qualified `c2.pid` binds against its FROM source instead of
			// surviving as opaque dotted text. Unresolvable → fall through to
			// the text path (fail-soft).
			bareArg := ac.aggArgBare
			if bareArg == "" {
				bareArg = ac.aggArg
			}
			var qualID semantic.Identifier
			if ac.aggArgQualified {
				qualID = semantic.FromNormalized(ac.aggArgQualifier)
			}
			qv, rerr := resolver.ResolveIdentifier(qualID, semantic.FromNormalized(bareArg))
			if rerr != nil || qv == nil {
				var unresArg *expr.UnresolvableOrdinalError
				if errors.As(rerr, &unresArg) {
					return unresArg
				}
				continue
			}
			v = qv
		}
		for _, idx := range idxs {
			operands[idx] = v
		}
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
		if i < len(sq.groupBy) && sq.groupBy[i].expr != nil {
			v, err := resolver.WalkExpressionForProjection(sq.groupBy[i].expr)
			if err != nil {
				continue
			}
			keyValues[i] = v
			filled = true
			continue
		}
		gk := agg.GroupKeys[i]
		// Structured segments, never a re-parse of the display text: a
		// delimited identifier containing a dot stays one bare segment.
		ref := colRef{table: gk.Qualifier, col: gk.Bare}
		if gk.Bare == "" {
			ref = colRef{col: gk.Display}
		}
		var qualID semantic.Identifier
		if gk.Qualified {
			qualID = semantic.FromNormalized(gk.Qualifier)
			// The dup-alias twin: a qualified key
			// binding a LATER duplicate-alias leg must group by the BINDING
			// correlation (`Q$DUP1.QID`) — the join row's actual namespace —
			// never the display alias, whose bare FieldValue fallback misses
			// and silently groups every row under NULL. Same helper as the
			// projection and ORDER-BY paths (qualifyShadowedSortKeys), so the
			// three cannot diverge; nil for every non-duplicate reference.
			// An AmbiguousColumnError here is DISCARDED on purpose: the
			// upstream group-key reference validation already terminated an
			// ambiguous key with 42702 before this pass runs (the ladder's
			// >=2 arm is owned there, not here).
			qv, qerr := resolver.ResolveQualifiedProjection(qualID, semantic.FromNormalized(ref.bare()))
			if qerr == nil && qv != nil {
				keyValues[i] = qv
				filled = true
				continue
			}
			var unres *expr.UnresolvableOrdinalError
			if errors.As(qerr, &unres) {
				// Born-baked (slice 2): never fall to the name channel.
				return unres
			}
			// Every other QUALIFIED group key resolves through
			// the scope to the quantifier-addressed source-relative baked
			// reference (QOV(leg).col with the construction-bound ordinal) —
			// the executor binds the leg's window off the merged row's own leg
			// boundaries (rowLegsBinder), so the key resolves positionally
			// instead of through a flat dotted "ALIAS.COL" name
			// read. The group-key OUTPUT column is labeled by the BARE field
			// (AggregateKeyColumnName) — the unified Java label rule.
			if bv := resolveQualifiedBaked(resolver, ref); bv != nil {
				keyValues[i] = bv
				filled = true
				continue
			}
			// A qualified reference over ONE source resolves to the same
			// childless source-relative bake as its unqualified twin: the
			// qualifier is redundant once the scope proves there is no leg
			// choice. resolveQualifiedBaked intentionally accepts only
			// QOV-addressed multi-source references, so retain this exact
			// single-source result explicitly. On a join, childless would lose
			// the defining leg and remains forbidden.
			if len(sq.joins) == 0 {
				rv, rerr := resolver.ResolveIdentifier(qualID, semantic.FromNormalized(ref.bare()))
				if rerr == nil {
					if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child == nil && fv.SourceRelativeBaked() {
						keyValues[i] = fv
						filled = true
						continue
					}
				}
				var unresKey *expr.UnresolvableOrdinalError
				if errors.As(rerr, &unresKey) {
					return unresKey
				}
			}
		}
		qv, ok, err := resolver.ResolveColumnShadowingQualified(qualID, semantic.FromNormalized(ref.bare()))
		if err == nil && ok {
			keyValues[i] = qv
			filled = true
		}
		var unresShadow *expr.UnresolvableOrdinalError
		if errors.As(err, &unresShadow) {
			return unresShadow
		}
		// A BARE non-shadowed group key resolves through the
		// scope so it carries the construction-bound ordinal (a childless
		// source-relative baked FieldValue — the resolver's single-source
		// bind). Field stays the bare column, so the aggregate's OUTPUT
		// column name (AggregateKeyColumnName = Field) and every downstream
		// name-keyed consumer are unchanged. Qualified keys, multi-source
		// resolutions, and unresolvable names keep the translator's name
		// emission.
		if keyValues[i] == nil && !ref.isQualified() {
			rv, rerr := resolver.ResolveIdentifier(semantic.Identifier{}, semantic.FromNormalized(ref.bare()))
			if rerr == nil {
				if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child == nil && fv.SourceRelativeBaked() {
					keyValues[i] = fv
					filled = true
				}
			}
			var unresKey *expr.UnresolvableOrdinalError
			if errors.As(rerr, &unresKey) {
				return unresKey
			}
		}
	}
	if filled {
		for i := range agg.GroupKeys {
			if keyValues[i] != nil {
				agg.GroupKeys[i].Value = keyValues[i]
			}
		}
	}
	return nil
}

func upgradeHavingPredicate(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) error {
	agg := findAggregate(op)
	if agg == nil || sq.havingExpr == nil {
		return nil
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
	}
	if resolver == nil {
		return nil
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
		// SEMANTIC errors surface with Java's codes: a bare HAVING re-read
		// of an ambiguous grouped column is 42702 (Java AMBIGUOUS_COLUMN),
		// exactly like the ORDER-BY twin — not a planner decline. An
		// unbindable ordinal is loud per born-baked (slice 2). Every OTHER
		// walk failure keeps the HasHaving decline sentinel: the translator
		// rejects a set-but-unresolved HAVING, so nothing is dropped.
		var ambig *semantic.AmbiguousColumnError
		if errors.As(err, &ambig) {
			return api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"Ambiguous reference %s", ambig.Reference())
		}
		var unres *expr.UnresolvableOrdinalError
		if errors.As(err, &unres) {
			return unres
		}
		return nil
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
		rewriteAggregateRefsInPredicate(pred, agg), agg,
	)
	if subqPlanner != nil && len(subqPlanner.subqueries) > 0 {
		agg.HavingExistsSubqueries = subqPlanner.subqueries
		subqPlanner.subqueries = nil
	}
	if subqPlanner != nil && len(subqPlanner.scalarSubqueries) > 0 {
		agg.HavingScalarSubqueries = subqPlanner.scalarSubqueries
		subqPlanner.scalarSubqueries = nil
	}
	return nil
}

func rewriteAggregateRefsInPredicate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) predicates.QueryPredicate {
	switch p := pred.(type) {
	case *predicates.ComparisonPredicate:
		lhs := rewriteAggregateValuesInTree(p.Operand, agg)
		// Copy the whole Comparison and replace ONLY the rewritten RHS operand,
		// preserving Escape and every other Comparison field. A fresh
		// {Type, Operand} would drop the LIKE escape rune (and the parameter /
		// text / distance-rank metadata) and change comparison semantics. RFC-142.
		cmp := p.Comparison
		cmp.Operand = rewriteAggregateValuesInTree(p.Comparison.Operand, agg)
		return predicates.NewComparisonPredicate(lhs, cmp)
	case *predicates.AndPredicate:
		rewritten := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sub := range p.SubPredicates {
			rewritten[i] = rewriteAggregateRefsInPredicate(sub, agg)
		}
		return predicates.NewAnd(rewritten...)
	case *predicates.OrPredicate:
		rewritten := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sub := range p.SubPredicates {
			rewritten[i] = rewriteAggregateRefsInPredicate(sub, agg)
		}
		return predicates.NewOr(rewritten...)
	case *predicates.NotPredicate:
		return predicates.NewNot(rewriteAggregateRefsInPredicate(p.Child, agg))
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

func rewriteAggregateValuesInTree(v values.Value, agg *logical.LogicalAggregate) values.Value {
	if v == nil {
		return nil
	}
	if _, ok := v.(*values.AggregateValue); ok {
		return rewriteAggregateValue(v, agg)
	}
	if av, ok := v.(*values.ArithmeticValue); ok {
		return &values.ArithmeticValue{
			Op:    av.Op,
			Left:  rewriteAggregateValuesInTree(av.Left, agg),
			Right: rewriteAggregateValuesInTree(av.Right, agg),
		}
	}
	if sf, ok := v.(*values.ScalarFunctionValue); ok {
		args := make([]values.Value, len(sf.Args))
		for i, a := range sf.Args {
			args[i] = rewriteAggregateValuesInTree(a, agg)
		}
		return values.NewScalarFunctionValue(sf.FuncName, sf.Typ, args...)
	}
	if cv, ok := v.(*values.CastValue); ok {
		return values.NewCastValue(rewriteAggregateValuesInTree(cv.Child, agg), cv.Target)
	}
	// A machine-inserted PromoteValue (expr.promoteColumnColumnNumeric,
	// RelOpValue's Java analogue: `HAVING SUM(int_col) > 5.5` rewrites to
	// PromoteValue(SUM(int_col), DOUBLE) > 5.5) can wrap an AggregateValue
	// exactly like CastValue can — without this case the aggregate stayed
	// buried one level down, unrewritten, and reached AggregateValue.Evaluate
	// at row time via the residual filter path (which always errors — an
	// aggregate has no per-row scalar semantics), turning a legal
	// `HAVING aggregate > scalar-subquery` of mismatched numeric types into
	// "42803: aggregate function is not allowed here".
	if pv, ok := v.(*values.PromoteValue); ok {
		return values.NewPromoteValue(rewriteAggregateValuesInTree(pv.Child, agg), pv.Target)
	}
	if pv, ok := v.(*values.PickValue); ok {
		alts := make([]values.Value, len(pv.Alternatives))
		for i, a := range pv.Alternatives {
			alts[i] = rewriteAggregateValuesInTree(a, agg)
		}
		return values.NewPickValue(rewriteAggregateValuesInTree(pv.Selector, agg), alts, pv.Typ)
	}
	if cs, ok := v.(*values.ConditionSelectorValue); ok {
		impl := make([]values.Value, len(cs.Implications))
		for i, c := range cs.Implications {
			impl[i] = rewriteAggregateValuesInTree(c, agg)
		}
		return values.NewConditionSelectorValue(impl)
	}
	if ph, ok := v.(expr.PredicateValueHolder); ok {
		rewritten := rewriteAggregateRefsInPredicate(ph.GetPredicate(), agg)
		ph.SetPredicate(rewritten)
		return ph
	}
	return v
}

// aggregateCallOutputSlot is THE structural matcher from an AggregateValue to
// the slot that aggregate occupies in the aggregate's output row, and the sole
// place a post-aggregate reference to an aggregate is born. It answers with a
// FieldValue whose ordinal is RECORDED — the call's index in agg.Calls, offset
// by the group keys, which is precisely the [keys..., calls...] order
// GroupByOutputColumnNames and the executor's aggregateCursor emit.
//
// The match is on the aggregate's IDENTITY (function plus semantic operand
// equality), never on a rendered name. That matters because the two renderings
// that would otherwise have to agree are produced by different code from
// different inputs — canonicalAggName walks the RESOLVED operand Value while
// AggregateResultColumnName renders the PARSE TEXT the builder captured — so a
// name channel between them is a coincidence the compiler cannot check.
//
// The canonical-name fallback survives for the one case identity cannot serve:
// a catalog-free or constant call that carries no resolved AggregateOperands
// entry, where the call's own canonical rendering is the only identity there
// is. It stays a fallback, not a first choice.
//
// The result is PINNED: the ordinal is final against the executor's assembled
// output row, not relative to any source's declared column order. That pin is
// what stops groupByOutputBaker from discarding a slot decided here and
// recovering one from a last-wins map keyed by the rendered output name.
func aggregateCallOutputSlot(av *values.AggregateValue, agg *logical.LogicalAggregate) (*values.FieldValue, bool) {
	if av == nil || agg == nil {
		return nil, false
	}
	var matches []int
	for i, call := range agg.Calls {
		if av.Op == values.AggCountStar {
			if call.Star {
				matches = append(matches, i)
			}
			continue
		}
		if call.Star || !strings.EqualFold(call.Func, av.Op.Symbol()) {
			continue
		}
		if i < len(agg.AggregateOperands) && agg.AggregateOperands[i] != nil &&
			values.SemanticEqualsUnderAliasMap(av.Operand, agg.AggregateOperands[i], values.AliasMap{}) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		want := normalizeAggregateBindingName(canonicalAggName(av.Op.Symbol(), av.Operand))
		for i, call := range agg.Calls {
			if normalizeAggregateBindingName(call.CanonicalName()) == want {
				matches = append(matches, i)
			}
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	// Repeated identical aggregate calls are value-equivalent; the first
	// native slot is a deterministic, semantics-preserving bind.
	ordinal := len(agg.GroupKeys) + matches[0]
	return values.NewFieldValueWithPinnedOrdinal(
		agg.Calls[matches[0]].CanonicalName(), ordinal, av.Type()), true
}

// bindPostAggregateValueToNativeOrdinals rewrites a computed SELECT item over a
// grouped row while producer identity is still structural. AggregateValue nodes
// bind only to aggregate-call slots; group-key Values bind only to key slots.
// This must run before aggregate aliases are rendered as FieldValue names:
// `SUM(v) AS a` and group key `a` intentionally share a label but never a slot.
func bindPostAggregateValueToNativeOrdinals(v values.Value, agg *logical.LogicalAggregate) (values.Value, error) {
	if v == nil || agg == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"post-aggregate expression has no aggregate output layout")
	}

	var bindErr error
	bound := values.Replace(v, func(node values.Value) values.Value {
		if bindErr != nil {
			return node
		}
		if av, ok := node.(*values.AggregateValue); ok {
			bound, bindOK := aggregateCallOutputSlot(av, agg)
			if !bindOK {
				bindErr = api.NewError(api.ErrCodeUnsupportedQuery,
					"post-aggregate expression could not bind an aggregate call to the native output row")
				return node
			}
			return bound
		}

		// Pre-order matching binds a whole computed GROUP BY expression before
		// considering any of its leaves.
		keyMatch := -1
		for i, key := range agg.GroupKeys {
			if key.Value != nil &&
				(values.SemanticEqualsUnderAliasMap(node, key.Value, values.AliasMap{}) ||
					fieldValueMatchesAggregateGroupKey(node, key.Value, agg)) {
				keyMatch = i
				break
			}
		}
		if keyMatch >= 0 {
			// keyMatch is the key's index in agg.GroupKeys, which IS its output
			// slot (the row is [group keys in this order..., calls...]). The
			// match above is structural (semantic equality / resolved path), so
			// the slot is a recorded fact; pinning keeps it one, instead of
			// handing the downstream binder a bare leaf whose name it must
			// re-key — the shape that bound two same-leaf group keys to one slot.
			return values.NewFieldValueWithPinnedOrdinal(
				aggregateGroupKeyOutputName(agg.GroupKeys[keyMatch].Value), keyMatch, node.Type())
		}
		if _, isField := node.(*values.FieldValue); isField {
			// This is an ORIGINAL resolver reference: replacement roots are
			// not revisited by values.Replace. If it was neither consumed as
			// an aggregate operand nor matched as a complete grouping-key
			// subtree, its source-relative ordinal has no meaning on the
			// private aggregate row. Even an ordinal that happens to fall
			// within nativeWidth would be a coincidental, silently wrong
			// reinterpretation.
			bindErr = api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate expression references a field outside the aggregate output contract")
			return node
		}
		return node
	})
	if bindErr != nil {
		return nil, bindErr
	}
	return bound, nil
}

// fieldValueMatchesAggregateGroupKey recognizes the one safe representation
// difference semantic equality preserves: a qualified read over a single
// source carries QOV(source), while the same source's GROUP BY key may already
// have had that redundant qualifier stripped. Resolved path and defining source
// still have to prove identical; a multi-source aggregate never gets this
// childless/qualified relaxation.
//
// The ordinal path is the identity (values.SameColumnPath — the domain-checked,
// non-negative form of Java's ordinal-only FieldPath.equals). The per-accessor
// DISPLAY-name check this used to AND on is gone: a group-key reference whose
// display name was aliased away still denotes the column its ordinal names, and
// refusing it was the name-as-identity conflation in its refusing direction. The
// name was load-bearing against Ordinal -1 name-only accessors, where ordinal
// equality is vacuous; SameColumnPath declines those outright, which covers the
// same hazard without consulting a display name.
func fieldValueMatchesAggregateGroupKey(candidate, key values.Value, agg *logical.LogicalAggregate) bool {
	cf, cok := candidate.(*values.FieldValue)
	kf, kok := key.(*values.FieldValue)
	if !cok || !kok || !values.SameColumnPath(cf.Resolved, kf.Resolved) {
		return false
	}
	cq, cHasQOV := cf.Child.(*values.QuantifiedObjectValue)
	kq, kHasQOV := kf.Child.(*values.QuantifiedObjectValue)
	switch {
	case cf.Child == nil && kf.Child == nil:
		// Childless resolved ordinals are source-relative. They identify a
		// producer field only when there is exactly one possible inner source.
		return len(innerSourceAliases(agg.Input)) == 1
	case cf.Child != nil && kf.Child != nil:
		return cHasQOV && kHasQOV &&
			strings.EqualFold(cq.Correlation.Name(), kq.Correlation.Name())
	default:
		var qualified *values.QuantifiedObjectValue
		switch {
		case cHasQOV && kf.Child == nil:
			qualified = cq
		case kHasQOV && cf.Child == nil:
			qualified = kq
		default:
			return false
		}
		aliases := innerSourceAliases(agg.Input)
		if len(aliases) != 1 {
			return false
		}
		_, sameSource := aliases[strings.ToUpper(qualified.Correlation.Name())]
		return sameSource
	}
}

func normalizeAggregateBindingName(s string) string {
	return strings.ReplaceAll(strings.ToUpper(s), " ", "")
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
		inner = strings.ToUpper(values.ColumnNameValue(operand))
		inner = strings.ReplaceAll(inner, " ", "")
		if len(inner) > 2 && inner[0] == '(' && inner[len(inner)-1] == ')' {
			inner = inner[1 : len(inner)-1]
		}
	}
	return fn + "(" + inner + ")"
}

// canonicalAggOperandText returns the operand segment of a canonical
// aggregate name this builder just produced via canonicalAggName — the text
// between the outermost parens. Operates only on our own rendering (never
// user SQL), purely to keep AggregateCall.Operand identical to the name's
// keyed segment. (Follow-up with F-3: canonicalAggName should return the
// pair instead of render-then-unrender.)
func canonicalAggOperandText(cname string) string {
	l := strings.Index(cname, "(")
	r := strings.LastIndex(cname, ")")
	if l < 0 || r <= l {
		return ""
	}
	return cname[l+1 : r]
}

func rewriteAggregateValue(v values.Value, agg *logical.LogicalAggregate) values.Value {
	if v == nil {
		return nil
	}
	av, ok := v.(*values.AggregateValue)
	if !ok {
		return v
	}
	// The aggregate's OUTPUT SLOT is decided here, where its structural identity
	// (function + resolved operand) is still in hand, and it is recorded on the
	// reference. Emitting only the rendered canonical name — which is what this
	// did — hands the slot decision to groupByOutputBaker, which recovers it from
	// a map keyed by AggregateResultColumnName's rendering of the PARSE TEXT.
	// The two renderings are produced by different code from different inputs and
	// agree only by convention: when they diverge the lookup MISSES and the
	// reference falls through to a name-model read, and when two of them collide
	// the map is last-wins. Java binds a post-aggregate reference by the call's
	// loop index for the same reason (CompensateRecordConstructorRule.java:92 over
	// Column.unnamedOf columns — the columns have no names to look up).
	if bound, ok := aggregateCallOutputSlot(av, agg); ok {
		return bound
	}
	// No aggregate composition in hand (a projection with no aggregate above it),
	// or a call this aggregate does not contain: keep the name-only reference so
	// the surface of shapes that resolve downstream is unchanged.
	//
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
	for _, gb := range sq.groupBy {
		if gb.expr != nil && expr.ContainsExistsAtom(gb.expr) {
			return api.NewError(api.ErrCodeUnsupportedQuery,
				"projected EXISTS in this query shape is not yet supported")
		}
	}

	groupBySet := make(map[string]bool, len(sq.groupBy))
	for _, gb := range sq.groupBy {
		groupBySet[strings.ToUpper(gb.display)] = true
		if gb.qualified {
			groupBySet[strings.ToUpper(gb.bare)] = true
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
	for _, gb := range sq.groupBy {
		if gb.expr != nil {
			groupByExprSet[strings.ToUpper(gb.display)] = true
		}
	}

	// HAVING obeys the SAME grouping rule as the SELECT list: a base column
	// referenced OUTSIDE an aggregate must be covered by GROUP BY, else it is a
	// 42803 grouping error. The OutsideSubqueries walk skips aggregate-call
	// operands (SUM(v)'s v) AND does not descend into nested subqueries — a
	// column syntactically inside a HAVING subquery (`… HAVING EXISTS(SELECT 1
	// FROM u WHERE u.v = k)`) binds to THAT query block, so it must not be
	// group-checked against the outer sources (else a subquery-local column
	// whose bare name collides with an ungrouped outer column would wrongly
	// 42803). "Covered" = the column is a GROUP BY key OR appears inside a
	// GROUP BY EXPRESSION key, so `GROUP BY k+1 HAVING k+1 > 5` stays valid.
	// Only genuine base columns of a KNOWN source are flagged (the tableFields
	// guard) — an aggregate OUTPUT ALIAS or a derived/CTE source (tableFields
	// == nil) is left to downstream resolution. Without this, `HAVING id > 2`
	// with id neither grouped nor aggregated silently read the group key at
	// id's colliding ordinal (wrong rows), or leaked an internal "ordinal
	// resolution ... malformed plan" when id's base ordinal exceeded the
	// aggregated row width. Mirrors the SELECT-list/aggCols validation above.
	if sq.havingExpr != nil {
		groupByColumns := make(map[string]bool)
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				for _, c := range harvestBareColumnRefsOutsideSubqueries(gb.expr) {
					groupByColumns[strings.ToUpper(c)] = true
				}
				continue
			}
			groupByColumns[parseColRef(strings.ToUpper(gb.display)).bare()] = true
			if gb.qualified {
				groupByColumns[strings.ToUpper(gb.bare)] = true
			}
		}
		for _, ref := range harvestBareColumnRefsOutsideSubqueries(sq.havingExpr) {
			bare := strings.ToUpper(ref)
			if groupByColumns[bare] {
				continue
			}
			if tableFields != nil && tableFields[bare] {
				return api.NewErrorf(api.ErrCodeGroupingError,
					"column %q must appear in the GROUP BY clause or be used in an aggregate function", ref)
			}
		}
	}

	// ORDER BY over a grouped row obeys the same coverage rule as HAVING.
	// Validate it before exact native-slot binding so an ungrouped source
	// reference remains the user-facing 42803 error, not an internal 0AF00
	// contract failure. Aggregate operands are skipped by this walk, while a
	// unique bare output alias wins before source-column interpretation.
	if len(sq.orderBy) > 0 {
		groupByColumns := make(map[string]bool)
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				for _, c := range harvestBareColumnRefsOutsideSubqueries(gb.expr) {
					groupByColumns[strings.ToUpper(c)] = true
				}
				continue
			}
			groupByColumns[parseColRef(strings.ToUpper(gb.display)).bare()] = true
			if gb.qualified {
				groupByColumns[strings.ToUpper(gb.bare)] = true
			}
		}
		for _, ob := range sq.orderBy {
			if ob.pos > 0 || ob.rawExpr == nil {
				continue
			}
			if bare, n := orderByOutputAliasBinding(ob.rawExpr, ob.colName, sq); bare && n == 1 {
				continue
			}
			for _, ref := range harvestBareColumnRefsOutsideSubqueries(ob.rawExpr) {
				bare := strings.ToUpper(ref)
				if groupByColumns[bare] {
					continue
				}
				if tableFields != nil && tableFields[bare] {
					diagnosticRef := ref
					// The structural harvester deliberately returns bare
					// segments so delimited identifiers containing dots are
					// never re-split. For a direct plain ORDER BY reference we
					// already carry its parse-derived qualifier separately;
					// restore that source spelling for the user-facing 42803
					// diagnostic without using it as binding identity.
					if ob.qualified && strings.EqualFold(ob.bare, ref) {
						diagnosticRef = ob.qualifier + "." + ob.bare
					}
					return api.NewErrorf(api.ErrCodeGroupingError,
						"column %q must appear in the GROUP BY clause or be used in an aggregate function", diagnosticRef)
				}
			}
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
		if err := checkColumn(col.name); err != nil {
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
	addSource := func(tableName, alias, bindingID string) bool {
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		// The binding correlation: the parser-minted duplicate-leg id when
		// present, else the alias.
		binding := bindingOrAlias(bindingID, aliasID)
		if src, ok := cteScopes[strings.ToUpper(tableName)]; ok {
			src.Alias = aliasID
			src.CorrelationName = binding
			return scope.AddSource(src) == nil
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: binding,
		}) == nil
	}
	addDerived := func(alias string, derivedQuery antlrgen.IQueryContext, bindingID string) bool {
		if src, ok := buildDerivedTableSource(md, alias, derivedQuery); ok {
			if bindingID != "" {
				src.CorrelationName = bindingID
			}
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
			if !addDerived(sq.tableName, sq.derivedQuery, "") {
				return nil
			}
		} else if !addSource(sq.tableName, sq.tableAlias, "") {
			return nil
		}
	}
	for i, j := range sq.joins {
		if j.derivedQuery != nil {
			if !addDerived(j.alias, j.derivedQuery, j.bindingID) {
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
		if !addSource(j.tableName, j.alias, j.bindingID) {
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
	// DELETE … LIMIT is rejected — Java rejects it too (QueryVisitor.visitDeleteStatement:
	// Assert ctx.limitClause()==null, "limit is not supported"), so this is conformant
	// in REJECTING with the same message. The shared grammar accepts a limitClause on a
	// DELETE, but honoring it is unimplemented — and the builder otherwise IGNORES it,
	// which silently DELETES ALL rows matching the WHERE instead of the requested subset
	// (data loss: `DELETE … WHERE p LIMIT 1` deleted every matching row). Fail closed.
	//
	// SQLSTATE: Java's 2-arg Assert.thatUnchecked(bool,String) defaults to
	// INTERNAL_ERROR (XX000) — a leaky internal code. We deliberately use the cleaner
	// 0AF00 (UNSUPPORTED_QUERY) instead, consistent with this PR's other XX000→clean-code
	// fixes; only the code differs from Java, not the reject-with-this-message behavior.
	if del != nil && del.LimitClause() != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "limit is not supported")
	}
	op := buildLogicalPlanForDelete(del)
	if op == nil || md == nil || del == nil {
		return op, nil
	}
	tableName := ""
	if tn := del.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	// Validate the schema qualifier (if any) BEFORE classifying WHERE-column errors: a bad
	// qualifier's 42F00 (Unknown database) must take precedence over a WHERE-column 42703
	// when the bare table happens to exist in the active schema. For a valid/absent
	// qualifier this strips to the bare name used below. (Missing-but-valid-qualifier target
	// tables are caught with 42F01 in planDML after resolveQualifiedTableNames.)
	if tableName != "" {
		resolved, qErr := functions.ResolveQualifiedTableName(tableName, schemaName)
		if qErr != nil {
			return nil, qErr
		}
		tableName = resolved
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
	pred, ok, werr := buildWherePredicateForTableE(md, bare, bare, w)
	if werr != nil {
		// e.g. 42804 from a bare non-boolean DELETE WHERE — surface it, don't
		// mask it as a generic DML translation error (RFC-146).
		return nil, werr
	}
	if !ok {
		return op, nil
	}
	if installErr := installFirstWherePredicate(op, pred); installErr != nil {
		return nil, installErr
	}
	return op, nil
}

// recordTypeCI resolves a record type by name CASE-INSENSITIVELY. SQL table names arrive
// upper-cased (functions.FullIdToName), but a proto-derived record type keeps its proto
// message-name case (e.g. "Order"), and the catalog/analyzer the WHERE/SELECT path uses
// resolves case-insensitively — so a raw, case-sensitive md.GetRecordType would wrongly
// miss a real table (e.g. SQL "ORDER" vs record type "Order"). The fast path tries the
// exact key first (the common CREATE TABLE case, where the type is already upper-cased).
func recordTypeCI(md *recordlayer.RecordMetaData, name string) *recordlayer.RecordType {
	if md == nil {
		return nil
	}
	if rt := md.GetRecordType(name); rt != nil {
		return rt
	}
	for n, rt := range md.RecordTypes() {
		if strings.EqualFold(n, name) {
			return rt
		}
	}
	return nil
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
		// Surface an AUTHORITATIVE semantic classification from this subquery-aware walk —
		// an undefined / ambiguous column or bad source. This walk (with a SubqueryPlanner),
		// unlike the plain text fallback, can see PAST an EXISTS atom to a LATER bad column,
		// so dropping its ColumnNotFoundError would leave `… WHERE EXISTS(…) AND nope = 1`
		// falling through to a generic 0AF00. The column genuinely doesn't exist for
		// the text fallback either, so surfacing it here never masks a fallback-resolvable
		// WHERE. mapPredicateWalkError maps these to the same 42703/42702 the SELECT path gives.
		var colNF *semantic.ColumnNotFoundError
		var ambig *semantic.AmbiguousColumnError
		var srcNF *semantic.SourceNotFoundError
		if errors.As(err, &colNF) || errors.As(err, &ambig) || errors.As(err, &srcNF) {
			return false, mapPredicateWalkError(err)
		}
		// A specific carried SQLSTATE from a subquery PLAN failure (an EXISTS inner
		// build, or a scalar build rejecting LIMIT > 1 / DISTINCT / a window)
		// takes precedence over the text fallback. Gate on the WHERE actually
		// containing either subquery atom: those are precisely the shapes the
		// plain text builder cannot plan, so the catalog path's error is
		// authoritative. For a plain comparison WHERE the text fallback may still
		// succeed, and swallowing a generic api error here preserves that.
		var apiErr *api.Error
		hasSubqueryAtom := expr.ContainsExistsAtom(whereExpr.Expression()) ||
			expr.ContainsSubqueryAtom(whereExpr.Expression())
		if errors.As(err, &apiErr) && hasSubqueryAtom {
			return false, apiErr
		}
		return false, nil
	}
	// The SELECT lowering hides its materialized scalar slot with an outer-only
	// projection. DML consumes record identity directly, so that projection is
	// not yet proven transparent to UPDATE/DELETE record+primary-key plumbing.
	// Decline before attaching the carrier; never let DML fall into an unbound
	// alias or a reshaped-record write.
	if len(existsPlanner.correlatedScalarSubqueries) > 0 {
		return false, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar subquery in a DML WHERE predicate is not supported")
	}
	if installErr := installFirstWherePredicate(op, predicates.SimplifyPredicateValues(walked)); installErr != nil {
		return false, installErr
	}
	if len(existsPlanner.subqueries) > 0 {
		if !upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries) {
			return false, api.NewError(api.ErrCodeUnsupportedQuery,
				"WHERE subqueries could not be installed on the logical plan")
		}
	}
	if len(existsPlanner.scalarSubqueries) > 0 {
		if !upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries) {
			return false, api.NewError(api.ErrCodeUnsupportedQuery,
				"WHERE scalar subqueries could not be installed on the logical plan")
		}
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
	// Validate the schema qualifier (if any) BEFORE the SET-column / WHERE classification: a
	// bad qualifier's 42F00 must take precedence over a 42703/42F01 when the bare table
	// exists in the active schema. Valid/absent qualifier → strips to the bare name.
	resolved, qErr := functions.ResolveQualifiedTableName(tableName, schemaName)
	if qErr != nil {
		return nil, qErr
	}
	tableName = resolved
	bare := bareTableName(tableName)

	// Resolve the target type case-insensitively for the SET-column check below. (Missing
	// target tables are rejected with 42F01 in planDML after resolveQualifiedTableNames;
	// rt stays nil here for a missing/qualified target and the SET check is then skipped.)
	rt := recordTypeCI(md, bare)

	// Validate each SET target column exists in the table, mirroring INSERT's
	// build-time check (insert_cascades.go). Without this, an UPDATE that assigns a
	// nonexistent column reaches the executor and surfaces a LEAKY raw error
	// ("executor: update field %q not found in descriptor", no SQLSTATE) instead of
	// a clean 42703 — the same condition INSERT and SELECT already report as 42703.
	// The check is case-insensitive (EqualFold over the descriptor field names) so it
	// is exactly as permissive as the executor's lookup (ByName(lower) →
	// fieldByNameFold) and never rejects a column the executor would accept. rt may be
	// nil for a schema-qualified target (validated downstream); skip the check then.
	if rt != nil && rt.Descriptor != nil {
		fields := rt.Descriptor.Fields()
		for _, set := range updOp.Sets {
			found := false
			for i := 0; i < fields.Len(); i++ {
				if strings.EqualFold(string(fields.Get(i).Name()), set.Column) {
					found = true
					break
				}
			}
			if !found {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in table %q", set.Column, bare)
			}
		}
	}

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
			pred, ok, werr := buildWherePredicateForTableE(md, bare, bare, w)
			if werr != nil {
				// e.g. 42804 from a bare non-boolean UPDATE WHERE — surface it.
				return nil, werr
			}
			if ok {
				if installErr := installFirstWherePredicate(op, pred); installErr != nil {
					return nil, installErr
				}
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
	alignInsertSelectColumns(insertOp, md)
	return insertOp, nil
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
// Every SQL aggregate source now has one final LogicalProject in SELECT-list
// order. An aggregate-derived slot appears in one of two shapes:
//   - a COMPUTED expression that CONTAINS an aggregate (e.g. AVG(v)+1) —
//     flagged by LogicalProject.AggregateSlots (provenance, captured pre-rewrite)
//     and reliably typed via the value's Type() (the aggregate reference carries
//     its result type, B′; ArithmeticValue propagates it). Provenance, NOT
//     type-presence: plain columns are concrete-typed too (ResolveIdentifier).
//   - a DIRECT aggregate (e.g. SELECT AVG(v)) — its exact projected Value carries
//     the native aggregate result type. The canonical-name lookup below remains
//     a defensive fallback for hand-built/legacy logical trees.
//
// Plain-column narrowing (LONG→INT, DOUBLE-col→INT) is NOT checked here — it
// stays deferred to the runtime converter, pending the Java end-state
// (PromoteValue projection nodes) that dissolves this guard. INSERT … SELECT
// with an explicit column list is rejected upstream, so the projection maps
// positionally onto the target record's fields.
//
// Scope: covers every SQL-built aggregate and ordinary projected source. A
// direct hand-built LogicalAggregate has no public SELECT contract and is
// conservatively outside this SQL-layer check.
func checkInsertSelectPromotable(insertOp *logical.LogicalInsert, md *recordlayer.RecordMetaData) error {
	proj := findProjection(insertOp.Source)
	if proj == nil {
		return nil
	}
	rt := md.GetRecordType(bareTableName(insertOp.Table))
	if rt == nil {
		return nil
	}
	// Canonical aggregate output name → reliable result type for a legacy or
	// hand-built Project whose direct aggregate Value was not populated.
	aggTypes := map[string]values.Type{}
	if agg := findAggregate(insertOp.Source); agg != nil {
		for j, call := range agg.Calls {
			var operand values.Value
			if j < len(agg.AggregateOperands) {
				operand = agg.AggregateOperands[j]
			}
			if t := aggResultTypeFromFunc(call.Func, operand); t != nil {
				aggTypes[strings.ToUpper(call.CanonicalName())] = t
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
// output function and its resolved operand. It is the defensive fallback for a
// legacy/hand-built aggregate projection without a ProjectedValue. AVG→DOUBLE
// and COUNT→LONG are function-determined; SUM/MIN/MAX
// inherit the operand type. The function prefix is read off the *internal*
// canonical name (the contract the executor's aggResultName also relies on), not
// user SQL text. Mirrors AggregateValue.Type() / Java's per-operator resultTypeCode
// — keep the two in sync until the PromoteValue follow-up (RFC-083) dissolves this
// function.
// Divergences from the shared javaAggregateResultCode table, deliberate
// for METADATA (this function feeds ResultSet column types, not the
// plan-time gates): COUNT reports NOT NULL (the metadata contract), and
// an unknown-operand SUM/MIN/MAX falls back to NullableLong rather than
// Unknown so the column still carries a displayable type.
func aggResultTypeFromFunc(fn string, operand values.Value) values.Type {
	switch fn {
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
	outerCTEOnScopes map[string]semantic.ScopeSource,
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
	// BOTH outer maps must be empty to short-circuit: a join/unnest-bodied
	// outer CTE lives ONLY in outerCTEOnScopes (never cteScopes), and dropping
	// it here sent a subquery's `... FROM c JOIN t ON ...` into the scope-less
	// variant where the ON silently dropped (cross-product rows — the
	// review-proven scalar-subquery path hole). RFC-142 (P2b).
	if len(outerCTEScopes) == 0 && len(outerCTEOnScopes) == 0 && schemaName == defaultEmbeddedSchema {
		return buildLogicalPlanForQueryWithCatalog(q, md)
	}
	if q == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()
	preState := map[string]cteScopePreState{}

	// Start with outer CTE scopes, then overlay any inner CTE defs
	// (inner shadows outer on name collision). cteOnScopes mirrors the
	// overlay for the ON-resolution-only sources (see buildCTEOnOnlySource).
	cteScopes := make(map[string]semantic.ScopeSource, len(outerCTEScopes))
	for k, v := range outerCTEScopes {
		cteScopes[k] = v
	}
	cteOnScopes := make(map[string]semantic.ScopeSource, len(outerCTEOnScopes))
	for k, v := range outerCTEOnScopes {
		cteOnScopes[k] = v
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
			// Snapshot the PRE-REGISTRATION state (outer binding or absent)
			// before any write — the body build swaps back to it
			// (buildCTEBodySelfHidden: self-invisible, outer visible).
			if _, seen := preState[upper]; !seen {
				sv, sh := cteScopes[upper]
				ov, oh := cteOnScopes[upper]
				preState[upper] = cteScopePreState{scopeVal: sv, scopeHad: sh, onVal: ov, onHad: oh}
			}
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
				delete(cteOnScopes, upper) // inner derivable shadows an outer ON-only entry
			} else {
				// Declared but not globally derivable (join/unnest body): the
				// ON-only registration keeps an enclosing explicit join's ON
				// resolvable — or LOUDLY dropped (marker) — never silent.
				// The registration-time derivation runs BEFORE the shadow
				// delete below: a body leg naming the outer same-name
				// correctly classifies against the OUTER binding (which is
				// what the body's reference means, pre-state scoping).
				if regErr := registerCTEOnOnlyScope(cteOnScopes, upper, nq.Query(), nq.GetColumnAliases(), md, schemaName, cteScopes); regErr != nil {
					return nil, regErr
				}
				// The mirror of the derivable arm's shadow delete: an inner
				// ON-ONLY registration must EVICT a same-named OUTER
				// derivable entry, or this level's MAIN query resolves the
				// inner CTE's reads against the STALE OUTER schema
				// (review-caught: MAX over a stale column returned the wrong
				// generation; the pre-registration snapshot keeps the outer
				// visible for the BODY build only). Post-evict the inner is
				// ON-only (in cteOnScopes, not cteScopes) — its main-query
				// reads join the SAME booked ON-only READ class as a
				// NON-shadow ON-only CTE: buildSelectScope's nil-resolver
				// leniency (load-bearing for the enclosed comma-FROM reads,
				// Q1-Q5) resolves a valid enclosed read via the executor
				// merge fabrication and lands an unresolvable/solo read in
				// the booked silent class (Q51/Q52 flip-sentinels). An
				// earlier attempt INSTALLED the inner's ON-only derived
				// schema into cteScopes to make the exotic coinciding
				// scalar-subquery read answer — but that schema is
				// ON-resolution-only and LOSSY (buildCTEOnOnlySource uses
				// NewUnquoted and permits duplicate output names), so
				// promoting it to general reads silently mis-resolved
				// quoted-alias and duplicate-name bodies (review-caught).
				// The sound answer for the coinciding read is
				// cteOnScopes-aware read resolution (booked); until then the
				// whole class is uniformly the booked silent state, never a
				// lossy-schema-specific wrong value. NO-shadow adds nothing
				// to cteScopes — the flatten-evasion gate pin's clean
				// decline holds.
				delete(cteScopes, upper)
			}
		}
	}

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
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
	// No clause = ANY (the planner picks); an explicit level_order pins
	// the level union (the clause's only remaining alternative).
	traversalOrder := logical.TraversalAnyOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		traversalOrder = logical.TraversalLevelOrder
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
			// Self-invisible body build (buildCTEBodySelfHidden): the
			// registration loop completed BEFORE this build, so the maps
			// carry the CTE's own entry — CTE-first resolution would
			// resolve the body against its own output schema.
			body, err = buildCTEBodySelfHidden(cteScopes, cteOnScopes, strings.ToUpper(name), preState, recursive, func() (logical.LogicalOperator, error) {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
			})
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
	preState := map[string]cteScopePreState{}

	// Pre-scan CTE definitions to extract column schemas. Process in
	// declaration order so CTE B can reference CTE A's derived schema.
	// This is the TOP-LEVEL (no external scope) variant — reached only from the
	// EXPLAIN-only generators and the WithCTECatalog default-schema short-circuit,
	// so it uses the default schema for the schema-qualified-table demotion. A
	// non-default session schema flows through the WithCTECatalog path instead.
	schemaName := defaultEmbeddedSchema
	var cteScopes map[string]semantic.ScopeSource
	var cteOnScopes map[string]semantic.ScopeSource
	if ctesCtx != nil {
		cteScopes = make(map[string]semantic.ScopeSource)
		cteOnScopes = make(map[string]semantic.ScopeSource)
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if _, exists := cteScopes[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			// An ON-only registration is a DECLARED name too — without this
			// arm a join-bodied duplicate (never in cteScopes) silently
			// last-wins here while the visitor and the WithCTECatalog loop
			// both error (the review-caught third-loop copy of the same hole;
			// reachable live via a subquery-nested WITH through the
			// empty-scope short-circuit).
			if _, exists := cteOnScopes[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			// Pre-registration snapshot (always ABSENT on this route — the
			// maps are fresh — kept uniform with the WithCTECatalog loop so
			// the shared wrap-loop block reads identically).
			if _, seen := preState[upper]; !seen {
				sv, sh := cteScopes[upper]
				ov, oh := cteOnScopes[upper]
				preState[upper] = cteScopePreState{scopeVal: sv, scopeHad: sh, onVal: ov, onHad: oh}
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
			} else {
				// Declared but not globally derivable (join/unnest body): the
				// ON-only registration keeps an enclosing explicit join's ON
				// resolvable — or LOUDLY dropped (marker) — never silent.
				// The registration-time derivation runs BEFORE the shadow
				// delete below: a body leg naming the outer same-name
				// correctly classifies against the OUTER binding (which is
				// what the body's reference means, pre-state scoping).
				if regErr := registerCTEOnOnlyScope(cteOnScopes, upper, nq.Query(), nq.GetColumnAliases(), md, schemaName, cteScopes); regErr != nil {
					return nil, regErr
				}
				// The mirror of the derivable arm's shadow delete: an inner
				// ON-ONLY registration must EVICT a same-named OUTER
				// derivable entry, or this level's MAIN query resolves the
				// inner CTE's reads against the STALE OUTER schema
				// (review-caught: MAX over a stale column returned the wrong
				// generation; the pre-registration snapshot keeps the outer
				// visible for the BODY build only). Post-evict the inner is
				// ON-only (in cteOnScopes, not cteScopes) — its main-query
				// reads join the SAME booked ON-only READ class as a
				// NON-shadow ON-only CTE: buildSelectScope's nil-resolver
				// leniency (load-bearing for the enclosed comma-FROM reads,
				// Q1-Q5) resolves a valid enclosed read via the executor
				// merge fabrication and lands an unresolvable/solo read in
				// the booked silent class (Q51/Q52 flip-sentinels). An
				// earlier attempt INSTALLED the inner's ON-only derived
				// schema into cteScopes to make the exotic coinciding
				// scalar-subquery read answer — but that schema is
				// ON-resolution-only and LOSSY (buildCTEOnOnlySource uses
				// NewUnquoted and permits duplicate output names), so
				// promoting it to general reads silently mis-resolved
				// quoted-alias and duplicate-name bodies (review-caught).
				// The sound answer for the coinciding read is
				// cteOnScopes-aware read resolution (booked); until then the
				// whole class is uniformly the booked silent state, never a
				// lossy-schema-specific wrong value. NO-shadow adds nothing
				// to cteScopes — the flatten-evasion gate pin's clean
				// decline holds.
				delete(cteScopes, upper)
			}
		}
	}

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
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
	// No clause = ANY (the planner picks); an explicit level_order pins
	// the level union (the clause's only remaining alternative).
	traversalOrder := logical.TraversalAnyOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		traversalOrder = logical.TraversalLevelOrder
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
			// Self-invisible body build (buildCTEBodySelfHidden): the
			// registration loop completed BEFORE this build, so the maps
			// carry the CTE's own entry — CTE-first resolution would
			// resolve the body against its own output schema.
			body, err = buildCTEBodySelfHidden(cteScopes, cteOnScopes, strings.ToUpper(name), preState, recursive, func() (logical.LogicalOperator, error) {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
			})
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
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
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
	cteOnScopes map[string]semantic.ScopeSource,
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
	// BOTH maps must be empty — a join/unnest-bodied outer CTE lives ONLY in
	// cteOnScopes, and dropping it here silently dropped the enclosing join's
	// ON on the subquery build path (cross-product rows).
	if len(cteScopes) == 0 && len(cteOnScopes) == 0 && schemaName == defaultEmbeddedSchema {
		return buildLogicalPlanForQueryBodyWithCatalog(body, md)
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		// Parenthesized UNION branch — recurse into the inner query body so
		// the branch's LIMIT/clauses survive (RFC-128 §4.7); see the
		// non-CTE variant above.
		if paren, ok := b.QueryTerm().(*antlrgen.ParenthesisQueryContext); ok {
			if inner := paren.Query(); inner != nil {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
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
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Unsupported operator "+fn)
		}
		if err := validateQualifiedStarSources(sq, md); err != nil {
			return nil, err
		}
		return buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, cteScopes, cteOnScopes)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCTECatalog(b, md, schemaName, cteScopes, cteOnScopes, false)
	}
	return nil, nil
}

func buildLogicalPlanForUnionWithCTECatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
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
	left, err := buildLogicalPlanForQueryBodyWithCTECatalog(setQ.GetLeft(), md, schemaName, cteScopes, cteOnScopes)
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
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, schemaName, cteScopes, cteOnScopes)
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
	cteOnScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, unionLiftedClauses, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	qtd, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, schemaName, cteScopes, cteOnScopes)
		return op, unionLiftedClauses{limit: -1}, err
	}
	simpleTable, ok := qtd.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, schemaName, cteScopes, cteOnScopes)
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
			// Carry the SELECT-list position for a POSITIONAL key: the ordinal
			// binds to the union OUTPUT slot (a Go extension — live-probed Java
			// 4.12.11.0 has NO positional ORDER BY at all, and attaches a
			// trailing ORDER BY to the RIGHT LEG ONLY, not the combined union;
			// Go deliberately implements the SQL-standard combined-result
			// semantics, see union_columns.yaml). Without Pos the key's TEXT
			// resolves against the RIGHT leg's spelling and then fails the
			// LEFT-leg name validation when the legs spell the position
			// differently (`SELECT '2024', … UNION ALL SELECT '2025', …
			// ORDER BY 1`). RFC-180.
			lifted.sortKeys = append(lifted.sortKeys, logical.SortKey{Expr: e, Pos: ob.pos, Dir: dir, NullsFirst: nullsFirst})
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
		return nil, lifted, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
	}
	if err := validateQualifiedStarSources(sq, md); err != nil {
		return nil, lifted, err
	}
	op, err := buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, cteScopes, cteOnScopes)
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
func upgradeSortKeyValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	sort := findSort(op)
	if sort == nil || len(sort.Keys) == 0 {
		return nil
	}

	// Build alias→column mapping from projections.
	aliasToCol := make(map[string]string)
	aliasToIdx := make(map[string]int)
	if sq.projAliases != nil && sq.projCols != nil {
		for i, a := range sq.projAliases {
			if a != "" && i < len(sq.projCols) {
				aliasToCol[strings.ToUpper(a)] = sq.projCols[i].name
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
	if agg != nil && len(agg.GroupKeys) > 0 {
		groupKeyExplainMap = make(map[string]string)
		for _, gk := range agg.GroupKeys {
			gkv := gk.Value
			if gkv == nil {
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
			groupKeyExplainMap[strings.ToUpper(gk.Display)] = aggregateGroupKeyOutputName(gkv)
		}
	}
	// colToIdx maps a NON-aliased select item's canonical text to its select-list
	// position — the correspondence `ORDER BY <n>` (positional, whose key Expr is
	// the item's rendered name) and a text-form computed key (`ORDER BY col1 +
	// 10`) resolve through. Copying the EXACT projected Value (pointer) lets the
	// translator's pull-up bake the key to its OUTPUT ordinal: the key
	// must carry a plan-time ordinal, since a runtime name read silently
	// no-op-sorts when the rendered text and the output column spelling
	// diverge, e.g. a baked computed column `(COL1#0 + 10)` vs the source text
	// `col1 + 10`. First-match on duplicate renderings — the duplicates are the
	// same expression, so the sort order is identical either way.
	colToIdx := make(map[string]int, len(sq.projCols))
	for i, c := range sq.projCols {
		key := strings.ToUpper(c.name)
		if _, dup := colToIdx[key]; !dup {
			colToIdx[key] = i
		}
	}
	// GROUPED-select correspondence (Java LogicalOperator.generateSelect): for
	// an aggregate query the SELECT list lives in aggCols, not projCols, so the
	// maps above are empty — but the reshaping POST-AGGREGATE projection carries
	// the items' rendered texts, aliases, and resolved output Values. Map them
	// to their output slots so a computed ORDER BY key (`ORDER BY a + b` over
	// `SELECT a + b, MAX(c) … GROUP BY a, b`) copies the EXACT projected Value
	// pointer and the translator's pull-up (pullUpToOutputField) bakes the key
	// to the projection OUTPUT ordinal. Without this the key resolves against
	// the FROM scope (base-row ordinals) and the enforcer sort ABOVE the
	// projection reads a foreign slot — silent mis-sort when the ordinal lands
	// in range, an ordinal-model malformed-plan error when it doesn't.
	// First-match semantics mirror colToIdx; existing entries win.
	if proj != nil {
		for i, ptext := range proj.Projections {
			key := strings.ToUpper(ptext)
			if _, dup := colToIdx[key]; !dup {
				colToIdx[key] = i
			}
		}
		for i, alias := range proj.Aliases {
			if alias == "" {
				continue
			}
			key := strings.ToUpper(alias)
			if _, dup := aliasToIdx[key]; !dup {
				aliasToIdx[key] = i
			}
		}
	}
	// POSITIONAL keys first, by ORDINAL — never by text. A positional key
	// is an ordinal into THIS select's output list; when the select's own
	// projection sits ABOVE the sort (the plain-select shape), the ordinal
	// resolves to that projection's item: the resolved item Value when the
	// catalog pass populated it (typed — immune to items whose rendered
	// texts or aliases collide), the item's underlying text otherwise. Pos
	// is CLEARED here so the translator can never bake the ordinal into
	// whatever projection roots the sort's INPUT (a derived source's
	// layout). When the projection is NOT an ancestor of the sort (the
	// aggregate reshaping strip below the sort, or a union), Pos survives
	// untouched — those inputs ARE select-list carriers and the
	// translator's Pos bake against them is the correct binding.
	positionalKey := make([]bool, len(sort.Keys))
	for i := range sort.Keys {
		positionalKey[i] = sort.Keys[i].Pos > 0
	}
	if proj != nil && sortOwnedBySelect(proj, sort) {
		for i := range sort.Keys {
			pos := sort.Keys[i].Pos
			if pos < 1 || pos > len(proj.Projections) {
				continue
			}
			if proj.ProjectedValues != nil && pos-1 < len(proj.ProjectedValues) && proj.ProjectedValues[pos-1] != nil {
				sort.Keys[i].Value = proj.ProjectedValues[pos-1]
				if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
					sort.Keys[i].AggregateOutputValueExact = true
				}
			} else {
				sort.Keys[i].Expr = proj.Projections[pos-1]
			}
			sort.Keys[i].Pos = 0
		}
	}

	for i := range sort.Keys {
		upper := strings.ToUpper(sort.Keys[i].Expr)
		// Output aliases bind BARE one-segment identifiers only
		// (SortKey.BareRef): a qualified key's Expr is already
		// qualifier-stripped and an aggregate key's Expr is its canonical
		// rendering, so without the flag `ORDER BY d.x` / `ORDER BY
		// SUM(s.score)` would bind a same-spelled SELECT alias and
		// silently mis-sort.
		if real, ok := aliasToCol[upper]; ok && sort.Keys[i].BareRef {
			sort.Keys[i].Expr = real
		}
		if idx, ok := aliasToIdx[upper]; ok && proj != nil && sort.Keys[i].BareRef {
			if idx < len(proj.ProjectedValues) && proj.ProjectedValues[idx] != nil {
				sort.Keys[i].Value = proj.ProjectedValues[idx]
				if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
					sort.Keys[i].AggregateOutputValueExact = true
				}
			}
		} else if idx, ok := colToIdx[upper]; ok && proj != nil && sort.Keys[i].Value == nil {
			if idx < len(proj.ProjectedValues) && proj.ProjectedValues[idx] != nil {
				sort.Keys[i].Value = proj.ProjectedValues[idx]
				if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
					sort.Keys[i].AggregateOutputValueExact = true
				}
			}
		}
		if groupKeyExplainMap != nil && !sort.Keys[i].AggregateOutputValueExact {
			if explain, ok := groupKeyExplainMap[strings.ToUpper(sort.Keys[i].Expr)]; ok {
				// The sort reads the row ABOVE the outermost operator. Directly
				// over the aggregate that is the AGGREGATE output name; with a
				// visible PROJECTION in between (mixed aliased/uneliased select
				// list) the sort key must read the PROJECTION's OUTPUT column
				// for the same underlying group key — the aggregate-output bare
				// name is not a column of the projected row (a lazy key naming
				// it is loud at runtime under the ordinal model; the retired
				// name read no-op-sorted silently).
				//
				// DEFERRED-strip inversion: when the reshaping projection sits
				// ABOVE the sort (a group key read only by ORDER BY defers the
				// strip), the sort reads the AGGREGATE row — redirecting the
				// key to the projection ALIAS would read a column that exists
				// only above (loud failure), or silently bind a same-named
				// hidden key. Redirect only when the sort is above the
				// projection.
				if proj != nil && !operatorContains(proj, sort) {
					for pi, ptext := range proj.Projections {
						if !strings.EqualFold(ptext, sort.Keys[i].Expr) {
							continue
						}
						if pi < len(proj.Aliases) && proj.Aliases[pi] != "" {
							explain = strings.ToUpper(proj.Aliases[pi])
						} else {
							explain = strings.ToUpper(ptext)
						}
						break
					}
				}
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
			return nil
		}
	}
	exactAggregateBoundary := agg != nil && proj != nil &&
		len(proj.AggregateOutputOrdinals) == len(proj.Projections)
	for i := range sort.Keys {
		// Positional keys were already bound by ordinal above (or retain Pos
		// for the translator). Never walk their raw parse node: it is the
		// numeric literal itself and would overwrite ORDER BY 2 with constant
		// 2. Likewise, a prior alias/project/group-key mapping is already the
		// authoritative SQL output binding.
		if positionalKey[i] || sort.Keys[i].HasAggregateOutputOrdinal ||
			sort.Keys[i].AggregateOutputValueExact ||
			(!exactAggregateBoundary && sort.Keys[i].Value != nil) {
			continue
		}
		ob := findOrderByForKey(sq, sort.Keys[i].Expr)
		if ob == nil || ob.rawExpr == nil {
			continue
		}
		// rawExpr is authoritative even when the parser could also render a
		// colName. Aggregate calls such as MAX(x.v) are name-classified, but
		// their qualified operand still has to resolve structurally and bind to
		// the producer-native aggregate slot. Skipping name-classified items
		// leaves a qualified spelling (MAX(X.V)) that cannot match the private
		// aggregate label (MAX(V)), causing either a malformed plan or a
		// name-based misbind. Plain column references are safe here too: in an
		// aggregate query the structural binder maps a group key to its native
		// key slot; outside one the normal resolver Value is retained.
		v, err := resolver.WalkExpression(ob.rawExpr)
		if err != nil {
			if exactAggregateBoundary {
				if mapped := mapPredicateWalkError(err); mapped != nil {
					return mapped
				}
				return api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"ORDER BY expression could not be resolved against the exact aggregate output: %v", err)
			}
			continue
		}
		if exactAggregateBoundary {
			v, err = bindPostAggregateValueToNativeOrdinals(v, agg)
			if err != nil {
				return err
			}
			sort.Keys[i].AggregateOutputValueExact = true
		} else {
			v = rewriteAggregateValuesInTree(v, nil)
		}
		// A bare unnest sort key (`ORDER BY v`) resolves through the unnest's
		// Shadowing scope source to a qualified FieldValue over the unnest
		// correlation, which the P2a path already qualifies via
		// qualifyShadowedSortKeys. A COMPUTED key (`v + 0`) wraps that FieldValue in
		// an arithmetic Value; the qualification is intrinsic to the resolved tree
		// (the FieldValue carries its Child correlation), so the executor's ValueExpr
		// evaluates the qualified reference per row and the sort sorts for real.
		sort.Keys[i].Value = v
	}
	return nil
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
	return strings.ToUpper(values.ColumnNameValue(gkv))
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
	if v == nil || agg == nil || len(agg.GroupKeys) == 0 {
		return v
	}
	return values.MapFieldValues(v, func(fv *values.FieldValue) values.Value {
		// A node that already carries a RECORDED slot is addressed against the
		// aggregate's OUTPUT row and is not a candidate for anything here. The
		// guard is load-bearing, not defensive: by the time this runs the tree's
		// aggregate references have already become FieldValues carrying their
		// output-row ordinals (rewriteAggregateValue), and the group keys carry
		// SOURCE-relative ordinals. Letting the two meet in an ordinal comparison
		// matches across two different layouts — measured, it rewrote the SUM(v)
		// in `HAVING g > SUM(v)` into a reference to the group key G whenever
		// their ordinals happened to coincide, after which the predicate looked
		// key-only and PushFilterThroughGroupByRule pushed it onto the raw scan.
		// The domain token on FieldPath exists to fail closed on exactly this
		// comparison; neither side carries one yet, so the provenance bit does
		// the job.
		if fv.Resolved != nil && fv.Resolved.FrontierPinned {
			return fv
		}
		for i, gk := range agg.GroupKeys {
			if gk.Value == nil {
				continue
			}
			qfv, ok := gk.Value.(*values.FieldValue)
			if !ok {
				continue
			}
			// Two ways a post-aggregate reference is provably THIS group key. A
			// QUALIFIED key carries the V.V mismatch and matches on structural
			// equality with the reference as resolved. A BARE key matches through
			// the SAME structural matcher the exact-boundary binder uses
			// (bindPostAggregateValueToNativeOrdinals) — semantic equality, or the
			// one sanctioned representation difference between a qualified read and
			// a de-qualified single-source key. Neither arm consults a name.
			matched := qfv.Child != nil && values.ValuesStructurallyEqual(fv, qfv)
			if !matched && qfv.Child == nil {
				matched = values.SemanticEqualsUnderAliasMap(fv, gk.Value, values.AliasMap{}) ||
					fieldValueMatchesAggregateGroupKey(fv, gk.Value, agg)
			}
			if matched {
				// The SLOT is recorded HERE, at the composition that decides it:
				// `i` is this key's index in agg.GroupKeys, and the aggregate
				// output row is [group keys in this order..., aggregates...]
				// (GroupByOutputColumnNames / translateAggregate's index-parallel
				// groupKeys build). Java pulls a post-aggregate reference up by
				// exactly this loop index (CompensateRecordConstructorRule.java:92
				// over Column.unnamedOf columns) rather than by any name.
				//
				// Emitting the BARE NAME alone — which is what this did — throws
				// that index away and leaves the downstream binder
				// (groupByOutputBaker) to RECOVER the slot from a map keyed by the
				// rendered output name. When two group keys share a leaf, that map
				// is last-wins and the recovery lands on the WRONG key: measured as
				// wrong rows for `GROUP BY o.k, i.k HAVING o.k + COUNT(*) > 2`,
				// pinned by TestFDB_GroupBySameLeafKeys_HavingRereadBindsItsOwnSlot.
				// The name is kept only as the display label.
				//
				// PINNED because the ordinal is FINAL against the executor's
				// assembled aggregate output row, not relative to any source's
				// declared column order — which is also the signal the binder reads
				// to leave this node alone instead of re-keying it.
				name := aggregateGroupKeyOutputName(qfv)
				return &values.FieldValue{
					Field:    name,
					Typ:      fv.Typ,
					Resolved: values.NewFieldPathOfSingle(name, i, true),
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
	if pred == nil || agg == nil || len(agg.GroupKeys) == 0 {
		return pred
	}
	if havingPredicatePushesBelowAggregate(pred, agg) {
		return pred // stays qualified; PushFilterThroughGroupByRule pushes it pre-aggregate
	}
	return rebasePostAggregateGroupKeyPredicate(pred, agg)
}

// havingPredicatePushesBelowAggregate asks the Cascades rule's OWN decider
// whether this HAVING predicate will be pushed below the GroupBy. The HAVING
// predicate is handed to the translator as ONE list entry, so the rule never
// splits a compound — the decision is binary at the top level. RFC-142.
//
// It used to be a hand-rolled MIRROR of that decider, matching a grouping key by
// BARE LEAF NAME, and the comment claimed the two "cannot drift". They had:
// PushFilterThroughGroupByRule keys its group-key set by the canonical ACCESSOR
// PATH (so a nested addr.city does not answer to a top-level city) and also
// requires the COMPARAND to reference only keys, neither of which the mirror
// did. A disagreement is not a lost optimization here — it decides which ROW a
// reference is bound against, and the two sides losing to each other are a
// pre-aggregate binding evaluated on the aggregate output, or the reverse.
// One decider, called from both places, is the only form that cannot drift.
func havingPredicatePushesBelowAggregate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) bool {
	keys := make([]values.Value, 0, len(agg.GroupKeys))
	for _, gk := range agg.GroupKeys {
		keys = append(keys, gk.Value)
	}
	return cascades.PredicatePushesBelowGroupBy(pred, keys)
}

// rebasePostAggregateGroupKeyPredicate applies rebasePostAggregateGroupKeyValue to
// every Value operand of a post-aggregate predicate tree. Mirrors
// rewriteAggregateRefsInPredicate's tree walk so a grouped-unnest group-key
// reference reads the bare aggregate-output column, not the qualified pre-aggregate
// `V.V`. RFC-142.
func rebasePostAggregateGroupKeyPredicate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) predicates.QueryPredicate {
	if pred == nil || agg == nil || len(agg.GroupKeys) == 0 {
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
	// Keep the derived alias on the logical tree — the same LogicalCTE
	// wrapper the visitor path uses (the tree's one alias carrier for
	// derived tables). A bare innerOp loses the alias: sourceAlias() walks
	// to the BASE table and a correlated EXISTS on the derived alias binds
	// the outer row under the wrong name (`SELECT e.*` routes here via the
	// qualified-star rebuild — the visitor-path fix's rebuild-path twin).
	var op logical.LogicalOperator = logical.NewCTE(sq.tableName, innerOp,
		logical.NewScan(sq.tableName, ""), false)
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
func expandQualifiedStars(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	if sq == nil || sq.projCols == nil || sq.projStarQualifiers == nil {
		return nil
	}
	hasQualifiedStar := false
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			hasQualifiedStar = true
			break
		}
	}
	if !hasQualifiedStar {
		return nil
	}

	// Build a map of source alias → table columns, CTE-FIRST (execution's
	// shadowing order): a preceding CTE shadowing a catalog table supplies
	// ITS columns — md-first expanded `p.*` over a shadowed source against
	// the BASE table's schema, minting columns the CTE row does not carry
	// (42703 downstream on a valid query). A tombstoned CTE (nil Table)
	// leaves the sentinel: downstream declines loud.
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	sourceColumns := make(map[string][]string)
	derivedAliases := make(map[string]struct{})
	addSource := func(tableName, alias string) {
		if src, found := cteScopes[strings.ToUpper(tableName)]; found {
			if src.Table == nil {
				return
			}
			cteCols := src.Table.Columns()
			cols := make([]string, len(cteCols))
			for i, c := range cteCols {
				cols[i] = strings.ToUpper(c.Id.Name())
			}
			key := strings.ToUpper(alias)
			if key == "" {
				key = strings.ToUpper(tableName)
			}
			sourceColumns[key] = cols
			return
		}
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
		// A DERIVED primary source registers NOTHING: sq.tableName is its
		// range alias, not a relation — a CTE or base table sharing that
		// name would supply the WRONG columns (star over the derived row
		// silently projected the unrelated relation's schema). The sentinel
		// stays; downstream resolves or declines loud.
		if sq.derivedQuery == nil {
			addSource(sq.tableName, alias)
		} else {
			derivedAliases[strings.ToUpper(alias)] = struct{}{}
		}
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
		// Derived join legs: same bypass as the derived primary above.
		if j.derivedQuery == nil {
			addSource(j.tableName, alias)
		} else {
			derivedAliases[strings.ToUpper(alias)] = struct{}{}
		}
	}

	// A qualified star BOUND TO A DERIVED source rejects PLAN-TIME: no
	// relation can speak for the derived alias here, and leaving the
	// sentinel produced a plan that died at row time with a raw
	// ordinal-resolution error on a valid-shaped query.
	for _, q := range sq.projStarQualifiers {
		if q == "" {
			continue
		}
		if _, isDerived := derivedAliases[strings.ToUpper(q)]; isDerived {
			return api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"qualified star over derived table %q is not supported", q)
		}
	}

	var newCols []projCol
	var newAliases, newQuals []string
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
			// Star expansion mints the qualified reference structurally —
			// the segments are known here, never re-derived from the name.
			newCols = append(newCols, projCol{name: qual + "." + c, bare: c, qualifier: qual, qualified: true})
			newAliases = append(newAliases, "")
			newExprs = append(newExprs, nil)
			newQuals = append(newQuals, "")
		}
	}
	sq.projCols = newCols
	sq.projAliases = newAliases
	sq.projExprs = newExprs
	sq.projStarQualifiers = newQuals
	return nil
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
		cols := make([]projCol, len(srcCols))
		for k, c := range srcCols {
			bare := strings.ToUpper(c.Id.Name())
			cols[k] = projCol{name: qual + "." + bare, bare: bare, qualifier: qual, qualified: true}
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
	cols := make([]projCol, fields.Len())
	aliases := make([]string, fields.Len())
	exprs := make([]antlrgen.IExpressionContext, fields.Len())
	quals := make([]string, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		bare := strings.ToUpper(string(fields.Get(i).Name()))
		cols[i] = projCol{name: qual + "." + bare, bare: bare, qualifier: qual, qualified: true}
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
		// The bare form comes from the RESOLVED channel: a childless
		// FieldValue's Field IS the bare column (the same structural truth
		// the upgrade passes bind), never a last-dot split of the rendering
		// — a delimited identifier containing a literal dot is one name.
		if i < len(leftProj.ProjectedValues) {
			if fv, ok := leftProj.ProjectedValues[i].(*values.FieldValue); ok && fv.Child == nil {
				leftNames[strings.ToUpper(fv.Field)] = true
			}
		}
		if i < len(leftProj.Aliases) && leftProj.Aliases[i] != "" {
			leftNames[strings.ToUpper(leftProj.Aliases[i])] = true
		}
	}
	for _, k := range sort.Keys {
		if k.Expr == "" {
			continue
		}
		// A POSITIONAL key binds to the union OUTPUT slot by ordinal — its
		// Expr carries the RIGHT leg's rendering of that slot (where the
		// parser resolved it), which legitimately differs from the left
		// leg's spelling. In-range is guaranteed upstream
		// (resolveSelectListPosition errors out-of-range) plus the union's
		// equal-column-count validation. RFC-180.
		if k.Pos > 0 {
			continue
		}
		upper := strings.ToUpper(k.Expr)
		bareName := upper
		if k.Bare != "" {
			// Structured bare segment — never a last-dot split of the
			// rendering (a delimited identifier may contain a literal dot).
			bareName = strings.ToUpper(k.Bare)
		}
		if !leftNames[upper] && !leftNames[bareName] {
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
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, defaultEmbeddedSchema, nil, nil)
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
func buildOuterScopeSources(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) []semantic.ScopeSource {
	// A DUPLICATE-PRESERVING slice in FROM order, never an alias-keyed map:
	// duplicate outer aliases are legal, and a
	// map collapsed them last-wins — an inner correlated reference then saw
	// only ONE leg (false 42703 for the lost leg's columns; a missed terminal
	// ambiguity for shared ones) and bound the survivor under the DISPLAY
	// alias, mis-correlating a later duplicate leg whose row namespace is its
	// minted BINDING. Every source carries bindingOrAlias — the same
	// convention as the SELECT/WHERE scope builders — so per-attribute
	// resolution and QOV emission work across scope depth exactly as at the
	// top level (the ladder: 1→bind, 0→fallthrough, ≥2→terminal 42702).
	if sq == nil || md == nil || sq.tableName == "" {
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	var sources []semantic.ScopeSource
	addSrc := func(tableName, alias, bindingID string) {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return
		}
		a := semantic.FromNormalized(alias)
		if alias == "" {
			a = semantic.FromNormalized(tableName)
		}
		sources = append(sources, semantic.ScopeSource{
			Table: tbl, Alias: a, CorrelationName: bindingOrAlias(bindingID, a),
		})
	}
	// A DERIVED-TABLE source (`FROM (SELECT ...) e`) is NOT a real table
	// either — register its VIRTUAL column schema (the SAME
	// buildDerivedTableSource the SELECT scope uses) so a CORRELATED
	// subquery referencing the derived alias resolves it. Without this,
	// addSrc's ResolveTable fails silently and the correlated reference
	// dies 42703 ("no FROM source aliased as E" single-source, `qualifier
	// "E" cannot be resolved` join form) — while the identical correlation
	// to a REAL table alias works. Mirrors the lateral-unnest leg
	// registration below.
	addDerived := func(alias, bindingID string, body antlrgen.IQueryContext) {
		if src, ok := buildDerivedTableSource(md, alias, body); ok {
			if bindingID != "" {
				src.CorrelationName = bindingID
			}
			sources = append(sources, src)
		}
	}
	if sq.derivedQuery != nil {
		// The primary derived source: the parser carries the alias in
		// tableAlias when present, else in tableName (the same convention
		// buildWherePredicateForDerived resolves against). The primary leg is
		// always a FIRST occurrence — the mint renames later duplicates only —
		// so it carries no binding id.
		alias := sq.tableAlias
		if alias == "" {
			alias = sq.tableName
		}
		addDerived(alias, "", sq.derivedQuery)
	} else {
		addSrc(sq.tableName, sq.tableAlias, "")
	}
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
				sources = append(sources, src)
			}
			continue
		}
		if j.derivedQuery != nil {
			addDerived(j.alias, j.bindingID, j.derivedQuery)
			continue
		}
		addSrc(j.tableName, j.alias, j.bindingID)
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
	schemaName string
	// outerScopes is a DUPLICATE-PRESERVING slice in FROM order — see
	// buildOuterScopeSources.
	outerScopes                []semantic.ScopeSource
	cteScopes                  map[string]semantic.ScopeSource
	cteOnScopes                map[string]semantic.ScopeSource    // ON-resolution-only CTE sources (join/unnest bodies; see buildCTEOnOnlySource)
	cteBodies                  map[string]logical.LogicalOperator // CTE name → body plan, for wrapping scalar subquery plans
	subqueries                 []logical.ExistsSubquery
	scalarSubqueries           []logical.ScalarSubquery
	correlatedScalarSubqueries []logical.CorrelatedScalarSubquery
	lastJoinPredicate          predicates.QueryPredicate
	// lastJoinPredicateOuterOnly mirrors lastJoinPredicate: the Case-1
	// nested-EXISTS middle routes OUTER-ONLY conjuncts through the join
	// predicate (the inside placement does not plan); the flag travels
	// onto the ExistsSubquery so the anti-join consumer can decline.
	lastJoinPredicateOuterOnly bool
}

// visibleScopeNames is the upper-cased set of every user-visible SQL name a
// subquery-level mint must avoid: each outer scope's Alias AND
// CorrelationName (the latter carries enclosing mints and dup-alias binding
// ids) plus the CTE registry's names. The collision invariant ("no
// generated identity equals a user-visible name") is enforced by TWO
// mechanisms with disjoint jobs: this SET covers names that can co-occur in
// the binding's resolution/registration context — the scope CHAIN (parent
// scopes carry an enclosing mint via nestedOuterScopes' CorrelationName)
// plus the CTE registry — while counter MONOTONICITY covers generated-vs-
// generated (two mints from one strictly-increasing counter can never
// collide, skip loop included, since skipping only advances). A sibling
// subquery at another level is deliberately EXCLUDED from the set: its
// binding registers at its own parent select, which is not in this
// subquery's chain — no co-occurrence, no hazard; do not "fix" the set by
// stuffing sibling names in. An ALIASED outer CTE leg is dropped from
// p.outerScopes by addSrc's silent resolve-failure arm and so escapes this
// set — booked with the outer-CTE-leg scope-registration gap (loud today:
// correlated refs to such legs die 42703).
func (p *existsSubqueryPlanner) visibleScopeNames() map[string]struct{} {
	visible := map[string]struct{}{}
	for _, src := range p.outerScopes {
		if n := src.Alias.Name(); n != "" {
			visible[strings.ToUpper(n)] = struct{}{}
		}
		if src.CorrelationName != "" {
			visible[strings.ToUpper(src.CorrelationName)] = struct{}{}
		}
	}
	for name := range p.cteScopes {
		visible[strings.ToUpper(name)] = struct{}{}
	}
	return visible
}

// mintSubqueryAlias mints the subquery binding identity (the esq / scalar /
// correlated-scalar Alias), skipping candidates a user-visible SQL name
// already spells. Without the skip, a quoted alias like `"Q$3"` regresses a
// VALID query to a loud planner failure whenever the process-global counter
// lands on it — and any counter-consuming change (e.g. the inner-correlation
// mint's own skip) SHIFTS which queries hit the alignment, so the failure
// set depends on planning history.
func (p *existsSubqueryPlanner) mintSubqueryAlias() values.CorrelationIdentifier {
	return mintDistinctIdentifier(p.visibleScopeNames(), values.UniqueCorrelationIdentifier)
}

// tryBuildCorrelatedPrimaryUnnest recognizes Java's correlated-array primary
// source inside EXISTS:
//
//	EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 9)
//
// The generic SELECT builder cannot classify this source because a primary
// FROM item normally has no source to its left. Here, however, R belongs to the
// enclosing query. Resolve the collection through the outer semantic scope and
// build a standalone LogicalUnnest carrying that resolved Value. The path is
// deliberately narrow: one direct array field, one explicit element alias,
// and no query-shaping clauses. Wider recognized shapes fail loudly rather
// than being rebuilt as a phantom table scan.
func (p *existsSubqueryPlanner) tryBuildCorrelatedPrimaryUnnest(
	q antlrgen.IQueryContext,
) (logical.LogicalOperator, bool, error) {
	if q == nil || len(p.outerScopes) == 0 || p.md == nil {
		return nil, false, nil
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, false, nil
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil || sq.derivedQuery != nil || len(sq.sourceSegments) < 2 {
		return nil, false, nil
	}
	// Java resolves a real table before falling through to correlated-field
	// access. Preserve that precedence for an active-schema-qualified table.
	if newUnnestTableResolver(p.md, p.effectiveSchemaName())(sq.sourceSegments) {
		return nil, false, nil
	}

	ownerID := semantic.FromNormalized(sq.sourceSegments[0])
	fieldID := semantic.FromNormalized(sq.sourceSegments[1])
	var owner *semantic.ScopeSource
	var col semantic.Column
	aliasSeen := false
	for i := range p.outerScopes {
		src := &p.outerScopes[i]
		if !src.Alias.EqualsIgnoreQuoting(ownerID) {
			continue
		}
		aliasSeen = true
		if src.Table == nil {
			continue
		}
		candidateColumn, found := src.Table.LookupColumn(fieldID)
		if !found {
			continue
		}
		if owner != nil {
			return nil, true, api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"correlated array source %q is ambiguous", strings.Join(sq.sourceSegments, "."))
		}
		owner = src
		col = candidateColumn
	}
	if owner == nil {
		if aliasSeen {
			return nil, true, api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist on source %q", sq.sourceSegments[1], sq.sourceSegments[0])
		}
		return nil, false, nil
	}
	if len(sq.sourceSegments) != 2 {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"nested correlated array sources in an EXISTS primary FROM clause are not yet supported")
	}
	if owner.Table == nil {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated array primary source requires a resolved outer table")
	}

	if !col.IsArray {
		return nil, true, api.NewError(api.ErrCodeInvalidColumnReference,
			"join correlation can occur only on a column of repeated (array) type")
	}

	innerAlias := sq.tableAlias
	if innerAlias == "" || strings.EqualFold(innerAlias, sq.tableName) {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated array primary source in EXISTS requires an explicit element alias")
	}
	for _, src := range p.outerScopes {
		if src.Alias.EqualsIgnoreQuoting(semantic.FromNormalized(innerAlias)) ||
			(src.CorrelationName != "" && strings.EqualFold(src.CorrelationName, innerAlias)) {
			return nil, true, api.NewError(api.ErrCodeDuplicateAlias,
				"correlated array element alias collides with an outer source alias")
		}
	}
	if len(sq.joins) != 0 || sq.distinct || len(sq.orderBy) != 0 ||
		len(sq.groupBy) != 0 || len(sq.aggCols) != 0 || sq.countStar ||
		sq.havingExpr != nil || sq.qualifyExpr != nil || sq.limit >= 0 || sq.offset != 0 {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"query-shaping clauses over a correlated array primary source in EXISTS are not yet supported")
	}
	// Validate the ignored EXISTS projection rather than silently accepting an
	// invalid SELECT list. This first production slice admits exactly the
	// element binding (`SELECT E`); EXISTS does not otherwise consume it.
	if len(sq.projCols) != 1 || len(sq.projExprs) != 1 || sq.projExprs[0] != nil ||
		sq.projCols[0].qualified {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated array EXISTS currently requires projecting its element alias")
	}
	if sq.projCols[0].bare != innerAlias {
		return nil, true, api.NewErrorf(api.ErrCodeUndefinedColumn,
			"column %q does not exist", sq.projCols[0].bare)
	}

	cat := rlcatalog.Wrap(p.md)
	analyzer := semantic.NewAnalyzer(cat, false)
	outerScope := semantic.NewScope(nil)
	for _, src := range p.outerScopes {
		if addErr := outerScope.AddSource(src); addErr != nil {
			return nil, true, addErr
		}
	}
	aliasID := semantic.FromNormalized(innerAlias)
	innerScope := semantic.NewScope(outerScope)
	virtual := &semantic.StaticTable{
		TableName: semantic.FromSegments([]string{innerAlias}, false),
		TableColumns: []semantic.Column{{
			Id:       aliasID,
			Type:     col.Type,
			Nullable: true,
		}},
	}
	if addErr := innerScope.AddSource(semantic.ScopeSource{
		Table:           virtual,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
		Shadowing:       true,
	}); addErr != nil {
		return nil, true, addErr
	}
	// Resolve the collection FROM THE INNER SCOPE so R is a parent-scope hit.
	// ResolveIdentifier then emits FieldValue(QOV(R), TAGS), preserving the
	// external correlation. Resolving against outerScope directly would treat
	// R as local and produce a childless baked FieldValue: executable only by
	// ambient-row accident and impossible to match to the candidate Explode.
	resolver := expr.New(analyzer, innerScope)
	collection, resolveErr := resolver.ResolveIdentifier(ownerID, fieldID)
	if resolveErr != nil {
		if mapped := mapPredicateWalkError(resolveErr); mapped != nil {
			return nil, true, mapped
		}
		return nil, true, resolveErr
	}

	unnest := &logical.LogicalUnnest{
		Segments:             append([]string(nil), sq.sourceSegments...),
		Alias:                innerAlias,
		CorrelatedCollection: collection,
	}
	if sq.whereExpr == nil || sq.whereExpr.Expression() == nil {
		return unnest, true, nil
	}
	if expr.ContainsSubqueryAtom(sq.whereExpr.Expression()) ||
		expr.ContainsExistsAtom(sq.whereExpr.Expression()) {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"nested subqueries inside a correlated array EXISTS predicate are not yet supported")
	}
	pred, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
	if walkErr != nil {
		if mapped := mapPredicateWalkError(walkErr); mapped != nil {
			return nil, true, mapped
		}
		return nil, true, walkErr
	}
	return &logical.LogicalFilter{
		Input:     unnest,
		Predicate: predicates.SimplifyPredicateValues(pred),
	}, true, nil
}

func (p *existsSubqueryPlanner) BuildExists(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: nil query context")
	}
	innerOp, correlatedPrimaryUnnest, err := p.tryBuildCorrelatedPrimaryUnnest(q)
	if err != nil {
		return values.CorrelationIdentifier{}, err
	}
	if !correlatedPrimaryUnnest {
		innerOp, err = buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.schemaName, p.cteScopes, p.cteOnScopes)
	}
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
	if isUndefinedCol && !correlatedPrimaryUnnest {
		p.lastJoinPredicate = nil
		p.lastJoinPredicateOuterOnly = false
		innerOp, err = p.buildCorrelatedExists(q)
		if err != nil {
			return values.CorrelationIdentifier{}, err
		}
	}
	if innerOp == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: inner query could not be planned")
	}
	// The correlated fallback deliberately ignores the SELECT values (EXISTS
	// observes only cardinality), but that is not enough when an aggregate or
	// pagination changes cardinality. Classify those operators in SQL order:
	// first establish the non-grouped aggregate's exact one-row output, then
	// apply LIMIT/OFFSET. A known result is folded by the translator, avoiding
	// the semi-join entirely while preserving correlation semantics. A
	// data-dependent OFFSET or a pagination atom still unresolved at planning
	// time cannot ride the fallback
	// safely and is rejected typed-loud rather than reverting to raw row
	// existence. The uncorrelated path keeps its real Aggregate/Limit operators.
	var knownTruth predicates.TriBool
	if isUndefinedCol && !correlatedPrimaryUnnest {
		knownTruth, err = correlatedExistsTruthAfterPagination(q)
		if err != nil {
			return values.CorrelationIdentifier{}, err
		}
	}
	alias := p.mintSubqueryAlias()
	p.subqueries = append(p.subqueries, logical.ExistsSubquery{
		Alias:                  alias,
		Plan:                   innerOp,
		JoinPredicate:          p.lastJoinPredicate,
		OuterOnlyJoinConjuncts: p.lastJoinPredicateOuterOnly,
		KnownTruth:             knownTruth,
	})
	p.lastJoinPredicate = nil
	p.lastJoinPredicateOuterOnly = false
	return alias, nil
}

// correlatedExistsTruthAfterPagination classifies the cardinality effects that
// buildCorrelatedExists otherwise drops with the ignored SELECT list.
//
// A non-grouped, non-windowed aggregate produces exactly one row before
// pagination. Applying a literal LIMIT/OFFSET to that one row therefore yields
// a compile-time EXISTS truth value. For every other supported inner shape,
// LIMIT n>=1 OFFSET 0 preserves row existence and LIMIT 0 is always empty, so
// those cases are also safe. A positive OFFSET is data-dependent (notably after
// GROUP BY), and a pagination atom still unresolved at planning time is unsafe;
// both are rejected typed-loud instead of falling through to the raw-row
// semi-join. Public SQL-driver arguments are substituted before parsing and
// therefore reach this classifier as ordinary literal values.
//
// A nil truth with nil error means the fallback may proceed because the dropped
// shaping operators provably preserve existence.
func correlatedExistsTruthAfterPagination(q antlrgen.IQueryContext) (predicates.TriBool, error) {
	if q == nil {
		return nil, nil
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, nil
	}
	simpleTable, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, nil
	}

	exactlyOneBeforePagination := queryInnerIsExactlyOneRowBeforePagination(q)
	limitClause := simpleTable.LimitClause()
	if limitClause == nil {
		if exactlyOneBeforePagination {
			return predicates.TriTrue, nil
		}
		return nil, nil
	}

	// parseLimitClause intentionally leaves a sentinel for an atom that is still
	// unresolved in this planner invocation. Here that sentinel is unsafe:
	// treating `LIMIT ?` as absent can change EXISTS. (The public driver
	// substitutes bound arguments before parsing, so those arrive as literals.)
	for _, atom := range limitClause.AllLimitClauseAtom() {
		if _, resolved, atomErr := resolveLimitAtom(atom); atomErr != nil {
			return nil, atomErr
		} else if !resolved {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"a correlated EXISTS with a planning-time unresolved LIMIT/OFFSET is not supported")
		}
	}
	limit, offset, limitErr := parseLimitClause(simpleTable)
	if limitErr != nil {
		return nil, limitErr
	}

	if exactlyOneBeforePagination {
		if limit == 0 || offset > 0 {
			return predicates.TriFalse, nil
		}
		return predicates.TriTrue, nil
	}
	if limit == 0 {
		return predicates.TriFalse, nil
	}
	if offset > 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated EXISTS with data-dependent OFFSET is not supported")
	}
	// LIMIT n>=1 with no OFFSET preserves whether a non-aggregate/grouped inner
	// is empty, so dropping that cap from an EXISTS plan is semantics-neutral.
	return nil, nil
}

// correlatedSubqueryJoinRight builds the right child for a comma/JOIN FROM leg of
// a correlated EXISTS / scalar subquery whose inner FROM clause is rebuilt here
// (the fallback paths buildCorrelatedExists / buildCorrelatedScalar). It reuses
// the EXACT lateral-unnest classification the main FROM path uses
// (lateralUnnestCandidate over visibleFromAliases + newUnnestTableResolver): a
// `t.arr AS x [AT ord]` comma source resolves to a LogicalUnnest so the Cascades
// translator lowers it to FlatMap(Scan, Explode), instead of mis-scanning
// `t.arr` as a table name. A DERIVED-TABLE leg (`… , (SELECT …) AS d`) builds its
// body via the LogicalCTE(alias) carrier (buildDerivedInnerCarrier) — the leg twin
// of the primary-source treatment — so it is never mis-scanned as a table `d`.
// Anything else stays a plain table scan. RFC-142.
func (p *existsSubqueryPlanner) correlatedSubqueryJoinRight(j joinClause, primaryTable, primaryAlias string, priorJoins []joinClause) (logical.LogicalOperator, error) {
	resolvesToTable := newUnnestTableResolver(p.md, p.effectiveSchemaName())
	visible := visibleFromAliases(primaryTable, primaryAlias, priorJoins, resolvesToTable)
	if u := lateralUnnestCandidate(j, visible, resolvesToTable); u != nil {
		return u, nil
	}
	// A DERIVED-TABLE comma/JOIN leg is NOT a catalog table: rebuilding it as
	// NewScan(j.tableName) scans a non-existent table `d` (the executor treats it
	// as EMPTY), so a cross-product leg silently collapses to ∅ and EXISTS answers
	// wrong rows — the leg twin of the primary bug correlatedInnerPrimarySource
	// fixes. Build the derived BODY and wrap it in the same CTE carrier.
	if j.derivedQuery != nil {
		return p.buildDerivedInnerCarrier(j.derivedQuery, j.tableName)
	}
	return logical.NewScan(j.tableName, j.alias), nil
}

// buildDerivedInnerCarrier builds a DERIVED-TABLE inner source
// (`(SELECT …) AS alias`) for a correlated subquery whose inner FROM this fallback
// rebuilds. A derived source is NOT a catalog table: rebuilding it as
// `NewScan(alias)` scans a non-existent table `alias`, which the executor treats
// as EMPTY — so the source silently reads the wrong (empty) relation and the query
// answers wrong rows. Plan the derived BODY through the SAME catalog-aware path the
// normal SELECT uses (buildLogicalPlanForQueryBodyWithCTECatalog) and wrap it in the
// LogicalCTE(alias) carrier buildOuterPlanOnDerived installs, so the inner FROM
// carries the derived subplan and sourceAlias resolves to the derived alias. A body
// the inner builder cannot plan declines LOUDLY (correct-or-conservative) rather
// than degrading to the empty bare scan. Mirrors the cteScopes resolution the
// WHERE/ON path uses for a CTE inner (a derived source is resolved via its body, not
// a WITH registry). Shared by the PRIMARY source (correlatedInnerPrimarySource) and
// each comma/JOIN LEG (correlatedSubqueryJoinRight). Its result is USED by the EXISTS
// fast path. On the EXISTS WHERE/ON path a derived leg's carrier is built here (all
// rights[i] are built up front) but then DISCARDED — addCorrelatedJoinScopeSource
// declines the derived leg loud before the join tree consumes rights[i]. On the SCALAR
// path it is NEVER built: buildCorrelatedScalar resolves its inner SCOPE (ResolveTable /
// addCorrelatedJoinScopeSource) and declines a derived source BEFORE its rights loop runs.
func (p *existsSubqueryPlanner) buildDerivedInnerCarrier(derivedQuery antlrgen.IQueryContext, alias string) (logical.LogicalOperator, error) {
	innerOp, innerErr := buildLogicalPlanForQueryBodyWithCTECatalog(
		derivedQuery.QueryExpressionBody(), p.md, p.effectiveSchemaName(), p.cteScopes, p.cteOnScopes)
	if innerErr != nil {
		// A STRUCTURED planner/resolution failure of the derived BODY (e.g. an
		// undefined column → 42703) is a FAITHFUL diagnostic of the inner query, not
		// an unsupported-shape decline. Surface it VERBATIM so the SQLSTATE is the
		// SAME in every EXISTS position: mapPredicateWalkError matches
		// CorrelatedExistsError (→ 0A000) before the raw api.Error, so wrapping this
		// would rewrite the derived body's 42703 to 0A000 in a WHERE EXISTS while the
		// projected position keeps 42703. Returning the api.Error unwrapped keeps both
		// faithful, and leaves the ResolveTable "table not found" decline (which is
		// NOT a derived-body failure) wrapped in CorrelatedExistsError → 0A000.
		var apiErr *api.Error
		if errors.As(innerErr, &apiErr) {
			return nil, apiErr
		}
		return nil, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated subquery: build derived inner %q: %v", alias, innerErr),
			Cause:   innerErr,
		}
	}
	if innerOp == nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated subquery: derived inner %q is out of scope", alias)}
	}
	return logical.NewCTE(alias, innerOp, logical.NewScan(alias, ""), false), nil
}

// correlatedInnerPrimarySource builds the primary FROM-source operator for a
// correlated EXISTS / scalar subquery whose inner FROM this fallback rebuilds. A
// plain table source is a bare scan; a DERIVED-TABLE source (`(SELECT …) AS d`)
// routes through buildDerivedInnerCarrier (which builds the body, never mis-scans
// `d` as a table).
func (p *existsSubqueryPlanner) correlatedInnerPrimarySource(sq *selectQuery, innerAlias string) (logical.LogicalOperator, error) {
	if sq.derivedQuery == nil {
		return logical.NewScan(sq.tableName, innerAlias), nil
	}
	return p.buildDerivedInnerCarrier(sq.derivedQuery, sq.tableName)
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
		// CTE-aware fallback (mirrors the primary source): a CTE join leg resolves
		// via the enclosing query's CTE registry, not the catalog.
		if src, found := p.cteScopes[strings.ToUpper(j.tableName)]; found {
			jTbl, jErr = src.Table, nil
		}
	}
	if jErr != nil {
		return jErr
	}
	jAliasID := semantic.FromNormalized(jAlias)
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

	// An inner with HAVING / QUALIFY cannot ride this fallback: the rebuild
	// below carries only FROM + WHERE, so a group-eliminating filter would be
	// silently DROPPED and the semijoin would keep outer rows whose every
	// group fails HAVING — wrong rows (yamsql exists_with_aggregate:
	// `EXISTS(… GROUP BY o.customer_id HAVING SUM(o.amount) > 150)` kept a
	// customer whose group sums to 50). Java plans this shape (an existential
	// quantifier over a GroupByExpression); the port is the RFC-180 booked
	// follow-up — until then decline TYPED, never wrong rows.
	//
	// A HAVING-less GROUP BY is deliberately NOT declined: for EXISTS the
	// drop is semantics-preserving — grouping a non-empty row set yields ≥1
	// group and grouping an empty set yields none, so EXISTS(GROUP BY over S)
	// ⇔ EXISTS(S) when pagination preserves existence. BuildExists separately
	// rejects a data-dependent grouped OFFSET. A NON-grouped aggregate inner
	// continues because its exact pre-pagination cardinality is one row;
	// BuildExists applies LIMIT/OFFSET to that cardinality and the translator
	// folds the resulting TRUE/FALSE in either polarity.
	if sq.havingExpr != nil || sq.qualifyExpr != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated EXISTS over a GROUP BY / HAVING subquery is not supported")
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

	// Resolve the leg operators + join kinds first — this needs no resolver
	// (correlatedSubqueryJoinRight classifies scan/unnest sources directly).
	rights := make([]logical.LogicalOperator, len(sq.joins))
	kinds := make([]logical.JoinKind, len(sq.joins))
	for i, j := range sq.joins {
		right, rErr := p.correlatedSubqueryJoinRight(j, sq.tableName, innerAlias, sq.joins[:i])
		if rErr != nil {
			return nil, rErr
		}
		rights[i] = right
		switch j.joinType {
		case joinTypeLeft:
			kinds[i] = logical.JoinLeft
		case joinTypeRight:
			kinds[i] = logical.JoinRight
		case joinTypeFull:
			kinds[i] = logical.JoinFull
		default:
			kinds[i] = logical.JoinInner
		}
	}

	// CTE-safe fast path: the scope+resolver below is needed ONLY to walk an ON or
	// a WHERE. A correlated fallback entered solely because the (ignored) SELECT
	// list references an outer column — no WHERE, no ON — must return the bare join
	// tree WITHOUT resolving the inner source as a catalog table: a CTE / derived
	// inner is not in the catalog, so reaching Analyzer.ResolveTable would reject a
	// valid inner ("table not found"). This restores the original pre-scope
	// position of the fast path.
	anyOn := false
	for _, j := range sq.joins {
		if j.onExpr != nil {
			anyOn = true
			break
		}
	}
	if (sq.whereExpr == nil || sq.whereExpr.Expression() == nil) && !anyOn {
		op, primErr := p.correlatedInnerPrimarySource(sq, innerAlias)
		if primErr != nil {
			return nil, primErr
		}
		for i := range sq.joins {
			op = logical.NewJoinWithPredicate(op, rights[i], kinds[i], nil)
		}
		return op, nil
	}

	// There is a WHERE or an ON — build the inner scope + resolver so each explicit
	// `JOIN … ON` clause can be walked and placed correctly (the sibling
	// buildCorrelatedScalar has the same ordering). An INNER-join ON is
	// equivalent to a WHERE conjunct and is folded into the inner predicate
	// stream below; an OUTER-join ON is NOT (unmatched preserved-side rows must
	// survive), so it stays on the join node — see the join loop.
	cat := rlcatalog.Wrap(p.md)
	analyzer := semantic.NewAnalyzer(cat, false)

	outerScope := semantic.NewScope(nil)
	for _, src := range p.outerScopes {
		_ = outerScope.AddSource(src)
	}

	innerScope := semantic.NewScope(outerScope)
	viaCTE := false
	tbl, tblErr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(sq.tableName, "."), false))
	if tblErr != nil {
		// CTE-aware fallback: a CTE inner source (`WITH c AS (…) … EXISTS (SELECT …
		// FROM c JOIN t ON …)`) is not a catalog table, so ResolveTable misses.
		// Resolve it via the enclosing query's CTE registry — the SAME cteScopes the
		// normal join-ON / WHERE resolvers consult (upgradeJoinOnPredicates,
		// buildWherePredicateForJoinsWithCTEScopes) — so an ON/WHERE over the CTE's
		// columns walks correctly instead of failing "table not found". (A derived
		// `(SELECT …) AS d` inner is not WITH-registered and stays a clean error.)
		if src, found := p.cteScopes[strings.ToUpper(sq.tableName)]; found {
			tbl, tblErr, viaCTE = src.Table, nil, true
		}
	}
	if tblErr != nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: resolve inner table %q: %v", sq.tableName, tblErr), Cause: tblErr}
	}
	aliasID := semantic.FromNormalized(innerAlias)
	// Collision mint: a SINGLE-TABLE catalog inner is BORN under a
	// unique correlation identity, never its SQL source name. The SQL name
	// (aliasID) stays the scope-resolution qualifier — `MA.c` inside the
	// subquery still resolves against the inner source, SHADOWING a
	// same-named outer leg (Java SemanticAnalyzer.resolveAcrossFragments:
	// innermost fragment first) — but every reference the walk emits is
	// qualified under the MINTED identity, so the join predicate can never
	// carry the ambiguous SQL name. Without the mint, an inner-bound ref
	// qualified under the source name collided with a same-named outer leg
	// at the join level: the name-model rebase reinterpreted it as an
	// OUTER-leg read and the positive-polarity outer-routing pre-filtered
	// per outer row — wrong rows vs Java's inner-shadow semantics (live-
	// verified: `FROM MA, MA.arr AS X WHERE EXISTS (SELECT 1 FROM MA WHERE
	// MA.c < X)` answers ALL elements with X > min(MA.c) in Java). The
	// SIMPLE NOT-EXISTS twin was already correct — negation forbids the
	// hoist, the conjunct stayed under the ∃ and bound inner — the polarity
	// split that proved the ambiguity, not the runtime, was the defect; a
	// NESTED NOT-EXISTS composition was still wrong pre-mint and is fixed
	// by the same identity (pinned: notexists_around_nested_colliding).
	// Uppercase because
	// every consumer (sourceAlias, outerBoundAliases, splitOuterOnly-
	// conjuncts) upper-cases SQL aliases; existsInnerCorrelation's rename
	// then rebases the minted name onto esq.Alias exactly as it did the
	// source name. Multi-source and CTE inners keep the SQL name (the
	// rename path declines them; mint-per-leg is booked follow-on work).
	mintedInnerCorr := ""
	if len(sq.joins) == 0 && !viaCTE {
		// A QUOTED SQL alias can legally spell `"Q$N"`, so a raw mint could
		// equal a visible outer name when the global counter happens to
		// align — the outer's refs would then be captured by the inner
		// binding, with results depending on planning history. Mint until
		// distinct from every visible name (see mintDistinctUpper): the
		// inner SQL alias, every outer scope's Alias AND CorrelationName
		// (the latter covers enclosing mints and dup-alias binding ids),
		// and the CTE registry's names — an unaliased CTE leg (`FROM c`)
		// is absent from p.outerScopes (addSrc drops catalog-resolution
		// failures), so its name would otherwise escape the set. An
		// ALIASED CTE leg (`FROM c AS "Q$44"`) is dropped alias-and-all —
		// that alias is unreachable here and is the one residual gap,
		// booked with the outer-CTE-leg scope-registration fix (the same
		// family as the derived-table registration above). esq.Alias
		// values (existsInnerCorrelation's rename targets) are a distinct
		// generated namespace off the SAME strictly-increasing counter —
		// and mintSubqueryAlias skips user-visible names for them too — so
		// a mint can never equal one; no entry needed for them.
		visible := p.visibleScopeNames()
		visible[strings.ToUpper(innerAlias)] = struct{}{}
		mintedInnerCorr = mintDistinctUpper(visible, values.UniqueCorrelationIdentifier)
	}
	innerCorrName := aliasID.Name()
	if mintedInnerCorr != "" {
		innerCorrName = mintedInnerCorr
	}
	_ = innerScope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: aliasID, CorrelationName: innerCorrName,
	})

	// Join sources are added to the inner scope INCREMENTALLY in the join loop
	// below — each leg registered right BEFORE its own ON is walked — so an ON at
	// join level i sees only {primary + legs[0..i]} (SQL left-to-current
	// visibility), never a LATER leg. Without this, a later leg that REUSES an
	// outer alias would capture an earlier ON's reference to that name (which must
	// bind the OUTER source), misclassifying a correlation as inner and misplacing
	// the predicate. The resolver holds innerScope by reference, so sources added
	// after construction are visible to subsequent walks; the WHERE walk (after the
	// loop) sees the FULL inner scope, which is correct — only ON visibility is
	// left-to-current.
	resolver := expr.New(analyzer, innerScope)

	// Install a SubqueryPlanner on the resolver so that nested EXISTS
	// subqueries in the inner WHERE can be planned. The nested planner's
	// outer scopes include both the current planner's outer scopes and
	// the inner table — this enables correlation across multiple levels
	// (e.g. innermost EXISTS referencing outermost emp.id).
	// The inner source SHADOWS a same-aliased outer for the next nesting
	// level (the semantics the alias-keyed map's overwrite used to encode):
	// drop same-aliased outers before appending, so a doubly-nested EXISTS
	// still resolves the nearer source first — never a same-level duplicate
	// of an outer leg with the inner table.
	nestedOuterScopes := make([]semantic.ScopeSource, 0, len(p.outerScopes)+1)
	for _, v := range p.outerScopes {
		if !v.Alias.EqualsIgnoreQuoting(aliasID) {
			nestedOuterScopes = append(nestedOuterScopes, v)
		}
	}
	// The nested scope carries the MINTED correlation (innerCorrName) so a
	// nested EXISTS's reference to THIS level's source emits the identity
	// the runtime actually binds — the minted scan alias — not the SQL name
	// (which may be an outer leg's).
	nestedOuterScopes = append(nestedOuterScopes, semantic.ScopeSource{
		Table: tbl, Alias: aliasID, CorrelationName: innerCorrName,
	})
	nestedPlanner := &existsSubqueryPlanner{
		md:          p.md,
		schemaName:  p.schemaName,
		outerScopes: nestedOuterScopes,
		cteScopes:   p.cteScopes,
		cteOnScopes: p.cteOnScopes,
	}
	resolver.SetSubqueryPlanner(nestedPlanner)

	// Build the join tree from the inner FROM clause (handles multi-table
	// EXISTS). A `t.arr AS x [AT ord]` comma source is a lateral array unnest,
	// not a table — classify it via the SAME helper the main FROM path uses so
	// the Cascades translator lowers it to FlatMap(Scan, Explode). RFC-142.
	//
	// Each explicit `JOIN … ON` is split against the inner-source universe:
	//   - INNER-INNER conjuncts (reference only inner sources, e.g. `f.fid=e.fid`)
	//     stay ON THAT JOIN'S NODE — applied at the correct join level in EVERY
	//     ordering. Folding them into one predicate below the whole inner join
	//     would misplace an INNER ON that precedes a later RIGHT/FULL join: a
	//     preserved outer-join row has NULL inner columns, so the folded ON goes
	//     NULL→false and drops a row that must keep EXISTS true.
	//   - CORRELATION conjuncts (reference the outer query, e.g. `e.eid=p.id`):
	//     an INNER join lifts them to the outer level (like a WHERE correlation);
	//     an OUTER (LEFT/RIGHT/FULL) join cannot — lifting a predicate out of an
	//     outer-join ON changes which rows are preserved — so decline cleanly.
	//
	// A conjunct is a liftable CORRELATION only if it references a REAL
	// OUTER-SCOPE source (a source in the enclosing query's scope), not merely a
	// name absent from the inner sources. This is the robustness boundary: a
	// nested subquery inside an ON binds a GENERATED alias that is neither an
	// inner source nor an outer-scope source — classifying it as "outer" would
	// lift it, and the downstream nested-EXISTS hoist would then drop the whole
	// join tree. Build the outer-scope name set here.
	outerAliases := map[string]struct{}{}
	for _, src := range p.outerScopes {
		if src.CorrelationName != "" {
			outerAliases[strings.ToUpper(src.CorrelationName)] = struct{}{}
		}
		if n := src.Alias.Name(); n != "" {
			outerAliases[strings.ToUpper(n)] = struct{}{}
		}
	}

	// The inner-source alias set is accumulated INCREMENTALLY (primary + legs seen
	// so far) so each ON's split reflects SQL left-to-current visibility: a name
	// that is only a LATER inner source is out of scope at an earlier ON — there it
	// binds an outer source with that name (a correlation) or is unresolved. A
	// conjunct is a liftable correlation only if it references an outer-scope name
	// that is NOT ALSO an (in-scope) inner source: when the outer query and the
	// inner FROM reuse the same alias, the inner source SHADOWS the outer, so a
	// reference to that name binds inner (the shadowing guard in
	// splitConjunctsByOuterRef).
	levelInnerAliases := innerSourceAliases(logical.NewScan(sq.tableName, innerAlias))

	// The FULL inner-source alias set (all legs) is used to detect an outer/inner
	// alias COLLISION: an earlier ON that references an alias which is ALSO a LATER
	// inner leg. Per-join scope correctly binds that reference to the OUTER source
	// (the later leg isn't in scope yet), but the lifted correlation's QOV(name)
	// then collides with the inner leg of the same name at runtime — ambiguous,
	// silent-wrong. Such a correlation is declined below rather than mis-answered.
	fullInnerAliases := innerSourceAliases(logical.NewScan(sq.tableName, innerAlias))
	for i := range sq.joins {
		for a := range innerSourceAliases(rights[i]) {
			fullInnerAliases[a] = struct{}{}
		}
	}

	// The scan itself is built under the minted identity (falling back to the
	// SQL alias for the unminted shapes) — the plan-side half of the mint:
	// sourceAlias(esq.Plan) and outerBoundAliases(esq.Plan) then report the
	// unique name, so a same-named outer leg can never alias-collide with
	// the inner at the join level.
	scanAlias := innerAlias
	if mintedInnerCorr != "" {
		scanAlias = mintedInnerCorr
	}
	op := logical.LogicalOperator(logical.NewScan(sq.tableName, scanAlias))
	var liftedOnCorr []predicates.QueryPredicate
	for i, j := range sq.joins {
		// Register leg i's source in the inner scope BEFORE walking its ON (so the
		// ON sees {primary + legs[0..i]}), and accumulate its aliases into the
		// per-level inner set used by the split. A lateral-unnest leg registers the
		// same virtual Shadowing source the main path uses (exposing the
		// element/ordinal binding) instead of resolving `t.arr` as a table. RFC-142.
		if jErr := p.addCorrelatedJoinScopeSource(innerScope, analyzer, j, sq.tableName, innerAlias, sq.joins[:i]); jErr != nil {
			return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: resolve join table %q: %v", j.tableName, jErr), Cause: jErr}
		}
		for a := range innerSourceAliases(rights[i]) {
			levelInnerAliases[a] = struct{}{}
		}
		var nodeOn predicates.QueryPredicate
		if j.onExpr != nil {
			subqBefore := len(nestedPlanner.subqueries) + len(nestedPlanner.scalarSubqueries) + len(nestedPlanner.correlatedScalarSubqueries)
			walkedOn, onErr := resolver.WalkPredicate(j.onExpr)
			if onErr != nil {
				// A nested subquery/EXISTS inside the ON is an unsupported shape
				// (declined below via onAddedSubquery). But the walk can FAIL first —
				// e.g. `ON EXISTS (SELECT 1 FROM h WHERE h.hid = f.fid)` where the
				// nested subquery references the CURRENT leg `f`, which the nested
				// planner's scope does not expose. Surface that as the deliberate
				// 0A000 decline (Unsupported) rather than a raw 42703 resolution
				// failure in the WHERE-EXISTS path.
				if expr.ContainsSubqueryAtom(j.onExpr) || expr.ContainsExistsAtom(j.onExpr) {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a nested subquery inside a JOIN ON clause is not supported", Unsupported: true}
				}
				return nil, wrapCorrelatedExistsWalkErr(fmt.Sprintf("correlated EXISTS: walk ON clause: %v", onErr), onErr)
			}
			// An ON that itself contains a nested EXISTS/scalar subquery cannot be
			// handled by this fallback: lifting it to the outer level misclassifies
			// the generated subquery alias as a correlation (and the downstream
			// nested-EXISTS hoist would drop the join tree), while keeping it on the
			// join node orphans the nested subquery's PLAN (the join node carries no
			// ExistsSubqueries slot, so the nested EXISTS evaluates as a dead
			// always-false predicate). Neither placement is correct, so decline
			// cleanly (correct-or-conservative) rather than answer wrong rows.
			onAddedSubquery := len(nestedPlanner.subqueries)+len(nestedPlanner.scalarSubqueries)+len(nestedPlanner.correlatedScalarSubqueries) > subqBefore
			if onAddedSubquery {
				return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a nested subquery inside a JOIN ON clause is not supported", Unsupported: true}
			}
			// A subquery-free ON is split into a liftable correlation (references a
			// real outer-scope source that is not shadowed by an inner source) and
			// the inner-inner part (stays on the node).
			correlation, innerInner := splitConjunctsByOuterRef(walkedOn, outerAliases, levelInnerAliases)
			nodeOn = innerInner
			if correlation != nil {
				if kinds[i] != logical.JoinInner {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlation inside an OUTER (LEFT/RIGHT/FULL) JOIN ON clause is not supported", Unsupported: true}
				}
				// Outer/inner alias collision: the correlation references an outer
				// name that is ALSO a (later) inner leg — the same name bound in two
				// scopes. Per-join scope bound it to the outer here, but lifting the
				// correlation makes its QOV(name) collide with the inner leg at runtime
				// (ambiguous). Decline (correct-or-conservative) rather than silent-wrong.
				for name := range predicates.GetCorrelatedToOfPredicate(correlation) {
					n := strings.ToUpper(name.Name())
					_, isOuter := outerAliases[n]
					_, isFullInner := fullInnerAliases[n]
					if isOuter && isFullInner {
						return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a JOIN ON references an alias reused as a later inner join source (outer/inner alias collision) is not supported", Unsupported: true}
					}
				}
				// Lifting a correlated INNER-join ON to the outer level applies it
				// AFTER the whole inner plan (like a WHERE correlation). That loses
				// the ON's join-level placement: if a LATER join is RIGHT or FULL, it
				// preserves g-side rows with NULL on this join's e/f columns, and the
				// lifted `e.eid=p.id` then evaluates NULL→false and rejects those
				// preserved rows — EXISTS wrongly false. (A later LEFT/INNER join does
				// not preserve NULL-e rows, so the lift is safe.) Reproducing the
				// correct join-level placement is not something this fallback can do,
				// so decline cleanly rather than answer wrong rows.
				laterOuterPreservesOtherSide := false
				for k := i + 1; k < len(kinds); k++ {
					if kinds[k] == logical.JoinRight || kinds[k] == logical.JoinFull {
						laterOuterPreservesOtherSide = true
						break
					}
				}
				if laterOuterPreservesOtherSide {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlation inside a JOIN ON clause before a later RIGHT/FULL JOIN is not supported", Unsupported: true}
				}
				liftedOnCorr = append(liftedOnCorr, correlation)
			}
		}
		op = logical.NewJoinWithPredicate(op, rights[i], kinds[i], nodeOn)
	}

	// With inner-inner ON conjuncts on their join nodes, only an INNER-join ON's
	// correlation or the WHERE still needs a filter. A `SELECT 1 FROM e JOIN f ON
	// f.fid=e.fid AND e.eid=p.id` inner with NO WHERE must NOT early-return the
	// bare join: that would drop the lifted correlation and make EXISTS silently
	// true over an empty inner join.
	if (sq.whereExpr == nil || sq.whereExpr.Expression() == nil) && len(liftedOnCorr) == 0 {
		return op, nil
	}

	var pred predicates.QueryPredicate
	if sq.whereExpr != nil && sq.whereExpr.Expression() != nil {
		var walkErr error
		pred, walkErr = resolver.WalkPredicate(sq.whereExpr.Expression())
		if walkErr != nil {
			return nil, wrapCorrelatedExistsWalkErr(fmt.Sprintf("correlated EXISTS: walk predicate: %v", walkErr), walkErr)
		}
	}

	// Lift each INNER-join ON's correlation conjuncts to the outer level, routed
	// by the same qualify + splitOuterOnlyConjuncts machinery as the WHERE.
	for _, onCorr := range liftedOnCorr {
		if pred == nil {
			pred = onCorr
		} else {
			pred = predicates.NewAnd(pred, onCorr)
		}
	}
	if pred == nil {
		return op, nil
	}

	// MULTI-SOURCE scope-ambiguity decline (correct-or-loud): an UNMINTED
	// multi-source inner keeps its SQL leg names, so a predicate ref to a
	// leg that REUSES an outer bound name is ambiguous at the join level —
	// the walk bound it INNER (SQL shadowing), but the name-model runtime
	// routes such refs by name against the merged outer row (per-outer-row
	// reads; Java's inner-shadow semantics answer differently — live-
	// verified). Decline LOUDLY rather than answer wrong rows; mint-per-leg
	// (booked) closes the reach gap for real. Placement: BEFORE the
	// nested-EXISTS branches — Case 1 assigns this predicate's non-EXISTS
	// part as the join predicate and would otherwise carry the ambiguous
	// ref out unchecked (a nested constant-true EXISTS must not disable
	// the guard); Case 2's hoist is covered by the nested planner running
	// this same check recursively for its own scope. Checking the full
	// walked predicate here is a SAFE SUPERSET of the old tail check
	// (rest ⊂ pred): an outer-only conjunct cannot carry an intersection
	// name (an intersection name IS an inner alias, so the split keeps it
	// in rest), and EXISTS/scalar markers carry only generated aliases —
	// which cannot equal user names BECAUSE of the mint law (the 3a skip
	// makes every generated identity distinct from user-visible names;
	// that law is load-bearing for this argument). The check runs on a
	// SIMPLIFIED COPY: constant folding can eliminate a ref entirely
	// (`COALESCE(1, a.id) = 1` never reads a), and the join predicate
	// that actually rides out is the simplified form — declining on a
	// foldable ref would 0A000 valid queries the tail-era check accepted.
	// Parent-fallthrough refs cannot false-positive: in a multi-source
	// scope a shadowed parent hit already dies at plan time
	// (CorrelatedShadowError, 42703), so a surviving ref carrying an
	// intersection name is inner-bound by construction. Minted single-
	// table inners never enter (their inner refs are Q$N).
	if len(sq.joins) > 0 {
		if n := scopeAmbiguousName(predicates.SimplifyPredicateValues(pred), fullInnerAliases, p.outerScopes); n != "" {
			return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: inner FROM source %q reuses an outer FROM name referenced by the subquery predicate (scope-ambiguous)", n), Unsupported: true}
		}
	}

	// Propagate SCALAR subquery plans the nested planner collected while walking
	// the inner WHERE (`… EXISTS (SELECT 1 FROM c WHERE p.id > (SELECT MIN(id)
	// FROM c2))`). The walked predicate references the scalar's ALIAS; without
	// the plan the executor never pre-evaluates it, the alias binding stays
	// unset, and the comparison is silently NULL → every outer row dropped.
	// Bubbling them into THIS planner routes them to the enclosing filter/
	// projection exactly like a top-level scalar subquery (an uncorrelated
	// scalar is a query-constant external binding — its evaluation point is
	// scope-free). Correlated scalars propagate the same way; their per-row
	// evaluation would need per-row re-execution (below).
	p.scalarSubqueries = append(p.scalarSubqueries, nestedPlanner.scalarSubqueries...)
	// A CORRELATED scalar inside an EXISTS WHERE has NO evaluation path: the
	// one-shot pre-eval cannot re-run it per row, and the WHERE channel has no
	// CorrelatedScalarSubquery consumer (only projections and HAVING do).
	// Dropping it silently NULLed the comparison and returned zero rows for
	// every outer row; decline LOUDLY instead (CORRECT-or-LOUD — the per-row
	// evaluation is tracked follow-on work with the EXISTS wrong-rows batch).
	if len(nestedPlanner.correlatedScalarSubqueries) > 0 {
		return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlated scalar subquery inside an EXISTS WHERE clause is not supported"}
	}

	// If the nested planner collected EXISTS subqueries, check whether
	// the middle level has its own correlation predicate (non-EXISTS).
	if len(nestedPlanner.subqueries) > 0 {
		innerCorr := strings.ToUpper(innerCorrName)
		nonExistsPred := splitNonExistsPredicatesFromWalked(pred)

		if nonExistsPred != nil {
			// Case 1: middle has BOTH correlation + nested EXISTS.
			// Build a proper LogicalFilter preserving the middle level.
			existsPred := stripNonExistsPredicates(pred)
			qualifyBareFields(nonExistsPred, innerCorr)
			simplified := predicates.SimplifyPredicateValues(nonExistsPred)
			// A NON-INNER conjunct here — an outer-only correlation OR a
			// reference-free constant/parameter, i.e. anything the
			// existential rule routes to the OUTER input — CANNOT take the
			// tail path's inside placement: a nested-EXISTS-carrying filter
			// with an extra plain conjunct (or an inner filter layer) does
			// not plan for this composition (the booked multi-EXISTS
			// best-expression family; both placements were tried and die at
			// physical planning). It rides lastJoinPredicate, which the
			// semi-join outer-routes — VALID for positive polarity
			// (P ∧ ∃(Q) ≡ ∃(P∧Q)) and WRONG under NOT EXISTS (computes
			// P ∧ ¬∃(Q), silently dropping every ¬P outer row — the
			// pre-existing leak this branch shipped with). The esq is
			// FLAGGED so the anti-join consumer declines LOUDLY; positive
			// polarity keeps its valid outer-routing unchanged. The flag
			// test is deliberately BROADER than splitOuterOnlyConjuncts:
			// that split keeps reference-free conjuncts (`1 = 0`, a
			// parameter) in rest, yet they outer-route all the same and
			// carry the identical polarity hazard.
			p.lastJoinPredicate = simplified
			p.lastJoinPredicateOuterOnly = hasNonInnerConjunct(simplified, innerSourceAliases(op))
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
		p.lastJoinPredicateOuterOnly = innerESQ.OuterOnlyJoinConjuncts
		return innerESQ.Plan, nil
	}

	// The predicate will be evaluated in a merged NLJ context where both
	// inner and outer columns coexist keyed by UPPER-CASE qualified names
	// (e.g. SUB.V, A.V). The resolver produced bare field names for inner
	// columns (e.g. "V") because the inner scope has only one source.
	// Qualify them with the inner correlation name — the MINTED identity
	// when the mint applies — so that merged-row lookup finds the inner
	// column, not the outer's value leaking through when the inner row has
	// a NULL (absent-from-map) field.
	innerCorr := strings.ToUpper(innerCorrName)
	qualifyBareFields(pred, innerCorr)
	pred = predicates.SimplifyPredicateValues(pred)

	// OUTER-ONLY conjuncts (`… WHERE p.id = 1` — no inner-source reference) stay
	// INSIDE the subquery as a filter on the inner plan, so they evaluate UNDER
	// the ∃ in both polarities: ¬∃(P∧Q) ≡ ¬P ∨ ¬∃(Q). Threading them through the
	// join predicate instead hands them to the semi-join implementation's
	// inner/outer routing, which pre-filters the OUTER on outer-only conjuncts —
	// an equivalence that holds ONLY for the positive polarity (P ∧ ∃(Q) ≡
	// ∃(P∧Q)); under NOT EXISTS it computes P ∧ ¬∃(Q) and wrongly drops every
	// ¬P outer row. Placement, not polarity, is the invariant: subquery-origin
	// conjuncts never leave the subquery. That INCLUDES conjuncts referencing a
	// SCALAR-subquery alias: the pre-evaluated binding lives in the root
	// evaluation context and IS visible below the FirstOrDefault (the filter
	// contexts thread it) — the RFC-141 R4 outer-routing rationale concerns
	// SIBLING predicates outside the ∃ (which must not be skipped when the
	// inner is empty), never subquery-internal conjuncts. Routing a
	// scalar-referencing internal conjunct outward reproduced the pre-filter
	// polarity bug for exactly the NOT-EXISTS + scalar shape.
	// The inner-source universe comes from the BINDER-EXACT collector over the
	// built op tree (the same helper the correlated-scalar scope discriminator
	// uses, pinned by TestInnerSourceAliases_MirrorsUnnestBinder) — one
	// inner-source authority, not a second joins-walk.
	outerOnly, rest := splitOuterOnlyConjuncts(pred, innerSourceAliases(op))
	if outerOnly != nil {
		op = &logical.LogicalFilter{Input: op, Predicate: outerOnly}
	}
	// The multi-source scope-ambiguity decline already ran on the FULL
	// walked predicate above (before the nested-EXISTS branches) — rest is
	// a subset of it, so no second check is needed here.
	p.lastJoinPredicate = rest
	return op, nil
}

// splitOuterOnlyConjuncts partitions a subquery WHERE's top-level AND tree into
// (outerOnly, rest): a conjunct is OUTER-ONLY iff it references at least one
// correlation and none of them is an inner FROM source. A scalar-subquery
// alias counts as OUTER here — the pre-evaluated binding lives in the root
// evaluation context and is visible under the ∃, and the placement invariant
// (subquery conjuncts evaluate under the ∃, both polarities) applies to it
// like any other outer-only conjunct. Inner-only conjuncts, genuine
// correlation conjuncts, and reference-free constants stay in rest (the join
// predicate), preserving the existing routing. An OR tree is one conjunct,
// classified atomically by its whole correlation set.
func splitOuterOnlyConjuncts(pred predicates.QueryPredicate, innerAliases map[string]struct{}) (outerOnly, rest predicates.QueryPredicate) {
	if pred == nil {
		return nil, nil
	}
	var outer, keep []predicates.QueryPredicate
	var walk func(p predicates.QueryPredicate)
	walk = func(p predicates.QueryPredicate) {
		if and, ok := p.(*predicates.AndPredicate); ok {
			for _, sub := range and.SubPredicates {
				walk(sub)
			}
			return
		}
		corrs := predicates.GetCorrelatedToOfPredicate(p)
		if len(corrs) == 0 {
			keep = append(keep, p)
			return
		}
		for c := range corrs {
			name := strings.ToUpper(c.Name())
			if _, isInner := innerAliases[name]; isInner {
				keep = append(keep, p)
				return
			}
		}
		outer = append(outer, p)
	}
	walk(pred)
	andOf := func(ps []predicates.QueryPredicate) predicates.QueryPredicate {
		switch len(ps) {
		case 0:
			return nil
		case 1:
			return ps[0]
		default:
			return predicates.NewAnd(ps...)
		}
	}
	return andOf(outer), andOf(keep)
}

// splitConjunctsByOuterRef partitions a walked ON's top-level AND tree into
// (refsOuter, innerOnly): a conjunct is refsOuter iff it references at least one
// correlation that is a REAL OUTER-SCOPE source (name in outerAliases) AND is NOT
// ALSO an inner source (name absent from innerAliases). Every other conjunct —
// referencing only inner sources, a generated (non-outer-scope) alias, an
// outer-scope name SHADOWED by a same-named inner source, or no correlation at
// all — is innerOnly. Two robustness boundaries live in this test:
//   - Membership in the outer-scope set (not mere ABSENCE from inner sources)
//     keeps a generated nested-subquery alias — which is neither outer-scope nor
//     inner — off the lift path.
//   - The `!isInner` guard handles ALIAS SHADOWING: when the outer query and the
//     inner FROM reuse the same alias (`c`), an inner reference `c.col` binds to
//     the inner source (inner shadows outer in the inner scope), so it must not be
//     misclassified as an outer correlation and over-decline a valid inner-only ON.
//
// This isolates a genuine correlation conjunct like `e.eid = p.id` (inner e +
// unshadowed outer p) into refsOuter so an INNER join's ON correlation can be
// lifted to the outer level while its inner-inner conjuncts stay on the join node.
// Either return may be nil.
func splitConjunctsByOuterRef(pred predicates.QueryPredicate, outerAliases, innerAliases map[string]struct{}) (refsOuter, innerOnly predicates.QueryPredicate) {
	if pred == nil {
		return nil, nil
	}
	var outer, inner []predicates.QueryPredicate
	var walk func(p predicates.QueryPredicate)
	walk = func(p predicates.QueryPredicate) {
		if and, ok := p.(*predicates.AndPredicate); ok {
			for _, sub := range and.SubPredicates {
				walk(sub)
			}
			return
		}
		hasOuter := false
		for c := range predicates.GetCorrelatedToOfPredicate(p) {
			name := strings.ToUpper(c.Name())
			_, isOuter := outerAliases[name]
			_, isInner := innerAliases[name]
			if isOuter && !isInner {
				hasOuter = true
				break
			}
		}
		if hasOuter {
			outer = append(outer, p)
		} else {
			inner = append(inner, p)
		}
	}
	walk(pred)
	andOf := func(ps []predicates.QueryPredicate) predicates.QueryPredicate {
		switch len(ps) {
		case 0:
			return nil
		case 1:
			return ps[0]
		default:
			return predicates.NewAnd(ps...)
		}
	}
	return andOf(outer), andOf(inner)
}

// mintDistinctIdentifier mints a fresh CorrelationIdentifier whose
// UPPER-CASED name is DISTINCT from every name in visible. A quoted SQL
// alias can legally spell `"Q$N"`, so a raw `UniqueCorrelationIdentifier`
// could equal a user-visible name whenever the process-global counter
// happens to align — capturing that name's references (the inner-
// correlation mint) or colliding a subquery binding with a user alias at
// the translator (esq/scalar Alias — observed as a loud planner failure
// on a valid query). The retry loop makes the outcome history-
// INDEPENDENT: a colliding candidate is skipped (the counter advances),
// and any non-colliding candidate yields identical semantics regardless
// of its numeric suffix. Terminates because visible is finite and the
// counter is strictly increasing. next is injected for deterministic
// unit testing; production passes values.UniqueCorrelationIdentifier.
func mintDistinctIdentifier(visible map[string]struct{}, next func() values.CorrelationIdentifier) values.CorrelationIdentifier {
	for {
		candidate := next()
		if _, taken := visible[strings.ToUpper(candidate.Name())]; !taken {
			return candidate
		}
	}
}

// mintDistinctUpper is mintDistinctIdentifier's upper-cased-name form —
// the inner-correlation mint consumes the NAME (scope CorrelationName,
// scan alias, qualifyBareFields), which every consumer upper-cases.
func mintDistinctUpper(visible map[string]struct{}, next func() values.CorrelationIdentifier) string {
	return strings.ToUpper(mintDistinctIdentifier(visible, next).Name())
}

// hasNonInnerConjunct reports whether any top-level conjunct of pred fails
// to reference an inner FROM source — the class the existential rule routes
// to the OUTER input: correlated outer-only conjuncts AND reference-free
// ones (constants, parameters). Used for the Case-1 polarity flag;
// splitOuterOnlyConjuncts alone under-covers it because that split
// deliberately keeps reference-free conjuncts in rest.
func hasNonInnerConjunct(pred predicates.QueryPredicate, innerAliases map[string]struct{}) bool {
	if pred == nil {
		return false
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		for _, sub := range and.SubPredicates {
			if hasNonInnerConjunct(sub, innerAliases) {
				return true
			}
		}
		return false
	}
	for c := range predicates.GetCorrelatedToOfPredicate(pred) {
		if _, isInner := innerAliases[strings.ToUpper(c.Name())]; isInner {
			return false
		}
	}
	// A non-inner leaf is hazardous only if it can actually FILTER: a
	// statically-TRUE conjunct (`1 = 1`) outer-routes as a no-op, so
	// flagging it would over-decline semantics-neutral tautologies that
	// planned correctly before the guard. A statically-FALSE or
	// non-static leaf stays flagged — a routed FALSE drops every outer
	// row, the exact hazard. Static means BOTH comparison sides are
	// row-context-independent (IsConstantValue), so Eval with a nil
	// context is safe and deterministic.
	if cp, ok := pred.(*predicates.ComparisonPredicate); ok &&
		cp.Operand != nil && values.IsConstantValue(cp.Operand) &&
		(cp.Comparison.Operand == nil || values.IsConstantValue(cp.Comparison.Operand)) {
		if tv, err := cp.Eval(nil); err == nil && tv == predicates.TriTrue {
			return false
		}
	}
	return true
}

// scopeAmbiguousName returns the first correlation name in pred that is BOTH
// an inner leg name AND an ACTUALLY-BOUND outer name, or "" when none — the
// multi-source scope-ambiguity test (see the decline site in
// buildCorrelatedExists). The outer set is the RUNTIME-BOUND name per source
// — CorrelationName when present, else Alias — deliberately NOT the display
// set the ON-split's outerAliases uses: a minted middle carries
// {Alias: MID, CorrelationName: Q$N} and only Q$N binds at runtime, so an
// innermost leg re-declaring MID cannot collide with it; testing display
// names 0A000'd valid queries. Do not "unify" this with outerAliases — the
// two sets answer different questions (walk-time reference matching vs
// runtime binding collision).
func scopeAmbiguousName(pred predicates.QueryPredicate, innerLegNames map[string]struct{}, outerScopes []semantic.ScopeSource) string {
	if pred == nil || len(innerLegNames) == 0 {
		return ""
	}
	outerBound := map[string]struct{}{}
	for _, src := range outerScopes {
		n := src.CorrelationName
		if n == "" {
			n = src.Alias.Name()
		}
		if n != "" {
			outerBound[strings.ToUpper(n)] = struct{}{}
		}
	}
	for c := range predicates.GetCorrelatedToOfPredicate(pred) {
		n := strings.ToUpper(c.Name())
		_, isInner := innerLegNames[n]
		_, isBoundOuter := outerBound[n]
		if isInner && isBoundOuter {
			return n
		}
	}
	return ""
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

func (p *existsSubqueryPlanner) BuildScalar(q antlrgen.IQueryContext) (values.CorrelationIdentifier, values.Type, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, values.UnknownType, fmt.Errorf("scalar subquery: nil query context")
	}
	innerOp, err := buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.schemaName, p.cteScopes, p.cteOnScopes)

	isUndefinedCol := false
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeUndefinedColumn {
			isUndefinedCol = true
		}
	}
	if err != nil && (!isUndefinedCol || len(p.outerScopes) == 0) {
		return values.CorrelationIdentifier{}, values.UnknownType, err
	}
	if isUndefinedCol {
		alias, cerr := p.buildCorrelatedScalar(q)
		// The correlated arm materializes its result through the NLJ slot
		// machinery; its output type is not derivable from a logical plan
		// here — Unknown keeps the pre-threading gate behavior for it.
		return alias, values.UnknownType, cerr
	}

	if innerOp == nil {
		return values.CorrelationIdentifier{}, values.UnknownType, fmt.Errorf("scalar subquery: inner query could not be planned")
	}
	// If the inner plan references outer CTEs (from a WITH clause on the
	// enclosing query), wrap it with LogicalCTE nodes so the Cascades
	// translator's cteScope can resolve the scan. Without this, a scan
	// on a CTE name (e.g. SELECT MIN(v) FROM high) would be translated
	// as a table scan on a nonexistent table.
	innerOp = p.wrapWithOuterCTEs(innerOp)
	alias := p.mintSubqueryAlias()
	p.scalarSubqueries = append(p.scalarSubqueries, logical.ScalarSubquery{
		Alias: alias,
		Plan:  innerOp,
	})
	return alias, scalarSubqueryOutputType(innerOp), nil
}

// scalarSubqueryOutputType derives the single output column's cascades
// type from an uncorrelated scalar subquery's inner logical plan — the
// type ScalarSubqueryValue flows so the plan-time gates (comparison
// promotion 42804, cast pairs 22F3H) see the real type instead of the
// Unknown that exempted every scalar subquery from the gates a direct
// column reference hits. UnknownType when the shape is underivable (no
// false claims — Unknown keeps the gate-exempt behavior).
func scalarSubqueryOutputType(op logical.LogicalOperator) values.Type {
	switch o := op.(type) {
	case *logical.LogicalLimit:
		return scalarSubqueryOutputType(o.Input)
	case *logical.LogicalSort:
		return scalarSubqueryOutputType(o.Input)
	case *logical.LogicalFilter:
		return scalarSubqueryOutputType(o.Input)
	case *logical.LogicalProject:
		if len(o.Projections) == 1 && len(o.AggregateOutputOrdinals) == 1 {
			if agg := findAggregate(o.Input); agg != nil {
				ordinal := o.AggregateOutputOrdinals[0]
				switch {
				case ordinal >= 0 && ordinal < len(agg.GroupKeys):
					if v := agg.GroupKeys[ordinal].Value; v != nil && v.Type() != nil {
						return v.Type()
					}
				case ordinal >= len(agg.GroupKeys) && ordinal < len(agg.GroupKeys)+len(agg.Calls):
					callIdx := ordinal - len(agg.GroupKeys)
					var operand []values.Value
					if callIdx < len(agg.AggregateOperands) {
						operand = []values.Value{agg.AggregateOperands[callIdx]}
					}
					return aggregateCallOutputType(agg.Calls[callIdx], operand)
				}
			}
		}
		if len(o.Projections) == 1 && len(o.ProjectedValues) == 1 && o.ProjectedValues[0] != nil {
			if t := o.ProjectedValues[0].Type(); t != nil {
				return t
			}
		}
		return values.UnknownType
	case *logical.LogicalAggregate:
		if len(o.GroupKeys) == 0 && len(o.Calls) == 1 {
			return aggregateCallOutputType(o.Calls[0], o.AggregateOperands)
		}
		return values.UnknownType
	}
	return values.UnknownType
}

// aggregateCallOutputType maps an aggregate call to the DECLARED Java
// result type (nullable — Type.primitiveType defaults nullable=true;
// a scalar subquery with zero rows is NULL anyway). The code-level
// table is javaAggregateResultCode; a combination with no Java row —
// including a STRUCTURED operand code, which has no NumericAggregation
// operator and must never reach NewPrimitiveType's structured-code
// panic — reports UnknownType (gate-exempt, no false claims).
func aggregateCallOutputType(call logical.AggregateCall, operands []values.Value) values.Type {
	opCode := values.TypeCodeUnknown
	if len(operands) >= 1 && operands[0] != nil {
		if t := operands[0].Type(); t != nil {
			opCode = t.Code()
		}
	}
	if code, ok := javaAggregateResultCode(call.Func, opCode); ok {
		return values.NewPrimitiveType(code, true)
	}
	return values.UnknownType
}

// javaAggregateResultCode is THE Java aggregate result-type table at the
// TypeCode level (NumericAggregationValue / CountValue, tag 4.12.11.0):
// COUNT and COUNT(*) return LONG regardless of operand; AVG returns
// DOUBLE for every numeric operand; SUM/MIN/MAX return the OPERAND's
// code — and Java defines those operators ONLY over INT/LONG/FLOAT/
// DOUBLE (SUM_I/L/F/D, MIN_*, MAX_*), so any other operand code has no
// row (ok=false). Both this table's consumers document their own
// nullability choice at the call site; aggResultTypeFromFunc layers its
// metadata-specific divergences over the same table.
func javaAggregateResultCode(fn string, operandCode values.TypeCode) (values.TypeCode, bool) {
	return values.JavaAggregateResultCode(fn, operandCode)
}

// colBareOrName: the structured bare segment, or the whole name as one
// opaque label for computed/rebased entries — never a dot split.
func colBareOrName(c projCol) string {
	if c.bare != "" {
		return c.bare
	}
	return c.name
}

// resolveCorrelatedColumnValueStructured resolves a structured group key of a
// correlated scalar subquery's inner aggregate through the semantic scope —
// ONE channel for single-source and join inners alike: a multi-source scope
// emits the QOV-addressed born-baked form, which the executor binds through
// the merged row's leg windows (rowLegsBinder). The flat merged-row-display
// mint the join case used to return is retired (dead-in-effect across all
// suites once the scope channel answered; the correlated join-inner
// column-agg FDB pin proves qualified AND bare shapes end-to-end).
func resolveCorrelatedColumnValueStructured(resolver *expr.Resolver, key logical.GroupKey) (values.Value, error) {
	bare := key.Bare
	if bare == "" {
		bare = key.Display
	}
	var qualifier semantic.Identifier
	if key.Qualified {
		qualifier = semantic.FromNormalized(key.Qualifier)
	}
	return resolver.ResolveIdentifier(qualifier, semantic.FromNormalized(bare))
}

// resolveCorrelatedColumnValue resolves a (possibly alias-qualified) column
// reference to a Value through the semantic scope — the same resolution the
// correlated WHERE clause uses, for single-source and join inners alike
// (the multi-source scope emits the QOV-addressed born-baked form; the
// executor binds it through the merged row's leg windows). Segments come
// STRUCTURED from the parse tree (AggregateCall.aggArgBare/Qualifier —
// WS-N slice 6: never a dot re-split of rendered text). A genuinely
// unresolvable column returns the resolver error so the caller can reject —
// silently falling back to a raw FieldValue would group every row under a
// null key (wrong results).
func resolveCorrelatedColumnValue(resolver *expr.Resolver, bare, qual string, qualified bool) (values.Value, error) {
	var qualifier semantic.Identifier
	if qualified {
		qualifier = semantic.FromNormalized(qual)
	}
	return resolver.ResolveIdentifier(qualifier, semantic.FromNormalized(bare))
}

// resolveCorrelatedGroupKeyValues resolves the GROUP BY keys of a correlated
// scalar subquery's inner aggregate to Value trees. The builder stores group
// keys as raw (often qualified) column-name strings with no expression context
// (groupByExprs nil), so resolve each name through the semantic scope rather
// than walking a parse node. An expression key (e.g. GROUP BY o.a + o.b) that
// fails to resolve is returned as an error — matching the top-level path
// (upgradeAggregate) — rather than silently falling back to an unresolvable
// raw FieldValue that would group every row under a null key.
func resolveCorrelatedGroupKeyValues(agg *logical.LogicalAggregate, sq *selectQuery, resolver *expr.Resolver) error {
	if agg == nil || len(agg.GroupKeys) == 0 {
		return nil
	}
	keyValues := make([]values.Value, len(agg.GroupKeys))
	for i, key := range agg.GroupKeys {
		if i < len(sq.groupBy) && sq.groupBy[i].expr != nil {
			v, err := resolver.WalkExpressionForProjection(sq.groupBy[i].expr)
			if err != nil {
				return err
			}
			keyValues[i] = v
			continue
		}
		v, err := resolveCorrelatedColumnValueStructured(resolver, key)
		if err != nil {
			return err
		}
		keyValues[i] = v
	}
	for i := range agg.GroupKeys {
		if keyValues[i] != nil {
			agg.GroupKeys[i].Value = keyValues[i]
		}
	}
	return nil
}

// resolveCorrelatedVisibleGroupKeyOrdinal binds a selected grouping-column
// reference to the exact private aggregate key slot after both sides have been
// resolved through the same semantic scope. Parse spelling is deliberately not
// an identity channel here: on a single source `SELECT status GROUP BY o.status`
// differs only by a redundant qualifier, while joined `a.k` and `b.k` must
// remain different despite their shared bare label. Repeated keys that resolve
// to the same producer value are interchangeable and deterministically use the
// first native slot.
func resolveCorrelatedVisibleGroupKeyOrdinal(
	agg *logical.LogicalAggregate,
	ac *aggSelectCol,
	resolver *expr.Resolver,
) (int, error) {
	if agg == nil || ac == nil || resolver == nil || ac.groupColBare == "" {
		return -1, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar grouping-key output has no structural identity")
	}
	selected, err := resolveCorrelatedColumnValue(
		resolver,
		ac.groupColBare,
		ac.groupColQualifier,
		ac.groupColQualified,
	)
	if err != nil {
		return -1, err
	}
	first := -1
	for i, key := range agg.GroupKeys {
		if key.Value == nil {
			continue
		}
		if values.SemanticEqualsUnderAliasMap(selected, key.Value, values.AliasMap{}) ||
			fieldValueMatchesAggregateGroupKey(selected, key.Value, agg) {
			if first < 0 {
				first = i
				continue
			}
			// Multiple native slots are safe only when they are themselves
			// the same resolved producer value.
			firstValue := agg.GroupKeys[first].Value
			if !values.SemanticEqualsUnderAliasMap(firstValue, key.Value, values.AliasMap{}) &&
				!fieldValueMatchesAggregateGroupKey(firstValue, key.Value, agg) {
				return -1, api.NewError(api.ErrCodeUnsupportedQuery,
					"correlated scalar grouping-key output matches multiple native producer values")
			}
		}
	}
	if first < 0 {
		return -1, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar grouping-key output could not be bound to the native aggregate row")
	}
	return first, nil
}

// groupedScalarSortKeys binds ORDER BY on a correlated scalar's grouped output
// to the exact native [keys...,calls...] ordinal. Positional keys and bare
// output aliases use their SQL output contract; every source/group/aggregate
// expression is otherwise walked through the semantic scope and structurally
// rebound against the resolved aggregate producer. Names are diagnostics only.
func groupedScalarSortKeys(
	sq *selectQuery,
	agg *logical.LogicalAggregate,
	outputOrdinals map[string]int,
	resolver *expr.Resolver,
) ([]logical.SortKey, error) {
	keys := make([]logical.SortKey, 0, len(sq.orderBy))
	for _, ob := range sq.orderBy {
		ordinal := -1
		// A positional ORDER BY item is an output ordinal by definition. The
		// scalar shape has one visible output, but bind through OutputSlots
		// rather than relying on that fact so the private aggregate ABI remains
		// explicit and self-checking.
		if ob.pos > 0 && agg != nil {
			for _, slot := range agg.OutputSlots {
				if slot.SelectOrdinal == ob.pos {
					ordinal = slot.NativeOrdinal
					break
				}
			}
		}
		// SQL output-alias precedence applies only to a bare one-segment key.
		if ordinal < 0 && ob.bareRef {
			if v, exists := outputOrdinals[strings.ToUpper(ob.colName)]; exists {
				ordinal = v
			}
		}

		// Source/group/aggregate expressions bind through the same structural
		// producer contract as post-aggregate SELECT expressions. This handles
		// redundant single-source qualification in either direction, computed
		// grouping keys, qualified aggregate operands, and same-bare joined
		// keys without a render-and-reparse heuristic.
		if ordinal < 0 && ob.rawExpr != nil {
			if resolver == nil {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"grouped correlated scalar ORDER BY has no semantic resolver")
			}
			walked, walkErr := resolver.WalkExpression(ob.rawExpr)
			if walkErr != nil {
				return nil, walkErr
			}
			bound, bindErr := bindPostAggregateValueToNativeOrdinals(walked, agg)
			if bindErr == nil {
				if fv, ok := bound.(*values.FieldValue); ok &&
					fv.Child == nil &&
					fv.Resolved != nil &&
					len(fv.Resolved.Accessors) == 1 {
					ordinal = fv.Resolved.Accessors[0].Ordinal
				}
			}
		}
		nativeWidth := 0
		if agg != nil {
			nativeWidth = len(agg.GroupKeys) + len(agg.Calls)
		}
		if ordinal < 0 || ordinal >= nativeWidth {
			return nil, api.NewErrorf(api.ErrCodeGroupingError,
				"ORDER BY %q must reference a grouping column or a selected aggregate in a grouped correlated scalar subquery", ob.colName)
		}
		if ordinal >= len(agg.GroupKeys) {
			selectedAggregate := false
			for _, slot := range agg.OutputSlots {
				if slot.NativeOrdinal == ordinal {
					selectedAggregate = true
					break
				}
			}
			if !selectedAggregate {
				return nil, api.NewErrorf(api.ErrCodeGroupingError,
					"ORDER BY %q must reference a grouping column or a selected aggregate in a grouped correlated scalar subquery", ob.colName)
			}
		}
		dir := logical.SortAsc
		if !ob.ascending {
			dir = logical.SortDesc
		}
		nativeName := aggregateNativeOutputName(agg, ordinal)
		sk := logical.SortKey{
			Expr:                      nativeName,
			Dir:                       dir,
			AggregateOutputOrdinal:    ordinal,
			HasAggregateOutputOrdinal: true,
		}
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
	userPagination, paginationErr := correlatedScalarHasResolvedPagination(q)
	if paginationErr != nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated scalar subquery: %v", paginationErr), Cause: paginationErr,
		}
	}
	if sq.distinct {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: "correlated scalar subquery: SELECT DISTINCT is not yet supported",
			Cause: api.NewError(api.ErrCodeUnsupportedQuery,
				"SELECT DISTINCT in a correlated scalar subquery is not yet supported"),
		}
	}
	if queryScopeHasWindowedAggregate(body) || sq.qualifyExpr != nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: "correlated scalar subquery: window functions are not yet supported",
			Cause: api.NewError(api.ErrCodeUnsupportedQuery,
				"window functions in a correlated scalar subquery are not yet supported"),
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
	aliasID := semantic.FromNormalized(innerAlias)
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
		right, rErr := p.correlatedSubqueryJoinRight(j, sq.tableName, innerAlias, sq.joins[:i])
		if rErr != nil {
			return values.CorrelationIdentifier{}, rErr
		}
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
	// The current non-strict LEFT-scalar join null-fills an empty inner but does
	// not collapse multiple post-pagination rows. LIMIT 0/1 therefore has an
	// exact lowering for every shape. A larger limit is also exact for a
	// non-grouped real aggregate, whose pre-pagination cardinality is already
	// <=1; for data-dependent rows/groups it could fan one outer row into
	// several. Preserve the written limit by declining those multi-row-capable
	// shapes typed-loud until a post-page scalar-collapse mode exists; never clamp
	// with a hidden LIMIT 1.
	if userPagination && sq.limit > 1 && (!hasRealAgg || len(sq.groupBy) > 0) {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: "correlated scalar subquery: LIMIT greater than 1 requires post-pagination scalar collapse",
			Cause: api.NewError(api.ErrCodeUnsupportedQuery,
				"LIMIT greater than 1 in a correlated scalar subquery is not yet supported"),
		}
	}
	// The group-key-only branch materializes grouping keys but has no HAVING
	// rewrite/bake onto that aggregate output. Letting it proceed would silently
	// ignore HAVING and then run the strict cardinality probe against the wrong
	// (pre-HAVING) group set. Real-aggregate HAVING is lowered below; this
	// distinct shape remains correct-or-loud until it has the same output rewrite.
	if !hasRealAgg && sq.havingExpr != nil {
		return values.CorrelationIdentifier{}, &CorrelatedExistsError{
			Message: "correlated scalar subquery: HAVING over a group-key-only projection is not yet supported",
			Cause: api.NewError(api.ErrCodeUnsupportedQuery,
				"HAVING over a group-key-only correlated scalar subquery is not yet supported"),
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
		if _, echo := visAggNames[strings.ToUpper(pc.name)]; !echo {
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
	scalarNativeOrdinal := -1
	// strictSingle is set whenever a data-dependent inner can emit more than one
	// row and the user did not write a LIMIT. The lowering then enforces
	// at-most-one via a strict FirstOrDefault.
	var strictSingle bool
	if hasRealAgg {
		// Build the aggregate over the correlated filter. With GROUP BY the
		// aggregate may emit more than one group, so an uncapped scalar must use
		// the same strict FirstOrDefault cardinality barrier as a non-aggregate
		// scalar: a second group is SQLSTATE 21000, never an implicit first-group
		// choice. Empty input => zero groups => NULL falls out naturally, whereas
		// the no-GROUP-BY scalar aggregate emits one row (e.g. COUNT=0).
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
		var aggAliases []string
		var aggCalls []logical.AggregateCall
		var aggOperands []values.Value
		aggSeen := make(map[string]struct{})
		exprAggNames := make(map[string]struct{}) // join-path collision tracking only
		// Match-name (uppercased SELECT alias / source FN(bareArg) /
		// canonical) → exact native aggregate-call ordinal, for ORDER BY.
		aggDatumOrdinal := make(map[string]int)
		aggCallOrdinalByName := make(map[string]int)
		addAgg := func(fn, arg, argBare, argQual string, argQualified bool, e antlrgen.IExpressionContext, distinct bool) (string, int, error) {
			// An expression argument has no bare column name, so it collapses to
			// FN(*). Two DISTINCT expression aggregates (e.g. SUM(a+b) projected
			// and SUM(c*d) in HAVING) would both synthesize "SUM(*)" and the
			// second would silently overwrite the first — so the HAVING would
			// read the projected aggregate's value. We cannot disambiguate them
			// by name, so reject rather than return wrong rows.
			bareArg := argBare
			if bareArg == "" && arg != "" {
				bareArg = arg
			}
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
					return "", -1, err
				}
				opVal = v
			} else if arg != "" {
				b := argBare
				if b == "" {
					b = arg
				}
				v, err := resolveCorrelatedColumnValue(resolver, b, argQual, argQualified)
				if err != nil {
					return "", -1, err
				}
				opVal = v
			}
			// DISTINCT aggregates are unsupported here (aggDistinct is not threaded
			// into the materialised slot, and COUNT(DISTINCT 1) != COUNT(*)). Reject
			// explicitly rather than rely on a name-prefix check.
			if distinct {
				return "", -1, fmt.Errorf("DISTINCT aggregate not supported in a correlated scalar subquery")
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
					return cname, aggCallOrdinalByName[cname], nil
				}
				callOrdinal := len(aggCalls)
				aggSeen[cname] = struct{}{}
				aggCallOrdinalByName[cname] = callOrdinal
				aggCalls = append(aggCalls, logical.AggregateCall{
					Func:       strings.ToUpper(fn),
					Operand:    canonicalAggOperandText(cname),
					Star:       opVal == nil,
					BareColumn: e == nil && arg != "",
				})
				aggAliases = append(aggAliases, cname)
				aggOperands = append(aggOperands, opVal)
				return cname, callOrdinal, nil
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
					return "", -1, fmt.Errorf("an expression-argument aggregate (e.g. SUM(<expr>)) collides with another aggregate named %q; not supported in a correlated scalar subquery", name)
				}
				// Identical bare-column / star aggregate referenced twice (e.g.
				// COUNT(*) in both SELECT and HAVING) — safe to reuse the slot.
				// (Any expression/constant arg is opaque and exited above, so
				// this dup is always a non-opaque, identically-named aggregate.)
				return name, aggCallOrdinalByName[name], nil
			}
			callOrdinal := len(aggCalls)
			aggSeen[name] = struct{}{}
			aggCallOrdinalByName[name] = callOrdinal
			if opaqueExpr {
				exprAggNames[name] = struct{}{}
			}
			aggCalls = append(aggCalls, logical.AggregateCall{
				Func:       strings.ToUpper(fn),
				Operand:    strings.ToUpper(bareArg),
				Star:       bareArg == "*",
				BareColumn: e == nil && arg != "",
			})
			aggAliases = append(aggAliases, name)
			aggOperands = append(aggOperands, opVal)
			return name, callOrdinal, nil
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
			name, callOrdinal, err := addAgg(ac.aggFunc, ac.aggArg, ac.aggArgBare, ac.aggArgQualifier, ac.aggArgQualified, ac.aggExpr, ac.aggDistinct)
			if err != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: resolve aggregate argument: %v", err), Cause: err,
				}
			}
			if ac.visible {
				scalarCol = name
				scalarNativeOrdinal = len(sq.groupBy) + callOrdinal
				// Record the native ordinal under every form an ORDER BY
				// might name it.
				aggDatumOrdinal[strings.ToUpper(name)] = scalarNativeOrdinal
				if ac.outName != "" {
					aggDatumOrdinal[strings.ToUpper(ac.outName)] = scalarNativeOrdinal
				}
				if bareArg := ac.aggArgBare; bareArg != "" {
					aggDatumOrdinal[strings.ToUpper(ac.aggFunc+"("+bareArg+")")] = scalarNativeOrdinal
				}
			}
		}
		// A sole COUNT(*) the parser flagged via countStar (no aggCol entry).
		if sq.countStar {
			name, callOrdinal, _ := addAgg("COUNT", "", "", "", false, nil, false) // -> COUNT(*)
			scalarCol = name
			scalarNativeOrdinal = len(sq.groupBy) + callOrdinal
			aggDatumOrdinal[strings.ToUpper(name)] = scalarNativeOrdinal
			if sq.countStarAlias != "" {
				aggDatumOrdinal[strings.ToUpper(sq.countStarAlias)] = scalarNativeOrdinal
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
		var visibleGroupCol *aggSelectCol
		if scalarCol == "" {
			for i := range sq.aggCols {
				if sq.aggCols[i].visible && sq.aggCols[i].aggFunc == "" && sq.aggCols[i].groupCol != "" {
					gcBare := sq.aggCols[i].groupColBare
					if gcBare == "" {
						gcBare = sq.aggCols[i].groupCol
					}
					scalarCol = strings.ToUpper(gcBare)
					visibleGroupCol = &sq.aggCols[i]
					break
				}
			}
		}
		if scalarCol == "" {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: "correlated scalar subquery: expected an aggregate function or grouping-key projection",
			}
		}
		groupKeys := logicalGroupKeys(sq.groupBy)
		aggOp := logical.NewAggregate(innerOp, groupKeys, aggCalls, aggAliases, false)
		aggOp.AggregateOperands = aggOperands
		if gkErr := resolveCorrelatedGroupKeyValues(aggOp, sq, resolver); gkErr != nil {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: fmt.Sprintf("correlated scalar subquery: resolve GROUP BY key: %v", gkErr), Cause: gkErr,
			}
		}
		if scalarNativeOrdinal < 0 && visibleGroupCol != nil {
			var ordinalErr error
			scalarNativeOrdinal, ordinalErr = resolveCorrelatedVisibleGroupKeyOrdinal(
				aggOp, visibleGroupCol, resolver,
			)
			if ordinalErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: bind visible GROUP BY key: %v", ordinalErr),
					Cause:   ordinalErr,
				}
			}
			if visibleGroupCol.outName != "" {
				aggDatumOrdinal[strings.ToUpper(visibleGroupCol.outName)] = scalarNativeOrdinal
			}
			aggDatumOrdinal[strings.ToUpper(visibleGroupCol.groupCol)] = scalarNativeOrdinal
		}
		if scalarNativeOrdinal < 0 ||
			scalarNativeOrdinal >= len(aggOp.GroupKeys)+len(aggOp.Calls) {
			return values.CorrelationIdentifier{}, &CorrelatedExistsError{
				Message: "correlated scalar subquery: visible grouped output has no exact native ordinal",
				Cause: api.NewError(api.ErrCodeUnsupportedQuery,
					"correlated scalar subquery grouped output could not be bound positionally"),
			}
		}
		aggOp.OutputSlots = []logical.AggregateOutputSlot{{
			SelectOrdinal: 1,
			NativeOrdinal: scalarNativeOrdinal,
		}}
		if sq.havingExpr != nil {
			havingPred, hErr := resolver.WalkPredicate(sq.havingExpr)
			if hErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: walk HAVING: %v", hErr), Cause: hErr,
				}
			}
			aggOp.HavingPredicate = rewriteAggregateRefsInPredicate(havingPred, aggOp)
		}
		innerOp = aggOp
		// ORDER BY over the grouped output: sort the groups before an explicit
		// user LIMIT (when present). Keys are canonicalised to the exact
		// post-aggregate datum keys (RFC-085).
		if len(sq.orderBy) > 0 {
			sortKeys, skErr := groupedScalarSortKeys(sq, aggOp, aggDatumOrdinal, resolver)
			if skErr != nil {
				return values.CorrelationIdentifier{}, skErr
			}
			innerOp = logical.NewSort(innerOp, sortKeys)
		}
		// Materialize the one SQL-visible scalar after grouped ORDER BY and
		// before pagination. The correlated-scalar lowering peels a root Limit
		// and reattaches it per outer row, so Project must be the Limit's input,
		// not its parent. The seed then reads ordinal 0 from a proven one-field
		// row even when the selected value lives after native grouping keys.
		scalarType := aggregateNativeOutputType(aggOp, scalarNativeOrdinal)
		scalarValue := values.NewFieldValueWithResolvedOrdinal(
			aggregateNativeOutputName(aggOp, scalarNativeOrdinal),
			scalarNativeOrdinal,
			scalarType,
		)
		scalarProj := logical.NewProject(innerOp, []string{scalarCol}, nil)
		scalarProj.ProjectedValues = []values.Value{scalarValue}
		scalarProj.IsComputed = []bool{false}
		scalarProj.AggregateOutputOrdinals = []int{scalarNativeOrdinal}
		innerOp = scalarProj
		// Accepted user LIMIT/OFFSET is deliberate pagination and is preserved
		// exactly. A grouped aggregate accepts only LIMIT 0/1 here, so the
		// post-page result is <=1 and needs no strict probe. A global aggregate,
		// with or without HAVING, is intrinsically <=1 and safely accepts a larger
		// limit too. Without pagination, GROUP BY is the only real-aggregate shape
		// capable of producing multiple rows, so leave it uncapped and strict.
		if userPagination {
			innerOp = logical.NewLimit(innerOp, sq.limit, sq.offset)
		} else if len(sq.groupBy) > 0 {
			strictSingle = true
		}
	} else {
		// Non-aggregate correlated scalar subquery. The single output column is
		// either a plain projected column or, under a GROUP BY, a bare
		// group-key projection stored as a visible aggCol (DISTINCT-of-key).
		// computedScalarVal is set for a COMPUTED projection (`UPPER(x)`, `a+b`);
		// it is materialized as the inner's projected output AFTER the sort/limit
		// below.
		var computedScalarVal values.Value
		var visibleGroupCol *aggSelectCol
		// classifyProjFieldValue routes a resolved single-column projection by
		// the SCOPE its reference binds: an OUTER-scoped field is NOT an inner
		// row key and must take the materialized path (its value comes from
		// the outer binding, evaluated per outer row); an inner-scoped field
		// keys the inner row (qualified for a join's merged row, bare for a
		// single source). Shared by the walked-expression arm and the plain
		// column arm so both spellings of the same reference classify
		// identically.
		classifyProjFieldValue := func(fv *values.FieldValue) {
			innerScoped, alias := true, ""
			if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
				alias = strings.ToUpper(qov.Correlation.Name())
			} else if ref := parseColRef(fv.Field); ref.isQualified() {
				alias = strings.ToUpper(ref.table)
			}
			if alias != "" {
				_, innerScoped = innerSourceAliases(op)[alias]
			}
			switch {
			case !innerScoped:
				computedScalarVal = fv
			case len(sq.joins) > 0:
				if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
					scalarCol = strings.ToUpper(qov.Correlation.Name()) + "." + strings.ToUpper(fv.Field)
				} else {
					scalarCol = strings.ToUpper(fv.Field)
				}
			default:
				scalarCol = strings.ToUpper(parseColRef(fv.Field).bare())
			}
		}
		switch {
		case len(sq.projCols) == 1:
			// A COMPUTED projection (`SELECT UPPER(x)`, `a+b`, `CAST(...)`) is NOT
			// a stored inner column, so — unlike a plain column ref — it is not
			// present in the inner row. Left as-is it resolves to nothing → a
			// SILENT NULL. Walk it here and MATERIALIZE it as
			// the inner's single projected output below (positional key `_0`,
			// mirroring Java's inner SelectExpression.resultValue). A plain column
			// ref keeps the existing bare/qualified path.
			//
			// A qualified plain projection (`SELECT o.amount`) must resolve to the
			// bare datum key the inner row carries. For a single inner table the
			// row is keyed bare (`AMOUNT`) and replaceScalarSubqueryRef
			// re-qualifies under the inner alias (`O.AMOUNT`) at read time — a
			// scalarCol that kept the `o.` qualifier would double-prefix to
			// `O.O.AMOUNT` and resolve to NULL (same failure mode the bare
			// group-key case below guards). For a join the row is keyed
			// qualified (see :910), so keep the qualifier there.
			if len(sq.projExprs) > 0 && sq.projExprs[0] != nil {
				cv, wErr := resolver.WalkExpression(sq.projExprs[0])
				if wErr != nil {
					return values.CorrelationIdentifier{}, &CorrelatedExistsError{
						Message: fmt.Sprintf("correlated scalar subquery: walk computed projection: %v", wErr), Cause: wErr,
					}
				}
				// A walked value that is a BARE column reference (a parenthesized
				// column like `(o.amount)`) is NOT a computation: the projection
				// executor keys field-valued projections by the COLUMN NAME, never
				// the positional `_0`, so materializing it would leave the seed
				// reading a key the row does not carry (review finding). Keep it
				// on the plain-column path — scalarCol comes from the WALKED
				// field's resolved name (the projection text may carry parens the
				// textual parse would garble: `(o.amount)` → `AMOUNT)`).
				//
				// A JOIN-inner keys its rows QUALIFIED, so the key must carry the
				// resolved qualifier (review finding, round 2): a bared key rides
				// the merged row's bare last-leg-wins TWIN and silently reads the
				// WRONG LEG when both legs carry the column (`(o.id)` returned
				// items.id). Derive `ALIAS.COL` from the walked value's QOV child;
				// a flat dotted field passes verbatim.
				//
				// Only INNER-scope fields are inner row keys at all (review
				// finding, round 3): an OUTER-scope parenthesized column
				// (`(c.id)`) bared to `ID` silently read the INNER column of the
				// same name (the order id, not the customer id). An outer-scoped
				// FieldValue takes the MATERIALIZED path like any computation —
				// its value comes from the outer binding, evaluated per outer row.
				if fv, isFV := cv.(*values.FieldValue); isFV {
					classifyProjFieldValue(fv)
				} else {
					computedScalarVal = cv
				}
			} else {
				// A PLAIN (unparenthesized) column has no expression context,
				// so it never reached the walk above — the old text path
				// derived scalarCol from the projection TEXT without ever
				// asking WHICH SCOPE the qualifier binds. An OUTER-scoped
				// plain column (`SELECT a.id FROM q AS z WHERE …`) then read
				// the INNER row's slot of that name — the seed's
				// ofOrdinal(inner, 0) served the inner's first column as the
				// scalar (silent wrong rows; the parenthesized twin `(a.id)`
				// was already fixed by the walked arm above). Resolve the
				// column through the semantic scope (inner first, outer
				// fallthrough — SQL scoping) and run the SAME classification:
				// outer-scoped materializes, inner-scoped keys the inner row.
				pc := sq.projCols[0]
				if rv, rErr := resolveCorrelatedColumnValue(resolver, colBareOrName(pc), pc.qualifier, pc.qualified); rErr == nil {
					if fv, isFV := rv.(*values.FieldValue); isFV {
						classifyProjFieldValue(fv)
					}
				}
			}
			if computedScalarVal == nil && scalarCol == "" {
				if len(sq.joins) > 0 {
					scalarCol = strings.ToUpper(sq.projCols[0].name)
				} else {
					scalarCol = strings.ToUpper(colBareOrName(sq.projCols[0]))
				}
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
					gcBare := sq.aggCols[i].groupColBare
					if gcBare == "" {
						gcBare = sq.aggCols[i].groupCol
					}
					scalarCol = strings.ToUpper(gcBare)
					visibleGroupCol = &sq.aggCols[i]
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
		var groupedKeyAgg *logical.LogicalAggregate
		groupedKeyNativeOrdinal := -1
		groupedOutputOrdinals := make(map[string]int)
		if len(sq.groupBy) > 0 {
			groupKeys := logicalGroupKeys(sq.groupBy)
			aggOp := logical.NewAggregate(innerOp, groupKeys, nil, nil, false)
			if gkErr := resolveCorrelatedGroupKeyValues(aggOp, sq, resolver); gkErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: resolve GROUP BY key: %v", gkErr), Cause: gkErr,
				}
			}
			if visibleGroupCol == nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: "correlated scalar subquery: grouping-key projection has no structural output identity",
					Cause: api.NewError(api.ErrCodeUnsupportedQuery,
						"correlated scalar subquery grouped output could not be bound positionally"),
				}
			}
			ordinal, ordinalErr := resolveCorrelatedVisibleGroupKeyOrdinal(
				aggOp, visibleGroupCol, resolver,
			)
			if ordinalErr != nil {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: fmt.Sprintf("correlated scalar subquery: bind visible GROUP BY key: %v", ordinalErr),
					Cause:   ordinalErr,
				}
			}
			if ordinal < 0 || ordinal >= len(groupKeys) {
				return values.CorrelationIdentifier{}, &CorrelatedExistsError{
					Message: "correlated scalar subquery: visible grouping key has no exact native ordinal",
					Cause: api.NewError(api.ErrCodeUnsupportedQuery,
						"correlated scalar subquery grouped output could not be bound positionally"),
				}
			}
			groupedKeyNativeOrdinal = ordinal
			aggOp.OutputSlots = []logical.AggregateOutputSlot{{
				SelectOrdinal: visibleGroupCol.selectOrdinal,
				NativeOrdinal: groupedKeyNativeOrdinal,
			}}
			if visibleGroupCol.outName != "" {
				groupedOutputOrdinals[strings.ToUpper(visibleGroupCol.outName)] = groupedKeyNativeOrdinal
			}
			groupedKeyAgg = aggOp
			innerOp = aggOp
		}

		// Add ORDER BY if present.
		if len(sq.orderBy) > 0 {
			if len(sq.groupBy) > 0 {
				// GROUP BY (group keys only): the sort runs over the POST-aggregate
				// row, whose keys are bare-uppercased — raw ob.colName (original case,
				// possibly qualified) would miss and sort every row equal. Canonicalise
				// to the exact group-key datum keys (RFC-085). No aggregates here.
				sortKeys, skErr := groupedScalarSortKeys(sq, groupedKeyAgg, groupedOutputOrdinals, resolver)
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
					if len(sq.joins) == 0 && ob.bare != "" {
						keyExpr = ob.bare
					}
					keys[i] = logical.SortKey{Expr: keyExpr, Dir: dir}
					if ob.nullsFirst != nil {
						keys[i].NullsFirst = *ob.nullsFirst
					}
				}
				innerOp = logical.NewSort(innerOp, keys)
			}
		}

		// A zero-call aggregate has the same private [keys..., calls...] ABI as
		// every other aggregate. Expose only the selected grouping-key slot,
		// after sorting but before pagination, so duplicate bare names from
		// joined sources cannot affect either the sort or scalar seed.
		if groupedKeyAgg != nil {
			scalarValue := values.NewFieldValueWithResolvedOrdinal(
				aggregateNativeOutputName(groupedKeyAgg, groupedKeyNativeOrdinal),
				groupedKeyNativeOrdinal,
				aggregateNativeOutputType(groupedKeyAgg, groupedKeyNativeOrdinal),
			)
			scalarProj := logical.NewProject(innerOp, []string{scalarCol}, nil)
			scalarProj.ProjectedValues = []values.Value{scalarValue}
			scalarProj.IsComputed = []bool{false}
			scalarProj.AggregateOutputOrdinals = []int{groupedKeyNativeOrdinal}
			innerOp = scalarProj
		}

		// SQL standard: scalar subquery must return at most 1 row.
		// An accepted user-written LIMIT (0/1; larger limits decline above) is
		// deliberate truncation intent — preserve it and do NOT enforce strict
		// cardinality. With NO user LIMIT,
		// leave the inner UNCAPPED and mark StrictSingle: the lowering then enforces
		// at-most-one via a strict FirstOrDefault barrier (a second inner row → 21000),
		// rather than a silent LIMIT 1 truncation (which the planner could also push
		// into the scan as a returned-row limit, bypassing the check).
		if userPagination {
			innerOp = logical.NewLimit(innerOp, sq.limit, sq.offset)
		} else {
			// No user LIMIT (and, in this grammar, therefore no OFFSET either — a
			// scalar subquery's OFFSET requires a LIMIT; `… OFFSET n` alone is a
			// 42601 syntax error, and there is no LIMIT ALL — so sq.offset is 0 here).
			strictSingle = true
		}

		// COMPUTED scalar: materialize the walked expression
		// as the inner's single projected output — positional key `_0`, mirroring
		// Java's inner SelectExpression.resultValue. Placed AFTER sort/limit so
		// ORDER BY keys resolved over the source rows (the projection drops them).
		// Both the name-model scalar ref (<inner>._0) and the ordinal seed
		// (ofOrdinal(inner,0)) resolve the computed value, so it never reads as NULL.
		if computedScalarVal != nil {
			proj := logical.NewProject(innerOp, []string{sq.projCols[0].name}, []string{""})
			proj.ProjectedValues = []values.Value{computedScalarVal}
			proj.IsComputed = []bool{true}
			innerOp = proj
			// The seed must read the EXACT key the projection executor writes
			// (the shared naming contract): `_0` is only emitted for
			// non-FieldValue projections; a FIELD-VALUED materialized slot (an
			// outer-scope column like `(c.id)`) is keyed by its column name.
			if _, isFV := computedScalarVal.(*values.FieldValue); isFV {
				scalarCol = values.ProjectionColumnName(computedScalarVal)
			} else {
				scalarCol = "_0"
			}
		}
	}

	alias := p.mintSubqueryAlias()
	p.correlatedScalarSubqueries = append(p.correlatedScalarSubqueries, logical.CorrelatedScalarSubquery{
		Alias:        alias,
		InnerPlan:    innerOp,
		InnerAlias:   strings.ToUpper(innerAlias),
		ScalarCol:    scalarCol,
		StrictSingle: strictSingle,
	})
	return alias, nil
}

// correlatedScalarHasResolvedPagination distinguishes an absent LIMIT from a
// clause whose atom is still unresolved in this planning invocation.
// parseLimitClause uses the same -1 sentinel for both, which would otherwise
// misclassify `LIMIT ?` as "no user LIMIT" and install a strict probe over the
// unpaginated input. Public driver bindings are substituted before parsing and
// therefore arrive here as resolved literals.
func correlatedScalarHasResolvedPagination(q antlrgen.IQueryContext) (bool, error) {
	if q == nil {
		return false, nil
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return false, nil
	}
	simpleTable, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return false, nil
	}
	limitClause := simpleTable.LimitClause()
	if limitClause == nil {
		return false, nil
	}
	for _, atom := range limitClause.AllLimitClauseAtom() {
		if _, resolved, atomErr := resolveLimitAtom(atom); atomErr != nil {
			return false, atomErr
		} else if !resolved {
			return false, api.NewError(api.ErrCodeUnsupportedQuery,
				"a correlated scalar subquery with a planning-time unresolved LIMIT/OFFSET is not supported")
		}
	}
	return true, nil
}

// innerSourceAliases collects the UPPER source aliases a correlated scalar
// subquery's scan/join tree binds — the universe that discriminates an
// INNER-scope projected field from an OUTER-scope one (the latter is not an
// inner row key and must take the materialized path; see the review-finding
// comment at the caller).
//
// This collector must be BINDER-EXACT, not merely inclusive: an
// over-inclusion flips an outer field to inner-scoped and skips its
// materialization (a wrong-key read). Its query-package twin
// (outerSubtreeAliases) is deliberately MORE inclusive — there an extra entry
// only pushes toward a decline or a skipped classification, never wrong rows.
// The asymmetry is load-bearing; do not harmonize the two collectors.
func innerSourceAliases(op logical.LogicalOperator) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if op == nil {
			return
		}
		switch o := op.(type) {
		case *logical.LogicalScan:
			a := o.Alias
			if a == "" {
				a = o.Table
			}
			out[strings.ToUpper(a)] = struct{}{}
		case *logical.LogicalUnnest:
			// Mirror the unnest BINDER's correlation rule
			// (unnestSourceCorrelation): the source correlation is the AS
			// alias, falling back to the AT alias only in the AT-only form.
			// With `AS v AT c` the ordinal alias `c` is NOT a source — it is a
			// column bound THROUGH v's row — so a same-named OUTER alias must
			// still classify as outer-scoped (review finding: adding the AT
			// alias here skipped materialization of an outer `(c.id)`).
			switch {
			case o.Alias != "":
				out[strings.ToUpper(o.Alias)] = struct{}{}
			case o.AtAlias != "":
				out[strings.ToUpper(o.AtAlias)] = struct{}{}
			}
		}
		for _, c := range op.Children() {
			walk(c)
		}
	}
	walk(op)
	return out
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

// sortOwnedBySelect reports whether sort is THIS select shell's own sort:
// reachable from the select's projection through row-preserving single-child
// operators only. A Sort below another Project/Aggregate belongs to a NESTED
// select (derived table / CTE body); rewriting its ordinals against the
// OUTER projection would swap in an unrelated item — `SELECT total FROM
// (SELECT id AS x, SUM(score) AS total … ORDER BY 1 …) d` must keep the
// inner ordinal on inner item 1, never the outer's slot 1.
func sortOwnedBySelect(proj *logical.LogicalProject, sort *logical.LogicalSort) bool {
	cur := proj.Input
	for cur != nil {
		if cur == logical.LogicalOperator(sort) {
			return true
		}
		switch cur.(type) {
		case *logical.LogicalFilter, *logical.LogicalLimit, *logical.LogicalDistinct:
			ch := cur.Children()
			if len(ch) != 1 {
				return false
			}
			cur = ch[0]
		default:
			return false
		}
	}
	return false
}

// operatorContains reports whether target appears in root's subtree
// (including root itself). Used to determine relative operator placement
// when a pass's rewrite depends on which of two operators is above.
func operatorContains(root, target logical.LogicalOperator) bool {
	if root == nil {
		return false
	}
	if root == target {
		return true
	}
	for _, ch := range root.Children() {
		if operatorContains(ch, target) {
			return true
		}
	}
	return false
}
