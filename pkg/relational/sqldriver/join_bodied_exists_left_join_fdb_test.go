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
	"fmt"
	"sort"
	"strings"
	"testing"
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
// It does not reproduce, and the reason is upstream of the planner entirely: the
// shape needs a correlation to an enclosing alias INSIDE an OUTER JOIN's ON
// clause, and the SQL layer refuses exactly that with 0A000
// (`CorrelatedExistsError`, logical_predicate.go). The rewrite rule never sees
// the select, so the collision has nothing to act on.
//
// THIS TEST PINS THE REJECTION, NOT THE RULE. If the 0A000 is ever relaxed —
// which is a reasonable thing to want, since Java accepts more here — the
// name-collision path becomes reachable and silent. What re-arms it is this test
// going green in the OTHER direction: an accepted query instead of a refusal.
// The durable fix at that point is to carry predicate ownership from
// `existsInnerCorrelation`, which knows it at translation, rather than
// re-deriving it by alias intersection in two rules.
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
		name string
		sql  string
	}{
		{
			// The collision itself: `t` is bound outside AND re-bound inside the
			// nested existential.
			name: "shadowed_enclosing_alias",
			sql: "SELECT id FROM t WHERE EXISTS (" +
				"SELECT 1 FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM t WHERE t.id = a.id))",
		},
		{
			// CONTROL, and it is what identifies the refusal's real cause: no name
			// is shadowed here, and it is refused identically. So the rejection is
			// about the correlation in the OUTER JOIN's ON clause, not about the
			// shadowing — which is why relaxing it re-arms the collision.
			name: "unshadowed_control_refused_identically",
			sql: "SELECT id FROM t WHERE EXISTS (" +
				"SELECT 1 FROM a LEFT JOIN b ON b.k = a.k AND b.z = t.z " +
				"WHERE EXISTS (SELECT 1 FROM b AS b2 WHERE b2.k = a.k))",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.sql)
			if err == nil {
				defer rows.Close()
				var got []string
				for rows.Next() {
					var s sql.NullString
					if scanErr := rows.Scan(&s); scanErr != nil {
						t.Fatalf("scan: %v", scanErr)
					}
					got = append(got, s.String)
				}
				t.Fatalf("%s: the query was ACCEPTED and returned %v.\n"+
					"  This is the re-arming signal, not a pass. The buried-alias map in\n"+
					"  RewriteOuterJoinRule keys on an alias NAME, and this shape re-binds an\n"+
					"  enclosing name inside an existential — so a genuine ON-conjunct can now be\n"+
					"  lifted above the null-extension, degrading LEFT JOIN to INNER silently.\n"+
					"  Correct answer here is [1] (a's row is null-extended and the inner EXISTS\n"+
					"  holds); [] means the conjunct was lifted. Carry predicate ownership from\n"+
					"  existsInnerCorrelation instead of re-deriving it by alias intersection.",
					tc.name, got)
			}
			if !strings.Contains(err.Error(), "correlation inside an OUTER") {
				t.Fatalf("%s: refused, but NOT by the guard this test pins.\n  got: %v\n"+
					"  The pin is specifically that a correlation inside an OUTER JOIN ON clause is\n"+
					"  rejected upstream; a different refusal means the shape now reaches the\n"+
					"  planner by some other route and the name-collision hazard needs re-checking.",
					tc.name, err)
			}
		})
	}
}
