package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_CorrelatedExistsDerivedInner pins the derived-table-inner correlated
// EXISTS fix. A correlated EXISTS whose inner FROM is a DERIVED TABLE
// (`(SELECT …) AS d`) and which enters the buildCorrelatedExists fallback ONLY
// because its (ignored) SELECT list references an outer column — with NO WHERE
// and NO ON (the fast path) — used to rebuild the inner FROM as `NewScan("d")`.
// `d` is NOT a catalog table, so the executor scanned a non-existent relation as
// EMPTY: EXISTS silently tested the wrong (empty) relation and answered wrong
// rows. The fix builds the derived BODY through the catalog-aware path and wraps
// it in the LogicalCTE(alias) carrier, so the inner FROM carries the `(SELECT …)`
// subplan.
//
// Data is BIPOLAR-DISCRIMINATING: the derived body is uncorrelated (EXISTS only
// tests non-emptiness), so a NON-EMPTY body must read EXISTS TRUE for every outer
// row, and an EMPTY body must read EXISTS FALSE. The two failure modes each flip
// exactly one polarity:
//   - the pre-fix body loss (bare scan of the missing table `d`) makes EVERY body
//     read empty → EXISTS always FALSE → the non-empty case flips true→false.
//   - a naive "always true" repair would flip the empty case false→true.
//
// ord = {(1,10),(2,20)}. Java 4.12.11.0: a derived body of {1 row} → EXISTS true
// for both outer rows; a derived body of {} → EXISTS false for both.
func TestFDB_CorrelatedExistsDerivedInner(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_derived_inner"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cedi_tmpl "+
		"CREATE TABLE ord (order_id BIGINT NOT NULL, cust_id BIGINT, PRIMARY KEY (order_id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cedi_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO ord VALUES (1, 10), (2, 20)")

	type idBool struct {
		id int64
		e  bool
	}
	queryIDBool := func(t *testing.T, sqlText string) []idBool {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr != nil {
			t.Fatalf("derived-inner EXISTS must plan (body built, not scanned as a table): %v\n  sql: %s", qerr, sqlText)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if serr := rows.Scan(&r.id, &r.e); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			out = append(out, r)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows.Err: %v", rerr)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}
	eq := func(t *testing.T, got, want []idBool) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}

	// NON-EMPTY derived body (d = {1}) → EXISTS true for every outer row. A dropped
	// body scans the missing table `d` as EMPTY → EXISTS false (the silent-wrong).
	t.Run("nonempty_body_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id = 1) AS d) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})

	// EMPTY derived body (d = {}) → EXISTS false for every outer row. Guards against
	// an "always true" repair (a dropped body ALSO reads false here, so this alone
	// would not catch the bug — it is the negative pole of the bipolar pair).
	t.Run("empty_body_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id > 100) AS d) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})

	// Whole-table derived body (SELECT *-free projection over all rows) → non-empty
	// → true. A dropped body flips to false.
	t.Run("all_rows_body_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord) AS d) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})

	// NOT EXISTS mirrors the polarity: non-empty body → false; empty body → true.
	// A dropped body reads NOT EXISTS true for BOTH, flipping the non-empty case.
	t.Run("not_exists_nonempty_body_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, NOT EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id = 1) AS d) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})
	t.Run("not_exists_empty_body_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, NOT EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id > 100) AS d) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})

	// A RENAMED-projection derived body: the virtual schema follows the body's
	// OUTPUT column, and the body is still built (non-empty → true).
	t.Run("renamed_projection_body_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id AS oid FROM ord WHERE order_id = 1) AS d) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})

	// WHERE-EXISTS filter form (not just projected): only the derived body's
	// non-emptiness matters, so a non-empty body keeps every outer row and an empty
	// body drops them all. A dropped body would read empty → drop every row.
	t.Run("where_exists_nonempty_keeps_all", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT o.order_id FROM ord AS o WHERE EXISTS "+
			"(SELECT o.order_id FROM (SELECT order_id FROM ord WHERE order_id = 1) AS d)")
		if qerr != nil {
			t.Fatalf("where-exists derived-inner must plan: %v", qerr)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if serr := rows.Scan(&v); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			got = append(got, v)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if fmt.Sprint(got) != fmt.Sprint([]int64{1, 2}) {
			t.Errorf("WHERE EXISTS got %v, want [1 2] (non-empty derived body keeps every outer row)", got)
		}
	})

	// Multi-source fast path: a derived PRIMARY followed by a real comma leg. The
	// primary derived body must build too (not a bare scan): non-empty d × non-empty
	// ord → non-empty cross product → EXISTS true; empty d → empty → false.
	t.Run("derived_primary_then_real_leg_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id = 1) AS d, ord b) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})
	t.Run("derived_primary_empty_then_real_leg_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id > 100) AS d, ord b) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})

	// REAL primary + DERIVED comma LEG: the leg twin of the primary bug. The leg
	// `(SELECT …) AS d` is NOT a catalog table; a bare NewScan("d") scans a
	// non-existent relation as EMPTY → `ord × ∅` = ∅ → EXISTS false for every outer
	// row (the silent-wrong). The fix builds the leg body via the CTE carrier, so
	// `ord(2) × d={1}(1)` = 2 rows → non-empty → true. Bipolar: an empty leg body
	// must still read false. (Without the leg fix `nonempty` flips true→false — the
	// dimensional gap the real-then-real leg test misses because its leg is a real
	// table, so a broken derived leg never shows.)
	t.Run("real_primary_then_derived_leg_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"ord a, (SELECT order_id FROM ord WHERE order_id = 1) AS d) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})
	t.Run("real_primary_then_derived_leg_empty_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"ord a, (SELECT order_id FROM ord WHERE order_id > 100) AS d) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})

	// DERIVED primary + DERIVED comma LEG: both sources route through the CTE
	// carrier. d={1} × e={2} = 1 row → non-empty → true; an empty d collapses the
	// product → false. Guards against a regression that fixed the primary but not
	// the leg (or vice versa).
	t.Run("derived_primary_then_derived_leg_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id = 1) AS d, "+
			"(SELECT order_id FROM ord WHERE order_id = 2) AS e) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})
	t.Run("derived_primary_then_derived_leg_empty_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord WHERE order_id > 100) AS d, "+
			"(SELECT order_id FROM ord WHERE order_id = 2) AS e) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})

	// The helper's OWN loud-decline branch: a derived body the inner builder cannot
	// plan (undefined column) reaches buildDerivedInnerCarrier's innerErr path and
	// must DECLINE LOUDLY — never degrade to the empty bare scan (which would make
	// EXISTS silently read false for every outer row with NO error). This pins the
	// commit's central "declines loudly, not an empty scan" claim, which the
	// plannable-body subtests above cannot exercise. (Primary position; the leg
	// position shares the same builder.)
	//
	// The body's failure is a FAITHFUL diagnostic of the inner query (undefined
	// column → 42703), NOT an unsupported-shape decline — so it must surface the SAME
	// SQLSTATE in BOTH the PROJECTED and the WHERE EXISTS position. buildDerivedInner-
	// Carrier surfaces the wrapped *api.Error unwrapped precisely so mapPredicateWalk-
	// Error does not rewrite the WHERE-position code to 0A000 (which it would if the
	// error stayed wrapped in CorrelatedExistsError). The two positions are pinned as
	// a pair; a divergence here is the position-dependent SQLSTATE regression.
	t.Run("unplannable_derived_body_declines_loudly_projected", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT nonexistent_col FROM ord) AS d) FROM ord AS o")
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a LOUD decline for an unplannable derived body (undefined column) — the fix must not degrade to an empty scan")
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUndefinedColumn)
	})
	t.Run("unplannable_derived_body_declines_loudly_where", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT o.order_id FROM ord AS o WHERE EXISTS "+
			"(SELECT o.order_id FROM (SELECT nonexistent_col FROM ord) AS d)")
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a LOUD decline for an unplannable derived body in a WHERE EXISTS")
		}
		// Must be the FAITHFUL 42703 (same as the projected position) — never rewritten
		// to 0A000 by the CorrelatedExistsError classification.
		requireSQLSTATE(t, qerr, api.ErrCodeUndefinedColumn)
	})
	// Unplannable derived LEG body in WHERE position: the leg's carrier build fails in
	// the rights loop and returns the faithful *api.Error EARLY (before the leg's own
	// scope-build 0A000 decline), so the WHERE EXISTS surfaces 42703 — same faithful,
	// position-independent code as the primary. The one forbidden outcome (an empty
	// scan → EXISTS silently false, no error) never occurs.
	t.Run("unplannable_derived_leg_body_declines_loudly_where", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT o.order_id FROM ord AS o WHERE EXISTS "+
			"(SELECT o.order_id FROM ord a, (SELECT nonexistent_col FROM ord) AS d)")
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a LOUD decline for an unplannable derived LEG body in a WHERE EXISTS")
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUndefinedColumn)
	})

	// A derived-inner correlated EXISTS WITH an EXISTS-body WHERE on the derived
	// alias skips the fast path and reaches the scope/resolver, where a derived
	// source is not resolvable via the WITH registry. That shape DECLINES cleanly
	// (0A000) — correct-or-conservative — rather than silently dropping the body.
	// Pinned so the decline stays LOUD (never regresses to a silent-wrong).
	t.Run("derived_inner_with_body_where_declines_0A000", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT o.order_id, EXISTS (SELECT o.order_id FROM "+
			"(SELECT order_id FROM ord) AS d WHERE d.order_id = 1) FROM ord AS o")
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a clean decline (0A000) for a derived-inner EXISTS with a body WHERE")
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUnsupportedOperation)
	})
}
