package executor

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// Count issuance, not materialization: a pipelined fetch may issue a record
// read that is never consumed before the returned-row limit closes the cursor.
type indexSkipLoadProbe struct {
	fdb.WritableTransaction
	recordPrefix []byte
	loads        int
}

func (p *indexSkipLoadProbe) GetRange(r fdb.Range, opts fdb.RangeOptions) fdb.RangeResult {
	begin, _ := r.FDBRangeKeySelectors()
	if bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), p.recordPrefix) {
		p.loads++
	}
	return p.WritableTransaction.GetRange(r, opts)
}

type indexSkipPage struct {
	keys         []int64
	continuation []byte
	reason       recordlayer.NoNextReason
	records      int
	bytes        int64
	loads        int
}

func readIndexSkipPage(t *testing.T, base *recordlayer.FDBRecordStore, plan plans.RecordQueryPlan, continuation []byte, props recordlayer.ExecuteProperties) indexSkipPage {
	t.Helper()
	ctx := context.Background()
	var page indexSkipPage
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		page = indexSkipPage{}
		probe := &indexSkipLoadProbe{
			WritableTransaction: rtx.Transaction(),
			recordPrefix:        testSubspace(t).Sub(recordlayer.RecordKey).Bytes(),
		}
		store, err := recordlayer.NewStoreBuilder().SetContext(testDB.NewRecordContext(probe)).
			SetMetaDataProvider(base.GetMetaData()).SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		props.ScanState = recordlayer.NewScanLimiterState()
		cursor, err := ExecutePlan(ctx, plan, store, EmptyEvaluationContext(), continuation, props)
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !result.HasNext() {
				page.reason = result.GetNoNextReason()
				page.continuation, err = result.GetContinuation().ToBytes()
				if err != nil {
					return nil, err
				}
				break
			}
			page.keys = append(page.keys, result.GetValue().PrimaryKey[0].(int64))
		}
		if err := cursor.Close(); err != nil {
			return nil, err
		}
		props.ScanState.WaitForIssuedRecordLoadCharges()
		page.records = props.ScanState.RecordsScanned()
		page.bytes = props.ScanState.BytesScanned()
		page.loads = probe.loads
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func indexSkipPlan(t *testing.T, index *plans.RecordQueryIndexPlan, kind string) plans.RecordQueryPlan {
	t.Helper()
	if kind == "ordinary" {
		return index
	}
	if kind == "covering_value" {
		return mustExecutorConstruct(plans.NewRecordQueryCoveringIndexValuePlan(index, "Order", orderPriceReader(t)))
	}
	covering := mustExecutorConstruct(plans.NewRecordQueryCoveringIndexPlan(index.WithEntryReader(orderPriceReader(t))))
	if kind == "covering" {
		return covering
	}
	return mustExecutorConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		covering, nil, integrationOrderType(), plans.FetchIndexRecordsPrimaryKey,
	))
}

func indexSkipPricePlan(t *testing.T, reverse bool, bound *int64) *plans.RecordQueryIndexPlan {
	t.Helper()
	var comparisons []*predicates.ComparisonRange
	if bound != nil {
		comparison := predicates.ComparisonGreaterThanEq
		if reverse {
			comparison = predicates.ComparisonLessThanOrEq
		}
		comparisons = []*predicates.ComparisonRange{scanRangeTestComparison(t, comparison, values.LiteralValue(*bound))}
	}
	return mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
		"order_price_idx", comparisons, []string{"Order"}, integrationOrderType(), reverse,
	)).WithKeyComponentTypes([]values.Type{values.NullableInt}).WithIndexMetadata([]string{"price"}, []string{"order_id"}, false)
}

