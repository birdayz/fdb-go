package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_IntersectionOrderingGate pins RFC-181 P0.1: the PK-intersection
// executor is a strict PK-sorted merge, but nothing gated leg ORDER — an
// INEQUALITY-bound index leg emits (a, pk) order, whose pk sequence
// interleaves, and the merge silently DROPPED intersection rows. Java gates
// every intersection on comparison-key compatibility per leg
// (WithPrimaryKeyDataAccessRule intersectOrderings +
// isCompatibleComparisonKey); Go must decline the non-PK-monotone
// composition (falling back to a filtered single-index plan — correct rows)
// and still allow the all-equality-bound composition.
func TestFDB_IntersectionOrderingGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ixgate")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ixgate")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ixgate CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t (a) CREATE INDEX idx_b ON t (b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ixgate/s WITH TEMPLATE ixgate")
	dsn := fmt.Sprintf("fdbsql:///testdb_ixgate?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// PK order and a-order INTERLEAVE (a = 1000 - id*7 % 997): the a-index
	// leg's pk sequence is non-monotone under the pk-keyed merge. Each
	// predicate alone matches many rows; the conjunction matches few — the
	// shape where an index intersection should out-cost a single leg.
	var sb strings.Builder
	sb.WriteString("INSERT INTO t (id, a, b) VALUES ")
	for i := 1; i <= 200; i++ {
		if i > 1 {
			sb.WriteString(", ")
		}
		b := 7
		if i%13 == 0 {
			b = 3 // 15 rows with b=3, scattered
		}
		fmt.Fprintf(&sb, "(%d, %d, %d)", i, 1000-(i*7)%997, b)
	}
	mwjoMustExec(t, db, ctx, sb.String())

	collect := func(q string) map[int64]bool {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		got := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[id] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return got
	}

	t.Run("range_plus_equality_returns_all_rows", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM t WHERE a > 5 AND b = 3").Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		t.Logf("plan: %s", plan)
		got := collect("SELECT id FROM t WHERE a > 5 AND b = 3")
		want := 0
		for i := 1; i <= 200; i++ {
			if i%13 == 0 && 1000-(i*7)%997 > 5 {
				want++
				if !got[int64(i)] {
					t.Fatalf("row id=%d MISSING (%d/%d found) — a non-PK-monotone leg reached the pk-sorted intersection merge and dropped rows (plan: %s)", i, len(got), want, plan)
				}
			}
		}
		if len(got) != want {
			t.Fatalf("got %d rows, want %d (plan: %s)", len(got), want, plan)
		}
	})
}
