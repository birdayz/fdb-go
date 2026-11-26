package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_LeftJoinWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ljoinagg",
		"CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE orders (id BIGINT, cust_id BIGINT, amount BIGINT, PRIMARY KEY (id))")

	for _, c := range []struct {
		id   int
		name string
	}{
		{1, "Alice"}, {2, "Bob"}, {3, "Charlie"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO customers VALUES (%d, '%s')", c.id, c.name,
		)); err != nil {
			t.Fatalf("INSERT customers: %v", err)
		}
	}
	for _, o := range []struct {
		id, cid, amount int
	}{
		{101, 1, 100}, {102, 1, 200}, {103, 2, 300},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO orders VALUES (%d, %d, %d)", o.id, o.cid, o.amount,
		)); err != nil {
			t.Fatalf("INSERT orders: %v", err)
		}
	}

	t.Run("left_join_count_with_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, COUNT(o.id) AS order_count
			 FROM customers c LEFT JOIN orders o ON c.id = o.cust_id
			 GROUP BY c.name
			 ORDER BY c.name`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" || rows[0][1].(int64) != 2 {
			t.Errorf("Alice: got %v, want [Alice, 2]", rows[0])
		}
		if rows[1][0].(string) != "Bob" || rows[1][1].(int64) != 1 {
			t.Errorf("Bob: got %v, want [Bob, 1]", rows[1])
		}
		if rows[2][0].(string) != "Charlie" || rows[2][1].(int64) != 0 {
			t.Errorf("Charlie: got %v, want [Charlie, 0]", rows[2])
		}
	})

	t.Run("left_join_sum_with_coalesce", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, COALESCE(SUM(o.amount), 0) AS total
			 FROM customers c LEFT JOIN orders o ON c.id = o.cust_id
			 GROUP BY c.name
			 ORDER BY c.name`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][1].(int64) != 300 {
			t.Errorf("Alice total: got %v, want 300", rows[0][1])
		}
		if rows[2][1].(int64) != 0 {
			t.Errorf("Charlie total: got %v, want 0", rows[2][1])
		}
	})

	t.Run("left_join_having_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, COUNT(o.id) AS cnt
			 FROM customers c LEFT JOIN orders o ON c.id = o.cust_id
			 GROUP BY c.name
			 HAVING COUNT(o.id) > 0
			 ORDER BY c.name`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows (Alice, Bob), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" {
			t.Errorf("first: got %v, want Alice", rows[0][0])
		}
	})

	_ = ctx
}

func TestFDB_SelfJoinAndBetweenJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "selfjn",
		"CREATE TABLE emp (id BIGINT, name STRING, mgr_id BIGINT, salary BIGINT, PRIMARY KEY (id))")

	for _, e := range []struct {
		id, mgr, sal int
		name         string
	}{
		{1, 0, 100, "Alice"},
		{2, 1, 80, "Bob"},
		{3, 1, 90, "Charlie"},
		{4, 2, 70, "Dave"},
	} {
		db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, '%s', %d, %d)", e.id, e.name, e.mgr, e.sal,
		))
	}

	t.Run("self_join_manager", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT e.name, m.name AS manager
			 FROM emp e, emp m
			 WHERE e.mgr_id = m.id
			 ORDER BY e.name`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Bob" || rows[0][1].(string) != "Alice" {
			t.Errorf("row 0: got %v, want [Bob, Alice]", rows[0])
		}
		if rows[2][0].(string) != "Dave" || rows[2][1].(string) != "Bob" {
			t.Errorf("row 2: got %v, want [Dave, Bob]", rows[2])
		}
	})

	t.Run("salary_between", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name, salary FROM emp WHERE salary BETWEEN 75 AND 95 ORDER BY salary")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Bob" || rows[1][0].(string) != "Charlie" {
			t.Errorf("got %v %v, want Bob Charlie", rows[0][0], rows[1][0])
		}
	})

	_ = ctx
}

