package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func budgetUnionPlan(t *testing.T, kind string) plans.RecordQueryPlan {
	t.Helper()
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
	legs := []plans.RecordQueryPlan{scan, scan}
	keys := []values.Value{integrationField(t, scan, 0)}
	switch kind {
	case "record":
		return scan
	case "projection":
		return mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(keys, scan))
	case "map":
		return mustExecutorConstruct(plans.NewRecordQueryMapPlan(scan,
			values.NewRecordConstructorValue(values.RecordConstructorField{Name: "order_id", Value: keys[0]})))
	case "index":
		return priceProbeIndexPlan(t, values.NamedCorrelationIdentifier("budget_binding"))
	case "aggregate":
		index := mustExecutorConstruct(plans.NewRecordQueryIndexPlan("budget_count", nil, []string{"Order"}, integrationOrderType(), false))
		resultType := exactTestRowType(
			values.Field{Name: "order_id", FieldType: values.NullableLong},
			values.Field{Name: "COUNT(*)", FieldType: values.NotNullLong},
		)
		return mustExecutorConstruct(plans.NewRecordQueryAggregateIndexPlan(index, "Order", resultType, "COUNT")).
			WithGroupColumns([]string{"order_id"}, "")
	case "concat":
		return mustExecutorConstruct(plans.NewRecordQueryUnionPlan(legs))
	case "unordered":
		return mustExecutorConstruct(plans.NewRecordQueryUnorderedUnionPlan(legs))
	case "unordered_single":
		return mustExecutorConstruct(plans.NewRecordQueryUnorderedUnionPlan(legs[:1]))
	case "merge":
		return mustExecutorConstruct(plans.NewRecordQueryMergeSortUnionPlan(legs, keys, false, true))
	case "merge_all":
		return mustExecutorConstruct(plans.NewRecordQueryMergeSortUnionPlan(legs, keys, false, false))
	case "merge_single":
		return mustExecutorConstruct(plans.NewRecordQueryMergeSortUnionPlan(legs[:1], keys, false, true))
	case "in", "in_concat", "in_single":
		if kind == "in_concat" {
			keys = nil
		}
		p := mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(scan, []string{"budget_binding"}, keys, false))
		source := []any{int64(1), int64(2)}
		if kind == "in_single" {
			source = source[:1]
		}
		return p.WithInSources([][]any{source})
	default:
		t.Fatalf("unknown union kind %q", kind)
		return nil
	}
}

