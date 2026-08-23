package sqldriver_test

// The bare-star rewrite that hides the __ROW_VERSION pseudo-field only ever
// handled an all-base-table FROM. Every other shape took an early return, and
// that return is not neutral: it leaves the star PROJECTION-LESS, so the base
// leg's scan row — which carries the appended pseudo-slot at run time — flows
// out as the statement's output row. Measured before the fix:
//
//	SELECT * FROM t3, (SELECT id AS did, b FROM t4) d  -> COLS [ID A __ROW_VERSION DID B]
//	SELECT * FROM t5, t5.arr AS x                      -> COLS [ID ARR __ROW_VERSION X]
//
// Java has no such hole because star expansion projects nonEphemeralVisible()
// uniformly over every source regardless of kind (SemanticAnalyzer.java:346-348)
// — the star is ALWAYS an explicit projection, so there is no "skip the rewrite"
// path for the ephemeral to escape through.
//
// The same function also uppercased the source alias when writing the qualified
// projection. A QUOTED alias is case-sensitive, so `AS "l"` became `L` and the
// projection referenced a qualifier that does not exist:
//
//	SELECT * FROM t3 AS "l", t4 AS "r"  -> ERROR 42703: column reference with qualifier "L" cannot be resolved
//
// Unquoted aliases arrive already uppercased, so the conversion was never doing
// anything except breaking the quoted case.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_RowVersionBareStar_MixedSourcesAndQuotedAliases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rvstar")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rvstar")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE rvstar_tpl
		CREATE TABLE t3(id BIGINT, a BIGINT, PRIMARY KEY(id))
		CREATE TABLE t4(id BIGINT, b BIGINT, PRIMARY KEY(id))
		CREATE TABLE t5(id BIGINT, arr BIGINT ARRAY, PRIMARY KEY(id))
		WITH OPTIONS(store_row_versions=true)`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rvstar/s1 WITH TEMPLATE rvstar_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_rvstar?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t3 VALUES (1, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t4 VALUES (1, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t5 VALUES (1, [7, 8])")

	cases := []struct {
		name string
		q    string
		want []string
	}{
		// The shapes that leaked. Each has a BASE leg (which carries the
		// ephemeral) beside a non-base leg (which used to abort the rewrite).
		{
			"base_join_derived", `SELECT * FROM t3, (SELECT id AS did, b FROM t4) d`,
			[]string{"ID", "A", "DID", "B"},
		},
		{
			"base_join_lateral_unnest", `SELECT * FROM t5, t5.arr AS x`,
			[]string{"ID", "ARR", "X"},
		},
		// The quoted-alias shape, which failed 42703 outright.
		{
			"quoted_aliases", `SELECT * FROM t3 AS "l", t4 AS "r"`,
			[]string{"ID", "A", "ID", "B"},
		},
		// A non-base leg's COLUMN names are case-sensitive for exactly the same
		// reason its alias is, and the first fix here preserved the alias while
		// still re-folding the columns — leaving the defect half-present.
		// `t5.arr AS "x"` resolved its alias verbatim and then asked for `x.X`.
		//
		// THESE TWO NOW REPORT THE AUTHORED SPELLING, and that is the change:
		// the star's projection resolved the quoted name once the rewrite kept
		// the alias, but the LABEL it published was still folded, so the user
		// saw DID and X for columns called did and x. Nothing folds an output
		// name any more, so the reported label is the one the query wrote.
		{
			"quoted_derived_column", `SELECT * FROM t3, (SELECT id AS "did" FROM t4) d`,
			[]string{"ID", "A", "did"},
		},
		{
			"quoted_unnest_alias", `SELECT * FROM t5, t5.arr AS "x"`,
			[]string{"ID", "ARR", "x"},
		},
		{
			"unquoted_derived_column", `SELECT * FROM t3, (SELECT id AS did FROM t4) d`,
			[]string{"ID", "A", "DID"},
		},
		// The shapes that already worked — a fix that rewrote the alias
		// handling or the source walk could easily have broken these.
		{"single_base", `SELECT * FROM t3`, []string{"ID", "A"}},
		{"two_base", `SELECT * FROM t3, t4`, []string{"ID", "A", "ID", "B"}},
		{
			"unquoted_aliases", `SELECT * FROM t3 AS l, t4 AS r`,
			[]string{"ID", "A", "ID", "B"},
		},
		{"single_base_with_array", `SELECT * FROM t5`, []string{"ID", "ARR"}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.q)
			if err != nil {
				t.Fatalf("%s: %v", tc.q, err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			if strings.Join(cols, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("%s\n got columns [%s]\nwant columns [%s]\n"+
					"__ROW_VERSION is EPHEMERAL — a bare star must never expose it",
					tc.q, strings.Join(cols, " "), strings.Join(tc.want, " "))
			}
			// The row must be readable into exactly that many slots: a column
			// list that agrees while the underlying row carries an extra slot
			// would be a different, worse bug.
			if !rows.Next() {
				t.Fatalf("%s returned no rows (err=%v)", tc.q, rows.Err())
			}
			slots := make([]any, len(cols))
			for i := range slots {
				slots[i] = new(any)
			}
			if err := rows.Scan(slots...); err != nil {
				t.Fatalf("%s: scanning %d columns failed — the output row does not "+
					"match the advertised column list: %v", tc.q, len(cols), err)
			}
		})
	}
}
