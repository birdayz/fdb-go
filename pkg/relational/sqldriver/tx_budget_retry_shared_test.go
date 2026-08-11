package sqldriver_test

// The shared retry every explicit-transaction test needs, and the seam that
// forces the condition it retries.
//
// WHAT THE ASSUMPTION IS. An explicit SQL transaction pins one FDB read
// version, and that version dies five seconds after it was obtained in WALL
// CLOCK — whether the client was working or merely queued behind other load.
// So a test that opens a transaction and issues more than one statement is
// silently asserting that all of them finish inside that window. Under a
// full-parallel suite run against one containerised FDB that assertion is
// reachable, not theoretical: TestFDB_RFC198_ReadYourWritesThroughIndex takes
// 0.08s standalone and was measured at 5.34s under load.
//
// TWO PRODUCERS, ONE PREDICATE — and the difference decides what each test
// below can force. The driver's own pre-emption (preflightTxBudget, 4s) runs
// ONLY on a READ page and ONLY inside an explicit transaction
// (cascades_generator.go, `if r.tx != nil`). A transaction whose statements are
// all DML therefore never meets it; it meets FDB's own transaction_too_old
// (1007) at the real five-second wall, at a later statement or at COMMIT. Both
// carry the same marker — api.NewTransactionTimeLimitError at one producer,
// api.MarkFDBTransactionTooOld at the other — which is why one predicate covers
// both and why no test here matches on SQLSTATE.
//
// WHY NO PRODUCTION KNOB. The budget is already injectable: the comparison is
// env.Since(instant) on the record layer's dst.Env clock, and RegisterBackend is
// an exported seam for binding a database built with a chosen Env to a
// cluster_file key. The condition is forced by moving the CLOCK. A knob added
// only so a test can turn it is a knob production carries forever.
//
// The clock is a WALL clock plus an offset, never an Epoch-pinned sim clock: on
// a real backend the read-version instant is stamped by the pure-Go client with
// time.Now(), so a pinned epoch would compare two unrelated epochs — the hazard
// dst/env.go warns about. An offset preserves the epoch and moves only the
// distance.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqldriver"
)

// txBeginner is what the retry needs from a handle: the ability to start a
// fresh transaction. Both *sql.DB and *sql.Conn satisfy it, and the *sql.Conn
// case is not hypothetical — a test that must pin per-connection options
// (pinEmbeddedConn) can only begin from the pinned connection, and losing that
// pinning on retry would silently drop the option the test exists to exercise.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// txRetryOpts configures retryTx. The zero value is usable: three attempts, no
// hooks.
type txRetryOpts struct {
	// Attempts bounds the retry. Bounded on purpose: an explicit transaction
	// whose work genuinely cannot fit in FDB's five-second window fails every
	// attempt, and looping forever converts a legible failure into a hang.
	Attempts int

	// BeforeAttempt runs immediately before each attempt begins its
	// transaction, including the first. It exists for state a re-run would
	// otherwise double-count — a metrics scrape being the concrete case: an
	// abandoned attempt still charged its writes to the exporter, so a test
	// asserting an exact counter must re-zero it per attempt or the retry turns
	// a precise assertion into an inequality nobody wrote.
	BeforeAttempt func(attempt int)

	// OnRetry runs after an attempt was pre-empted, before the next begins.
	OnRetry func(attempt int, err error)
}

// txAttempt is what retryTx hands each attempt. The attempt number is exposed
// because a test that injects a one-shot spike must be able to assert the spike
// actually pre-empted something — a retry that never fired proves nothing.
type txAttempt struct {
	tx  *sql.Tx
	num int
}

