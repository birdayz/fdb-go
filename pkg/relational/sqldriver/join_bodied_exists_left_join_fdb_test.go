package sqldriver_test

// A join-bodied EXISTS beside a LEFT JOIN must keep its child WHERE inside
// the existential input, while the existence test stays above null-extension.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_JoinBodiedExistsOverLeftJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/jbexists")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/jbexists")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE jbexists "+
			"CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid)) "+
			"CREATE TABLE r (id BIGINT, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE s (k BIGINT, PRIMARY KEY (k))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/jbexists/s WITH TEMPLATE jbexists")
	dsn := fmt.Sprintf("fdbsql:///FRL/JBEXISTS?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// p.id ∈ {1,2}. q.qid = 1 matches p.id=1 only, so p.id=2 is NULL-extended.
	// r has one row keyed to q.qid=1 whose k joins s. So the EXISTS is TRUE for
	// the matched row and FALSE for the null-extended one — which is exactly the
	// discrimination a predicate folded below the null-extension destroys.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO p VALUES (1, 10), (2, 20)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO q VALUES (1)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO r VALUES (1, 100)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO s VALUES (100)")

	scan := func(t *testing.T, query string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", query, err)
		}
		var out []string
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(sql.NullString)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("scan %q: %v", query, err)
			}
			row := ""
			for i, c := range cells {
				if i > 0 {
					row += "|"
				}
				v := c.(*sql.NullString)
				if !v.Valid {
					row += "NULL"
					continue
				}
				row += v.String
			}
			out = append(out, row)
		}
		// Checked BEFORE the comparison: an iteration that died mid-stream
		// otherwise reads as a short result set, which is the same green an
		// empty table produces.
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", query, err)
		}
		return out
	}

	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		{
			// PROJECTED. Both preserved rows survive the LEFT JOIN; the EXISTS
			// discriminates them. If the hoisted `r.id = q.qid` is folded below
			// the null-extension the discrimination is lost — every row reports
			// the same flag, or the query dies on an unbindable correlation.
			name: "projected_join_bodied_exists",
			sql: "SELECT p.v, EXISTS (SELECT 1 FROM r, s WHERE r.k = s.k AND r.id = q.qid) " +
				"FROM p LEFT JOIN q ON q.qid = p.id",
			want: []string{"10|true", "20|false"},
		},
		{
			// WHERE spelling of the same subquery. Only the matched row passes,
			// and the null-extended one must be filtered out rather than dropped
			// before the extension happens.
			name: "where_join_bodied_exists",
			sql: "SELECT p.v FROM p LEFT JOIN q ON q.qid = p.id " +
				"WHERE EXISTS (SELECT 1 FROM r, s WHERE r.k = s.k AND r.id = q.qid)",
			want: []string{"10"},
		},
		{
			// THE CONTROL, and it is what makes the two above readable: a
			// SCAN-bodied EXISTS over the same data IS renameable, so its
			// predicate names the existential alias and took the correct path
			// even before the fix. Identical expected rows to the projected case.
			// If this one ever diverges from it, the cause is the LEFT JOIN or
			// the data, not the buried-alias split.
			name: "scan_bodied_exists_control",
			sql: "SELECT p.v, EXISTS (SELECT 1 FROM r WHERE r.id = q.qid) " +
				"FROM p LEFT JOIN q ON q.qid = p.id",
			want: []string{"10|true", "20|false"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Sorted in GO, not by SQL. An ORDER BY on top of this fold is a
			// separate planner path that declines (0AF00), so adding one here would
			// test the sort rather than the null-extension — the same reason the
			// sibling projected-EXISTS pin is deliberately ORDER-BY-free.
			got := scan(t, tc.sql)
			sort.Strings(got)
			if len(got) != len(tc.want) {
				t.Fatalf("%s: got %v (%d rows), want %v (%d rows)",
					tc.name, got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: row %d = %q, want %q (full: %v vs %v)",
						tc.name, i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
	}
}

// TestFDB_BuriedAliasShadowingIsRejectedUpstream (the name predates the
// change) pins an enclosing correlation inside an OUTER JOIN's ON below an
// EXISTS whose nested existential re-binds the enclosing name.
//
// Predicate ownership keys on a binding, and a subquery's legs have private
// bindings, so the ON's t.z stays on the LEFT JOIN, above the null-extension,
// and the nested EXISTS reads its own t. Go refused the EXISTS route with
// 0A000 ("correlation inside an OUTER JOIN ON clause") until 2026-10-07; it now
// keeps the correlation in the ON and answers Java's rows (conformance
// ExistsInnerShadowJavaProbe, outer_join_on_corr_*). A lifted ON conjunct
// would turn the LEFT JOIN inner: b is null-extended here (a.k=5, b.k=9), so
// the shadowed case would lose its row.
func TestFDB_BuriedAliasShadowingIsRejectedUpstream(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/shadowreject")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/shadowreject")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE shadowreject "+
			"CREATE TABLE t (id BIGINT, z BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE a (k BIGINT, id BIGINT, PRIMARY KEY (k)) "+
			"CREATE TABLE b (k BIGINT, z BIGINT, PRIMARY KEY (k))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/shadowreject/s WITH TEMPLATE shadowreject")
	dsn := fmt.Sprintf("fdbsql:///FRL/SHADOWREJECT?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a's only row has NO matching b (k=5 vs k=9), so the LEFT JOIN would
	// null-extend b — which is what makes a wrongly-lifted ON conjunct
	// observable, if the query were ever planned.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t VALUES (1, 100)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO a VALUES (5, 1)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO b VALUES (9, 100)")

	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// `t` is bound outside AND re-bound inside the nested existential.
			name: "exists_route_shadowed_enclosing_alias",
			sql: "SELECT id FROM t WHERE EXISTS (" +
				"SELECT 1 FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM t WHERE t.id = a.id))",
			want: "[1]",
		},
		{
			// No name is shadowed; the nested EXISTS finds no b2 with k = 5.
			name: "exists_route_unshadowed_control",
			sql: "SELECT id FROM t WHERE EXISTS (" +
				"SELECT 1 FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM b AS b2 WHERE b2.k = a.k))",
			want: "[]",
		},
		{
			// The correlated-scalar route through the shared full-query visitor.
			name: "scalar_route_shadowed_enclosing_alias",
			sql: "SELECT id, (SELECT COUNT(*) FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM t WHERE t.id = a.id)) FROM t",
			want: "[1|1]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, queryErr := db.QueryContext(ctx, tc.sql)
			if queryErr != nil {
				t.Fatalf("%s: %v", tc.name, queryErr)
			}
			defer rows.Close()
			cols, colErr := rows.Columns()
			if colErr != nil {
				t.Fatalf("columns: %v", colErr)
			}
			got := []string{}
			for rows.Next() {
				var a, b sql.NullString
				if len(cols) == 1 {
					if scanErr := rows.Scan(&a); scanErr != nil {
						t.Fatalf("scan: %v", scanErr)
					}
					got = append(got, a.String)
					continue
				}
				if scanErr := rows.Scan(&a, &b); scanErr != nil {
					t.Fatalf("scan: %v", scanErr)
				}
				got = append(got, a.String+"|"+b.String)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("%s: rows = %v, want %s", tc.name, got, tc.want)
			}
		})
	}
}
