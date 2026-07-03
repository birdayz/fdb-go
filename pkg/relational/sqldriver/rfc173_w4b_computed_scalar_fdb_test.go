package sqldriver_test

import (
	"context"
	"database/sql"
	"testing"
)

// TestFDB_RFC173W4b_ComputedScalar pins RFC-173 W4b shape 3: a COMPUTED
// correlated scalar subquery in a projection returns the computed value, not a
// silent NULL. Before the fix the computed expression was applied above the
// inner and never landed in the inner row, so the name-model scalar reference
// resolved to nothing (the query returned NULL — a silent wrong result the
// existing ComputedGate unit test never caught, since it only pinned the gate
// decision, not end-to-end rows). The fix materializes the computation into the
// inner subquery's projected output (positional `_0`), so both the name-model
// and ordinal paths resolve it.
func TestFDB_RFC173W4b_ComputedScalar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_cs"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_cs_tmpl"
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
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1,100),(2,1,50),(3,2,7)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// (a) Computed over an INNER column (self-correlated on PK → one inner row).
	var got sql.NullString
	const qUpper = "SELECT (SELECT UPPER(c2.name) FROM customers c2 WHERE c2.id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, qUpper).Scan(&got); err != nil {
		t.Fatalf("UPPER computed scalar: %v\n  sql: %s", err, qUpper)
	}
	if !got.Valid || got.String != "ALICE" {
		t.Errorf("UPPER computed correlated scalar = %q (valid=%v), want \"ALICE\" (silent-NULL regression)", got.String, got.Valid)
	}

	// (b) Computed ARITHMETIC over a correlated aggregate-free single row.
	var n sql.NullInt64
	const qArith = "SELECT (SELECT o.amount + 1 FROM orders o WHERE o.id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, qArith).Scan(&n); err != nil {
		t.Fatalf("arithmetic computed scalar: %v\n  sql: %s", err, qArith)
	}
	if !n.Valid || n.Int64 != 101 {
		t.Errorf("arithmetic computed correlated scalar = %d (valid=%v), want 101", n.Int64, n.Valid)
	}
}
