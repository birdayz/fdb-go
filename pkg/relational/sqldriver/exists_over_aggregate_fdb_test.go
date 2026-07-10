package sqldriver_test

// TestFDB_ExistsOverNonGroupedAggregate pins the fix for a PRE-EXISTING
// silent-wrong: a CORRELATED EXISTS whose inner SELECT is a NON-GROUPED
// aggregate (COUNT(*)/MAX/SUM, no GROUP BY / HAVING / row-eliminating
// LIMIT/OFFSET) is UNCONDITIONALLY TRUE — the subquery always produces exactly
// one row even over an empty post-WHERE input. Java 4.12.11.0 keeps every outer
// row; Go dropped the non-matching ones because the correlated EXISTS fallback
// rebuilds the inner from FROM+WHERE and drops the SELECT list, killing the
// aggregate that forces one-row cardinality.
//
// The edge subtests cover the shapes that made an earlier attempt (reverted)
// unsound plus adversarial correlation-wiring shapes: the nested-scope case
// (now correct because the aggregate-detection scope leak is closed); a
// single-level mixed predicate (WRAPS — no decline, the outer-only conjunct is
// pushed below upstream); LIMIT 0 / QUALIFY (the detector DECLINES the
// always-true wrap — the [1] fallback is a booked separate residual); and
// multi-table-inner / LIMIT-1 / wrapped-aggregate shapes that must all stay
// always-true.

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_ExistsOverNonGroupedAggregate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_exists_nongrouped_agg"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE enga_tmpl"+
		" CREATE TABLE p (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"+
		" CREATE TABLE f (fid BIGINT, PRIMARY KEY (fid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE enga_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"INSERT INTO p VALUES (1), (2), (3)",
		"INSERT INTO e VALUES (301, 1)", // only p.id=1 has a correlated e row
		"INSERT INTO f VALUES (900)",
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	ids := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query %q: %v", q, qErr)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if sErr := rows.Scan(&id); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got = append(got, id)
		}
		return got
	}
	eq := func(t *testing.T, name string, got, want []int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: rows=%v want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: rows=%v want %v", name, got, want)
			}
		}
	}

	// THE FIX: non-grouped correlated aggregate EXISTS is always TRUE → all p.
	t.Run("count_star", func(t *testing.T) {
		eq(t, "count_star", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("max", func(t *testing.T) {
		eq(t, "max", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT MAX(e.eid) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("sum", func(t *testing.T) {
		eq(t, "sum", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT SUM(e.eid) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	// NOT EXISTS of an always-true → always FALSE → no rows.
	t.Run("not_exists_count", func(t *testing.T) {
		eq(t, "not_exists_count", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), nil)
	})

	// CONTROL: uncorrelated non-grouped aggregate — was already correct.
	t.Run("uncorrelated_count", func(t *testing.T) {
		eq(t, "uncorrelated_count", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e) ORDER BY p.id"), []int64{1, 2, 3})
	})
	// CONTROL: GROUPED aggregate emits ZERO rows over an empty group → filters.
	t.Run("grouped_still_filters", func(t *testing.T) {
		eq(t, "grouped_still_filters", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id GROUP BY e.eref) ORDER BY p.id"), []int64{1})
	})
	// CONTROL: a plain SELECT 1 EXISTS must still filter (row existence).
	t.Run("plain_exists_still_filters", func(t *testing.T) {
		eq(t, "plain_exists_still_filters", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1})
	})

	// A row-preserving MIDDLE SELECT projecting an EXISTS-of-aggregate must NOT
	// be misclassified as an aggregate. Middle yields one row per matching e →
	// EXISTS = has-e-match → only p.id=1.
	t.Run("nested_scope_correct", func(t *testing.T) {
		eq(t, "nested_scope",
			ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT EXISTS (SELECT COUNT(*) FROM f) FROM e WHERE e.eref = p.id) ORDER BY p.id"),
			[]int64{1})
	})

	// A single-level mixed predicate (`e.eref=p.id AND p.id>0`) does NOT decline:
	// the outer-only conjunct `p.id>0` is split out and pushed INSIDE the inner
	// (below the future aggregate) upstream, leaving the join predicate
	// inner-only, so the wrap fires and the aggregate always emits → all p that
	// satisfy p.id>0 = [1 2 3]. (The `!OuterOnlyJoinConjuncts` guard is for a
	// DIFFERENT shape — a nested-EXISTS-middle — not this one.)
	t.Run("mixed_predicate_wraps", func(t *testing.T) {
		eq(t, "mixed_predicate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id AND p.id > 0) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// LIMIT 0 / QUALIFY can eliminate the single aggregate row, so the detector
	// DECLINES the always-true wrap (guards on sq.limit / sq.qualifyExpr). The
	// declined fallback (drop-the-SELECT semi-join) then answers [1] (row
	// existence), which is itself a SEPARATE pre-existing residual — the
	// strictly-correct answer is [] (LIMIT 0 / QUALIFY-false → zero rows → EXISTS
	// false). What THIS test pins is that the guard held: the result is [1], NOT
	// the always-true [1 2 3] the unguarded wrap would produce. The [1]→[]
	// residual is booked separately (drop-SELECT loses LIMIT/QUALIFY).
	t.Run("limit_zero_declines_guard_held", func(t *testing.T) {
		eq(t, "limit_zero", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 0) ORDER BY p.id"), []int64{1})
	})
	t.Run("qualify_declines_guard_held", func(t *testing.T) {
		got, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id QUALIFY ROW_NUMBER() OVER (ORDER BY e.eid) < 1) ORDER BY p.id")
		if qErr != nil {
			t.Fatalf("qualify: %v", qErr)
		}
		var v []int64
		for got.Next() {
			var x int64
			if sErr := got.Scan(&x); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			v = append(v, x)
		}
		got.Close()
		// Guard held: [1] (declined fallback), NOT the always-true [1 2 3].
		if len(v) != 1 || v[0] != 1 {
			t.Fatalf("qualify: rows=%v want [1] (guard declined the always-true wrap; [1 2 3] = QUALIFY not guarded)", v)
		}
	})

	// Adversarial correlation-wiring shapes (verified against a review probe):
	// each is a non-grouped aggregate → always one row → always TRUE → all p.
	// multi-table inner exercises the correlation-below-aggregate wiring over a
	// join; LIMIT 1 exercises the limit>=1 boundary (wraps, unlike LIMIT 0);
	// wrapped COALESCE(SUM(...)) exercises aggregate detection through a wrapping
	// function.
	t.Run("multi_table_inner", func(t *testing.T) {
		eq(t, "multi_table_inner", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e, f WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("limit_1_wraps", func(t *testing.T) {
		eq(t, "limit_1_wraps", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 0) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("wrapped_aggregate", func(t *testing.T) {
		eq(t, "wrapped_aggregate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COALESCE(SUM(e.eid), 0) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// PROJECTED position: EXISTS(agg) in the SELECT list is TRUE for every row
	// (the scope-leak fix removed the outer-42803 that used to block it).
	t.Run("projected_all_true", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx, "SELECT p.id, EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) FROM p ORDER BY p.id")
		if qErr != nil {
			t.Fatalf("projected: %v", qErr)
		}
		defer rows.Close()
		var got []struct {
			id int64
			ex bool
		}
		for rows.Next() {
			var id int64
			var ex sql.NullBool
			if sErr := rows.Scan(&id, &ex); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got = append(got, struct {
				id int64
				ex bool
			}{id, ex.Valid && ex.Bool})
		}
		want := []struct {
			id int64
			ex bool
		}{{1, true}, {2, true}, {3, true}}
		if len(got) != len(want) {
			t.Fatalf("projected: got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("projected: got %v want %v", got, want)
			}
		}
	})
}
