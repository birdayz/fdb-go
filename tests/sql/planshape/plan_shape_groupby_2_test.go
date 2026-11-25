package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_MultipleAggregates — multiple different aggregates in same query
func TestFDB_MultipleAggregates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mulagg", "CREATE TABLE multi_agg(id BIGINT, category STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO multi_agg VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'A', 30),
		(4, 'B', 15), (5, 'B', 25),
		(6, 'C', 100)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_sum_min_max_global", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount) FROM multi_agg")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("COUNT(*) want 6, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 200 {
			t.Errorf("SUM want 200, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 10 {
			t.Errorf("MIN want 10, got %v", rows[0][2])
		}
		if toInt64(rows[0][3]) != 100 {
			t.Errorf("MAX want 100, got %v", rows[0][3])
		}
	})

	t.Run("grouped_multi_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT category, COUNT(*), SUM(amount), MIN(amount), MAX(amount) FROM multi_agg GROUP BY category ORDER BY category")
		if len(rows) != 3 {
			t.Fatalf("want 3 groups, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" {
			t.Errorf("row 0: want A, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("A COUNT want 3, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 60 {
			t.Errorf("A SUM want 60, got %v", rows[0][2])
		}
		if toInt64(rows[0][3]) != 10 {
			t.Errorf("A MIN want 10, got %v", rows[0][3])
		}
		if toInt64(rows[0][4]) != 30 {
			t.Errorf("A MAX want 30, got %v", rows[0][4])
		}
		if toInt64(rows[1][1]) != 2 {
			t.Errorf("B COUNT want 2, got %v", rows[1][1])
		}
		if toInt64(rows[1][2]) != 40 {
			t.Errorf("B SUM want 40, got %v", rows[1][2])
		}
		if toInt64(rows[2][1]) != 1 {
			t.Errorf("C COUNT want 1, got %v", rows[2][1])
		}
		if toInt64(rows[2][2]) != 100 {
			t.Errorf("C SUM want 100, got %v", rows[2][2])
		}
	})

	t.Run("having_with_multiple_aggregates", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT category, COUNT(*), SUM(amount) FROM multi_agg GROUP BY category HAVING COUNT(*) > 1 ORDER BY category")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups with COUNT>1 (A,B), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" {
			t.Errorf("first should be A, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "B" {
			t.Errorf("second should be B, got %v", rows[1][0])
		}
	})
}

// TestFDB_GroupByWithWherePush — GROUP BY queries with WHERE filters that can push down
func TestFDB_GroupByWithWherePush(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbwp",
		"CREATE TABLE sales(id BIGINT, region STRING, product STRING, qty BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sales VALUES
		(1, 'east', 'apples', 10), (2, 'east', 'bananas', 20),
		(3, 'west', 'apples', 30), (4, 'west', 'bananas', 40),
		(5, 'east', 'apples', 50), (6, 'north', 'cherries', 5)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_by_with_eq_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT product, SUM(qty) FROM sales WHERE region = 'east' GROUP BY product ORDER BY product")
		if len(rows) != 2 {
			t.Fatalf("want 2 (apples, bananas), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "apples" {
			t.Errorf("first should be apples, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 60 {
			t.Errorf("east apples = 10+50 = 60, got %v", rows[0][1])
		}
	})

	t.Run("group_by_with_range_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT region, COUNT(*) FROM sales WHERE qty > 15 GROUP BY region ORDER BY region")
		if len(rows) != 2 {
			t.Fatalf("want 2 (east=1, west=2), got %d: %v", len(rows), rows)
		}
	})

	t.Run("group_by_two_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT region, product, SUM(qty) FROM sales GROUP BY region, product ORDER BY region, product")
		if len(rows) != 5 {
			t.Fatalf("want 5 distinct region+product combos, got %d: %v", len(rows), rows)
		}
	})

	t.Run("group_by_count_with_having_and_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT product, COUNT(*), SUM(qty)
			FROM sales
			WHERE region IN ('east', 'west')
			GROUP BY product
			HAVING COUNT(*) > 1
			ORDER BY product
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 products with >1 sale in east+west, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "apples" {
			t.Errorf("first should be apples, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("apples count should be 3, got %v", rows[0][1])
		}
	})

	t.Run("group_by_with_between", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT product, SUM(qty) FROM sales WHERE qty BETWEEN 10 AND 30 GROUP BY product ORDER BY product")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups (apples=10+30=40, bananas=20), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 40 {
			t.Errorf("apples sum = 10+30 = 40, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 20 {
			t.Errorf("bananas sum = 20, got %v", rows[1][1])
		}
	})
}

