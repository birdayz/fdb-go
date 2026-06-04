package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_JoinMerge_OuterColumn_NotDropped is the E2E regression sentinel for
// REVIEW.md #216. composeFieldOverJoinMerge canonicalizes a bare FieldValue over
// a binary JoinMergeAllValue to the merge's INNER quantifier (Aliases[1]). That is
// sound only because SelectMergeRule re-flows the merge under the inner alias, so only
// inner-side fields are ever composed onto it (outer/third-table columns resolve
// via their own QOV + the merge's qualified keys). This test pins that structural
// invariant from the SQL surface: projecting and filtering an OUTER-side column
// across a multi-way join must return the correct values, never NULL. If the
// merge's alias convention ever drifts so an outer-only column gets composed onto
// the merge and blindly rewritten to the inner side, these rows go NULL and this
// test fails — converting a latent landmine into a caught regression.
//
// Shapes covered (the axis the prior suite missed — every existing join test
// projected/filtered INNER or join-key columns): outer-only column projection in
// both FROM-orders; a spanning predicate that forces the upper level to touch the
// lower join's OUTER table; and a WHERE filter on the outer-only column.
func TestFDB_JoinMerge_OuterColumn_NotDropped(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_jm_outer")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_jm_outer")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE jm_outer_tmpl "+
			// a <- b <- c chain. apay/cpay are OUTER-only payload columns (not join
			// keys), the exact shape composeFieldOverJoinMerge must not mis-resolve.
			"CREATE TABLE a (aid BIGINT NOT NULL, apay STRING, PRIMARY KEY (aid)) "+
			"CREATE TABLE b (bid BIGINT NOT NULL, b_aid BIGINT, PRIMARY KEY (bid)) "+
			"CREATE TABLE c (cid BIGINT NOT NULL, c_bid BIGINT, c_aid BIGINT, cpay STRING, PRIMARY KEY (cid)) "+
			"CREATE INDEX b_by_a ON b (b_aid) "+
			"CREATE INDEX c_by_b ON c (c_bid) "+
			"CREATE INDEX c_by_a ON c (c_aid)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_jm_outer/s WITH TEMPLATE jm_outer_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_jm_outer?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// a:2 rows; b links each to a; c links each to b AND back to a (spanning FK).
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 'apay1')")
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (2, 'apay2')")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (11, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 10, 1, 'cpay1')")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (101, 11, 2, 'cpay2')")

	chain := "b.b_aid = a.aid AND c.c_bid = b.bid"

	// (1) Project the OUTER-only column a.apay across the 3-way join, both
	// FROM-orders. Expect exactly the two outer payloads, no NULLs.
	for _, q := range []string{
		"SELECT a.apay FROM a, b, c WHERE " + chain,
		"SELECT a.apay FROM c, b, a WHERE " + chain,
	} {
		assertStringSet(t, db, ctx, q, []string{"apay1", "apay2"})
	}

	// (2) Spanning predicate c.c_aid = a.aid forces the upper join level to touch
	// the lower join's OUTER table (a). Still project the outer-only column.
	assertStringSet(t, db, ctx,
		"SELECT a.apay FROM a, b, c WHERE "+chain+" AND c.c_aid = a.aid",
		[]string{"apay1", "apay2"})

	// (3) Filter ON the outer-only column — it must resolve to its real value, not
	// NULL, for the predicate to select correctly.
	assertStringSet(t, db, ctx,
		"SELECT a.apay FROM a, b, c WHERE "+chain+" AND a.apay = 'apay1'",
		[]string{"apay1"})

	// (4) Project a third-table outer-only column (c.cpay) alongside — the merge
	// must not drop it either.
	assertStringSet(t, db, ctx,
		"SELECT c.cpay FROM a, b, c WHERE "+chain,
		[]string{"cpay1", "cpay2"})
}

// assertStringSet runs q and asserts the single-column string results equal want
// as a set (order-independent), with no NULLs.
func assertStringSet(t *testing.T, db *sql.DB, ctx context.Context, q string, want []string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		if !v.Valid {
			t.Fatalf("query %q returned a NULL outer column — join_merge dropped an outer-side field (REVIEW.md #216)", q)
		}
		got = append(got, v.String)
	}
	sort.Strings(got)
	exp := append([]string(nil), want...)
	sort.Strings(exp)
	if strings.Join(got, ",") != strings.Join(exp, ",") {
		t.Errorf("query %q: got %v, want %v", q, got, exp)
	}
}
