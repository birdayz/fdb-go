package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_DerivedStarJoinBesideAnotherSource executes, on real FDB, a derived
// table whose body is a star over a join, beside another source — every shape
// with the rows the Java target answers
// (conformance/ws_f_join_unnest_conformance_test.go measures them).
//
// Each failed planning before (XX000 "reference members disagree on result
// type"): SelectMergeRule dissolved the body into the outer join and the
// merged member re-tiled the join's row by the body's own legs, [W, G, D],
// beside the translator's [A, D]. The merge now declines when it would change
// the reference's leg table, and the nested join plans.
func TestFDB_DerivedStarJoinBesideAnotherSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/derived_star_join_beside_source"
	setup := testkit.OpenDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE derived_star_join_beside_source_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (k BIGINT, v BIGINT, PRIMARY KEY (k))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE derived_star_join_beside_source_tmpl",
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
		"INSERT INTO w VALUES (1, 1, [10, 11]), (2, 2, [20]), (3, 3, [])",
		"INSERT INTO h VALUES (1, 10)",
		"INSERT INTO g VALUES (1, 100), (3, 300)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The GROUP BY arm already answered at the merge-base; its plan is pinned
	// so a declined merge cannot silently change its join order.
	pinnedPlans := map[string]string{
		`SELECT a.k, COUNT(*) FROM (SELECT * FROM w, g) AS a, h AS d WHERE a.id = d.id GROUP BY a.k`: "FlatMap(outer=NestedLoopJoin(INNER, Scan(G), Scan(W)), inner=Scan(H, [=]))",
	}
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT d.x FROM (SELECT * FROM w, h) AS a, (SELECT h.f AS x FROM h) AS d`, []string{"[10]", "[10]", "[10]"}},
		{`SELECT d.x FROM (SELECT * FROM w, g) AS a, (SELECT h.f AS x FROM h) AS d`, []string{"[10]", "[10]", "[10]", "[10]", "[10]", "[10]"}},
		{`SELECT a.id, a.k, a.v, d.id FROM (SELECT * FROM w, g) AS a, h AS d WHERE d.id = a.k`, []string{"[1 1 100 1]", "[2 1 100 1]", "[3 1 100 1]"}},
		{`SELECT a.f, a.v, d.f FROM (SELECT * FROM w, g) AS a, h AS d WHERE d.id = a.id AND a.k > 1`, []string{"[1 300 10]"}},
		{`SELECT e.id, a.id, a.k FROM h AS e, (SELECT * FROM w, g) AS a WHERE e.id = a.k AND a.id = e.id`, []string{"[1 1 1]"}},
		{`WITH a AS (SELECT * FROM w, g) SELECT a.id, a.v, d.f FROM a, h AS d WHERE a.k = d.id`, []string{"[1 100 10]", "[2 100 10]", "[3 100 10]"}},
		{`SELECT a.k, COUNT(*) FROM (SELECT * FROM w, g) AS a, h AS d WHERE a.id = d.id GROUP BY a.k`, []string{"[1 1]", "[3 1]"}},
		{`SELECT a.id, d.id FROM (SELECT * FROM w, g) AS a LEFT JOIN h AS d ON d.id = a.k`, []string{"[1 1]", "[1 <nil>]", "[2 1]", "[2 <nil>]", "[3 1]", "[3 <nil>]"}},
		// Two such derived tables joined: at the merge-base an execution error
		// with no SQLSTATE (the leg typed by the name model's qualified keys,
		// a row no plan emits).
		{`SELECT a.id, a.k, d.k FROM (SELECT * FROM w, g) AS a, (SELECT * FROM w, g) AS d WHERE a.k = d.id AND a.id = d.k`, []string{"[1 1 1]", "[1 3 1]", "[3 1 3]", "[3 3 3]"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			if pinned, ok := pinnedPlans[tc.sql]; ok {
				var plan string
				if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.sql).Scan(&plan); err != nil {
					t.Fatalf("EXPLAIN: %v", err)
				}
				if !strings.Contains(plan, pinned) {
					t.Errorf("plan %s does not contain %s", plan, pinned)
				}
			}
			rows, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				cells := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, fmt.Sprint(cells))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_FilteredDerivedTableInsideAJoin pins a star derived table over ONE
// filtered table as a leg of a join. The join's row states its legs by the
// derived table's alias (A, D); merging the derived body into the join renames
// that leg to the body's own table (W, D), and the join's reference then held
// two members stating different leg tables, which fails planning outright
// ("reference members disagree on result type") — at the merge-base even for
// the bare two-leg cross product. The merge is declined; the rows are the
// target's (the same shapes run against the Java engine in conformance's
// ws_f_join_unnest spec).
func TestFDB_FilteredDerivedTableInsideAJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/filtered_derived_table_inside_a_join"
	setup := testkit.OpenDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE filtered_derived_table_inside_a_join_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (k BIGINT, v BIGINT, PRIMARY KEY (k))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE filtered_derived_table_inside_a_join_tmpl",
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
		"INSERT INTO g VALUES (1, 100), (2, 200), (3, 300)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// a keeps w2 and w3.
	a := `(SELECT * FROM w WHERE w.f > 1) AS a`
	// The GROUP BY arm already planned at the merge-base (no lower pairs a with
	// a sibling under a star row); its plan is pinned so the declined merge
	// cannot silently change its join order (w, then h, then g). The chain
	// nests in the inner, as Java's correlated FlatMaps do.
	pinnedPlans := map[string]string{
		`SELECT e.k, COUNT(*) FROM ` + a + `, h AS d, g AS e WHERE a.id = d.id AND d.id >= e.k GROUP BY e.k`: "FlatMap(outer=PredicatesFilter(Scan(W), [1 preds]), inner=FlatMap(outer=Scan(H, [=]), inner=Scan(G, [<>])))",
	}
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT a.id, d.id FROM ` + a + `, h AS d`, []string{"[2 1]", "[2 2]", "[2 3]", "[3 1]", "[3 2]", "[3 3]"}},
		{`SELECT a.id, d.id FROM ` + a + `, h AS d WHERE a.id = d.id`, []string{"[2 2]", "[3 3]"}},
		{`SELECT a.id, d.id FROM ` + a + `, h AS d, g AS e WHERE a.id = d.id AND d.id = e.k`, []string{"[2 2]", "[3 3]"}},
		{`SELECT a.id, a.f, d.id FROM ` + a + `, h AS d, g AS e WHERE a.id = d.id AND d.id = e.k`, []string{"[2 2 2]", "[3 3 3]"}},
		{`SELECT a.id, d.id FROM h AS d, ` + a + `, g AS e WHERE a.id = d.id AND d.id = e.k`, []string{"[2 2]", "[3 3]"}},
		{`SELECT a.id, d.id FROM ` + a + `, (SELECT * FROM h) AS d, g AS e WHERE a.id = e.k AND d.id = e.k`, []string{"[2 2]", "[3 3]"}},
		// d.f * 9 is 180 for d2 (e2, e3 pass) and 270 for d3 (e3 passes).
		{`SELECT a.id, d.id, e.k FROM ` + a + `, h AS d, g AS e WHERE a.id = d.id AND e.v > d.f * 9`, []string{"[2 2 2]", "[2 2 3]", "[3 3 3]"}},
		{`SELECT e.k, COUNT(*) FROM ` + a + `, h AS d, g AS e WHERE a.id = d.id AND d.id >= e.k GROUP BY e.k`, []string{"[1 2]", "[2 2]", "[3 1]"}},
		{`SELECT a.id, d.id FROM ` + a + ` LEFT JOIN h AS d ON d.id = a.id + 1`, []string{"[2 3]", "[3 <nil>]"}},
		// g[a.id].v is 200 for a2 (beats d.f * 9 = 90, 180) and 300 for a3.
		{
			`SELECT a.id, d.id FROM ` + a + `, h AS d WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = a.id AND z.v > d.f * 9)`,
			[]string{"[2 1]", "[2 2]", "[3 1]", "[3 2]", "[3 3]"},
		},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			if pinned, ok := pinnedPlans[tc.sql]; ok {
				var plan string
				if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.sql).Scan(&plan); err != nil {
					t.Fatalf("EXPLAIN: %v", err)
				}
				if !strings.Contains(plan, pinned) {
					t.Errorf("plan %s does not contain %s", plan, pinned)
				}
			}
			if got := testkit.SortedRowStrings(t, db, ctx, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_DerivedOuterJoinBesideAnotherSource pins a star derived table over a
// LEFT, RIGHT or FULL join as a leg beside another source. The parent join's
// seed typed that leg by the name model's qualified keys (`W.ID`, …) while the
// dissolved outer box emits its legs' columns under their own names, so a read
// that bound the leg's row failed at execution ("source binding: bound
// RECORD(W.ID, …) but read as RECORD(ID, …)", or the leg adapter's "carries
// NONE of the leg type's columns") — five of these ten arms at 00a3e35e0, the
// others planned so that no reader bound the mis-typed row; at the merge-base
// too. The leg is typed as the
// box's own row, its null-supplying columns nullable. The LEFT and RIGHT rows
// are the Java target's (conformance's ws_f_join_unnest spec); the target's
// grammar has no FULL JOIN.
func TestFDB_DerivedOuterJoinBesideAnotherSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/derived_outer_join_beside_source"
	setup := testkit.OpenDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE derived_outer_join_beside_source_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (k BIGINT, v BIGINT, PRIMARY KEY (k))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE derived_outer_join_beside_source_tmpl",
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
		"INSERT INTO h VALUES (1, 10), (3, 30)",
		"INSERT INTO g VALUES (1, 100), (3, 300), (4, 400)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// a over the LEFT join: w1–g1, w2 null-extended, w3–g3.
	left := `(SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a`
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT a.id, a.k, d.id FROM ` + left + `, h AS d WHERE d.f = 10`, []string{"[1 1 1]", "[2 <nil> 1]", "[3 3 1]"}},
		{`SELECT a.id, a.k, d.id FROM h AS d, ` + left + ` WHERE a.k IS NULL`, []string{"[2 <nil> 1]", "[2 <nil> 3]"}},
		{`SELECT a.id, a.v FROM ` + left + `, h AS d WHERE a.id >= d.id AND a.v IS NOT NULL`, []string{"[1 100]", "[3 300]", "[3 300]"}},
		{`SELECT COUNT(*), COUNT(a.k) FROM ` + left + `, h AS d`, []string{"[6 4]"}},
		{`SELECT a.id, a.v, d.f FROM ` + left + `, h AS d WHERE d.id = a.k`, []string{"[1 100 10]", "[3 300 30]"}},
		{`SELECT a.id, a.k, d.id FROM ` + left + ` LEFT JOIN h AS d ON d.id = a.k`, []string{"[1 1 1]", "[2 <nil> <nil>]", "[3 3 3]"}},
		// Two outer-body legs: b pairs w with g[w.id + 1] (w3 with g4).
		{
			`SELECT a.id, b.k FROM ` + left + `, (SELECT * FROM w LEFT JOIN g ON g.k = w.id + 1) AS b WHERE a.id = b.id`,
			[]string{"[1 <nil>]", "[2 3]", "[3 4]"},
		},
		// g[a.id].v beats d.f * 10 only for (a1, d1): 100 > 100 is false,
		// so only a3 (300) against d1 (100).
		{
			`SELECT a.id, d.id FROM ` + left + `, h AS d WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = a.id AND z.v > d.f * 10)`,
			[]string{"[3 1]"},
		},
		// RIGHT: the star order is g's columns, then w's.
		{`SELECT a.k, a.v, a.id FROM (SELECT * FROM g RIGHT JOIN w ON g.k = w.id) AS a, h AS d WHERE d.f = 10`, []string{"[1 100 1]", "[3 300 3]", "[<nil> <nil> 2]"}},
		// FULL (a Go extension): g4 has no w.
		{`SELECT a.id, a.k FROM (SELECT * FROM w FULL JOIN g ON g.k = w.id) AS a, h AS d WHERE d.f = 10`, []string{"[1 1]", "[2 <nil>]", "[3 3]", "[<nil> 4]"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			if got := testkit.SortedRowStrings(t, db, ctx, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}
