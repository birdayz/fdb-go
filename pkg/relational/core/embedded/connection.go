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
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	fdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

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

	// currentSourceAliases holds the uppercased set of qualifier aliases
	// for the outer proto-path scan currently executing. Consumed by the
	// proto-path evaluator (eval_proto.go) so a correlated inner reference
	// resolves against the outer's user alias. nil falls back to the proto
	// descriptor name.
	currentSourceAliases map[string]bool

	// planCache caches Cascades physical plans keyed by normalized SQL
	// hash. Per-connection (and therefore per-schema), invalidated on
	// DDL. Lazily initialized on first query.
	planCache *PlanCache

	// planLogger receives one PlanGenerationInfo per Plan() call for
	// operational debuggability (RFC-034). nil = silent (the default).
	// The Go analog of Java's RelationalLoggingUtil; sampling and
	// log-level policy live in the handler, not the engine.
	planLogger PlanGenerationLogger

	// slowQueryThresholdMicros marks a planning call as slow when its
	// duration exceeds this many microseconds. Defaults to the canonical
	// api.OptLogSlowQueryThresholdMicros value (see New).
	slowQueryThresholdMicros int64

	// options carries the per-connection api.Options that drive
	// per-statement resource governance (RFC-106a): the scan-limit options
	// (OptExecutionScannedRowsLimit / OptExecutionScannedBytesLimit /
	// OptExecutionTimeLimit, all per-page) and OptMaxRows (statement-wide
	// returned-row cap). nil means "use option defaults" (effectively
	// unlimited — see api.DefaultOptionValues). Mirrors Java's
	// EmbeddedRelationalConnection.getOptions().
	options *api.Options

	// failOnScanLimitReached, when true, makes a leaf cursor that hits its
	// scanned-records / scanned-bytes limit return a ScanLimitReachedError
	// (SQLSTATE 54F01) instead of paginating across pages. Default false
	// (paginate; unchanged). Mirrors Java's
	// ExecuteProperties.setFailOnScanLimitReached(true). Go-local config
	// (there is no api.OptionName for it in Java's enum).
	failOnScanLimitReached bool

	// statementTimeout is a wall-clock deadline spanning the WHOLE
	// statement (all pages of one Execute). 0 = off. Go-only read-path
	// extension stored as a connection-local config rather than an
	// api.OptionName (api.OptionName mirrors Java's enum exactly; Java has
	// no such option). PER-REQUEST: one Execute is bounded; a continuation
	// resumed by a NEW request starts fresh (see cascadesPlan.Execute).
	statementTimeout time.Duration

	// maxResultBytes caps the cumulative tuple-encoded size of returned
	// rows across one statement (all pages of one Execute). 0 = off.
	// Go-only read-path extension (a non-exact egress ceiling — the
	// estimate is the cheap encoded length, not exact heap).
	maxResultBytes int64
}

// Options returns the connection's api.Options, or api.NoOptions() when
// none have been set. Used by the execution path to read the per-page
// scan-limit options and the statement-wide MAX_ROWS cap (RFC-106a).
func (c *EmbeddedConnection) Options() *api.Options {
	if c.options == nil {
		return api.NoOptions()
	}
	return c.options
}

// SetOptions installs the per-connection api.Options (RFC-106a scan-limit
// + MAX_ROWS wiring). Passing nil resets to defaults. Not safe to call
// concurrently with query execution on the same connection (matches
// database/sql's per-Conn threading contract).
func (c *EmbeddedConnection) SetOptions(o *api.Options) {
	c.options = o
}

// SetFailOnScanLimitReached toggles the Java setFailOnScanLimitReached(true)
// behavior (RFC-106a): when true a leaf cursor hitting a scan/byte limit
// errors (54F01) instead of paginating. Default false.
func (c *EmbeddedConnection) SetFailOnScanLimitReached(v bool) {
	c.failOnScanLimitReached = v
}

