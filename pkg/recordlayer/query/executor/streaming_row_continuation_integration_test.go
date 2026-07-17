package executor

// RFC-180 A6 — FDB integration pins for the per-row continuations of the
// streaming aggregate and the in-memory sort: every EMITTED row's continuation
// is a genuine resume point. Each test drives its plan ONE ROW PER TRANSACTION,
// resuming from the emitted row's continuation — the strictest per-row paging —
// and requires the concatenation of pages to equal the unpaginated output
// exactly (no duplicates, no drops). Before A6 every such resume failed with
// "invalid aggregate/sort continuation" (the fake {0x00} bytes).

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// onePerTxRows drives makePlan one row per transaction: each page opens the
// store, executes the plan from the previous row's continuation, takes ONE row
// (recording render(row)), and carries that row's continuation into the next
// transaction. Returns the rendered rows in emission order.
func onePerTxRows(t *testing.T, store *recordlayer.FDBRecordStore, makePlan func() plans.RecordQueryPlan, render func(QueryResult) (string, error)) []string {
	t.Helper()
	ctx := context.Background()
	var got []string
	var continuation []byte
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("per-row resume loop did not converge (over 50 pages)")
		}
		done := false
		_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
				SetSubspace(testSubspace(t)).Open()
			if err != nil {
				return nil, err
			}
			cursor, err := ExecutePlan(ctx, makePlan(), s, EmptyEvaluationContext(), continuation, recordlayer.DefaultExecuteProperties())
			if err != nil {
				return nil, err
			}
			defer cursor.Close()

			res, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !res.HasNext() {
				if !res.GetNoNextReason().IsSourceExhausted() {
					return nil, fmt.Errorf("page %d stopped with %v, want a row or SourceExhausted", page, res.GetNoNextReason())
				}
				done = true
				return nil, nil
			}
			r, rerr := render(res.GetValue())
			if rerr != nil {
				return nil, rerr
			}
			got = append(got, r)
			cb, cerr := res.GetContinuation().ToBytes()
			if cerr != nil {
				return nil, fmt.Errorf("row continuation ToBytes: %w", cerr)
			}
			continuation = cb
			return nil, nil
		})
		if err != nil {
			t.Fatalf("page %d (resume from the previous ROW's continuation): %v", page, err)
		}
		if done {
			break
		}
	}
	return got
}

// aggPriceCountPlan builds StreamingAgg(Sort(Scan)) — COUNT(*) GROUP BY price —
// the planner's GROUP-BY-without-index lowering, so the aggregate's inner
// continuation transitively exercises the sort codec too.
func aggPriceCountPlan(grouped bool) plans.RecordQueryPlan {
	scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
	// price is Order proto field 3 → positional ordinal 2 (the sibling PRICE
	// tests use ordinal 2).
	priceVal := values.NewFieldValueWithResolvedOrdinal("PRICE", 2, values.UnknownType)
	inner := plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{{
		Field: "PRICE", ValueExpr: priceVal, NullsFirst: true,
	}})
	var groupKeys []values.Value
	if grouped {
		groupKeys = []values.Value{priceVal}
	}
	return plans.NewRecordQueryStreamingAggregationPlan(inner, groupKeys, []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
	})
}

// TestIntegration_AggregateRowContinuation_OneRowPerTx pins the GROUP BY
// per-row continuation end-to-end: paging that lands BETWEEN groups (after
// every emitted group row) resumes into exactly the remaining groups.
func TestIntegration_AggregateRowContinuation_OneRowPerTx(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(250)},
	)

	got := onePerTxRows(t, store,
		func() plans.RecordQueryPlan { return aggPriceCountPlan(true) },
		func(qr QueryResult) (string, error) {
			key, ok1 := qr.Positional.Get(0)
			cnt, ok2 := qr.Positional.Get(1)
			if !ok1 || !ok2 {
				return "", fmt.Errorf("aggregate row missing slots: %+v", qr.Positional)
			}
			return fmt.Sprintf("%v=%v", key, cnt), nil
		})

	want := "[50=2 150=2 250=1]"
	if fmt.Sprint(got) != want {
		t.Fatalf("GROUP BY paged one row per tx = %v, want %v (no duplicate/dropped groups across per-row resumes)", got, want)
	}
}

// TestIntegration_ScalarAggregateRowContinuation_NoDuplicate pins the scalar
// COUNT(*) per-row continuation: the single aggregate row resumes to
// end-of-stream — never a duplicate row, never a fabricated COUNT=0 (the
// OrElse stays USE_INNER across the resume, Java's DefaultOnEmpty structure).
func TestIntegration_ScalarAggregateRowContinuation_NoDuplicate(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
	)

	got := onePerTxRows(t, store,
		func() plans.RecordQueryPlan { return aggPriceCountPlan(false) },
		func(qr QueryResult) (string, error) {
			cnt, ok := qr.Positional.Get(0)
			if !ok {
				return "", fmt.Errorf("scalar aggregate row missing slot 0: %+v", qr.Positional)
			}
			return fmt.Sprintf("%v", cnt), nil
		})

	if fmt.Sprint(got) != "[3]" {
		t.Fatalf("scalar COUNT(*) paged one row per tx = %v, want [3] (exactly one row across per-row resumes)", got)
	}
}

// TestIntegration_ScalarAggregateOnEmpty_DefaultRowResumable pins the
// COUNT(*)-on-empty default row (the OrElse USE_OTHER branch): emitted exactly
// once, and a resume from ITS continuation yields nothing more.
func TestIntegration_ScalarAggregateOnEmpty_DefaultRowResumable(t *testing.T) {
	t.Parallel()
	store := setupStore(t) // no rows inserted

	got := onePerTxRows(t, store,
		func() plans.RecordQueryPlan { return aggPriceCountPlan(false) },
		func(qr QueryResult) (string, error) {
			cnt, ok := qr.Positional.Get(0)
			if !ok {
				return "", fmt.Errorf("scalar aggregate row missing slot 0: %+v", qr.Positional)
			}
			return fmt.Sprintf("%v", cnt), nil
		})

	if fmt.Sprint(got) != "[0]" {
		t.Fatalf("scalar COUNT(*) on empty input paged one row per tx = %v, want [0] (the default row exactly once)", got)
	}
}

// TestIntegration_SortEmitContinuation_OneRowPerTx pins the sort EMIT-PHASE
// per-row continuation end-to-end: every emitted row's continuation restores
// the remaining sorted buffer (inner exhausted, straight to emit) and the
// concatenation across single-row transactions is the full sorted output.
func TestIntegration_SortEmitContinuation_OneRowPerTx(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(500)},
		&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(400)},
	)

	makePlan := func() plans.RecordQueryPlan {
		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		return plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{{
			Field:     "PRICE",
			ValueExpr: values.NewFieldValueWithResolvedOrdinal("PRICE", 2, values.UnknownType),
		}})
	}
	got := onePerTxRows(t, store, makePlan, func(qr QueryResult) (string, error) {
		price, ok := qr.Positional.Get(2)
		if !ok {
			return "", fmt.Errorf("sorted row missing price slot: %+v", qr.Positional)
		}
		return fmt.Sprintf("%v", price), nil
	})

	want := "[100 200 300 400 500]"
	if fmt.Sprint(got) != want {
		t.Fatalf("ORDER BY paged one row per tx = %v, want %v (no duplicate/dropped/misordered rows across mid-emit resumes)", got, want)
	}
}
