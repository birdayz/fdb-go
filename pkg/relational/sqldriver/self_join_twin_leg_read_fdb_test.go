package sqldriver_test

// The SELF-JOIN TWIN: a correlated read of one leg's column while the OTHER leg
// of the same self-join declares an identical column, at an identical
// leg-relative ordinal, in an identical row layout.
//
// This is the shape `rebaseOuterLegValue`'s leg-match arm has to place, and it
// is the one shape on which every element of a column's identity except the
// CORRELATION is shared between two live legs. A self-join over one table gives
// both legs the same row type, so:
//
//   - the leaf NAME is the same in both legs ("K");
//   - the leg-relative ORDINAL is the same in both legs (2);
//   - the ordinal DOMAIN is the same in both legs (one row type, one domain
//     token).
//
// Only the quantifier the read hangs off can say which of the two it means.
//
// THE SHAPE THAT REACHES THE ARM is the PROJECTED existential over a THREE-leg
// join, not the WHERE one and not a two-leg join. Measured on this corpus: the
// three-leg projected form drives the leg-match arm twelve times, and both the
// WHERE-existential form and the two-leg projected form drive it ZERO times.
// All three are kept — the arm-reaching one because it is the subject, the
// other two because a shape that STOPS reaching the arm has to be visible as a
// change rather than as silence.
//
// The ISOLATION LADDER below varies one axis at a time (distinct column names /
// same names in distinct tables / the same table twice) so that a wrong answer
// names its own cause instead of being attributed by argument. That mattered:
// the first run of this test failed on ALL THREE rungs, which is what showed the
// defect it found was not about twins at all — see
// TestFDB_ProjectedExistsCorrelatedToNonFirstLeg.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_SelfJoinTwinLegCorrelatedRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_twinleg"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE w4l_twinleg_tmpl"+
		" CREATE TABLE twn (id BIGINT, partner BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE twc (id BIGINT, partner BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE twd (did BIGINT, dpartner BIGINT, dk BIGINT, PRIMARY KEY (did))"+
		" CREATE TABLE twq (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE twp (pid BIGINT, fk BIGINT, PRIMARY KEY (pid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE w4l_twinleg_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// TWN/TWC/TWD hold the same two rows under different column names. The
	// pairing `b.<id> = a.partner` therefore always mates row 1 with row 2, so
	// the two legs' K values on any surviving pair are 100 and 200 — DIFFERENT,
	// which is what makes a twin-leg read detectable at all.
	for _, stmt := range []string{
		"INSERT INTO twn VALUES (1, 2, 100), (2, 1, 200)",
		"INSERT INTO twc VALUES (1, 2, 100), (2, 1, 200)",
		"INSERT INTO twd VALUES (1, 2, 100), (2, 1, 200)",
		"INSERT INTO twq VALUES (1), (2)",
		// TWP holds 200 and not 100, so the existential separates the two legs.
		"INSERT INTO twp VALUES (1, 200)",
	} {
		if _, sErr := db.ExecContext(ctx, stmt); sErr != nil {
			t.Fatalf("seed %q: %v", stmt, sErr)
		}
	}

	explain := func(q string) string {
		var p string
		if eErr := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&p); eErr != nil {
			return fmt.Sprintf("<EXPLAIN failed: %v>", eErr)
		}
		return p
	}
	rows2 := func(t *testing.T, q string) []string {
		t.Helper()
		r, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query must ANSWER: %v\n  sql: %s", qErr, q)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var a int64
			var e sql.NullBool
			if sErr := r.Scan(&a, &e); sErr != nil {
				t.Fatalf("scan: %v\n  sql: %s", sErr, q)
			}
			out = append(out, fmt.Sprintf("(%d,%v)", a, e.Valid && e.Bool))
		}
		if rErr := r.Err(); rErr != nil {
			t.Fatalf("rows: %v\n  sql: %s", rErr, q)
		}
		sort.Strings(out)
		return out
	}
	ids := func(t *testing.T, q string) []string {
		t.Helper()
		r, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query must ANSWER: %v\n  sql: %s", qErr, q)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var a, b int64
			if sErr := r.Scan(&a, &b); sErr != nil {
				t.Fatalf("scan: %v\n  sql: %s", sErr, q)
			}
			out = append(out, fmt.Sprintf("(%d,%d)", a, b))
		}
		if rErr := r.Err(); rErr != nil {
			t.Fatalf("rows: %v\n  sql: %s", rErr, q)
		}
		sort.Strings(out)
		return out
	}

	// THE LADDER. a=1 mates with b=2 (K=200, in TWP) → true; a=2 mates with b=1
	// (K=100, absent) → false. Identical on all three rungs by construction, so
	// a rung that answers differently from its neighbours names the axis that
	// broke it.
	for _, rung := range []struct{ name, mid, midCol string }{
		{"distinct-names", "twd AS b ON b.did = a.partner", "dk"},
		{"same-names-distinct-tables", "twc AS b ON b.id = a.partner", "k"},
		{"self-join-twin", "twn AS b ON b.id = a.partner", "k"},
	} {
		q := fmt.Sprintf(`SELECT a.id, EXISTS (SELECT 1 FROM twp WHERE twp.fk = b.%s) `+
			`FROM twn AS a JOIN %s JOIN twq ON twq.qid = a.id`, rung.midCol, rung.mid)
		if got := fmt.Sprint(rows2(t, q)); got != "[(1,true) (2,false)]" {
			t.Errorf("[%s] rows = %s, want [(1,true) (2,false)]\n  sql: %s\n  plan: %s\n\n"+
				"  The existential reads the SECOND leg's column. On the self-join rung\n"+
				"  both legs declare that column at the same leg-relative ordinal in the\n"+
				"  same row layout, so the correlation is the only element that can pick\n"+
				"  one — and the two neighbouring rungs remove the name and the domain\n"+
				"  collision in turn, so a failure that spans all three is NOT about the\n"+
				"  twin.\n"+
				"  Both flags TRUE means the existential's own emptiness never reached\n"+
				"  the fold; SWAPPED means the read resolved against the twin's window;\n"+
				"  both FALSE means it resolved against nothing and evaluated NULL.",
				rung.name, got, q, explain(q))
		}
	}

	// The same twin, correlated to each leg in turn, projecting both ids. The
	// two answers are exact complements, so neither can be produced by a rebase
	// that always picks the first leg or one that always picks the last.
	const projToB = `SELECT a.id, EXISTS (SELECT 1 FROM twp WHERE twp.fk = b.k) ` +
		`FROM twn AS a JOIN twn AS b ON b.id = a.partner JOIN twq ON twq.qid = a.id`
	const projToA = `SELECT a.id, EXISTS (SELECT 1 FROM twp WHERE twp.fk = a.k) ` +
		`FROM twn AS a JOIN twn AS b ON b.id = a.partner JOIN twq ON twq.qid = a.id`
	gotB, gotA := fmt.Sprint(rows2(t, projToB)), fmt.Sprint(rows2(t, projToA))
	if gotB != "[(1,true) (2,false)]" {
		t.Errorf("rows = %s, want [(1,true) (2,false)]\n  sql: %s\n  plan: %s",
			gotB, projToB, explain(projToB))
	}
	if gotA != "[(1,false) (2,true)]" {
		t.Errorf("rows = %s, want [(1,false) (2,true)]\n  sql: %s\n  plan: %s\n\n"+
			"  The mirror of the case above, and it is not redundant with it: a rebase\n"+
			"  that always resolved to the FIRST leg's window would pass that one and\n"+
			"  fail this one, and one that always resolved to the LAST would do the\n"+
			"  reverse. Only running both pins that the correlation is what decides.",
			gotA, projToA, explain(projToA))
	}
	if gotB == gotA {
		t.Fatalf("both correlations returned %s. The fixture makes the two legs' K "+
			"values differ on every surviving pair; if the answers agree, this test "+
			"cannot tell a correct leg resolution from a twin-leg one and the two "+
			"assertions above are vacuous.", gotB)
	}

	// The WHERE-existential twin. It does NOT reach the rebase arm (measured:
	// zero firings), so it pins the runtime half alone — that the merged-leg
	// binder resolves each self-join leg to its own window on a shape that never
	// asked the planner to re-anchor anything.
	const whereToB = `SELECT a.id, b.id FROM twn AS a, twn AS b ` +
		`WHERE a.partner = b.id AND EXISTS (SELECT 1 FROM twp WHERE twp.fk = b.k)`
	const whereToA = `SELECT a.id, b.id FROM twn AS a, twn AS b ` +
		`WHERE a.partner = b.id AND EXISTS (SELECT 1 FROM twp WHERE twp.fk = a.k)`
	if got := fmt.Sprint(ids(t, whereToB)); got != "[(1,2)]" {
		t.Errorf("rows = %s, want [(1,2)]\n  sql: %s\n  plan: %s", got, whereToB, explain(whereToB))
	}
	if got := fmt.Sprint(ids(t, whereToA)); got != "[(2,1)]" {
		t.Errorf("rows = %s, want [(2,1)]\n  sql: %s\n  plan: %s", got, whereToA, explain(whereToA))
	}
}
