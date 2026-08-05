package sqldriver

// A paged result set's position must advance with the transaction that produced
// the page, or not at all.
//
// paginatingRows.fetchPage runs one page inside a closure that DB.Run's retry
// loop may re-execute. The closure RESUMES the plan from r.continuation and
// truncates r.buf at its top, so any position written to r before the page's
// transaction succeeds is position a re-execution inherits: it resumes past the
// rows the failed attempt drained, having just discarded that attempt's buffer.
// The rows vanish silently — no error, no duplicate, a short result set.
//
// REACHABILITY, MEASURED. On this head no production shape can drive the
// closure a second time. Three independent properties, each owned elsewhere,
// close the window — and each is pinned below, because relaxing any ONE of them
// re-arms the defect:
//
//	1. An auto-commit SELECT page is READ-ONLY, and FDB completes a commit with
//	   no mutations and no write conflict ranges client-side, with no proxy
//	   round-trip (ReadYourWrites.actor.cpp's read-only fast path, modelled in
//	   pkg/simfdb/conflict.go). It cannot come back not_committed.
//	2. DML would carry the writes that make a page's commit fault-able, but DML
//	   NEVER PAGINATES: executeDelete/executeInsert/executeUpdate each
//	   materialize the whole target set through CollectAllBounded and return a
//	   list cursor, so a DML statement is always exactly one page and has no
//	   continuation to advance.
//	3. An explicit transaction is NOT RETRIED at all — runInCapturedTx calls the
//	   closure directly when a transaction was captured, so RFC-198's
//	   write-carrying SELECT pages have no retry loop to inherit anything.
//
// That makes this a LATENT defect rather than a shipped one, and it is why the
// regression below drives the retry through a backend wrapper instead of a SQL
// shape: no black-box query reaches the window today, and a test that cannot go
// red is not coverage. The wrapper fails ONE page's transaction with a
// retryable not_committed AFTER the closure completed — precisely the shape of
// a commit-time conflict — and asserts the paging loop survives it.
//
// WHAT LET IT THROUGH. Pagination and commit faults were each well covered, on
// disjoint axes: the sqlpage oracle (pkg/simfdb/hunt/sqlpage) paginates whole
// SQL queries hard but runs with faults OFF, and the sim SQL fault tests inject
// at commit but drive single-page statements. The defect lives only at the
// crossing, which nothing reached.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/simfdb"
)

// pageRetryRows is the seeded row count, several times pageRetryScanLimit: with
// one page there is no continuation to advance and the defect cannot be
// expressed at all.
const pageRetryRows = 12

// pageRetryScanLimit is the per-page scanned-rows budget that forces the pages.
const pageRetryScanLimit = 2

// retryOnceBackend is a SimFDB that fails ONE transaction retryably AFTER its
// closure has run to completion, then lets the retry through.
//
// It models a commit-time not_committed: the closure did all its work, the
// transaction then failed and rolled back, and SimFDB's own Transact loop
// re-executes the closure. That re-execution is the only thing this test needs
// and the one thing no SQL shape can currently produce (see the reachability
// note above). Raising the error from inside fn rather than from commit is
// deliberate — SimFDB faithfully models FDB's read-only commit fast path, which
// would swallow an injected commit fault on exactly the read-only page
// transactions under test, and the failure being modelled is not about WHERE
// the error is raised but about the closure being re-run with the result set's
// position already moved.
//
// Arming is by transaction ORDINAL so a test can choose which page fails.
// SimDB has no TransactCtx, so overriding Transact covers every route through
// recordlayer's runTransactCtx; the embedded *SimDB supplies the rest of the
// fdb.BackendDatabase surface unchanged.
type retryOnceBackend struct {
	*simfdb.SimDB
	// failAt is the 0-based ordinal of the transaction to fail, or -1 for none.
	failAt atomic.Int64
	// seen counts Transact calls since the last arm/reset — the retry route's
	// own traffic, which is what property 3 below measures.
	seen atomic.Int64
	// fired records whether the injection actually happened, so a test fails
	// loudly rather than passing because nothing was exercised.
	fired atomic.Bool
}

func newRetryOnceBackend(sim *simfdb.SimDB) *retryOnceBackend {
	b := &retryOnceBackend{SimDB: sim}
	b.failAt.Store(-1)
	return b
}

// arm schedules the failure for the nth transaction started from now.
func (b *retryOnceBackend) arm(n int64) {
	b.seen.Store(0)
	b.fired.Store(false)
	b.failAt.Store(n)
}

func (b *retryOnceBackend) disarm() { b.failAt.Store(-1) }