// SetStatementTimeout sets the per-Execute wall-clock deadline (RFC-106a).
// A non-positive duration disables it. PER-REQUEST semantics: it bounds a
// single Execute (all its pages); a continuation resumed by a new request
// starts a fresh deadline. There is intentionally no `SET statement_timeout
// = …` SQL path — the parser grammar has no generic SET <var> = <val> rule
// (only SET TRANSACTION), so a grammar change would be required; this
// connection-field setter is the Go-local config instead (RFC-106a §3).
func (c *EmbeddedConnection) SetStatementTimeout(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.statementTimeout = d
}

// SetMaxResultBytes sets the statement-wide returned-row byte cap
// (RFC-106a §5). A non-positive value disables it. The accounted size is
// the cheap tuple-encoded length of each returned row, not exact heap — a
// non-exact egress ceiling.
func (c *EmbeddedConnection) SetMaxResultBytes(n int64) {
	if n < 0 {
		n = 0
	}
	c.maxResultBytes = n
}

// SetPlanLogger installs a planning-metrics logger (RFC-034). Passing nil
// disables planning logging. Not safe to call concurrently with query
// planning on the same connection (matches database/sql's per-Conn threading
// contract).
func (c *EmbeddedConnection) SetPlanLogger(l PlanGenerationLogger) {
	c.planLogger = l
}

// SetSlowQueryThresholdMicros sets the slow-query threshold in microseconds.
// A non-positive value disables the slow-query flag.
func (c *EmbeddedConnection) SetSlowQueryThresholdMicros(micros int64) {
	c.slowQueryThresholdMicros = micros
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

// invalidatePlanCache clears all cached query plans. Called after any
// DDL statement that may change table/index metadata.
func (c *EmbeddedConnection) invalidatePlanCache() {
	if c.planCache != nil {
		c.planCache.Invalidate()
	}
}

// cachedMetaData returns the RecordMetaData for the connection's
// current (DBPath, Schema) if the session schema cache already
// holds it, or nil otherwise. Read-only and synchronous — no
// transaction, no IO. Used by ExplainFn paths that opportunistically
// upgrade text-only logical plans to predicate-tree form when
// metadata is cheap; cold-cache lookups produce nil rather than
// blocking on a catalog fetch, so Explain stays fast and side-
// effect-free.
//
// Returns nil when: cache miss, schema template isn't a
// RecordLayerSchemaTemplate, or the underlying RecordMetaData is
// itself nil. Callers fall back to the text builder on nil.
func (c *EmbeddedConnection) cachedMetaData() *recordlayer.RecordMetaData {
	if c.sess == nil {
		return nil
	}
	key := session.SchemaCacheKey(c.sess.DBPath, c.sess.Schema)
	schema, ok := c.sess.SchemaCache[key]
	if !ok || schema == nil {
		return nil
	}
	tmpl, ok := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
	if !ok {
		return nil
	}
	return tmpl.Underlying()
}

// ensureMetaData loads the schema into the cache if not already present,
// so that cachedMetaData returns non-nil. Runs a read-only FDB transaction
// to fetch the catalog entry. Called by the Cascades generator before
// planning so the planner has access to table/index metadata.
func (c *EmbeddedConnection) ensureMetaData(ctx context.Context) error {
	if c.sess == nil || c.sess.Schema == "" {
		return nil
	}
	if c.cachedMetaData() != nil {
		return nil
	}
	_, err := c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		_, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		return nil, loadErr
	})
	return err
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
		sess:                     sess,
		slowQueryThresholdMicros: defaultSlowQueryThresholdMicros(),
	}
}

// defaultSlowQueryThresholdMicros reads the canonical slow-query threshold
// default from the options package so there is a single source of truth for
// the value (api.OptLogSlowQueryThresholdMicros). Falls back to 0 (disabled)
// if the option is absent or not an int64.
func defaultSlowQueryThresholdMicros() int64 {
	if v, ok := api.DefaultOptionValues()[api.OptLogSlowQueryThresholdMicros].(int64); ok {
		return v
	}
	return 0
}

