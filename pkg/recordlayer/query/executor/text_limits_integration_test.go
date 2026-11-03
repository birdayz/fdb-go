package executor

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
				exhausted := false
				_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
					c, e := ExecutePlan(context.Background(), textTestPlan(t, predicates.ComparisonTextContainsAny, "red blue", false, false), s, EmptyEvaluationContext(), continuation, props)
					if e != nil {
						return nil, e
					}
					defer c.Close()
					for {
						r, e := c.OnNext(context.Background())
						if e != nil {
							return nil, e
						}
						if !r.HasNext() {
							exhausted = r.GetNoNextReason().IsSourceExhausted()
							if !exhausted {
								stops++
							}
							continuation, e = r.GetContinuation().ToBytes()
							return nil, e
						}
						got = append(got, r.GetValue().Record.Record.(*demopb.Order).GetOrderId())
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				if exhausted {
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
