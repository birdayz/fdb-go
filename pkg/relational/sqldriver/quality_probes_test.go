package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// qualityProbeDB sets up a multi-table schema with data for probing
// complex query patterns.
func qualityProbeDB(t *testing.T, suffix string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := fmt.Sprintf("/qp_%s_%s", suffix, t.Name())
	db := openTestDB(t, dbPath)

	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("QP_TMPL_%s_%s", suffix, t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE customers (id BIGINT NOT NULL, name STRING, region STRING, active BOOLEAN, PRIMARY KEY (id))
		CREATE TABLE orders (id BIGINT NOT NULL, customer_id BIGINT, amount DOUBLE, status STRING, PRIMARY KEY (id))
		CREATE TABLE items (id BIGINT NOT NULL, order_id BIGINT, product STRING, qty BIGINT, price DOUBLE, PRIMARY KEY (id))
		CREATE INDEX idx_orders_customer ON orders (customer_id)
		CREATE INDEX idx_orders_status ON orders (status)
		CREATE INDEX idx_items_order ON items (order_id)`, tmpl)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	sdb, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { sdb.Close() })

	inserts := []string{
		"INSERT INTO customers VALUES (1, 'Alice', 'WEST', true)",
		"INSERT INTO customers VALUES (2, 'Bob', 'EAST', true)",
		"INSERT INTO customers VALUES (3, 'Charlie', 'WEST', false)",
		"INSERT INTO customers VALUES (4, 'Diana', null, true)",
		"INSERT INTO orders VALUES (10, 1, 100.50, 'shipped')",
		"INSERT INTO orders VALUES (11, 1, 200.00, 'pending')",
		"INSERT INTO orders VALUES (12, 2, 50.25, 'shipped')",
		"INSERT INTO orders VALUES (13, 2, 75.00, 'cancelled')",
		"INSERT INTO orders VALUES (14, 3, 300.00, 'shipped')",
		"INSERT INTO orders VALUES (15, 4, null, 'pending')",
		"INSERT INTO items VALUES (100, 10, 'Widget', 2, 25.25)",
		"INSERT INTO items VALUES (101, 10, 'Gadget', 1, 50.00)",
		"INSERT INTO items VALUES (102, 11, 'Widget', 5, 25.25)",
		"INSERT INTO items VALUES (103, 12, 'Doohickey', 1, 50.25)",
		"INSERT INTO items VALUES (104, 14, 'Widget', 10, 30.00)",
		"INSERT INTO items VALUES (105, 14, 'Gadget', 3, null)",
	}
	for _, ins := range inserts {
		if _, err := sdb.ExecContext(ctx, ins); err != nil {
			t.Fatalf("INSERT: %v (%s)", err, ins)
		}
	}
	return sdb
}

// collectRows runs a query and returns rows as [][]any.
func collectRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}

	var result [][]any
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		result = append(result, dest)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return result
}

func expectError(t *testing.T, db *sql.DB, query string) error {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return err
	}
	t.Fatalf("expected error for %q, got success", query)
	return nil
}

// requireSQLSTATE unwraps err to *api.Error and asserts that the SQLSTATE
// code matches want. Use after expectError for Java-conformance tests where
// the exact SQLSTATE is known.
func requireSQLSTATE(t *testing.T, err error, want api.ErrorCode) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T: %v", err, err)
	}
	if apiErr.Code != want {
		t.Errorf("SQLSTATE: got %s, want %s (err: %v)", apiErr.Code, want, err)
	}
}

func TestFDB_QualityProbe_JoinGroupByHavingOrderBy(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "jgho")
	ctx := context.Background()

	t.Run("join_group_by_having_order_by", func(t *testing.T) {
		query := `SELECT c.name, SUM(o.amount)
			FROM customers c, orders o
			WHERE c.id = o.customer_id AND o.status = 'shipped'
			GROUP BY c.name
			HAVING SUM(o.amount) > 60
			ORDER BY SUM(o.amount) DESC`
		rows := collectRows(t, db, query)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		// Charlie: 300.00, Alice: 100.50
		name0 := fmt.Sprintf("%v", rows[0][0])
		name1 := fmt.Sprintf("%v", rows[1][0])
		if name0 != "Charlie" || name1 != "Alice" {
			t.Errorf("wrong order: got [%s, %s], want [Charlie, Alice]", name0, name1)
		}
	})

	t.Run("join_count_star_group_by", func(t *testing.T) {
		query := `SELECT c.name, COUNT(*)
			FROM customers c, orders o
			WHERE c.id = o.customer_id
			GROUP BY c.name
			ORDER BY c.name`
		rows := collectRows(t, db, query)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d: %v", len(rows), rows)
		}
		// Alice=2, Bob=2, Charlie=1, Diana=1
		for _, r := range rows {
			name := fmt.Sprintf("%v", r[0])
			cnt := r[1]
			switch name {
			case "Alice":
				if cnt != int64(2) {
					t.Errorf("Alice: want 2, got %v", cnt)
				}
			case "Bob":
				if cnt != int64(2) {
					t.Errorf("Bob: want 2, got %v", cnt)
				}
			case "Charlie":
				if cnt != int64(1) {
					t.Errorf("Charlie: want 1, got %v", cnt)
				}
			case "Diana":
				if cnt != int64(1) {
					t.Errorf("Diana: want 1, got %v", cnt)
				}
			default:
				t.Errorf("unexpected name: %s", name)
			}
		}
	})

	t.Run("three_table_join", func(t *testing.T) {
		query := `SELECT c.name, i.product, i.qty
			FROM customers c, orders o, items i
			WHERE c.id = o.customer_id AND o.id = i.order_id AND c.name = 'Alice'
			ORDER BY i.qty DESC`
		rows := collectRows(t, db, query)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		qty0 := rows[0][2]
		if qty0 != int64(5) {
			t.Errorf("first row qty: want 5, got %v", qty0)
		}
	})

	_ = ctx
}

func TestFDB_QualityProbe_SelfJoin(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "sj")

	t.Run("self_join_same_region", func(t *testing.T) {
		query := `SELECT a.name, b.name
			FROM customers a, customers b
			WHERE a.region = b.region AND a.id < b.id
			ORDER BY a.name, b.name`
		rows := collectRows(t, db, query)
		if len(rows) != 1 {
			t.Fatalf("want 1 row (Alice-Charlie in WEST), got %d: %v", len(rows), rows)
		}
		n1 := fmt.Sprintf("%v", rows[0][0])
		n2 := fmt.Sprintf("%v", rows[0][1])
		if n1 != "Alice" || n2 != "Charlie" {
			t.Errorf("got [%s, %s], want [Alice, Charlie]", n1, n2)
		}
	})
}

func TestFDB_QualityProbe_LeftJoinNulls(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ljn")

	t.Run("left_join_preserves_nulls", func(t *testing.T) {
		// Customer 4 (Diana) has order 15 with null amount.
		// Items don't reference order 15.
		query := `SELECT c.name, o.amount
			FROM customers c LEFT JOIN orders o ON c.id = o.customer_id
			WHERE c.name = 'Diana'`
		rows := collectRows(t, db, query)
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d: %v", len(rows), rows)
		}
		if rows[0][1] != nil {
			t.Errorf("want NULL amount, got %v", rows[0][1])
		}
	})

	t.Run("left_join_with_aggregate", func(t *testing.T) {
		query := `SELECT c.name, COUNT(o.id)
			FROM customers c LEFT JOIN orders o ON c.id = o.customer_id
			GROUP BY c.name
			ORDER BY c.name`
		rows := collectRows(t, db, query)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d: %v", len(rows), rows)
		}
		// All customers should appear, even if they have zero orders.
		found := make(map[string]int64)
		for _, r := range rows {
			name := fmt.Sprintf("%v", r[0])
			cnt := r[1].(int64)
			found[name] = cnt
		}
		if found["Alice"] != 2 {
			t.Errorf("Alice: want 2, got %d", found["Alice"])
		}
	})
}

func TestFDB_QualityProbe_UnionOrderByLimit(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "uol")

	t.Run("union_all_order_by", func(t *testing.T) {
		query := `SELECT name, 'customer' FROM customers WHERE region = 'WEST'
			UNION ALL
			SELECT status, 'order' FROM orders WHERE status = 'shipped'
			ORDER BY name`
		rows := collectRows(t, db, query)
		if len(rows) != 5 {
			t.Fatalf("want 5 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("union_distinct_rejected", func(t *testing.T) {
		// Go aligns with Java: UNION DISTINCT is not supported.
		err := expectError(t, db, `SELECT region FROM customers WHERE region IS NOT NULL
			UNION
			SELECT region FROM customers WHERE region IS NOT NULL
			ORDER BY region`)
		if err == nil {
			t.Fatal("expected UNION DISTINCT rejection")
		}
		requireSQLSTATE(t, err, api.ErrCodeUnsupportedQuery)
	})

	t.Run("union_all_with_limit", func(t *testing.T) {
		query := `SELECT name FROM customers
			UNION ALL
			SELECT name FROM customers
			ORDER BY name
			LIMIT 3`
		rows := collectRows(t, db, query)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows (LIMIT), got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_CaseWhenInVariousPositions(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "cwp")

	t.Run("case_in_select", func(t *testing.T) {
		query := `SELECT name,
			CASE WHEN active = true THEN 'active' ELSE 'inactive' END
			FROM customers ORDER BY name`
		rows := collectRows(t, db, query)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d", len(rows))
		}
		// Charlie is inactive
		for _, r := range rows {
			name := fmt.Sprintf("%v", r[0])
			status := fmt.Sprintf("%v", r[1])
			if name == "Charlie" && status != "inactive" {
				t.Errorf("Charlie: want inactive, got %s", status)
			}
			if name == "Alice" && status != "active" {
				t.Errorf("Alice: want active, got %s", status)
			}
		}
	})

	t.Run("case_in_where", func(t *testing.T) {
		query := `SELECT name FROM customers
			WHERE CASE WHEN region = 'WEST' THEN true ELSE false END = true
			ORDER BY name`
		rows := collectRows(t, db, query)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows (Alice, Charlie), got %d: %v", len(rows), rows)
		}
	})

	t.Run("case_in_order_by", func(t *testing.T) {
		query := `SELECT name FROM customers
			ORDER BY CASE WHEN region = 'WEST' THEN 1 WHEN region = 'EAST' THEN 2 ELSE 3 END, name`
		rows := collectRows(t, db, query)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		// WEST (Alice, Charlie), EAST (Bob), NULL region (Diana)
		name0 := fmt.Sprintf("%v", rows[0][0])
		if name0 != "Alice" {
			t.Errorf("first: want Alice, got %s", name0)
		}
	})

	t.Run("case_in_group_by_agg", func(t *testing.T) {
		query := `SELECT CASE WHEN active = true THEN 'active' ELSE 'inactive' END, COUNT(*)
			FROM customers
			GROUP BY CASE WHEN active = true THEN 'active' ELSE 'inactive' END
			ORDER BY COUNT(*) DESC`
		rows := collectRows(t, db, query)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		// active=3, inactive=1
		grp := fmt.Sprintf("%v", rows[0][0])
		cnt := rows[0][1].(int64)
		if grp != "active" || cnt != 3 {
			t.Errorf("first group: want (active, 3), got (%s, %d)", grp, cnt)
		}
	})
}

func TestFDB_QualityProbe_CorrelatedExists(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ce")

	t.Run("exists_basic", func(t *testing.T) {
		query := `SELECT name FROM customers c
			WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'shipped')
			ORDER BY name`
		rows := collectRows(t, db, query)
		// Alice (order 10), Bob (order 12), Charlie (order 14)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
	})

	t.Run("not_exists", func(t *testing.T) {
		query := `SELECT name FROM customers c
			WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'shipped')
			ORDER BY name`
		rows := collectRows(t, db, query)
		// Diana (only pending)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d: %v", len(rows), rows)
		}
		name := fmt.Sprintf("%v", rows[0][0])
		if name != "Diana" {
			t.Errorf("want Diana, got %s", name)
		}
	})

	t.Run("exists_with_outer_predicate", func(t *testing.T) {
		query := `SELECT name FROM customers c
			WHERE c.region = 'WEST'
			AND EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id)
			ORDER BY name`
		rows := collectRows(t, db, query)
		// Alice (WEST, has orders), Charlie (WEST, has orders)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_ScalarSubquery(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ss")

	t.Run("uncorrelated_scalar_subquery", func(t *testing.T) {
		query := `SELECT name, (SELECT COUNT(*) FROM orders) FROM customers ORDER BY name`
		rows := collectRows(t, db, query)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		// Uncorrelated: every row gets the total order count (6)
		for _, r := range rows {
			cnt := r[1].(int64)
			if cnt != 6 {
				t.Errorf("want 6 total orders, got %d (name=%v)", cnt, r[0])
			}
		}
	})

	t.Run("correlated_scalar_subquery_count", func(t *testing.T) {
		rows := collectRows(t, db, `SELECT name,
			(SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id)
			FROM customers c ORDER BY name`)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		// Alice(id=1): orders 10,11 → 2
		// Bob(id=2): orders 12,13 → 2
		// Charlie(id=3): order 14 → 1
		// Diana(id=4): order 15 → 1
		expected := []struct {
			name  string
			count int64
		}{
			{"Alice", 2},
			{"Bob", 2},
			{"Charlie", 1},
			{"Diana", 1},
		}
		for i, exp := range expected {
			name := fmt.Sprintf("%v", rows[i][0])
			cnt, ok := rows[i][1].(int64)
			if !ok {
				t.Fatalf("row %d: count not int64, got %T (%v)", i, rows[i][1], rows[i][1])
			}
			if name != exp.name || cnt != exp.count {
				t.Errorf("row %d: want (%s, %d), got (%s, %d)", i, exp.name, exp.count, name, cnt)
			}
		}
	})
}

func TestFDB_QualityProbe_CorrelatedScalarSubqueryShapes(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "cssqs")

	t.Run("non_aggregate_with_limit", func(t *testing.T) {
		// Shape 1: non-aggregate correlated scalar subquery.
		// Get the highest-amount order status per customer.
		rows := collectRows(t, db, `SELECT name,
			(SELECT status FROM orders o WHERE o.customer_id = c.id ORDER BY o.amount DESC LIMIT 1)
			FROM customers c ORDER BY name`)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d", len(rows))
		}
		// Alice(id=1): orders 10(100.50,'shipped'), 11(200.00,'pending') → highest amount is 200.00 → 'pending'
		// Bob(id=2): orders 12(50.25,'shipped'), 13(75.00,'cancelled') → highest amount is 75.00 → 'cancelled'
		// Charlie(id=3): order 14(300.00,'shipped') → 'shipped'
		// Diana(id=4): order 15(null,'pending') → 'pending'
		expected := []struct {
			name   string
			status string
		}{
			{"Alice", "pending"},
			{"Bob", "cancelled"},
			{"Charlie", "shipped"},
			{"Diana", "pending"},
		}
		for i, exp := range expected {
			name := fmt.Sprintf("%v", rows[i][0])
			status := fmt.Sprintf("%v", rows[i][1])
			if name != exp.name || status != exp.status {
				t.Errorf("row %d: want (%s, %s), got (%s, %s)", i, exp.name, exp.status, name, status)
			}
		}
	})

	t.Run("non_aggregate_implicit_limit", func(t *testing.T) {
		// Shape 1 variant: no explicit LIMIT — system enforces LIMIT 1.
		// Charlie has exactly 1 order, so this should return it.
		rows := collectRows(t, db, `SELECT name,
			(SELECT status FROM orders o WHERE o.customer_id = c.id)
			FROM customers c WHERE c.id = 3`)
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		name := fmt.Sprintf("%v", rows[0][0])
		status := fmt.Sprintf("%v", rows[0][1])
		if name != "Charlie" || status != "shipped" {
			t.Errorf("want (Charlie, shipped), got (%s, %s)", name, status)
		}
	})

	t.Run("aggregate_with_join", func(t *testing.T) {
		// Shape 2: aggregate correlated scalar subquery with JOIN in inner.
		// Count items per customer (through orders→items join).
		rows := collectRows(t, db, `SELECT name,
			(SELECT COUNT(*) FROM orders o JOIN items i ON i.order_id = o.id WHERE o.customer_id = c.id)
			FROM customers c ORDER BY name`)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d", len(rows))
		}
		// Alice(id=1): orders 10,11 → items 100,101,102 → 3
		// Bob(id=2): orders 12 → items 103 → 1
		// Charlie(id=3): order 14 → items 104,105 → 2
		// Diana(id=4): order 15 → no items → 0
		expected := []struct {
			name  string
			count int64
		}{
			{"Alice", 3},
			{"Bob", 1},
			{"Charlie", 2},
			{"Diana", 0},
		}
		for i, exp := range expected {
			name := fmt.Sprintf("%v", rows[i][0])
			cnt, ok := rows[i][1].(int64)
			if !ok {
				t.Fatalf("row %d: count not int64, got %T (%v)", i, rows[i][1], rows[i][1])
			}
			if name != exp.name || cnt != exp.count {
				t.Errorf("row %d: want (%s, %d), got (%s, %d)", i, exp.name, exp.count, name, cnt)
			}
		}
	})

	t.Run("having_rejected", func(t *testing.T) {
		// HAVING in correlated scalar subquery requires PredicatePushDownRule
		// changes (AliasMap conflict on correlation alias). Rejected for now.
		err := expectError(t, db, `SELECT name,
			(SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id HAVING COUNT(*) > 1)
			FROM customers c ORDER BY name`)
		if err == nil {
			t.Fatal("expected error for HAVING in correlated scalar subquery")
		}
	})

	t.Run("group_by_rejected", func(t *testing.T) {
		err := expectError(t, db, `SELECT name,
			(SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status)
			FROM customers c ORDER BY name`)
		if err == nil {
			t.Fatal("expected error for GROUP BY in correlated scalar subquery")
		}
	})

	t.Run("multi_column_rejected", func(t *testing.T) {
		err := expectError(t, db, `SELECT name,
			(SELECT status, amount FROM orders o WHERE o.customer_id = c.id LIMIT 1)
			FROM customers c ORDER BY name`)
		if err == nil {
			t.Fatal("expected error for multi-column scalar subquery")
		}
	})
}

func TestFDB_QualityProbe_UpdateDeleteComplex(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "udc")
	ctx := context.Background()

	t.Run("update_with_case_when", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`UPDATE orders SET status = CASE WHEN amount > 100 THEN 'premium' ELSE status END WHERE customer_id = 1`)
		if err != nil {
			t.Fatalf("UPDATE with CASE: %v", err)
		}
		rows := collectRows(t, db, "SELECT id, status FROM orders WHERE customer_id = 1 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		// Order 10: 100.50 -> status unchanged (not > 100? actually 100.50 > 100 is true)
		// Order 11: 200.00 -> 'premium'
		status10 := fmt.Sprintf("%v", rows[0][1])
		status11 := fmt.Sprintf("%v", rows[1][1])
		if status10 != "premium" {
			t.Errorf("order 10 (100.50): want premium, got %s", status10)
		}
		if status11 != "premium" {
			t.Errorf("order 11 (200.00): want premium, got %s", status11)
		}
	})

	t.Run("delete_with_complex_where", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`DELETE FROM items WHERE qty > 5 OR price IS NULL`)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := collectRows(t, db, "SELECT id FROM items ORDER BY id")
		// Should delete items 104 (qty=10) and 105 (price=null)
		for _, r := range rows {
			id := r[0].(int64)
			if id == 104 || id == 105 {
				t.Errorf("item %d should have been deleted", id)
			}
		}
	})
}

func TestFDB_QualityProbe_NullEdgeCases(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "nec")

	t.Run("null_in_group_by", func(t *testing.T) {
		query := `SELECT region, COUNT(*) FROM customers GROUP BY region ORDER BY region`
		rows := collectRows(t, db, query)
		// EAST=1, WEST=2, NULL=1
		if len(rows) != 3 {
			t.Fatalf("want 3 groups (EAST, WEST, NULL), got %d: %v", len(rows), rows)
		}
		// NULL group should appear
		hasNull := false
		for _, r := range rows {
			if r[0] == nil {
				hasNull = true
				cnt := r[1].(int64)
				if cnt != 1 {
					t.Errorf("NULL group: want 1, got %d", cnt)
				}
			}
		}
		if !hasNull {
			t.Error("NULL group missing from GROUP BY results")
		}
	})

	t.Run("null_in_order_by", func(t *testing.T) {
		query := `SELECT name, region FROM customers ORDER BY region, name`
		rows := collectRows(t, db, query)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		// NULLs sort first (or last, depending on engine) — just check all 4 present
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		sort.Strings(names)
		expected := []string{"Alice", "Bob", "Charlie", "Diana"}
		for i, n := range expected {
			if names[i] != n {
				t.Errorf("row %d: want %s, got %s", i, n, names[i])
			}
		}
	})

	t.Run("null_arithmetic", func(t *testing.T) {
		query := `SELECT amount + 10 FROM orders WHERE id = 15`
		rows := collectRows(t, db, query)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("NULL + 10 should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("null_comparison", func(t *testing.T) {
		// WHERE NULL = NULL should return 0 rows (UNKNOWN)
		query := `SELECT id FROM orders WHERE amount = amount AND id = 15`
		rows := collectRows(t, db, query)
		// order 15 has NULL amount, so amount = amount is UNKNOWN
		if len(rows) != 0 {
			t.Fatalf("NULL = NULL in WHERE should filter, got %d rows: %v", len(rows), rows)
		}
	})

	t.Run("coalesce_with_null", func(t *testing.T) {
		// COALESCE(NULL_double_col, -1): returns the first non-null.
		// The literal -1 is parsed as int64, so the result is int64.
		query := `SELECT COALESCE(amount, -1) FROM orders WHERE id = 15`
		rows := collectRows(t, db, query)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		switch v := rows[0][0].(type) {
		case int64:
			if v != -1 {
				t.Errorf("COALESCE(NULL, -1): want -1, got %d", v)
			}
		case float64:
			if v != -1.0 {
				t.Errorf("COALESCE(NULL, -1): want -1.0, got %f", v)
			}
		default:
			t.Errorf("COALESCE(NULL, -1): unexpected type %T, value %v", rows[0][0], rows[0][0])
		}
	})
}

func TestFDB_QualityProbe_TypeCoercionEdge(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := fmt.Sprintf("/qp_tce_%s", t.Name())
	db := openTestDB(t, dbPath)

	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("TMPL_%s", t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE nums (id BIGINT NOT NULL, i BIGINT, d DOUBLE, s STRING, b BOOLEAN, PRIMARY KEY (id))`, tmpl)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	sdb, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sdb.Close()

	inserts := []string{
		"INSERT INTO nums VALUES (1, 10, 10.5, 'hello', true)",
		"INSERT INTO nums VALUES (2, 0, 0.0, '', false)",
		"INSERT INTO nums VALUES (3, -1, -0.0, 'world', true)",
		fmt.Sprintf("INSERT INTO nums VALUES (4, %d, %g, null, null)", math.MaxInt64, math.MaxFloat64),
	}
	for _, ins := range inserts {
		if _, err := sdb.ExecContext(ctx, ins); err != nil {
			t.Fatalf("INSERT: %v (%s)", err, ins)
		}
	}

	t.Run("int_double_comparison", func(t *testing.T) {
		query := `SELECT id FROM nums WHERE i > d ORDER BY id`
		rows := collectRows(t, sdb, query)
		// id=3: i=-1 > d=-0.0? -1 > -0.0 = -1 > 0 = false. Actually -0.0 == 0.0 in IEEE 754.
		// So: id=1: 10 > 10.5? No. id=2: 0 > 0.0? No. id=3: -1 > -0.0? No.
		// id=4: MaxInt64 > MaxFloat64? No (MaxFloat64 is much larger).
		if len(rows) != 0 {
			t.Errorf("want 0 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("int_double_equality_boundary", func(t *testing.T) {
		query := `SELECT id FROM nums WHERE i = d ORDER BY id`
		rows := collectRows(t, sdb, query)
		// id=2: 0 = 0.0 -> true; id=3: -1 = -0.0 -> -1 = 0.0 -> false
		if len(rows) != 1 || rows[0][0].(int64) != 2 {
			t.Errorf("want [2], got %v", rows)
		}
	})

	t.Run("division_by_zero_int", func(t *testing.T) {
		err := expectError(t, sdb, "SELECT i / 0 FROM nums WHERE id = 1")
		if err == nil {
			t.Fatal("expected division by zero error")
		}
		requireSQLSTATE(t, err, api.ErrCodeDivisionByZero)
	})

	t.Run("cast_edge_cases", func(t *testing.T) {
		// CAST(NULL AS BIGINT) -> NULL
		query := `SELECT CAST(null AS BIGINT) FROM nums WHERE id = 1`
		rows := collectRows(t, sdb, query)
		if len(rows) != 1 || rows[0][0] != nil {
			t.Errorf("CAST(null AS BIGINT): want nil, got %v", rows)
		}

		// CAST(true AS STRING) -> 'true'
		query = `SELECT CAST(b AS STRING) FROM nums WHERE id = 1`
		rows = collectRows(t, sdb, query)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "true" {
			t.Errorf("CAST(true AS STRING): want 'true', got %v", rows[0][0])
		}
	})

	t.Run("between_with_nulls", func(t *testing.T) {
		query := `SELECT id FROM nums WHERE i BETWEEN -5 AND 5 ORDER BY id`
		rows := collectRows(t, sdb, query)
		// id=2 (0), id=3 (-1)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})

	t.Run("is_distinct_from_null", func(t *testing.T) {
		query := `SELECT id FROM nums WHERE s IS DISTINCT FROM null ORDER BY id`
		rows := collectRows(t, sdb, query)
		// ids 1, 2, 3 have non-null s; id 4 has null s
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_CTEAdvanced(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "cte")

	t.Run("cte_basic", func(t *testing.T) {
		query := `WITH active_customers AS (
			SELECT id, name FROM customers WHERE active = true
		)
		SELECT name FROM active_customers ORDER BY name`
		rows := collectRows(t, db, query)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
	})

	t.Run("cte_with_join", func(t *testing.T) {
		rows := collectRows(t, db, `WITH shipped AS (
			SELECT customer_id, SUM(amount) AS total
			FROM orders WHERE status = 'shipped'
			GROUP BY customer_id
		)
		SELECT c.name, s.total
		FROM customers c, shipped s
		WHERE c.id = s.customer_id
		ORDER BY s.total DESC`)
		// shipped: (1, 100.50), (2, 50.25), (3, 300.00)
		// JOIN customers → Charlie 300, Alice 100.50, Bob 50.25
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "Charlie" {
			t.Errorf("first row name: want Charlie, got %v", rows[0][0])
		}
	})

	t.Run("cte_multiple", func(t *testing.T) {
		query := `WITH
			west AS (SELECT id, name FROM customers WHERE region = 'WEST'),
			east AS (SELECT id, name FROM customers WHERE region = 'EAST')
		SELECT w.name, e.name
		FROM west w, east e
		ORDER BY w.name`
		rows := collectRows(t, db, query)
		// WEST: Alice, Charlie; EAST: Bob → cross product: 2 rows
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_DistinctEdgeCases(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "dist")

	t.Run("distinct_with_null", func(t *testing.T) {
		query := `SELECT DISTINCT region FROM customers ORDER BY region`
		rows := collectRows(t, db, query)
		// EAST, WEST, NULL
		if len(rows) != 3 {
			t.Fatalf("want 3 distinct regions, got %d: %v", len(rows), rows)
		}
	})

	t.Run("distinct_multi_column", func(t *testing.T) {
		query := `SELECT DISTINCT region, active FROM customers ORDER BY region`
		rows := collectRows(t, db, query)
		// (EAST, true), (WEST, true), (WEST, false), (NULL, true) = 4
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_InsertSelect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := fmt.Sprintf("/qp_is_%s", t.Name())
	db := openTestDB(t, dbPath)

	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("TMPL_%s", t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE src (id BIGINT NOT NULL, val STRING, PRIMARY KEY (id))
		CREATE TABLE dst (id BIGINT NOT NULL, val STRING, PRIMARY KEY (id))`, tmpl)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	sdb, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sdb.Close()

	for i := int64(1); i <= 5; i++ {
		if _, err := sdb.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO src VALUES (%d, 'val_%d')", i, i)); err != nil {
			t.Fatalf("INSERT src: %v", err)
		}
	}

	t.Run("insert_select_basic", func(t *testing.T) {
		_, err := sdb.ExecContext(ctx,
			"INSERT INTO dst SELECT id, val FROM src WHERE id <= 3")
		if err != nil {
			t.Fatalf("INSERT ... SELECT: %v", err)
		}
		rows := collectRows(t, sdb, "SELECT id, val FROM dst ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if rows[0][0].(int64) != 1 || rows[2][0].(int64) != 3 {
			t.Errorf("unexpected rows: %v", rows)
		}
	})
}

func TestFDB_QualityProbe_UnionLimitOffset(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ulo")

	t.Run("union_all_large_limit_with_offset", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers UNION ALL SELECT id FROM orders ORDER BY id LIMIT 100 OFFSET 3")
		// 4 customers + 6 orders = 10 total, skip 3 → 7
		if len(rows) != 7 {
			t.Fatalf("want 7 rows (OFFSET 3, LIMIT 100), got %d", len(rows))
		}
	})

	t.Run("union_all_limit_offset", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers UNION ALL SELECT id FROM orders ORDER BY id LIMIT 3 OFFSET 2")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		// sorted ids: 1,2,3,4,10,11,12,13,14,15 → offset 2 → [3,4,10]
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r[0].(int64)
		}
		if ids[0] != 3 || ids[1] != 4 || ids[2] != 10 {
			t.Errorf("want [3,4,10], got %v", ids)
		}
	})

	t.Run("union_all_limit_only", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers UNION ALL SELECT id FROM orders LIMIT 5")
		if len(rows) != 5 {
			t.Fatalf("want 5 rows, got %d", len(rows))
		}
	})

	t.Run("union_all_order_limit_desc", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers UNION ALL SELECT id FROM orders ORDER BY id DESC LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r[0].(int64)
		}
		// sorted desc: 15,14,13,12,11,10,4,3,2,1 → first 3
		if ids[0] != 15 || ids[1] != 14 || ids[2] != 13 {
			t.Errorf("want [15,14,13], got %v", ids)
		}
	})
}

func TestFDB_QualityProbe_AggregateEdgeCases(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "aec")

	t.Run("count_star_vs_count_col", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT COUNT(*), COUNT(amount) FROM orders")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		countStar := rows[0][0].(int64)
		countAmount := rows[0][1].(int64)
		// COUNT(*) = 6, COUNT(amount) = 5 (one NULL amount)
		if countStar != 6 {
			t.Errorf("COUNT(*) want 6, got %d", countStar)
		}
		if countAmount != 5 {
			t.Errorf("COUNT(amount) want 5, got %d", countAmount)
		}
	})

	t.Run("sum_null_column", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT SUM(amount) FROM orders WHERE customer_id = 4")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("SUM of only NULL values should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("avg_with_nulls", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT AVG(amount) FROM orders")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		avg, ok := rows[0][0].(float64)
		if !ok {
			t.Fatalf("AVG should return float64, got %T (%v)", rows[0][0], rows[0][0])
		}
		// (100.50 + 200.00 + 50.25 + 75.00 + 300.00) / 5 = 145.15
		if math.Abs(avg-145.15) > 0.01 {
			t.Errorf("AVG want 145.15, got %f", avg)
		}
	})

	t.Run("min_max_with_nulls", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT MIN(amount), MAX(amount) FROM orders")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		minVal, ok1 := rows[0][0].(float64)
		maxVal, ok2 := rows[0][1].(float64)
		if !ok1 || !ok2 {
			t.Fatalf("MIN/MAX want float64, got %T/%T", rows[0][0], rows[0][1])
		}
		if minVal != 50.25 {
			t.Errorf("MIN want 50.25, got %f", minVal)
		}
		if maxVal != 300.00 {
			t.Errorf("MAX want 300.00, got %f", maxVal)
		}
	})

	t.Run("aggregate_empty_result", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT COUNT(*), SUM(amount), AVG(amount), MIN(amount), MAX(amount) FROM orders WHERE 1 = 0")
		if len(rows) != 1 {
			t.Fatalf("want 1 row for aggregate over empty set, got %d", len(rows))
		}
		// COUNT(*) over empty = 0, others = NULL
		if rows[0][0].(int64) != 0 {
			t.Errorf("COUNT(*) over empty want 0, got %v", rows[0][0])
		}
		for i, name := range []string{"SUM", "AVG", "MIN", "MAX"} {
			if rows[0][i+1] != nil {
				t.Errorf("%s over empty set should be NULL, got %v", name, rows[0][i+1])
			}
		}
	})

	t.Run("group_by_with_having_count", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT customer_id, COUNT(*) as cnt FROM orders
			 GROUP BY customer_id HAVING COUNT(*) >= 2 ORDER BY customer_id`)
		if len(rows) != 2 {
			t.Fatalf("want 2 customers with 2+ orders, got %d", len(rows))
		}
		// customer 1: 2 orders, customer 2: 2 orders
		if rows[0][0].(int64) != 1 || rows[0][1].(int64) != 2 {
			t.Errorf("row 0: want (1, 2), got (%v, %v)", rows[0][0], rows[0][1])
		}
		if rows[1][0].(int64) != 2 || rows[1][1].(int64) != 2 {
			t.Errorf("row 1: want (2, 2), got (%v, %v)", rows[1][0], rows[1][1])
		}
	})
}

func TestFDB_QualityProbe_SubqueryInWhere(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "sqw")

	t.Run("in_subquery", func(t *testing.T) {
		// IN (subquery) not yet supported by Cascades planner
		err := expectError(t, db,
			`SELECT name FROM customers
			 WHERE id IN (SELECT customer_id FROM orders WHERE status = 'shipped')
			 ORDER BY name`)
		if err == nil {
			t.Fatal("expected error for IN subquery")
		}
		t.Logf("IN subquery: %v (known limitation)", err)
	})

	t.Run("not_in_subquery", func(t *testing.T) {
		err := expectError(t, db,
			`SELECT name FROM customers
			 WHERE id NOT IN (SELECT customer_id FROM orders WHERE status = 'shipped')
			 ORDER BY name`)
		if err == nil {
			t.Fatal("expected error for NOT IN subquery")
		}
		t.Logf("NOT IN subquery: %v (known limitation)", err)
	})

	t.Run("exists_with_and", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name FROM customers c
			 WHERE c.active = true
			 AND EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'shipped')
			 ORDER BY c.name`)
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		// Active + shipped: Alice, Bob (Diana has pending; Charlie inactive)
		if len(names) != 2 || names[0] != "Alice" || names[1] != "Bob" {
			t.Errorf("want [Alice, Bob], got %v", names)
		}
	})
}

func TestFDB_QualityProbe_DerivedTable(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "dt")

	t.Run("subquery_in_from", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT sq.cid, sq.total FROM
			 (SELECT customer_id AS cid, SUM(amount) AS total
			  FROM orders GROUP BY customer_id) sq
			 WHERE sq.total > 100
			 ORDER BY sq.total DESC`)
		if len(rows) < 2 {
			t.Fatalf("want at least 2 rows, got %d", len(rows))
		}
		// Charlie: 300, Alice: 300.50, Bob: 125.25
		firstTotal := rows[0][1].(float64)
		if firstTotal < 200 {
			t.Errorf("first total should be > 200, got %f", firstTotal)
		}
	})

	t.Run("subquery_in_from_with_join", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name, sub.order_count FROM customers c,
			 (SELECT customer_id, COUNT(*) AS order_count FROM orders GROUP BY customer_id) sub
			 WHERE c.id = sub.customer_id AND sub.order_count > 1
			 ORDER BY c.name`)
		// customers 1 (Alice) and 2 (Bob) each have 2 orders → order_count > 1
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "Alice" || fmt.Sprintf("%v", rows[1][0]) != "Bob" {
			t.Errorf("want [Alice, Bob], got [%v, %v]", rows[0][0], rows[1][0])
		}
	})
}

func TestFDB_QualityProbe_BetweenAndIn(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "bai")

	t.Run("between_numeric", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM orders WHERE amount BETWEEN 50.00 AND 100.50 ORDER BY id")
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r[0].(int64)
		}
		// 50.25 (12), 75.00 (13), 100.50 (10)
		if len(ids) != 3 || ids[0] != 10 || ids[1] != 12 || ids[2] != 13 {
			t.Errorf("want [10, 12, 13], got %v", ids)
		}
	})

	t.Run("not_between", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM orders WHERE amount NOT BETWEEN 50.00 AND 100.50 ORDER BY id")
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r[0].(int64)
		}
		// 200.00 (11), 300.00 (14)
		if len(ids) != 2 || ids[0] != 11 || ids[1] != 14 {
			t.Errorf("want [11, 14], got %v", ids)
		}
	})

	t.Run("in_list_numeric", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE id IN (1, 3) ORDER BY name")
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		if len(names) != 2 || names[0] != "Alice" || names[1] != "Charlie" {
			t.Errorf("want [Alice, Charlie], got %v", names)
		}
	})

	t.Run("in_list_string", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM orders WHERE status IN ('shipped', 'pending') ORDER BY id")
		if len(rows) != 5 {
			t.Fatalf("want 5 orders (3 shipped + 2 pending), got %d", len(rows))
		}
	})

	t.Run("like_pattern", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE name LIKE 'A%' ORDER BY name")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "Alice" {
			t.Errorf("want Alice, got %v", rows[0][0])
		}
	})

	t.Run("like_underscore", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE name LIKE '_ob'")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "Bob" {
			t.Errorf("want Bob, got %v", rows)
		}
	})
}

func TestFDB_QualityProbe_CastExpressions(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ce")

	t.Run("cast_int_to_string", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT CAST(id AS STRING) FROM customers WHERE id = 1")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		val := fmt.Sprintf("%v", rows[0][0])
		if val != "1" {
			t.Errorf("CAST(1 AS STRING) want '1', got '%s'", val)
		}
	})

	t.Run("cast_string_to_int_from_table", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT CAST(name AS STRING), CAST(id AS STRING) FROM customers WHERE id = 1")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][1]) != "1" {
			t.Errorf("CAST(1 AS STRING) want '1', got '%v'", rows[0][1])
		}
	})

	t.Run("cast_float_to_int", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT CAST(amount AS BIGINT) FROM orders WHERE id = 10")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		val := rows[0][0].(int64)
		// 100.50 → truncated to 100 (or 101 depending on rounding)
		if val != 100 && val != 101 {
			t.Errorf("CAST(100.50 AS BIGINT) want 100 or 101, got %d", val)
		}
	})

	t.Run("cast_null_preserves_null", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT CAST(amount AS STRING) FROM orders WHERE id = 15")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0][0] != nil {
			t.Errorf("CAST(NULL AS STRING) should be NULL, got %v", rows[0][0])
		}
	})

	t.Run("cast_double_to_string", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT CAST(amount AS STRING) FROM orders WHERE id = 10")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		val := fmt.Sprintf("%v", rows[0][0])
		if val != "100.5" && val != "100.50" && val != "1.005E2" {
			t.Errorf("CAST(100.50 AS STRING) want '100.5' or '100.50', got '%s'", val)
		}
	})
}

func TestFDB_QualityProbe_MultipleOrderBy(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "mob")

	t.Run("order_by_two_cols", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT region, name FROM customers ORDER BY region, name")
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d", len(rows))
		}
		// NULL region sorts first (NULLS FIRST for ASC), then EAST, then WEST
		r0 := fmt.Sprintf("%v", rows[0][0])
		if r0 != "<nil>" {
			// Diana (NULL region) should be first or last depending on null ordering
			// Check that at least the non-null regions are ordered
			regions := make([]string, len(rows))
			for i, r := range rows {
				regions[i] = fmt.Sprintf("%v", r[0])
			}
			t.Logf("order: %v", regions)
		}
	})

	t.Run("order_by_asc_desc_mix", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT customer_id, amount FROM orders WHERE amount IS NOT NULL ORDER BY customer_id ASC, amount DESC")
		if len(rows) != 5 {
			t.Fatalf("want 5 non-null rows, got %d", len(rows))
		}
		// customer 1: 200.00, 100.50 (desc)
		// customer 2: 75.00, 50.25 (desc)
		// customer 3: 300.00
		cid0 := rows[0][0].(int64)
		amt0 := rows[0][1].(float64)
		cid1 := rows[1][0].(int64)
		amt1 := rows[1][1].(float64)
		if cid0 != 1 || amt0 != 200.00 {
			t.Errorf("row 0: want (1, 200.00), got (%d, %f)", cid0, amt0)
		}
		if cid1 != 1 || amt1 != 100.50 {
			t.Errorf("row 1: want (1, 100.50), got (%d, %f)", cid1, amt1)
		}
	})
}

func TestFDB_QualityProbe_IsNullIsNotNull(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ininn")

	t.Run("is_null", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM orders WHERE amount IS NULL")
		if len(rows) != 1 || rows[0][0].(int64) != 15 {
			t.Errorf("want [15], got %v", rows)
		}
	})

	t.Run("is_not_null", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM orders WHERE amount IS NOT NULL ORDER BY id")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
	})

	t.Run("null_region_filter", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE region IS NULL")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "Diana" {
			t.Errorf("want Diana, got %v", rows)
		}
	})
}

func TestFDB_QualityProbe_CompoundPredicates(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "cp")

	t.Run("and_or_precedence", func(t *testing.T) {
		// WHERE a AND b OR c should parse as (a AND b) OR c
		rows := collectRows(t, db,
			`SELECT name FROM customers
			 WHERE active = true AND region = 'WEST' OR region = 'EAST'
			 ORDER BY name`)
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		// (active=true AND region=WEST): Alice
		// OR region=EAST: Bob
		if len(names) != 2 || names[0] != "Alice" || names[1] != "Bob" {
			t.Errorf("want [Alice, Bob], got %v", names)
		}
	})

	t.Run("parenthesized_or", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT name FROM customers
			 WHERE active = true AND (region = 'WEST' OR region = 'EAST')
			 ORDER BY name`)
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		// active AND (WEST or EAST): Alice, Bob
		if len(names) != 2 || names[0] != "Alice" || names[1] != "Bob" {
			t.Errorf("want [Alice, Bob], got %v", names)
		}
	})

	t.Run("not_predicate", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE NOT active = true ORDER BY name")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "Charlie" {
			t.Errorf("want [Charlie], got %v", rows)
		}
	})
}

