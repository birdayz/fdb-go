package sqltest

import (
	"context"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_RangePredicates — OR/AND range combinations from standard-tests.yamsql
func TestFDB_RangePredicates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "rngpred", "CREATE TABLE rng(id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO rng VALUES
		(1, 10, 1), (2, 10, 2), (3, 10, 3), (4, 10, 4), (5, 10, 5),
		(6, 20, 6), (7, 20, 7), (8, 20, 8), (9, 20, 9), (10, 20, 10),
		(11, 20, 11), (12, 20, 12), (13, 20, 13)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("eq_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM rng WHERE col1 = 20 ORDER BY id")
		if len(rows) != 8 {
			t.Fatalf("want 8 (col1=20), got %d", len(rows))
		}
	})

	t.Run("range_and", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM rng WHERE col1 >= 10 AND col1 <= 20 ORDER BY id")
		if len(rows) != 13 {
			t.Fatalf("want 13 (all), got %d", len(rows))
		}
	})

	t.Run("range_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM rng WHERE col1 >= 10 OR col1 <= 20 ORDER BY id")
		if len(rows) != 13 {
			t.Fatalf("want 13 (OR covers all), got %d", len(rows))
		}
	})

	t.Run("duplicate_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM rng WHERE col1 = 20 OR col1 = 20 ORDER BY id")
		if len(rows) != 8 {
			t.Fatalf("want 8 (dedup OR), got %d", len(rows))
		}
	})

	t.Run("duplicate_and", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM rng WHERE col1 = 20 AND col1 = 20 ORDER BY id")
		if len(rows) != 8 {
			t.Fatalf("want 8 (dedup AND), got %d", len(rows))
		}
	})

	t.Run("or_of_equals", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM rng WHERE col1 = 10 OR col1 = 20 ORDER BY id")
		if len(rows) != 13 {
			t.Fatalf("want 13 (10 OR 20 = all), got %d", len(rows))
		}
	})

	t.Run("complex_or_and", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM rng WHERE (col1 = 20 OR col1 = 10) AND (col1 = 20 OR col1 = 10) ORDER BY id")
		if len(rows) != 13 {
			t.Fatalf("want 13, got %d", len(rows))
		}
	})

	t.Run("greater_than_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM rng WHERE col2 > 10 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (col2>10: 11,12,13), got %d: %v", len(rows), rows)
		}
	})

	t.Run("less_than_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM rng WHERE col2 < 3 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (col2<3: 1,2), got %d", len(rows))
		}
	})

	t.Run("not_equal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM rng WHERE col1 <> 10 ORDER BY id")
		if len(rows) != 8 {
			t.Fatalf("want 8 (col1<>10 = col1=20), got %d", len(rows))
		}
	})
}

