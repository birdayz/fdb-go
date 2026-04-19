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
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

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
	dbPath        string // current database URI (e.g. "/mydb")
	schema        string // current schema name (set via USE SCHEMA / SetSchema)
	defaultSchema string // schema set at connection creation time; restored on ResetSession
	fdbDB         *recordlayer.FDBDatabase
	cat           *catalog.RecordLayerStoreCatalog
	ks            *keyspace.RelationalKeyspace
	factory       apiddl.MetadataOperationsFactory
	closed        atomic.Bool

	// activeTx is non-nil when an explicit transaction is open (BeginTx called).
	// nil means auto-commit mode.
	activeTx *embeddedTx

	// schemaCache is a connection-level cache: (dbPath+"\x00"+schemaName) → api.Schema.
	// Safe because driver connections are single-goroutine. Invalidated by DDL.
	schemaCache map[string]api.Schema

	// catalogReady is set to true after the first successful catalog init.
	// Protected by catalogMu so transient failures can be retried.
	catalogMu    sync.Mutex
	catalogReady bool
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
	tx.conn.activeTx = nil
	return tx.rctx.CommitWithHooks()
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
	return c.fdbDB.Run(ctx, fn)
}

func (c *EmbeddedConnection) schemaCacheKey(dbPath, schemaName string) string {
	return dbPath + "\x00" + schemaName
}

// cachedLoadSchema returns the api.Schema for (dbPath, schemaName), using the
// connection-level cache to avoid repeated FDB reads within the same session.
// The cache is invalidated by any DDL that modifies schema definitions.
func (c *EmbeddedConnection) cachedLoadSchema(txn api.Transaction, dbPath, schemaName string) (api.Schema, error) {
	key := c.schemaCacheKey(dbPath, schemaName)
	if s, ok := c.schemaCache[key]; ok {
		return s, nil
	}
	s, err := c.cat.LoadSchema(txn, dbPath, schemaName)
	if err != nil {
		return nil, err
	}
	c.schemaCache[key] = s
	return s, nil
}

func (c *EmbeddedConnection) invalidateSchemaCache(dbPath, schemaName string) {
	delete(c.schemaCache, c.schemaCacheKey(dbPath, schemaName))
}

// New returns a ready-to-use embedded connection.
func New(
	dbPath string,
	fdbDB *recordlayer.FDBDatabase,
	cat *catalog.RecordLayerStoreCatalog,
	factory apiddl.MetadataOperationsFactory,
	ks *keyspace.RelationalKeyspace,
) *EmbeddedConnection {
	return &EmbeddedConnection{
		dbPath:      dbPath,
		fdbDB:       fdbDB,
		cat:         cat,
		factory:     factory,
		ks:          ks,
		schemaCache: make(map[string]api.Schema),
	}
}

// ExecContext executes SQL (DDL only in phase 1) and returns the result.
// Implements driver.ExecerContext so database/sql skips the Prepare round-trip.
func (c *EmbeddedConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	var err error
	if query, err = substituteParams(query, args); err != nil {
		return nil, err
	}
	root, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return driver.RowsAffected(0), nil
	}
	var totalRows int64
	for _, stmt := range stmts.AllStatement() {
		rows, execErr := c.execStatement(ctx, stmt)
		if execErr != nil {
			return nil, execErr
		}
		totalRows += rows
	}
	return driver.RowsAffected(totalRows), nil
}

// QueryContext handles read-only queries. Supports SHOW statements and
// SELECT * FROM <table>; all other queries return ErrCodeUnsupportedOperation.
func (c *EmbeddedConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	var subErr error
	if query, subErr = substituteParams(query, args); subErr != nil {
		return nil, subErr
	}
	if err := c.ensureCatalogInit(ctx); err != nil {
		return nil, err
	}
	root, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return emptyRows{}, nil
	}
	if len(stmts.AllStatement()) > 1 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "multi-statement queries are not supported")
	}
	stmt := stmts.AllStatement()[0]

	// Route SELECT statements.
	if sel := stmt.SelectStatement(); sel != nil {
		return c.execSelect(ctx, sel)
	}

	admin := stmt.AdministrationStatement()
	if admin == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "only SHOW and SELECT statements are supported")
	}
	show := admin.ShowStatement()
	if show == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "only SHOW statements are supported")
	}
	return c.execShowStatement(ctx, show)
}

