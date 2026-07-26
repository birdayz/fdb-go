package sqldriver_test

// RFC-106a: per-statement resource governance — FDB integration tests.
//
// Each piece is OFF by default; these tests prove (a) that a configured
// limit surfaces as SQLSTATE 54F01 (api.ErrCodeExecutionLimitReached) and
// (b) — critically — that the no-option/default path is INERT
// (default-safety). The configs are installed on a pinned *sql.Conn's
// underlying *embedded.EmbeddedConnection via Raw, mirroring the
// installLogger pattern in plan_logging_fdb_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
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

// wantScanLimitReached asserts err is SQLSTATE 54F01 caused SPECIFICALLY by a
// leaf cursor's *recordlayer.ScanLimitReachedError with the given Reason —
// not merely SOME 54F01. 54F01 is shared with other resource-limit causes
// (the paginating driver's liveness tripwire in cascades_generator.go, a
// statement timeout, RFC-130's memory budget, …), any of which would also
// satisfy wantExecLimit; a query that stalls instead of aggregating its scan
// budget could still trip the liveness tripwire and wrongly pass a bare
// wantExecLimit check. errors.As unwraps through api.Error.Cause (set by
// WrapError in translateExecError's scan-limit branch) to the underlying
// typed error.
func wantScanLimitReached(t *testing.T, err error, wantReason recordlayer.NoNextReason) {
	t.Helper()
	wantExecLimit(t, err)
	var scanErr *recordlayer.ScanLimitReachedError
	if !errors.As(err, &scanErr) {
		t.Fatalf("want *recordlayer.ScanLimitReachedError in the error chain, got %T (%v)", err, err)
	}
	if scanErr.Reason != wantReason {
		t.Fatalf("ScanLimitReachedError.Reason = %v, want %v (full: %v)", scanErr.Reason, wantReason, err)
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

// TestFDB_RFC106a_MaxRowsStatementWide pins the review ruling: OptMaxRows is
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

// TestFDB_RFC106a_ScalarSubqueryHonorsLimit pins the review P2: an uncorrelated
// scalar subquery must respect the statement's scan limit, not bypass it with
// DefaultExecuteProperties. Isolation uses TWO tables so the outer scan can
// never trip the limit on its own (a single-table WHERE id < (subquery) fails
// to isolate — the planner folds the subquery value into a range scan over the
// outer):
//
//	Small: 1 row   — the outer table; scans ≤1 no matter how the predicate plans
//	Big:   50 rows — the subquery's COUNT(*) source; full scan >> limit
//
//	control: SELECT id FROM Small                              — scans 1 → OK
//	subject: SELECT id FROM Small WHERE id < (SELECT COUNT(*) FROM Big)
//
// The ONLY scan that exceeds the limit in the subject is the COUNT(*) over Big.
// With the limit reaching the subquery it errors 54F01; before the fix the
// subquery used DefaultExecuteProperties and the subject succeeded like the
// control (revert-proof: drop the props thread → subject goes green).
func TestFDB_RFC106a_ScalarSubqueryHonorsLimit(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_ssqlimit", "ssqlimit",
		"CREATE TABLE Big (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE Small (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const bigRows = 50
	const scanLimit = 5 // COUNT(*) over 50 rows >> 5; Small has a single row

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	// Seed via INSERTs (point writes — unaffected by the scan limit).
	for i := 0; i < bigRows; i++ {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO Big (id) VALUES (%d)", i)); err != nil {
			t.Fatalf("INSERT Big row %d: %v", i, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO Small (id) VALUES (0)"); err != nil {
		t.Fatalf("INSERT Small: %v", err)
	}

	// control: the outer (Small, 1 row) alone stays under the limit → no error.
	if _, err := drainIDs(ctx, conn, "SELECT id FROM Small"); err != nil {
		t.Fatalf("control (1-row outer) must not trip the scan limit, got: %v", err)
	}

	// subject: same outer + a COUNT(*) subquery over Big that scans all rows → 54F01.
	_, err := drainIDs(ctx, conn,
		"SELECT id FROM Small WHERE id < (SELECT COUNT(*) FROM Big)")
	wantExecLimit(t, err)
}

// TestFDB_RFC106a_BufferedScanLimitErrorsNotTruncates pins the review follow-up:
// in PAGINATE mode (FailOnScanLimitReached=false) a leaf cursor that hits the
// scan limit returns an out-of-band NoNext + continuation — but a BUFFERED
// operator (here a scalar subquery) cannot paginate, so silently breaking on
// !HasNext would truncate its input and fabricate a wrong scalar value. The
// errIfBufferTruncated guard turns that out-of-band stop into 54F01 instead.
//
// Isolation: the subquery `SELECT id FROM Big WHERE val = 'target'` full-scans
// Big (val is unindexed) and the single matching row (id=49) sits PAST the
// scan limit, so the buffered scan is cut off before finding it. WITH the guard
// → 54F01; without it → the subquery truncates to no row, the outer predicate
// `id < NULL` yields zero rows, and the query wrongly succeeds (revert-proof:
// drop errIfBufferTruncated in scalar_subquery.go → this returns 0 rows, no err).
func TestFDB_RFC106a_BufferedScanLimitErrorsNotTruncates(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_buftrunc", "buftrunc",
		"CREATE TABLE Big (id BIGINT, val STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE Small (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const bigRows = 50
	const scanLimit = 5 // the only matching Big row is id=49, well past 5

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		// FailOnScanLimitReached deliberately LEFT FALSE → paginate mode. The
		// buffered subquery must still error rather than truncate.
	})
	for i := 0; i < bigRows; i++ {
		val := "x"
		if i == bigRows-1 {
			val = "target"
		}
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Big (id, val) VALUES (%d, '%s')", i, val)); err != nil {
			t.Fatalf("INSERT Big row %d: %v", i, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO Small (id) VALUES (0)"); err != nil {
		t.Fatalf("INSERT Small: %v", err)
	}

	_, err := drainIDs(ctx, conn,
		"SELECT id FROM Small WHERE id < (SELECT id FROM Big WHERE val = 'target')")
	wantExecLimit(t, err)
}

// TestFDB_RFC106a_AggregateIndexScanLimit pins the review P2: a grouped
// aggregate-index scan (countKVCursor) must honor the wired scan limits — before
// the fix it checked only ReturnedRowLimit, so a high-cardinality GROUP BY could
// read every group entry past the cap without 54F01. The COUNT(*) GROUP BY index
// gives one entry per group; with 50 groups and ScannedRowsLimit=5 + fail mode,
// countKVCursor stops at the 5th group entry and errors. Revert-proof: drop the
// ScannedRecordsLimit branch from countKVCursor.OnNext → all 50 groups return.
func TestFDB_RFC106a_AggregateIndexScanLimit(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_aggscan", "aggscan",
		"CREATE TABLE ga (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM ga GROUP BY g")
	ctx := context.Background()

	const groups = 50
	const scanLimit = 5

	// Prove the grouped COUNT plans as an AggregateIndex scan (the countKVCursor
	// leaf review flagged), not a full-scan fallback that would hit a different
	// cursor. EXPLAIN is static — no data needed.
	const groupedCount = "SELECT g, COUNT(*) FROM ga GROUP BY g"
	if plan := planExplainVia(t, ctx, db, groupedCount); !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("grouped COUNT must plan as AggregateIndex (exercises countKVCursor), got: %s", plan)
	}

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	// Distinct g per row → `groups` aggregate-index entries. Atomic COUNT-index
	// maintenance is an atomic ADD (no read), so the limit never bites the seed.
	for i := 0; i < groups; i++ {
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO ga (id, g) VALUES (%d, %d)", i, i)); err != nil {
			t.Fatalf("INSERT ga row %d: %v", i, err)
		}
	}

	rows, qerr := conn.QueryContext(ctx, groupedCount)
	if qerr == nil {
		for rows.Next() { //nolint:revive // draining to reach the terminal error
		}
		qerr = rows.Err()
		rows.Close()
	}
	wantExecLimit(t, qerr)
}

