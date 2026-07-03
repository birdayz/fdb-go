package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestFDB_RFC173W4b_JoinInnerScalar pins RFC-173 W4b shape 2: a correlated
// scalar subquery whose INNER contains a JOIN. The inner's first table shares
// the SQL alias the seed's inner leg is keyed by, which collided the typed
// QOV(InnerAlias, N-field) of the inner's own ordinal join with the seed's
// typed 1-field inner leg (the executor's DIVERGENT-baked-types tripwire) —
// the reason join-inners were gated to the name model. The fresh-unique inner
// correlation id dissolves the collision, so join-inners ordinalize: same
// rows, no tripwire, and the seed survives the S4 name-model deletion.
func TestFDB_RFC173W4b_JoinInnerScalar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_ji"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_ji_tmpl"
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
	// alice: orders 1,2 with 3 items total; bob: order 3 with exactly 1 item;
	// carol: no orders.
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1),(2,1),(3,2)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO items VALUES (10,1,5),(11,1,7),(12,2,9),(13,3,4)"); err != nil {
		t.Fatalf("seed items: %v", err)
	}

	// (a) AGGREGATE join-inner: per-customer item count across the join.
	rows, err := db.QueryContext(ctx,
		"SELECT c.name, (SELECT COUNT(*) FROM orders o JOIN items i ON i.order_id = o.id "+
			"WHERE o.customer_id = c.id) FROM customers c ORDER BY c.id")
	if err != nil {
		t.Fatalf("aggregate join-inner: %v", err)
	}
	defer rows.Close()
	want := map[string]int64{"alice": 3, "bob": 1, "carol": 0}
	got := 0
	for rows.Next() {
		var name string
		var n sql.NullInt64
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !n.Valid || n.Int64 != want[name] {
			t.Errorf("COUNT for %s = %d (valid=%v), want %d", name, n.Int64, n.Valid, want[name])
		}
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got != 3 {
		t.Errorf("got %d rows, want 3", got)
	}

	// (b) PLAIN-COLUMN join-inner (exactly one matching joined row for bob).
	var qty sql.NullInt64
	const qPlain = "SELECT (SELECT i.qty FROM orders o JOIN items i ON i.order_id = o.id " +
		"WHERE o.customer_id = c.id) FROM customers c WHERE c.id = 2"
	if err := db.QueryRowContext(ctx, qPlain).Scan(&qty); err != nil {
		t.Fatalf("plain-column join-inner: %v\n  sql: %s", err, qPlain)
	}
	if !qty.Valid || qty.Int64 != 4 {
		t.Errorf("plain join-inner scalar = %d (valid=%v), want 4", qty.Int64, qty.Valid)
	}

	// (c) STRICT-SINGLE violation through the join-inner: alice's subquery
	// matches 3 joined rows — the SQL scalar contract must fail with 21000,
	// not silently truncate.
	const qMulti = "SELECT (SELECT i.qty FROM orders o JOIN items i ON i.order_id = o.id " +
		"WHERE o.customer_id = c.id) FROM customers c WHERE c.id = 1"
	var x sql.NullInt64
	if err := db.QueryRowContext(ctx, qMulti).Scan(&x); err == nil {
		t.Errorf("multi-row join-inner scalar must fail the cardinality contract, got %d (valid=%v)", x.Int64, x.Valid)
	} else if !strings.Contains(err.Error(), "21000") {
		t.Errorf("multi-row join-inner scalar error = %v, want SQLSTATE 21000", err)
	}

	// (d) NULL fill for an empty join-inner (carol) is covered by (a)'s
	// COUNT=0; the plain-column empty case must yield NULL through the
	// LEFT-OUTER null leg.
	var none sql.NullInt64
	const qEmpty = "SELECT (SELECT i.qty FROM orders o JOIN items i ON i.order_id = o.id " +
		"WHERE o.customer_id = c.id) FROM customers c WHERE c.id = 3"
	if err := db.QueryRowContext(ctx, qEmpty).Scan(&none); err != nil {
		t.Fatalf("empty join-inner: %v", err)
	}
	if none.Valid {
		t.Errorf("empty join-inner scalar = %d, want NULL (LEFT-OUTER null fill)", none.Int64)
	}
}
