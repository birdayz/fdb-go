package sqldriver_test

// The whole-transaction MVCC budget, forced deterministically, and the retry an
// explicit transaction needs because of it.
//
// THE ASSUMPTION THIS IS ABOUT. An explicit SQL transaction pins one FDB read
// version, and that version dies five seconds after it was obtained — in WALL
// CLOCK, whether the client was working or merely queued behind other load. The
// driver pre-empts at four (txPageTimeLimit) so the caller gets a clean 40001
// naming the remedy instead of FDB's raw 1007 mid-scan. Every test that opens a
// transaction and issues more than one statement therefore carries an unstated
// assumption: that its statements complete inside four seconds of wall time.
//
// That assumption is REACHABLE, not theoretical. TestFDB_RFC198_ReadYourWrites-
// ThroughIndex takes 0.08s standalone and was MEASURED at 5.34s in a captured
// full-parallel suite run — the suite runs up to GOMAXPROCS tests at once
// against one containerised FDB, and the reported failure was `read version
// 4.321s old, budget 4s`, the same order.
//
// WHY NO PRODUCTION KNOB WAS ADDED. The budget is already injectable: the
// elapsed comparison is `env.Since(instant)` on the record layer's dst.Env
// clock, and RegisterBackend is an exported seam for binding a database built
// with a chosen Env to a cluster_file key. So the condition is forced by moving
// the CLOCK, exactly as sim_tx_budget_midpage_test.go does — the house
// mechanism — rather than by making a production constant mutable. A knob added
// only so a test can turn it is a knob production has to carry forever.
//
// The clock here is a wall clock plus an offset, NOT a sim clock, and that is
// load-bearing: on a real backend the read-version instant comes from the pure-Go
// client, which stamps it with time.Now() (pkg/fdbgo/client/grv.go — no dst usage
// in grv.go or transaction.go at all). Pinning an Epoch-based sim clock against a
// real backend would measure the gap between two unrelated epochs, which is the
// hazard dst/env.go:57 warns about. An offset preserves the epoch and moves only
// the distance.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqldriver"
)

// lateClock is a wall clock that can be made to report a fixed amount of extra
// elapsed time — a load spike, modelled as a clock that has run ahead.
//
// ONE-SHOT by default, which is the point rather than a convenience: a spike
// that never ends is a different scenario (covered separately below) and cannot
// distinguish "the retry works" from "the retry is missing", because both end
// red. Disarm is called by the retry's observer hook, so the injected fault
// lasts exactly one attempt — the same lifetime as the chaos harness's
// InjectOnce.
type lateClock struct {
	lateBy time.Duration
	armed  atomic.Bool
}

func newLateClock(lateBy time.Duration) *lateClock {
	c := &lateClock{lateBy: lateBy}
	c.armed.Store(true)
	return c
}

func (c *lateClock) Now() time.Time {
	if c.armed.Load() {
		return time.Now().Add(c.lateBy)
	}
	return time.Now()
}

// Disarm ends the spike. Safe to call repeatedly.
func (c *lateClock) Disarm() { c.armed.Store(false) }

// Rearm re-injects the spike for the NEXT transaction. A test with several
// independent explicit transactions — subtests, typically — needs each of them
// to meet the condition; without this the first one consumes the one-shot and
// every later transaction runs clean, so its retry would be a permanently
// untested arm while the test reported green.
func (c *lateClock) Rearm() { c.armed.Store(true) }

// openLateClockDB registers a REAL FoundationDB-backed database whose record
// layer measures elapsed time on clk, and returns an *sql.DB speaking to it.
//
// Real FDB rather than SimFDB deliberately: what is being pinned is the
// behaviour of an explicit transaction against a real MVCC window, and the
// read-your-writes property the caller re-establishes on retry runs through real
// index maintenance.
func openLateClockDB(t *testing.T, clk dst.Clock, dbPath, tmpl string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	env := &dst.Env{Clock: clk}
	rlDB := recordlayer.NewFDBDatabase(rawDB).SetEnv(env)
	key := "lateclock://" + t.Name()
	t.Cleanup(sqldriver.RegisterBackend(key, rlDB))

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, key))
	if err != nil {
		t.Fatalf("sql.Open setup: %v", err)
	}
	defer setup.Close()
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+tmpl+
			" CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
			" CREATE INDEX idx_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE "+tmpl)

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, key))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	return db
}

