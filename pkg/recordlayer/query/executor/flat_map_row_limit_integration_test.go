package executor

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestIntegration_FlatMapInnerRowLimitResume supplies an explicit child request
// cap to the real FlatMap cursor over FDB scans. SQL's executeFlatMap normally
// clears that cap: this is an internal cursor-contract regression, not a claim
// that a SQL LIMIT currently takes this branch. Each resume decodes the real
// FlatMap continuation, rebinds the outer row, and reopens the inner FDB scan.
func TestIntegration_FlatMapInnerRowLimitResume(t *testing.T) {
	t.Parallel()
	for _, budget := range []int{1, 2} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			store := setupStore(t)
			insertOrders(t, store,
				&gen.Order{OrderId: proto.Int64(1)},
				&gen.Order{OrderId: proto.Int64(2)},
				&gen.Order{OrderId: proto.Int64(3)},
			)
			scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
			outerAlias := values.NamedCorrelationIdentifier("outer")
			innerAlias := values.NamedCorrelationIdentifier("inner")
			plan := mustExecutorConstruct(plans.NewRecordQueryFlatMapPlan(
				scan, scan, outerAlias, innerAlias,
				mustTestQOV(t, innerAlias, integrationOrderType()), false,
			))
			type pageResult struct {
				ids    []int64
				resume []byte
				reason recordlayer.NoNextReason
			}
			var continuation []byte
			var got []int64
			stops := 0
			for page := 0; ; page++ {
				if page >= 16 {
					t.Fatal("FlatMap did not exhaust within the page bound")
				}
				result, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					cursor, err := executeFlatMap(ctx, plan, s, EmptyEvaluationContext(), continuation,
						recordlayer.DefaultExecuteProperties())
					if err != nil {
						return nil, err
					}
					defer cursor.Close()
					fm, ok := cursor.(*flatMapCursor)
					if !ok {
						return nil, fmt.Errorf("expected FlatMap cursor, got %T", cursor)
					}
					// Override only child request propagation to reach the otherwise
					// cleared row-cap arm. Both legs remain real production scans.
					fm.props = fm.props.WithReturnedRowLimit(budget)
					var out pageResult
					for {
						r, err := fm.OnNext(ctx)
						if err != nil {
							return nil, err
						}
						if !r.HasNext() {
							out.reason = r.GetNoNextReason()
							out.resume, err = r.GetContinuation().ToBytes()
							return out, err
						}
						out.ids = append(out.ids, r.GetValue().PrimaryKey[0].(int64))
						if len(out.ids) > 9 {
							return nil, fmt.Errorf("FlatMap repeated rows within one page: %v", out.ids)
						}
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				out := result.(pageResult)
				got = append(got, out.ids...)
				if out.reason == recordlayer.SourceExhausted {
					if out.resume != nil {
						t.Fatalf("exhausted FlatMap carries a live continuation: %x", out.resume)
					}
					break
				}
				if out.reason != recordlayer.ReturnLimitReached || len(out.resume) == 0 {
					t.Fatalf("page %d: reason=%v continuation=%x, want resumable returned-row limit", page, out.reason, out.resume)
				}
				stops++
				continuation = out.resume
			}
			want := []int64{1, 2, 3, 1, 2, 3, 1, 2, 3}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("limited inner lost rows across FlatMap resumes: got %v, want %v", got, want)
			}
			if stops == 0 {
				t.Fatal("no returned-row-limit stop observed; the faulty inner arm was not exercised")
			}
			t.Logf("FLATMAP-ROW-LIMIT budget=%d stops=%d rows=%v", budget, stops, got)
		})
	}
}
