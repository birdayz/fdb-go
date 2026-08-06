package sqldriver_test

// Post-execution query statistics (RFC-211), end to end over real FDB.
//
// The counters these tests pin are not new measurements. Go's ScanLimiterState
// has always counted scanned records and bytes, because that is what the
// scanned-rows / scanned-bytes limits are checked against — the ports of
// Java's ExecuteState.getRecordsScanned() (ExecuteState.java:114) and
// getBytesScanned() (:122). What was missing was any way to READ them: nothing
// aggregated them per statement and nothing handed them to a caller. So these
// tests are about delivery and arithmetic, and they pin EXACT counts rather
// than bounds — a bound would pass with the arithmetic broken in either
// direction.
//
// The dimensions are deliberately separated, because they fail independently:
// scanned is not returned (a filter makes them differ), scanned survives the
// error that kills the statement (the charge is per attempt on the way out, not
// at a success checkpoint), and returned is not affected (DML never passes
// through the row-returning path). A test that only ever ran an unfiltered
// SELECT would see one number in every field and could not tell any of these
// apart.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// statsCapture is a concurrency-safe ExecutionStatsLogger for tests.
type statsCapture struct {
	mu     sync.Mutex
	events []embedded.ExecutionStats
}

func (c *statsCapture) LogExecutionStats(_ context.Context, s embedded.ExecutionStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, s)
}

func (c *statsCapture) snapshot() []embedded.ExecutionStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]embedded.ExecutionStats, len(c.events))
	copy(out, c.events)
	return out
}

// reset drops captured events so a test can ignore its own seeding traffic.
func (c *statsCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

// only returns the single captured event, failing loudly on any other count.
// "Exactly one" is itself an assertion: a statement that emitted twice (a
// non-idempotent finish) or zero times (a path that never reaches Close) is a
// bug this would otherwise hide behind an index into a slice.
func (c *statsCapture) only(t *testing.T) embedded.ExecutionStats {
	t.Helper()
	ev := c.snapshot()
	if len(ev) != 1 {
		t.Fatalf("want exactly 1 execution-stats event, got %d: %+v", len(ev), ev)
	}
	return ev[0]
}

// execStatsRows is the seeded row count. Ten rows over five distinct `g`
// values makes an index equality match exactly two — small enough to pin
// exactly, and far enough below the table size that a full scan cannot be
// mistaken for a selective one.
const execStatsRows = 10

// execStatsGroups is the number of distinct `g` values, so each matches
// execStatsRows/execStatsGroups rows.
const execStatsGroups = 5

// setupExecStatsDB creates the table, seeds it, and returns the database. The
// index on (g) is what makes the selectivity dimension testable at all.
func setupExecStatsDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db := setupErrorTestDB(t, "/testdb_"+name, name,
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON t (g)")
	ctx := context.Background()
	for i := 0; i < execStatsRows; i++ {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, g, v) VALUES (%d, %d, %d)", i, i%execStatsGroups, i)); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	return db
}

// pinStatsConn pins a connection with an execution-stats logger installed,
// through the same conn.Raw route the operator documentation prescribes.
func pinStatsConn(t *testing.T, db *sql.DB, extra func(*embedded.EmbeddedConnection)) (*sql.Conn, *statsCapture) {
	t.Helper()
	cap := &statsCapture{}
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetExecutionStatsLogger(cap)
		if extra != nil {
			extra(ec)
		}
	})
	return conn, cap
}

// drain iterates a result set to exhaustion and returns the row count. Closing
// is what emits the record, and database/sql closes on exhaustion, so this is
// the ordinary caller shape rather than a test-only one.
func drain(t *testing.T, rows *sql.Rows) int {
	t.Helper()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return n
}

