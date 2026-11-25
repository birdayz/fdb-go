package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_ThreeWayJoinWithAggregateExpr(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "threeway",
		"CREATE TABLE categories (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE products (id BIGINT, cat_id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE sales (id BIGINT, prod_id BIGINT, qty BIGINT, price BIGINT, PRIMARY KEY (id))")

	db.ExecContext(ctx, "INSERT INTO categories VALUES (1, 'Electronics')")
	db.ExecContext(ctx, "INSERT INTO categories VALUES (2, 'Books')")
	db.ExecContext(ctx, "INSERT INTO products VALUES (1, 1, 'Laptop')")
	db.ExecContext(ctx, "INSERT INTO products VALUES (2, 1, 'Phone')")
	db.ExecContext(ctx, "INSERT INTO products VALUES (3, 2, 'Novel')")
	db.ExecContext(ctx, "INSERT INTO sales VALUES (1, 1, 2, 1000)")
	db.ExecContext(ctx, "INSERT INTO sales VALUES (2, 1, 1, 1200)")
	db.ExecContext(ctx, "INSERT INTO sales VALUES (3, 2, 5, 500)")
	db.ExecContext(ctx, "INSERT INTO sales VALUES (4, 3, 10, 20)")

	t.Run("category_revenue", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT c.name, SUM(s.qty * s.price) AS revenue
			 FROM categories c, products p, sales s
			 WHERE c.id = p.cat_id AND p.id = s.prod_id
			 GROUP BY c.name
			 ORDER BY revenue DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		// Electronics: 2*1000 + 1*1200 + 5*500 = 5700
		// Books: 10*20 = 200
		if rows[0][0].(string) != "Electronics" || rows[0][1].(int64) != 5700 {
			t.Errorf("row 0: got %v, want [Electronics, 5700]", rows[0])
		}
		if rows[1][0].(string) != "Books" || rows[1][1].(int64) != 200 {
			t.Errorf("row 1: got %v, want [Books, 200]", rows[1])
		}
	})

	_ = ctx
}

func TestFDB_ComplexJoinAggregatePatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "cxjoin1",
		"CREATE TABLE products (id BIGINT, name STRING, category STRING, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE orders (id BIGINT, product_id BIGINT, qty BIGINT, customer STRING, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO products VALUES (1, 'Widget', 'A', 100)",
		"INSERT INTO products VALUES (2, 'Gadget', 'A', 200)",
		"INSERT INTO products VALUES (3, 'Doohickey', 'B', 50)",
		"INSERT INTO products VALUES (4, 'Thingamajig', 'B', 300)",
		"INSERT INTO orders VALUES (1, 1, 5, 'Alice')",
		"INSERT INTO orders VALUES (2, 1, 3, 'Bob')",
		"INSERT INTO orders VALUES (3, 2, 2, 'Alice')",
		"INSERT INTO orders VALUES (4, 3, 10, 'Charlie')",
		"INSERT INTO orders VALUES (5, 4, 1, 'Alice')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("join_group_by_category", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT p.category, SUM(o.qty) AS total_qty, SUM(o.qty * p.price) AS revenue
			 FROM products p, orders o
			 WHERE o.product_id = p.id
			 GROUP BY p.category
			 ORDER BY p.category`)
		if len(rows) != 2 {
			t.Fatalf("want 2 categories, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" {
			t.Errorf("first category: got %v, want A", rows[0][0])
		}
		aQty := int64(5 + 3 + 2) // 10
		aRev := int64(5*100 + 3*100 + 2*200)
		if toInt64(rows[0][1]) != aQty {
			t.Errorf("A qty: got %v, want %d", rows[0][1], aQty)
		}
		if toInt64(rows[0][2]) != aRev {
			t.Errorf("A revenue: got %v, want %d", rows[0][2], aRev)
		}
		bQty := int64(10 + 1) // 11
		bRev := int64(10*50 + 1*300)
		if toInt64(rows[1][1]) != bQty {
			t.Errorf("B qty: got %v, want %d", rows[1][1], bQty)
		}
		if toInt64(rows[1][2]) != bRev {
			t.Errorf("B revenue: got %v, want %d", rows[1][2], bRev)
		}
	})

	t.Run("join_having_on_sum_expr", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT p.category, SUM(o.qty * p.price) AS revenue
			 FROM products p, orders o
			 WHERE o.product_id = p.id
			 GROUP BY p.category
			 HAVING SUM(o.qty * p.price) > 1000`)
		if len(rows) != 1 {
			t.Fatalf("want 1 category with revenue>1000, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" {
			t.Errorf("want A (rev=1200), got %v", rows[0][0])
		}
	})

	t.Run("per_customer_total", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT o.customer, COUNT(*) AS orders, SUM(o.qty) AS total_qty
			 FROM orders o
			 GROUP BY o.customer
			 ORDER BY o.customer`)
		if len(rows) != 3 {
			t.Fatalf("want 3 customers, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" || toInt64(rows[0][1]) != 3 || toInt64(rows[0][2]) != 8 {
			t.Errorf("Alice: got %v, want (Alice, 3, 8)", rows[0])
		}
		if rows[1][0].(string) != "Bob" || toInt64(rows[1][1]) != 1 || toInt64(rows[1][2]) != 3 {
			t.Errorf("Bob: got %v, want (Bob, 1, 3)", rows[1])
		}
		if rows[2][0].(string) != "Charlie" || toInt64(rows[2][1]) != 1 || toInt64(rows[2][2]) != 10 {
			t.Errorf("Charlie: got %v, want (Charlie, 1, 10)", rows[2])
		}
	})

	t.Run("customer_revenue_with_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT o.customer, SUM(o.qty * p.price) AS spent
			 FROM orders o, products p
			 WHERE o.product_id = p.id
			 GROUP BY o.customer
			 HAVING SUM(o.qty * p.price) > 500
			 ORDER BY spent DESC`)
		if len(rows) != 1 {
			t.Fatalf("want 1 customer (Alice, 1200), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Alice" {
			t.Errorf("want Alice, got %v", rows[0][0])
		}
		aliceSpent := int64(5*100 + 3*100 + 2*200 + 1*300) // but wait, Alice ordered products 1,1,2,4
		// product 1: 5*100=500, product 1: 3*100=300 (Bob), product 2: 2*200=400, product 4: 1*300=300
		// Alice: orders 1 (prod 1, qty 5), 3 (prod 2, qty 2), 5 (prod 4, qty 1)
		// 5*100 + 2*200 + 1*300 = 500 + 400 + 300 = 1200
		aliceSpent = 1200
		if toInt64(rows[0][1]) != aliceSpent {
			t.Errorf("Alice spent: got %v, want %d", rows[0][1], aliceSpent)
		}
	})

	t.Run("products_with_no_orders", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT p.name FROM products p
			 WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.product_id = p.id)
			 ORDER BY p.name`)
		if len(rows) != 0 {
			t.Fatalf("all products have orders, want 0, got %d: %v", len(rows), rows)
		}
	})

	t.Run("products_with_exists", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT p.name FROM products p
			 WHERE EXISTS (SELECT 1 FROM orders o WHERE o.product_id = p.id AND o.qty > 5)
			 ORDER BY p.name`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (Doohickey, qty=10), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "Doohickey" {
			t.Errorf("want Doohickey, got %v", rows[0][0])
		}
	})
}

