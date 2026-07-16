package sqldriver_test

// RFC-179 F43 — revert-proof pin for the PLANNING-phase re-fire of
// NewRewriteOuterJoinRule (cascades/default_rules.go, in PlanningExplorationRules).
//
// RewriteOuterJoinRule is registered TWICE: once in RewritingRules() (REWRITING)
// and once in PlanningExplorationRules() (PLANNING). The REWRITING copy canonicalizes
// only the LEFT OUTERs that already exist as a distinct 2-quantifier LEFT-OUTER
// SelectExpression before PLANNING (e.g. a top-level `a LEFT JOIN c`). A LEFT OUTER
// that is the ENCLOSED leg of a larger join — here `(a LEFT JOIN c) JOIN b` — is
// re-materialized as a fresh 2-quantifier LEFT-OUTER sub-select by the PLANNING-phase
// join-partitioning rules (PartitionSelectRule / PartitionBinarySelectRule) AFTER the
// REWRITING phase closed, so REWRITING never canonicalized THAT sub-select. Only the
// PLANNING re-fire (the line this pins) rewrites it to the correlated null-supplying
// form the data-access FlatMap path consumes, which lets the null-supplying leg C be
// reached by an INDEX PROBE (IndexScan(C_A_ID)).
//
// Remove the RewriteOuterJoinRule() from PlanningExplorationRules() and this enclosed
// LEFT OUTER stops being canonicalized in PLANNING: it falls back to a materialized
// NestedLoopJoin(LEFT OUTER, ..., Scan(A), Scan(C)) — a FULL SCAN of C. The rows stay
// correct either way (the materialized NLJ null-extends correctly), so this is a plan/
// efficiency regression a row-only test cannot see — the pin is on the EXPLAIN shape.
//
// Contrast: the top-level `a LEFT JOIN c` (present before PLANNING) does NOT depend on
// the re-fire — the REWRITING copy already canonicalized it — which is why every
// existing outer-join test stays green when the PLANNING copy is removed.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_EnclosedLeftOuter_PlanningRefire pins that an enclosed correlated LEFT
// OUTER — `a LEFT JOIN c ON c.a_id = a.id JOIN b ON b.a_id = a.id` — plans its
// `a LEFT JOIN c` sub-join as the correlated index-probe FlatMap (produced ONLY by
// the PLANNING-phase re-fire of RewriteOuterJoinRule), never as a materialized
// full-scan NestedLoopJoin.
func TestFDB_EnclosedLeftOuter_PlanningRefire(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_elopr"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE elopr "+
			"CREATE TABLE a (id BIGINT NOT NULL, flag BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE elopr")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 0), (2, 0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1), (11, 2)") // both A rows have a B match
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 1)")         // only A=1 has a C match

	// The LEFT OUTER (a LEFT JOIN c) is the ENCLOSED leg of the enclosing INNER
	// join to b, so it surfaces as a 2-quantifier LEFT-OUTER sub-select only during
	// PLANNING (re-partitioned by PartitionSelectRule / PartitionBinarySelectRule).
	const q = "SELECT a.id, c.id FROM a LEFT JOIN c ON c.a_id = a.id JOIN b ON b.a_id = a.id ORDER BY a.id"

	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}

	// Discriminators (all three flip when the PLANNING re-fire is removed):
	//   with re-fire:    FlatMap(outer=Scan(A), inner=DefaultOnEmpty(Fetch(IndexScan(C_A_ID, [=]))))
	//   without re-fire: NestedLoopJoin(LEFT OUTER, [1 preds], Scan(A), Scan(C))
	if strings.Contains(plan, "NestedLoopJoin") {
		t.Errorf("enclosed LEFT OUTER fell back to the materialized NestedLoopJoin "+
			"(the PLANNING re-fire of RewriteOuterJoinRule was not applied):\n  %s", plan)
	}
	if !strings.Contains(plan, "IndexScan(C_A_ID") {
		t.Errorf("null-supplying leg C is not index-probed — the correlated LEFT-OUTER "+
			"FlatMap did not fire (expected IndexScan(C_A_ID)):\n  %s", plan)
	}
	if !strings.Contains(plan, "DefaultOnEmpty") {
		t.Errorf("no DefaultOnEmpty null-supplying boundary — the enclosed LEFT OUTER was "+
			"not canonicalized to the correlated null-supplying FlatMap form:\n  %s", plan)
	}

	// Plan determinism: the enclosed-box canonicalization + partition composition must
	// not race to distinct-but-equivalent members. Pin the plan stable across re-plans.
	for i := 1; i < 5; i++ {
		var again string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&again); err != nil {
			t.Fatalf("EXPLAIN run %d: %v", i, err)
		}
		if again != plan {
			t.Fatalf("plan UNSTABLE across runs:\n  run 0: %s\n  run %d: %s", plan, i, again)
		}
	}

	// Execution correctness (stays green regardless of the plan shape — the materialized
	// NLJ null-extends correctly too — but guards the LEFT-OUTER semantics end-to-end):
	// A=1 matches C=100; A=2 has no C → null-extended. Both A rows have a B match.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type row struct {
		aID int64
		cID sql.NullInt64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.aID, &r.cID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []row{
		{1, sql.NullInt64{Int64: 100, Valid: true}},
		{2, sql.NullInt64{Valid: false}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].aID != want[i].aID || got[i].cID != want[i].cID {
			t.Errorf("row %d = {a:%d c:%v}, want {a:%d c:%v}", i, got[i].aID, got[i].cID, want[i].aID, want[i].cID)
		}
	}
}
