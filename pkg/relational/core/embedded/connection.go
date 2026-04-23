// Package embedded implements the embedded (in-process) SQL execution engine
// for the FoundationDB relational layer.
//
// EmbeddedConnection is the Go equivalent of Java's EmbeddedRelationalConnection.
// It parses SQL, routes DDL statements through the MetadataOperationsFactory,
// and (eventually) routes DML through the query planner.
package embedded

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/antlr4-go/antlr/v4"
	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// cteData holds the materialized result of a WITH clause named query.
type cteData struct {
	cols []string
	rows [][]driver.Value
}

// ambiguousColumnMarker is the sentinel stored in a JOIN row map's bare
// column slot when the same column name is defined on more than one
// FROM-clause source. The map keeps qualified `alias.col` slots intact;
// the bare slot is poisoned so any unqualified reference surfaces the
// ambiguity as SQLSTATE 42702 at lookup time instead of silently
// returning last-write-wins data. Matches Java's
// AmbiguousColumnReferenceException (plan-time) behavior at the runtime
// map-eval boundary we have today.
type ambiguousColumnMarker struct {
	Col string
}

// EmbeddedConnection is an in-process SQL connection backed by FDB.
//
// Implements driver.Conn and driver.ExecerContext so DDL statements can
// execute without a Prepare round-trip.
//
// Transaction model:
//
//	Auto-commit: every statement runs in its own FDB transaction via fdbDB.Run().
//	Explicit transaction: BeginTx opens an FDB transaction; all statements in
//	the transaction share it. Commit/Rollback close it.
type EmbeddedConnection struct {
	// sess carries the durable resource handles + session identifiers
	// (FDB database, catalog, keyspace, metadata factory, current /
	// default schema + database path). Extracted to the core/session
	// package in Phase 1b of RFC 021 so future frontends (gRPC, REPL,
	// direct in-process API without database/sql) can hold the same
	// session object.
	sess *session.Session

	closed atomic.Bool

	// activeTx is non-nil when an explicit transaction is open (BeginTx called).
	// nil means auto-commit mode.
	activeTx *embeddedTx

	// ctes holds materialized CTE results for the current SELECT statement.
	// Non-nil only during execSelect; nil outside of that scope.
	ctes map[string]*cteData

	// scalarSubqueryCache memoises uncorrelated scalar subquery results
	// (one entry per SubqueryExpressionAtomContext) for the current
	// SELECT. Populated before the outer query's runInTx starts to avoid
	// nested FDB transactions; read from during per-row evaluation.
	// Non-nil only during execSelect.
	scalarSubqueryCache map[antlrgen.IQueryContext]any

	// validQualifiers holds the uppercased set of valid qualifier aliases
	// for the JOIN query currently executing (left source + every join
	// source). Used by evalExprAtomOnMap to reject WHERE/ON references
	// like `c.name` when no source `c` is in scope (42F01). Set via
	// pushValidQualifiersScope at the top of execSelectJoin and cleared
	// on return. nil outside of that scope — the map-path evaluator
	// silently falls back to bare-column lookup when nil, matching the
	// pre-dayshift-40 behavior for non-JOIN code paths.
	validQualifiers map[string]bool

	// outerScopes is a stack of outer-row scopes used to resolve correlated
	// column references from inside a subquery (EXISTS / IN / scalar).
	// Pushed at each subquery entry point (snapshots the current row of
	// the outer scan), popped on return via the pushOuterScope pop func.
	// The innermost scope is at the end. Empty stack = no correlation
	// context. See evalExprAtom / evalExprAtomOnMap for lookup semantics.
	outerScopes []outerScope

	// currentSourceAliases holds the uppercased set of qualifier aliases
	// for the outer proto-path scan currently executing (e.g. `FROM emp
	// AS e` → {"E", "EMP"}). Consumed by outerScopeFromMsg so a
	// correlated inner reference like `e.id` resolves even when the
	// outer uses an explicit user alias distinct from the proto
	// descriptor name. nil outside a proto scan — outerScopeFromMsg
	// falls back to msg's descriptor name.
	currentSourceAliases map[string]bool
}

// embeddedTx is the driver.Tx returned by BeginTx. It holds the open FDB
// record context for the duration of the explicit transaction.
type embeddedTx struct {
	conn *EmbeddedConnection
	rctx *recordlayer.FDBRecordContext
}

// Commit runs pre-commit hooks, flushes version mutations, commits the FDB
// transaction, runs post-commit hooks, and clears the connection's activeTx.
func (tx *embeddedTx) Commit() error {
	err := tx.rctx.CommitWithHooks()
	tx.conn.activeTx = nil
	return err
}

// Rollback cancels the FDB transaction and clears the connection's activeTx.
func (tx *embeddedTx) Rollback() error {
	tx.conn.activeTx = nil
	tx.rctx.Cancel()
	return nil
}

// runInTx executes fn either inside the open explicit transaction (if one
// exists) or inside a new auto-commit transaction via fdbDB.Run.
// In explicit-transaction mode, fn errors propagate without retry.
func (c *EmbeddedConnection) runInTx(ctx context.Context, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	if c.activeTx != nil {
		return fn(c.activeTx.rctx)
	}
	return c.sess.DB.Run(ctx, fn)
}

// pushCTEScope replaces c.ctes with a fresh map that inherits the outer
// scope's entries (so inner queries can reference outer CTEs) and returns
// a pop function that restores the previous scope verbatim. Use with
// `defer c.pushCTEScope()()` at every point that introduces new CTE names
// (WITH clauses, derived tables) so inner definitions don't leak out.
func (c *EmbeddedConnection) pushCTEScope() func() {
	prior := c.ctes
	next := make(map[string]*cteData, len(prior))
	for k, v := range prior {
		next[k] = v
	}
	c.ctes = next
	return func() { c.ctes = prior }
}

// pushValidQualifiersScope installs a per-query set of valid qualifier
// aliases (uppercased) and returns a pop function restoring the prior
// scope. Called from execSelectJoin so the map-path evaluator can
// reject WHERE/ON references like `c.name` when no source matches
// `c`. Outside the JOIN scope c.validQualifiers is nil and the
// evaluator preserves the pre-fix silent bare-column fallback — the
// map-path evaluator is JOIN-only so that scope is sufficient.
func (c *EmbeddedConnection) pushValidQualifiersScope(set map[string]bool) func() {
	prior := c.validQualifiers
	c.validQualifiers = set
	return func() { c.validQualifiers = prior }
}

// outerScope is one level of outer-row binding for correlated subqueries.
// At least one of msg / row is non-nil:
//   - msg  : proto-backed outer (single-source SELECT, WHERE call site).
//   - row  : map-backed outer (JOIN / CTE / HAVING / aggregate). Keys are
//     both unqualified (`col`) and qualified (`alias.col`) per
//     scanTableToMaps convention.
//
// qualifiers holds the uppercased set of valid qualifier aliases for this
// outer. A correlated reference `qual.col` matches this scope iff qual is
// in the set. Unqualified `col` falls back through scopes innermost-first
// regardless of qualifiers.
type outerScope struct {
	msg        proto.Message
	row        map[string]driver.Value
	qualifiers map[string]bool
}

// pushOuterScope appends one outer-row scope to the correlation stack and
// returns a pop function that trims it back. Use with `defer` at every
// subquery entry point (EXISTS, IN, scalar subquery) so nested
// correlations stack correctly. Safe to call with a zero-value scope
// (msg == nil && row == nil) — lookups fall through to the next level.
func (c *EmbeddedConnection) pushOuterScope(s outerScope) func() {
	c.outerScopes = append(c.outerScopes, s)
	return func() { c.outerScopes = c.outerScopes[:len(c.outerScopes)-1] }
}

// outerScopeFromMsg builds an outerScope for a proto-backed outer row.
// Qualifier set combines:
//   - the message's descriptor name (always)
//   - any user-level aliases recorded on conn.currentSourceAliases
//     (e.g. `FROM emp AS e` → {"E"} plus the descriptor "EMP")
//
// Returns a zero-value scope when msg is nil so the caller doesn't need
// to nil-check. conn may be nil in unit tests; descriptor name alone
// is sufficient there.
func outerScopeFromMsg(conn *EmbeddedConnection, msg proto.Message) outerScope {
	if msg == nil {
		return outerScope{}
	}
	quals := map[string]bool{
		strings.ToUpper(string(msg.ProtoReflect().Descriptor().Name())): true,
	}
	if conn != nil {
		for a := range conn.currentSourceAliases {
			quals[a] = true
		}
	}
	return outerScope{msg: msg, qualifiers: quals}
}

// pushSourceAliases records the current outer-scan source aliases so
// a subquery's outerScopeFromMsg can expose them to correlated column
// resolution. Pass any SQL-level aliases (e.g. sq.tableAlias and
// sq.tableName) — they're uppercased for case-insensitive match. Returns
// a pop function.
func (c *EmbeddedConnection) pushSourceAliases(aliases ...string) func() {
	prior := c.currentSourceAliases
	m := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		if a == "" {
			continue
		}
		m[strings.ToUpper(a)] = true
	}
	c.currentSourceAliases = m
	return func() { c.currentSourceAliases = prior }
}

// outerScopeFromMapRow builds an outerScope for a map-backed outer row
// (JOIN / CTE / HAVING aggregate). qualifiers is derived from every
// qualified key in the row: for each key of the form `alias.col`, the
// prefix is added (uppercased) to the qualifier set. Returns a zero-
// value scope for a nil/empty row.
func outerScopeFromMapRow(row map[string]driver.Value) outerScope {
	if len(row) == 0 {
		return outerScope{}
	}
	quals := make(map[string]bool)
	for k := range row {
		if dot := strings.LastIndex(k, "."); dot >= 0 {
			quals[strings.ToUpper(k[:dot])] = true
		}
	}
	return outerScope{row: row, qualifiers: quals}
}

// outerScopesContainQualifier reports whether any outer scope on the
// stack declares qualUpper as a valid qualifier alias. Used by the
// map-path evaluator to let correlated `outer.col` references bypass
// the JOIN-scope valid-qualifier reject before falling through to
// resolveOuterColumn.
func outerScopesContainQualifier(c *EmbeddedConnection, qualUpper string) bool {
	for _, s := range c.outerScopes {
		if s.qualifiers[qualUpper] {
			return true
		}
	}
	return false
}

// resolveOuterColumn walks the outer-scope stack innermost-first trying
// to resolve a column reference that was not found in the inner scope.
// Returns (value, found, err).
//
// Qualified `qual.col`: only scopes whose qualifiers set contains qual
// are consulted. A qualified reference binds to exactly one source per
// SQL semantics, so when a scope's qualifier matches but the bare
// column is missing, resolution stops with 42703 — we do NOT continue
// to the next outer scope (another scope with the same qualifier name
// would be a shadowing violation at the SQL level).
//
// Unqualified `col`: every scope is tried in order; first match wins.
// Identifier case is preserved verbatim from the AST; if a GROUP BY
// clause and a correlated reference use different casing, the lookup
// will miss (matches the rest of this evaluator's case-sensitive
// column semantics).
func (c *EmbeddedConnection) resolveOuterColumn(colName string) (driver.Value, bool, error) {
	qual := ""
	bare := colName
	if dot := strings.LastIndex(colName, "."); dot >= 0 {
		qual = strings.ToUpper(colName[:dot])
		bare = colName[dot+1:]
	}
	for i := len(c.outerScopes) - 1; i >= 0; i-- {
		s := c.outerScopes[i]
		if qual != "" && !s.qualifiers[qual] {
			continue
		}
		switch {
		case s.msg != nil:
			fd := s.msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(bare))
			if fd == nil {
				if qual != "" {
					return nil, false, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"column %q not found in correlated source %q", bare, qual)
				}
				continue
			}
			if !s.msg.ProtoReflect().Has(fd) {
				return nil, true, nil
			}
			return functions.ProtoValueToDriver(fd, s.msg.ProtoReflect().Get(fd)), true, nil
		case s.row != nil:
			if qual != "" {
				// Row keys preserve the SQL-level alias case (e.g. `e.id`
				// when the outer wrote `FROM emp AS e`); the qualifier
				// set and lookup qual are uppercased. Do a case-
				// insensitive prefix match so `E.id` → `e.id`.
				for k, v := range s.row {
					dot := strings.LastIndex(k, ".")
					if dot < 0 {
						continue
					}
					if strings.EqualFold(k[:dot], qual) && k[dot+1:] == bare {
						if _, isAmb := v.(ambiguousColumnMarker); isAmb {
							return nil, false, api.NewErrorf(api.ErrCodeAmbiguousColumn,
								"correlated column reference %q is ambiguous", colName)
						}
						return v, true, nil
					}
				}
				return nil, false, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in correlated source %q", bare, qual)
			}
			if v, ok := s.row[bare]; ok {
				if _, isAmb := v.(ambiguousColumnMarker); isAmb {
					return nil, false, api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"correlated column reference %q is ambiguous", bare)
				}
				return v, true, nil
			}
		}
	}
	return nil, false, nil
}

// cachedLoadSchema returns the api.Schema for (dbPath, schemaName), using the
// connection-level cache to avoid repeated FDB reads within the same session.
// The cache is invalidated by any DDL that modifies schema definitions.
//
// When an explicit user transaction is active we read the catalog via a
// separate auto-commit transaction so that catalog reads do not add read
// conflict ranges to the user's write transaction, which would cause spurious
// not_committed (1020) conflicts when other tests run DDL concurrently.
func (c *EmbeddedConnection) cachedLoadSchema(txn api.Transaction, dbPath, schemaName string) (api.Schema, error) {
	key := session.SchemaCacheKey(dbPath, schemaName)
	if s, ok := c.sess.SchemaCache[key]; ok {
		return s, nil
	}
	var s api.Schema
	var err error
	if c.activeTx != nil {
		// Read catalog outside the user transaction to avoid adding catalog
		// read-conflict ranges that conflict with concurrent DDL.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			readTxn := catalog.NewFDBTransaction(rctx)
			s, err = c.sess.Catalog.LoadSchema(readTxn, dbPath, schemaName)
			return nil, err
		})
	} else {
		s, err = c.sess.Catalog.LoadSchema(txn, dbPath, schemaName)
	}
	if err != nil {
		return nil, err
	}
	c.sess.SchemaCache[key] = s
	return s, nil
}

func (c *EmbeddedConnection) invalidateSchemaCache(dbPath, schemaName string) {
	c.sess.InvalidateSchema(dbPath, schemaName)
}

// New returns a ready-to-use embedded connection.
func New(
	dbPath string,
	fdbDB *recordlayer.FDBDatabase,
	cat *catalog.RecordLayerStoreCatalog,
	factory apiddl.MetadataOperationsFactory,
	ks *keyspace.RelationalKeyspace,
) *EmbeddedConnection {
	sess := session.New(fdbDB, cat, ks, factory)
	sess.DBPath = dbPath
	return &EmbeddedConnection{
		sess: sess,
	}
}

// ExecContext executes SQL (DDL/DML/transaction) and returns the row-
// count result. Routes through the query.Generator seam — the naive
// Generator parses, dispatches to execStatement, and returns a Plan
// whose Execute aggregates RowsAffected across a multi-statement
// batch.
func (c *EmbeddedConnection) ExecContext(ctx context.Context, sql string, args []driver.NamedValue) (driver.Result, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	defer c.beginStatement()()
	substituted, err := substituteParams(sql, args)
	if err != nil {
		return nil, err
	}
	gen := &naiveGenerator{c: c}
	plan, err := gen.Plan(ctx, substituted)
	if err != nil {
		return nil, err
	}
	// ExecContext accepts only update-shaped plans. A bare SELECT or
	// SHOW passed to Exec is rejected with the pre-seam error message
	// (matches TestFDB_EmbeddedSelectReturnsUnsupported). Callers use
	// QueryContext for row-returning statements.
	if !plan.IsUpdate() {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"unsupported statement type; supported: DDL, INSERT, UPDATE, DELETE")
	}
	result, err := plan.Execute(ctx)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(result.RowsAffected), nil
}

// QueryContext handles read-only queries (SELECT / SHOW). Routes
// through the query.Generator seam. Rejects multi-statement batches
// and non-row-returning Plans — behaviour matches the pre-seam
// QueryContext.
func (c *EmbeddedConnection) QueryContext(ctx context.Context, sql string, args []driver.NamedValue) (driver.Rows, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	defer c.beginStatement()()
	substituted, err := substituteParams(sql, args)
	if err != nil {
		return nil, err
	}
	if err := c.ensureCatalogInit(ctx); err != nil {
		return nil, err
	}
	gen := &naiveGenerator{c: c}
	plan, err := gen.Plan(ctx, substituted)
	if err != nil {
		return nil, err
	}
	// QueryContext expects a Rows-returning plan. A multi-statement
	// batch (MultiPlan) is always an update plan under today's
	// semantics; reject with the same message the pre-seam code used.
	if _, isMulti := plan.(*query.MultiPlan); isMulti {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "multi-statement queries are not supported")
	}
	if plan.IsUpdate() {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "only SHOW and SELECT statements are supported")
	}
	result, err := plan.Execute(ctx)
	if err != nil {
		return nil, err
	}
	return rowsOrEmpty(result.Rows), nil
}

// execQueryBodyRows executes a queryExpressionBody and returns (colNames, rows).
// Handles both simple queries (QueryTermDefaultContext) and UNION (SetQueryContext).
func (c *EmbeddedConnection) execQueryBodyRows(ctx context.Context, body antlrgen.IQueryExpressionBodyContext) ([]string, [][]driver.Value, error) {
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		sq, err := extractFromQueryTerm(b)
		if err != nil {
			return nil, nil, err
		}
		rows, err := c.execSelectQuery(ctx, sq)
		if err != nil {
			return nil, nil, err
		}
		sr := rows.(*staticRows)
		return sr.cols, sr.rows, nil
	case *antlrgen.SetQueryContext:
		r, err := c.execUnion(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		sr := r.(*staticRows)
		return sr.cols, sr.rows, nil
	default:
		return nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported query expression type %T", body)
	}
}

// execUnion executes a UNION ALL / UNION DISTINCT query.
//
// Trailing ORDER BY / LIMIT / OFFSET on the rightmost simpleTable is lifted
// to the combined result. SQL-standard semantics (and Postgres, MySQL): a
// trailing `ORDER BY ... LIMIT N` on a UNION applies to the whole result,
// not just the last branch. The ANTLR grammar nests the clause inside the
// right-side simpleTable because the parser greedily attaches it to the
// last selectElements production, so we pull it back up here.
//
// For a three-way union `A UNION B UNION C ORDER BY col`, the grammar
// produces SetQuery(SetQuery(A, B), C). The outer execUnion lifts C's
// ORDER BY post-combined — correct. A three-way union with an ORDER BY
// bound to the middle SELECT (e.g. `A UNION B ORDER BY col UNION C`)
// would be parsed as SetQuery(SetQuery(A, B_ordered), C) and the inner
// execUnion would sort A∪B without the outer UNION re-sorting; that
// form is also a syntax error in Postgres (ORDER BY can only appear at
// the end without parentheses), so we do not expect valid SQL to hit
// the degenerate case.
func (c *EmbeddedConnection) execUnion(ctx context.Context, setQ *antlrgen.SetQueryContext) (driver.Rows, error) {
	leftCols, leftRows, err := c.execQueryBodyRows(ctx, setQ.GetLeft())
	if err != nil {
		return nil, err
	}

	var unionOrder []orderByClause
	var unionLimit int64 = -1
	var unionOffset int64 = 0
	var rightCols []string
	var rightRows [][]driver.Value
	if rb, ok := setQ.GetRight().(*antlrgen.QueryTermDefaultContext); ok {
		// Run the right side with ORDER BY / LIMIT / OFFSET stripped so those
		// clauses apply post-union. Leaving LIMIT in place on the right side
		// would truncate before dedup/concat and produce wrong results for
		// queries like `... UNION ... LIMIT 5`.
		rsq, parseErr := extractFromQueryTerm(rb)
		if parseErr != nil {
			return nil, parseErr
		}
		unionOrder = rsq.orderBy
		rsq.orderBy = nil
		unionLimit = rsq.limit
		rsq.limit = -1
		unionOffset = rsq.offset
		rsq.offset = 0
		rows, rErr := c.execSelectQuery(ctx, rsq)
		if rErr != nil {
			return nil, rErr
		}
		sr := rows.(*staticRows)
		rightCols, rightRows = sr.cols, sr.rows
	} else {
		rightCols, rightRows, err = c.execQueryBodyRows(ctx, setQ.GetRight())
		if err != nil {
			return nil, err
		}
	}

	quantifier := ""
	if q := setQ.GetQuantifier(); q != nil {
		quantifier = strings.ToUpper(q.GetText())
	}

	// SQL standard: UNION sides must have matching column counts; names
	// are positional (left's names become the result schema). Java's
	// union.yamsql asymmetrically splits the SQLSTATE on the quantifier:
	// UNION ALL arity mismatch errors 42F64 (UNION_INCORRECT_COLUMN_COUNT
	// — class-22-style data error), while UNION (implicit DISTINCT) with
	// arity mismatch errors 0AF00 (FEATURE_NOT_SUPPORTED). The DISTINCT
	// variant can't even be expressed when rows have different arities
	// because set-membership has no meaning.
	if len(leftCols) != len(rightCols) {
		if quantifier != "ALL" {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"UNION DISTINCT column count mismatch: left has %d columns, right has %d",
				len(leftCols), len(rightCols))
		}
		return nil, api.NewErrorf(api.ErrCodeUnionIncorrectColumnCount,
			"UNION ALL column count mismatch: left has %d columns, right has %d",
			len(leftCols), len(rightCols))
	}

	// Java's union.yamsql errors 42F65 UNION_INCOMPATIBLE_COLUMNS when a
	// positional column pair has non-unifiable types. Best-effort runtime
	// check: sample the first non-NULL value from each side per column
	// and require them to be comparable (numeric pairs are fine, same
	// concrete type is fine; anything else errors). When one side has
	// all NULLs for a column we skip that column — can't infer a type
	// without schema-typed columns.
	for ci := 0; ci < len(leftCols); ci++ {
		var lSample, rSample driver.Value
		for _, row := range leftRows {
			if ci < len(row) && row[ci] != nil {
				lSample = row[ci]
				break
			}
		}
		for _, row := range rightRows {
			if ci < len(row) && row[ci] != nil {
				rSample = row[ci]
				break
			}
		}
		if lSample == nil || rSample == nil {
			continue
		}
		if !valuesComparable(lSample, rSample) {
			return nil, api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
				"UNION column %d has incompatible types: left is %T, right is %T",
				ci+1, lSample, rSample)
		}
	}

	combined := append(leftRows, rightRows...) //nolint:gocritic
	if quantifier != "ALL" {
		// UNION (implicit DISTINCT) — deduplicate.
		seen := make(map[string]struct{}, len(combined))
		deduped := combined[:0]
		for _, row := range combined {
			key := rowKey(row)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				deduped = append(deduped, row)
			}
		}
		combined = deduped
	}

	// Apply union-level ORDER BY against the result schema (leftCols by position).
	if len(unionOrder) > 0 {
		colIdx := make(map[string]int, len(leftCols))
		for i, name := range leftCols {
			// Case-insensitive lookup to match the standard SELECT-list /
			// ORDER BY semantics the single-source path uses.
			colIdx[strings.ToLower(name)] = i
		}
		// Resolve each ORDER BY entry to a column index. Expression-based
		// ORDER BY is not supported at the union level — the combined row
		// set has no backing map/message to evaluate against.
		indices := make([]int, len(unionOrder))
		for i, ob := range unionOrder {
			if ob.expr != nil {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"ORDER BY expression not supported on UNION result; use a column name from the left SELECT list")
			}
			idx, ok := colIdx[strings.ToLower(ob.colName)]
			if !ok {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"ORDER BY column %q not found in UNION result schema", ob.colName)
			}
			indices[i] = idx
		}
		sort.SliceStable(combined, func(a, b int) bool {
			for k, idx := range indices {
				less, equal := orderByLess(combined[a][idx], combined[b][idx], unionOrder[k])
				if equal {
					continue
				}
				return less
			}
			return false
		})
	}

	// Apply union-level OFFSET / LIMIT.
	if unionOffset > 0 {
		if unionOffset >= int64(len(combined)) {
			combined = combined[:0]
		} else {
			combined = combined[unionOffset:]
		}
	}
	if unionLimit >= 0 && int64(len(combined)) > unionLimit {
		combined = combined[:unionLimit]
	}

	return &staticRows{cols: leftCols, rows: combined}, nil
}

// extractColOpLiteral generalises extractColEqualsLiteral to any
// comparison operator among `=`, `>`, `>=`, `<`, `<=`. Returns the
// operator text (one of the above), the bare column name, and the
// literal value. When the RHS side is the column and the LHS is the
// literal, the operator is flipped to preserve col-on-left semantics
// (so `5 < id` becomes `id > 5`).
func extractColOpLiteral(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (op, col string, val any, ok bool) {
	pred, good := expr.(*antlrgen.PredicatedExpressionContext)
	if !good {
		return "", "", nil, false
	}
	if pred.Predicate() != nil {
		return "", "", nil, false
	}
	bcp, good := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !good {
		return "", "", nil, false
	}
	opC := bcp.ComparisonOperator()
	if opC == nil {
		return "", "", nil, false
	}
	opText := strings.ReplaceAll(opC.GetText(), " ", "")
	switch opText {
	case "=", ">", ">=", "<", "<=":
	default:
		return "", "", nil, false
	}
	// Column-on-left, literal-on-right.
	if name, isCol := extractColumnRef(bcp.GetLeft()); isCol {
		if v, isLit := evalConstantAtom(ctx, c, bcp.GetRight()); isLit {
			return opText, name, v, true
		}
	}
	// Column-on-right, literal-on-left — flip the operator.
	if name, isCol := extractColumnRef(bcp.GetRight()); isCol {
		if v, isLit := evalConstantAtom(ctx, c, bcp.GetLeft()); isLit {
			return flipComparisonOp(opText), name, v, true
		}
	}
	return "", "", nil, false
}

// extractColBetweenLiteral recognises `col BETWEEN lo AND hi` where
// col is a bare column reference and lo, hi are constant literals.
// Returns the column name and both literal values. NOT BETWEEN,
// non-constant bounds, and NULL bounds bail out — NOT BETWEEN is a
// non-contiguous key range (two open half-ranges), and a NULL bound
// makes the predicate UNKNOWN per SQL 3VL so it cannot narrow the
// scan. BETWEEN's SQL semantics are inclusive on both sides, which
// the callers translate into `col >= lo AND col <= hi` bounds.
func extractColBetweenLiteral(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (col string, lo, hi any, ok bool) {
	pred, good := expr.(*antlrgen.PredicatedExpressionContext)
	if !good {
		return "", nil, nil, false
	}
	bet, good := pred.Predicate().(*antlrgen.BetweenComparisonPredicateContext)
	if !good {
		return "", nil, nil, false
	}
	if bet.NOT() != nil {
		return "", nil, nil, false
	}
	name, isCol := extractColumnRef(pred.ExpressionAtom())
	if !isCol {
		return "", nil, nil, false
	}
	loVal, isLit := evalConstantAtom(ctx, c, bet.GetLeft())
	if !isLit || loVal == nil {
		return "", nil, nil, false
	}
	hiVal, isLit := evalConstantAtom(ctx, c, bet.GetRight())
	if !isLit || hiVal == nil {
		return "", nil, nil, false
	}
	return name, loVal, hiVal, true
}

// flipComparisonOp flips a comparison operator for the case where
// the column ref appears on the right (`5 < id` → treat as `id > 5`).
func flipComparisonOp(op string) string {
	switch op {
	case ">":
		return "<"
	case ">=":
		return "<="
	case "<":
		return ">"
	case "<=":
		return ">="
	}
	return op // `=` is symmetric
}

// extractPKUserFields returns the ordered list of user field names
// making up the primary key when pushdown is safe, or nil otherwise.
//
// Only CompositeKeyExpression is supported today: SQL DDL's default
// (non-intermingled) path emits `Concat(RecordTypeKey, Field(col)…)`,
// and the RecordTypeKey prefix in the range tuple naturally scopes
// the FDB scan to the right record type. The bare FieldKeyExpression
// branch — which SQL DDL only emits for `SetIntermingleTables(true)`
// schemas — has NO type filter; an intermingled multi-table schema
// where different types share a PK column space could return a
// wrong-typed record at the same key. We bail on that shape until
// a type-filtering wrapper is added; the scan path still handles
// intermingled tables correctly.
func extractPKUserFields(pk recordlayer.KeyExpression) []string {
	if e, ok := pk.(*recordlayer.CompositeKeyExpression); ok {
		// FieldNames() on a CompositeKeyExpression returns just the
		// Field children, not the RecordTypeKey (which contributes no
		// named column). That's exactly the user field list.
		return e.FieldNames()
	}
	return nil
}

// flattenAndPredicates walks a WHERE expression as a conjunction of
// leaf predicates. Returns the flat list of leaves and `true` on
// success. Fails (returns false) when the expression contains any
// non-AND logical operator (OR, XOR, NOT) — those break the
// "everything the scan would also have matched" invariant that
// pushdown relies on.
func flattenAndPredicates(expr antlrgen.IExpressionContext) ([]antlrgen.IExpressionContext, bool) {
	le, ok := expr.(*antlrgen.LogicalExpressionContext)
	if !ok {
		return []antlrgen.IExpressionContext{expr}, true
	}
	op := le.LogicalOperator()
	opText := strings.ReplaceAll(op.GetText(), " ", "")
	isAnd := op.AND() != nil || opText == "&&"
	if !isAnd {
		return nil, false
	}
	left, lok := flattenAndPredicates(le.Expression(0))
	if !lok {
		return nil, false
	}
	right, rok := flattenAndPredicates(le.Expression(1))
	if !rok {
		return nil, false
	}
	return append(left, right...), true
}

// extractColEqualsLiteral returns (colName, literalValue, true) when
// the expression is exactly a `col = literal` equality. NULL on the
// RHS and any non-constant RHS cause a `false` return, in which case
// the pushdown caller falls back to the full scan.
func extractColEqualsLiteral(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (string, any, bool) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", nil, false
	}
	if pred.Predicate() != nil {
		return "", nil, false
	}
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return "", nil, false
	}
	op := bcp.ComparisonOperator()
	if op == nil || strings.ReplaceAll(op.GetText(), " ", "") != "=" {
		return "", nil, false
	}
	// One side must be a column ref; the other must evaluate to a
	// literal. Try both orderings.
	if colName, ok := extractColumnRef(bcp.GetLeft()); ok {
		if val, ok := evalConstantAtom(ctx, c, bcp.GetRight()); ok {
			return colName, val, true
		}
	}
	if colName, ok := extractColumnRef(bcp.GetRight()); ok {
		if val, ok := evalConstantAtom(ctx, c, bcp.GetLeft()); ok {
			return colName, val, true
		}
	}
	return "", nil, false
}