// execSelect executes a SELECT [cols | *] FROM <tableName> [WHERE col = val]
// [ORDER BY col [ASC|DESC]] [LIMIT n].
// Only single-table scans with simple equality WHERE are supported.
// Joins, subqueries, GROUP BY, HAVING, etc. are not.
func (c *EmbeddedConnection) execSelect(ctx context.Context, sel antlrgen.ISelectStatementContext) (driver.Rows, error) {
	sq, err := extractSelectParts(sel)
	if err != nil {
		return nil, err
	}

	// Route INFORMATION_SCHEMA.* queries to system table handlers.
	// System table queries do not require a schema to be set.
	upper := strings.ToUpper(sq.tableName)
	if strings.HasPrefix(upper, "INFORMATION_SCHEMA.") {
		sysTable := upper[len("INFORMATION_SCHEMA."):]
		return c.execSystemTable(ctx, sysTable, sq.whereExpr)
	}

	if c.schema == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	type row = []driver.Value
	type outField struct {
		name string
		fd   protoreflect.FieldDescriptor
	}
	var cols []string
	var data []row
	var extraSortFields []outField

	_, runErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry so duplicate rows aren't appended
		cols = nil
		extraSortFields = nil
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(sq.tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", sq.tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
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

		cursor := store.ScanRecordsByType(sq.tableName, nil, recordlayer.ForwardScan())
		defer cursor.Close() //nolint:errcheck

		if sq.countStar {
			cols = []string{"COUNT(*)"}
			var count int64
			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				match, matchErr := evalPredicate(result.GetValue().Record, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if match {
					count++
				}
			}
			data = []row{{count}}
			return nil, nil
		}

		// Resolve output fields: either the explicit projection or all fields.
		allFields := msgDesc.Fields()
		var outFields []outField
		// extraSortFields (outer variable) are ORDER BY columns not in the projection.
		if sq.projCols == nil {
			// SELECT * — all fields in descriptor order.
			outFields = make([]outField, allFields.Len())
			for i := 0; i < allFields.Len(); i++ {
				fd := allFields.Get(i)
				outFields[i] = outField{string(fd.Name()), fd}
			}
		} else {
			// Named projection — look up each column, apply alias if present.
			outFields = make([]outField, len(sq.projCols))
			projByCol := make(map[string]bool, len(sq.projCols))
			for i, colName := range sq.projCols {
				fd := allFields.ByName(protoreflect.Name(colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"column %q not found in table %q", colName, sq.tableName)
				}
				outName := colName
				if i < len(sq.projAliases) && sq.projAliases[i] != "" {
					outName = sq.projAliases[i]
				}
				outFields[i] = outField{outName, fd}
				projByCol[colName] = true
			}
			// Add any ORDER BY columns not already in the projection.
			for _, ob := range sq.orderBy {
				if projByCol[ob.colName] {
					continue
				}
				fd := allFields.ByName(protoreflect.Name(ob.colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"ORDER BY column %q not found in table %q", ob.colName, sq.tableName)
				}
				extraSortFields = append(extraSortFields, outField{ob.colName, fd})
				projByCol[ob.colName] = true // avoid duplicates
			}
		}
		// fullFields = projected + extra sort columns; output strips extra at end.
		fullFields := append(outFields, extraSortFields...) //nolint:gocritic
		cols = make([]string, len(outFields))
		for i, f := range outFields {
			cols[i] = f.name
		}

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			msg := rec.Record
			match, matchErr := evalPredicate(msg, sq.whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			vals := make([]driver.Value, len(fullFields))
			for i, f := range fullFields {
				if msg.ProtoReflect().Has(f.fd) {
					vals[i] = protoValueToDriver(f.fd, msg.ProtoReflect().Get(f.fd))
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

	// Apply DISTINCT deduplication before sort.
	if sq.distinct && !sq.countStar {
		seen := make(map[string]struct{}, len(data))
		deduped := data[:0]
		for _, row := range data {
			key := rowKey(row)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				deduped = append(deduped, row)
			}
		}
		data = deduped
	}

	// Apply ORDER BY (post-scan in-memory sort).
	if len(sq.orderBy) > 0 {
		// Build a map from column name to row index (covers projected + extra sort cols).
		colIdx := make(map[string]int, len(cols)+len(extraSortFields))
		for i, c := range cols {
			colIdx[c] = i
		}
		for i, f := range extraSortFields {
			colIdx[f.name] = len(cols) + i
		}
		sort.SliceStable(data, func(i, j int) bool {
			for _, ob := range sq.orderBy {
				idx, ok := colIdx[ob.colName]
				if !ok {
					// Column validated during scan setup; safe to skip.
					continue
				}
				a, b := data[i][idx], data[j][idx]
				cmp := compareValues(a, b)
				if cmp != 0 {
					if ob.ascending {
						return cmp < 0
					}
					return cmp > 0
				}
			}
			return false
		})
	}

	// Strip extra sort columns that were not in the SELECT list.
	if len(extraSortFields) > 0 {
		projLen := len(cols)
		for i, row := range data {
			data[i] = row[:projLen]
		}
	}

	// Apply LIMIT.
	if sq.limit >= 0 && int64(len(data)) > sq.limit {
		data = data[:sq.limit]
	}

	return &staticRows{cols: cols, rows: data}, nil
}

// execSystemTable dispatches INFORMATION_SCHEMA.* queries.
func (c *EmbeddedConnection) execSystemTable(ctx context.Context, name string, whereExpr antlrgen.IWhereExprContext) (driver.Rows, error) {
	switch name {
	case "SCHEMATA":
		return c.execSysSchemata(ctx, whereExpr)
	case "TABLES":
		return c.execSysTables(ctx, whereExpr)
	case "COLUMNS":
		return c.execSysColumns(ctx, whereExpr)
	case "INDEXES":
		return c.execSysIndexes(ctx, whereExpr)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unknown INFORMATION_SCHEMA table: %q", name)
	}
}

// execSysSchemata implements SELECT * FROM INFORMATION_SCHEMA.SCHEMATA.
func (c *EmbeddedConnection) execSysSchemata(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	if where != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "WHERE filtering is not yet supported for INFORMATION_SCHEMA tables")
	}
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			data = append(data, row{dbID, schemaName, "", "", ""})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	cols := []string{"CATALOG_NAME", "SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME", "SQL_PATH"}
	return &staticRows{cols: cols, rows: data}, nil
}

// execSysTables implements SELECT * FROM INFORMATION_SCHEMA.TABLES.
// WHERE filtering is not yet supported for system tables.
func (c *EmbeddedConnection) execSysTables(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	if where != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "WHERE filtering is not yet supported for INFORMATION_SCHEMA tables")
	}
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		// Snapshot (db, schema) pairs first; LoadSchema below opens another txn context.
		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				data = append(data, row{ref.db, ref.schema, tbl.MetadataName(), "TABLE", "", "", "", "", "", ""})
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE",
		"REMARKS", "TYPE_CAT", "TYPE_SCHEM", "TYPE_NAME",
		"SELF_REFERENCING_COL_NAME", "REF_GENERATION",
	}
	return &staticRows{cols: cols, rows: data}, nil
}

