package sqldriver_test

// FULL OUTER JOIN has no Java oracle, so it gets an algebraic one.
//
// Java REFUSES this join outright — QueryVisitor.visitOuterJoin asserts
// `joinType != JoinType.FULL` with UNSUPPORTED_QUERY "FULL OUTER JOIN is not
// currently supported" — so it is a Go-only extension, which this port permits
// on the read side provided the coverage is deep. What that permission costs is
// the cross-engine net: every other join shape can be checked against a second
// implementation, and this one cannot. There is nothing to compare against.
//
// So it is checked against its DEFINITION instead. A full outer join is the
// left outer join plus the right rows that matched nothing:
//
//	A FULL JOIN B ON p
//	  ==  (A LEFT JOIN B ON p)
//	      UNION ALL
//	      (the B rows with no partner, padded with NULLs on the A side)
//
// and the second term is itself expressible as a LEFT JOIN whose A side came
// back NULL. Both terms are built from LEFT JOIN — a shape that DOES have a
// Java oracle and a large existing corpus — so a defect in FULL cannot hide
// behind a matching defect in the decomposition unless LEFT is broken too, and
// LEFT being broken is loudly visible elsewhere.
//
// The existing full_outer_join_fdb_test.go is 437 lines of hand-written
// expectations. That is worth having and it is a different instrument: it
// catches what its author anticipated. This catches what nobody did.
//
// THE FIXTURE IS BUILT AROUND THE THREE WAYS A ROW CAN BE UNMATCHED, because a
// full join that quietly behaved as an inner join would still satisfy any
// fixture where everything matches:
//
//	an A row with no partner        -> A side present, B side NULL
//	a B row with no partner         -> A side NULL, B side present
//	a row whose join key is NULL    -> matches NOTHING, on either side, since
//	                                   NULL = NULL is UNKNOWN — so it appears
//	                                   once from its own side, padded

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func openFullJoinDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fulljoin_decomp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fulljoin_decomp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE fjd_t "+
		"CREATE TABLE a (id BIGINT, k BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT, k BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fulljoin_decomp/s WITH TEMPLATE fjd_t")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_fulljoin_decomp?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFDB_FullOuterJoinMatchesItsDecomposition(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openFullJoinDB(t)

	// a: k=1 (matches b twice), k=2 (matches once), k=7 (no partner), k=NULL
	// b: k=1 twice, k=2 once, k=9 (no partner), k=NULL
	//
	// k=1 appearing twice on each side makes the matched part a 2x2 fan-out,
	// so a decomposition that deduplicated anywhere would show up as a row
	// count difference rather than needing a value to change.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, k) VALUES "+
		"(1, 1), (2, 1), (3, 2), (4, 7), (5, NULL)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, k) VALUES "+
		"(10, 1), (11, 1), (12, 2), (13, 9), (14, NULL)")

	// multiset renders an unordered result comparably. FULL JOIN has no
	// inherent order and neither does UNION ALL, so order is not part of the
	// answer and comparing it would report a difference that is not one.
	multiset := func(q string) ([]string, error) {
		rows, err := mmRows(t, ctx, db, q)
		if err != nil {
			return nil, err
		}
		out := append([]string(nil), rows...)
		sort.Strings(out)
		return out, nil
	}

	cases := []struct{ name, on string }{
		{"equality on a nullable key", "a.k = b.k"},
		// A predicate that matches NOTHING: every row of both sides is
		// unmatched, so the full join must return |a| + |b| rows. An engine
		// that degraded to an inner join returns none, and one that degraded to
		// a cross product returns |a| * |b|.
		{"a predicate that never matches", "a.k = b.k AND 1 = 0"},
		// A predicate that matches EVERYTHING — the cross product, with no row
		// unmatched. The opposite end of the same axis.
		{"a predicate that always matches", "1 = 1"},
		// An inequality, so the match is many-to-many but not by equality —
		// a shape an equality-only join implementation would get wrong.
		{"an inequality", "a.k < b.k"},
		// Compound, with one conjunct on each side.
		{"compound", "a.k = b.k AND a.id < 4"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			full := fmt.Sprintf(
				"SELECT a.id, b.id FROM a FULL OUTER JOIN b ON %s", c.on)
			// The decomposition: the left outer join, plus the b rows that
			// matched nothing padded with a NULL on the a side. The second term
			// is a LEFT JOIN from b whose a side came back NULL — a.id is the
			// primary key, so it is NULL there only when no partner existed.
			decomposed := fmt.Sprintf(
				"SELECT a.id, b.id FROM a LEFT JOIN b ON %s "+
					"UNION ALL "+
					"SELECT a.id, b.id FROM b LEFT JOIN a ON %s WHERE a.id IS NULL",
				c.on, c.on)

			gotFull, err := multiset(full)
			if err != nil {
				t.Fatalf("the FULL join failed to run: %v\n  q: %s", err, full)
			}
			gotDecomp, err := multiset(decomposed)
			if err != nil {
				t.Fatalf("the decomposition failed to run, so the oracle has no reading: %v\n"+
					"  q: %s", err, decomposed)
			}
			if !mmEqRows(gotFull, gotDecomp) {
				t.Errorf("FULL JOIN does not equal its decomposition\n"+
					"  on         : %s\n  full       : %v\n  decomposed : %v\n  %s\n"+
					"  (both terms of the decomposition are LEFT JOINs, which have a Java "+
					"oracle and a large corpus — so a difference here is FULL's, unless LEFT "+
					"is broken in a way nothing else has noticed)",
					c.on, gotFull, gotDecomp, mmFirstDiff(gotFull, gotDecomp))
			}
		})
	}

	// `FULL JOIN` WITHOUT THE `OUTER` KEYWORD.
	//
	// SQL makes OUTER optional — `FULL [OUTER] JOIN` — and the grammar here
	// agrees: `(LEFT | RIGHT | FULL) OUTER? JOIN tableSourceItem`. But FULL is
	// also in the non-reserved keyword list, so it can be an ALIAS, and
	// `FROM a FULL JOIN b` is genuinely ambiguous: it can read as a full outer
	// join, or as `FROM a AS FULL JOIN b` — a plain inner join whose left table
	// has been renamed.
	//
	// The alias reading is the one that wins, and the qualified form gives it
	// away: `SELECT a.id … FROM a FULL JOIN b ON a.k = b.k` fails with
	// 42703 "column reference with qualifier A cannot be resolved", which is
	// precisely what happens when `a` has been renamed to FULL.
	//
	// That is a loud failure and therefore the lucky case. This arm exists for
	// the UNLUCKY one: a query that never qualifies with `a` has nothing to
	// fail on, so it parses, runs, and returns an INNER join under a name that
	// says FULL. The two spellings are compared directly, because a count
	// asserted by hand would only say one of them is wrong without saying
	// which reading produced it.
	t.Run("FULL JOIN without OUTER is not the same query", func(t *testing.T) {
		// An ON clause mentioning only `b`, so neither reading has a qualifier
		// to choke on and both spellings are accepted.
		const withOuter = "SELECT COUNT(*) FROM a FULL OUTER JOIN b ON b.k = 1"
		const withoutOuter = "SELECT COUNT(*) FROM a FULL JOIN b ON b.k = 1"

		outer, err := mmRows(t, ctx, db, withOuter)
		if err != nil {
			t.Fatalf("FULL OUTER JOIN failed: %v", err)
		}
		short, shortErr := mmRows(t, ctx, db, withoutOuter)

		t.Logf("MEASURED\n  %s -> %v (err %v)\n  %s -> %v (err %v)",
			withOuter, outer, err, withoutOuter, short, shortErr)

		if shortErr != nil {
			// A clean rejection is an acceptable outcome — it is the loud
			// direction, and it means no user can silently get the wrong join.
			return
		}
		if !mmEqRows(short, outer) {
			t.Errorf("`FULL JOIN` and `FULL OUTER JOIN` answer DIFFERENTLY, so the short "+
				"spelling is not a full outer join\n  FULL OUTER -> %v\n  FULL       -> %v\n"+
				"  SQL makes OUTER optional, so a user writing the short form gets a different "+
				"query than the one they wrote — silently, because this shape has no qualifier "+
				"to fail on. The alias reading (`a AS FULL`) turns it into an INNER join.",
				outer, short)
		}
	})

	// The three unmatched shapes, asserted by COUNT so a full join that had
	// quietly become an inner join — or a cross product — is caught by a number
	// rather than by someone reading a row list.
	t.Run("unmatched rows from both sides are present", func(t *testing.T) {
		counts := func(q string) int {
			rows, err := mmRows(t, ctx, db, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			return len(rows)
		}
		// a.k=7, a.k=NULL, b.k=9, b.k=NULL never match; k=1 is 2x2 and k=2 is
		// 1x1, so the matched part is 5 rows and the unmatched part is 4.
		const wantFull = 9
		if got := counts("SELECT a.id, b.id FROM a FULL OUTER JOIN b ON a.k = b.k"); got != wantFull {
			t.Errorf("FULL JOIN returned %d rows, want %d\n"+
				"  (5 is the matched part alone — an INNER join wearing FULL's name; "+
				"25 is the cross product; 7 or 8 means one side's unmatched rows, or the "+
				"NULL-keyed ones, were dropped)", got, wantFull)
		}
		// The two one-sided joins bracket it: FULL is at least as large as
		// either, and smaller than their sum (the matched part is not doubled).
		left := counts("SELECT a.id, b.id FROM a LEFT JOIN b ON a.k = b.k")
		right := counts("SELECT a.id, b.id FROM b LEFT JOIN a ON a.k = b.k")
		if wantFull < left || wantFull < right {
			t.Errorf("FULL (%d) is smaller than LEFT (%d) or RIGHT (%d), which no outer "+
				"join can be", wantFull, left, right)
		}
		if wantFull >= left+right {
			t.Errorf("FULL (%d) is not smaller than LEFT + RIGHT (%d + %d) — the matched "+
				"part is being counted twice", wantFull, left, right)
		}
	})
}
