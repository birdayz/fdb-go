package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_OrderByDeepNestedPathRefusedCleanly pins that an ORDER BY key which
// descends TWO levels into a struct (`r.v.z`) is refused at plan time with a
// SQLSTATE, rather than planning and dying inside the executor.
//
// THIS PIN IS TRANSIENT AND MUST NOT BE DEFENDED. The refusal exists only
// because the identifier resolver cannot descend past ONE level:
// semantic.Scope.ResolveQualifiedColumnNested takes a single qualifier
// Identifier and performs exactly one LookupStructField step, and the parser
// flattens the leading segments to the dotted string "R.V", which names neither
// a source alias nor a struct column. When that is fixed, `r.v.z` RESOLVES, this
// refusal stops being reachable, and this test should be rewritten to assert
// ROWS — the ids below in ascending r.v.z order — not to assert an error. A
// change here means the capability landed. It is not a regression.
//
// WHAT WAS WRONG. The ORDER BY validation loop (embedded/plan_visitor.go)
// matches three semantic error types: AmbiguousColumnError, SourceNotFoundError
// and ColumnNotFoundError. The walker's decline for a 3+-segment reference was
// none of them, so it fell out of the loop and was discarded. The key kept its
// raw dotted text and reached the executor as a flat FieldValue{"R.V.Z"} over a
// row whose columns are [ID Q R]:
//
//	ordinal resolution: field "R.V.Z" not resolvable in the runtime row
//	(ordinal -1, row columns [ID Q R]) — malformed plan
//
// That is internal state surfacing where a capability gap belongs.
//
// WHY ORDER BY ONLY. A behavioural sweep of every clause position that can
// carry such a reference found ORDER BY to be the sole leak. SELECT, GROUP BY,
// HAVING, an aggregate argument and DISTINCT all report 42703; WHERE, CASE,
// arithmetic and JOIN ON report a planner decline; UPDATE and DELETE report a
// DML translation decline. Only ORDER BY reached the executor.
//
// WHY 0AF00 AND NOT 42703. The reference is well-formed and Java resolves it,
// so "undefined column" would name a column that demonstrably exists — the same
// argument the nested-group-key refusal makes. A remaining inconsistency is
// deliberate and left alone: `SELECT r.v.z` reports 42703, which is arguably
// wrong for the identical reason, but it comes from the resolver genuinely
// failing to bind and changing it means making the path resolve.
func TestFDB_OrderByDeepNestedPathRefusedCleanly(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/obdeep"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE obdeep_tmpl "+
			"CREATE TYPE AS STRUCT st1 (y BIGINT, z BIGINT) "+
			"CREATE TYPE AS STRUCT st3 (u BIGINT, v st1) "+
			"CREATE TABLE nested (id BIGINT, q BIGINT, r st3, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE obdeep_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Q IS TIED ACROSS EVERY ROW, AND THAT IS THE POINT. The two-key shape
	// `ORDER BY q, r.v.z` below reaches the deep reference ONLY when the leading
	// key does not already impose a total order: with distinct q values the
	// planner prunes the redundant second key before anything evaluates it, the
	// query answers, and a pin written that way would pass with the defect fully
	// present. Measured both ways — distinct q returned 3 rows and no error on
	// the broken build; tied q reproduced the executor death. The struct's z
	// values are distinct so they can decide the order once the capability
	// lands and this test is rewritten to assert rows.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO nested VALUES (1, 7, (5, (1, 300))), (2, 7, (5, (1, 100))), (3, 7, (5, (1, 200)))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// CONTROL. A ONE-level nested path in the same clause, over the same table,
	// in both the single-key and the two-key form. It must ANSWER. Without it a
	// refusal below proves nothing: a build that rejected every nested ORDER BY
	// key, or every ORDER BY, would satisfy the gate while being far more broken.
	for _, c := range []string{
		"SELECT id FROM nested ORDER BY r.u",
		"SELECT id FROM nested ORDER BY q, r.u",
	} {
		rows, cerr := db.QueryContext(ctx, c)
		if cerr != nil {
			t.Fatalf("CONTROL query failed: %s\n  err: %v\n"+
				"  A ONE-level nested ORDER BY key must still answer. If it does "+
				"not, the refusal asserted below is not specific to a DEEP path "+
				"and this test is measuring a much larger breakage.", c, cerr)
		}
		n := 0
		for rows.Next() {
			n++
		}
		iterErr := rows.Err()
		rows.Close()
		if iterErr != nil {
			t.Fatalf("CONTROL iterate failed: %s\n  err: %v", c, iterErr)
		}
		if n != 3 {
			t.Fatalf("CONTROL returned %d rows, want 3: %s", n, c)
		}
	}

	// THE GATE. Each shape must be refused with 0AF00 rather than reaching the
	// executor.
	for _, q := range []string{
		"SELECT id FROM nested ORDER BY r.v.z",
		"SELECT id FROM nested ORDER BY r.v.z DESC",
		// The two-key form, which only reaches the deep key because q is tied.
		"SELECT id FROM nested ORDER BY q, r.v.z",
	} {
		_, qerr := db.QueryContext(ctx, q)
		if qerr == nil {
			t.Fatalf("A DEEP NESTED ORDER BY KEY NOW ANSWERS.\n"+
				"  query: %s\n\n"+
				"  This is the expected and welcome end state, not a regression: "+
				"it means the resolver can now descend more than one level, so "+
				"the reference resolves and the refusal is unreachable.\n\n"+
				"  What to do: DELETE the gate and assert the ROWS instead — over "+
				"the seeded z values 300/100/200 for ids 1/2/3, ascending r.v.z "+
				"is [2 3 1] and DESC is [1 3 2]. Keep the controls and keep q "+
				"tied, so the two-key shape still exercises the deep key.", q)
		}
		if !strings.Contains(qerr.Error(), "0AF00") {
			t.Fatalf("a deep nested ORDER BY key was refused with the WRONG error.\n"+
				"  query: %s\n  got:   %v\n  want:  0AF00 (unsupported_query)\n\n"+
				"  If this says `ordinal resolution: field \"R.V.Z\" not resolvable "+
				"in the runtime row … malformed plan`, the walker's decline is "+
				"being swallowed again and internal state is reaching the user "+
				"where a capability gap belongs — the exact defect this pins.\n"+
				"  If it says 42703, the reference is being reported as an "+
				"undefined column, which would name a column that demonstrably "+
				"exists and that Java resolves.", q, qerr)
		}
	}
}