// TestIntegration_UnionReadBudget measures actual FDB read-conflict ranges, not
// rows pulled by the streaming union. Unlimited WANT_ALL and SERIAL controls
// must conflict at distant ID 20; SMALL and finite budgets must not. The reader
// commits once: retrying would hide the defect.
func TestIntegration_UnionReadBudget(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"record", "projection", "map", "index", "aggregate", "concat", "unordered", "unordered_single", "merge", "merge_all", "merge_single", "in", "in_concat", "in_single"} {
		for _, tc := range []struct {
			cap          int
			mode         recordlayer.CursorStreamingMode
			wantConflict bool
		}{
			{1, recordlayer.StreamingModeWantAll, false},
			{0, recordlayer.StreamingModeWantAll, true},
			{0, recordlayer.StreamingModeSerial, true},
			{0, recordlayer.StreamingModeSmall, false},
		} {
			t.Run(fmt.Sprintf("%s/cap=%d/mode=%d", kind, tc.cap, tc.mode), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				var extraIndexes []*recordlayer.Index
				if kind == "aggregate" {
					extraIndexes = append(extraIndexes, recordlayer.NewCountIndex("budget_count", recordlayer.GroupAll(recordlayer.Field("order_id"))))
				}
				store := setupStore(t, extraIndexes...)
				orders := make([]*gen.Order, 20)
				for i := range orders {
					orders[i] = &gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(50)}
				}
				insertOrders(t, store, orders...)
				if store.GetMetaData().IsSplitLongRecords() {
					t.Fatal("read-budget probe requires unsplit records so the leaf can bound its FDB range")
				}
				tx, err := testDB.CreateWritableTransaction()
				if err != nil {
					t.Fatal(err)
				}
				rctx := recordlayer.NewFDBRecordContext(tx, testDB.Env())
				defer rctx.Cancel()
				s, err := recordlayer.NewStoreBuilder().SetContext(rctx).
					SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
				if err != nil {
					t.Fatal(err)
				}
				props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(tc.cap).WithSkip(2)
				props.DefaultCursorStreamingMode = tc.mode
				if props.IsolationLevel != recordlayer.SerializableIsolation {
					t.Fatal("conflict-range probe requires serializable reads")
				}
				plan := budgetUnionPlan(t, kind)
				eval := EmptyEvaluationContext().WithBinding(values.NamedCorrelationIdentifier("budget_binding"), int64(50))
				cursor, err := ExecutePlan(ctx, plan, s, eval, nil, props)
				if err != nil {
					t.Fatal(err)
				}
				defer cursor.Close()
				row, err := cursor.OnNext(ctx)
				wantID := int64(3)
				if kind == "merge_all" {
					wantID = 2 // merged duplicates: skip the two copies of ID 1
				}
				if err != nil || !row.HasNext() {
					t.Fatalf("first row after skip: %v, %v; want ID %d", row, err, wantID)
				}
				if id, _ := row.GetValue().Positional.Get(0); id != wantID {
					t.Fatalf("first ID after skip = %v, want %d", id, wantID)
				}
				if tc.cap > 0 {
					end, err := cursor.OnNext(ctx)
					if err != nil || end.HasNext() || end.GetNoNextReason() != recordlayer.ReturnLimitReached || end.GetContinuation().IsEnd() || !bytes.Equal(continuationBytesForTest(t, end.GetContinuation()), continuationBytesForTest(t, row.GetContinuation())) {
						t.Fatalf("finite request must stop at its last row token: %v, %v", end, err)
					}
				}
				// A write outside the record/index subspaces forces a real commit
				// without introducing another overlap with the concurrent writer.
				rctx.Transaction().Set(testSubspace(t).Sub("reader_marker").FDBKey(), []byte{1})
				_, err = testDB.Run(ctx, func(writer *recordlayer.FDBRecordContext) (any, error) {
					ws, err := recordlayer.NewStoreBuilder().SetContext(writer).
						SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					deleted, err := ws.DeleteRecord(tuple.Tuple{int64(20)})
					if err == nil && !deleted {
						t.Fatal("distant-row writer deleted nothing")
					}
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
				commitErr := rctx.Commit()
				var conflict fdb.Error
				if tc.wantConflict {
					if !errors.As(commitErr, &conflict) || conflict.Code != 1020 {
						t.Fatalf("unlimited control committed with %v, want not_committed (1020)", commitErr)
					}
				} else if commitErr != nil {
					t.Fatalf("bounded prefetch read distant ID 20: %v; plan %s", commitErr, plan.Explain())
				}
			})
		}
	}
}

// A record of another type consumes the raw scan's child budget before the
// type filter can emit anything. This forces a genuine child limit stop, not
// merely the parent wrapper stopping after its first output row.
func TestIntegration_UnionChildBudgetResume(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"concat", "unordered", "unordered_single", "merge", "merge_all", "merge_single", "in", "in_concat", "in_single"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := setupStore(t)
			insertOrders(t, store,
				&gen.Order{OrderId: proto.Int64(2)},
				&gen.Order{OrderId: proto.Int64(3)},
				&gen.Order{OrderId: proto.Int64(4)},
			)
			_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
				if err != nil {
					return nil, err
				}
				_, err = s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1)})
				return nil, err
			})
			if err != nil {
				t.Fatal(err)
			}
			plan := budgetUnionPlan(t, kind)
			var continuation []byte
			var ids []int64
			ended := false
			for page := 0; page < 12 && !ended; page++ {
				var next []byte
				var emitted []int64
				var sourceExhausted bool
				_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), continuation,
						recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1))
					if err != nil {
						return nil, err
					}
					defer cursor.Close()
					var pageIDs []int64
					for {
						row, err := cursor.OnNext(ctx)
						if err != nil {
							return nil, err
						}
						if row.HasNext() {
							pageIDs = append(pageIDs, row.GetValue().PrimaryKey[0].(int64))
							continue
						}
						if len(pageIDs) > 1 || (page == 0 && len(pageIDs) != 0) {
							t.Fatalf("page %d returned %v; first page must stop inside the type-filtered child", page, pageIDs)
						}
						sourceExhausted = row.GetNoNextReason().IsSourceExhausted()
						if !sourceExhausted && row.GetNoNextReason() != recordlayer.ReturnLimitReached {
							t.Fatalf("page %d stopped with %v, want child returned-row limit", page, row.GetNoNextReason())
						}
						next, err = row.GetContinuation().ToBytes()
						emitted = pageIDs
						return nil, err
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				continuation, ended = next, sourceExhausted
				ids = append(ids, emitted...)
			}
			want := "[2 3 4]"
			if kind == "concat" || kind == "unordered" || kind == "in_concat" {
				want = "[2 3 4 2 3 4]"
			} else if kind == "merge_all" {
				want = "[2 2 3 3 4 4]"
			}
			if !ended || fmt.Sprint(ids) != want {
				t.Fatalf("paged %s: ended=%v rows=%v, want %s", kind, ended, ids, want)
			}
		})
	}
}