// extractColumnRef returns the bare (last-segment) column name from a
// FullColumnName expression atom.
func extractColumnRef(atom antlrgen.IExpressionAtomContext) (string, bool) {
	fcn, ok := atom.(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return "", false
	}
	name := functions.FullIdToName(fcn.FullColumnName().FullId())
	return name[strings.LastIndex(name, ".")+1:], true
}

// evalConstantAtom attempts to evaluate an expression atom without a
// row context. Succeeds for literals / bound params / pure-constant
// expressions; fails otherwise (including for NULL, since NULL on the
// RHS of `=` is never true under three-valued logic and should fall
// back to scan for consistent semantics).
func evalConstantAtom(ctx context.Context, c *EmbeddedConnection, atom antlrgen.IExpressionAtomContext) (any, bool) {
	v, err := evalExprAtom(ctx, c, nil, atom)
	if err != nil {
		return nil, false
	}
	if v == nil {
		return nil, false
	}
	return v, true
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

	// Execute derived table query and register it as a temporary CTE.
	if sq.derivedQuery != nil {
		// Push a CTE scope so the derived-table alias is visible during this
		// query's evaluation without leaking back out to an enclosing scope.
		defer c.pushCTEScope()()
		cols, rows, err := c.execQueryBodyRows(ctx, sq.derivedQuery.QueryExpressionBody())
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
		c.ctes[strings.ToUpper(sq.tableName)] = &cteData{cols: cols, rows: rows}
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
			// RECURSIVE is a scope enabler, not a requirement: a CTE
			// marked RECURSIVE that does not actually self-reference is
			// evaluated non-recursively (matches Postgres / SQL spec).
			if recursiveKeyword && containsTableRef(body, cteName) {
				cteCols, cteRows, cteErr = c.materializeRecursiveCTE(ctx, body, cteName, renameList, traversalOrder)
			} else {
				cteCols, cteRows, cteErr = c.execQueryBodyRows(ctx, body)
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
			c.ctes[cteName] = &cteData{cols: cteCols, rows: cteRows}
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

// cteRowsToMaps converts materialized CTE data into the map format used by JOIN evaluation.
func cteRowsToMaps(cte *cteData, alias string) []map[string]driver.Value {
	result := make([]map[string]driver.Value, len(cte.rows))
	for i, row := range cte.rows {
		m := make(map[string]driver.Value, len(cte.cols)*2)
		for j, col := range cte.cols {
			m[col] = row[j]
			m[alias+"."+col] = row[j]
		}
		result[i] = m
	}
	return result
}

// scanTableToMaps scans all records of tableName into a slice of maps.
// Each map has two key styles:
//   - "alias.colName" (qualified, using alias or tableName)
//   - "colName" (unqualified, for convenience)
func (c *EmbeddedConnection) scanTableToMaps(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	tableName, alias string,
) ([]map[string]driver.Value, error) {
	// If the table name resolves to a CTE, return materialized rows directly.
	if c.ctes != nil {
		if cte, ok := c.ctes[strings.ToUpper(tableName)]; ok {
			return cteRowsToMaps(cte, alias), nil
		}
	}

	cursor := store.ScanRecordsByType(tableName, nil, recordlayer.ForwardScan())
	defer cursor.Close() //nolint:errcheck

	var rows []map[string]driver.Value
	for {
		result, nextErr := cursor.OnNext(ctx)
		if nextErr != nil {
			return nil, nextErr
		}
		if !result.HasNext() {
			break
		}
		msg := result.GetValue().Record
		msgRef := msg.ProtoReflect()
		fields := msgRef.Descriptor().Fields()
		m := make(map[string]driver.Value, fields.Len()*2)
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			col := string(fd.Name())
			var v driver.Value
			if msgRef.Has(fd) {
				v = functions.ProtoValueToDriver(fd, msgRef.Get(fd))
			}
			m[col] = v
			m[alias+"."+col] = v
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// resolveQualifierColumns resolves a `<qualifier>.*` SELECT list against
// the FROM-clause sources. Returns the ordered column list from the matching
// source and the effective alias (useful for row-key lookups of the form
// "alias.col"). Returns ErrCodeUndefinedTable when the qualifier does not
// match any source.
//
// Matching rules (first match wins):
//  1. tableAlias (or tableName when no explicit alias was given).
//  2. Any joins[i].alias (or joins[i].tableName when no alias).
//
// Columns come from: the CTE definition when the source names a CTE; the
// record type descriptor otherwise.
func (c *EmbeddedConnection) resolveQualifierColumns(md *recordlayer.RecordMetaData, sq *selectQuery, qualifier string) ([]string, string, error) {
	type source struct {
		tableName string
		alias     string // falls back to tableName when not explicitly aliased
	}
	sources := make([]source, 0, 1+len(sq.joins))
	leftAlias := sq.tableAlias
	if leftAlias == "" {
		leftAlias = sq.tableName
	}
	sources = append(sources, source{tableName: sq.tableName, alias: leftAlias})
	for _, jc := range sq.joins {
		a := jc.alias
		if a == "" {
			a = jc.tableName
		}
		sources = append(sources, source{tableName: jc.tableName, alias: a})
	}

	for _, s := range sources {
		if !strings.EqualFold(s.alias, qualifier) {
			continue
		}
		if c.ctes != nil {
			if cte, ok := c.ctes[strings.ToUpper(s.tableName)]; ok {
				cols := make([]string, len(cte.cols))
				copy(cols, cte.cols)
				return cols, s.alias, nil
			}
		}
		rt := md.GetRecordType(s.tableName)
		if rt == nil {
			return nil, "", api.NewErrorf(api.ErrCodeUndefinedTable,
				"qualifier %q resolves to table %q which has no record type", qualifier, s.tableName)
		}
		fields := rt.Descriptor.Fields()
		cols := make([]string, 0, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			cols = append(cols, string(fields.Get(i).Name()))
		}
		return cols, s.alias, nil
	}

	return nil, "", api.NewErrorf(api.ErrCodeUndefinedTable,
		"SELECT %s.*: qualifier does not match any FROM-clause source", qualifier)
}

// resolveSelectListPosition maps a SQL-92 positional reference (e.g.
// `ORDER BY 2` or `GROUP BY 1`) to the matching output column name from
// the current SELECT list. `clause` is the SQL keyword used for the
// out-of-range error message ("ORDER BY" or "GROUP BY"). Accepts a
// positive integer literal (DecimalConstant wrapped in
// PredicatedExpression→ConstantExpressionAtom).
//
// Returns:
//   - (name, true, nil): positional reference resolved to an output column.
//   - ("", false, nil): the expression isn't a positional reference at all
//     (caller falls through to column / expression paths).
//   - ("", false, err): expression IS a positive integer literal but N is
//     out of range. Postgres / MySQL error on this instead of treating the
//     integer as a constant sort / group key, so we do the same.
func resolveSelectListPosition(clause string, expr antlrgen.IExpressionContext, projCols, projAliases []string, aggCols []aggSelectCol) (string, bool, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", false, nil
	}
	atom, ok := pred.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return "", false, nil
	}
	dec, ok := atom.Constant().(*antlrgen.DecimalConstantContext)
	if !ok {
		return "", false, nil
	}
	n, err := strconv.ParseInt(dec.DecimalLiteral().GetText(), 10, 64)
	if err != nil || n < 1 {
		return "", false, nil
	}
	listLen := len(projCols)
	if listLen == 0 {
		listLen = len(aggCols)
	}
	if int(n) > listLen {
		return "", false, api.NewErrorf(api.ErrCodeInvalidParameter,
			"%s position %d is out of range: SELECT list has %d entries", clause, n, listLen)
	}
	switch {
	case len(projCols) > 0:
		if int(n) <= len(projAliases) && projAliases[n-1] != "" {
			return projAliases[n-1], true, nil
		}
		return projCols[n-1], true, nil
	case len(aggCols) > 0:
		return aggCols[n-1].outName, true, nil
	}
	return "", false, nil
}

// expandStarSlots expands mixed SELECT lists of the form
// `SELECT a.*, b.label FROM a, b` by rewriting each `<qualifier>.*` slot
// (marked via sq.projStarQualifiers[i]) into its resolved per-source
// column list. After expansion projStarQualifiers is zeroed out so the
// downstream execution loop can treat every slot as a plain named column.
//
// For each expanded column `col` from source with alias `A`, projCols[k]
// becomes `A.col` (alias-qualified, matching the keys scanTableToMaps
// writes and the ORDER BY resolver which expects qualified names to
// appear in cols[]) and projAliases[k] stays empty — the downstream
// cols-from-projCols fallback uses projCols verbatim, which keeps
// ORDER BY a.id resolvable. The runner compares row values, not
// column names, so the qualified output name is fine. projExprs[k] = nil.
//
// No-op when projCols is nil (pure SELECT * / pure qualifier-star take
// the legacy projQualifier / nil-projCols paths) or when no slot is a
// star. Safe to call multiple times — subsequent calls see an empty
// star set and bail.
func (c *EmbeddedConnection) expandStarSlots(md *recordlayer.RecordMetaData, sq *selectQuery) error {
	if sq.projCols == nil {
		return nil
	}
	hasStar := false
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			hasStar = true
			break
		}
	}
	if !hasStar {
		return nil
	}
	newCols := make([]string, 0, len(sq.projCols))
	newAliases := make([]string, 0, len(sq.projCols))
	newExprs := make([]antlrgen.IExpressionContext, 0, len(sq.projCols))
	newStars := make([]string, 0, len(sq.projCols))
	for i, col := range sq.projCols {
		qual := ""
		if i < len(sq.projStarQualifiers) {
			qual = sq.projStarQualifiers[i]
		}
		if qual == "" {
			newCols = append(newCols, col)
			newAliases = append(newAliases, sq.projAliases[i])
			newExprs = append(newExprs, sq.projExprs[i])
			newStars = append(newStars, "")
			continue
		}
		cols, qAlias, err := c.resolveQualifierColumns(md, sq, qual)
		if err != nil {
			return err
		}
		for _, cn := range cols {
			newCols = append(newCols, qAlias+"."+cn)
			newAliases = append(newAliases, "")
			newExprs = append(newExprs, nil)
			newStars = append(newStars, "")
		}
	}
	sq.projCols = newCols
	sq.projAliases = newAliases
	sq.projExprs = newExprs
	sq.projStarQualifiers = newStars
	return nil
}

// collectLeftJoinKeys returns the set of row-map keys that describe the
// left-hand side of a RIGHT JOIN — unqualified column names and
// alias-qualified variants for every source that has already been
// merged into the nested-loop `joined` accumulator. Used for NULL-
// padding unmatched right rows; deriving the keys from metadata
// (record type or CTE) instead of sampling a runtime row means the
// NULL-padding works even when the left side is entirely empty.
//
// `sources` must list the sources in the order they were merged in,
// with the same tableName / alias that scanTableToMaps was given (so
// the alias-qualified keys match the ones stored on real rows).
func (c *EmbeddedConnection) collectLeftJoinKeys(md *recordlayer.RecordMetaData, sources []struct{ tableName, alias string }) []string {
	seen := make(map[string]struct{})
	var keys []string
	addKey := func(k string) {
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}

	for _, s := range sources {
		alias := s.alias
		if alias == "" {
			alias = s.tableName
		}
		var cols []string
		if c.ctes != nil {
			if cte, ok := c.ctes[strings.ToUpper(s.tableName)]; ok {
				cols = cte.cols
			}
		}
		if cols == nil {
			rt := md.GetRecordType(s.tableName)
			if rt != nil {
				fields := rt.Descriptor.Fields()
				cols = make([]string, 0, fields.Len())
				for i := 0; i < fields.Len(); i++ {
					cols = append(cols, string(fields.Get(i).Name()))
				}
			}
		}
		for _, col := range cols {
			addKey(col)
			addKey(alias + "." + col)
		}
	}
	return keys
}

// computeAmbiguousBareColumns returns the set of bare column names that
// appear in the schema of more than one FROM-clause source (including
// comma-cross-joins and explicit JOINs). Unqualified references to such
// columns are ambiguous per SQL §6.4 and must error 42702 at lookup
// time. Column sources come from the CTE column list when the source
// names a CTE, and from the record type descriptor otherwise.
func (c *EmbeddedConnection) computeAmbiguousBareColumns(md *recordlayer.RecordMetaData, sq *selectQuery) map[string]bool {
	sources := make([]struct{ tableName string }, 0, 1+len(sq.joins))
	sources = append(sources, struct{ tableName string }{sq.tableName})
	for _, jc := range sq.joins {
		sources = append(sources, struct{ tableName string }{jc.tableName})
	}
	counts := make(map[string]int)
	for _, s := range sources {
		var cols []string
		if c.ctes != nil {
			if cte, ok := c.ctes[strings.ToUpper(s.tableName)]; ok {
				cols = cte.cols
			}
		}
		if cols == nil {
			rt := md.GetRecordType(s.tableName)
			if rt != nil {
				fields := rt.Descriptor.Fields()
				cols = make([]string, 0, fields.Len())
				for i := 0; i < fields.Len(); i++ {
					cols = append(cols, string(fields.Get(i).Name()))
				}
			}
		}
		// A single source listing the same column twice (descriptors
		// shouldn't, CTEs also shouldn't) must not self-bump the count.
		seen := make(map[string]bool, len(cols))
		for _, col := range cols {
			if !seen[col] {
				counts[col]++
				seen[col] = true
			}
		}
	}
	result := make(map[string]bool)
	for col, count := range counts {
		if count > 1 {
			result[col] = true
		}
	}
	return result
}

// poisonAmbiguousBareCols overwrites any bare key in row that matches an
// entry in ambiguous with the ambiguousColumnMarker sentinel. Qualified
// (alias.col) entries are left untouched, so callers that qualify their
// reference still resolve normally. Call after every row merge/build
// path in execSelectJoin so no emitted row exposes the last-write-wins
// bare value.
func poisonAmbiguousBareCols(row map[string]driver.Value, ambiguous map[string]bool) {
	for col := range ambiguous {
		if _, has := row[col]; has {
			row[col] = ambiguousColumnMarker{Col: col}
		}
	}
}

// execSelectJoin executes a SELECT with one or more JOIN clauses.
// Supports INNER JOIN and LEFT OUTER JOIN using nested-loop join.
// aggregateMapRows applies GROUP BY + aggregate computation to a slice of map rows
// (as produced by JOIN evaluation or CTE materialization). Returns the resulting
// output column names and tuple rows.
//
// Behavior:
//   - COUNT(*) (sq.countStar, no aggCols): returns [["COUNT(*)"]] with a single row
//     holding int64(len(filtered)). NOTE: parser sets countStar only when the
//     entire SELECT list is a bare COUNT(*); with GROUP BY, COUNT(*) flows through
//     sq.aggCols instead.
//   - Otherwise: computes per-group aggregates (COUNT, COUNT(DISTINCT col), SUM,
//     MIN, MAX, AVG), emits one row per group (or a single synthetic group when
//     sq.groupBy is empty), optionally filtered by sq.havingExpr.
//
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

func (c *EmbeddedConnection) execSelectQueryFull(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	if len(sq.joins) > 0 {
		return c.execSelectJoin(ctx, sq)
	}

	type row = []driver.Value
	type outField struct {
		name string
		fd   protoreflect.FieldDescriptor
		// expr is set when the slot holds a computed expression (used for
		// extra sort-only fields like `ORDER BY v * 2`). Evaluated against
		// the current message in the scan loop; fd is nil in that case.
		expr antlrgen.IExpressionContext
	}
	var cols []string
	var data []row
	var extraSortFields []outField
	// naturalOrder holds the column names the chosen scan cursor emits
	// rows in, always ASC, without tiebreakers. Used by the post-scan
	// sort to skip the in-memory ORDER BY when sq.orderBy is already
	// a prefix of the natural order (and all ASC). Empty means the
	// cursor's emission order is unspecified — always sort.
	var naturalOrder []string
	// naturalOrderAliases maps uppercase SELECT-list alias names to
	// their underlying column names, so `SELECT id AS pk ... ORDER BY
	// pk` resolves to the PK col for the natural-order prefix check.
	// Captured from the scan loop so the out-of-closure sort path can
	// use it without re-parsing.
	var naturalOrderAliases map[string]string
	// reverseScanApplied tracks whether the chosen cursor uses a
	// reverse scan to satisfy an all-DESC ORDER BY prefix of
	// naturalOrder. When true, the post-scan sort is skipped (the
	// cursor's reverse emission IS the requested DESC order) and the
	// LIMIT early-termination logic treats the reverse-DESC match the
	// same as a forward-ASC match.
	var reverseScanApplied bool

	_, runErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry so duplicate rows aren't appended
		cols = nil
		extraSortFields = nil
		naturalOrder = nil
		naturalOrderAliases = nil
		reverseScanApplied = false
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		// Qualified-star (SELECT <q>.*) on a single-source FROM must match
		// this source. Delegate to resolveQualifierColumns so the alias-
		// matching rule stays in one place; we ignore the returned column
		// list because for a single-source query `a.*` projects the same
		// columns as `*`.
		if sq.projQualifier != "" {
			if _, _, qErr := c.resolveQualifierColumns(md, sq, sq.projQualifier); qErr != nil {
				return nil, qErr
			}
		}
		// Expand mixed qualifier-star + named-column slots. Single-source
		// FROM only has one alias to match against, but the expansion
		// still works uniformly — a wrong qualifier errors 42F01 from
		// resolveQualifierColumns.
		if expandErr := c.expandStarSlots(md, sq); expandErr != nil {
			return nil, expandErr
		}

		rt := md.GetRecordType(sq.tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "table %q not found in schema", sq.tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Scan-cursor pushdown chain. Order is "narrowest first": each
		// branch represents a progressively looser shape, so the first
		// match is always the tightest narrowing. Fallthrough is the
		// full type scan. The scan loop below still applies the full
		// WHERE via evalPredicate, so every pushdown is a superset of
		// the rows the scan would have matched — partial narrowing is
		// correct.
		//
		// Each branch sets naturalOrder — the column sequence the
		// resulting cursor emits rows in, always ASC. Used downstream
		// by naturalOrderSatisfies to skip the in-memory ORDER BY sort
		// and to enable LIMIT early-termination when the ORDER BY
		// clause is a prefix of this order.
		//
		// Covering-index optimisation (skip the by-PK fetch) is
		// considered at every secondary-index branch when the SELECT's
		// referenced column set fits the (idx cols, PK cols) union.
		//
		// Chain order:
		//   PK:
		//     1. equality on every PK col          — 1 key
		//     2. single-col IN-list                — N keys
		//     3. single-col range / BETWEEN / LIKE
		//     4. composite leading-eq + IN-list    — N keys on composite
		//     5. composite range with leading eq
		//     6. composite pure-prefix             — equalities on a
		//        leading PK subset, no range / IN — tuple-prefix scan
		//   Secondary index (each gated on index scannability inside
		//   its helper):
		//     7. equality                          — exact key (covered)
		//     8. single-col IN-list                — N keys (covered)
		//     9. composite leading-eq + IN-list    — N keys (covered)
		//    10. range / BETWEEN / LIKE prefix     — (covered)
		//    11. composite range with leading eq   — (covered)
		//    12. full type scan (fallback)
		var cursor recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
		pkCols := extractPKUserFields(rt.PrimaryKey)
		// Populate naturalOrderAliases early so each branch's reverse-
		// scan decision can resolve ORDER BY aliases against the
		// SELECT-list. The scan-setup block below overwrites this with
		// the same value; hoisting is cheap.
		naturalOrderAliases = buildOrderByAliases(sq)
		// pkScanProps carries the direction (forward/reverse) the PK
		// branches pass into their cursor builders. Each branch sets
		// this based on whether sq.orderBy is an all-DESC prefix of
		// pkCols (the naturalOrder these branches all produce). On ASC
		// prefix (or empty orderBy), stays ForwardScan. naturalOrder
		// is still recorded as pkCols — the sort eliminator downstream
		// uses reverseApplied to accept either direction.
		pkScanProps, pkReverseApplied := scanPropsForOrder(sq.orderBy, pkCols, naturalOrderAliases)
		if pkVals, ok := c.tryPKEqualityPushdown(ctx, sq, rt); ok {
			// At-most-one row — direction is irrelevant for the record
			// set, but forwarding pkScanProps keeps the branch uniform.
			cursor = pkPushdownScanCursor(store, rt, pkVals, pkScanProps)
			// At-most-one row — sort is trivially no-op. Flag it as
			// fully ordered by PK so ORDER BY on PK cols is skipped.
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if pkVals, ok := c.tryPKInListPushdown(ctx, sq, rt); ok {
			if len(pkVals) == 1 {
				// Degenerate IN-list: `pk IN (v)` is equivalent to `pk =
				// v` — take the equality path. Single point scan instead
				// of a one-element lazy chain, and naturalOrder can flag
				// PK cols (at-most-one row) to enable ORDER BY
				// elimination and LIMIT early-termination.
				cursor = pkPushdownScanCursor(store, rt, pkVals, pkScanProps)
				naturalOrder = pkCols
				reverseScanApplied = pkReverseApplied
			} else {
				cursor = pkPushdownInListScanCursor(store, rt, pkVals)
				// IN-list lazy chain — emits in declared-list order,
				// no usable natural order for ORDER BY elimination.
			}
		} else if bounds, ok := c.tryPKRangePushdown(ctx, sq, rt); ok {
			cursor = pkPushdownRangeScanCursor(store, rt, bounds, pkScanProps)
			// Single-col PK range → ASC on the PK col, then nothing.
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if cil, ok := c.tryPKCompositeInListPushdown(ctx, sq, rt); ok {
			cursor = pkCompositeInListScanCursor(store, rt, cil)
			// Per-sub-scan natural order is ASC on PK cols; but
			// sub-scans run sequentially over IN-list values in
			// declared order, so the overall emission isn't sorted.
		} else if cr, ok := c.tryPKCompositeRangePushdown(ctx, sq, rt); ok {
			cursor = pkPushdownCompositeRangeScanCursor(store, rt, cr, pkScanProps)
			// Composite PK range emits rows in ASC PK order.
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if cp, ok := c.tryPKCompositePrefixPushdown(ctx, sq, rt); ok {
			// Pure-prefix composite PK: `WHERE a = 1 AND b = 5` on PK
			// (a, b, c) narrows to the tuple-prefix scan [rtk, 1, 5]
			// without any range/IN-list on trailing cols. Last PK
			// branch before secondary — tighter composite forms have
			// already been tried.
			cursor = pkPushdownScanCursor(store, rt, cp.prefixVals, pkScanProps)
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if idxName, idxVal, ok := c.trySecondaryIndexPushdown(ctx, store, sq, rt, md); ok {
			// The helper itself filters out WRITE_ONLY / DISABLED
			// indexes while iterating, so any returned index is
			// guaranteed scannable. Falls through to the next branch
			// (and ultimately to a full scan) when no scannable match
			// exists.
			//
			// Covering-index optimisation: when every column the SELECT
			// reads from each row is derivable from the index key + PK,
			// bypass the per-row LoadRecord fetch by synthesising a
			// dynamicpb record from the IndexEntry. One FDB round-trip
			// per row instead of two. See covering_index.go.
			idx := md.GetIndex(idxName)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexEqualityTupleRange(idxVal))
			} else {
				cursor = secondaryIndexPushdownCursor(store, idxName, idxVal)
			}
			// Index equality → rows emit in (idxCols..., PKCols...)
			// tuple order. Equality fixes idxCols, so effective
			// sort key is PKCols.
			naturalOrder = pkCols
		} else if sil, ok := c.trySecondaryIndexInListPushdown(ctx, store, sq, rt, md); ok {
			// Covering also applies to IN-list: each sub-scan can skip
			// the by-PK fetch when the index covers every referenced
			// column. Same decision as the equality path.
			idx := md.GetIndex(sil.indexName)
			if len(sil.values) == 1 {
				// Degenerate IN-list: single sub-scan is an index
				// equality point scan — take the equality path directly
				// to drop the lazy-chain wrapper and enable ORDER BY
				// elimination on (idxCols..., PKCols...) = PKCols (with
				// idxCols fixed).
				if idx != nil && canCoverIndex(sq, idx, rt) {
					cursor = coveringIndexRangeScanCursor(store, rt, idx,
						buildSecondaryIndexEqualityTupleRange(sil.values[0]))
				} else {
					cursor = secondaryIndexPushdownCursor(store, sil.indexName, sil.values[0])
				}
				naturalOrder = pkCols
			} else if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = secondaryIndexInListScanCursor(store, sil, rt, idx)
			} else {
				cursor = secondaryIndexInListScanCursor(store, sil, nil, nil)
			}
			// IN-list lazy chain — not sorted across sub-scans.
		} else if cil, ok := c.trySecondaryIndexCompositeInListPushdown(ctx, store, sq, rt, md); ok {
			if idx := md.GetIndex(cil.indexName); idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = secondaryIndexCompositeInListScanCursor(store, cil, rt, idx)
			} else {
				cursor = secondaryIndexCompositeInListScanCursor(store, cil, nil, nil)
			}
		} else if sir, ok := c.trySecondaryIndexRangePushdown(ctx, store, sq, rt, md); ok {
			idx := md.GetIndex(sir.indexName)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexRangeTupleRange(sir.bounds))
			} else {
				cursor = secondaryIndexRangeScanCursor(store, sir.indexName, sir.bounds)
			}
			// Index range → (idxCol ASC, PKCols ASC).
			if idx != nil {
				naturalOrder = append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
			}
		} else if sicr, ok := c.trySecondaryIndexCompositeRangePushdown(ctx, store, sq, rt, md); ok {
			idx := md.GetIndex(sicr.indexName)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexCompositeRangeTupleRange(sicr))
			} else {
				cursor = secondaryIndexCompositeRangeScanCursor(store, sicr)
			}
			if idx != nil {
				naturalOrder = append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
			}
		} else {
			// Full type scan emits in PK tuple order (record-type-key
			// prefix keeps records of the same type contiguous). Use
			// reverse scan when ORDER BY is an all-DESC prefix of
			// pkCols — same direction-selection rule as the PK
			// pushdown branches.
			cursor = store.ScanRecordsByType(sq.tableName, nil, pkScanProps)
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		}
		defer cursor.Close() //nolint:errcheck

		// Record the SQL-level aliases of this scan so correlated
		// subqueries can expose them to outerScopeFromMsg (e.g.
		// `FROM emp AS e` → {"E", "EMP"}). Pop on function return.
		defer c.pushSourceAliases(sq.tableName, sq.tableAlias)()

		if sq.countStar {
			cols = []string{countStarOutName(sq)}
			var count int64
			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				match, matchErr := evalPredicate(ctx, c, result.GetValue().Record, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if match {
					count++
				}
			}
			// HAVING on a bare COUNT(*) query: evaluate against the single
			// aggregate row and drop it when the predicate fails. Without
			// this the COUNT(*) fast path emitted one row unconditionally.
			// HAVING references the aggregate function (canonical name),
			// not the SELECT-list alias — see aggregateMapRows comment.
			if sq.havingExpr != nil {
				keep, hErr := evalHaving(ctx, c, map[string]driver.Value{"COUNT(*)": count}, sq.havingExpr)
				if hErr != nil {
					return nil, hErr
				}
				if !keep {
					data = nil
					return nil, nil
				}
			}
			data = []row{{count}}
			return nil, nil
		}

		// GROUP BY aggregate query: scan → group → aggregate.
		if len(sq.aggCols) > 0 {
			// Resolve group-by field descriptors. Expression group keys
			// (sq.groupByExprs[i] != nil) skip FD resolution — they are
			// evaluated per message below via evalExpr.
			groupFDs := make([]protoreflect.FieldDescriptor, len(sq.groupBy))
			for i, col := range sq.groupBy {
				if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
					continue
				}
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"GROUP BY column %q not found in table %q", col, sq.tableName)
				}
				groupFDs[i] = fd
			}
			// Resolve aggregate arg field descriptors (nil for COUNT(*) and for
			// expression args, which are evaluated per-message via ac.aggExpr).
			//
			// groupCol entries are group-by references lifted out of the SELECT
			// list during extractFromSimpleTable's aggregate re-classification.
			// Their value comes from gs.groupVals at emit time, not from the
			// proto scan — so we only validate the FD exists when it's a bare
			// column name. A groupCol whose name matches an entry in groupBy[]
			// with a non-nil groupByExprs[] is an expression group (e.g.
			// GROUP BY CASE ...); skip the FD lookup for those.
			groupExprByName := make(map[string]bool, len(sq.groupBy))
			for i, gn := range sq.groupBy {
				if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
					groupExprByName[gn] = true
				}
			}
			// groupByNames holds the declared GROUP BY bare-column list so we
			// can enforce SQL §7.10 GR1 — a projected bare column that isn't
			// in GROUP BY (and isn't an aggregate argument) is 42803. Pre-
			// dayshift-40 the emission loop silently NULL-filled instead.
			groupByNames := make(map[string]bool, len(sq.groupBy))
			for i, gn := range sq.groupBy {
				// Expression-based GROUP BY (e.g. `GROUP BY a + b`) is keyed
				// by the raw expression text as a synthetic display name —
				// handled via groupExprByName below. Skip here.
				if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
					continue
				}
				groupByNames[gn] = true
			}
			aggArgFDs := make([]protoreflect.FieldDescriptor, len(sq.aggCols))
			for i, ac := range sq.aggCols {
				if ac.groupCol != "" {
					if groupExprByName[ac.groupCol] {
						continue
					}
					fd := msgDesc.Fields().ByName(protoreflect.Name(ac.groupCol))
					if fd == nil {
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column %q not found in table %q", ac.groupCol, sq.tableName)
					}
					// Java-aligned 42803. The fd-exists check above fired
					// first so undefined columns still surface as 42703,
					// matching Java's error order.
					if !groupByNames[ac.groupCol] {
						return nil, api.NewErrorf(api.ErrCodeGroupingError,
							"column %q must appear in the GROUP BY clause or be used in an aggregate function",
							ac.groupCol)
					}
					aggArgFDs[i] = fd
				} else if ac.aggArg != "" {
					fd := msgDesc.Fields().ByName(protoreflect.Name(ac.aggArg))
					if fd == nil {
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"aggregate column %q not found in table %q", ac.aggArg, sq.tableName)
					}
					aggArgFDs[i] = fd
				}
			}

			type groupState struct {
				groupVals []driver.Value // values for the group-by columns
				// accumulators parallel to sq.aggCols
				counts       []int64
				sums         []float64
				mins         []driver.Value
				maxes        []driver.Value
				avgs         []float64             // running sum for AVG
				avgsN        []int64               // count for AVG
				distinctSets []map[string]struct{} // nil unless COUNT(DISTINCT)
			}
			groupOrder := []string{} // insertion order for deterministic output
			groups := map[string]*groupState{}

			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				msg := result.GetValue().Record
				match, matchErr := evalPredicate(ctx, c, msg, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if !match {
					continue
				}

				// Build group-by key.
				gVals := make([]driver.Value, len(sq.groupBy))
				for i := range sq.groupBy {
					if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
						v, evalErr := evalExpr(ctx, c, msg, sq.groupByExprs[i])
						if evalErr != nil {
							return nil, evalErr
						}
						gVals[i] = v
						continue
					}
					fd := groupFDs[i]
					if fd != nil && msg.ProtoReflect().Has(fd) {
						gVals[i] = functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd))
					}
				}
				key := groupByKey(gVals)
				gs, exists := groups[key]
				if !exists {
					distinctSets := make([]map[string]struct{}, len(sq.aggCols))
					for di, ac := range sq.aggCols {
						if ac.aggDistinct {
							distinctSets[di] = make(map[string]struct{})
						}
					}
					gs = &groupState{
						groupVals:    gVals,
						counts:       make([]int64, len(sq.aggCols)),
						sums:         make([]float64, len(sq.aggCols)),
						mins:         make([]driver.Value, len(sq.aggCols)),
						maxes:        make([]driver.Value, len(sq.aggCols)),
						avgs:         make([]float64, len(sq.aggCols)),
						avgsN:        make([]int64, len(sq.aggCols)),
						distinctSets: distinctSets,
					}
					groups[key] = gs
					groupOrder = append(groupOrder, key)
				}
				// Update accumulators.
				for i, ac := range sq.aggCols {
					if ac.groupCol != "" {
						continue // group-by reference, no accumulation
					}
					if ac.outExpr != nil {
						// Post-aggregation expression — evaluated at emit time.
						continue
					}
					// Fetch the argument value.
					//   - aggExpr != nil: evaluate expression (e.g. SUM(qty*price)).
					//   - aggArg  != "": read the bare column via field descriptor.
					//   - neither:       COUNT(*) — no argument, counted unconditionally below.
					var v driver.Value
					hasArg := ac.aggArg != "" || ac.aggExpr != nil
					if ac.aggExpr != nil {
						ev, evalErr := evalExpr(ctx, c, msg, ac.aggExpr)
						if evalErr != nil {
							return nil, evalErr
						}
						v = ev
					} else if aggArgFDs[i] != nil && msg.ProtoReflect().Has(aggArgFDs[i]) {
						v = functions.ProtoValueToDriver(aggArgFDs[i], msg.ProtoReflect().Get(aggArgFDs[i]))
					}
					if ac.aggDistinct && hasArg {
						// *(DISTINCT col|expr): accumulate only the first occurrence
						// of each distinct non-null value — supports COUNT, SUM,
						// AVG, MIN, MAX symmetrically.
						if v == nil {
							continue
						}
						// Type-tagged to keep distinct values of different
						// concrete types apart (matches valuesEqual's
						// mixed-type-equality semantic).
						dk := fmt.Sprintf("%T\x00%v", v, v)
						if _, seen := gs.distinctSets[i][dk]; !seen {
							gs.distinctSets[i][dk] = struct{}{}
							gs.counts[i]++
							switch ac.aggFunc {
							case "SUM", "AVG":
								fv, ok := functions.ToFloat64(v)
								if !ok {
									return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
										"%s(DISTINCT) requires numeric input, got %T", ac.aggFunc, v)
								}
								if ac.aggFunc == "SUM" {
									gs.sums[i] += fv
								} else {
									gs.avgs[i] += fv
									gs.avgsN[i]++
								}
							case "MIN":
								if gs.mins[i] == nil || functions.CompareValues(v, gs.mins[i]) < 0 {
									gs.mins[i] = v
								}
							case "MAX":
								if gs.maxes[i] == nil || functions.CompareValues(v, gs.maxes[i]) > 0 {
									gs.maxes[i] = v
								}
							}
						}
						continue
					}
					// COUNT(*) counts every row including all-NULL; no argument.
					if ac.aggFunc == "COUNT" && !hasArg {
						gs.counts[i]++
						continue
					}
					// COUNT(<col|expr>)/SUM/MIN/MAX/AVG skip NULLs per SQL standard.
					if v == nil {
						continue
					}
					gs.counts[i]++
					switch ac.aggFunc {
					case "SUM", "AVG":
						fv, ok := functions.ToFloat64(v)
						if !ok {
							return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
								"%s requires numeric input, got %T", ac.aggFunc, v)
						}
						if ac.aggFunc == "SUM" {
							gs.sums[i] += fv
						} else {
							gs.avgs[i] += fv
							gs.avgsN[i]++
						}
					case "MIN":
						if gs.mins[i] == nil || functions.CompareValues(v, gs.mins[i]) < 0 {
							gs.mins[i] = v
						}
					case "MAX":
						if gs.maxes[i] == nil || functions.CompareValues(v, gs.maxes[i]) > 0 {
							gs.maxes[i] = v
						}
					}
				}
			}

			// SQL spec: ungrouped aggregate over empty input emits one row
			// (COUNT=0, SUM/MIN/MAX/AVG=NULL).
			if len(sq.groupBy) == 0 && len(groupOrder) == 0 {
				dsets := make([]map[string]struct{}, len(sq.aggCols))
				for di, ac := range sq.aggCols {
					if ac.aggDistinct {
						dsets[di] = make(map[string]struct{})
					}
				}
				groups[""] = &groupState{
					groupVals:    nil,
					counts:       make([]int64, len(sq.aggCols)),
					sums:         make([]float64, len(sq.aggCols)),
					mins:         make([]driver.Value, len(sq.aggCols)),
					maxes:        make([]driver.Value, len(sq.aggCols)),
					avgs:         make([]float64, len(sq.aggCols)),
					avgsN:        make([]int64, len(sq.aggCols)),
					distinctSets: dsets,
				}
				groupOrder = append(groupOrder, "")
			}

			// Build output cols — visible (non-hidden, non-sortOnly) entries
			// first, then sortOnly columns (harvested from ORDER BY) so the
			// post-aggregation sort can find them via colIdx. Hidden entries
			// (harvested from HAVING) drop out entirely. Caller strips the
			// trailing sortOnly columns after the sort.
			groupColIdx := map[string]int{}
			for i, col := range sq.groupBy {
				groupColIdx[col] = i
				// Bare last-segment alias (symmetric with
				// aggregateMapRows) so qualified GROUP BY keys resolve
				// against unqualified SELECT-list references.
				// First-wins on bare collision; see aggregateMapRows.
				if dot := strings.LastIndex(col, "."); dot >= 0 {
					bare := col[dot+1:]
					if _, exists := groupColIdx[bare]; !exists {
						groupColIdx[bare] = i
					}
				}
			}
			emitIdx := make([]int, 0, len(sq.aggCols))
			for i, ac := range sq.aggCols {
				if !ac.hidden && !ac.sortOnly {
					emitIdx = append(emitIdx, i)
				}
			}
			for i, ac := range sq.aggCols {
				if !ac.hidden && ac.sortOnly {
					emitIdx = append(emitIdx, i)
				}
			}
			cols = make([]string, len(emitIdx))
			for out, i := range emitIdx {
				cols[out] = sq.aggCols[i].outName
			}

			// Emit one row per group (with HAVING filter). Two passes:
			// (1) populate fullVals + rowMap for non-outExpr entries;
			// (2) evaluate outExpr entries against the now-filled rowMap.
			for _, key := range groupOrder {
				gs := groups[key]
				fullVals := make([]driver.Value, len(sq.aggCols))
				rowMap := make(map[string]driver.Value, len(sq.aggCols))
				for i, ac := range sq.aggCols {
					if ac.outExpr != nil {
						continue
					}
					if ac.groupCol != "" {
						idx, ok := groupColIdx[ac.groupCol]
						if !ok {
							if dot := strings.LastIndex(ac.groupCol, "."); dot >= 0 {
								idx, ok = groupColIdx[ac.groupCol[dot+1:]]
							}
						}
						if ok {
							fullVals[i] = gs.groupVals[idx]
						}
					} else {
						switch ac.aggFunc {
						case "COUNT":
							fullVals[i] = gs.counts[i]
						case "SUM":
							// SUM of empty-or-all-NULL group is NULL, not 0.
							// DISTINCT path accumulates on first-seen so this
							// is correct for SUM(DISTINCT col) too.
							if gs.counts[i] > 0 {
								fullVals[i] = gs.sums[i]
							}
						case "MIN":
							fullVals[i] = gs.mins[i]
						case "MAX":
							fullVals[i] = gs.maxes[i]
						case "AVG":
							if gs.avgsN[i] > 0 {
								fullVals[i] = gs.avgs[i] / float64(gs.avgsN[i])
							}
						}
					}
					rowMap[ac.outName] = fullVals[i]
				}
				for i, ac := range sq.aggCols {
					if ac.outExpr == nil {
						continue
					}
					v, evalErr := evalExprOnMap(ctx, c, rowMap, ac.outExpr)
					if evalErr != nil {
						return nil, evalErr
					}
					fullVals[i] = v
					rowMap[ac.outName] = v
				}
				if sq.havingExpr != nil {
					keep, havErr := evalHaving(ctx, c, rowMap, sq.havingExpr)
					if havErr != nil {
						return nil, havErr
					}
					if !keep {
						continue
					}
				}
				rowVals := make([]driver.Value, len(emitIdx))
				for out, i := range emitIdx {
					rowVals[out] = fullVals[i]
				}
				data = append(data, rowVals)
			}
			return nil, nil
		}

		// Resolve output fields: either the explicit projection or all fields.
		allFields := msgDesc.Fields()
		var outFields []outField
		// extraSortFields (outer variable) are ORDER BY columns not in the projection.
		//
		// Expression-based ORDER BY items (`ORDER BY v * 2`) work on both
		// SELECT * and named projections — carry each expression as a
		// sentinel-named extra sort field, evaluated per row in the scan
		// loop. Runs BEFORE the projection branch split so SELECT * paths
		// don't silently drop expression sort keys.
		for obIdx, ob := range sq.orderBy {
			if ob.expr == nil {
				continue
			}
			sentinel := fmt.Sprintf("__orderby_expr_%d__", obIdx)
			extraSortFields = append(extraSortFields, outField{name: sentinel, expr: ob.expr})
			sq.orderBy[obIdx].colName = sentinel
			sq.orderBy[obIdx].expr = nil
		}
		if sq.projCols == nil {
			// SELECT * — all fields in descriptor order.
			outFields = make([]outField, allFields.Len())
			for i := 0; i < allFields.Len(); i++ {
				fd := allFields.Get(i)
				outFields[i] = outField{name: string(fd.Name()), fd: fd}
			}
		} else {
			// Named projection — look up each column, apply alias if present.
			outFields = make([]outField, len(sq.projCols))
			projByCol := make(map[string]bool, len(sq.projCols))
			for i, colName := range sq.projCols {
				// Computed expression: no field descriptor needed.
				if i < len(sq.projExprs) && sq.projExprs[i] != nil {
					outName := colName
					if i < len(sq.projAliases) && sq.projAliases[i] != "" {
						outName = sq.projAliases[i]
					}
					outFields[i] = outField{name: outName}
					// Don't add to projByCol (computed cols can't be in ORDER BY as proto fields).
					continue
				}
				// Strip a trivial qualifier (`d.id` where `d` is this
				// source's table name or alias) before the field lookup.
				// Matches how the correlated-subquery path handles
				// qualified refs in evalExprAtom via currentSourceAliases.
				// Without this, `SELECT d.id FROM t AS d` errored 42703
				// at the ByName(`d.id`) lookup. The output column name
				// keeps the qualifier — downstream derived-table
				// materialisation relies on that preserved form to
				// detect duplicate-column shapes like `SELECT a.*,
				// a.* FROM a`, which collapse to equal names only
				// after qualifier stripping.
				lookupName := colName
				if dot := strings.LastIndex(colName, "."); dot >= 0 {
					qual := strings.ToUpper(colName[:dot])
					if strings.EqualFold(qual, sq.tableName) || (sq.tableAlias != "" && strings.EqualFold(qual, sq.tableAlias)) {
						lookupName = colName[dot+1:]
					}
				}
				fd := allFields.ByName(protoreflect.Name(lookupName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"column %q not found in table %q", colName, sq.tableName)
				}
				outName := colName
				if i < len(sq.projAliases) && sq.projAliases[i] != "" {
					outName = sq.projAliases[i]
				}
				outFields[i] = outField{name: outName, fd: fd}
				projByCol[colName] = true
			}
			// Alias redirection: if ORDER BY references a SELECT-list alias
			// (`SELECT id AS n ... ORDER BY n`), it's already projected — no
			// extra field lookup needed. Build an alias → underlying-col map
			// so the sort path's colIdx lookup (which keys off the output
			// name) still matches when cols[] uses the alias.
			aliasToCol := make(map[string]string, len(sq.projCols))
			for i, colName := range sq.projCols {
				if i < len(sq.projAliases) && sq.projAliases[i] != "" {
					aliasToCol[sq.projAliases[i]] = colName
				}
			}
			// Capture aliases for the out-of-closure ORDER BY eliminator
			// so `ORDER BY <alias>` resolves to the underlying column
			// when checking natural-order prefix.
			naturalOrderAliases = make(map[string]string, len(aliasToCol))
			for alias, col := range aliasToCol {
				naturalOrderAliases[strings.ToUpper(alias)] = col
			}
			// Add any ORDER BY columns not already in the projection.
			// Expression ORDER BY was already converted to sentinel extra
			// sort fields above; mark those sentinels present in projByCol
			// so the FD-lookup loop below skips them.
			for _, f := range extraSortFields {
				if f.expr != nil {
					projByCol[f.name] = true
				}
			}
			for _, ob := range sq.orderBy {
				if projByCol[ob.colName] {
					continue
				}
				if _, isAlias := aliasToCol[ob.colName]; isAlias {
					// Alias refers to an already-projected column; no extra
					// sort field. The sort path looks up cols[] which stores
					// the alias, so no further remapping is needed.
					continue
				}
				fd := allFields.ByName(protoreflect.Name(ob.colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"ORDER BY column %q not found in table %q", ob.colName, sq.tableName)
				}
				extraSortFields = append(extraSortFields, outField{name: ob.colName, fd: fd})
				projByCol[ob.colName] = true // avoid duplicates
			}
		}
		// fullFields = projected + extra sort columns; output strips extra at end.
		fullFields := append(outFields, extraSortFields...) //nolint:gocritic
		cols = make([]string, len(outFields))
		for i, f := range outFields {
			cols[i] = f.name
		}

		// Early-termination target: when the scan's natural order
		// already satisfies sq.orderBy (ORDER BY elimination is
		// eligible), and there's no DISTINCT, the scan accumulates
		// rows in final output order — so we can stop reading from
		// the cursor once we've collected enough rows to cover
		// OFFSET + LIMIT. Saves FDB round-trips on queries like
		// `SELECT id FROM t WHERE v > 1000 ORDER BY v LIMIT 5`
		// against a multi-million-row table.
		//
		// Negative (-1) means "no early termination" — read the
		// cursor to exhaustion. Aggregate / DISTINCT / ORDER BY
		// not-satisfiable-by-natural-order all fall back to the
		// full scan and sort later.
		earlyTermTarget := int64(-1)
		if sq.limit >= 0 && !sq.distinct && !sq.countStar && len(sq.aggCols) == 0 &&
			(naturalOrderSatisfies(sq.orderBy, naturalOrder, naturalOrderAliases) || reverseScanApplied) {
			earlyTermTarget = sq.offset + sq.limit
		}

		for {
			if earlyTermTarget >= 0 && int64(len(data)) >= earlyTermTarget {
				break
			}
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			msg := rec.Record
			match, matchErr := evalPredicate(ctx, c, msg, sq.whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			vals := make([]driver.Value, len(fullFields))
			for i, f := range fullFields {
				// Check for a computed expression at this position. SELECT-list
				// expressions come from sq.projExprs (parallel to projCols);
				// extra sort-field expressions live on outField.expr (set when
				// the ORDER BY loop built the field for `ORDER BY v * 2`).
				if i < len(sq.projExprs) && sq.projExprs[i] != nil {
					v, evalErr := evalExpr(ctx, c, msg, sq.projExprs[i])
					if evalErr != nil {
						return nil, evalErr
					}
					if v != nil {
						vals[i] = v.(driver.Value) //nolint:forcetypeassert
					}
					continue
				}
				if f.expr != nil {
					v, evalErr := evalExpr(ctx, c, msg, f.expr)
					if evalErr != nil {
						return nil, evalErr
					}
					if v != nil {
						vals[i] = v.(driver.Value) //nolint:forcetypeassert
					}
					continue
				}
				if msg.ProtoReflect().Has(f.fd) {
					vals[i] = functions.ProtoValueToDriver(f.fd, msg.ProtoReflect().Get(f.fd))
				}
				// else nil (proto2 optional field absent → NULL)
			}
			data = append(data, vals)
		}
		return nil, nil
	})
	if runErr != nil {
		return nil, runErr
	}

	// Apply DISTINCT deduplication before sort. Key off the PROJECTED
	// columns only (data may contain trailing extraSortFields used
	// for ORDER BY-on-non-projected-column; including those in the
	// dedup key would treat (v=30, id=1) and (v=30, id=3) as
	// "distinct" and silently re-emit the duplicate v=30 row).
	if sq.distinct && !sq.countStar {
		projLen := len(cols)
		seen := make(map[string]struct{}, len(data))
		deduped := data[:0]
		for _, row := range data {
			key := rowKey(row[:projLen])
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				deduped = append(deduped, row)
			}
		}
		data = deduped
	}

	// Apply ORDER BY (post-scan in-memory sort).
	if len(sq.orderBy) > 0 {
		for _, ob := range sq.orderBy {
			if ob.expr != nil {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"ORDER BY on an expression is only supported in CTE / JOIN queries; use a column name or alias")
			}
		}
		// Build a map from column name to row index (covers projected + extra sort cols).
		colIdx := make(map[string]int, len(cols)+len(extraSortFields))
		for i, c := range cols {
			colIdx[c] = i
		}
		for i, f := range extraSortFields {
			colIdx[f.name] = len(cols) + i
		}
		// Aggregate-path ORDER BY name validation. The non-aggregate
		// path validated each name when building extraSortFields; the
		// aggregate path doesn't, so a typo (`ORDER BY no_such_col` on
		// `SELECT grp, COUNT(*) ... GROUP BY grp`) silently no-op'd.
		// Mirror the CTE / JOIN validation added in 82bd4382 / 9500c512.
		if len(sq.aggCols) > 0 || sq.countStar {
			for _, ob := range sq.orderBy {
				if _, ok := colIdx[ob.colName]; !ok {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"ORDER BY column %q not found in aggregate result", ob.colName)
				}
			}
		}
		// ORDER BY elimination: if the scan cursor emitted rows in a
		// natural order that already satisfies sq.orderBy, skip the
		// in-memory sort.
		//
		// Two satisfying cases:
		//   (a) Forward scan + all-ASC prefix match: cursor emits in
		//       naturalOrder; ASC prefix is trivially satisfied.
		//   (b) Reverse scan + all-DESC prefix match: the pushdown
		//       branch already picked ReverseScan — cursor emits in
		//       reverse of naturalOrder, which IS the DESC order the
		//       user asked for. Flagged via reverseScanApplied.
		//
		// Correctness: a stable sort on a sequence already sorted by
		// the same key is a no-op. Skipping the sort preserves row
		// order — which for naturally-ordered cursors IS the ORDER BY
		// result.
		//
		// Bail conditions:
		//   - aggregate path: data is post-aggregate, naturalOrder
		//     doesn't apply.
		//   - mixed ASC / DESC across clauses: neither direction
		//     satisfies the full prefix, so we sort.
		//   - NULLS ordering opposite the scan direction's native
		//     placement (ASC + NULLS LAST, or DESC + NULLS FIRST).
		//   - ORDER BY col not in naturalOrder prefix.
		sortSkippable := len(sq.aggCols) == 0 && !sq.countStar &&
			(naturalOrderSatisfies(sq.orderBy, naturalOrder, naturalOrderAliases) || reverseScanApplied)
		if !sortSkippable {
			sort.SliceStable(data, func(i, j int) bool {
				for _, ob := range sq.orderBy {
					idx, ok := colIdx[ob.colName]
					if !ok {
						// Column validated during scan setup; safe to skip.
						continue
					}
					less, equal := orderByLess(data[i][idx], data[j][idx], ob)
					if !equal {
						return less
					}
				}
				return false
			})
		}
	}

	// Strip extra sort columns that were not in the SELECT list.
	if len(extraSortFields) > 0 {
		projLen := len(cols)
		for i, row := range data {
			data[i] = row[:projLen]
		}
	}

	// Apply OFFSET then LIMIT.
	if sq.offset > 0 {
		if sq.offset >= int64(len(data)) {
			data = data[:0]
		} else {
			data = data[sq.offset:]
		}
	}
	if sq.limit >= 0 && int64(len(data)) > sq.limit {
		data = data[:sq.limit]
	}
	// Drop trailing sort-only aggregate columns now that the sort
	// has consumed them. No-op when the query had no ORDER BY
	// references to hidden aggregates.
	if len(sq.aggCols) > 0 {
		cols, data = stripAggregateSortOnly(sq, cols, data)
	}

	return &staticRows{cols: cols, rows: data}, nil
}

