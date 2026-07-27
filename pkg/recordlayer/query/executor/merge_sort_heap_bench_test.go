package executor

// Isolated CPU benchmark for mergeSortCursor's leg-selection algorithm:
// heap-based (production OnNext) vs the replaced O(N)-per-row linear scan
// (referenceMergeSortOnNext, merge_sort_heap_differential_test.go). Both
// sides drive the IDENTICAL cursor plumbing (pull/consume/evalCompKeys) over
// in-memory legs — no FDB, no network, no query planning — so the only
// thing this measures is the selection algorithm itself, uncontaminated by
// I/O latency that would otherwise dwarf a per-row comparison-count
// difference. See TestFDB_InUnionMergeSort_RowsPerSec (sqldriver package)
// for the end-to-end, real-FDB-backed counterpart.

import (
	"context"
	"fmt"
	"testing"
)

// buildBenchLegs builds numLegs ascending legs of rowsPerLeg int64 "id" rows
// each, with globally distinct values (leg L holds L, L+numLegs, L+2*numLegs,
// ...) so there are effectively no cross-leg ties — the common case an
// InUnion over a long, mostly-distinct IN list hits.
func buildBenchLegs(numLegs, rowsPerLeg int) [][]QueryResult {
	legs := make([][]QueryResult, numLegs)
	for l := 0; l < numLegs; l++ {
		rows := make([]QueryResult, rowsPerLeg)
		for r := 0; r < rowsPerLeg; r++ {
			rows[r] = qr("id", int64(r*numLegs+l))
		}
		legs[l] = rows
	}
	return legs
}

// benchDrainMergeSort times draining b.N freshly-built merges of numLegs x
// rowsPerLeg rows via either the production heap OnNext or the reference
// linear scan, reporting rows/sec. Leg construction and cursor setup happen
// OUTSIDE the timed region (b.StopTimer/b.StartTimer) so only the merge
// itself is measured.
func benchDrainMergeSort(b *testing.B, numLegs, rowsPerLeg int, useHeap bool) {
	b.Helper()
	ctx := context.Background()
	var totalRows int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		legs := buildBenchLegs(numLegs, rowsPerLeg)
		c := newMergeSortCursor(rowsToCursors(legs), idKey(), false, true)
		b.StartTimer()

		var n int64
		if useHeap {
			for {
				r, err := c.OnNext(ctx)
				if err != nil {
					b.Fatalf("OnNext: %v", err)
				}
				if !r.HasNext() {
					break
				}
				n++
			}
		} else {
			for {
				r, err := referenceMergeSortOnNext(c, ctx)
				if err != nil {
					b.Fatalf("referenceMergeSortOnNext: %v", err)
				}
				if !r.HasNext() {
					break
				}
				n++
			}
		}

		b.StopTimer()
		c.Close()
		totalRows += n
	}
	b.StopTimer()
	if secs := b.Elapsed().Seconds(); secs > 0 {
		b.ReportMetric(float64(totalRows)/secs, "rows/sec")
	}
}

// BenchmarkMergeSortCursor_HeapVsLinear compares the two selection
// algorithms across the leg counts requested for the InJoin/InUnion
// measurement (3, 10, 100, 1000), 20 rows/leg, no cross-leg ties.
func BenchmarkMergeSortCursor_HeapVsLinear(b *testing.B) {
	for _, n := range []int{3, 10, 100, 1000} {
		n := n
		b.Run(fmt.Sprintf("heap/N=%d", n), func(b *testing.B) {
			benchDrainMergeSort(b, n, 20, true)
		})
		b.Run(fmt.Sprintf("linear/N=%d", n), func(b *testing.B) {
			benchDrainMergeSort(b, n, 20, false)
		})
	}
}