// execSysColumns implements SELECT * FROM INFORMATION_SCHEMA.COLUMNS.
// WHERE filtering is not yet supported for system tables.
func (c *EmbeddedConnection) execSysColumns(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	if where != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "WHERE filtering is not yet supported for INFORMATION_SCHEMA tables")
	}
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				cols := tbl.Columns()
				for i, col := range cols {
					nullable := "NO"
					if col.DataType().IsNullable() {
						nullable = "YES"
					}
					data = append(data, row{
						ref.db,
						ref.schema,
						tbl.MetadataName(),
						col.MetadataName(),
						int64(i + 1),
						"",
						nullable,
						col.DataType().Code().String(),
						"",
						"",
						"",
					})
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
		"CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE",
	}
	return &staticRows{cols: cols, rows: data}, nil
}

// execSysIndexes implements SELECT * FROM INFORMATION_SCHEMA.INDEXES.
// Returns one row per index across all (database, schema, table) tuples.
func (c *EmbeddedConnection) execSysIndexes(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	if where != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "WHERE filtering is not yet supported for INFORMATION_SCHEMA tables")
	}
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				for _, idx := range tbl.Indexes() {
					isUnique := "NO"
					if idx.IsUnique() {
						isUnique = "YES"
					}
					isSparse := "NO"
					if idx.IsSparse() {
						isSparse = "YES"
					}
					data = append(data, row{
						ref.db,
						ref.schema,
						tbl.MetadataName(),
						idx.MetadataName(),
						idx.IndexType(),
						isUnique,
						isSparse,
					})
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME",
		"INDEX_NAME", "INDEX_TYPE", "IS_UNIQUE", "IS_SPARSE",
	}
	return &staticRows{cols: cols, rows: data}, nil
}

// selectQuery holds the parsed components of a SELECT statement.
type selectQuery struct {
	tableName   string
	projCols    []string // nil = SELECT *; ignored when countStar is true
	projAliases []string // parallel to projCols; empty string = no alias (use column name)
	countStar   bool     // true when SELECT list is exactly COUNT(*)
	distinct    bool     // true when SELECT DISTINCT
	whereExpr   antlrgen.IWhereExprContext
	// orderBy holds column-name + ascending pairs (nil = no ORDER BY).
	orderBy []orderByClause
	// limit < 0 means no limit.
	limit int64
}