// selectQuery holds the parsed components of a SELECT statement.
type selectQuery struct {
	tableName   string
	tableAlias  string   // alias or tableName if no alias given
	projCols    []string // nil = SELECT * or SELECT <qualifier>.*; ignored when countStar or aggCols non-empty
	projAliases []string // parallel to projCols; empty string = no alias (use column name)
	// projExprs holds computed projection expressions parallel to projCols.
	// Non-nil entry overrides the plain column lookup for that position.
	projExprs []antlrgen.IExpressionContext
	// projQualifier is set when SELECT list is exactly `<qualifier>.*`.
	// Projection restricts to columns from the source whose alias (or
	// table name when no alias) equals projQualifier. Empty = SELECT *
	// (all sources) or explicit column list.
	projQualifier string
	// projStarQualifiers is parallel to projCols. When
	// projStarQualifiers[i] != "" that slot is a `<qualifier>.*` to be
	// expanded at execution time (e.g. `SELECT a.*, b.label FROM a, b`).
	// When empty, the slot is a regular named column / expression. Always
	// empty when projCols == nil (SELECT * or pure qualifier-star use the
	// legacy projQualifier / nil-projCols paths).
	projStarQualifiers []string
	countStar          bool // true when SELECT list is exactly COUNT(*)
	// countStarAlias holds the optional `AS alias` on a bare COUNT(*)
	// SELECT. Emitted as the output column name so the derived-table
	// materializer / UNION arity / etc. see the aliased name instead of
	// the canonical "COUNT(*)".
	countStarAlias string
	distinct       bool // true when SELECT DISTINCT
	whereExpr      antlrgen.IWhereExprContext
	// orderBy holds column-name + ascending pairs (nil = no ORDER BY).
	orderBy []orderByClause
	// limit < 0 means no limit.
	limit int64
	// offset >= 0 means skip that many rows after sort/group (OFFSET n).
	offset int64
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
	// aggCols describes a mixed GROUP BY + aggregate SELECT list.
	// Non-nil only when groupBy is non-empty.
	aggCols []aggSelectCol
	// havingExpr is the HAVING clause expression (nil = no HAVING).
	havingExpr antlrgen.IExpressionContext
	// joins describes JOIN clauses (nil = no joins).
	joins []joinClause
	// derivedQuery is non-nil when the FROM clause is a subquery (derived table).
	// When set, tableName holds the alias; the query is materialized at execution time.
	derivedQuery antlrgen.IQueryContext
}

// joinClause describes a single JOIN part in a SELECT query.
type joinClause struct {
	tableName string
	joinType  string // "INNER", "LEFT", "RIGHT"
	alias     string
	onExpr    antlrgen.IExpressionContext
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
	// Exactly one of groupCol / aggFunc / outExpr is set (hidden entries
	// always have aggFunc set).
	groupCol string // plain group-by column reference
	aggFunc  string // COUNT/SUM/MIN/MAX/AVG
	aggArg   string // argument column name — set only when arg is a bare column; used for the proto-path FD fast path. Empty for COUNT(*) and for expression args.
	// aggExpr is the IExpressionContext of the aggregate's argument when it is not a bare
	// column reference (e.g. SUM(qty*price), AVG(CASE ... END)). Evaluated per input row.
	// nil for bare-column args and for COUNT(*).
	aggExpr     antlrgen.IExpressionContext
	aggDistinct bool // true when COUNT(DISTINCT col)
	// hidden aggregates contribute to group accumulation and HAVING evaluation
	// but are excluded from the projected output and the sort. Used for
	// aggregates harvested from HAVING that aren't also in the SELECT list,
	// so `SELECT grp FROM t GROUP BY grp HAVING SUM(v) > 0` returns one column
	// (grp) while still running the SUM.
	hidden bool
	// sortOnly aggregates are harvested from ORDER BY and appended to the
	// emit data so the sort can find them via colIdx. They're stripped from
	// the user-visible output after the sort runs. Combined with hidden:
	//   visible row column = !hidden && !sortOnly.
	//   sort-accessible column = !hidden.
	//   accumulated column = always.
	sortOnly bool
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
		display = argExpr.GetText()
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
		// Aggregate function in ORDER BY — return the canonical output name
		// (e.g. COUNT(*), SUM(col)) so it can be matched against the SELECT list.
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
		switch {
		case awf.COUNT() != nil && awf.STAR() != nil:
			return "COUNT(*)", nil
		case awf.COUNT() != nil && awf.FunctionArg() != nil:
			return "COUNT(" + awf.FunctionArg().GetText() + ")", nil
		case awf.SUM() != nil && awf.FunctionArg() != nil:
			return "SUM(" + awf.FunctionArg().GetText() + ")", nil
		case awf.MIN() != nil && awf.FunctionArg() != nil:
			return "MIN(" + awf.FunctionArg().GetText() + ")", nil
		case awf.MAX() != nil && awf.FunctionArg() != nil:
			return "MAX(" + awf.FunctionArg().GetText() + ")", nil
		case awf.AVG() != nil && awf.FunctionArg() != nil:
			return "AVG(" + awf.FunctionArg().GetText() + ")", nil
		}
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation, "%s: unsupported aggregate function", context)
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

