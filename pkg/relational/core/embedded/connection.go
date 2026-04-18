// Package embedded implements the embedded (in-process) SQL execution engine
// for the FoundationDB relational layer.
//
// EmbeddedConnection is the Go equivalent of Java's EmbeddedRelationalConnection.
// It parses SQL, routes DDL statements through the MetadataOperationsFactory,
// and (eventually) routes DML through the query planner.
package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// EmbeddedConnection is an in-process SQL connection backed by FDB.
//
// Implements driver.Conn and driver.ExecerContext so DDL statements can
// execute without a Prepare round-trip.
//
// Transaction model (phase 1 — auto-commit only):
//
//	Every DDL statement runs in its own FDB transaction.
//	DML and queries return ErrCodeUnsupportedOperation until the query
//	planner lands in a later phase.
type EmbeddedConnection struct {
	dbPath  string // current database URI (e.g. "/mydb")
	schema  string // current schema name (set via USE SCHEMA / SetSchema)
	fdbDB   *recordlayer.FDBDatabase
	cat     *catalog.RecordLayerStoreCatalog
	ks      *keyspace.RelationalKeyspace
	factory apiddl.MetadataOperationsFactory
	closed  bool

	// catalogInit ensures catalog.Initialize is called exactly once.
	catalogInit    sync.Once
	catalogInitErr error
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
		dbPath:  dbPath,
		fdbDB:   fdbDB,
		cat:     cat,
		factory: factory,
		ks:      ks,
	}
}

// ExecContext executes SQL (DDL only in phase 1) and returns the result.
// Implements driver.ExecerContext so database/sql skips the Prepare round-trip.
func (c *EmbeddedConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.closed {
		return nil, driver.ErrBadConn
	}
	if len(args) != 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"parameterised DDL is not supported")
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

// QueryContext is not yet implemented (query planner pending).
func (c *EmbeddedConnection) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return nil, api.NewError(api.ErrCodeUnsupportedOperation,
		"query execution is not yet implemented; only DDL is supported")
}

// Prepare returns a prepared statement. DDL statements have no bind parameters.
func (c *EmbeddedConnection) Prepare(query string) (driver.Stmt, error) {
	if c.closed {
		return nil, driver.ErrBadConn
	}
	return &embeddedStmt{conn: c, query: query}, nil
}

// Close marks the connection as closed.
func (c *EmbeddedConnection) Close() error {
	c.closed = true
	return nil
}

// Begin is a no-op stub. Phase 1 is auto-commit only.
func (c *EmbeddedConnection) Begin() (driver.Tx, error) {
	return nil, api.NewError(api.ErrCodeUnsupportedOperation,
		"explicit transactions are not yet implemented")
}

// SetSchema sets the current schema label used when no schema is specified in SQL.
func (c *EmbeddedConnection) SetSchema(s string) { c.schema = s }

// GetSchema returns the current schema label.
func (c *EmbeddedConnection) GetSchema() string { return c.schema }

// GetDBPath returns the current database path.
func (c *EmbeddedConnection) GetDBPath() string { return c.dbPath }

// execStatement routes a single parsed statement to the right handler.
func (c *EmbeddedConnection) execStatement(ctx context.Context, stmt antlrgen.IStatementContext) (int64, error) {
	ddl := stmt.DdlStatement()
	if ddl == nil {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation,
			"only DDL statements are supported in this release")
	}
	create := ddl.CreateStatement()
	drop := ddl.DropStatement()
	switch {
	case create != nil:
		return c.execCreate(ctx, create)
	case drop != nil:
		return c.execDrop(ctx, drop)
	default:
		return 0, api.NewError(api.ErrCodeUnsupportedOperation,
			"unsupported DDL statement")
	}
}

func (c *EmbeddedConnection) execCreate(ctx context.Context, cs antlrgen.ICreateStatementContext) (int64, error) {
	switch t := cs.(type) {
	case *antlrgen.CreateDatabaseStatementContext:
		return c.execCreateDatabase(ctx, t)
	case *antlrgen.CreateSchemaStatementContext:
		return c.execCreateSchema(ctx, t)
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
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropSchemaTemplate(ctx context.Context, s *antlrgen.DropSchemaTemplateStatementContext) (int64, error) {
	templateID := s.Uid().GetText()
	throwIfNotExist := s.IfExists() == nil
	action := c.factory.DropSchemaTemplate(templateID, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

// Ping implements driver.Pinger. Bootstraps the catalog on first call.
func (c *EmbeddedConnection) Ping(ctx context.Context) error {
	if c.closed {
		return driver.ErrBadConn
	}
	c.catalogInit.Do(func() {
		_, c.catalogInitErr = c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			txn := catalog.NewFDBTransaction(rctx)
			if err := c.cat.Initialize(txn); err != nil {
				return nil, err
			}
			return nil, txn.Commit()
		})
	})
	return c.catalogInitErr
}

// runDDL bootstraps the catalog on first call, then executes action.
func (c *EmbeddedConnection) runDDL(ctx context.Context, action apiddl.ConstantAction) error {
	c.catalogInit.Do(func() {
		_, c.catalogInitErr = c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			txn := catalog.NewFDBTransaction(rctx)
			if err := c.cat.Initialize(txn); err != nil {
				return nil, err
			}
			return nil, txn.Commit()
		})
	})
	if c.catalogInitErr != nil {
		return c.catalogInitErr
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
		return id[:idx], id[idx+1:], nil
	}
	return currentDB, id, nil
}

// validateDatabasePath checks that the path starts with /.
func validateDatabasePath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"database path must start with /: %q", p)
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
	return nil, fmt.Errorf("query not implemented")
}

// Static interface checks.
var (
	_ driver.Conn          = (*EmbeddedConnection)(nil)
	_ driver.ExecerContext = (*EmbeddedConnection)(nil)
	_ driver.Pinger        = (*EmbeddedConnection)(nil)
)