// TestFDB_JoinWithGroupBy — JOIN queries with GROUP BY
func TestFDB_JoinWithGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jgb",
		"CREATE TABLE dept(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE emp(id BIGINT, dept_id BIGINT, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'engineering'), (2, 'sales'), (3, 'hr')"); err != nil {
		t.Fatalf("INSERT dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO emp VALUES
		(1, 1, 100), (2, 1, 120), (3, 1, 130),
		(4, 2, 80), (5, 2, 90),
		(6, 3, 70)
	`); err != nil {
		t.Fatalf("INSERT emp: %v", err)
	}

	t.Run("join_group_by_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.name, COUNT(*)
			FROM dept d JOIN emp e ON d.id = e.dept_id
			GROUP BY d.name
			ORDER BY d.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "engineering" {
			t.Errorf("first should be engineering, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("engineering count = 3, got %v", rows[0][1])
		}
	})

	t.Run("join_group_by_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.name, SUM(e.salary)
			FROM dept d JOIN emp e ON d.id = e.dept_id
			GROUP BY d.name
			ORDER BY SUM(e.salary) DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 350 {
			t.Errorf("engineering sum = 100+120+130 = 350, got %v", rows[0][1])
		}
	})

	t.Run("join_with_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.name, COUNT(*), SUM(e.salary)
			FROM dept d JOIN emp e ON d.id = e.dept_id
			GROUP BY d.name
			HAVING COUNT(*) > 1
			ORDER BY d.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (engineering=3, sales=2), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_AggregateOverJoinWithNulls — aggregate over JOIN with NULL-producing LEFT JOIN
func TestFDB_AggregateOverJoinWithNulls(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "agjn",
		"CREATE TABLE aj_parent(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE aj_child(id BIGINT, parent_id BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO aj_parent VALUES (1, 'p1'), (2, 'p2'), (3, 'p3')"); err != nil {
		t.Fatalf("INSERT parent: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO aj_child VALUES (10, 1, 100), (20, 1, 200), (30, 2, 50)"); err != nil {
		t.Fatalf("INSERT child: %v", err)
	}

	t.Run("left_join_sum_with_null_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, SUM(c.amount) AS total
			FROM aj_parent p LEFT JOIN aj_child c ON p.id = c.parent_id
			GROUP BY p.name ORDER BY p.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 300 {
			t.Errorf("p1 sum = 100+200 = 300, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 50 {
			t.Errorf("p2 sum = 50, got %v", rows[1][1])
		}
		if rows[2][1] != nil {
			t.Errorf("p3 has no children → SUM should be NULL, got %v", rows[2][1])
		}
	})

	t.Run("left_join_count_col_excludes_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, COUNT(c.amount)
			FROM aj_parent p LEFT JOIN aj_child c ON p.id = c.parent_id
			GROUP BY p.name ORDER BY p.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[2][1]) != 0 {
			t.Errorf("p3 COUNT(c.amount) should be 0 (no children), got %v", rows[2][1])
		}
	})

	t.Run("left_join_coalesce_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, COALESCE(SUM(c.amount), 0) AS total
			FROM aj_parent p LEFT JOIN aj_child c ON p.id = c.parent_id
			GROUP BY p.name ORDER BY p.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[2][1]) != 0 {
			t.Errorf("p3 COALESCE(NULL, 0) should be 0, got %v", rows[2][1])
		}
	})
}

// TestFDB_JoinAggregateWithHaving — JOIN + GROUP BY + HAVING + ORDER BY combined
func TestFDB_JoinAggregateWithHaving(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jagh",
		"CREATE TABLE jagh_teams(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jagh_scores(id BIGINT, team_id BIGINT, points BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jagh_teams VALUES (1, 'alpha'), (2, 'beta'), (3, 'gamma')"); err != nil {
		t.Fatalf("INSERT teams: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jagh_scores VALUES
		(1, 1, 10), (2, 1, 20), (3, 1, 30),
		(4, 2, 5), (5, 2, 15),
		(6, 3, 100)
	`); err != nil {
		t.Fatalf("INSERT scores: %v", err)
	}

	t.Run("join_group_having_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT t.name, COUNT(*) AS games, SUM(s.points) AS total
			FROM jagh_teams t JOIN jagh_scores s ON t.id = s.team_id
			GROUP BY t.name
			HAVING SUM(s.points) >= 20
			ORDER BY total DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (gamma=100, alpha=60, beta=20 all >=20), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "gamma" || toInt64(rows[0][2]) != 100 {
			t.Errorf("first should be gamma(100), got %v %v", rows[0][0], rows[0][2])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "alpha" || toInt64(rows[1][2]) != 60 {
			t.Errorf("second should be alpha(60), got %v %v", rows[1][0], rows[1][2])
		}
		if fmt.Sprintf("%v", rows[2][0]) != "beta" || toInt64(rows[2][2]) != 20 {
			t.Errorf("third should be beta(20), got %v %v", rows[2][0], rows[2][2])
		}
	})
}

