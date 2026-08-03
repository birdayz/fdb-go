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
)

// rfc198IndexedDB creates a table with a secondary index on v, so a predicate
// on v has an index access path and a predicate on id has the primary-key one.
func rfc198IndexedDB(t *testing.T, dbPath, tmpl string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+tmpl+
			" CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
			" CREATE INDEX idx_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE "+tmpl)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
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
func explainInTx(t *testing.T, ctx context.Context, tx *sql.Tx, query string) string {
	t.Helper()
	var plan string
	if err := tx.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", query, err)
	}
	return plan
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

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (2, 777)"); err != nil {
		t.Fatalf("in-tx INSERT: %v", err)
	}

	const q = "SELECT id FROM t WHERE v = 777"
	mustUseIndexScan(t, explainInTx(t, ctx, tx, q), q)

	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("in-tx SELECT: %v", err)
	}
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	rows.Close()
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("in-tx index scan on v=777 returned ids %v, want [2]: the index "+
			"entry the uncommitted INSERT wrote is not visible to the "+
			"transaction's own read — the index scan is not running on the "+
			"transaction (RFC-198 criterion 3)", got)
	}

	// The row is still uncommitted: a separate connection must not see it.
	var outside int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE v = 777").Scan(&outside); err != nil {
		t.Fatalf("outside count: %v", err)
	}
	if outside != 0 {
		t.Fatalf("a separate connection already sees %d rows with v=777: the in-tx "+
			"INSERT is not isolated, which would make the RYW assert above "+
			"vacuous", outside)
	}
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
	db := rfc198IndexedDB(t, "/testdb_rfc198_rywdel", "rfc198rywdel")
	for _, v := range []struct{ id, v int64 }{{1, 100}, {2, 100}, {3, 100}, {4, 900}} {
		mwjoMustExec(t, db, ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", v.id, v.v))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Clear a RANGE of primary keys: ids 1..3, all with v=100. Their index
	// entries under idx_v go with them.
	res, err := tx.ExecContext(ctx, "DELETE FROM t WHERE id >= 1 AND id <= 3")
	if err != nil {
		t.Fatalf("in-tx DELETE: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 3 {
		t.Fatalf("in-tx DELETE affected %d rows (err %v), want 3", n, err)
	}

	// idsInTx runs query inside the transaction and returns the ids it yields.
	// The EXPLAINed text and the executed text are the SAME string in every
	// arm below: EXPLAINing one query and running another would assert the
	// plan shape of a statement nobody executed.
	idsInTx := func(t *testing.T, query string) []int64 {
		t.Helper()
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("in-tx %q: %v", query, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err on %q: %v", query, err)
		}
		return ids
	}

	t.Run("index_scan_over_cleared_range", func(t *testing.T) {
		const q = "SELECT id FROM t WHERE v = 100"
		mustUseIndexScan(t, explainInTx(t, ctx, tx, q), q)
		if ids := idsInTx(t, q); len(ids) != 0 {
			t.Fatalf("in-tx index scan on v=100 returned ids %v, want none: the "+
				"transaction does not see its own index-entry clears "+
				"(RFC-198 criterion 4)", ids)
		}
	})

	t.Run("record_scan_over_cleared_range", func(t *testing.T) {
		const q = "SELECT id FROM t WHERE id >= 1 AND id <= 3"
		mustNotUseIndexScan(t, explainInTx(t, ctx, tx, q), q)
		if ids := idsInTx(t, q); len(ids) != 0 {
			t.Fatalf("in-tx record scan over the cleared key range returned ids "+
				"%v, want none: the transaction does not see its own record "+
				"clears (RFC-198 criterion 4)", ids)
		}
		// The row OUTSIDE the cleared range is untouched — without this the
		// two asserts above would also pass on a store that lost everything.
		if ids := idsInTx(t, "SELECT id FROM t"); len(ids) != 1 || ids[0] != 4 {
			t.Fatalf("in-tx full scan returned ids %v, want [4] (the row outside "+
				"the cleared range survives)", ids)
		}
	})

	// Still uncommitted: a separate connection sees all four rows.
	var outside int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&outside); err != nil {
		t.Fatalf("outside count: %v", err)
	}
	if outside != 4 {
		t.Fatalf("a separate connection sees %d rows, want 4: the in-tx DELETE "+
			"is not isolated, which would make the asserts above vacuous", outside)
	}
}
