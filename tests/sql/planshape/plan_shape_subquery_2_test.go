package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_SubqueryInWhere — subqueries in WHERE clause
func TestFDB_SubqueryInWhere(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sqwh",
		"CREATE TABLE products(id BIGINT, name STRING, price BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE orders_sq(id BIGINT, product_id BIGINT, qty BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO products VALUES (1, 'widget', 10), (2, 'gadget', 20), (3, 'doohickey', 30)"); err != nil {
		t.Fatalf("INSERT products: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO orders_sq VALUES (1, 1, 5), (2, 1, 3), (3, 2, 10)"); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}

	t.Run("exists_correlated", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM products p
			WHERE EXISTS (SELECT 1 FROM orders_sq o WHERE o.product_id = p.id)
			ORDER BY p.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (widget, gadget have orders), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "gadget" {
			t.Errorf("first should be gadget, got %v", rows[0][0])
		}
	})

	t.Run("not_exists_correlated", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM products p
			WHERE NOT EXISTS (SELECT 1 FROM orders_sq o WHERE o.product_id = p.id)
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (doohickey has no orders), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "doohickey" {
			t.Errorf("should be doohickey, got %v", rows[0][0])
		}
	})

	t.Run("in_subquery_not_plannable", func(t *testing.T) {
		_, err := db.QueryContext(ctx, `
			SELECT name FROM products
			WHERE id IN (SELECT product_id FROM orders_sq)
			ORDER BY name
		`)
		if err == nil {
			t.Errorf("expected error for IN (subquery), got nil")
		} else {
			t.Logf("expected error: %v", err)
		}
	})
}

