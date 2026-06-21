package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_ProjectedExists_UnaliasedComputedColumn pins RFC-141 R4 round-9 P2:
// an UNALIASED COMPUTED select item alongside a projected EXISTS.
//
//	SELECT id + 1, EXISTS (SELECT 1 FROM t2 WHERE t2.id = t.ref) AS e FROM t
//
// The bug: the projected-EXISTS fold named the folded computed field with the
// expression TEXT (`ID + 1`), so Rows.Columns() reported `ID + 1`. The normal
// (non-EXISTS) projection path exposes an unaliased non-field (computed)
// expression under a GENERATED positional name (`_0`). Adding the EXISTS thus
// CHANGED the public column name from `_0` to `ID + 1` and broke downstream
// references to the generated column.
//
// The fix reuses the normal path's positional naming for an unaliased computed
// column in the fold, so the column name is IDENTICAL with or without the EXISTS.
//
// Java's generateSelect names an anonymous expression projection by its
// zero-based position (`_N`); the qualifier/expression text never becomes the
// user-visible column name.
func TestFDB_ProjectedExists_UnaliasedComputedColumn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_existscomputed")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_existscomputed")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ecc_tmpl "+
		"CREATE TABLE t (id BIGINT NOT NULL, ref BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_existscomputed/s WITH TEMPLATE ecc_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_existscomputed?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t.ref points at t2.id for row 1; row 2 points at a missing t2 ⇒ EXISTS
	// true,false.
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 100), (2, 999)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100)")

	columnsOf := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		return cols
	}

	// The control: an unaliased computed column WITHOUT an EXISTS. Whatever
	// generated name this exposes for `id + 1` is the contract the EXISTS
	// variant must match — we read it dynamically (rather than hardcoding `_0`)
	// so the test pins PARITY, not a specific naming scheme.
	control := columnsOf(t, "SELECT id + 1 FROM t")
	if len(control) != 1 {
		t.Fatalf("control: expected 1 column, got %v", control)
	}
	genName := control[0]
	// Sanity: the generated name must NOT be the raw expression text — that is
	// exactly the bug (and would make the parity assertion vacuous).
	if genName == "ID + 1" || genName == "ID+1" {
		t.Fatalf("control exposed the expression text %q as the column name; expected a generated name", genName)
	}

	// P2: the SAME unaliased computed column, now alongside a projected EXISTS.
	// The fold must name `id + 1` with the SAME generated name, and the EXISTS
	// alias `e` must be the second column.
	t.Run("columns_match_control", func(t *testing.T) {
		q := "SELECT id + 1, EXISTS (SELECT 1 FROM t2 WHERE t2.id = t.ref) AS e FROM t"
		got := columnsOf(t, q)
		want := []string{genName, "E"}
		if len(got) != len(want) {
			t.Fatalf("columns: got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("column[%d]: got %q, want %q", i, got[i], want[i])
			}
		}
	})

	// The VALUE of the computed column and the EXISTS boolean must be correct,
	// read positionally (the generated name keys the row). id+1 = 2,3; EXISTS
	// = true,false.
	t.Run("values_correct", func(t *testing.T) {
		q := "SELECT id + 1, EXISTS (SELECT 1 FROM t2 WHERE t2.id = t.ref) AS e FROM t"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		type row struct {
			c int64
			e bool
		}
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.c, &r.e); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		want := []row{{2, true}, {3, false}}
		if len(out) != len(want) {
			t.Fatalf("values: got %v, want %v", out, want)
		}
		for i := range out {
			if out[i] != want[i] {
				t.Errorf("row[%d]: got %v, want %v", i, out[i], want[i])
			}
		}
	})
}
