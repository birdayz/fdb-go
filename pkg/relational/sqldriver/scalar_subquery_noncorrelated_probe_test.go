package sqldriver_test

// Probes NON-correlated scalar subqueries (aggregate over another table) used in a
// WHERE comparison and projected in the SELECT list. The subquery's single value is
// computed and compared/projected correctly. (Correlated scalar-subquery cardinality
// enforcement is a separate documented gap — TODO.md.)

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_ScalarSubqueryNonCorrelatedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ssqp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ssqp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ssqp "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE other (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ssqp/s WITH TEMPLATE ssqp")
	dsn := fmt.Sprintf("fdbsql:///testdb_ssqp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,1),(2,5),(3,15)")
	mwjoMustExec(t, db, ctx, "INSERT INTO other (id) VALUES (1),(2)") // COUNT=2, MAX=2, MIN=1

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	t.Run("where_gt_count_subquery", func(t *testing.T) {
		// a > COUNT(other)=2 → a∈{5,15} → ids {2,3}; excludes a=1.
		if got := ids("SELECT id FROM t WHERE a > (SELECT COUNT(*) FROM other)"); !eq(got, []int64{2, 3}) {
			t.Errorf("WHERE a > (SELECT COUNT(*)) = %v, want [2 3]", got)
		}
	})
	t.Run("where_gt_max_subquery", func(t *testing.T) {
		// a > MAX(other.id)=2 → ids {2,3}
		if got := ids("SELECT id FROM t WHERE a > (SELECT MAX(id) FROM other)"); !eq(got, []int64{2, 3}) {
			t.Errorf("WHERE a > (SELECT MAX(id)) = %v, want [2 3]", got)
		}
	})
	t.Run("scalar_subquery_in_select_list", func(t *testing.T) {
		// each row gets the constant subquery value COUNT(other)=2.
		var id, cnt int64
		if err := db.QueryRowContext(ctx,
			"SELECT id, (SELECT COUNT(*) FROM other) FROM t WHERE id = 2").Scan(&id, &cnt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != 2 || cnt != 2 {
			t.Errorf("SELECT id, (subquery) = (%d, %d), want (2, 2)", id, cnt)
		}
	})
}