// TestFDB_WhereWithMultipleConditions — complex WHERE with many predicates
func TestFDB_WhereWithMultipleConditions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wmcond", "CREATE TABLE wmc_t(id BIGINT, a BIGINT, b STRING, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wmc_t VALUES
		(1, 10, 'foo', 100), (2, 20, 'bar', 200), (3, 30, 'foo', 300),
		(4, 40, 'baz', 400), (5, 50, 'foo', 500), (6, 10, 'bar', 150)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("and_chain", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wmc_t WHERE a >= 10 AND a <= 30 AND b = 'foo' ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (id=1,3), got %d: %v", len(rows), rows)
		}
	})

	t.Run("or_with_and", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wmc_t WHERE (b = 'foo' AND c > 200) OR (b = 'bar' AND c < 200) ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (id=3:foo+300, id=5:foo+500, id=6:bar+150), got %d: %v", len(rows), rows)
		}
	})

	t.Run("between_and_eq", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wmc_t WHERE a BETWEEN 20 AND 40 AND b = 'foo' ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Errorf("want id=3, got %v", rows)
		}
	})

	t.Run("not_equal_combined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wmc_t WHERE b <> 'foo' AND c > 100 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (id=2,4,6), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_WhereWithNegation — NOT, negative numbers, subtraction in WHERE
func TestFDB_WhereWithNegation(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wneg", "CREATE TABLE wn_t(id BIGINT, val BIGINT, flag BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wn_t VALUES (1, 10, 1), (2, -5, 0), (3, 20, 1), (4, -10, 0), (5, 0, 1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("negative_values", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wn_t WHERE val < 0 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 negative, got %d: %v", len(rows), rows)
		}
	})

	t.Run("not_flag", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wn_t WHERE NOT (flag = 1) ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (flag<>1), got %d: %v", len(rows), rows)
		}
	})

	t.Run("abs_via_case", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN val < 0 THEN val * -1 ELSE val END AS abs_val
			FROM wn_t ORDER BY id
		`)
		if toInt64(rows[1][1]) != 5 {
			t.Errorf("id=2: abs(-5)=5, got %v", rows[1][1])
		}
		if toInt64(rows[3][1]) != 10 {
			t.Errorf("id=4: abs(-10)=10, got %v", rows[3][1])
		}
	})
}

// TestFDB_WhereWithOrAndIn — WHERE combining OR with IN
func TestFDB_WhereWithOrAndIn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "woai", "CREATE TABLE woai_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO woai_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'C', 30), (4, 'D', 40), (5, 'A', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("or_with_in", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM woai_t WHERE cat IN ('A', 'B') OR val > 35 ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4 (A:1,5 + B:2 + val>35:4), got %d: %v", len(rows), rows)
		}
	})

	t.Run("and_with_in", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM woai_t WHERE cat IN ('A', 'C') AND val >= 30 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (C:30, A:50), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_WhereWithSubtraction — WHERE with subtraction and negative results
func TestFDB_WhereWithSubtraction(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wwsub", "CREATE TABLE wwsub_t(id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wwsub_t VALUES (1, 100, 30), (2, 50, 80), (3, 200, 100), (4, 10, 10)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("positive_difference", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wwsub_t WHERE a - b > 50 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (id=1: 70, id=3: 100), got %d: %v", len(rows), rows)
		}
	})

	t.Run("negative_difference", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, a - b FROM wwsub_t WHERE a - b < 0 ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want id=2 (50-80=-30), got %v", rows)
		}
	})

	t.Run("zero_difference", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wwsub_t WHERE a - b = 0")
		if len(rows) != 1 || toInt64(rows[0][0]) != 4 {
			t.Errorf("want id=4 (10-10=0), got %v", rows)
		}
	})

	t.Run("sum_of_differences", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(a - b) FROM wwsub_t")
		if toInt64(rows[0][0]) != 140 {
			t.Errorf("SUM(a-b) = 70+(-30)+100+0 = 140, got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereComparisonOperators — all comparison operators
func TestFDB_WhereComparisonOperators(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wco", "CREATE TABLE wco_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wco_t VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("equal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wco_t WHERE val = 30")
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("want 1, got %v", rows[0][0])
		}
	})

	t.Run("not_equal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wco_t WHERE val <> 30")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4, got %v", rows[0][0])
		}
	})

	t.Run("less_than", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wco_t WHERE val < 30")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2, got %v", rows[0][0])
		}
	})

	t.Run("less_equal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wco_t WHERE val <= 30")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})

	t.Run("greater_than", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wco_t WHERE val > 30")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2, got %v", rows[0][0])
		}
	})

	t.Run("greater_equal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wco_t WHERE val >= 30")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereWithGreaterAndLess — combined > and < in WHERE
func TestFDB_WhereWithGreaterAndLess(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wwgl", "CREATE TABLE wwgl_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wwgl_t VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("range_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wwgl_t WHERE val > 15 AND val < 45 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (20,30,40), got %d: %v", len(rows), rows)
		}
	})

	t.Run("exclusive_range_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wwgl_t WHERE val > 10 AND val < 50")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereWithMultipleBetween — multiple BETWEEN conditions with OR
func TestFDB_WhereWithMultipleBetween(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wwmb", "CREATE TABLE wwmb_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wwmb_t VALUES (1, 5), (2, 15), (3, 25), (4, 35), (5, 45), (6, 55)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("two_between_with_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wwmb_t WHERE val BETWEEN 10 AND 20 OR val BETWEEN 40 AND 50 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (15, 45), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 2 || toInt64(rows[1][0]) != 5 {
			t.Errorf("want ids 2,5 got %v,%v", rows[0][0], rows[1][0])
		}
	})

	t.Run("between_and_eq", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wwmb_t WHERE val BETWEEN 20 AND 40 AND id > 2")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 (25,35 with id>2), got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectWithArithmeticInWhere — arithmetic expressions in WHERE clause
func TestFDB_SelectWithArithmeticInWhere(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swaiw", "CREATE TABLE swaiw_t(id BIGINT, price BIGINT, qty BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO swaiw_t VALUES (1, 10, 5), (2, 20, 3), (3, 30, 2), (4, 5, 100)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("where_product_gt_50", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM swaiw_t WHERE price * qty > 50 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (id=2: 60, id=3: 60, id=4: 500), got %d: %v", len(rows), rows)
		}
	})

	t.Run("where_sum_gt", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM swaiw_t WHERE price + qty > 30 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (id=3: 32, id=4: 105), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_ComplexBooleanWhere — complex boolean logic in WHERE
func TestFDB_ComplexBooleanWhere(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cbw",
		"CREATE TABLE cbw_t(id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cbw_t VALUES
		(1,10,20,30),(2,5,25,35),(3,15,15,15),(4,20,10,30),(5,1,1,1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("and_or_combined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id FROM cbw_t
			WHERE (a > 10 OR b > 20) AND c >= 30
			ORDER BY id
		`)
		// id=1: (10>10=F OR 20>20=F)=F → out
		// id=2: (5>10=F OR 25>20=T)=T AND 35>=30=T → in
		// id=3: (15>10=T OR 15>20=F)=T AND 15>=30=F → out
		// id=4: (20>10=T OR 10>20=F)=T AND 30>=30=T → in
		want := []int64{2, 4}
		if len(rows) != len(want) {
			t.Fatalf("want %d rows, got %d: %v", len(want), len(rows), rows)
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("not_and_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM cbw_t WHERE NOT (a = b AND b = c)
		`)
		// a=b AND b=c: id=3(15,15,15) and id=5(1,1,1) → NOT → 3 others
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})

	t.Run("nested_or_and", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id FROM cbw_t
			WHERE (a >= 10 AND b >= 10) OR (c = 1)
			ORDER BY id
		`)
		// id=1: a=10,b=20 → T  id=3: a=15,b=15 → T  id=4: a=20,b=10 → T  id=5: c=1 → T
		want := []int64{1, 3, 4, 5}
		if len(rows) != len(want) {
			t.Fatalf("want %d, got %d: %v", len(want), len(rows), rows)
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}

// TestFDB_ComparisonOperatorCoverage — systematic =, <>, <, <=, >, >= coverage
func TestFDB_ComparisonOperatorCoverage(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "coc",
		"CREATE TABLE wco_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO wco_t VALUES (1,10),(2,20),(3,30),(4,40),(5,50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  int64
	}{
		{"eq", "SELECT COUNT(*) FROM wco_t WHERE val = 30", 1},
		{"neq", "SELECT COUNT(*) FROM wco_t WHERE val <> 30", 4},
		{"lt", "SELECT COUNT(*) FROM wco_t WHERE val < 30", 2},
		{"le", "SELECT COUNT(*) FROM wco_t WHERE val <= 30", 3},
		{"gt", "SELECT COUNT(*) FROM wco_t WHERE val > 30", 2},
		{"ge", "SELECT COUNT(*) FROM wco_t WHERE val >= 30", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := testkit.CollectRows(t, db, tt.query)
			if toInt64(rows[0][0]) != tt.want {
				t.Errorf("%s: want %d, got %v", tt.name, tt.want, rows[0][0])
			}
		})
	}
}

