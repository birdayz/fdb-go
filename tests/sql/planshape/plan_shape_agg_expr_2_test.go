package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_AggregateEmptyTable — Java aggregate-empty-table.yamsql patterns
func TestFDB_AggregateEmptyTable(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aget", "CREATE TABLE empty_t(id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id))")

	t.Run("count_star_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM empty_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("COUNT(*) on empty table should be 0, got %v", rows[0][0])
		}
	})

	t.Run("count_star_with_false_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM empty_t WHERE col1 = 0")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("COUNT(*) with WHERE on empty table should be 0, got %v", rows[0][0])
		}
	})

	t.Run("sum_empty_returns_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(col1) FROM empty_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("SUM on empty table should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("sum_with_where_empty_returns_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(col1) FROM empty_t WHERE col1 > 0")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("SUM with WHERE on empty table should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("count_column_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(col2) FROM empty_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("COUNT(col2) on empty table should be 0, got %v", rows[0][0])
		}
	})

	t.Run("group_by_empty_returns_no_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT col1, COUNT(*) FROM empty_t GROUP BY col1")
		if len(rows) != 0 {
			t.Errorf("GROUP BY on empty table should return 0 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("min_max_empty_returns_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT MIN(col1), MAX(col1) FROM empty_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("MIN on empty table should be NULL, got %v", rows[0][0])
		}
		if rows[0][1] != nil {
			t.Errorf("MAX on empty table should be NULL, got %v", rows[0][1])
		}
	})

	t.Run("insert_delete_then_count", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO empty_t VALUES (1, 10, 20), (2, 30, 40), (3, 50, 60)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM empty_t")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Fatalf("after insert: want COUNT(*)=3, got %v", rows)
		}

		if _, err := db.ExecContext(ctx, "DELETE FROM empty_t WHERE id >= 1"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows = testkit.CollectRows(t, db, "SELECT COUNT(*) FROM empty_t")
		if len(rows) != 1 || toInt64(rows[0][0]) != 0 {
			t.Errorf("after delete: want COUNT(*)=0, got %v", rows)
		}

		rows = testkit.CollectRows(t, db, "SELECT SUM(col1) FROM empty_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("SUM after delete should be NULL, got %v", rows[0][0])
		}
	})
}

// TestFDB_AggregateWithNullGroups — aggregates when group key contains NULL
func TestFDB_AggregateWithNullGroups(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "agng", "CREATE TABLE agng_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO agng_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, NULL, 30), (4, NULL, 40), (5, 'B', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("null_group_key", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*), SUM(val) FROM agng_t GROUP BY grp ORDER BY grp")
		if len(rows) < 2 {
			t.Fatalf("want at least 2 groups, got %d: %v", len(rows), rows)
		}
		t.Logf("NULL group results: %v", rows)
	})

	t.Run("count_star_vs_count_col", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*), COUNT(grp) FROM agng_t")
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("COUNT(*) should be 5, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("COUNT(grp) should be 3 (excludes NULLs), got %v", rows[0][1])
		}
	})
}

// TestFDB_MinMaxWithStrings — MIN/MAX on string columns
func TestFDB_MinMaxWithStrings(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mmstr", "CREATE TABLE mms_t(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO mms_t VALUES (1, 'charlie'), (2, 'alice'), (3, 'bob'), (4, 'david')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("min_string_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT MIN(name) FROM mms_t")
		if err == nil {
			t.Logf("MIN(string) succeeded unexpectedly")
		} else {
			t.Logf("MIN(string) not supported: %v", err)
		}
	})

	t.Run("count_string_works", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(name) FROM mms_t")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("COUNT(name) should be 4, got %v", rows[0][0])
		}
	})
}