// seriousLog reports an unexpected, must-not-be-silent event (a recovered panic) at
// ERROR level through log/slog, passing structured attributes (the panic value, the
// stack) rather than a pre-formatted string so a JSON/structured handler gets
// queryable fields. Routing through slog.Default() makes the library's diagnostics
// pluggable via the standard Go mechanism: applications call slog.SetDefault with
// their own handler (JSON, level routing, shipping to a collector) and these events
// flow there with no record-layer-specific API to learn. It stays a var so tests can
// capture it; a recovered panic is always a bug and must never be swallowed silently.
var seriousLog = func(msg string, attrs ...any) {
	slog.Default().Error(msg, attrs...)
}

// recoveredPanicError converts a panic that escaped statement planning/execution
// into a returned error. This is the Go "don't leak panics" API boundary (the
// encoding/json model): genuine internal-invariant asserts may panic freely below
// here, but at this boundary they become a generic internal error so a single
// statement cannot crash a shared, multi-tenant process. The panic value and the
// stack are logged SERIOUS (debug.Stack here still shows the original panic site —
// the stack is live until recover completes the unwind); the caller gets a generic
// message because the panic value may carry schema/row data. It deliberately does
// NOT re-panic by type — see TODO-production.md P0.3.
func recoveredPanicError(r any) error {
	seriousLog("recovered panic in statement execution",
		"panic", r, "stack", string(debug.Stack()))
	return api.NewError(api.ErrCodeInternalError, "internal error")
}

// ExecContext executes SQL (DDL/DML/transaction) and returns the row-
// count result. Routes through cascadesGenerator in exec mode, which
// dispatches DML/DDL/transaction through execStatement and returns a
// Plan whose Execute aggregates RowsAffected across a multi-statement
// batch.
func (c *EmbeddedConnection) ExecContext(ctx context.Context, sql string, args []driver.NamedValue) (res driver.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, recoveredPanicError(r)
		}
	}()
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	defer c.beginStatement()()

	substituted, err := substituteParams(sql, args)
	if err != nil {
		return nil, err
	}

	gen := newCascadesGenerator(c)
	plan, err := gen.Plan(ctx, substituted)
	if err != nil {
		return nil, translateFDBError(err)
	}
	if !plan.IsUpdate() {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"unsupported statement type; supported: DDL, INSERT, UPDATE, DELETE")
	}
	result, err := plan.Execute(ctx)
	if err != nil {
		return nil, translateFDBError(err)
	}
	return driver.RowsAffected(result.RowsAffected), nil
}

