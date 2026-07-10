package sqldriver_test

// TestFDB_ExistsOverNonGroupedAggregate pins the CONSTANT-FOLD fix for a
// PRE-EXISTING silent-wrong: a correlated positive WHERE-EXISTS whose inner is a
// NON-GROUPED aggregate (COUNT(*)/MAX/SUM, no GROUP BY / HAVING / QUALIFY /
// LIMIT 0 / OFFSET / windowed) is UNCONDITIONALLY TRUE. The fold drops the
// existential quantifier (EXISTS -> TRUE) rather than wrapping/semi-joining, so
// a JOINED OUTER source works too (the wrap approach regressed it to 0AF00).

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
	dbPath := "/testdb_exists_agg_fold"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE eagf_tmpl"+
		" CREATE TABLE p (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"+
		" CREATE TABLE g (gid BIGINT, PRIMARY KEY (gid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE eagf_tmpl"); err != nil {
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
		"INSERT INTO g VALUES (901)",
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

	// THE FIX (single outer): positive WHERE-EXISTS over a correlated non-grouped
	// aggregate is always TRUE -> all p.
	t.Run("count_star", func(t *testing.T) {
		eq(t, "count_star", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("max", func(t *testing.T) {
		eq(t, "max", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT MAX(e.eid) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("sum", func(t *testing.T) {
		eq(t, "sum", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT SUM(e.eid) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// THE JOINED-OUTER CASE (P1#5, the wrap approach regressed this to 0AF00):
	// the fold drops the quantifier, so no semi-join / translateJoinWithExists.
	t.Run("joined_outer_cross", func(t *testing.T) {
		eq(t, "joined_outer_cross", ids(t, "SELECT p.id FROM p, g WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("joined_outer_with_conjunct", func(t *testing.T) {
		eq(t, "joined_outer_with_conjunct", ids(t, "SELECT p.id FROM p, g WHERE g.gid = 901 AND EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// CONTROLS: not always-true / not folded — must still filter correctly.
	t.Run("grouped_still_filters", func(t *testing.T) {
		eq(t, "grouped_still_filters", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id GROUP BY e.eref) ORDER BY p.id"), []int64{1})
	})
	t.Run("plain_exists_still_filters", func(t *testing.T) {
		eq(t, "plain_exists_still_filters", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1})
	})
	t.Run("uncorrelated_count", func(t *testing.T) {
		eq(t, "uncorrelated_count", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e) ORDER BY p.id"), []int64{1, 2, 3})
	})
	// A non-EXISTS conjunct alongside the folded EXISTS must survive (P AND TRUE == P).
	t.Run("other_conjunct_survives", func(t *testing.T) {
		eq(t, "other_conjunct_survives", ids(t, "SELECT p.id FROM p WHERE p.id > 1 AND EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{2, 3})
	})

	// GUARD: a WINDOWED aggregate (COUNT(*) OVER ()) is
	// row-preserving, NOT unconditionally-one-row, so queryOuterHasWindowedAggregate
	// keeps it from being flagged AlwaysTrue — it is NOT folded. Folding it would
	// wrongly yield all-p; instead the engine cleanly REJECTS the unsupported
	// windowed aggregate (0AF00), never a silent-wrong.
	t.Run("windowed_not_folded", func(t *testing.T) {
		_, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) OVER () FROM e WHERE e.eref = p.id) ORDER BY p.id")
		if qErr == nil {
			t.Fatalf("windowed aggregate EXISTS planned — it must NOT be folded to always-true (row-preserving); expected the unsupported-windowed rejection")
		}
	})

	// RESIDUAL (booked): NOT EXISTS(always-true) is FALSE (empty), but the fold
	// only handles POSITIVE consumers, so a negated one keeps the pre-existing
	// behavior — [2 3] here (row-existence semantics: p 2,3 have no e), where the
	// strictly-correct answer is []. Flip-sentinel: when NOT-EXISTS folding lands
	// this flips to []. Pinned so the residual is explicit, not silent.
	t.Run("not_exists_residual", func(t *testing.T) {
		eq(t, "not_exists_residual", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{2, 3})
	})
}