// TestFDB_ExecutionStats_FullScanPinsExactCounts is the base case: an
// unfiltered scan of N rows must report N scanned and N returned, with real
// bytes, one statement, and no retries.
//
// It also pins the fields that identify the record — SQL text and plan hash —
// because a stats record an operator cannot attribute to a query is telemetry
// they cannot act on. The plan hash in particular is the join key back to the
// PlanGenerationInfo (RFC-034) that produced the plan; a cached plan executes
// many times under one planning record, so the hash is the only thing relating
// the N execution records to the one plan.
func TestFDB_ExecutionStats_FullScanPinsExactCounts(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatsfull")
	ctx := context.Background()

	conn, cap := pinStatsConn(t, db, nil)
	cap.reset()

	const q = "SELECT id, g, v FROM t"
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if got := drain(t, rows); got != execStatsRows {
		t.Fatalf("returned %d rows, want %d", got, execStatsRows)
	}

	s := cap.only(t)
	if s.RecordsScanned != execStatsRows {
		t.Errorf("RecordsScanned = %d, want %d (a full scan charges one record per row)",
			s.RecordsScanned, execStatsRows)
	}
	if s.RowsReturned != execStatsRows {
		t.Errorf("RowsReturned = %d, want %d", s.RowsReturned, execStatsRows)
	}
	if s.RowsAffected != 0 {
		t.Errorf("RowsAffected = %d, want 0 for a SELECT", s.RowsAffected)
	}
	if s.BytesScanned <= 0 {
		t.Errorf("BytesScanned = %d, want > 0 — the rows carry key and value bytes", s.BytesScanned)
	}
	if s.Pages < 1 {
		t.Errorf("Pages = %d, want at least 1", s.Pages)
	}
	if s.Retries != 0 {
		t.Errorf("Retries = %d, want 0 on an uncontended scan", s.Retries)
	}
	if s.ExecutionDuration <= 0 {
		t.Errorf("ExecutionDuration = %v, want > 0", s.ExecutionDuration)
	}
	if s.Err != nil {
		t.Errorf("Err = %v, want nil", s.Err)
	}
	if s.SQL != q {
		t.Errorf("SQL = %q, want %q (canonical text, not GetText())", s.SQL, q)
	}
	if s.PlanHash == 0 {
		t.Errorf("PlanHash = 0; the record cannot be joined to its planning record")
	}
}

// TestFDB_ExecutionStats_IndexEqualityIsSelective proves the scanned count
// tracks the ACCESS PATH rather than echoing the result size or the table size.
//
// This is the dimension that makes the number worth reporting: an operator
// attributing cluster load needs to know that a query touched two entries and
// not ten. The EXPLAIN assertion is load-bearing — without it a regression that
// turned the index scan into a full scan with a filter would still return two
// rows, and an assertion on rows alone would stay green while the reported cost
// silently quintupled.
func TestFDB_ExecutionStats_IndexEqualityIsSelective(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatsidx")
	ctx := context.Background()

	const q = "SELECT id FROM t WHERE g = 3"
	plan := planExplainVia(t, ctx, db, q)
	if !strings.Contains(strings.ToLower(plan), "t_g") {
		t.Fatalf("plan %q does not use the t_g index; the selectivity claim would be untestable", plan)
	}

	conn, cap := pinStatsConn(t, db, nil)
	cap.reset()

	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	const wantMatches = execStatsRows / execStatsGroups
	if got := drain(t, rows); got != wantMatches {
		t.Fatalf("returned %d rows, want %d", got, wantMatches)
	}

	s := cap.only(t)
	if s.RecordsScanned != wantMatches {
		t.Errorf("RecordsScanned = %d, want %d (the index visits only the matching entries)",
			s.RecordsScanned, wantMatches)
	}
	if s.RecordsScanned >= execStatsRows {
		t.Errorf("RecordsScanned = %d is not below the table size %d — the count is not tracking the access path",
			s.RecordsScanned, execStatsRows)
	}
	if s.RowsReturned != wantMatches {
		t.Errorf("RowsReturned = %d, want %d", s.RowsReturned, wantMatches)
	}
}

// TestFDB_ExecutionStats_ScannedExceedsReturned pins the dimension every
// single-number test misses: a filtered LIMIT 1 returns one row while scanning
// the whole table to find it.
//
// Cost attribution lives or dies on this distinction. A tenant issuing this
// query costs the cluster ten reads and reports one row; a stats surface that
// conflated the two would report the cheapest possible number for the most
// expensive shape. The filter is on `v`, which carries no index, so the scan
// genuinely has to walk to the last row.
func TestFDB_ExecutionStats_ScannedExceedsReturned(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatslimit")
	ctx := context.Background()

	conn, cap := pinStatsConn(t, db, nil)
	cap.reset()

	// v == execStatsRows-1 is the LAST row, so the scan cannot short-circuit
	// early and the scanned count is the full table.
	q := fmt.Sprintf("SELECT id FROM t WHERE v = %d LIMIT 1", execStatsRows-1)
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if got := drain(t, rows); got != 1 {
		t.Fatalf("returned %d rows, want 1", got)
	}

	s := cap.only(t)
	if s.RowsReturned != 1 {
		t.Errorf("RowsReturned = %d, want 1 (LIMIT 1)", s.RowsReturned)
	}
	if s.RecordsScanned != execStatsRows {
		t.Errorf("RecordsScanned = %d, want %d — the true cost of finding one matching row",
			s.RecordsScanned, execStatsRows)
	}
	if s.RecordsScanned == s.RowsReturned {
		t.Errorf("RecordsScanned == RowsReturned == %d; the two dimensions are not being measured separately",
			s.RowsReturned)
	}
}