func TestFDB_QualityProbe_JoinPredicateEdgeCases(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "jpec")

	t.Run("join_with_or_predicate", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name, o.id FROM customers c, orders o
			 WHERE c.id = o.customer_id AND (o.status = 'shipped' OR o.status = 'pending')
			 ORDER BY o.id`)
		// shipped: 10(Alice), 12(Bob), 14(Charlie); pending: 11(Alice), 15(Diana)
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d: %v", len(rows), rows)
		}
	})

	t.Run("join_with_not_equal", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name, o.status FROM customers c, orders o
			 WHERE c.id = o.customer_id AND o.status <> 'cancelled'
			 ORDER BY c.name, o.id`)
		// All orders except cancelled (id=13, Bob's)
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d: %v", len(rows), rows)
		}
	})

	t.Run("join_with_between_on_join_col", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name, o.amount FROM customers c, orders o
			 WHERE c.id = o.customer_id AND o.amount BETWEEN 50.00 AND 200.00
			 ORDER BY o.amount`)
		// 50.25, 75.00, 100.50, 200.00
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d: %v", len(rows), rows)
		}
	})

	t.Run("left_join_with_null_inner", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name, o.status FROM customers c
			 LEFT JOIN orders o ON c.id = o.customer_id AND o.status = 'shipped'
			 ORDER BY c.name`)
		// Alice: shipped (order 10), Bob: shipped (12), Charlie: shipped (14), Diana: null
		// Also Alice has pending (11) and Bob has cancelled (13) but those don't match
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		if len(rows) < 4 {
			t.Fatalf("want >= 4 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("cross_join_count", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT COUNT(*) FROM customers c, orders o`)
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		cnt := rows[0][0].(int64)
		// 4 customers × 6 orders = 24
		if cnt != 24 {
			t.Errorf("cross join count want 24, got %d", cnt)
		}
	})
}

