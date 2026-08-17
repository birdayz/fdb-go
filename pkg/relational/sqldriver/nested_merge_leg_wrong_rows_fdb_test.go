package sqldriver_test

// RFC-200 gate (a): the end-to-end row probe for the merged-leg EXISTS, and the
// measured reason its four mutation directions are still not writable.
//
// WHAT GATE (a) ASKS FOR: an end-to-end WRONG-ROWS probe over a multi-leg EXISTS
// whose merged row is addressed through a NESTED leg window — distinct leg
// widths, a duplicate column name across legs, and the projected value drawn
// from a non-first leg at a non-zero leg-local ordinal.
//
// THE SHAPE IS A *PROJECTED*-EXISTS FOLD OVER THREE SOURCES WITH EQUIJOINS, and
// every clause of that was arrived at by measurement rather than reading:
//
//   - PROJECTED, not WHERE-EXISTS. foldStep1Seed's condition (2) declines a
//     plain WHERE-EXISTS as `rv-no-exist-ref` — its projection sits ABOVE the
//     existential level, so there is nothing to fold — and no step-1 seed is
//     built at all. Measured: with a WHERE-EXISTS fixture all four mutation
//     directions stayed GREEN.
//   - THREE sources, so one step-1 leg is the COLLAPSED PAIR whose result value
//     is the positional merge. Two sources give flat windows throughout.
//   - WITH EQUIJOINS. A predicate-free comma join does not plan through this arm
//     at all; it fails loudly, before and after the nested acceptance. That is a
//     separate pre-existing defect, pinned below in its own test.
//
// WHAT IS STILL NOT MEASURABLE, and why. Even on this shape the NESTED READER
// ARM IS NOT ENTERED. Measured directly: the seed-window reader census reports
// `NESTED-HIT 0` at both keyed readers over the whole corpus, and mutating the
// fused two-step address back to flat `Offset + legOrdinal` leaves this test
// GREEN. The nested acceptance is live at the LAYOUT (the foldStep1Seed census
// moved 78→138 ACCEPTs exactly as predicted, and existentialRebase grew 962 →
// 1086, which is RFC-200 §6's own prediction confirmed) — but every window those
// reads select is a FLAT top-level run. A nested SUB-window is only selected by
// a reference to a leg BURIED INSIDE the merge, and no corpus query, this
// fixture included, produces one.
//
// So gate (a)'s four directions are unmeasurable for a reason quite different
// from the one first recorded here: not a query that cannot run, but a reader
// arm no query reaches. The rows below are still worth asserting — they pin that
// the merged-leg EXISTS answers correctly on all four addresses — and the four
// directions become writable the moment `NESTED-HIT` is non-zero.
//
// THE FIXTURE'S THREE DELIBERATE PROPERTIES, each defeating a way a probe could
// pass while broken:
//
//  1. DISTINCT LEG WIDTHS (TA 3 columns, TB 2, TC 4). Under a flat reading the
//     address is `Offset + legOrdinal`; under the nested one it is "slot Offset,
//     then descend legOrdinal". With equal widths those coincide; with these
//     they land on different columns.
//  2. A DUPLICATE COLUMN NAME ACROSS LEGS (`K` in both TA and TC). A wrong
//     ordinal a NAME fallback could rescue is not a wrong-rows probe — it is a
//     probe of the fallback.
//  3. THE PROJECTED VALUE FROM A NON-FIRST LEG AT A NON-ZERO LEG-LOCAL ORDINAL
//     (`TC.K`). At leg 0 / ordinal 0 every addressing scheme agrees and the
//     probe would pass with the defect fully present.
//
// EVERY ASSERTED ADDRESS IS A NON-KEY COLUMN WITH A DISJOINT VALUE RANGE — TA.K
// 101-102, TA.AV 201-202, TB.BV 301-302, TC.K 901-902 — so any wrong column read
// shows as a wrong VALUE and never as a coincidentally equal one.
//
// THE JOIN KEYS ARE DELIBERATELY NOT ADDRESSED. TA.AID, TB.BID and TC.CID must
// all share the range 1-2 for the equijoin to match anything, so an assertion on
// one of them is pinned BY NAME and not by value: a window misreading TB.BID as
// TA.AID or TC.CID returns byte-identical rows and the probe passes with the
// defect present. An earlier version of this fixture addressed the middle leg
// through TB.BID for exactly that reason — TB had no other column — which is why
// TB now carries BV. This is the same trap as property 3 one level over: an
// address whose wrong answers are indistinguishable from its right ones is not
// an address the probe can use.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// twoSourceFoldWant is the TWO-source control's answer, stated independently of
// any plan: TA x TC is 2 x 2 = 4 rows, each projecting TC.K and whether TA.AID
// appears in TP.OWNER. TP holds owner=1 only.
//
// Two sources means no collapsed pair and therefore FLAT windows throughout, so
// this arm does NOT exercise the nested address. It is here as the positive
// control that separates "the projected-EXISTS fold is broken in general" from
// "it is broken at three sources" — and it answers correctly, which is what
// makes the three-source failure specific.
var twoSourceFoldWant = []string{"901|true", "902|false"}

