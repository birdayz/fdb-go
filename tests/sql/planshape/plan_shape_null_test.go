package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_CoalesceEdgeCases(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "coalesce1",
		"CREATE TABLE t1 (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 100)",
		"INSERT INTO t1 VALUES (2, NULL, 200)",
		"INSERT INTO t1 VALUES (3, NULL, NULL)",
		"INSERT INTO t1 VALUES (4, 40, NULL)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("coalesce_first_non_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, COALESCE(a, b, 0) FROM t1 ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		wantVals := []int64{10, 200, 0, 40}
		for i, w := range wantVals {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][1], w)
			}
		}
	})

	t.Run("coalesce_all_null_returns_fallback", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COALESCE(a, b, -1) FROM t1 WHERE id = 3")
		if len(rows) != 1 || toInt64(rows[0][0]) != -1 {
			t.Fatalf("want -1, got %v", rows)
		}
	})

	t.Run("coalesce_in_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(COALESCE(a, 0)) FROM t1")
		if len(rows) != 1 || toInt64(rows[0][0]) != 50 {
			t.Fatalf("want 50 (10+0+0+40), got %v", rows)
		}
	})

	t.Run("coalesce_in_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE COALESCE(a, 0) > 0 ORDER BY id")
		wantIDs := []int64{1, 4}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d rows, got %d", len(wantIDs), len(rows))
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})
}

func TestFDB_IsDistinctFromJavaPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "distfrom1",
		"CREATE TABLE t1 (id INTEGER, col1 INTEGER, col2 INTEGER, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 1)",
		"INSERT INTO t1 VALUES (2, 10, NULL)",
		"INSERT INTO t1 VALUES (3, 10, 3)",
		"INSERT INTO t1 VALUES (4, 10, NULL)",
		"INSERT INTO t1 VALUES (5, 10, 5)",
		"INSERT INTO t1 VALUES (6, 20, NULL)",
		"INSERT INTO t1 VALUES (7, 20, NULL)",
		"INSERT INTO t1 VALUES (8, 20, NULL)",
		"INSERT INTO t1 VALUES (9, 20, 9)",
		"INSERT INTO t1 VALUES (10, 20, 10)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("is_distinct_from_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE col2 IS DISTINCT FROM NULL ORDER BY id")
		wantIDs := []int64{1, 3, 5, 9, 10}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("is_distinct_from_value", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE col1 IS DISTINCT FROM 10 ORDER BY id")
		wantIDs := []int64{6, 7, 8, 9, 10}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("null_distinct_from_null_is_false", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE NULL IS DISTINCT FROM NULL")
		if len(rows) != 0 {
			t.Fatalf("NULL IS DISTINCT FROM NULL should be false, got %d rows", len(rows))
		}
	})

	t.Run("value_distinct_from_same_value_is_false", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE 10 IS DISTINCT FROM 10")
		if len(rows) != 0 {
			t.Fatalf("10 IS DISTINCT FROM 10 should be false, got %d rows", len(rows))
		}
	})

	t.Run("not_distinct_from_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE col2 IS NOT DISTINCT FROM NULL ORDER BY id")
		wantIDs := []int64{2, 4, 6, 7, 8}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("not_distinct_from_value", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE col1 IS NOT DISTINCT FROM 20 ORDER BY id")
		wantIDs := []int64{6, 7, 8, 9, 10}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("null_not_distinct_from_null_is_true", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT COUNT(*) FROM t1 WHERE NULL IS NOT DISTINCT FROM NULL")
		if len(rows) != 1 || toInt64(rows[0][0]) != 10 {
			t.Fatalf("NULL IS NOT DISTINCT FROM NULL should be true (all 10 rows), got %v", rows)
		}
	})

	t.Run("reversed_operand_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE NULL IS DISTINCT FROM col2 ORDER BY id")
		wantIDs := []int64{1, 3, 5, 9, 10}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("distinct_from_with_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT col1, COUNT(*) FROM t1
			 WHERE col2 IS DISTINCT FROM NULL
			 GROUP BY col1 ORDER BY col1`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[0][1]) != 3 {
			t.Errorf("group 10: got %v, want (10, 3)", rows[0])
		}
		if toInt64(rows[1][0]) != 20 || toInt64(rows[1][1]) != 2 {
			t.Errorf("group 20: got %v, want (20, 2)", rows[1])
		}
	})
}

// TestFDB_NullOperatorPatterns — Java null-operator-tests.yamsql patterns
func TestFDB_NullOperatorPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nullop", "CREATE TABLE null_op(id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO null_op VALUES
		(1, 10, 1), (2, 10, 2), (3, 10, 3), (4, 10, 4), (5, 10, 5),
		(6, 20, 6), (7, 20, 7), (8, 20, 8), (9, 20, 9), (10, 20, 10),
		(11, 20, 11), (12, 20, 12), (13, 20, 13)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("nested_derived_is_null_returns_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM (SELECT * FROM (SELECT * FROM null_op) AS x WHERE id IS NULL) AS y")
		if len(rows) != 0 {
			t.Errorf("ID IS NULL on non-null PK should return 0 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("nested_derived_is_not_null_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM (SELECT * FROM (SELECT * FROM null_op) AS x WHERE id IS NOT NULL) AS y")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 13 {
			t.Errorf("COUNT(*) of IS NOT NULL on non-null PK should be 13, got %v", rows[0][0])
		}
	})

	t.Run("null_insert_and_filter", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO null_op(id, col1) VALUES (100, NULL)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id FROM null_op WHERE col1 IS NULL ORDER BY id")
		if len(rows) != 1 {
			t.Fatalf("want 1 row with NULL col1, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 100 {
			t.Errorf("want id=100, got %v", rows[0][0])
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM null_op WHERE id = 100"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
	})

	t.Run("is_null_in_case_when", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN col2 > 10 THEN 'high' ELSE 'low' END
			FROM null_op ORDER BY id
		`)
		if len(rows) != 13 {
			t.Fatalf("want 13 rows, got %d", len(rows))
		}
		lowCount := 0
		highCount := 0
		for _, r := range rows {
			v := fmt.Sprintf("%v", r[1])
			if v == "low" {
				lowCount++
			} else if v == "high" {
				highCount++
			}
		}
		if lowCount != 10 {
			t.Errorf("want 10 'low' (col2 1-10), got %d", lowCount)
		}
		if highCount != 3 {
			t.Errorf("want 3 'high' (col2 11-13), got %d", highCount)
		}
	})
}

