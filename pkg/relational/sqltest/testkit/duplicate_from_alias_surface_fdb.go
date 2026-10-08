package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// READ THIS BEFORE "FIXING" ANYTHING HERE.
//
// A duplicated FROM alias whose two sources carry DISJOINT columns is ACCEPTED,
// and the queries in TestFDB_DuplicateFromAliasPerAttributeBindsTheRightLeg
// RETURN ROWS on purpose. That is DELIBERATE JAVA PARITY, not an oversight and
// not a hole waiting to be closed.
//
// Java resolves a reference by counting candidates PER REFERENCE, not per alias:
// SemanticAnalyzer.lookup walks every operator's output attribute and appends one
// candidate per match, and resolveIdentifier rejects only when that list holds
// more than one entry. Two operators both named `a`, one carrying `id` and the
// other `pid`, therefore yield exactly ONE candidate for `a.id` and one for
// `a.pid`, and both resolve. No alias-uniqueness check exists anywhere in that
// class — its only DUPLICATE_ALIAS assert, SemanticAnalyzer.java:180, is CTE-name
// lookup, which is a different thing entirely.
//
// POSTGRES DIVERGES, and that is context rather than a defect report. Postgres
// rejects the FROM clause itself with 42712 even when nothing references the
// alias. This is the SHARED query surface — an input Java also attempts — where
// the conformance rule is parity with Java, not "whichever engine reads as more
// principled". Adopting the Postgres rule would break fifteen live-verified
// dup_from_alias_* entries in the cross-engine corpus, among them
// dup_from_alias_order_by_second_leg, whose multi-row second leg would expose a
// wrong-leg read — the corpus already tests the failure a declaration-time check
// would claim to prevent, and it passes.
//
// So: if you arrived here from Postgres semantics intending to reject the
// duplicate at declaration time, that change is a SPEC VIOLATION. It needs an
// owner ruling and a corpus re-verification against a live Java server, not a
// green local suite.
//
// ─────────────────────────────────────────────────────────────────────────────
//
// With that settled, this file maps the SURFACE of one gate: semantic.Scope's
// qualified multi-match arm, which refuses a reference that a duplicated alias
// makes genuinely ambiguous (SQLSTATE 42702). The gate is thin — it is the only
// thing standing between such a reference and the leg-window readers, which
// select a leg by matching the qualifier TEXT first-match. The loser of that
// first match is a real column of the same type, so a relaxation of the gate does
// not surface as an error. It surfaces as WRONG ROWS.
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
//  2. The arm counts matches per COLUMN REFERENCE, not per ALIAS — the Java rule
//     spelled out above. This is what makes the disjoint-column case resolve
//     rather than error.
//
// Fact 2 is why the ACCEPTED half of this file matters more than the refused
// half. Those queries return rows today, so the only thing keeping them honest
// is that per-attribute binding picks the RIGHT source. A regression to
// first-match-by-alias would still return rows — just wrong ones. Every accepted
// shape below therefore asserts VALUES, over legs whose value ranges are
// disjoint, so the leg a value came from is readable off the value itself, and
// each sits beside an UNDUPLICATED control so a shared regression cannot pass by
// agreeing with itself.

// dupAliasSurfaceDB provisions the shared fixture. zn.id ∈ {1,2} and
// zp.pid ∈ {5,7,9} are disjoint ranges; zn.k runs WITH id while zp.w runs
// AGAINST pid, so neither a wrong leg nor a wrong column within a leg can tie.
func DupAliasSurfaceDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/FRL/dupaliassurface_" + tag
	setup := OpenDB(t, dbPath)
	MustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE dupaliassurface_tmpl_"+tag+
		" CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT)"+
		" CREATE TABLE zn (id BIGINT, k BIGINT, n nst, arr BIGINT ARRAY, PRIMARY KEY (id))"+
		" CREATE TABLE zp (pid BIGINT, w BIGINT, m nst, PRIMARY KEY (pid))"+
		" CREATE TABLE zs (sid BIGINT, k BIGINT, PRIMARY KEY (sid))")
	MustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE dupaliassurface_tmpl_"+tag)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	MustExec(t, db, ctx, "INSERT INTO zn VALUES (1, 100, (11, 12), [7, 8]), (2, 200, (21, 22), [9])")
	MustExec(t, db, ctx, "INSERT INTO zp VALUES (5, 90, (31, 32)), (7, 70, (41, 42)), (9, 50, (51, 52))")
	MustExec(t, db, ctx, "INSERT INTO zs VALUES (1, 111)")
	return db, ctx
}