// retryTx runs body inside an explicit transaction, restarting the WHOLE
// transaction when it is pre-empted for outliving its MVCC window.
//
// Restarting the whole thing is the only thing that CAN work and also the only
// thing that stays correct. A fresh transaction takes a fresh read version,
// which is the only way the window problem goes away; and because body re-runs
// in full, everything it established — an uncommitted INSERT, a DELETE whose
// clears a later read must see — is re-established INSIDE the new transaction
// rather than smuggled across the boundary. A retry that resumed mid-body would
// be asserting read-your-writes against writes made by a transaction that no
// longer exists.
//
// It deliberately retries NOTHING else. A genuine write conflict is the same
// SQLSTATE and is somebody else's business; retrying it here would hide a real
// isolation failure behind a loop. That is the whole reason the predicate is
// api.IsTransactionTimeLimit and not a 40001 comparison.
//
// body owns its own COMMIT. Tests that must commit call a.tx.Commit() as their
// last act; the Rollback below is then a no-op. Tests that must not commit
// simply return, and the rollback discards the attempt.
//
// ASSERTIONS INSIDE body MUST STILL FATAL. Only DRIVER errors are returned for
// the retry to classify; a failed assertion is a verdict, not a transient, and
// returning it would hand a real bug to the retry loop to paper over.
func retryTx(t *testing.T, b txBeginner, o txRetryOpts, body func(txAttempt) error) {
	t.Helper()
	attempts := o.Attempts
	if attempts == 0 {
		attempts = 3
	}
	var last error
	for i := 1; i <= attempts; i++ {
		if o.BeforeAttempt != nil {
			o.BeforeAttempt(i)
		}
		tx, err := b.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("begin (attempt %d): %v", i, err)
		}
		err = body(txAttempt{tx: tx, num: i})
		_ = tx.Rollback()
		if err == nil {
			return
		}
		if !api.IsTransactionTimeLimit(err) {
			t.Fatalf("attempt %d failed with %v — only the whole-transaction budget "+
				"pre-emption is retried here, because it is the one 40001 a FRESH "+
				"transaction clears. Anything else, a genuine write conflict included, "+
				"must surface.", i, err)
		}
		last = err
		if o.OnRetry != nil {
			o.OnRetry(i, err)
		}
	}
	t.Fatalf("the transaction was pre-empted for outliving its MVCC window on all %d "+
		"attempts; last error: %v", attempts, last)
}

// spikeOnce returns retry options that end a one-shot clock spike after the
// first pre-emption, plus a pointer that records the attempt the body last ran.
//
// The one-shot lifetime is the point rather than a convenience. A spike that
// never ends cannot distinguish "the retry works" from "the retry is missing",
// because both end red; a spike lasting exactly one attempt reddens only when
// the retry is absent. It is the same lifetime as the chaos harness's
// InjectOnce.
func spikeOnce(clk *lateClock, ranAttempt *int) txRetryOpts {
	return txRetryOpts{
		Attempts: 3,
		OnRetry:  func(int, error) { clk.Disarm() },
		BeforeAttempt: func(i int) {
			if ranAttempt != nil {
				*ranAttempt = i
			}
		},
	}
}

// mustHaveRetried fails unless the injected spike actually pre-empted an
// attempt. Without it a converted test degrades silently into one that never
// exercises the retry — green, and proving nothing about the condition it was
// converted for.
func mustHaveRetried(t *testing.T, attemptsRun int) {
	t.Helper()
	if attemptsRun < 2 {
		t.Fatalf("the transaction succeeded on attempt %d, so the injected spike never "+
			"pre-empted anything and this test proves nothing about the retry. Either "+
			"the clock injection is not reaching preflightTxBudget, or this transaction "+
			"no longer issues an in-transaction READ page — the driver's pre-emption "+
			"runs only there (`if r.tx != nil` around preflightTxBudget), so a body that "+
			"became DML-only stops meeting it.", attemptsRun)
	}
}

// spikedClusterKey registers a REAL FoundationDB-backed database whose record
// layer measures elapsed time on a one-shot late clock, and returns the key to
// pass as `cluster_file=` in a DSN.
//
// Returning a KEY rather than an *sql.DB is deliberate: the tests below build
// several handles on one backend (a setup handle, a schema-scoped handle, a
// second connection that must observe isolation from outside), and they must
// all reach the SAME registered database or the isolation assertions would be
// comparing two unrelated stores.
//
// Autocommit is unaffected by the spike, which is what makes this safe to arm
// for a whole test: preflightTxBudget runs under `if r.tx != nil`, so DDL and
// seed statements issued outside an explicit transaction never meet it.
func spikedClusterKey(t *testing.T, lateBy time.Duration) (string, *lateClock) {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	clk := newLateClock(lateBy)
	rlDB := recordlayer.NewFDBDatabase(rawDB).SetEnv(&dst.Env{Clock: clk})
	key := "spikeclock://" + t.Name()
	t.Cleanup(sqldriver.RegisterBackend(key, rlDB))
	return key, clk
}

// spikedDSN is the DSN form the converted tests use, so the cluster_file key is
// the ONLY difference from the handle they opened before conversion.
func spikedDSN(key, dbPath, schema string) string {
	if schema == "" {
		return fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, key)
	}
	return fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, key, schema)
}

// openSpiked opens a handle on a spiked backend and closes it with the test.
func openSpiked(t *testing.T, key, dbPath, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("fdbsql", spikedDSN(key, dbPath, schema))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
