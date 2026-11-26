package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_AggregateColumnCaseSensitivity(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aggcase",
		"CREATE TABLE orders (id BIGINT, Status STRING, Amount BIGINT, PRIMARY KEY (id))")

	for _, o := range []struct {
		id     int
		status string
		amount int
	}{
		{1, "pending", 100},
		{2, "pending", 200},
		{3, "shipped", 300},
		{4, "shipped", 400},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO orders VALUES (%d, '%s', %d)", o.id, o.status, o.amount,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("having_mixed_case_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT status, SUM(amount) FROM orders GROUP BY status HAVING SUM(Amount) > 200 ORDER BY status")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "pending" || rows[0][1].(int64) != 300 {
			t.Errorf("row 0: got %v, want [pending, 300]", rows[0])
		}
	})

	t.Run("having_uppercase_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT STATUS, COUNT(*) FROM orders GROUP BY STATUS HAVING COUNT(*) >= 2")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
	})

	t.Run("mixed_case_group_by_and_select", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT Status, SUM(AMOUNT) AS total FROM orders GROUP BY Status ORDER BY total DESC")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][1].(int64) != 700 {
			t.Errorf("first total: got %v, want 700", rows[0][1])
		}
	})

	_ = ctx
}

func TestFDB_GroupByAlias(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "gbalias1",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 1)",
		"INSERT INTO t1 VALUES (2, 10, 2)",
		"INSERT INTO t1 VALUES (3, 10, 3)",
		"INSERT INTO t1 VALUES (4, 20, 4)",
		"INSERT INTO t1 VALUES (5, 20, 5)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("select_group_col", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT col1 FROM t1 GROUP BY col1 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[1][0]) != 20 {
			t.Errorf("got %v, %v", rows[0][0], rows[1][0])
		}
	})

	t.Run("select_group_col_with_alias", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT col1 AS xx FROM t1 GROUP BY col1 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[1][0]) != 20 {
			t.Errorf("got %v, %v", rows[0][0], rows[1][0])
		}
	})

	t.Run("select_star_group_by_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT * FROM t1 GROUP BY col1")
		if err == nil {
			t.Fatal("expected error for SELECT * with GROUP BY")
		}
		if !strings.Contains(err.Error(), "42803") {
			t.Errorf("want SQLSTATE 42803, got %v", err)
		}
	})

	t.Run("select_non_grouped_col_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id FROM t1 GROUP BY col1")
		if err == nil {
			t.Fatal("expected error for non-grouped column")
		}
		if !strings.Contains(err.Error(), "42803") {
			t.Errorf("want SQLSTATE 42803, got %v", err)
		}
	})

	t.Run("select_undefined_col_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT bla FROM t1 GROUP BY col1")
		if err == nil {
			t.Fatal("expected error for undefined column")
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Errorf("want SQLSTATE 42703, got %v", err)
		}
	})

	t.Run("max_min_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1, MAX(col2), MIN(col2) FROM t1 GROUP BY col1 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 3 || toInt64(rows[0][2]) != 1 {
			t.Errorf("group 10: want max=3,min=1, got %v,%v", rows[0][1], rows[0][2])
		}
		if toInt64(rows[1][1]) != 5 || toInt64(rows[1][2]) != 4 {
			t.Errorf("group 20: want max=5,min=4, got %v,%v", rows[1][1], rows[1][2])
		}
	})

	t.Run("having_min_and_col", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MAX(id) FROM t1 GROUP BY col1 HAVING MIN(id) > 0 AND col1 = 20")
		if len(rows) != 1 || toInt64(rows[0][0]) != 5 {
			t.Fatalf("want [{5}], got %v", rows)
		}
	})

	t.Run("count_star_ungrouped", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM t1")
		if len(rows) != 1 || toInt64(rows[0][0]) != 5 {
			t.Fatalf("want 5, got %v", rows)
		}
	})

	t.Run("group_col_expr_plus_literal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1 + 10 FROM t1 GROUP BY col1 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 20 || toInt64(rows[1][0]) != 30 {
			t.Errorf("got %v, %v, want 20, 30", rows[0][0], rows[1][0])
		}
	})

	t.Run("duplicate_group_alias_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT col1 FROM t1 GROUP BY col1 AS x, col2 AS x")
		if err == nil {
			t.Fatal("expected error for duplicate group alias")
		}
		if !strings.Contains(err.Error(), "42702") {
			t.Errorf("want SQLSTATE 42702, got %v", err)
		}
	})

	t.Run("derived_table_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1 FROM (SELECT col1 FROM t1) AS x GROUP BY col1 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[1][0]) != 20 {
			t.Errorf("got %v, %v", rows[0][0], rows[1][0])
		}
	})

	t.Run("derived_table_max", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MAX(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY x.col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 3 || toInt64(rows[1][0]) != 5 {
			t.Errorf("got %v, %v, want 3, 5", rows[0][0], rows[1][0])
		}
	})

	t.Run("derived_table_min", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MIN(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY x.col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[1][0]) != 4 {
			t.Errorf("got %v, %v, want 1, 4", rows[0][0], rows[1][0])
		}
	})

	t.Run("derived_table_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT COUNT(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY x.col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 3 || toInt64(rows[1][0]) != 2 {
			t.Errorf("got %v, %v, want 3, 2", rows[0][0], rows[1][0])
		}
	})

	t.Run("derived_table_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT SUM(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY x.col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 6 || toInt64(rows[1][0]) != 9 {
			t.Errorf("got %v, %v, want 6, 9", rows[0][0], rows[1][0])
		}
	})

	t.Run("sum_div_count_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT SUM(x.col2) / COUNT(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY x.col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("group 10: want 6/3=2, got %v", rows[0][0])
		}
		if toInt64(rows[1][0]) != 4 {
			t.Errorf("group 20: want 9/2=4, got %v", rows[1][0])
		}
	})

	t.Run("derived_max_col_not_in_derived_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"SELECT MAX(x.col2) FROM (SELECT col1 FROM t1) AS x GROUP BY x.col1")
		if err == nil {
			t.Fatal("expected error: col2 not in derived table")
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Errorf("want SQLSTATE 42703, got %v", err)
		}
	})

	t.Run("ungrouped_col_through_derived_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"SELECT x.col2 FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1")
		if err == nil {
			t.Fatal("expected error: col2 not in GROUP BY")
		}
		if !strings.Contains(err.Error(), "42803") {
			t.Errorf("want SQLSTATE 42803, got %v", err)
		}
	})

	t.Run("ungrouped_max", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MAX(x.col2) FROM (SELECT col1, col2 FROM t1) AS x")
		if len(rows) != 1 || toInt64(rows[0][0]) != 5 {
			t.Fatalf("want 5, got %v", rows)
		}
	})

	t.Run("ungrouped_min", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MIN(x.col2) FROM (SELECT col1, col2 FROM t1) AS x")
		if len(rows) != 1 || toInt64(rows[0][0]) != 1 {
			t.Fatalf("want 1, got %v", rows)
		}
	})

	t.Run("ungrouped_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT COUNT(x.col2) FROM (SELECT col1, col2 FROM t1) AS x")
		if len(rows) != 1 || toInt64(rows[0][0]) != 5 {
			t.Fatalf("want 5, got %v", rows)
		}
	})

	t.Run("nested_derived_agg_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT g + 4 FROM
			   (SELECT MIN(x.col2) AS g FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1) AS y
			 WHERE g > 3`)
		if len(rows) != 1 || toInt64(rows[0][0]) != 8 {
			t.Fatalf("want [{8}] (MIN for col1=20 is 4, +4=8), got %v", rows)
		}
	})
}

func TestFDB_GroupByTableAliasEdgeCases(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "gbtalias1",
		"CREATE TABLE sales (id BIGINT, region STRING, amount BIGINT, rep STRING, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO sales VALUES (1, 'US', 100, 'Alice')",
		"INSERT INTO sales VALUES (2, 'US', 200, 'Bob')",
		"INSERT INTO sales VALUES (3, 'EU', 150, 'Charlie')",
		"INSERT INTO sales VALUES (4, 'EU', 250, 'Diana')",
		"INSERT INTO sales VALUES (5, 'APAC', 300, 'Eve')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("aliased_group_by_string", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT s.region, COUNT(*), SUM(s.amount) FROM sales s GROUP BY s.region ORDER BY s.region")
		if len(rows) != 3 {
			t.Fatalf("want 3 regions, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "APAC" || toInt64(rows[0][1]) != 1 || toInt64(rows[0][2]) != 300 {
			t.Errorf("APAC: got %v", rows[0])
		}
		if rows[1][0].(string) != "EU" || toInt64(rows[1][1]) != 2 || toInt64(rows[1][2]) != 400 {
			t.Errorf("EU: got %v", rows[1])
		}
		if rows[2][0].(string) != "US" || toInt64(rows[2][1]) != 2 || toInt64(rows[2][2]) != 300 {
			t.Errorf("US: got %v", rows[2])
		}
	})

	t.Run("aliased_having_with_alias_prefix", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT s.region, SUM(s.amount)
			 FROM sales s
			 GROUP BY s.region
			 HAVING SUM(s.amount) > 300
			 ORDER BY s.region`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (EU, 400), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "EU" {
			t.Errorf("want EU, got %v", rows[0][0])
		}
	})

	t.Run("aliased_group_by_two_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT s.region, s.rep, SUM(s.amount)
			 FROM sales s
			 GROUP BY s.region, s.rep
			 ORDER BY s.region, s.rep`)
		if len(rows) != 5 {
			t.Fatalf("want 5 (each row is its own group), got %d: %v", len(rows), rows)
		}
	})

	t.Run("unaliased_same_results", func(t *testing.T) {
		aliased := testkit.CollectRows(t, db,
			"SELECT s.region, COUNT(*) FROM sales s GROUP BY s.region ORDER BY s.region")
		unaliased := testkit.CollectRows(t, db,
			"SELECT region, COUNT(*) FROM sales GROUP BY region ORDER BY region")
		if len(aliased) != len(unaliased) {
			t.Fatalf("aliased (%d) != unaliased (%d)", len(aliased), len(unaliased))
		}
		for i := range aliased {
			if aliased[i][0] != unaliased[i][0] || toInt64(aliased[i][1]) != toInt64(unaliased[i][1]) {
				t.Errorf("row %d: aliased=%v, unaliased=%v", i, aliased[i], unaliased[i])
			}
		}
	})

	_ = ctx
}

func TestFDB_NestedAggregateErrors(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "nesterr1",
		"CREATE TABLE t1 (id BIGINT, val BIGINT, PRIMARY KEY (id))")

	db.ExecContext(ctx, "INSERT INTO t1 VALUES (1, 10)")
	db.ExecContext(ctx, "INSERT INTO t1 VALUES (2, 20)")

	t.Run("nested_aggregate_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT SUM(MAX(val)) FROM t1")
		if err == nil {
			t.Fatal("expected error for nested aggregate")
		}
	})

	t.Run("aggregate_in_where_treated_as_having", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT val FROM t1 WHERE SUM(val) > 10")
		if err != nil {
			t.Logf("aggregate in WHERE rejects: %v (acceptable)", err)
		} else {
			t.Log("aggregate in WHERE accepted (may be treated as HAVING)")
		}
	})
}

func TestFDB_WindowOfAggregation(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "winagg1",
		"CREATE TABLE sales (id BIGINT, region STRING, year BIGINT, amount BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO sales VALUES (1, 'US', 2023, 100)",
		"INSERT INTO sales VALUES (2, 'US', 2023, 200)",
		"INSERT INTO sales VALUES (3, 'US', 2024, 300)",
		"INSERT INTO sales VALUES (4, 'EU', 2023, 150)",
		"INSERT INTO sales VALUES (5, 'EU', 2024, 250)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("group_by_two_cols", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT region, year, SUM(amount) AS total
			 FROM sales
			 GROUP BY region, year
			 ORDER BY region, year`)
		if len(rows) != 4 {
			t.Fatalf("want 4 groups, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "EU" || toInt64(rows[0][1]) != 2023 || toInt64(rows[0][2]) != 150 {
			t.Errorf("EU/2023: got %v", rows[0])
		}
		if rows[1][0].(string) != "EU" || toInt64(rows[1][1]) != 2024 || toInt64(rows[1][2]) != 250 {
			t.Errorf("EU/2024: got %v", rows[1])
		}
		if rows[2][0].(string) != "US" || toInt64(rows[2][1]) != 2023 || toInt64(rows[2][2]) != 300 {
			t.Errorf("US/2023: got %v", rows[2])
		}
		if rows[3][0].(string) != "US" || toInt64(rows[3][1]) != 2024 || toInt64(rows[3][2]) != 300 {
			t.Errorf("US/2024: got %v", rows[3])
		}
	})

	t.Run("having_on_multi_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT region, year, SUM(amount)
			 FROM sales
			 GROUP BY region, year
			 HAVING SUM(amount) >= 250
			 ORDER BY region, year`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (exclude EU/2023=150), got %d: %v", len(rows), rows)
		}
	})

	t.Run("derived_table_yearly_total", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT year, SUM(total) AS grand_total FROM (
				SELECT region, year, SUM(amount) AS total
				FROM sales
				GROUP BY region, year
			) AS yearly
			GROUP BY year
			ORDER BY year`)
		if len(rows) != 2 {
			t.Fatalf("want 2 years, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 2023 || toInt64(rows[0][1]) != 450 {
			t.Errorf("2023: got %v, want (2023, 450)", rows[0])
		}
		if toInt64(rows[1][0]) != 2024 || toInt64(rows[1][1]) != 550 {
			t.Errorf("2024: got %v, want (2024, 550)", rows[1])
		}
	})

	_ = ctx
}