// TestFDB_ExecutionStats_ScanLimitReportsConsumedAlongsideError is the error
// path the whole design turns on: a statement killed by
// EXECUTION_SCANNED_ROWS_LIMIT must still report what it consumed.
//
// This is not a courtesy. The statements an operator most needs accounted for
// are the ones that blew a budget, and a stats surface that only reported on
// success would go silent exactly there. It is structural rather than
// special-cased — the per-attempt charge is a defer, so it fires on the way out
// of the failing attempt — and it is Java's own guarantee: the limiter object
// lives on the caller's ExecuteState and outlives ScanLimitReachedException, so
// getRecordsScanned() still reads after the trip (ExecuteState.java:114).
func TestFDB_ExecutionStats_ScanLimitReportsConsumedAlongsideError(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatslimiterr")
	ctx := context.Background()

	const scanLimit = 3
	conn, cap := pinStatsConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		// Fail rather than paginate, so the limit ENDS the statement and the
		// record has to be emitted from the error path.
		ec.SetFailOnScanLimitReached(true)
	})
	cap.reset()

	rows, err := conn.QueryContext(ctx, "SELECT id, g, v FROM t")
	if err == nil {
		// Some shapes surface the limit during iteration rather than at
		// QueryContext; both must still emit, so drain and re-read the error.
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if err == nil {
		t.Fatalf("expected the scanned-rows limit to end the statement, got no error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeExecutionLimitReached {
		t.Fatalf("error = %v, want %s (54F01)", err, api.ErrCodeExecutionLimitReached)
	}

	s := cap.only(t)
	if s.Err == nil {
		t.Errorf("Err = nil on a statement that failed; the error path is not reporting")
	}
	if s.RecordsScanned < scanLimit {
		t.Errorf("RecordsScanned = %d, want at least the %d it was allowed to consume — "+
			"a killed statement must still account for what it used",
			s.RecordsScanned, scanLimit)
	}
	if s.BytesScanned <= 0 {
		t.Errorf("BytesScanned = %d, want > 0 on a statement that read before failing", s.BytesScanned)
	}
}

// TestFDB_ExecutionStats_DMLReportsAffectedNotReturned pins the split between
// the two row dimensions. DML drains through countAll rather than Next, so a
// collector that only ever incremented on the row-returning path would report
// zero for every mutation — the statements with the most cluster impact.
func TestFDB_ExecutionStats_DMLReportsAffectedNotReturned(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatsdml")
	ctx := context.Background()

	conn, cap := pinStatsConn(t, db, nil)
	cap.reset()

	// g = 3 matches exactly execStatsRows/execStatsGroups rows.
	res, err := conn.ExecContext(ctx, "DELETE FROM t WHERE g = 3")
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	const wantAffected = execStatsRows / execStatsGroups
	if n != wantAffected {
		t.Fatalf("RowsAffected = %d, want %d", n, wantAffected)
	}

	s := cap.only(t)
	if s.RowsAffected != wantAffected {
		t.Errorf("stats RowsAffected = %d, want %d", s.RowsAffected, wantAffected)
	}
	if s.RowsReturned != 0 {
		t.Errorf("stats RowsReturned = %d, want 0 — DML returns a count, not a result set", s.RowsReturned)
	}
	if s.RecordsScanned <= 0 {
		t.Errorf("RecordsScanned = %d, want > 0 — the DELETE had to find its targets", s.RecordsScanned)
	}
	if s.Err != nil {
		t.Errorf("Err = %v, want nil", s.Err)
	}
}

// TestFDB_ExecutionStats_ExplicitTransactionChargesPerStatement pins the one
// dimension no auto-commit test can express: that the scan charge is a DELTA
// around each attempt and not an absolute read of the counter.
//
// In auto-commit the two are indistinguishable — executeProps mints a fresh
// ScanLimiterState per page, so the entry count is always zero and
// `delta == absolute`. Every other test in this file therefore stays green with
// the arithmetic replaced by a plain read. Inside an explicit transaction the
// state is TRANSACTION-scoped (RFC-198 Decision 5) and cumulative across every
// page of every statement, so an absolute read makes the second statement
// re-report the first one's scan, the third report both, and so on — a tenant's
// reported cost growing quadratically in the length of their transaction while
// every auto-commit test stays green.
//
// Two statements is the minimum shape that can express it. One statement cannot
// (its entry count is still zero), which is exactly the "pin the reproducer,
// not a simpler cousin of it" trap.
func TestFDB_ExecutionStats_ExplicitTransactionChargesPerStatement(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatstx")
	ctx := context.Background()

	conn, cap := pinStatsConn(t, db, nil)
	cap.reset()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const q = "SELECT id, g, v FROM t"
	for i := 0; i < 2; i++ {
		rows, err := tx.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("SELECT %d in transaction: %v", i, err)
		}
		if got := drain(t, rows); got != execStatsRows {
			t.Fatalf("SELECT %d returned %d rows, want %d", i, got, execStatsRows)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	events := cap.snapshot()
	if len(events) != 2 {
		t.Fatalf("want 2 execution-stats events (one per statement), got %d: %+v", len(events), events)
	}
	for i, s := range events {
		if s.RecordsScanned != execStatsRows {
			t.Errorf("statement %d: RecordsScanned = %d, want %d — each statement must be charged "+
				"its OWN scan, not the transaction-scoped counter's running total",
				i, s.RecordsScanned, execStatsRows)
		}
		if s.RowsReturned != execStatsRows {
			t.Errorf("statement %d: RowsReturned = %d, want %d", i, s.RowsReturned, execStatsRows)
		}
		if s.Retries != 0 {
			t.Errorf("statement %d: Retries = %d, want 0 — an explicit transaction is never retried",
				i, s.Retries)
		}
	}
	// Stated as its own assertion because it is the actual defect shape: the
	// second statement inheriting the first's total.
	if events[1].RecordsScanned > events[0].RecordsScanned {
		t.Errorf("the second statement reported MORE scan (%d) than the first (%d) over identical work — "+
			"the transaction-scoped counter is being read absolutely instead of per statement",
			events[1].RecordsScanned, events[0].RecordsScanned)
	}
}

// TestFDB_ExecutionStats_SlowQueryFiresOnExecutionTime pins the third piece of
// the brief: the slow-query threshold now has a second, independent consumer.
//
// Before this, SlowQuery could only ever mean "planning was slow" — the one
// half of a query an operator investigating a slow statement is least likely to
// be looking for. The two sub-tests are the two directions, because a flag
// hardwired to either constant passes one of them.
func TestFDB_ExecutionStats_SlowQueryFiresOnExecutionTime(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatsslow")
	ctx := context.Background()

	run := func(t *testing.T, thresholdMicros int64) embedded.ExecutionStats {
		t.Helper()
		conn, cap := pinStatsConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetSlowQueryThresholdMicros(thresholdMicros)
		})
		cap.reset()
		rows, err := conn.QueryContext(ctx, "SELECT id FROM t")
		if err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		drain(t, rows)
		return cap.only(t)
	}

	t.Run("tiny threshold flags the statement", func(t *testing.T) {
		t.Parallel()
		// 1µs: any real FDB round trip exceeds it.
		if s := run(t, 1); !s.SlowQuery {
			t.Errorf("SlowQuery = false at a 1µs threshold with ExecutionDuration %v", s.ExecutionDuration)
		}
	})

	t.Run("huge threshold does not", func(t *testing.T) {
		t.Parallel()
		// One hour.
		if s := run(t, 3_600_000_000); s.SlowQuery {
			t.Errorf("SlowQuery = true at a one-hour threshold with ExecutionDuration %v", s.ExecutionDuration)
		}
	})
}

