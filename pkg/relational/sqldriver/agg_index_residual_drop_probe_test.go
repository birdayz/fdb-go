package sqldriver_test

// Bug hunt probe: AggregateDataAccessRule drops residual WHERE predicates.
//
// Go's AggregateDataAccessRule.OnMatch (rule_aggregate_data_access.go) extracts
// the inner filter predicates and converts ONLY group-column EQUALITY predicates
// into aggregate-index scan bounds (buildAggScanPrefix). Any other filter
// predicate — one on a non-group column, or an inequality on a group column —
// is silently dropped, and the rule yields the AggregateIndex scan with NO
// compensating PredicatesFilter and NO impossibility check.
//
// Java's AggregateDataAccessRule (extends AbstractDataAccessRule) runs the full
// compensation machinery: reduce(impossibleCompensation, Compensation::intersect),
// then checks compensation.isImpossible() and applies applyAllNeededCompensations.
// A residual that filters the AGGREGATION INPUT (e.g. a non-group column) cannot
// be compensated on a pre-aggregated index, so Java rejects the match and falls
// back to StreamingAgg over a filtered scan.
//
// Result: Go reads pre-aggregated sums computed over ALL rows, ignoring the WHERE
// → WRONG aggregate values / WRONG groups.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_AggIndexResidualDrop(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggresid")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggresid")
	// f is a NON-group, NON-aggregate column. sum_by_g groups by g and sums v.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggresid "+
			"CREATE TABLE ga (id BIGINT, g BIGINT, v BIGINT, f BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_by_g AS SELECT SUM(v) FROM ga GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggresid/s WITH TEMPLATE aggresid")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggresid?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// g=1: rows (v=10,f=0),(v=30,f=1)   → SUM(v) where f=1 is 30 ; total 40
	// g=2: rows (v=5,f=0),(v=25,f=1),(v=15,f=1) → SUM(v) where f=1 is 40 ; total 45
	mwjoMustExec(t, db, ctx, "INSERT INTO ga (id,g,v,f) VALUES (1,1,10,0),(2,1,30,1),(3,2,5,0),(4,2,25,1),(5,2,15,1)")

	dump := func(q string) string {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		return plan
	}
	run := func(q string) map[int64]int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		got := map[int64]int64{}
		var keys []int64
		for rows.Next() {
			var g, a int64
			if err := rows.Scan(&g, &a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[g] = a
			keys = append(keys, g)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		return got
	}

	// Case A: residual on a NON-group column. The aggregate index cannot enforce
	// f=1 (it pre-summed over all f). Correct answer: g=1 => 30, g=2 => 40.
	t.Run("non_group_residual", func(t *testing.T) {
		q := "SELECT g, SUM(v) FROM ga WHERE f = 1 GROUP BY g"
		plan := dump(q)
		got := run(q)
		t.Logf("PLAN: %s", plan)
		t.Logf("ROWS: %v", got)
		want := map[int64]int64{1: 30, 2: 40}
		for g, w := range want {
			if got[g] != w {
				t.Errorf("WRONG SUM: g=%d => %d, want %d (residual f=1 dropped?). plan=%s", g, got[g], w, plan)
			}
		}
		if len(got) != len(want) {
			t.Errorf("WRONG group count: got %d groups %v, want %d", len(got), got, len(want))
		}
	})

	// Case C: equality whose RHS is another COLUMN (g = f), not a constant — can
	// never be a scan bound. Correct answer: only the row where g==f (row 2:
	// g=1,f=1,v=30) => g=1 SUM 30. Not all groups.
	t.Run("non_constant_rhs_residual", func(t *testing.T) {
		q := "SELECT g, SUM(v) FROM ga WHERE g = f GROUP BY g"
		plan := dump(q)
		got := run(q)
		t.Logf("PLAN: %s", plan)
		t.Logf("ROWS: %v", got)
		if _, leaked := got[2]; leaked {
			t.Errorf("group g=2 leaked despite WHERE g=f (RHS-column residual dropped). got=%v plan=%s", got, plan)
		}
		if len(got) != 1 || got[1] != 30 {
			t.Errorf("WHERE g=f: got %v, want {1:30}. plan=%s", got, plan)
		}
	})

	// Case B: inequality on the GROUP column. buildAggScanPrefix only handles
	// equality, so g>1 is dropped. Correct answer: only g=2 => 45.
	t.Run("group_inequality_residual", func(t *testing.T) {
		q := "SELECT g, SUM(v) FROM ga WHERE g > 1 GROUP BY g"
		plan := dump(q)
		got := run(q)
		t.Logf("PLAN: %s", plan)
		t.Logf("ROWS: %v", got)
		if _, leaked := got[1]; leaked {
			t.Errorf("group g=1 leaked despite WHERE g>1 (inequality residual dropped). got=%v plan=%s", got, plan)
		}
		if got[2] != 45 {
			t.Errorf("g=2 => %d, want 45. got=%v", got[2], got)
		}
	})
}

// Multi-key GROUP BY: an equality on a NON-LEADING grouping key cannot become a
// scan bound (ToScanPlan breaks at the first gap), so the aggregate index must
// not serve it. The review's repro.
func TestFDB_AggIndexResidualDrop_NonLeadingKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggresid2")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggresid2")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggresid2 "+
			"CREATE TABLE ga2 (id BIGINT, g1 BIGINT, g2 STRING, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_by_g1g2 AS SELECT SUM(v) FROM ga2 GROUP BY g1, g2")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggresid2/s WITH TEMPLATE aggresid2")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggresid2?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO ga2 (id,g1,g2,v) VALUES (1,1,'north',10),(2,1,'south',20),(3,2,'north',30),(4,2,'south',40)")

	// WHERE g2='north' (non-leading key): correct groups are (1,north)=10, (2,north)=30.
	q := "SELECT g1, g2, SUM(v) FROM ga2 WHERE g2 = 'north' GROUP BY g1, g2"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[int64]int64{}
	for rows.Next() {
		var g1, sum int64
		var g2 string
		if err := rows.Scan(&g1, &g2, &sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if g2 != "north" {
			t.Errorf("non-leading residual g2='north' dropped: got group g2=%q", g2)
		}
		got[g1] = sum
	}
	if len(got) != 2 || got[1] != 10 || got[2] != 30 {
		t.Errorf("wrong result for non-leading-key residual: got %v, want {1:10, 2:30}", got)
	}
}

// COUNT(col) must return the count of NON-NULL col values, not 0 — the
// force-covering bug made the counted column read as NULL.
func TestFDB_CountColumnNonZero(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_countcol")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_countcol")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE countcol "+
			"CREATE TABLE orders (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_amount ON orders(amount)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_countcol/s WITH TEMPLATE countcol")
	dsn := fmt.Sprintf("fdbsql:///testdb_countcol?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 3 rows match amount>5; two have non-NULL status, one is NULL.
	mwjoMustExec(t, db, ctx, "INSERT INTO orders (id,status,amount) VALUES (1,'paid',10),(2,'open',20),(3,NULL,30),(4,'paid',1)")

	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(status) FROM orders WHERE amount > 5").Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 2 {
		t.Errorf("COUNT(status) WHERE amount>5 = %d, want 2 (non-NULL statuses among the 3 matches)", n)
	}
}
