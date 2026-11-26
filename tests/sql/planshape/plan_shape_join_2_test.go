package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_SelectStarWithJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "selstar",
		"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))")

	for _, d := range []struct {
		id   int
		name string
	}{
		{1, "Engineering"}, {2, "Sales"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO dept VALUES (%d, '%s')", d.id, d.name,
		)); err != nil {
			t.Fatalf("INSERT dept: %v", err)
		}
	}
	for _, e := range []struct {
		id, did, sal int
		name         string
	}{
		{1, 1, 100, "Alice"}, {2, 1, 120, "Bob"}, {3, 2, 90, "Charlie"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, '%s', %d, %d)", e.id, e.name, e.did, e.sal,
		)); err != nil {
			t.Fatalf("INSERT emp: %v", err)
		}
	}

	t.Run("select_star_single_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM dept ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][1].(string) != "Engineering" {
			t.Errorf("got %v, want Engineering", rows[0][1])
		}
	})

	t.Run("inner_join_with_projection", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT e.name, d.name AS dept_name, e.salary
			 FROM emp e, dept d
			 WHERE e.dept_id = d.id
			 ORDER BY e.name`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if rows[0][0].(string) != "Alice" || rows[0][1].(string) != "Engineering" {
			t.Errorf("row 0: got %v, want [Alice, Engineering, ...]", rows[0])
		}
		if rows[2][0].(string) != "Charlie" || rows[2][1].(string) != "Sales" {
			t.Errorf("row 2: got %v, want [Charlie, Sales, ...]", rows[2])
		}
	})

	t.Run("subquery_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name, sub.total
			 FROM dept d,
			      (SELECT dept_id, SUM(salary) AS total
			       FROM emp GROUP BY dept_id) sub
			 WHERE d.id = sub.dept_id
			 ORDER BY d.name`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Engineering" || rows[0][1].(int64) != 220 {
			t.Errorf("row 0: got %v, want [Engineering, 220]", rows[0])
		}
		if rows[1][0].(string) != "Sales" || rows[1][1].(int64) != 90 {
			t.Errorf("row 1: got %v, want [Sales, 90]", rows[1])
		}
	})

	_ = ctx
}

func TestFDB_OrPredicateWithJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "orjoin",
		"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, level STRING, PRIMARY KEY (id))")

	db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'Engineering')")
	db.ExecContext(ctx, "INSERT INTO dept VALUES (2, 'Sales')")
	db.ExecContext(ctx, "INSERT INTO dept VALUES (3, 'HR')")
	for _, e := range []struct {
		id, did   int
		name, lvl string
	}{
		{1, 1, "Alice", "senior"},
		{2, 1, "Bob", "junior"},
		{3, 2, "Charlie", "senior"},
		{4, 2, "Dave", "junior"},
		{5, 3, "Eve", "senior"},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO emp VALUES (%d, '%s', %d, '%s')", e.id, e.name, e.did, e.lvl))
	}

	t.Run("or_on_different_tables", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT e.name, d.name AS dept
			 FROM emp e, dept d
			 WHERE e.dept_id = d.id AND (d.name = 'Engineering' OR e.level = 'senior')
			 ORDER BY e.name`)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d: %v", len(rows), rows)
		}
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = r[0].(string)
		}
		t.Logf("names: %v", names)
		if names[0] != "Alice" || names[1] != "Bob" {
			t.Errorf("first two should be Alice, Bob (Engineering)")
		}
	})

	t.Run("or_same_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT e.name FROM emp e
			 WHERE e.level = 'senior' OR e.level = 'junior'
			 ORDER BY e.name`)
		if len(rows) != 5 {
			t.Fatalf("want 5 (all employees), got %d", len(rows))
		}
	})

	_ = ctx
}

