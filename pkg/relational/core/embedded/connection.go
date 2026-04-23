// Package embedded implements the embedded (in-process) SQL execution engine
// for the FoundationDB relational layer.
//
// EmbeddedConnection is the Go equivalent of Java's EmbeddedRelationalConnection.
// It parses SQL, routes DDL statements through the MetadataOperationsFactory,
// and (eventually) routes DML through the query planner.
package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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
