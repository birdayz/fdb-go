package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_TwoDerivedTablesJoined(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "2dt",
		"CREATE TABLE a (ida BIGINT, a1 BIGINT, PRIMARY KEY (ida)) "+
			"CREATE TABLE b (idb BIGINT, b1 BIGINT, PRIMARY KEY (idb))")

	if _, err := db.ExecContext(ctx, "INSERT INTO a VALUES (1, 100)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO b VALUES (4, 200)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Java: select sq2.y, sq1.x from (select ida as x from a) sq1, (select idb as y from b) sq2
	rows, err := db.QueryContext(ctx,
		"SELECT sq2.y, sq1.x FROM (SELECT ida AS x FROM a) sq1, (SELECT idb AS y FROM b) sq2")
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	var y, x int64
	if !rows.Next() {
		t.Fatal("expected 1 row")
	}
	if err := rows.Scan(&y, &x); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if y != 4 || x != 1 {
		t.Errorf("got y=%d x=%d, want y=4 x=1", y, x)
	}
	if rows.Next() {
		t.Error("expected exactly 1 row")
	}
}

func TestFDB_NestedDerivedTableNullFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nestdt",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id))")

	for i := 1; i <= 5; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t1 VALUES (%d, %d)", i, i*10)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("nested_is_null_empty", func(t *testing.T) {
		// Java: select * from (select * from (select * from T1) as x where ID is null) as y
		// All IDs are NOT NULL, so result is empty
		rows, err := db.QueryContext(ctx,
			"SELECT * FROM (SELECT * FROM (SELECT * FROM t1) AS x WHERE id IS NULL) AS y")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			count++
		}
		if count != 0 {
			t.Errorf("got %d rows, want 0", count)
		}
	})

	t.Run("nested_is_not_null_all", func(t *testing.T) {
		// Java: select count(*) from (select * from (select * from T1) as x where ID is not null) as y
		var cnt int64
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM (SELECT * FROM (SELECT * FROM t1) AS x WHERE id IS NOT NULL) AS y").Scan(&cnt)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if cnt != 5 {
			t.Errorf("got %d, want 5", cnt)
		}
	})
}

func TestFDB_GroupByDerivedTableAgg(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbdt",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

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

	t.Run("max_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT MAX(id) FROM t1 GROUP BY col1 ORDER BY MAX(id)")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 5 {
			t.Errorf("row 0: got %v, want 5", rows[0][0])
		}
		if rows[1][0].(int64) != 13 {
			t.Errorf("row 1: got %v, want 13", rows[1][0])
		}
	})

	t.Run("having_min_and_equality", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MAX(id) FROM t1 GROUP BY col1 HAVING MIN(id) > 0 AND col1 = 20")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0].(int64) != 13 {
			t.Errorf("got %v, want 13", rows[0][0])
		}
	})

	t.Run("ungrouped_col_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT * FROM t1 GROUP BY col1")
		if err == nil {
			t.Fatal("expected error for ungrouped columns in SELECT *")
		}
	})

	t.Run("undefined_col_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT bla FROM t1 GROUP BY col1")
		if err == nil {
			t.Fatal("expected error for undefined column")
		}
	})

	t.Run("derived_table_max", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT MAX(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY MAX(x.col2)")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 5 {
			t.Errorf("row 0: got %v, want 5", rows[0][0])
		}
		if rows[1][0].(int64) != 13 {
			t.Errorf("row 1: got %v, want 13", rows[1][0])
		}
	})

	t.Run("derived_table_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT COUNT(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY COUNT(x.col2)")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 5 {
			t.Errorf("row 0: got %v, want 5", rows[0][0])
		}
		if rows[1][0].(int64) != 8 {
			t.Errorf("row 1: got %v, want 8", rows[1][0])
		}
	})

	t.Run("derived_table_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT SUM(x.col2) FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY SUM(x.col2)")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 15 {
			t.Errorf("row 0: got %v, want 15", rows[0][0])
		}
		if rows[1][0].(int64) != 76 {
			t.Errorf("row 1: got %v, want 76", rows[1][0])
		}
	})

	_ = ctx
}