func extractFromSimpleTable(simpleTable *antlrgen.SimpleTableContext) (*selectQuery, error) {
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
					aggCols = append(aggCols, aggSelectCol{outName: alias, aggFunc: fn, aggArg: argCol, aggExpr: argExpr, aggDistinct: isDistinct})
				} else {
					colName, alias, nameErr := selectExprToColumnName(e)
					var expr antlrgen.IExpressionContext
					if nameErr != nil {
						// Not a plain column name — treat as a computed expression.
						// Use alias as the output name; fall back to the raw expression text.
						alias = ""
						if e.Uid() != nil {
							alias = functions.StripIdentifierQuotes(e.Uid().GetText())
						}
						if alias == "" {
							alias = e.Expression().GetText()
						}
						colName = alias
						expr = e.Expression()
					}
					if len(aggCols) > 0 {
						// Mixed aggregate query. Three classifications for
						// the trailing SELECT element based on what the
						// expression references:
						//   - wraps aggregates → harvest any novel inner
						//     aggregates (add as hidden accumulators) and
						//     route the expression itself to outExpr.
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
							// is novel — must be added as hidden so the
							// rowMap at emit time has SUM(b) available for
							// outExpr evaluation. Dedup by outName.
							existingNames := make(map[string]struct{}, len(aggCols))
							for _, ac := range aggCols {
								existingNames[ac.outName] = struct{}{}
							}
							for _, h := range harvestAggregates(expr) {
								if _, seen := existingNames[h.outName]; seen {
									continue
								}
								h.hidden = true
								aggCols = append(aggCols, h)
								existingNames[h.outName] = struct{}{}
							}
							aggCols = append(aggCols, aggSelectCol{outName: outName, outExpr: expr})
						case expr != nil && !exprReferencesColumn(expr):
							aggCols = append(aggCols, aggSelectCol{outName: outName, outExpr: expr})
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
							aggCols = append(aggCols, aggSelectCol{outName: outName, outExpr: expr})
						default:
							aggCols = append(aggCols, aggSelectCol{outName: outName, groupCol: colName})
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
		// harvest the referenced aggregates as hidden accumulators, and
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
					h.hidden = true
					promoted = append(promoted, h)
				}
				outName := projAliases[i]
				if outName == "" {
					outName = col
				}
				promoted = append(promoted, aggSelectCol{outName: outName, outExpr: projExprs[i]})
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
					extra[i] = aggSelectCol{outName: out, outExpr: slotExpr}
				case slotExpr != nil:
					// Expression on group-by columns (no aggregates, no
					// constants-only). Java permits this when all referenced
					// columns are in GROUP BY. Route to outExpr — evaluated
					// post-aggregation against the rowMap holding group-by
					// values. Symmetric with the in-SELECT-loop case at the
					// mixed-agg classification site above.
					extra[i] = aggSelectCol{outName: out, outExpr: slotExpr}
				default:
					extra[i] = aggSelectCol{outName: out, groupCol: c}
				}
			}
			aggCols = append(extra, aggCols...)
			projCols = nil
			projAliases = nil
			projExprs = nil
			projStarQualifiers = nil
		}
	}

	fromClause := simpleTable.FromClause()
	if fromClause == nil {
		// SELECT without FROM: evaluate expressions as constants (single-row result).
		if projQualifier != "" {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
				"qualifier %q in SELECT list but query has no FROM clause", projQualifier)
		}
		for _, q := range projStarQualifiers {
			if q != "" {
				return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
					"qualifier %q.* in SELECT list but query has no FROM clause", q)
			}
		}
		return &selectQuery{
			projCols:    projCols,
			projAliases: projAliases,
			projExprs:   projExprs,
		}, nil
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
		atomItem, atomOk := eb.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !atomOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"FROM: comma-separated sources must be plain table names, got %T",
				eb.TableSourceItem())
		}
		uids := atomItem.TableName().FullId().AllUid()
		parts := make([]string, len(uids))
		for i, u := range uids {
			parts[i] = functions.StripIdentifierQuotes(u.GetText())
		}
		tblName := strings.Join(parts, ".")
		alias := tblName
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			alias = functions.StripIdentifierQuotes(atomItem.Uid().GetText())
		}
		extraCrossJoins = append(extraCrossJoins, joinClause{
			tableName: tblName,
			joinType:  "INNER",
			alias:     alias,
			onExpr:    nil,
		})
		// Bare-source joins are not supported on extras (grammar quirk).
		if len(eb.AllJoinPart()) > 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"JOIN clauses on comma-separated FROM sources are not supported")
		}
	}
	// Resolve FROM source: derived table `FROM (SELECT ...) AS alias` or
	// a plain atom table. Build a common `sq` in either case so the
	// post-construction pipeline (ORDER BY / LIMIT / GROUP BY / HAVING /
	// GR1 validation) applies uniformly — pre-swingshift-41 the derived
	// branch returned early, dropping all of those.
	var sq *selectQuery
	if subItem, isSub := srcBase.TableSourceItem().(*antlrgen.SubqueryTableItemContext); isSub {
		alias := ""
		if subItem.GetAlias() != nil {
			alias = functions.StripIdentifierQuotes(subItem.GetAlias().GetText())
		}
		if alias == "" {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation, "derived table in FROM must have an alias")
		}
		// Derived-table-on-the-right + comma-joined remains a separate
		// gap (the extra-source parser still rejects SubqueryTableItem at
		// line ~3757; see derived_table_renamed.yaml's 0A000 pin). For the
		// left-derived case, thread extraCrossJoins so `(sub) AS x, b, c`
		// runs the comma-joined real tables on the right.
		sq = &selectQuery{
			tableName:          alias,
			tableAlias:         alias,
			joins:              extraCrossJoins,
			projCols:           projCols,
			projAliases:        projAliases,
			projExprs:          projExprs,
			projStarQualifiers: projStarQualifiers,
			projQualifier:      projQualifier,
			countStar:          countStar,
			countStarAlias:     countStarAlias,
			aggCols:            aggCols,
			distinct:           simpleTable.DISTINCT() != nil,
			whereExpr:          fromClause.WhereExpr(),
			limit:              -1,
			derivedQuery:       subItem.Query(),
		}
	} else {
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
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			leftAlias = functions.StripIdentifierQuotes(atomItem.Uid().GetText())
		} else if atomItem.Uid() != nil {
			misAlias := strings.ToUpper(atomItem.Uid().GetText())
			if misAlias == "LEFT" || misAlias == "RIGHT" {
				promotedJoinType = misAlias
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

		sq = &selectQuery{
			tableName:          strings.Join(parts, "."),
			tableAlias:         leftAlias,
			joins:              joins,
			projCols:           projCols,
			projAliases:        projAliases,
			projExprs:          projExprs,
			projStarQualifiers: projStarQualifiers,
			projQualifier:      projQualifier,
			countStar:          countStar,
			countStarAlias:     countStarAlias,
			aggCols:            aggCols,
			distinct:           simpleTable.DISTINCT() != nil,
			whereExpr:          fromClause.WhereExpr(),
			limit:              -1,
		}
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
				if seenOrderCols[posName] {
					return nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists,
						"duplicate column %q in ORDER BY", posName)
				}
				seenOrderCols[posName] = true
				sq.orderBy = append(sq.orderBy, orderByClause{colName: posName, ascending: ascending, nullsFirst: nullsFirst, rawExpr: obExpr.Expression()})
				continue
			}
			// Prefer plain column / aggregate lookup (works in all sort paths,
			// including the proto single-table path). Fall back to storing the
			// expression for CTE / JOIN sort keys like `ORDER BY a + b`.
			colName, nameErr := columnNameFromExpr(obExpr.Expression(), "ORDER BY expression")
			if nameErr == nil {
				if seenOrderCols[colName] {
					return nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists,
						"duplicate column %q in ORDER BY", colName)
				}
				seenOrderCols[colName] = true
				sq.orderBy = append(sq.orderBy, orderByClause{colName: colName, ascending: ascending, nullsFirst: nullsFirst, rawExpr: obExpr.Expression()})
			} else {
				sq.orderBy = append(sq.orderBy, orderByClause{ascending: ascending, nullsFirst: nullsFirst, expr: obExpr.Expression(), rawExpr: obExpr.Expression()})
			}
		}
	}

	// Parse LIMIT [OFFSET] clause.
	limitClauseCtx := simpleTable.LimitClause()
	if limitClauseCtx != nil {
		parseLimitAtom := func(a antlrgen.ILimitClauseAtomContext, label string) (int64, error) {
			if a == nil || a.DecimalLiteral() == nil {
				return 0, nil
			}
			n, parseErr := strconv.ParseInt(a.DecimalLiteral().GetText(), 10, 64)
			if parseErr != nil {
				return 0, api.NewErrorf(api.ErrCodeInvalidRowCountInLimitClause, "invalid %s value %q: %v", label, a.DecimalLiteral().GetText(), parseErr)
			}
			// Postgres, MySQL, Oracle, and SQL:2008 all reject negative
			// LIMIT/OFFSET. Previously Go silently treated negative LIMIT
			// as "no limit" (the downstream guard uses `sq.limit >= 0`),
			// hiding user bugs like `LIMIT -1` instead of surfacing them.
			// SQLSTATE: 2201W (invalid_row_count_in_limit_clause), the
			// SQL-standard class-22 code for this exact case.
			if n < 0 {
				return 0, api.NewErrorf(api.ErrCodeInvalidRowCountInLimitClause, "%s cannot be negative: %d", label, n)
			}
			return n, nil
		}
		// Grammar exposes GetLimit() / GetOffset() for "LIMIT n OFFSET m" form,
		// and AllLimitClauseAtom() for the MySQL "LIMIT offset, count" form.
		if limitClauseCtx.GetLimit() != nil {
			n, parseErr := parseLimitAtom(limitClauseCtx.GetLimit(), "LIMIT")
			if parseErr != nil {
				return nil, parseErr
			}
			sq.limit = n
			if limitClauseCtx.GetOffset() != nil {
				off, parseErr := parseLimitAtom(limitClauseCtx.GetOffset(), "OFFSET")
				if parseErr != nil {
					return nil, parseErr
				}
				sq.offset = off
			}
		} else {
			atoms := limitClauseCtx.AllLimitClauseAtom()
			if len(atoms) > 1 {
				// MySQL allows "LIMIT offset, count" — reject for simplicity.
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"LIMIT offset,count syntax is not supported; use LIMIT count OFFSET n")
			}
			if len(atoms) == 1 {
				n, parseErr := parseLimitAtom(atoms[0], "LIMIT")
				if parseErr != nil {
					return nil, parseErr
				}
				sq.limit = n
			}
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
			posName, isPos, posErr := resolveSelectListPosition("GROUP BY", item.Expression(), projCols, projAliases, sq.aggCols)
			if posErr != nil {
				return nil, posErr
			}
			if isPos {
				sq.groupBy = append(sq.groupBy, posName)
				sq.groupByExprs = append(sq.groupByExprs, nil)
				if aliasName != "" {
					if sq.groupByAliases == nil {
						sq.groupByAliases = make(map[string]int)
					}
					sq.groupByAliases[strings.ToUpper(aliasName)] = len(sq.groupBy) - 1
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
					sq.groupBy = append(sq.groupBy, selectExprsSnapshot[i].GetText())
					sq.groupByExprs = append(sq.groupByExprs, selectExprsSnapshot[i])
					redirected = true
					break
				}
				if !redirected {
					sq.groupBy = append(sq.groupBy, colName)
					sq.groupByExprs = append(sq.groupByExprs, nil)
				}
			} else {
				// Synthesize a display name from the expression text; the
				// value used for grouping comes from evaluating the expr.
				sq.groupBy = append(sq.groupBy, item.Expression().GetText())
				sq.groupByExprs = append(sq.groupByExprs, item.Expression())
			}
			if aliasName != "" {
				if sq.groupByAliases == nil {
					sq.groupByAliases = make(map[string]int)
				}
				sq.groupByAliases[strings.ToUpper(aliasName)] = len(sq.groupBy) - 1
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
			idx, aliased := sq.groupByAliases[strings.ToUpper(name)]
			if !aliased {
				return "", "", false
			}
			if idx < len(sq.groupByExprs) && sq.groupByExprs[idx] != nil {
				return "", "", false
			}
			return sq.groupBy[idx], name, true
		}
		for i := range sq.projCols {
			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				continue
			}
			col := sq.projCols[i]
			if col == "" {
				continue
			}
			underlying, outName, ok := aliasResolves(col)
			if !ok {
				continue
			}
			if i >= len(sq.projAliases) {
				padded := make([]string, i+1)
				copy(padded, sq.projAliases)
				sq.projAliases = padded
			}
			if sq.projAliases[i] == "" {
				sq.projAliases[i] = outName
			}
			sq.projCols[i] = underlying
		}
		// Also rewrite aggCols entries: when the SELECT list mixes
		// plain-col refs with aggregates, bare columns are classified
		// into aggCols with groupCol set rather than into projCols.
		// Also rewrite aggregate arguments — `MAX(z)` where z is a
		// GROUP BY alias needs the arg resolved to the underlying col
		// before per-row evaluation.
		for i := range sq.aggCols {
			ac := &sq.aggCols[i]
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
	}

	// SQL §7.10 General Rule 1 / Java alignment: when GROUP BY is present,
	// every SELECT-list column reference must be in GROUP BY or wrapped in
	// an aggregate. Both SELECT * and SELECT qualifier.* with GROUP BY
	// error 42803 because the star expansion includes all source columns,
	// which generally aren't all in GROUP BY.
	if len(sq.groupBy) > 0 && len(projCols) == 0 && !countStar && len(sq.aggCols) == 0 {
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
	if len(sq.groupBy) > 0 && len(sq.aggCols) == 0 && len(projCols) > 0 {
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
				extra[i] = aggSelectCol{outName: out, outExpr: slotExpr}
			default:
				extra[i] = aggSelectCol{outName: out, groupCol: c}
			}
		}
		sq.aggCols = extra
		projCols = nil
		projAliases = nil
		projExprs = nil
		projStarQualifiers = nil
	}

	// SQL §7.10 GR1: when a SELECT list contains aggregates, every
	// non-aggregate column reference must appear in GROUP BY. With no
	// GROUP BY at all, the query is implicitly one group and bare
	// column references violate the rule. Java errors 42803. Matches
	// Java's groupby-tests.yamsql 42803 pattern extended to the
	// no-GROUP-BY-at-all variant.
	//
	// The SELECT loop at line 3352 silently reclassifies a bare-column
	// element as `aggSelectCol{groupCol: ...}` when aggregates are in
	// the list — checking projCols alone misses those. Walk sq.aggCols
	// for entries that are neither aggregates nor outExprs (bare group
	// column references) and for outExprs that reference columns:
	// both are GR1 violations when there's no GROUP BY.
	hasAggregates := sq.countStar
	for _, ac := range sq.aggCols {
		if ac.aggFunc != "" {
			hasAggregates = true
			break
		}
	}
	if hasAggregates && len(sq.groupBy) == 0 {
		for _, ac := range sq.aggCols {
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
		sq.havingExpr = havingCtx.GetHavingExpr()
	}

	// Redirect aggCols groupCol entries that came from a SELECT-list
	// expression (`v/10 AS bucket`) to point at the matching GROUP BY
	// expression text, so the proto path's groupExprByName check fires
	// and skips the FD lookup. Walks selectExprsSnapshot to find the
	// original projExpr for each groupCol entry; matches against
	// sq.groupBy[] by GetText. Idempotent — runs once after both
	// SELECT-list reclassification (if any) and GROUP BY parsing.
	if len(sq.aggCols) > 0 && len(sq.groupBy) > 0 && len(selectExprsSnapshot) > 0 {
		for ai, ac := range sq.aggCols {
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
			projText := origExpr.GetText()
			for gi, gn := range sq.groupBy {
				if gi < len(sq.groupByExprs) && sq.groupByExprs[gi] != nil && projText == gn {
					sq.aggCols[ai].groupCol = gn
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
	if len(sq.aggCols) > 0 && len(sq.groupBy) > 0 {
		for ai, ac := range sq.aggCols {
			if ac.outExpr == nil || ac.aggFunc != "" {
				continue
			}
			outExprText := ac.outExpr.GetText()
			for gi, gn := range sq.groupBy {
				if gi < len(sq.groupByExprs) && sq.groupByExprs[gi] != nil && outExprText == gn {
					sq.aggCols[ai].outExpr = nil
					sq.aggCols[ai].groupCol = gn
					break
				}
			}
		}
	}

	// countStar fast path assumes a single synthetic row. With GROUP BY
	// present we need a per-group COUNT(*), so demote to aggCols. The
	// alias (if any) propagates so `SELECT COUNT(*) AS n FROM t GROUP BY g`
	// emits the column as `n`.
	if sq.countStar && len(sq.groupBy) > 0 {
		sq.countStar = false
		outName := "COUNT(*)"
		if sq.countStarAlias != "" {
			outName = sq.countStarAlias
		}
		sq.aggCols = append(sq.aggCols, aggSelectCol{outName: outName, aggFunc: "COUNT"})
	}

	// Harvest aggregates referenced in HAVING and ORDER BY that aren't
	// already in aggCols. Otherwise queries like
	//   SELECT grp FROM t GROUP BY grp HAVING SUM(v) > 0
	//   SELECT grp FROM t GROUP BY grp ORDER BY SUM(v) DESC
	// have aggCols == nil → the executor never runs the aggregate pipeline
	// → GROUP BY is silently ignored. The HAVING / ORDER BY resolver already
	// looks up aggregates by their reconstructed output name ("COUNT(*)",
	// "SUM(v)"), so matching aggCols entries make the evaluation round-trip.
	// If projCols still holds plain columns at this point, reclassify them
	// as group-by references in aggCols (mirror of the SELECT-list-aggregate
	// path's existing reclassification).
	type harvestSource struct {
		expr     antlrgen.IExpressionContext
		sortOnly bool // true when the source is ORDER BY (sort-visible); false for HAVING (hidden)
	}
	var harvestSources []harvestSource
	if sq.havingExpr != nil {
		harvestSources = append(harvestSources, harvestSource{expr: sq.havingExpr, sortOnly: false})
	}
	for _, ob := range sq.orderBy {
		if ob.rawExpr != nil {
			harvestSources = append(harvestSources, harvestSource{expr: ob.rawExpr, sortOnly: true})
		}
	}
	if len(harvestSources) > 0 {
		existing := make(map[string]struct{}, len(sq.aggCols))
		for _, ac := range sq.aggCols {
			existing[ac.outName] = struct{}{}
		}
		var newAggs []aggSelectCol
		for _, src := range harvestSources {
			for _, ac := range harvestAggregates(src.expr) {
				if _, ok := existing[ac.outName]; ok {
					// Already accumulated. If we now see this aggregate from
					// an ORDER BY source and the existing entry is hidden
					// (HAVING-only), upgrade to sortOnly so the sort can
					// find it via colIdx. sortOnly subsumes hidden — both
					// HAVING (via rowMap) and ORDER BY (via colIdx) are
					// satisfied, and the column gets stripped post-sort.
					// Walk both already-attached sq.aggCols and the
					// pending newAggs since HAVING harvest runs first.
					if src.sortOnly {
						for k := range sq.aggCols {
							if sq.aggCols[k].outName == ac.outName && sq.aggCols[k].hidden {
								sq.aggCols[k].hidden = false
								sq.aggCols[k].sortOnly = true
							}
						}
						for k := range newAggs {
							if newAggs[k].outName == ac.outName && newAggs[k].hidden {
								newAggs[k].hidden = false
								newAggs[k].sortOnly = true
							}
						}
					}
					continue
				}
				existing[ac.outName] = struct{}{}
				if src.sortOnly {
					ac.sortOnly = true
				} else {
					ac.hidden = true
				}
				newAggs = append(newAggs, ac)
			}
		}
		// ORDER BY items that wrap aggregates in an expression (e.g.
		// `ORDER BY SUM(v) * 2`) get their own sortOnly outExpr aggCols
		// entry. The proto sort path can then look up the entry via
		// colIdx[sentinel] and find a per-group value evaluated from the
		// wrapping expression. Inner aggregates were harvested as hidden
		// above so the rowMap at outExpr eval time has them available.
		for obIdx, ob := range sq.orderBy {
			if ob.expr == nil || len(harvestAggregates(ob.expr)) == 0 {
				continue
			}
			sentinel := fmt.Sprintf("__orderby_aggexpr_%d__", obIdx)
			newAggs = append(newAggs, aggSelectCol{
				outName:  sentinel,
				outExpr:  ob.expr,
				sortOnly: true,
			})
			sq.orderBy[obIdx].colName = sentinel
			sq.orderBy[obIdx].expr = nil
		}
		if len(newAggs) > 0 {
			if len(sq.aggCols) == 0 && len(projCols) > 0 {
				// No SELECT-list aggregates yet; demote the plain projCols
				// to group-by references so the aggregate pipeline knows
				// how to surface them in each output row. When the projExpr
				// matches a GROUP BY expression by text (e.g. `SELECT v/10
				// AS bucket ... GROUP BY v/10`), point groupCol at the
				// matching groupBy[] string so the proto path's
				// groupExprByName check fires and skips the FD lookup.
				prepended := make([]aggSelectCol, 0, len(projCols)+len(sq.aggCols))
				for i, c := range projCols {
					out := projAliases[i]
					if out == "" {
						out = c
					}
					gc := c
					if i < len(projExprs) && projExprs[i] != nil {
						projText := projExprs[i].GetText()
						for gi, gn := range sq.groupBy {
							if gi < len(sq.groupByExprs) && sq.groupByExprs[gi] != nil && projText == gn {
								gc = gn
								break
							}
						}
					}
					prepended = append(prepended, aggSelectCol{outName: out, groupCol: gc})
				}
				sq.aggCols = append(prepended, sq.aggCols...)
				sq.projCols = nil
				sq.projAliases = nil
				sq.projExprs = nil
				sq.projStarQualifiers = nil
			}
			sq.aggCols = append(sq.aggCols, newAggs...)
		}
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

// extractJoinClause parses a single JOIN part (INNER JOIN, LEFT JOIN, etc.) from
// the grammar. Only INNER JOIN and LEFT OUTER JOIN are implemented.
func extractJoinClause(jp antlrgen.IJoinPartContext) (joinClause, error) {
	switch j := jp.(type) {
	case *antlrgen.InnerJoinContext:
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
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			alias = functions.StripIdentifierQuotes(atomItem.Uid().GetText())
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
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			alias = functions.StripIdentifierQuotes(atomItem.Uid().GetText())
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

// stripIdentifierQuotes removes surrounding double-quotes or backticks from a

// execShowStatement routes SHOW … to the appropriate catalog reader.
type staticRows struct {
	cols    []string
	rows    [][]driver.Value
	current int
}

// projectSystemRows applies the SELECT-list projection, ORDER BY, and
// LIMIT/OFFSET of `sq` to the rows returned by an INFORMATION_SCHEMA
// handler. System-table handlers always emit every column; without a
// projection step `SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES"`
// returns all 10 TABLES columns. Column name matching is case-
// insensitive — CREATE TABLE preserves identifier case, but an
// INFORMATION_SCHEMA filter typically uses the canonical upper-cased
// column names regardless.
//
// Computed expressions (SELECT UPPER(TABLE_NAME) ...) are not
// supported — system-table SELECT lists are limited to plain column
// references and SELECT *. Projection aliases override the column
// name in the returned row set.
func projectSystemRows(in driver.Rows, sq *selectQuery) (driver.Rows, error) {
	sr, ok := in.(*staticRows)
	if !ok {
		// Handler returned a non-staticRows implementation; pass through.
		return in, nil
	}
	rows := sr
	if sq.projCols != nil {
		idxByCol := make(map[string]int, len(rows.cols))
		for i, c := range rows.cols {
			idxByCol[strings.ToUpper(c)] = i
		}
		projIdx := make([]int, len(sq.projCols))
		projNames := make([]string, len(sq.projCols))
		for i, col := range sq.projCols {
			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"computed expressions in INFORMATION_SCHEMA SELECT are not supported (%s)", col)
			}
			idx, found := idxByCol[strings.ToUpper(col)]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in INFORMATION_SCHEMA.%s", col, sq.tableName)
			}
			projIdx[i] = idx
			name := col
			if i < len(sq.projAliases) && sq.projAliases[i] != "" {
				name = sq.projAliases[i]
			}
			projNames[i] = name
		}
		projected := make([][]driver.Value, len(rows.rows))
		for i, row := range rows.rows {
			out := make([]driver.Value, len(projIdx))
			for j, idx := range projIdx {
				out[j] = row[idx]
			}
			projected[i] = out
		}
		rows = &staticRows{cols: projNames, rows: projected}
	}

	// ORDER BY — column-name based. Expression-based ORDER BY
	// (`ORDER BY LENGTH(TABLE_NAME)`) is silently ignored on system
	// tables — `ob.expr != nil` falls through the `continue` below.
	// Consistent with the "plain column references only" policy the
	// SELECT list also enforces; users can alias the expression in
	// a derived table if they need it. `ob.colName` is matched case-
	// insensitively against the projected column names so aliased
	// columns in the SELECT list sort under their alias.
	if len(sq.orderBy) > 0 {
		colIdx := make(map[string]int, len(rows.cols))
		for i, c := range rows.cols {
			colIdx[strings.ToUpper(c)] = i
		}
		sort.SliceStable(rows.rows, func(ii, jj int) bool {
			for _, ob := range sq.orderBy {
				if ob.expr != nil {
					continue // not supported here
				}
				idx, found := colIdx[strings.ToUpper(ob.colName)]
				if !found {
					continue
				}
				a, b := rows.rows[ii][idx], rows.rows[jj][idx]
				less, equal := orderByLess(a, b, ob)
				if !equal {
					return less
				}
			}
			return false
		})
	}

	// OFFSET then LIMIT.
	if sq.offset > 0 {
		if sq.offset >= int64(len(rows.rows)) {
			rows.rows = nil
		} else {
			rows.rows = rows.rows[sq.offset:]
		}
	}
	if sq.limit >= 0 && int64(len(rows.rows)) > sq.limit {
		rows.rows = rows.rows[:sq.limit]
	}

	return rows, nil
}

func (r *staticRows) Columns() []string { return r.cols }
func (r *staticRows) Close() error      { r.current = len(r.rows); return nil }
func (r *staticRows) Next(dest []driver.Value) error {
	if r.current >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.current])
	r.current++
	return nil
}

// emptyRows is a driver.Rows with no columns and no data.
type emptyRows struct{}

func (emptyRows) Columns() []string           { return []string{} }
func (emptyRows) Close() error                { return nil }
func (emptyRows) Next(_ []driver.Value) error { return io.EOF }

// Prepare returns a prepared statement. DDL statements have no bind parameters.
func (c *EmbeddedConnection) Prepare(query string) (driver.Stmt, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	return &embeddedStmt{conn: c, query: query}, nil
}

// Close marks the connection as closed.
func (c *EmbeddedConnection) Close() error {
	c.closed.Store(true)
	return nil
}

// Begin implements driver.Conn by delegating to BeginTx with default options.
func (c *EmbeddedConnection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx. Opens an FDB transaction that spans
// all subsequent statements until Commit or Rollback is called.
// Isolation levels other than the default and ReadCommitted return an error.
// Read-only transactions are not separately enforced at the FDB level.
func (c *EmbeddedConnection) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	if c.activeTx != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"nested transactions are not supported")
	}
	switch sql.IsolationLevel(opts.Isolation) { //nolint:exhaustive
	case sql.LevelDefault, sql.LevelSerializable:
		// FDB is always serializable — this is fine.
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"isolation level %v is not supported; use LevelDefault or LevelSerializable",
			sql.IsolationLevel(opts.Isolation))
	}
	return c.beginTransaction()
}

func (c *EmbeddedConnection) beginTransaction() (*embeddedTx, error) {
	fdbTx, err := c.sess.DB.CreateTransaction()
	if err != nil {
		return nil, err
	}
	fdbTx.Options().SetReadSystemKeys()
	rctx := recordlayer.NewFDBRecordContext(fdbTx)
	tx := &embeddedTx{conn: c, rctx: rctx}
	c.activeTx = tx
	return tx, nil
}

// SetDefaultSchema sets the initial schema that is restored by ResetSession.
// Called by the driver when the DSN contains ?schema=.
func (c *EmbeddedConnection) SetDefaultSchema(s string) {
	c.sess.DefaultSchema = s
	c.sess.Schema = s
}

// ResetSession implements driver.SessionResetter. Resets per-request
// state so pooled connections start clean:
//   - schema → defaultSchema (original CONNECT value)
//   - activeTx → rolled back (prevents a leaked transaction bleeding into
//     the next checkout)
//   - ctes → cleared (mid-query panic/error could leave the map populated)
//   - schemaCache → cleared (schema evolution between checkouts would
//     otherwise serve a stale descriptor)
func (c *EmbeddedConnection) ResetSession(_ context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	c.sess.Schema = c.sess.DefaultSchema
	if c.activeTx != nil {
		// Best-effort rollback; we're about to release the connection
		// back to the pool and must not leak the open FDB tx.
		tx := c.activeTx
		c.activeTx = nil
		tx.rctx.Cancel()
	}
	c.ctes = nil
	// Drop any cached scalar-subquery results from the last statement.
	// Cache entries key off parse-tree pointers that belong to the
	// caller's freshly-parsed statement; retaining them across pool
	// checkouts would slowly leak memory (and the keys are invalid
	// against the next statement's tree anyway).
	c.scalarSubqueryCache = nil
	c.sess.ResetSchemaCache()
	return nil
}

