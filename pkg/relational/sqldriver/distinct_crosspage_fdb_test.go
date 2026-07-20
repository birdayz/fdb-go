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

// TestFDB_SelectDistinct_CrossPageDedup_Unordered pins the HASH-path fix
// (Approach B): with no ORDER BY and no index on the dedup column,
// the input is UNORDERED, so the planner keeps the hash distinct (streaming's
// adjacency precondition fails). The resume-clean hash cursor must carry its
// seen-set through the scanned-rows continuation so duplicates SCATTERED across
// pages are not re-admitted. g = id % 10 interleaves every value across the
// whole scan — worst case for the fresh-per-page set.
func TestFDB_SelectDistinct_CrossPageDedup_Unordered(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_distinct_unord", "distinctunord",
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const scanLimit = 2
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
	})

	const distinctVals = 10
	const total = 50
	for id := 1; id <= total; id++ {
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, g) VALUES (%d, %d)", id, id%distinctVals)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// No ORDER BY, no index on g → unordered primary scan → hash path.
	rows, err := conn.QueryContext(ctx, "SELECT DISTINCT g FROM t")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var got []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, g)
		seen[g] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != distinctVals {
		t.Fatalf("unordered SELECT DISTINCT g over %d-row scan-limit pages = %d rows %v, want %d distinct "+
			"(a fresh-per-page hash-set re-admits scattered straddling duplicates)",
			scanLimit, len(got), got, distinctVals)
	}
	for v := int64(0); v < distinctVals; v++ {
		if !seen[v] {
			t.Fatalf("unordered DISTINCT dropped value %d (got %v)", v, got)
		}
	}
}

// TestFDB_SelectDistinct_HashUnderLimit_CrossPage pins the hash path composed
// UNDER a LIMIT plan across page boundaries. The g sequence 0,1,0,1,0,1,2,3,4...
// repeats 0 and 1 across scanLimit=2 pages BEFORE the 5th distinct is found, so
// LIMIT 5 = {0,1,2,3,4} only if the hash cursor's seen-set survives the
// continuation: a fresh-per-page set would re-emit the 0,1 duplicates and the
// limit would stop at {0,1} with duplicate output rows. Exercises the limit
// envelope's eager continuation serialization wrapping the resume-clean hash
// continuation.
func TestFDB_SelectDistinct_HashUnderLimit_CrossPage(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_distinct_hlim", "distincthlim",
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const scanLimit = 2
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
	})

	// g by id: 0,1,0,1,0,1 (dups straddling pages), then 2,3,4,5,6,7,8,9.
	gSeq := []int{0, 1, 0, 1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	for id, g := range gSeq {
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, g) VALUES (%d, %d)", id+1, g)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rows, err := conn.QueryContext(ctx, "SELECT DISTINCT g FROM t LIMIT 5")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var got []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, g)
		seen[g] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(got) != 5 || len(seen) != 5 {
		t.Fatalf("DISTINCT g LIMIT 5 under paginated hash = %v (%d distinct), want 5 distinct rows "+
			"(a lost seen-set re-emits the 0,1 duplicates and the limit fills with duplicates)", got, len(seen))
	}
	for v := int64(0); v < 5; v++ {
		if !seen[v] {
			t.Fatalf("DISTINCT g LIMIT 5 = %v, want the set {0,1,2,3,4}", got)
		}
	}
}

// TestFDB_SelectDistinct_BudgetLoudFail pins the safety property the whole
// design leans on: a hash DISTINCT whose seen-set would exceed the statement
// memory budget FAILS LOUDLY (ErrCodeExecutionLimitReached) rather than
// silently dropping or re-admitting rows. A tiny MAX_STATEMENT_MEMORY_BYTES with
// many distinct values forces the boundedSet charge to breach.
func TestFDB_SelectDistinct_BudgetLoudFail(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_distinct_budget", "distinctbudget",
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		// Tiny budget: a few hundred distinct keys cannot fit.
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxStatementMemoryBytes, 256).Build())
	})

	for id := 1; id <= 400; id++ {
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, g) VALUES (%d, %d)", id, id)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rows, err := conn.QueryContext(ctx, "SELECT DISTINCT g FROM t")
	if err == nil {
		// Some drivers surface the error on the first scan rather than at query
		// time; drain and capture it.
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if err == nil {
		t.Fatal("SELECT DISTINCT over a 400-key set with a 256-byte budget must FAIL LOUDLY " +
			"(ErrCodeExecutionLimitReached), never silently drop/complete")
	}
	if !strings.Contains(err.Error(), "limit") && !strings.Contains(err.Error(), "memory") &&
		!strings.Contains(err.Error(), string(api.ErrCodeExecutionLimitReached)) {
		t.Fatalf("budget breach surfaced as %q, want an execution-limit/memory error", err.Error())
	}
}

// TestFDB_SelectDistinct_CrossPageDedup_WithFilter pins cross-page DISTINCT
// dedup when a WHERE predicate on a non-indexed column is present: the input is
// ordered for the distinct by an in-memory sort (driven by ORDER BY g) sitting
// above the filter, and the streaming distinct must still dedup cleanly across
// scanned-rows pages. (The push-below-filter rebuild's streaming recompute is
// pinned deterministically by TestPushDistinctBelowFilter_PreservesStreaming in
// the cascades package.)
func TestFDB_SelectDistinct_CrossPageDedup_WithFilter(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_distinct_xpage_filt", "distinctxpagefilt",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON t (g)")
	ctx := context.Background()

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
			// v = id (non-indexed, unique) so the WHERE predicate is a genuine
			// filter on a column the g-index does not cover.
			if _, err := conn.ExecContext(ctx,
				fmt.Sprintf("INSERT INTO t (id, g, v) VALUES (%d, %d, %d)", id, g, id)); err != nil {
				t.Fatalf("insert: %v", err)
			}
			id++
		}
	}

	// WHERE v <> 3 removes exactly one g=0 duplicate (id=3); every distinct g
	// still survives, so the deduped result stays 0..9. The predicate is on the
	// non-indexed v, so it stays a Filter over the ordered g-index scan.
	const q = "SELECT DISTINCT g FROM t WHERE v <> 3 ORDER BY g"
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
		t.Fatalf("SELECT DISTINCT g WHERE v<>3 over %d-row scan-limit pages = %d rows %v, want %d distinct "+
			"(a push-below-filter rebuild that dropped Streaming re-admits straddling duplicates)",
			scanLimit, len(got), got, distinctVals)
	}
	for i, g := range got {
		if g != int64(i) {
			t.Fatalf("distinct row %d = %d, want %d (full result %v)", i, g, i, got)
		}
	}
}
