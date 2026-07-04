package sqldriver_test

// RFC-173 W4-left — MIXED outer nesting runtime pins. BOTH nesting
// directions stay name-model: a single-source LEFT/RIGHT box that is a leg
// of an enclosing name-model INNER join (`d LEFT JOIN e ON … JOIN c ON …`)
// is kept name-model by the gate's enclosure guard (a positional box row
// under a name-merge parent reads the wrong source), and the reverse
// nesting (`a JOIN b ON … LEFT JOIN c ON …`) poisons — the LEFT's preserved
// leg is a cluster. Both classes planned clean but shipped with ZERO
// row-level pins (review finding), and the pins immediately caught two
// pre-existing master bugs: fabricated ALIAS.* keys on the null-padded box
// row (wrong-source rows) and the nondeterministic anchored re-enumeration
// planning panic. Row values, NULL-padding, and INNER drops are asserted
// for each nesting direction.

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_RFC173W4Left_MixedOuterNesting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_mixed"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE w4l_mixed_tmpl"+
		" CREATE TABLE dept (id BIGINT, cid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE emp (id BIGINT, did BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE cat (id BIGINT, label STRING, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE w4l_mixed_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// dept 1 → has an emp, cat matches   → survives with the emp
	// dept 2 → no emp, cat matches       → survives NULL-padded
	// dept 3 → has an emp, cat MISSING   → dropped by the INNER parent
	for _, ins := range []string{
		"INSERT INTO dept VALUES (1, 10), (2, 10), (3, 99)",
		"INSERT INTO emp VALUES (100, 1), (300, 3)",
		"INSERT INTO cat VALUES (10, 'retail')",
	} {
		if _, err := db.ExecContext(ctx, ins); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	type row struct {
		d, c int64
		e    sql.NullInt64
	}
	collect := func(t *testing.T, q string) []row {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v\n  sql: %s", err, q)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.d, &r.e, &r.c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}
	wantMixed := func(t *testing.T, q string) {
		t.Helper()
		got := collect(t, q)
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 (dept 3 must DROP at the INNER parent)\n  rows: %+v\n  sql: %s", len(got), got, q)
		}
		if got[0].d != 1 || !got[0].e.Valid || got[0].e.Int64 != 100 || got[0].c != 10 {
			t.Errorf("row 0 = %+v, want (1, 100, 10)", got[0])
		}
		if got[1].d != 2 || got[1].e.Valid || got[1].c != 10 {
			t.Errorf("row 1 = %+v, want (2, NULL, 10) — the LEFT's NULL-pad must SURVIVE the enclosing INNER", got[1])
		}
	}

	// (a) The gated LEFT box as a leg of a name-model INNER parent:
	// ((dept LEFT JOIN emp) JOIN cat).
	wantMixed(t, "SELECT d.id, e.id, c.id FROM dept d"+
		" LEFT JOIN emp e ON e.did = d.id"+
		" JOIN cat c ON c.id = d.cid ORDER BY d.id")

	// (b) The same shape with the box RIGHT-normalized:
	// ((emp RIGHT JOIN dept) JOIN cat) — dept stays the preserved side.
	wantMixed(t, "SELECT d.id, e.id, c.id FROM emp e"+
		" RIGHT JOIN dept d ON e.did = d.id"+
		" JOIN cat c ON c.id = d.cid ORDER BY d.id")

	// (c) The REVERSE nesting: ((dept JOIN cat) LEFT JOIN emp) — the LEFT's
	// preserved leg is a CLUSTER, the gate poisons, the box stays
	// name-model. Same rows, different plan class.
	wantMixed(t, "SELECT d.id, e.id, c.id FROM dept d"+
		" JOIN cat c ON c.id = d.cid"+
		" LEFT JOIN emp e ON e.did = d.id ORDER BY d.id")

	// (d) SELECT * across the mixed nesting keeps FROM-declaration column
	// order: dept(id, cid), emp(id, did), cat(id, label).
	rows, err := db.QueryContext(ctx, "SELECT * FROM dept d"+
		" LEFT JOIN emp e ON e.did = d.id"+
		" JOIN cat c ON c.id = d.cid ORDER BY d.id")
	if err != nil {
		t.Fatalf("select *: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	want := []string{"ID", "CID", "ID", "DID", "ID", "LABEL"}
	if len(cols) != len(want) {
		t.Fatalf("SELECT * columns = %v, want %v", cols, want)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Fatalf("SELECT * column %d = %q, want %q (declaration order)", i, cols[i], want[i])
		}
	}
	n := 0
	for rows.Next() {
		var dID, dCID int64
		var eID, eDID sql.NullInt64
		var cID int64
		var label string
		if err := rows.Scan(&dID, &dCID, &eID, &eDID, &cID, &label); err != nil {
			t.Fatalf("scan *: %v", err)
		}
		if n == 1 && (eID.Valid || eDID.Valid) {
			t.Errorf("dept 2's emp columns = (%v, %v), want NULLs", eID, eDID)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows *: %v", err)
	}
	if n != 2 {
		t.Fatalf("SELECT * rows = %d, want 2", n)
	}
}
