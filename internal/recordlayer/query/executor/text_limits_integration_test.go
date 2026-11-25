package executor

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestIntegrationTextSharedLimitsAndCancellation(t *testing.T) {
	t.Parallel()
	store := setupStore(t, recordlayer.NewTextIndex("text_idx", recordlayer.Nest("flower", recordlayer.Field("type"))))
	for i, document := range []string{"red blue", "red", "blue", "red other blue"} {
		insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(int64(i + 1)), Flower: &demopb.Flower{Type: proto.String(document)}})
	}
	for _, limit := range []string{"records", "bytes", "time"} {
		t.Run(limit, func(t *testing.T) {
			var got []int64
			var continuation []byte
			stops := 0
			for page := 0; page < 100; page++ {
				result, err := readTextTestPage(context.Background(), continuation, testDB.Run, func(rtx *recordlayer.FDBRecordContext, input []byte) (recordlayer.RecordCursor[QueryResult], error) {
					s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
					if e != nil {
						return nil, e
					}
					props := recordlayer.DefaultExecuteProperties()
					switch limit {
					case "records":
						props.ScannedRecordsLimit = 1
					case "bytes":
						props.ScannedBytesLimit = 1
					case "time":
						props.TimeLimit = time.Nanosecond
					}
					return ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAny, "red blue", false, false), s, EmptyEvaluationContext(), input, props)
				})
				if err != nil {
					t.Fatal(err)
				}
				continuation = result.continuation
				for _, row := range result.rows {
					got = append(got, row.Record.Record.(*demopb.Order).GetOrderId())
				}
				if !result.reason.IsSourceExhausted() {
					stops++
				}
				if result.reason.IsSourceExhausted() {
					break
				}
				if page == 99 {
					t.Fatal("limited text scan failed to progress")
				}
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if !reflect.DeepEqual(got, []int64{1, 2, 3, 4}) {
				t.Fatalf("got %v", got)
			}
			if stops == 0 {
				t.Fatal("budget did not stop the text scan")
			}
		})
	}
	_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
		if e != nil {
			return nil, e
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		_, e = ExecutePlan(canceled, textTestPlan(t, predicates.ComparisonTextContainsAll, "red", false, false), s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancellation=%v", e)
		}
		props := recordlayer.DefaultExecuteProperties()
		props.ScannedRecordsLimit = 1
		props.FailOnScanLimitReached = true
		c, e := ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAll, "red", false, false), s, EmptyEvaluationContext(), nil, props)
		if e != nil {
			return nil, e
		}
		defer c.Close()
		_, e = CollectAll(context.Background(), c)
		var limitErr *recordlayer.ScanLimitReachedError
		if !errors.As(e, &limitErr) {
			t.Fatalf("fail-on-limit=%v", e)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationTextRejectsPrimaryKeyTrimmedIntoToken(t *testing.T) {
	t.Parallel()
	expression := recordlayer.Nest("flower", recordlayer.Field("type"))
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(expression)
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewTextIndex("text_idx", expression))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if positions := metadata.GetIndex("text_idx").PrimaryKeyComponentPositions(); !reflect.DeepEqual(positions, []int{0}) {
		t.Fatalf("overlap precondition=%v", positions)
	}
	_, err = testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(testSubspace(t)).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAll, "red", false, false), store, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		var invalid *predicates.TextComparisonError
		if !errors.As(err, &invalid) {
			t.Fatalf("trimmed primary key error=%v", err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationTextOmittedPositionsAndRebuild(t *testing.T) {
	t.Parallel()
	index := recordlayer.NewTextIndex("text_idx", recordlayer.Nest("flower", recordlayer.Field("type"))).SetOption(recordlayer.IndexOptionTextOmitPositions, "true")
	store := setupStore(t, index)
	insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(1), Flower: &demopb.Flower{Type: proto.String("red other blue")}}, &demopb.Order{OrderId: proto.Int64(2), Flower: &demopb.Flower{Type: proto.String("red")}})
	_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
		if e != nil {
			return nil, e
		}
		if e = s.RebuildIndex(index); e != nil {
			return nil, e
		}
		// Java's raw positional scan treats omitted lists as wildcard positions;
		// planner eligibility separately declines this access path.
		for _, typ := range []predicates.ComparisonType{predicates.ComparisonTextContainsPhrase, predicates.ComparisonTextContainsAllWithin} {
			c, e := ExecutePlan(context.Background(), textTestPlan(t, typ, "red blue", false, false), s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if e != nil {
				return nil, e
			}
			rows, e := CollectAll(context.Background(), c)
			c.Close()
			if e != nil {
				return nil, e
			}
			if len(rows) != 1 || rows[0].Record.Record.(*demopb.Order).GetOrderId() != 1 {
				t.Fatalf("omitted-position %v rows=%v", typ, rows)
			}
		}
		if _, e = s.MarkIndexDisabled(index.Name); e != nil {
			return nil, e
		}
		_, e = ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAll, "red", false, false), s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		var unreadable *recordlayer.IndexNotReadableError
		if !errors.As(e, &unreadable) {
			t.Fatalf("unreadable=%v", e)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTextScanGroupBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		low, high tuple.Tuple
		grouping  int
		valid     bool
	}{
		{"short low", nil, tuple.Tuple{int64(1)}, 1, false},
		{"short high", tuple.Tuple{int64(1)}, nil, 1, false},
		{"different groups", tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)}, 1, false},
		{"different zero signs", tuple.Tuple{math.Copysign(0, -1)}, tuple.Tuple{float64(0)}, 1, false},
		{"same negative zero", tuple.Tuple{math.Copysign(0, -1)}, tuple.Tuple{math.Copysign(0, -1)}, 1, true},
		{"same group different suffix", tuple.Tuple{int64(1), int64(10)}, tuple.Tuple{int64(1), int64(20)}, 1, true},
		{"ungrouped", nil, nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rng := recordlayer.TupleRangeBetween(tc.low, tc.high)
			group, err := textScanGroup("text_idx", rng, tc.grouping)
			if !tc.valid {
				var invalid *TextScanGroupBindingError
				if !errors.As(err, &invalid) || invalid.IndexName != "text_idx" || invalid.GroupingColumns != tc.grouping || !reflect.DeepEqual(invalid.Range, rng) || group != nil {
					t.Fatalf("group=%v error=%#v", group, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(group.Pack(), tc.low[:tc.grouping].Pack()) {
				t.Fatalf("group=%v error=%v", group, err)
			}
			if tc.grouping > 0 {
				group[0] = "changed"
				if reflect.DeepEqual(group, rng.Low[:tc.grouping]) {
					t.Fatal("group aliases the bound range")
				}
			}
		})
	}
}

