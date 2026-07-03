package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestFDB_RFC173W4b_ClusteredOuterScalar pins RFC-173 W4b shape 1: a correlated
// scalar subquery in the projection over a MULTI-TABLE outer FROM. The outer
// cluster is a gated inner join (flows positionally once ordinalized), so the
// 2-leg correlated-scalar seed must resolve dotted outer projections (C.NAME,
// E.TAG) and the inner's correlated outer reference (o.id = c.id — the FIRST
// leg, not the rightmost the outer quantifier is named after) over that row.
// Correlation to the rightmost leg and a WHERE mixing both legs are pinned too.
func TestFDB_RFC173W4b_ClusteredOuterScalar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_co"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_co_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE extras (id BIGINT, ref BIGINT, tag STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, amount BIGINT, PRIMARY KEY (id))"); err != nil {
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
	if _, err := db.ExecContext(ctx, "INSERT INTO extras VALUES (10, 2, 'x'), (11, 1, 'y')"); err != nil {
		t.Fatalf("seed extras: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1, 100), (2, 50)"); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// (a) Comma cluster, csq correlated to the FIRST leg (not the rightmost —
	// the outer quantifier is NAMED after the rightmost leaf, so this exercises
	// binding a non-eponymous leg alias inside the inner).
	var name, tag sql.NullString
	var amt sql.NullInt64
	const qFirst = "SELECT c.name, e.tag, (SELECT o.amount FROM orders o WHERE o.id = c.id) " +
		"FROM customers c, extras e WHERE c.id = 1 AND e.id = 10"
	if err := db.QueryRowContext(ctx, qFirst).Scan(&name, &tag, &amt); err != nil {
		t.Errorf("csq over comma cluster (correlate to first leg): %v\n  sql: %s", err, qFirst)
	}
	if name.String != "alice" || tag.String != "x" || !amt.Valid || amt.Int64 != 100 {
		t.Errorf("first-leg correlation = (%q, %q, %d valid=%v), want (alice, x, 100)",
			name.String, tag.String, amt.Int64, amt.Valid)
	}

	// (b) csq correlated to the RIGHTMOST leg (the alias the outer quantifier
	// carries).
	const qLast = "SELECT (SELECT o.amount FROM orders o WHERE o.id = e.ref) " +
		"FROM customers c, extras e WHERE c.id = 1 AND e.id = 10"
	if err := db.QueryRowContext(ctx, qLast).Scan(&amt); err != nil {
		t.Errorf("csq over comma cluster (correlate to rightmost leg): %v\n  sql: %s", err, qLast)
	}
	if !amt.Valid || amt.Int64 != 50 {
		t.Errorf("rightmost-leg correlation = %d (valid=%v), want 50 (e.ref=2 -> orders[2])", amt.Int64, amt.Valid)
	}

	// (c) Explicit JOIN ... ON cluster (same gated shape via the ON predicate).
	const qOn = "SELECT c.name, (SELECT o.amount FROM orders o WHERE o.id = c.id) " +
		"FROM customers c JOIN extras e ON e.ref = c.id WHERE c.id = 1"
	if err := db.QueryRowContext(ctx, qOn).Scan(&name, &amt); err != nil {
		t.Errorf("csq over JOIN..ON cluster: %v\n  sql: %s", err, qOn)
	}
	if name.String != "alice" || !amt.Valid || amt.Int64 != 100 {
		t.Errorf("JOIN..ON cluster = (%q, %d valid=%v), want (alice, 100)", name.String, amt.Int64, amt.Valid)
	}

	// (d) UNGATED outer (LEFT-join box) + NON-rightmost correlation: the LEFT
	// cluster cannot ordinalize until W4-left, and the name model returned a
	// SILENT NULL here (the first leg's alias is unbound at eval) — the ruled
	// CORRECT-or-LOUD policy DECLINES the query instead (clean plan error,
	// never silently wrong rows). W4-left flips this pin to (bob, 50).
	const qLeft = "SELECT c.name, (SELECT o.amount FROM orders o WHERE o.id = c.id) " +
		"FROM customers c LEFT JOIN extras e ON e.ref = c.id WHERE c.id = 2"
	if err := db.QueryRowContext(ctx, qLeft).Scan(&name, &amt); err == nil {
		t.Errorf("LEFT-join outer with non-rightmost correlation must DECLINE (clean plan error), got rows (%q, %d valid=%v) — a silent NULL regression risk\n  sql: %s",
			name.String, amt.Int64, amt.Valid, qLeft)
	} else if !strings.Contains(err.Error(), "0AF00") {
		t.Errorf("LEFT-join non-rightmost decline error = %v, want the clean plan error (0AF00), not a runtime failure", err)
	}

	// (e) UNGATED outer, correlation to the RIGHTMOST leg (the outer
	// quantifier's own alias) — the residual name-model binding.
	const qLeftLast = "SELECT (SELECT o.amount FROM orders o WHERE o.id = e.ref) " +
		"FROM customers c LEFT JOIN extras e ON e.ref = c.id WHERE c.id = 2"
	if err := db.QueryRowContext(ctx, qLeftLast).Scan(&amt); err != nil {
		t.Errorf("csq over LEFT-join outer (rightmost correlation): %v\n  sql: %s", err, qLeftLast)
	}
	if !amt.Valid || amt.Int64 != 50 {
		t.Errorf("LEFT-join rightmost correlation = %d (valid=%v), want 50", amt.Int64, amt.Valid)
	}

	// (f) Comma cluster, first-leg correlation, SINGLE projection (isolates the
	// correlation axis from the multi-leg projection axis of (a)).
	const qFirstOnly = "SELECT (SELECT o.amount FROM orders o WHERE o.id = c.id) " +
		"FROM customers c, extras e WHERE c.id = 1 AND e.id = 10"
	if err := db.QueryRowContext(ctx, qFirstOnly).Scan(&amt); err != nil {
		t.Errorf("csq over comma cluster (first-leg corr, single proj): %v\n  sql: %s", err, qFirstOnly)
	}
	if !amt.Valid || amt.Int64 != 100 {
		t.Errorf("first-leg corr single proj = %d (valid=%v), want 100", amt.Int64, amt.Valid)
	}

	// Merge-interaction pin (design-ruling condition on the 2-leg seed): if
	// SelectMergeRule flattens the gated outer leg into the LEFT-outer select,
	// the baked concat-QOV refs must compose to per-leg ordinals — a wrong or
	// unstable composition shows up as a nondeterministic plan (and the row
	// pins above catch a wrong-ordinal read). Pin plan determinism across 5
	// fresh plans of the widest query.
	var firstPlan string
	for i := 0; i < 5; i++ {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+qFirst).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN run %d: %v", i, err)
		}
		if i == 0 {
			firstPlan = plan
		} else if plan != firstPlan {
			t.Fatalf("plan UNSTABLE across runs (merge/compose nondeterminism):\n  run 0: %s\n  run %d: %s", firstPlan, i, plan)
		}
	}
}
