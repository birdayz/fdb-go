package sqldriver_test

import (
	"context"
	"database/sql"
	"testing"
)

// ogsDB sets up customers + orders for the ordered-grouped correlated scalar
// subquery tests (RFC-085). Customer 1's orders: status 'a' → {10,5} (SUM 15),
// status 'b' → {20} (SUM 20). So GROUP BY status ORDER BY status picks group 'a'
// (ASC, SUM 15) or 'b' (DESC, SUM 20) deterministically.
func ogsDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/ogs_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "ogs_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, customer_id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))"); err != nil {
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
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'c1')"); err != nil {
		t.Fatalf("seed c: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1,'a',10),(2,1,'a',5),(3,1,'b',20)"); err != nil {
		t.Fatalf("seed o: %v", err)
	}
	return db, ctx
}

func ogsScalar(t *testing.T, ctx context.Context, db *sql.DB, q string) (int64, bool) {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n.Int64, n.Valid
}

// TestFDB_OrderedGroupedScalarSubquery pins RFC-085: ORDER BY over the grouped
// output of a correlated scalar subquery makes the multi-group FirstOrDefault
// choice deterministic (was rejected outright).
func TestFDB_OrderedGroupedScalarSubquery(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "core")

	// ORDER BY the group key ASC → first group 'a' → SUM(amount)=15.
	asc := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, asc); !ok || got != 15 {
		t.Fatalf("ORDER BY status ASC: got %d (valid=%v), want 15 (group 'a')", got, ok)
	}

	// ORDER BY the group key DESC → first group 'b' → SUM(amount)=20.
	desc := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status DESC LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, desc); !ok || got != 20 {
		t.Fatalf("ORDER BY status DESC: got %d (valid=%v), want 20 (group 'b')", got, ok)
	}
}

// TestFDB_OrderedGroupedScalarSubquery_Determinism runs the ordered subquery
// repeatedly — the whole point is a stable group choice.
func TestFDB_OrderedGroupedScalarSubquery_Determinism(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "det")
	q := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status LIMIT 1) FROM customers c WHERE c.id = 1"
	for i := 0; i < 10; i++ {
		if got, ok := ogsScalar(t, ctx, db, q); !ok || got != 15 {
			t.Fatalf("run %d: got %d (valid=%v), want 15", i, got, ok)
		}
	}
}

// TestFDB_OrderedGroupedScalarSubquery_Reject pins that ORDER BY over a column
// that is neither grouped nor a selected aggregate is rejected LOUDLY (not a
// silent-nil sort that would defeat the determinism this feature provides).
func TestFDB_OrderedGroupedScalarSubquery_Reject(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "rej")

	// ORDER BY a non-grouped, non-aggregated column (amount) → reject.
	_, err := db.ExecContext(ctx, "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.amount LIMIT 1) FROM customers c WHERE c.id = 1")
	if err == nil {
		t.Fatal("ORDER BY a non-grouped/non-aggregated column should be rejected, not silently ignored")
	}

	// ORDER BY an aggregate that is NOT selected (the subquery selects SUM, orders
	// by COUNT) → reject (harvesting ORDER-BY-only aggregates is a future extension).
	_, err = db.ExecContext(ctx, "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY COUNT(*) LIMIT 1) FROM customers c WHERE c.id = 1")
	if err == nil {
		t.Fatal("ORDER BY an unselected aggregate should be rejected, not silently ignored")
	}
}

// TestFDB_OrderedScalarSubquery_NoGroupByUnchanged guards that ORDER BY WITHOUT
// GROUP BY (ordering rows before LIMIT 1) still works (it sorts the raw rows),
// with the ORDER BY key written either bare or qualified.
func TestFDB_OrderedScalarSubquery_NoGroupByUnchanged(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "nogb")
	// Pick the lowest-amount order's amount for c.id=1: ORDER BY amount ASC LIMIT 1 → 5.
	bare := "SELECT (SELECT amount FROM orders o WHERE o.customer_id = c.id ORDER BY amount LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, bare); !ok || got != 5 {
		t.Fatalf("non-GROUP-BY ORDER BY (bare key): got %d (valid=%v), want 5", got, ok)
	}
	// A QUALIFIED ORDER BY key (`ORDER BY o.amount`) must resolve to the same
	// bare datum key the single-table scan row carries — otherwise it misses,
	// sorts every row equal, and silently returns an arbitrary row. Regression
	// for the qualifier-not-stripped bug surfaced by RFC-085.
	qualKey := "SELECT (SELECT amount FROM orders o WHERE o.customer_id = c.id ORDER BY o.amount LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, qualKey); !ok || got != 5 {
		t.Fatalf("non-GROUP-BY ORDER BY (qualified key): got %d (valid=%v), want 5", got, ok)
	}
}

// TestFDB_QualifiedNonAggScalarSubquery pins that a QUALIFIED projection in a
// non-aggregate correlated scalar subquery (`SELECT o.amount`) resolves to the
// bare datum key instead of silently returning NULL. The scalarCol used to keep
// the `o.` qualifier (`O.AMOUNT`), which replaceScalarSubqueryRef double-prefixed
// to `O.O.AMOUNT` at read time → NULL. Surfaced while testing RFC-085. The
// no-ORDER-BY case proves the bug is in column resolution, not the sort.
func TestFDB_QualifiedNonAggScalarSubquery(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "qproj")

	// Qualified projection, no ORDER BY → first scan row's amount (id=1 → 10).
	noOrder := "SELECT (SELECT o.amount FROM orders o WHERE o.customer_id = c.id LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, noOrder); !ok || got != 10 {
		t.Fatalf("qualified projection (no ORDER BY): got %d (valid=%v), want 10 (was NULL)", got, ok)
	}

	// Qualified projection + qualified ORDER BY → lowest amount (5).
	ordered := "SELECT (SELECT o.amount FROM orders o WHERE o.customer_id = c.id ORDER BY o.amount LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, ordered); !ok || got != 5 {
		t.Fatalf("qualified projection + ORDER BY: got %d (valid=%v), want 5 (was NULL)", got, ok)
	}
}
