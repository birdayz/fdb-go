package executor

import (
	"context"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestIntegrationTextChargesCallerBudgetAndRecordFetch(t *testing.T) {
	t.Parallel()
	index := recordlayer.NewTextIndex("text_idx", recordlayer.Nest("flower", recordlayer.Field("type")))
	store := setupStore(t, index)
	for i, document := range []string{"red blue", "red", "blue", "red other blue"} {
		insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(int64(i + 1)), Flower: &demopb.Flower{Type: proto.String(document)}})
	}
	for _, typ := range []predicates.ComparisonType{predicates.ComparisonTextContainsAll, predicates.ComparisonTextContainsAny, predicates.ComparisonTextContainsAllPrefixes} {
		for _, kind := range []string{"records", "bytes", "time"} {
			_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(store.Subspace()).Open()
				if e != nil {
					return nil, e
				}
				postingState := recordlayer.NewScanLimiterState()
				baseline := recordlayer.DefaultExecuteProperties()
				baseline.ScanState = postingState
				// TEXT_CONTAINS_ALL_PREFIXES reads every prefix's postings into
				// its probable intersection (TextScan.java:205-218).
				for _, term := range []string{"red", "blue"} {
					c := s.ScanIndexByType(index, recordlayer.IndexScanByTextToken, recordlayer.TupleRangeAllOf(tuple.Tuple{term}), nil, recordlayer.NewScanProperties(baseline))
					_, e := recordlayer.AsList(context.Background(), c)
					c.Close()
					if e != nil {
						return nil, e
					}
				}
				postingBytes := postingState.BytesScanned()
				if postingBytes <= 0 {
					t.Fatal("empty posting byte baseline")
				}
				state := recordlayer.NewScanLimiterState()
				for i := 0; i < 10; i++ {
					state.AddRecordScanned()
				}
				state.AddBytesScanned(1_000_000_000)
				state.AnchorAt(time.Now().Add(-24 * time.Hour))
				props := recordlayer.DefaultExecuteProperties()
				props.ScanState = state
				wantReason := recordlayer.ScanLimitReached
				switch kind {
				case "records":
					props.ScannedRecordsLimit = 12
				case "bytes":
					props.ScannedBytesLimit = 1_000_000_000
					wantReason = recordlayer.ByteLimitReached
				case "time":
					props.TimeLimit = time.Hour
					wantReason = recordlayer.TimeLimitReached
				}
				c, e := ExecutePlan(context.Background(), textTestPlan(t, typ, "red blue", false, false), s, EmptyEvaluationContext(), nil, props)
				if e != nil {
					return nil, e
				}
				var ids []int64
				var reason recordlayer.NoNextReason
				for {
					r, e := c.OnNext(context.Background())
					if e != nil {
						c.Close()
						return nil, e
					}
					if !r.HasNext() {
						reason = r.GetNoNextReason()
						break
					}
					ids = append(ids, r.GetValue().Record.Record.(*demopb.Order).GetOrderId())
				}
				c.Close()
				state.WaitForIssuedRecordLoadCharges()
				const expectedReads = 12
				if state.RecordsScanned() != expectedReads {
					t.Fatalf("%v/%s aggregate posting reads=%d want %d", typ, kind, state.RecordsScanned(), expectedReads)
				}
				if reason != wantReason {
					t.Fatalf("%v/%s stop=%v want %v", typ, kind, reason, wantReason)
				}
				if !reflect.DeepEqual(ids, []int64{1}) {
					t.Fatalf("%v/%s page ids=%v want [1]", typ, kind, ids)
				}
				charged := state.BytesScanned() - 1_000_000_000
				if charged <= postingBytes {
					t.Fatalf("%v/%s bytes=%d posting-only baseline=%d: base-record fetch not charged", typ, kind, charged, postingBytes)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