type orderByClause struct {
	colName   string
	ascending bool
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

// columnNameFromExpr extracts a plain column name from an IExpressionContext.
// context is used in error messages (e.g. "SELECT expression", "ORDER BY expression").
func columnNameFromExpr(expr antlrgen.IExpressionContext, context string) (string, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s must be a column name, got %T", context, expr)
	}
	colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s must be a column name, got expression atom %T", context, pred.ExpressionAtom())
	}
	return fullIdToName(colAtom.FullColumnName().FullId()), nil
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
		alias = stripIdentifierQuotes(e.Uid().GetText())
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
	simpleTable, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported query term %T; only simple SELECT FROM <table> is supported",
			body.QueryTerm())
	}

	// Parse SELECT list: either *, a list of column name expressions, or COUNT(*).
	selElems := simpleTable.SelectElements()
	var projCols []string    // nil = SELECT *
	var projAliases []string // parallel to projCols
	var countStar bool
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
			case *antlrgen.SelectExpressionElementContext:
				if checkCountStar(e) {
					if len(elems) > 1 {
						return nil, api.NewError(api.ErrCodeUnsupportedOperation,
							"cannot mix COUNT(*) with other columns in SELECT list")
					}
					countStar = true
				} else {
					colName, alias, nameErr := selectExprToColumnName(e)
					if nameErr != nil {
						return nil, nameErr
					}
					projCols = append(projCols, colName)
					projAliases = append(projAliases, alias)
				}
			default:
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported SELECT element type %T", elem)
			}
		}
	}

	fromClause := simpleTable.FromClause()
	if fromClause == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "SELECT without FROM is not supported")
	}

	sources := fromClause.TableSources()
	if sources == nil || len(sources.AllTableSource()) != 1 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only single-table SELECT is supported; joins are not yet implemented")
	}
	srcBase, ok := sources.AllTableSource()[0].(*antlrgen.TableSourceBaseContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported table source %T", sources.AllTableSource()[0])
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
		parts[i] = stripIdentifierQuotes(u.GetText())
	}
	sq := &selectQuery{
		tableName:   strings.Join(parts, "."),
		projCols:    projCols,
		projAliases: projAliases,
		countStar:   countStar,
		distinct:    simpleTable.DISTINCT() != nil,
		whereExpr:   fromClause.WhereExpr(),
		limit:       -1,
	}

	// Parse ORDER BY clause.
	orderByClauseCtx := simpleTable.OrderByClause()
	if orderByClauseCtx != nil {
		for _, obExpr := range orderByClauseCtx.AllOrderByExpression() {
			colName, nameErr := columnNameFromExpr(obExpr.Expression(), "ORDER BY expression")
			if nameErr != nil {
				return nil, nameErr
			}
			ascending := true
			if oc := obExpr.OrderClause(); oc != nil && oc.DESC() != nil {
				ascending = false
			}
			sq.orderBy = append(sq.orderBy, orderByClause{colName, ascending})
		}
	}

	// Parse LIMIT clause.
	limitClauseCtx := simpleTable.LimitClause()
	if limitClauseCtx != nil {
		atoms := limitClauseCtx.AllLimitClauseAtom()
		if len(atoms) > 1 {
			// MySQL allows "LIMIT offset, count" — reject for simplicity.
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"LIMIT offset,count syntax is not supported; use LIMIT count")
		}
		if len(atoms) == 1 && atoms[0].DecimalLiteral() != nil {
			n, parseErr := strconv.ParseInt(atoms[0].DecimalLiteral().GetText(), 10, 64)
			if parseErr != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
					"invalid LIMIT value %q: %v", atoms[0].DecimalLiteral().GetText(), parseErr)
			}
			sq.limit = n
		}
	}

	return sq, nil
}

// stripIdentifierQuotes removes surrounding double-quotes or backticks from a
// SQL identifier produced by the ANTLR parser (e.g. `"FOO"` → `FOO`).
func stripIdentifierQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '`' && s[len(s)-1] == '`')) {
		return s[1 : len(s)-1]
	}
	return s
}

// fullIdToName converts a FullId parse-tree node to a dot-separated,
// quote-stripped name. Used for table names in INSERT, UPDATE, DELETE.
func fullIdToName(fid antlrgen.IFullIdContext) string {
	uids := fid.AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = stripIdentifierQuotes(u.GetText())
	}
	return strings.Join(parts, ".")
}

// protoValueToDriver converts a protoreflect.Value to a driver.Value.
// For proto2 optional fields that are not set, returns nil (SQL NULL).
func protoValueToDriver(fd protoreflect.FieldDescriptor, v protoreflect.Value) driver.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint()) //nolint:gosec
	case protoreflect.FloatKind:
		return float64(v.Float())
	case protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return []byte(v.Bytes())
	default:
		return v.Interface()
	}
}

// execShowStatement routes SHOW … to the appropriate catalog reader.
func (c *EmbeddedConnection) execShowStatement(ctx context.Context, show antlrgen.IShowStatementContext) (driver.Rows, error) {
	switch show.(type) {
	case *antlrgen.ShowDatabasesStatementContext:
		return c.execShowDatabases(ctx)
	case *antlrgen.ShowSchemaTemplatesStatementContext:
		return c.execShowSchemaTemplates(ctx)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported SHOW statement: %s", show.GetText())
	}
}

func (c *EmbeddedConnection) execShowDatabases(ctx context.Context) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListDatabases(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			id, strErr := rs.StringByName("DATABASE_ID")
			if strErr != nil {
				return nil, strErr
			}
			data = append(data, row{id})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: []string{"DATABASE_ID"}, rows: data}, nil
}