func TestFDB_DerivedTableArithmeticOnAggregates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtagg",
		"CREATE TABLE emp (id BIGINT, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))")

	for _, e := range []struct {
		id, did, sal int
	}{
		{1, 1, 100}, {2, 1, 200}, {3, 2, 300},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, %d, %d)", e.id, e.did, e.sal,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("direct_sum_div_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT dept_id, SUM(salary) / COUNT(*) AS avg FROM emp GROUP BY dept_id ORDER BY dept_id")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		t.Logf("direct: %v", rows)
		if rows[0][1].(int64) != 150 {
			t.Errorf("dept 1 avg: got %v, want 150", rows[0][1])
		}
	})

	t.Run("derived_table_sum_div_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT sub.dept_id, sub.avg_sal
			 FROM (SELECT dept_id, SUM(salary) / COUNT(*) AS avg_sal
			       FROM emp GROUP BY dept_id) sub
			 ORDER BY sub.dept_id`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		t.Logf("derived: %v", rows)
		if rows[0][1] == nil {
			t.Errorf("dept 1 avg_sal is NULL — arithmetic on aggregates lost in derived table")
		} else if rows[0][1].(int64) != 150 {
			t.Errorf("dept 1 avg: got %v, want 150", rows[0][1])
		}
	})

	_ = ctx
}

func TestFDB_DerivedTableEdgeCases(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtedge",
		"CREATE TABLE items (id BIGINT, category STRING, price BIGINT, qty BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id       int
		cat      string
		price, q int
	}{
		{1, "A", 10, 5},
		{2, "A", 20, 3},
		{3, "A", 30, 1},
		{4, "B", 100, 2},
		{5, "B", 200, 1},
		{6, "C", 50, 10},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO items VALUES (%d, '%s', %d, %d)", r.id, r.cat, r.price, r.q,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("nested_derived_with_aggregate_arithmetic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT sub.category, sub.total_value
			 FROM (SELECT category, SUM(price * qty) AS total_value
			       FROM items GROUP BY category) sub
			 ORDER BY sub.total_value DESC`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][1] == nil {
			t.Fatalf("SUM(price*qty) is NULL")
		}
		if rows[0][0].(string) != "C" || rows[0][1].(int64) != 500 {
			t.Errorf("row 0: got %v, want [C, 500]", rows[0])
		}
		if rows[2][0].(string) != "A" || rows[2][1].(int64) != 140 {
			t.Errorf("row 2: got %v, want [A, 140]", rows[2])
		}
	})

	t.Run("derived_table_with_having_in_inner", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT sub.category, sub.cnt
			 FROM (SELECT category, COUNT(*) AS cnt
			       FROM items GROUP BY category HAVING COUNT(*) >= 2) sub
			 ORDER BY sub.cnt DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows (A=3, B=2), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || rows[0][1].(int64) != 3 {
			t.Errorf("row 0: got %v, want [A, 3]", rows[0])
		}
	})

	t.Run("derived_table_with_where_and_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT sub.category, sub.avg_price
			 FROM (SELECT category, SUM(price) / COUNT(*) AS avg_price
			       FROM items WHERE qty > 1 GROUP BY category) sub
			 ORDER BY sub.category`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][1] == nil {
			t.Fatalf("avg_price is NULL for category %v", rows[0][0])
		}
		if rows[0][0].(string) != "A" || rows[0][1].(int64) != 15 {
			t.Errorf("row 0: got %v, want [A, 15] (10+20)/2", rows[0])
		}
	})

	t.Run("join_two_derived_tables", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT a.category, a.total, b.cnt
			 FROM (SELECT category, SUM(price) AS total FROM items GROUP BY category) a,
			      (SELECT category, COUNT(*) AS cnt FROM items GROUP BY category) b
			 WHERE a.category = b.category
			 ORDER BY a.category`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || rows[0][1].(int64) != 60 || rows[0][2].(int64) != 3 {
			t.Errorf("row 0: got %v, want [A, 60, 3]", rows[0])
		}
	})

	_ = ctx
}