// IsValid implements driver.Validator. Returns true if the connection
// is open; the FDB client is stateless so a non-closed connection is
// always usable (catalog init is lazy, not a validity condition).
func (c *EmbeddedConnection) IsValid() bool {
	return !c.closed.Load()
}

// PrepareContext implements driver.ConnPrepareContext.
func (c *EmbeddedConnection) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return c.Prepare(query)
}

// SetSchema sets the current schema label used when no schema is specified in SQL.
func (c *EmbeddedConnection) SetSchema(s string) { c.sess.Schema = s }

// GetSchema returns the current schema label.
func (c *EmbeddedConnection) GetSchema() string { return c.sess.Schema }

// GetDBPath returns the current database path.
func (c *EmbeddedConnection) GetDBPath() string { return c.sess.DBPath }

// execStatement routes a single parsed statement to the right handler.
func (c *EmbeddedConnection) execStatement(ctx context.Context, stmt antlrgen.IStatementContext) (int64, error) {
	if ddl := stmt.DdlStatement(); ddl != nil {
		create := ddl.CreateStatement()
		drop := ddl.DropStatement()
		switch {
		case create != nil:
			return c.execCreate(ctx, create)
		case drop != nil:
			return c.execDrop(ctx, drop)
		default:
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported DDL statement")
		}
	}
	if dml := stmt.DmlStatement(); dml != nil {
		if ins := dml.InsertStatement(); ins != nil {
			return c.execInsert(ctx, ins)
		}
		if del := dml.DeleteStatement(); del != nil {
			return c.execDelete(ctx, del)
		}
		if upd := dml.UpdateStatement(); upd != nil {
			return c.execUpdate(ctx, upd)
		}
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported DML statement")
	}
	if txn := stmt.TransactionStatement(); txn != nil {
		return c.execTransactionStatement(txn)
	}
	return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported statement type; supported: DDL, INSERT, UPDATE, DELETE")
}

// execTransactionStatement handles SQL COMMIT, ROLLBACK, and START TRANSACTION.
// These mirror what database/sql sends when applications use explicit transactions
// via the driver rather than BeginTx directly.
func (c *EmbeddedConnection) execTransactionStatement(txn antlrgen.ITransactionStatementContext) (int64, error) {
	switch {
	case txn.CommitStatement() != nil:
		if c.activeTx == nil {
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "COMMIT: no active transaction")
		}
		if err := c.activeTx.Commit(); err != nil {
			return 0, err
		}
		return 0, nil
	case txn.RollbackStatement() != nil:
		if c.activeTx == nil {
			return 0, nil // ROLLBACK outside transaction is a no-op
		}
		return 0, c.activeTx.Rollback()
	case txn.StartTransaction() != nil:
		if c.activeTx != nil {
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "nested transactions are not supported")
		}
		if _, err := c.beginTransaction(); err != nil {
			return 0, err
		}
		return 0, nil
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported transaction statement: %s", txn.GetText())
	}
}

// wrapSaveRecordError translates record-layer-level errors thrown by
// store.SaveRecord into api.Error values carrying the Java-matching
// SQLSTATE. Without this, SQL callers would see a raw recordlayer
// error type that doesn't `errors.As` to `*api.Error`, defeating the
// SQLSTATE contract that the relational layer documents.
//
// Java's relational layer performs the equivalent mapping in
// RelationalException.toRelationalException (class 23 -> SQLSTATE).
func wrapSaveRecordError(err error) error {
	if err == nil {
		return nil
	}
	var uniqErr *recordlayer.RecordIndexUniquenessViolationError
	if errors.As(err, &uniqErr) {
		return api.WrapErrorf(err, api.ErrCodeUniqueConstraintViolation,
			"unique index %q violated: value %v already exists", uniqErr.IndexName, uniqErr.IndexKey)
	}
	var existsErr *recordlayer.RecordAlreadyExistsError
	if errors.As(err, &existsErr) {
		return api.WrapErrorf(err, api.ErrCodeUniqueConstraintViolation,
			"primary key %v already exists", existsErr.PrimaryKey)
	}
	var keySizeErr *recordlayer.IndexKeySizeError
	if errors.As(err, &keySizeErr) {
		return api.WrapErrorf(err, api.ErrCodeInvalidParameter,
			"index %q key size %d exceeds limit %d", keySizeErr.IndexName, keySizeErr.KeySize, keySizeErr.Limit)
	}
	var valueSizeErr *recordlayer.IndexValueSizeError
	if errors.As(err, &valueSizeErr) {
		return api.WrapErrorf(err, api.ErrCodeInvalidParameter,
			"index %q value size %d exceeds limit %d", valueSizeErr.IndexName, valueSizeErr.ValueSize, valueSizeErr.Limit)
	}
	// Already a relational-layer error (e.g. from validation upstream of
	// the save) — pass through untouched.
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return err
	}
	// Unknown record-layer error — wrap as internal so callers still see a
	// stable SQLSTATE and can `errors.As` to *api.Error for logging. The
	// original record-layer error is preserved via %w.
	return api.WrapErrorf(err, api.ErrCodeInternalError, "record save failed")
}

// execInsert executes INSERT INTO table (col1, col2, ...) VALUES (...), (...).
func (c *EmbeddedConnection) execInsert(ctx context.Context, ins antlrgen.IInsertStatementContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Explicit column list (optional).
	colCtx := ins.UidListWithNestingsInParens()
	var explicitCols []string // nil = no column list (use schema order)
	if colCtx != nil {
		for _, uw := range colCtx.UidListWithNestings().AllUidWithNestings() {
			explicitCols = append(explicitCols, functions.StripIdentifierQuotes(uw.Uid().GetText()))
		}
	}

	tableName := functions.FullIdToName(ins.TableName().FullId())

	// Handle INSERT INTO ... SELECT (insertStatementValueSelect).
	if selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext); ok {
		return c.execInsertSelect(ctx, tableName, explicitCols, selCtx.QueryExpressionBody())
	}

	// Only handle VALUES path.
	valCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueValuesContext)
	if !ok {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "only INSERT ... VALUES (...) is supported")
	}

	var totalRows int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0 // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Resolve column order: explicit list or all fields in descriptor order.
		cols := explicitCols
		if cols == nil {
			fds := msgDesc.Fields()
			cols = make([]string, fds.Len())
			for i := 0; i < fds.Len(); i++ {
				cols[i] = string(fds.Get(i).Name())
			}
		}

		for _, rowCtx := range valCtx.AllRecordConstructorForInsert() {
			exprs := rowCtx.AllExpressionWithOptionalName()
			// Java alignment (inserts-updates-deletes.yamsql):
			//   - Explicit column list + arity mismatch (either direction) →
			//     42601 SYNTAX_ERROR. Java 4.1.5.0+ treats the mismatch
			//     as a parse-level error because the user named the target
			//     columns explicitly.
			//   - Implicit column list (schema-derived) + fewer VALUES than
			//     columns → 22000 CANNOT_CONVERT_TYPE. Java surfaces this
			//     as a type-conversion error since the partial tuple can't
			//     be coerced into the full row.
			if len(exprs) != len(cols) {
				if explicitCols != nil {
					return nil, api.NewErrorf(api.ErrCodeSyntaxError,
						"INSERT column list has %d columns but VALUES has %d", len(cols), len(exprs))
				}
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"column count %d does not match value count %d", len(cols), len(exprs))
			}
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in table %q", col, tableName)
				}
				val, evalErr := evalExpr(ctx, c, nil, exprs[i].Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					// NULL — must reject for NOT NULL columns per SQL standard.
					if fd.Cardinality() == protoreflect.Required {
						return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
							"NULL value in column %q violates NOT NULL constraint", col)
					}
					// Nullable — leave field absent (proto2 optional semantics).
					continue
				}
				protoVal, convErr := functions.ConvertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			// Catch the case where a NOT NULL column is missing from the
			// explicit column list entirely (no value provided at all).
			fds := msgDesc.Fields()
			for i := 0; i < fds.Len(); i++ {
				fd := fds.Get(i)
				if fd.Cardinality() == protoreflect.Required && !msg.Has(fd) {
					return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
						"column %q has NOT NULL constraint but no value was provided", fd.Name())
				}
			}
			// ErrorIfExists: duplicate PRIMARY KEY raises
			// *recordlayer.RecordAlreadyExistsError which wrapSaveRecordError
			// maps to SQLSTATE 23505 (unique_constraint_violation). Without
			// this check, plain SaveRecord silently overwrites the existing
			// row — divergence from Java's INSERT semantics.
			if _, saveErr := store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfExists); saveErr != nil {
				return nil, wrapSaveRecordError(saveErr)
			}
			totalRows++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return totalRows, nil
}

// execInsertSelect implements INSERT INTO table (cols) SELECT ...
// It evaluates the SELECT query and inserts each row into the table.
func (c *EmbeddedConnection) execInsertSelect(ctx context.Context, tableName string, explicitCols []string, body antlrgen.IQueryExpressionBodyContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Execute the SELECT in a separate transaction from the INSERT. The two operations are
	// not atomic — a concurrent writer may modify rows between the SELECT and INSERT
	// (TOCTOU window). This is a known limitation of the current implementation.
	srcCols, srcRows, err := c.execQueryBodyRows(ctx, body)
	if err != nil {
		return 0, err
	}

	var totalRows int64
	_, err = c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Determine target columns. When the user specifies an explicit
		// column list (`INSERT INTO t (c1, c2) SELECT ...`), match by
		// that list. Otherwise fall back to positional mapping against
		// the table's declared field order — matches Postgres / SQL-92
		// semantics. Previously we used srcCols (the SELECT output
		// names), which broke on expression projections like
		// `SELECT id + 100, v * 2` because the synthetic output name
		// "id+100" isn't a real table field.
		var cols []string
		if explicitCols != nil {
			cols = explicitCols
		} else {
			fds := msgDesc.Fields()
			cols = make([]string, fds.Len())
			for i := 0; i < fds.Len(); i++ {
				cols[i] = string(fds.Get(i).Name())
			}
		}
		if len(cols) != len(srcCols) {
			// Java alignment: column-count mismatch errors 22000.
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"column count %d does not match SELECT column count %d", len(cols), len(srcCols))
		}

		for _, row := range srcRows {
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in table %q", col, tableName)
				}
				val := row[i]
				if val == nil {
					// NOT NULL enforcement — matches Java's SQLSTATE 23502.
					if fd.Cardinality() == protoreflect.Required {
						return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
							"NULL value in column %q violates NOT NULL constraint", col)
					}
					continue
				}
				protoVal, convErr := functions.ConvertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			// Missing-from-column-list check, same as execInsert.
			fds := msgDesc.Fields()
			for i := 0; i < fds.Len(); i++ {
				fd := fds.Get(i)
				if fd.Cardinality() == protoreflect.Required && !msg.Has(fd) {
					return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
						"column %q has NOT NULL constraint but no value was provided", fd.Name())
				}
			}
			// ErrorIfExists: same rationale as execInsert above.
			if _, saveErr := store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfExists); saveErr != nil {
				return nil, wrapSaveRecordError(saveErr)
			}
			totalRows++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return totalRows, nil
}

// evalLiteralExpr evaluates a literal expression from an INSERT VALUES list.
// Returns nil for NULL literals.
func evalLiteralExpr(expr antlrgen.IExpressionWithOptionalNameContext) (any, error) {
	pred, ok := expr.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression type %T in INSERT", expr.Expression())
	}
	atomCtx, ok := pred.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom %T in INSERT", pred.ExpressionAtom())
	}
	return evalConstant(atomCtx.Constant())
}

// evalExpr evaluates an expression against msg, returning a scalar driver.Value.
// Used in SELECT projections, UPDATE SET, and WHERE/HAVING predicates.
// Supports: literals, column references, and binary arithmetic (+, -, *, /).
func evalExpr(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (any, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		// Boolean expressions (AND/OR/NOT, comparisons) return bool or
		// nil-for-UNKNOWN when used as a value. Java-aligned: SELECT
		// projection of a boolean expression preserves UNKNOWN as NULL,
		// not collapses to FALSE. Use the tri-state evaluator and map
		// triNull → nil.
		t, err := evalExprPredicateTri(ctx, conn, msg, expr)
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return true, nil
		case triFalse:
			return false, nil
		default:
			return nil, nil
		}
	}
	// If a predicate modifier is present (IN, IS, LIKE, BETWEEN), evaluate
	// via evalExprPredicateTri so UNKNOWN propagates to NULL at projection.
	// Note: IS predicates (IS TRUE / IS FALSE / IS NULL) are 2-valued by
	// definition — the tri-state evaluator already returns triFromBool for
	// them, so their projection collapses cleanly to true/false.
	if pred.Predicate() != nil {
		t, err := evalExprPredicateTri(ctx, conn, msg, expr)
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return true, nil
		case triFalse:
			return false, nil
		default:
			return nil, nil
		}
	}
	return evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
}

// looksBoolean reports whether an expression atom is clearly a boolean
// (comparison or nested parenthesised boolean). Used to route a
// parenthesised group through the tri-state predicate evaluator
// instead of the value evaluator when the inner looks predicate-ish.
// False negatives are OK — they just fall through to the value path
// which handles non-boolean atoms correctly.
func looksBoolean(atom antlrgen.IExpressionAtomContext) bool {
	switch atom.(type) {
	case *antlrgen.BinaryComparisonPredicateContext:
		return true
	case *antlrgen.RecordConstructorExpressionAtomContext:
		return true
	}
	return false
}

func evalExprAtom(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, atom antlrgen.IExpressionAtomContext) (any, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		return evalConstant(a.Constant())
	case *antlrgen.FullColumnNameExpressionAtomContext:
		colName := functions.FullIdToName(a.FullColumnName().FullId())
		// Try inner scope first: strip any qualifier and look up on msg.
		// For qualified `qual.col`, fall through to outer scopes when qual
		// does not match the inner msg's descriptor name — otherwise
		// `emp.id` in an inner `FROM project` would silently resolve to
		// `project.id`. Unqualified `col` prefers inner; falls through
		// only on miss.
		bare := colName
		qual := ""
		if dot := strings.LastIndex(colName, "."); dot >= 0 {
			qual = strings.ToUpper(colName[:dot])
			bare = colName[dot+1:]
		}
		if msg != nil {
			// Inner qualifier match: accept the descriptor name always;
			// also accept any SQL-level alias declared by the current
			// scan (conn.currentSourceAliases, populated by scan loops
			// when they enter), so `FROM project AS p WHERE p.emp_id`
			// resolves p → project even though the descriptor is
			// PROJECT. nil conn (unit-test eval) falls back to the
			// descriptor-only check.
			innerName := strings.ToUpper(string(msg.ProtoReflect().Descriptor().Name()))
			innerMatches := qual == "" || qual == innerName
			if !innerMatches && conn != nil && conn.currentSourceAliases[qual] {
				innerMatches = true
			}
			if innerMatches {
				fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(bare))
				if fd != nil {
					// Absent proto2 optional fields are SQL NULL — distinct from the zero
					// value. Predicates already use Has(); function arguments must too,
					// otherwise UPPER(NULL) would produce "" instead of NULL.
					if !msg.ProtoReflect().Has(fd) {
						return nil, nil
					}
					return functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd)), nil
				}
			}
		}
		// Correlated subquery fallback: walk outer-row stack when inner
		// lookup failed (qualifier mismatch or missing field).
		if conn != nil && len(conn.outerScopes) > 0 {
			v, found, oerr := conn.resolveOuterColumn(colName)
			if oerr != nil {
				return nil, oerr
			}
			if found {
				return v, nil
			}
		}
		if msg == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "column reference %q not allowed in this context", colName)
		}
		return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found", colName)
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		return functions.ApplyMathOp(left, right, a.MathOperator().GetText())
	case *antlrgen.BitExpressionAtomContext:
		// Grammar: bitOperator : '<' '<' | '>' '>' | '&' | '^' | '|'
		// Java registers bitand/bitor/bitxor + shifts in SqlFunctionCatalog.
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		return functions.ApplyBitOp(left, right, a.BitOperator().GetText())
	case *antlrgen.FunctionCallExpressionAtomContext:
		return evalScalarFunctionCall(ctx, conn, msg, a.FunctionCall())
	case *antlrgen.RecordConstructorExpressionAtomContext:
		// A single-field parenthesised group `(expr)` parses as a
		// RecordConstructor with one unnamed expression. SQL convention
		// is that single-element tuples are just the element — treat
		// it as the inner expression. Real multi-field record
		// constructors `(a, b)` / `(a AS x, b AS y)` still error.
		//
		// For boolean predicates like `(b = NULL)`, route through the
		// tri-state predicate evaluator so UNKNOWN propagates as nil
		// (the value-encoding of UNKNOWN — the caller in
		// evalComparisonPredicateTri maps `nil` back to triNull).
		// Without this, a NULL comparison would collapse to FALSE
		// inside the value evaluator and NOT (b = NULL) would wrongly
		// flip to TRUE.
		rc := a.RecordConstructor()
		if rc == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "empty record constructor")
		}
		if rc.STAR() != nil || rc.OfTypeClause() != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "record constructor with STAR / OF TYPE not supported")
		}
		fields := rc.AllExpressionWithOptionalName()
		if len(fields) != 1 {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "multi-field record constructor not supported in this context")
		}
		f := fields[0]
		if f.AS() != nil || f.Uid() != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "named record field not supported in this context")
		}
		inner := f.Expression()
		if pred, ok := inner.(*antlrgen.PredicatedExpressionContext); ok {
			// If the inner expression is a bare predicate (comparison,
			// IS, LIKE, IN, BETWEEN, logical op), evaluate as tri-state.
			// Value-returning atoms fall through to evalExpr below.
			if pred.Predicate() != nil || looksBoolean(pred.ExpressionAtom()) {
				t, err := evalExprPredicateTri(ctx, conn, msg, inner)
				if err != nil {
					return nil, err
				}
				switch t {
				case triTrue:
					return true, nil
				case triFalse:
					return false, nil
				default:
					return nil, nil
				}
			}
		}
		// Non-predicate (e.g. arithmetic, function call, constant) —
		// evaluate as a plain value.
		return evalExpr(ctx, conn, msg, inner)
	case *antlrgen.BinaryComparisonPredicateContext:
		// Comparison used as a value (e.g. SELECT b = true, IF(a > b, ...),
		// CASE WHEN ... END). Java-aligned SQL 3-valued logic: when an
		// operand is NULL the result is UNKNOWN, encoded as nil for the
		// value evaluator. Pre-fix returned false which collapsed UNKNOWN
		// to FALSE — wrong at projection (Java returns NULL).
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		op := a.ComparisonOperator().GetText()
		// IS [NOT] DISTINCT FROM is NULL-safe — must be handled before
		// the generic NULL → UNKNOWN short-circuit below, since two
		// NULLs are NOT distinct (returns true for NOT DISTINCT FROM,
		// false for DISTINCT FROM). Mirrors the tri-predicate path
		// at line 7731. Pre-fix the value-eval path fell through to
		// "unsupported comparison operator" errors for
		// `SELECT (col IS DISTINCT FROM NULL)` projections.
		switch op {
		case "ISDISTINCTFROM":
			return !nullSafeEqual(left, right), nil
		case "ISNOTDISTINCTFROM":
			return nullSafeEqual(left, right), nil
		}
		if left == nil || right == nil {
			return nil, nil
		}
		if !valuesComparable(left, right) {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"cannot compare %T with %T", left, right)
		}
		cmp := functions.CompareValues(left, right)
		switch op {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">":
			return cmp > 0, nil
		case ">=":
			return cmp >= 0, nil
		}
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", op)
	case *antlrgen.SubqueryExpressionAtomContext:
		return evalScalarSubquery(ctx, conn, a.Query())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom %T", atom)
	}
}

// evalScalarSubquery returns a `(SELECT ...)` subquery's single scalar value
// from the connection's pre-populated cache. The cache is filled by
// preEvaluateScalarSubqueries BEFORE the outer query's runInTx starts, so
// the inner query runs as its own top-level transaction — no FDB nested-tx
// weirdness from re-entering runInTx during a scan.
//
// SQL standard semantics: exactly one column (else 42601 syntax error);
// at most one row (else 21000 cardinality violation); zero rows → NULL.
// Uncorrelated only — inner query has no access to outer-row columns.
func evalScalarSubquery(ctx context.Context, conn *EmbeddedConnection, q antlrgen.IQueryContext) (any, error) {
	if conn == nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "scalar subquery not supported in this context")
	}
	if q == nil {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError, "empty subquery")
	}
	if conn.scalarSubqueryCache != nil {
		if v, ok := conn.scalarSubqueryCache[q]; ok {
			return v, nil
		}
	}
	// Fallback path: cache miss (shouldn't happen for well-formed queries
	// but a safety net for code paths that bypass preEvaluateScalarSubqueries).
	// Run the subquery now; this may fail with a nested-tx error if we're
	// inside a scan, but the error is preferable to silent wrong behaviour.
	return runScalarSubqueryOnce(ctx, conn, q)
}

// runScalarSubqueryOnce does the actual execution + arity validation.
// Called both during pre-evaluation (before any outer runInTx) and as a
// fallback from evalScalarSubquery.
func runScalarSubqueryOnce(ctx context.Context, conn *EmbeddedConnection, q antlrgen.IQueryContext) (any, error) {
	cols, rows, err := conn.execQueryBodyRows(ctx, q.QueryExpressionBody())
	if err != nil {
		return nil, err
	}
	if len(cols) != 1 {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError,
			"scalar subquery must return exactly one column, got %d", len(cols))
	}
	if len(rows) > 1 {
		return nil, api.NewErrorf(api.ErrCodeCardinalityViolation,
			"scalar subquery returned %d rows (expected at most 1)", len(rows))
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0][0], nil
}

// preEvaluateScalarSubqueries walks sq's projExprs / whereExpr / havingExpr /
// orderBy expressions, finds every SubqueryExpressionAtomContext, runs each
// once, and stores the result in conn.scalarSubqueryCache. Called before
// the outer query enters runInTx so inner subqueries run as top-level
// transactions (no FDB nesting). Idempotent — already-cached subqueries
// are skipped. Returns the first error hit; on error the cache may be
// partially populated but the caller will abort the outer query anyway.
//
// Uncorrelated only: since the inner query runs before the outer scan
// starts, it cannot reference outer-row columns. Correlated subquery
// support would require a different strategy (per-row re-execution with
// an outer-row binding).
func (c *EmbeddedConnection) preEvaluateScalarSubqueries(ctx context.Context, sq *selectQuery) error {
	if c.scalarSubqueryCache == nil {
		c.scalarSubqueryCache = make(map[antlrgen.IQueryContext]any)
	}
	var walkErr error
	visit := func(expr antlrgen.IExpressionContext) {
		if walkErr != nil || expr == nil {
			return
		}
		walkScalarSubqueries(expr, func(q antlrgen.IQueryContext) {
			if walkErr != nil {
				return
			}
			if _, ok := c.scalarSubqueryCache[q]; ok {
				return
			}
			v, err := runScalarSubqueryOnce(ctx, c, q)
			if err != nil {
				walkErr = err
				return
			}
			c.scalarSubqueryCache[q] = v
		})
	}
	for _, e := range sq.projExprs {
		visit(e)
	}
	if sq.whereExpr != nil {
		visit(sq.whereExpr.Expression())
	}
	visit(sq.havingExpr)
	for _, ob := range sq.orderBy {
		if ob.expr != nil {
			visit(ob.expr)
		}
	}
	return walkErr
}

// walkScalarSubqueries recurses through an expression AST, invoking
// callback for every SubqueryExpressionAtomContext. Mirrors the atom
// shapes understood by evalExprAtom so we do not miss a subquery nested
// inside arithmetic, comparison, function args, or parenthesis groups.
func walkScalarSubqueries(expr antlrgen.IExpressionContext, cb func(antlrgen.IQueryContext)) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *antlrgen.PredicatedExpressionContext:
		walkScalarSubqueriesAtom(e.ExpressionAtom(), cb)
	case *antlrgen.LogicalExpressionContext:
		for i := 0; ; i++ {
			sub := e.Expression(i)
			if sub == nil {
				break
			}
			walkScalarSubqueries(sub, cb)
		}
	case *antlrgen.NotExpressionContext:
		walkScalarSubqueries(e.Expression(), cb)
	}
}

