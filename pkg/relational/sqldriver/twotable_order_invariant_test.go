package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
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
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_2t")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_2t")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE t2t "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, t1_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_by_t1 ON t2 (t1_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_2t/s WITH TEMPLATE t2t")
	dsn := fmt.Sprintf("fdbsql:///testdb_2t?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 VALUES (1)")
	for i := 1; i <= 50; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, 1)", i))
	}

	pe := mwjoExplainer(t, db, ctx)
	a := pe("SELECT t1.id FROM t1, t2 WHERE t2.t1_id = t1.id")
	b := pe("SELECT t1.id FROM t2, t1 WHERE t2.t1_id = t1.id")

	if a != b {
		t.Errorf("plan depends on FROM-order (not cost-based reordering):\n t1,t2: %s\n t2,t1: %s", a, b)
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
