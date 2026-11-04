package executor

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func textTestPlan(t *testing.T, typ predicates.ComparisonType, query any, grouped bool, reverse bool) plans.RecordQueryPlan {
	t.Helper()
	var groups []values.Value
	keyTypes := []values.Type{values.NullableString}
	if grouped {
		groups = []values.Value{values.LiteralValue(int64(1))}
		keyTypes = []values.Type{values.NullableInt, values.NullableString}
	}
	p, err := plans.NewRecordQueryTextIndexPlan("text_idx", plans.TextScan{IndexName: "text_idx", Predicate: &predicates.Comparison{Type: typ, Operand: values.LiteralValue(query), TextMaxDistance: 2}, Grouping: groups, KeyTypes: keyTypes}, integrationOrderType(), reverse)
	if err != nil {
		t.Fatal(err)
	}
	if typ == predicates.ComparisonTextContainsPrefix || typ == predicates.ComparisonTextContainsAnyPrefix || typ == predicates.ComparisonTextContainsAllPrefixes {
		d, e := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(p)
		if e != nil {
			t.Fatal(e)
		}
		return d
	}
	return p
}

func TestIntegrationTextQueryPredicatesAndPagination(t *testing.T) {
	t.Parallel()
	for _, grouped := range []bool{false, true} {
		t.Run(map[bool]string{false: "ungrouped", true: "grouped"}[grouped], func(t *testing.T) {
			expression := recordlayer.Nest("flower", recordlayer.Field("type"))
			if grouped {
				expression = recordlayer.GroupBy(expression, recordlayer.Field("price"))
			}
			store := setupStore(t, recordlayer.NewTextIndex("text_idx", expression))
			documents := []string{"red blue red", "blue green", "red other blue", "prefix prelude", "CAFÉ, red!", "", "red red", "prefix alone"}
			for i, document := range documents {
				insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(1), Flower: &demopb.Flower{Type: proto.String(document)}})
			}
			insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(20), Price: proto.Int32(2), Flower: &demopb.Flower{Type: proto.String("red blue")}})
			cases := []struct {
				typ   predicates.ComparisonType
				query any
				want  []int64
			}{
				{predicates.ComparisonTextContainsAll, "red blue", []int64{1, 3}},
				{predicates.ComparisonTextContainsAny, "red green", []int64{1, 2, 3, 5, 7}},
				{predicates.ComparisonTextContainsPhrase, "red blue", []int64{1}},
				{predicates.ComparisonTextContainsPhrase, "red red", []int64{7}},
				{predicates.ComparisonTextContainsAllWithin, "blue red", []int64{1, 3}},
				{predicates.ComparisonTextContainsPrefix, []string{"pre"}, []int64{4, 8}},
				{predicates.ComparisonTextContainsAnyPrefix, "pre re", []int64{1, 3, 4, 5, 7, 8}},
				{predicates.ComparisonTextContainsAllPrefixes, "pr prel", []int64{4}},
				{predicates.ComparisonTextContainsAllPrefixes, "pr missing", nil},
				{predicates.ComparisonTextContainsAll, "cafe", []int64{5}},
				{predicates.ComparisonTextContainsAll, "absent", nil},
				{predicates.ComparisonTextContainsAll, "", nil},
			}
			for _, tc := range cases {
				for _, reverse := range []bool{false, true} {
					for _, pageSize := range []int{0, 1, 2} {
						want := append([]int64(nil), tc.want...)
						if !grouped {
							switch tc.typ {
							case predicates.ComparisonTextContainsAll, predicates.ComparisonTextContainsAny, predicates.ComparisonTextContainsPhrase, predicates.ComparisonTextContainsAllWithin:
								if tc.query != "cafe" && tc.query != "absent" && tc.query != "" && tc.query != "red red" {
									want = append(want, 20)
								}
							case predicates.ComparisonTextContainsAnyPrefix:
								want = append(want, 20)
							}
						}
						plan := textTestPlan(t, tc.typ, tc.query, grouped, reverse)
						var got []int64
						var continuation []byte
						for page := 0; page < 100; page++ {
							result, err := readTextTestPage(context.Background(), continuation, testDB.Run, func(rtx *recordlayer.FDBRecordContext, input []byte) (recordlayer.RecordCursor[QueryResult], error) {
								s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
								if e != nil {
									return nil, e
								}
								props := recordlayer.DefaultExecuteProperties()
								props.ReturnedRowLimit = pageSize
								return ExecutePlan(context.Background(), plan, s, EmptyEvaluationContext(), input, props)
							})
							if err != nil {
								t.Fatalf("%v query=%v reverse=%v page=%d: %v", tc.typ, tc.query, reverse, pageSize, err)
							}
							continuation = result.continuation
							for _, row := range result.rows {
								got = append(got, row.Record.Record.(*demopb.Order).GetOrderId())
							}
							if result.reason.IsSourceExhausted() {
								break
							}
							if page == 99 {
								t.Fatal("pagination did not terminate")
							}
						}
						sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
						prefix := tc.typ == predicates.ComparisonTextContainsPrefix || tc.typ == predicates.ComparisonTextContainsAllPrefixes || tc.typ == predicates.ComparisonTextContainsAnyPrefix
						if prefix {
							sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
						} else if reverse {
							slices.Reverse(want)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("%v query=%v reverse=%v page=%d got %v want %v", tc.typ, tc.query, reverse, pageSize, got, want)
						}
					}
				}
			}
		})
	}
}

func TestIntegrationTextQueryUpdateDelete(t *testing.T) {
	t.Parallel()
	store := setupStore(t, recordlayer.NewTextIndex("text_idx", recordlayer.Nest("flower", recordlayer.Field("type"))))
	insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(1), Flower: &demopb.Flower{Type: proto.String("old word")}})
	insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(1), Flower: &demopb.Flower{Type: proto.String("new word")}})
	_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
		if e != nil {
			return nil, e
		}
		for _, tc := range []struct {
			term  string
			count int
		}{{"old", 0}, {"new", 1}} {
			c, e := ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAll, tc.term, false, false), s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if e != nil {
				return nil, e
			}
			rows, e := CollectAll(context.Background(), c)
			c.Close()
			if e != nil {
				return nil, e
			}
			if len(rows) != tc.count {
				t.Fatalf("%s rows=%d want %d", tc.term, len(rows), tc.count)
			}
		}
		_, e = s.DeleteRecord(tuple.Tuple{int64(1)})
		if e != nil {
			return nil, e
		}
		c, e := ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAll, "new", false, false), s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if e != nil {
			return nil, e
		}
		defer c.Close()
		rows, e := CollectAll(context.Background(), c)
		if len(rows) != 0 {
			t.Fatalf("deleted record returned %v", rows)
		}
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
}