// TestFDB_RFC106a_RowLimitBeatsScanLimit pins the review r3 ordering fix: when
// pageRowBudget injects MAX_ROWS as the page's ReturnedRowLimit AND a scan limit
// is set to the SAME value, a leaf cursor must check the returned-row limit FIRST
// (clean ReturnLimitReached) — the caller asked for and received exactly N rows;
// the scan backstop was not exceeded, so turning it into a 54F01 is wrong. Uses
// the aggregate-index (countKVCursor) path. Revert-proof: check ScannedRecordsLimit
// before ReturnedRowLimit in countKVCursor → MAX_ROWS=5 errors 54F01.
func TestFDB_RFC106a_RowLimitBeatsScanLimit(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_rowbeats", "rowbeats",
		"CREATE TABLE ga (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM ga GROUP BY g")
	ctx := context.Background()
	const groups = 50

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxRows, 5).
			Set(api.OptExecutionScannedRowsLimit, 5).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	for i := 0; i < groups; i++ {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO ga (id, g) VALUES (%d, %d)", i, i)); err != nil {
			t.Fatalf("INSERT ga row %d: %v", i, err)
		}
	}
	n, err := drainPairs(ctx, conn, "SELECT g, COUNT(*) FROM ga GROUP BY g")
	if err != nil {
		t.Fatalf("MAX_ROWS=5 with an equal scan limit must return cleanly, got: %v", err)
	}
	if n != 5 {
		t.Fatalf("want exactly 5 rows (MAX_ROWS satisfied), got %d", n)
	}
}

