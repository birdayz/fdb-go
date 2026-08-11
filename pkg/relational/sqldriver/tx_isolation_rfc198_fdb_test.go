package sqldriver_test

// RFC-198: an explicit transaction's reads belong to that transaction.
//
// Two pins:
//
//   - The lost update becomes a 40001 (criterion 2). Before RFC-198 an in-tx
//     SELECT ran in its own auto-commit transaction, so a read-modify-write
//     across two explicit transactions lost a write with NO error — the read
//     and the write were never in the same transaction, so there was no
//     serializable schedule to violate. Routing the read into the transaction
//     adds the read conflict range that makes the commit fail loudly.
//
//   - A result set cannot outlive its transaction — all four doors (criterion
//     5). paginatingRows captures the *embeddedTx at Execute time; pages
//     fetched after the transaction ended must be 25F01, never rows from a
//     silently opened fresh transaction. The four doors are the four sites
//     that end a transaction, two of which (Close, ResetSession) bypass
//     Rollback entirely — a terminal flag set only in Commit/Rollback would
//     pass the first two doors and fail those.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func rfc198SetupDB(t *testing.T, dbPath, tmpl string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	return rfc198SetupDBOn(t, clusterFilePath, dbPath, tmpl)
}

// rfc198SetupDBOn is rfc198SetupDB against a named backend key, so a test can
// put this schema on a database whose record layer measures elapsed time on an
// injected clock.
func rfc198SetupDBOn(t *testing.T, key, dbPath, tmpl string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, key))
	if err != nil {
		t.Fatalf("sql.Open setup: %v", err)
	}
	t.Cleanup(func() { _ = setup.Close() })
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+tmpl+" CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE "+tmpl)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, key))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFDB_RFC198_LostUpdateBecomes40001 pins the exact schedule from RFC-198's
// problem statement: T1 reads, T2 commits a write to the same row, T1 writes
// and commits. T1's COMMIT must fail with SQLSTATE 40001 and T2's write must
// survive. Before RFC-198 this schedule silently lost T2's write.
//
// Mutation direction (verified at introduction): route the in-tx SELECT back
// to DB.Run → T1's read version is only taken at its UPDATE, after T2
// committed → no conflict, commit succeeds, T2's write is lost → this test
// reddens on the commit assert.
//
// NOTE (measured): forcing IsolationLevelSnapshot on the in-tx read path does
// NOT flip THIS shape green-wrong — the UPDATE's own record load reads id=1
// through the plain transaction and supplies a conflict range regardless of
// the SELECT's isolation. The snapshot-vs-serializable decision is pinned by
// TestFDB_RFC198_ReadConflictFromSelectAlone below, whose read set and write
// set are disjoint.
func TestFDB_RFC198_LostUpdateBecomes40001(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	key, clk := spikedClusterKey(t, 30*time.Second)
	db := rfc198SetupDBOn(t, key, "/testdb_rfc198_lostupd", "rfc198lu")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	// THE WHOLE INTERLEAVING IS RETRYABLE — T1's read, T2's committed write, and
	// T1's own write-then-commit. It has to be all three: the conflict this test
	// asserts exists only because T2 committed BETWEEN T1's read and T1's commit,
	// so an attempt that restarted at T1's UPDATE would be commiting a
	// transaction whose read version postdates T2 and would not conflict at all.
	// Re-running the body re-establishes the interleaving inside the new
	// transaction.
	//
	// AND THE COMMIT ERROR IS CLASSIFIED, not merely code-checked. That closes a
	// vacuity this test had before conversion: FDB's transaction_too_old also
	// maps to 40001, so a T1 whose MVCC window simply expired produced exactly
	// the SQLSTATE the assertion demanded and the test passed while proving
	// nothing about read conflict ranges. Now a lost window is returned for the
	// retry and only a genuine conflict — 40001 with NO time-limit marker —
	// satisfies the assertion.
	var commitErr error
	var attemptsRun int
	retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
		// T1 reads the row. This is the read that must contribute a read conflict
		// range — and it is the FIRST read, so it is also what takes T1's read
		// version, BEFORE T2 commits.
		var v int64
		if err := a.tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			return err
		}
		if v != 100 {
			t.Fatalf("T1 read v=%d, want 100", v)
		}

		// T2 (a separate connection, auto-commit) updates the same row and
		// commits. Idempotent, so re-running it on a later attempt is harmless.
		if _, err := db.ExecContext(ctx, "UPDATE t SET v = 500 WHERE id = 1"); err != nil {
			return err
		}

		// T1 writes the row (its scan still reads at its own read version) and
		// commits. The commit must fail with 40001 — this is the serialization
		// the connection promises (BeginTx accepts LevelSerializable).
		if _, err := a.tx.ExecContext(ctx, "UPDATE t SET v = 101 WHERE id = 1"); err != nil {
			return err
		}
		commitErr = a.tx.Commit()
		if api.IsTransactionTimeLimit(commitErr) {
			// T1's window expired rather than conflicting. Retry the whole
			// interleaving; asserting on this would be asserting a conflict that
			// was never detected.
			return commitErr
		}
		return nil
	})
	mustHaveRetried(t, attemptsRun)

	if commitErr == nil {
		t.Fatalf("T1 COMMIT succeeded after T2 committed a conflicting write: " +
			"the lost update shipped silently — the in-tx SELECT is not adding a " +
			"read conflict range (RFC-198 Decision 1/2)")
	}
	var apiErr *api.Error
	if !errors.As(commitErr, &apiErr) {
		t.Fatalf("T1 COMMIT failed with %v (%T), which is not an *api.Error: "+
			"embeddedTx.Commit is bypassing translateFDBError (RFC-198 Decision 7)", commitErr, commitErr)
	}
	if apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("T1 COMMIT failed with SQLSTATE %s, want %s (40001): %v",
			apiErr.Code, api.ErrCodeSerializationFailure, commitErr)
	}
	if api.IsTransactionTimeLimit(commitErr) {
		t.Fatalf("T1 COMMIT failed 40001 because its MVCC WINDOW EXPIRED, not because "+
			"of a conflict: %v.\nThe two share a SQLSTATE, so a code-only assertion "+
			"passes here with read conflict ranges completely absent. Only a 40001 "+
			"WITHOUT the time-limit marker is evidence of the serialization this test "+
			"pins.", commitErr)
	}

	// T2's write survives.
	var finalV int64
	if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&finalV); err != nil {
		t.Fatalf("final read: %v", err)
	}
	if finalV != 500 {
		t.Fatalf("v=%d after the conflicted commit, want 500 (T2's committed write must survive)", finalV)
	}
}

