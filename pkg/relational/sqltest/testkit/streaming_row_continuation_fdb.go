package testkit

// RFC-180 A6 — driver-level pins for the streaming-aggregate and in-memory-sort
// PER-ROW continuations. The consumer of an emitted row's continuation in the
// engine is a parent cursor that snapshots its child's position mid-stream —
// here a FlatMap join whose OUTER is the aggregate / sort: when a page boundary
// (EXECUTION_SCANNED_ROWS_LIMIT, paginate mode) lands mid-inner, the
// FlatMapContinuation carries priorOuter = the OUTER ROW's continuation, and the
// next page re-opens the aggregate/sort FROM that row's continuation. Before A6
// those rows carried the fake {0x00} bytes and the resume failed with "invalid
// aggregate/sort continuation" — a paged query ERRORED where the unpaged one
// succeeded.

import (
	"database/sql"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// pagedConn returns a connection in paginate mode with a small scanned-rows
// budget, so the engine crosses many internal page boundaries and resumes via
// continuations transparently.
func PagedConn(t *testing.T, db *sql.DB, budget int) *sql.Conn {
	t.Helper()
	return PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, budget).Build())
	})
}