func walkScalarSubqueriesAtom(atom antlrgen.IExpressionAtomContext, cb func(antlrgen.IQueryContext)) {
	if atom == nil {
		return
	}
	switch a := atom.(type) {
	case *antlrgen.SubqueryExpressionAtomContext:
		cb(a.Query())
	case *antlrgen.MathExpressionAtomContext:
		walkScalarSubqueriesAtom(a.GetLeft(), cb)
		walkScalarSubqueriesAtom(a.GetRight(), cb)
	case *antlrgen.BitExpressionAtomContext:
		walkScalarSubqueriesAtom(a.GetLeft(), cb)
		walkScalarSubqueriesAtom(a.GetRight(), cb)
	case *antlrgen.BinaryComparisonPredicateContext:
		walkScalarSubqueriesAtom(a.GetLeft(), cb)
		walkScalarSubqueriesAtom(a.GetRight(), cb)
	case *antlrgen.RecordConstructorExpressionAtomContext:
		if rc := a.RecordConstructor(); rc != nil {
			for _, f := range rc.AllExpressionWithOptionalName() {
				walkScalarSubqueries(f.Expression(), cb)
			}
		}
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Function arguments may contain scalar subqueries (e.g.
		// UPPER((SELECT name FROM t WHERE id = 1))). Recurse into each.
		fc := a.FunctionCall()
		if fc == nil {
			return
		}
		switch f := fc.(type) {
		case *antlrgen.ScalarFunctionCallContext:
			if args := f.FunctionArgs(); args != nil {
				for _, fa := range args.AllFunctionArg() {
					walkScalarSubqueries(fa.Expression(), cb)
				}
			}
		case *antlrgen.UserDefinedScalarFunctionCallContext:
			if args := f.FunctionArgs(); args != nil {
				for _, fa := range args.AllFunctionArg() {
					walkScalarSubqueries(fa.Expression(), cb)
				}
			}
		}
	}
}

// exprEvaluator is the function-pointer adapter that abstracts over the two
// expression-evaluation contexts (proto record vs. map row). Both the scalar
// and specific function cores drive all argument evaluation through this.
type exprEvaluator func(expr antlrgen.IExpressionContext) (driver.Value, error)

// predicateEvaluator is the boolean-predicate counterpart of exprEvaluator,
// used by the searched CASE WHEN branch of evalSpecificFunctionCore.
type predicateEvaluator func(expr antlrgen.IExpressionContext) (bool, error)

// evalScalarFunctionCallCore is the unified implementation shared by
// evalScalarFunctionCall (proto path) and evalScalarFunctionCallOnMap (map
// path). The two callers differ only in how they evaluate sub-expressions;
// that variation is captured in the eval / predicateEval adapters.
//
// unsupportedFmt is the format string ("... %q ...") used for the default
// case — proto and map paths use subtly different wording which we preserve
// verbatim. It must accept exactly one %q for the function name.
func evalScalarFunctionCallCore(
	now time.Time,
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	unsupportedFmt string,
	unsupportedSpecificFmt string,
	fc antlrgen.IFunctionCallContext,
) (driver.Value, error) {
	// Handle CASE expressions routed through SpecificFunctionCall.
	if sf, ok := fc.(*antlrgen.SpecificFunctionCallContext); ok {
		return evalSpecificFunctionCore(now, eval, predicateEval, unsupportedSpecificFmt, sf.SpecificFunction())
	}

	var name string
	var args antlrgen.IFunctionArgsContext
	switch f := fc.(type) {
	case *antlrgen.ScalarFunctionCallContext:
		name = strings.ToUpper(f.ScalarFunctionName().GetText())
		args = f.FunctionArgs()
	case *antlrgen.UserDefinedScalarFunctionCallContext:
		name = strings.ToUpper(f.UserDefinedScalarFunctionName().GetText())
		args = f.FunctionArgs()
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported function call type %T", fc)
	}
	var fArgs []antlrgen.IFunctionArgContext
	if args != nil {
		fArgs = args.AllFunctionArg()
	}
	switch name {
	case "COALESCE":
		for _, fa := range fArgs {
			v, err := eval(fa.Expression())
			if err != nil {
				return nil, err
			}
			if v != nil {
				return v, nil
			}
		}
		return nil, nil
	case "IFNULL":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "IFNULL requires 2 arguments")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if v != nil {
			return v, nil
		}
		return eval(fArgs[1].Expression())
	case "UPPER":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "UPPER requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "UPPER: argument must be string, got %T", v)
		}
		return strings.ToUpper(s), nil
	case "LOWER":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOWER requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOWER: argument must be string, got %T", v)
		}
		return strings.ToLower(s), nil
	case "LENGTH", "LEN", "CHAR_LENGTH", "CHARACTER_LENGTH":
		// LENGTH / CHAR_LENGTH are synonyms in SQL:2003 and across
		// Postgres / Oracle / SQL Server when applied to a string —
		// all count logical characters (Unicode code points), not
		// bytes. CHARACTER_LENGTH is the spec name; LENGTH and LEN
		// are the common short forms. Byte-length is OCTET_LENGTH.
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be string, got %T", name, v)
		}
		return int64(utf8.RuneCountInString(s)), nil
	case "OCTET_LENGTH":
		// SQL:2003 OCTET_LENGTH — byte count of a string / bytes value,
		// regardless of encoding. Distinct from CHAR_LENGTH which counts
		// Unicode code points. Both Postgres and Oracle support it.
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "OCTET_LENGTH requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		switch x := v.(type) {
		case string:
			return int64(len(x)), nil
		case []byte:
			return int64(len(x)), nil
		default:
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "OCTET_LENGTH: argument must be STRING or BYTES, got %T", v)
		}
	case "TRIM", "LTRIM", "RTRIM":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be string, got %T", name, v)
		}
		switch name {
		case "LTRIM":
			return strings.TrimLeft(s, " \t\n\r"), nil
		case "RTRIM":
			return strings.TrimRight(s, " \t\n\r"), nil
		}
		return strings.TrimSpace(s), nil
	case "ABS":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "ABS requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		switch n := v.(type) {
		case int64:
			// Two's-complement: -math.MinInt64 overflows back to MinInt64.
			// MySQL/Postgres error; mirror that here rather than returning
			// the still-negative value.
			if n == math.MinInt64 {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
					"ABS: integer overflow for MinInt64 (-9223372036854775808)")
			}
			if n < 0 {
				return -n, nil
			}
			return n, nil
		case float64:
			if n < 0 {
				return -n, nil
			}
			return n, nil
		default:
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "ABS: argument must be numeric, got %T", v)
		}
	case "FLOOR", "CEIL", "CEILING", "ROUND":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires at least 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		var f float64
		switch n := v.(type) {
		case int64:
			return n, nil // already integer
		case float64:
			f = n
		default:
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be numeric", name)
		}
		var result float64
		switch name {
		case "FLOOR":
			result = math.Floor(f)
		case "CEIL", "CEILING":
			result = math.Ceil(f)
		case "ROUND":
			decimals := int64(0)
			if len(fArgs) >= 2 {
				dv, derr := eval(fArgs[1].Expression())
				if derr != nil {
					return nil, derr
				}
				// NULL decimals → NULL result (SQL standard NULL propagation).
				if dv == nil {
					return nil, nil
				}
				d, ierr := functions.ToIntegerArg(dv, "ROUND", "decimals")
				if ierr != nil {
					return nil, ierr
				}
				decimals = d
			}
			if decimals == 0 {
				result = math.Round(f)
			} else {
				factor := math.Pow(10, float64(decimals))
				result = math.Round(f*factor) / factor
			}
		}
		// Return int64 if no fractional part.
		if result == math.Trunc(result) && result >= math.MinInt64 && result <= math.MaxInt64 {
			return int64(result), nil
		}
		return result, nil
	case "MOD":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD requires 2 arguments")
		}
		av, aerr := eval(fArgs[0].Expression())
		if aerr != nil || av == nil {
			return nil, aerr
		}
		bv, berr := eval(fArgs[1].Expression())
		if berr != nil || bv == nil {
			return nil, berr
		}
		toFloat := func(v driver.Value) (float64, bool) {
			switch n := v.(type) {
			case int64:
				return float64(n), true
			case float64:
				return n, true
			}
			return 0, false
		}
		af, aok := toFloat(av)
		bf, bok := toFloat(bv)
		if !aok || !bok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD: arguments must be numeric")
		}
		if bf == 0 {
			return nil, api.NewErrorf(api.ErrCodeDivisionByZero, "MOD: division by zero")
		}
		if _, aIsInt := av.(int64); aIsInt {
			if _, bIsInt := bv.(int64); bIsInt {
				return int64(af) % int64(bf), nil
			}
		}
		return math.Mod(af, bf), nil
	case "POWER", "POW":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POWER requires 2 arguments")
		}
		baseV, berr := eval(fArgs[0].Expression())
		if berr != nil || baseV == nil {
			return nil, berr
		}
		expV, eerr := eval(fArgs[1].Expression())
		if eerr != nil || expV == nil {
			return nil, eerr
		}
		base, ok := functions.ToFloat64(baseV)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POWER: base must be numeric, got %T", baseV)
		}
		exp, ok := functions.ToFloat64(expV)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POWER: exponent must be numeric, got %T", expV)
		}
		result := math.Pow(base, exp)
		// NaN (e.g. POWER(-1, 0.5)) and ±Inf (e.g. POWER(0, -1)) are math
		// domain errors. SQL standard says these are undefined; returning
		// NULL matches SQRT's existing negative-arg convention on this
		// engine and avoids poisoning downstream aggregates / comparisons
		// (which treat NaN != NaN).
		if math.IsNaN(result) || math.IsInf(result, 0) {
			return nil, nil
		}
		if result == math.Trunc(result) && result >= math.MinInt64 && result <= math.MaxInt64 {
			return int64(result), nil
		}
		return result, nil
	case "SIGN":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SIGN requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		switch n := v.(type) {
		case int64:
			if n > 0 {
				return int64(1), nil
			} else if n < 0 {
				return int64(-1), nil
			}
			return int64(0), nil
		case float64:
			if n > 0 {
				return float64(1), nil
			} else if n < 0 {
				return float64(-1), nil
			}
			return float64(0), nil
		}
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SIGN: argument must be numeric")
	case "CONCAT", "CONCAT_WS":
		// CONCAT_WS(sep, s1, s2, ...) — first arg is separator.
		// CONCAT(s1, s2, ...) — no separator.
		sep := ""
		startIdx := 0
		if name == "CONCAT_WS" {
			if len(fArgs) < 1 {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "CONCAT_WS requires at least 1 argument")
			}
			sv, err := eval(fArgs[0].Expression())
			if err != nil {
				return nil, err
			}
			if sv != nil {
				sep = fmt.Sprintf("%v", sv)
			}
			startIdx = 1
		}
		var parts []string
		for _, fa := range fArgs[startIdx:] {
			v, err := eval(fa.Expression())
			if err != nil {
				return nil, err
			}
			if v == nil {
				// NULL-skip behaviour, matching MySQL and Postgres's
				// CONCAT(). SQL standard / Oracle / SQL Server
				// propagate NULL through concatenation instead —
				// pinned as-is by trim_concat.yaml until a Java
				// reference settles the question.
				continue
			}
			parts = append(parts, fmt.Sprintf("%v", v))
		}
		return strings.Join(parts, sep), nil
	case "NULLIF":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "NULLIF requires 2 arguments")
		}
		a, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		b, err2 := eval(fArgs[1].Expression())
		if err2 != nil {
			return nil, err2
		}
		if functions.CompareValues(a, b) == 0 {
			return nil, nil
		}
		return a, nil
	case "GREATEST", "LEAST":
		// Java conformance: GREATEST/LEAST return NULL if any argument
		// is NULL. VariadicFunctionValue.PhysicalOperator's per-typecode
		// lambdas (GREATEST_INT/LONG/FLOAT/DOUBLE/STRING/BOOLEAN, and
		// the LEAST_* mirror) all short-circuit `if (i == null) return null`
		// on the first NULL arg. Postgres skips NULLs; Oracle and Java
		// propagate them. Match Java.
		if len(fArgs) == 0 {
			return nil, nil
		}
		best, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if best == nil {
			return nil, nil
		}
		isGreatest := name == "GREATEST"
		for _, fa := range fArgs[1:] {
			v, verr := eval(fa.Expression())
			if verr != nil {
				return nil, verr
			}
			if v == nil {
				return nil, nil
			}
			// Java alignment: cross-type GREATEST/LEAST errors 22000
			// (CANNOT_CONVERT_TYPE), matching the comparison-operator
			// path. Pre-fix Go silently picked one via the type-name
			// string compare in compareValues, yielding semantically
			// undefined results.
			if !valuesComparable(v, best) {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"cannot compare %T with %T in %s", v, best, name)
			}
			cmp := functions.CompareValues(v, best)
			if (isGreatest && cmp > 0) || (!isGreatest && cmp < 0) {
				best = v
			}
		}
		return best, nil
	case "PI":
		return math.Pi, nil
	case "SQRT":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SQRT requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		f, ok := functions.ToFloat64(v)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SQRT: argument must be numeric, got %T", v)
		}
		// NaN input already fails the < 0 check (NaN comparisons always
		// return false), so it would propagate a NaN result. Treat it the
		// same as the negative-arg case — NULL.
		if math.IsNaN(f) || f < 0 {
			return nil, nil // SQRT of NaN or negative returns NULL per SQL standard.
		}
		return math.Sqrt(f), nil
	case "EXP":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "EXP requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		f, ok := functions.ToFloat64(v)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "EXP: argument must be numeric, got %T", v)
		}
		result := math.Exp(f)
		if math.IsInf(result, 0) || math.IsNaN(result) {
			return nil, nil // Overflow / NaN → NULL, matching MySQL.
		}
		return result, nil
	case "LN":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LN requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		f, ok := functions.ToFloat64(v)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LN: argument must be numeric, got %T", v)
		}
		// NaN: `f <= 0` is false for NaN, so guard misses. Same treatment
		// as <= 0 — undefined → NULL.
		if math.IsNaN(f) || f <= 0 {
			return nil, nil
		}
		return math.Log(f), nil
	case "LOG":
		// LOG(x) = natural log; LOG(base, x) = log_base(x).
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOG requires 1 or 2 arguments")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		f, ok := functions.ToFloat64(v)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOG: argument must be numeric, got %T", v)
		}
		if len(fArgs) == 1 {
			// NaN input: `f <= 0` is false for NaN, so the guard misses.
			// Same treatment as <= 0 — undefined → NULL.
			if math.IsNaN(f) || f <= 0 {
				return nil, nil
			}
			return math.Log(f), nil
		}
		v2, err := eval(fArgs[1].Expression())
		if err != nil || v2 == nil {
			return nil, err
		}
		f2, ok := functions.ToFloat64(v2)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOG: argument must be numeric, got %T", v2)
		}
		if math.IsNaN(f) || math.IsNaN(f2) || f <= 0 || f == 1 || f2 <= 0 {
			return nil, nil
		}
		return math.Log(f2) / math.Log(f), nil
	case "REVERSE":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "REVERSE requires 1 argument")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		s := fmt.Sprintf("%v", sv)
		runes := []rune(s)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return string(runes), nil
	case "POSITION":
		// POSITION(substr, str) — 1-based rune index of first occurrence, 0 if not found.
		// (POSITION(substr IN str) has a special grammar form — not supported here.)
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POSITION requires 2 arguments")
		}
		substrV, err := eval(fArgs[0].Expression())
		if err != nil || substrV == nil {
			return nil, err
		}
		strV, err := eval(fArgs[1].Expression())
		if err != nil || strV == nil {
			return nil, err
		}
		needle := fmt.Sprintf("%v", substrV)
		haystack := fmt.Sprintf("%v", strV)
		byteIdx := strings.Index(haystack, needle)
		if byteIdx < 0 {
			return int64(0), nil
		}
		return int64(utf8.RuneCountInString(haystack[:byteIdx]) + 1), nil
	case "LEFT":
		// LEFT(str, n) — first n runes, or whole string if n >= length.
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LEFT requires 2 arguments")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		s := fmt.Sprintf("%v", sv)
		nVal, nErr := eval(fArgs[1].Expression())
		if nErr != nil {
			return nil, nErr
		}
		n, err := functions.ToIntegerArg(nVal, "LEFT", "length")
		if err != nil {
			return nil, err
		}
		if n < 0 {
			n = 0
		}
		runes := []rune(s)
		if int(n) >= len(runes) {
			return s, nil
		}
		return string(runes[:n]), nil
	case "RIGHT":
		// RIGHT(str, n) — last n runes, or whole string if n >= length.
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "RIGHT requires 2 arguments")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		s := fmt.Sprintf("%v", sv)
		nVal, nErr := eval(fArgs[1].Expression())
		if nErr != nil {
			return nil, nErr
		}
		n, err := functions.ToIntegerArg(nVal, "RIGHT", "length")
		if err != nil {
			return nil, err
		}
		if n < 0 {
			n = 0
		}
		runes := []rune(s)
		if int(n) >= len(runes) {
			return s, nil
		}
		return string(runes[len(runes)-int(n):]), nil
	case "SUBSTRING", "SUBSTR":
		// SUBSTRING(str, pos [, len]) — 1-based position per SQL standard.
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SUBSTRING requires at least 2 arguments")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		s := fmt.Sprintf("%v", sv)
		posVal, posErr := eval(fArgs[1].Expression())
		if posErr != nil {
			return nil, posErr
		}
		pos, err := functions.ToIntegerArg(posVal, "SUBSTRING", "position")
		if err != nil {
			return nil, err
		}
		if pos < 1 {
			pos = 1
		}
		runes := []rune(s)
		start := int(pos) - 1
		if start >= len(runes) {
			return "", nil
		}
		if len(fArgs) >= 3 {
			lenVal, lenErr := eval(fArgs[2].Expression())
			if lenErr != nil {
				return nil, lenErr
			}
			n, err := functions.ToIntegerArg(lenVal, "SUBSTRING", "length")
			if err != nil {
				return nil, err
			}
			end := start + int(n)
			if end > len(runes) {
				end = len(runes)
			}
			if end < start {
				return "", nil
			}
			return string(runes[start:end]), nil
		}
		return string(runes[start:]), nil
	case "REPLACE":
		// REPLACE(str, from, to)
		if len(fArgs) < 3 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "REPLACE requires 3 arguments")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		fromV, err := eval(fArgs[1].Expression())
		if err != nil || fromV == nil {
			return nil, err
		}
		toV, err := eval(fArgs[2].Expression())
		if err != nil {
			return nil, err
		}
		toStr := ""
		if toV != nil {
			toStr = fmt.Sprintf("%v", toV)
		}
		return strings.ReplaceAll(fmt.Sprintf("%v", sv), fmt.Sprintf("%v", fromV), toStr), nil
	case "IF", "IIF":
		// IF(cond, true_val, false_val)
		if len(fArgs) < 3 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "IF requires 3 arguments")
		}
		cond, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if functions.IsTruthy(cond) {
			return eval(fArgs[1].Expression())
		}
		return eval(fArgs[2].Expression())
	case "NOW", "CURDATE", "CURTIME", "SYSDATE", "UTC_TIMESTAMP", "UTC_DATE", "UTC_TIME":
		// MySQL-style datetime aliases. NOW/SYSDATE/UTC_TIMESTAMP →
		// CURRENT_TIMESTAMP; CURDATE/UTC_DATE → CURRENT_DATE;
		// CURTIME/UTC_TIME → CURRENT_TIME. All take 0 args (a fractional
		// seconds precision arg is ignored if present). Use the
		// statement timestamp for within-statement consistency.
		switch name {
		case "CURDATE", "UTC_DATE":
			return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
		default:
			return now, nil
		}
	case "YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "SECOND",
		"DAYOFMONTH", "DAYOFWEEK", "DAYOFYEAR":
		// Date-part functions taking a single time.Time argument.
		// SQL standard returns an integer (1-based for month/day/dow,
		// 0-based for hour/minute/second). Mostly aligns with Go's
		// time accessors; DAYOFWEEK returns 1=Sunday..7=Saturday per
		// MySQL/Oracle (Go's Weekday is 0=Sunday..6=Saturday → +1).
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		t, ok := v.(time.Time)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be a date/time, got %T", name, v)
		}
		switch name {
		case "YEAR":
			return int64(t.Year()), nil
		case "MONTH":
			return int64(t.Month()), nil
		case "DAY", "DAYOFMONTH":
			return int64(t.Day()), nil
		case "HOUR":
			return int64(t.Hour()), nil
		case "MINUTE":
			return int64(t.Minute()), nil
		case "SECOND":
			return int64(t.Second()), nil
		case "DAYOFWEEK":
			// MySQL convention: Sunday=1, Saturday=7.
			return int64(t.Weekday()) + 1, nil
		case "DAYOFYEAR":
			return int64(t.YearDay()), nil
		}
		return nil, nil // unreachable
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, unsupportedFmt, name)
	}
}

// makeProtoExprEvaluator builds the exprEvaluator adapter for the proto path.
// evalExpr returns (any, error); driver.Value is an alias for any so the
// conversion is a no-op except we explicitly preserve nil → nil.
func makeProtoExprEvaluator(ctx context.Context, conn *EmbeddedConnection, msg proto.Message) exprEvaluator {
	return func(e antlrgen.IExpressionContext) (driver.Value, error) {
		v, err := evalExpr(ctx, conn, msg, e)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		return driver.Value(v), nil
	}
}

// makeMapExprEvaluator builds the exprEvaluator adapter for the map path.
func makeMapExprEvaluator(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value) exprEvaluator {
	return func(e antlrgen.IExpressionContext) (driver.Value, error) {
		return evalExprOnMap(ctx, conn, row, e)
	}
}

func evalScalarFunctionCall(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, fc antlrgen.IFunctionCallContext) (any, error) {
	eval := makeProtoExprEvaluator(ctx, conn, msg)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalExprPredicate(ctx, conn, msg, e)
	}
	return evalScalarFunctionCallCore(conn.statementNow(), eval, predEval, "unsupported scalar function %q", "unsupported specific function %T", fc)
}

func evalScalarFunctionCallOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, fc antlrgen.IFunctionCallContext) (driver.Value, error) {
	eval := makeMapExprEvaluator(ctx, conn, row)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalPredicateOnMapExpr(ctx, conn, row, e)
	}
	return evalScalarFunctionCallCore(conn.statementNow(), eval, predEval, "unsupported function %q in map eval context", "unsupported specific function %T in map eval", fc)
}

// statementNow forwards to Session.StatementNow. Retained as a
// thin shim while exec* callers still live in this file; will be
// deleted as Phase 1c moves those bodies into core/plan/physical.
func (c *EmbeddedConnection) statementNow() time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	return c.sess.StatementNow()
}

// beginStatement forwards to Session.BeginStatement. Thin shim —
// see statementNow's note for removal trigger.
func (c *EmbeddedConnection) beginStatement() func() {
	return c.sess.BeginStatement()
}

// evalSpecificFunctionCore is the unified implementation shared by
// evalSpecificFunction (proto path) and evalSpecificFunctionOnMap (map path).
// Handles grammar-level SpecificFunction nodes: CASE WHEN ... END, simple CASE,
// CAST(expr AS type), and the no-argument datetime / user functions
// (CURRENT_DATE, CURRENT_TIME, CURRENT_TIMESTAMP, LOCALTIME, CURRENT_USER).
// The searched CASE branch needs a boolean predicate evaluator, hence
// predicateEval in addition to eval.
//
// unsupportedFmt must accept exactly one %T for the specific-function type.
func evalSpecificFunctionCore(
	now time.Time,
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	unsupportedFmt string,
	sf antlrgen.ISpecificFunctionContext,
) (driver.Value, error) {
	switch c := sf.(type) {
	case *antlrgen.SimpleFunctionCallContext:
		// CURRENT_DATE / CURRENT_TIME / CURRENT_TIMESTAMP / LOCALTIME /
		// CURRENT_USER. SQL standard says all references to these
		// functions within one statement return the same value (statement
		// timestamp). `now` is captured by the caller from
		// conn.statementNow() at the start of statement execution.
		switch {
		case c.CURRENT_DATE() != nil:
			return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
		case c.CURRENT_TIMESTAMP() != nil, c.LOCALTIME() != nil:
			return now, nil
		case c.CURRENT_TIME() != nil:
			// CURRENT_TIME returns just the time-of-day portion; we
			// surface the full timestamp because Go has no time-only
			// type and yamsql doesn't pin time-only values either.
			return now, nil
		case c.CURRENT_USER() != nil:
			// No user-identity concept yet; return empty string. The
			// connection tracks dbPath/schema, not a user. Java's
			// fdb-relational returns empty too.
			return "", nil
		}
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported simple function call")
	case *antlrgen.CaseFunctionCallContext:
		// Searched CASE: CASE WHEN cond THEN val ... [ELSE val] END
		// WHEN conditions are full boolean expressions (comparisons, AND/OR, etc.).
		for _, alt := range c.AllCaseFuncAlternative() {
			ok, err := predicateEval(alt.GetCondition().Expression())
			if err != nil {
				return nil, err
			}
			if ok {
				return eval(alt.GetConsequent().Expression())
			}
		}
		if c.GetElseArg() != nil {
			return eval(c.GetElseArg().Expression())
		}
		return nil, nil
	case *antlrgen.CaseExpressionFunctionCallContext:
		// Simple CASE: CASE expr WHEN val THEN result ... [ELSE result] END
		subject, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		for _, alt := range c.AllCaseFuncAlternative() {
			whenVal, wErr := eval(alt.GetCondition().Expression())
			if wErr != nil {
				return nil, wErr
			}
			// Simple CASE uses = semantics; NULL = anything is UNKNOWN, so a
			// NULL subject or whenVal never matches a branch (falls to ELSE).
			if subject == nil || whenVal == nil {
				continue
			}
			if functions.CompareValues(subject, whenVal) == 0 {
				return eval(alt.GetConsequent().Expression())
			}
		}
		if c.GetElseArg() != nil {
			return eval(c.GetElseArg().Expression())
		}
		return nil, nil
	case *antlrgen.DataTypeFunctionCallContext:
		// CAST(expr AS type)
		val, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil // CAST(NULL AS type) = NULL
		}
		typeName := strings.ToUpper(c.ConvertedDataType().GetText())
		return functions.CastValue(val, typeName)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, unsupportedFmt, sf)
	}
}

// triBool is a Kleene three-valued truth type used by predicate evaluators so
// that NOT/AND/OR preserve SQL UNKNOWN (NULL) instead of collapsing it to
// FALSE. In a WHERE/HAVING/ON boundary, only triTrue keeps the row — both
// triFalse and triNull filter it out, matching SQL semantics.
type triBool int8

const (
	triFalse triBool = iota
	triTrue
	triNull
)

func triFromBool(b bool) triBool {
	if b {
		return triTrue
	}
	return triFalse
}

// IsTrue reports whether the value is strictly TRUE. UNKNOWN is NOT true —
// this is the predicate-filter boundary: `if !t.IsTrue() { skip row }`.
func (t triBool) IsTrue() bool { return t == triTrue }

// Not implements SQL's NOT with UNKNOWN preservation: NOT TRUE = FALSE,
// NOT FALSE = TRUE, NOT UNKNOWN = UNKNOWN.
func (t triBool) Not() triBool {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triNull
}

// triAnd implements SQL's AND: FALSE AND x = FALSE, otherwise UNKNOWN if either
// is UNKNOWN, else TRUE. Short-circuit on FALSE is done by the caller.
func triAnd(a, b triBool) triBool {
	if a == triFalse || b == triFalse {
		return triFalse
	}
	if a == triNull || b == triNull {
		return triNull
	}
	return triTrue
}

// triOr implements SQL's OR: TRUE OR x = TRUE, otherwise UNKNOWN if either is
// UNKNOWN, else FALSE. Short-circuit on TRUE is done by the caller.
func triOr(a, b triBool) triBool {
	if a == triTrue || b == triTrue {
		return triTrue
	}
	if a == triNull || b == triNull {
		return triNull
	}
	return triFalse
}

// execUpdate executes UPDATE <table> SET col = val [, ...] [WHERE col = val].
func (c *EmbeddedConnection) execUpdate(ctx context.Context, upd antlrgen.IUpdateStatementContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	tableName := functions.FullIdToName(upd.TableName().FullId())
	whereExpr := upd.WhereExpr()
	updatedElems := upd.AllUpdatedElement()

	var updated int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		updated = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := pkPushdownCursor(ctx, c, store, rt, md, whereExpr, tableName)
		defer cursor.Close() //nolint:errcheck

		// Record the source alias so correlated EXISTS / IN inside WHERE
		// can resolve outer-row refs. UPDATE/DELETE don't expose a user
		// alias in the grammar today; descriptor name + tableName match.
		defer c.pushSourceAliases(tableName)()

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(ctx, c, rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}

			cloned := proto.Clone(rec.Record)
			clonedRefl := cloned.ProtoReflect()
			for _, elem := range updatedElems {
				colName := functions.FullIdToName(elem.FullColumnName().FullId())
				fd := msgDesc.Fields().ByName(protoreflect.Name(colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in table %q", colName, tableName)
				}
				val, evalErr := evalExpr(ctx, c, cloned, elem.Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					// UPDATE SET col = NULL on a NOT NULL column must reject
					// with ErrCodeNotNullViolation (23502), matching Java.
					if fd.Cardinality() == protoreflect.Required {
						return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
							"NULL value in column %q violates NOT NULL constraint", colName)
					}
					clonedRefl.Clear(fd)
					continue
				}
				protoVal, convErr := functions.ConvertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				clonedRefl.Set(fd, protoVal)
			}
			// UPDATE legitimately overwrites an existing record, so no
			// existence check — but secondary UNIQUE indexes can still
			// fire if the UPDATE sets an indexed column to a value
			// another row already holds. Wrap so callers get SQLSTATE
			// 23505 instead of the raw recordlayer error type.
			if _, saveErr := store.SaveRecord(cloned); saveErr != nil {
				return nil, wrapSaveRecordError(saveErr)
			}
			updated++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
}

// execDelete executes DELETE FROM <table> [WHERE col = value].
func (c *EmbeddedConnection) execDelete(ctx context.Context, del antlrgen.IDeleteStatementContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	tableName := functions.FullIdToName(del.TableName().FullId())
	whereExpr := del.WhereExpr()

	var deleted int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		deleted = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "table %q not found in schema", tableName)
		}

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := pkPushdownCursor(ctx, c, store, rt, md, whereExpr, tableName)
		defer cursor.Close() //nolint:errcheck

		// Record the source alias so correlated EXISTS / IN inside WHERE
		// can resolve outer-row refs (mirrors execUpdate).
		defer c.pushSourceAliases(tableName)()

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(ctx, c, rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			if _, delErr := store.DeleteRecord(rec.PrimaryKey); delErr != nil {
				return nil, delErr
			}
			deleted++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// evalPredicate returns true if msg satisfies whereExpr.
// Only col = constant comparisons are supported. If whereExpr is nil, returns true.
func evalPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, whereExpr antlrgen.IWhereExprContext) (bool, error) {
	if whereExpr == nil {
		return true, nil
	}
	return evalExprPredicate(ctx, conn, msg, whereExpr.Expression())
}

// evalExprPredicate evaluates an IExpressionContext as a boolean predicate.
// Supports: col = constant, col != constant, col < constant, col > constant,
// col <= constant, col >= constant, AND, OR, NOT.
func evalExprPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalExprPredicateTri(ctx, conn, msg, expr)
	return t.IsTrue(), err
}

// evalExprPredicateTri is the Kleene three-valued implementation: UNKNOWN
// propagates through AND/OR/NOT so `NOT (x = NULL)` correctly stays UNKNOWN
// (filtered out) instead of flipping to TRUE. The bool wrapper above
// collapses UNKNOWN→false at the WHERE/HAVING filter boundary.
func evalExprPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (triBool, error) {
	switch e := expr.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		// Push outer-row scope so a correlated inner reference like
		// `outer_tbl.col` resolves against this msg via resolveOuterColumn.
		// Qualifier taken from the proto descriptor name (single-source
		// FROM without an explicit AS alias — the common case).
		defer conn.pushOuterScope(outerScopeFromMsg(conn, msg))()
		_, subRows, subErr := conn.execQueryBodyRows(ctx, e.Query().QueryExpressionBody())
		if subErr != nil {
			return triFalse, subErr
		}
		return triFromBool(len(subRows) > 0), nil

	case *antlrgen.LogicalExpressionContext:
		left, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(0))
		if err != nil {
			return triFalse, err
		}
		op := e.LogicalOperator()
		// Grammar: AND | '&' '&' | XOR | OR | '|' '|'. op.AND()/OR()/XOR()
		// are only non-nil for the keyword forms; the symbolic `&&` and
		// `||` forms need text-based detection.
		opText := strings.ReplaceAll(op.GetText(), " ", "")
		isAnd := op.AND() != nil || opText == "&&"
		isOr := op.OR() != nil || opText == "||"
		isXor := op.XOR() != nil
		switch {
		case isAnd:
			if left == triFalse {
				return triFalse, nil // short-circuit
			}
			right, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		case isOr:
			if left == triTrue {
				return triTrue, nil // short-circuit
			}
			right, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triOr(left, right), nil
		case isXor:
			// SQL XOR: a XOR b = (a AND NOT b) OR (NOT a AND b). Any NULL
			// operand → NULL (can't short-circuit without both concrete).
			right, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			if left == triNull || right == triNull {
				return triNull, nil
			}
			return triFromBool((left == triTrue) != (right == triTrue)), nil
		}
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported logical operator %q", op.GetText())

	case *antlrgen.NotExpressionContext:
		v, err := evalExprPredicateTri(ctx, conn, msg, e.Expression())
		if err != nil {
			return triFalse, err
		}
		return v.Not(), nil

	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			switch p := e.Predicate().(type) {
			case *antlrgen.InPredicateContext:
				return evalInPredicateTri(ctx, conn, msg, e, p)
			case *antlrgen.IsExpressionContext:
				// IS NULL / IS TRUE / IS FALSE are always 2-state (never UNKNOWN).
				b, err := evalIsNullPredicate(ctx, conn, msg, e, p)
				return triFromBool(b), err
			case *antlrgen.LikePredicateContext:
				return evalLikePredicateTri(ctx, conn, msg, e, p)
			case *antlrgen.BetweenComparisonPredicateContext:
				return evalBetweenPredicateTri(ctx, conn, msg, e, p)
			}
		}
		return evalComparisonPredicateTri(ctx, conn, msg, e)

	default:
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T", expr)
	}
}

