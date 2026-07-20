package sqldriver_test

// REGRESSION for a fixed executor/ordinal-binding defect, surfaced by the
// RFC-182 rowdiff harness when LEFT OUTER JOIN generation was added (rowdiff
// seed 960000225).
//
// A LEFT OUTER JOIN on the PRIMARY KEY (`l.id = r.id`), combined with a WHERE
// filter that lowers to an IN-join (`r.s IN (…)` — an L-side `l.b IN (…)`
// triggers the same shape, seed 980000056) AND an ORDER BY on the left key,
// USED TO fail at execution with "multi-leg row cannot serve a source-relative
// ordinal — no frontier row resolved".
//
// Root cause: the plan is `InMemorySort(InJoin(PredicatesFilter(FlatMap(join))))`.
// The in-memory sort resolves a source-relative ORDER-BY key (`l.id`, addressed
// to L's own leg window) over the join's MERGED multi-leg row via leg windows —
// but `downstreamLegWindows`/`unwrapToJoinPlan` did not treat an InJoin as a
// row-layout-preserving passthrough, so it under-provided the leg windows and
// the correlated fall-through guard went LOUD. An InJoin re-emits the inner
// join's merged rows verbatim under a per-in-value binding (the binding changes
// row count/content, never the row LAYOUT), so it IS a positional passthrough;
// adding it to unwrapToJoinPlan restores the leg windows. Removing ANY one
// factor already worked (non-PK join key / no WHERE filter / no ORDER BY).
//
// This regression pins the fixed behavior: the query now returns the correct
// rows (l.id = r.id is a self-match, so no row is NULL-extended; the WHERE keeps
// only s ∈ {delta,beta} → ids 1 and 2; ORDER BY l.id DESC → (2,2) then (1,1)).
import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_LeftJoinPkOrdinal_InJoinSortRegression(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ljpk")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ljpk")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ljpk "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id)) "+
		"CREATE INDEX idx_b ON t (b) "+
		"CREATE INDEX idx_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ljpk/s WITH TEMPLATE ljpk")
	dsn := fmt.Sprintf("fdbsql:///testdb_ljpk?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b,c,s,f) VALUES "+
		"(1,2,8,3,'beta',TRUE),(2,3,5,9,'delta',FALSE),(3,7,8,1,'alpha',TRUE)")

	// PK-join LEFT OUTER + R-side IN filter (lowers to an InJoin) + ORDER BY on
	// the left key — the exact shape the InJoin leg-window passthrough fixed.
	const q = "SELECT l.id AS l_id, r.id AS r_id FROM t AS l LEFT JOIN t AS r ON l.id = r.id " +
		"WHERE r.s IN ('delta','beta') ORDER BY l.id DESC, r.id"

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query failed (the InJoin leg-window regression is back): %v", err)
	}
	defer rows.Close()

	type pair struct{ l, r int64 }
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.l, &p.r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}

	want := []pair{{2, 2}, {1, 1}}
	if len(got) != len(want) {
		t.Fatalf("row count: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}
