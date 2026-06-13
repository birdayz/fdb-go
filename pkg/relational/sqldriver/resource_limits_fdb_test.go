package sqldriver_test

// RFC-106a: per-statement resource governance — FDB integration tests.
//
// Each piece is OFF by default; these tests prove (a) that a configured
// limit surfaces as SQLSTATE 54F01 (api.ErrCodeExecutionLimitReached) and
// (b) — critically — that the no-option/default path is INERT (Torvalds
// default-safety). The configs are installed on a pinned *sql.Conn's
// underlying *embedded.EmbeddedConnection via Raw, mirroring the
// installLogger pattern in plan_logging_fdb_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

// pinEmbeddedConn pins a single *sql.Conn and hands back its underlying
// *embedded.EmbeddedConnection so a test can install the RFC-106a
// connection-local config (options / fail-on-scan / statement timeout /
// result-byte cap). The returned *sql.Conn MUST be used for every
// subsequent statement so the configured connection is the one that
// executes them.
func pinEmbeddedConn(t *testing.T, db *sql.DB, configure func(*embedded.EmbeddedConnection)) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("driver conn is %T, want *embedded.EmbeddedConnection", driverConn)
		}
		configure(ec)
		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	return conn
}

// seedItemsOnConn inserts n rows (id, payload) into table Item via the pinned conn.
func seedItemsOnConn(t *testing.T, ctx context.Context, conn *sql.Conn, n int, payload string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Item (id, payload) VALUES (%d, '%s')", i, payload)); err != nil {
			t.Fatalf("INSERT row %d: %v", i, err)
		}
	}
}

// wantExecLimit asserts err is an *api.Error carrying SQLSTATE 54F01.
func wantExecLimit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", api.ErrCodeExecutionLimitReached)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if apiErr.Code != api.ErrCodeExecutionLimitReached {
		t.Fatalf("error code = %q, want %q (full: %v)", apiErr.Code, api.ErrCodeExecutionLimitReached, err)
	}
}

// drainQuery runs sql on conn, scanning into one int column, draining all
// rows; returns the row count and the terminal error (query or iteration).
func drainIDs(ctx context.Context, conn *sql.Conn, sqlText string) (int, error) {
	r, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	n := 0
	for r.Next() {
		var id int64
		if serr := r.Scan(&id); serr != nil {
			return n, serr
		}
		n++
	}
	return n, r.Err()
}

// TestFDB_RFC106a_ScanLimitFail proves the FailOnScanLimitReached parity
// path: with OptExecutionScannedRowsLimit set AND FailOnScanLimitReached,
// a scan that exceeds the limit errors 54F01. Without the flag (default),
// the same option just paginates and the query completes with every row.
func TestFDB_RFC106a_ScanLimitFail(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_scanfail", "scanfail",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 50
	const scanLimit = 5

	// --- fail mode: scan limit + FailOnScanLimitReached → 54F01 ---
	failConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	seedItemsOnConn(t, ctx, failConn, rows, "x")

	_, err := drainIDs(ctx, failConn, "SELECT id FROM Item")
	wantExecLimit(t, err)

	// --- paginate mode (default): same scan limit, NO fail flag → all rows ---
	pageConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		// FailOnScanLimitReached left false (default) → paginate.
	})
	got, perr := drainIDs(ctx, pageConn, "SELECT id FROM Item")
	if perr != nil {
		t.Fatalf("paginate drain: %v", perr)
	}
	if got != rows {
		t.Fatalf("paginate returned %d rows, want %d (pagination must be transparent)", got, rows)
	}
}

// TestFDB_RFC106a_MaxRowsStatementWide pins the codex ruling: OptMaxRows is
// a TOTAL returned-row cap across all pages, not a per-page size. With a
// scanned-records limit forcing multi-page pagination and MAX_ROWS=10 over
// a 30-row table, EXACTLY 10 rows come back.
func TestFDB_RFC106a_MaxRowsStatementWide(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_maxrows", "maxrows",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 30
	const maxRows = 10
	const perPage = 4 // force several pages so a per-page misread would over/under-count

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxRows, maxRows).
			Set(api.OptExecutionScannedRowsLimit, perPage). // paginate every perPage rows
			Build())
		// No fail flag → pages roll forward transparently.
	})
	seedItemsOnConn(t, ctx, conn, rows, "x")

	got, err := drainIDs(ctx, conn, "SELECT id FROM Item")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got != maxRows {
		t.Fatalf("MAX_ROWS=%d returned %d rows total, want exactly %d (statement-wide, not per-page)", maxRows, got, maxRows)
	}
}

// TestFDB_RFC106a_StatementTimeout proves the §4 statement timeout maps to
// 54F01 "statement timeout". Deterministic: the timeout is 1ns, already in
// the past by the time the first cursor checks ctx.Err() — no wall-clock
// race. The same query with NO timeout completes and returns every row.
func TestFDB_RFC106a_StatementTimeout(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_timeout", "timeout",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 20

	// Seed via a no-config conn so the timeout never bites the inserts.
	seedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	seedItemsOnConn(t, ctx, seedConn, rows, "x")

	// --- timeout fires: 1ns is already expired at execution time ---
	toConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetStatementTimeout(1 * time.Nanosecond)
	})
	_, err := drainIDs(ctx, toConn, "SELECT id FROM Item ORDER BY id")
	wantExecLimit(t, err)
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Message != "statement timeout" {
		t.Fatalf("timeout message = %q, want %q", apiErr.Message, "statement timeout")
	}

	// --- no timeout: same query completes ---
	okConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		// statementTimeout left 0 → off.
	})
	got, oerr := drainIDs(ctx, okConn, "SELECT id FROM Item ORDER BY id")
	if oerr != nil {
		t.Fatalf("no-timeout drain: %v", oerr)
	}
	if got != rows {
		t.Fatalf("no-timeout returned %d rows, want %d", got, rows)
	}
}

// TestFDB_RFC106a_ResultSizeCap proves the §5 result-byte cap: a wide-row
// scan whose returned bytes exceed the cap errors 54F01. The same scan
// with no cap completes.
func TestFDB_RFC106a_ResultSizeCap(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_bytes", "bytes",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 20
	// Each payload is 200 bytes; 20 rows ~= 4KB of string bytes. Cap at 1KB.
	wide := make([]byte, 200)
	for i := range wide {
		wide[i] = 'a'
	}
	const byteCap = 1024

	seedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	seedItemsOnConn(t, ctx, seedConn, rows, string(wide))

	// --- cap fires ---
	capConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetMaxResultBytes(byteCap)
	})
	_, err := func() (int, error) {
		r, qerr := capConn.QueryContext(ctx, "SELECT id, payload FROM Item")
		if qerr != nil {
			return 0, qerr
		}
		defer r.Close()
		n := 0
		for r.Next() {
			var id int64
			var p string
			if serr := r.Scan(&id, &p); serr != nil {
				return n, serr
			}
			n++
		}
		return n, r.Err()
	}()
	wantExecLimit(t, err)

	// --- no cap: completes ---
	okConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	r, qerr := okConn.QueryContext(ctx, "SELECT id, payload FROM Item")
	if qerr != nil {
		t.Fatalf("no-cap query: %v", qerr)
	}
	defer r.Close()
	got := 0
	for r.Next() {
		var id int64
		var p string
		if serr := r.Scan(&id, &p); serr != nil {
			t.Fatalf("scan: %v", serr)
		}
		got++
	}
	if err := r.Err(); err != nil {
		t.Fatalf("no-cap drain: %v", err)
	}
	if got != rows {
		t.Fatalf("no-cap returned %d rows, want %d", got, rows)
	}
}