// Java's IndexPlan.executeEntries passes executeProperties.asScanProperties
// through to the KV cursor; CoveringIndexPlan and FetchFromPartialRecordPlan
// preserve those properties. Selector offsets skip entries before byte/record
// charging and, crucially, before ordinary or partial-record fetches.
func TestIntegrationIndexSkipSingleRangeAccounting(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"ordinary", "covering", "covering_value", "fetch"} {
		for _, reverse := range []bool{false, true} {
			for _, budget := range []string{"returned", "records", "bytes"} {
				t.Run(fmt.Sprintf("%s/reverse=%t/%s", kind, reverse, budget), func(t *testing.T) {
					t.Parallel()
					store := setupStore(t)
					for id := int64(1); id <= 8; id++ {
						insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))})
					}
					props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(2)
					want := []int64{3, 4}
					bound := int64(3)
					if reverse {
						want, bound = []int64{6, 5}, 6
					}
					wantReason := recordlayer.ReturnLimitReached
					minRecords, maxRecords := 2, 2
					if budget == "returned" && kind == "ordinary" {
						// The fetch pipeline may probe the terminal row-limit result.
						maxRecords = 3
					}
					switch budget {
					case "records":
						props = props.WithReturnedRowLimit(3).WithScannedRecordsLimit(2)
						wantReason = recordlayer.ScanLimitReached
						minRecords, maxRecords = 3, 3 // Includes the denied CursorLimitManager attempt.
					case "bytes":
						props = props.WithScannedBytesLimit(1)
						want = want[:1]
						wantReason = recordlayer.ByteLimitReached
					}
					// An independently bounded scan begins at the exact same first
					// unskipped KV, so even full physical key/record byte charges agree.
					control := readIndexSkipPage(t, store, indexSkipPlan(t, indexSkipPricePlan(t, reverse, &bound), kind), nil, props)
					if !slices.Equal(control.keys, want) || control.reason != wantReason || control.records < minRecords || control.records > maxRecords || control.bytes == 0 {
						t.Fatalf("no-skip control = %+v, want keys %v, reason %v, %d..%d scan attempts and nonzero bytes", control, want, wantReason, minRecords, maxRecords)
					}
					wantLoads := len(want)
					if kind == "covering" || kind == "covering_value" {
						wantLoads = 0
					}
					if control.loads != wantLoads {
						t.Fatalf("no-skip control issued %d record loads, want %d", control.loads, wantLoads)
					}
					props.Skip = 2
					got := readIndexSkipPage(t, store, indexSkipPlan(t, indexSkipPricePlan(t, reverse, nil), kind), nil, props)
					if !slices.Equal(got.keys, want) || got.reason != wantReason {
						t.Errorf("skip page = %+v, want keys %v and reason %v", got, want, wantReason)
					}
					if got.records < minRecords || got.records > maxRecords || got.bytes != control.bytes || got.loads != wantLoads {
						t.Errorf("skip charged (%d records, %d bytes, %d loads), no-skip suffix charged (%d records, %d bytes, %d loads)",
							got.records, got.bytes, got.loads, control.records, control.bytes, control.loads)
					}
				})
			}
		}
	}
}