// TestFDB_JoinWithGroupByAndCoalesce — JOIN + GROUP BY + COALESCE for NULL handling
func TestFDB_JoinWithGroupByAndCoalesce(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jgbc",
		"CREATE TABLE jgbc_p(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jgbc_c(id BIGINT, pid BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jgbc_p VALUES (1, 'alice'), (2, 'bob'), (3, 'charlie')"); err != nil {
		t.Fatalf("INSERT p: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jgbc_c VALUES (10, 1, 100), (20, 1, 200), (30, 2, 50)"); err != nil {
		t.Fatalf("INSERT c: %v", err)
	}

	t.Run("left_join_coalesce_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name, COALESCE(SUM(c.amount), 0) AS total
			FROM jgbc_p p LEFT JOIN jgbc_c c ON p.id = c.pid
			GROUP BY p.name ORDER BY p.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][1]) != 300 {
			t.Errorf("alice: want 300, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[2][0]) != "charlie" || toInt64(rows[2][1]) != 0 {
			t.Errorf("charlie: want COALESCE(NULL,0)=0, got %v %v", rows[2][0], rows[2][1])
		}
	})
}

// TestFDB_JoinSumWithHavingAndLimit — JOIN + SUM + HAVING + LIMIT combined
func TestFDB_JoinSumWithHavingAndLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jshl",
		"CREATE TABLE jshl_cat(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jshl_item(id BIGINT, cat_id BIGINT, price BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jshl_cat VALUES (1, 'food'), (2, 'toys'), (3, 'books')"); err != nil {
		t.Fatalf("INSERT cat: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jshl_item VALUES
		(1, 1, 5), (2, 1, 10), (3, 1, 15),
		(4, 2, 20), (5, 2, 30),
		(6, 3, 8)
	`); err != nil {
		t.Fatalf("INSERT item: %v", err)
	}

	t.Run("join_sum_having_limit", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, SUM(i.price) AS total
			FROM jshl_cat c JOIN jshl_item i ON c.id = i.cat_id
			GROUP BY c.name
			HAVING SUM(i.price) > 10
			ORDER BY total DESC
			LIMIT 1
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "toys" || toInt64(rows[0][1]) != 50 {
			t.Errorf("want toys(50), got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_JoinWithCoalesceInGroupBy — JOIN + GROUP BY + COALESCE in SELECT
func TestFDB_JoinWithCoalesceInGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jcgb",
		"CREATE TABLE jcgb_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jcgb_b(id BIGINT, aid BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jcgb_a VALUES (1, 'alice'), (2, 'bob'), (3, 'charlie')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jcgb_b VALUES (10, 1, 100), (20, 1, 200), (30, 2, 50)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("left_join_coalesce_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, COALESCE(SUM(b.amount), 0) AS total
			FROM jcgb_a a LEFT JOIN jcgb_b b ON a.id = b.aid
			GROUP BY a.name
			ORDER BY total DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][1]) != 300 {
			t.Errorf("alice: want 300, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[2][0]) != "charlie" || toInt64(rows[2][1]) != 0 {
			t.Errorf("charlie: want 0 (no orders), got %v %v", rows[2][0], rows[2][1])
		}
	})
}

// TestFDB_JoinWithGroupByCountAndLimit — JOIN + GROUP BY + COUNT + ORDER BY + LIMIT
func TestFDB_JoinWithGroupByCountAndLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jgcl",
		"CREATE TABLE jgcl_authors(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jgcl_books(id BIGINT, author_id BIGINT, title STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jgcl_authors VALUES (1, 'tolkien'), (2, 'rowling'), (3, 'martin')"); err != nil {
		t.Fatalf("INSERT authors: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jgcl_books VALUES
		(1, 1, 'lotr'), (2, 1, 'hobbit'), (3, 1, 'silmarillion'),
		(4, 2, 'hp1'), (5, 2, 'hp2'),
		(6, 3, 'got')
	`); err != nil {
		t.Fatalf("INSERT books: %v", err)
	}

	t.Run("most_prolific_author", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, COUNT(*) AS book_count
			FROM jgcl_authors a JOIN jgcl_books b ON a.id = b.author_id
			GROUP BY a.name ORDER BY book_count DESC LIMIT 1
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "tolkien" || toInt64(rows[0][1]) != 3 {
			t.Errorf("want tolkien(3), got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_JoinSumGroupByOrderDesc — JOIN + SUM + GROUP BY + ORDER BY DESC
func TestFDB_JoinSumGroupByOrderDesc(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jsgod",
		"CREATE TABLE jsgod_store(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jsgod_sale(id BIGINT, store_id BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jsgod_store VALUES (1, 'main'), (2, 'branch')"); err != nil {
		t.Fatalf("INSERT store: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jsgod_sale VALUES (1, 1, 500), (2, 1, 300), (3, 2, 200), (4, 2, 800)"); err != nil {
		t.Fatalf("INSERT sale: %v", err)
	}

	t.Run("revenue_by_store_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT s.name, SUM(sa.amount) AS revenue
			FROM jsgod_store s JOIN jsgod_sale sa ON s.id = sa.store_id
			GROUP BY s.name ORDER BY revenue DESC
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "branch" || toInt64(rows[0][1]) != 1000 {
			t.Errorf("first: branch 1000, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "main" || toInt64(rows[1][1]) != 800 {
			t.Errorf("second: main 800, got %v %v", rows[1][0], rows[1][1])
		}
	})
}

// TestFDB_JoinThreeTablesWithAgg — 3-way JOIN with aggregate
func TestFDB_JoinThreeTablesWithAgg(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "j3a",
		"CREATE TABLE j3a_cust(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE j3a_orders(id BIGINT, cust_id BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE j3a_items(id BIGINT, order_id BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO j3a_cust VALUES (1,'alice'),(2,'bob')"); err != nil {
		t.Fatalf("INSERT cust: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO j3a_orders VALUES (10,1),(20,1),(30,2)"); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO j3a_items VALUES (100,10,50),(101,10,30),(102,20,70),(103,30,40)"); err != nil {
		t.Fatalf("INSERT items: %v", err)
	}

	t.Run("total_per_customer", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, SUM(i.amount) FROM j3a_cust c
			JOIN j3a_orders o ON c.id = o.cust_id
			JOIN j3a_items i ON o.id = i.order_id
			GROUP BY c.name
			ORDER BY c.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 customers, got %d", len(rows))
		}
		// alice: orders 10,20 → items 50+30+70 = 150
		// bob: order 30 → items 40
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][1]) != 150 {
			t.Errorf("alice: want 150, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "bob" || toInt64(rows[1][1]) != 40 {
			t.Errorf("bob: want 40, got %v %v", rows[1][0], rows[1][1])
		}
	})

	t.Run("order_count_per_customer", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, COUNT(*) FROM j3a_cust c
			JOIN j3a_orders o ON c.id = o.cust_id
			GROUP BY c.name
			ORDER BY c.name
		`)
		if toInt64(rows[0][1]) != 2 {
			t.Errorf("alice: want 2 orders, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 1 {
			t.Errorf("bob: want 1 order, got %v", rows[1][1])
		}
	})
}

// TestFDB_GroupByHavingSumWithJoin — JOIN two tables, GROUP BY, HAVING on SUM
func TestFDB_GroupByHavingSumWithJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ghsj",
		"CREATE TABLE ghsj_cat(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ghsj_prod(id BIGINT, cat_id BIGINT, price BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ghsj_cat VALUES (1,'electronics'),(2,'clothing'),(3,'food')"); err != nil {
		t.Fatalf("INSERT cat: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ghsj_prod VALUES
		(10,1,500),(11,1,300),(12,1,200),
		(20,2,50),(21,2,30),
		(30,3,10),(31,3,5),(32,3,8)`); err != nil {
		t.Fatalf("INSERT prod: %v", err)
	}

	t.Run("categories_with_total_gt_100", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, SUM(p.price) FROM ghsj_cat c
			JOIN ghsj_prod p ON c.id = p.cat_id
			GROUP BY c.name
			HAVING SUM(p.price) > 100
			ORDER BY c.name
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1 category (electronics=1000), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "electronics" || toInt64(rows[0][1]) != 1000 {
			t.Errorf("want [electronics 1000], got %v", rows[0])
		}
	})

	t.Run("all_categories_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, COUNT(*) FROM ghsj_cat c
			JOIN ghsj_prod p ON c.id = p.cat_id
			GROUP BY c.name
			ORDER BY c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 categories, got %d", len(rows))
		}
		// ORDER BY c.name: clothing=2, electronics=3, food=3
		wantCnt := []int64{2, 3, 3}
		for i, w := range wantCnt {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d (%v): want count %d, got %v", i, rows[i][0], w, rows[i][1])
			}
		}
	})
}