// TestFDB_SumWithArithmeticExpressions — SUM of arithmetic expressions
func TestFDB_SumWithArithmeticExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sumae", "CREATE TABLE sae_t(id BIGINT, qty BIGINT, price BIGINT, discount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sae_t VALUES
		(1, 10, 100, 5), (2, 20, 50, 10), (3, 5, 200, 0)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("sum_of_product", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(qty * price) FROM sae_t")
		if toInt64(rows[0][0]) != 3000 {
			t.Errorf("SUM(qty*price) = 1000+1000+1000 = 3000, got %v", rows[0][0])
		}
	})

	t.Run("sum_of_difference", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(price - discount) FROM sae_t")
		if toInt64(rows[0][0]) != 335 {
			t.Errorf("SUM(price-discount) = 95+40+200 = 335, got %v", rows[0][0])
		}
	})

	t.Run("multiple_sum_expressions", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(qty), SUM(price), SUM(qty * price) FROM sae_t")
		if toInt64(rows[0][0]) != 35 {
			t.Errorf("SUM(qty) = 35, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 350 {
			t.Errorf("SUM(price) = 350, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 3000 {
			t.Errorf("SUM(qty*price) = 3000, got %v", rows[0][2])
		}
	})
}

// TestFDB_GroupByWithMinMaxBigint — MIN/MAX on BIGINT with GROUP BY
func TestFDB_GroupByWithMinMaxBigint(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbmmb", "CREATE TABLE gbmm_t(id BIGINT, dept STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbmm_t VALUES
		(1, 'eng', 80), (2, 'eng', 120), (3, 'eng', 100),
		(4, 'hr', 70), (5, 'hr', 90),
		(6, 'sales', 110)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("min_max_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT dept, MIN(salary), MAX(salary) FROM gbmm_t GROUP BY dept ORDER BY dept")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 80 || toInt64(rows[0][2]) != 120 {
			t.Errorf("eng: MIN=80 MAX=120, got %v %v", rows[0][1], rows[0][2])
		}
		if toInt64(rows[1][1]) != 70 || toInt64(rows[1][2]) != 90 {
			t.Errorf("hr: MIN=70 MAX=90, got %v %v", rows[1][1], rows[1][2])
		}
		if toInt64(rows[2][1]) != 110 || toInt64(rows[2][2]) != 110 {
			t.Errorf("sales: MIN=MAX=110, got %v %v", rows[2][1], rows[2][2])
		}
	})

	t.Run("max_minus_min_range", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT dept, MAX(salary) - MIN(salary) AS range_val FROM gbmm_t GROUP BY dept ORDER BY dept")
		if toInt64(rows[0][1]) != 40 {
			t.Errorf("eng range = 120-80 = 40, got %v", rows[0][1])
		}
		if toInt64(rows[2][1]) != 0 {
			t.Errorf("sales range = 110-110 = 0, got %v", rows[2][1])
		}
	})
}

