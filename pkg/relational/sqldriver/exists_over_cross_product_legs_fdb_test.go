package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_ExistsOverLegsOfACrossProduct pins a WHERE EXISTS whose conjuncts
// each read a different FROM leg, so the legs are connected only THROUGH the
// semi-join. The flat select's one valid bipartition keeps every leg below the
// existential; at the merge-base the partition rule pruned it as a disconnected
// lower and the query had no plan (0AF00), and with a derived table among the
// legs the translator enclosed the legs and the derived body's join refused
// ("join did not ordinalize"). Every leg holds several rows, so a plan that
// collapsed a leg's multiplicity into the semi-join, or answered it once per
// outer row, would disagree with these rows. The same shapes are compared
// against the Java engine in conformance's ws_f_join_unnest spec.
func TestFDB_ExistsOverLegsOfACrossProduct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/FRL/exists_over_cross_product_legs"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE exists_over_cross_product_legs_tmpl" +
			" CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE h (id BIGINT, f BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE g (k BIGINT, v BIGINT, PRIMARY KEY (k))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE exists_over_cross_product_legs_tmpl",
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
		"INSERT INTO w VALUES (1, 1), (2, 2), (3, 3)",
		"INSERT INTO h VALUES (1, 10), (2, 20), (3, 5)",
		"INSERT INTO g VALUES (1, 15), (2, 4), (3, 25)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// g[w.id].v is 15, 4, 25: w=1 keeps the h rows below 15 (h1, h3), w=2
	// keeps none, w=3 keeps all three.
	semi := `EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f)`
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT w.id, h.id FROM w, h WHERE ` + semi, []string{"[1 1]", "[1 3]", "[3 1]", "[3 2]", "[3 3]"}},
		{`SELECT w.id, h.id FROM h, w WHERE ` + semi, []string{"[1 1]", "[1 3]", "[3 1]", "[3 2]", "[3 3]"}},
		// One leg projected: the other leg's multiplicity survives.
		{`SELECT w.id FROM w, h WHERE ` + semi, []string{"[1]", "[1]", "[3]", "[3]", "[3]"}},
		{`SELECT w.id, h.id FROM w, h WHERE NOT ` + semi, []string{"[1 2]", "[2 1]", "[2 2]", "[2 3]"}},
		{`SELECT w.id, h.id FROM w, h WHERE ` + semi + ` AND w.f < 3`, []string{"[1 1]", "[1 3]"}},
		{`SELECT w.id, COUNT(*) FROM w, h WHERE ` + semi + ` GROUP BY w.id`, []string{"[1 2]", "[3 3]"}},
		// Three legs connected only through the semi-join: g.v below z.v
		// keeps g2 for w=1 and g1, g2 for w=3.
		{
			`SELECT w.id, h.id, g.k FROM w, h, g WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f AND z.v > g.v)`,
			[]string{"[1 1 2]", "[1 3 2]", "[3 1 1]", "[3 1 2]", "[3 2 1]", "[3 2 2]", "[3 3 1]", "[3 3 2]"},
		},
		// Controls: legs already connected without the semi-join.
		{`SELECT w.id, h.id FROM w, h WHERE w.id = h.id AND ` + semi, []string{"[1 1]", "[3 3]"}},
		{
			`SELECT w.id, h.id FROM w, h WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > 10) AND EXISTS (SELECT 1 FROM g AS y WHERE y.v > h.f + 10)`,
			[]string{"[1 1]", "[1 3]", "[3 1]", "[3 3]"},
		},
		// A derived table among the legs: a star over a join (h[a.k].f is 10,
		// 20, 5 for k = 1, 2, 3), a star over one table (second in FROM), and
		// a projection over a join (h's f values are distinct, so d = h[a.k]).
		{
			`SELECT a.id, a.v, d.id FROM (SELECT * FROM w, g) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k AND z.f > d.f)`,
			[]string{"[1 15 3]", "[1 4 1]", "[1 4 3]", "[2 15 3]", "[2 4 1]", "[2 4 3]", "[3 15 3]", "[3 4 1]", "[3 4 3]"},
		},
		{
			`SELECT a.id, d.id FROM h AS d, (SELECT * FROM w) AS a WHERE NOT EXISTS (SELECT 1 FROM g AS z WHERE z.k = a.id AND z.v > d.f)`,
			[]string{"[1 2]", "[2 1]", "[2 2]", "[2 3]"},
		},
		{
			`SELECT a.id, a.k, d.id FROM (SELECT w.id, g.k FROM w, g) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k AND z.f = d.f)`,
			[]string{"[1 1 1]", "[1 2 2]", "[1 3 3]", "[2 1 1]", "[2 2 2]", "[2 3 3]", "[3 1 1]", "[3 2 2]", "[3 3 3]"},
		},
		{
			`SELECT COUNT(*) FROM (SELECT w.id, g.k FROM w, g) AS a, h AS d WHERE NOT EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k AND z.f = d.f)`,
			[]string{"[18]"},
		},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			if got := sortedRowStrings(t, db, ctx, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// sortedRowStrings runs q and returns each row rendered by fmt, sorted, so an
// expected multiset of rows compares as one string.
func sortedRowStrings(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
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
	return got
}
