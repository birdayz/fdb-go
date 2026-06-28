package sqldriver_test

// Probes complex GROUP BY: multiple grouping keys with all aggregate functions
// (SUM/COUNT/MAX/MIN/AVG) in one query. Verifies group partitioning and each
// aggregate by NAME (robust to the separately-tracked GROUP-BY column-order bug,
// TODO.md "GROUP BY ignores SELECT-list column order").

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_GroupByMultiKeyAggProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gmk")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gmk")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gmk CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gmk/s WITH TEMPLATE gmk")
	dsn := fmt.Sprintf("fdbsql:///testdb_gmk?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, v) VALUES "+
		"(1,1,100,10),(2,1,100,20),(3,1,200,30),(4,2,100,40)")

	// read all rows as name→value maps (order-robust).
	rows, err := db.QueryContext(ctx,
		"SELECT a, b, SUM(v) AS s, COUNT(*) AS c, MAX(v) AS mx, MIN(v) AS mn, AVG(v) AS av "+
			"FROM t GROUP BY a, b")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	groups := map[string]map[string]float64{}
	for rows.Next() {
		raw := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range raw {
			ptr[i] = &raw[i]
		}
		if err := rows.Scan(ptr...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		m := map[string]float64{}
		for i, c := range cols {
			switch x := raw[i].(type) {
			case int64:
				m[c] = float64(x)
			case float64:
				m[c] = x
			}
		}
		key := fmt.Sprintf("%v|%v", int64(m["A"]), int64(m["B"]))
		groups[key] = m
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := map[string]struct{ s, c, mx, mn, av float64 }{
		"1|100": {30, 2, 20, 10, 15},
		"1|200": {30, 1, 30, 30, 30},
		"2|100": {40, 1, 40, 40, 40},
	}
	if len(groups) != len(want) {
		t.Fatalf("got %d groups %v, want %d", len(groups), groups, len(want))
	}
	for k, w := range want {
		g, ok := groups[k]
		if !ok {
			t.Errorf("missing group %s", k)
			continue
		}
		if g["S"] != w.s || g["C"] != w.c || g["MX"] != w.mx || g["MN"] != w.mn || g["AV"] != w.av {
			t.Errorf("group %s = SUM=%v COUNT=%v MAX=%v MIN=%v AVG=%v, want %v",
				k, g["S"], g["C"], g["MX"], g["MN"], g["AV"], w)
		}
	}
}