// TestFDB_RFC198_ReadConflictFromSelectAlone pins that the in-tx SELECT
// ITSELF contributes a read conflict range (RFC-198 Decision 2, SERIALIZABLE
// not snapshot). The read set and the write set are disjoint — T1 reads row 1
// and writes row 2, T2 writes row 1 — so no DML-internal record load can
// supply the conflict range on row 1; only the SELECT's read can. Under
// serializable in-tx reads T1's commit must fail 40001 (write skew is
// forbidden); under snapshot reads it would succeed with both writes in place,
// which is the dangerous half of the defect shipping under a serializable
// label.
//
// Mutation direction (verified at introduction): force IsolationLevelSnapshot
// on the in-tx read path → read-your-writes still works (the probe test stays
// green) but this test reddens on the commit assert. That asymmetry is what
// makes snapshot-vs-serializable a TESTED decision rather than a prose one.
func TestFDB_RFC198_ReadConflictFromSelectAlone(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	key, clk := spikedClusterKey(t, 30*time.Second)
	db := rfc198SetupDBOn(t, key, "/testdb_rfc198_skew", "rfc198skew")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (2, 200)")

	// Retried as ONE interleaving, for the same reason as the lost-update shape
	// above and with the same extra classification on the commit error: a T1
	// whose MVCC window expired reports the identical SQLSTATE as the write skew
	// this test is here to forbid, so without the marker check the assertion
	// could be satisfied by a transaction that never contributed a read conflict
	// range at all.
	var commitErr error
	var attemptsRun int
	retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
		// T1 reads row 1 — the ONLY touch of row 1 in this transaction.
		var v int64
		if err := a.tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			return err
		}
		if v != 100 {
			t.Fatalf("T1 read v=%d, want 100", v)
		}

		// T2 writes row 1 and commits.
		if _, err := db.ExecContext(ctx, "UPDATE t SET v = 500 WHERE id = 1"); err != nil {
			return err
		}

		// T1 writes row 2 (never row 1) and commits: serializable in-tx reads
		// make this fail 40001; snapshot reads would let it commit.
		if _, err := a.tx.ExecContext(ctx, "UPDATE t SET v = 201 WHERE id = 2"); err != nil {
			return err
		}
		commitErr = a.tx.Commit()
		if api.IsTransactionTimeLimit(commitErr) {
			return commitErr
		}
		return nil
	})
	mustHaveRetried(t, attemptsRun)

	if commitErr == nil {
		t.Fatalf("T1 COMMIT succeeded: the in-tx SELECT of row 1 added no read conflict " +
			"range — in-tx reads are snapshot, not the SERIALIZABLE the connection " +
			"promises (RFC-198 Decision 2)")
	}
	var apiErr *api.Error
	if !errors.As(commitErr, &apiErr) {
		t.Fatalf("T1 COMMIT failed with %v (%T), want *api.Error 40001", commitErr, commitErr)
	}
	if api.IsTransactionTimeLimit(commitErr) {
		t.Fatalf("T1 COMMIT failed 40001 because its MVCC WINDOW EXPIRED, not because of "+
			"write skew: %v. A code-only assertion is satisfied by that, with the read "+
			"conflict range this test exists to prove entirely absent.", commitErr)
	}
	if apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("T1 COMMIT: SQLSTATE %s, want %s (40001): %v",
			apiErr.Code, api.ErrCodeSerializationFailure, commitErr)
	}
}