// evalComparisonPredicateTri handles a leaf comparison between two arbitrary
// expressions. Returns triNull when either operand is NULL so that enclosing
// NOT/AND/OR can apply proper Kleene logic (previously NULL collapsed to FALSE
// and `NOT (x = NULL)` returned TRUE).
func evalComparisonPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext) (triBool, error) {
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		// Non-comparison atom (e.g. `WHERE CASE WHEN ... END`, `WHERE some_bool_fn(x)`)
		// — evaluate as a value. NULL result is UNKNOWN; else use truthiness.
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return triFalse, err
		}
		if v == nil {
			return triNull, nil
		}
		return triFromBool(functions.IsTruthy(v)), nil
	}
	opText := bcp.ComparisonOperator().GetText()

	left, err := evalExprAtom(ctx, conn, msg, bcp.GetLeft())
	if err != nil {
		return triFalse, err
	}
	right, err := evalExprAtom(ctx, conn, msg, bcp.GetRight())
	if err != nil {
		return triFalse, err
	}
	// SQL `IS [NOT] DISTINCT FROM` is null-safe equality — it always
	// returns TRUE or FALSE, never UNKNOWN, even when operands are NULL.
	// Grammar joins tokens without whitespace: `IS DISTINCT FROM` →
	// "ISDISTINCTFROM", `IS NOT DISTINCT FROM` → "ISNOTDISTINCTFROM".
	// Must branch BEFORE the any-NULL → UNKNOWN fallback below.
	switch opText {
	case "ISDISTINCTFROM":
		return triFromBool(!nullSafeEqual(left, right)), nil
	case "ISNOTDISTINCTFROM":
		return triFromBool(nullSafeEqual(left, right)), nil
	}
	// SQL 3-valued logic: any other comparison involving NULL is UNKNOWN.
	// Use IS NULL / IS NOT NULL for explicit NULL tests.
	if left == nil || right == nil {
		return triNull, nil
	}

	// Java alignment: Java's PromoteValue.isPromotionNeeded errors with
	// SemanticException(INCOMPATIBLE_TYPE) → SQLSTATE 22000
	// (CANNOT_CONVERT_TYPE) when the two operands have non-promotable
	// types (e.g. STRING vs BIGINT). Pre-fix Go silently returned
	// FALSE for these comparisons → empty result set, the dangerous
	// kind of bug. Now we error to match Java.
	if !valuesComparable(left, right) {
		return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
			"cannot compare %T with %T", left, right)
	}

	cmp := functions.CompareValues(left, right)
	switch opText {
	case "=":
		return triFromBool(cmp == 0), nil
	case "!=", "<>":
		return triFromBool(cmp != 0), nil
	case "<":
		return triFromBool(cmp < 0), nil
	case ">":
		return triFromBool(cmp > 0), nil
	case "<=":
		return triFromBool(cmp <= 0), nil
	case ">=":
		return triFromBool(cmp >= 0), nil
	default:
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
	}
}

// valuesComparable reports whether two non-NULL driver values can be
// compared by SQL `=`/`<`/`>`/etc. without an explicit CAST. Mirrors
// Java's PromoteValue.isPromotionNeeded outcome: numeric↔numeric is
// always OK (auto-promote int→float); same concrete type is OK;
// everything else is incompatible. Both args must be non-nil.
func valuesComparable(a, b driver.Value) bool {
	_, aInt := a.(int64)
	_, aFloat := a.(float64)
	_, bInt := b.(int64)
	_, bFloat := b.(float64)
	if (aInt || aFloat) && (bInt || bFloat) {
		return true
	}
	return reflect.TypeOf(a) == reflect.TypeOf(b)
}

// nullSafeEqual is the underpinning of SQL's `IS NOT DISTINCT FROM`: two
// NULLs are equal, a NULL and a non-NULL are never equal, and two non-NULL
// values are compared by valuesEqual (same type-strict rules as `=`).
func nullSafeEqual(a, b driver.Value) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return valuesEqual(a, b)
}

// matchSubqueryIN evaluates `fieldVal [NOT] IN (subRows)` per SQL §8.4.
// Returns triTrue/triFalse if a concrete match/non-match can be decided,
// or triNull when no concrete match is found and at least one subquery row
// contributed a NULL (the expansion into an AND/OR chain of equalities
// collapses to UNKNOWN in that case). WHERE callers collapse triNull to
// false; NOT IN sees an UNKNOWN that must not flip to TRUE.
func matchSubqueryIN(fieldVal driver.Value, subRows [][]driver.Value, negated bool) (triBool, error) {
	var hadNull bool
	for _, row := range subRows {
		if len(row) == 0 {
			continue
		}
		v := row[0]
		if v == nil {
			// NULL in subquery result contributes UNKNOWN to the expansion.
			hadNull = true
			continue
		}
		// Cross-type comparison is 22000 per Java alignment (matches the
		// IN-list path's valuesComparable check at evalInPredicateTri).
		// fieldVal != nil is guaranteed by callers — evalInPredicateTri
		// returns triNull early on NULL fieldVal.
		if fieldVal != nil && !valuesComparable(fieldVal, v) {
			return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
				"subquery IN: cannot compare %T and %T", fieldVal, v)
		}
		if valuesEqual(fieldVal, v) {
			if negated {
				return triFalse, nil
			}
			return triTrue, nil
		}
	}
	if hadNull {
		return triNull, nil
	}
	if negated {
		return triTrue, nil
	}
	return triFalse, nil
}

// evalInPredicate handles: expr [NOT] IN (val1, val2, ...) or expr [NOT] IN (subquery)
func evalInPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, in *antlrgen.InPredicateContext) (triBool, error) {
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() so unset optionals (SQL NULL) yield UNKNOWN.
		colName := functions.FullIdToName(colAtom.FullColumnName().FullId())
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found", colName)
		}
		if !msg.ProtoReflect().Has(fd) {
			return triNull, nil // NULL [NOT] IN (...) = UNKNOWN
		}
		fieldVal = functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd))
	} else {
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return triFalse, err
		}
		if v == nil {
			return triNull, nil // NULL [NOT] IN (...) = UNKNOWN
		}
		fieldVal = v
	}

	if qb := in.InList().QueryExpressionBody(); qb != nil {
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "subquery IN not supported in this context")
		}
		defer conn.pushOuterScope(outerScopeFromMsg(conn, msg))()
		subCols, subRows, err := conn.execQueryBodyRows(ctx, qb)
		if err != nil {
			return triFalse, err
		}
		// SQL standard: `x IN (SELECT a, b FROM t)` is a column-count
		// mismatch error (row constructor IN needs `(a, b) IN (...)`).
		// Previously matchSubqueryIN silently compared against column 0
		// only — wrong semantics.
		if len(subCols) != 1 {
			return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
				"subquery for IN must return exactly one column, got %d", len(subCols))
		}
		return matchSubqueryIN(fieldVal, subRows, in.NOT() != nil)
	}

	// The inList grammar rule admits three shapes:
	//   1. '(' (queryExpressionBody | expressions) ')' — subquery or
	//      parenthesized literal list
	//   2. preparedStatementParameter — `IN ?` / `IN :name`
	//   3. fullColumnName — `IN someCol`
	// Only shape 1 carries a non-nil Expressions() child. Shapes 2
	// and 3 hit this path with Expressions() == nil — reject cleanly
	// rather than crashing on AllExpression().
	exprsCtx := in.InList().Expressions()
	if exprsCtx == nil {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"IN requires a parenthesized expression list or subquery")
	}
	exprs := exprsCtx.AllExpression()
	var hadNullElement bool
	for _, expr := range exprs {
		// Java-aligned: IN list elements are arbitrary expressions, not
		// just constants. `b IN (1+0, 3+0, 5, 7)` is valid SQL that
		// Java's in-predicate.yamsql tests directly. Use evalExpr to
		// evaluate each element against the same proto message, allowing
		// arithmetic, function calls, even subqueries.
		litVal, err := evalExpr(ctx, conn, msg, expr)
		if err != nil {
			return triFalse, err
		}
		if litVal == nil {
			// NULL in the list can never match (x = NULL is UNKNOWN), but
			// contributes UNKNOWN to the expansion if nothing else matches.
			// SQL §8.4: `x IN (..., NULL)` = UNKNOWN, `x NOT IN (..., NULL)` = UNKNOWN.
			hadNullElement = true
			continue
		}
		// Java alignment: cross-type IN element errors 22000
		// (CANNOT_CONVERT_TYPE), matching the comparison-operator path.
		if !valuesComparable(fieldVal, litVal) {
			return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
				"cannot compare %T with %T in IN list", fieldVal, litVal)
		}
		if valuesEqual(fieldVal, litVal) {
			if in.NOT() != nil {
				return triFalse, nil
			}
			return triTrue, nil
		}
	}
	// No element matched. If any NULL literal was seen, the overall result
	// is UNKNOWN — the row filters out in WHERE but NOT of it stays UNKNOWN.
	if hadNullElement {
		return triNull, nil
	}
	if in.NOT() != nil {
		return triTrue, nil
	}
	return triFalse, nil
}

// evalIsNullPredicate handles: expr IS [NOT] NULL / IS TRUE / IS FALSE
func evalIsNullPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, is *antlrgen.IsExpressionContext) (bool, error) {
	// Evaluate the expression on the left side (may be a column, function call, etc.).
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() to distinguish NULL (unset optional) from zero.
		colName := functions.FullIdToName(colAtom.FullColumnName().FullId())
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return false, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found", colName)
		}
		if msg.ProtoReflect().Has(fd) {
			fieldVal = functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd))
		}
	} else {
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return false, err
		}
		fieldVal = v
	}
	negated := is.NOT() != nil

	switch {
	case is.NULL_LITERAL() != nil:
		isNull := fieldVal == nil
		if negated {
			return !isNull, nil
		}
		return isNull, nil
	case is.TRUE() != nil:
		b, ok := fieldVal.(bool)
		result := ok && b
		if negated {
			return !result, nil
		}
		return result, nil
	case is.FALSE() != nil:
		b, ok := fieldVal.(bool)
		result := ok && !b
		if negated {
			return !result, nil
		}
		return result, nil
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported IS test value")
	}
}

// evalLikePredicateTri handles: expr [NOT] LIKE 'pattern' [ESCAPE 'char'].
// Supports SQL wildcards: % (any sequence) and _ (any single character).
// If ESCAPE is given, the escape char preceding %, _, or itself makes the
// following char literal. Matches Java's ExpressionVisitor.visitLikePredicate
// behaviour (escape char must be exactly one char).
// Returns triNull when the expression is NULL so NOT LIKE NULL stays UNKNOWN.
func evalLikePredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, like *antlrgen.LikePredicateContext) (triBool, error) {
	rawVal, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
	if err != nil {
		return triFalse, err
	}
	if rawVal == nil {
		return triNull, nil // NULL [NOT] LIKE pattern = UNKNOWN
	}
	s, ok2 := rawVal.(string)
	if !ok2 {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "LIKE requires a string expression, got %T", rawVal)
	}

	// Pattern is the first STRING_LITERAL token; strip surrounding quotes.
	patternLit := like.GetPattern().GetText()
	pattern := functions.StripStringLiteralQuotes(patternLit)

	// Optional ESCAPE 'c' clause — Java asserts length==1 too.
	var escape rune = -1
	if esc := like.GetEscape(); esc != nil {
		escStr := functions.StripStringLiteralQuotes(esc.GetText())
		runes := []rune(escStr)
		if len(runes) != 1 {
			return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
				"LIKE ESCAPE must be exactly one character, got %q", escStr)
		}
		escape = runes[0]
	}

	matched := functions.LikeMatch(pattern, s, escape)
	if like.NOT() != nil {
		return triFromBool(!matched), nil
	}
	return triFromBool(matched), nil
}

// evalBetweenPredicateTri handles: expr [NOT] BETWEEN lo AND hi (inclusive).
//
// Java conformance: rather than collapsing any NULL to UNKNOWN, decompose
// per Java's ExpressionVisitor.visitBetweenComparisonPredicate:
//
//	x BETWEEN lo AND hi    →  (lo <= x) AND (x <= hi)
//	x NOT BETWEEN lo AND hi →  (x < lo)  OR  (x > hi)
//
// then let triAnd/triOr do Kleene short-circuit. This matters when one
// side is definitively FALSE (NOT BETWEEN) or TRUE (NOT BETWEEN with
// OR short-circuit) — e.g. `5 NOT BETWEEN 1 AND NULL` evaluates to
// `5 < 1 OR 5 > NULL` = `FALSE OR UNKNOWN` = UNKNOWN (previously correct),
// but `0 NOT BETWEEN 1 AND NULL` evaluates to `0 < 1 OR 0 > NULL` =
// `TRUE OR UNKNOWN` = TRUE (previously UNKNOWN, wrongly filtered out).
func evalBetweenPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, bet *antlrgen.BetweenComparisonPredicateContext) (triBool, error) {
	fieldVal, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
	if err != nil {
		return triFalse, err
	}
	lo, err := evalExprAtom(ctx, conn, msg, bet.GetLeft())
	if err != nil {
		return triFalse, err
	}
	hi, err := evalExprAtom(ctx, conn, msg, bet.GetRight())
	if err != nil {
		return triFalse, err
	}

	// Cross-type bounds are an error, same as plain comparison (Java's
	// between.yamsql pins XX000 for this; we use 22000 CANNOT_CONVERT_TYPE
	// matching the rest of our cross-type rejection surface).
	if fieldVal != nil && lo != nil && !valuesComparable(fieldVal, lo) {
		return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
			"BETWEEN bounds incompatible: cannot compare %T and %T", fieldVal, lo)
	}
	if fieldVal != nil && hi != nil && !valuesComparable(fieldVal, hi) {
		return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
			"BETWEEN bounds incompatible: cannot compare %T and %T", fieldVal, hi)
	}

	// compareTri returns TRUE/FALSE/NULL based on whether the comparison
	// can be determined; any NULL operand yields UNKNOWN.
	compareTri := func(a, b driver.Value, want func(int) bool) triBool {
		if a == nil || b == nil {
			return triNull
		}
		return triFromBool(want(functions.CompareValues(a, b)))
	}

	if bet.NOT() != nil {
		// (x < lo) OR (x > hi)
		lt := compareTri(fieldVal, lo, func(c int) bool { return c < 0 })
		gt := compareTri(fieldVal, hi, func(c int) bool { return c > 0 })
		return triOr(lt, gt), nil
	}
	// (lo <= x) AND (x <= hi)
	geLo := compareTri(fieldVal, lo, func(c int) bool { return c >= 0 })
	leHi := compareTri(fieldVal, hi, func(c int) bool { return c <= 0 })
	return triAnd(geLo, leHi), nil
}

// groupByKey builds a comparable string key from the group-by column values.
// Uses a type-tagged, length-prefixed encoding so that a NULL entry and the
// literal string "<nil>" produce different keys (fmt.Sprintf("%v", nil)
// would otherwise collide them), and so that values containing the
// separator byte cannot accidentally straddle adjacent columns. SQL groups
// NULLs together (NULL=NULL under GROUP BY), which is preserved because
// every NULL produces the same "N|" sentinel regardless of column type.
func groupByKey(groupVals []driver.Value) string {
	var b strings.Builder
	for _, v := range groupVals {
		if v == nil {
			b.WriteString("N|")
			continue
		}
		s := fmt.Sprintf("%T\x00%v", v, v)
		fmt.Fprintf(&b, "V:%d:%s|", len(s), s)
	}
	return b.String()
}

// evalHaving evaluates a HAVING clause expression against a map of
// output-column-name → aggregate value. Bool wrapper over evalHavingTri —
// UNKNOWN collapses to false at the filter boundary.
func evalHaving(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalHavingTri(ctx, conn, row, expr)
	return t.IsTrue(), err
}

// evalHavingTri is the Kleene three-valued implementation for HAVING.
// Supports comparisons, AND/OR/NOT, and aggregate function references.
func evalHavingTri(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (triBool, error) {
	// EXISTS subquery
	if exists, ok := expr.(*antlrgen.ExistsExpressionAtomContext); ok {
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		defer conn.pushOuterScope(outerScopeFromMapRow(row))()
		_, subRows, subErr := conn.execQueryBodyRows(ctx, exists.Query().QueryExpressionBody())
		if subErr != nil {
			return triFalse, subErr
		}
		return triFromBool(len(subRows) > 0), nil
	}
	// Handle logical expressions: AND / OR / XOR (+ symbolic forms).
	if le, ok := expr.(*antlrgen.LogicalExpressionContext); ok {
		left, err := evalHavingTri(ctx, conn, row, le.Expression(0))
		if err != nil {
			return triFalse, err
		}
		op := le.LogicalOperator()
		opText := strings.ReplaceAll(strings.ToUpper(op.GetText()), " ", "")
		isAnd := op.AND() != nil || opText == "&&"
		isOr := op.OR() != nil || opText == "||"
		isXor := op.XOR() != nil
		if isXor {
			right, err := evalHavingTri(ctx, conn, row, le.Expression(1))
			if err != nil {
				return triFalse, err
			}
			if left == triNull || right == triNull {
				return triNull, nil
			}
			return triFromBool((left == triTrue) != (right == triTrue)), nil
		}
		if isAnd {
			if left == triFalse {
				return triFalse, nil
			}
			right, err := evalHavingTri(ctx, conn, row, le.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		}
		if !isOr {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported logical operator %q", op.GetText())
		}
		// OR (including symbolic ||)
		if left == triTrue {
			return triTrue, nil
		}
		right, err := evalHavingTri(ctx, conn, row, le.Expression(1))
		if err != nil {
			return triFalse, err
		}
		return triOr(left, right), nil
	}
	// Handle NOT
	if ne, ok := expr.(*antlrgen.NotExpressionContext); ok {
		v, err := evalHavingTri(ctx, conn, row, ne.Expression())
		if err != nil {
			// On error the zero-value `v` is triFalse; v.Not() would return
			// triTrue and bury the error. Match evalExprPredicateTri NOT path.
			return triFalse, err
		}
		return v.Not(), nil
	}
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING expression %T", expr)
	}
	// WHERE-style predicate: expressionAtom + separate predicate (IS NULL, LIKE, BETWEEN, IN, =).
	if pred.Predicate() != nil {
		return evalPredicateOnMapTri(ctx, conn, row, pred)
	}
	// Parenthesised HAVING: `HAVING (SUM(v) > 20)` parses the atom as a
	// RecordConstructorExpressionAtom with one unnamed expression. Unwrap
	// it and recurse on the inner expression so the rest of the HAVING
	// evaluator (comparison + logical ops) applies uniformly.
	if rc, isRC := pred.ExpressionAtom().(*antlrgen.RecordConstructorExpressionAtomContext); isRC {
		rec := rc.RecordConstructor()
		if rec == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "empty record constructor in HAVING")
		}
		if rec.STAR() != nil || rec.OfTypeClause() != nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING does not support record constructor with STAR / OF TYPE")
		}
		fields := rec.AllExpressionWithOptionalName()
		if len(fields) == 1 && fields[0].AS() == nil && fields[0].Uid() == nil {
			return evalHavingTri(ctx, conn, row, fields[0].Expression())
		}
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING does not support multi-field / named record constructor")
	}
	// HAVING-style: the full comparison is the expression atom (BinaryComparisonPredicateContext).
	compPred, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING supports only comparison predicates, got %T", pred.ExpressionAtom())
	}

	var resolveAtom func(atom antlrgen.IExpressionAtomContext) (driver.Value, error)
	resolveAtom = func(atom antlrgen.IExpressionAtomContext) (driver.Value, error) {
		switch a := atom.(type) {
		case *antlrgen.ConstantExpressionAtomContext:
			return evalConstant(a.Constant())
		case *antlrgen.FullColumnNameExpressionAtomContext:
			name := functions.FullIdToName(a.FullColumnName().FullId())
			v, found := row[name]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "HAVING column %q not in SELECT list", name)
			}
			return v, nil
		case *antlrgen.FunctionCallExpressionAtomContext:
			// Aggregate function reference — match by reconstructed output name.
			agg, aggok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
			if !aggok {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING function call %T", a.FunctionCall())
			}
			awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
			if !awfok {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING aggregate %T", agg.AggregateWindowedFunction())
			}
			// Reuse extractAwfFields which already handles both plain and
			// DISTINCT forms (COUNT(*), COUNT(col), COUNT(DISTINCT col),
			// SUM/MIN/MAX/AVG with or without ALL/DISTINCT). This keeps
			// the HAVING lookup-name in sync with the SELECT-list alias
			// computed by extractAggFunc — so SELECT COUNT(DISTINCT v)
			// HAVING COUNT(DISTINCT v) > 0 finds the same aggregate.
			_, _, _, lookupName, _, fieldsOk := extractAwfFields(awf)
			if !fieldsOk {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported HAVING aggregate shape")
			}
			v, found := row[lookupName]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "HAVING aggregate %q not in SELECT list", lookupName)
			}
			return v, nil
		case *antlrgen.MathExpressionAtomContext:
			// HAVING on arithmetic over aggregates / constants, e.g.
			// `HAVING SUM(v) * 2 > 50` or `HAVING COUNT(*) + SUM(v) > 5`.
			// Recursively resolve both sides, then apply the same math
			// operator helper that the row-level evaluator uses — NULL
			// propagation comes from applyMathOp (nil-in / nil-out).
			left, lErr := resolveAtom(a.GetLeft())
			if lErr != nil {
				return nil, lErr
			}
			right, rErr := resolveAtom(a.GetRight())
			if rErr != nil {
				return nil, rErr
			}
			return functions.ApplyMathOp(left, right, a.MathOperator().GetText())
		case *antlrgen.BitExpressionAtomContext:
			// Same shape as MathExpression but with bitwise ops. HAVING on
			// bitwise expressions (`COUNT(*) & 1`) is unusual but valid and
			// costs nothing to mirror.
			left, lErr := resolveAtom(a.GetLeft())
			if lErr != nil {
				return nil, lErr
			}
			right, rErr := resolveAtom(a.GetRight())
			if rErr != nil {
				return nil, rErr
			}
			return functions.ApplyBitOp(left, right, a.BitOperator().GetText())
		case *antlrgen.SubqueryExpressionAtomContext:
			// HAVING `agg <op> (SELECT ... )` — uncorrelated subquery
			// pre-evaluated before the outer query started. Look up the
			// cached scalar.
			return evalScalarSubquery(ctx, conn, a.Query())
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING atom %T", atom)
		}
	}

	leftVal, err := resolveAtom(compPred.GetLeft())
	if err != nil {
		return triFalse, err
	}
	rightVal, err := resolveAtom(compPred.GetRight())
	if err != nil {
		return triFalse, err
	}
	opText := compPred.ComparisonOperator().GetText()
	// Null-safe equality (mirror of proto path) — branch before NULL→UNKNOWN.
	switch opText {
	case "ISDISTINCTFROM":
		return triFromBool(!nullSafeEqual(leftVal, rightVal)), nil
	case "ISNOTDISTINCTFROM":
		return triFromBool(nullSafeEqual(leftVal, rightVal)), nil
	}
	// SQL 3-valued logic: NULL comparison → UNKNOWN.
	if leftVal == nil || rightVal == nil {
		return triNull, nil
	}
	cmp := functions.CompareValues(leftVal, rightVal)
	switch opText {
	case "=":
		return triFromBool(cmp == 0), nil
	case "!=", "<>":
		return triFromBool(cmp != 0), nil
	case "<":
		return triFromBool(cmp < 0), nil
	case ">":
		return triFromBool(cmp > 0), nil
	case "<=":
		return triFromBool(cmp <= 0), nil
	case ">=":
		return triFromBool(cmp >= 0), nil
	}
	return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING: unsupported operator %q", opText)
}

// evalExprAtomOnMap resolves an expression atom using a map[string]driver.Value
// row (used for JOIN WHERE and ON condition evaluation).
func evalExprAtomOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, atom antlrgen.IExpressionAtomContext) (driver.Value, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		v, err := evalConstant(a.Constant())
		if err != nil {
			return nil, err
		}
		return v, nil
	case *antlrgen.FullColumnNameExpressionAtomContext:
		name := functions.FullIdToName(a.FullColumnName().FullId())
		v, found := row[name]
		if !found {
			// Try unqualified: "Order.amount" → "amount".
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				qual := name[:dot]
				qualUpper := strings.ToUpper(qual)
				// When a JOIN scope is active, reject a qualified
				// reference whose qualifier isn't a valid FROM source
				// alias — symmetric with the SELECT projection check.
				// Fires before the bare-column fallback so wrong
				// qualifiers error 42F01 instead of silently picking
				// whichever source populated the bare key.
				//
				// Correlated subquery exception: if the qualifier matches
				// an outer-scope alias, skip the reject and let the outer
				// fallback below resolve it.
				if conn != nil && conn.validQualifiers != nil && !conn.validQualifiers[qualUpper] {
					if !outerScopesContainQualifier(conn, qualUpper) {
						return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
							"column reference %q names unknown table/alias %q", name, qual)
					}
					// Outer qualifier: fall through to outer lookup.
				} else {
					v, found = row[name[dot+1:]]
				}
			}
		}
		if !found {
			// Correlated subquery fallback: walk outer-row stack.
			if conn != nil && len(conn.outerScopes) > 0 {
				ov, ofound, oerr := conn.resolveOuterColumn(name)
				if oerr != nil {
					return nil, oerr
				}
				if ofound {
					return ov, nil
				}
			}
			return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in row", name)
		}
		if m, isAmb := v.(ambiguousColumnMarker); isAmb {
			return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"column reference %q is ambiguous", m.Col)
		}
		return v, nil
	case *antlrgen.BinaryComparisonPredicateContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		opText := a.ComparisonOperator().GetText()
		// IS [NOT] DISTINCT FROM is null-safe equality — always 2-valued.
		// Must branch BEFORE the any-NULL → nil fallback below.
		switch opText {
		case "ISDISTINCTFROM":
			return !nullSafeEqual(left, right), nil
		case "ISNOTDISTINCTFROM":
			return nullSafeEqual(left, right), nil
		}
		// Java-aligned SQL 3-valued logic: NULL comparison → UNKNOWN
		// → nil at the value evaluator (NOT false; that collapsed
		// UNKNOWN to FALSE which is wrong for SELECT projection).
		if left == nil || right == nil {
			return nil, nil
		}
		if !valuesComparable(left, right) {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"cannot compare %T with %T", left, right)
		}
		cmp := functions.CompareValues(left, right)
		switch opText {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case ">":
			return cmp > 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">=":
			return cmp >= 0, nil
		}
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		return applyArithmeticOp(left, right, a.MathOperator().GetText())
	case *antlrgen.BitExpressionAtomContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		return functions.ApplyBitOp(left, right, a.BitOperator().GetText())
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Aggregate function calls inside a row-map expression evaluate
		// by looking up the reconstructed aggregate name in the row map.
		// This is how post-aggregation SELECT expressions like
		// `SUM(a) + SUM(b)` or `COALESCE(SUM(v), 0)` get their values:
		// the emit-time rowMap is populated with {"SUM(a)": n, "SUM(b)": m}
		// exactly as evalHavingTri's resolver expects.
		if agg, ok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext); ok {
			if awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext); awfok {
				if _, _, _, outName, _, ok := extractAwfFields(awf); ok {
					if v, present := row[outName]; present {
						return v, nil
					}
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"aggregate %q not available in this context", outName)
				}
			}
		}
		return evalScalarFunctionCallOnMap(ctx, conn, row, a.FunctionCall())
	case *antlrgen.RecordConstructorExpressionAtomContext:
		// Single-field parenthesised group — unwrap and recurse. For
		// boolean inners route through the tri-state predicate
		// evaluator so NULL comparisons encode as nil (UNKNOWN) rather
		// than collapsing to false — without this, JOIN `WHERE NOT (b
		// = NULL)` would return TRUE instead of UNKNOWN because
		// evalExprOnMap's fallback through evalExprAtomOnMap collapses
		// NULL-compared operands to false at the value-evaluator
		// boundary.
		rc := a.RecordConstructor()
		if rc == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "empty record constructor")
		}
		if rc.STAR() != nil || rc.OfTypeClause() != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "record constructor with STAR / OF TYPE not supported")
		}
		fields := rc.AllExpressionWithOptionalName()
		if len(fields) != 1 {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "multi-field record constructor not supported in this context")
		}
		f := fields[0]
		if f.AS() != nil || f.Uid() != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "named record field not supported in this context")
		}
		inner := f.Expression()
		if pred, ok := inner.(*antlrgen.PredicatedExpressionContext); ok {
			if pred.Predicate() != nil || looksBoolean(pred.ExpressionAtom()) {
				t, err := evalPredicateOnMapExprTri(ctx, conn, row, inner)
				if err != nil {
					return nil, err
				}
				switch t {
				case triTrue:
					return true, nil
				case triFalse:
					return false, nil
				default:
					return nil, nil
				}
			}
		}
		return evalExprOnMap(ctx, conn, row, inner)
	case *antlrgen.SubqueryExpressionAtomContext:
		return evalScalarSubquery(ctx, conn, a.Query())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom type %T in map eval", atom)
	}
}