// QueryContext handles read-only queries (SELECT / SHOW). Routes
// through the query.Generator seam. Rejects multi-statement batches
// and non-row-returning Plans — behaviour matches the pre-seam
// QueryContext.
func (c *EmbeddedConnection) QueryContext(ctx context.Context, sql string, args []driver.NamedValue) (rows driver.Rows, err error) {
	defer func() {
		if r := recover(); r != nil {
			rows, err = nil, recoveredPanicError(r)
		}
	}()
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	defer c.beginStatement()()
	substituted, err := substituteParams(sql, args)
	if err != nil {
		return nil, err
	}
	if cerr := c.ensureCatalogInit(ctx); cerr != nil {
		return nil, cerr
	}
	gen := newCascadesGenerator(c)
	plan, err := gen.Plan(ctx, substituted)
	if err != nil {
		return nil, translateFDBError(err)
	}
	// QueryContext expects a Rows-returning plan. A multi-statement
	// batch (MultiPlan) is always an update plan under today's
	// semantics; reject with the same message the pre-seam code used.
	if _, isMulti := plan.(*query.MultiPlan); isMulti {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "multi-statement queries are not supported")
	}
	// QueryContext returns rows; a DML (update) plan returns an affected-row
	// count, so it belongs on Exec, not Query. This is statement-layer
	// method routing (the analog of JDBC executeQuery rejecting an update
	// plan), not a Cascades limitation — DML plans plan and execute fine via
	// ExecContext. We reject before executing (no surprise mutation), a
	// deliberate divergence from Java's execute-then-throw (see DIVERGENCES).
	if plan.IsUpdate() {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"INSERT/UPDATE/DELETE return a row count, not rows — use Exec, not Query")
	}
	result, err := plan.Execute(ctx)
	if err != nil {
		return nil, translateFDBError(err)
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

func (c *EmbeddedConnection) newStoreBuilder() *recordlayer.StoreBuilder {
	return recordlayer.NewStoreBuilder().SetDatabase(c.sess.DB)
}

// Close marks the connection as closed and cancels any open FDB transaction.
func (c *EmbeddedConnection) Close() error {
	c.closed.Store(true)
	if c.activeTx != nil {
		tx := c.activeTx
		c.activeTx = nil
		tx.rctx.Cancel()
	}
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
	// CreateWritableTransaction (not CreateTransaction) so explicit SQL transactions
	// (database/sql BeginTx → COMMIT) work on ANY backend, including the libfdb_c
	// escape hatch. BeginTx spans multiple driver calls, so it needs a long-lived
	// handle and cannot use the closure-based Run gold path; the backend-agnostic
	// interface form is what keeps it from being silently pure-Go-only.
	fdbTx, err := c.sess.DB.CreateWritableTransaction()
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
	c.currentSourceAliases = nil
	c.sess.ResetSchemaCache()
	c.invalidatePlanCache()
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

// PlanExplain runs the SQL through the Cascades planner and returns
// the physical plan's Explain string without executing the query.
// Useful for testing plan structure (e.g. verifying sort elimination).
func (c *EmbeddedConnection) PlanExplain(ctx context.Context, sql string) (string, error) {
	if err := c.ensureCatalogInit(ctx); err != nil {
		return "", err
	}
	gen := newCascadesGenerator(c)
	plan, err := gen.Plan(ctx, sql)
	if err != nil {
		return "", err
	}
	return plan.Explain(), nil
}

// execStatement routes a single parsed statement to the right handler.
func (c *EmbeddedConnection) execStatement(ctx context.Context, stmt antlrgen.IStatementContext) (int64, error) {
	if ddl := stmt.DdlStatement(); ddl != nil {
		create := ddl.CreateStatement()
		drop := ddl.DropStatement()
		var n int64
		var err error
		switch {
		case create != nil:
			n, err = c.execCreate(ctx, create)
		case drop != nil:
			n, err = c.execDrop(ctx, drop)
		default:
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported DDL statement")
		}
		if err == nil {
			// DDL changes schema metadata — invalidate cached query plans
			// so subsequent queries are re-planned against the new schema.
			c.invalidatePlanCache()
		}
		return n, err
	}
	// DML (INSERT/UPDATE/DELETE) no longer routes here — it executes through
	// the Cascades path (planDML). execStatement now handles only DDL and
	// transaction statements.
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

// CheckNamedValue implements driver.NamedValueChecker. Converts custom
// Go types to driver-compatible values before they reach substituteParams.
// Accepts: uuid.UUID → string (canonical 36-char form).
// All standard types (int64, float64, string, bool, []byte, time.Time)
// pass through unchanged.
func (c *EmbeddedConnection) CheckNamedValue(nv *driver.NamedValue) error {
	if nv.Value == nil {
		return nil
	}
	switch v := nv.Value.(type) {
	case int64, float64, string, bool, []byte, time.Time:
		return nil
	default:
		if s, ok := v.(fmt.Stringer); ok {
			nv.Value = s.String()
			return nil
		}
		return driver.ErrSkip
	}
}

// translateFDBCode maps an FDB numeric error code to a SQLSTATE-wrapped error.
// Returns the original error unchanged if the code is not recognized.
func translateFDBCode(code int, err error) error {
	switch code {
	case 1031: // transaction_timed_out
		return api.WrapError(api.ErrCodeTransactionTimeout, "FDB transaction timed out", err)
	case 1020: // not_committed
		return api.WrapError(api.ErrCodeSerializationFailure, "FDB transaction conflict", err)
	case 1007: // transaction_too_old
		return api.WrapError(api.ErrCodeSerializationFailure, "FDB transaction too old", err)
	case 2017: // used_during_commit
		return api.WrapError(api.ErrCodeTransactionInactive, "FDB transaction used during commit", err)
	}
	return err
}

// translateFDBError maps known FDB wire errors to SQLSTATE error codes.
// Mirrors Java's ExceptionUtil.translateException for FDB-specific errors.
func translateFDBError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return err
	}
	var metaErr *recordlayer.MetaDataError
	if errors.As(err, &metaErr) {
		return api.WrapError(api.ErrCodeSyntaxOrAccessViolation, metaErr.Error(), err)
	}
	var existsErr *recordlayer.RecordAlreadyExistsError
	if errors.As(err, &existsErr) {
		return api.WrapError(api.ErrCodeUniqueConstraintViolation, existsErr.Error(), err)
	}
	// Secondary UNIQUE index violation (distinct from a duplicate primary
	// key) must also surface SQLSTATE 23505 — the deleted naive path mapped
	// this; the Cascades executor returns the raw record-layer error.
	var uniqErr *recordlayer.RecordIndexUniquenessViolationError
	if errors.As(err, &uniqErr) {
		return api.WrapErrorf(err, api.ErrCodeUniqueConstraintViolation,
			"unique index %q violated: value %v already exists", uniqErr.IndexName, uniqErr.IndexKey)
	}
	// Execution-time "no record at this key" — most commonly UPDATE of a PK
	// column, whose save targets the new (nonexistent) key. This is Java's
	// exact path and code: Java has no plan-time PK guard; the in-place save
	// throws RecordDoesNotExistException, which ExceptionUtil does NOT map,
	// so it surfaces as ErrorCode.UNKNOWN. (The deleted Go naive path's
	// plan-time ErrCodeUnsupportedOperation reject was a Go-only divergence —
	// do not reintroduce it.)
	var notExistErr *recordlayer.RecordDoesNotExistError
	if errors.As(err, &notExistErr) {
		return api.WrapError(api.ErrCodeUnknown, "record does not exist", err)
	}
	var deserErr *recordlayer.RecordDeserializationError
	if errors.As(err, &deserErr) {
		return api.WrapError(api.ErrCodeDeserializationFailure, deserErr.Error(), err)
	}
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) {
		return translateFDBCode(fdbErr.Code, err)
	}
	var fdbValErr fdb.Error
	if errors.As(err, &fdbValErr) {
		return translateFDBCode(fdbValErr.Code, err)
	}
	// Fallback: string matching for wrapped errors that lost the typed FDBError.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "transaction_timed_out"):
		return api.WrapError(api.ErrCodeTransactionTimeout, "FDB transaction timed out", err)
	case strings.Contains(msg, "not_committed"):
		return api.WrapError(api.ErrCodeSerializationFailure, "FDB transaction conflict", err)
	case strings.Contains(msg, "transaction_too_old"):
		return api.WrapError(api.ErrCodeSerializationFailure, "FDB transaction too old", err)
	case strings.Contains(msg, "used_during_commit"):
		return api.WrapError(api.ErrCodeTransactionInactive, "FDB transaction used during commit", err)
	}
	return err
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
	_ driver.NamedValueChecker  = (*EmbeddedConnection)(nil)
	_ driver.Tx                 = (*embeddedTx)(nil)
)
