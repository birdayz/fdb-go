package sqldriver_test

// Probes aggregate edges: COUNT(*) vs COUNT(1) vs COUNT(col-with-NULL),
// aggregate of an expression (SUM(a+b), AVG(a*2)), and HAVING without GROUP BY
// (a scalar aggregate over all rows, filtered by HAVING).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
)

func TestFDB_AggregateEdgesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggedge")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggedge")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggedge "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggedge/s WITH TEMPLATE aggedge")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggedge?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: 10, 20, NULL ; b: 1, 2, 3
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1, 10, 1), (2, 20, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, b) VALUES (3, 3)") // a NULL

	scalar := func(q string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	t.Run("count_star_vs_one_vs_col", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM t"); got != 3 {
			t.Errorf("COUNT(*) = %d, want 3", got)
		}
		if got := scalar("SELECT COUNT(1) FROM t"); got != 3 {
			t.Errorf("COUNT(1) = %d, want 3", got)
		}
		if got := scalar("SELECT COUNT(a) FROM t"); got != 2 {
			t.Errorf("COUNT(a) = %d, want 2 (NULL a skipped)", got)
		}
	})
	t.Run("sum_of_expression", func(t *testing.T) {
		// SUM(a+b): rows with a non-null: (10+1)+(20+2)=33. a NULL → a+b NULL → skipped.
		if got := scalar("SELECT SUM(a + b) FROM t"); got != 33 {
			t.Errorf("SUM(a+b) = %d, want 33", got)
		}
	})
	t.Run("sum_b_includes_all", func(t *testing.T) {
		// SUM(b): 1+2+3 = 6 (b never NULL).
		if got := scalar("SELECT SUM(b) FROM t"); got != 6 {
			t.Errorf("SUM(b) = %d, want 6", got)
		}
	})
	t.Run("avg_of_expression", func(t *testing.T) {
		// AVG(a*2): a non-null = 10,20 → *2 = 20,40 → avg 30.
		var avg float64
		if err := db.QueryRowContext(ctx, "SELECT AVG(a * 2) FROM t").Scan(&avg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if math.Abs(avg-30) > 1e-9 {
			t.Errorf("AVG(a*2) = %v, want 30 (over non-null a)", avg)
		}
	})
	t.Run("having_without_group_by_true", func(t *testing.T) {
		// scalar aggregate, HAVING keeps the single group: SUM(b)=6 > 5 → one row=6.
		got := scalar("SELECT SUM(b) FROM t HAVING SUM(b) > 5")
		if got != 6 {
			t.Errorf("SUM(b) HAVING SUM(b)>5 = %d, want 6", got)
		}
	})
	t.Run("having_without_group_by_false", func(t *testing.T) {
		// HAVING false → zero rows.
		rows, err := db.QueryContext(ctx, "SELECT SUM(b) FROM t HAVING SUM(b) > 100")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if n != 0 {
			t.Errorf("HAVING SUM(b)>100 returned %d rows, want 0", n)
		}
	})
}
