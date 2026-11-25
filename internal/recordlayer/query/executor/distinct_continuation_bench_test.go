package executor

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// benchmarkDistinctPagedDrain drains an all-distinct DISTINCT over rows rows in
// pages of rows/pages, round-tripping the real continuation bytes, and reports
// the total continuation bytes written across the drain as a custom metric.
//
// withScratch selects the two encodings, which is the whole point: BY REFERENCE
// (the statement scratch hands the live seen-set to the next page) versus BY
// VALUE (every page's continuation carries every key emitted so far). The
// second is O(pages^2) in bytes written and keys re-parsed, and comparing them
// in ONE binary makes that a measurement rather than a claim about two builds.
func benchmarkDistinctPagedDrain(b *testing.B, rows, pages int, withScratch bool) {
	ctx := context.Background()
	perPage := rows / pages
	alias := values.NamedCorrelationIdentifier(fmt.Sprintf("bench_%s", b.Name()))
	base := EmptyEvaluationContext()
	table := base.GetOrCreateTempTable(alias, nil)
	for i := 0; i < rows; i++ {
		if err := table.Add(dmap(map[string]any{"V": int64(i)})); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	plan := mustExecutorConstruct(plans.NewRecordQueryDistinctPlan(mustTempTableScan(b, base, alias)))

	var contBytes int64
	b.ReportAllocs()
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		evalCtx := base
		if withScratch {
			evalCtx = base.WithExecutionScratch(NewExecutionScratch())
		}
		state := recordlayer.NewExecuteState(1 << 30)
		var cont []byte
		emitted := 0
		for {
			props := recordlayer.DefaultExecuteProperties()
			props.State = state
			props = props.WithReturnedRowLimit(perPage)
			cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
			if err != nil {
				b.Fatalf("ExecutePlan: %v", err)
			}
			var last recordlayer.RecordCursorResult[QueryResult]
			for {
				res, err := cursor.OnNext(ctx)
				if err != nil {
					b.Fatalf("OnNext: %v", err)
				}
				last = res
				if !res.HasNext() {
					break
				}
				emitted++
			}
			next := last.GetContinuation()
			var encoded []byte
			if next != nil && !next.IsEnd() {
				encoded, err = next.ToBytes()
				if err != nil {
					b.Fatalf("ToBytes: %v", err)
				}
			}
			_ = cursor.Close()
			if encoded == nil {
				break
			}
			contBytes += int64(len(encoded))
			cont = encoded
		}
		if emitted != rows {
			b.Fatalf("drain emitted %d rows, want %d", emitted, rows)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(contBytes)/float64(b.N), "contBytes/op")
}

func BenchmarkDistinctPagedDrain_ByReference(b *testing.B) {
	benchmarkDistinctPagedDrain(b, 20000, 20, true)
}

func BenchmarkDistinctPagedDrain_ByValue(b *testing.B) {
	benchmarkDistinctPagedDrain(b, 20000, 20, false)
}
