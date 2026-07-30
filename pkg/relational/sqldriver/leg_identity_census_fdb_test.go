package sqldriver_test

// Leg-identity census pass (CQ-61): the acceptance instrument for retyping
// RecordTypeLeg's identity from TEXT to a CorrelationIdentifier.
//
// The sites that ask "does this correlation name that leg window?" do NOT agree
// on how to compare leg identity. Two compare the leg's stored text EXACTLY
// against the counterparty correlation's own spelling; two upper-FOLD one side
// first. Routing all of them through values.SameLeg makes every one EXACT, which
// is what Java does (CorrelationIdentifier.equals is Objects.equals on the raw
// id, and Java never case-folds an alias anywhere).
//
// Converting a folding comparison to an exact one is a BEHAVIOR CHANGE for any
// pair that folds equal but is not equal. This test measures whether such a pair
// occurs in real execution: it runs the join / buried-join / correlated-scalar /
// lateral-unnest shapes the Group-A sites serve against a live FDB and asserts
// the FoldOnlyEqual population is EMPTY at every site.
//
// The zero is what makes the conversion free. A nonzero count is NOT a licence to
// keep folding — it names a producer that stores a leg under one spelling and
// looks it up under another, and the fix belongs there.
//
// Read the counts as LOWER BOUNDS. The census counters are package-scoped in
// values and this test is t.Parallel(), so sibling tests planning or executing
// their own corpora contribute too. That asymmetry is safe in the only direction
// the assertion depends on: extra traffic can make FoldOnlyEqual==0 FAIL, never
// falsely pass.
//
// What this test is and is NOT, stated plainly so its green is not over-read:
//
//   - It is the always-green FLOOR: whatever leg traffic the shapes below (plus
//     any sibling running concurrently) generate, none of it may be fold-only.
//   - It does NOT by itself reach the per-row leg BINDERS. Those sites only see
//     rows whose type carries leg boundaries, and the shapes that produce such
//     rows live across the wider suite rather than in any one fixture — measured:
//     run this package with LEG_IDENTITY_CENSUS=1 (see TestMain) and the binder
//     sites report thousands of comparisons, all exact. A single test that claimed
//     to cover them would be asserting a zero over a population it never looked
//     at, which is the one thing this census exists to avoid.
//   - The binders' identity semantics are pinned DETERMINISTICALLY, not
//     statistically, by the unit tests in the executor package
//     (leg_identity_binding_test.go): a case-variant forgery must not bind, and a
//     leg that states no identity must not bind by its Name.
//
// So the liveness check below requires only that SOME site was reached — enough to
// prove the instrument is wired and running, which is the failure this test can
// actually detect on its own.

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFDB_LegIdentityCensus_NoFoldOnlyTraffic(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/s4legid"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE s4legid_tmpl"+
		// na/nb/nc/nd mirror the N-way comma-join harness's shapes EXACTLY (two-column
		// na, single-column narrowing legs). The shape is load-bearing, not incidental:
		// the same query over a THREE-column first leg does not reach the leg-window
		// path at all — it declines during the ordinal join build — so a
		// nearly-identical table definition measures nothing.
		" CREATE TABLE na (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE nb (id BIGINT NOT NULL, PRIMARY KEY (id))"+
		" CREATE TABLE nc (id BIGINT NOT NULL, PRIMARY KEY (id))"+
		" CREATE TABLE nd (id BIGINT NOT NULL, PRIMARY KEY (id))"+
		" CREATE TABLE la (id BIGINT NOT NULL, v BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE lb (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE lc (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE ld (id BIGINT NOT NULL, PRIMARY KEY (id))"+
		" CREATE TABLE lorders (id BIGINT NOT NULL, amount BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE lextras (id BIGINT NOT NULL, ref BIGINT, tag STRING, PRIMARY KEY (id))"+
		" CREATE TABLE larr (id BIGINT NOT NULL, arr INTEGER ARRAY, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE s4legid_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO na VALUES (1, 100), (2, 200), (3, 300)",
		"INSERT INTO nb VALUES (1), (2), (3)",
		"INSERT INTO nc VALUES (1), (2), (3)",
		"INSERT INTO nd VALUES (1), (3)",
		"INSERT INTO la VALUES (1, 100, 7), (2, 200, 8), (3, 300, 7)",
		"INSERT INTO lb VALUES (1, 8), (2, 8), (3, 8)",
		"INSERT INTO lc VALUES (1, 9), (2, 9), (3, 9)",
		"INSERT INTO ld VALUES (1), (3)",
		"INSERT INTO lorders VALUES (1, 100), (2, 50)",
		"INSERT INTO lextras VALUES (10, 2, 'x'), (11, 1, 'y')",
		// SQL cannot populate an ARRAY column — there is no array literal that
		// type-checks against one (see conformance/yamsql/testdata/array_column_type.yaml).
		// A NULL array still gives the lateral-unnest shapes below what this pass
		// needs from them: the seed-window derivation is a PLAN-TIME site, so it is
		// exercised by planning the unnest whether or not any element flows.
		"INSERT INTO larr VALUES (1, NULL), (2, NULL)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Enable AFTER seeding so the DML path's own traffic is excluded, and reset so
	// the pass starts from a known floor.
	//
	// The census gate is a PACKAGE-global, and this test runs in parallel with the
	// rest of the suite — so when the whole-suite census owns the gate
	// (LEG_IDENTITY_CENSUS=1, see TestMain) this test must not touch it. Resetting
	// would discard every sibling's accumulated traffic, and disabling on cleanup
	// would silently switch the census OFF partway through the run, leaving the
	// whole-suite report a floor of zero that looks like a proof and is an artifact.
	// That is not hypothetical: it is exactly what the first version of this test did.
	ownsGate := os.Getenv("LEG_IDENTITY_CENSUS") != "1"
	if ownsGate {
		values.ResetLegIdentityCensus()
		values.SetLegIdentityCensusEnabled(true)
		t.Cleanup(func() { values.SetLegIdentityCensusEnabled(false) })
	}

	// Each shape is a Group-A leg-window consumer. They are run for their leg
	// TRAFFIC, so a query that legitimately declines to plan is not a failure of
	// this test — the shapes' ANSWERS are pinned by their own dedicated tests. What
	// this pass asserts is a property of every comparison those shapes make.
	for _, q := range []struct {
		name string
		sql  string
	}{
		{
			// The shape that actually reaches the leg binders: a 3-way flat
			// existential fold whose merged row carries per-leg windows.
			"comma-join 3-way + projected EXISTS",
			"SELECT a.v, EXISTS (SELECT 1 FROM nd d WHERE d.id = a.id) AS has_d " +
				"FROM na a, nb b, nc c WHERE a.id = b.id AND b.id = c.id",
		},
		{
			"buried JOIN..ON 3-way + projected EXISTS",
			"SELECT a.v, EXISTS (SELECT 1 FROM nd d WHERE d.id = a.id) " +
				"FROM na a JOIN nb b ON b.id = a.id JOIN nc c ON c.id = a.id",
		},
		{
			"buried JOIN..ON chain + EXISTS on the deepest leg",
			"SELECT a.k, EXISTS (SELECT 1 FROM ld d WHERE d.id = a.k) " +
				"FROM la a JOIN lb b ON b.id = a.id JOIN lc c ON c.id = a.id",
		},
		{
			"correlated scalar over a comma cluster, first leg",
			"SELECT (SELECT o.amount FROM lorders o WHERE o.id = c.id) " +
				"FROM la c, lextras e WHERE c.id = 1 AND e.id = 10",
		},
		{
			"correlated scalar over a comma cluster, rightmost leg",
			"SELECT (SELECT o.amount FROM lorders o WHERE o.id = e.ref) " +
				"FROM la c, lextras e WHERE c.id = 1 AND e.id = 10",
		},
		{
			"correlated scalar over a gated LEFT-join outer",
			"SELECT c.v, (SELECT o.amount FROM lorders o WHERE o.id = c.id) " +
				"FROM la c LEFT JOIN lextras e ON e.ref = c.id WHERE c.id = 2",
		},
		{
			"lateral unnest element beside its outer leg",
			"SELECT t.id, v FROM larr t, t.arr AS v",
		},
		{
			"lateral unnest beside a second table leg",
			"SELECT t.id, v, u.k FROM larr t, t.arr AS v, lb u WHERE u.id = t.id",
		},
		{
			"projected EXISTS over a derived-table leg",
			"SELECT d.aid, EXISTS (SELECT 1 FROM ld x WHERE x.id = d.aid) " +
				"FROM (SELECT a.id AS aid, b.k AS bk FROM la a JOIN lb b ON b.id = a.id) d",
		},
	} {
		rows, qErr := db.QueryContext(ctx, q.sql)
		if qErr != nil {
			t.Logf("shape %q did not plan (%v) — no leg traffic contributed", q.name, qErr)
			continue
		}
		for rows.Next() {
			// Drain: the leg binder runs per ROW, so the row loop is where the
			// runtime sites accumulate their population.
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			t.Logf("shape %q errored mid-scan (%v) — partial leg traffic contributed", q.name, rowsErr)
		}
	}

	var wired int
	for _, site := range values.LegIdentitySites() {
		c := values.LegIdentityCensusOf(site)
		t.Logf("site %-52s total=%-7d exact=%-7d foldOnly=%-3d neither=%d",
			site.String(), c.Total, c.ExactEqual, c.FoldOnlyEqual, c.Neither)
		if c.Total > 0 {
			wired++
		}
		if c.FoldOnlyEqual != 0 {
			t.Errorf("site %s: FoldOnlyEqual = %d, want 0 — a leg is STORED under one "+
				"spelling and LOOKED UP under another, so making this site exact "+
				"(values.SameLeg) would change which row it binds. Witnesses: %v.\n"+
				"The fix belongs at the PRODUCER that normalizes one side, not in the "+
				"comparison: SameLeg is exact because the alias namespaces here are "+
				"case-DISJOINT (a quoted \"q$5\" must not forge a planner-minted q$5).",
				site, c.FoldOnlyEqual, c.FoldOnlySamples)
		}
	}
	if wired == 0 {
		t.Fatal("every leg-identity site reported ZERO comparisons — the census is not " +
			"wired, so its zero fold-only population is vacuous rather than evidence")
	}
}
