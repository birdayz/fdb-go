package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// This file maps the SURFACE of one gate: semantic.Scope's qualified multi-match
// arm, which is what refuses a column reference through a duplicated FROM alias
// (SQLSTATE 42702). The gate is thin — it is the only thing standing between
// such a reference and the leg-window readers, which select a leg by matching
// the qualifier TEXT first-match. The loser of that first match is a real column
// of the same type, so a relaxation of the gate does not surface as an error. It
// surfaces as WRONG ROWS.
//
// Two facts about the gate were established by mutation and are worth stating
// here because both had been recorded wrongly:
//
//  1. The gate is ResolveQualifiedColumnNested's arm, NOT ResolveColumn's.
//     Deleting ResolveColumn's multi-match arm leaves every assertion in this
//     file green; only deleting the QUALIFIED arm reddens the 42702 group. The
//     unit-level witness for that distinction lives in the semantic package
//     (AmbiguousColumnError.Qualifier is populated by one arm and not the
//     other) — a control over deleted code has no runtime form otherwise.
//
//  2. The arm counts matches per COLUMN REFERENCE, not per ALIAS. So a
//     duplicated alias whose two sources carry DISJOINT columns produces one
//     match per reference and RESOLVES. That is Java's per-attribute rule, not
//     a hole: SemanticAnalyzer.lookup walks every operator's output and counts
//     candidates for the reference, and no alias-uniqueness check exists
//     anywhere in that class (its only DUPLICATE_ALIAS assert,
//     SemanticAnalyzer.java:180, is CTE-name lookup). Postgres rejects the
//     declaration outright; Go follows Java, and fifteen dup_from_alias_*
//     entries in the cross-engine corpus are live-verified on that basis.
//
// Fact 2 is why the ACCEPTED half of this file matters more than the refused
// half. Those queries return rows today, so the only thing keeping them honest
// is that per-attribute binding picks the RIGHT source. A regression to
// first-match-by-alias would still return rows — just wrong ones. Every accepted
// shape below therefore asserts VALUES, over legs whose value ranges are
// disjoint, so the leg a value came from is readable off the value itself.

// dupAliasSurfaceDB provisions the shared fixture. zn.id ∈ {1,2} and
// zp.pid ∈ {5,7,9} are disjoint ranges; zn.k runs WITH id while zp.w runs
// AGAINST pid, so neither a wrong leg nor a wrong column within a leg can tie.
func dupAliasSurfaceDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/dupaliassurface_" + tag
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE dupaliassurface_tmpl_"+tag+
		" CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT)"+
		" CREATE TABLE zn (id BIGINT, k BIGINT, n nst, arr BIGINT ARRAY, PRIMARY KEY (id))"+
		" CREATE TABLE zp (pid BIGINT, w BIGINT, m nst, PRIMARY KEY (pid))"+
		" CREATE TABLE zs (sid BIGINT, k BIGINT, PRIMARY KEY (sid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE dupaliassurface_tmpl_"+tag)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO zn VALUES (1, 100, (11, 12), [7, 8]), (2, 200, (21, 22), [9])")
	mustExec(t, db, ctx, "INSERT INTO zp VALUES (5, 90, (31, 32)), (7, 70, (41, 42)), (9, 50, (51, 52))")
	mustExec(t, db, ctx, "INSERT INTO zs VALUES (1, 111)")
	return db, ctx
}

// dupAliasQueryInts reads an all-BIGINT result. sorted=true canonicalises row order for
// queries with no ORDER BY, whose order is unspecified; ORDER BY cases pass
// false so the ORDER itself is asserted.
func dupAliasQueryInts(t *testing.T, db *sql.DB, ctx context.Context, q string, sorted bool) [][]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %q: %v", q, err)
	}
	var out [][]int64
	for rows.Next() {
		vals := make([]int64, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", q, err)
	}
	if sorted {
		sort.Slice(out, func(i, j int) bool {
			for c := range out[i] {
				if out[i][c] != out[j][c] {
					return out[i][c] < out[j][c]
				}
			}
			return false
		})
	}
	return out
}

