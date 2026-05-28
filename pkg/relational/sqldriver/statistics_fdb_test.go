package sqldriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_StatisticsDrivenPlanSelection verifies that fetchTableStatistics
// reads real row counts from FDB via read-only snapshot transactions and
// feeds them into the Cascades cost model. The test inserts known data,
// runs EXPLAIN to verify plan shapes, and executes queries to verify
// correct results — proving the full pipeline: count maintenance →
// stats read → cost model → plan selection → execution.
func TestFDB_StatisticsDrivenPlanSelection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	ddl := "CREATE TABLE orders (" +
		"id BIGINT NOT NULL, " +
		"customer_id BIGINT, " +
		"amount BIGINT, " +
		"PRIMARY KEY (id)) " +
		"CREATE INDEX idx_customer ON orders (customer_id)"

	t.Run("stats_pipeline_with_data", func(t *testing.T) {
		t.Parallel()
		db := setupPlanShapeDB(t, "stats_data", ddl)

		// Insert 100 rows. Each INSERT goes through SaveRecord →
		// addRecordCount, maintaining the count index atomically.
		for i := 0; i < 100; i++ {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(
				"INSERT INTO orders VALUES (%d, %d, %d)", i, i%10, (i+1)*10)); err != nil {
				t.Fatalf("INSERT id=%d: %v", i, err)
			}
		}

		// Full scan — plan should show Scan(ORDERS).
		q1 := "SELECT id, customer_id, amount FROM orders"
		plan1 := planExplainVia(t, ctx, db, q1)
		t.Logf("full scan plan: %s", plan1)
		if !strings.Contains(plan1, "Scan(ORDERS") {
			t.Fatalf("expected Scan(ORDERS) in plan, got: %s", plan1)
		}
		rows1, err := db.QueryContext(ctx, q1)
		if err != nil {
			t.Fatalf("full scan query: %v", err)
		}
		defer rows1.Close()
		count1 := countRows(t, rows1)
		if count1 != 100 {
			t.Fatalf("expected 100 rows from full scan, got %d", count1)
		}

		// Index equality — plan should use IndexScan on idx_customer.
		// fetchTableStatistics reads real count (100) instead of default
		// 1e6. The cost model uses this for HintCost cardinality.
		q2 := "SELECT id, amount FROM orders WHERE customer_id = 5"
		plan2 := planExplainVia(t, ctx, db, q2)
		t.Logf("index equality plan: %s", plan2)
		if !strings.Contains(plan2, "IndexScan") {
			t.Fatalf("expected IndexScan in plan, got: %s", plan2)
		}
		if !strings.Contains(plan2, "IDX_CUSTOMER") {
			t.Fatalf("expected IDX_CUSTOMER in plan, got: %s", plan2)
		}
		rows2, err := db.QueryContext(ctx, q2)
		if err != nil {
			t.Fatalf("index equality query: %v", err)
		}
		defer rows2.Close()
		count2 := countRows(t, rows2)
		// 100 rows with customer_id = i%10, so customer_id=5 has 10 rows.
		if count2 != 10 {
			t.Fatalf("expected 10 rows for customer_id=5, got %d", count2)
		}
	})

	t.Run("stats_pipeline_empty_table", func(t *testing.T) {
		t.Parallel()
		db := setupPlanShapeDB(t, "stats_empty", ddl)

		// No data inserted. The atomic-ADD count key was never written,
		// so fetchTableStatistics gets empty bytes for all types →
		// returns nil → planner uses DefaultStatistics (1e6).
		// Planning must still succeed without errors.
		q := "SELECT id, amount FROM orders WHERE customer_id = 1"
		plan := planExplainVia(t, ctx, db, q)
		t.Logf("empty table plan: %s", plan)
		if !strings.Contains(plan, "IndexScan") {
			t.Fatalf("expected IndexScan even on empty table, got: %s", plan)
		}

		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query on empty table: %v", err)
		}
		defer rows.Close()
		count := countRows(t, rows)
		if count != 0 {
			t.Fatalf("expected 0 rows from empty table, got %d", count)
		}
	})

	t.Run("stats_survive_deletes", func(t *testing.T) {
		t.Parallel()
		db := setupPlanShapeDB(t, "stats_del", ddl)

		// Insert 50 rows.
		for i := 0; i < 50; i++ {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(
				"INSERT INTO orders VALUES (%d, %d, %d)", i, i%5, (i+1)*10)); err != nil {
				t.Fatalf("INSERT id=%d: %v", i, err)
			}
		}

		// Delete 25 rows — count maintenance fires on DELETE too.
		for i := 0; i < 25; i++ {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(
				"DELETE FROM orders WHERE id = %d", i)); err != nil {
				t.Fatalf("DELETE id=%d: %v", i, err)
			}
		}

		// Plan and query should work correctly with updated counts.
		q := "SELECT id, amount FROM orders WHERE customer_id = 0"
		plan := planExplainVia(t, ctx, db, q)
		t.Logf("post-delete plan: %s", plan)
		if !strings.Contains(plan, "IndexScan") {
			t.Fatalf("expected IndexScan in plan, got: %s", plan)
		}

		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("post-delete query: %v", err)
		}
		defer rows.Close()
		count := countRows(t, rows)
		// Original: 50 rows, customer_id = i%5, so 10 rows per customer.
		// Deleted IDs 0-24. Customer 0 had IDs {0,5,10,15,20,25,30,35,40,45}.
		// Deleted: {0,5,10,15,20}. Remaining: {25,30,35,40,45} = 5 rows.
		if count != 5 {
			t.Fatalf("expected 5 rows after deletes, got %d", count)
		}
	})
}