func TestIntegrationTextPlanIndexAdmission(t *testing.T) {
	t.Parallel()
	expression := recordlayer.Nest("flower", recordlayer.Field("type"))
	store := setupStore(t, recordlayer.NewTextIndex("text_idx", expression), recordlayer.NewIndex("value_idx", expression))
	insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(1), Flower: &demopb.Flower{Type: proto.String("red")}})
	for _, name := range []string{"missing_idx", "value_idx", "text_idx"} {
		t.Run(name, func(t *testing.T) {
			_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
				if err != nil {
					return nil, err
				}
				plan, err := plans.NewRecordQueryTextIndexPlan(name, plans.TextScan{
					Predicate: &predicates.Comparison{Type: predicates.ComparisonTextContainsAll, Operand: values.LiteralValue("red")},
					KeyTypes:  []values.Type{values.NullableString},
				}, integrationOrderType(), false)
				if err != nil {
					return nil, err
				}
				cursor, err := ExecutePlan(context.Background(), plan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if name != "text_idx" {
					if cursor != nil {
						cursor.Close()
						t.Fatal("invalid TEXT index returned a cursor")
					}
					var invalid *predicates.TextComparisonError
					if !errors.As(err, &invalid) || invalid.Message != "text index plan requires a TEXT index" {
						t.Fatalf("index=%s error=%T(%v)", name, err, err)
					}
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				rows, err := CollectAll(context.Background(), cursor)
				if err != nil {
					return nil, err
				}
				if len(rows) != 1 || rows[0].Record.Record.(*demopb.Order).GetOrderId() != 1 {
					t.Fatalf("valid TEXT control rows=%v", rows)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIntegrationTextPlanGuards: the executor refuses a text plan whose
// bindings or comparison do not fit the index.
func TestIntegrationTextPlanGuards(t *testing.T) {
	t.Parallel()
	index := recordlayer.NewTextIndex("text_idx", recordlayer.GroupBy(recordlayer.Nest("flower", recordlayer.Field("type")), recordlayer.Field("price")))
	store := setupStore(t, index)
	all := predicates.Comparison{Type: predicates.ComparisonTextContainsAll, Operand: values.LiteralValue("red")}
	grouping := []values.Value{values.LiteralValue(int64(1))}
	keyTypes := []values.Type{values.NullableInt, values.NullableString}
	other := all
	other.TextTokenizerName = "other"
	null := all
	null.Operand = values.NewNullValue(values.NullableString)
	for _, tc := range []struct {
		name         string
		scan         plans.TextScan
		continuation []byte
		message      string
	}{
		{"missing grouping", plans.TextScan{Predicate: &all, KeyTypes: keyTypes}, nil, "equality bindings for every grouping column"},
		{"short key types", plans.TextScan{Predicate: &all, Grouping: grouping}, nil, "key types do not cover"},
		{"tokenizer", plans.TextScan{Predicate: &other, Grouping: grouping, KeyTypes: keyTypes}, nil, "tokenizer does not match"},
		{"null resume", plans.TextScan{Predicate: &null, Grouping: grouping, KeyTypes: keyTypes}, []byte{1}, "cannot resume an empty text query"},
		{"no comparison", plans.TextScan{Grouping: grouping, KeyTypes: keyTypes}, nil, "requires a typed text comparison"},
	} {
		_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
			if e != nil {
				return nil, e
			}
			p, e := plans.NewRecordQueryTextIndexPlan(index.Name, tc.scan, integrationOrderType(), false)
			if e != nil {
				return nil, e
			}
			_, e = ExecutePlan(context.Background(), p, s, EmptyEvaluationContext(), tc.continuation, recordlayer.DefaultExecuteProperties())
			var invalid *predicates.TextComparisonError
			if !errors.As(e, &invalid) || !strings.Contains(e.Error(), tc.message) {
				t.Fatalf("%s: error=%v", tc.name, e)
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
