package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists_Round4 pins the two round-4 codex-found silent-wrong
// fold-bypass bugs and the clean-rejection of the newly-rejected shape:
//
//	bug 1 — projected EXISTS + a CORRELATED scalar subquery in the same SELECT:
//	        the fold's early return bypassed the correlated-scalar dispatch, so
//	        the correlated ScalarSubqueryValue was left unbound and read NULL
//	        every row. NOW rejected cleanly (the existential SelectExpression and
//	        the correlated-scalar LEFT-OUTER join select are incompatible
//	        structures — composing them is the multi-quantifier boundary). Guard
//	        sentinel asserts the clean ErrCodeUnsupportedQuery.
//
//	bug 2 — projected EXISTS + QUALIFIED ORDER BY key (ORDER BY t1.col1 DESC):
//	        the appended/pulled-up sort key was a flat FieldValue "T1.COL1", but
//	        the folded output record carries the column under a name that the key
//	        must match — a qualifier mismatch silently no-oped the sort. NOW the
//	        qualified key is rebased to the bare output column so the sort orders
//	        for real.
func TestFDB_ProjectedExists_Round4(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_r4")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_r4")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r4_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, val BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_r4/s WITH TEMPLATE projexists_r4_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_r4?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1: ids 1..5, col1 = 10,20,30,40,50 (ascending with id, so ORDER BY col1
	// DESC yields ids 5,4,3,2,1 — distinct from any id-order default; a no-op
	// sort would visibly fail). t2 references t1 ids {1,3,5}; each t2 row carries
	// a val (used for the correlated-scalar case).
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1, 11), (200, 3, 33), (300, 5, 55)")

	requireSortOverFlatMap := func(t *testing.T, q string) {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "FlatMap") {
			t.Errorf("expected FlatMap (existential probe) in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "FirstOrDefault") {
			t.Errorf("expected FirstOrDefault (existential one-row inner) in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "Sort") {
			t.Errorf("expected a Sort node above the existential FlatMap for %q, got:\n%s", q, plan)
		}
	}

	type idBool struct {
		id int64
		b  bool
	}
	queryIDBoolOrdered := func(t *testing.T, q string) []idBool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}

	// ── Bug 2: QUALIFIED ORDER BY key, single-table, NOT in SELECT output ────
	//
	// `... ORDER BY t1.col1 DESC`. col1 is not in the SELECT list, so it routes
	// through the remainingOrderByExpressions append-then-pullup branch. The
	// qualifier (t1.) must be stripped/rebased onto the outer alias so the sort
	// key resolves and DESC truly reverses to ids 5,4,3,2,1. Before the fix the
	// qualified key did not resolve → NULL every row → DESC silently fell to scan
	// order (1,2,3,4,5).
	t.Run("qualified_orderby_col_not_in_select_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.col1 DESC"
		requireSortOverFlatMap(t, q)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) != 2 {
			t.Fatalf("expected 2 result columns (col1 must not leak), got %v", cols)
		}
		var got []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		want := []idBool{{5, true}, {4, false}, {3, true}, {2, false}, {1, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v) — qualified ORDER BY t1.col1 DESC silently no-oped?", i, got[i], want[i], got)
			}
		}
	})

	// Bug 2 variant: QUALIFIED ORDER BY key on a column that IS in the SELECT
	// output (`... ORDER BY t1.id DESC`). The qualified key must rebase onto the
	// bare output column ID. Before the fix the flat FieldValue "T1.ID" did not
	// match the bare ID output field → NULL key → scan order.
	t.Run("qualified_orderby_selected_col_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.id DESC"
		requireSortOverFlatMap(t, q)
		got := queryIDBoolOrdered(t, q)
		want := []idBool{{5, true}, {4, false}, {3, true}, {2, false}, {1, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v) — qualified ORDER BY t1.id DESC silently no-oped?", i, got[i], want[i], got)
			}
		}
	})

	// Bug 2 variant: QUALIFIED ORDER BY id ASC — the ascending direction, to pin
	// the rebase works regardless of direction (a no-op sort would coincidentally
	// match scan order ASC and hide the bug, so DESC above is the real probe; ASC
	// here guards the rebase didn't break the common case).
	t.Run("qualified_orderby_selected_col_asc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.id ASC"
		got := queryIDBoolOrdered(t, q)
		want := []idBool{{1, true}, {2, false}, {3, true}, {4, false}, {5, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	// ── Bug 1: projected EXISTS + CORRELATED scalar subquery → clean reject ───
	//
	// `SELECT id, EXISTS(...), (SELECT val FROM t2 WHERE t2.t1_id = t1.id) FROM t1`.
	// The scalar subquery is CORRELATED (its WHERE references the outer t1.id), so
	// it routes to translateProjectWithCorrelatedScalar — a structure the
	// projected-EXISTS fold cannot compose with. Before the fix the fold's early
	// return dropped the correlated scalar → that column read NULL every row (a
	// silent wrong result). NOW the query rejects cleanly with the §8 guard's
	// unsupported message. Revert-proof guard sentinel: without the rejection the
	// query returns 5 rows with a NULL scalar column.
	t.Run("guard_rejects_correlated_scalar_plus_projected_exists", func(t *testing.T) {
		q := "SELECT id, " +
			"EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2, " +
			"(SELECT val FROM t2 WHERE t2.t1_id = t1.id) AS the_val FROM t1"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			// The query must NOT silently return rows with a NULL scalar column.
			var n int
			anyNullScalar := false
			for rows.Next() {
				var id int64
				var b sql.NullBool
				var v sql.NullInt64
				if scanErr := rows.Scan(&id, &b, &v); scanErr == nil && !v.Valid {
					anyNullScalar = true
				}
				n++
			}
			rows.Close()
			t.Fatalf("projected EXISTS + correlated scalar returned %d rows (anyNullScalar=%v) instead of a clean error — "+
				"the fold dropped the correlated scalar (silent NULL)", n, anyNullScalar)
		}
		if !strings.Contains(err.Error(), "projected EXISTS in this query shape is not yet supported") {
			t.Fatalf("expected the §8 guard's clean unsupported message, got: %v", err)
		}
	})

	// Bug 1 control: the SAME shape but with an UNCORRELATED scalar subquery
	// MUST still work (it is pre-evaluated and collected before the fold's early
	// return). This proves the rejection is narrow — correlated only — and does
	// not regress the supported uncorrelated case.
	t.Run("uncorrelated_scalar_plus_projected_exists_still_works", func(t *testing.T) {
		q := "SELECT id, " +
			"EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2, " +
			"(SELECT MAX(val) FROM t2) AS maxval FROM t1"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("uncorrelated scalar + projected EXISTS must still work, got: %v", err)
		}
		defer rows.Close()
		type r3 struct {
			id     int64
			b      bool
			maxval sql.NullInt64
		}
		var got []r3
		for rows.Next() {
			var r r3
			if err := rows.Scan(&r.id, &r.b, &r.maxval); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// MAX(val) over t2 = 55 for every row; booleans per id: 1,3,5 true; 2,4 false.
		if len(got) != 5 {
			t.Fatalf("got %d rows, want 5: %v", len(got), got)
		}
		want := map[int64]bool{1: true, 2: false, 3: true, 4: false, 5: true}
		for _, r := range got {
			if r.b != want[r.id] {
				t.Errorf("id=%d: got bool=%v, want %v", r.id, r.b, want[r.id])
			}
			if !r.maxval.Valid {
				t.Errorf("id=%d: uncorrelated scalar maxval came back NULL (regression)", r.id)
			} else if r.maxval.Int64 != 55 {
				t.Errorf("id=%d: maxval=%d, want 55", r.id, r.maxval.Int64)
			}
		}
	})
}
