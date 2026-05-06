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
	// driver.Stmt.Query has no context parameter — Go's database/sql
	// only calls this fallback path when StmtQueryContext (below) is
	// unimplemented, so this branch never fires in practice. Kept for
	// the legacy driver.Stmt contract.
	return s.conn.QueryContext(context.Background(), s.query, named)
}

// ExecContext implements driver.StmtExecContext — the context-aware
// execution path. Go's database/sql prefers this over Exec when the
// statement implements the interface, propagating the user's ctx
// (cancellation + timeout) into the executor.
func (s *embeddedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

// QueryContext implements driver.StmtQueryContext — same rationale as
// ExecContext.
func (s *embeddedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

// Compile-time assertions: embeddedStmt satisfies the modern
// context-aware interfaces in addition to driver.Stmt. database/sql
// type-asserts these at runtime; pinning them at compile time
// catches accidental signature drift on the methods above.
var (
	_ driver.Stmt             = (*embeddedStmt)(nil)
	_ driver.StmtExecContext  = (*embeddedStmt)(nil)
	_ driver.StmtQueryContext = (*embeddedStmt)(nil)
)