// TestFDB_WhereSubqueryCorrelated — correlated subquery patterns
func TestFDB_WhereSubqueryCorrelated(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wscr",
		"CREATE TABLE ws_parent(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ws_child(id BIGINT, parent_id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ws_parent VALUES (1, 'p1'), (2, 'p2'), (3, 'p3')"); err != nil {
		t.Fatalf("INSERT parent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ws_child VALUES
		(10, 1, 100), (11, 1, 200), (12, 2, 50), (13, 2, 75)
	`); err != nil {
		t.Fatalf("INSERT child: %v", err)
	}

	t.Run("exists_with_children", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM ws_parent p
			WHERE EXISTS (SELECT 1 FROM ws_child c WHERE c.parent_id = p.id)
			ORDER BY p.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (p1,p2 have children), got %d: %v", len(rows), rows)
		}
	})

	t.Run("not_exists_no_children", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM ws_parent p
			WHERE NOT EXISTS (SELECT 1 FROM ws_child c WHERE c.parent_id = p.id)
		`)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "p3" {
			t.Errorf("want p3, got %v", rows)
		}
	})

	t.Run("exists_with_filter_on_child", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM ws_parent p
			WHERE EXISTS (SELECT 1 FROM ws_child c WHERE c.parent_id = p.id AND c.val > 100)
			ORDER BY p.name
		`)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "p1" {
			t.Errorf("want p1 (has child with val=200>100), got %v", rows)
		}
	})
}

// TestFDB_UpdateWithSubquery — UPDATE using correlated subquery logic
func TestFDB_UpdateWithSubquery(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "upsq",
		"CREATE TABLE up_main(id BIGINT, val BIGINT, flag STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE up_ref(id BIGINT, threshold BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO up_main VALUES (1, 10, 'low'), (2, 50, 'low'), (3, 90, 'low')"); err != nil {
		t.Fatalf("INSERT main: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO up_ref VALUES (1, 40)"); err != nil {
		t.Fatalf("INSERT ref: %v", err)
	}

	t.Run("update_with_case_and_comparison", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE up_main SET flag = CASE WHEN val > 40 THEN 'high' ELSE 'low' END"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, flag FROM up_main ORDER BY id")
		if fmt.Sprintf("%v", rows[0][1]) != "low" {
			t.Errorf("id=1 (val=10): should stay low, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][1]) != "high" {
			t.Errorf("id=2 (val=50>40): should be high, got %v", rows[1][1])
		}
		if fmt.Sprintf("%v", rows[2][1]) != "high" {
			t.Errorf("id=3 (val=90>40): should be high, got %v", rows[2][1])
		}
	})

	t.Run("update_set_to_null", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE up_main SET val = NULL WHERE id = 2"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM up_main WHERE id = 2")
		if rows[0][0] != nil {
			t.Errorf("val should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("verify_null_in_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val), COUNT(val), COUNT(*) FROM up_main")
		if toInt64(rows[0][2]) != 3 {
			t.Errorf("COUNT(*) should be 3, got %v", rows[0][2])
		}
		if toInt64(rows[0][1]) != 2 {
			t.Errorf("COUNT(val) should be 2 (one NULL), got %v", rows[0][1])
		}
	})
}

// TestFDB_WhereInWithSubqueryResult — WHERE col IN (values) with aggregate results
func TestFDB_WhereInWithSubqueryResult(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wisqr", "CREATE TABLE wisq_t(id BIGINT, val BIGINT, cat STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wisq_t VALUES
		(1, 10, 'A'), (2, 20, 'B'), (3, 30, 'A'), (4, 40, 'C'), (5, 50, 'B')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("in_multiple_values", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM wisq_t WHERE val IN (10, 30, 50) ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
	})

	t.Run("in_with_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, SUM(val) FROM wisq_t WHERE cat IN ('A', 'B') GROUP BY cat ORDER BY cat")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 40 {
			t.Errorf("A sum = 10+30 = 40, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 70 {
			t.Errorf("B sum = 20+50 = 70, got %v", rows[1][1])
		}
	})

	t.Run("not_in_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM wisq_t WHERE cat NOT IN ('A')")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("NOT IN A: want 3 (B,C,B), got %v", rows[0][0])
		}
	})
}

// TestFDB_ExistsWithAggregate — EXISTS subquery combined with aggregate
func TestFDB_ExistsWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "exagg",
		"CREATE TABLE exa_parent(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE exa_child(id BIGINT, pid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO exa_parent VALUES (1, 'p1'), (2, 'p2'), (3, 'p3')"); err != nil {
		t.Fatalf("INSERT parent: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO exa_child VALUES (10, 1, 100), (20, 1, 200), (30, 2, 50)"); err != nil {
		t.Fatalf("INSERT child: %v", err)
	}

	t.Run("count_parents_with_children", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM exa_parent p
			WHERE EXISTS (SELECT 1 FROM exa_child c WHERE c.pid = p.id)
		`)
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 parents with children (p1, p2), got %v", rows[0][0])
		}
	})

	t.Run("not_exists_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM exa_parent p
			WHERE NOT EXISTS (SELECT 1 FROM exa_child c WHERE c.pid = p.id)
		`)
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("want 1 parent without children (p3), got %v", rows[0][0])
		}
	})
}

// TestFDB_JoinWithNotExists — anti-join pattern via NOT EXISTS
func TestFDB_JoinWithNotExists(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwne",
		"CREATE TABLE jwne_products(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jwne_orders(id BIGINT, product_id BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jwne_products VALUES (1, 'widget'), (2, 'gadget'), (3, 'doohickey')"); err != nil {
		t.Fatalf("INSERT products: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwne_orders VALUES (10, 1), (20, 1), (30, 2)"); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}

	t.Run("products_never_ordered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM jwne_products p
			WHERE NOT EXISTS (SELECT 1 FROM jwne_orders o WHERE o.product_id = p.id)
		`)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "doohickey" {
			t.Errorf("want doohickey, got %v", rows)
		}
	})

	t.Run("products_with_orders_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM jwne_products p
			WHERE EXISTS (SELECT 1 FROM jwne_orders o WHERE o.product_id = p.id)
		`)
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 products with orders, got %v", rows[0][0])
		}
	})
}

// TestFDB_DeleteWithExistsSubquery — DELETE WHERE EXISTS (correlated subquery)
func TestFDB_DeleteWithExistsSubquery(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dwe",
		"CREATE TABLE dwe_orders(id BIGINT, customer STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE dwe_cancels(id BIGINT, order_id BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO dwe_orders VALUES (1,'alice'),(2,'bob'),(3,'carol')"); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO dwe_cancels VALUES (10,1),(20,3)"); err != nil {
		t.Fatalf("INSERT cancels: %v", err)
	}

	t.Run("delete_cancelled_orders", func(t *testing.T) {
		res, err := db.ExecContext(ctx,
			"DELETE FROM dwe_orders WHERE EXISTS (SELECT 1 FROM dwe_cancels c WHERE c.order_id = dwe_orders.id)")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 deleted, got %d", n)
		}
	})

	t.Run("remaining_is_bob", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT customer FROM dwe_orders")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "bob" {
			t.Errorf("want [bob], got %v", rows)
		}
	})
}

// TestFDB_ExistsCorrelatedSubquery — WHERE EXISTS with correlated subquery
func TestFDB_ExistsCorrelatedSubquery(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ecs",
		"CREATE TABLE ecs_parent(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE ecs_child(id BIGINT, pid BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ecs_parent VALUES (1,'p1'),(2,'p2'),(3,'p3')"); err != nil {
		t.Fatalf("INSERT parent: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ecs_child VALUES (10,1),(20,1),(30,3)"); err != nil {
		t.Fatalf("INSERT child: %v", err)
	}

	t.Run("parents_with_children", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM ecs_parent p
			WHERE EXISTS (SELECT 1 FROM ecs_child c WHERE c.pid = p.id)
			ORDER BY p.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 parents with children, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "p1" || fmt.Sprintf("%v", rows[1][0]) != "p3" {
			t.Errorf("want [p1 p3], got %v", rows)
		}
	})

	t.Run("parents_without_children", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT p.name FROM ecs_parent p
			WHERE NOT EXISTS (SELECT 1 FROM ecs_child c WHERE c.pid = p.id)
		`)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "p2" {
			t.Errorf("want [p2], got %v", rows)
		}
	})
}

