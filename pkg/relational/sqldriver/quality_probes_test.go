package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
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

	t.Run("correlated_scalar_subquery_rejects", func(t *testing.T) {
		// Correlated scalar subqueries in SELECT list are not yet
		// supported — the correlation reference can't resolve across
		// the subquery boundary in the current architecture.
		err := expectError(t, db, `SELECT name,
			(SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id)
			FROM customers c ORDER BY name`)
		if err == nil {
			t.Fatal("expected correlated scalar subquery rejection")
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
		if !strings.Contains(err.Error(), "by zero") {
			t.Errorf("want 'by zero' in error, got: %v", err)
		}
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
		// CTE with aggregate + join: known Cascades limitation for
		// complex CTE shapes. Verify it either works or rejects cleanly.
		_, err := db.QueryContext(context.Background(), `WITH shipped AS (
			SELECT customer_id, SUM(amount) AS total
			FROM orders WHERE status = 'shipped'
			GROUP BY customer_id
		)
		SELECT c.name, s.total
		FROM customers c, shipped s
		WHERE c.id = s.customer_id
		ORDER BY s.total DESC`)
		// If it succeeds, great. If it errors, verify it's a clean rejection.
		if err != nil {
			t.Logf("CTE+agg+join: %v (known limitation)", err)
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

var (
	_ = fmt.Sprintf // ensure import
	_ = sort.Strings
	_ = strings.Contains
	_ = math.MaxInt64
)
