package sqldriver_test

// RFC-198 criteria 3 and 4: read-your-writes reaches the INDEX subspace and
// survives a CLEARED range — not just the record subspace under a full scan.
//
// Criterion 1's probe (an in-tx INSERT read back by an in-tx SELECT) is
// satisfied by a full record scan, so it says nothing about the index. An
// uncommitted INSERT writes index entries into the same transaction's write
// buffer; an index scan that read outside the transaction would miss them and
// return zero rows while the record-scan probe stayed green. That is why the
// PLAN SHAPE is part of the assertion here: a query that silently fell back to
// a full scan would pass criterion 3 while the index RYW path was broken.
//
// Criterion 4 is the mirror image: a DELETE clears keys, and a read that does
// not see the transaction's own clears returns rows that no longer exist for
// this transaction. Both access paths are asserted, because the record clear
// and the index-entry clear are separate maintenance writes.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rfc198IndexedDB creates a table with a secondary index on v, so a predicate
// on v has an index access path and a predicate on id has the primary-key one.
func rfc198IndexedDB(t *testing.T, dbPath, tmpl string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	return rfc198IndexedDBOn(t, clusterFilePath, dbPath, tmpl)
}

// rfc198IndexedDBOn is rfc198IndexedDB against a named backend key, so a test
// can put the SAME schema on a database whose record layer measures elapsed
// time on an injected clock. Every handle in a test must come from one key or
// the isolation assertions would be comparing two unrelated stores.
func rfc198IndexedDBOn(t *testing.T, key, dbPath, tmpl string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, key))
	if err != nil {
		t.Fatalf("sql.Open setup: %v", err)
	}
	t.Cleanup(func() { _ = setup.Close() })
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

// explainInTx returns the EXPLAIN text for query, executed INSIDE tx so the
// plan is the one that transaction's statements actually get (the catalog is
// transaction-bound — RFC-198 Decision 8b).
//
// It RETURNS its driver error rather than fatalling, and that is what makes it
// usable from inside a retried transaction body. An EXPLAIN is a read page like
// any other, so it is a place the whole-transaction budget pre-emption can
// arrive; a helper that fatalled there would end the test before the retry loop
// ever saw the error, which is precisely the assumption being closed. The
// caller keeps fatalling on a WRONG PLAN — that is a verdict, not a transient.
func explainInTx(ctx context.Context, tx *sql.Tx, query string) (string, error) {
	var plan string
	if err := tx.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		return "", fmt.Errorf("EXPLAIN %q: %w", query, err)
	}
	return plan, nil
}

func mustUseIndexScan(t *testing.T, plan, query string) {
	t.Helper()
	if !strings.Contains(strings.ToUpper(plan), "IDX_V") {
		t.Fatalf("query %q planned to %q, which does not scan idx_v: the RYW "+
			"assertion below would then be a test of the RECORD subspace and "+
			"would stay green with index read-your-writes fully broken "+
			"(RFC-198 criterion 3)", query, plan)
	}
}

func mustNotUseIndexScan(t *testing.T, plan, query string) {
	t.Helper()
	if strings.Contains(strings.ToUpper(plan), "IDX_V") {
		t.Fatalf("query %q planned to %q, which scans idx_v: this arm exists to "+
			"cover the RECORD access path, and covering the index path twice "+
			"leaves the record clear unprobed (RFC-198 criterion 4)", query, plan)
	}
}