// TestFDB_GroupByWithOrderByAndLimit — GROUP BY + ORDER BY + LIMIT combined
func TestFDB_GroupByWithOrderByAndLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbol", "CREATE TABLE gbol_t(id BIGINT, region STRING, revenue BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbol_t VALUES
		(1, 'east', 100), (2, 'east', 200), (3, 'west', 50),
		(4, 'west', 150), (5, 'north', 300), (6, 'south', 75), (7, 'south', 125)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("top_2_by_revenue", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, SUM(revenue) AS total
			FROM gbol_t GROUP BY region
			ORDER BY total DESC LIMIT 2
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (top 2), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 300 {
			t.Errorf("top region total should be 300 (east or north), got %v", rows[0][1])
		}
	})

	t.Run("bottom_1_by_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, COUNT(*) AS cnt
			FROM gbol_t GROUP BY region
			ORDER BY cnt LIMIT 1
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 1 {
			t.Errorf("smallest group has 1 row (north), got %v", rows[0][1])
		}
	})

	t.Run("all_groups_ordered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, SUM(revenue) AS total
			FROM gbol_t GROUP BY region
			ORDER BY region
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4 groups, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "east" {
			t.Errorf("first alphabetically should be east, got %v", rows[0][0])
		}
	})
}

// TestFDB_BetweenWithGroupBy — BETWEEN combined with GROUP BY and HAVING
func TestFDB_BetweenWithGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "btgb", "CREATE TABLE btgb_t(id BIGINT, score BIGINT, grade STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO btgb_t VALUES
		(1, 95, 'A'), (2, 85, 'B'), (3, 75, 'C'), (4, 65, 'D'),
		(5, 92, 'A'), (6, 88, 'B'), (7, 72, 'C'), (8, 55, 'F')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("between_filter_then_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grade, COUNT(*), SUM(score)
			FROM btgb_t WHERE score BETWEEN 70 AND 95
			GROUP BY grade ORDER BY grade
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (A,B,C in range 70-95), got %d: %v", len(rows), rows)
		}
	})

	t.Run("not_between_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM btgb_t WHERE score NOT BETWEEN 70 AND 90 ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4 (id=1:95, id=4:65, id=5:92, id=8:55), got %d: %v", len(rows), rows)
		}
	})

	t.Run("between_with_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grade, COUNT(*)
			FROM btgb_t WHERE score BETWEEN 50 AND 100
			GROUP BY grade HAVING COUNT(*) >= 2
			ORDER BY grade
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (A=2, B=2, C=2), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_GroupByOrderByNonAggColumn — ORDER BY on GROUP BY key
func TestFDB_GroupByOrderByNonAggColumn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbobn", "CREATE TABLE gobn_t(id BIGINT, city STRING, revenue BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gobn_t VALUES
		(1, 'NYC', 500), (2, 'LA', 300), (3, 'NYC', 400),
		(4, 'SF', 200), (5, 'LA', 100), (6, 'SF', 600)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_group_key", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT city, SUM(revenue) FROM gobn_t GROUP BY city ORDER BY city")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "LA" {
			t.Errorf("first alphabetically should be LA, got %v", rows[0][0])
		}
	})

	t.Run("order_by_aggregate_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT city, SUM(revenue) AS total FROM gobn_t GROUP BY city ORDER BY total")
		if toInt64(rows[0][1]) != 400 {
			t.Errorf("smallest total should be LA(400), got %v", rows[0][1])
		}
	})

	t.Run("order_by_count_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT city, COUNT(*) AS cnt FROM gobn_t GROUP BY city ORDER BY cnt DESC, city")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 2 {
			t.Errorf("all groups have 2, got %v", rows[0][1])
		}
	})
}

