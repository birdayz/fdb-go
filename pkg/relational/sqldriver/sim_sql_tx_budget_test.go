package sqldriver

// RFC-198 criteria 6 and 7: the transaction-scoped scan budget, env-clocked.
//
// Criterion 6 — the whole-transaction time budget is anchored on the CLIENT'S
// READ-VERSION INSTANT (when FDB's 5-second MVCC window opened), not on
// statement start (the refuted proxy) and not on BeginTx (an idle transaction
// has no window). Time moves ONLY via SimClock.Advance — dst has no Sleep, and
// SimFDB's version counter has no relationship to time, so without the
// driver's pre-emption an over-budget transaction would simply keep reading
// (real FDB would 1007).
//
// Criterion 7 — the scanned-records counter is TRANSACTION-scoped: three
// statements in one transaction whose combined scans cross an armed
// OptExecutionScannedRowsLimit trip on the THIRD (per-statement counters stay
// green through all three — which is exactly what reverting the sharing
// produces, the mutation direction). Three statements, not two: with two, a
// shared counter and a per-statement counter can only be distinguished if the
// limit sits between them, and the test would silently become a test of where
// the limit was placed. The companion pins the split RFC-198 Decision 5 names:
// the RFC-130 memory budget (ExecuteState) stays STATEMENT-scoped.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/simfdb"
)

// openSimSchemaWithEnv is openSimSchema plus the dst.Env, for tests that move
// the sim clock.
func openSimSchemaWithEnv(t *testing.T, seed uint64, tableDDL string) (*sql.DB, *simfdb.SimDB, *dst.Env) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	simDB := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })

	ctx := context.Background()
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /simdb")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE tmpl "+tableDDL)
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, sim, env
}

func advanceSim(t *testing.T, env *dst.Env, d time.Duration) {
	t.Helper()
	sc, ok := env.Clock.(*dst.SimClock)
	if !ok {
		t.Fatalf("env.Clock is %T, want *dst.SimClock", env.Clock)
	}
	sc.Advance(d)
}

// TestSimTxBudget_ExhaustedBudgetPreemptsWith40001 pins criterion 6's first
// case: once the transaction's read version is older than the budget, the next
// page fails 40001 naming the 5-second window — not 54F01 (no limit the caller
// can raise helps), and never the zero-progress tripwire's "cannot progress"
// message (whose remedy — raise the limits — is not the problem).
//
// Mutation directions (verified at introduction):
//   - remove the preflight (or revert the anchor to per-page/per-statement,
//     the refuted proxy): the second SELECT re-anchors at its own start,
//     elapsed reads ~0, and the query SUCCEEDS reading at a read version FDB
//     would have refused (SimFDB does not model MVCC aging — exactly why the
//     driver must pre-empt) → this test reddens on "want an error, got rows".
func TestSimTxBudget_ExhaustedBudgetPreemptsWith40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _, env := openSimSchemaWithEnv(t, 47,
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// First read opens the MVCC window (takes the read version).
	var v int64
	if err := tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("first in-tx select: %v", err)
	}

	// The window is now 5 simulated seconds old.
	advanceSim(t, env, 5*time.Second)

	err = tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v)
	if err == nil {
		t.Fatalf("in-tx SELECT succeeded %v past the read version: the transaction "+
			"budget is not anchored on the read-version instant (SimFDB does not model "+
			"MVCC aging, so only the driver's pre-emption can stop this read)", 5*time.Second)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("over-budget in-tx SELECT failed with %v (%T), want *api.Error 40001", err, err)
	}
	if apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("over-budget in-tx SELECT: SQLSTATE %s, want %s (40001; 54F01 would tell "+
			"the caller to raise limits that cannot help): %v",
			apiErr.Code, api.ErrCodeSerializationFailure, err)
	}
	// THE TYPED IDENTITY, and it is the load-bearing arm of this test — the
	// SQLSTATE above cannot carry it, because 40001 is shared with a genuine
	// read/write conflict and the two demand opposite responses (retry as-is vs
	// decompose the work). Java draws this distinction as a first-class value
	// (Continuation.Reason.TRANSACTION_LIMIT_REACHED, Continuation.java:38);
	// api.TransactionTimeLimitError is its Go spelling.
	//
	// It replaces a strings.Contains on the remedy sentence. That was the only
	// handle anyone had, api.Error's own doc forbids parsing its wording, and the
	// absence of a typed handle had a cost: the DISTINCT cost probe's regime
	// detector could not recognise this error at all, so a loaded CI box fatalled
	// the probe instead of withholding its wall-clock criteria.
	if !api.IsTransactionTimeLimit(err) {
		t.Fatalf("the pre-emption carries no api.TransactionTimeLimitError: %v (%T).\n"+
			"Without it no caller can distinguish an exhausted MVCC window from a "+
			"genuine conflict — both are 40001 — so one of the two is always handled "+
			"wrongly, and every consumer is pushed back onto matching the message.", err, err)
	}
	var ttl *api.TransactionTimeLimitError
	if errors.As(err, &ttl); ttl.Limit != 4*time.Second || ttl.Elapsed < ttl.Limit {
		t.Fatalf("the pre-emption reports elapsed=%v limit=%v; the limit must be the "+
			"driver's own budget and the elapsed time must be at least it, or the "+
			"marker is being minted somewhere other than the budget check",
			ttl.Elapsed, ttl.Limit)
	}
	// The human-facing remedy still names the window, because the marker tells
	// code what happened and the message tells the operator what to do.
	if !strings.Contains(err.Error(), "5-second") {
		t.Fatalf("the pre-emption must name the 5-second window; got: %v", err)
	}
	if strings.Contains(err.Error(), "cannot progress under the configured per-page resource limits") {
		t.Fatalf("the pre-emption surfaced as the zero-progress liveness tripwire, whose "+
			"remedy (raise the limits) is not the problem: %v", err)
	}
}