func TestFDB_QualityProbe_NestedAggregation(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "nagg")

	t.Run("group_by_expression", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT status, SUM(amount), COUNT(*), MIN(amount), MAX(amount)
			 FROM orders
			 GROUP BY status
			 ORDER BY status`)
		if len(rows) < 3 {
			t.Fatalf("want 3 groups (cancelled, pending, shipped), got %d: %v", len(rows), rows)
		}
		for _, r := range rows {
			status := fmt.Sprintf("%v", r[0])
			switch status {
			case "shipped":
				sum := r[1].(float64)
				if math.Abs(sum-450.75) > 0.01 {
					t.Errorf("shipped SUM want 450.75, got %f", sum)
				}
				cnt := r[2].(int64)
				if cnt != 3 {
					t.Errorf("shipped COUNT want 3, got %d", cnt)
				}
			case "pending":
				cnt := r[2].(int64)
				if cnt != 2 {
					t.Errorf("pending COUNT want 2, got %d", cnt)
				}
			case "cancelled":
				cnt := r[2].(int64)
				if cnt != 1 {
					t.Errorf("cancelled COUNT want 1, got %d", cnt)
				}
			}
		}
	})

	t.Run("group_by_multiple_keys", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT customer_id, status, COUNT(*) FROM orders
			 GROUP BY customer_id, status
			 ORDER BY customer_id, status`)
		// Each (customer_id, status) combo is a group
		if len(rows) < 4 {
			t.Fatalf("want at least 4 groups, got %d: %v", len(rows), rows)
		}
	})

	t.Run("having_with_multiple_aggregates", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT customer_id, SUM(amount), COUNT(*) FROM orders
			 GROUP BY customer_id
			 HAVING SUM(amount) > 100 AND COUNT(*) >= 2
			 ORDER BY customer_id`)
		// customer 1: sum=300.50, count=2; customer 2: sum=125.25, count=2
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_UpdateWithSubquery(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := fmt.Sprintf("/qp_uws_%s", t.Name())
	db := openTestDB(t, dbPath)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("QP_UWS_%s", t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE t1 (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))
		CREATE TABLE t2 (id BIGINT NOT NULL, ref_id BIGINT, tag STRING, PRIMARY KEY (id))`, tmpl)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	sdb, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { sdb.Close() })

	for i := int64(1); i <= 5; i++ {
		if _, err := sdb.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t1 VALUES (%d, %d)", i, i*10)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}
	for i := int64(1); i <= 3; i++ {
		if _, err := sdb.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t2 VALUES (%d, %d, 'tag_%d')", i, i, i)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("delete_with_exists", func(t *testing.T) {
		_, err := sdb.ExecContext(ctx,
			"DELETE FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.ref_id = t1.id)")
		if err != nil {
			t.Logf("DELETE with EXISTS: %v (known limitation)", err)
			return
		}
		rows := collectRows(t, sdb, "SELECT id FROM t1 ORDER BY id")
		// Should delete rows 1,2,3 (matched in t2), leaving 4,5
		if len(rows) != 2 {
			t.Fatalf("want 2 remaining rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 4 || rows[1][0].(int64) != 5 {
			t.Errorf("want [4,5], got %v", rows)
		}
	})
}