func TestFDB_DerivedTableJoinWithAggExpr(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "dtjnagg",
		"CREATE TABLE products (id BIGINT, name STRING, category STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE orders (id BIGINT, product_id BIGINT, qty BIGINT, unit_price BIGINT, PRIMARY KEY (id))")

	for _, p := range []struct {
		id  int
		n   string
		cat string
	}{
		{1, "Widget", "A"}, {2, "Gadget", "A"}, {3, "Doohickey", "B"},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO products VALUES (%d, '%s', '%s')", p.id, p.n, p.cat))
	}
	for _, o := range []struct {
		id, pid, qty, price int
	}{
		{1, 1, 10, 5}, {2, 1, 20, 5}, {3, 2, 5, 10}, {4, 3, 100, 1},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO orders VALUES (%d, %d, %d, %d)", o.id, o.pid, o.qty, o.price))
	}

	t.Run("join_with_agg_derived_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT p.name, s.total
			 FROM products p,
			      (SELECT product_id, SUM(qty * unit_price) AS total
			       FROM orders GROUP BY product_id) s
			 WHERE p.id = s.product_id
			 ORDER BY s.total DESC`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Widget" || rows[0][1].(int64) != 150 {
			t.Errorf("row 0: got %v, want [Widget, 150]", rows[0])
		}
		if rows[1][0].(string) != "Doohickey" || rows[1][1].(int64) != 100 {
			t.Errorf("row 1: got %v, want [Doohickey, 100]", rows[1])
		}
		if rows[2][0].(string) != "Gadget" || rows[2][1].(int64) != 50 {
			t.Errorf("row 2: got %v, want [Gadget, 50]", rows[2])
		}
	})

	t.Run("category_revenue_via_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT p.category, SUM(o.qty * o.unit_price) AS revenue
			 FROM products p, orders o
			 WHERE p.id = o.product_id
			 GROUP BY p.category
			 ORDER BY p.category`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || rows[0][1].(int64) != 200 {
			t.Errorf("A: got %v, want [A, 200] (150+50)", rows[0])
		}
		if rows[1][0].(string) != "B" || rows[1][1].(int64) != 100 {
			t.Errorf("B: got %v, want [B, 100]", rows[1])
		}
	})

	_ = ctx
}

func TestFDB_NestedDerivedWithIsNullNotNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "nestdnull",
		"CREATE TABLE t (id BIGINT, label STRING, val BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id int
		l  string
		v  *int64
	}{
		{1, "A", ptr(int64(10))},
		{2, "A", nil},
		{3, "B", ptr(int64(30))},
		{4, "B", ptr(int64(40))},
	} {
		if r.v == nil {
			db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, label) VALUES (%d, '%s')", r.id, r.l))
		} else {
			db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, '%s', %d)", r.id, r.l, *r.v))
		}
	}

	t.Run("nested_is_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT * FROM (SELECT * FROM (SELECT * FROM t) x WHERE id IS NULL) y`)
		if len(rows) != 0 {
			t.Fatalf("want 0 rows (id is NOT NULL), got %d", len(rows))
		}
	})

	t.Run("nested_is_not_null_count", func(t *testing.T) {
		var cnt int64
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM (SELECT * FROM (SELECT * FROM t) x WHERE val IS NOT NULL) y`).Scan(&cnt)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if cnt != 3 {
			t.Errorf("got %d, want 3 (id 1,3,4 have non-null val)", cnt)
		}
	})

	t.Run("nested_agg_over_filtered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT label, SUM(val)
			 FROM (SELECT * FROM t WHERE val IS NOT NULL) sub
			 GROUP BY label ORDER BY label`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][1].(int64) != 10 {
			t.Errorf("A: got %v, want 10", rows[0][1])
		}
		if rows[1][1].(int64) != 70 {
			t.Errorf("B: got %v, want 70 (30+40)", rows[1][1])
		}
	})

	_ = ctx
}
