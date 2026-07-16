package sqldriver_test

// EXISTS-over-join through the ordinal seed.
// The matrix deliberately covers axes that are easy to leave untested:
// NOT EXISTS and NON-CORRELATED EXISTS alongside the
// correlated forms, plus RIGHT-JOIN+EXISTS SELECT * column
// order (translateJoinWithExists must build its result value in DECLARATION
// order, not post-swap order — a latent declaration-order divergence).

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_ExistsOverJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_ex"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4l_ex_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE emp (id BIGINT, dept_id BIGINT, fname STRING, PRIMARY KEY (id))"+
		" CREATE TABLE dept (id BIGINT, dname STRING, PRIMARY KEY (id))"+
		" CREATE TABLE badge (id BIGINT, emp_id BIGINT, PRIMARY KEY (id))"); err != nil {
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
	// alice(dept 1, badge), bob(dept 2, NO badge); dept 3 empty.
	if _, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 1, 'alice'), (2, 2, 'bob')"); err != nil {
		t.Fatalf("seed emp: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'eng'), (2, 'ops'), (3, 'empty')"); err != nil {
		t.Fatalf("seed dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO badge VALUES (10, 1)"); err != nil {
		t.Fatalf("seed badge: %v", err)
	}

	one := func(t *testing.T, q string, wantName string) {
		t.Helper()
		var got sql.NullString
		if err := db.QueryRowContext(ctx, q).Scan(&got); err != nil {
			t.Errorf("query error: %v\n  sql: %s", err, q)
			return
		}
		if got.String != wantName {
			t.Errorf("got %q, want %q\n  sql: %s", got.String, wantName, q)
		}
	}

	// (a) Correlated EXISTS over a gated inner join — correlation to the
	// FIRST (non-rightmost) leg.
	one(t, "SELECT e.fname FROM emp e JOIN dept d ON e.dept_id = d.id "+
		"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)", "alice")

	// (b) Correlated NOT EXISTS — the blind axis.
	one(t, "SELECT e.fname FROM emp e JOIN dept d ON e.dept_id = d.id "+
		"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)", "bob")

	// (c) NON-correlated EXISTS (true) — the other blind axis: every joined
	// row passes; pin the count.
	rows, qerr := db.QueryContext(ctx, "SELECT e.fname FROM emp e JOIN dept d ON e.dept_id = d.id "+
		"WHERE EXISTS (SELECT 1 FROM badge b)")
	if qerr != nil {
		t.Fatalf("non-correlated EXISTS: %v", qerr)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != 2 {
		t.Errorf("non-correlated EXISTS(true) row count = %d, want 2", n)
	}

	// (d) NON-correlated NOT EXISTS over a non-empty inner (false) — zero rows.
	rows2, qerr2 := db.QueryContext(ctx, "SELECT e.fname FROM emp e JOIN dept d ON e.dept_id = d.id "+
		"WHERE NOT EXISTS (SELECT 1 FROM badge b)")
	if qerr2 != nil {
		t.Fatalf("non-correlated NOT EXISTS: %v", qerr2)
	}
	if rows2.Next() {
		t.Error("non-correlated NOT EXISTS over non-empty inner must yield 0 rows")
	}
	rows2.Close()

	// (e) Correlated EXISTS referencing the RIGHTMOST leg.
	one(t, "SELECT e.fname FROM emp e JOIN dept d ON e.dept_id = d.id "+
		"WHERE EXISTS (SELECT 1 FROM dept d2 WHERE d2.id = d.id AND d2.dname = 'eng')", "alice")

	// (f) A latent declaration-order divergence: RIGHT JOIN + EXISTS SELECT * —
	// column order must be DECLARATION order (emp columns first), exactly as
	// the plain RIGHT JOIN. Without this, the result value would be
	// built POST-SWAP (dept first).
	cols := func(t *testing.T, q string) []string {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("cols query: %v\n  sql: %s", err, q)
		}
		defer r.Close()
		cs, _ := r.Columns()
		return cs
	}
	gotCols := cols(t, "SELECT * FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id "+
		"WHERE EXISTS (SELECT 1 FROM badge b)")
	wantCols := []string{"ID", "DEPT_ID", "FNAME", "ID", "DNAME"}
	if len(gotCols) != len(wantCols) {
		t.Fatalf("RIGHT+EXISTS SELECT * columns = %v, want %v (declaration order)", gotCols, wantCols)
	}
	for i := range wantCols {
		if gotCols[i] != wantCols[i] {
			t.Fatalf("RIGHT+EXISTS SELECT * columns = %v, want %v (declaration order — the post-swap divergence)", gotCols, wantCols)
		}
	}

	// (g) LEFT JOIN + correlated EXISTS: null-extended dept rows survive the
	// existential filter when the correlation matches.
	one(t, "SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
		"WHERE d.id = 3 AND NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)", "empty")
}