// TestFDB_RFC106a_DMLNoPartialMutationInExplicitTx pins the review r3 P1: a DML
// statement cut off by a resource limit must abort with ZERO mutations, never
// leave a partially-applied DELETE staged in an explicit transaction that a later
// commit would persist. Pre-materializing the target set means the 54F01 fires
// before any record is deleted. (Auto-commit can't distinguish this — the error
// rolls back either way — so the test drives an EXPLICIT tx and commits after the
// error.) Revert-proof: stream the DELETE (delete-as-you-go) → 5 rows commit, 45 remain.
func TestFDB_RFC106a_DMLNoPartialMutationInExplicitTx(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_dmltx", "dmltx",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	const rows = 50

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 5).Build())
		// non-fail (paginate): the DELETE's inner scan stops OUT-OF-BAND at 5 →
		// pre-materialize errors before any delete.
	})
	for i := 0; i < rows; i++ {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	_, derr := tx.ExecContext(ctx, "DELETE FROM t")
	wantExecLimit(t, derr)
	// Commit after the error: the staged writes (if any) become durable. With
	// pre-materialize there are none, so all rows survive.
	_ = tx.Commit()

	plain := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	n, cerr := drainIDs(ctx, plain, "SELECT id FROM t")
	if cerr != nil {
		t.Fatalf("count after failed DELETE: %v", cerr)
	}
	if n != rows {
		t.Fatalf("a resource-limit-aborted DELETE must leave all %d rows intact (no partial commit), got %d", rows, n)
	}
}

// TestFDB_RFC106a_DMLDeadlineAbortsCleanly pins the guarantee that a DML statement
// whose deadline has expired aborts with ZERO mutations — never stages the target
// set and returns success. The PRIMARY guard is the ctx-gated collection phase
// (CollectAllBounded checks ctx.Err() before each OnNext), which this 1ns-timeout
// case exercises; the explicit pre-mutation-loop re-check in executeDelete is
// defensive-in-depth for the (non-deterministically reproducible) window after
// collection and is intentionally NOT what this test pins. Driven in an explicit
// tx + commit-after-error to prove no partial writes become durable
// (honest scope — this pins atomic abort, not the specific defensive line).
func TestFDB_RFC106a_DMLDeadlineAbortsCleanly(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_dmldeadline", "dmldeadline",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	const rows = 30

	seed := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	for i := 0; i < rows; i++ {
		if _, err := seed.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetStatementTimeout(1 * time.Nanosecond) // already expired at execution
	})
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	_, derr := tx.ExecContext(ctx, "DELETE FROM t")
	wantExecLimit(t, derr)
	_ = tx.Commit()

	n, cerr := drainIDs(ctx, seed, "SELECT id FROM t")
	if cerr != nil {
		t.Fatalf("count after timed-out DELETE: %v", cerr)
	}
	if n != rows {
		t.Fatalf("a deadline-aborted DELETE must leave all %d rows intact, got %d", rows, n)
	}
}

