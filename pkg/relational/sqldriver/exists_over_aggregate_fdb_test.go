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
// The last three subtests cover the edge shapes that made an earlier attempt
// (reverted) unsound: LIMIT 0 declines, mixed predicate declines, and the
// nested-scope case (now correct because the aggregate-detection scope leak is
// closed — its detector no longer misclassifies a row-preserving middle SELECT
// as an aggregate).

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

	// LIMIT 0 eliminates the single aggregate row → NOT always-true; the
	// detector declines (guard held → not the newly-broken all-p [1 2 3]).
	t.Run("limit_zero_declines", func(t *testing.T) {
		got := ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 0) ORDER BY p.id")
		if len(got) == 3 {
			t.Fatalf("limit_zero: LIMIT 0 was wrongly wrapped always-true (rows=%v)", got)
		}
	})

	// A row-preserving MIDDLE SELECT projecting an EXISTS-of-aggregate must NOT
	// be misclassified as an aggregate. This was the misfire that hard-blocked
	// the fix; the aggregate-detection scope-leak fix closes it at the source
	// (harvestAggregates no longer promotes the nested COUNT(*) into the middle's
	// aggregate set). Middle yields one row per matching e → EXISTS = has-e-match
	// → only p.id=1.
	t.Run("nested_scope_correct", func(t *testing.T) {
		eq(t, "nested_scope",
			ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT EXISTS (SELECT COUNT(*) FROM f) FROM e WHERE e.eref = p.id) ORDER BY p.id"),
			[]int64{1})
	})

	// A predicate with an outer-only conjunct (`p.id > 0`) sets
	// OuterOnlyJoinConjuncts → the wrap DECLINES (never strands an inner ref
	// above the aggregate → always-false []). Guard held (not empty).
	t.Run("mixed_predicate_declines", func(t *testing.T) {
		got := ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id AND p.id > 0) ORDER BY p.id")
		if len(got) == 0 {
			t.Fatalf("mixed_predicate: wrapped with the inner ref stranded above the aggregate → always-false []")
		}
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
