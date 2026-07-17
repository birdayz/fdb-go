package sqldriver_test

// RFC-180 A6 — driver-level pins for the streaming-aggregate and in-memory-sort
// PER-ROW continuations. The consumer of an emitted row's continuation in the
// engine is a parent cursor that snapshots its child's position mid-stream —
// here a FlatMap join whose OUTER is the aggregate / sort: when a page boundary
// (EXECUTION_SCANNED_ROWS_LIMIT, paginate mode) lands mid-inner, the
// FlatMapContinuation carries priorOuter = the OUTER ROW's continuation, and the
// next page re-opens the aggregate/sort FROM that row's continuation. Before A6
// those rows carried the fake {0x00} bytes and the resume failed with "invalid
// aggregate/sort continuation" — a paged query ERRORED where the unpaged one
// succeeded.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func setupRowContDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db := setupErrorTestDB(t, "/testdb_rowcont_"+tag, "rowcont"+tag,
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON t (g) "+
			"CREATE TABLE t2 (id BIGINT, PRIMARY KEY (id))")
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// t: three groups on g (sizes 2, 2, 1); t2: join fodder so each outer row
	// has several inner rows and a tiny scan budget lands mid-inner.
	exec("INSERT INTO t (id, g) VALUES (1, 1), (2, 1), (3, 2), (4, 2), (5, 3)")
	exec("INSERT INTO t2 (id) VALUES (1), (2), (3), (4)")
	return db, ctx
}

// pagedConn returns a connection in paginate mode with a small scanned-rows
// budget, so the engine crosses many internal page boundaries and resumes via
// continuations transparently.
func pagedConn(t *testing.T, db *sql.DB, budget int) *sql.Conn {
	t.Helper()
	return pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, budget).Build())
	})
}

// TestFDB_AggregateRowContinuation_FlatMapOuter_Paged: a GROUP BY derived table
// as the FlatMap OUTER. A page boundary mid-inner snapshots the current
// aggregate row's continuation into the FlatMapContinuation; the resume
// re-opens the GROUP BY between groups — paged must equal unpaged exactly.
func TestFDB_AggregateRowContinuation_FlatMapOuter_Paged(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := setupRowContDB(t, "agg")

	const q = "SELECT x.g, b.id FROM (SELECT g, COUNT(*) AS c FROM t GROUP BY g) x, t2 b WHERE b.id > x.g"

	// The shape under test: the streaming aggregate must sit on the FlatMap's
	// OUTER side (its Explain segment precedes ", inner=") — only then does a
	// mid-inner page boundary serialize the AGGREGATE ROW's continuation.
	plan := planExplainVia(t, ctx, db, q)
	if !strings.Contains(plan, "FlatMap(") || !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("query must plan as FlatMap over a StreamingAgg outer; got:\n%s", plan)
	}
	if agg, inner := strings.Index(plan, "StreamingAgg"), strings.Index(plan, ", inner="); agg > inner {
		t.Fatalf("StreamingAgg must be the FlatMap OUTER (before \", inner=\"); got:\n%s", plan)
	}

	unpaged := sortPairs(readIDPairs(t, ctx, db, q))
	if len(unpaged) == 0 {
		t.Fatal("setup: join returned no rows")
	}
	// Sweep budgets: a page boundary lands mid-inner while a LATER aggregate
	// row is the current outer (priorOuter = the previous AGGREGATE ROW's
	// continuation) only for some budget/data alignments — sweeping pins every
	// alignment, including the ones where the fake bytes used to ride.
	for budget := 2; budget <= 8; budget++ {
		paged := sortPairs(readIDPairsConn(t, ctx, pagedConn(t, db, budget), q))
		if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
			t.Fatalf("GROUP BY per-row continuation dropped/duplicated rows across page boundaries (budget %d):\n paged (%d) = %v\n unpaged (%d) = %v",
				budget, len(paged), paged, len(unpaged), unpaged)
		}
	}
}

// TestFDB_SortEmitRowContinuation_FlatMapOuter_Paged: an ORDER BY (in-memory
// sort) derived table as the FlatMap OUTER. A page boundary mid-inner
// snapshots the current SORTED ROW's emit-phase continuation (remaining
// buffer + inner-exhausted marker); the resume goes straight to emit — paged
// must equal unpaged exactly.
func TestFDB_SortEmitRowContinuation_FlatMapOuter_Paged(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := setupRowContDB(t, "sort")

	const q = "SELECT a.id, b.id FROM (SELECT id FROM t ORDER BY g DESC, id ASC LIMIT 4) a, t2 b WHERE b.id > a.id"

	plan := planExplainVia(t, ctx, db, q)
	if !strings.Contains(plan, "FlatMap(") || !strings.Contains(plan, "Sort(") {
		t.Fatalf("query must plan as FlatMap over an in-memory Sort outer; got:\n%s", plan)
	}
	if srt, inner := strings.Index(plan, "Sort("), strings.Index(plan, ", inner="); srt > inner {
		t.Fatalf("Sort must be the FlatMap OUTER (before \", inner=\"); got:\n%s", plan)
	}

	unpaged := sortPairs(readIDPairs(t, ctx, db, q))
	if len(unpaged) == 0 {
		t.Fatal("setup: join returned no rows")
	}
	// Sweep budgets (see the aggregate sibling): sorted-row emission costs no
	// scanned rows, so even budget 2 lands boundaries mid-inner with a sorted
	// row live — but sweep anyway to pin every alignment.
	for budget := 2; budget <= 8; budget++ {
		paged := sortPairs(readIDPairsConn(t, ctx, pagedConn(t, db, budget), q))
		if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
			t.Fatalf("ORDER BY emit-phase per-row continuation dropped/duplicated rows across page boundaries (budget %d):\n paged (%d) = %v\n unpaged (%d) = %v",
				budget, len(paged), paged, len(unpaged), unpaged)
		}
	}
}
