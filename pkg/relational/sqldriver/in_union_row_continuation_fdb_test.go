package sqldriver_test

// RFC-180 A3/A5 — driver-level pins for the IN-plan continuations: an IN-list
// query over an indexed column, driven in paginate mode under a sweep of
// EXECUTION_SCANNED_ROWS_LIMIT budgets, must equal the unpaged read exactly.
// The ordered variant (IN + ORDER BY on the indexed column) plans as an
// InUnion whose mergeSortCursor now carries per-child UnionContinuation states
// (A5); the unordered variant exercises the IN plan's flatMap/concat
// continuation (A3). Before A3/A5 both executors DISCARDED the incoming
// continuation, so any page boundary inside the IN plan either errored
// (out-of-band stop → RFC-106a ScanLimitReachedError) or replayed rows.

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func setupInUnionContDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := setupErrorTestDB(t, "/testdb_inunioncont", "inunioncont",
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON t (g)")
	// Three g buckets with several rows each, so per-leg index probes span
	// multiple rows and a small scanned-rows budget lands INSIDE legs and
	// between them. ORDER BY (g, id) — the index's full (g, pk) entry order —
	// is what makes the InUnion candidate satisfy the requested ordering
	// outright and win over the InJoin + InMemorySort alternative.
	const q = "SELECT id, g FROM t WHERE g IN (30, 10, 20) ORDER BY g, id"
	return db, q
}

// TestFDB_InUnionRowContinuation_BudgetSweep_Paged pins the InUnion per-child
// continuation states end-to-end: the IN + ORDER BY query (InUnion plan shape,
// asserted via EXPLAIN) under every small scanned-rows budget must return
// exactly the unpaged rows, in order — every internal page boundary resumes
// the merge from its UnionContinuation snapshot.
func TestFDB_InUnionRowContinuation_BudgetSweep_Paged(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, q := setupInUnionContDB(t)
	ctx := t.Context()

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t (id, g) VALUES (1,10),(2,10),(3,10),(4,20),(5,20),(6,30),(7,30),(8,30),(9,40)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The shape under test: the ordered IN-list must plan as an InUnion —
	// otherwise this pins some other cursor's continuation.
	plan := planExplainVia(t, ctx, db, q)
	if !strings.Contains(plan, "InUnion(") {
		t.Fatalf("IN + ORDER BY (g, id) over the indexed column must plan as InUnion; got:\n%s", plan)
	}

	// Order matters: the merge emits in (g, id) order — compare verbatim.
	unpaged := readIDPairs(t, ctx, db, q)
	want := "[[1 10] [2 10] [3 10] [4 20] [5 20] [6 30] [7 30] [8 30]]" // buckets 10,20,30; 40 excluded
	if fmt.Sprint(unpaged) != want {
		t.Fatalf("unpaged IN query = %v, want %v", unpaged, want)
	}

	// Budget sweep: every alignment of the internal page boundary — inside a
	// leg, at a leg boundary, mid-merge — must resume exactly. Before A5 the
	// merge ERRORED on any out-of-band child stop (RFC-106a) instead of
	// checkpointing, so every one of these paged reads failed.
	for budget := 2; budget <= 8; budget++ {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, budget).Build())
		})
		paged := readIDPairsConn(t, ctx, conn, q)
		if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
			t.Fatalf("InUnion continuation dropped/duplicated/reordered rows (budget %d):\n paged (%d) = %v\n unpaged (%d) = %v",
				budget, len(paged), paged, len(unpaged), unpaged)
		}
	}
}
