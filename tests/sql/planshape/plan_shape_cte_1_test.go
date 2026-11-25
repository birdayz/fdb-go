package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_CTEWithAggregateExpression(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "cteagg",
		"CREATE TABLE orders (id BIGINT, region STRING, qty BIGINT, price BIGINT, PRIMARY KEY (id))")

	for _, o := range []struct {
		id         int
		region     string
		qty, price int
	}{
		{1, "US", 10, 100},
		{2, "US", 20, 50},
		{3, "EU", 5, 200},
		{4, "EU", 15, 80},
	} {
		db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO orders VALUES (%d, '%s', %d, %d)", o.id, o.region, o.qty, o.price,
		))
	}

	t.Run("cte_with_sum_expr", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`WITH revenue AS (
			   SELECT region, SUM(qty * price) AS total
			   FROM orders GROUP BY region
			 )
			 SELECT region, total FROM revenue ORDER BY total DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][1] == nil {
			t.Fatalf("total is NULL for %v", rows[0][0])
		}
		if rows[0][0].(string) != "EU" || rows[0][1].(int64) != 2200 {
			t.Errorf("row 0: got %v, want [EU, 2200]", rows[0])
		}
		if rows[1][0].(string) != "US" || rows[1][1].(int64) != 2000 {
			t.Errorf("row 1: got %v, want [US, 2000]", rows[1])
		}
	})

	t.Run("cte_joined_with_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`WITH totals AS (
			   SELECT region, SUM(qty) AS total_qty
			   FROM orders GROUP BY region
			 )
			 SELECT o.id, t.total_qty
			 FROM orders o, totals t
			 WHERE o.region = t.region AND o.qty > 10
			 ORDER BY o.id`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows (id=2 qty=20, id=4 qty=15), got %d: %v", len(rows), rows)
		}
		if rows[0][0].(int64) != 2 || rows[0][1].(int64) != 30 {
			t.Errorf("row 0: got %v, want [2, 30]", rows[0])
		}
	})

	_ = ctx
}

func TestFDB_CTEJavaPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "ctejava1",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 1)",
		"INSERT INTO t1 VALUES (2, 10, 2)",
		"INSERT INTO t1 VALUES (6, 20, 6)",
		"INSERT INTO t1 VALUES (7, 20, 7)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("basic_cte", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"WITH c1 AS (SELECT col1, col2 FROM t1) SELECT col1, col2 FROM c1 ORDER BY col2")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[0][1]) != 1 {
			t.Errorf("row 0: got %v, want (10, 1)", rows[0])
		}
	})

	t.Run("cte_select_star", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"WITH c1 AS (SELECT * FROM t1) SELECT * FROM c1 ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[3][0]) != 7 {
			t.Errorf("first=%v, last=%v, want 1,7", rows[0][0], rows[3][0])
		}
	})

	t.Run("cte_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"WITH c1 AS (SELECT col1, col2 FROM t1) SELECT col1 FROM c1 WHERE col2 < 3 ORDER BY col1")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		for _, r := range rows {
			if toInt64(r[0]) != 10 {
				t.Errorf("want col1=10, got %v", r[0])
			}
		}
	})

	t.Run("cte_ignored_unused", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"WITH ignored AS (SELECT * FROM t1) SELECT COUNT(*) FROM t1")
		if len(rows) != 1 || toInt64(rows[0][0]) != 4 {
			t.Fatalf("want 4, got %v", rows)
		}
	})

	t.Run("cte_column_alias_mismatch_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"WITH c1(w, z, x1, x2, x3, x4) AS (SELECT id, col1 FROM t1) SELECT * FROM c1")
		if err == nil {
			t.Fatal("expected error for mismatched CTE column count")
		}
	})

	t.Run("duplicate_cte_name_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"WITH c1 AS (SELECT id FROM t1), c1 AS (SELECT id FROM t1) SELECT * FROM c1")
		if err == nil {
			t.Fatal("expected error for duplicate CTE name")
		}
	})

	t.Run("cte_renamed_col_not_visible_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"WITH c1(w, z) AS (SELECT id, col1 FROM t1) SELECT col1 FROM c1")
		if err == nil {
			t.Fatal("expected error: col1 renamed to z")
		}
	})

	t.Run("cte_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`WITH summary AS (
				SELECT col1, SUM(col2) AS total FROM t1 GROUP BY col1
			) SELECT col1, total FROM summary ORDER BY col1`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[0][1]) != 3 {
			t.Errorf("group 10: got %v, want (10, 3)", rows[0])
		}
		if toInt64(rows[1][0]) != 20 || toInt64(rows[1][1]) != 13 {
			t.Errorf("group 20: got %v, want (20, 13)", rows[1])
		}
	})

	t.Run("cte_joined_with_base_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`WITH high_vals AS (
				SELECT id, col1 FROM t1 WHERE col2 >= 6
			) SELECT h.id, h.col1, t.col2
			  FROM high_vals h, t1 t
			  WHERE h.id = t.id
			  ORDER BY h.id`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 6 || toInt64(rows[1][0]) != 7 {
			t.Errorf("got ids %v, %v, want 6, 7", rows[0][0], rows[1][0])
		}
	})
}

func TestFDB_RecursiveCTEBasic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "rcte1",
		"CREATE TABLE nodes (id BIGINT, parent_id BIGINT, name STRING, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO nodes VALUES (1, NULL, 'root')",
		"INSERT INTO nodes VALUES (2, 1, 'child1')",
		"INSERT INTO nodes VALUES (3, 1, 'child2')",
		"INSERT INTO nodes VALUES (4, 2, 'grandchild1')",
		"INSERT INTO nodes VALUES (5, 2, 'grandchild2')",
		"INSERT INTO nodes VALUES (6, 3, 'grandchild3')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("recursive_cte_descendants", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`WITH RECURSIVE descendants AS (
				SELECT id, parent_id, name FROM nodes WHERE id = 1
				UNION ALL
				SELECT n.id, n.parent_id, n.name
				FROM nodes n, descendants d
				WHERE n.parent_id = d.id
			)
			SELECT name FROM descendants ORDER BY id`)
		if len(rows) < 1 {
			t.Fatalf("want at least root, got 0")
		}
		if rows[0][0].(string) != "root" {
			t.Errorf("first should be root, got %v", rows[0][0])
		}
		t.Logf("recursive CTE returned %d rows: %v", len(rows), rows)
	})

	t.Run("recursive_cte_leaf_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT n.name FROM nodes n
			 WHERE NOT EXISTS (
				SELECT 1 FROM nodes c WHERE c.parent_id = n.id
			 )
			 ORDER BY n.name`)
		if len(rows) != 3 {
			t.Fatalf("want 3 leaf nodes, got %d: %v", len(rows), rows)
		}
		want := []string{"grandchild1", "grandchild2", "grandchild3"}
		for i, w := range want {
			if rows[i][0].(string) != w {
				t.Errorf("row %d: got %v, want %s", i, rows[i][0], w)
			}
		}
	})
}

