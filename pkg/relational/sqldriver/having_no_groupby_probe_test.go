package sqldriver_test

// Probes HAVING without GROUP BY (a whole-table aggregate filter over the implicit
// single group): the aggregate row is returned only when the HAVING predicate holds.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_HavingNoGroupByProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_hngp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_hngp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE hngp CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_hngp/s WITH TEMPLATE hngp")
	dsn := fmt.Sprintf("fdbsql:///testdb_hngp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,30),(2,40)") // SUM=70, COUNT=2

	one := func(q string) (int, int64) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		n := 0
		var last int64
		for rows.Next() {
			_ = rows.Scan(&last)
			n++
		}
		return n, last
	}

	t.Run("sum_having_pass", func(t *testing.T) {
		if n, v := one("SELECT SUM(v) FROM t HAVING SUM(v) > 50"); n != 1 || v != 70 {
			t.Errorf("SUM HAVING SUM>50 = (%d rows, %d), want (1, 70)", n, v)
		}
	})
	t.Run("sum_having_fail", func(t *testing.T) {
		if n, _ := one("SELECT SUM(v) FROM t HAVING SUM(v) > 100"); n != 0 {
			t.Errorf("SUM HAVING SUM>100 rows = %d, want 0", n)
		}
	})
	t.Run("count_having_pass", func(t *testing.T) {
		if n, v := one("SELECT COUNT(*) FROM t HAVING COUNT(*) > 1"); n != 1 || v != 2 {
			t.Errorf("COUNT HAVING COUNT>1 = (%d rows, %d), want (1, 2)", n, v)
		}
	})
	t.Run("count_having_fail", func(t *testing.T) {
		if n, _ := one("SELECT COUNT(*) FROM t HAVING COUNT(*) > 5"); n != 0 {
			t.Errorf("COUNT HAVING COUNT>5 rows = %d, want 0", n)
		}
	})
}