func TestFDB_TwoDerivedTablesCrossJoined(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "dtcross",
		"CREATE TABLE t (id BIGINT, grp STRING, val BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id int
		g  string
		v  int
	}{
		{1, "A", 10}, {2, "A", 20}, {3, "B", 30},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, '%s', %d)", r.id, r.g, r.v))
	}

	t.Run("cross_join_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT a.grp, a.total, b.cnt
			 FROM (SELECT grp, SUM(val) AS total FROM t GROUP BY grp) a,
			      (SELECT grp, COUNT(*) AS cnt FROM t GROUP BY grp) b
			 WHERE a.grp = b.grp
			 ORDER BY a.grp`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || rows[0][1].(int64) != 30 || rows[0][2].(int64) != 2 {
			t.Errorf("A: got %v, want [A, 30, 2]", rows[0])
		}
		if rows[1][0].(string) != "B" || rows[1][1].(int64) != 30 || rows[1][2].(int64) != 1 {
			t.Errorf("B: got %v, want [B, 30, 1]", rows[1])
		}
	})

	_ = ctx
}

func TestFDB_SelfJoinHierarchy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "selfjoin1",
		"CREATE TABLE emp (id BIGINT, name STRING, manager_id BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO emp VALUES (1, 'CEO', NULL)",
		"INSERT INTO emp VALUES (2, 'VP', 1)",
		"INSERT INTO emp VALUES (3, 'Director', 1)",
		"INSERT INTO emp VALUES (4, 'Manager', 2)",
		"INSERT INTO emp VALUES (5, 'Engineer', 4)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("self_join_parent_child", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT child.name, parent.name
			 FROM emp child, emp parent
			 WHERE child.manager_id = parent.id
			 ORDER BY child.name`)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d: %v", len(rows), rows)
		}
		wantPairs := [][2]string{
			{"Director", "CEO"},
			{"Engineer", "Manager"},
			{"Manager", "VP"},
			{"VP", "CEO"},
		}
		for i, w := range wantPairs {
			child := rows[i][0].(string)
			parent := rows[i][1].(string)
			if child != w[0] || parent != w[1] {
				t.Errorf("row %d: got (%s, %s), want (%s, %s)",
					i, child, parent, w[0], w[1])
			}
		}
	})

	t.Run("self_join_count_reports", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT mgr.name, COUNT(*) AS cnt
			 FROM emp mgr, emp report
			 WHERE report.manager_id = mgr.id
			 GROUP BY mgr.name
			 ORDER BY cnt DESC`)
		if len(rows) < 1 {
			t.Fatalf("want at least 1, got %d", len(rows))
		}
		if rows[0][0].(string) != "CEO" || toInt64(rows[0][1]) != 2 {
			t.Errorf("top manager: got %v, want (CEO, 2)", rows[0])
		}
	})

	t.Run("self_join_with_not_exists", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT e.name FROM emp e
			 WHERE NOT EXISTS (SELECT 1 FROM emp r WHERE r.manager_id = e.id)
			 ORDER BY e.name`)
		wantLeaves := []string{"Director", "Engineer"}
		if len(rows) != len(wantLeaves) {
			t.Fatalf("want %d leaves, got %d: %v", len(wantLeaves), len(rows), rows)
		}
		for i, w := range wantLeaves {
			if rows[i][0].(string) != w {
				t.Errorf("row %d: got %v, want %s", i, rows[i][0], w)
			}
		}
	})
}

// TestFDB_CrossJoinBasic — CROSS JOIN (cartesian product)
func TestFDB_CrossJoinBasic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "xjoin",
		"CREATE TABLE xj1(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE xj2(code BIGINT, label STRING, PRIMARY KEY(code))")
	if _, err := db.ExecContext(ctx, "INSERT INTO xj1 VALUES (1, 'a'), (2, 'b')"); err != nil {
		t.Fatalf("INSERT xj1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO xj2 VALUES (10, 'x'), (20, 'y'), (30, 'z')"); err != nil {
		t.Fatalf("INSERT xj2: %v", err)
	}

	t.Run("cross_join_row_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT xj1.id, xj2.code FROM xj1, xj2 ORDER BY xj1.id, xj2.code")
		if len(rows) != 6 {
			t.Fatalf("want 2*3=6 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("cross_join_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT xj1.id, xj2.code FROM xj1, xj2 WHERE xj2.code > 15 ORDER BY xj1.id, xj2.code")
		if len(rows) != 4 {
			t.Fatalf("want 2*2=4 rows (code>15: 20,30), got %d: %v", len(rows), rows)
		}
	})

	t.Run("cross_join_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM xj1, xj2")
		if len(rows) != 1 || toInt64(rows[0][0]) != 6 {
			t.Errorf("COUNT(*) of cross join should be 6, got %v", rows)
		}
	})
}