// TestFDB_NullSafeComparisons — NULL behavior in comparisons
func TestFDB_NullSafeComparisons(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nscmp", "CREATE TABLE ns_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ns_t VALUES (1, 10), (2, 20), (3, NULL)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("null_eq_returns_no_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM ns_t WHERE val = NULL")
		if len(rows) != 0 {
			t.Errorf("val = NULL should return 0 rows (NULL = NULL is UNKNOWN), got %d: %v", len(rows), rows)
		}
	})

	t.Run("null_ne_returns_no_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM ns_t WHERE val <> NULL")
		if len(rows) != 0 {
			t.Errorf("val <> NULL should return 0 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("is_null_finds_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM ns_t WHERE val IS NULL")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Errorf("IS NULL should find id=3, got %v", rows)
		}
	})

	t.Run("is_not_null_excludes_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM ns_t WHERE val IS NOT NULL ORDER BY id")
		if len(rows) != 2 {
			t.Errorf("IS NOT NULL should find 2 rows, got %d", len(rows))
		}
	})

	t.Run("coalesce_null_replacement", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, COALESCE(val, 0) FROM ns_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[2][1]) != 0 {
			t.Errorf("COALESCE(NULL, 0) should be 0, got %v", rows[2][1])
		}
	})

	t.Run("sum_ignores_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM ns_t")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 30 {
			t.Errorf("SUM should be 10+20=30 (NULL ignored), got %v", rows[0][0])
		}
	})

	t.Run("count_star_includes_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM ns_t")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("COUNT(*) includes NULL rows, should be 3, got %v", rows[0][0])
		}
	})

	t.Run("count_column_excludes_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(val) FROM ns_t")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("COUNT(val) excludes NULL, should be 2, got %v", rows[0][0])
		}
	})
}

