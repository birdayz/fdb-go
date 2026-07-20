package sqldriver_test

// End-to-end proof for the streaming DISTINCT fix (TODO.md C5): over input
// ordered by the dedup key, a duplicate run that STRADDLES a scanned-rows
// continuation boundary must NOT be re-admitted on resume.
//
// SELECT DISTINCT is a Go-only extension (Java's fdb-relational does not dedup
// it). The pre-fix executeDistinct rebuilt its seen-set FRESH per page, so with
// EXECUTION_SCANNED_ROWS_LIMIT forcing many mid-run breaks every straddling
// duplicate leaked. The streaming distinct (RecordQueryDistinctPlan.Streaming,
// set by ImplementDistinctFinalRule when the inner ordering makes equal rows
// adjacent) carries only the last emitted key through the DedupContinuation and
// dedups cleanly across pages.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_SelectDistinct_CrossPageDedup(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_distinct_xpage", "distinctxpage",
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON t (g)")
	ctx := context.Background()

	// scanLimit=2 with 5-row runs guarantees every distinct value's run
	// straddles multiple page boundaries — the fresh-per-page hash-set loses
	// its dedup state at each break and re-admits.
	const scanLimit = 2
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
	})

	const distinctVals = 10
	const repeats = 5
	id := 1
	for g := 0; g < distinctVals; g++ {
		for r := 0; r < repeats; r++ {
			if _, err := conn.ExecContext(ctx,
				fmt.Sprintf("INSERT INTO t (id, g) VALUES (%d, %d)", id, g)); err != nil {
				t.Fatalf("insert: %v", err)
			}
			id++
		}
	}

	const q = "SELECT DISTINCT g FROM t ORDER BY g"

	// The ordering-aware fix only engages when the DISTINCT input is ordered by
	// the dedup key. ORDER BY g drives the planner onto the ordered t_g index
	// (no in-memory sort), which feeds the distinct g-ordered rows — the
	// precondition for the streaming path. A Sort here would mean the input was
	// unordered and the streaming path never engaged.
	plan := planExplainVia(t, ctx, db, q)
	if strings.Contains(plan, "Sort") {
		t.Fatalf("DISTINCT input must be ordered by the t_g index (no in-memory sort) to exercise "+
			"the streaming path; got plan:\n%s", plan)
	}

	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != distinctVals {
		t.Fatalf("SELECT DISTINCT g over %d-row scan-limit pages = %d rows %v, want %d distinct "+
			"(a fresh-per-page hash-set re-admits straddling duplicates → %d rows)",
			scanLimit, len(got), got, distinctVals, distinctVals*repeats)
	}
	for i, g := range got {
		if g != int64(i) {
			t.Fatalf("distinct row %d = %d, want %d (full result %v)", i, g, i, got)
		}
	}

	// LIMIT/OFFSET over the paginated DISTINCT — the C5 repro flagged a wrong
	// window here: with re-admitted duplicates the OFFSET/LIMIT counted phantom
	// rows. The streaming dedup feeds the correct deduped stream, so OFFSET 3
	// LIMIT 4 selects distinct values 3,4,5,6.
	const qWin = "SELECT DISTINCT g FROM t ORDER BY g LIMIT 4 OFFSET 3"
	wrows, werr := conn.QueryContext(ctx, qWin)
	if werr != nil {
		t.Fatalf("query window: %v", werr)
	}
	defer wrows.Close()
	var win []int64
	for wrows.Next() {
		var g int64
		if err := wrows.Scan(&g); err != nil {
			t.Fatalf("scan window: %v", err)
		}
		win = append(win, g)
	}
	if err := wrows.Err(); err != nil {
		t.Fatalf("rows.Err window: %v", err)
	}
	want := []int64{3, 4, 5, 6}
	if fmt.Sprint(win) != fmt.Sprint(want) {
		t.Fatalf("DISTINCT g ORDER BY g LIMIT 4 OFFSET 3 over paginated scan = %v, want %v", win, want)
	}
}
