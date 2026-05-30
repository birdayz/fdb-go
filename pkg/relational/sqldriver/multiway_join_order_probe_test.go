package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_MultiwayJoinOrder_Probe is the acceptance test for RFC-042:
// FROM-order-independent, cost-optimal multi-way join ordering. The same 3-way
// chain join (t1=1 row ← t2=20 ← t3=200, joined on indexed FK columns) planned
// under two OPPOSITE FROM-orders must yield:
//
//	(a) BYTE-IDENTICAL physical plans — cost-based join reordering, not
//	    FROM-clause order. A FROM-order-bound planner yields two different trees.
//	(b) COST-OPTIMAL — drive from the 1-row t1 and reach the 200-row t3 last via
//	    its index (IndexScan(t3_by_t2)), never a full Scan(T3).
//	(c) CORRECT ROWS — both orders return the 200 chain rows (t1.id = 1).
//
// This exercises the full RFC-042 stack: L1 REWRITING flattens to a flat seed
// (PushProjectionBelowJoinRule removed); PartitionSelectRule re-enumerates
// associativities, routing spanning join predicates to the upper so the
// re-enumerated (t1⋈t2)⋈t3 associativity is generated for EVERY FROM-order, and
// skipping degenerate disconnected cross-product partitions; the correlated
// equi-predicate SARGs t3_by_t2; and the NLJ hash-join column extraction
// qualifies QOV-child FieldValues so the join returns rows at scale.
func TestFDB_MultiwayJoinOrder_Probe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_mwjo")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mwjo")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mwjo_tmpl "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, t1_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t3 (id BIGINT NOT NULL, t2_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_by_t1 ON t2 (t1_id) "+
			"CREATE INDEX t3_by_t2 ON t3 (t2_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mwjo/s WITH TEMPLATE mwjo_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_mwjo?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO t1 VALUES (1)")
	for i := 1; i <= 20; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, 1)", i))
	}
	for i := 1; i <= 200; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t3 VALUES (%d, %d)", i, (i%20)+1))
	}

	planExplain := mwjoExplainer(t, db, ctx)

	qBigFirst := "SELECT t1.id FROM t3, t2, t1 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id"
	qSmallFirst := "SELECT t1.id FROM t1, t2, t3 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id"

	planBig := planExplain(qBigFirst)
	planSmall := planExplain(qSmallFirst)

	// (a) Order-invariance.
	if planBig != planSmall {
		t.Errorf("MULTI-WAY ORDERING: plan depends on FROM-order (not cost-based reordering):\n big-first:   %s\n small-first: %s", planBig, planSmall)
	}

	// (b) Cost-optimal.
	for _, p := range []string{planBig, planSmall} {
		up := strings.ToUpper(p)
		if strings.Contains(up, "SCAN(T3)") {
			t.Errorf("COST: plan full-scans the 200-row T3 instead of index-probing it: %s", p)
		}
		if !strings.Contains(up, "INDEXSCAN(T3_BY_T2") {
			t.Errorf("COST: plan does not index-probe T3 via t3_by_t2: %s", p)
		}
		if !strings.Contains(up, "SCAN(T1)") {
			t.Errorf("COST: plan does not drive from the 1-row t1: %s", p)
		}
	}

	// (c) Correctness — both orders return the 200 chain rows, all t1.id = 1.
	for _, q := range []string{qBigFirst, qSmallFirst} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		var n, bad int
		for rows.Next() {
			var id sql.NullInt64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
			if !id.Valid || id.Int64 != 1 {
				bad++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		rows.Close()
		if n != 200 {
			t.Errorf("CORRECTNESS: query %q returned %d rows, want 200:\n  %s", q, n, planExplain(q))
		}
		if bad != 0 {
			t.Errorf("CORRECTNESS: query %q returned %d rows with t1.id != 1, want 0", q, bad)
		}
	}
}
