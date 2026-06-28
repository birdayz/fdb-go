package sqldriver_test

// Probes conditional aggregation (the pivot pattern): SUM(CASE WHEN cond THEN v
// ELSE 0 END) and COUNT(CASE WHEN cond THEN 1 END) grouped by a key. Combines the
// aggregate and CASE paths; COUNT over a CASE-with-no-ELSE relies on NULL being
// skipped by COUNT.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ConditionalAggregationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_condagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_condagg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE condagg "+
			"CREATE TABLE sales (id BIGINT NOT NULL, region STRING, product STRING, amount BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_condagg/s WITH TEMPLATE condagg")
	dsn := fmt.Sprintf("fdbsql:///testdb_condagg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx,
		"INSERT INTO sales (id, region, product, amount) VALUES "+
			"(1,'east','A',100),(2,'east','B',200),(3,'west','A',300),(4,'west','A',50)")

	t.Run("sum_case_pivot", func(t *testing.T) {
		// SUM(CASE WHEN product='A' THEN amount ELSE 0) per region: east=100, west=350.
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(CASE WHEN product = 'A' THEN amount ELSE 0 END) FROM sales GROUP BY region ORDER BY region")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[string]int64{}
		for rows.Next() {
			var r string
			var s int64
			if err := rows.Scan(&r, &s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[r] = s
		}
		if got["east"] != 100 || got["west"] != 350 {
			t.Errorf("SUM(CASE) pivot = %v, want east=100 west=350", got)
		}
	})

	t.Run("count_case_no_else_skips_null", func(t *testing.T) {
		// COUNT(CASE WHEN amount > 100 THEN 1 END): rows with amount>100 are 200,300 → 2.
		// (CASE with no ELSE yields NULL for the others; COUNT skips NULL.)
		var c int64
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(CASE WHEN amount > 100 THEN 1 END) FROM sales").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 2 {
			t.Errorf("COUNT(CASE WHEN amount>100) = %d, want 2", c)
		}
	})

	t.Run("count_star_vs_conditional", func(t *testing.T) {
		// COUNT(*)=4 total but conditional count of product='A' per region.
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(CASE WHEN product = 'A' THEN 1 ELSE 0 END) FROM sales GROUP BY region ORDER BY region")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[string]int64{}
		for rows.Next() {
			var r string
			var c int64
			if err := rows.Scan(&r, &c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[r] = c
		}
		if got["east"] != 1 || got["west"] != 2 {
			t.Errorf("conditional A-count = %v, want east=1 west=2", got)
		}
	})
}
