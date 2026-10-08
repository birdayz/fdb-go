package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_LateralLeftJoinOfTwoCorrelatedLegs pins a LEFT JOIN inside a lateral
// derived table whose two legs both read the enclosing row. Neither leg
// provides that row, so the join's only implementation is a FlatMap evaluated
// under it. Every ON conjunct must filter the null-supplying leg BELOW the
// null-extension: filtered above it, a preserved row whose inner is non-empty
// but matches nothing is dropped (INNER rows); filtered on the preserved leg,
// the row itself is dropped. RewriteOuterJoinRule rewrites every outer join,
// as Java's does, whatever its ON reads; the LEFT OUTER select it rewrites stays
// a member, and its own lowering places the conjuncts the same way. The same
// shapes run against the Java engine in conformance's ws_f_join_unnest spec.
func TestFDB_LateralLeftJoinOfTwoCorrelatedLegs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/lateral_left_join_correlated_legs"
	setup := testkit.OpenDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE lateral_left_join_correlated_legs_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (k BIGINT, v BIGINT, PRIMARY KEY (k))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE lateral_left_join_correlated_legs_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+testkit.ClusterFile()+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO w VALUES (1, 1), (2, 2), (3, 3)",
		"INSERT INTO h VALUES (1, 10), (2, 20), (3, 30)",
		"INSERT INTO g VALUES (1, 0), (2, 0), (3, 0), (5, 0)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// a is h[w.id]; b reads w too.
	a := `(SELECT h.id FROM h WHERE h.f = w.f * 10) AS a`
	lateral := func(b, on string) string {
		return `SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM ` + a +
			` LEFT JOIN ` + b + ` ON ` + on + `) AS d`
	}
	// b holds g[k >= w.id]; a.id + 1 is 2, 3 and 4, and g has no 4, so w3's
	// preserved row meets a non-empty inner that matches nothing.
	mixed := lateral(`(SELECT g.k FROM g WHERE g.k >= w.id) AS b`, `a.id + 1 = b.k`)
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{mixed, []string{"[1 1 2]", "[2 2 3]", "[3 3 <nil>]"}},
		// b is g[w.id + 2]: non-empty for w1 and w3, never matching a.id.
		{lateral(`(SELECT g.k FROM g WHERE g.k = w.id + 2) AS b`, `a.id = b.k`), []string{"[1 1 <nil>]", "[2 2 <nil>]", "[3 3 <nil>]"}},
		// Control: every preserved row matches.
		{lateral(`(SELECT g.k FROM g WHERE g.k = w.id) AS b`, `a.id = b.k`), []string{"[1 1 1]", "[2 2 2]", "[3 3 3]"}},
		// An ON that does not read the preserved leg still filters below the
		// null-extension. b is g[w.id + 2]: g3 for w1 (3 > 1 keeps it), g5 for
		// w3; w2 has none.
		{lateral(`(SELECT g.k FROM g WHERE g.k = w.id + 2) AS b`, `b.k > 1`), []string{"[1 1 3]", "[2 2 <nil>]", "[3 3 5]"}},
		{lateral(`(SELECT g.k FROM g WHERE g.k = w.id + 2) AS b`, `1 = 1`), []string{"[1 1 3]", "[2 2 <nil>]", "[3 3 5]"}},
		// An ON reading only the preserved leg null-extends the rows it
		// rejects instead of dropping them.
		{lateral(`(SELECT g.k FROM g WHERE g.k = w.id + 2) AS b`, `a.id > 1`), []string{"[1 1 <nil>]", "[2 2 <nil>]", "[3 3 5]"}},
		{
			`SELECT w.id FROM w WHERE EXISTS (SELECT 1 FROM ` + a + ` LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON a.id + 1 = b.k WHERE b.k IS NULL)`,
			[]string{"[3]"},
		},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			if got := testkit.SortedRowStrings(t, db, ctx, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+mixed).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	if !strings.Contains(plan, "DefaultOnEmpty(Map(PredicatesFilter(") {
		t.Errorf("the ON conjunct must filter below the null-extension: %s", plan)
	}
}
