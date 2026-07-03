package sqldriver_test

import (
	"context"
	"database/sql"
	"testing"
)

// TestFDB_RFC173W4b_ComputedScalarWithLimit pins the shape-3 × user-LIMIT
// dimension (impl-review condition): the materializing Project (positional
// `_0`) wraps ABOVE the builder's Sort/Limit, so the translator's limit-peel
// must see THROUGH it — a LIMIT left in-position inside a correlated inner
// risks being applied once globally instead of per outer row (one row gets a
// value, every other outer row gets NULL — silent wrong rows).
func TestFDB_RFC173W4b_ComputedScalarWithLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_cl"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_cl_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, PRIMARY KEY (id))"); err != nil {
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
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice'), (2, 'bob')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	// alice: amounts 100, 300; bob: amount 50.
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1,100),(2,1,300),(3,2,50)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// COMPUTED scalar + ORDER BY + LIMIT 1, evaluated PER OUTER ROW: alice's
	// max amount + 1 = 301, bob's = 51. A globally-applied LIMIT would leave
	// one of the outer rows NULL.
	rows, err := db.QueryContext(ctx,
		"SELECT c.name, (SELECT o.amount + 1 FROM orders o WHERE o.customer_id = c.id "+
			"ORDER BY o.amount DESC LIMIT 1) FROM customers c ORDER BY c.id")
	if err != nil {
		t.Fatalf("computed scalar with LIMIT: %v", err)
	}
	defer rows.Close()
	want := map[string]int64{"alice": 301, "bob": 51}
	n := 0
	for rows.Next() {
		var name string
		var v sql.NullInt64
		if err := rows.Scan(&name, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !v.Valid || v.Int64 != want[name] {
			t.Errorf("computed+LIMIT for %s = %d (valid=%v), want %d (per-outer-row LIMIT)",
				name, v.Int64, v.Valid, want[name])
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 2 {
		t.Errorf("got %d rows, want 2", n)
	}
}
