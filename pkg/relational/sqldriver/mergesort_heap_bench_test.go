package sqldriver_test

// End-to-end, real-FDB companion to
// pkg/recordlayer/query/executor.BenchmarkMergeSortCursor_HeapVsLinear: the
// same InUnion shape (IN list + ORDER BY the indexed column, which plans as
// an InUnion per TestFDB_InUnionRowContinuation_BudgetSweep_Paged) driven
// through the full driver stack against a real FoundationDB cluster, so any
// wall-clock effect (or lack of one, once FDB round-trip latency is in the
// loop) is measured rather than assumed. Run with the mergeSortCursor heap
// change present vs. absent (e.g. a clean worktree of the parent commit) to
// get a before/after comparison; this file alone only measures "after".

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// benchInUnionMergeSort seeds a g-bucketed table (numLegs buckets,
// rowsPerLeg rows each) with a secondary index on g, then times
// "SELECT id, g FROM t WHERE g IN (<numLegs values>) ORDER BY g, id" — the
// InUnion shape whose per-row leg selection is exactly what the heap change
// replaces. Reports rows/sec.
func benchInUnionMergeSort(b *testing.B, numLegs, rowsPerLeg int) {
	b.Helper()
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_inunion_%d", seq)
	tmpl := fmt.Sprintf("bench_inunion_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE T (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON T (g)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Seed numLegs g-buckets (1..numLegs), rowsPerLeg rows each, one batched
	// INSERT so setup cost doesn't scale with round trips.
	var values strings.Builder
	id := int64(1)
	for g := 1; g <= numLegs; g++ {
		for r := 0; r < rowsPerLeg; r++ {
			if id > 1 {
				values.WriteByte(',')
			}
			fmt.Fprintf(&values, "(%d,%d)", id, g)
			id++
		}
	}
	execOrFail(b, db, ctx, "INSERT INTO T (id, g) VALUES "+values.String())

	inList := make([]string, numLegs)
	for g := 1; g <= numLegs; g++ {
		inList[g-1] = strconv.Itoa(g)
	}
	query := fmt.Sprintf("SELECT id, g FROM T WHERE g IN (%s) ORDER BY g, id", strings.Join(inList, ","))

	// Confirm the InUnion shape once, outside the timed loop — a plan that
	// silently fell back to some other shape would make the measurement
	// meaningless.
	explainRows, err := db.QueryContext(ctx, "EXPLAIN "+query)
	if err != nil {
		b.Fatalf("EXPLAIN: %v", err)
	}
	var plan strings.Builder
	for explainRows.Next() {
		var line string
		if err := explainRows.Scan(&line); err != nil {
			b.Fatalf("explain scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	explainRows.Close()
	if !strings.Contains(plan.String(), "InUnion(") {
		b.Fatalf("query did not plan as InUnion, got:\n%s", plan.String())
	}

	totalRows := int64(numLegs * rowsPerLeg)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		var n int64
		for rows.Next() {
			var rid, g int64
			if err := rows.Scan(&rid, &g); err != nil {
				b.Fatalf("scan: %v", err)
			}
			n++
		}
		rows.Close()
		if n != totalRows {
			b.Fatalf("iteration %d: got %d rows, want %d", i, n, totalRows)
		}
	}
	b.StopTimer()
	if secs := b.Elapsed().Seconds(); secs > 0 {
		b.ReportMetric(float64(b.N)*float64(totalRows)/secs, "rows/sec")
	}
}

// BenchmarkFDB_InUnionMergeSort_N3 through _N1000 measure the InUnion merge
// end to end at the leg counts requested for the InJoin/InUnion planner
// comparison (3, 10, 100, 1000 IN-list values), 5 rows per leg.
func BenchmarkFDB_InUnionMergeSort_N3(b *testing.B)    { benchInUnionMergeSort(b, 3, 5) }
func BenchmarkFDB_InUnionMergeSort_N10(b *testing.B)   { benchInUnionMergeSort(b, 10, 5) }
func BenchmarkFDB_InUnionMergeSort_N100(b *testing.B)  { benchInUnionMergeSort(b, 100, 5) }
func BenchmarkFDB_InUnionMergeSort_N1000(b *testing.B) { benchInUnionMergeSort(b, 1000, 5) }
