package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_PlanShapeExistsFlatMap(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/FRL/ps_exists_%s", t.Name())
	setup := testkit.OpenDB(t, dbPath)
	_, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("tmpl_%s", t.Name())
	_, err = setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE parent (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE child (id BIGINT, parent_id BIGINT, PRIMARY KEY (id))", tmpl,
	))
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl))
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// RFC-141: WHERE-EXISTS is a PURE-MAP FlatMap whose existential inner is
	// FirstOrDefault(inner, NULL) topped by a residual existential filter
	// (QOV IS NOT NULL). There is no fused EXISTS join mode any more — the
	// semi-join is emergent (matches Java's
	// "... MAP | DEFAULT NULL | FILTER _ NOT_NULL ..." shape).
	//
	// Non-PK correlated EXISTS: the inner correlation can't push to a scan
	// range, so the inner is Scan(child)+filter | FirstOrDefault | filter.
	plan := testkit.ExplainVia(t, ctx, db,
		"SELECT id FROM parent WHERE EXISTS (SELECT 1 FROM child WHERE child.parent_id = parent.id)")
	t.Logf("Non-PK EXISTS plan:\n%s", plan)
	if !strings.Contains(plan, "FlatMap") {
		t.Errorf("expected FlatMap in plan, got:\n%s", plan)
	}
	if !strings.Contains(plan, "FirstOrDefault") {
		t.Errorf("expected FirstOrDefault (existential one-row inner) in plan, got:\n%s", plan)
	}
	if !strings.Contains(plan, "PredicatesFilter") {
		t.Errorf("expected PredicatesFilter (residual existential filter) in plan, got:\n%s", plan)
	}

	// PK-matching correlated EXISTS: the inner correlation pushes into a
	// parameterized PK scan (Scan(parent, [=])) below the FirstOrDefault.
	plan = testkit.ExplainVia(t, ctx, db,
		"SELECT id FROM child WHERE EXISTS (SELECT 1 FROM parent WHERE parent.id = child.parent_id)")
	t.Logf("PK EXISTS plan:\n%s", plan)
	if !strings.Contains(plan, "FlatMap") {
		t.Errorf("expected FlatMap in plan, got:\n%s", plan)
	}
	if !strings.Contains(plan, "FirstOrDefault") {
		t.Errorf("expected FirstOrDefault (existential one-row inner) in plan, got:\n%s", plan)
	}
}

func TestFDB_DerivedTableJoinExists(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtjex",
		"CREATE TABLE emp (id BIGINT, fname STRING, dept_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE project (id BIGINT, name STRING, emp_id BIGINT, PRIMARY KEY (id))")

	for _, e := range []struct {
		id   int
		name string
		dept int
	}{
		{1, "Jack", 1},
		{2, "Thomas", 1},
		{3, "Emily", 1},
		{5, "Daniel", 2},
		{8, "Megan", 3},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, '%s', %d)", e.id, e.name, e.dept,
		)); err != nil {
			t.Fatalf("INSERT emp: %v", err)
		}
	}
	for _, d := range []struct {
		id   int
		name string
	}{
		{1, "Engineering"}, {2, "Sales"}, {3, "Marketing"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO dept VALUES (%d, '%s')", d.id, d.name,
		)); err != nil {
			t.Fatalf("INSERT dept: %v", err)
		}
	}
	for _, p := range []struct {
		id    int
		name  string
		empID int
	}{
		{1, "OLAP", 3}, {2, "SEO", 8}, {3, "Feedback", 5},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO project VALUES (%d, '%s', %d)", p.id, p.name, p.empID,
		)); err != nil {
			t.Fatalf("INSERT project: %v", err)
		}
	}

	t.Run("subquery_exists_join_filter", func(t *testing.T) {
		// Java: derived table with EXISTS + join to dept with filter
		rows, err := db.QueryContext(ctx,
			`SELECT sq.fname FROM
				(SELECT fname, dept_id FROM emp WHERE EXISTS (SELECT * FROM project WHERE emp_id = emp.id)) AS sq,
				dept
			WHERE sq.dept_id = dept.id AND dept.name = 'Sales'`)
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var fname string
			if err := rows.Scan(&fname); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, fname)
		}
		if len(got) != 1 || got[0] != "Daniel" {
			t.Fatalf("got %v, want [Daniel]", got)
		}
	})

	t.Run("three_way_dept_project", func(t *testing.T) {
		// Java: 3-way join to find departments with projects
		rows, err := db.QueryContext(ctx,
			`SELECT dept.name, project.name FROM emp, dept, project
			WHERE emp.dept_id = dept.id AND project.emp_id = emp.id
			ORDER BY dept.name`)
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			dept    string
			project string
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.dept, &r.project); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{{"Engineering", "OLAP"}, {"Marketing", "SEO"}, {"Sales", "Feedback"}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})
}