// nestedMergeWant is the THREE-source probe's answer, stated INDEPENDENTLY of
// any plan.
//
// The equijoin chains TA.AID = TB.BID = TC.CID, so the two matching triples are
// (aid 1, bid 1, cid 1) and (aid 2, bid 2, cid 2). Each projects TC.K — 901 and
// 902 — and whether TA.AID appears in TP.OWNER, which holds owner=1 only.
//
// A wrong window is visible in EITHER column: mis-reading the projection shows
// TA.K (101/102) or TA.AV (201/202) instead of 901/902, and mis-reading the
// correlated comparand compares TP.OWNER against a TA column that is never 1, so
// the flag goes false everywhere.
var nestedMergeWant = []string{"901|true", "902|false"}

func TestFDB_NestedMergeLegProjectedExistsFold(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nested_merge_leg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nested_merge_leg")
	// Widths 3 / 2 / 4 — all DISTINCT — and K declared in BOTH ta and tc. See the
	// header for why each is load-bearing.
	//
	// EVERY LEG CARRIES A NON-KEY COLUMN WITH ITS OWN DISJOINT VALUE RANGE, and
	// that is the correction the equijoin re-point forced. The join keys AID/BID/
	// CID must share the range 1-2 for the equijoin to match anything, so an
	// address pinned on a JOIN KEY is pinned by name and not by value: a window
	// misreading TB.BID as TA.AID or TC.CID returns identical rows. Giving TB a
	// BV column (301-302) means all four asserted addresses discriminate.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nested_merge_leg "+
			"CREATE TABLE ta (aid BIGINT, k BIGINT, av BIGINT, PRIMARY KEY (aid)) "+
			"CREATE TABLE tb (bid BIGINT, bv BIGINT, PRIMARY KEY (bid)) "+
			"CREATE TABLE tc (cid BIGINT, k BIGINT, cv BIGINT, cw BIGINT, PRIMARY KEY (cid)) "+
			"CREATE TABLE tp (pid BIGINT, owner BIGINT, PRIMARY KEY (pid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nested_merge_leg/s WITH TEMPLATE nested_merge_leg")
	dsn := fmt.Sprintf("fdbsql:///testdb_nested_merge_leg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO ta (aid, k, av) VALUES (1, 101, 201), (2, 102, 202)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tb (bid, bv) VALUES (1, 301), (2, 302)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tc (cid, k, cv, cw) VALUES (1, 901, 951, 971), (2, 902, 952, 972)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tp (pid, owner) VALUES (401, 1)")

	// scanPairs reads (value, flag) rows as "value|flag", so a wrong COLUMN and a
	// wrong EXISTS answer are both visible in one comparison.
	scanPairs := func(q string) ([]string, error) {
		rows, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			return nil, qErr
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v sql.NullInt64
			var f sql.NullBool
			if sErr := rows.Scan(&v, &f); sErr != nil {
				return nil, sErr
			}
			out = append(out, fmt.Sprintf("%s|%s", nestedMergeInt(v), nestedMergeBool(f)))
		}
		if rErr := rows.Err(); rErr != nil {
			return nil, rErr
		}
		sort.Strings(out)
		return out, nil
	}

	// THE CONTROL: two sources, flat windows, and a correct answer. Without it a
	// failure below would be consistent with the whole projected-EXISTS fold
	// being broken, and would say nothing about the merged leg.
	t.Run("two sources — flat windows, correct rows", func(t *testing.T) {
		got, err := scanPairs(
			`SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) FROM ta, tc ` +
				`WHERE ta.aid = tc.cid`)
		if err != nil {
			t.Fatalf("the TWO-source projected-EXISTS fold failed to execute: %v\n"+
				"  This is the control. If it is broken too, the three-source assertion "+
				"below is not about the merged leg at all and the whole diagnosis has to "+
				"be re-made.", err)
		}
		if strings.Join(got, ",") != strings.Join(twoSourceFoldWant, ",") {
			t.Fatalf("two-source fold returned %v, want %v — the correlated comparand "+
				"TA.AID resolves through a FLAT leg window here, so a wrong answer means "+
				"the flat path moved.", got, twoSourceFoldWant)
		}
	})

	// THE PROBE: THREE sources, so one step-1 leg is the COLLAPSED PAIR whose
	// result value is the positional merge — the shape that reaches the nested
	// window.
	//
	// THE EQUIJOIN PREDICATES ARE LOAD-BEARING, and their absence is what made an
	// earlier version of this fixture useless. A predicate-free comma join
	// (`FROM ta, tb, tc` with no WHERE) does not plan through this arm at all — it
	// fails LOUDLY with "multi-leg row cannot serve a source-relative ordinal / no
	// frontier row resolved", before AND after the nested acceptance, so all four
	// mutation directions stayed green on it. The corpus's own shape for this arm
	// carries equijoins, and with them the query plans and runs.
	const q = `SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) ` +
		`FROM ta, tb, tc WHERE ta.aid = tb.bid AND tb.bid = tc.cid`

	got, err := scanPairs(q)
	if err != nil {
		t.Fatalf("the THREE-source projected-EXISTS fold failed to execute: %v\n"+
			"  This gate asserts ROWS; an execution error means the query never reached "+
			"the layout the probe exists to test.", err)
	}
	if strings.Join(got, ",") != strings.Join(nestedMergeWant, ",") {
		t.Fatalf("the projected-EXISTS fold over a merged leg returned\n  %v\nwant\n  %v\n"+
			"  TC.K lives at leg-local ordinal 1 of the LAST leg and its name is shared\n"+
			"  with TA.K, so a wrong window resolves to a real column with a different\n"+
			"  value and no name lookup can repair it. The value ranges are disjoint by\n"+
			"  construction (TA.AID 1-2, TA.K 101-102, TA.AV 201-202, TB.BID 1-2,\n"+
			"  TC.CID 1-2, TC.K 901-902), so the values returned name the column\n"+
			"  actually read, and the FLAG names the column the correlated comparand\n"+
			"  actually read.", got, nestedMergeWant)
	}

	// THE COMPANION READS, so a layout that happens to be right for one address
	// and wrong for the rest cannot pass.
	for _, tc := range []struct {
		name string
		q    string
		want []string
	}{
		{
			name: "the DUPLICATE name's other occurrence, first leg",
			q: `SELECT ta.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) ` +
				`FROM ta, tb, tc WHERE ta.aid = tb.bid AND tb.bid = tc.cid`,
			want: []string{"101|true", "102|false"},
		},
		{
			name: "the first leg's LAST column, past the duplicate",
			q: `SELECT ta.av, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) ` +
				`FROM ta, tb, tc WHERE ta.aid = tb.bid AND tb.bid = tc.cid`,
			want: []string{"201|true", "202|false"},
		},
		{
			// The MIDDLE leg, addressed through its NON-KEY column. TB.BID would
			// have been the obvious choice and is useless here: it shares the range
			// 1-2 with TA.AID and TC.CID by equijoin necessity, so a window
			// misreading it returns identical rows. BV (301-302) discriminates.
			name: "the MIDDLE leg's non-key column",
			q: `SELECT tb.bv, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) ` +
				`FROM ta, tb, tc WHERE ta.aid = tb.bid AND tb.bid = tc.cid`,
			want: []string{"301|true", "302|false"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanPairs(tc.q)
			if err != nil {
				t.Fatalf("%s: query failed: %v", tc.name, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("%s:\n  got  %v\n  want %v\n"+
					"  The layout is right for one address and wrong for this one, which is "+
					"exactly what a single-address probe cannot see.", tc.name, got, tc.want)
			}
		})
	}
}

