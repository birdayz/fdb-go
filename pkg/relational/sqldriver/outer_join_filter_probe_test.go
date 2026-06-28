package sqldriver_test

// Probes for the subtle LEFT-OUTER semantics around predicate placement:
//   - a filter in the ON clause keeps the row null-extended;
//   - the SAME filter in WHERE on the null-supplying side drops the
//     null-extended row (NULL fails the comparison) — effectively inner;
//   - `WHERE nullside.col IS NULL` is the anti-join;
//   - a WHERE on the preserved side filters but keeps null-extension.
// These are textbook engine-correctness traps.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OuterJoinFilterProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_oj_filter")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_oj_filter")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE oj_filter "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_oj_filter/s WITH TEMPLATE oj_filter")
	dsn := fmt.Sprintf("fdbsql:///testdb_oj_filter?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: (1,5),(2,10),(3,7); b: a1→{v8,v3}, a2→{v20}, a3→none.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10), (3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id, v) VALUES (100, 1, 8), (101, 1, 3), (102, 2, 20)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return siScanRows(t, rows)
	}
	cases := []struct {
		name string
		q    string
		want []string
	}{
		{
			"plain_left", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id",
			[]string{"1|100", "1|101", "2|102", "3|NULL"},
		},
		{
			"filter_in_on_keeps_nullextend", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id AND b.v > 5",
			[]string{"1|100", "2|102", "3|NULL"},
		},
		{
			"filter_in_where_drops_null", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id WHERE b.v > 5",
			[]string{"1|100", "2|102"},
		},
		{
			"anti_join_is_null", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id WHERE b.id IS NULL",
			[]string{"3|NULL"},
		},
		{
			"where_on_preserved_keeps_extend", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id WHERE a.x > 5",
			[]string{"2|102", "3|NULL"},
		},
		{
			"on_and_where_combined", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id AND b.v > 5 WHERE a.x >= 5",
			[]string{"1|100", "2|102", "3|NULL"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pairs(tc.q)
			if !eqStrSlices(got, tc.want) {
				t.Errorf("%s rows = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