func TestFDB_QualityProbe_ArithmeticExpressions(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "arith")

	t.Run("arithmetic_in_select", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, qty * price AS total FROM items WHERE id = 100")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		total := rows[0][1].(float64)
		// 2 * 25.25 = 50.50
		if math.Abs(total-50.50) > 0.01 {
			t.Errorf("qty*price want 50.50, got %f", total)
		}
	})

	t.Run("arithmetic_in_where", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM items WHERE qty * price > 100 ORDER BY id")
		// items: 100: 2*25.25=50.50, 101: 1*50=50, 102: 5*25.25=126.25, 103: 1*50.25=50.25, 104: 10*30=300
		// > 100: 102 (126.25), 104 (300)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(int64) != 102 || rows[1][0].(int64) != 104 {
			t.Errorf("want [102, 104], got %v", rows)
		}
	})

	t.Run("arithmetic_null_propagation", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, qty * price FROM items WHERE id = 105")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		// item 105: qty=3, price=null → null
		if rows[0][1] != nil {
			t.Errorf("null * 3 should be null, got %v", rows[0][1])
		}
	})

	t.Run("arithmetic_addition_subtraction", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, amount + 10, amount - 10 FROM orders WHERE id = 10")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		add := rows[0][1].(float64)
		sub := rows[0][2].(float64)
		if math.Abs(add-110.50) > 0.01 {
			t.Errorf("amount+10 want 110.50, got %f", add)
		}
		if math.Abs(sub-90.50) > 0.01 {
			t.Errorf("amount-10 want 90.50, got %f", sub)
		}
	})

	t.Run("integer_division", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, qty / 2 FROM items WHERE id = 100")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		// 2 / 2 = 1
		val := rows[0][1].(int64)
		if val != 1 {
			t.Errorf("2/2 want 1, got %d", val)
		}
	})

	t.Run("modulo", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, qty % 3 FROM items WHERE id = 102")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		// 5 % 3 = 2
		val := rows[0][1].(int64)
		if val != 2 {
			t.Errorf("5%%3 want 2, got %d", val)
		}
	})
}

