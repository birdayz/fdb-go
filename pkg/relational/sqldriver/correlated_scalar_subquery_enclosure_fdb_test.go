package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_CorrelatedScalarSubqueryEnclosure pins the correlated-scalar
// subquery's enclosure behavior: translateProjectWithCorrelatedScalar does
// not force `inInnerCluster` on its OUTER, and routes its INNER through
// translateSubqueryRef so the never-merged scalar subquery roots a FRESH
// cluster — the same primitive the existential inner uses.
//
// A single shared enclosure flag would conflate two architecturally distinct
// positions:
//   - OUTER: only a single-source or ungated outer reaches this path (a gated
//     multi-table outer ordinalizes earlier); a buried-join / multi-source
//     outer has arity≠1 and declines LOUDLY (0AF00) regardless of any
//     enclosure flag — so forcing enclosure here has no observable effect.
//     The outer instead inherits whatever enclosure state produced it.
//   - INNER: a correlated-scalar subquery is never merged into its parent, so
//     it must gate on its own arity, not the outer's enclosure — routed via
//     translateSubqueryRef.
//
// This test pins BOTH sides: the outer residency (a derived-JOIN outer stays
// 0AF00, the reach gap that masks it), and the inner name-model constructs
// (JOIN, aggregate) answering CORRECT values with the inner rooted fresh —
// plus the StrictSingle cardinality guard, which must never silently pick
// one row.
func TestFDB_CorrelatedScalarSubqueryEnclosure(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_4051_cleanlift"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cl4051_tmpl "+
		"CREATE TABLE t (tid BIGINT NOT NULL, k BIGINT, PRIMARY KEY (tid)) "+
		"CREATE TABLE a (aid BIGINT NOT NULL, k BIGINT, PRIMARY KEY (aid)) "+
		"CREATE TABLE b (bid BIGINT NOT NULL, bref BIGINT, PRIMARY KEY (bid)) "+
		"CREATE TABLE c (cid BIGINT NOT NULL, ck BIGINT, cv BIGINT, PRIMARY KEY (cid)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, eref BIGINT, PRIMARY KEY (eid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cl4051_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 100), (2, 200)")
	mustExec(t, db, ctx, "INSERT INTO a VALUES (1, 100), (2, 200)")
	mustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1), (20, 2)")
	mustExec(t, db, ctx, "INSERT INTO c VALUES (1, 100, 7), (2, 200, 8)")
	mustExec(t, db, ctx, "INSERT INTO e VALUES (10, 1)")

	rowsOf := func(t *testing.T, q string) ([]string, error) {
		rows, qerr := db.QueryContext(ctx, q)
		if qerr != nil {
			return nil, qerr
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			cols, _ := rows.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if serr := rows.Scan(ptrs...); serr != nil {
				return nil, serr
			}
			got = append(got, fmt.Sprintf("%v", vals))
		}
		if rerr := rows.Err(); rerr != nil {
			return nil, rerr
		}
		sort.Strings(got)
		return got, nil
	}
	wantRows := func(t *testing.T, q string, want []string) {
		t.Helper()
		got, err := rowsOf(t, q)
		if err != nil {
			t.Fatalf("query must plan+run: %v\n  sql: %s", err, q)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("rows = %v, want %v\n  sql: %s", got, want, q)
		}
	}

	// INNER name-model constructs, rooted FRESH — CORRECT values.
	// t.k=100 → c.cid=1(ck=100) JOIN e(eref=1) → cv=7; t.k=200 → c.cid=2(ck=200)
	// JOIN e → no e match → NULL. A wrong-direction inner treatment (un-gathered /
	// mis-bound) would change these, so actual values are pinned (not mere identity).
	t.Run("inner_join_scalar_correct", func(t *testing.T) {
		wantRows(t, "SELECT t.k, (SELECT c.cv FROM c JOIN e ON e.eref = c.cid WHERE c.ck = t.k) FROM t ORDER BY t.k",
			[]string{"[100 7]", "[200 <nil>]"})
	})
	t.Run("inner_aggregate_over_join_correct", func(t *testing.T) {
		wantRows(t, "SELECT t.k, (SELECT MAX(c.cv) FROM c JOIN e ON e.eref = c.cid WHERE c.ck = t.k) FROM t ORDER BY t.k",
			[]string{"[100 7]", "[200 <nil>]"})
	})

	// OUTER decomposition: single-source / plain-join / derived-single outers plan
	// correctly; the derived-JOIN outer (the retired producer's residency) declines
	// 0AF00 — the reach gap that masks it, unchanged by the lift.
	t.Run("outer_plain_table", func(t *testing.T) {
		wantRows(t, "SELECT a.k, (SELECT c.cv FROM c WHERE c.ck = a.k) FROM a ORDER BY a.k",
			[]string{"[100 7]", "[200 8]"})
	})
	t.Run("outer_derived_single", func(t *testing.T) {
		wantRows(t, "SELECT d.k, (SELECT c.cv FROM c WHERE c.ck = d.k) FROM (SELECT k FROM a) d ORDER BY d.k",
			[]string{"[100 7]", "[200 8]"})
	})
	t.Run("outer_derived_join_declines_0AF00", func(t *testing.T) {
		_, err := rowsOf(t, "SELECT d.k, (SELECT c.cv FROM c WHERE c.ck = d.k) "+
			"FROM (SELECT a.k FROM a JOIN b ON b.bref = a.aid) d ORDER BY d.k")
		if err == nil {
			t.Fatalf("derived-JOIN outer: expected the 0AF00 reach-gap decline (unchanged by the lift), got no error")
		}
		requireSQLSTATE(t, err, api.ErrCodeUnsupportedQuery)
	})

	// A WHERE-EXISTS inside a correlated-scalar inner declines at the FRONT-END
	// (0A000, no SubqueryPlanner) — it never reaches the translator enclosure,
	// so this path's inner-EXISTS handling is untested. Pinned so a future
	// front-end change that admits this shape forces a re-verification of the
	// inner path above.
	t.Run("inner_where_exists_front_end_declines", func(t *testing.T) {
		_, err := rowsOf(t, "SELECT t.k, (SELECT c.cv FROM c WHERE c.ck = t.k "+
			"AND EXISTS (SELECT 1 FROM e WHERE e.eref = c.cid)) FROM t")
		if err == nil {
			t.Fatalf("inner WHERE-EXISTS scalar unexpectedly planned — re-verify the inner path (translateSubqueryRef enclosure)")
		}
		requireSQLSTATE(t, err, api.ErrCodeUnsupportedOperation)
	})

	// StrictSingle: a second e row for c.cid=1 makes the inner JOIN scalar return TWO
	// rows for t.k=100 → cardinality violation. The fresh-rooted inner must still
	// ERROR (21000), never silently pick one row.
	t.Run("inner_join_scalar_two_rows_strict_single", func(t *testing.T) {
		mustExec(t, db, ctx, "INSERT INTO e VALUES (11, 1)")
		_, err := rowsOf(t, "SELECT t.k, (SELECT c.cv FROM c JOIN e ON e.eref = c.cid WHERE c.ck = t.k) FROM t WHERE t.k = 100")
		if err == nil {
			t.Fatalf("two-inner-row scalar: expected a 21000 cardinality violation, got no error (silent pick)")
		}
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})
}