func (c *EmbeddedConnection) execShowSchemaTemplates(ctx context.Context) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.SchemaTemplateCatalog().ListTemplates(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			name, strErr := rs.StringByName("TEMPLATE_NAME")
			if strErr != nil {
				return nil, strErr
			}
			ver, intErr := rs.LongByName("TEMPLATE_VERSION")
			if intErr != nil {
				return nil, intErr
			}
			data = append(data, row{name, ver})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: []string{"TEMPLATE_NAME", "TEMPLATE_VERSION"}, rows: data}, nil
}

// staticRows is a driver.Rows backed by a pre-materialised slice.
type staticRows struct {
	cols    []string
	rows    [][]driver.Value
	current int
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
	fdbTx, err := c.fdbDB.CreateTransaction()
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
	c.defaultSchema = s
	c.schema = s
}

// ResetSession implements driver.SessionResetter. Resets per-request
// state (schema) so pooled connections start clean.
func (c *EmbeddedConnection) ResetSession(_ context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	c.schema = c.defaultSchema
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
func (c *EmbeddedConnection) SetSchema(s string) { c.schema = s }

// GetSchema returns the current schema label.
func (c *EmbeddedConnection) GetSchema() string { return c.schema }

// GetDBPath returns the current database path.
func (c *EmbeddedConnection) GetDBPath() string { return c.dbPath }

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
		fdbTx, err := c.fdbDB.CreateTransaction()
		if err != nil {
			return 0, err
		}
		fdbTx.Options().SetReadSystemKeys()
		rctx := recordlayer.NewFDBRecordContext(fdbTx)
		c.activeTx = &embeddedTx{conn: c, rctx: rctx}
		return 0, nil
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported transaction statement: %s", txn.GetText())
	}
}

// execInsert executes INSERT INTO table (col1, col2, ...) VALUES (...), (...).
func (c *EmbeddedConnection) execInsert(ctx context.Context, ins antlrgen.IInsertStatementContext) (int64, error) {
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Explicit column list (optional).
	colCtx := ins.UidListWithNestingsInParens()
	var explicitCols []string // nil = no column list (use schema order)
	if colCtx != nil {
		for _, uw := range colCtx.UidListWithNestings().AllUidWithNestings() {
			explicitCols = append(explicitCols, stripIdentifierQuotes(uw.Uid().GetText()))
		}
	}

	// Only handle VALUES path.
	valCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueValuesContext)
	if !ok {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "only INSERT ... VALUES (...) is supported")
	}

	tableName := fullIdToName(ins.TableName().FullId())

	var totalRows int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0 // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
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
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
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
			if len(exprs) != len(cols) {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
					"column count %d does not match value count %d", len(cols), len(exprs))
			}
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found in table %q", col, tableName)
				}
				val, evalErr := evalLiteralExpr(exprs[i])
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					// NULL — leave field absent (proto2 optional semantics).
					continue
				}
				protoVal, convErr := convertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			if _, saveErr := store.SaveRecord(msg); saveErr != nil {
				return nil, saveErr
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

// convertToProtoValue converts a Go value (int64, float64, string, bool) to
// a protoreflect.Value matching the field descriptor's kind.
func convertToProtoValue(fd protoreflect.FieldDescriptor, val any) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		switch v := val.(type) {
		case bool:
			return protoreflect.ValueOfBool(v), nil
		case int64:
			return protoreflect.ValueOfBool(v != 0), nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfInt32(int32(v)), nil //nolint:gosec
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfInt64(v), nil
		}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfUint32(uint32(v)), nil //nolint:gosec
		}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfUint64(uint64(v)), nil //nolint:gosec
		}
	case protoreflect.FloatKind:
		switch v := val.(type) {
		case float64:
			return protoreflect.ValueOfFloat32(float32(v)), nil //nolint:gosec
		case int64:
			return protoreflect.ValueOfFloat32(float32(v)), nil //nolint:gosec
		}
	case protoreflect.DoubleKind:
		switch v := val.(type) {
		case float64:
			return protoreflect.ValueOfFloat64(v), nil
		case int64:
			return protoreflect.ValueOfFloat64(float64(v)), nil
		}
	case protoreflect.StringKind:
		if v, ok := val.(string); ok {
			return protoreflect.ValueOfString(v), nil
		}
	case protoreflect.BytesKind:
		if v, ok := val.(string); ok {
			return protoreflect.ValueOfBytes([]byte(v)), nil
		}
	}
	return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInvalidParameter,
		"cannot convert %T to proto field kind %s", val, fd.Kind())
}

// evalExpr evaluates a SET expression from an UPDATE statement against the current row msg.
// Supports: literals, column references, and binary arithmetic (+, -, *, /).
func evalExpr(msg proto.Message, expr antlrgen.IExpressionContext) (any, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression type %T in SET", expr)
	}
	return evalExprAtom(msg, pred.ExpressionAtom())
}

func evalExprAtom(msg proto.Message, atom antlrgen.IExpressionAtomContext) (any, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		return evalConstant(a.Constant())
	case *antlrgen.FullColumnNameExpressionAtomContext:
		colName := fullIdToName(a.FullColumnName().FullId())
		if msg == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "column reference %q not allowed in this context", colName)
		}
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
		}
		return protoValueToDriver(fd, msg.ProtoReflect().Get(fd)), nil
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtom(msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		return applyMathOp(left, right, a.MathOperator().GetText())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom %T in SET", atom)
	}
}

