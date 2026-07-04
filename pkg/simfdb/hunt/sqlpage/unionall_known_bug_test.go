package sqlpage

import (
	"context"
	"fmt"
	"testing"
)

// TestKnownBug_UnionAllContinuation PINS a third executor continuation bug the SQL-pagination oracle's
// audit found (recorded in TODO.md "## DST findings"): `UNION ALL` is planned as a RecordQueryUnionPlan
// executed over a `concatCursor`, the SAME stateless concat combinator behind the multi-value-IN
// (InJoin) bug — it carries no per-branch continuation, so when the first branch's scan hits a small
// execution scanned-rows limit the concat errors 54F01 instead of resuming. Distinct SQL surface from
// InJoin (a set operation, not an IN predicate), same root cause; one fix (serialize {branch-index,
// branch-continuation}, as the intersection combinator already does) closes both.
//
// FIX-DETECTOR, not a correctness assertion: it asserts the bug is STILL PRESENT (paged UNION ALL
// errors). When the concat combinator gains per-branch continuation, the paged query will return the
// same rows as unpaged, this test will FAIL, and whoever fixed it should delete this test.
func TestKnownBug_UnionAllContinuation(t *testing.T) {
	ctx := context.Background()
	h, err := newHarness(997)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	// cat 0 → ids {0,1}; cat 1 → ids {2,3}. Two matching rows per branch so the first branch's scan
	// hits the scanned-rows budget mid-branch.
	rows := [][3]int64{{0, 0, 10}, {1, 0, 11}, {2, 1, 12}, {3, 1, 13}}
	for _, r := range rows {
		if _, err := h.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, cat, val) VALUES (%d, %d, %d)", r[0], r[1], r[2])); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	conn, err := h.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	const q = "SELECT id FROM t WHERE cat = 0 UNION ALL SELECT id FROM t WHERE cat = 1"

	if err := setPageLimit(conn, noPageLimit); err != nil {
		t.Fatalf("set ref limit: %v", err)
	}
	ref, err := runQuery(ctx, conn, q)
	if err != nil {
		t.Fatalf("unpaged %q: %v", q, err)
	}
	if len(ref) != 4 {
		t.Fatalf("unpaged UNION ALL should return 4 rows, got %d: %v", len(ref), ref)
	}

	// THE BUG: paged at scanned-rows-limit=1, the concat combinator errors 54F01 instead of resuming.
	if err := setPageLimit(conn, 1); err != nil {
		t.Fatalf("set page limit: %v", err)
	}
	_, pagedErr := runQuery(ctx, conn, q)
	if pagedErr == nil {
		t.Fatalf("UNION ALL now paginates without error — the known concat continuation bug is FIXED. " +
			"Delete TestKnownBug_UnionAllContinuation (and TestKnownBug_InJoinContinuation if InJoin also " +
			"resumes) and add UNION ALL / multi-value IN to the sqlpage sweep.")
	}
	t.Logf("known bug still present: paged UNION ALL errored instead of resuming: %v", pagedErr)
}