func dupAliasSameRows(a, b [][]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// TestFDB_DuplicateFromAliasReachesTheQualifiedArm pins every shape that DOES
// reach the qualified multi-match arm. Each names a column carried by BOTH
// same-aliased sources, which is the only way a single reference produces two
// candidates.
func TestFDB_DuplicateFromAliasReachesTheQualifiedArm(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dupAliasSurfaceDB(t, "arm")

	for _, tc := range []struct{ name, query, reference string }{
		{"comma join, two base tables", "SELECT a.k FROM zn AS a, zs AS a", "A.K"},
		{"explicit JOIN", "SELECT a.k FROM zn AS a JOIN zs AS a ON a.k = a.k", "A.K"},
		{"self-join under one alias", "SELECT a.k FROM zn AS a, zn AS a", "A.K"},
		{"WHERE clause", "SELECT a.id FROM zn AS a, zs AS a WHERE a.k = 100", "A.K"},
		{"GROUP BY clause", "SELECT COUNT(*) FROM zn AS a, zs AS a GROUP BY a.k", "A.K"},
		{"ORDER BY clause", "SELECT a.id FROM zn AS a, zs AS a ORDER BY a.k", "A.K"},
		{"JOIN ... ON", "SELECT a.id FROM zn AS a JOIN zs AS a ON a.k > 0", "A.K"},
		// The alias collides with a BASE TABLE's own name rather than with
		// another AS. The unaliased source is visible under its table name, so
		// this is the same duplicate through a different door — and it is the
		// door that also registers a leg by its scan TABLE name downstream.
		{"alias collides with a base table name", "SELECT zn.k FROM zn, zs AS zn", "ZN.K"},
		// A CTE bound twice, and a derived table duplicated: the duplicate is
		// created by a synthesised source, not by two base tables.
		{"CTE bound twice", "WITH c AS (SELECT id FROM zn) SELECT c.id FROM c, c", "C.ID"},
		{"derived table duplicated", "SELECT d.k FROM (SELECT k FROM zn) AS d, (SELECT k FROM zs) AS d", "D.K"},
		// Quoted spellings that genuinely ARE the same alias.
		{"both aliases quoted, same case", `SELECT "a".k FROM zn AS "a", zs AS "a"`, "a.K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err == nil {
				n := 0
				for rows.Next() {
					n++
				}
				err = rows.Err()
				rows.Close()
				if err == nil {
					t.Fatalf("A REFERENCE THROUGH A DUPLICATED FROM ALIAS RESOLVED (%d rows).\n"+
						"  query: %s\n"+
						"  Both same-aliased sources carry that column, so a qualifier match\n"+
						"  has no honest answer. This arm is the WHOLE defence for the shape:\n"+
						"  the leg-window readers downstream select a leg by matching the\n"+
						"  qualifier text FIRST-MATCH, and the loser of that match is a real\n"+
						"  column of the same type. Relaxing this does not produce a loud\n"+
						"  downstream decline — it produces WRONG ROWS.", n, tc.query)
				}
			}
			if !strings.Contains(err.Error(), "42702") {
				t.Fatalf("duplicated FROM alias refused with the WRONG error.\n"+
					"  query: %s\n  got:   %v\n  want:  42702 (ambiguous_column).\n"+
					"  A refusal for an unrelated reason would let this pin pass while the\n"+
					"  ambiguity check itself is gone.", tc.query, err)
			}
			// Java renders the reference AS WRITTEN, qualifier included. Asserting
			// the QUALIFIED spelling is what shows the refusal came from the
			// qualified arm rather than the bare one — the bare arm would name the
			// column alone.
			if !strings.Contains(err.Error(), tc.reference) {
				t.Fatalf("the 42702 did not name the QUALIFIED reference.\n"+
					"  query: %s\n  got:   %v\n  want message containing: %s\n"+
					"  A bare-arm refusal names the column without its qualifier. If this\n"+
					"  starts failing, the gate moved to a different arm and the qualified\n"+
					"  path may no longer be guarded at all.", tc.query, err, tc.reference)
			}
		})
	}
}

