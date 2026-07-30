package sqldriver_test

// The SQL-EXPRESSIBLE half of the leg-identity census (CQ-61, the retyping of
// RecordTypeLeg's identity from TEXT to a CorrelationIdentifier).
//
// The sites that ask "does this correlation name that leg window?" used to
// disagree on how to compare leg identity: two compared the leg's stored text
// EXACTLY against the counterparty correlation's own spelling, two upper-FOLDED
// one side first. They all go through values.SameLeg now, which is EXACT, as Java
// is (CorrelationIdentifier.equals is Objects.equals on the raw id; Java never
// case-folds an alias anywhere). Making a folding comparison exact is a BEHAVIOR
// CHANGE for any pair that folds equal but is not equal, so whether such a pair
// occurs is a measurement — that is what the census is.
//
// WHERE THE GATE ACTUALLY LIVES: in TestMain (assertLegIdentityCensus). The
// census counters are package-scoped in values and Go orders no tests, so only
// after m.Run() is the population complete; a test asserting mid-run sees a
// floor. TestMain enforces both the per-site population FLOORS and the zeros.
//
// WHAT THIS TEST ADDS, and what it provably cannot: it drives the join /
// buried-join / correlated-scalar / lateral-unnest shapes that the SQL DDL can
// express, and asserts the invariants over whatever population exists when it
// runs. Measured, running this test ALONE: five of the six sites report Total 0
// and the sixth reports 4. That is not a fixable defect in the shapes below —
// the per-row leg BINDERS only see rows whose type carries leg boundaries, and
// those rows come from struct-array unnest chains that SQL DDL cannot build (see
// the INSERT comment below: there is no array literal that type-checks against
// an ARRAY column, so an unnest here iterates nothing). The binder population
// comes from the metadata-driven tests that construct their records as dynamicpb
// messages — TestFDB_ChainedUnnest, TestFDB_ChainedUnnestOrdinal,
// TestFDB_ChainedStarBodyCTE and the buried-window tests. Hence TestMain, whose
// corpus is all of them.
//
// The binders' identity semantics are additionally pinned DETERMINISTICALLY, not
// statistically, by the unit tests in the executor package
// (leg_identity_binding_test.go): a case-variant forgery must not bind, and a leg
// that states no identity must not bind by its Name.

import (
	"context"
	"database/sql"
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

	// This test does NOT touch the census gate. TestMain owns it for the whole run,
	// and the counters are a package-global: resetting here would discard every
	// sibling's accumulated traffic, and disabling on cleanup would switch the
	// census OFF partway through, leaving TestMain's report a floor of zero that
	// looks like a proof and is an artifact. That is not hypothetical — it is
	// exactly what the first version of this test did.

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

	// Read these as LOWER BOUNDS in both directions: the counters are
	// package-scoped and this test is t.Parallel(), so siblings contribute, and the
	// suite is not finished. The asymmetry is safe for the only thing asserted here
	// — a zero over a sum of non-negative terms is exact however many extra passes
	// contribute, so extra traffic can make it FAIL, never falsely pass. The
	// POPULATION floors are TestMain's job, because only it runs last.
	for _, site := range values.LegIdentitySites() {
		c := values.LegIdentityCensusOf(site)
		t.Logf("site %-52s total=%-7d exact=%-7d foldOnly=%-3d neither=%d",
			site.String(), c.Total, c.ExactEqual, c.FoldOnlyEqual, c.Neither)
		if c.FoldOnlyEqual != 0 {
			t.Errorf("site %s: FoldOnlyEqual = %d, want 0 — a leg is STORED under one "+
				"spelling and LOOKED UP under another, so making this site exact "+
				"(values.SameLeg) would change which row it binds. Witnesses: %v.\n"+
				"The fix belongs at the PRODUCER that normalizes one side, not in the "+
				"comparison: SameLeg is exact because the alias namespaces here are "+
				"case-DISJOINT (a quoted \"q$5\" must not forge a planner-minted q$5).",
				site, c.FoldOnlyEqual, c.FoldOnlySamples)
		}
		// The two IDENTITY-PAIR sites record something different from a comparison
		// the code makes: their pair is (leg.Name, leg.Alias.Name()) for the SAME
		// leg. There the load-bearing invariant is that a leg's two spellings AGREE
		// — ExactEqual == Total, equivalently Neither == 0 — and FoldOnlyEqual == 0
		// alone cannot see the failure that matters. A producer that stores Name "X"
		// against Alias "Y" lands in Neither, leaving FoldOnlyEqual at zero while the
		// text channel and the identity channel name different legs. That divergence
		// is what retires Name; it must be loud, not logged.
		switch site {
		case values.LegSiteTextVsIdentity, values.LegSiteSelectOutputLegs:
			if c.Neither != 0 {
				t.Errorf("site %s: Neither = %d of Total = %d, want 0 — a leg's TEXT and "+
					"its IDENTITY name different things. The readers now compare through "+
					"the identity while the dotted channel still reads the text, so the "+
					"two channels have diverged and Name can no longer be retired by "+
					"asserting they agree. Find the producer that sets one without the "+
					"other.", site, c.Neither, c.Total)
			}
		}
	}
}
