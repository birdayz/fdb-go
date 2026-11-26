package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_OrPredicateFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "orpred",
		"CREATE TABLE vals (id BIGINT, v BIGINT, PRIMARY KEY (id))")

	for i := 1; i <= 10; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO vals VALUES (%d, %d)", i, i*10)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("or_two_ranges", func(t *testing.T) {
		// Java: WHERE col >= X OR col <= Y (produces UNION of scans)
		// Go: filter over full scan (correct, different plan)
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM vals WHERE v <= 20 OR v >= 90 ORDER BY id")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, id)
		}
		// v<=20: id=1(10),2(20). v>=90: id=9(90),10(100).
		want := []int64{1, 2, 9, 10}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %d, want %d", i, got[i], w)
			}
		}
	})

	t.Run("or_equality", func(t *testing.T) {
		// WHERE v = 30 OR v = 70
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM vals WHERE v = 30 OR v = 70 ORDER BY id")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, id)
		}
		want := []int64{3, 7}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %d, want %d", i, got[i], w)
			}
		}
	})
}

func TestFDB_AndRangePredicateWithIndex(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "andrange",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX i1 ON t1 (col1)")

	for _, r := range []struct {
		id, col1, col2 int
	}{
		{1, 10, 1},
		{2, 10, 2},
		{3, 10, 3},
		{4, 10, 4},
		{5, 10, 5},
		{6, 20, 6},
		{7, 20, 7},
		{8, 20, 8},
		{9, 20, 9},
		{10, 20, 10},
		{11, 20, 11},
		{12, 20, 12},
		{13, 20, 13},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO t1 VALUES (%d, %d, %d)", r.id, r.col1, r.col2,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("equality_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, col1, col2 FROM t1 WHERE col1 = 20 ORDER BY id")
		if len(rows) != 8 {
			t.Fatalf("want 8 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 6 {
			t.Errorf("first row id: got %v, want 6", rows[0][0])
		}
	})

	t.Run("and_range_both_bounds", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE col1 >= 10 AND col1 <= 20 ORDER BY id")
		if len(rows) != 13 {
			t.Fatalf("want 13 rows, got %d", len(rows))
		}
	})

	t.Run("and_equality_plus_secondary_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id, col1, col2 FROM t1 WHERE col1 = 20 AND col2 > 10 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows (ids 11,12,13), got %d", len(rows))
		}
		if rows[0][0].(int64) != 11 {
			t.Errorf("first row id: got %v, want 11", rows[0][0])
		}
	})

	t.Run("not_equal_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id, col1 FROM t1 WHERE col1 <> 10 ORDER BY id")
		if len(rows) != 8 {
			t.Fatalf("want 8 rows, got %d", len(rows))
		}
		for _, r := range rows {
			if r[1].(int64) == 10 {
				t.Errorf("unexpected col1=10 in row %v", r)
			}
		}
	})

	_ = ctx
}

func TestFDB_BetweenOperator(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "between1",
		"CREATE TABLE t1 (id INTEGER, col1 INTEGER, col2 INTEGER, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 1)",
		"INSERT INTO t1 VALUES (2, 10, 2)",
		"INSERT INTO t1 VALUES (3, 10, 3)",
		"INSERT INTO t1 VALUES (4, 10, 4)",
		"INSERT INTO t1 VALUES (5, 10, 5)",
		"INSERT INTO t1 VALUES (6, 20, 6)",
		"INSERT INTO t1 VALUES (7, 20, 7)",
		"INSERT INTO t1 VALUES (8, 20, 8)",
		"INSERT INTO t1 VALUES (9, 20, 9)",
		"INSERT INTO t1 VALUES (10, 20, 10)",
		"INSERT INTO t1 VALUES (11, 30, 11)",
		"INSERT INTO t1 VALUES (12, 30, 12)",
		"INSERT INTO t1 VALUES (13, 30, 13)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("between_range", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE col2 BETWEEN 4 AND 6 ORDER BY id")
		wantIDs := []int64{4, 5, 6}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d rows, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			got := toInt64(rows[i][0])
			if got != w {
				t.Errorf("row %d: got %d, want %d", i, got, w)
			}
		}
	})

	t.Run("between_single_value", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE col2 BETWEEN 4 AND 4")
		if len(rows) != 1 || toInt64(rows[0][0]) != 4 {
			t.Fatalf("want [4], got %v", rows)
		}
	})

	t.Run("between_empty_range", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE col2 BETWEEN 4 AND 3")
		if len(rows) != 0 {
			t.Fatalf("want 0 rows for reversed range, got %d", len(rows))
		}
	})

	t.Run("not_between", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE col2 NOT BETWEEN 2 AND 12 ORDER BY id")
		wantIDs := []int64{1, 13}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d rows, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("not_between_reversed_returns_all", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM t1 WHERE col2 NOT BETWEEN 12 AND 2 ORDER BY id")
		if len(rows) != 13 {
			t.Fatalf("want 13 rows for NOT BETWEEN with reversed range, got %d", len(rows))
		}
	})

	t.Run("between_or_between", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE col2 BETWEEN 2 AND 4 OR col2 BETWEEN 6 AND 7 ORDER BY id")
		wantIDs := []int64{2, 3, 4, 6, 7}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d rows, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("between_with_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1, COUNT(*) FROM t1 WHERE col2 BETWEEN 3 AND 8 GROUP BY col1 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[0][1]) != 3 {
			t.Errorf("group 10: want count=3, got %v", rows[0])
		}
		if toInt64(rows[1][0]) != 20 || toInt64(rows[1][1]) != 3 {
			t.Errorf("group 20: want count=3, got %v", rows[1])
		}
	})

	t.Run("between_in_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1, SUM(col2) AS s FROM t1 GROUP BY col1 HAVING SUM(col2) BETWEEN 10 AND 20 ORDER BY col1")
		if len(rows) != 1 {
			t.Fatalf("want 1 group (col1=10, sum=15), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("want col1=10, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 15 {
			t.Errorf("want sum=15, got %v", rows[0][1])
		}
	})
}

