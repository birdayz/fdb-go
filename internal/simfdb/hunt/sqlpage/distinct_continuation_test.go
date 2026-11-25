package sqlpage

import (
	"context"
	"fmt"
	"testing"
)

// TestDistinctContinuationDedups pins that a DISTINCT query paginating mid-stream still
// returns exactly the distinct values.
//
// This started life as a FIX-DETECTOR: the SQL-pagination oracle found that the hash
// DISTINCT operator kept its dedup set in memory per page, so a resume across a
// scanned-rows boundary re-admitted values it had already emitted and the statement
// returned duplicate rows. The detector asserted the bug was still PRESENT and instructed
// whoever fixed it to convert it into a correctness test. The executor now carries the
// seen-set through the continuation (executeHashDistinct over
// gen.DistinctHashContinuation), so this is that correctness assertion, and bare DISTINCT
// is back in the queries() sweep.
//
// GROUP BY over the same data is checked alongside it, deliberately: the two operators
// dedup by different machinery, so exercising both means a regression names which one
// broke instead of leaving that to be re-derived.
func TestDistinctContinuationDedups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := newHarness(999)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	// cat ∈ {0,1} scattered (unsorted), so DISTINCT cat must be exactly {0,1}. The
	// scattering is load-bearing: it puts a page boundary inside a duplicate run, where
	// an adjacent-dedup implementation that carries no state would fail. Sorted input
	// would let that implementation pass with the defect fully present.
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

	// Unpaged DISTINCT is the reference: exactly the 2 distinct values.
	unpaged := rowsAt("SELECT DISTINCT cat FROM t", noPageLimit)
	if len(unpaged) != 2 {
		t.Fatalf("unpaged DISTINCT should be 2 distinct rows, got %d: %v", len(unpaged), unpaged)
	}

	// GROUP BY under pagination — the aggregate continuation path.
	if got := rowsAt("SELECT cat FROM t GROUP BY cat", 1); len(got) != 2 {
		t.Fatalf("paged GROUP BY returned %d rows, want 2: %v — the aggregate continuation "+
			"lost its group state", len(got), got)
	}

	// The former bug: paged DISTINCT re-emitting duplicates. A page limit of 1 forces a
	// resume between every single row, so every duplicate run straddles a boundary.
	paged := rowsAt("SELECT DISTINCT cat FROM t", 1)
	if len(paged) != 2 {
		t.Fatalf("paged DISTINCT returned %d rows, want 2 (unpaged %v): %v — the hash-distinct "+
			"seen-set is not surviving the continuation resume, so duplicates are re-admitted",
			len(paged), unpaged, paged)
	}

	// And it is the same SET, not merely the same count — the opposite failure (a resume
	// that DROPS a distinct value) also lands on a count of 2 if a duplicate slips in.
	pagedSet := map[string]bool{}
	for _, r := range paged {
		pagedSet[r] = true
	}
	if len(pagedSet) != len(paged) {
		t.Fatalf("paged DISTINCT %v contains a repeated value — dedup state was lost across a page", paged)
	}
	for _, want := range unpaged {
		if !pagedSet[want] {
			t.Fatalf("paged DISTINCT %v is missing value %q present unpaged %v — the resume "+
				"DROPPED a distinct value", paged, want, unpaged)
		}
	}
}
