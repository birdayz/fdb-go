package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_LateralSpineLinksAndCorrelatedLegs executes, on real FDB, the
// shapes a lateral-unnest spine and a FROM of mutually correlated legs gained,
// each with the rows the Java target answers
// (conformance/ws_f_join_unnest_conformance_test.go measures them all):
//
//   - a chain over a block's first FROM item (its Explode is the chain's
//     bottom, whose element the next item explodes);
//   - sibling unnests of one table, and of an enclosing row;
//   - a WHERE reading the element of the link under a spine's tip, whose
//     binding also names the spine's merged row — planned before, but
//     malformed at EXECUTION ("leg B carries DIVERGENT baked types"), which is
//     why this is an FDB test and not a plan pin;
//   - lateral derived legs correlated to other legs, planned inside the leg
//     they read, including a LEFT JOIN's null-supplying leg;
//   - an unnest behind a later table inside a derived table that is itself
//     a join leg (the leg's gate reads the rotated cluster);
//   - sibling unnests over a two-table bottom, in either table order;
//   - a lateral derived leg reading an AT ordinal or a mid-link element
//     (a clean refusal before the AT pair's declared names were bound to the
//     Explode's positional _0/_1, then a runtime type mismatch).
func TestFDB_LateralSpineLinksAndCorrelatedLegs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/lateral_spine_links"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE lateral_spine_links_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TYPE AS STRUCT b (k BIGINT, tags BIGINT ARRAY)" +
			" CREATE TABLE q (id BIGINT, bs b ARRAY, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE lateral_spine_links_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO w VALUES (1, 1, [10, 11]), (2, 2, [20]), (3, 3, [])",
		"INSERT INTO h VALUES (1, 10)",
		"INSERT INTO q VALUES (1, [(1, [7, 8]), (2, [9])])",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	for _, tc := range []struct {
		sql  string
		want []string
	}{
		// A chain over a first FROM item.
		{`SELECT id FROM q WHERE EXISTS (SELECT t FROM q.bs AS b, b.tags AS t WHERE t = 9)`, []string{"[1]"}},
		{`SELECT d.t FROM q, (SELECT t FROM q.bs AS b, b.tags AS t) AS d`, []string{"[7]", "[8]", "[9]"}},
		{`SELECT d.t, d.o, d.p FROM q, (SELECT t, o, p FROM q.bs AS b AT o, b.tags AS t AT p) AS d`, []string{"[7 1 1]", "[8 1 2]", "[9 2 1]"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, b.tags AS t, h WHERE t + 1 = h.f)`, []string{"[1]"}},
		// Sibling unnests.
		{`SELECT v, v2 FROM w, w.arr AS v, w.arr AS v2`, []string{"[10 10]", "[10 11]", "[11 10]", "[11 11]", "[20 20]"}},
		{`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, w.arr AS v2 WHERE v2 = v + 1)`, []string{"[1]"}},
		{`SELECT x.k, x2.k, y FROM q, q.bs AS x, q.bs AS x2, x.tags AS y`, []string{"[1 1 7]", "[1 1 8]", "[1 2 7]", "[1 2 8]", "[2 1 9]", "[2 2 9]"}},
		{`SELECT v, v2 FROM w, h, w.arr AS v, w.arr AS v2`, []string{"[10 10]", "[10 11]", "[11 10]", "[11 11]", "[20 20]"}},
		{`SELECT v, v2, h.f FROM h, w, w.arr AS v, w.arr AS v2`, []string{"[10 10 10]", "[10 11 10]", "[11 10 10]", "[11 11 10]", "[20 20 10]"}},
		// A WHERE reading the element under the tip.
		{`SELECT t FROM q, q.bs AS b, b.tags AS t WHERE t > b.k + 6`, []string{"[8]", "[9]"}},
		{`SELECT t, o FROM q, q.bs AS b AT o, b.tags AS t WHERE t > o + 6`, []string{"[8 1]", "[9 2]"}},
		{`SELECT v, p, v2 FROM w, w.arr AS v AT p, w.arr AS v2 WHERE v2 > v`, []string{"[10 1 11]"}},
		// An unnest behind a later table inside a derived leg.
		{`SELECT d.b, d.f FROM w, (SELECT b.k AS b, h.f FROM q, q.bs AS b, h) AS d`, []string{"[1 10]", "[1 10]", "[1 10]", "[2 10]", "[2 10]", "[2 10]"}},
		{`SELECT d.t, d.f FROM q, (SELECT t, h.f FROM q.bs AS b, b.tags AS t, h) AS d`, []string{"[7 10]", "[8 10]", "[9 10]"}},
		{`SELECT d.t FROM w, (SELECT t FROM q, q.bs AS b, b.tags AS t, h WHERE t + 1 = h.f) AS d`, []string{"[9]", "[9]", "[9]"}},
		// A lateral derived leg reading an AT ordinal or a mid-link element.
		{`SELECT d.id FROM w, w.arr AS v AT p, (SELECT h.id FROM h WHERE h.f = p + 9) AS d`, []string{"[1]", "[1]"}},
		{`SELECT d.x FROM w, w.arr AS v AT p, (SELECT p + h.f AS x FROM h) AS d`, []string{"[11]", "[11]", "[12]"}},
		{`SELECT d.id FROM q, q.bs AS b, b.tags AS t, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d`, []string{"[1]"}},
		{`SELECT d.id FROM q, q.bs AS b AT o, b.tags AS t, (SELECT h.id FROM h WHERE h.f = o + 9) AS d`, []string{"[1]", "[1]"}},
		// Lateral legs correlated to other legs.
		{`SELECT e.k FROM w, (SELECT v AS k FROM w.arr AS v) AS d, (SELECT d.k AS k FROM h) AS e`, []string{"[10]", "[11]", "[20]"}},
		{`SELECT e.k FROM w, h, (SELECT w.f + h.f AS k FROM h AS h2) AS e`, []string{"[11]", "[12]", "[13]"}},
		{`SELECT d.k, e.k FROM w, (SELECT v AS k FROM w.arr AS v) AS d, (SELECT w.f AS k FROM h) AS e`, []string{"[10 1]", "[11 1]", "[20 2]"}},
		{`SELECT w.id FROM w LEFT JOIN h ON h.id = w.id WHERE h.id IS NULL AND NOT EXISTS (SELECT 1 FROM q WHERE q.id = h.id)`, []string{"[2]", "[3]"}},
		{`SELECT w.id, d.x FROM w LEFT JOIN h ON h.id = w.id, (SELECT h.f AS x FROM q) AS d`, []string{"[1 10]", "[2 <nil>]", "[3 <nil>]"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
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