// evalExprOnMap evaluates a scalar IExpressionContext against a map row, returning
// a driver.Value. Handles arithmetic, column refs, constants, and nested expressions.
func evalExprOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (driver.Value, error) {
	switch e := expr.(type) {
	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			t, err := evalPredicateOnMapTri(ctx, conn, row, e)
			if err != nil {
				return nil, err
			}
			if t == triNull {
				return nil, nil
			}
			return t == triTrue, nil
		}
		return evalExprAtomOnMap(ctx, conn, row, e.ExpressionAtom())
	case *antlrgen.LogicalExpressionContext:
		// Value-eval must preserve UNKNOWN as NULL, not collapse to
		// false. `SELECT b AND TRUE FROM x` for b=NULL should project
		// NULL, matching the proto-path fix at d0f2a3a1. Using the
		// 2-valued bool wrapper here dropped UNKNOWN → false and
		// diverged from Java.
		t, err := evalPredicateOnMapExprTri(ctx, conn, row, expr)
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return true, nil
		case triFalse:
			return false, nil
		default:
			return nil, nil
		}
	case *antlrgen.NotExpressionContext:
		// Kleene NOT: NOT TRUE = FALSE, NOT FALSE = TRUE, NOT NULL = NULL.
		t, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression())
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return false, nil
		case triFalse:
			return true, nil
		default:
			return nil, nil
		}
	case *antlrgen.ExistsExpressionAtomContext:
		ok, err := evalPredicateOnMapExpr(ctx, conn, row, expr)
		if err != nil {
			return nil, err
		}
		return ok, nil
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression type %T in map eval", expr)
	}
}

// evalPredicateOnMap evaluates a WHERE-style PredicatedExpressionContext against
// a map[string]driver.Value row. Handles IS NULL, LIKE, BETWEEN, IN, comparisons.
func evalPredicateOnMapTri(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, pred *antlrgen.PredicatedExpressionContext) (triBool, error) {
	fieldVal, err := evalExprAtomOnMap(ctx, conn, row, pred.ExpressionAtom())
	if err != nil {
		return triFalse, err
	}

	if pred.Predicate() == nil {
		// Leaf expression (e.g. a boolean constant) — treat NULL as UNKNOWN,
		// otherwise use truthiness.
		if fieldVal == nil {
			return triNull, nil
		}
		return triFromBool(functions.IsTruthy(fieldVal)), nil
	}

	switch p := pred.Predicate().(type) {
	case *antlrgen.IsExpressionContext:
		// IS NULL / IS TRUE / IS FALSE are always 2-state.
		negated := p.NOT() != nil
		isNull := fieldVal == nil
		switch {
		case p.NULL_LITERAL() != nil:
			res := isNull
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		case p.TRUE() != nil:
			b, _ := fieldVal.(bool)
			res := b
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		case p.FALSE() != nil:
			b, _ := fieldVal.(bool)
			res := !b && fieldVal != nil
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		}
		return triFalse, nil

	case *antlrgen.LikePredicateContext:
		if fieldVal == nil {
			return triNull, nil
		}
		s, ok := fieldVal.(string)
		if !ok {
			// Proto path errors on non-string LIKE; match that for consistency.
			return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
				"LIKE requires a string expression, got %T", fieldVal)
		}
		patternLit := p.GetPattern().GetText()
		var escape rune = -1
		if esc := p.GetEscape(); esc != nil {
			escStr := functions.StripStringLiteralQuotes(esc.GetText())
			runes := []rune(escStr)
			if len(runes) != 1 {
				return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
					"LIKE ESCAPE must be exactly one character, got %q", escStr)
			}
			escape = runes[0]
		}
		matched := functions.LikeMatch(functions.StripStringLiteralQuotes(patternLit), s, escape)
		if p.NOT() != nil {
			matched = !matched
		}
		return triFromBool(matched), nil

	case *antlrgen.BetweenComparisonPredicateContext:
		if fieldVal == nil {
			return triNull, nil
		}
		lo, loErr := evalExprAtomOnMap(ctx, conn, row, p.GetLeft())
		if loErr != nil {
			return triFalse, loErr
		}
		hi, hiErr := evalExprAtomOnMap(ctx, conn, row, p.GetRight())
		if hiErr != nil {
			return triFalse, hiErr
		}
		if lo == nil || hi == nil {
			return triNull, nil
		}
		result := functions.CompareValues(fieldVal, lo) >= 0 && functions.CompareValues(fieldVal, hi) <= 0
		if p.NOT() != nil {
			result = !result
		}
		return triFromBool(result), nil

	case *antlrgen.InPredicateContext:
		if fieldVal == nil {
			return triNull, nil // NULL [NOT] IN (...) = UNKNOWN
		}
		if qb := p.InList().QueryExpressionBody(); qb != nil {
			if conn == nil {
				return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "subquery IN not supported in this context")
			}
			defer conn.pushOuterScope(outerScopeFromMapRow(row))()
			_, subRows, subErr := conn.execQueryBodyRows(ctx, qb)
			if subErr != nil {
				return triFalse, subErr
			}
			return matchSubqueryIN(fieldVal, subRows, p.NOT() != nil)
		}
		// Same grammar-shape bail as evalInPredicateTri — `IN ?` /
		// `IN someCol` parse through the preparedStatementParameter /
		// fullColumnName alternatives, which don't carry an
		// ExpressionsContext. The previous silent-FALSE (and silent-
		// TRUE for NOT IN) behaviour was surprising; align with the
		// proto path and surface 0A000 for every non-parenthesized-
		// list, non-subquery IN.
		if p.InList().Expressions() == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"IN requires a parenthesized expression list or subquery")
		}
		var hadNullElement bool
		for _, inExpr := range p.InList().Expressions().AllExpression() {
			ep, ok := inExpr.(*antlrgen.PredicatedExpressionContext)
			if !ok {
				continue
			}
			litVal, litErr := evalExprAtomOnMap(ctx, conn, row, ep.ExpressionAtom())
			if litErr != nil {
				return triFalse, litErr
			}
			if litVal == nil {
				// See evalInPredicateTri: NULL list element contributes UNKNOWN.
				hadNullElement = true
				continue
			}
			if !valuesComparable(fieldVal, litVal) {
				return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
					"cannot compare %T with %T in IN list", fieldVal, litVal)
			}
			if valuesEqual(fieldVal, litVal) {
				if p.NOT() != nil {
					return triFalse, nil
				}
				return triTrue, nil
			}
		}
		if hadNullElement {
			return triNull, nil
		}
		if p.NOT() != nil {
			return triTrue, nil
		}
		return triFalse, nil
	}

	// Fallback: interpret as binary comparison (the predicate part has = / <> / < / > / <= / >=).
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if ok {
		rightVal, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetRight())
		if err != nil {
			return triFalse, err
		}
		if fieldVal == nil || rightVal == nil {
			return triNull, nil
		}
		cmp := functions.CompareValues(fieldVal, rightVal)
		switch bcp.ComparisonOperator().GetText() {
		case "=":
			return triFromBool(cmp == 0), nil
		case "!=", "<>":
			return triFromBool(cmp != 0), nil
		case "<":
			return triFromBool(cmp < 0), nil
		case ">":
			return triFromBool(cmp > 0), nil
		case "<=":
			return triFromBool(cmp <= 0), nil
		case ">=":
			return triFromBool(cmp >= 0), nil
		}
	}
	return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported predicate type %T in map eval", pred.Predicate())
}

// evalPredicateOnMapExpr is the bool wrapper used by WHERE/ON/HAVING filter
// sites. The Tri variant carries the UNKNOWN flag through AND/OR/NOT; here we
// collapse it to false at the filter boundary.
func evalPredicateOnMapExpr(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalPredicateOnMapExprTri(ctx, conn, row, expr)
	return t.IsTrue(), err
}

// evalPredicateOnMapExprTri mirrors evalExprPredicateTri but resolves column
// references from a map[string]driver.Value (used for JOIN/CTE/derived-table
// paths).
func evalPredicateOnMapExprTri(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (triBool, error) {
	switch e := expr.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		defer conn.pushOuterScope(outerScopeFromMapRow(row))()
		_, subRows, subErr := conn.execQueryBodyRows(ctx, e.Query().QueryExpressionBody())
		if subErr != nil {
			return triFalse, subErr
		}
		return triFromBool(len(subRows) > 0), nil
	case *antlrgen.LogicalExpressionContext:
		left, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(0))
		if err != nil {
			return triFalse, err
		}
		op := e.LogicalOperator()
		if op.AND() != nil {
			if left == triFalse {
				return triFalse, nil
			}
			right, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		}
		if left == triTrue {
			return triTrue, nil
		}
		right, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(1))
		if err != nil {
			return triFalse, err
		}
		return triOr(left, right), nil
	case *antlrgen.NotExpressionContext:
		v, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression())
		if err != nil {
			return triFalse, err
		}
		return v.Not(), nil
	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			return evalPredicateOnMapTri(ctx, conn, row, e)
		}
		// No separate predicate — expression atom (e.g. BinaryComparisonPredicateContext).
		bcp, ok := e.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
		if ok {
			left, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetLeft())
			if err != nil {
				return triFalse, err
			}
			right, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetRight())
			if err != nil {
				return triFalse, err
			}
			opText := bcp.ComparisonOperator().GetText()
			// IS [NOT] DISTINCT FROM is null-safe — always 2-valued;
			// branch before the any-NULL → UNKNOWN fallback.
			switch opText {
			case "ISDISTINCTFROM":
				return triFromBool(!nullSafeEqual(left, right)), nil
			case "ISNOTDISTINCTFROM":
				return triFromBool(nullSafeEqual(left, right)), nil
			}
			if left == nil || right == nil {
				return triNull, nil
			}
			cmp := functions.CompareValues(left, right)
			switch opText {
			case "=":
				return triFromBool(cmp == 0), nil
			case "!=", "<>":
				return triFromBool(cmp != 0), nil
			case "<":
				return triFromBool(cmp < 0), nil
			case ">":
				return triFromBool(cmp > 0), nil
			case "<=":
				return triFromBool(cmp <= 0), nil
			case ">=":
				return triFromBool(cmp >= 0), nil
			}
		}
		v, err := evalExprAtomOnMap(ctx, conn, row, e.ExpressionAtom())
		if err != nil {
			return triFalse, err
		}
		if v == nil {
			return triNull, nil
		}
		return triFromBool(functions.IsTruthy(v)), nil
	default:
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T in map eval", expr)
	}
}

// applyArithmeticOp is the map-path arithmetic entry. It delegates to the
// canonical `applyMathOp` so proto and map paths stay behaviourally identical
// (div/0 errors per SQL standard, int64 preservation, `%` support).
func applyArithmeticOp(left, right driver.Value, op string) (driver.Value, error) {
	return functions.ApplyMathOp(left, right, op)
}

// substituteParams replaces positional '?' placeholders in a query with
// SQL literal representations of the supplied driver values. Named params
// (@name) are not supported — only positional '?' is handled.
func substituteParams(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	var b strings.Builder
	argIdx := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		// Skip single-quoted string literals so a '?' inside a string value
		// is not treated as a placeholder.
		if ch == '\'' {
			b.WriteByte(ch)
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if c == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						// escaped quote inside string
						i++
						b.WriteByte(query[i])
					} else {
						break
					}
				}
				i++
			}
			continue
		}
		// Skip line comments `-- ...\n`. A '?' in a comment is literal.
		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			for i < len(query) && query[i] != '\n' {
				b.WriteByte(query[i])
				i++
			}
			if i < len(query) {
				b.WriteByte(query[i]) // write the trailing newline
			}
			continue
		}
		// Skip block comments `/* ... */`. A '?' in a comment is literal.
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			b.WriteByte(query[i])
			i++
			b.WriteByte(query[i])
			i++
			for i+1 < len(query) {
				if query[i] == '*' && query[i+1] == '/' {
					b.WriteByte(query[i])
					i++
					b.WriteByte(query[i])
					break
				}
				b.WriteByte(query[i])
				i++
			}
			continue
		}
		if ch != '?' {
			b.WriteByte(ch)
			continue
		}
		if argIdx >= len(args) {
			return "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"more '?' placeholders than bound parameters (placeholder %d, have %d args)",
				argIdx+1, len(args))
		}
		v := args[argIdx].Value
		argIdx++
		switch val := v.(type) {
		case nil:
			b.WriteString("NULL")
		case bool:
			if val {
				b.WriteString("TRUE")
			} else {
				b.WriteString("FALSE")
			}
		case int64:
			fmt.Fprintf(&b, "%d", val)
		case float64:
			fmt.Fprintf(&b, "%g", val)
		case string:
			// Escape single quotes by doubling them.
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(val, "'", "''"))
			b.WriteByte('\'')
		case []byte:
			// Represent as hex string literal using SQL standard X'...' is not
			// in our grammar — encode as quoted string for now.
			b.WriteByte('\'')
			for _, bv := range val {
				fmt.Fprintf(&b, "%02x", bv)
			}
			b.WriteByte('\'')
		default:
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported parameter type %T for placeholder %d", v, argIdx)
		}
	}
	if argIdx < len(args) {
		return "", api.NewErrorf(api.ErrCodeInvalidParameter,
			"fewer '?' placeholders than bound parameters (%d placeholders, %d args)",
			argIdx, len(args))
	}
	return b.String(), nil
}

// evalConstant evaluates a constant parse-tree node to a Go value.
// Returns nil for NULL.
func evalConstant(c antlrgen.IConstantContext) (any, error) {
	switch cv := c.(type) {
	case *antlrgen.DecimalConstantContext:
		text := cv.DecimalLiteral().GetText()
		if iv, err := strconv.ParseInt(text, 10, 64); err == nil {
			return iv, nil
		}
		fv, err := strconv.ParseFloat(text, 64)
		if err != nil {
			// strconv.ParseFloat returns (±Inf, ErrRange) on magnitude
			// overflow — treat as 22003 NUMERIC_VALUE_OUT_OF_RANGE. Any
			// other parse error is a malformed literal → 22023.
			if errors.Is(err, strconv.ErrRange) {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "decimal literal %q overflows float64", text)
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot parse decimal literal %q: %v", text, err)
		}
		return fv, nil
	case *antlrgen.NegativeDecimalConstantContext:
		text := "-" + cv.DecimalLiteral().GetText()
		if iv, err := strconv.ParseInt(text, 10, 64); err == nil {
			return iv, nil
		}
		fv, err := strconv.ParseFloat(text, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "decimal literal %q overflows float64", text)
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot parse decimal literal %q: %v", text, err)
		}
		return fv, nil
	case *antlrgen.StringConstantContext:
		raw := cv.StringLiteral().GetText()
		if len(raw) >= 2 {
			raw = raw[1 : len(raw)-1]
		}
		// Unescape doubled single-quotes produced by substituteParams or typed literally.
		raw = strings.ReplaceAll(raw, "''", "'")
		return raw, nil
	case *antlrgen.NullConstantContext:
		return nil, nil
	case *antlrgen.BooleanConstantContext:
		return cv.BooleanLiteral().TRUE() != nil, nil
	case *antlrgen.BytesConstantContext:
		// Grammar produces either HEXADECIMAL_LITERAL ('x' followed by
		// hex in single quotes) or BASE64_LITERAL ('b64' followed by
		// base64 in single quotes).
		bl := cv.BytesLiteral()
		if bl == nil {
			return nil, api.NewError(api.ErrCodeInvalidParameter, "empty bytes literal")
		}
		if hexLit := bl.HEXADECIMAL_LITERAL(); hexLit != nil {
			text := hexLit.GetText()
			// text looks like: x'deadbeef' or X'deadbeef'
			body := stripBytesWrapper(text, "x")
			// encoding/hex.DecodeString handles both odd-length and
			// non-hex-char failures uniformly.
			out, err := hex.DecodeString(body)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidBinaryRepresentation, "invalid hex literal %q: %v", text, err)
			}
			return out, nil
		}
		if b64 := bl.BASE64_LITERAL(); b64 != nil {
			text := b64.GetText()
			body := stripBytesWrapper(text, "b64")
			out, err := decodeBase64(body)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidBinaryRepresentation, "invalid base64 in %q: %v", text, err)
			}
			return out, nil
		}
		return nil, api.NewError(api.ErrCodeInvalidParameter, "bytes literal must be hex or base64")
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported constant type %T in WHERE", c)
	}
}

// stripBytesWrapper removes the `<prefix>'...'` wrapping from a bytes
// literal text token. Case-insensitive on the prefix to accept x / X
// and b64 / B64.
func stripBytesWrapper(text, prefix string) string {
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, prefix) {
		text = text[len(prefix):]
	}
	text = strings.TrimPrefix(text, "'")
	text = strings.TrimSuffix(text, "'")
	return text
}

// base64StdStrict is the standard Base64 encoding with strict
// padding (no line breaks, no URL-safe alternative). Mirrors what
// Java's Base64.getDecoder() accepts for the b64'...' literal form.
var base64StdStrict = base64.StdEncoding.Strict()

func decodeBase64(s string) ([]byte, error) {
	return base64StdStrict.DecodeString(s)
}

// valuesEqual compares two driver values that may have different numeric types.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Exact int64 comparison avoids float64 precision loss for large integers
	// (> 2^53 cannot be represented exactly as float64).
	if ai, ok1 := a.(int64); ok1 {
		if bi, ok2 := b.(int64); ok2 {
			return ai == bi
		}
	}
	// Normalise mixed int64/float64 pairs to float64.
	toFloat := func(v any) (float64, bool) {
		switch n := v.(type) {
		case int64:
			return float64(n), true
		case float64:
			return n, true
		}
		return 0, false
	}
	fa, aIsNum := toFloat(a)
	fb, bIsNum := toFloat(b)
	if aIsNum && bIsNum {
		return fa == fb
	}
	// One numeric and one non-numeric → not equal. SQL rejects cross-type
	// comparison (PostgreSQL errors; we return false to stay non-fatal).
	if aIsNum != bIsNum {
		return false
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	}
	// Last resort for exotic driver values: compare only if concrete types
	// match, avoid `'5' = 5` stringification bugs.
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// rowKey serializes a result row to a collision-free string key for DISTINCT deduplication.
// Each field is length-prefixed: "<type-tag>:<len>:<bytes>|" so that string values
// containing separator characters cannot collide with other fields or NULL markers.
func rowKey(row []driver.Value) string {
	var b strings.Builder
	for _, v := range row {
		if v == nil {
			b.WriteString("N:0:|")
			continue
		}
		s := fmt.Sprintf("%T\x00%v", v, v)
		fmt.Fprintf(&b, "V:%d:%s|", len(s), s)
	}
	return b.String()
}

func (c *EmbeddedConnection) execCreate(ctx context.Context, cs antlrgen.ICreateStatementContext) (int64, error) {
	switch t := cs.(type) {
	case *antlrgen.CreateDatabaseStatementContext:
		return c.execCreateDatabase(ctx, t)
	case *antlrgen.CreateSchemaStatementContext:
		return c.execCreateSchema(ctx, t)
	case *antlrgen.CreateSchemaTemplateStatementContext:
		return c.execCreateSchemaTemplate(ctx, t)
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported CREATE statement: %T", cs)
	}
}

func (c *EmbeddedConnection) execDrop(ctx context.Context, ds antlrgen.IDropStatementContext) (int64, error) {
	switch t := ds.(type) {
	case *antlrgen.DropDatabaseStatementContext:
		return c.execDropDatabase(ctx, t)
	case *antlrgen.DropSchemaStatementContext:
		return c.execDropSchema(ctx, t)
	case *antlrgen.DropSchemaTemplateStatementContext:
		return c.execDropSchemaTemplate(ctx, t)
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported DROP statement: %T", ds)
	}
}

func (c *EmbeddedConnection) execCreateDatabase(ctx context.Context, s *antlrgen.CreateDatabaseStatementContext) (int64, error) {
	dbPath := s.Path().GetText()
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	action := c.sess.Factory.CreateDatabase(dbPath, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropDatabase(ctx context.Context, s *antlrgen.DropDatabaseStatementContext) (int64, error) {
	dbPath := s.Path().GetText()
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	throwIfNotExist := s.IfExists() == nil
	action := c.sess.Factory.DropDatabase(dbPath, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchema(ctx context.Context, s *antlrgen.CreateSchemaStatementContext) (int64, error) {
	schemaText := s.SchemaId().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.sess.DBPath)
	if err != nil {
		return 0, err
	}
	templateID := s.SchemaTemplateId().GetText()
	action := c.sess.Factory.CreateSchema(dbPath, schemaName, templateID, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropSchema(ctx context.Context, s *antlrgen.DropSchemaStatementContext) (int64, error) {
	schemaText := s.Uid().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.sess.DBPath)
	if err != nil {
		return 0, err
	}
	if dbPath == "" {
		return 0, api.NewErrorf(api.ErrCodeUnknownDatabase,
			"invalid database identifier in %q", schemaText)
	}
	action := c.sess.Factory.DropSchema(dbPath, schemaName, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	c.invalidateSchemaCache(dbPath, schemaName)
	return 0, nil
}

func (c *EmbeddedConnection) execDropSchemaTemplate(ctx context.Context, s *antlrgen.DropSchemaTemplateStatementContext) (int64, error) {
	templateID := s.Uid().GetText()
	throwIfNotExist := s.IfExists() == nil
	action := c.sess.Factory.DropSchemaTemplate(templateID, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchemaTemplate(ctx context.Context, s *antlrgen.CreateSchemaTemplateStatementContext) (int64, error) {
	templateID := s.SchemaTemplateId().GetText()
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)

	// First pass: register tables (indexes reference them by name).
	for _, clause := range s.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		tableName := td.Uid().GetText()
		cols, pkCols, err := parseTableDefinition(td)
		if err != nil {
			return 0, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q: %v", tableName, err)
		}
		b.AddTable(tableName, cols, pkCols)
	}

	// Second pass: register indexes.
	for _, clause := range s.AllTemplateClause() {
		idxDef := clause.IndexDefinition()
		if idxDef == nil {
			continue
		}
		if err := parseIndexDefinition(idxDef, b); err != nil {
			return 0, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "index: %v", err)
		}
	}

	tmpl, err := b.Build()
	if err != nil {
		return 0, err
	}
	action := c.sess.Factory.SaveSchemaTemplate(tmpl, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	// Template change may affect any schema using it — flush the whole cache.
	c.sess.ResetSchemaCache()
	return 0, nil
}

// parseIndexDefinition handles a single CREATE INDEX clause within a schema template.
// Only INDEX ON SOURCE form (INDEX name ON table (cols)) is supported.
func parseIndexDefinition(idxDef antlrgen.IIndexDefinitionContext, b *metadata.Builder) error {
	switch def := idxDef.(type) {
	case *antlrgen.IndexOnSourceDefinitionContext:
		indexName := def.GetIndexName().GetText()
		tableName := def.GetSource().GetText()
		unique := def.UNIQUE() != nil
		var cols []string
		if cl := def.IndexColumnList(); cl != nil {
			for _, spec := range cl.AllIndexColumnSpec() {
				cols = append(cols, spec.GetColumnName().GetText())
			}
		}
		if len(cols) == 0 {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"index %q has no columns", indexName)
		}
		b.AddIndex(tableName, indexName, cols, unique)
		return nil
	default:
		return api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported index definition type %T; only INDEX … ON … is supported", idxDef)
	}
}

// parseTableDefinition extracts column specs and primary key column
// names from a TableDefinitionContext.
func parseTableDefinition(td antlrgen.ITableDefinitionContext) ([]metadata.ColumnSpec, []string, error) {
	var cols []metadata.ColumnSpec
	var pkCols []string

	for i, colDef := range td.AllColumnDefinition() {
		colName := colDef.Uid().GetText()
		ct := colDef.ColumnType()
		if ct == nil {
			return nil, nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"column %q has no type", colName)
		}
		nullable := true
		if cc := colDef.ColumnConstraint(); cc != nil {
			if nc, ok := cc.(*antlrgen.NullColumnConstraintContext); ok {
				if nn := nc.NullNotnull(); nn != nil && nn.NOT() != nil {
					nullable = false
				}
			}
		}
		dt, err := parseColumnType(ct, nullable)
		if err != nil {
			return nil, nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"column %q", colName)
		}
		cols = append(cols, metadata.NewColumnSpec(colName, dt, int32(i+1)))
	}

	if pkDef := td.PrimaryKeyDefinition(); pkDef != nil {
		for _, fullID := range pkDef.FullIdList().AllFullId() {
			pkCols = append(pkCols, fullID.GetText())
		}
	}

	return cols, pkCols, nil
}

// parseColumnType maps a ColumnTypeContext to an api.DataType.
func parseColumnType(ct antlrgen.IColumnTypeContext, nullable bool) (api.DataType, error) {
	pt := ct.PrimitiveType()
	if pt == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only primitive column types are supported")
	}
	switch {
	case pt.BOOLEAN() != nil:
		return api.NewBooleanType(nullable), nil
	case pt.INTEGER() != nil:
		return api.NewIntegerType(nullable), nil
	case pt.BIGINT() != nil:
		return api.NewLongType(nullable), nil
	case pt.FLOAT() != nil:
		return api.NewFloatType(nullable), nil
	case pt.DOUBLE() != nil:
		return api.NewDoubleType(nullable), nil
	case pt.STRING() != nil:
		return api.NewStringType(nullable), nil
	case pt.BYTES() != nil:
		return api.NewBytesType(nullable), nil
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported column type: %s", ct.GetText())
	}
}

// ensureCatalogInit bootstraps the catalog. Retries on transient failure
// (unlike sync.Once, a mutex+bool allows retry when the previous attempt failed).
func (c *EmbeddedConnection) ensureCatalogInit(ctx context.Context) error {
	c.sess.CatalogMu.Lock()
	defer c.sess.CatalogMu.Unlock()
	if c.sess.CatalogReady {
		return nil
	}
	_, err := c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		if initErr := c.sess.Catalog.Initialize(txn); initErr != nil {
			return nil, initErr
		}
		return nil, txn.Commit()
	})
	if err != nil {
		return err
	}
	c.sess.CatalogReady = true
	return nil
}

// Ping implements driver.Pinger. Bootstraps the catalog on first call.
func (c *EmbeddedConnection) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	return c.ensureCatalogInit(ctx)
}

// runDDL bootstraps the catalog on first call, then executes action.
func (c *EmbeddedConnection) runDDL(ctx context.Context, action apiddl.ConstantAction) error {
	if err := c.ensureCatalogInit(ctx); err != nil {
		return err
	}
	_, err := c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		execErr := action.Execute(txn)
		if execErr != nil {
			return nil, execErr
		}
		return nil, txn.Commit()
	})
	return err
}

// parseSchemaIdentifier splits "/dbpath/schemaname" into its parts.
// If the identifier has no leading slash, the current dbPath is used.
// Mirrors Java's SemanticAnalyzer.parseSchemaIdentifier.
func parseSchemaIdentifier(id, currentDB string) (dbPath, schemaName string, err error) {
	if strings.HasPrefix(id, "/") {
		idx := strings.LastIndex(id, "/")
		if idx == len(id)-1 {
			return "", "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"schema identifier %q must not end with /", id)
		}
		if idx == 0 {
			return "", "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"schema identifier %q must include both database and schema segments", id)
		}
		return id[:idx], id[idx+1:], nil
	}
	return currentDB, id, nil
}

// validateDatabasePath checks that the path starts with / and has a non-empty name.
func validateDatabasePath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) < 2 || strings.HasSuffix(p, "/") {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"database path must be /name (not empty, bare /, or trailing /): %q", p)
	}
	return nil
}

// embeddedStmt is a prepared DDL statement (no bind parameters).
type embeddedStmt struct {
	conn  *EmbeddedConnection
	query string
}

func (s *embeddedStmt) Close() error { return nil }

func (s *embeddedStmt) NumInput() int { return -1 } // unknown, variadic-safe

func (s *embeddedStmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.conn.ExecContext(context.Background(), s.query, named)
}

func (s *embeddedStmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	// TODO: driver.Stmt.Query has no context parameter; use context.Background() until
	// database/sql upgrades all call sites to QueryContext.
	return s.conn.QueryContext(context.Background(), s.query, named)
}

// Static interface checks.
var (
	_ driver.Conn               = (*EmbeddedConnection)(nil)
	_ driver.ExecerContext      = (*EmbeddedConnection)(nil)
	_ driver.QueryerContext     = (*EmbeddedConnection)(nil)
	_ driver.Pinger             = (*EmbeddedConnection)(nil)
	_ driver.ConnBeginTx        = (*EmbeddedConnection)(nil)
	_ driver.SessionResetter    = (*EmbeddedConnection)(nil)
	_ driver.Validator          = (*EmbeddedConnection)(nil)
	_ driver.ConnPrepareContext = (*EmbeddedConnection)(nil)
)
