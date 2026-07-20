package sqldriver_test

// LIVE PIN for a KNOWN executor/ordinal-binding defect, surfaced by the
// RFC-182 rowdiff harness when LEFT OUTER JOIN generation was added (rowdiff
// seed 960000225).
//
// A LEFT OUTER JOIN on the PRIMARY KEY (`l.id = r.id`), combined with an R-side
// WHERE filter AND an ORDER BY on the left key, plans successfully but then
// FAILS AT EXECUTION with:
//
//	correlated FieldValue "ID" (correlation "L") evaluated against an
//	unbound/unrecognized context (…multi-leg row cannot serve a
//	source-relative ordinal…) — no frontier row resolved (planner/executor bug)
//
// This is the same "multi-leg merged rows serve source-relative ordinals"
// defect documented in rule_implement_nested_loop_join.go (~line 2785): the
// obvious rebase fix was tried and reverted, and it is booked as a distinct
// executor/ordinal-binding workstream. It is a fail-LOUD execution error, never
// a wrong-rows outcome, so the rowdiff ledger classifies its signature as a
// tracked DECLINE (knownGaps in run.go) rather than a soundness finding.
//
// Removing ANY one of the three factors makes the query execute:
//   - a non-PK join key (l.b = r.b) works;
//   - dropping the R-side WHERE works;
//   - dropping the ORDER BY works.
//
// WHEN THE WORKSTREAM LANDS this test goes RED (the query starts returning
// rows). At that point: delete the knownGaps entry, and replace the
// expect-error assertion below with the correct result — for this data and
// query the answer is, in l.id-DESC order, l_id/r_id pairs (2,2) then (1,1)
// (l.id = r.id is a self-match, so no row is NULL-extended; the WHERE keeps
// only s ∈ {delta,beta} → ids 1 and 2).
import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_LeftJoinPkOrdinal_KnownExecutorGap(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ljpk")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ljpk")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ljpk "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id)) "+
		"CREATE INDEX idx_b ON t (b) "+
		"CREATE INDEX idx_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ljpk/s WITH TEMPLATE ljpk")
	dsn := fmt.Sprintf("fdbsql:///testdb_ljpk?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b,c,s,f) VALUES "+
		"(1,2,8,3,'beta',TRUE),(2,3,5,9,'delta',FALSE),(3,7,8,1,'alpha',TRUE)")

	// The minimal failing shape: PK-join LEFT OUTER + R-side filter + ORDER BY
	// on the left key.
	const q = "SELECT l.id AS l_id, r.id AS r_id FROM t AS l LEFT JOIN t AS r ON l.id = r.id " +
		"WHERE r.s IN ('delta','beta') ORDER BY l.id DESC, r.id"

	rows, err := db.QueryContext(ctx, q)
	if err == nil {
		// The driver may surface the plan/execution error on the first drain.
		var drainErr error
		func() {
			defer rows.Close()
			for rows.Next() {
			}
			drainErr = rows.Err()
		}()
		err = drainErr
	}
	if err == nil {
		t.Fatalf("PK-join LEFT OUTER + R-filter + ORDER BY now EXECUTES — the "+
			"multi-leg ordinal-binding workstream has landed. Remove the knownGaps "+
			"entry in rowdiff/run.go and assert the correct rows [(2,2),(1,1)]. Query: %s", q)
	}
	if !strings.Contains(err.Error(), "multi-leg row cannot serve a source-relative ordinal") ||
		!strings.Contains(err.Error(), "no frontier row resolved") {
		t.Fatalf("expected the known multi-leg ordinal-binding executor error, got a DIFFERENT failure "+
			"(investigate — may be a new bug): %v", err)
	}
}