// TestFDB_RFC198_ResultSetDiesWithItsTransaction pins criterion 5: a
// multi-page in-tx SELECT whose transaction ends mid-iteration must fail the
// next page with 25F01 — one subtest per door. It drives the driver interfaces
// directly (via Raw) because two of the doors (raw COMMIT via ExecContext,
// ResetSession) mutate activeTx behind database/sql's back, and database/sql's
// own Tx.Commit force-closes the Rows first, which would mask exactly the
// hazard under test.
//
// Mutation direction: restoring runInCapturedTx's silent nil-fallback for a
// terminated captured transaction turns every subtest here into rows served
// from a fresh auto-commit transaction — all four redden. A terminal flag set
// only in Commit/Rollback keeps raw_commit/raw_rollback green and reddens
// close/reset_session.
func TestFDB_RFC198_ResultSetDiesWithItsTransaction(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := rfc198SetupDB(t, "/testdb_rfc198_doors", "rfc198doors")
	const rows = 20
	for i := 0; i < rows; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i))
	}

	doors := []struct {
		name string
		kill func(t *testing.T, ec *embedded.EmbeddedConnection)
	}{
		{"raw_commit", func(t *testing.T, ec *embedded.EmbeddedConnection) {
			if _, err := ec.ExecContext(ctx, "COMMIT", nil); err != nil {
				t.Fatalf("raw COMMIT: %v", err)
			}
		}},
		{"raw_rollback", func(t *testing.T, ec *embedded.EmbeddedConnection) {
			if _, err := ec.ExecContext(ctx, "ROLLBACK", nil); err != nil {
				t.Fatalf("raw ROLLBACK: %v", err)
			}
		}},
		{"close", func(t *testing.T, ec *embedded.EmbeddedConnection) {
			if err := ec.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}},
		{"reset_session", func(t *testing.T, ec *embedded.EmbeddedConnection) {
			if err := ec.ResetSession(ctx); err != nil {
				t.Fatalf("ResetSession: %v", err)
			}
		}},
	}

	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("pin conn: %v", err)
			}
			defer conn.Close() //nolint:errcheck
			rawErr := conn.Raw(func(driverConn any) error {
				ec, ok := driverConn.(*embedded.EmbeddedConnection)
				if !ok {
					t.Fatalf("driver conn is %T, want *embedded.EmbeddedConnection", driverConn)
				}
				// Force multi-page pagination so iteration outlives the first
				// (eagerly fetched) page.
				ec.SetOptions(api.NewOptionsBuilder().
					Set(api.OptExecutionScannedRowsLimit, 2).Build())

				tx, err := ec.Begin()
				if err != nil {
					t.Fatalf("driver Begin: %v", err)
				}
				_ = tx // ended through the door, not through driver.Tx
				dRows, err := ec.QueryContext(ctx, "SELECT id FROM t", nil)
				if err != nil {
					t.Fatalf("in-tx query: %v", err)
				}
				defer dRows.Close() //nolint:errcheck

				dest := make([]driver.Value, 1)
				if err := dRows.Next(dest); err != nil {
					t.Fatalf("first row: %v", err)
				}

				door.kill(t, ec)

				// Drain the rest. The already-buffered page may still yield
				// rows; the next page FETCH must fail 25F01 — and must never
				// silently complete (io.EOF before an error would mean the
				// remaining pages were served from a fresh transaction).
				got := 1
				var iterErr error
				for {
					iterErr = dRows.Next(dest)
					if iterErr != nil {
						break
					}
					got++
				}
				if iterErr == io.EOF {
					t.Fatalf("door %q: iteration completed cleanly with %d/%d rows — the pages "+
						"after the transaction ended were served from a fresh auto-commit "+
						"transaction (the silent nil-fallback RFC-198 Decision 3 removes)",
						door.name, got, rows)
				}
				var apiErr *api.Error
				if !errors.As(iterErr, &apiErr) {
					t.Fatalf("door %q: page fetch after the transaction ended failed with %v (%T), "+
						"want *api.Error 25F01", door.name, iterErr, iterErr)
				}
				if apiErr.Code != api.ErrCodeTransactionInactive {
					t.Fatalf("door %q: page fetch after the transaction ended: SQLSTATE %s, want %s (25F01): %v",
						door.name, apiErr.Code, api.ErrCodeTransactionInactive, iterErr)
				}
				return nil
			})
			if rawErr != nil && door.name != "close" {
				t.Fatalf("Raw: %v", rawErr)
			}
		})
	}
}
