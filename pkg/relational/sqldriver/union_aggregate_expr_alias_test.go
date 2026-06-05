package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_UnionAggregateExprAlias is the RFC-079 (RFC-078 follow-up b) regression:
// a UNION whose branches project a POST-AGGREGATE EXPRESSION with an alias (e.g.
// COUNT(*)+1 AS x), read downstream BY NAME, must return the expression values —
// not NULL.
//
// Root cause: UNION branches are built by the legacy buildSelectShell path, whose
// post-aggregate projection used nil aliases (logical.NewProject(op, allProj, nil)),
// so a branch's `COUNT(*)+1 AS x` lost its `x` alias — the branch row was keyed only
// by the expression text `(COUNT(*)+1)`, never `X`. The outer Project([X]) then read
// a missing key → NULL. (RFC-078 fixed the BARE aggregate alias case by reading the
// alias off the StreamingAgg's AggregateSpec; the EXPRESSION case has a Project on top
// whose alias was the dropped one — a distinct root cause.) The modern visitSelectGroupBy
// path applied the alias; the fix extracts the projection-building loop into one shared
// helper (buildPostAggregateProjection) so both builders carry the alias.
func TestFDB_UnionAggregateExprAlias(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_union_expralias")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_union_expralias")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE union_expralias_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_union_expralias/s WITH TEMPLATE union_expralias_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_union_expralias?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 0), (2, 0)")            // count(a) = 2 → +1 = 3
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 0), (20, 0), (30, 0)") // count(b) = 3 → +1 = 4

	// (1) The core regression: mismatched-alias post-aggregate EXPRESSION branches,
	// projected by the first branch's name → both expression values, no NULL.
	// Was [NULL, NULL] on master.
	assertInt64Set(t, db, ctx,
		"SELECT u.x FROM (SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b) u",
		[]int64{3, 4})

	// (2) ORDER BY the first-branch expression column → correct values in order; the
	// sort key must resolve to a real value on every branch (not NULL).
	assertInt64Ordered(t, db, ctx,
		"SELECT x FROM (SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b) u ORDER BY x",
		[]int64{3, 4})

	// (3) GROUP BY + post-aggregate expression with mismatched aliases → the expression
	// flows on every branch. a/g=0 count=2 → 12; b/g=0 count=3 → 13.
	assertInt64Set(t, db, ctx,
		"SELECT u.s FROM (SELECT g, COUNT(*)+10 AS s FROM a GROUP BY g UNION ALL SELECT g, COUNT(*)+10 AS t FROM b GROUP BY g) u",
		[]int64{12, 13})

	// (4) Same-named expression branches still work (alias remap is a no-op here) — no
	// regression to the common case.
	assertInt64Set(t, db, ctx,
		"SELECT u.x FROM (SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS x FROM b) u",
		[]int64{3, 4})
}