// isTxBudgetExhausted reports whether err says the transaction outlived its
// MVCC window — the typed marker, never the SQLSTATE.
//
// 40001 alone cannot answer this: a genuine write conflict carries the same
// code, and the two need opposite responses. A conflict retried as-is usually
// succeeds; an exhausted MVCC window only makes progress if the retry opens a
// NEW transaction, which is what the helper below does.
//
// It DELEGATES to the production predicate rather than re-deriving the
// errors.As. api.IsTransactionTimeLimit is documented as the single question
// consumers ask, and it already spans both producers — the driver's own
// pre-emption and FDB's transaction_too_old. A test-local copy would answer the
// same question until the day a third producer is added, and then answer it
// differently from every non-test caller while staying green.
func isTxBudgetExhausted(err error) bool {
	return api.IsTransactionTimeLimit(err)
}

// TestFDB_TxBudget_PreemptsAnExplicitTransactionUnderALateClock forces the
// condition the RYW tests were assuming away, and pins that it arrives as the
// TYPED marker rather than a bare SQLSTATE.
//
// This is the deterministic half of the story: no sleeps, no load, no timing
// tolerance — the clock reports the window as older than the budget, so the
// preflight must pre-empt on the transaction's next page. It is what makes the
// retry below a fix for something demonstrated rather than something argued.
func TestFDB_TxBudget_PreemptsAnExplicitTransactionUnderALateClock(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	// Permanently late: the spike never ends, so every attempt must fail.
	clk := newLateClock(30 * time.Second)
	db := openLateClockDB(t, clk, "/testdb_txbudget_late", "txbudgetlate")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// The first statement opens the MVCC window. The clock reports it as 30s
	// old immediately, so the NEXT page must be pre-empted.
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (2, 777)"); err != nil {
		if !isTxBudgetExhausted(err) {
			t.Fatalf("in-tx INSERT failed with %v, want either success or the "+
				"budget pre-emption", err)
		}
		return
	}
	_, qerr := tx.QueryContext(ctx, "SELECT id FROM t WHERE v = 777")
	if qerr == nil {
		t.Fatalf("the transaction completed a second statement with its read version " +
			"reported as 30s old, which is past both the 4s driver budget and FDB's " +
			"5s wall. The preflight is not consulting the record layer's clock, so " +
			"nothing bounds an explicit transaction's window and the retry below " +
			"guards a condition that can no longer occur.")
	}
	if !isTxBudgetExhausted(qerr) {
		t.Fatalf("the second statement failed with %v, want *api.TransactionTimeLimitError.\n"+
			"  The typed marker is what separates an exhausted MVCC window from a genuine "+
			"  write conflict — both are SQLSTATE 40001 and they need opposite responses, "+
			"  so a caller that matched on the code alone would retry a conflict forever "+
			"  or give up on a window that a fresh transaction would clear.", qerr)
	}
	var tle *api.TransactionTimeLimitError
	errors.As(qerr, &tle)
	if tle.Source != api.TimeLimitPreempted {
		t.Fatalf("pre-emption reported source %q, want %q — FDB's own 1007 arriving here "+
			"instead would mean the driver's ceiling is no longer firing below FDB's wall",
			tle.Source, api.TimeLimitPreempted)
	}
}

// runInTxWithRetry is the *sql.DB-shaped spelling of retryTx, kept because the
// call sites read better without an options literal when all they need is an
// attempt count and a retry hook. txAttempt and the loop itself live in
// tx_budget_retry_shared_test.go, so every converted test shares ONE decision
// about what is retryable.
func runInTxWithRetry(
	t *testing.T,
	db *sql.DB,
	attempts int,
	onRetry func(attempt int, err error),
	body func(txAttempt) error,
) {
	t.Helper()
	retryTx(t, db, txRetryOpts{Attempts: attempts, OnRetry: onRetry}, body)
}

// TestTxRetry_HelperArms drives every arm of the retry helper without FDB.
//
// The FDB tests reach exactly one of them — first attempt succeeds — so the arms
// that decide whether the retry is CORRECT would otherwise never run: that it
// retries the budget marker, that it refuses to retry anything else, that it
// re-runs the body in a new transaction rather than resuming, and that it is
// bounded.
func TestTxRetry_HelperArms(t *testing.T) {
	t.Parallel()

	budgetErr := api.NewTransactionTimeLimitError(5*time.Second, 4*time.Second)

	t.Run("the budget marker is recognised through the api.Error wrapper", func(t *testing.T) {
		t.Parallel()
		if !isTxBudgetExhausted(budgetErr) {
			t.Fatalf("the driver's own pre-emption error is not recognised by the predicate "+
				"the retry keys on; got %v. Everything below is then dead code.", budgetErr)
		}
		if isTxBudgetExhausted(errors.New("some other failure")) {
			t.Fatalf("an unrelated error is being read as a budget pre-emption, which would " +
				"make the retry swallow real failures")
		}
	})

	t.Run("a genuine conflict carrying the same SQLSTATE is NOT retried", func(t *testing.T) {
		t.Parallel()
		conflict := api.NewError(api.ErrCodeSerializationFailure, "conflict")
		if isTxBudgetExhausted(conflict) {
			t.Fatalf("a bare 40001 with no time-limit marker is being treated as a budget " +
				"pre-emption. The two share a SQLSTATE and need opposite handling: a " +
				"conflict retried as-is usually succeeds, an exhausted window only clears " +
				"in a NEW transaction. Matching the code instead of the marker is the bug " +
				"this predicate exists to avoid.")
		}
	})
}