func TestFDB_ScalarSubqueryWithAggExpr(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "ssqagg",
		"CREATE TABLE items (id BIGINT, price BIGINT, qty BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct{ id, p, q int }{
		{1, 10, 5}, {2, 20, 3}, {3, 100, 2},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items VALUES (%d, %d, %d)", r.id, r.p, r.q))
	}

	t.Run("scalar_sum_expr", func(t *testing.T) {
		var total int64
		err := db.QueryRowContext(ctx,
			"SELECT SUM(price * qty) FROM items").Scan(&total)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if total != 310 {
			t.Errorf("got %d, want 310 (50+60+200)", total)
		}
	})

	t.Run("where_gt_scalar_subquery", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT id, price * qty AS value FROM items
			 WHERE price * qty > (SELECT SUM(price) FROM items)
			 ORDER BY id`)
		// SUM(price) = 130. price*qty: 50, 60, 200. Only id=3 (200) > 130.
		if len(rows) != 1 {
			t.Fatalf("want 1 row (id=3, 200 > 130), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(int64) != 3 {
			t.Errorf("got id=%v, want 3", rows[0][0])
		}
	})

	_ = ctx
}

func TestFDB_ExistsWithGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "exgrp",
		"CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE orders (id BIGINT, cust_id BIGINT, amount BIGINT, PRIMARY KEY (id))")

	for _, c := range []struct {
		id   int
		name string
	}{
		{1, "Alice"}, {2, "Bob"}, {3, "Charlie"},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO customers VALUES (%d, '%s')", c.id, c.name))
	}
	for _, o := range []struct {
		id, cid, amt int
	}{
		{1, 1, 100}, {2, 1, 200}, {3, 2, 50},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO orders VALUES (%d, %d, %d)", o.id, o.cid, o.amt))
	}

	t.Run("exists_filters_customers", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name FROM customers c
			 WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust_id = c.id)
			 ORDER BY c.name`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (Alice, Bob have orders), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" {
			t.Errorf("row 0: got %v, want Alice", rows[0][0])
		}
		if rows[1][0].(string) != "Bob" {
			t.Errorf("row 1: got %v, want Bob", rows[1][0])
		}
	})

	t.Run("not_exists_filters_customers", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name FROM customers c
			 WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.cust_id = c.id)
			 ORDER BY c.name`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (Charlie has no orders), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Charlie" {
			t.Errorf("got %v, want Charlie", rows[0][0])
		}
	})

	t.Run("exists_with_aggregate_in_outer", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, SUM(o.amount) AS total
			 FROM customers c, orders o
			 WHERE c.id = o.cust_id
			 GROUP BY c.name
			 HAVING SUM(o.amount) > 100
			 ORDER BY c.name`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (Alice=300 > 100), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" || rows[0][1].(int64) != 300 {
			t.Errorf("got %v, want [Alice, 300]", rows[0])
		}
	})

	_ = ctx
}

func TestFDB_DerivedTableExistsJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "dtexjn",
		"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE project (id BIGINT, name STRING, dept_id BIGINT, PRIMARY KEY (id))")

	db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'Engineering')")
	db.ExecContext(ctx, "INSERT INTO dept VALUES (2, 'Sales')")
	db.ExecContext(ctx, "INSERT INTO dept VALUES (3, 'HR')")
	db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 'Alice', 1)")
	db.ExecContext(ctx, "INSERT INTO emp VALUES (2, 'Bob', 1)")
	db.ExecContext(ctx, "INSERT INTO emp VALUES (3, 'Charlie', 2)")
	db.ExecContext(ctx, "INSERT INTO project VALUES (1, 'Alpha', 1)")
	db.ExecContext(ctx, "INSERT INTO project VALUES (2, 'Beta', 2)")

	t.Run("derived_table_join_agg", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT sub.dept_name, sub.emp_count
			 FROM (SELECT d.name AS dept_name, COUNT(e.id) AS emp_count
			       FROM dept d, emp e
			       WHERE d.id = e.dept_id
			       GROUP BY d.name) sub
			 ORDER BY sub.dept_name`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Engineering" || rows[0][1].(int64) != 2 {
			t.Errorf("row 0: got %v, want [Engineering, 2]", rows[0])
		}
		if rows[1][0].(string) != "Sales" || rows[1][1].(int64) != 1 {
			t.Errorf("row 1: got %v, want [Sales, 1]", rows[1])
		}
	})

	t.Run("three_way_dept_project_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name, p.name AS project, COUNT(e.id) AS team_size
			 FROM dept d, emp e, project p
			 WHERE d.id = e.dept_id AND d.id = p.dept_id
			 GROUP BY d.name, p.name
			 ORDER BY d.name, p.name`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Engineering" || rows[0][2].(int64) != 2 {
			t.Errorf("row 0: got %v, want [Engineering, Alpha, 2]", rows[0])
		}
	})

	_ = ctx
}

