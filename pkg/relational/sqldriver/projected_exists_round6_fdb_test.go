package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists_Round6 pins the two round-6 review-found regressions,
// both rooted in the projected-EXISTS fold reconstructing column-metadata and
// ORDER-BY sort-key derivation PIECEMEAL instead of reusing the normal
// (non-EXISTS) projection path's logic. The root fix unifies both derivations
// with the normal path; these tests assert the folded path now produces the
// SAME labels and the SAME ordering the normal path would.
//
//	P2a — ORDER BY a SELECT-list alias whose value is a simple column:
//	      `SELECT col1 AS id, id AS x, EXISTS(...) FROM t1 ORDER BY x`
//	      The fold re-applies the sort ON TOP of the folded projection, so the
//	      sort key must resolve to the OUTPUT field `X` (value = t1.id), not the
//	      underlying column the alias was rewritten to. Before the fix, the
//	      FieldValue pull-up returned early without the output-field-value match
//	      the non-FieldValue case has, so `ORDER BY x` read field `ID`
//	      (= col1 AS id) → sorted by col1, the WRONG column.
//
//	P2b — column LABEL for a qualified projected column alongside EXISTS:
//	      `SELECT t1.id, EXISTS(...) FROM t1 JOIN ...`
//	      The folded ColumnDef left Label empty so the ResultSet exposed the
//	      qualified `T1.ID`, whereas a non-EXISTS control query keeps the
//	      qualified Name for lookup but sets the DISPLAY label to bare `ID`.
//	      Adding a projected EXISTS must NOT change the labels of the other
//	      projected columns.
func TestFDB_ProjectedExists_Round6(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_r6")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_r6")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r6_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_r6/s WITH TEMPLATE projexists_r6_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_r6?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1: id ascends 1..5; col1 = id*10 — DESCENDS as id ascends would be a
	// different value sequence, but here col1 ascends WITH id. The P2a probe
	// `col1 AS id, id AS x ORDER BY x` swaps the visible "id" column to col1's
	// value, so to distinguish "sort by x (=t1.id)" from "sort by the field
	// named ID (=col1)" we need col1 and id to produce DIFFERENT row orders.
	// Choose col1 = 100 - id*10 so col1 DESCENDS as id ascends:
	//   id  : 1   2   3   4   5
	//   col1: 90  80  70  60  50
	// Then `ORDER BY x` (= ORDER BY t1.id ASC) must yield id 1,2,3,4,5, whereas
	// a buggy "sort by the field named ID" (= col1 ASC) yields id 5,4,3,2,1.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 90), (2, 80), (3, 70), (4, 60), (5, 50)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (10, 1), (30, 3), (50, 5)")

	// ════════════════════════════════════════════════════════════════════════
	// P2a: ORDER BY a SELECT-list alias whose value is a simple column.
	// ════════════════════════════════════════════════════════════════════════
	//
	// `SELECT col1 AS id, id AS x, EXISTS(...) FROM t1 ORDER BY x`:
	//   - output column 0 is named `ID` but holds col1's value (90,80,70,60,50);
	//   - output column 1 is named `X` and holds t1.id's value (1,2,3,4,5);
	//   - `ORDER BY x` must sort by X = t1.id ASC → x sequence 1,2,3,4,5,
	//     so col1-column (ID) sequence is 90,80,70,60,50.
	// The bug sorted by the field NAMED ID (col1) → ID column 50,60,70,80,90
	// and x 5,4,3,2,1. We read the x column (output col 1) to assert the order.
	t.Run("p2a_orderby_column_alias", func(t *testing.T) {
		q := "SELECT col1 AS id, id AS x, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY x"
		// X (= t1.id) ascending.
		assertXOrder(t, db, ctx, q, []int64{1, 2, 3, 4, 5})
	})

	// Control: ORDER BY x DESC reverses it (5,4,3,2,1). Proves the pull-up
	// resolves the right field in both directions, not by coincidence.
	t.Run("p2a_orderby_column_alias_desc", func(t *testing.T) {
		q := "SELECT col1 AS id, id AS x, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY x DESC"
		assertXOrder(t, db, ctx, q, []int64{5, 4, 3, 2, 1})
	})

	// P2a control: ORDER BY the OTHER alias `id` (= col1) sorts by col1.
	// col1 = 90,80,70,60,50, so ORDER BY id ASC ⇒ col1 50..90 ⇒ x (t1.id) 5,4,3,2,1.
	t.Run("p2a_orderby_other_alias", func(t *testing.T) {
		q := "SELECT col1 AS id, id AS x, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY id"
		assertXOrder(t, db, ctx, q, []int64{5, 4, 3, 2, 1})
	})

	// P2a expression-alias: ORDER BY an alias whose value is a COMPUTED
	// expression (id*1). col1=90..50, id=1..5; `(id) AS y` ascends with t1.id.
	// We can't easily distinguish a computed alias from a column here unless the
	// expression diverges from a plain column; use `col1 + 0 AS y` so y = col1
	// (DESC with id). ORDER BY y ASC ⇒ col1 50..90 ⇒ x 5,4,3,2,1.
	t.Run("p2a_orderby_expression_alias", func(t *testing.T) {
		q := "SELECT (col1 + 0) AS y, id AS x, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY y"
		assertXOrder(t, db, ctx, q, []int64{5, 4, 3, 2, 1})
	})

	// ════════════════════════════════════════════════════════════════════════
	// P2b: column LABEL parity with a non-EXISTS control query.
	// ════════════════════════════════════════════════════════════════════════
	//
	// For each projection shape, the labels reported by the driver for the
	// projected-EXISTS query must EXACTLY equal the labels reported by an
	// otherwise-identical NON-EXISTS control query (same projection minus the
	// EXISTS column). Adding a projected EXISTS must not change other columns'
	// labels.

	// bare column: `SELECT id, EXISTS(...) FROM t1` vs `SELECT id FROM t1`.
	t.Run("p2b_label_bare_column", func(t *testing.T) {
		existsQ := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1"
		controlQ := "SELECT id FROM t1"
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 1)
	})

	// aliased column: `SELECT id AS the_id, EXISTS(...) FROM t1`.
	t.Run("p2b_label_aliased_column", func(t *testing.T) {
		existsQ := "SELECT id AS the_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1"
		controlQ := "SELECT id AS the_id FROM t1"
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 1)
	})

	// qualified column (single-table): `SELECT t1.id, EXISTS(...) FROM t1`.
	// The control `SELECT t1.id FROM t1` exposes label `ID` (bare leaf). The
	// EXISTS query must match — not `T1.ID`.
	t.Run("p2b_label_qualified_column", func(t *testing.T) {
		existsQ := "SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1"
		controlQ := "SELECT t1.id FROM t1"
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 1)

		// Beyond the label, the qualified column's VALUE must still resolve: the
		// unification changed the folded datum-lookup Name for an unaliased
		// single-table qualified column from `T1.ID` to bare `ID` (the merged
		// outer row carries bare keys). Pin that the t1.id value is correct and
		// the EXISTS boolean tracks t2 membership ({1,3,5} have a t2).
		rows, err := db.QueryContext(ctx, existsQ+" ORDER BY t1.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		type rec struct {
			id  int64
			has bool
		}
		var got []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.has); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		want := []rec{{1, true}, {2, false}, {3, true}, {4, false}, {5, true}}
		if len(got) != len(want) {
			t.Fatalf("qualified projected-EXISTS returned %d rows %v, want %v", len(got), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d = %+v, want %+v — qualified datum or EXISTS boolean wrong", i, got[i], want[i])
			}
		}
	})

	// qualified column over a JOIN: `SELECT t1.id, t2.id, EXISTS(...) FROM t1 JOIN t2 ...`.
	// The control `SELECT t1.id, t2.id FROM t1 JOIN t2 ...` exposes labels
	// `ID`, `ID` (bare leaves of both legs). The EXISTS query must match.
	t.Run("p2b_label_qualified_column_join", func(t *testing.T) {
		existsQ := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t2 x WHERE x.t1_id = t1.id) AS has_x " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id"
		controlQ := "SELECT t1.id, t2.id FROM t1 JOIN t2 ON t2.t1_id = t1.id"
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 2)
	})
}

