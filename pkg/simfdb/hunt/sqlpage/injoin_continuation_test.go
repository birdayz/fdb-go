package sqlpage

import (
	"context"
	"fmt"
	"testing"
)

// TestInJoinContinuation pins the fix for the InJoin/concat continuation bug the SQL-pagination oracle
// found: a multi-value `IN (a, b)` is planned as an InJoin over a concat of per-value index scans. The
// concat now serializes {active-branch index, that branch's child continuation}, so an out-of-band stop
// (the execution scanned-rows limit hit mid-branch) mints a RESUMABLE continuation instead of erroring
// 54F01. This test asserts CORRECTNESS: the paged result (scanned-rows-limit=1, forcing a resume after
// almost every scanned row) equals the unpaged result as a multiset.
func TestInJoinContinuation(t *testing.T) {
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

	// Unpaged reference.
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

	// Paged at the hardest scanned-rows limit: the InJoin/concat must resume across every
	// branch boundary and byte-offset, returning the same rows as unpaged.
	if err := setPageLimit(conn, 1); err != nil {
		t.Fatalf("set page limit: %v", err)
	}
	paged, err := runQuery(ctx, conn, q)
	if err != nil {
		t.Fatalf("paged multi-value IN errored instead of resuming (InJoin concat continuation bug regressed): %v", err)
	}
	if d := compare(ref, paged, false); d != "" {
		t.Fatalf("paged multi-value IN diverged from unpaged: %s", d)
	}
}

// TestInJoinLimit pins the nit both reviewers flagged with the concat fix: a multi-value IN composed
// with LIMIT must not over-return. executeInJoin now clears skip+limit on each inner branch and applies
// the limit once at the concat (matching executeInUnion and Java RecordQueryInJoinPlan). A per-branch
// limit would let each branch emit up to LIMIT rows, so the total would exceed LIMIT.
func TestInJoinLimit(t *testing.T) {
	ctx := context.Background()
	h, err := newHarness(996)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	// cat 0 → ids {0,1,2}, cat 1 → ids {3,4,5}: `cat IN (0,1)` matches 6 rows.
	rows := [][3]int64{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}, {3, 1, 0}, {4, 1, 0}, {5, 1, 0}}
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
	if err := setPageLimit(conn, noPageLimit); err != nil {
		t.Fatalf("set limit: %v", err)
	}

	// 6 rows match; LIMIT 4 must return exactly 4 (a per-branch limit would over-return to 6).
	got, err := runQuery(ctx, conn, "SELECT id FROM t WHERE cat IN (0, 1) LIMIT 4")
	if err != nil {
		t.Fatalf("IN … LIMIT: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("`cat IN (0,1) LIMIT 4` returned %d rows, want exactly 4 (InJoin over-return regressed): %v", len(got), got)
	}
}