// TestFDB_WhereOrWithDifferentColumns — OR across different columns
func TestFDB_WhereOrWithDifferentColumns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wodc",
		"CREATE TABLE wodc_t(id BIGINT, name STRING, age BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wodc_t VALUES
		(1,'alice',30),(2,'bob',25),(3,'carol',40),(4,'dave',20),(5,'alice',35)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("name_or_age", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id FROM wodc_t WHERE name = 'bob' OR age >= 35 ORDER BY id
		`)
		// bob(id=2), carol(id=3,age=40), alice(id=5,age=35)
		want := []int64{2, 3, 5}
		if len(rows) != len(want) {
			t.Fatalf("want %d rows, got %d: %v", len(want), len(rows), rows)
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("count_name_alice_or_age_lt_25", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM wodc_t WHERE name = 'alice' OR age < 25
		`)
		// alice: ids 1,5; age<25: id=4(dave,20) → 3 total
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereNestedBoolean — deeply nested AND/OR boolean logic
func TestFDB_WhereNestedBoolean(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wnb",
		"CREATE TABLE wnb_t(id BIGINT, a BIGINT, b BIGINT, c BIGINT, d BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wnb_t VALUES
		(1,1,2,3,4),(2,5,6,7,8),(3,10,20,30,40),(4,0,0,0,0),(5,1,1,1,1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("nested_and_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id FROM wnb_t
			WHERE (a > 0 AND b > 0) AND (c > 5 OR d > 5)
			ORDER BY id
		`)
		// id=1: a>0,b>0 → T; c=3>5=F, d=4>5=F → F → out
		// id=2: a>0,b>0 → T; c=7>5=T → T → in
		// id=3: a>0,b>0 → T; c=30>5=T → T → in
		// id=4: a=0 → F → out
		// id=5: a>0,b>0 → T; c=1>5=F, d=1>5=F → F → out
		want := []int64{2, 3}
		if len(rows) != len(want) {
			t.Fatalf("want %d, got %d: %v", len(want), len(rows), rows)
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}

// TestFDB_WhereMultipleInPredicates — multiple IN predicates with AND
func TestFDB_WhereMultipleInPredicates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wmip",
		"CREATE TABLE wmip_t(id BIGINT, color STRING, size STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wmip_t VALUES
		(1,'red','S'),(2,'blue','M'),(3,'red','L'),(4,'green','S'),(5,'blue','S')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("color_in_and_size_in", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id FROM wmip_t WHERE color IN ('red') AND size IN ('S')
			ORDER BY id
		`)
		// red+S: only id=1
		if len(rows) != 1 || toInt64(rows[0][0]) != 1 {
			t.Errorf("want [1], got %v", rows)
		}
	})

	t.Run("size_s_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wmip_t WHERE size IN ('S')")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})
}