func applyMathOp(left, right any, op string) (any, error) {
	lf, lok := toFloat64(left)
	rf, rok := toFloat64(right)
	if !lok || !rok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"arithmetic operator %q requires numeric operands, got %T and %T", op, left, right)
	}
	var result float64
	switch op {
	case "+":
		result = lf + rf
	case "-":
		result = lf - rf
	case "*":
		result = lf * rf
	case "/":
		if rf == 0 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "division by zero")
		}
		result = lf / rf
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported math operator %q", op)
	}
	// Preserve int64 if both operands were integers and result is whole.
	_, leftIsInt := left.(int64)
	_, rightIsInt := right.(int64)
	if leftIsInt && rightIsInt && result == float64(int64(result)) {
		return int64(result), nil
	}
	return result, nil
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// execUpdate executes UPDATE <table> SET col = val [, ...] [WHERE col = val].
func (c *EmbeddedConnection) execUpdate(ctx context.Context, upd antlrgen.IUpdateStatementContext) (int64, error) {
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	tableName := fullIdToName(upd.TableName().FullId())
	whereExpr := upd.WhereExpr()
	updatedElems := upd.AllUpdatedElement()

	var updated int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		updated = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
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
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
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

		cursor := store.ScanRecordsByType(tableName, nil, recordlayer.ForwardScan())
		defer cursor.Close() //nolint:errcheck

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}

			cloned := proto.Clone(rec.Record)
			clonedRefl := cloned.ProtoReflect()
			for _, elem := range updatedElems {
				colName := fullIdToName(elem.FullColumnName().FullId())
				fd := msgDesc.Fields().ByName(protoreflect.Name(colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found in table %q", colName, tableName)
				}
				val, evalErr := evalExpr(cloned, elem.Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					clonedRefl.Clear(fd)
					continue
				}
				protoVal, convErr := convertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				clonedRefl.Set(fd, protoVal)
			}
			if _, saveErr := store.SaveRecord(cloned); saveErr != nil {
				return nil, saveErr
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
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	tableName := fullIdToName(del.TableName().FullId())
	whereExpr := del.WhereExpr()

	var deleted int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		deleted = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
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
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
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

		cursor := store.ScanRecordsByType(tableName, nil, recordlayer.ForwardScan())
		defer cursor.Close() //nolint:errcheck

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(rec.Record, whereExpr)
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
func evalPredicate(msg proto.Message, whereExpr antlrgen.IWhereExprContext) (bool, error) {
	if whereExpr == nil {
		return true, nil
	}
	return evalExprPredicate(msg, whereExpr.Expression())
}

// evalExprPredicate evaluates an IExpressionContext as a boolean predicate.
// Supports: col = constant, col != constant, col < constant, col > constant,
// col <= constant, col >= constant, AND, OR, NOT.
func evalExprPredicate(msg proto.Message, expr antlrgen.IExpressionContext) (bool, error) {
	switch e := expr.(type) {
	case *antlrgen.LogicalExpressionContext:
		left, err := evalExprPredicate(msg, e.Expression(0))
		if err != nil {
			return false, err
		}
		op := e.LogicalOperator()
		if op.AND() != nil {
			if !left {
				return false, nil // short-circuit
			}
			return evalExprPredicate(msg, e.Expression(1))
		}
		if op.OR() != nil {
			if left {
				return true, nil // short-circuit
			}
			return evalExprPredicate(msg, e.Expression(1))
		}
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported logical operator %q", op.GetText())

	case *antlrgen.NotExpressionContext:
		v, err := evalExprPredicate(msg, e.Expression())
		if err != nil {
			return false, err
		}
		return !v, nil

	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			switch p := e.Predicate().(type) {
			case *antlrgen.InPredicateContext:
				return evalInPredicate(msg, e, p)
			case *antlrgen.IsExpressionContext:
				return evalIsNullPredicate(msg, e, p)
			case *antlrgen.LikePredicateContext:
				return evalLikePredicate(msg, e, p)
			case *antlrgen.BetweenComparisonPredicateContext:
				return evalBetweenPredicate(msg, e, p)
			}
		}
		return evalComparisonPredicate(msg, e)

	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T", expr)
	}
}

// evalComparisonPredicate handles a leaf comparison: col <op> constant.
func evalComparisonPredicate(msg proto.Message, pred *antlrgen.PredicatedExpressionContext) (bool, error) {
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE predicate type %T", pred.ExpressionAtom())
	}
	opText := bcp.ComparisonOperator().GetText()

	var colName string
	var valAtom *antlrgen.ConstantExpressionAtomContext

	leftCol, leftIsCol := bcp.GetLeft().(*antlrgen.FullColumnNameExpressionAtomContext)
	rightConst, rightIsConst := bcp.GetRight().(*antlrgen.ConstantExpressionAtomContext)
	rightCol, rightIsCol := bcp.GetRight().(*antlrgen.FullColumnNameExpressionAtomContext)
	leftConst, leftIsConst := bcp.GetLeft().(*antlrgen.ConstantExpressionAtomContext)

	switch {
	case leftIsCol && rightIsConst:
		colName = fullIdToName(leftCol.FullColumnName().FullId())
		valAtom = rightConst
	case leftIsConst && rightIsCol:
		// Flip: constant <op> column → column <flipped-op> constant
		colName = fullIdToName(rightCol.FullColumnName().FullId())
		valAtom = leftConst
		switch opText {
		case "<":
			opText = ">"
		case ">":
			opText = "<"
		case "<=":
			opText = ">="
		case ">=":
			opText = "<="
		}
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"WHERE comparison must be col <op> constant or constant <op> col, got %T %s %T",
			bcp.GetLeft(), opText, bcp.GetRight())
	}

	litVal, err := evalConstant(valAtom.Constant())
	if err != nil {
		return false, err
	}

	fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
	if fd == nil {
		return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
	}
	fieldVal := msg.ProtoReflect().Get(fd)
	driverVal := protoValueToDriver(fd, fieldVal)

	cmp := compareValues(driverVal, litVal)
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
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
	}
}

