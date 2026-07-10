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
	// row-preserving, NOT unconditionally-one-row, so queryScopeHasWindowedAggregate
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

	// P1 (mixed polarity): a positive AlwaysTrue esq beside a NEGATED one must
	// NOT partial-fold (that left the broken negated residual [2 3] where the
	// truth is empty). The fold DECLINES the whole filter, preserving the base
	// behavior (this two-quantifier shape is rejected upstream) — never the
	// silent-wrong [2 3].
	t.Run("mixed_polarity_declines", func(t *testing.T) {
		got, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) AND NOT EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id")
		if qErr != nil {
			return // rejected (base behavior) — acceptable, NOT silent-wrong
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
		if len(v) == 2 && v[0] == 2 && v[1] == 3 {
			t.Fatalf("mixed_polarity: partial-folded to the broken negated residual [2 3] (must decline)")
		}
	})

	// A positive literal LIMIT n>=1 keeps the single aggregate row, so it MUST
	// still fold to always-true (the finer guard folds sq.limit>=1). A blanket
	// LIMIT-clause decline would wrongly drop it to [1].
	t.Run("limit_1_folds", func(t *testing.T) {
		eq(t, "limit_1_folds", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("limit_5_folds", func(t *testing.T) {
		eq(t, "limit_5_folds", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 5) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// Unparsed LIMIT (LIMIT 0.0 / 0L): parseLimitClause can't ParseInt these and
	// silently leaves the -1 no-limit sentinel — a SEPARATE PRE-EXISTING bug
	// (standalone `... LIMIT 0.0` returns ALL rows; correct is []). The fold's
	// finer guard (LIMIT clause present && sq.limit < 1) correctly DECLINES to
	// fold it always-true; the declined fallback answers [1] (the pre-existing
	// correlated-drop behavior), which is itself the residual (correct []). This
	// pins the exact residual as a flip-sentinel: it flips when the booked
	// parseLimitClause-reject lands (then LIMIT 0.0 rejects or enforces 0). Not
	// laundered — the [1] is documented as pre-existing, NOT accepted as correct.
	t.Run("limit_unparsed_residual_not_always_true", func(t *testing.T) {
		for _, lim := range []string{"LIMIT 0.0", "LIMIT 0L"} {
			v := ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id "+lim+") ORDER BY p.id")
			if len(v) == 3 {
				t.Fatalf("%s: wrongly FOLDED to always-true [1 2 3] (unparsed LIMIT must decline)", lim)
			}
			if len(v) != 1 || v[0] != 1 {
				t.Fatalf("%s: pre-existing residual changed from [1] to %v — parseLimitClause-reject may have landed; assert the corrected rows", lim, v)
			}
		}
	})

	// A cleanly-parsed positive OFFSET (`OFFSET 2`) skips the single aggregate
	// row, AND a syntactically-accepted-but-unparseable OFFSET (`OFFSET 1.0` /
	// `OFFSET 1L`, both valid decimalLiterals that ParseInt rejects) leaves
	// sq.offset silently 0 — so reading sq.offset alone would wrongly FOLD these
	// to always-true [1 2 3] (the bug: a positive LIMIT paired with an
	// unverifiable OFFSET). limitClauseKeepsSingleRow declines on either — a
	// present-but-unparseable atom OR a nonzero offset. All fall to the
	// pre-existing residual [1] (flip-sentinel: flips when parseLimitClause-reject
	// lands, then OFFSET>0 on a one-row aggregate is []).
	t.Run("offset_declines_not_always_true", func(t *testing.T) {
		for _, off := range []string{"LIMIT 1 OFFSET 2", "LIMIT 1 OFFSET 1.0", "LIMIT 1 OFFSET 1L"} {
			v := ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id "+off+") ORDER BY p.id")
			if len(v) == 3 {
				t.Fatalf("%s: wrongly FOLDED to always-true [1 2 3] (a row-skipping/unverifiable OFFSET must decline)", off)
			}
			if len(v) != 1 || v[0] != 1 {
				t.Fatalf("%s: pre-existing residual changed from [1] to %v — parseLimitClause-reject may have landed; assert corrected rows", off, v)
			}
		}
	})

	// POSITIVE CONTROL: OFFSET 0 is a clean no-op — the single row survives, so a
	// LIMIT 1 OFFSET 0 aggregate is still unconditionally TRUE and MUST fold. Pins
	// that the offset guard rejects only unverifiable/row-skipping offsets, not a
	// legitimate zero.
	t.Run("offset_0_folds", func(t *testing.T) {
		eq(t, "offset_0_folds", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 0) ORDER BY p.id"), []int64{1, 2, 3})
	})
}