// TestSimTxBudget_IdleBeforeFirstReadIsFree pins criterion 6's second case:
// the anchor is the READ VERSION, not BeginTx. A transaction that idles past
// the whole budget before reading anything has not opened an MVCC window, and
// its first read must succeed — FDB itself would serve it.
func TestSimTxBudget_IdleBeforeFirstReadIsFree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _, env := openSimSchemaWithEnv(t, 53,
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Idle far past the budget with NO statement issued.
	advanceSim(t, env, 10*time.Second)

	var v int64
	if err := tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("first read after a long idle failed (%v): the budget anchored on "+
			"BeginTx or on statement minting instead of the read-version instant — "+
			"no MVCC window was open during the idle", err)
	}
	if v != 100 {
		t.Fatalf("v = %d, want 100", v)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// simTxBudgetConn pins a single *sql.Conn on db and configures its embedded
// connection.
func simTxBudgetConn(t *testing.T, db *sql.DB, configure func(*embedded.EmbeddedConnection)) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(dc any) error {
		configure(dc.(*embedded.EmbeddedConnection))
		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	return conn
}

// TestSimTxBudget_ScannedRecordsSharedAcrossStatements pins criterion 7: with
// an armed scanned-records limit and fail-on-scan-limit, three statements in
// one explicit transaction whose combined scans cross the limit trip on the
// THIRD. The records check is the one whose free initial pass is bypassed in
// fail mode (Java's CursorLimitManager.java:135-136 usedInitialPass ||
// failOnScanLimitReached), which is what makes the trip deterministic.
//
// Mutation direction (verified at introduction): revert the transaction-scoped
// ScanState sharing (fresh state per page) → each statement's counter resets,
// all three pass, this test reddens.
func TestSimTxBudget_ScannedRecordsSharedAcrossStatements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _, _ := openSimSchemaWithEnv(t, 59,
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	for i := 0; i < 12; i++ {
		mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i))
	}

	conn := simTxBudgetConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 30).Build())
		ec.SetFailOnScanLimitReached(true)
	})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	count := func(stmt int) (int64, error) {
		var n int64
		err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n)
		if err == nil && n != 12 {
			t.Fatalf("statement %d: COUNT(*) = %d, want 12", stmt, n)
		}
		return n, err
	}

	if _, err := count(1); err != nil { // counter ≈ 12 of 30
		t.Fatalf("statement 1 tripped the shared budget early: %v", err)
	}
	if _, err := count(2); err != nil { // counter ≈ 24 of 30
		t.Fatalf("statement 2 tripped the shared budget early: %v", err)
	}
	_, err = count(3) // crosses 30 mid-scan
	if err == nil {
		t.Fatalf("statement 3 completed a full scan with the transaction's combined " +
			"scanned records past the armed limit: the ScanLimiterState is per-statement, " +
			"not transaction-scoped (RFC-198 Decision 5)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeExecutionLimitReached {
		t.Fatalf("statement 3 failed with %v, want *api.Error 54F01 (the armed scan limit "+
			"in fail mode)", err)
	}
}