// TestFDB_UnionAllResumesAcrossScanLimitPages pins the RFC-180 WS-A union
// resume: a UNION ALL whose first branch spans many scan-limit pages must
// return the COMPLETE row set — the streaming union folds its branches
// through recordlayer.ConcatCursors (Java's branch-tagged ConcatContinuation),
// so each page resumes the in-progress branch exactly where it stopped.
// Historically this asserted a loud 54F01 instead: the correct-or-loud
// stopgap for the era when the union DISCARDED its continuation and a resume
// silently replayed branch A from row 0. Revert-proof: restore the
// continuation-discarding executeUnionStreaming → duplicated branch-A pages
// (row count > 51) or a loud out-of-band error — either fails this pin.
func TestFDB_UnionAllResumesAcrossScanLimitPages(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_unionall", "unionall",
		"CREATE TABLE A (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE B (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 5).Build())
		// non-fail: the first branch's scan stops OUT-OF-BAND at 5 → concat errors.
	})
	for i := 0; i < 50; i++ {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO A (id) VALUES (%d)", i)); err != nil {
			t.Fatalf("INSERT A %d: %v", i, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO B (id) VALUES (100)"); err != nil {
		t.Fatalf("INSERT B: %v", err)
	}

	// Branch A (50 rows) hits the scanned-rows limit of 5 per page. With
	// per-branch continuation states (RFC-180 WS-A: the streaming union folds
	// through recordlayer.ConcatCursors, Java's branch-tagged
	// ConcatContinuation) the union RESUMES across pages exactly like Java's
	// paginating execution — the old loud 54F01 here was the correct-or-loud
	// STOPGAP for the era when the union discarded its continuation and a
	// resume silently replayed branch A from row 0. The stopgap is obsolete;
	// the stronger pin is the complete result: all 50 A rows plus B's one,
	// no drops, no duplicates.
	n, err := drainIDs(ctx, conn, "SELECT id FROM A UNION ALL SELECT id FROM B")
	if err != nil {
		t.Fatalf("union across page boundaries: %v", err)
	}
	if n != 51 {
		t.Fatalf("union across page boundaries returned %d rows, want 51 (50 from A + 1 from B)", n)
	}
}

