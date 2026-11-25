package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_OrderByWithNulls — ORDER BY behavior with NULL values
func TestFDB_OrderByWithNulls(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "obnull", "CREATE TABLE obn_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO obn_t VALUES (1, 30), (2, NULL), (3, 10), (4, NULL), (5, 20)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_asc_nulls", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM obn_t ORDER BY val, id")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		t.Logf("ORDER BY val ASC: %v", rows)
	})

	t.Run("order_by_desc_nulls", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM obn_t ORDER BY val DESC, id")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		t.Logf("ORDER BY val DESC: %v", rows)
	})

	t.Run("count_non_null_ordered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(val) FROM obn_t")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("COUNT(val) should be 3 (excludes 2 NULLs), got %v", rows[0][0])
		}
	})
}

// TestFDB_OrderByWithLimitAndOffset — ORDER BY + LIMIT combinations
func TestFDB_OrderByWithLimitAndOffset(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "oblim", "CREATE TABLE obl_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	for i := 1; i <= 10; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO obl_t VALUES (%d, %d)", i, (11-i)*10)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	t.Run("top_3_by_val_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM obl_t ORDER BY val DESC LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 100 {
			t.Errorf("top val should be 100, got %v", rows[0][1])
		}
	})

	t.Run("bottom_1_by_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM obl_t ORDER BY val LIMIT 1")
		if toInt64(rows[0][1]) != 10 {
			t.Errorf("bottom val should be 10, got %v", rows[0][1])
		}
	})

	t.Run("limit_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM obl_t WHERE val >= 50 ORDER BY val LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3 from >=50, got %d", len(rows))
		}
	})
}

// TestFDB_SelectWithWhereAndOrderByLimit — SELECT + WHERE + ORDER BY + LIMIT
func TestFDB_SelectWithWhereAndOrderByLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swol", "CREATE TABLE swol_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO swol_t VALUES
		(1, 'A', 50), (2, 'B', 30), (3, 'A', 70), (4, 'B', 10),
		(5, 'A', 90), (6, 'C', 60), (7, 'B', 80)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("top_2_cat_a_by_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM swol_t WHERE cat = 'A' ORDER BY val DESC LIMIT 2")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 90 || toInt64(rows[1][1]) != 70 {
			t.Errorf("want 90,70 got %v,%v", rows[0][1], rows[1][1])
		}
	})

	t.Run("bottom_3_all_by_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM swol_t ORDER BY val LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 10 {
			t.Errorf("smallest should be 10, got %v", rows[0][1])
		}
	})
}