// TestFDB_LeftJoinNullHandling — LEFT JOIN NULL propagation edge cases
func TestFDB_LeftJoinNullHandling(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ljnull",
		"CREATE TABLE lj_left(id BIGINT, val STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE lj_right(id BIGINT, left_id BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO lj_left VALUES (1, 'a'), (2, 'b'), (3, 'c')"); err != nil {
		t.Fatalf("INSERT left: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO lj_right VALUES (10, 1, 100), (20, 1, 200), (30, 3, 50)"); err != nil {
		t.Fatalf("INSERT right: %v", err)
	}

	t.Run("left_join_null_right_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT l.val, r.score
			FROM lj_left l LEFT JOIN lj_right r ON l.id = r.left_id
			ORDER BY l.id, r.score
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4 (a:100, a:200, b:NULL, c:50), got %d: %v", len(rows), rows)
		}
		found_null := false
		for _, r := range rows {
			if fmt.Sprintf("%v", r[0]) == "b" && r[1] == nil {
				found_null = true
			}
		}
		if !found_null {
			t.Errorf("b should have NULL score from left join")
		}
	})

	t.Run("left_join_count_with_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT l.val, COUNT(r.score)
			FROM lj_left l LEFT JOIN lj_right r ON l.id = r.left_id
			GROUP BY l.val
			ORDER BY l.val
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[1][0]) != "b" || toInt64(rows[1][1]) != 0 {
			t.Errorf("b: COUNT(r.score) should be 0 (NULL), got %v %v", rows[1][0], rows[1][1])
		}
	})

	t.Run("left_join_sum_with_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT l.val, SUM(r.score)
			FROM lj_left l LEFT JOIN lj_right r ON l.id = r.left_id
			GROUP BY l.val
			ORDER BY l.val
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "a" || toInt64(rows[0][1]) != 300 {
			t.Errorf("a: SUM=300, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "b" {
			t.Errorf("second should be b, got %v", rows[1][0])
		}
		if rows[1][1] != nil {
			t.Errorf("b: SUM should be NULL (no matches), got %v", rows[1][1])
		}
	})

	t.Run("left_join_coalesce_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT l.val, COALESCE(r.score, 0)
			FROM lj_left l LEFT JOIN lj_right r ON l.id = r.left_id
			ORDER BY l.id, r.score
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		for _, r := range rows {
			if fmt.Sprintf("%v", r[0]) == "b" && toInt64(r[1]) != 0 {
				t.Errorf("b: COALESCE(NULL, 0) should be 0, got %v", r[1])
			}
		}
	})
}