// The PREDICATE-FREE comma join is a SEPARATE, pre-existing defect, pinned so it
// does not evaporate — and so the reason the probe above carries equijoins stays
// on the record.
//
// `SELECT tc.k, EXISTS (...) FROM ta, tb, tc` with NO join predicate fails
// loudly. Measured with the nested acceptance DISABLED as well (by disabling
// legOrdinalSafety's FlatMap arm, the one line that activates it): same error,
// same message. Pre-existing, not an RFC-200 regression, and LOUD rather than
// silent — no wrong row reaches a user.
func TestFDB_PredicateFreeCommaJoinProjectedExistsFailsLoud(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nested_merge_nopred")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nested_merge_nopred")
	// The SAME shape as the probe above, so the only difference between the two
	// tests is the presence of the join predicates.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nested_merge_nopred "+
			"CREATE TABLE ta (aid BIGINT, k BIGINT, av BIGINT, PRIMARY KEY (aid)) "+
			"CREATE TABLE tb (bid BIGINT, bv BIGINT, PRIMARY KEY (bid)) "+
			"CREATE TABLE tc (cid BIGINT, k BIGINT, cv BIGINT, cw BIGINT, PRIMARY KEY (cid)) "+
			"CREATE TABLE tp (pid BIGINT, owner BIGINT, PRIMARY KEY (pid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nested_merge_nopred/s WITH TEMPLATE nested_merge_nopred")
	dsn := fmt.Sprintf("fdbsql:///testdb_nested_merge_nopred?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO ta (aid, k, av) VALUES (1, 101, 201)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tb (bid, bv) VALUES (1, 301)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tc (cid, k, cv, cw) VALUES (1, 901, 951, 971)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tp (pid, owner) VALUES (401, 1)")

	delivered := 0
	rows, qErr := db.QueryContext(ctx,
		`SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) FROM ta, tb, tc`)
	if qErr == nil {
		defer rows.Close()
		for rows.Next() {
			delivered++
		}
		qErr = rows.Err()
	}
	if qErr == nil {
		t.Fatal("the PREDICATE-FREE three-source projected-EXISTS fold now EXECUTES.\n" +
			"  That is good news: delete this pin and assert its rows. It was failing\n" +
			"  loudly before AND after RFC-200's nested acceptance, so its repair is\n" +
			"  independent of that work.")
	}
	// The pin records the failure CLASS, not a message. It is LOUD that makes
	// this defect survivable — no wrong row reaches a user — and the two
	// properties that establish it are that the query fails and that it hands
	// back nothing on the way.
	//
	// The internal WORDING is deliberately not asserted. It has already moved
	// once for a reason that changed nothing about the defect: the read used to
	// die naming the context that could not serve it ("multi-leg row cannot
	// serve a source-relative ordinal" / "no frontier row resolved"), and under
	// the ordinal layout the same shape is refused one level earlier, at the
	// carrier ("row type and layout carrier type disagree"). Pinning either
	// spelling makes this test report a wording change as a defect change.
	if delivered != 0 {
		t.Fatalf("the predicate-free fold delivered %d row(s) before failing with %v.\n"+
			"  Rows escaping ahead of the failure is the SILENT half this pin exists to "+
			"exclude — a partial answer is a wrong answer.", delivered, qErr)
	}
	// A 42-class failure would mean the fixture drifted — the query stopped
	// being the shape under test and started being rejected as malformed —
	// which would make the two assertions above pass for the wrong reason.
	if strings.Contains(qErr.Error(), "SQLSTATE 42") || strings.Contains(qErr.Error(), "42703") {
		t.Fatalf("the predicate-free fold was rejected as a malformed query: %v\n"+
			"  The schema or the SELECT list has drifted; this pin no longer describes "+
			"the three-source projected-EXISTS fold.", qErr)
	}
}

func nestedMergeInt(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", v.Int64)
}

func nestedMergeBool(b sql.NullBool) string {
	if !b.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%t", b.Bool)
}