// TestFDB_CTEWithAggregate — CTE + aggregate patterns
func TestFDB_CTEWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctea", "CREATE TABLE cte_data(id BIGINT, category STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cte_data VALUES
		(1, 'X', 10), (2, 'X', 20), (3, 'Y', 30), (4, 'Y', 40), (5, 'Z', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_with_aggregate_in_body", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH totals AS (SELECT category, SUM(amount) AS total FROM cte_data GROUP BY category)
			SELECT * FROM totals ORDER BY total DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "Y" {
			t.Errorf("first should be Y(70), got %v", rows[0][0])
		}
	})

	t.Run("cte_filtered_by_outer_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH all_data AS (SELECT * FROM cte_data)
			SELECT id, amount FROM all_data WHERE amount > 25 ORDER BY id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (30,40,50), got %d: %v", len(rows), rows)
		}
	})

	t.Run("cte_used_twice", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH base AS (SELECT category, SUM(amount) AS total FROM cte_data GROUP BY category)
			SELECT b1.category, b1.total FROM base b1
			WHERE b1.total > 20
			ORDER BY b1.category
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (X=30, Y=70, Z=50 all >20), got %d: %v", len(rows), rows)
		}
	})

	t.Run("cte_count_over_cte", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH filtered AS (SELECT * FROM cte_data WHERE amount >= 20)
			SELECT COUNT(*) FROM filtered
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("COUNT of amount>=20 should be 4, got %v", rows[0][0])
		}
	})
}

