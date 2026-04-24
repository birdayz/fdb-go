package embedded

import (
	"context"
	"database/sql/driver"
)

// embeddedStmt is a prepared DDL statement (no bind parameters).
//
// DDL statements are single-use; database/sql hands us a string and
// we route Exec / Query through the parent EmbeddedConnection's
// ExecContext / QueryContext which do the actual parse + plan + run.
// NumInput returns -1 so database/sql doesn't enforce a placeholder
// count before the query parses.
//
// Destined for pkg/relational/core/embedded/stmt.go per RFC 021
// Phase 1c — already in its own file, just lifted out of the main
// connection.go to keep the driver-layer glue small.
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
