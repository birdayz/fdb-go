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
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestFDB_ExplainUnplannableQueryFailsLoudly is the red→green pin for the
// silent-degrade fix: a shape Cascades attempts and declines must raise the
// SAME 0AF00 under EXPLAIN as it does when run, never a logical plan tree.
//
// The specimen is the CTE spelling of the flattening-evasion shape. The two
// FROM-derived spellings that used to sit beside it now PLAN — a join-bodied
// derived table's output row is derived from its own legs, so the gap that
// made them unplannable is gone — and they moved to the agreement arm below.
// The loud-degrade direction needs SOME query Cascades declines: if the CTE
// leg's derivation closes too, this arm needs a new specimen, not a deletion.
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
			"CREATE TABLE a (id BIGINT, av BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT, a_id BIGINT, bv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, cv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT, c_id BIGINT, dw BIGINT, PRIMARY KEY (id))")
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
			// The CTE spelling of the flattening-evasion shape: a CTE leg's
			// output row is derived by the WITH scope, which still cannot
			// enumerate a join body.
			name: "cte_over_two_derived_joins",
			sql: "WITH t1 AS (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id), " +
				"t2 AS (SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) " +
				"SELECT t1.aid, t1.bv, t2.cid, t2.dw FROM t1, t2 WHERE t1.aid = t2.cid",
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

	t.Run("formerly_unplannable_derived_joins_agree", func(t *testing.T) {
		// The other half of the same invariant, from the side that changed:
		// these two used to be 0AF00 on BOTH sides and are now plannable on
		// both. EXPLAIN and execution must never disagree in either direction,
		// so a shape that starts planning has to keep its EXPLAIN, exactly as a
		// shape that stops planning has to lose it.
		//
		// The rows are CONSUMED and rows.Err() checked, not just requested: the
		// driver defers a planning failure to the first Next — which is why the
		// arm above reaches for assertUnsupported, whose whole first half exists
		// to drive Next and rows.Err() before believing a nil QueryContext error.
		// A QueryContext that returns nil proves only that a *sql.Rows came
		// back. The
		// expected rows are asserted for the same reason — a cross-product
		// degrade also "returns something".
		for _, tc := range []struct {
			sql  string
			want []string
		}{
			{
				sql: "SELECT t1.aid, t1.bv, t2.cid, t2.dw " +
					"FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1, " +
					"(SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) t2 " +
					"WHERE t1.aid = t2.cid",
				want: []string{"1|111|1|41"},
			},
			{
				sql:  "SELECT t1.aid, t1.bv FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1 WHERE t1.aid = 1",
				want: []string{"1|111"},
			},
		} {
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows = %v, want %v\n  sql: %s", got, tc.want, tc.sql)
			}
			if plan := pinExplain(t, db, ctx, tc.sql); plan == "" {
				t.Fatalf("EXPLAIN of a plannable statement returned nothing\n  sql: %s", tc.sql)
			}
		}
	})

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
			"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
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