// TestFDB_CTEWithJoin — CTE used in JOIN context
func TestFDB_CTEWithJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctejn",
		"CREATE TABLE cj_orders(id BIGINT, customer STRING, total BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE cj_customers(name STRING, tier STRING, PRIMARY KEY(name))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cj_orders VALUES
		(1, 'alice', 100), (2, 'alice', 200), (3, 'bob', 50), (4, 'bob', 150), (5, 'charlie', 300)
	`); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO cj_customers VALUES ('alice', 'gold'), ('bob', 'silver'), ('charlie', 'gold')"); err != nil {
		t.Fatalf("INSERT customers: %v", err)
	}

	t.Run("cte_joined_with_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH order_totals AS (
				SELECT customer, SUM(total) AS sum_total FROM cj_orders GROUP BY customer
			)
			SELECT c.name, c.tier, ot.sum_total
			FROM cj_customers c
			JOIN order_totals ot ON c.name = ot.customer
			ORDER BY c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][2]) != 300 {
			t.Errorf("alice: want sum=300, got %v %v", rows[0][0], rows[0][2])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "bob" || toInt64(rows[1][2]) != 200 {
			t.Errorf("bob: want sum=200, got %v %v", rows[1][0], rows[1][2])
		}
	})

	t.Run("cte_with_having_joined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH big_spenders AS (
				SELECT customer, SUM(total) AS spend FROM cj_orders GROUP BY customer HAVING SUM(total) >= 200
			)
			SELECT c.tier, COUNT(*)
			FROM cj_customers c
			JOIN big_spenders bs ON c.name = bs.customer
			GROUP BY c.tier
			ORDER BY c.tier
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 tiers, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "gold" || toInt64(rows[0][1]) != 2 {
			t.Errorf("gold: want 2 (alice=300, charlie=300), got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "silver" || toInt64(rows[1][1]) != 1 {
			t.Errorf("silver: want 1 (bob=200), got %v %v", rows[1][0], rows[1][1])
		}
	})
}

// TestFDB_MultipleCTEs — multiple CTEs in single query
func TestFDB_MultipleCTEs(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mctes", "CREATE TABLE mc_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO mc_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'B', 30), (4, 'B', 40), (5, 'C', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("two_ctes", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH
				totals AS (SELECT grp, SUM(val) AS total FROM mc_t GROUP BY grp),
				counts AS (SELECT grp, COUNT(*) AS cnt FROM mc_t GROUP BY grp)
			SELECT t.grp, t.total, c.cnt
			FROM totals t JOIN counts c ON t.grp = c.grp
			ORDER BY t.grp
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" || toInt64(rows[0][1]) != 30 || toInt64(rows[0][2]) != 2 {
			t.Errorf("A: want total=30,cnt=2, got %v,%v,%v", rows[0][0], rows[0][1], rows[0][2])
		}
	})

	t.Run("cte_referencing_earlier_cte", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH
				base AS (SELECT * FROM mc_t WHERE val > 15),
				summary AS (SELECT grp, COUNT(*) AS cnt FROM base GROUP BY grp)
			SELECT * FROM summary ORDER BY grp
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_CTEInDML — CTE used in INSERT...SELECT
func TestFDB_CTEInDML(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctdml",
		"CREATE TABLE ct_src(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE ct_dst(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ct_src VALUES (1, 10), (2, 20), (3, 30), (4, 40)"); err != nil {
		t.Fatalf("INSERT src: %v", err)
	}

	t.Run("cte_insert_select_unsupported", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `
			WITH filtered AS (SELECT * FROM ct_src WHERE val > 15)
			INSERT INTO ct_dst SELECT * FROM filtered
		`)
		if err == nil {
			t.Errorf("expected error for CTE in INSERT...SELECT")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("plain_insert_select_works", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO ct_dst SELECT * FROM ct_src WHERE val > 25")
		if err != nil {
			t.Fatalf("INSERT...SELECT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 (val>25: 30,40), got %d", n)
		}
	})
}

// TestFDB_CTERecursiveDepthLimit — recursive CTE hits depth limit
func TestFDB_CTERecursiveDepthLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctrdl", "CREATE TABLE tree(id BIGINT, parent_id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO tree VALUES
		(1, NULL, 'root'), (2, 1, 'child1'), (3, 1, 'child2'),
		(4, 2, 'grandchild1'), (5, 3, 'grandchild2')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("recursive_cte_hits_depth_limit", func(t *testing.T) {
		_, err := db.QueryContext(ctx, `
			WITH RECURSIVE descendants AS (
				SELECT id, name FROM tree WHERE id = 1
				UNION ALL
				SELECT t.id, t.name FROM tree t JOIN descendants d ON t.parent_id = d.id
			)
			SELECT name FROM descendants ORDER BY id
		`)
		if err == nil {
			t.Logf("recursive CTE unexpectedly succeeded")
		} else {
			if !strings.Contains(err.Error(), "depth") {
				t.Logf("error: %v", err)
			}
		}
	})

	t.Run("recursive_cte_leaf_nodes", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT t.name FROM tree t
			WHERE NOT EXISTS (SELECT 1 FROM tree c WHERE c.parent_id = t.id)
			ORDER BY t.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 leaf nodes, got %d: %v", len(rows), rows)
		}
	})
}
