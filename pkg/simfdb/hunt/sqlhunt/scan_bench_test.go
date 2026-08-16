package sqlhunt

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// The read path over SimFDB, with no Docker and no real cluster: SimFDB is a
// full in-process FDB, so a query here executes the SAME planner, cursors and
// Value evaluation a container run does, minus the network and the storage
// engine. That makes it the right instrument for a PER-ROW cost question — the
// thing being measured is Go work per row, and the thing being removed is the
// 2m36s of container ingest that dominates the stress suite and hides it.
//
// It is written against the SQL surface on purpose, so the identical file
// compiles and runs on master. A per-row regression is a RATIO between two
// trees, and a benchmark that only exists on one of them cannot state one.

func benchHarness(b *testing.B, rows int) *sql.DB {
	b.Helper()
	h, err := qcNewHarness(uint64(rows))
	if err != nil {
		b.Fatalf("harness: %v", err)
	}
	b.Cleanup(h.close)

	ctx := context.Background()
	const perTx = 500
	for start := 0; start < rows; start += perTx {
		stmt := "INSERT INTO t VALUES "
		for i := start; i < start+perTx && i < rows; i++ {
			if i > start {
				stmt += ","
			}
			stmt += fmt.Sprintf("(%d,%d,%d)", i, i%7, i*3)
		}
		if _, err := h.db.ExecContext(ctx, stmt); err != nil {
			b.Fatalf("insert at %d: %v", start, err)
		}
	}
	return h.db
}

func drain(b *testing.B, db *sql.DB, query string) int {
	b.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		b.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close() //nolint:errcheck
	cols, err := rows.Columns()
	if err != nil {
		b.Fatalf("columns: %v", err)
	}
	cells := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range cells {
		ptrs[i] = &cells[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			b.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("rows: %v", err)
	}
	return n
}

// scanBenchRows is stated as a constant rather than passed per-benchmark so the
// two trees cannot be compared at different populations by accident.
const scanBenchRows = 20_000

func benchScan(b *testing.B, query string, wantRows int) {
	db := benchHarness(b, scanBenchRows)
	// One drain outside the timer: the first execution plans the query and
	// warms the plan cache, and planning is a PER-QUERY cost being measured
	// separately. Leaving it inside would blend the two regressions.
	if got := drain(b, db, query); got != wantRows {
		b.Fatalf("warmup returned %d rows, want %d — the benchmark is not measuring the shape it names", got, wantRows)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := drain(b, db, query); got != wantRows {
			b.Fatalf("iteration %d returned %d rows, want %d", i, got, wantRows)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(wantRows), "rows/op")
}

// BenchmarkScanAllWide is the stress suite's "scan all rows wide" — every
// column of every row, the shape that regressed hardest.
func BenchmarkScanAllWide(b *testing.B) {
	benchScan(b, "SELECT id, cat, val FROM t", scanBenchRows)
}

// BenchmarkScanOneColumn isolates per-COLUMN cost against the benchmark above:
// same rows, one third of the columns.
func BenchmarkScanOneColumn(b *testing.B) {
	benchScan(b, "SELECT id FROM t", scanBenchRows)
}

// BenchmarkScanFilterSparse is the stress suite's "full scan sparse filter" —
// every row read and evaluated, almost none returned, so the cost is predicate
// evaluation rather than result materialisation.
func BenchmarkScanFilterSparse(b *testing.B) {
	benchScan(b, "SELECT id FROM t WHERE val = 29997", 1)
}

// BenchmarkScanOrdered is "scan all rows ordered" — the PK-ordered drain.
func BenchmarkScanOrdered(b *testing.B) {
	benchScan(b, "SELECT id, cat, val FROM t ORDER BY id", scanBenchRows)
}

// BenchmarkIndexRange is the stress suite's "idx_amount range" — a covering
// index scan over a large slice of the table.
func BenchmarkIndexRange(b *testing.B) {
	benchScan(b, "SELECT id FROM t WHERE val > 30000", scanBenchRows-10001)
}

// BenchmarkPlanInList is the IN-list shape, which regressed 4.37× on FORTY-SIX
// rows — far too few for a per-row cause. Its cost is planning, so this one
// deliberately does NOT warm the plan cache: each iteration opens a fresh
// statement text so the planner runs every time.
func BenchmarkPlanInList(b *testing.B) {
	db := benchHarness(b, scanBenchRows)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Vary the literal so the plan cache cannot serve the answer; the cost
		// under test is producing the plan, not executing it.
		q := fmt.Sprintf("SELECT id, val FROM t WHERE cat IN (0,1,2,3,4) AND id > %d ORDER BY id", i%17)
		drain(b, db, q)
	}
}