// evalInPredicate handles: col [NOT] IN (val1, val2, ...)
func evalInPredicate(msg proto.Message, pred *antlrgen.PredicatedExpressionContext, in *antlrgen.InPredicateContext) (bool, error) {
	colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "IN left side must be a column name, got %T", pred.ExpressionAtom())
	}
	colName := fullIdToName(colAtom.FullColumnName().FullId())

	fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
	if fd == nil {
		return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
	}
	// NULL IN (...) and NULL NOT IN (...) are both UNKNOWN in SQL 3-valued logic;
	// rows with a NULL column are filtered out in both cases.
	if !msg.ProtoReflect().Has(fd) {
		return false, nil
	}
	fieldVal := protoValueToDriver(fd, msg.ProtoReflect().Get(fd))

	exprs := in.InList().Expressions().AllExpression()
	for _, expr := range exprs {
		ep, ok := expr.(*antlrgen.PredicatedExpressionContext)
		if !ok {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "IN list value must be a constant, got %T", expr)
		}
		cAtom, ok := ep.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
		if !ok {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "IN list value must be a constant, got atom %T", ep.ExpressionAtom())
		}
		litVal, err := evalConstant(cAtom.Constant())
		if err != nil {
			return false, err
		}
		if valuesEqual(fieldVal, litVal) {
			if in.NOT() != nil {
				return false, nil
			}
			return true, nil
		}
	}
	// none matched
	if in.NOT() != nil {
		return true, nil
	}
	return false, nil
}

// evalIsNullPredicate handles: col IS [NOT] NULL / IS TRUE / IS FALSE
func evalIsNullPredicate(msg proto.Message, pred *antlrgen.PredicatedExpressionContext, is *antlrgen.IsExpressionContext) (bool, error) {
	colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "IS left side must be a column name, got %T", pred.ExpressionAtom())
	}
	colName := fullIdToName(colAtom.FullColumnName().FullId())

	fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
	if fd == nil {
		return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
	}
	// Use Has() for proto2 optional presence; unset optional = SQL NULL.
	var fieldVal driver.Value
	if msg.ProtoReflect().Has(fd) {
		fieldVal = protoValueToDriver(fd, msg.ProtoReflect().Get(fd))
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

// evalLikePredicate handles: col [NOT] LIKE 'pattern'
// Supports SQL wildcards: % (any sequence) and _ (any single character).
func evalLikePredicate(msg proto.Message, pred *antlrgen.PredicatedExpressionContext, like *antlrgen.LikePredicateContext) (bool, error) {
	colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "LIKE left side must be a column name, got %T", pred.ExpressionAtom())
	}
	colName := fullIdToName(colAtom.FullColumnName().FullId())

	fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
	if fd == nil {
		return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
	}
	if fd.Kind() != protoreflect.StringKind {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "LIKE requires a string column, got %v", fd.Kind())
	}

	fieldStr := msg.ProtoReflect().Get(fd).String()

	// Pattern is the first STRING_LITERAL token; strip surrounding quotes.
	patternLit := like.GetPattern().GetText()
	pattern := stripStringLiteralQuotes(patternLit)

	matched := likeMatch(pattern, fieldStr)
	if like.NOT() != nil {
		return !matched, nil
	}
	return matched, nil
}

// likeMatch implements SQL LIKE pattern matching: % = any sequence, _ = any single char.
func likeMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	p, str := []rune(pattern), []rune(s)
	return likeMatchRunes(p, str)
}