// TestFDB_RFC198_ReadYourWritesThroughIndex pins criterion 3: a row INSERTed
// inside an explicit transaction is visible to a later in-tx SELECT that reads
// it THROUGH THE INDEX, before any commit.
//
// Mutation direction (verified at introduction): route the in-tx SELECT back
// to a fresh auto-commit transaction (drop r.tx in paginatingRows / restore
// the DB.Run path) → the index scan reads at a read version that predates the
// uncommitted INSERT and returns zero rows → this test reddens on the row
// assert. The EXPLAIN assert is the second, independent direction: a planner
// change that stops choosing idx_v reddens the plan assert rather than
// silently converting this into a duplicate of the record-scan probe.
func TestFDB_RFC198_ReadYourWritesThroughIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := rfc198IndexedDB(t, "/testdb_rfc198_rywidx", "rfc198rywidx")
	// A committed neighbour with a DIFFERENT v, so the index has pre-existing
	// entries and an empty-index artifact cannot be mistaken for a pass.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	// THE WHOLE TRANSACTION IS RETRYABLE, and the retry is not defensive
	// padding — it is the contract this shape has to honour.
	//
	// An explicit transaction pins one FDB read version, and that version dies
	// five seconds after it was obtained in WALL CLOCK, whether the client was
	// working or merely queued. The driver pre-empts at four with a typed 40001
	// (RFC-198), so a body issuing three statements is asserting that all three
	// finish inside four seconds — an assumption this suite can break on its own:
	// MEASURED at 5.34s for this test in a full-parallel run against 0.08s
	// standalone, with a reported failure at `read version 4.321s old, budget 4s`.
	//
	// Retrying the WHOLE transaction is the only correct response and the only
	// effective one: a fresh transaction takes a fresh read version, and because
	// the body re-runs in full the uncommitted INSERT is re-established INSIDE
	// the new transaction. Nothing is carried across the boundary, so the
	// read-your-writes property below is proven in whichever transaction ends up
	// asserting it, never smuggled from a dead one. Only the time-limit marker is
	// retried — a genuine write conflict shares the SQLSTATE and must surface.
	//
	// The retry's own mechanics are pinned in tx_budget_retry_fdb_test.go, which
	// forces the pre-emption deterministically with an injected clock rather than
	// waiting for a loaded machine to supply one.
	runInTxWithRetry(t, db, 3, nil, func(a txAttempt) error {
		if _, err := a.tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (2, 777)"); err != nil {
			return err
		}

		const q = "SELECT id FROM t WHERE v = 777"
		plan, err := explainInTx(ctx, a.tx, q)
		if err != nil {
			return err
		}
		mustUseIndexScan(t, plan, q)

		rows, err := a.tx.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(got) != 1 || got[0] != 2 {
			t.Fatalf("in-tx index scan on v=777 returned ids %v, want [2]: the index "+
				"entry the uncommitted INSERT wrote is not visible to the "+
				"transaction's own read — the index scan is not running on the "+
				"transaction (RFC-198 criterion 3)", got)
		}

		// The row is still uncommitted: a separate connection must not see it.
		// Asserted INSIDE the body, while this transaction is still open — that
		// is the only window in which the claim means anything.
		var outside int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE v = 777").Scan(&outside); err != nil {
			return err
		}
		if outside != 0 {
			t.Fatalf("a separate connection already sees %d rows with v=777: the in-tx "+
				"INSERT is not isolated, which would make the RYW assert above "+
				"vacuous", outside)
		}
		return nil
	})
}

