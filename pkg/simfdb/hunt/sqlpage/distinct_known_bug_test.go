package sqlpage

import (
	"context"
	"fmt"
	"testing"
)

// TestKnownBug_DistinctContinuation PINS a known Cascades/executor bug the SQL-pagination oracle
// found (recorded in TODO.md "## DST findings"): the streaming DISTINCT operator does not carry its
// dedup state across a continuation resume, so a DISTINCT query that paginates mid-stream re-emits
// duplicate rows. GROUP BY over the same data stays correct — the aggregate continuation IS
// serialized — which is why DISTINCT should be fixable the same way.
//
// This is a FIX-DETECTOR, not a correctness assertion: it asserts the bug is STILL PRESENT (paged
// DISTINCT returns more rows than the distinct count). When the executor is fixed, the paged result
// will dedup correctly, this test will FAIL, and whoever fixed it should delete this test and
// re-enable bare DISTINCT in the sqlpage sweep (see the quarantine comment in queries()).
func TestKnownBug_DistinctContinuation(t *testing.T) {
	ctx := context.Background()
	h, err := newHarness(999)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	// cat ∈ {0,1} scattered (unsorted), so DISTINCT cat must be exactly {0,1}.
	cats := []int64{0, 1, 0, 1, 1, 0, 1, 0}
	for i, c := range cats {
		if _, err := h.db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, cat, val) VALUES (%d, %d, %d)", i, c, i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	conn, err := h.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	rowsAt := func(q string, limit int) []string {
		if err := setPageLimit(conn, limit); err != nil {
			t.Fatalf("set limit: %v", err)
		}
		r, err := runQuery(ctx, conn, q)
		if err != nil {
			t.Fatalf("%s @ %d: %v", q, limit, err)
		}
		return r
	}

	// Unpaged DISTINCT is correct: exactly the 2 distinct values.
	if got := rowsAt("SELECT DISTINCT cat FROM t", noPageLimit); len(got) != 2 {
		t.Fatalf("unpaged DISTINCT should be 2 distinct rows, got %d: %v", len(got), got)
	}
	// GROUP BY under pagination is correct — proof the engine CAN dedup across continuations.
	if got := rowsAt("SELECT cat FROM t GROUP BY cat", 1); len(got) != 2 {
		t.Fatalf("paged GROUP BY should be 2 rows, got %d: %v — the DISTINCT bug may now be fixed; "+
			"if GROUP BY regressed instead, investigate", len(got), got)
	}
	// THE BUG: paged DISTINCT re-emits duplicates. If this ever returns 2, DISTINCT was fixed —
	// delete this test and re-enable bare DISTINCT in queries().
	paged := rowsAt("SELECT DISTINCT cat FROM t", 1)
	if len(paged) == 2 {
		t.Fatalf("DISTINCT-under-continuation now dedups correctly (%v) — the known bug is FIXED. "+
			"Delete TestKnownBug_DistinctContinuation and re-enable bare DISTINCT in queries().", paged)
	}
	t.Logf("known bug still present: paged DISTINCT returned %d rows (dedup lost across continuation): %v", len(paged), paged)
}