func (b *retryOnceBackend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	ordinal := b.seen.Add(1) - 1
	return b.SimDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		result, err := fn(tx)
		if err != nil {
			return result, err
		}
		if target := b.failAt.Load(); target >= 0 && target == ordinal {
			// One shot: clear before returning so the retry succeeds.
			b.failAt.Store(-1)
			b.fired.Store(true)
			return nil, fdb.Error{Code: 1020} // not_committed
		}
		return result, nil
	})
}

// openRetryOnceSchema is openSimSchema over a retryOnceBackend.
func openRetryOnceSchema(t *testing.T, seed uint64, tableDDL string) (*sql.DB, *retryOnceBackend) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	backend := newRetryOnceBackend(simfdb.New(env))
	simDB := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
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
	return db, backend
}

// seedPageRetryRows fills the table and returns a connection whose per-page scan
// budget forces pagination.
func seedPageRetryRows(t *testing.T, ctx context.Context, db *sql.DB) *sql.Conn {
	t.Helper()
	for i := 0; i < pageRetryRows; i++ {
		mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, a) VALUES (%d, %d)", i, i))
	}
	return pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, pageRetryScanLimit).
			Build())
	})
}

// TestPageRetry_ContinuationAdvancesOnlyOnTransactionSuccess is the regression:
// a paged SELECT whose page transaction fails retryably after the page was
// drained must still return every row, exactly once.
//
// The transaction ordinal is swept so the failure lands on each of the query's
// pages in turn — a fixed ordinal would be a guess about how many transactions
// the connection makes before the first page, and the invariant is supposed to
// hold wherever the failure lands.
// Two query shapes, because a stale continuation costs differently in each. A
// plain scan resumes from a key position; DISTINCT resumes a STATEFUL operator
// whose continuation carries accumulated dedup state, which is where a position
// the transaction never earned does the most damage. `a` is seeded so every
// value is distinct, making the expected DISTINCT result the full key range and
// any loss immediately legible.
func TestPageRetry_ContinuationAdvancesOnlyOnTransactionSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	shapes := []struct {
		name  string
		query string
	}{
		{"scan", "SELECT id FROM t"},
		{"distinct", "SELECT DISTINCT a FROM t"},
	}

	for _, shape := range shapes {
		for n := int64(0); n < 8; n++ {
			t.Run(fmt.Sprintf("%s/fail_tx_%d", shape.name, n), func(t *testing.T) {
				t.Parallel()
				db, backend := openRetryOnceSchema(t, uint64(9100+n),
					"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
				conn := seedPageRetryRows(t, ctx, db)

				backend.arm(n)
				rows, err := conn.QueryContext(ctx, shape.query)
				if err != nil {
					t.Fatalf("%q with a retryable failure on transaction %d: %v",
						shape.query, n, err)
				}
				seen := map[int64]int{}
				for rows.Next() {
					var v int64
					if err := rows.Scan(&v); err != nil {
						t.Fatalf("scan: %v", err)
					}
					seen[v]++
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("rows: %v", err)
				}
				rows.Close()
				backend.disarm()

				if !backend.fired.Load() {
					t.Fatalf("the retryable failure armed for transaction %d never fired: this "+
						"subtest exercised nothing. %q makes fewer transactions than the "+
						"ordinal swept, so the sweep no longer covers its pages", n, shape.query)
				}

				var missing, duplicated []int64
				for i := int64(0); i < pageRetryRows; i++ {
					switch seen[i] {
					case 1:
					case 0:
						missing = append(missing, i)
					default:
						duplicated = append(duplicated, i)
					}
				}
				if len(missing) > 0 || len(duplicated) > 0 {
					t.Fatalf("%q returned %d distinct of %d rows after a retryable failure on "+
						"transaction %d (missing %v, duplicated %v). The page whose transaction "+
						"failed had already advanced paginatingRows.continuation, so the retry "+
						"resumed past it and its buffered rows were dropped",
						shape.query, len(seen), pageRetryRows, n, missing, duplicated)
				}
			})
		}
	}
}

// TestPageRetry_NoFailureIsUnchanged is the control: the same paged query with
// nothing armed. It keeps the test above honest — a harness that broke the
// query outright would otherwise be indistinguishable from one that exposed the
// defect.
func TestPageRetry_NoFailureIsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9200,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	rows, err := conn.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	n := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if n != pageRetryRows {
		t.Fatalf("unfaulted paged SELECT returned %d rows, want %d", n, pageRetryRows)
	}
	if backend.fired.Load() {
		t.Fatalf("nothing was armed, yet the backend reports a fired injection")
	}
}