func TestFDB_JoinNotInPattern(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "jnotin2",
		"CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id))")

	db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'Engineering')")
	db.ExecContext(ctx, "INSERT INTO dept VALUES (2, 'Sales')")
	db.ExecContext(ctx, "INSERT INTO dept VALUES (3, 'HR')")
	db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 'Alice', 1)")
	db.ExecContext(ctx, "INSERT INTO emp VALUES (2, 'Bob', 1)")
	db.ExecContext(ctx, "INSERT INTO emp VALUES (3, 'Charlie', 2)")

	t.Run("not_in_subquery_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"SELECT d.name FROM dept d WHERE d.id NOT IN (SELECT dept_id FROM emp)")
		if err == nil {
			t.Fatal("expected error for NOT IN (subquery)")
		}
	})

	t.Run("join_not_in_workaround", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name FROM dept d
			 WHERE NOT EXISTS (SELECT 1 FROM emp e WHERE e.dept_id = d.id)
			 ORDER BY d.name`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (HR), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "HR" {
			t.Errorf("got %v, want HR", rows[0][0])
		}
	})
}

func TestFDB_ThreeWayJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "3wjoin1",
		"CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE orders (id BIGINT, customer_id BIGINT, product_id BIGINT, qty BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE products (id BIGINT, name STRING, price BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO customers VALUES (1, 'Alice')",
		"INSERT INTO customers VALUES (2, 'Bob')",
		"INSERT INTO products VALUES (10, 'Widget', 100)",
		"INSERT INTO products VALUES (20, 'Gadget', 200)",
		"INSERT INTO orders VALUES (1, 1, 10, 5)",
		"INSERT INTO orders VALUES (2, 1, 20, 2)",
		"INSERT INTO orders VALUES (3, 2, 10, 3)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("three_table_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, p.name, o.qty
			 FROM customers c, orders o, products p
			 WHERE o.customer_id = c.id AND o.product_id = p.id
			 ORDER BY c.name, p.name`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		type row struct {
			cust, prod string
			qty        int64
		}
		want := []row{
			{"Alice", "Gadget", 2},
			{"Alice", "Widget", 5},
			{"Bob", "Widget", 3},
		}
		for i, w := range want {
			if rows[i][0].(string) != w.cust || rows[i][1].(string) != w.prod || toInt64(rows[i][2]) != w.qty {
				t.Errorf("row %d: got (%v, %v, %v), want (%s, %s, %d)",
					i, rows[i][0], rows[i][1], rows[i][2], w.cust, w.prod, w.qty)
			}
		}
	})

	t.Run("three_way_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, SUM(o.qty * p.price) AS total_spent
			 FROM customers c, orders o, products p
			 WHERE o.customer_id = c.id AND o.product_id = p.id
			 GROUP BY c.name
			 ORDER BY total_spent DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		aliceSpent := int64(5*100 + 2*200)
		bobSpent := int64(3 * 100)
		if rows[0][0].(string) != "Alice" || toInt64(rows[0][1]) != aliceSpent {
			t.Errorf("first: got %v, want (Alice, %d)", rows[0], aliceSpent)
		}
		if rows[1][0].(string) != "Bob" || toInt64(rows[1][1]) != bobSpent {
			t.Errorf("second: got %v, want (Bob, %d)", rows[1], bobSpent)
		}
	})

	t.Run("three_way_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, SUM(o.qty * p.price) AS total
			 FROM customers c, orders o, products p
			 WHERE o.customer_id = c.id AND o.product_id = p.id
			 GROUP BY c.name
			 HAVING SUM(o.qty * p.price) > 500`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (Alice, 900), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" {
			t.Errorf("want Alice, got %v", rows[0][0])
		}
	})

	_ = ctx
}

// TestFDB_MultiTableJoinPatterns — various JOIN patterns
func TestFDB_MultiTableJoinPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mtjp",
		"CREATE TABLE t_a(id BIGINT, val STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE t_b(id BIGINT, a_id BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO t_a VALUES (1, 'x'), (2, 'y'), (3, 'z')"); err != nil {
		t.Fatalf("INSERT t_a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t_b VALUES (10, 1, 100), (20, 1, 200), (30, 2, 150), (40, 2, 250)"); err != nil {
		t.Fatalf("INSERT t_b: %v", err)
	}

	t.Run("inner_join_basic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT t_a.val, t_b.score FROM t_a JOIN t_b ON t_a.id = t_b.a_id ORDER BY t_b.score")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 100 {
			t.Errorf("first score should be 100, got %v", rows[0][1])
		}
	})

	t.Run("left_join_includes_unmatched", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT t_a.val, t_b.score FROM t_a LEFT JOIN t_b ON t_a.id = t_b.a_id ORDER BY t_a.id")
		if len(rows) != 5 {
			t.Fatalf("want 5 (4 matched + 1 unmatched z), got %d: %v", len(rows), rows)
		}
		if rows[4][1] != nil {
			t.Errorf("z's score should be NULL (no match), got %v", rows[4][1])
		}
	})

	t.Run("join_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT t_a.val, SUM(t_b.score), COUNT(*)
			FROM t_a JOIN t_b ON t_a.id = t_b.a_id
			GROUP BY t_a.val
			ORDER BY t_a.val
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (x,y), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 300 {
			t.Errorf("x sum = 100+200 = 300, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 400 {
			t.Errorf("y sum = 150+250 = 400, got %v", rows[1][1])
		}
	})
}

// TestFDB_MultiJoinWithFilter — multi-table join with various filter positions
func TestFDB_MultiJoinWithFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mjwf",
		"CREATE TABLE regions(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE stores(id BIGINT, region_id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE sales_mj(id BIGINT, store_id BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO regions VALUES (1, 'north'), (2, 'south')"); err != nil {
		t.Fatalf("INSERT regions: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO stores VALUES (10, 1, 'store_a'), (20, 1, 'store_b'), (30, 2, 'store_c')"); err != nil {
		t.Fatalf("INSERT stores: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sales_mj VALUES
		(100, 10, 500), (101, 10, 300), (102, 20, 700), (103, 30, 200), (104, 30, 400)
	`); err != nil {
		t.Fatalf("INSERT sales: %v", err)
	}

	t.Run("three_table_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT r.name, s.name, sm.amount
			FROM regions r
			JOIN stores s ON r.id = s.region_id
			JOIN sales_mj sm ON s.id = sm.store_id
			ORDER BY sm.amount
		`)
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d: %v", len(rows), rows)
		}
	})

	t.Run("three_table_join_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT r.name, SUM(sm.amount)
			FROM regions r
			JOIN stores s ON r.id = s.region_id
			JOIN sales_mj sm ON s.id = sm.store_id
			GROUP BY r.name
			ORDER BY r.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 regions, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 1500 {
			t.Errorf("north = 500+300+700 = 1500, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 600 {
			t.Errorf("south = 200+400 = 600, got %v", rows[1][1])
		}
	})

	t.Run("join_with_where_on_leaf", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT r.name, COUNT(*)
			FROM regions r
			JOIN stores s ON r.id = s.region_id
			JOIN sales_mj sm ON s.id = sm.store_id
			WHERE sm.amount > 300
			GROUP BY r.name
			ORDER BY r.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 2 {
			t.Errorf("north with amount>300: 500,700 = 2, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 1 {
			t.Errorf("south with amount>300: 400 = 1, got %v", rows[1][1])
		}
	})
}

// TestFDB_JoinWithLeftAndCrossVariants — different JOIN types
func TestFDB_JoinWithLeftAndCrossVariants(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jvar",
		"CREATE TABLE jv_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jv_b(id BIGINT, a_id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jv_a VALUES (1, 'x'), (2, 'y'), (3, 'z')"); err != nil {
		t.Fatalf("INSERT jv_a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jv_b VALUES (10, 1, 100), (20, 1, 200), (30, 2, 300)"); err != nil {
		t.Fatalf("INSERT jv_b: %v", err)
	}

	t.Run("inner_join_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM jv_a JOIN jv_b ON jv_a.id = jv_b.a_id")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 matched rows, got %v", rows[0][0])
		}
	})

	t.Run("left_join_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM jv_a LEFT JOIN jv_b ON jv_a.id = jv_b.a_id")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4 (3 matched + 1 unmatched z), got %v", rows[0][0])
		}
	})

	t.Run("cross_join_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM jv_a, jv_b")
		if toInt64(rows[0][0]) != 9 {
			t.Errorf("want 3*3=9 cross product, got %v", rows[0][0])
		}
	})

	t.Run("inner_vs_left_difference", func(t *testing.T) {
		inner := testkit.CollectRows(t, db, "SELECT jv_a.name FROM jv_a JOIN jv_b ON jv_a.id = jv_b.a_id GROUP BY jv_a.name ORDER BY jv_a.name")
		left := testkit.CollectRows(t, db, "SELECT jv_a.name FROM jv_a LEFT JOIN jv_b ON jv_a.id = jv_b.a_id GROUP BY jv_a.name ORDER BY jv_a.name")
		if len(inner) != 2 {
			t.Errorf("inner join groups: want 2 (x,y), got %d", len(inner))
		}
		if len(left) != 3 {
			t.Errorf("left join groups: want 3 (x,y,z), got %d", len(left))
		}
	})
}

// TestFDB_JoinWithMultipleConditions — JOIN ON with multiple predicates
func TestFDB_JoinWithMultipleConditions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jmcond",
		"CREATE TABLE jmc_a(id BIGINT, x BIGINT, y STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jmc_b(id BIGINT, x BIGINT, y STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jmc_a VALUES (1, 10, 'foo'), (2, 20, 'bar'), (3, 10, 'bar')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jmc_b VALUES
		(10, 10, 'foo', 100), (20, 10, 'bar', 200), (30, 20, 'bar', 300), (40, 30, 'baz', 400)
	`); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("join_on_two_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.id, b.val
			FROM jmc_a a JOIN jmc_b b ON a.x = b.x AND a.y = b.y
			ORDER BY a.id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 matches, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 100 {
			t.Errorf("a.id=1 (10,foo) matches b(10,foo)=100, got %v", rows[0][1])
		}
	})

	t.Run("join_two_col_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.y, SUM(b.val)
			FROM jmc_a a JOIN jmc_b b ON a.x = b.x AND a.y = b.y
			GROUP BY a.y ORDER BY a.y
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_WhereOnJoinColumns — WHERE filtering on columns from both sides of JOIN
func TestFDB_WhereOnJoinColumns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wojc",
		"CREATE TABLE wj_a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE wj_b(id BIGINT, a_id BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wj_a VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO wj_b VALUES (10, 1, 100), (20, 2, 200), (30, 3, 300)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("where_on_left_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a.id, b.score FROM wj_a a JOIN wj_b b ON a.id = b.a_id WHERE a.val > 15")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})

	t.Run("where_on_right_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a.id, b.score FROM wj_a a JOIN wj_b b ON a.id = b.a_id WHERE b.score >= 200")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})

	t.Run("where_on_both_tables", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a.id FROM wj_a a JOIN wj_b b ON a.id = b.a_id WHERE a.val >= 20 AND b.score <= 200")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want id=2 (val=20>=20 AND score=200<=200), got %v", rows)
		}
	})
}

// TestFDB_JoinWithOrderByOnBothTables — ORDER BY referencing columns from both joined tables
func TestFDB_JoinWithOrderByOnBothTables(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jobt",
		"CREATE TABLE job_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE job_b(id BIGINT, aid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO job_a VALUES (1, 'z'), (2, 'a'), (3, 'm')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO job_b VALUES (10, 1, 300), (20, 2, 100), (30, 3, 200)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("order_by_left_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a.name, b.val FROM job_a a JOIN job_b b ON a.id = b.aid ORDER BY a.name")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "a" {
			t.Errorf("first should be 'a', got %v", rows[0][0])
		}
	})

	t.Run("order_by_right_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a.name, b.val FROM job_a a JOIN job_b b ON a.id = b.aid ORDER BY b.val DESC")
		if toInt64(rows[0][1]) != 300 {
			t.Errorf("first val DESC should be 300, got %v", rows[0][1])
		}
	})
}
