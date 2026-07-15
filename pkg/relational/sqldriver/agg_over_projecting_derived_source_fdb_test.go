package sqldriver_test

// Regression pin for GROUP BY over a derived source that re-lays-out a
// MULTI-SOURCE base (group-key / operand read over a derived table / CTE body).
// Java-parity: Java plans GROUP BY over a multi-source-FROM derived table
// (GroupByQueryTests.java:699,
//
//	SELECT max(y) FROM (SELECT y, b AS L FROM t1,t2) AS q GROUP BY l ).
//
// The derived source's projection re-lays-out the t1×t2 join merge into a flat
// positional row [Y, L]; the streaming aggregate's group key `l` and operand `y`
// name PROJECTED OUTPUT columns, so they resolve against that projection row by
// ordinal-in-row — exactly as executeProjection resolves the projection itself.
// The derived projection's flat positional row [Y, L] is the only source, and the
// group key `l` / operand `y` resolve against it by ordinal. This test proves the
// ordinal path end-to-end by asserting the exact rows. The schema is unique to
// this test, so no cached plan is shared.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_AggregateOverProjectingDerivedSource(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggproj_ord")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggproj_ord")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggproj_ord "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggproj_ord/s WITH TEMPLATE aggproj_ord")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggproj_ord?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1.y ∈ {10,20}; t2.b ∈ {100,200}. The t1×t2 join is 4 rows:
	// (y,b) ∈ {(10,100),(10,200),(20,100),(20,200)}. Projected to (y, L=b),
	// GROUP BY L: group b=100 → {y=10,y=20} MAX(y)=20, MIN(y)=10, COUNT=2, SUM=30;
	// group b=200 → identical. Two groups.
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 (id, y) VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 (id, b) VALUES (1, 100), (2, 200)")

	pairs := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d=%d", a, b))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	scalars := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d", v))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	// The minimal Java-parity repro: group key `l` (a projected ALIAS output),
	// operand `y` (a projected bare output) — both resolve against the derived
	// projection's positional row [Y, L] by ordinal, over the buried t1×t2 join.
	t.Run("groupby_projected_alias_key", func(t *testing.T) {
		if got := scalars(t, `SELECT max(y) FROM (SELECT y, b AS L FROM t1, t2) AS q GROUP BY l`); got != "20 20" {
			t.Errorf("max(y) GROUP BY l = %q, want %q", got, "20 20")
		}
	})
	// The group key ALSO projected out (a bare output ref alongside the aggregate)
	// resolves to the same ordinal — proves both the SELECT-list ref and the
	// GROUP BY ref bake to the projection output, not the pre-projection seed.
	t.Run("select_key_and_aggregate", func(t *testing.T) {
		if got := pairs(t, `SELECT l, max(y) FROM (SELECT y, b AS L FROM t1, t2) AS q GROUP BY l`); got != "100=20 200=20" {
			t.Errorf("l, max(y) GROUP BY l = %q, want %q", got, "100=20 200=20")
		}
	})
	// A QUALIFIED reference through the derived alias (q.l / q.y) that the resolver
	// strips to the bare projected output name still resolves positionally.
	t.Run("groupby_qualified_alias_key", func(t *testing.T) {
		if got := scalars(t, `SELECT max(q.y) FROM (SELECT y, b AS L FROM t1, t2) AS q GROUP BY q.l`); got != "20 20" {
			t.Errorf("max(q.y) GROUP BY q.l = %q, want %q", got, "20 20")
		}
	})
	// Multiple aggregates + a computed operand over projected outputs: MIN/COUNT/SUM
	// each read a projected column by ordinal (SUM(y) over each 2-row group = 30).
	t.Run("multi_aggregate_over_projected", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, `SELECT l, min(y), count(*), sum(y) FROM (SELECT y, b AS L FROM t1, t2) AS q GROUP BY l`)
		if err != nil {
			t.Fatalf("multi-agg: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var l, mn, cnt, sm int64
			if err := rows.Scan(&l, &mn, &cnt, &sm); err != nil {
				t.Fatalf("scan multi-agg: %v", err)
			}
			out = append(out, fmt.Sprintf("%d:%d:%d:%d", l, mn, cnt, sm))
		}
		sort.Strings(out)
		if got := strings.Join(out, " "); got != "100:10:2:30 200:10:2:30" {
			t.Errorf("multi-agg = %q, want %q", got, "100:10:2:30 200:10:2:30")
		}
	})
}
