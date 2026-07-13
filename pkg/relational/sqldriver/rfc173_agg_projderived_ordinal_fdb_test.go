package sqldriver_test

// RFC-173 regression pin for the AGGREGATE-over-PROJECTING-DERIVED-SOURCE input
// edge (group-key / operand read over a derived table / CTE body that re-lays-out
// a MULTI-SOURCE base). Java-parity: Java plans GROUP BY over a multi-source-FROM
// derived table (GroupByQueryTests.java:699,
//
//	SELECT max(y) FROM (SELECT y, b AS L FROM t1,t2) AS q GROUP BY l ).
//
// The derived source's projection re-lays-out the t1×t2 join merge into a flat
// positional row [Y, L]; the streaming aggregate's group key `l` and operand `y`
// name PROJECTED OUTPUT columns, so they resolve against that projection row by
// ordinal-in-row — exactly as executeProjection resolves the projection itself —
// NOT the name-keyed Datum map and NOT the buried join's leg windows. Before this
// slice the aggregate fell through to the name-keyed row.Datum arm for a projection
// OVER A JOIN (aggregateInputIsFlatFrontier bails at the join, downstreamLegWindows
// can't peel the reshaping projection), so `l` read by name.
//
// The proof is the values.NameReadForbidden measurement gate: with it ARMED, EVERY
// name-keyed Datum read (present or absent) is a hard error. A query that still read
// its aggregate group key / operand by name would fail; the ordinalized one succeeds
// and returns the correct rows. This is the dimension the dual-window differential
// cannot see (both models agree when BOTH silently read by name). This test does NOT
// call t.Parallel(): the gate is a process-global runtime flag, so Go runs it in the
// serial phase and it resets the flag in defer — no other test observes it armed.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFDB_RFC173_AggregateProjectingDerivedOrdinal(t *testing.T) {
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

	// Arm the measurement gate ONLY for the duration of this serial test: any
	// name-keyed read on the aggregate group-key / operand path now hard-errors.
	values.NameReadForbidden = true
	defer func() { values.NameReadForbidden = false }()

	pairs := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q under NameReadForbidden: %v", q, err)
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
			t.Fatalf("query %q under NameReadForbidden: %v", q, err)
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
			t.Fatalf("multi-agg under NameReadForbidden: %v", err)
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
