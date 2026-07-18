package sqldriver_test

import (
	"context"
	"database/sql"
	"testing"
)

// TestFDB_CorrelatedScalarJoinInnerColumnAgg pins the correlated scalar
// subquery whose join-inner aggregates a COLUMN ARG (SUM(i.qty)) — plus
// the inner-GROUP-BY and comma-join variants. All three route through
// the ordinal gather, NOT the retired flat-name arms (the correlated
// join-inner resolvers now fail loudly instead of minting a lazy
// merged-row-display read; these shapes prove the structural path
// answers, so the retirement removed a dead channel, not a feature).
func TestFDB_CorrelatedScalarJoinInnerColumnAgg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/wsn3_hj"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "wsn3_hj_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, customer_id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE items (id BIGINT, order_id BIGINT, qty BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice'), (2, 'bob'), (3, 'carol')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	// alice: orders 1,2 → items qty 5+7+9=21; bob: order 3 → qty 4; carol: none.
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1),(2,1),(3,2)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO items VALUES (10,1,5),(11,1,7),(12,2,9),(13,3,4)"); err != nil {
		t.Fatalf("seed items: %v", err)
	}

	check := func(label, query string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer rows.Close()
		want := map[string]sql.NullInt64{
			"alice": {Int64: 21, Valid: true},
			"bob":   {Int64: 4, Valid: true},
			"carol": {Valid: false},
		}
		n := 0
		for rows.Next() {
			var name string
			var got sql.NullInt64
			if err := rows.Scan(&name, &got); err != nil {
				t.Fatalf("%s scan: %v", label, err)
			}
			if got != want[name] {
				t.Errorf("%s: %s = %+v, want %+v", label, name, got, want[name])
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s rows: %v", label, err)
		}
		if n != 3 {
			t.Errorf("%s: got %d rows, want 3", label, n)
		}
	}

	check("column-arg aggregate join-inner",
		"SELECT c.name, (SELECT SUM(i.qty) FROM orders o JOIN items i ON i.order_id = o.id "+
			"WHERE o.customer_id = c.id) FROM customers c ORDER BY c.id")
	check("inner GROUP BY join-inner",
		"SELECT c.name, (SELECT SUM(i.qty) FROM orders o JOIN items i ON i.order_id = o.id "+
			"WHERE o.customer_id = c.id GROUP BY o.customer_id) FROM customers c ORDER BY c.id")
	check("comma-join inner",
		"SELECT c.name, (SELECT SUM(i.qty) FROM orders o, items i "+
			"WHERE i.order_id = o.id AND o.customer_id = c.id) FROM customers c ORDER BY c.id")
	// BARE references over the join-inner: qty lives only on items,
	// customer_id only on orders — unique bare names across the merged
	// row, resolved through the same scope channel as qualified refs.
	check("bare column-arg aggregate join-inner",
		"SELECT c.name, (SELECT SUM(qty) FROM orders o JOIN items i ON i.order_id = o.id "+
			"WHERE o.customer_id = c.id) FROM customers c ORDER BY c.id")
	check("bare inner GROUP BY key join-inner",
		"SELECT c.name, (SELECT SUM(i.qty) FROM orders o JOIN items i ON i.order_id = o.id "+
			"WHERE o.customer_id = c.id GROUP BY customer_id) FROM customers c ORDER BY c.id")
}
