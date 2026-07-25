package sqldriver_test

// EXPLAIN must render the plan the engine would actually run, or fail with the
// error the statement itself raises. It must never print a plan the engine
// cannot execute.
//
// computeExplainText used to swallow a Cascades planning failure and print
// logical-plan text instead, so `SELECT q` returned 0AF00 while `EXPLAIN q`
// returned a tidy plan tree. Java has no such degrade: an unplannable EXPLAIN
// lets UnableToPlanException propagate as 0AF00, and its relational layer has
// no logical-plan renderer on the EXPLAIN path at all.
//
// These pin both directions over real FDB: the attempted-and-failed case is
// loud, and the two shapes that never reach Cascades still render.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ExplainUnplannableQueryFailsLoudly is the red→green pin for the
// silent-degrade fix. The flattening-evasion shape (`FROM (a JOIN b) t1,
// (c JOIN d) t2 WHERE t1.aid = t2.cid`) is a pre-existing planner gap:
// buildDerivedTableSource declines join-bodied derived tables, so Cascades
// yields no physical plan. Running it is 0AF00; EXPLAIN of it must be the
// SAME 0AF00, not a logical plan tree.
func TestFDB_ExplainUnplannableQueryFailsLoudly(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_explainloud")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_explainloud")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE explainloud_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, av BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, bv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, cv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, c_id BIGINT, dw BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_explainloud/s WITH TEMPLATE explainloud_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_explainloud?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, av) VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id, bv) VALUES (10, 1, 111)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, cv) VALUES (1, 51)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, c_id, dw) VALUES (1000, 1, 41)")

	unplannable := []struct {
		name string
		sql  string
	}{
		{
			// Two join-bodied derived tables joined on a cross-derived predicate.
			name: "comma_over_two_derived_joins",
			sql: "SELECT t1.aid, t1.bv, t2.cid, t2.dw " +
				"FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1, " +
				"(SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) t2 " +
				"WHERE t1.aid = t2.cid",
		},
		{
			// The CTE spelling of the same shape.
			name: "cte_over_two_derived_joins",
			sql: "WITH t1 AS (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id), " +
				"t2 AS (SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) " +
				"SELECT t1.aid, t1.bv, t2.cid, t2.dw FROM t1, t2 WHERE t1.aid = t2.cid",
		},
		{
			// A single join-bodied derived table — the underlying capability gap.
			name: "solo_derived_join",
			sql:  "SELECT t1.aid, t1.bv FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1 WHERE t1.aid = 1",
		},
	}

	for _, tc := range unplannable {
		t.Run(tc.name, func(t *testing.T) {
			// The statement itself is 0AF00 — establishes that Cascades was
			// attempted and declined, so this is case (1), not a shape that
			// legitimately has no physical plan.
			assertUnsupported(t, db, ctx, tc.sql)
			// EXPLAIN of it must be the SAME 0AF00. A rendered plan tree here
			// is the defect: EXPLAIN describing something the engine refuses
			// to run.
			assertUnsupported(t, db, ctx, "EXPLAIN "+tc.sql)
		})
	}

	t.Run("plannable_still_renders_physical_plan", func(t *testing.T) {
		// The other direction: the fix must not make EXPLAIN loud in general.
		// A plannable query still yields a PHYSICAL plan (Scan/IndexScan
		// vocabulary), never the logical `Scan(A)`-only rendering.
		plan := pinExplain(t, db, ctx, "SELECT a.id FROM a WHERE a.id = 1")
		if !strings.Contains(plan, "Scan(A") {
			t.Fatalf("plannable EXPLAIN lost its physical plan text:\n%s", plan)
		}
	})
}

// TestFDB_ExplainInformationSchemaStillRenders pins the surviving
// INFORMATION_SCHEMA arm. INFORMATION_SCHEMA is a Go-only extension served off
// the catalog by execSystemTableQuery, never by Cascades: the query executes
// fine and its plan's own Explain is this logical text, so EXPLAIN reports the
// plan that really runs. Dropping the referencesInformationSchema guard would
// now route it into Cascades and turn a working query's EXPLAIN into 0AF00 —
// before the split there was nothing asserting that guard at all.
func TestFDB_ExplainInformationSchemaStillRenders(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_explaininfo")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_explaininfo")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE explaininfo_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_explaininfo/s WITH TEMPLATE explaininfo_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_explaininfo?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// TABLES/COLUMNS are reserved words; the corpus spells them quoted.
	const q = `SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES"`
	// The query runs — so EXPLAIN owes a plan, not an error.
	if _, err := db.QueryContext(ctx, q); err != nil {
		t.Fatalf("INFORMATION_SCHEMA query must execute: %v", err)
	}
	plan := pinExplain(t, db, ctx, q)
	if plan == "" {
		t.Fatal("EXPLAIN of an executable INFORMATION_SCHEMA query rendered nothing")
	}
	if !strings.Contains(strings.ToUpper(plan), "TABLES") {
		t.Fatalf("EXPLAIN of INFORMATION_SCHEMA lost the table reference:\n%s", plan)
	}
}