// TestFDB_RFC198_ReadYourWritesOverClearedRange pins criterion 4: rows DELETEd
// inside an explicit transaction are gone for that transaction's own later
// reads — through BOTH access paths, because the record clear and the
// index-entry clear are separate writes and a read that missed either would
// resurrect a deleted row.
//
// Mutation direction (verified at introduction): route the in-tx SELECT back
// to a fresh auto-commit transaction → both arms read at a version that
// predates the DELETE and return the deleted rows → both subtests redden. The
// plan asserts are the second direction: they keep the two arms on the two
// different access paths, so a planner change that collapsed them into one
// reddens loudly instead of quietly halving the coverage.
func TestFDB_RFC198_ReadYourWritesOverClearedRange(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	// A one-shot clock spike, so the retry below is not a guard against a
	// condition nobody ever produces: attempt 1 is pre-empted on its first
	// in-transaction READ page, the retry ends the spike, attempt 2 runs clean.
	// The arm that decides whether the conversion is correct therefore executes
	// on EVERY run rather than only on a loaded machine.
	//
	// Arming it for the whole test is safe because preflightTxBudget runs under
	// `if r.tx != nil`: the seed INSERTs below are autocommit and never meet it.
	key, clk := spikedClusterKey(t, 30*time.Second)
	db := rfc198IndexedDBOn(t, key, "/testdb_rfc198_rywdel", "rfc198rywdel")
	for _, v := range []struct{ id, v int64 }{{1, 100}, {2, 100}, {3, 100}, {4, 900}} {
		mwjoMustExec(t, db, ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", v.id, v.v))
	}

	// EVERYTHING THE TRANSACTION SAW, captured so the assertions can run ONCE.
	//
	// Two things forced this shape, and both are properties of the retry rather
	// than of the test. First, subtests cannot live inside the body: a second
	// attempt would re-register `index_scan_over_cleared_range` under a name
	// already taken, and Go renames the duplicate rather than failing, so the
	// suite would report subtests that silently describe a discarded attempt.
	// Second, a helper that fatals inside the body ends the test before the
	// retry loop can classify the error. So the body only READS and returns
	// driver errors, and every verdict is passed on the captured values below,
	// after an attempt has succeeded in full.
	type observed struct {
		deletedRows           int64
		indexPlan, recordPlan string
		indexIDs, recordIDs   []int64
		survivingIDs          []int64
		outsideCount          int64
	}
	var obs observed

	// idsInTx runs query inside the transaction and returns the ids it yields.
	// The EXPLAINed text and the executed text are the SAME string in every arm
	// below: EXPLAINing one query and running another would assert the plan
	// shape of a statement nobody executed.
	idsInTx := func(tx *sql.Tx, query string) ([]int64, error) {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("in-tx %q: %w", query, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan %q: %w", query, err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("rows.Err on %q: %w", query, err)
		}
		return ids, nil
	}

	var attemptsRun int
	retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
		// Reset per attempt: a partially-filled observed from a discarded
		// attempt must never reach the assertions.
		obs = observed{}

		// Clear a RANGE of primary keys: ids 1..3, all with v=100. Their index
		// entries under idx_v go with them. Re-done inside EVERY attempt, which
		// is the point of restarting the whole transaction — the clears the
		// reads below must see belong to the transaction doing the reading.
		res, err := a.tx.ExecContext(ctx, "DELETE FROM t WHERE id >= 1 AND id <= 3")
		if err != nil {
			return err
		}
		if obs.deletedRows, err = res.RowsAffected(); err != nil {
			return err
		}

		const indexQ = "SELECT id FROM t WHERE v = 100"
		if obs.indexPlan, err = explainInTx(ctx, a.tx, indexQ); err != nil {
			return err
		}
		if obs.indexIDs, err = idsInTx(a.tx, indexQ); err != nil {
			return err
		}

		const recordQ = "SELECT id FROM t WHERE id >= 1 AND id <= 3"
		if obs.recordPlan, err = explainInTx(ctx, a.tx, recordQ); err != nil {
			return err
		}
		if obs.recordIDs, err = idsInTx(a.tx, recordQ); err != nil {
			return err
		}
		if obs.survivingIDs, err = idsInTx(a.tx, "SELECT id FROM t"); err != nil {
			return err
		}

		// Read from OUTSIDE while this transaction is still open — the only
		// window in which the isolation claim means anything.
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&obs.outsideCount); err != nil {
			return err
		}
		return nil
	})
	mustHaveRetried(t, attemptsRun)

	if obs.deletedRows != 3 {
		t.Fatalf("in-tx DELETE affected %d rows, want 3", obs.deletedRows)
	}

	t.Run("index_scan_over_cleared_range", func(t *testing.T) {
		mustUseIndexScan(t, obs.indexPlan, "SELECT id FROM t WHERE v = 100")
		if len(obs.indexIDs) != 0 {
			t.Fatalf("in-tx index scan on v=100 returned ids %v, want none: the "+
				"transaction does not see its own index-entry clears "+
				"(RFC-198 criterion 4)", obs.indexIDs)
		}
	})

	t.Run("record_scan_over_cleared_range", func(t *testing.T) {
		mustNotUseIndexScan(t, obs.recordPlan, "SELECT id FROM t WHERE id >= 1 AND id <= 3")
		if len(obs.recordIDs) != 0 {
			t.Fatalf("in-tx record scan over the cleared key range returned ids "+
				"%v, want none: the transaction does not see its own record "+
				"clears (RFC-198 criterion 4)", obs.recordIDs)
		}
		// The row OUTSIDE the cleared range is untouched — without this the
		// two asserts above would also pass on a store that lost everything.
		if len(obs.survivingIDs) != 1 || obs.survivingIDs[0] != 4 {
			t.Fatalf("in-tx full scan returned ids %v, want [4] (the row outside "+
				"the cleared range survives)", obs.survivingIDs)
		}
	})

	// Still uncommitted: a separate connection saw all four rows.
	if obs.outsideCount != 4 {
		t.Fatalf("a separate connection sees %d rows, want 4: the in-tx DELETE "+
			"is not isolated, which would make the asserts above vacuous", obs.outsideCount)
	}
}