// TestSimTxBudget_ArmedLimitWithoutFailModeStaysCorrect guards the hazard the
// shared counter could introduce: WITHOUT fail mode, a spent shared budget
// degrades every later page to the per-cursor free initial pass (one record
// per page) — slow, but it must stay CORRECT and terminate, never livelock
// into the zero-progress tripwire or truncate rows.
func TestSimTxBudget_ArmedLimitWithoutFailModeStaysCorrect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _, _ := openSimSchemaWithEnv(t, 61,
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	for i := 0; i < 12; i++ {
		mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i))
	}

	conn := simTxBudgetConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 10).Build())
		// FailOnScanLimitReached deliberately left false.
	})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for stmt := 1; stmt <= 3; stmt++ {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM t")
		if err != nil {
			t.Fatalf("statement %d: %v", stmt, err)
		}
		got := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("statement %d scan: %v", stmt, err)
			}
			got++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("statement %d iteration: %v (paginate mode must stay correct when "+
				"the shared budget is spent — degraded page size, never an error)", stmt, err)
		}
		rows.Close()
		if got != 12 {
			t.Fatalf("statement %d returned %d rows, want 12 — a spent shared budget must "+
				"not truncate results in paginate mode", stmt, got)
		}
	}
}

// TestSimTxBudget_MemoryBudgetStaysStatementScoped is criterion 7's companion
// at the e2e level: two ORDER BY statements in one transaction, each of whose
// sorts individually clears the armed RFC-130 budget, both succeed.
//
// MEASURED at introduction: this e2e shape CANNOT detect a
// transaction-scoped ExecuteState, because every RFC-130 charge is released
// within its page (executor.go's ReleaseMemory pairs), so sequential
// statements never see each other's consumption even through a shared
// counter. The load-bearing pins live in the embedded package instead:
// TestExecuteStateIsStatementScopedInTx asserts the structural split (fresh
// state per statement — reddens if props.State is dragged onto the
// embeddedTx) and pins the released-to-zero fact whose change would make
// sharing observable again.
func TestSimTxBudget_MemoryBudgetStaysStatementScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _, _ := openSimSchemaWithEnv(t, 67,
		"CREATE TABLE t (id BIGINT, v STRING, PRIMARY KEY (id))")
	// Rows with a payload big enough that ONE in-memory sort uses more than
	// half the armed budget: two statements sharing one counter would exceed
	// it, two statements with fresh counters both fit.
	payload := strings.Repeat("x", 200)
	for i := 0; i < 40; i++ {
		mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, '%s')", i, payload))
	}

	// Calibrate: measure one statement's charge by finding a budget that a
	// single ORDER BY fits in. 40 rows × ~200B payload ≈ 8KB+ of sort buffer;
	// a 15000-byte budget fits one sort comfortably but not two.
	conn := simTxBudgetConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxStatementMemoryBytes, int64(15000)).Build())
	})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// ORDER BY v (non-PK, unindexed) forces the in-memory sort that charges
	// the RFC-130 budget.
	for stmt := 1; stmt <= 2; stmt++ {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM t ORDER BY v, id")
		if err != nil {
			t.Fatalf("statement %d: %v — each statement must get the FULL RFC-130 memory "+
				"budget; a failure here means ExecuteState was dragged onto the transaction", stmt, err)
		}
		got := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("statement %d scan: %v", stmt, err)
			}
			got++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("statement %d iteration: %v — the memory budget must reset per "+
				"statement, not accumulate across the transaction", stmt, err)
		}
		rows.Close()
		if got != 40 {
			t.Fatalf("statement %d returned %d rows, want 40", stmt, got)
		}
	}
}