// TestFDB_CoalesceChain — COALESCE with multiple fallbacks
func TestFDB_CoalesceChain(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "coalch", "CREATE TABLE cc_t(id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cc_t VALUES
		(1, 10, 20, 30), (2, NULL, 20, 30), (3, NULL, NULL, 30), (4, NULL, NULL, NULL)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("coalesce_three_args", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, COALESCE(a, b, c) FROM cc_t ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 10 {
			t.Errorf("id=1: COALESCE(10,20,30)=10, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 20 {
			t.Errorf("id=2: COALESCE(NULL,20,30)=20, got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 30 {
			t.Errorf("id=3: COALESCE(NULL,NULL,30)=30, got %v", rows[2][1])
		}
		if rows[3][1] != nil {
			t.Errorf("id=4: COALESCE(NULL,NULL,NULL)=NULL, got %v", rows[3][1])
		}
	})

	t.Run("coalesce_with_constant", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, COALESCE(a, b, c, 999) FROM cc_t ORDER BY id")
		if toInt64(rows[3][1]) != 999 {
			t.Errorf("id=4: COALESCE(NULL,NULL,NULL,999)=999, got %v", rows[3][1])
		}
	})

	t.Run("coalesce_in_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(COALESCE(a, 0)) FROM cc_t")
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("SUM(COALESCE(a,0)) = 10+0+0+0 = 10, got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereWithNullAndNotNull — WHERE IS NULL and IS NOT NULL combined
func TestFDB_WhereWithNullAndNotNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wnn", "CREATE TABLE wnn_t(id BIGINT, a BIGINT, b STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wnn_t VALUES
		(1, 10, 'x'), (2, NULL, 'y'), (3, 30, NULL), (4, NULL, NULL), (5, 50, 'z')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("a_null_and_b_not_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wnn_t WHERE a IS NULL AND b IS NOT NULL ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want id=2 (a=NULL, b='y'), got %v", rows)
		}
	})

	t.Run("both_not_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wnn_t WHERE a IS NOT NULL AND b IS NOT NULL")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 (id=1: a=10,b='x' + id=5: a=50,b='z'), got %v", rows[0][0])
		}
	})

	t.Run("either_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wnn_t WHERE a IS NULL OR b IS NULL")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 (id=2:a=null, id=3:b=null, id=4:both), got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereIsNullIsNotNull — IS NULL and IS NOT NULL filters
func TestFDB_WhereIsNullIsNotNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "winn",
		"CREATE TABLE winn_t(id BIGINT, val BIGINT, label STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO winn_t VALUES (1,10,'x'),(2,null,'y'),(3,30,null),(4,null,null)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("val_is_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM winn_t WHERE val IS NULL ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (ids 2,4), got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 2 || toInt64(rows[1][0]) != 4 {
			t.Errorf("want [2 4], got [%v %v]", rows[0][0], rows[1][0])
		}
	})

	t.Run("val_is_not_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM winn_t WHERE val IS NOT NULL ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (ids 1,3), got %d", len(rows))
		}
	})

	t.Run("both_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM winn_t WHERE val IS NULL AND label IS NULL")
		if len(rows) != 1 || toInt64(rows[0][0]) != 4 {
			t.Errorf("want [4], got %v", rows)
		}
	})

	t.Run("either_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM winn_t WHERE val IS NULL OR label IS NULL")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 (ids 2,3,4), got %v", rows[0][0])
		}
	})
}

// TestFDB_CoalesceThreeColumnFallback — COALESCE(a, b, c) cascading NULL fallback
func TestFDB_CoalesceThreeColumnFallback(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cchn",
		"CREATE TABLE cchn_t(id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cchn_t VALUES
		(1,10,20,30),(2,null,20,30),(3,null,null,30),(4,null,null,null)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("coalesce_three_cols", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, COALESCE(a, b, c) FROM cchn_t ORDER BY id")
		want := []any{int64(10), int64(20), int64(30), nil}
		for i, w := range want {
			if w == nil {
				if rows[i][1] != nil {
					t.Errorf("id %d: want NULL, got %v", i+1, rows[i][1])
				}
			} else if toInt64(rows[i][1]) != w.(int64) {
				t.Errorf("id %d: want %v, got %v", i+1, w, rows[i][1])
			}
		}
	})

	t.Run("coalesce_with_constant", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COALESCE(a, b, c, 0) FROM cchn_t WHERE id = 4")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("want 0 fallback, got %v", rows[0][0])
		}
	})
}