func TestFDB_GroupByAliasWithTableName(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "gbtn1",
		"CREATE TABLE items (id BIGINT, category STRING, price BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO items VALUES (1, 'A', 100)",
		"INSERT INTO items VALUES (2, 'A', 200)",
		"INSERT INTO items VALUES (3, 'B', 300)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("group_by_table_name_qualified", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT items.category, SUM(items.price) FROM items GROUP BY items.category ORDER BY items.category")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || toInt64(rows[0][1]) != 300 {
			t.Errorf("A: got %v", rows[0])
		}
		if rows[1][0].(string) != "B" || toInt64(rows[1][1]) != 300 {
			t.Errorf("B: got %v", rows[1])
		}
	})

	t.Run("select_alias_group_unqualified", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT i.category, SUM(i.price) FROM items i GROUP BY category ORDER BY category")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || toInt64(rows[0][1]) != 300 {
			t.Errorf("A: got %v", rows[0])
		}
	})

	t.Run("select_alias_group_alias", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT i.category, SUM(i.price) FROM items i GROUP BY i.category ORDER BY i.category")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || toInt64(rows[0][1]) != 300 {
			t.Errorf("A: got %v", rows[0])
		}
	})

	t.Run("having_with_alias_qualified_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT i.category FROM items i GROUP BY i.category HAVING SUM(i.price) > 200")
		if len(rows) != 2 {
			t.Fatalf("want 2 (both groups have sum >= 300), got %d: %v", len(rows), rows)
		}
	})

	_ = ctx
}