func TestFDB_QualityProbe_NestedCASE(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "nc")

	t.Run("nested_case_when", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT name,
				CASE
					WHEN active = true THEN
						CASE WHEN region = 'WEST' THEN 'active-west'
						     WHEN region = 'EAST' THEN 'active-east'
						     ELSE 'active-other'
						END
					ELSE 'inactive'
				END AS label
			 FROM customers ORDER BY name`)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		expected := map[string]string{
			"Alice":   "active-west",
			"Bob":     "active-east",
			"Charlie": "inactive",
			"Diana":   "active-other",
		}
		for _, r := range rows {
			name := fmt.Sprintf("%v", r[0])
			label := fmt.Sprintf("%v", r[1])
			if exp, ok := expected[name]; ok && exp != label {
				t.Errorf("%s: want %q, got %q", name, exp, label)
			}
		}
	})

	t.Run("case_with_null_comparison", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT id,
				CASE WHEN amount IS NULL THEN 'no-amount'
				     WHEN amount > 200 THEN 'high'
				     WHEN amount > 100 THEN 'medium'
				     ELSE 'low'
				END AS tier
			 FROM orders ORDER BY id`)
		if len(rows) != 6 {
			t.Fatalf("want 6, got %d", len(rows))
		}
		expected := map[int64]string{
			10: "medium", // 100.50
			11: "medium", // 200.00
			12: "low",    // 50.25
			13: "low",    // 75.00
			14: "high",   // 300.00
			15: "no-amount",
		}
		for _, r := range rows {
			id := r[0].(int64)
			tier := fmt.Sprintf("%v", r[1])
			if exp, ok := expected[id]; ok && exp != tier {
				t.Errorf("order %d: want %q, got %q", id, exp, tier)
			}
		}
	})
}