func TestIntegrationIndexSkipContinuation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"ordinary", "covering", "covering_value", "fetch"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", kind, reverse), func(t *testing.T) {
				t.Parallel()
				store := setupStore(t)
				for id := int64(1); id <= 8; id++ {
					insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))})
				}
				plan := indexSkipPlan(t, indexSkipPricePlan(t, reverse, nil), kind)
				props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(2)
				props.Skip = 2
				first := readIndexSkipPage(t, store, plan, nil, props)
				want := []int64{3, 4}
				if reverse {
					want = []int64{6, 5}
				}
				if !slices.Equal(first.keys, want) || first.reason != recordlayer.ReturnLimitReached || len(first.continuation) == 0 {
					t.Fatalf("first page = %+v, want %v and resumable returned limit", first, want)
				}
				for _, skip := range []int{0, 1} {
					props.Skip = skip
					resumed := readIndexSkipPage(t, store, plan, first.continuation, props)
					want = []int64{int64(5 + skip), int64(6 + skip)}
					bound := int64(5 + skip)
					if reverse {
						want, bound = []int64{int64(4 - skip), int64(3 - skip)}, int64(4-skip)
					}
					controlProps := props
					controlProps.Skip = 0
					control := readIndexSkipPage(t, store, indexSkipPlan(t, indexSkipPricePlan(t, reverse, &bound), kind), nil, controlProps)
					if !slices.Equal(resumed.keys, want) || resumed.reason != recordlayer.ReturnLimitReached {
						t.Errorf("resumed skip=%d page = %+v, want %v", skip, resumed, want)
					}
					wantLoads := 2
					if kind == "covering" || kind == "covering_value" {
						wantLoads = 0
					}
					if !slices.Equal(control.keys, want) || control.reason != recordlayer.ReturnLimitReached ||
						control.records < 2 || control.records > 3 || control.bytes == 0 || control.loads != wantLoads {
						t.Fatalf("resumed skip=%d suffix control = %+v, want %v, 2..3 attempts and %d loads", skip, control, want, wantLoads)
					}
					if resumed.records < 2 || resumed.records > 3 || resumed.bytes != control.bytes || resumed.loads != wantLoads {
						t.Errorf("resumed skip=%d charged (%d,%d,%d), suffix control charged (%d,%d,%d)", skip,
							resumed.records, resumed.bytes, resumed.loads, control.records, control.bytes, control.loads)
					}
				}
			})
		}
	}
}

// IEEE equality binds both physical zero encodings. OFFSET is logical across
// those ranges, not a fresh selector offset in each range.
func TestIntegrationIndexSkipMultipleRanges(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			builder.AddIndex("TypedRecord", recordlayer.NewIndex("double_idx", recordlayer.Concat(recordlayer.Field("val_double"), recordlayer.Field("val_int32"))))
			md, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			var base *recordlayer.FDBRecordStore
			_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				base, err = recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(testSubspace(t)).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if err := base.DeleteAllRecords(); err != nil {
					return nil, err
				}
				for i, v := range []float64{math.Copysign(0, -1), math.Copysign(0, -1), 0, 0} {
					if _, err := base.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(int64(i + 1)), ValDouble: proto.Float64(v), ValInt32: proto.Int32(1)}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			comparisons := []*predicates.ComparisonRange{scanRangeTestEq(t, float64(0)), scanRangeTestEq(t, int64(1))}
			physicalTypes := []values.Type{values.NullableDouble, values.NullableInt}
			spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(comparisons, physicalTypes, nil, reverse, "skip-multi-control")
			if err != nil || len(materializeAllRanges(t, spec)) != 2 {
				t.Fatalf("zero equality must bind two ranges: spec=%+v err=%v", spec, err)
			}
			plan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
				"double_idx", comparisons, []string{"TypedRecord"}, integrationTypedRecordType(), reverse,
			)).WithKeyComponentTypes(physicalTypes).WithIndexMetadata([]string{"val_double", "val_int32"}, []string{"id"}, false)
			control := readIndexSkipPage(t, base, plan, nil, recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(4))
			wantAll := []int64{1, 2, 3, 4}
			if reverse {
				slices.Reverse(wantAll)
			}
			if !slices.Equal(control.keys, wantAll) || control.loads != 4 || control.records < 4 || control.bytes == 0 {
				t.Fatalf("multi-range no-skip control = %+v, want %v and four record loads", control, wantAll)
			}
			props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1)
			props.Skip = 3
			got := readIndexSkipPage(t, base, plan, nil, props)
			if !slices.Equal(got.keys, wantAll[3:]) || got.loads != 4 || got.records < 4 || got.records > 6 || got.bytes != control.bytes {
				t.Fatalf("multi-range skip must remain outer: got %+v, no-skip control %+v", got, control)
			}
		})
	}
}
