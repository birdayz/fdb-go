package executor

import (
	"context"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// wideRows builds n computed QueryResults each carrying a `wbytes`-byte string
// payload under the given key, so charging them advances the budget by a
// meaningful, observable amount.
func wideRows(n, wbytes int, key string) []QueryResult {
	pad := string(make([]byte, wbytes))
	rows := make([]QueryResult, n)
	for i := range rows {
		rows[i] = QueryResult{Datum: map[string]any{"ID": int64(i), key: pad}}
	}
	return rows
}

// TestChargeCoverage_AllBufferPaths drives one (or a few) wide row(s) through
// EACH accounted buffer path RFC-130 §2.4 enumerates and asserts the SHARED
// ExecuteState.memUsed strictly advanced — proving no path silently bypasses
// the accountant. A single ExecuteState with an effectively-unlimited budget is
// threaded through every path so the only thing under test is "did this path
// charge", never "did it trip". Each sub-case captures memUsed before/after and
// fails if it did not grow.
//
// This is the no-bypass pin: if a future refactor routes a buffer site around
// boundedBuffer/boundedSet/CollectAllBounded/TempTable.Add, its sub-case here
// stops advancing memUsed and fails.
func TestChargeCoverage_AllBufferPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// One shared, effectively-unlimited budget across every path (a huge
	// positive limit keeps the active counter running without ever tripping).
	st := recordlayer.NewExecuteState(1 << 62)

	// advanced runs fn and asserts memUsed strictly grew.
	advanced := func(t *testing.T, name string, fn func(t *testing.T)) {
		t.Helper()
		before := st.MemUsed()
		fn(t)
		after := st.MemUsed()
		if after <= before {
			t.Fatalf("%s: ExecuteState.memUsed did not advance (before=%d after=%d) — "+
				"this buffer path bypassed the RFC-130 accountant", name, before, after)
		}
	}

	const w = 256 // payload bytes per row

	// 1. CollectAllBounded (covers buffered-union branch, NLJ inner, DML target
	//    sets, recursive-CTE per level, DFS root/children — all share it).
	advanced(t, "CollectAllBounded", func(t *testing.T) {
		cur := recordlayer.FromList(wideRows(5, w, "P"))
		if _, err := CollectAllBounded(ctx, cur, st, 1_000_000, "coverage"); err != nil {
			t.Fatalf("CollectAllBounded: %v", err)
		}
	})

	// 2. boundedBuffer.Append directly (executeLoadByKeys). (DML result echoes are
	// deliberately NOT charged — they ride the pre-charged target set; codex #328.)
	advanced(t, "boundedBuffer", func(t *testing.T) {
		buf := newBoundedBuffer[QueryResult](st, 0, "coverage", estimateQueryResultBytes)
		for _, r := range wideRows(3, w, "P") {
			if err := buf.Append(r); err != nil {
				t.Fatalf("boundedBuffer.Append: %v", err)
			}
		}
	})

	// 3. boundedSet.Add on new keys (distinct seen-set, recursive dedup).
	advanced(t, "boundedSet", func(t *testing.T) {
		set := newBoundedSet[string](st)
		for _, k := range []string{"a-very-long-distinct-key-1", "a-very-long-distinct-key-2"} {
			if _, err := set.Add(k, int64(len(k))); err != nil {
				t.Fatalf("boundedSet.Add: %v", err)
			}
		}
	})

	// 4. TempTable.Add (recursive-CTE working set + TempTableInsertPlan target).
	advanced(t, "TempTable.Add", func(t *testing.T) {
		tt := NewTempTableWithState(st)
		for _, r := range wideRows(4, w, "P") {
			if err := tt.Add(r); err != nil {
				t.Fatalf("TempTable.Add: %v", err)
			}
		}
	})

	// 5. memorySortCursor in-memory sort buffer.
	advanced(t, "memorySortCursor", func(t *testing.T) {
		inner := recordlayer.FromList(wideRows(6, w, "ID"))
		c := newMemorySortCursor(inner, []string{"ID"}, []bool{false}, st)
		drainCursor(t, ctx, c)
	})

	// 6. customSortCursor in-memory sort buffer.
	advanced(t, "customSortCursor", func(t *testing.T) {
		inner := recordlayer.FromList(wideRows(6, w, "ID"))
		c := newCustomSortCursor(inner, func([]QueryResult) error { return nil }, st)
		drainCursor(t, ctx, c)
	})

	// 7. nljCursor hash-index (≥100 inner rows + a single-column equijoin →
	//    tryBuildHashIndex charges the index).
	advanced(t, "nljCursor hash-index", func(t *testing.T) {
		innerRows := make([]QueryResult, 150)
		for i := range innerRows {
			innerRows[i] = QueryResult{Datum: map[string]any{"K": int64(i)}}
		}
		outer := recordlayer.FromList([]QueryResult{{Datum: map[string]any{"K": int64(1)}}})
		preds := []predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: &values.FieldValue{
					Child: &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier("O")},
					Field: "K",
				},
				Comparison: predicates.Comparison{
					Type: predicates.ComparisonEquals,
					Operand: &values.FieldValue{
						Child: &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier("I")},
						Field: "K",
					},
				},
			},
		}
		c := newNLJCursor(outer, innerRows, plans.JoinInner, "O", "I", preds, EmptyEvaluationContext(), st)
		if c.hashIndex == nil {
			t.Fatal("hash index was not built — coverage case does not exercise the charge path")
		}
		drainCursor(t, ctx, c)
	})
}

// drainCursor pulls a cursor to exhaustion, failing on any error.
func drainCursor(t *testing.T, ctx context.Context, c recordlayer.RecordCursor[QueryResult]) {
	t.Helper()
	for {
		res, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !res.HasNext() {
			return
		}
	}
}