// TestFDB_SelectWithCaseInOrderBy — ORDER BY with CASE expression
func TestFDB_SelectWithCaseInOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "scob", "CREATE TABLE scob_t(id BIGINT, priority STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO scob_t VALUES
		(1, 'low', 10), (2, 'high', 20), (3, 'medium', 30), (4, 'high', 40), (5, 'low', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_case_priority", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, priority, val
			FROM scob_t
			ORDER BY CASE priority WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END, val DESC
		`)
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][1]) != "high" {
			t.Errorf("first should be high priority, got %v", rows[0][1])
		}
		t.Logf("priority order: %v", rows)
	})
}

// TestFDB_SelectWithOrderByMultipleColumns — ORDER BY with 2+ columns
func TestFDB_SelectWithOrderByMultipleColumns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "somc", "CREATE TABLE somc_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO somc_t VALUES
		(1, 'B', 20), (2, 'A', 30), (3, 'B', 10), (4, 'A', 10), (5, 'C', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_cat_then_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, val FROM somc_t ORDER BY cat, val")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" || toInt64(rows[0][1]) != 10 {
			t.Errorf("first: want A,10 got %v,%v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "A" || toInt64(rows[1][1]) != 30 {
			t.Errorf("second: want A,30 got %v,%v", rows[1][0], rows[1][1])
		}
	})
}

// TestFDB_MultiColumnOrderTies — ORDER BY with tied first column, break by second
func TestFDB_MultiColumnOrderTies(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mcot",
		"CREATE TABLE mcot_t(id BIGINT, grade STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO mcot_t VALUES (1,'A',90),(2,'B',80),(3,'A',70),(4,'B',95),(5,'A',85)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("asc_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grade, score FROM mcot_t ORDER BY grade, score")
		want := []int64{70, 85, 90, 80, 95}
		for i, w := range want {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want score %d, got %v", i, w, rows[i][1])
			}
		}
	})

	t.Run("asc_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grade, score FROM mcot_t ORDER BY grade ASC, score DESC")
		want := []int64{90, 85, 70, 95, 80}
		for i, w := range want {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want score %d, got %v", i, w, rows[i][1])
			}
		}
	})

	t.Run("desc_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grade, score FROM mcot_t ORDER BY grade DESC, score ASC")
		want := []int64{80, 95, 70, 85, 90}
		for i, w := range want {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want score %d, got %v", i, w, rows[i][1])
			}
		}
	})
}

// TestFDB_BetweenOrderBy — BETWEEN filter combined with ORDER BY
func TestFDB_BetweenOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "btob",
		"CREATE TABLE btob_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO btob_t VALUES (1,10),(2,25),(3,30),(4,45),(5,50),(6,15),(7,35)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("between_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT val FROM btob_t WHERE val BETWEEN 20 AND 40 ORDER BY val")
		want := []int64{25, 30, 35}
		if len(rows) != len(want) {
			t.Fatalf("want %d rows, got %d", len(want), len(rows))
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("between_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT val FROM btob_t WHERE val BETWEEN 20 AND 40 ORDER BY val DESC")
		want := []int64{35, 30, 25}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("not_between", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT val FROM btob_t WHERE val NOT BETWEEN 20 AND 40 ORDER BY val")
		want := []int64{10, 15, 45, 50}
		if len(rows) != len(want) {
			t.Fatalf("want %d rows, got %d", len(want), len(rows))
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}

// TestFDB_StringOrderByLexicographic — verify strings sort lexicographically
func TestFDB_StringOrderByLexicographic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sol",
		"CREATE TABLE sol_t(id BIGINT, word STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO sol_t VALUES (1,'banana'),(2,'apple'),(3,'cherry'),(4,'date'),(5,'elderberry')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT word FROM sol_t ORDER BY word ASC")
		want := []string{"apple", "banana", "cherry", "date", "elderberry"}
		for i, w := range want {
			if fmt.Sprintf("%v", rows[i][0]) != w {
				t.Errorf("row %d: want %s, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT word FROM sol_t ORDER BY word DESC")
		want := []string{"elderberry", "date", "cherry", "banana", "apple"}
		for i, w := range want {
			if fmt.Sprintf("%v", rows[i][0]) != w {
				t.Errorf("row %d: want %s, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("like_prefix_ordered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT word FROM sol_t WHERE word LIKE 'a%' OR word LIKE 'b%' ORDER BY word")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "apple" || fmt.Sprintf("%v", rows[1][0]) != "banana" {
			t.Errorf("want [apple banana], got %v", rows)
		}
	})
}

// TestFDB_WhereInWithOrderByLimit — IN predicate + ORDER BY + LIMIT
func TestFDB_WhereInWithOrderByLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wiol",
		"CREATE TABLE wiol_t(id BIGINT, status STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wiol_t VALUES
		(1,'a',100),(2,'b',200),(3,'a',300),(4,'c',400),(5,'b',500),(6,'a',600)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("in_a_order_amount_desc_limit2", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, amount FROM wiol_t
			WHERE status IN ('a')
			ORDER BY amount DESC
			LIMIT 2
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 600 || toInt64(rows[1][1]) != 300 {
			t.Errorf("want [600 300], got [%v %v]", rows[0][1], rows[1][1])
		}
	})

	t.Run("in_a_or_b_via_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM wiol_t
			WHERE status = 'a' OR status = 'b'
		`)
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5 (3 a's + 2 b's), got %v", rows[0][0])
		}
	})

	t.Run("not_in_c", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM wiol_t WHERE status NOT IN ('c')
		`)
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5 (all except c), got %v", rows[0][0])
		}
	})
}

// TestFDB_NullOrderingSortBehavior — NULL sort position in ASC/DESC
func TestFDB_NullOrderingSortBehavior(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "obn",
		"CREATE TABLE obn_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO obn_t VALUES (1,30),(2,10),(3,null),(4,20),(5,null)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("nulls_in_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM obn_t ORDER BY val ASC")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		// NULLs sort first or last depending on implementation
		// Just verify non-null values are in ascending order
		var nonNulls []int64
		for _, r := range rows {
			if r[1] != nil {
				nonNulls = append(nonNulls, toInt64(r[1]))
			}
		}
		for i := 1; i < len(nonNulls); i++ {
			if nonNulls[i] < nonNulls[i-1] {
				t.Errorf("non-null values not in ASC order: %v", nonNulls)
				break
			}
		}
	})

	t.Run("nulls_in_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM obn_t ORDER BY val DESC")
		var nonNulls []int64
		for _, r := range rows {
			if r[1] != nil {
				nonNulls = append(nonNulls, toInt64(r[1]))
			}
		}
		for i := 1; i < len(nonNulls); i++ {
			if nonNulls[i] > nonNulls[i-1] {
				t.Errorf("non-null values not in DESC order: %v", nonNulls)
				break
			}
		}
	})
}

// TestFDB_LimitZeroReturnsNothing — LIMIT 0 returns no rows
func TestFDB_LimitZeroReturnsNothing(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "lzrn",
		"CREATE TABLE lzrn_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO lzrn_t VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("limit_0", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM lzrn_t LIMIT 0")
		if len(rows) != 0 {
			t.Errorf("want 0 rows with LIMIT 0, got %d", len(rows))
		}
	})

	t.Run("limit_1", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lzrn_t ORDER BY id LIMIT 1")
		if len(rows) != 1 || toInt64(rows[0][0]) != 1 {
			t.Errorf("want [1], got %v", rows)
		}
	})

	t.Run("limit_exceeds_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lzrn_t LIMIT 100")
		if len(rows) != 3 {
			t.Errorf("want 3 (all rows), got %d", len(rows))
		}
	})
}

// TestFDB_OrderByThreeColumnsLimit — ORDER BY 3 columns with LIMIT
func TestFDB_OrderByThreeColumnsLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "o3cl",
		"CREATE TABLE o3cl_t(id BIGINT, a STRING, b BIGINT, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO o3cl_t VALUES
		(1,'x',1,10),(2,'x',1,20),(3,'x',2,5),(4,'y',1,15),(5,'y',2,25)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_a_b_c_limit3", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM o3cl_t ORDER BY a, b, c LIMIT 3")
		// x,1,10(id=1) → x,1,20(id=2) → x,2,5(id=3)
		want := []int64{1, 2, 3}
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}