// TestFDB_RFC106a_INJoinScanLimitAggregatesAcrossLegs pins the fix for a real
// hang class: indexFetchCursor/keyValueCursor — the leaf cursors under a
// RecordQueryInJoinPlan — previously tracked scanned-records against a LOCAL,
// per-cursor counter reset fresh for EVERY IN-value's own leg
// (key_value_cursor.go's c.recordsScanned, index_scan.go's c.recordsRead).
// A many-legged IN-join whose individual legs each stayed under the
// configured scan limit therefore never tripped it, however large the
// IN-list: production impact was a query with a large IN-list and small
// per-value match counts running unbounded inside a single FDB transaction,
// past the 5s hard wall, retried forever by FDBDatabase.Run with no error
// ever surfaced. ScanLimiterState (recordlayer/scan_limiter_state.go) fixes
// this by sharing ONE counter set across every leg of the IN-join — mirroring
// Java's ExecuteState.RecordScanLimiter, held by reference across
// ExecuteProperties.clearSkipAndLimit() (ExecuteState.java:44-47,
// CursorLimitManager.java:88-98).
//
// The scanned-RECORDS limit is the deterministic proxy for the reported
// wall-clock hang (reproducing that directly needs an actual multi-second FDB
// round trip): it is the SAME shared/reset-per-leg counter, checked by the
// SAME leaf cursors, on the line immediately next to the TimeLimit check —
// see key_value_cursor.go's OnNext and index_scan.go's indexCursor.OnNext.
//
// 30 IN-values, each matching exactly ONE row (id = literal), plan as
// InJoin(Scan(ITEM,[=])) — a primary-key equality scan per leg, verified by
// EXPLAIN. ScannedRowsLimit=5: with the bug, every leg's LOCAL counter starts
// at 0 and never reaches 5 (1 row per leg), so all 30 legs run to completion
// and the query wrongly returns 30 rows / never errors even in fail mode.
// Fixed, the shared counter crosses 5 partway through the IN-list and the
// query stops — errors in fail mode, or truncates to a resumable page in
// paginate mode (proving no silent unbounded run AND no data loss across
// pages).
func TestFDB_RFC106a_INJoinScanLimitAggregatesAcrossLegs(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_injoinscan", "injoinscan",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 30
	const scanLimit = 5

	seed := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	seedItemsOnConn(t, ctx, seed, rows, "x")

	inList := make([]string, rows)
	for i := range inList {
		inList[i] = fmt.Sprintf("%d", i)
	}
	q := "SELECT id FROM Item WHERE id IN (" + strings.Join(inList, ",") + ")"

	// Plan-shape guard: this must be an IN-JOIN over a primary-key equality
	// scan (the leaf cursor this test pins), not some other shape a future
	// planner change might substitute.
	if plan := planExplainVia(t, ctx, db, q); !strings.Contains(plan, "InJoin(Scan(") {
		t.Fatalf("want InJoin(Scan(...)) plan shape, got: %s", plan)
	}

	// --- fail mode: the AGGREGATE scan across every leg must trip the limit,
	// even though no single leg (1 row) comes close to it on its own.
	failConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	_, err := drainIDs(ctx, failConn, q)
	wantScanLimitReached(t, err, recordlayer.ScanLimitReached)

	// --- paginate mode: the same aggregate limit forces multiple pages, but
	// every row still comes back — the fix bounds the scan, it does not drop
	// rows or fail to resume across the IN-list.
	pageConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
	})
	got, perr := drainIDs(ctx, pageConn, q)
	if perr != nil {
		t.Fatalf("paginate drain: %v", perr)
	}
	if got != rows {
		t.Fatalf("paginate returned %d rows, want %d (pagination must be transparent, no row loss)", got, rows)
	}
}

// TestFDB_RFC106a_INUnionScanLimitAggregatesAcrossLegs is the InUnion sibling
// of the InJoin test above — the exact "indexFetchCursor" mechanism named in
// the original hang report (a many-legged IN-union over a SECONDARY index,
// each leg's entries fetched by indexFetchCursor wrapping the index_scan.go
// leaf cursor). ORDER BY on the IN column forces the InUnion (merge-sort)
// plan shape instead of InJoin (verified by EXPLAIN).
func TestFDB_RFC106a_INUnionScanLimitAggregatesAcrossLegs(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc106a_inunionscan", "inunionscan",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX payload_idx ON Item (payload)")
	ctx := context.Background()

	const rows = 30
	const scanLimit = 5

	seed := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	for i := 0; i < rows; i++ {
		if _, err := seed.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Item (id, payload) VALUES (%d, 'p%d')", i, i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	inList := make([]string, rows)
	for i := range inList {
		inList[i] = fmt.Sprintf("'p%d'", i)
	}
	q := "SELECT id FROM Item WHERE payload IN (" + strings.Join(inList, ",") + ") ORDER BY payload"

	if plan := planExplainVia(t, ctx, db, q); !strings.Contains(plan, "InUnion(IndexScan(") {
		t.Fatalf("want InUnion(IndexScan(...)) plan shape, got: %s", plan)
	}

	failConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	_, err := drainIDs(ctx, failConn, q)
	wantScanLimitReached(t, err, recordlayer.ScanLimitReached)

	pageConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
	})
	got, perr := drainIDs(ctx, pageConn, q)
	if perr != nil {
		t.Fatalf("paginate drain: %v", perr)
	}
	if got != rows {
		t.Fatalf("paginate returned %d rows, want %d (pagination must be transparent, no row loss)", got, rows)
	}
}

// drainPairs runs sql scanning two columns (int, int), returning the row count.
func drainPairs(ctx context.Context, conn *sql.Conn, sqlText string) (int, error) {
	r, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	n := 0
	for r.Next() {
		var a, b int64
		if serr := r.Scan(&a, &b); serr != nil {
			return n, serr
		}
		n++
	}
	return n, r.Err()
}
