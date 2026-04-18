package api

import (
	"context"
	"net/url"
)

// Driver is the relational-layer driver. Mirrors Java's
// com.apple.foundationdb.relational.api.RelationalDriver.
//
// This is the Go-native driver surface. The database/sql adapter
// lives in pkg/relational/sqldriver and delegates to an
// implementation of this interface.
//
// Deliberate deviation from Java: we drop the java.sql.Driver
// inheritance (getPropertyInfo / jdbcCompliant / getParentLogger /
// accepts / DriverPropertyInfo[]) — those are JDBC boilerplate that
// Go's database/sql doesn't need.
type Driver interface {
	// Connect opens a connection to the database described by url
	// with the given options. ctx propagates cancellation/deadlines.
	// Mirrors Java's RelationalDriver.connect(URI, Options).
	Connect(ctx context.Context, url *url.URL, opts *Options) (Connection, error)

	// MajorVersion / MinorVersion report the driver version. Java
	// exposes these via java.sql.Driver; kept here because our
	// DatabaseMetaData will surface them.
	MajorVersion() int
	MinorVersion() int
}

// Connection is the Go-native connection interface. Mirrors Java's
// RelationalConnection, minus the JDBC legacy surface.
//
// Match methods use context.Context for cancellation, return typed
// errors instead of throwing, and return concrete Statement /
// PreparedStatement / ResultSet interfaces. The adapter in
// pkg/relational/sqldriver wraps this into a
// database/sql/driver.Conn.
type Connection interface {
	// CreateStatement constructs an empty RelationalStatement. Matches
	// Java's createStatement(). The statement shares the connection's
	// lifecycle — closing the connection invalidates it.
	CreateStatement(ctx context.Context) (Statement, error)

	// PrepareStatement compiles sql (with `?` placeholders) into a
	// reusable prepared statement. Matches Java's
	// prepareStatement(String).
	PrepareStatement(ctx context.Context, sql string) (PreparedStatement, error)

	// Options returns the current options map. Matches Java's
	// getOptions().
	Options() *Options
	// SetOption applies name=value to the connection-level options.
	// Matches Java's setOption(Name, Object).
	SetOption(name OptionName, value any) error

	// Path returns the database path this connection targets (matches
	// Java's getPath() returning a URI).
	Path() *url.URL

	// SetAutoCommit toggles auto-commit mode. Matches JDBC's
	// Connection.setAutoCommit.
	SetAutoCommit(autoCommit bool) error
	// AutoCommit reports the current auto-commit mode.
	AutoCommit() bool

	// Commit / Rollback end the current explicit transaction. Return
	// ErrCodeCannotCommitRollbackWithAutocommit if called with
	// auto-commit enabled (matches Java's behavior).
	Commit() error
	Rollback() error

	// SetSchema selects the default schema. Matches JDBC's
	// setSchema(String).
	SetSchema(schema string) error
	// Schema returns the current default schema name. Empty if none.
	Schema() string

	// Close releases the connection. Idempotent — close twice must
	// not panic.
	Close() error
	// IsClosed reports whether Close has been called.
	IsClosed() bool
}

// Statement is a SQL execution context. Mirrors Java's
// RelationalStatement — lean Go subset.
type Statement interface {
	// ExecuteQuery runs a SELECT and returns a ResultSet. The cursor
	// owns the underlying FDB transaction for its lifetime (auto-close
	// when the ResultSet is closed).
	ExecuteQuery(ctx context.Context, sql string) (ResultSet, error)

	// ExecuteUpdate runs a DML statement (INSERT/UPDATE/DELETE) or DDL
	// and returns the number of affected rows (matching Java's
	// executeUpdate / Statement.executeUpdate).
	ExecuteUpdate(ctx context.Context, sql string) (int64, error)

	// Execute runs an arbitrary statement. Returns true if it produces
	// a ResultSet; false if it returns an update count. After calling,
	// use ResultSet() / UpdateCount() to retrieve the result.
	Execute(ctx context.Context, sql string) (bool, error)

	// ResultSet returns the ResultSet from the last Execute, or nil.
	ResultSet() ResultSet
	// UpdateCount returns the row count from the last Execute, or -1
	// if the last operation produced a ResultSet.
	UpdateCount() int64

	// Close releases the statement. Idempotent.
	Close() error
	// IsClosed reports whether Close has been called.
	IsClosed() bool
}

// PreparedStatement is a parameterised SQL statement. Mirrors Java's
// RelationalPreparedStatement — lean Go subset.
type PreparedStatement interface {
	Statement

	// SetObject binds value to parameter at parameterIndex
	// (1-indexed per JDBC convention, not 0-indexed).
	SetObject(parameterIndex int, value any) error
	// ClearParameters unbinds every parameter.
	ClearParameters() error

	// ExecuteQueryPrepared runs the statement with the currently-bound
	// parameters.
	ExecuteQueryPrepared(ctx context.Context) (ResultSet, error)
	// ExecuteUpdatePrepared runs the statement with bound parameters
	// as an update.
	ExecuteUpdatePrepared(ctx context.Context) (int64, error)
}