// TestFDB_JoinSelfReference — self-join patterns
func TestFDB_JoinSelfReference(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sjref", "CREATE TABLE employees(id BIGINT, name STRING, manager_id BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO employees VALUES
		(1, 'ceo', NULL), (2, 'vp1', 1), (3, 'vp2', 1),
		(4, 'mgr1', 2), (5, 'mgr2', 2), (6, 'dev1', 4)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("self_join_manager", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT e.name, m.name AS manager
			FROM employees e JOIN employees m ON e.manager_id = m.id
			ORDER BY e.name
		`)
		if len(rows) != 5 {
			t.Fatalf("want 5 (all except ceo who has NULL manager_id), got %d: %v", len(rows), rows)
		}
	})

	t.Run("self_join_count_reports", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT m.name, COUNT(*) AS reports
			FROM employees e JOIN employees m ON e.manager_id = m.id
			GROUP BY m.name
			ORDER BY reports DESC
		`)
		if len(rows) < 2 {
			t.Fatalf("want at least 2 managers, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "ceo" || toInt64(rows[0][1]) != 2 {
			t.Errorf("ceo should have 2 reports, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_LeftJoinWithAggregateAndHaving — LEFT JOIN + GROUP BY + HAVING
func TestFDB_LeftJoinWithAggregateAndHaving(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ljah",
		"CREATE TABLE ljah_p(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ljah_c(id BIGINT, pid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ljah_p VALUES (1, 'p1'), (2, 'p2'), (3, 'p3')"); err != nil {
		t.Fatalf("INSERT p: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ljah_c VALUES (10, 1, 100), (20, 1, 200), (30, 1, 300), (40, 2, 50)"); err != nil {
		t.Fatalf("INSERT c: %v", err)
	}

	t.Run("left_join_having_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, COUNT(c.val)
			FROM ljah_p p LEFT JOIN ljah_c c ON p.id = c.pid
			GROUP BY p.name
			HAVING COUNT(c.val) > 0
			ORDER BY p.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (p1=3, p2=1; p3 excluded by HAVING), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("p1 count should be 3, got %v", rows[0][1])
		}
	})

	t.Run("left_join_sum_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, SUM(c.val)
			FROM ljah_p p LEFT JOIN ljah_c c ON p.id = c.pid
			GROUP BY p.name
			HAVING SUM(c.val) > 100
			ORDER BY p.name
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (p1=600>100), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 600 {
			t.Errorf("p1 sum = 100+200+300 = 600, got %v", rows[0][1])
		}
	})
}

// TestFDB_LeftJoinWithGroupByHavingOrder — LEFT JOIN full pipeline
func TestFDB_LeftJoinWithGroupByHavingOrder(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ljgho",
		"CREATE TABLE ljgho_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ljgho_b(id BIGINT, aid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ljgho_a VALUES (1, 'x'), (2, 'y'), (3, 'z')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ljgho_b VALUES (10, 1, 100), (20, 1, 200), (30, 1, 300), (40, 2, 50)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("left_join_group_having_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, COUNT(b.val), COALESCE(SUM(b.val), 0) AS total
			FROM ljgho_a a LEFT JOIN ljgho_b b ON a.id = b.aid
			GROUP BY a.name
			HAVING COUNT(b.val) > 0
			ORDER BY total DESC
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (x,y have matches; z excluded by HAVING), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "x" || toInt64(rows[0][2]) != 600 {
			t.Errorf("first: x total=600, got %v %v", rows[0][0], rows[0][2])
		}
	})
}

// TestFDB_LeftJoinWithCoalesceAndHavingFull — LEFT JOIN full pipeline
func TestFDB_LeftJoinWithCoalesceAndHavingFull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ljchf",
		"CREATE TABLE ljchf_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ljchf_b(id BIGINT, aid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ljchf_a VALUES (1, 'x'), (2, 'y'), (3, 'z')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ljchf_b VALUES (10, 1, 50), (20, 1, 60), (30, 2, 70)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("coalesce_sum_having_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, COALESCE(SUM(b.val), 0) AS total, COUNT(b.val) AS cnt
			FROM ljchf_a a LEFT JOIN ljchf_b b ON a.id = b.aid
			GROUP BY a.name
			HAVING COALESCE(SUM(b.val), 0) > 0
			ORDER BY total DESC
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (x,y have matches; z excluded), got %d: %v", len(rows), rows)
		}
		t.Logf("LEFT JOIN COALESCE+HAVING results: %v", rows)
		if fmt.Sprintf("%v", rows[0][0]) != "x" {
			t.Errorf("first should be x, got %v", rows[0][0])
		}
	})
}

