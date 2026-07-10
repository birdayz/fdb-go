package sqldriver_test

// TestFDB_ExistsOverNonGroupedAggregate pins the fix for a PRE-EXISTING
// silent-wrong: a CORRELATED EXISTS whose inner SELECT is a NON-GROUPED
// aggregate (`COUNT(*)`/`MAX`/`SUM` with no GROUP BY, no HAVING) is
// UNCONDITIONALLY TRUE — such a subquery always produces exactly one row even
// over an empty post-WHERE input (COUNT→0, MAX/SUM→NULL). Java 4.12.11.0 keeps
// every outer row; Go dropped the non-matching ones because the correlated
// EXISTS fallback rebuilds the inner from FROM+WHERE and drops the SELECT list,
// killing the aggregate that forces one-row cardinality (fix: wrap the rebuilt
// inner in a trivial COUNT(*) so the semi-join always matches).
//
// Controls prove the fix is SURGICAL, not a blanket "aggregate → always true":
//   - GROUPED aggregate emits ZERO rows over an empty group → must still filter.
//   - UNCORRELATED aggregate was already correct (full inner plan keeps the agg).
//   - a plain `SELECT 1` EXISTS must still filter.

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
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"); err != nil {
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

	// CONTROL: GROUPED aggregate emits ZERO rows over an empty group → must
	// still filter to the matching outer rows only.
	t.Run("grouped_still_filters", func(t *testing.T) {
		eq(t, "grouped_still_filters", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id GROUP BY e.eref) ORDER BY p.id"), []int64{1})
	})

	// CONTROL: a plain SELECT 1 EXISTS must still filter (row existence).
	t.Run("plain_exists_still_filters", func(t *testing.T) {
		eq(t, "plain_exists_still_filters", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1})
	})

	// PROJECTED position: EXISTS(agg) in the SELECT list is blocked by a SEPARATE
	// PRE-EXISTING bug — the outer query's aggregate detection descends into the
	// projected EXISTS subquery, sees the COUNT(*), and wrongly flags the OUTER
	// query as an aggregate → 42803 "P.ID must appear in GROUP BY". This is
	// orthogonal to the cardinality fix above and is independent of it: the
	// UNCORRELATED projected EXISTS-of-aggregate (which never reaches the
	// correlated fallback / this fix) 42803s identically. Sentinel: this FLIPS
	// (query succeeds, all-TRUE) when that separate parse-time bug is fixed,
	// forcing the author to assert the corrected rows. See TODO.md.
	t.Run("projected_separate_preexisting_42803", func(t *testing.T) {
		_, qErr := db.QueryContext(ctx, "SELECT p.id, EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) FROM p ORDER BY p.id")
		if qErr == nil {
			t.Fatalf("projected EXISTS-of-aggregate now PLANS — the separate outer-aggregate-detection bug is fixed; update this to assert all-TRUE rows [[1 true][2 true][3 true]]")
		}
		if got := qErr.Error(); !contains42803(got) {
			t.Fatalf("projected: want the pre-existing 42803, got %v", got)
		}
	})
}

func contains42803(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if s[i:i+5] == "42803" {
			return true
		}
	}
	return false
}