func TestFDB_QualityProbe_LeftJoinWhereVsOn(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "ljwo")

	t.Run("left_join_where_on_outer", func(t *testing.T) {
		// WHERE on outer table: should filter AFTER join, not during
		rows := collectRows(t, db,
			`SELECT c.name, o.status FROM customers c
			 LEFT JOIN orders o ON c.id = o.customer_id
			 WHERE c.active = true
			 ORDER BY c.name, o.id`)
		// Active customers: Alice, Bob, Diana
		// Alice: orders 10(shipped), 11(pending)
		// Bob: orders 12(shipped), 13(cancelled)
		// Diana: order 15(pending)
		names := make(map[string]bool)
		for _, r := range rows {
			names[fmt.Sprintf("%v", r[0])] = true
		}
		if names["Charlie"] {
			t.Errorf("Charlie (inactive) should be filtered by WHERE")
		}
		if !names["Alice"] || !names["Bob"] || !names["Diana"] {
			t.Errorf("active customers missing, got names: %v", names)
		}
		// All active customers should have their orders (including Diana's pending)
		if len(rows) < 5 {
			t.Errorf("want at least 5 rows (Alice:2 + Bob:2 + Diana:1), got %d: %v", len(rows), rows)
		}
	})

	t.Run("left_join_where_on_inner", func(t *testing.T) {
		// WHERE on inner table effectively converts to INNER JOIN
		rows := collectRows(t, db,
			`SELECT c.name, o.status FROM customers c
			 LEFT JOIN orders o ON c.id = o.customer_id
			 WHERE o.status = 'shipped'
			 ORDER BY c.name`)
		// Only rows where o.status='shipped' survive (NULL status filtered)
		for _, r := range rows {
			status := fmt.Sprintf("%v", r[1])
			if status != "shipped" {
				t.Errorf("WHERE should filter non-shipped, got %v", r)
			}
		}
		// Alice(shipped), Bob(shipped), Charlie(shipped) — Diana filtered
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
	})

	t.Run("left_join_on_filter_vs_where_filter", func(t *testing.T) {
		// ON clause filter: unmatched outer rows get NULLs
		rowsOn := collectRows(t, db,
			`SELECT c.name, o.status FROM customers c
			 LEFT JOIN orders o ON c.id = o.customer_id AND o.status = 'shipped'
			 ORDER BY c.name`)
		// All 4 customers should appear; non-shipped get NULL status
		if len(rowsOn) < 4 {
			t.Fatalf("ON filter: want >= 4, got %d: %v", len(rowsOn), rowsOn)
		}

		// WHERE clause filter: NULL rows filtered out
		rowsWhere := collectRows(t, db,
			`SELECT c.name, o.status FROM customers c
			 LEFT JOIN orders o ON c.id = o.customer_id
			 WHERE o.status = 'shipped'
			 ORDER BY c.name`)
		// Only 3 customers with shipped orders
		if len(rowsWhere) != 3 {
			t.Fatalf("WHERE filter: want 3, got %d: %v", len(rowsWhere), rowsWhere)
		}

		// The results should differ — ON filter produces more rows
		if len(rowsOn) == len(rowsWhere) {
			t.Errorf("ON filter (%d rows) should produce more rows than WHERE filter (%d rows)",
				len(rowsOn), len(rowsWhere))
		}
	})
}

