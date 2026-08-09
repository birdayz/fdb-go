package sqldriver_test

// Executing the primary-key dedup that now sits inside every UPDATE plan.
//
// Java interposes a RecordQueryUnorderedPrimaryKeyDistinctPlan between the
// access path and the mutation in every UPDATE (ImplementUpdateRule.java:79-80)
// and in every DELETE whose access path does not already prove
// DistinctRecordsProperty.distinctRecords() (ImplementDeleteRule.java:79-82).
// The plan shapes are pinned without a store in
// pkg/relational/core/embedded/dml_pk_dedup_plan_test.go.
//
// What only a real store can answer is whether that NEW operator — newly
// present in the plan of every UPDATE this engine runs — changes the ANSWER.
// It sits between a scan and a mutation, keyed on the packed primary key, and
// the two ways it could be wrong are opposite and both silent: dropping a row
// that should have been mutated, or breaking the affected-row count the SQL
// layer reports from the mutation's own output cursor. A relative transform
// (v = v + 1) is used throughout because it is the only kind that makes a
// wrong visit count observable in the stored data — a constant assignment
// rewrites the same value and hides both a skip's absence and a revisit.
//
// This is NOT a Halloween-problem test and cannot be one. Go's executor
// pre-materializes the entire target set before the first write
// (executor.go executeUpdate / executeDelete), so no write is ever visible to
// the scan that selected it, and a DML statement runs in exactly one page
// (cascades_generator.go pageRowBudget returns 0 for DML) so no scan resumes
// across a commit boundary. Both of those are independent of the dedup.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_DMLPrimaryKeyDedupExecutes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dmldedup")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dmldedup")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dmldedup "+
			"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a) "+
			"CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dmldedup/s WITH TEMPLATE dmldedup")
	dsn := fmt.Sprintf("fdbsql:///testdb_dmldedup?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	reset := func() {
		mwjoMustExec(t, db, ctx, "DELETE FROM t")
		mwjoMustExec(t, db, ctx,
			"INSERT INTO t (id, a, b, v) VALUES (1, 10, 1, 100), (2, 20, 1, 200), (3, 30, 1, 300)")
	}
	scalar := func(q string) int64 {
		var out int64
		if err := db.QueryRowContext(ctx, q).Scan(&out); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return out
	}
	countRows := func(q string) int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var n int64
		for rows.Next() {
			n++
		}
		return n
	}

	// One case per access path the plan-shape test enumerates, so the dedup is
	// executed over a primary scan, a single-column index scan, a filtered scan
	// and an IN-join rather than over whichever one this schema happens to pick.
	for _, tc := range []struct {
		name    string
		where   string
		targets []int64 // ids whose v must advance by exactly 1
	}{
		{"primary_scan", "id = 1", []int64{1}},
		{"index_scan", "a > 15", []int64{2, 3}},
		{"filtered_scan", "b > 0", []int64{1, 2, 3}},
		{"in_join", "a IN (10, 20)", []int64{1, 2}},
	} {
		t.Run("update_"+tc.name, func(t *testing.T) {
			reset()
			before := map[int64]int64{}
			for _, id := range []int64{1, 2, 3} {
				before[id] = scalar(fmt.Sprintf("SELECT v FROM t WHERE id = %d", id))
			}
			res, err := db.ExecContext(ctx, "UPDATE t SET v = v + 1 WHERE "+tc.where)
			if err != nil {
				t.Fatalf("UPDATE ... WHERE %s: %v", tc.where, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				t.Fatalf("RowsAffected: %v", err)
			}
			if n != int64(len(tc.targets)) {
				t.Errorf("RowsAffected = %d, want %d — the dedup sits between the "+
					"scan and the mutation, so a row it wrongly swallowed is missing "+
					"from the mutation's output cursor too", n, len(tc.targets))
			}
			targeted := map[int64]bool{}
			for _, id := range tc.targets {
				targeted[id] = true
			}
			for _, id := range []int64{1, 2, 3} {
				want := before[id]
				if targeted[id] {
					want++
				}
				if got := scalar(fmt.Sprintf("SELECT v FROM t WHERE id = %d", id)); got != want {
					t.Errorf("id=%d v = %d, want %d (%d = the transform was applied "+
						"twice, %d = the dedup swallowed the row)",
						id, got, want, want+1, before[id])
				}
			}
		})
	}

	// The DELETE side elides the dedup over these access paths, so this is the
	// control: the same statements must still remove exactly their targets.
	t.Run("delete_removes_exactly_its_targets", func(t *testing.T) {
		reset()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE a IN (10, 20)")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected: %v", err)
		}
		if n != 2 {
			t.Errorf("DELETE RowsAffected = %d, want 2", n)
		}
		if got := countRows("SELECT id FROM t"); got != 1 {
			t.Errorf("rows left = %d, want 1", got)
		}
		if got := scalar("SELECT id FROM t"); got != 3 {
			t.Errorf("surviving id = %d, want 3", got)
		}
	})
}