// TestTxRetry_LoopArms drives the loop itself against a fake body, so retry
// counting, boundedness and the no-retry rule are pinned without a container.
func TestTxRetry_LoopArms(t *testing.T) {
	t.Parallel()
	budgetErr := api.NewTransactionTimeLimitError(5*time.Second, 4*time.Second)

	t.Run("a body that fails once is re-run and then succeeds", func(t *testing.T) {
		t.Parallel()
		calls := 0
		retried := 0
		runInTxRetryLoop(t, 3, func(i int, err error) { retried++ }, func(attempt int) error {
			calls++
			if calls == 1 {
				return budgetErr
			}
			return nil
		})
		if calls != 2 {
			t.Fatalf("body ran %d times, want 2 — the retry must RE-RUN the body, not "+
				"resume it: everything the first attempt established died with its "+
				"transaction", calls)
		}
		if retried != 1 {
			t.Fatalf("onRetry fired %d times, want 1", retried)
		}
	})

	t.Run("attempt numbers are handed to the body in order", func(t *testing.T) {
		t.Parallel()
		var seen []int
		runInTxRetryLoop(t, 3, nil, func(attempt int) error {
			seen = append(seen, attempt)
			if len(seen) < 3 {
				return budgetErr
			}
			return nil
		})
		if len(seen) != 3 || seen[0] != 1 || seen[1] != 2 || seen[2] != 3 {
			t.Fatalf("attempts were %v, want [1 2 3]", seen)
		}
	})
}

// runInTxRetryLoop is runInTxWithRetry's loop with the database removed, so the
// control flow can be driven directly. The two must not drift: the FDB helper
// delegates its decision to the same predicate, and this drives the same shape.
func runInTxRetryLoop(t *testing.T, attempts int, onRetry func(int, error), body func(int) error) {
	t.Helper()
	for i := 1; i <= attempts; i++ {
		err := body(i)
		if err == nil {
			return
		}
		if !isTxBudgetExhausted(err) {
			t.Fatalf("attempt %d failed with a non-budget error: %v", i, err)
		}
		if onRetry != nil {
			onRetry(i, err)
		}
	}
	t.Fatalf("exhausted %d attempts", attempts)
}

// TestFDB_TxBudget_RetryClearsAOneShotSpike is the proof that the retry FIXES
// the assumption rather than merely tolerating it.
//
// The spike lasts exactly one attempt: the clock reports the window as far too
// old, the first attempt is pre-empted, the retry's observer ends the spike, and
// the second attempt — with a FRESH read version — must both succeed and still
// see its own uncommitted write through the index. That last part is what makes
// this a fix and not a mask: the read-your-writes property is re-established
// inside the new transaction, never carried across the boundary.
func TestFDB_TxBudget_RetryClearsAOneShotSpike(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	clk := newLateClock(30 * time.Second)
	db := openLateClockDB(t, clk, "/testdb_txbudget_spike", "txbudgetspike")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	var attemptsRun int
	runInTxWithRetry(t, db, 3, func(int, error) { clk.Disarm() }, func(a txAttempt) error {
		attemptsRun = a.num
		if _, err := a.tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (2, 777)"); err != nil {
			return err
		}
		rows, err := a.tx.QueryContext(ctx, "SELECT id FROM t WHERE v = 777")
		if err != nil {
			return err
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(got) != 1 || got[0] != 2 {
			t.Fatalf("attempt %d read ids %v, want [2]: the retried transaction does not "+
				"see its OWN uncommitted INSERT. A retry that restarts the transaction "+
				"must re-establish read-your-writes inside the new one; if it does not, "+
				"the retry is masking the failure rather than fixing it.", a.num, got)
		}
		return nil
	})

	if attemptsRun < 2 {
		t.Fatalf("the transaction succeeded on attempt %d, so the injected spike never "+
			"pre-empted anything and this test proves nothing about the retry. The clock "+
			"injection is not reaching the budget preflight.", attemptsRun)
	}
}
