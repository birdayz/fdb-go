package sqldriver_test

// Regression: a CASE expression used as a comparison operand in a CROSS-TABLE
// predicate (ON or WHERE over a join) returned WRONG ROWS. Root cause: a CASE
// WHEN condition (`a.x > 5`) is lowered to an opaque predicateValue wrapping a
// QueryPredicate, whose Children() returned empty — so the column refs inside the
// WHEN (a.x) were invisible to values.WalkValue. PushFilterBelowJoinRule's
// predicateSingleSide then mis-classified `CASE(a.x) = c.y` as referencing only
// the c side and pushed it below the join onto Scan(c), where a.x is unbound →
// the WHEN evaluated against a stale binding → wrong filtering. Fixed by exposing
// the wrapped predicate's operand values via predicateValue.Children().

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_CaseCrossTablePredicate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_case_xtab")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_case_xtab")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE case_xtab "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_case_xtab/s WITH TEMPLATE case_xtab")
	dsn := fmt.Sprintf("fdbsql:///testdb_case_xtab?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, y) VALUES (50, 5), (51, 99)")

	cases := []struct {
		name string
		q    string
		want []string
	}{
		{
			// CASE(a.x) with c.y in the THEN, compared to c.y, in an ON clause.
			"on_case_a_ref_then_cy",
			"SELECT a.id, c.id FROM a JOIN c ON CASE WHEN a.x > 5 THEN c.y ELSE 5 END = c.y",
			[]string{"1|50", "2|50", "2|51"},
		},
		{
			// CASE(a.x) with a constant THEN, compared to c.y, in a WHERE over a
			// comma cross join. a1→5=c.y→c50; a2→99=c.y→c51.
			"where_case_a_const_then",
			"SELECT a.id, c.id FROM a, c WHERE CASE WHEN a.x > 5 THEN 99 ELSE 5 END = c.y",
			[]string{"1|50", "2|51"},
		},
		{
			// CASE on the c side, compared to a.x. c50→ELSE 5=a.x→a1; c51→THEN 10=a.x→a2.
			"on_case_c_ref_eq_ax",
			"SELECT a.id, c.id FROM a JOIN c ON CASE WHEN c.y > 50 THEN 10 ELSE 5 END = a.x",
			[]string{"1|50", "2|51"},
		},
		{
			// Reversed operand order: c.y = CASE(a.x).
			"on_reversed_cy_eq_case",
			"SELECT a.id, c.id FROM a, c WHERE c.y = CASE WHEN a.x > 5 THEN 99 ELSE 5 END",
			[]string{"1|50", "2|51"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.q)
			if err != nil {
				t.Fatalf("query %q: %v", tc.q, err)
			}
			got := siScanRows(t, rows)
			if !eqStrSlices(got, tc.want) {
				t.Errorf("%s rows = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
