package sqldriver_test

// REGRESSION for a FIXED 3-way-join ordinal-resolution defect, surfaced by the
// RFC-182 rowdiff harness (seed 1001000229) once 3-way self-joins were
// generated at scale.
//
// A 3-WAY join in which the SAME column (here c) is referenced across all three
// legs — an equality filter on the first (`l.c = 1`), the join key between the
// second and third (`m.c = r.c`), and a filter on the third (`r.c IS NULL`) —
// USED TO fail at execution with "field \"C\" not resolvable in the runtime row
// (ordinal -1 …) — malformed plan".
//
// Root cause: the join key `m.c = r.c`, when its `r.c` side is pushed below the
// index Fetch (PushFilterThroughFetchRule), was translated to the index-scan
// domain by ValueIndexScanMatchCandidate.buildTranslateValueFunction, which
// DROPPED the reference's baked ordinal (a bare NewFieldValue → LAZY). When that
// pushed predicate is evaluated as a residual (the equality lost the index bound
// to a competing `r.c IS NULL`), the executor has no name→ordinal fallback and
// fails loud. Fix: the translate function preserves the incoming baked ordinal
// (the fetch's inner presents a logical-slot-shaped partial record, so the
// full-record ordinal reads the same slot; correct-or-loud otherwise).
//
// Same ordinal-binding family as the multi-leg gap (also FIXED —
// TestFDB_LeftJoinPkOrdinal_InJoinSortRegression) but a distinct root cause.
//
// This pins the fixed behavior: the query executes and returns ZERO rows
// (m.c = r.c is UNKNOWN, never TRUE, whenever r.c IS NULL).
import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ThreeWaySharedColOrdinal_Regression(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_3wc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_3wc")
	// The full column set matters — the error names the runtime row's columns
	// [ID A B C S F]; a narrower table does not reproduce the malformed plan.
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE t3wc "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id)) "+
		"CREATE INDEX idx_c ON t (c) CREATE INDEX idx_a ON t (a) CREATE INDEX idx_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3wc/s WITH TEMPLATE t3wc")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_3wc?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,c) VALUES (1,2,1),(2,3,7),(3,5,NULL),(4,2,1),(5,9,NULL)")

	const q = "SELECT l.id AS lid, m.id AS mid, r.id AS rid FROM t AS l, t AS m, t AS r " +
		"WHERE l.a = m.id AND m.c = r.c AND l.c = 1 AND r.c IS NULL"

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("3-way shared-column query failed (the ordinal-binding regression is "+
			"back): %v\nQuery: %s", err, q)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Correct result is EMPTY: `m.c = r.c` is UNKNOWN (never TRUE) whenever
	// `r.c IS NULL`, so the two conjuncts are jointly unsatisfiable.
	if n != 0 {
		t.Fatalf("3-way shared-column query returned %d rows, want 0 (m.c=r.c is "+
			"unsatisfiable under r.c IS NULL). Query: %s", n, q)
	}
}