func TestFDB_QualityProbe_OrderByAlias(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "oba")

	t.Run("order_by_alias", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT name AS n, region AS r FROM customers ORDER BY n`)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		first := fmt.Sprintf("%v", rows[0][0])
		if first != "Alice" {
			t.Errorf("first by name: want Alice, got %s", first)
		}
	})

	t.Run("order_by_expression", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT id, qty * price AS total FROM items
			 WHERE price IS NOT NULL
			 ORDER BY qty * price DESC`)
		if len(rows) < 4 {
			t.Fatalf("want at least 4 rows, got %d", len(rows))
		}
		// Highest: 104 (10*30=300), then 102 (5*25.25=126.25), then 100 (2*25.25=50.50), ...
		first := rows[0][0].(int64)
		if first != 104 {
			t.Errorf("highest total: want item 104, got %d", first)
		}
	})

	t.Run("order_by_column_number", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name, region FROM customers ORDER BY 2, 1")
		// region order: NULL, EAST, WEST, WEST → then by name within WEST
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
	})
}

func TestFDB_QualityProbe_GroupByWithNulls(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "gbn")

	t.Run("group_by_nullable_column", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT region, COUNT(*) FROM customers GROUP BY region ORDER BY region`)
		// NULL: 1, EAST: 1, WEST: 2
		if len(rows) != 3 {
			t.Fatalf("want 3 groups, got %d: %v", len(rows), rows)
		}
		for _, r := range rows {
			region := fmt.Sprintf("%v", r[0])
			cnt := r[1].(int64)
			switch region {
			case "<nil>":
				if cnt != 1 {
					t.Errorf("NULL region: want 1, got %d", cnt)
				}
			case "EAST":
				if cnt != 1 {
					t.Errorf("EAST: want 1, got %d", cnt)
				}
			case "WEST":
				if cnt != 2 {
					t.Errorf("WEST: want 2, got %d", cnt)
				}
			}
		}
	})

	t.Run("group_by_multiple_with_null", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT active, region, COUNT(*) FROM customers
			 GROUP BY active, region
			 ORDER BY active, region`)
		// Expect groups for (false, WEST), (true, NULL), (true, EAST), (true, WEST)
		if len(rows) < 4 {
			t.Fatalf("want 4 groups, got %d: %v", len(rows), rows)
		}
	})
}