// TestFDB_SelfJoinPairs — self-join to find all pairs where a.val < b.val
func TestFDB_SelfJoinPairs(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sjp",
		"CREATE TABLE sjp_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO sjp_t VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("all_ascending_pairs", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.id, b.id FROM sjp_t a JOIN sjp_t b ON a.val < b.val
			ORDER BY a.id, b.id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 pairs (1,2)(1,3)(2,3), got %d: %v", len(rows), rows)
		}
		type pair struct{ a, b int64 }
		want := []pair{{1, 2}, {1, 3}, {2, 3}}
		for i, w := range want {
			a, b := toInt64(rows[i][0]), toInt64(rows[i][1])
			if a != w.a || b != w.b {
				t.Errorf("pair %d: want (%d,%d), got (%d,%d)", i, w.a, w.b, a, b)
			}
		}
	})

	t.Run("pair_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM sjp_t a JOIN sjp_t b ON a.val < b.val
		`)
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 pairs, got %v", rows[0][0])
		}
	})
}

// TestFDB_JoinSameTableTwice — join same table as two different aliases
func TestFDB_JoinSameTableTwice(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jst2",
		"CREATE TABLE jst2_t(id BIGINT, parent_id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO jst2_t VALUES
		(1,0,'root'),(2,1,'child1'),(3,1,'child2'),(4,2,'grandchild1')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("parent_child_pairs", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, c.name FROM jst2_t p
			JOIN jst2_t c ON p.id = c.parent_id
			ORDER BY p.name, c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 pairs, got %d: %v", len(rows), rows)
		}
		// child1→grandchild1, root→child1, root→child2
		type pair struct{ parent, child string }
		want := []pair{{"child1", "grandchild1"}, {"root", "child1"}, {"root", "child2"}}
		for i, w := range want {
			p := fmt.Sprintf("%v", rows[i][0])
			c := fmt.Sprintf("%v", rows[i][1])
			if p != w.parent || c != w.child {
				t.Errorf("row %d: want [%s %s], got [%s %s]", i, w.parent, w.child, p, c)
			}
		}
	})

	t.Run("count_children_per_parent", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, COUNT(*) FROM jst2_t p
			JOIN jst2_t c ON p.id = c.parent_id
			GROUP BY p.name
			ORDER BY p.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 parents with children, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "child1" || toInt64(rows[0][1]) != 1 {
			t.Errorf("child1: want 1 child, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "root" || toInt64(rows[1][1]) != 2 {
			t.Errorf("root: want 2 children, got %v", rows[1])
		}
	})
}

// TestFDB_LeftJoinCountSumPerDept — LEFT JOIN dept→emp, COUNT(e.id) and SUM(salary) per dept
func TestFDB_LeftJoinCountSumPerDept(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ljcsd",
		"CREATE TABLE ljcsd_dept(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ljcsd_emp(id BIGINT, dept_id BIGINT, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ljcsd_dept VALUES (1,'eng'),(2,'sales'),(3,'hr')"); err != nil {
		t.Fatalf("INSERT dept: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ljcsd_emp VALUES (10,1,100),(11,1,120),(12,2,80)"); err != nil {
		t.Fatalf("INSERT emp: %v", err)
	}

	t.Run("count_per_dept_left", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.name, COUNT(e.id) FROM ljcsd_dept d
			LEFT JOIN ljcsd_emp e ON d.id = e.dept_id
			GROUP BY d.name
			ORDER BY d.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 depts, got %d", len(rows))
		}
		// eng=2, hr=0, sales=1
		wantCnt := []int64{2, 0, 1}
		for i, w := range wantCnt {
			if toInt64(rows[i][1]) != w {
				t.Errorf("%v: want %d, got %v", rows[i][0], w, rows[i][1])
			}
		}
	})

	t.Run("sum_salary_coalesce", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.name, COALESCE(SUM(e.salary), 0) FROM ljcsd_dept d
			LEFT JOIN ljcsd_emp e ON d.id = e.dept_id
			GROUP BY d.name
			ORDER BY d.name
		`)
		// eng=220, hr=0, sales=80
		wantSum := []int64{220, 0, 80}
		for i, w := range wantSum {
			if toInt64(rows[i][1]) != w {
				t.Errorf("%v: want %d, got %v", rows[i][0], w, rows[i][1])
			}
		}
	})
}
