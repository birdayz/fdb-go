package sqldriver_test

// RFC-173 W4-left commit 4 — duplicate FROM-source aliases reject with
// Java's AMBIGUOUS code (42702). Java allows the duplicate at FROM (unique
// quantifier ids) and errors at reference resolution — which every practical
// query over the duplicate hits; Go rejects at the FROM walk (the 7.1
// namespace unification owns the exact per-reference check). Pre-rejection,
// a referenced duplicate silently bound LAST-LEG-WINS (wrong rows, never an
// error) and SELECT * died with an internal planner error.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestFDB_RFC173W4Left_DuplicateFromAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_dup"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE w4l_dup_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE w4l_dup_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO p VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("seed p: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO q VALUES (7)"); err != nil {
		t.Fatalf("seed q: %v", err)
	}

	reject := func(t *testing.T, q string) {
		t.Helper()
		var x any
		err := db.QueryRowContext(ctx, q).Scan(&x)
		if err == nil {
			t.Errorf("duplicate FROM alias must reject (42702), got rows\n  sql: %s", q)
			return
		}
		if !strings.Contains(err.Error(), "42702") {
			t.Errorf("duplicate FROM alias error = %v, want 42702 Ambiguous reference\n  sql: %s", err, q)
		}
	}

	// COLUMN-COLLIDING duplicates — every shared-column reference is
	// ambiguous per Java (attributes.size()==1); pre-fix Go silently bound
	// last-leg-wins (wrong rows, no error).
	reject(t, "SELECT p.id FROM p, q, p")
	reject(t, "SELECT a.v FROM p AS a, p AS a")

	// DISJOINT-column duplicate aliases pass the FROM check (Java's
	// ambiguity is per-ATTRIBUTE — conformance-verified, a.id resolves
	// uniquely and Java ANSWERS) but Go cannot EXECUTE the
	// indistinguishable-correlation cluster without the 7.1 per-reference
	// binding: the decline is the CLEAN 0AF00 (never wrong rows) — the
	// second marked divergence corner (DIVERGENCES.md).
	var aid int64
	if err := db.QueryRowContext(ctx, "SELECT a.id FROM p AS a, q AS a WHERE a.id = 2").Scan(&aid); err == nil {
		t.Errorf("disjoint-column dup alias unexpectedly ANSWERED %d — if the 7.1 binding landed, flip this pin to (2) and unmark the divergence", aid)
	} else if !strings.Contains(err.Error(), "0AF00") {
		t.Errorf("disjoint-column dup alias decline = %v, want the clean 0AF00 (never wrong rows)", err)
	}
	// The documented over-rejection corner: Java answers SELECT * over the
	// duplicate with duplicate columns; Go rejects until the 7.1 unification
	// (pre-fix this died with an INTERNAL planner error, not even a clean
	// code). DIVERGENCES.md records it.
	reject(t, "SELECT * FROM p, q, p")

	// A legitimate self-join with DISTINCT aliases keeps working.
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM p AS a, p AS b WHERE a.id = b.id").Scan(&n); err != nil {
		t.Fatalf("distinct-alias self-join: %v", err)
	}
	if n != 2 {
		t.Errorf("self-join count = %d, want 2", n)
	}
}