func TestFDB_ComplexWhereConditions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "cxwhere1",
		"CREATE TABLE t1 (id BIGINT, a BIGINT, b STRING, c BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 'foo', 100)",
		"INSERT INTO t1 VALUES (2, 20, 'bar', 200)",
		"INSERT INTO t1 VALUES (3, 30, 'foo', 300)",
		"INSERT INTO t1 VALUES (4, 10, 'bar', 400)",
		"INSERT INTO t1 VALUES (5, 20, 'baz', NULL)",
		"INSERT INTO t1 VALUES (6, NULL, 'foo', 600)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("and_or_combined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE (a = 10 AND b = 'foo') OR (a = 20 AND b = 'bar') ORDER BY id")
		wantIDs := []int64{1, 2}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("not_equal_and_is_not_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE a <> 10 AND c IS NOT NULL ORDER BY id")
		wantIDs := []int64{2, 3}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("in_list_with_null_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE a IN (10, 30) ORDER BY id")
		wantIDs := []int64{1, 3, 4}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
	})

	t.Run("between_and_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE c BETWEEN 100 AND 400 AND b LIKE 'f%' ORDER BY id")
		wantIDs := []int64{1, 3}
		if len(rows) != len(wantIDs) {
			t.Fatalf("want %d, got %d: %v", len(wantIDs), len(rows), rows)
		}
	})

	t.Run("null_safe_comparison", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id FROM t1 WHERE c IS NULL ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 5 {
			t.Fatalf("want [5], got %v", rows)
		}
	})

	t.Run("complex_having_with_case", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT b, COUNT(*),
				CASE WHEN SUM(COALESCE(c, 0)) > 500 THEN 'high' ELSE 'low' END
			 FROM t1
			 WHERE a IS NOT NULL
			 GROUP BY b
			 ORDER BY b`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "bar" || toInt64(rows[0][1]) != 2 {
			t.Errorf("bar: got %v", rows[0])
		}
		if rows[1][0].(string) != "baz" || toInt64(rows[1][1]) != 1 {
			t.Errorf("baz: got %v", rows[1])
		}
		if rows[2][0].(string) != "foo" || toInt64(rows[2][1]) != 2 {
			t.Errorf("foo: got %v", rows[2])
		}
	})

	t.Run("count_distinct_values_workaround", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT DISTINCT a FROM t1 WHERE a IS NOT NULL ORDER BY a")
		if len(rows) != 3 {
			t.Fatalf("want 3 distinct a values (10,20,30), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[1][0]) != 20 || toInt64(rows[2][0]) != 30 {
			t.Errorf("got %v, %v, %v", rows[0][0], rows[1][0], rows[2][0])
		}
	})

	_ = ctx
}

// TestFDB_InPredicatePatterns — Java in-predicate.yamsql patterns
func TestFDB_InPredicatePatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "inpred", "CREATE TABLE in_t(a BIGINT, b BIGINT, c STRING, d BIGINT, PRIMARY KEY(a))")
	if _, err := db.ExecContext(ctx, `INSERT INTO in_t VALUES
		(0, 9, 'foo', 100),
		(1, 8, 'bar', 200),
		(2, 7, 'doe', 300),
		(3, 6, 'arc', 400),
		(4, 5, 'per', 500),
		(5, 4, 'doe', 600),
		(6, 3, 'foo', 700),
		(7, 2, 'arc', 800),
		(8, 1, 'bar', 900),
		(9, 0, 'doe', 1000)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("in_list_long", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a, b FROM in_t WHERE b IN (1, 3, 5, 7) ORDER BY a")
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d: %v", len(rows), rows)
		}
		wantA := []int64{2, 4, 6, 8}
		for i, wa := range wantA {
			if toInt64(rows[i][0]) != wa {
				t.Errorf("row %d: want a=%d, got %v", i, wa, rows[i][0])
			}
		}
	})

	t.Run("in_singleton", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a, b FROM in_t WHERE b IN (6)")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want a=3, got %v", rows[0][0])
		}
	})

	t.Run("in_no_match", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a, b FROM in_t WHERE b IN (10, 33, 66)")
		if len(rows) != 0 {
			t.Errorf("want 0 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("in_string", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM in_t WHERE c IN ('bar', 'doe') ORDER BY a")
		if len(rows) != 5 {
			t.Fatalf("want 5 rows, got %d: %v", len(rows), rows)
		}
		wantA := []int64{1, 2, 5, 8, 9}
		for i, wa := range wantA {
			if toInt64(rows[i][0]) != wa {
				t.Errorf("row %d: want a=%d, got %v", i, wa, rows[i][0])
			}
		}
	})

	t.Run("not_in", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM in_t WHERE c NOT IN ('foo', 'bar', 'doe', 'arc') ORDER BY a")
		if len(rows) != 1 {
			t.Fatalf("want 1 row (per), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want a=4, got %v", rows[0][0])
		}
	})

	t.Run("in_with_arithmetic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a, b FROM in_t WHERE b IN (1 + 0, 3 + 0, 5, 7) ORDER BY a")
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d", len(rows))
		}
	})

	t.Run("constant_in_returns_all", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM in_t WHERE 1 IN (1, 2, 3) ORDER BY a")
		if len(rows) != 10 {
			t.Errorf("constant TRUE IN should return all 10 rows, got %d", len(rows))
		}
	})

	t.Run("constant_in_returns_none", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM in_t WHERE 1 IN (2, 3)")
		if len(rows) != 0 {
			t.Errorf("constant FALSE IN should return 0 rows, got %d", len(rows))
		}
	})

	t.Run("in_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c, COUNT(*) FROM in_t WHERE c IN ('foo', 'bar') GROUP BY c ORDER BY c")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "bar" {
			t.Errorf("first group should be 'bar', got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 2 {
			t.Errorf("bar count should be 2, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "foo" {
			t.Errorf("second group should be 'foo', got %v", rows[1][0])
		}
		if toInt64(rows[1][1]) != 2 {
			t.Errorf("foo count should be 2, got %v", rows[1][1])
		}
	})
}

