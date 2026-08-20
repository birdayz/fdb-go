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
			"CREATE TABLE a (id BIGINT, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, y BIGINT, lo BIGINT, hi BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_xtab_probe/s WITH TEMPLATE xtab_probe")
	dsn := fmt.Sprintf("fdbsql:///testdb_xtab_probe?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10), (3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, y, lo, hi) VALUES (50, 5, 1, 6), (51, 99, 8, 12)")

	// A cross-table IN-list (`x IN (col, col)`) in an ON clause. This shape has
	// been through all three states and the history is why it is asserted by
	// ROW COUNT rather than by "it works":
	//
	//	1. silently DROPPED from the ON clause -> the join became a CROSS PRODUCT
	//	2. fail-closed -> a clean 0AF00, because expr.ResolveIn took constants only
	//	3. planned -> a non-constant list is an ArrayConstructorValue compared
	//	   per row, so the predicate applies
	//
	// State 1 and state 3 both "return something", so the only assertion that
	// separates them is the row set. a=(1,5),(2,10),(3,7) and c=(50,y=5,lo=1),
	// (51,y=99,lo=8): only a=1 satisfies `x IN (y, lo)`, against c=50. The full
	// cross product is 6 rows, so a 6-row answer is state 1 returning.
	t.Run("in_list_cross_applies_the_predicate", func(t *testing.T) {
		const onQ = "SELECT a.id, c.id FROM a JOIN c ON a.x IN (c.y, c.lo) ORDER BY a.id, c.id"
		got, err := mmRows(t, ctx, db, onQ)
		if err != nil {
			t.Fatalf("a cross-table IN in an ON clause failed to plan: %v", err)
		}
		want := []string{"1|50"}
		if !mmEqRows(got, want) {
			t.Fatalf("cross-table IN in an ON clause is wrong\n  got  %v\n  want %v\n"+
				"  (6 rows is the join's full cross product — the ON predicate dropped)",
				got, want)
		}
		// The WHERE spelling of the same inner join must agree; a predicate that
		// applies in one and not the other is the drop wearing a different hat.
		gotWhere, err := mmRows(t, ctx, db,
			"SELECT a.id, c.id FROM a, c WHERE a.x IN (c.y, c.lo) ORDER BY a.id, c.id")
		if err != nil {
			t.Fatalf("the WHERE spelling failed to plan: %v", err)
		}
		if !mmEqRows(gotWhere, want) {
			t.Fatalf("ON and WHERE spellings disagree\n  ON   : %v\n  WHERE: %v", got, gotWhere)
		}
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