// TestFDB_ExecutionStats_NilLoggerIsSilentAndHarmless pins the production
// default. Every nil-guard in the scope is on this path, and the whole
// zero-overhead claim rests on the statement behaving identically without a
// logger — including the DML and error shapes, whose emission points are
// explicit calls rather than database/sql's Close.
func TestFDB_ExecutionStats_NilLoggerIsSilentAndHarmless(t *testing.T) {
	t.Parallel()
	db := setupExecStatsDB(t, "xstatsnil")
	ctx := context.Background()

	// No logger installed at all.
	rows, err := db.QueryContext(ctx, "SELECT id, g, v FROM t")
	if err != nil {
		t.Fatalf("SELECT with no logger: %v", err)
	}
	if got := drain(t, rows); got != execStatsRows {
		t.Fatalf("returned %d rows, want %d", got, execStatsRows)
	}

	res, err := db.ExecContext(ctx, "DELETE FROM t WHERE g = 1")
	if err != nil {
		t.Fatalf("DELETE with no logger: %v", err)
	}
	if n, _ := res.RowsAffected(); n != execStatsRows/execStatsGroups {
		t.Fatalf("RowsAffected = %d, want %d", n, execStatsRows/execStatsGroups)
	}

	// And the error path, whose statsErr staging runs whether or not a logger
	// is present.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 2).Build())
		ec.SetFailOnScanLimitReached(true)
	})
	r2, err := conn.QueryContext(ctx, "SELECT id, g, v FROM t")
	if err == nil {
		for r2.Next() {
		}
		err = r2.Err()
		r2.Close()
	}
	if err == nil {
		t.Fatalf("expected the scan limit to end the statement even with no logger")
	}
}
