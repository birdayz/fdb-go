package sqldriver_test

// A JOIN-BODIED EXISTS beside a LEFT JOIN — the shape whose hoisted correlation
// predicate does NOT name the existential's own alias.
//
// existsInnerCorrelation rebases a hoisted EXISTS correlation onto the
// existential's alias only when existsInnerSafeToRename allows it, and that
// refuses a JOIN- or CTE-bodied subquery. For those the predicate keeps the
// subquery-INTERNAL alias — `R.id = q.qid`, naming R and the null-supplying leg,
// but never the existential.
//
// RewriteOuterJoinRule splits predicates by side of the null-extension: ON goes
// below, WHERE-EXISTS stays above. Splitting on "names an existential alias"
// alone therefore read this predicate as an ON-predicate and folded it BELOW the
// null-extension, where R is bound by nothing — an unbindable correlation, or a
// NULL evaluation that empties the inner and null-extends every row.
//
// PartitionSelectRule already compensated for exactly this with a
// buried-alias map; the rewrite rule did not. These are the row-level pins for
// the fix, in both spellings, because the defect is invisible to a plan-shape
// assertion: the plan is well-formed either way and only the ROWS differ.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_JoinBodiedExistsOverLeftJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/jbexists")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /jbexists")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE jbexists "+
			"CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid)) "+
			"CREATE TABLE r (id BIGINT, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE s (k BIGINT, PRIMARY KEY (k))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /jbexists/s WITH TEMPLATE jbexists")
	dsn := fmt.Sprintf("fdbsql:///jbexists?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// p.id ∈ {1,2}. q.qid = 1 matches p.id=1 only, so p.id=2 is NULL-extended.
	// r has one row keyed to q.qid=1 whose k joins s. So the EXISTS is TRUE for
	// the matched row and FALSE for the null-extended one — which is exactly the
	// discrimination a predicate folded below the null-extension destroys.
	mwjoMustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO q VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO r VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO s VALUES (100)")

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

// TestFDB_BuriedAliasShadowingIsRejectedUpstream is a NEGATIVE result, pinned
// because the conclusion it supports is load-bearing.
//
// The fix above maps every alias BOUND INSIDE an existential's subgraph to that
// existential, so a hoisted predicate naming a subquery-internal alias stays
// above the null-extension. The map keys on a NAME, and a
// CorrelationIdentifier's name is not unique across scopes — nothing uniquifies
// a table binding across nesting levels. So a predicate naming an ENCLOSING
// alias whose name is re-bound inside one of this select's existentials would be
// classified above by mistake, lifting a genuine ON-conjunct over the
// null-extension and degrading LEFT JOIN to INNER silently. That is the same
// failure class the fix closes, mirrored.
//
// It does not reproduce, and the reason is upstream of the planner in BOTH
// routes that can build a JoinLeftOuter select carrying an external
// correlation. Each arm below names its own guard, because they are different
// guards and only one of them is about outer joins at all:
//
//   - the EXISTS route (existsSubqueryPlanner.buildCorrelatedExists) refuses a
//     correlation inside an OUTER JOIN's ON clause outright;
//   - the correlated-SCALAR route (buildCorrelatedScalar) builds its JoinLeft
//     legs with the walked ON predicate verbatim and has NO outer-join decline —
//     it is stopped earlier and for an unrelated reason, by the predicate walk
//     refusing a nested EXISTS at all.
//
// SCOPE, because a reader will otherwise close this on the wrong guard: the
// outer/inner alias-collision decline that sits a few lines below the EXISTS
// guard does NOT cover this. Its inner-alias set is the EXISTS body's own scan
// plus its join legs — SAME LEVEL only — so a name re-bound inside a NESTED
// existential passes it untouched.
//
// THIS TEST PINS THE REJECTIONS, NOT THE RULE. If either is relaxed — and the
// scalar one especially, since it is incidental rather than a considered
// outer-join guard — the name-collision path becomes reachable and silent. What
// re-arms it is this test going green in the OTHER direction: an accepted query
// instead of a refusal. The durable fix at that point is to carry predicate
// ownership from `existsInnerCorrelation`, which knows it at translation, rather
// than re-deriving it by alias intersection in two rules.
func TestFDB_BuriedAliasShadowingIsRejectedUpstream(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/shadowreject")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /shadowreject")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE shadowreject "+
			"CREATE TABLE t (id BIGINT, z BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE a (k BIGINT, id BIGINT, PRIMARY KEY (k)) "+
			"CREATE TABLE b (k BIGINT, z BIGINT, PRIMARY KEY (k))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /shadowreject/s WITH TEMPLATE shadowreject")
	dsn := fmt.Sprintf("fdbsql:///shadowreject?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a's only row has NO matching b (k=5 vs k=9), so the LEFT JOIN would
	// null-extend b — which is what makes a wrongly-lifted ON conjunct
	// observable, if the query were ever planned.
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (5, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (9, 100)")

	for _, tc := range []struct {
		name      string
		sql       string
		wantGuard string
	}{
		{
			// The collision itself, EXISTS route: `t` is bound outside AND
			// re-bound inside the nested existential.
			name: "exists_route_shadowed_enclosing_alias",
			sql: "SELECT id FROM t WHERE EXISTS (" +
				"SELECT 1 FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM t WHERE t.id = a.id))",
			wantGuard: "correlation inside an OUTER",
		},
		{
			// CONTROL, and it is what identifies the refusal's real cause: no name
			// is shadowed here, and it is refused identically. So the rejection is
			// about the correlation in the OUTER JOIN's ON clause, not about the
			// shadowing — which is why relaxing it re-arms the collision.
			name: "exists_route_unshadowed_control_refused_identically",
			sql: "SELECT id FROM t WHERE EXISTS (" +
				"SELECT 1 FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM b AS b2 WHERE b2.k = a.k))",
			wantGuard: "correlation inside an OUTER",
		},
		{
			// The SECOND route. buildCorrelatedScalar puts the walked ON predicate
			// onto a JoinLeft node verbatim with no outer-join decline, so if this
			// shape planned it would reach the rewrite carrying the same external
			// correlation. It is stopped for an unrelated reason — the predicate
			// walk refuses a nested EXISTS — which is exactly why it is pinned
			// separately: that guard could be lifted by work that has nothing to do
			// with outer joins.
			name: "scalar_route_shadowed_enclosing_alias",
			sql: "SELECT id, (SELECT COUNT(*) FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM t WHERE t.id = a.id)) FROM t",
			wantGuard: "unsupported shape: EXISTS",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, queryErr := db.QueryContext(ctx, tc.sql)
			if queryErr == nil {
				defer rows.Close()
				var got []string
				for rows.Next() {
					var a, b sql.NullString
					cols, colErr := rows.Columns()
					if colErr != nil {
						t.Fatalf("columns: %v", colErr)
					}
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
				t.Fatalf("%s: the query was ACCEPTED and returned %v.\n"+
					"  This is the re-arming signal, not a pass. The buried-alias map in\n"+
					"  RewriteOuterJoinRule keys on an alias NAME, and this shape re-binds an\n"+
					"  enclosing name inside an existential — so a genuine ON-conjunct can now be\n"+
					"  lifted above the null-extension, degrading LEFT JOIN to INNER silently.\n"+
					"  For the EXISTS-route arms the correct answer is [1] (a's row is\n"+
					"  null-extended and the inner EXISTS holds); [] means the conjunct was\n"+
					"  lifted. Carry predicate ownership from existsInnerCorrelation instead of\n"+
					"  re-deriving it by alias intersection.", tc.name, got)
			}
			// The engine's own error type, not just its text: a refusal that stops
			// being an *api.Error means the query died somewhere other than the
			// planner, which would satisfy a substring check while proving nothing.
			var apiErr *api.Error
			if !errors.As(queryErr, &apiErr) {
				t.Fatalf("%s: refused with %T, not *api.Error — the refusal did not come from\n"+
					"  the engine, so it says nothing about whether this shape can be planned.\n  got: %v",
					tc.name, queryErr, queryErr)
			}
			if apiErr.Code != "0A000" {
				t.Fatalf("%s: refused with SQLSTATE %s, want 0A000 (unsupported). A different\n"+
					"  code means a different failure, and the unreachability argument no longer\n"+
					"  rests on what this test checked.\n  got: %v", tc.name, apiErr.Code, queryErr)
			}
			// The substring is deliberate ON TOP of the typed check: 0A000 covers
			// every unsupported shape, and the claim here is about ONE specific
			// guard per arm. Do not "tidy" it away into the code check.
			if !strings.Contains(queryErr.Error(), tc.wantGuard) {
				t.Fatalf("%s: refused with 0A000 but NOT by the guard this arm pins (%q).\n  got: %v\n"+
					"  A different guard means the shape now reaches the planner by another route\n"+
					"  and the name-collision hazard needs re-checking.", tc.name, tc.wantGuard, queryErr)
			}
		})
	}
}
