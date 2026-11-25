package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_HavingWithAggExpr(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "havagg",
		"CREATE TABLE orders (id BIGINT, region STRING, qty BIGINT, price BIGINT, PRIMARY KEY (id))")

	for _, o := range []struct {
		id         int
		r          string
		qty, price int
	}{
		{1, "US", 10, 5},
		{2, "US", 20, 3},
		{3, "EU", 5, 100},
		{4, "EU", 2, 200},
		{5, "AP", 1, 10},
	} {
		db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO orders VALUES (%d, '%s', %d, %d)", o.id, o.r, o.qty, o.price,
		))
	}

	t.Run("having_sum_expr_threshold", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT region, SUM(qty * price) AS revenue
			 FROM orders GROUP BY region
			 HAVING SUM(qty * price) > 100
			 ORDER BY revenue DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows (EU=900, US=110), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "EU" || rows[0][1].(int64) != 900 {
			t.Errorf("row 0: got %v, want [EU, 900]", rows[0])
		}
		if rows[1][0].(string) != "US" || rows[1][1].(int64) != 110 {
			t.Errorf("row 1: got %v, want [US, 110]", rows[1])
		}
	})

	t.Run("having_with_bare_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT region, COUNT(*) AS cnt
			 FROM orders GROUP BY region
			 HAVING COUNT(*) >= 2
			 ORDER BY region`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (EU=2, US=2), got %d: %v", len(rows), rows)
		}
	})

	_ = ctx
}

// TestFDB_HavingEdgeCases — HAVING with various predicate shapes
func TestFDB_HavingEdgeCases(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "havec", "CREATE TABLE hav_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO hav_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'A', 30),
		(4, 'B', 5), (5, 'B', 15),
		(6, 'C', 100),
		(7, 'D', 1), (8, 'D', 2), (9, 'D', 3), (10, 'D', 4)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("having_count_gt", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*) FROM hav_t GROUP BY grp HAVING COUNT(*) > 2 ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups (A=3, D=4), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" {
			t.Errorf("first should be A, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "D" {
			t.Errorf("second should be D, got %v", rows[1][0])
		}
	})

	t.Run("having_sum_lt", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, SUM(val) FROM hav_t GROUP BY grp HAVING SUM(val) < 30 ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups (B=20, D=10), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "B" {
			t.Errorf("first should be B, got %v", rows[0][0])
		}
	})

	t.Run("having_min_max", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, MIN(val), MAX(val) FROM hav_t GROUP BY grp HAVING MIN(val) > 3 ORDER BY grp")
		if len(rows) != 3 {
			t.Fatalf("want 3 groups (A min=10, B min=5, C min=100), got %d: %v", len(rows), rows)
		}
	})

	t.Run("having_count_eq_1", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp FROM hav_t GROUP BY grp HAVING COUNT(*) = 1")
		if len(rows) != 1 {
			t.Fatalf("want 1 group (C), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "C" {
			t.Errorf("should be C, got %v", rows[0][0])
		}
	})

	t.Run("having_with_order_by_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, SUM(val) AS total FROM hav_t GROUP BY grp HAVING SUM(val) > 15 ORDER BY SUM(val) DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3 (C=100, A=60, B=20), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 100 {
			t.Errorf("first should be C (100), got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 60 {
			t.Errorf("second should be A (60), got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 20 {
			t.Errorf("third should be B (20), got %v", rows[2][1])
		}
	})
}

// TestFDB_GroupByMultipleAggregatesWithHaving — GROUP BY with multiple aggregates in HAVING
func TestFDB_GroupByMultipleAggregatesWithHaving(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gmah", "CREATE TABLE gmah_t(id BIGINT, dept STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gmah_t VALUES
		(1, 'eng', 100), (2, 'eng', 120), (3, 'eng', 80),
		(4, 'sales', 90), (5, 'sales', 110),
		(6, 'hr', 70), (7, 'hr', 60), (8, 'hr', 65), (9, 'hr', 75)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("having_count_and_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT dept, COUNT(*), SUM(salary), MIN(salary), MAX(salary)
			FROM gmah_t
			GROUP BY dept
			HAVING COUNT(*) >= 3
			ORDER BY dept
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (eng=3, hr=4), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "eng" {
			t.Errorf("first should be eng, got %v", rows[0][0])
		}
		if toInt64(rows[0][2]) != 300 {
			t.Errorf("eng SUM = 100+120+80 = 300, got %v", rows[0][2])
		}
		if toInt64(rows[0][3]) != 80 {
			t.Errorf("eng MIN = 80, got %v", rows[0][3])
		}
		if toInt64(rows[0][4]) != 120 {
			t.Errorf("eng MAX = 120, got %v", rows[0][4])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "hr" {
			t.Errorf("second should be hr, got %v", rows[1][0])
		}
		if toInt64(rows[1][1]) != 4 {
			t.Errorf("hr COUNT = 4, got %v", rows[1][1])
		}
	})

	t.Run("having_avg_via_sum_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT dept, SUM(salary) / COUNT(*) AS avg_salary
			FROM gmah_t
			GROUP BY dept
			HAVING SUM(salary) / COUNT(*) > 90
			ORDER BY dept
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (eng avg=100, sales avg=100), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_GroupByHavingWithMultipleConditions — HAVING with AND/OR
func TestFDB_GroupByHavingWithMultipleConditions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbhmc", "CREATE TABLE ghmc_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ghmc_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'A', 30),
		(4, 'B', 5), (5, 'B', 15),
		(6, 'C', 100), (7, 'C', 200)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("having_count_and_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, COUNT(*), SUM(val) FROM ghmc_t
			GROUP BY grp
			HAVING COUNT(*) >= 2 AND SUM(val) > 30
			ORDER BY grp
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (A: cnt=3 sum=60, C: cnt=2 sum=300), got %d: %v", len(rows), rows)
		}
	})

	t.Run("having_min_check", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, MIN(val) FROM ghmc_t
			GROUP BY grp HAVING MIN(val) >= 5
			ORDER BY grp
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (all groups have MIN>=5), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_GroupByHavingOrderLimit — full pipeline: GROUP BY + HAVING + ORDER BY + LIMIT
func TestFDB_GroupByHavingOrderLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ghol", "CREATE TABLE ghol_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ghol_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'B', 30), (4, 'B', 40),
		(5, 'C', 50), (6, 'D', 5), (7, 'D', 15), (8, 'D', 25)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("full_pipeline", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT cat, SUM(val) AS total
			FROM ghol_t
			GROUP BY cat
			HAVING SUM(val) > 20
			ORDER BY total DESC
			LIMIT 2
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (top 2 with SUM>20), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 70 {
			t.Errorf("first should be B(70), got %v", rows[0][1])
		}
	})
}

// TestFDB_GroupByHavingSumMinMax — HAVING with SUM, MIN, MAX combined
func TestFDB_GroupByHavingSumMinMax(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ghsmm", "CREATE TABLE ghsmm_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ghsmm_t VALUES
		(1, 'A', 10), (2, 'A', 90),
		(3, 'B', 40), (4, 'B', 60),
		(5, 'C', 25), (6, 'C', 35), (7, 'C', 40)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("having_max_gt_50", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, MAX(val) FROM ghsmm_t
			GROUP BY grp HAVING MAX(val) > 50 ORDER BY grp
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (A max=90, B max=60), got %d: %v", len(rows), rows)
		}
	})

	t.Run("having_sum_and_min", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, SUM(val), MIN(val) FROM ghsmm_t
			GROUP BY grp HAVING SUM(val) >= 100 AND MIN(val) >= 10 ORDER BY grp
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (A: sum=100 min=10, B: sum=100 min=40, C: sum=100 min=25), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_SelectCountGroupByHavingLimit — COUNT + GROUP BY + HAVING + LIMIT
func TestFDB_SelectCountGroupByHavingLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "scghl", "CREATE TABLE scghl_t(id BIGINT, tag STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO scghl_t VALUES
		(1, 'go'), (2, 'go'), (3, 'go'), (4, 'java'), (5, 'java'),
		(6, 'python'), (7, 'rust'), (8, 'rust'), (9, 'rust'), (10, 'rust')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("top_2_tags_by_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT tag, COUNT(*) AS cnt FROM scghl_t
			GROUP BY tag HAVING COUNT(*) >= 2
			ORDER BY cnt DESC LIMIT 2
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "rust" || toInt64(rows[0][1]) != 4 {
			t.Errorf("first: rust 4, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_HavingCountThreshold — HAVING COUNT(*) >= N filters groups
func TestFDB_HavingCountThreshold(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "hct",
		"CREATE TABLE hct_t(id BIGINT, color STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO hct_t VALUES (1,'red'),(2,'blue'),(3,'red'),(4,'green'),(5,'red'),(6,'blue')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_ge_2", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT color, COUNT(*) FROM hct_t GROUP BY color HAVING COUNT(*) >= 2 ORDER BY color")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups (blue=2, red=3), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "blue" || toInt64(rows[0][1]) != 2 {
			t.Errorf("row 0: want [blue 2], got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "red" || toInt64(rows[1][1]) != 3 {
			t.Errorf("row 1: want [red 3], got %v", rows[1])
		}
	})

	t.Run("count_ge_3", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT color, COUNT(*) FROM hct_t GROUP BY color HAVING COUNT(*) >= 3")
		if len(rows) != 1 {
			t.Fatalf("want 1 group (red=3), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "red" {
			t.Errorf("want red, got %v", rows[0][0])
		}
	})

	t.Run("count_eq_1", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT color, COUNT(*) FROM hct_t GROUP BY color HAVING COUNT(*) = 1 ORDER BY color")
		if len(rows) != 1 {
			t.Fatalf("want 1 group (green=1), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "green" {
			t.Errorf("want green, got %v", rows[0][0])
		}
	})
}

// TestFDB_GroupByWithWhereAndHaving — WHERE filters before GROUP BY, HAVING filters after
func TestFDB_GroupByWithWhereAndHaving(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gwh",
		"CREATE TABLE gwh_t(id BIGINT, region STRING, amount BIGINT, active BOOLEAN, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gwh_t VALUES
		(1,'east',100,true),(2,'east',200,true),(3,'east',50,false),
		(4,'west',300,true),(5,'west',150,true),(6,'west',75,false),
		(7,'north',80,true)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("where_active_having_sum_gt_200", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, SUM(amount) FROM gwh_t
			WHERE active = true
			GROUP BY region
			HAVING SUM(amount) > 200
			ORDER BY region
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 groups (east=300, west=450), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "east" || toInt64(rows[0][1]) != 300 {
			t.Errorf("row 0: want [east 300], got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "west" || toInt64(rows[1][1]) != 450 {
			t.Errorf("row 1: want [west 450], got %v", rows[1])
		}
	})

	t.Run("north_excluded_by_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, SUM(amount) FROM gwh_t
			WHERE active = true
			GROUP BY region
			HAVING SUM(amount) > 200
		`)
		for _, r := range rows {
			if fmt.Sprintf("%v", r[0]) == "north" {
				t.Errorf("north (sum=80) should be excluded by HAVING > 200")
			}
		}
	})
}

// TestFDB_CountWithGroupByHavingCountEq — COUNT+HAVING with equality
func TestFDB_CountWithGroupByHavingCountEq(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cghce",
		"CREATE TABLE cghce_t(id BIGINT, tag STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cghce_t VALUES
		(1,'a'),(2,'a'),(3,'b'),(4,'b'),(5,'b'),(6,'c')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("having_count_eq_2", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT tag FROM cghce_t GROUP BY tag HAVING COUNT(*) = 2")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "a" {
			t.Errorf("want [a], got %v", rows)
		}
	})

	t.Run("having_count_gt_1", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT tag, COUNT(*) FROM cghce_t GROUP BY tag HAVING COUNT(*) > 1 ORDER BY tag")
		if len(rows) != 2 {
			t.Fatalf("want 2 (a=2, b=3), got %d", len(rows))
		}
	})
}
