package sqldriver_test

// Adversarial probes for CROSS-TABLE predicates with complex operands (BETWEEN,
// IN-list, nested CASE, CASE under a LEFT JOIN). These exercise the correlation
// detection that PushFilterBelowJoinRule (and the join planner) rely on; a
// mis-classified cross-table predicate would be pushed to one leg → wrong rows.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_CrossTablePredicateProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_xtab_probe")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_xtab_probe")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE xtab_probe "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, y BIGINT, lo BIGINT, hi BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_xtab_probe/s WITH TEMPLATE xtab_probe")
	dsn := fmt.Sprintf("fdbsql:///testdb_xtab_probe?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10), (3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, y, lo, hi) VALUES (50, 5, 1, 6), (51, 99, 8, 12)")

	// Non-constant IN-list (`x IN (col, col)`) is a constant-only limitation in
	// Go (expr.ResolveIn) — rejected in BOTH WHERE and ON. Verify the ON form is
	// rejected CLEANLY (the Phase 1 fail-closed gate turned the prior silent ON
	// drop → cross product into a clean error). NOT a wrong-rows case.
	t.Run("in_list_cross_rejected", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN c ON a.x IN (c.y, c.lo)")
	})

	cases := []struct {
		name string
		q    string
		want []string
	}{
		{
			// cross-table BETWEEN: a.x in [c.lo, c.hi].
			"between_cross",
			"SELECT a.id, c.id FROM a JOIN c ON a.x BETWEEN c.lo AND c.hi",
			[]string{"1|50", "2|51"},
		},
		{
			// nested CASE cross-table: a.x>6 → c.y else c.lo; compared to c.lo.
			// a1(5)≤6→c.lo=c.lo always true → (1,50),(1,51). a2/a3>6→c.y; c.y=c.lo
			// never (5≠1, 99≠8).
			"nested_case_cross",
			"SELECT a.id, c.id FROM a JOIN c ON CASE WHEN a.x > 6 THEN c.y ELSE c.lo END = c.lo",
			[]string{"1|50", "1|51"},
		},
		{
			// CASE under a LEFT JOIN ON (pushdown rule is INNER-only, so the CASE
			// stays on the NLJ): every a null-extends (no c matches).
			"case_left_join_on",
			"SELECT a.id, c.id FROM a LEFT JOIN c ON CASE WHEN a.x > 5 THEN a.x ELSE 0 END = c.y",
			[]string{"1|NULL", "2|NULL", "3|NULL"},
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