// TestPageRetry_DMLIsSinglePage pins reachability property 2: DML materializes
// its whole target set, so it never paginates and its write-carrying page never
// has a continuation to advance.
//
// The observable is that a DML whose scan exceeds the per-page budget FAILS
// (54F01) rather than resuming on a second page — the signature of a
// materializing executor, not a streaming one. If DML ever gains a streaming,
// resumable executor this goes red, and that is the point: at that moment a
// write-carrying page acquires a continuation and fetchPage's staged-then-
// published position stops being latent protection and starts being the thing
// standing between a commit conflict and silently unmodified records.
func TestPageRetry_DMLIsSinglePage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := openRetryOnceSchema(t, 9300,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	// The same budget under which the SELECT above paginates cleanly.
	_, err := conn.ExecContext(ctx, "UPDATE t SET a = 7")
	if err == nil {
		t.Fatalf("a multi-row UPDATE under a %d-row per-page scan budget SUCCEEDED. DML used to "+
			"materialize its whole target set (CollectAllBounded) and fail rather than "+
			"paginate; if it now resumes across pages, a write-carrying page transaction has "+
			"a continuation and the paging loop's retry safety is load-bearing on a real "+
			"production shape", pageRetryScanLimit)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeExecutionLimitReached {
		t.Fatalf("UPDATE over budget failed with %v (%T), want 54F01 (execution limit reached): "+
			"the materialize-or-fail behaviour this property rests on has changed shape",
			err, err)
	}
}

// TestPageRetry_ExplicitTransactionPagesBypassTheRetryLoop pins reachability
// property 3: statements inside an explicit transaction do not go through the
// retrying Transact route at all, so RFC-198's write-carrying SELECT pages have
// no re-execution to inherit a stale position from.
//
// Measured directly — the backend counts Transact calls, and an explicit
// transaction's statements must add none. If a retry loop is ever put in front
// of captured transactions, this goes red, and at that moment the defect's
// window opens on the one path where pages really do carry writes.
func TestPageRetry_ExplicitTransactionPagesBypassTheRetryLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9400,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	drain := func(rows *sql.Rows, err error) int {
		t.Helper()
		if err != nil {
			t.Fatalf("paged SELECT: %v", err)
		}
		n := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		rows.Close()
		return n
	}

	// CALIBRATION. The same paged query in AUTO-COMMIT, which is the mode whose
	// pages do go through the retrying route. Its Transact count is the number
	// the in-transaction run is compared against — without it, a zero below
	// could mean "the counter counts nothing" just as easily as "the pages
	// bypassed the retry loop". (Run first so the one-shot catalog/metadata
	// bootstrap is charged here rather than to the measurement.)
	drain(conn.QueryContext(ctx, "SELECT id FROM t"))
	backend.seen.Store(0)
	if got := drain(conn.QueryContext(ctx, "SELECT id FROM t")); got != pageRetryRows {
		t.Fatalf("auto-commit paged SELECT returned %d rows, want %d", got, pageRetryRows)
	}
	autoCommitTxns := backend.seen.Load()
	// pageRetryRows/pageRetryScanLimit pages, each its own transaction.
	if wantPages := int64(pageRetryRows / pageRetryScanLimit); autoCommitTxns < wantPages {
		t.Fatalf("auto-commit paged SELECT made %d transactions through the retrying route, "+
			"want at least %d (one per page). The counter is not observing the page loop, so "+
			"the in-transaction comparison below would be vacuous", autoCommitTxns, wantPages)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	// Make the transaction carry writes, so its SELECT pages run on a
	// transaction whose commit really can fail — the shape that would be
	// dangerous if anything retried it.
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (100, 100)"); err != nil {
		t.Fatalf("in-transaction INSERT: %v", err)
	}

	// Zero the counter AFTER BeginTx and the write, so what follows measures
	// only the paged in-transaction SELECT.
	backend.seen.Store(0)

	if got := drain(tx.QueryContext(ctx, "SELECT id FROM t")); got != pageRetryRows+1 {
		t.Fatalf("in-transaction paged SELECT saw %d rows, want %d (read-your-writes across "+
			"pages)", got, pageRetryRows+1)
	}
	if got := backend.seen.Load(); got != 0 {
		t.Fatalf("a paged SELECT inside an explicit transaction made %d call(s) through the "+
			"retrying Transact route (the same query in auto-commit made %d), want 0. "+
			"Explicit-transaction statements are supposed to run directly on the captured "+
			"transaction (runInCapturedTx); routing them through a retry loop makes their "+
			"write-carrying pages re-executable, which is exactly the shape "+
			"paginatingRows.fetchPage must not be replayed under", got, autoCommitTxns)
	}
}