func TestFDB_SubqueryScalarComparison(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "subsclr1",
		"CREATE TABLE emp (id BIGINT, name STRING, salary BIGINT, dept_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO dept VALUES (1, 'Engineering')",
		"INSERT INTO dept VALUES (2, 'Sales')",
		"INSERT INTO emp VALUES (1, 'Alice', 100, 1)",
		"INSERT INTO emp VALUES (2, 'Bob', 120, 1)",
		"INSERT INTO emp VALUES (3, 'Charlie', 80, 2)",
		"INSERT INTO emp VALUES (4, 'Diana', 90, 2)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("exists_with_correlated_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name FROM dept d
			 WHERE EXISTS (SELECT 1 FROM emp e WHERE e.dept_id = d.id AND e.salary > 100)
			 ORDER BY d.name`)
		if len(rows) != 1 {
			t.Fatalf("want 1 dept (Engineering), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Engineering" {
			t.Errorf("want Engineering, got %v", rows[0][0])
		}
	})

	t.Run("not_exists_correlated", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name FROM dept d
			 WHERE NOT EXISTS (SELECT 1 FROM emp e WHERE e.dept_id = d.id AND e.salary > 100)
			 ORDER BY d.name`)
		if len(rows) != 1 {
			t.Fatalf("want 1 dept (Sales), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Sales" {
			t.Errorf("want Sales, got %v", rows[0][0])
		}
	})

	t.Run("exists_non_correlated_all_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT name FROM emp WHERE EXISTS (SELECT 1 FROM dept WHERE id = 1) ORDER BY name`)
		if len(rows) != 4 {
			t.Fatalf("want 4 (non-correlated EXISTS returns all), got %d", len(rows))
		}
	})

	t.Run("join_aggregate_per_dept", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name, COUNT(*) AS cnt, SUM(e.salary) AS total
			 FROM dept d, emp e
			 WHERE e.dept_id = d.id
			 GROUP BY d.name
			 ORDER BY d.name`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][0].(string) != "Engineering" || toInt64(rows[0][1]) != 2 || toInt64(rows[0][2]) != 220 {
			t.Errorf("Engineering: got %v, want (Engineering, 2, 220)", rows[0])
		}
		if rows[1][0].(string) != "Sales" || toInt64(rows[1][1]) != 2 || toInt64(rows[1][2]) != 170 {
			t.Errorf("Sales: got %v, want (Sales, 2, 170)", rows[1])
		}
	})

	t.Run("having_on_joined_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT d.name, SUM(e.salary)
			 FROM dept d, emp e
			 WHERE e.dept_id = d.id
			 GROUP BY d.name
			 HAVING SUM(e.salary) > 200`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (Engineering sum=220), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Engineering" {
			t.Errorf("want Engineering, got %v", rows[0][0])
		}
	})
}

// TestFDB_ScalarSubqueryInSelect — scalar subquery in SELECT list
func TestFDB_ScalarSubqueryInSelect(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "scsq",
		"CREATE TABLE main_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE ref_t(cat STRING, label STRING, PRIMARY KEY(cat))")
	if _, err := db.ExecContext(ctx, `INSERT INTO main_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'B', 30), (4, 'B', 40), (5, 'C', 50)
	`); err != nil {
		t.Fatalf("INSERT main_t: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ref_t VALUES
		('A', 'alpha'), ('B', 'beta'), ('C', 'gamma')
	`); err != nil {
		t.Fatalf("INSERT ref_t: %v", err)
	}

	t.Run("scalar_subquery_in_select", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT (SELECT COUNT(*) FROM main_t)")
		if len(rows) != 1 || len(rows[0]) != 1 || toInt64(rows[0][0]) != 5 {
			t.Fatalf("scalar count = %v, want [[5]]", rows)
		}
	})

	t.Run("exists_subquery", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM main_t WHERE EXISTS (SELECT 1 FROM ref_t WHERE ref_t.cat = main_t.cat) ORDER BY id")
		if len(rows) != 5 {
			t.Fatalf("all rows have matching ref_t, want 5 got %d", len(rows))
		}
	})

	t.Run("not_exists_subquery", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO main_t VALUES (6, 'D', 60)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id FROM main_t WHERE NOT EXISTS (SELECT 1 FROM ref_t WHERE ref_t.cat = main_t.cat) ORDER BY id")
		if len(rows) != 1 {
			t.Fatalf("want 1 (cat=D has no ref), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("want id=6, got %v", rows[0][0])
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM main_t WHERE id = 6"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
	})
}