type budgetModeTransaction struct {
	fdb.WritableTransaction
	observe func(fdb.Range, fdb.RangeOptions)
}

func (tx *budgetModeTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.observe(r, options)
	return tx.WritableTransaction.GetRange(r, options)
}

func (tx *budgetModeTransaction) Snapshot() fdb.ReadTransaction {
	return &budgetModeReadTransaction{ReadTransaction: tx.WritableTransaction.Snapshot(), observe: tx.observe}
}

type budgetModeReadTransaction struct {
	fdb.ReadTransaction
	observe func(fdb.Range, fdb.RangeOptions)
}

func (tx *budgetModeReadTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.observe(r, options)
	return tx.ReadTransaction.GetRange(r, options)
}

func (tx *budgetModeReadTransaction) Snapshot() fdb.ReadTransaction {
	return &budgetModeReadTransaction{ReadTransaction: tx.ReadTransaction.Snapshot(), observe: tx.observe}
}

func TestIntegration_PermutedAggregateRepairStreamingMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		inherited recordlayer.CursorStreamingMode
		effective recordlayer.CursorStreamingMode
	}{
		{"inherited", recordlayer.StreamingModeWantAll, recordlayer.StreamingModeWantAll},
		{"override_small", recordlayer.StreamingModeWantAll, recordlayer.StreamingModeSmall},
		{"override_want_all", recordlayer.StreamingModeIterator, recordlayer.StreamingModeWantAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			idx := recordlayer.NewPermutedMinIndex("repair_mode",
				recordlayer.GroupBy(recordlayer.Field("quantity"), recordlayer.Field("price")), 1)
			store := setupStore(t, idx)
			insertOrders(t, store,
				&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)},
				&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50), Quantity: proto.Int32(7)},
			)
			_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				ordinaryPrefix := testSubspace(t).Sub(recordlayer.IndexKey).Bytes()
				permutedPrefix := testSubspace(t).Sub(recordlayer.IndexSecondarySpaceKey).Bytes()
				var ordinaryModes, permutedModes []fdb.StreamingMode
				observe := func(r fdb.Range, options fdb.RangeOptions) {
					begin, _ := r.FDBRangeKeySelectors()
					key := begin.FDBKeySelector().Key.FDBKey()
					if bytes.HasPrefix(key, ordinaryPrefix) {
						ordinaryModes = append(ordinaryModes, options.Mode)
					}
					if bytes.HasPrefix(key, permutedPrefix) {
						permutedModes = append(permutedModes, options.Mode)
					}
				}
				observedCtx := testDB.NewRecordContext(&budgetModeTransaction{WritableTransaction: rtx.Transaction(), observe: observe})
				s, err := recordlayer.NewStoreBuilder().SetContext(observedCtx).
					SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
				if err != nil {
					return nil, err
				}
				props := recordlayer.DefaultExecuteProperties()
				props.DefaultCursorStreamingMode = tc.inherited
				scanProps := recordlayer.NewScanProperties(props).WithStreamingMode(tc.effective)
				cursor, err := newPermutedAggregateIndexCursor(s, idx, recordlayer.TupleRangeAll, nil, scanProps, 1, 0,
					exactTestRowType(values.Field{Name: "price", FieldType: values.NullableLong}, values.Field{Name: "minimum", FieldType: values.NullableLong}))
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				rows, err := CollectAll(ctx, cursor)
				if err != nil {
					return nil, err
				}
				if len(rows) != 1 || fmt.Sprint(rows[0].Positional.Slots) != "[50 7]" {
					t.Fatalf("permuted aggregate did not repair its NULL extremum: %v", rows)
				}
				for name, modes := range map[string][]fdb.StreamingMode{"aggregate": permutedModes, "repair": ordinaryModes} {
					if len(modes) == 0 {
						t.Fatalf("no real %s range reached the observer", name)
					}
					for _, mode := range modes {
						if mode != tc.effective.ToFDB() {
							t.Fatalf("%s used streaming mode %v, want effective override %v", name, mode, tc.effective.ToFDB())
						}
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