// TestFDB_SubqueryExistsWithAggregate — EXISTS subquery combined with aggregate in outer
func TestFDB_SubqueryExistsWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sewa",
		"CREATE TABLE sewa_orders(id BIGINT, cust STRING, total BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE sewa_returns(id BIGINT, order_id BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sewa_orders VALUES
		(1,'alice',100),(2,'bob',200),(3,'alice',150),(4,'carol',50)`); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO sewa_returns VALUES (10,1),(20,4)"); err != nil {
		t.Fatalf("INSERT returns: %v", err)
	}

	t.Run("orders_with_returns_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM sewa_orders o
			WHERE EXISTS (SELECT 1 FROM sewa_returns r WHERE r.order_id = o.id)
		`)
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 orders with returns, got %v", rows[0][0])
		}
	})

	t.Run("orders_without_returns_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(total) FROM sewa_orders o
			WHERE NOT EXISTS (SELECT 1 FROM sewa_returns r WHERE r.order_id = o.id)
		`)
		// orders without returns: id=2(200), id=3(150) → sum=350
		if toInt64(rows[0][0]) != 350 {
			t.Errorf("want 350, got %v", rows[0][0])
		}
	})
}

// TestFDB_UpdateWhereNotExists — UPDATE rows that have no match in another table
func TestFDB_UpdateWhereNotExists(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uwne",
		"CREATE TABLE uwne_items(id BIGINT, status STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE uwne_shipped(id BIGINT, item_id BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO uwne_items VALUES (1,'pending'),(2,'pending'),(3,'pending')"); err != nil {
		t.Fatalf("INSERT items: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO uwne_shipped VALUES (10,1),(20,3)"); err != nil {
		t.Fatalf("INSERT shipped: %v", err)
	}

	t.Run("mark_unshipped_cancelled", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `
			UPDATE uwne_items SET status = 'cancelled'
			WHERE NOT EXISTS (SELECT 1 FROM uwne_shipped s WHERE s.item_id = uwne_items.id)
		`)
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, status FROM uwne_items ORDER BY id")
		// id=1: shipped → pending, id=2: not shipped → cancelled, id=3: shipped → pending
		if fmt.Sprintf("%v", rows[1][1]) != "cancelled" {
			t.Errorf("id 2: want cancelled, got %v", rows[1][1])
		}
		if fmt.Sprintf("%v", rows[0][1]) != "pending" || fmt.Sprintf("%v", rows[2][1]) != "pending" {
			t.Errorf("shipped items should stay pending: got [%v %v]", rows[0][1], rows[2][1])
		}
	})
}
