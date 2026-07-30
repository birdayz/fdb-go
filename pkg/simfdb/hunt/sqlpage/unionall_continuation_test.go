package sqlpage

import (
	"context"
	"fmt"
	"testing"
)

// TestUnionAllContinuation pins the fix for the UNION ALL/concat continuation bug: `UNION ALL` is planned
// as a RecordQueryUnionPlan executed over the SAME concat combinator behind the multi-value-IN (InJoin)
// bug. The concat now serializes {active-branch index, that branch's child continuation}, so a branch
// that hits the execution scanned-rows limit mid-scan mints a RESUMABLE continuation instead of erroring
// 54F01. This test asserts CORRECTNESS: the paged result (scanned-rows-limit=1) equals the unpaged result
// as a multiset (UNION ALL keeps duplicates; no order contract).
func TestUnionAllContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := newHarness(997)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	// cat 0 → ids {0,1}; cat 1 → ids {2,3}. Two matching rows per branch so the first branch's scan
	// hits the scanned-rows budget mid-branch and must resume there.
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

	// Paged at the hardest scanned-rows limit: the concat must resume across the branch boundary,
	// returning the same rows as unpaged.
	if err := setPageLimit(conn, 1); err != nil {
		t.Fatalf("set page limit: %v", err)
	}
	paged, err := runQuery(ctx, conn, q)
	if err != nil {
		t.Fatalf("paged UNION ALL errored instead of resuming (concat continuation bug regressed): %v", err)
	}
	if d := compare(ref, paged, false); d != "" {
		t.Fatalf("paged UNION ALL diverged from unpaged: %s", d)
	}
}
