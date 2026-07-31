package sqldriver_test

// RFC-200 gate (a) — and the BLOCKER that stops it being satisfiable today.
//
// WHAT THIS FILE RECORDS. RFC-200's gate (a) asks for an end-to-end WRONG-ROWS
// probe over a multi-leg EXISTS whose merged row is addressed through a NESTED
// leg window: distinct leg widths, a duplicate column name across legs, and the
// projected value drawn from a non-first leg at a non-zero leg-local ordinal.
// The shape that reaches that window is a PROJECTED-EXISTS fold over a
// THREE-source FROM — the EXISTS in the SELECT list (a plain WHERE-EXISTS is
// declined by foldStep1Seed's condition (2) as `rv-no-exist-ref`, so it never
// builds a step-1 seed at all), and three sources so that one step-1 leg is the
// collapsed pair whose result value is the positional merge.
//
// MEASURED: that query does not execute, and it does not execute BEFORE the
// nested window is activated either. Both runs fail identically and LOUDLY:
//
//	correlated FieldValue "K" (correlation "TC") evaluated against an
//	unbound/unrecognized context (*RowEvalContext (multi-leg row cannot serve a
//	source-relative ordinal)) — no frontier row resolved (planner/executor bug)
//
// The comparison was made by disabling legOrdinalSafety's FlatMap arm — the one
// line that activates the nested acceptance — and re-running: same error, same
// message. So this is a PRE-EXISTING defect in the projected-EXISTS fold over a
// 3-source FROM, not a regression introduced by RFC-200, and it is LOUD rather
// than silent: no wrong row reaches a user.
//
// It is pinned here rather than written up and dropped, because the fact it
// establishes is load-bearing in two directions. It is why gate (a) is NOT
// satisfied — the four mutation directions cannot be measured on a query that
// cannot run — and it is the thing whose repair immediately makes them
// measurable. The moment this shape executes, this test goes red and the
// wrong-rows probe below it becomes writable.
//
// THE FIXTURE'S THREE DELIBERATE PROPERTIES survive for that day, each defeating
// a way a probe could pass while broken:
//
//  1. DISTINCT LEG WIDTHS (TA 3 columns, TB 1, TC 2). Under a flat reading the
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
// The value ranges are disjoint by construction (TA.AID 1-2, TA.K 101-102,
// TA.AV 201-202, TB.BID 301-302, TC.CID 801-802, TC.K 901-902) so any wrong
// column read shows as a wrong VALUE, never as a coincidentally equal one.

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
var twoSourceFoldWant = []string{"901|false", "901|true", "902|false", "902|true"}

func TestFDB_NestedMergeLegProjectedExistsFold(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nested_merge_leg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nested_merge_leg")
	// Widths 3 / 1 / 2, and K declared in BOTH ta and tc — see the header for
	// why each of those is load-bearing.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nested_merge_leg "+
			"CREATE TABLE ta (aid BIGINT NOT NULL, k BIGINT, av BIGINT, PRIMARY KEY (aid)) "+
			"CREATE TABLE tb (bid BIGINT NOT NULL, PRIMARY KEY (bid)) "+
			"CREATE TABLE tc (cid BIGINT NOT NULL, k BIGINT, PRIMARY KEY (cid)) "+
			"CREATE TABLE tp (pid BIGINT NOT NULL, owner BIGINT, PRIMARY KEY (pid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nested_merge_leg/s WITH TEMPLATE nested_merge_leg")
	dsn := fmt.Sprintf("fdbsql:///testdb_nested_merge_leg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO ta (aid, k, av) VALUES (1, 101, 201), (2, 102, 202)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tb (bid) VALUES (301), (302)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tc (cid, k) VALUES (801, 901), (802, 902)")
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

	// THE CONTROL: two sources, flat windows, and a correct answer. Without it
	// the negative below would be consistent with the whole projected-EXISTS fold
	// being broken, and it would say nothing about the merged leg.
	t.Run("two sources — flat windows, correct rows", func(t *testing.T) {
		got, err := scanPairs(
			`SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) FROM ta, tc`)
		if err != nil {
			t.Fatalf("the TWO-source projected-EXISTS fold failed to execute: %v\n"+
				"  This is the control. If it is broken too, the three-source negative "+
				"below is not about the merged leg at all and the whole diagnosis has to "+
				"be re-made.", err)
		}
		if strings.Join(got, ",") != strings.Join(twoSourceFoldWant, ",") {
			t.Fatalf("two-source fold returned %v, want %v — the correlated comparand "+
				"TA.AID resolves through a FLAT leg window here, so a wrong answer means "+
				"the flat path moved.", got, twoSourceFoldWant)
		}
	})

	// THE PINNED NEGATIVE, and what it re-arms.
	t.Run("three sources — the merged leg shape does not execute", func(t *testing.T) {
		_, err := scanPairs(
			`SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) FROM ta, tb, tc`)
		if err == nil {
			t.Fatal("the THREE-source projected-EXISTS fold now EXECUTES.\n" +
				"  That is good news and it makes this test obsolete in the best way: the\n" +
				"  shape that reaches the NESTED merge-leg window is now runnable, so\n" +
				"  RFC-200's gate (a) is finally measurable. Replace this negative with the\n" +
				"  wrong-rows probe it is standing in for — assert the eight rows, then\n" +
				"  mutate FOUR directions separately:\n" +
				"    1. a nested window read as flat (Offset + legOrdinal);\n" +
				"    2. a flat window read as nested (the fused two-step);\n" +
				"    3. leg ORIENTATION swapped relative to the seed's baked layout —\n" +
				"       record the OBSERVED failure mode; the site claims an ordinal read\n" +
				"       there is 'either right or loud', and silently wrong rows would\n" +
				"       REFUTE that claim and are a finding in their own right;\n" +
				"    4. the census-invisible bare-column arm in query.slotInGatheredSeed\n" +
				"       allowed to contribute a hits++ for a nested window.\n" +
				"  The fixture above already has the three properties those directions\n" +
				"  need: distinct leg widths, a duplicate column name across legs, and a\n" +
				"  projection from a non-first leg at a non-zero leg-local ordinal.")
		}
		// LOUD, not silently wrong. That distinction is the whole reason this is a
		// pinned negative rather than an open wrong-rows defect: no user gets a
		// wrong answer from this shape, they get an error.
		if !strings.Contains(err.Error(), "no frontier row resolved") {
			t.Fatalf("the three-source fold failed with an UNEXPECTED error: %v\n"+
				"  The measured failure is a LOUD unbound-correlation error ('multi-leg row "+
				"cannot serve a source-relative ordinal ... no frontier row resolved'), and "+
				"it is loud that makes this defect survivable. A different failure — and "+
				"above all a SILENT one — changes what this pin is recording.", err)
		}
	})
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