// assertXOrder runs a `SELECT <c0>, x, has_* ...` query and asserts the x
// column (output col index 1) appears in the given order.
func assertXOrder(t *testing.T, db *sql.DB, ctx context.Context, q string, wantX []int64) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var c0 int64
		var x int64
		var b bool
		if err := rows.Scan(&c0, &x, &b); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		got = append(got, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	if len(got) != len(wantX) {
		t.Fatalf("%q: got %d rows x=%v, want %d %v", q, len(got), got, len(wantX), wantX)
	}
	for i := range wantX {
		if got[i] != wantX[i] {
			t.Fatalf("%q: row %d x=%d, want %d (full x order %v, want %v) — sort read the wrong field?",
				q, i, got[i], wantX[i], got, wantX)
		}
	}
}

// assertLeadingLabelsMatch asserts that the FIRST n column labels (and type
// names) reported for the projected-EXISTS query EXACTLY equal those of the
// non-EXISTS control query. Labels are read via Rows.Columns(); type names via
// Rows.ColumnTypes(). Adding a projected EXISTS must not change the other
// columns' labels.
func assertLeadingLabelsMatch(t *testing.T, db *sql.DB, ctx context.Context, existsQ, controlQ string, n int) {
	t.Helper()
	labels := func(q string) (names, types, nulls []string) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("columnTypes %q: %v", q, err)
		}
		types = make([]string, len(cts))
		nulls = make([]string, len(cts))
		for i, ct := range cts {
			types[i] = ct.DatabaseTypeName()
			if nullable, ok := ct.Nullable(); ok {
				if nullable {
					nulls[i] = "NULLABLE"
				} else {
					nulls[i] = "NOT_NULL"
				}
			} else {
				nulls[i] = "UNKNOWN"
			}
		}
		names = make([]string, len(cols))
		for i, c := range cols {
			names[i] = strings.ToUpper(c)
		}
		return names, types, nulls
	}
	existsLabels, existsTypes, existsNulls := labels(existsQ)
	ctrlLabels, ctrlTypes, ctrlNulls := labels(controlQ)
	if len(existsLabels) < n {
		t.Fatalf("EXISTS query %q reported %d columns %v — fewer than the %d control columns", existsQ, len(existsLabels), existsLabels, n)
	}
	if len(ctrlLabels) < n {
		t.Fatalf("control query %q reported %d columns %v — fewer than %d", controlQ, len(ctrlLabels), ctrlLabels, n)
	}
	for i := 0; i < n; i++ {
		if existsLabels[i] != ctrlLabels[i] {
			t.Errorf("column %d label: EXISTS query=%q control=%q — adding a projected EXISTS changed the label (full exists=%v control=%v)",
				i, existsLabels[i], ctrlLabels[i], existsLabels, ctrlLabels)
		}
		if existsTypes[i] != ctrlTypes[i] {
			t.Errorf("column %d type: EXISTS query=%q control=%q (full exists=%v control=%v)",
				i, existsTypes[i], ctrlTypes[i], existsTypes, ctrlTypes)
		}
		if existsNulls[i] != ctrlNulls[i] {
			t.Errorf("column %d nullability: EXISTS query=%q control=%q (full exists=%v control=%v)",
				i, existsNulls[i], ctrlNulls[i], existsNulls, ctrlNulls)
		}
	}
}
