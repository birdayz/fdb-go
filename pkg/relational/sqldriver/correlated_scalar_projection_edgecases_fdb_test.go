package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestFDB_FieldValuedComputedScalar pins a bug where a PARENTHESIZED plain
// column in a correlated scalar projection walks to a bare FieldValue, and the
// materialization must NOT re-key it to `_0` — the projection executor keys
// field-valued projections by the COLUMN NAME, so a seed expecting `_0` fails
// the leg adapter on a valid query.
func TestFDB_FieldValuedComputedScalar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_fv"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_fv_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, amount BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE corders (id BIGINT, customer_id BIGINT, PRIMARY KEY (id))"); err != nil {
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
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1, 100)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	var v sql.NullInt64
	const q = "SELECT (SELECT (o.amount) FROM orders o WHERE o.id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		t.Fatalf("field-valued computed scalar: %v\n  sql: %s", err, q)
	}
	if !v.Valid || v.Int64 != 100 {
		t.Errorf("field-valued computed scalar = %d (valid=%v), want 100", v.Int64, v.Valid)
	}

	// A COMPUTED wrapper around the same column (`(o.amount) + 0`) must still
	// take the materialized `_0` path — the fix routes only BARE field values
	// back to the plain-column path.
	const qComputed = "SELECT (SELECT (o.amount) + 0 FROM orders o WHERE o.id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, qComputed).Scan(&v); err != nil {
		t.Fatalf("computed-over-parenthesized scalar: %v\n  sql: %s", err, qComputed)
	}
	if !v.Valid || v.Int64 != 100 {
		t.Errorf("computed-over-parenthesized scalar = %d (valid=%v), want 100", v.Int64, v.Valid)
	}

	// An OUTER-scope parenthesized column (`(c.id)`) is NOT an inner row key at
	// all: routing it to the plain-column path strips the outer qualifier and
	// silently reads the INNER column of the same name (the order id, not the
	// customer id). It must take the MATERIALIZED path — its value comes from
	// the outer binding, evaluated per outer row. The seeded order (id=5,
	// customer_id=1) keeps the ids DISTINCT: want the outer c.id=1, never the
	// inner o.id=5.
	if _, err := db.ExecContext(ctx, "INSERT INTO corders VALUES (5, 1)"); err != nil {
		t.Fatalf("seed corder: %v", err)
	}
	const qOuter = "SELECT (SELECT (c.id) FROM corders o WHERE o.customer_id = c.id) " +
		"FROM customers c WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, qOuter).Scan(&v); err != nil {
		t.Fatalf("outer-scope parenthesized scalar: %v\n  sql: %s", err, qOuter)
	}
	if !v.Valid || v.Int64 != 1 {
		t.Errorf("outer-scope parenthesized scalar = %d (valid=%v), want 1 (the OUTER c.id; the inner o.id=5 means the bared key read the wrong scope)", v.Int64, v.Valid)
	}

	// (The AT-ordinal-alias scope corner — `AS v AT c` colliding with an outer
	// alias `c` — is pinned white-box in the embedded package
	// (TestInnerSourceAliases_MirrorsUnnestBinder): SQL INSERT cannot seed
	// array columns, and the rule under test is exactly the binder mirror.)
}

// TestFDB_AggregateLocalOuterRef pins a clustered-outer correlated scalar whose
// AGGREGATE OPERAND references an outer column. Aggregate operands evaluate
// against the aggregate's inner input row, which does not carry the level-2
// positional outer binding — the pull-up must NOT bake aggregate-local outer
// refs. This shape has NEVER planned (0AF00 on master, verified in a master
// worktree); an un-guarded bake would regress the clean plan error into a loud
// baked-ref runtime error. The pin asserts master parity: the clean 0AF00,
// never the runtime error. (Making the shape actually WORK needs aggregate
// evaluation to receive the outer binding — a separate follow-on.)
func TestFDB_AggregateLocalOuterRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_ag"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_ag_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE extras (id BIGINT, ref BIGINT, PRIMARY KEY (id))"+
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
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO extras VALUES (10, 7)"); err != nil {
		t.Fatalf("seed extras: %v", err)
	}
	// alice: orders 100 + 300; SUM(amount + e.ref) = 100+7 + 300+7 = 414.
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1,100),(2,1,300)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// The aggregate operand references E — the RIGHTMOST outer leg. The
	// ordinal path must decline (un-bakeable aggregate-local carrier) and the
	// query falls to the name-model path, which cannot plan it — exactly as on
	// master. A runtime baked-ref error here means the guard regressed.
	var v sql.NullInt64
	const q = "SELECT (SELECT SUM(o.amount + e.ref) FROM orders o WHERE o.customer_id = c.id) " +
		"FROM customers c, extras e WHERE c.id = 1 AND e.id = 10"
	err = db.QueryRowContext(ctx, q).Scan(&v)
	if err == nil {
		t.Fatalf("aggregate-local outer ref unexpectedly planned: got %d (valid=%v) — flip this pin to rows if the shape gained support", v.Int64, v.Valid)
	}
	if !strings.Contains(err.Error(), "0AF00") {
		t.Errorf("aggregate-local outer ref error = %v, want master-parity 0AF00 (clean plan error), never a baked-ref runtime error", err)
	}
}
