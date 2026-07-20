package sqldriver_test

// LIVE PIN for a KNOWN 3-way-join ordinal-resolution defect, surfaced by the
// RFC-182 rowdiff harness (seed 1001000229) once 3-way self-joins were
// generated at scale.
//
// A 3-WAY join in which the SAME column (here c) is referenced across all three
// legs — an equality filter on the first (`l.c = 1`), the join key between the
// second and third (`m.c = r.c`), and a filter on the third (`r.c IS NULL`) —
// plans but then FAILS AT EXECUTION with:
//
//	ordinal resolution: field "C" not resolvable in the runtime row
//	(ordinal -1, row columns [ID A B C S F]) — malformed plan
//
// It is fail-LOUD (never wrong rows), and the query's correct result is EMPTY
// (m.c = r.c can never hold when r.c IS NULL). Removing ANY one factor fixes it:
//   - the 2-way analog `l.c = r.c AND r.c IS NULL` works;
//   - dropping the `m.c = r.c` join, the `l.c = 1` filter, or the `r.c IS NULL`
//     filter works.
//
// This is the same ordinal-binding family as the multi-leg source-relative
// ordinal gap (now FIXED — TestFDB_LeftJoinPkOrdinal_InJoinSortRegression) but a
// distinct error signature and a distinct root cause (an index match-candidate
// placeholder leaking into a reapplied residual, not a missing leg-window
// passthrough); the rowdiff ledger declines it separately (knownGaps in run.go).
//
// WHEN FIXED this test goes RED (the query starts returning rows). Then: delete
// the knownGaps entry and assert the correct result — zero rows.
import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ThreeWaySharedColOrdinal_KnownGap(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_3wc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_3wc")
	// The full column set matters — the error names the runtime row's columns
	// [ID A B C S F]; a narrower table does not reproduce the malformed plan.
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE t3wc "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id)) "+
		"CREATE INDEX idx_c ON t (c) CREATE INDEX idx_a ON t (a) CREATE INDEX idx_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3wc/s WITH TEMPLATE t3wc")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_3wc?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,c) VALUES (1,2,1),(2,3,7),(3,5,NULL),(4,2,1),(5,9,NULL)")

	const q = "SELECT l.id AS lid, m.id AS mid, r.id AS rid FROM t AS l, t AS m, t AS r " +
		"WHERE l.a = m.id AND m.c = r.c AND l.c = 1 AND r.c IS NULL"

	rows, err := db.QueryContext(ctx, q)
	if err == nil {
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
		t.Fatalf("3-way shared-column query now EXECUTES — the ordinal-binding "+
			"workstream has landed. Remove the knownGaps entry in rowdiff/run.go and "+
			"assert ZERO rows. Query: %s", q)
	}
	if !strings.Contains(err.Error(), "not resolvable in the runtime row") ||
		!strings.Contains(err.Error(), "ordinal -1") {
		t.Fatalf("expected the known ordinal-resolution malformed-plan error, got a "+
			"DIFFERENT failure (investigate — may be a new bug): %v", err)
	}
}
