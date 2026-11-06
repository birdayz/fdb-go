package executor

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestIntegrationTextSuffixBounds(t *testing.T) {
	t.Parallel()
	index := recordlayer.NewTextIndex("text_idx", recordlayer.Concat(recordlayer.Nest("flower", recordlayer.Field("type")), recordlayer.Field("price")))
	store := setupStore(t, index)
	for i := int64(1); i <= 5; i++ {
		insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(6 - i)), Flower: &demopb.Flower{Type: proto.String("red blue")}})
	}
	bounds := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(1))}).Range
	bounds = bounds.Merge(&predicates.Comparison{Type: predicates.ComparisonLessThanOrEq, Operand: values.LiteralValue(int64(4))}).Range
	for _, reverse := range []bool{false, true} {
		for _, pageSize := range []int{0, 1, 2} {
			p, err := plans.NewRecordQueryTextIndexPlan(index.Name, plans.TextScan{Predicate: &predicates.Comparison{Type: predicates.ComparisonTextContainsAll, Operand: values.LiteralValue("red blue")}, Suffix: []*predicates.ComparisonRange{bounds}, KeyTypes: []values.Type{values.NullableString, values.NullableInt}}, integrationOrderType(), reverse)
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			var continuation []byte
			for page := 0; page < 20; page++ {
				result, err := readTextTestPage(context.Background(), continuation, testDB.Run, func(rtx *recordlayer.FDBRecordContext, input []byte) (recordlayer.RecordCursor[QueryResult], error) {
					s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
					if e != nil {
						return nil, e
					}
					props := recordlayer.DefaultExecuteProperties()
					props.ReturnedRowLimit = pageSize
					return ExecutePlan(context.Background(), p, s, EmptyEvaluationContext(), input, props)
				})
				if err != nil {
					t.Fatal(err)
				}
				continuation = result.continuation
				for _, row := range result.rows {
					got = append(got, row.Record.Record.(*demopb.Order).GetOrderId())
				}
				if result.reason.IsSourceExhausted() {
					break
				}
				if page == 19 {
					t.Fatal("suffix scan failed to terminate")
				}
			}
			want := []int64{4, 3, 2}
			if reverse {
				slices.Reverse(want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("reverse=%v page=%d got %v want %v", reverse, pageSize, got, want)
			}
		}
	}
	_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
		if e != nil {
			return nil, e
		}
		p, e := plans.NewRecordQueryTextIndexPlan(index.Name, plans.TextScan{Predicate: &predicates.Comparison{Type: predicates.ComparisonTextContainsPrefix, Operand: values.LiteralValue("re")}, Suffix: []*predicates.ComparisonRange{bounds}}, integrationOrderType(), false)
		if e != nil {
			return nil, e
		}
		_, e = ExecutePlan(context.Background(), p, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		// Java's RecordCoreException (TextScan.java:429-431).
		var invalid *recordlayer.RecordCoreError
		if !errors.As(e, &invalid) || invalid.IndexName != index.Name {
			t.Fatalf("prefix suffix error=%v", e)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationTextContinuationRejectsChangedBindings(t *testing.T) {
	t.Parallel()
	index := recordlayer.NewTextIndex("text_idx", recordlayer.GroupBy(recordlayer.Nest("flower", recordlayer.Field("type")), recordlayer.Field("price")))
	store := setupStore(t, index)
	for i := int64(1); i <= 4; i++ {
		insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(1), Flower: &demopb.Flower{Type: proto.String("prefix prelude red blue")}})
	}
	for _, typ := range []predicates.ComparisonType{predicates.ComparisonTextContainsAll, predicates.ComparisonTextContainsAllPrefixes} {
		makePlan := func(reverse bool) *plans.RecordQueryTextIndexPlan {
			p, e := plans.NewRecordQueryTextIndexPlan(index.Name, plans.TextScan{Predicate: &predicates.Comparison{Type: typ, Operand: values.NewParameterValue(2)}, Grouping: []values.Value{values.NewParameterValue(1)}, KeyTypes: []values.Type{values.NullableInt, values.NullableString}}, integrationOrderType(), reverse)
			if e != nil {
				t.Fatal(e)
			}
			return p
		}
		term := "red blue"
		changed := "red absent"
		if typ == predicates.ComparisonTextContainsAllPrefixes {
			term = "pre prel"
			changed = "pre absent"
		}
		plan := makePlan(false)
		eval := EmptyEvaluationContext().WithParams([]any{int64(1), term})
		var continuation []byte
		_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
			if e != nil {
				return nil, e
			}
			props := recordlayer.DefaultExecuteProperties()
			props.ReturnedRowLimit = 1
			c, e := ExecutePlan(context.Background(), plan, s, eval, nil, props)
			if e != nil {
				return nil, e
			}
			defer c.Close()
			first, e := c.OnNext(context.Background())
			if e != nil {
				return nil, e
			}
			if !first.HasNext() {
				t.Fatal("no first row")
			}
			continuation, e = first.GetContinuation().ToBytes()
			return nil, e
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(continuation) == 0 {
			t.Fatal("missing continuation")
		}
		for _, tc := range []struct {
			name   string
			plan   *plans.RecordQueryTextIndexPlan
			params []any
			valid  bool
		}{{"unchanged", plan, []any{int64(1), term}, true}, {"later term", plan, []any{int64(1), changed}, false}, {"group", plan, []any{int64(2), term}, false}, {"direction", makePlan(true), []any{int64(1), term}, false}} {
			_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
				if e != nil {
					return nil, e
				}
				c, e := ExecutePlan(context.Background(), tc.plan, s, EmptyEvaluationContext().WithParams(tc.params), continuation, recordlayer.DefaultExecuteProperties())
				if e == nil {
					defer c.Close()
					_, e = c.OnNext(context.Background())
				}
				var mismatch *recordlayer.ContinuationParseError
				if tc.valid && e != nil || !tc.valid && (!errors.As(e, &mismatch) || !strings.Contains(e.Error(), "fingerprint does not match")) {
					t.Fatalf("%v %s resume=%v expected valid=%v", typ, tc.name, e, tc.valid)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
