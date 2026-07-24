package sqldriver_test

// TestFDB_ExistsOverNonGroupedAggregate pins the cardinality fold for a
// correlated WHERE-EXISTS whose inner is a NON-GROUPED aggregate. COUNT/MAX/SUM
// produce exactly one row before pagination; a literal LIMIT/OFFSET then either
// preserves or removes that row. The fold substitutes the resulting boolean
// instead of wrapping/semi-joining, so joined outer sources and both
// EXISTS/NOT-EXISTS polarities keep exact SQL semantics.

import (
	"context"
	"database/sql"
	"testing"

	"fdb.dev/pkg/relational/api"
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
		" CREATE TABLE g (gid BIGINT, PRIMARY KEY (gid))"+
		" CREATE TABLE d (id BIGINT, x BIGINT, PRIMARY KEY (id))"); err != nil {
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
		// Only p.id=1 has correlated rows. Two matches are load-bearing for the
		// OFFSET 1 regression below: applying OFFSET to raw rows would leave one
		// and answer TRUE, while SQL aggregates those rows to one result first
		// and then skips that single aggregate row (FALSE).
		"INSERT INTO e VALUES (301, 1), (302, 1)",
		"INSERT INTO g VALUES (901)",
		"INSERT INTO d VALUES (1, 0), (2, 0), (3, 0)",
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
	// keeps KnownTruth unset — it is NOT folded. Folding it would
	// wrongly yield all-p; instead the engine cleanly REJECTS the unsupported
	// windowed aggregate (0AF00), never a silent-wrong.
	t.Run("windowed_not_folded", func(t *testing.T) {
		_, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) OVER () FROM e WHERE e.eref = p.id) ORDER BY p.id")
		if qErr == nil {
			t.Fatalf("windowed aggregate EXISTS planned — it must NOT be folded to always-true (row-preserving); expected the unsupported-windowed rejection")
		}
	})

	// The same known cardinality naturally inverts under NOT EXISTS: the
	// non-grouped aggregate always emits one row, so NOT EXISTS is FALSE.
	t.Run("not_exists_known_false", func(t *testing.T) {
		eq(t, "not_exists_known_false", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), nil)
	})

	// Mixed polarity is now exact rather than partially folded:
	// TRUE AND NOT(TRUE) is FALSE.
	t.Run("mixed_polarity_folds_false", func(t *testing.T) {
		eq(t, "mixed_polarity_folds_false", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) AND NOT EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) ORDER BY p.id"), nil)
	})

	// A positive literal LIMIT n>=1 keeps the single aggregate row, so it MUST
	// still fold to TRUE (the cardinality classifier proves the row survives).
	t.Run("limit_1_folds", func(t *testing.T) {
		eq(t, "limit_1_folds", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("limit_5_folds", func(t *testing.T) {
		eq(t, "limit_5_folds", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 5) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// Invalid LIMIT literal (LIMIT 0.0 / 0L): a decimalLiteral that is not a
	// valid integer is now REJECTED with a loud 42601 syntax error, never
	// silently dropped. Before the parseLimitClause-reject, `... LIMIT 0.0`
	// left the -1 no-limit sentinel and returned ALL rows (standalone) / the
	// raw correlated-row path (this EXISTS shape). The reject lands at
	// plan time, so QueryContext returns the error directly.
	t.Run("limit_invalid_literal_rejected", func(t *testing.T) {
		for _, lim := range []string{"LIMIT 0.0", "LIMIT 0L"} {
			rows, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id "+lim+") ORDER BY p.id")
			if qErr == nil {
				rows.Close()
				t.Fatalf("%s: expected a 42601 syntax error (LIMIT must be an integer literal), got a successful query", lim)
			}
			requireSQLSTATE(t, qErr, api.ErrCodeSyntaxError)
		}
	})

	// SQL operator order is aggregate first, pagination second. Every query
	// below has exactly one aggregate row before pagination, regardless of the
	// two raw matches for p.id=1; LIMIT 0 or any positive OFFSET removes that
	// aggregate row, so EXISTS is FALSE for every p.
	t.Run("limit_zero_after_aggregate", func(t *testing.T) {
		eq(t, "limit_zero_after_aggregate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 0) ORDER BY p.id"), nil)
	})
	t.Run("offset_one_after_aggregate", func(t *testing.T) {
		eq(t, "offset_one_after_aggregate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), nil)
	})
	t.Run("max_offset_one_after_aggregate", func(t *testing.T) {
		eq(t, "max_offset_one_after_aggregate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT MAX(e.eid) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), nil)
	})
	t.Run("sum_offset_one_after_aggregate", func(t *testing.T) {
		eq(t, "sum_offset_one_after_aggregate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT SUM(e.eid) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), nil)
	})
	t.Run("offset_one_with_larger_limit", func(t *testing.T) {
		eq(t, "offset_one_with_larger_limit", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 5 OFFSET 1) ORDER BY p.id"), nil)
	})
	t.Run("offset_two_after_aggregate", func(t *testing.T) {
		eq(t, "offset_two_after_aggregate", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 2) ORDER BY p.id"), nil)
	})
	t.Run("not_exists_inverts_empty_page", func(t *testing.T) {
		eq(t, "not_exists_inverts_empty_page", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("max_not_exists_inverts_empty_page", func(t *testing.T) {
		eq(t, "max_not_exists_inverts_empty_page", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT MAX(e.eid) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("sum_not_exists_inverts_empty_page", func(t *testing.T) {
		eq(t, "sum_not_exists_inverts_empty_page", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT SUM(e.eid) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), []int64{1, 2, 3})
	})
	t.Run("other_conjunct_and_known_false", func(t *testing.T) {
		eq(t, "other_conjunct_and_known_false", ids(t, "SELECT p.id FROM p WHERE p.id > 1 AND EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), nil)
	})
	t.Run("known_true_and_ordinary_exists", func(t *testing.T) {
		eq(t, "known_true_and_ordinary_exists", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id) AND EXISTS (SELECT 1 FROM e WHERE e.eref = p.id) ORDER BY p.id"), []int64{1})
	})
	t.Run("joined_outer_empty_page", func(t *testing.T) {
		eq(t, "joined_outer_empty_page", ids(t, "SELECT p.id FROM p, g WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) ORDER BY p.id"), nil)
	})

	// The arity>=3 gathered-cluster projection fold is a distinct early-return
	// path. It must perform the same known-truth substitution before building
	// the gathered EXISTS wrap. The two raw rows for p.id=1 make the false case
	// discriminate aggregate-before-OFFSET from raw-row pagination; the no-page
	// twin proves known TRUE does not degrade to raw existence either.
	t.Run("arity_three_gathered_known_false", func(t *testing.T) {
		eq(t, "arity_three_gathered_known_false", ids(t,
			"SELECT p.id FROM p, g AS g1, g AS g2 WHERE EXISTS ("+
				"SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1"+
				") ORDER BY p.id"), nil)
	})
	t.Run("arity_three_gathered_known_true", func(t *testing.T) {
		eq(t, "arity_three_gathered_known_true", ids(t,
			"SELECT p.id FROM p, g AS g1, g AS g2 WHERE EXISTS ("+
				"SELECT COUNT(*) FROM e WHERE e.eref = p.id"+
				") ORDER BY p.id"), []int64{1, 2, 3})
	})

	// Invalid OFFSET literal (OFFSET 1.0 / 1L): same reject as an invalid LIMIT
	// literal — a non-integer decimalLiteral in OFFSET is 42601, not a silent
	// offset=0. Before the reject these left sq.offset=0, and reading it alone
	// would wrongly fold `LIMIT 1 OFFSET 1.0` to always-true.
	t.Run("offset_invalid_literal_rejected", func(t *testing.T) {
		for _, off := range []string{"LIMIT 1 OFFSET 1.0", "LIMIT 1 OFFSET 1L"} {
			rows, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id "+off+") ORDER BY p.id")
			if qErr == nil {
				rows.Close()
				t.Fatalf("%s: expected a 42601 syntax error (OFFSET must be an integer literal), got a successful query", off)
			}
			requireSQLSTATE(t, qErr, api.ErrCodeSyntaxError)
		}
	})

	// POSITIVE CONTROL: OFFSET 0 is a clean no-op — the single row survives, so a
	// LIMIT 1 OFFSET 0 aggregate is still unconditionally TRUE and MUST fold. Pins
	// that the offset guard rejects only unverifiable/row-skipping offsets, not a
	// legitimate zero.
	t.Run("offset_0_folds", func(t *testing.T) {
		eq(t, "offset_0_folds", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 0) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// UNCORRELATED controls already carry the real Aggregate -> Limit pipeline;
	// they must agree with the correlated cardinality fold.
	t.Run("uncorrelated_offset_false", func(t *testing.T) {
		eq(t, "uncorrelated_offset_false", ids(t, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e LIMIT 1 OFFSET 1) ORDER BY p.id"), nil)
	})
	t.Run("uncorrelated_not_exists_true", func(t *testing.T) {
		eq(t, "uncorrelated_not_exists_true", ids(t, "SELECT p.id FROM p WHERE NOT EXISTS (SELECT COUNT(*) FROM e LIMIT 1 OFFSET 1) ORDER BY p.id"), []int64{1, 2, 3})
	})

	// GROUP BY + positive OFFSET has data-dependent post-group cardinality. The
	// correlated fallback cannot carry that pagination yet, so it must reject
	// typed-loud instead of applying OFFSET to raw rows or ignoring it.
	t.Run("grouped_offset_rejected", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx, "SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id GROUP BY e.eid LIMIT 1 OFFSET 1) ORDER BY p.id")
		if qErr == nil {
			rows.Close()
			t.Fatal("grouped correlated EXISTS with OFFSET planned; expected typed unsupported rejection")
		}
		requireSQLSTATE(t, qErr, api.ErrCodeUnsupportedQuery)
	})

	// The SQL driver substitutes bound parameters before parsing, so production
	// sees a literal and classifies its exact post-pagination cardinality.
	t.Run("bound_limit_semantics", func(t *testing.T) {
		eq(t, "bound_limit_zero", idsWithArg(t, db, ctx,
			"SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT ?) ORDER BY p.id", 0), nil)
		eq(t, "bound_limit_one", idsWithArg(t, db, ctx,
			"SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT ?) ORDER BY p.id", 1), []int64{1, 2, 3})
		eq(t, "bound_offset_zero", idsWithArg(t, db, ctx,
			"SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET ?) ORDER BY p.id", 0), []int64{1, 2, 3})
		eq(t, "bound_offset_one", idsWithArg(t, db, ctx,
			"SELECT p.id FROM p WHERE EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET ?) ORDER BY p.id", 1), nil)
	})

	// Projected and JOIN-ON consumers do not yet have constant Value/ON-marker
	// substitution. KnownTruth must therefore reject them typed-loud; neither is
	// allowed to fall through to the raw-row semi-join.
	t.Run("projected_known_truth_rejected", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx, "SELECT p.id, EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1) FROM p")
		if qErr == nil {
			rows.Close()
			t.Fatal("projected cardinality-known EXISTS planned; expected typed unsupported rejection")
		}
		requireSQLSTATE(t, qErr, api.ErrCodeUnsupportedQuery)
	})
	t.Run("join_on_known_truth_rejected", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx, "SELECT p.id FROM p JOIN g ON g.gid = 901 AND EXISTS (SELECT COUNT(*) FROM e WHERE e.eref = p.id LIMIT 1 OFFSET 1)")
		if qErr == nil {
			rows.Close()
			t.Fatal("JOIN ON cardinality-known EXISTS planned; expected typed unsupported rejection")
		}
		requireSQLSTATE(t, qErr, api.ErrCodeUnsupportedQuery)
	})

	// DML must share SELECT's parse-tree window-aggregate guard. The mixed
	// global/window projection is deliberately correlated to d: ids 2 and 3
	// have empty raw input, so letting DML lower it through the raw EXISTS
	// fallback would silently target a different row set.
	t.Run("dml_windowed_aggregate_exists_rejected", func(t *testing.T) {
		res, execErr := db.ExecContext(ctx,
			"UPDATE d SET x = 9 WHERE EXISTS ("+
				"SELECT COUNT(*), COUNT(*) OVER () FROM e "+
				"WHERE e.eref = d.id LIMIT 1 OFFSET 0)")
		if execErr == nil {
			affected, rowsErr := res.RowsAffected()
			t.Fatalf("windowed aggregate DML EXISTS planned: affected=%d rowsErr=%v; want SQLSTATE %s",
				affected, rowsErr, api.ErrCodeUnsupportedQuery)
		}
		requireSQLSTATE(t, execErr, api.ErrCodeUnsupportedQuery)
	})

	// DML WHERE routes through the same filter consumer. Pin FALSE, negated
	// FALSE, and TRUE so UPDATE/DELETE cannot bypass the cardinality fold.
	t.Run("dml_where_known_truth", func(t *testing.T) {
		res, execErr := db.ExecContext(ctx,
			"UPDATE d SET x = 1 WHERE EXISTS ("+
				"SELECT COUNT(*) FROM e WHERE e.eref = d.id LIMIT 1 OFFSET 1)")
		if execErr != nil {
			t.Fatalf("UPDATE known FALSE: %v", execErr)
		}
		if affected, err := res.RowsAffected(); err != nil || affected != 0 {
			t.Fatalf("UPDATE known FALSE affected = %d, err=%v; want 0", affected, err)
		}

		res, execErr = db.ExecContext(ctx,
			"UPDATE d SET x = 2 WHERE NOT EXISTS ("+
				"SELECT COUNT(*) FROM e WHERE e.eref = d.id LIMIT 1 OFFSET 1)")
		if execErr != nil {
			t.Fatalf("UPDATE NOT known FALSE: %v", execErr)
		}
		if affected, err := res.RowsAffected(); err != nil || affected != 3 {
			t.Fatalf("UPDATE NOT known FALSE affected = %d, err=%v; want 3", affected, err)
		}

		res, execErr = db.ExecContext(ctx,
			"DELETE FROM d WHERE EXISTS ("+
				"SELECT COUNT(*) FROM e WHERE e.eref = d.id)")
		if execErr != nil {
			t.Fatalf("DELETE known TRUE: %v", execErr)
		}
		if affected, err := res.RowsAffected(); err != nil || affected != 3 {
			t.Fatalf("DELETE known TRUE affected = %d, err=%v; want 3", affected, err)
		}
	})
}

func idsWithArg(t *testing.T, db *sql.DB, ctx context.Context, query string, arg any) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, arg)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}