func likeMatchRunes(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			// skip consecutive %
			for len(p) > 0 && p[0] == '%' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if likeMatchRunes(p, s[i:]) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// stripStringLiteralQuotes removes surrounding single-quotes and unescapes ” → '.
func stripStringLiteralQuotes(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "''", "'")
}

// evalBetweenPredicate handles: col [NOT] BETWEEN lo AND hi (inclusive).
func evalBetweenPredicate(msg proto.Message, pred *antlrgen.PredicatedExpressionContext, bet *antlrgen.BetweenComparisonPredicateContext) (bool, error) {
	colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "BETWEEN left side must be a column name, got %T", pred.ExpressionAtom())
	}
	colName := fullIdToName(colAtom.FullColumnName().FullId())

	fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
	if fd == nil {
		return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
	}
	fieldVal := protoValueToDriver(fd, msg.ProtoReflect().Get(fd))

	loAtom, ok := bet.GetLeft().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "BETWEEN low bound must be a constant, got %T", bet.GetLeft())
	}
	hiAtom, ok := bet.GetRight().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "BETWEEN high bound must be a constant, got %T", bet.GetRight())
	}

	lo, err := evalConstant(loAtom.Constant())
	if err != nil {
		return false, err
	}
	hi, err := evalConstant(hiAtom.Constant())
	if err != nil {
		return false, err
	}

	result := compareValues(fieldVal, lo) >= 0 && compareValues(fieldVal, hi) <= 0
	if bet.NOT() != nil {
		return !result, nil
	}
	return result, nil
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
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported constant type %T in WHERE", c)
	}
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
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// compareValues returns -1, 0, or 1 for two driver.Values.
// Used by ORDER BY post-sort. NULL sorts last.
func compareValues(a, b driver.Value) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		case float64:
			fav := float64(av)
			if fav < bv {
				return -1
			}
			if fav > bv {
				return 1
			}
			return 0
		}
	case float64:
		var fbv float64
		switch bv := b.(type) {
		case float64:
			fbv = bv
		case int64:
			fbv = float64(bv)
		default:
			return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
		}
		if av < fbv {
			return -1
		}
		if av > fbv {
			return 1
		}
		return 0
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			if av == bv {
				return 0
			}
			if !av {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
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
	action := c.factory.CreateDatabase(dbPath, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropDatabase(ctx context.Context, s *antlrgen.DropDatabaseStatementContext) (int64, error) {
	dbPath := s.Path().GetText()
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	throwIfNotExist := s.IfExists() == nil
	action := c.factory.DropDatabase(dbPath, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchema(ctx context.Context, s *antlrgen.CreateSchemaStatementContext) (int64, error) {
	schemaText := s.SchemaId().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.dbPath)
	if err != nil {
		return 0, err
	}
	templateID := s.SchemaTemplateId().GetText()
	action := c.factory.CreateSchema(dbPath, schemaName, templateID, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropSchema(ctx context.Context, s *antlrgen.DropSchemaStatementContext) (int64, error) {
	schemaText := s.Uid().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.dbPath)
	if err != nil {
		return 0, err
	}
	if dbPath == "" {
		return 0, api.NewErrorf(api.ErrCodeUnknownDatabase,
			"invalid database identifier in %q", schemaText)
	}
	action := c.factory.DropSchema(dbPath, schemaName, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	c.invalidateSchemaCache(dbPath, schemaName)
	return 0, nil
}

func (c *EmbeddedConnection) execDropSchemaTemplate(ctx context.Context, s *antlrgen.DropSchemaTemplateStatementContext) (int64, error) {
	templateID := s.Uid().GetText()
	throwIfNotExist := s.IfExists() == nil
	action := c.factory.DropSchemaTemplate(templateID, throwIfNotExist, *api.NoOptions())
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
	action := c.factory.SaveSchemaTemplate(tmpl, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	// Template change may affect any schema using it — flush the whole cache.
	c.schemaCache = make(map[string]api.Schema)
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
			return fmt.Errorf("index %q has no columns", indexName)
		}
		b.AddIndex(tableName, indexName, cols, unique)
		return nil
	default:
		return fmt.Errorf("unsupported index definition type %T; only INDEX … ON … is supported", idxDef)
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
			return nil, nil, fmt.Errorf("column %q has no type", colName)
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
			return nil, nil, fmt.Errorf("column %q: %w", colName, err)
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
		return nil, fmt.Errorf("only primitive column types are supported")
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
		return nil, fmt.Errorf("unsupported column type: %s", ct.GetText())
	}
}

// ensureCatalogInit bootstraps the catalog. Retries on transient failure
// (unlike sync.Once, a mutex+bool allows retry when the previous attempt failed).
func (c *EmbeddedConnection) ensureCatalogInit(ctx context.Context) error {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.catalogReady {
		return nil
	}
	_, err := c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		if initErr := c.cat.Initialize(txn); initErr != nil {
			return nil, initErr
		}
		return nil, txn.Commit()
	})
	if err != nil {
		return err
	}
	c.catalogReady = true
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
	_, err := c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
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