// TestFDB_DuplicateFromAliasPerAttributeBindsTheRightLeg pins the arm's
// COMPLEMENT: references that a duplicated alias lets through because only one
// of the two sources carries the column.
//
// These queries RETURN ROWS today, deliberately and in agreement with Java. The
// risk they carry is not that they answer — it is that they might answer off the
// wrong leg. Every assertion below is on VALUES drawn from disjoint ranges, so a
// wrong-leg read is visible rather than merely a differing row count. An
// UNDUPLICATED control accompanies each, so a shared regression that changes
// both cannot pass by agreeing with itself.
func TestFDB_DuplicateFromAliasPerAttributeBindsTheRightLeg(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dupAliasSurfaceDB(t, "perattr")

	for _, tc := range []struct {
		name    string
		query   string
		control string // the same query with DISTINCT aliases
		ordered bool
		want    [][]int64
	}{
		{
			name:    "SELECT list spanning both legs",
			query:   "SELECT a.id, a.pid, a.k, a.w FROM zn AS a, zp AS a",
			control: "SELECT a.id, b.pid, a.k, b.w FROM zn AS a, zp AS b",
			want: [][]int64{
				{1, 5, 100, 90},
				{1, 7, 100, 70},
				{1, 9, 100, 50},
				{2, 5, 200, 90},
				{2, 7, 200, 70},
				{2, 9, 200, 50},
			},
		},
		{
			name:    "WHERE over both legs",
			query:   "SELECT a.k, a.w FROM zn AS a, zp AS a WHERE a.id = 2 AND a.pid = 9",
			control: "SELECT a.k, b.w FROM zn AS a, zp AS b WHERE a.id = 2 AND b.pid = 9",
			want:    [][]int64{{200, 50}},
		},
		{
			name:    "GROUP BY the second leg's column",
			query:   "SELECT a.pid, COUNT(*) FROM zn AS a, zp AS a GROUP BY a.pid",
			control: "SELECT b.pid, COUNT(*) FROM zn AS a, zp AS b GROUP BY b.pid",
			want:    [][]int64{{5, 2}, {7, 2}, {9, 2}},
		},
		{
			name:    "HAVING over the first leg's column",
			query:   "SELECT a.id, COUNT(*) FROM zn AS a, zp AS a GROUP BY a.id HAVING a.id > 1",
			control: "SELECT a.id, COUNT(*) FROM zn AS a, zp AS b GROUP BY a.id HAVING a.id > 1",
			want:    [][]int64{{2, 3}},
		},
		{
			name:    "ORDER BY the second leg's column",
			query:   "SELECT a.pid FROM zn AS a, zp AS a ORDER BY a.pid DESC",
			control: "SELECT b.pid FROM zn AS a, zp AS b ORDER BY b.pid DESC",
			ordered: true,
			want:    [][]int64{{9}, {9}, {7}, {7}, {5}, {5}},
		},
		{
			name:    "ORDER BY the first leg's column",
			query:   "SELECT a.id FROM zn AS a, zp AS a ORDER BY a.id DESC",
			control: "SELECT a.id FROM zn AS a, zp AS b ORDER BY a.id DESC",
			ordered: true,
			want:    [][]int64{{2}, {2}, {2}, {1}, {1}, {1}},
		},
		{
			name:    "JOIN ... ON correlating the two legs",
			query:   "SELECT a.id, a.pid FROM zn AS a JOIN zp AS a ON a.id + 4 = a.pid",
			control: "SELECT a.id, b.pid FROM zn AS a JOIN zp AS b ON a.id + 4 = b.pid",
			want:    [][]int64{{1, 5}},
		},
		{
			// The alias collides with a BASE TABLE's own name. This door also
			// feeds the downstream leg registration keyed by scan TABLE name, so
			// a wrong-leg read here has a second route in.
			name:    "alias collides with a base table name",
			query:   "SELECT zn.k, zn.w FROM zn, zp AS zn WHERE zn.id = 2 AND zn.pid = 9",
			control: "SELECT zn.k, b.w FROM zn, zp AS b WHERE zn.id = 2 AND b.pid = 9",
			want:    [][]int64{{200, 50}},
		},
		{
			name:    "alias collides with a base table name, ORDER BY the aliased leg",
			query:   "SELECT zn.pid FROM zn, zp AS zn ORDER BY zn.pid DESC",
			control: "SELECT b.pid FROM zn, zp AS b ORDER BY b.pid DESC",
			ordered: true,
			want:    [][]int64{{9}, {9}, {7}, {7}, {5}, {5}},
		},
		{
			name:    "duplicate created by DERIVED TABLES",
			query:   "SELECT a.id FROM (SELECT id, k FROM zn) AS a, (SELECT pid, w FROM zp) AS a WHERE a.pid = 9",
			control: "SELECT a.id FROM (SELECT id, k FROM zn) AS a, (SELECT pid, w FROM zp) AS b WHERE b.pid = 9",
			want:    [][]int64{{1}, {2}},
		},
		{
			name:    "duplicate created by CTEs, ORDER BY the second leg",
			query:   "WITH c1 AS (SELECT id FROM zn), c2 AS (SELECT pid FROM zp) SELECT a.pid FROM c1 AS a, c2 AS a ORDER BY a.pid DESC",
			control: "WITH c1 AS (SELECT id FROM zn), c2 AS (SELECT pid FROM zp) SELECT b.pid FROM c1 AS a, c2 AS b ORDER BY b.pid DESC",
			ordered: true,
			want:    [][]int64{{9}, {9}, {7}, {7}, {5}, {5}},
		},
		{
			// Unquoted aliases fold, so `a` and `A` ARE the same alias and this is
			// a genuine duplicate resolved per-attribute.
			name:    "case-varied unquoted aliases are one alias",
			query:   "SELECT A.id, A.pid FROM zn AS a, zp AS A",
			control: "SELECT a.id, b.pid FROM zn AS a, zp AS b",
			want:    [][]int64{{1, 5}, {1, 7}, {1, 9}, {2, 5}, {2, 7}, {2, 9}},
		},
		{
			name:    "both aliases quoted alike",
			query:   `SELECT "a".id, "a".pid FROM zn AS "a", zp AS "a"`,
			control: "SELECT a.id, b.pid FROM zn AS a, zp AS b",
			want:    [][]int64{{1, 5}, {1, 7}, {1, 9}, {2, 5}, {2, 7}, {2, 9}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dupAliasQueryInts(t, db, ctx, tc.query, !tc.ordered)
			if !dupAliasSameRows(got, tc.want) {
				t.Fatalf("A DUPLICATED-ALIAS REFERENCE BOUND THE WRONG LEG.\n"+
					"  query: %s\n  got:   %v\n  want:  %v\n"+
					"  Only one of the two same-aliased sources carries each referenced\n"+
					"  column, so Java's per-attribute rule gives exactly one candidate and\n"+
					"  the query is expected to ANSWER. What is at stake is WHICH leg it\n"+
					"  answers from: the leg value ranges here are disjoint, so a differing\n"+
					"  value names a wrong-leg read directly. This is the failure the 42702\n"+
					"  arm cannot catch, because a per-COLUMN count of 1 never reaches it.",
					tc.query, got, tc.want)
			}
			ctl := dupAliasQueryInts(t, db, ctx, tc.control, !tc.ordered)
			if !dupAliasSameRows(ctl, tc.want) {
				t.Fatalf("THE UNDUPLICATED CONTROL DISAGREES WITH THE EXPECTATION.\n"+
					"  control: %s\n  got:     %v\n  want:    %v\n"+
					"  The control shares no alias, so it cannot be affected by duplicate-\n"+
					"  alias resolution at all. Its disagreeing means the expectation above\n"+
					"  is stale for an unrelated reason and the duplicate-alias arm of this\n"+
					"  test would otherwise have been re-blessed against a moved baseline.",
					tc.control, ctl, tc.want)
			}
		})
	}
}

