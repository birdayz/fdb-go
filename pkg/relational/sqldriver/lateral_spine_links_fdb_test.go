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
	dbPath := "/FRL/lateral_spine_links"
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
		// A first FROM item joined to two later sources, with a WHERE conjunct
		// that reads only the item and the middle source.
		{`SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h, w AS z WHERE v = h.f AND z.id = h.id)`, []string{"[1]"}},
		{`SELECT id FROM w WHERE NOT EXISTS (SELECT v FROM w.arr AS v, h, w AS z WHERE v = h.f AND z.id = h.id)`, []string{"[2]", "[3]"}},
		{`SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h, w AS z WHERE v = h.f AND z.id = h.id + 3)`, nil},
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

// TestFDB_NestedExistsOverASpineIsOneProduct executes an EXISTS that is the
// whole WHERE of an EXISTS block over a lateral-unnest spine: the builder makes
// the two one product, and the inner WHERE — reading the spine's tip element —
// is the product's predicate. Left on the inner table it made that table a
// lateral leg the spine's re-association placed below the tip (no plan, 0AF00).
// Rows are the Java target's.
func TestFDB_NestedExistsOverASpineIsOneProduct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/nested_exists_spine_product"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE nested_exists_spine_product_tmpl" +
			" CREATE TYPE AS STRUCT b (k BIGINT, tags BIGINT ARRAY)" +
			" CREATE TABLE q (id BIGINT, bs b ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE nested_exists_spine_product_tmpl",
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
		"INSERT INTO q VALUES (1, [(1, [7, 8]), (2, [9])]), (2, [(3, [11])]), (3, [(4, [20])])",
		"INSERT INTO h VALUES (1, 10), (2, 9), (3, 12)",
		"INSERT INTO g VALUES (1, 1), (2, 3)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	const spine = `SELECT t FROM q.bs AS b, b.tags AS t WHERE `
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT id FROM q WHERE EXISTS (` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1))`, []string{"1", "2"}},
		{`SELECT id FROM q WHERE NOT EXISTS (` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1))`, []string{"3"}},
		{`SELECT id FROM q WHERE EXISTS (` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1 AND h.id > 1))`, []string{"1", "2"}},
		{`SELECT id FROM q WHERE EXISTS (` + spine + `EXISTS (SELECT 1 FROM h WHERE t > 8 AND h.id = 1))`, []string{"1", "2", "3"}},
		{`SELECT id FROM q WHERE EXISTS (` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + q.id))`, []string{"1"}},
		{`SELECT id FROM q WHERE EXISTS (` + spine + `EXISTS (SELECT 1 FROM h, g WHERE h.f = t + 1 AND g.id = h.id))`, []string{"1"}},
		{`SELECT id FROM q WHERE EXISTS (` + spine + `EXISTS (SELECT 1 FROM h WHERE EXISTS (SELECT 1 FROM g WHERE g.v = b.k AND h.f = t + 1)))`, []string{"1", "2"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b WHERE EXISTS (SELECT 1 FROM h WHERE h.f = b.k + 9))`, []string{"1", "2"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_ExistsOverASpine executes a WHERE EXISTS over a lateral-unnest spine
// whose bottom is a table: the chained select, as without the EXISTS, with the
// existential correlation and every conjunct beside it baked over the spine's
// merged row (a link's element is one whole-object slot of it). It was refused
// (0AF00, "multiple lateral array unnests"). Rows are the Java target's.
func TestFDB_ExistsOverASpine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/exists_over_a_spine"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE exists_over_a_spine_tmpl" +
			" CREATE TYPE AS STRUCT b (k BIGINT, tags BIGINT ARRAY)" +
			" CREATE TYPE AS STRUCT c (k BIGINT, bs b ARRAY)" +
			" CREATE TABLE q (id BIGINT, bs b ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE qq (id BIGINT, cs c ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE w (id BIGINT, f BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE exists_over_a_spine_tmpl",
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
		"INSERT INTO q VALUES (1, [(1, [7, 8]), (2, [9])]), (2, [(3, [11])]), (3, [(4, [20])])",
		"INSERT INTO qq VALUES (1, [(1, [(1, [7, 8]), (2, [9])]), (2, [])]), (2, [(3, [(4, [11, 12])]), (5, [(6, [13])])])",
		"INSERT INTO h VALUES (1, 10), (2, 9), (3, 12)",
		"INSERT INTO g VALUES (1, 1), (2, 3)",
		"INSERT INTO w VALUES (1, 1, [10, 11]), (2, 2, [20])",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	const spine = `FROM q, q.bs AS b, b.tags AS t WHERE `
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		// The tip element, a deeper element and the bottom table.
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`, []string{"11", "8", "9"}},
		{`SELECT t ` + spine + `NOT EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`, []string{"20", "7"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = b.k + 8)`, []string{"20", "7", "8", "9"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.id = q.id)`, []string{"11", "20", "7", "8", "9"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = b.k + t)`, []string{"8"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1 AND h.id = q.id + 1)`, []string{"11", "8"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h, g WHERE h.f = b.k + 8 AND g.id = h.id)`, []string{"7", "8", "9"}},
		// Conjuncts beside the EXISTS, two EXISTS, and projections.
		{`SELECT q.id, t ` + spine + `t > 7 AND EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`, []string{"1|8", "1|9", "2|11"}},
		{`SELECT t ` + spine + `b.k > 1 AND EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`, []string{"11", "9"}},
		{`SELECT t ` + spine + `q.id = 1 AND EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`, []string{"8", "9"}},
		{`SELECT t ` + spine + `t > b.k + 6 AND EXISTS (SELECT 1 FROM h WHERE h.f > b.k + t)`, []string{"8", "9"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1) AND NOT EXISTS (SELECT 1 FROM h WHERE h.f = t + 2)`, []string{"11", "9"}},
		{`SELECT t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1) AND EXISTS (SELECT 1 FROM g WHERE g.v = b.k)`, []string{"11", "8"}},
		{`SELECT b.k, t ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`, []string{"1|8", "2|9", "3|11"}},
		{`SELECT COUNT(*) ` + spine + `EXISTS (SELECT 1 FROM h WHERE h.f > t)`, []string{"4"}},
		// AT ordinals, a sibling spine, a three-link spine and a box bottom.
		{`SELECT t, o FROM q, q.bs AS b AT o, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = t + o)`, []string{"11|1", "8|1"}},
		{`SELECT t, p FROM q, q.bs AS b, b.tags AS t AT p WHERE EXISTS (SELECT 1 FROM h WHERE h.f = t + p)`, []string{"11|1", "8|2", "9|1"}},
		{`SELECT v, v2 FROM w, w.arr AS v, w.arr AS v2 WHERE EXISTS (SELECT 1 FROM h WHERE h.f + 1 = v2)`, []string{"10|10", "10|11", "11|10", "11|11"}},
		{`SELECT t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = c.k + 8)`, []string{"7", "8", "9"}},
		{`SELECT t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE NOT EXISTS (SELECT 1 FROM h WHERE h.f = c.k + b.k + 7)`, []string{"11", "12", "13"}},
		{`SELECT t, o FROM qq, qq.cs AS c AT o, c.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = t + o)`, []string{"11|1", "8|1", "9|1"}},
		{`SELECT g.id, t FROM q, g, q.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = t + g.id)`, []string{"1|11", "1|8", "1|9", "2|7", "2|8"}},
		// A spine over a block's first FROM item, its EXISTS reading the bottom.
		{`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = b.k + 8) AND t > 7)`, []string{"1", "3"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, b.tags AS t WHERE NOT EXISTS (SELECT 1 FROM h WHERE h.f = b.k + 8))`, []string{"2"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_ExistsOverARecordElementUnnestReadsTheTable executes a correlated
// EXISTS beside an unnest of a STRUCT array that reads the unnest's table. A
// record element states no executor window, so the correlation used to fall
// to a name-keyed rebase over the positional row and the query could not be
// translated (0AF00); the table refs now bake over the seed's row and the
// element refs stay bound by the FlatMap. Rows are the Java target's.
func TestFDB_ExistsOverARecordElementUnnestReadsTheTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/exists_over_record_element_unnest"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE exists_over_record_element_unnest_tmpl" +
			" CREATE TYPE AS STRUCT deeper (dk BIGINT)" +
			" CREATE TYPE AS STRUCT elem (ek BIGINT, d deeper)" +
			" CREATE TABLE t (id BIGINT, arr elem ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE exists_over_record_element_unnest_tmpl",
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
		"INSERT INTO t VALUES (1, [(10, (91)), (20, (92))]), (2, [(30, (93))]), (3, [(40, (94))])",
		"INSERT INTO h VALUES (1, 10), (2, 30), (5, 92)",
		"INSERT INTO g VALUES (1, 1)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	const from = `SELECT x.ek FROM t, t.arr AS x WHERE `
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{from + `EXISTS (SELECT 1 FROM h WHERE h.id = t.id)`, []string{"10", "20", "30"}},
		{from + `NOT EXISTS (SELECT 1 FROM h WHERE h.id = t.id)`, []string{"40"}},
		{from + `EXISTS (SELECT 1 FROM h WHERE h.id = t.id AND h.f = x.ek)`, []string{"10", "30"}},
		{from + `EXISTS (SELECT 1 FROM t AS m WHERE m.id = t.id AND m.id * 10 = x.ek)`, []string{"10"}},
		{from + `EXISTS (SELECT 1 FROM t AS m WHERE m.id = t.id)`, []string{"10", "20", "30", "40"}},
		{from + `NOT EXISTS (SELECT 1 FROM t AS m WHERE m.id = t.id + 1)`, []string{"40"}},
		{from + `EXISTS (SELECT 1 FROM h, g WHERE h.id = t.id AND g.id = h.id)`, []string{"10", "20"}},
		{`SELECT t.id, x.ek FROM t, t.arr AS x WHERE x.ek > 10 AND EXISTS (SELECT 1 FROM h WHERE h.id = t.id)`, []string{"1|20", "2|30"}},
		{`SELECT x.ek, o FROM t, t.arr AS x AT o WHERE EXISTS (SELECT 1 FROM h WHERE h.id = t.id + o - 1)`, []string{"10|1", "20|2", "30|1"}},
		{`SELECT COUNT(*) FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM h WHERE h.id = t.id)`, []string{"3"}},
		// The element alone, which the FlatMap binds.
		{from + `EXISTS (SELECT 1 FROM h WHERE h.f = x.d.dk)`, []string{"20"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_SpineWhereReadsADeeperLinksElement executes a WHERE reading the
// element of a spine link below the one under the tip (`c.k` under
// `qq.cs AS c, c.bs AS b, b.tags AS t`). That element is one whole-object slot
// of the merged row, not a run of named columns, so the leg-window rebase
// declined the conjunct (0AF00), and a scalar one bound as a one-field record
// failed at execution. Rows are the Java target's.
func TestFDB_SpineWhereReadsADeeperLinksElement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/spine_deeper_link_where"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE spine_deeper_link_where_tmpl" +
			" CREATE TYPE AS STRUCT b (k BIGINT, tags BIGINT ARRAY)" +
			" CREATE TYPE AS STRUCT c (k BIGINT, bs b ARRAY)" +
			" CREATE TABLE qq (id BIGINT, cs c ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE spine_deeper_link_where_tmpl",
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
		"INSERT INTO qq VALUES (1, [(1, [(1, [7, 8]), (2, [9])]), (2, [])])",
		"INSERT INTO qq VALUES (2, [(3, [(4, [11, 12])]), (5, [(6, [13])])])",
		"INSERT INTO h VALUES (1, 8)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT id FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = c.k + 8`, []string{"1", "2", "2"}},
		{`SELECT id, t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = qq.id + c.k + 6`, []string{"1|8", "2|11", "2|13"}},
		{`SELECT t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE t > c.k + b.k + 5`, []string{"8", "9"}},
		{`SELECT t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE c.k > 1 AND t > c.k + 7`, []string{"11", "12", "13"}},
		{`SELECT c.k, t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE t > c.k + 6 OR t = 13`, []string{"1|8", "1|9", "3|11", "3|12", "5|13"}},
		{`SELECT t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t WHERE t NOT IN (c.k + 8, b.k + 5)`, []string{"12", "7", "8"}},
		{`SELECT t FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t, h WHERE t = c.k + h.f`, []string{"11", "13", "9"}},
		{`SELECT t FROM qq JOIN qq.cs AS c ON 1 = 1 JOIN c.bs AS b ON 1 = 1 JOIN b.tags AS t ON t = c.k + 8`, []string{"11", "13", "9"}},
		// A deeper SCALAR element, and a deeper sibling's element.
		{`SELECT t, u, z FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t, b.tags AS u, b.tags AS z WHERE z = t + 1`, []string{"11|11|12", "11|12|12", "7|7|8", "7|8|8"}},
		{`SELECT t, b2.k FROM qq, qq.cs AS c, c.bs AS b, c.bs AS b2, b2.tags AS t WHERE t = b.k + 7`, []string{"11|4", "13|6", "8|1", "9|2"}},
		// AT links: the element/ordinal run.
		{`SELECT t FROM qq, qq.cs AS c AT o, c.bs AS b AT p, b.tags AS t WHERE t = c.k + b.k + o + p + 3`, []string{"12", "7", "9"}},
		// Over a block's first FROM item.
		{`SELECT id FROM qq WHERE EXISTS (SELECT 1 FROM qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = c.k + 8)`, []string{"1", "2"}},
		{`SELECT id FROM qq WHERE NOT EXISTS (SELECT 1 FROM qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = c.k + 8)`, nil},
		{`SELECT id FROM qq WHERE EXISTS (SELECT 1 FROM qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = c.k + 8 AND t > qq.id + 8)`, []string{"2"}},
		{`SELECT id FROM qq WHERE EXISTS (SELECT 1 FROM qq.cs AS c AT o, c.bs AS b, b.tags AS t WHERE t = c.k + o + 6)`, []string{"1", "2"}},
		{`SELECT id FROM qq WHERE EXISTS (SELECT 1 FROM qq.cs AS c, c.bs AS b, c.bs AS b2, b2.tags AS t WHERE t = b.k + 7)`, []string{"1", "2"}},
		{`SELECT d.t FROM qq, (SELECT t FROM qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = c.k + 8) AS d`, []string{"11", "13", "9"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFDB_LateralLegReadsASpineLinksElement executes later FROM items that
// read a spine link's element: they stay above the links, as more quantifiers
// of the tip's select. Rotated below the links they found the element unbound
// (0AF00). It also executes a link separated from its owner link by another
// FROM item and unnests of a second table standing after the first table's
// links (0AF00, and XX000 with a leg reading both, before), an unnest of a
// lateral leg's array column (0AF00 before), and an AT unnest's element and
// ordinal projected
// beside a third FROM item, once failing at execution because the Explode
// flowed `_0`/`_1` while its readers read the AS/AT names.
func TestFDB_LateralLegReadsASpineLinksElement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/lateral_leg_spine_element"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE lateral_leg_spine_element_tmpl" +
			" CREATE TYPE AS STRUCT b (k BIGINT, tags BIGINT ARRAY)" +
			" CREATE TYPE AS STRUCT c (k BIGINT, bs b ARRAY)" +
			" CREATE TABLE q (id BIGINT, bs b ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE qq (id BIGINT, cs c ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE w (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE lateral_leg_spine_element_tmpl",
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
		"INSERT INTO q VALUES (1, [(1, [7, 8]), (2, [9])]), (2, [(3, [11])])",
		"INSERT INTO qq VALUES (1, [(1, [(1, [7, 8]), (2, [9])]), (2, [])])",
		"INSERT INTO h VALUES (1, 10), (2, 9), (3, 12)",
		"INSERT INTO w VALUES (1, [10, 11]), (2, [20])",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	const spine = `FROM q, q.bs AS b, b.tags AS t, `
	const d = `(SELECT h.id FROM h WHERE h.f = t + 1) AS d`
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT d.id ` + spine + d, []string{"1", "2", "3"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT t FROM q.bs AS b, b.tags AS t, ` + d + `)`, []string{"1", "2"}},
		{`SELECT id FROM q WHERE NOT EXISTS (SELECT t FROM q.bs AS b, b.tags AS t, ` + d + `)`, nil},
		{`SELECT COUNT(*) ` + spine + d, []string{"3"}},
		// Reads of a deeper link, the bottom table and a sibling FROM item.
		{`SELECT d.id, t ` + spine + `(SELECT h.id FROM h WHERE h.f = b.k + t) AS d`, []string{"2|8"}},
		{`SELECT d.id, t, q.id ` + spine + `(SELECT h.id FROM h WHERE h.f = t + q.id) AS d`, []string{"1|9|1", "2|8|1"}},
		{`SELECT d.id, t, z.id ` + spine + `h AS z, ` + d + ` WHERE z.id = d.id`, []string{"1|9|1", "2|8|2", "3|11|3"}},
		{`SELECT d.id, e.id, t ` + spine + d + `, (SELECT h.id FROM h WHERE h.id = d.id) AS e`, []string{"1|1|9", "2|2|8", "3|3|11"}},
		// A WHERE over the legs and the links.
		{`SELECT d.id, t ` + spine + d + ` WHERE d.id > 1`, []string{"2|8", "3|11"}},
		{`SELECT d.id, t ` + spine + d + ` WHERE q.id = 1`, []string{"1|9", "2|8"}},
		{`SELECT d.id, t, b.k ` + spine + d + ` WHERE b.k + d.id = 3`, []string{"1|9|2", "2|8|1"}},
		{`SELECT d.id, t ` + spine + `(SELECT h.id FROM h WHERE h.f > t) AS d WHERE t > 8`, []string{"1|9", "3|11", "3|9"}},
		// AT links, and a three-link spine.
		{`SELECT d.id, t, o, p FROM q, q.bs AS b AT o, b.tags AS t AT p, (SELECT h.id FROM h WHERE h.f = t + p) AS d`, []string{"1|8|1|2", "1|9|2|1", "3|11|1|1"}},
		{`SELECT t, d.id FROM qq, qq.cs AS c, c.bs AS b, b.tags AS t, (SELECT h.id FROM h WHERE h.f = t + c.k) AS d`, []string{"8|2", "9|1"}},
		// A link separated from its owner link by another FROM item.
		{`SELECT t, h.id FROM q, q.bs AS b, h, b.tags AS t WHERE h.id = 1`, []string{"11|1", "7|1", "8|1", "9|1"}},
		{`SELECT t FROM q, q.bs AS b, h, b.tags AS t WHERE t + 1 = h.f`, []string{"11", "8", "9"}},
		{`SELECT t, o FROM q, q.bs AS b AT o, h, b.tags AS t WHERE t + o = h.f`, []string{"11|1", "8|1"}},
		{`SELECT d.t FROM q, (SELECT t FROM q.bs AS b, h, b.tags AS t WHERE t + 1 = h.f) AS d`, []string{"11", "8", "9"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, h, b.tags AS t WHERE t + 1 = h.f)`, []string{"1", "2"}},
		{`SELECT id FROM q WHERE NOT EXISTS (SELECT 1 FROM q.bs AS b, h, b.tags AS t WHERE t + 1 = h.f)`, nil},
		{`SELECT t, u FROM q, q.bs AS b, h, b.tags AS t, b.tags AS u WHERE u = t + 1 AND h.id = 1`, []string{"7|8"}},
		{`SELECT t FROM qq, qq.cs AS c, h, c.bs AS b, b.tags AS t WHERE t = c.k + 8 AND h.id = 1`, []string{"9"}},
		{`SELECT t, h.id, d.k FROM q, q.bs AS b, h, (SELECT b.k AS k FROM h AS h2 WHERE h2.id = 1) AS d, b.tags AS t WHERE h.id = 1`, []string{"11|1|3", "7|1|1", "8|1|1", "9|1|2"}},
		// Unnests of a second table standing after the first table's links.
		{`SELECT v, x.k FROM w, w.arr AS v, q, q.bs AS x WHERE q.id = 1`, []string{"10|1", "10|2", "11|1", "11|2", "20|1", "20|2"}},
		{`SELECT v, t FROM w, w.arr AS v, q, q.bs AS b, b.tags AS t WHERE v = t + 3`, []string{"10|7", "11|8"}},
		{`SELECT t, v FROM q, q.bs AS b, b.tags AS t, w, w.arr AS v WHERE v = t + 3`, []string{"7|10", "8|11"}},
		{`SELECT t, v FROM q, q.bs AS b, w, w.arr AS v, b.tags AS t WHERE v = t + 3`, []string{"7|10", "8|11"}},
		{`SELECT t, v, d.k FROM q, q.bs AS b, b.tags AS t, w, w.arr AS v, (SELECT b.k + v AS k FROM h WHERE h.id = 1) AS d WHERE v = t + 3`, []string{"7|10|11", "8|11|12"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, w, w.arr AS v, b.tags AS t WHERE v = t + 3)`, []string{"1"}},
		// An unnest of a lateral leg's array column, over a link and over a tip.
		{`SELECT z FROM q, q.bs AS b, (SELECT b.tags AS arr FROM h WHERE h.id = 1) AS d, d.arr AS z`, []string{"11", "7", "8", "9"}},
		{`SELECT z FROM w, w.arr AS v, (SELECT w.arr AS arr FROM h WHERE h.f = v) AS d, d.arr AS z`, []string{"10", "11"}},
		{`SELECT t, z FROM q, q.bs AS b, b.tags AS t, (SELECT b.tags AS arr FROM h WHERE h.id = 1) AS d, d.arr AS z WHERE z > t`, []string{"7|8"}},
		{`SELECT z, p FROM q, q.bs AS b, (SELECT b.tags AS arr FROM h WHERE h.id = 1) AS d, d.arr AS z AT p WHERE p = 2`, []string{"8|2"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT z FROM q.bs AS b, b.tags AS t, (SELECT b.tags AS arr FROM h WHERE h.id = 1) AS d, d.arr AS z WHERE z > t)`, []string{"1"}},
		// An array-owning source indirectly depends on the element through D.
		{`SELECT x.k, d.id FROM q, q.bs AS b, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d, (SELECT q.bs AS arr FROM h WHERE h.id = d.id) AS e, e.arr AS x`, []string{"1|1", "1|2", "2|1", "2|2"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT x.k FROM q.bs AS b, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d, (SELECT q.bs AS arr FROM h WHERE h.id = d.id) AS e, e.arr AS x)`, []string{"1"}},
		{`SELECT x.k, p, d.id FROM q, q.bs AS b, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d, (SELECT q.bs AS arr FROM h WHERE h.id = d.id) AS e, e.arr AS x AT p WHERE p = d.id`, []string{"1|1|1", "2|2|2"}},
		{`SELECT id FROM q WHERE EXISTS (SELECT x.k FROM q.bs AS b AT o, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d, (SELECT q.bs AS arr FROM h WHERE h.id = d.id) AS e, e.arr AS x AT p WHERE p = d.id AND o = 2 AND x.k = b.k - 1)`, []string{"1"}},
		{`SELECT id FROM q WHERE NOT EXISTS (SELECT x.k FROM q.bs AS b AT o, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d, (SELECT q.bs AS arr FROM h WHERE h.id = d.id) AS e, e.arr AS x AT p WHERE p = d.id AND o = 2 AND x.k = b.k - 1)`, []string{"2"}},
		{`SELECT z.k FROM q, (SELECT x.k AS k FROM q.bs AS b, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d, (SELECT q.bs AS arr FROM h WHERE h.id = d.id) AS e, e.arr AS x WHERE x.k = d.id) AS z`, []string{"1", "2"}},
		// One AT unnest beside a third FROM item.
		{`SELECT v, p, d.id FROM w, w.arr AS v AT p, (SELECT h.id FROM h WHERE h.f = v + p - 1) AS d`, []string{"10|1|1", "11|2|3"}},
		{`SELECT v, p, h.id FROM w, w.arr AS v AT p, h WHERE h.f = p + 9`, []string{"10|1|1", "20|1|1"}},
		{`SELECT p, h.id FROM w, w.arr AS v AT p, h WHERE h.f = v + p + 1`, []string{"1|3"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}
