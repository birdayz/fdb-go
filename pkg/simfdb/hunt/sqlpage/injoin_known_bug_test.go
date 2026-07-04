package sqlpage

import (
	"context"
	"fmt"
	"testing"
)

// TestKnownBug_InJoinContinuation PINS a second Cascades/executor continuation bug the SQL-pagination
// oracle found (recorded in TODO.md "## DST findings"): a multi-value `IN (a, b)` is planned as an
// InJoin over a concat of per-value index scans, and that concat carries NO per-branch continuation
// state (executeInJoin/executeInUnion pass nil continuation; concatCursor deliberately errors on an
// out-of-band stop rather than silently drop — RFC-106a). So under a tiny execution scanned-rows
// limit the first inner scan hits the budget and the whole query errors 54F01 instead of resuming.
// Single-value IN degenerates to a bare equality scan (no concat) and is unaffected.
//
// FIX-DETECTOR, not a correctness assertion: it asserts the bug is STILL PRESENT (paged multi-value IN
// errors). When the InJoin/concat executors gain continuation support, the paged query will return the
// same rows as unpaged, this test will FAIL, and whoever fixed it should delete this test and add a
// multi-value IN query to the sqlpage sweep.
func TestKnownBug_InJoinContinuation(t *testing.T) {
	ctx := context.Background()
	h, err := newHarness(998)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	// (id, cat, val): cat 2 and 3 each present once, so `cat IN (2,3)` selects ids {0,1}.
	rows := [][3]int64{{0, 2, 10}, {1, 3, 11}, {2, 0, 12}, {3, 1, 13}}
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

	const q = "SELECT id FROM t WHERE cat IN (2, 3)"

	// Unpaged is correct: {0, 1}.
	if err := setPageLimit(conn, noPageLimit); err != nil {
		t.Fatalf("set ref limit: %v", err)
	}
	ref, err := runQuery(ctx, conn, q)
	if err != nil {
		t.Fatalf("unpaged %q: %v", q, err)
	}
	if len(ref) != 2 {
		t.Fatalf("unpaged multi-value IN should return 2 rows, got %d: %v", len(ref), ref)
	}

	// THE BUG: paged at scanned-rows-limit=1, the InJoin/concat errors 54F01 instead of resuming.
	if err := setPageLimit(conn, 1); err != nil {
		t.Fatalf("set page limit: %v", err)
	}
	_, pagedErr := runQuery(ctx, conn, q)
	if pagedErr == nil {
		t.Fatalf("multi-value IN now paginates without error — the known InJoin continuation bug is " +
			"FIXED. Delete TestKnownBug_InJoinContinuation and add a multi-value IN query to the sqlpage " +
			"sweep (queries()).")
	}
	t.Logf("known bug still present: paged multi-value IN errored instead of resuming: %v", pagedErr)
}