// TestFDB_DuplicateQualifierShapesCaughtByAnotherGate pins the shapes that never
// reach the qualified multi-match arm, together with the gate that DOES catch
// them. Without these, retiring one of those gates would look like it changed
// nothing while silently handing its shape to a first-match reader.
func TestFDB_DuplicateQualifierShapesCaughtByAnotherGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dupAliasSurfaceDB(t, "othergate")

	for _, tc := range []struct{ name, query, wantCode, why string }{
		{
			name:     "UNNEST element alias collides with a leg alias",
			query:    "SELECT a.id FROM zn AS a, zp AS a, a.arr AS a",
			wantCode: "42712",
			why: "Caught at DECLARATION by Scope.AddSource's shadowing arm, not at\n" +
				"  resolution: a lateral-unnest binding SHADOWS same-named columns, so a\n" +
				"  duplicate involving one cannot be adjudicated per-attribute the way a\n" +
				"  plain duplicate can. Java forbids the duplicate unnest alias outright\n" +
				"  (RFC-142). If this stops being 42712, the shape falls through to the\n" +
				"  per-attribute path, where a shadowing source silently outranks a real\n" +
				"  column of the same name — wrong rows, not an error.",
		},
		{
			name:     "three-segment reference through a duplicated qualifier",
			query:    "SELECT a.n.sk FROM zn AS a, zp AS a",
			wantCode: "42703",
			why: "Three-segment paths do not resolve on this route at all yet — the\n" +
				"  reference is refused before any duplicate-alias question is asked, and\n" +
				"  the UNDUPLICATED control below refuses identically, which is what shows\n" +
				"  the refusal is about arity rather than about the duplicate.\n" +
				"  THIS IS A TRIPWIRE. Work to make three-segment paths resolve is in\n" +
				"  flight; when it lands this shape becomes reachable and must be routed\n" +
				"  through the qualified multi-match arm (`a` is carried by two sources)\n" +
				"  rather than resolved first-match. Do not simply update the expected\n" +
				"  code here — add the shape to the 42702 group and prove it with a\n" +
				"  mutation.",
		},
		{
			name:     "three-segment reference, UNDUPLICATED control",
			query:    "SELECT a.n.sk FROM zn AS a, zp AS b",
			wantCode: "42703",
			why: "The control for the tripwire above. It shares no alias, so if it ever\n" +
				"  diverges from the duplicated form, three-segment resolution has landed\n" +
				"  and the duplicated form needs the 42702 treatment.",
		},
		{
			name:     "quoted lowercase beside unquoted: NOT a duplicate",
			query:    `SELECT a.id, a.pid FROM zn AS "a", zp AS a`,
			wantCode: "42703",
			why: "`\"a\"` is case-preserved and `a` folds to `A`, so these are two\n" +
				"  DIFFERENT aliases and no duplicate exists. The unqualified reference\n" +
				"  folds to `A`, matching only the zp leg, which has no ID. If this ever\n" +
				"  becomes 42702, quote-awareness collapsed and two distinct aliases are\n" +
				"  being conflated — the AddSource fold-collision guard's territory.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err == nil {
				n := 0
				for rows.Next() {
					n++
				}
				err = rows.Err()
				rows.Close()
				if err == nil {
					t.Fatalf("A SHAPE THAT NO GATE ADJUDICATES RETURNED %d ROWS.\n"+
						"  query: %s\n  expected gate: %s\n%s", n, tc.query, tc.wantCode, tc.why)
				}
			}
			if !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("THE FIRST-CATCHING GATE FOR THIS SHAPE CHANGED.\n"+
					"  query: %s\n  got:   %v\n  want:  %s\n%s", tc.query, err, tc.wantCode, tc.why)
			}
		})
	}
}