// TestFDB_BooleanThreeValueLogic — Java boolean.yamsql patterns
func TestFDB_BooleanThreeValueLogic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "bool3v", "CREATE TABLE lb(a BIGINT, b BOOLEAN, PRIMARY KEY(a))")
	if _, err := db.ExecContext(ctx, "INSERT INTO lb VALUES (1, true), (2, false)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("where_b_eq_true", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM lb WHERE b = true")
		if len(rows) != 1 || toInt64(rows[0][0]) != 1 {
			t.Errorf("want a=1, got %v", rows)
		}
	})

	t.Run("where_b_eq_false", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM lb WHERE b = false")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want a=2, got %v", rows)
		}
	})

	t.Run("where_b_ne_true", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM lb WHERE b <> TRUE")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want a=2, got %v", rows)
		}
	})

	t.Run("where_b_is_true", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM lb WHERE b IS TRUE")
		if len(rows) != 1 || toInt64(rows[0][0]) != 1 {
			t.Errorf("want a=1, got %v", rows)
		}
	})

	t.Run("where_b_is_false", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM lb WHERE b IS FALSE")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want a=2, got %v", rows)
		}
	})

	t.Run("where_b_is_not_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a FROM lb WHERE b IS NOT NULL ORDER BY a")
		if len(rows) != 2 {
			t.Errorf("want 2 rows (both non-null), got %d: %v", len(rows), rows)
		}
	})

	t.Run("select_b_eq_true", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b = true FROM lb ORDER BY a")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "true" {
			t.Errorf("row 0: b=true, b=true should be true, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "false" {
			t.Errorf("row 1: b=false, b=true should be false, got %v", rows[1][0])
		}
	})

	t.Run("select_not_b", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT NOT b FROM lb ORDER BY a")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "false" {
			t.Errorf("NOT true should be false, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "true" {
			t.Errorf("NOT false should be true, got %v", rows[1][0])
		}
	})

	t.Run("boolean_and_or", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b AND TRUE, b OR FALSE FROM lb ORDER BY a")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "true" {
			t.Errorf("true AND TRUE should be true, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[0][1]) != "true" {
			t.Errorf("true OR FALSE should be true, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "false" {
			t.Errorf("false AND TRUE should be false, got %v", rows[1][0])
		}
		if fmt.Sprintf("%v", rows[1][1]) != "false" {
			t.Errorf("false OR FALSE should be false, got %v", rows[1][1])
		}
	})

	t.Run("count_boolean_groups", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, COUNT(*) FROM lb GROUP BY b ORDER BY b")
		if len(rows) < 2 {
			t.Fatalf("want at least 2 groups, got %d: %v", len(rows), rows)
		}
		t.Logf("boolean GROUP BY: %v", rows)
	})
}