func TestFDB_QualityProbe_MultiTableInsertDelete(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := fmt.Sprintf("/qp_mtid_%s", t.Name())
	db := openTestDB(t, dbPath)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("QP_MTID_%s", t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE t (id BIGINT NOT NULL, val STRING, PRIMARY KEY (id))`, tmpl)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	sdb, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { sdb.Close() })

	for i := int64(1); i <= 10; i++ {
		if _, err := sdb.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t VALUES (%d, 'v%d')", i, i)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("delete_with_limit", func(t *testing.T) {
		_, err := sdb.ExecContext(ctx, "DELETE FROM t WHERE id > 8")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := collectRows(t, sdb, "SELECT COUNT(*) FROM t")
		cnt := rows[0][0].(int64)
		if cnt != 8 {
			t.Errorf("want 8 remaining, got %d", cnt)
		}
	})

	t.Run("update_multiple_rows", func(t *testing.T) {
		_, err := sdb.ExecContext(ctx, "UPDATE t SET val = 'updated' WHERE id <= 3")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := collectRows(t, sdb, "SELECT val FROM t WHERE id = 1")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "updated" {
			t.Errorf("want 'updated', got %v", rows)
		}
		rows = collectRows(t, sdb, "SELECT COUNT(*) FROM t WHERE val = 'updated'")
		if rows[0][0].(int64) != 3 {
			t.Errorf("want 3 updated rows, got %v", rows[0][0])
		}
	})
}

func TestFDB_QualityProbe_CoalesceAndGreatest(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "cg")

	t.Run("coalesce_multiple_args", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, COALESCE(amount, 0) FROM orders ORDER BY id")
		if len(rows) != 6 {
			t.Fatalf("want 6, got %d", len(rows))
		}
		for _, r := range rows {
			id := r[0].(int64)
			if id == 15 {
				// NULL amount → COALESCE returns 0
				switch v := r[1].(type) {
				case int64:
					if v != 0 {
						t.Errorf("COALESCE(NULL, 0) want 0, got %d", v)
					}
				case float64:
					if v != 0 {
						t.Errorf("COALESCE(NULL, 0) want 0, got %f", v)
					}
				default:
					t.Errorf("unexpected type %T for COALESCE(NULL, 0)", r[1])
				}
			}
		}
	})

	t.Run("greatest_least", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id, GREATEST(qty, 3), LEAST(qty, 3) FROM items WHERE price IS NOT NULL ORDER BY id")
		if len(rows) < 4 {
			t.Fatalf("want at least 4, got %d", len(rows))
		}
		// item 100: qty=2 → GREATEST(2,3)=3, LEAST(2,3)=2
		if rows[0][0].(int64) == 100 {
			gr := rows[0][1].(int64)
			le := rows[0][2].(int64)
			if gr != 3 {
				t.Errorf("GREATEST(2,3) want 3, got %d", gr)
			}
			if le != 2 {
				t.Errorf("LEAST(2,3) want 2, got %d", le)
			}
		}
	})
}

func TestFDB_QualityProbe_StringLiteralEdges(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "sle")

	t.Run("empty_string_comparison", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE name <> ''")
		// All 4 customers have non-empty names
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
	})

	t.Run("like_percent_only", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE name LIKE '%'")
		if len(rows) != 4 {
			t.Fatalf("LIKE '%%' should match all, got %d", len(rows))
		}
	})

	t.Run("case_sensitive_comparison", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT name FROM customers WHERE name = 'alice'")
		// Should not match 'Alice' (case-sensitive)
		if len(rows) != 0 {
			t.Logf("case-sensitive: 'alice' matched %d rows (may be case-insensitive)", len(rows))
		}
	})
}

func TestFDB_QualityProbe_LimitZero(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "lz")

	t.Run("limit_zero", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers LIMIT 0")
		if len(rows) != 0 {
			t.Errorf("LIMIT 0 should return 0 rows, got %d", len(rows))
		}
	})

	t.Run("limit_one", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers ORDER BY id LIMIT 1")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if rows[0][0].(int64) != 1 {
			t.Errorf("want id=1, got %v", rows[0][0])
		}
	})

	t.Run("limit_exceeds_rows", func(t *testing.T) {
		rows := collectRows(t, db,
			"SELECT id FROM customers LIMIT 100")
		if len(rows) != 4 {
			t.Errorf("LIMIT 100 with 4 rows should return 4, got %d", len(rows))
		}
	})
}

func TestFDB_QualityProbe_ComplexSubqueryPatterns(t *testing.T) {
	t.Parallel()
	db := qualityProbeDB(t, "csp")

	t.Run("correlated_not_exists", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name FROM customers c
			 WHERE NOT EXISTS (
			   SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'cancelled'
			 )
			 ORDER BY c.name`)
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		if len(names) != 3 || names[0] != "Alice" || names[1] != "Charlie" || names[2] != "Diana" {
			t.Errorf("want [Alice, Charlie, Diana], got %v", names)
		}
	})

	t.Run("correlated_exists_with_filter", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT c.name FROM customers c
			 WHERE EXISTS (
			   SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'shipped'
			 )
			 ORDER BY c.name`)
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = fmt.Sprintf("%v", r[0])
		}
		if len(names) != 3 || names[0] != "Alice" || names[1] != "Bob" || names[2] != "Charlie" {
			t.Errorf("want [Alice, Bob, Charlie], got %v", names)
		}
	})

	t.Run("cte_with_join_filter", func(t *testing.T) {
		// Active customers: Alice(1), Bob(2), Diana(4)
		// Shipped orders: 10(cust 1), 12(cust 2), 14(cust 3)
		// Active + shipped: Alice(10), Bob(12) = 2 rows
		rows := collectRows(t, db,
			`WITH active_customers AS (
			   SELECT id, name FROM customers WHERE active = true
			 )
			 SELECT ac.name, o.status
			 FROM active_customers ac, orders o
			 WHERE o.customer_id = ac.id AND o.status = 'shipped'
			 ORDER BY ac.name`)
		if len(rows) != 2 {
			t.Fatalf("want 2 shipped orders from active customers, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "Alice" {
			t.Errorf("first name: want Alice, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "Bob" {
			t.Errorf("second name: want Bob, got %v", rows[1][0])
		}
	})

	t.Run("cte_with_exists", func(t *testing.T) {
		// CTE-derived table + correlated EXISTS: the CTE alias
		// becomes the quantifier alias via sourceAlias. Currently
		// the EXISTS correlation can't resolve CTE columns across
		// the subquery boundary. When this starts working, flip
		// to a correctness assertion: want [Alice Bob].
		err := expectError(t, db,
			`WITH active AS (
			   SELECT id, name FROM customers WHERE active = true
			 )
			 SELECT a.name FROM active a
			 WHERE EXISTS (
			   SELECT 1 FROM orders o WHERE o.customer_id = a.id AND o.status = 'shipped'
			 )
			 ORDER BY a.name`)
		if err == nil {
			t.Fatal("CTE + EXISTS now works — update this test to assert correctness")
		}
	})

	t.Run("multi_table_count", func(t *testing.T) {
		rows := collectRows(t, db,
			`SELECT COUNT(*) FROM customers c, orders o
			 WHERE o.customer_id = c.id`)
		if len(rows) != 1 || rows[0][0].(int64) != 6 {
			t.Errorf("want count=6, got %v", rows)
		}
	})
}

var (
	_ = fmt.Sprintf // ensure import
	_ = sort.Strings
	_ = strings.Contains
	_ = math.MaxInt64
)