// TestFDB_JoinCountGroupByHaving — JOIN + GROUP BY + HAVING COUNT
func TestFDB_JoinCountGroupByHaving(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jcgh",
		"CREATE TABLE jcgh_authors(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jcgh_books(id BIGINT, author_id BIGINT, title STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO jcgh_authors VALUES (1,'alice'),(2,'bob'),(3,'carol')"); err != nil {
		t.Fatalf("INSERT authors: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jcgh_books VALUES
		(10,1,'book1'),(11,1,'book2'),(12,1,'book3'),
		(20,2,'book4'),
		(30,3,'book5'),(31,3,'book6')`); err != nil {
		t.Fatalf("INSERT books: %v", err)
	}

	t.Run("authors_with_2plus_books", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, COUNT(*) FROM jcgh_authors a
			JOIN jcgh_books b ON a.id = b.author_id
			GROUP BY a.name
			HAVING COUNT(*) >= 2
			ORDER BY a.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (alice=3, carol=2), got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][1]) != 3 {
			t.Errorf("alice: want 3, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "carol" || toInt64(rows[1][1]) != 2 {
			t.Errorf("carol: want 2, got %v", rows[1])
		}
	})
}

// TestFDB_JoinSumGroupOrderSum — JOIN + SUM + GROUP BY + ORDER BY SUM
func TestFDB_JoinSumGroupOrderSum(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jsgo",
		"CREATE TABLE jsgo_stores(id BIGINT, city STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jsgo_sales(id BIGINT, store_id BIGINT, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO jsgo_stores VALUES (1,'berlin'),(2,'munich'),(3,'hamburg')"); err != nil {
		t.Fatalf("INSERT stores: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jsgo_sales VALUES
		(10,1,100),(11,1,200),(12,2,300),(13,2,150),(14,2,50),(15,3,500)`); err != nil {
		t.Fatalf("INSERT sales: %v", err)
	}

	t.Run("revenue_per_city_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT s.city, SUM(sa.amount) FROM jsgo_stores s
			JOIN jsgo_sales sa ON s.id = sa.store_id
			GROUP BY s.city
			ORDER BY SUM(sa.amount) DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 cities, got %d", len(rows))
		}
		// hamburg=500, munich=500, berlin=300
		if fmt.Sprintf("%v", rows[0][0]) != "hamburg" && fmt.Sprintf("%v", rows[0][0]) != "munich" {
			t.Errorf("top: want hamburg or munich (500), got %v", rows[0])
		}
		if toInt64(rows[2][1]) != 300 {
			t.Errorf("bottom: want 300 (berlin), got %v", rows[2][1])
		}
	})

	t.Run("total_revenue", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(sa.amount) FROM jsgo_stores s
			JOIN jsgo_sales sa ON s.id = sa.store_id
		`)
		if toInt64(rows[0][0]) != 1300 {
			t.Errorf("want 1300, got %v", rows[0][0])
		}
	})
}