// TestFDB_GroupByWithSumAndCoalesce — SUM with COALESCE in GROUP BY context
func TestFDB_GroupByWithSumAndCoalesce(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbsc", "CREATE TABLE gbsc_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbsc_t VALUES
		(1, 'A', 10), (2, 'A', NULL), (3, 'B', 30), (4, 'B', 40), (5, 'A', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("sum_with_coalesce_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, SUM(COALESCE(val, 0)) AS total
			FROM gbsc_t GROUP BY grp ORDER BY grp
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 60 {
			t.Errorf("A: SUM(COALESCE) = 10+0+50 = 60, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 70 {
			t.Errorf("B: SUM(COALESCE) = 30+40 = 70, got %v", rows[1][1])
		}
	})

	t.Run("count_vs_count_col_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, COUNT(*), COUNT(val) FROM gbsc_t GROUP BY grp ORDER BY grp
		`)
		if toInt64(rows[0][1]) != 3 || toInt64(rows[0][2]) != 2 {
			t.Errorf("A: COUNT(*)=3 COUNT(val)=2, got %v %v", rows[0][1], rows[0][2])
		}
	})
}

// TestFDB_SumWithMultiplication — SUM(col1*col2) expression aggregate
func TestFDB_SumWithMultiplication(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swmul", "CREATE TABLE swmul_t(id BIGINT, qty BIGINT, price BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO swmul_t VALUES (1, 5, 10), (2, 3, 20), (3, 7, 15)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("sum_product", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(qty * price) FROM swmul_t")
		if toInt64(rows[0][0]) != 215 {
			t.Errorf("SUM(qty*price) = 50+60+105 = 215, got %v", rows[0][0])
		}
	})

	t.Run("individual_products", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, qty * price AS total FROM swmul_t ORDER BY total DESC")
		if toInt64(rows[0][1]) != 105 {
			t.Errorf("max product: 7*15=105, got %v", rows[0][1])
		}
	})
}

// TestFDB_NullAggregateEdges — aggregate behavior with all-NULL column
func TestFDB_NullAggregateEdges(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nae",
		"CREATE TABLE nae_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO nae_t(id) VALUES (1),(2),(3)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_star_includes_nulls", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM nae_t")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("COUNT(*) should count all rows including NULL val, got %v", rows[0][0])
		}
	})

	t.Run("count_col_excludes_nulls", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(val) FROM nae_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("COUNT(val) should be 0 for all-NULL column, got %v", rows[0][0])
		}
	})

	t.Run("sum_null_is_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM nae_t")
		if rows[0][0] != nil {
			t.Errorf("SUM(NULL col) should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("min_null_is_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT MIN(val) FROM nae_t")
		if rows[0][0] != nil {
			t.Errorf("MIN(NULL col) should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("coalesce_sum_zero", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COALESCE(SUM(val), 0) FROM nae_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("COALESCE(SUM(NULL), 0) should be 0, got %v", rows[0][0])
		}
	})
}

// TestFDB_MinMaxGroupByOrderByAggregate — MIN/MAX with GROUP BY, ORDER BY aggregate
func TestFDB_MinMaxGroupByOrderByAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mmga",
		"CREATE TABLE mmga_t(id BIGINT, team STRING, pts BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO mmga_t VALUES
		(1,'a',10),(2,'a',50),(3,'a',30),
		(4,'b',20),(5,'b',40),
		(6,'c',5),(7,'c',15),(8,'c',25),(9,'c',35)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("min_per_team_order_by_min", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT team, MIN(pts) FROM mmga_t GROUP BY team ORDER BY MIN(pts)
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 teams, got %d", len(rows))
		}
		// c:min=5, a:min=10, b:min=20
		wantMin := []int64{5, 10, 20}
		for i, w := range wantMin {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want min %d, got %v", i, w, rows[i][1])
			}
		}
	})

	t.Run("max_per_team_order_by_max_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT team, MAX(pts) FROM mmga_t GROUP BY team ORDER BY MAX(pts) DESC
		`)
		// a:max=50, b:max=40, c:max=35
		wantMax := []int64{50, 40, 35}
		for i, w := range wantMax {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want max %d, got %v", i, w, rows[i][1])
			}
		}
	})

	t.Run("range_per_team", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT team, MAX(pts) - MIN(pts) AS spread FROM mmga_t
			GROUP BY team ORDER BY team
		`)
		// a: 50-10=40, b: 40-20=20, c: 35-5=30
		wantSpread := []int64{40, 20, 30}
		for i, w := range wantSpread {
			if toInt64(rows[i][1]) != w {
				t.Errorf("team %v: want spread %d, got %v", rows[i][0], w, rows[i][1])
			}
		}
	})
}

// TestFDB_SumFilteredGrouped — SUM filtered by WHERE then grouped
func TestFDB_SumFilteredGrouped(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sfg",
		"CREATE TABLE sfg_t(id BIGINT, region STRING, year BIGINT, revenue BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sfg_t VALUES
		(1,'east',2024,100),(2,'west',2024,200),(3,'east',2025,150),
		(4,'west',2025,250),(5,'east',2024,50),(6,'north',2025,300)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("sum_2024_per_region", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, SUM(revenue) FROM sfg_t
			WHERE year = 2024
			GROUP BY region
			ORDER BY region
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 regions in 2024, got %d", len(rows))
		}
		// east: 100+50=150, west: 200
		if fmt.Sprintf("%v", rows[0][0]) != "east" || toInt64(rows[0][1]) != 150 {
			t.Errorf("east: want 150, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "west" || toInt64(rows[1][1]) != 200 {
			t.Errorf("west: want 200, got %v", rows[1])
		}
	})

	t.Run("sum_2025_total", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(revenue) FROM sfg_t WHERE year = 2025")
		// 150+250+300=700
		if toInt64(rows[0][0]) != 700 {
			t.Errorf("want 700, got %v", rows[0][0])
		}
	})

	t.Run("count_per_year", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT year, COUNT(*) FROM sfg_t GROUP BY year ORDER BY year
		`)
		if toInt64(rows[0][1]) != 3 || toInt64(rows[1][1]) != 3 {
			t.Errorf("want [3 3], got [%v %v]", rows[0][1], rows[1][1])
		}
	})
}
