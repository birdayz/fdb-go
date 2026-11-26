package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_TwoTableOrderInvariantIndexJoin proves order-invariant, cost-optimal
// index-nested-loop join selection: the same 2-table join (t1 1 row, t2 50 rows,
// joined on the indexed FK t2.t1_id) planned under both FROM-orders yields the
// BYTE-IDENTICAL physical plan — and that plan drives from the 1-row table and
// index-probes t2 via t2_by_t1 (the cost-optimal nested-loop order), regardless
// of FROM-clause position. This is the order-invariant cost-based join-ordering
// property (RFC-041/042) with the index-nested-loop join (RFC-042 L3) for the
// 2-table case. The N-way generalization is tracked separately (the partitioned
// sub-product's index-probe).
func TestFDB_TwoTableOrderInvariantIndexJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_2t")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_2t")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE t2t "+
			"CREATE TABLE t1 (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_by_t1 ON t2 (t1_id)")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_2t/s WITH TEMPLATE t2t")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_2T?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t1 VALUES (1)")
	for i := 1; i <= 50; i++ {
		testkit.MustExecCtx(t, db, ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, 1)", i))
	}

	pe := testkit.Explainer(t, db, ctx)
	a := pe("SELECT t1.id FROM t1, t2 WHERE t2.t1_id = t1.id")
	b := pe("SELECT t1.id FROM t2, t1 WHERE t2.t1_id = t1.id")

	// The query block is one select with the join, so the whole plan is the
	// access path and must not depend on FROM order.
	if a != b {
		t.Errorf("access path depends on FROM-order (not cost-based reordering):\n t1,t2: %s\n t2,t1: %s", a, b)
	}
	// The query block is one select with the join, so no Map reads the merged
	// row by ordinal; what each order projects is checked on its rows.
	if strings.HasPrefix(a, "Map(") || strings.HasPrefix(b, "Map(") {
		t.Errorf("a projection sits over the merged row:\n t1,t2: %s\n t2,t1: %s", a, b)
	}
	for _, q := range []string{
		"SELECT t1.id FROM t1, t2 WHERE t2.t1_id = t1.id",
		"SELECT t1.id FROM t2, t1 WHERE t2.t1_id = t1.id",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		n := 0
		for rows.Next() {
			var id sql.NullInt64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !id.Valid || id.Int64 != 1 {
				t.Errorf("%q returned t1.id = %v, want 1", q, id)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		rows.Close()
		if n != 50 {
			t.Errorf("%q returned %d rows, want 50", q, n)
		}
	}
	for _, p := range []string{a, b} {
		up := strings.ToUpper(p)
		if !strings.Contains(up, "INDEXSCAN(T2_BY_T1") {
			t.Errorf("plan does not index-probe t2 via t2_by_t1: %s", p)
		}
		if !strings.Contains(up, "OUTER=SCAN(T1)") {
			t.Errorf("plan does not drive from the 1-row t1: %s", p)
		}
	}
}