// TestFDB_CaseWhenInGroupBy — CASE WHEN expression used as GROUP BY key
func TestFDB_CaseWhenInGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cwgb", "CREATE TABLE cwgb_t(id BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cwgb_t VALUES
		(1, 95), (2, 85), (3, 75), (4, 65), (5, 55), (6, 45), (7, 35)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("case_when_group_by_expression_not_matched", func(t *testing.T) {
		_, err := db.QueryContext(ctx, `
			SELECT
				CASE WHEN score >= 90 THEN 'A'
				     WHEN score >= 70 THEN 'B'
				     WHEN score >= 50 THEN 'C'
				     ELSE 'F' END AS grade,
				COUNT(*)
			FROM cwgb_t
			GROUP BY CASE WHEN score >= 90 THEN 'A'
			              WHEN score >= 70 THEN 'B'
			              WHEN score >= 50 THEN 'C'
			              ELSE 'F' END
			ORDER BY grade
		`)
		if err != nil {
			t.Logf("CASE WHEN expression GROUP BY not supported: %v", err)
		} else {
			t.Logf("CASE WHEN expression GROUP BY succeeded (unexpectedly)")
		}
	})
}

// TestFDB_GroupByWithCoalesceAndCase — GROUP BY with expression columns
func TestFDB_GroupByWithCoalesceAndCase(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbcc", "CREATE TABLE gbcc_t(id BIGINT, region STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbcc_t VALUES
		(1, 'east', 100), (2, NULL, 200), (3, 'west', 300), (4, NULL, 400), (5, 'east', 500)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_by_coalesce", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COALESCE(region, 'unknown'), SUM(amount)
			FROM gbcc_t
			GROUP BY COALESCE(region, 'unknown')
			ORDER BY COALESCE(region, 'unknown')
		`)
		t.Logf("GROUP BY COALESCE: %v", rows)
		if len(rows) < 2 {
			t.Fatalf("want at least 2 groups, got %d", len(rows))
		}
	})
}

// TestFDB_GroupByTwoColumnsWithAggregate — GROUP BY on two columns
func TestFDB_GroupByTwoColumnsWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gb2ca", "CREATE TABLE gb2_t(id BIGINT, region STRING, product STRING, sales BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gb2_t VALUES
		(1, 'east', 'A', 10), (2, 'east', 'A', 20), (3, 'east', 'B', 30),
		(4, 'west', 'A', 40), (5, 'west', 'B', 50), (6, 'west', 'B', 60)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_by_two_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, product, SUM(sales), COUNT(*)
			FROM gb2_t GROUP BY region, product
			ORDER BY region, product
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4 groups, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "east" || fmt.Sprintf("%v", rows[0][1]) != "A" || toInt64(rows[0][2]) != 30 {
			t.Errorf("east/A: want sum=30, got %v %v %v", rows[0][0], rows[0][1], rows[0][2])
		}
		if toInt64(rows[3][2]) != 110 {
			t.Errorf("west/B: want sum=50+60=110, got %v", rows[3][2])
		}
	})

	t.Run("group_by_two_with_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, product, SUM(sales)
			FROM gb2_t GROUP BY region, product
			HAVING SUM(sales) > 35
			ORDER BY region, product
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (west/A=40, west/B=110), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_SelectCountWithVariousFilters — COUNT(*) with different WHERE patterns
func TestFDB_SelectCountWithVariousFilters(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "scvf", "CREATE TABLE scvf_t(id BIGINT, status STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO scvf_t VALUES
		(1, 'active', 90), (2, 'inactive', 60), (3, 'active', 80),
		(4, 'active', 70), (5, 'inactive', 50), (6, 'active', 95)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_all", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM scvf_t")
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("want 6, got %v", rows[0][0])
		}
	})

	t.Run("count_with_eq", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM scvf_t WHERE status = 'active'")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4 active, got %v", rows[0][0])
		}
	})

	t.Run("count_with_gt", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM scvf_t WHERE score > 75")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 (80,90,95), got %v", rows[0][0])
		}
	})

	t.Run("count_with_combined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM scvf_t WHERE status = 'active' AND score >= 80")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 (80,90,95 active), got %v", rows[0][0])
		}
	})
}

// TestFDB_AggregateWithWhereAndOrderBy — aggregate + WHERE + ORDER BY combined
func TestFDB_AggregateWithWhereAndOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "awwo", "CREATE TABLE awwo_t(id BIGINT, dept STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO awwo_t VALUES
		(1, 'eng', 100), (2, 'eng', 120), (3, 'sales', 80),
		(4, 'sales', 90), (5, 'hr', 70), (6, 'eng', 150)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_where_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT dept, SUM(salary) AS total
			FROM awwo_t WHERE salary > 75
			GROUP BY dept ORDER BY total DESC
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (eng, sales after filtering hr=70), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "eng" || toInt64(rows[0][1]) != 370 {
			t.Errorf("first: eng total=100+120+150=370, got %v %v", rows[0][0], rows[0][1])
		}
	})

	t.Run("having_and_where_combined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT dept, COUNT(*), SUM(salary)
			FROM awwo_t WHERE salary >= 80
			GROUP BY dept HAVING COUNT(*) >= 2
			ORDER BY dept
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (eng=3, sales=2 after WHERE>=80), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_GroupByWithWhereOnDifferentColumn — GROUP BY one col, WHERE on another
func TestFDB_GroupByWithWhereOnDifferentColumn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbwdc", "CREATE TABLE gbwdc_t(id BIGINT, dept STRING, level STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbwdc_t VALUES
		(1, 'eng', 'senior', 150), (2, 'eng', 'junior', 80),
		(3, 'sales', 'senior', 120), (4, 'sales', 'junior', 70),
		(5, 'eng', 'senior', 160), (6, 'hr', 'junior', 60)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_by_dept_where_level", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT dept, COUNT(*), SUM(salary)
			FROM gbwdc_t WHERE level = 'senior'
			GROUP BY dept ORDER BY dept
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (eng, sales have seniors), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "eng" || toInt64(rows[0][1]) != 2 || toInt64(rows[0][2]) != 310 {
			t.Errorf("eng seniors: want cnt=2 sum=310, got %v %v %v", rows[0][0], rows[0][1], rows[0][2])
		}
	})
}

// TestFDB_GroupByWithMaxMinAndOrder — GROUP BY with MAX-MIN range and ORDER BY
func TestFDB_GroupByWithMaxMinAndOrder(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbmmo", "CREATE TABLE gbmmo_t(id BIGINT, team STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbmmo_t VALUES
		(1, 'A', 10), (2, 'A', 50), (3, 'B', 30), (4, 'B', 35),
		(5, 'C', 20), (6, 'C', 80), (7, 'C', 45)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("range_per_team", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT team, MAX(score) - MIN(score) AS score_range
			FROM gbmmo_t GROUP BY team ORDER BY score_range DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "C" || toInt64(rows[0][1]) != 60 {
			t.Errorf("C range = 80-20 = 60, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "A" || toInt64(rows[1][1]) != 40 {
			t.Errorf("A range = 50-10 = 40, got %v %v", rows[1][0], rows[1][1])
		}
	})
}
