package executor

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

var indexFetchMethods = []indexfetch.Method{
	indexfetch.ScanAndFetch, indexfetch.UseRemoteFetch, indexfetch.UseRemoteFetchWithFallback,
}

// priceScan is the RecordQueryIndexPlan Java's RemoteFetchTest plans for a
// num_value predicate: a BY_VALUE scan of order_price_idx that fetches each
// entry's record with method.
func priceScan(method indexfetch.Method, reverse bool, comps ...*predicates.ComparisonRange) *plans.RecordQueryIndexPlan {
	if len(comps) == 0 {
		comps = []*predicates.ComparisonRange{predicates.EmptyComparisonRange()}
	}
	return mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
		"order_price_idx", comps, []string{"Order"}, integrationOrderType(), reverse,
	)).WithKeyComponentTypes([]values.Type{values.NullableInt}).WithIndexFetchMethod(method)
}

func priceRange(t *testing.T, kind predicates.ComparisonType, price int32) *predicates.ComparisonRange {
	t.Helper()
	merged := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type: kind, Operand: &values.ConstantValue{Value: int64(price), Typ: values.NotNullLong},
	})
	if !merged.Complete() {
		t.Fatalf("build %v %d range", kind, price)
	}
	return merged.Range
}

// seedPricedOrders saves orders 0..count-1 with price = id, in batches.
func seedPricedOrders(t *testing.T, store *recordlayer.FDBRecordStore, count int) {
	t.Helper()
	for first := 0; first < count; first += 100 {
		var batch []*demopb.Order
		for id := first; id < min(count, first+100); id++ {
			batch = append(batch, &demopb.Order{OrderId: proto.Int64(int64(id)), Price: proto.Int32(int32(id)), Quantity: proto.Int32(1)})
		}
		insertOrders(t, store, batch...)
	}
}

// inUncommittedStore runs fn over the test's store in a transaction that is
// never committed, with a fresh timer, so a local write cannot leak into the
// next case.
func inUncommittedStore(t *testing.T, store *recordlayer.FDBRecordStore, fn func(*recordlayer.FDBRecordStore, *recordlayer.StoreTimer)) {
	t.Helper()
	transaction, err := testDB.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Cancel()
	rtx := testDB.NewRecordContext(transaction)
	timer := recordlayer.NewStoreTimer()
	rtx.SetTimer(timer)
	s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
		SetSubspace(testSubspace(t)).Open()
	if err != nil {
		t.Fatal(err)
	}
	fn(s, timer)
}

type fetchedPage struct {
	ids          []int64
	continuation []byte
	err          error
}

func executeFetch(t *testing.T, s *recordlayer.FDBRecordStore, plan plans.RecordQueryPlan, continuation []byte, props recordlayer.ExecuteProperties) fetchedPage {
	t.Helper()
	ctx := context.Background()
	cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), continuation, props)
	if err != nil {
		return fetchedPage{err: err}
	}
	defer cursor.Close()
	var page fetchedPage
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			page.err = err
			return page
		}
		if !result.HasNext() {
			if !result.GetContinuation().IsEnd() {
				if page.continuation, err = result.GetContinuation().ToBytes(); err != nil {
					t.Fatal(err)
				}
			}
			return page
		}
		page.ids = append(page.ids, result.GetValue().Record.Record.(*demopb.Order).GetOrderId())
	}
}

func idRange(from, to int64) []int64 {
	var ids []int64
	if from <= to {
		for id := from; id <= to; id++ {
			ids = append(ids, id)
		}
	} else {
		for id := from; id >= to; id-- {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestIntegration_IndexFetchMethod_RemoteFetchRuns is RemoteFetchTest's
// indexPrefetchSimpleIndexTest and its reverse at plan level: every method
// returns SCAN_AND_FETCH's rows, and Java's counters (assertCounters, :223-233)
// prove the remote methods took the mapped read and SCAN_AND_FETCH did not.
func TestIntegration_IndexFetchMethod_RemoteFetchRuns(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	seedPricedOrders(t, store, 20)
	for _, reverse := range []bool{false, true} {
		for _, bounds := range []struct {
			comps []*predicates.ComparisonRange
			ids   []int64
		}{
			{nil, idRange(0, 19)},
			{[]*predicates.ComparisonRange{priceRange(t, predicates.ComparisonGreaterThanEq, 12)}, idRange(12, 19)},
			{[]*predicates.ComparisonRange{priceRange(t, predicates.ComparisonEquals, 7)}, []int64{7}},
		} {
			want := slices.Clone(bounds.ids)
			if reverse {
				slices.Reverse(want)
			}
			for _, method := range indexFetchMethods {
				inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, timer *recordlayer.StoreTimer) {
					page := executeFetch(t, s, priceScan(method, reverse, bounds.comps...), nil, recordlayer.DefaultExecuteProperties())
					if page.err != nil || !slices.Equal(page.ids, want) {
						t.Fatalf("%v reverse=%v: rows %v err %v, want %v", method, reverse, page.ids, page.err, want)
					}
					remoteFetches := timer.GetCount(recordlayer.CountRemoteFetch)
					remoteEntries := timer.GetCount(recordlayer.EventScanRemoteFetchEntry)
					indexScans := timer.GetCount(recordlayer.EventScanIndex)
					if method == indexfetch.ScanAndFetch {
						if remoteFetches != 0 || remoteEntries != 0 || indexScans == 0 {
							t.Fatalf("SCAN_AND_FETCH: remote fetches %d, entries %d, index scans %d; want 0, 0, >0",
								remoteFetches, remoteEntries, indexScans)
						}
						return
					}
					// One mapped read, and one entry event per result including the exhausted one.
					if remoteFetches != 1 || remoteEntries != int64(len(want)+1) || indexScans != 0 {
						t.Fatalf("%v: remote fetches %d, entries %d, index scans %d; want 1, %d, 0",
							method, remoteFetches, remoteEntries, indexScans, len(want)+1)
					}
				})
			}
		}
	}
}

// TestIntegration_IndexFetchMethod_ContinuationsResumeAcrossMethods is
// RemoteFetchTest.indexPrefetchWithMixedContinuationTest (:210): Java's
// continuation planHash leaves the method out (RecordQueryIndexPlan.java
// :568-585), so a page under one method resumes under any other with the same
// rows and the same tokens.
func TestIntegration_IndexFetchMethod_ContinuationsResumeAcrossMethods(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	seedPricedOrders(t, store, 20)
	limited := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(7)
	for _, reverse := range []bool{false, true} {
		want := idRange(0, 19)
		if reverse {
			slices.Reverse(want)
		}
		for _, first := range indexFetchMethods {
			for _, second := range indexFetchMethods {
				inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, _ *recordlayer.StoreTimer) {
					head := executeFetch(t, s, priceScan(first, reverse), nil, limited)
					if head.err != nil || !slices.Equal(head.ids, want[:7]) || head.continuation == nil {
						t.Fatalf("%v page: rows %v err %v continuation %x", first, head.ids, head.err, head.continuation)
					}
					reference := executeFetch(t, s, priceScan(indexfetch.ScanAndFetch, reverse), nil, limited)
					if string(reference.continuation) != string(head.continuation) {
						t.Fatalf("%v token %x differs from SCAN_AND_FETCH's %x", first, head.continuation, reference.continuation)
					}
					tail := executeFetch(t, s, priceScan(second, reverse), head.continuation, recordlayer.DefaultExecuteProperties())
					if tail.err != nil || !slices.Equal(tail.ids, want[7:]) {
						t.Fatalf("%v page resumed under %v: rows %v err %v, want %v", first, second, tail.ids, tail.err, want[7:])
					}
				})
			}
		}
	}
}

// TestIntegration_IndexFetchMethod_FallbackAfterReturnedRows is
// RemoteFetchTest.failAfterRecordsReturnedTest (:421, :457): a record written
// in the transaction fails the mapped read once the scan reaches its batch.
// USE_REMOTE_FETCH fails after returning rows; WITH_FALLBACK resumes after the
// last returned row with Java's fallBackContinueFrom, which passes the same
// ExecuteProperties (RecordQueryIndexPlan.java:707-720), so a row limit
// applies again from the resumed position.
func TestIntegration_IndexFetchMethod_FallbackAfterReturnedRows(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	const count = 500
	seedPricedOrders(t, store, count)
	for _, reverse := range []bool{false, true} {
		// The 101st row in scan order, past the first mapped batch.
		written := int64(100)
		want := idRange(0, count-1)
		if reverse {
			written = count - 101
			slices.Reverse(want)
		}
		for _, limit := range []int{0, 150} {
			props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit)
			var failedAfter int
			inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, _ *recordlayer.StoreTimer) {
				rewriteQuantity(t, s, written)
				page := executeFetch(t, s, priceScan(indexfetch.UseRemoteFetch, reverse), nil, props)
				var native fdb.Error
				if !errors.As(page.err, &native) || native.Code != 2039 {
					t.Fatalf("USE_REMOTE_FETCH over a written record: err %v, want get_mapped_range_reads_your_writes", page.err)
				}
				if len(page.ids) == 0 || !slices.Equal(page.ids, want[:len(page.ids)]) {
					t.Fatalf("USE_REMOTE_FETCH returned %v before failing, want a non-empty prefix", page.ids)
				}
				failedAfter = len(page.ids)
			})
			wantFallback := want
			if limit > 0 {
				// The remote rows, then the fallback's own limit from there.
				wantFallback = want[:min(len(want), failedAfter+limit)]
			}
			inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, timer *recordlayer.StoreTimer) {
				rewriteQuantity(t, s, written)
				page := executeFetch(t, s, priceScan(indexfetch.UseRemoteFetchWithFallback, reverse), nil, props)
				if page.err != nil || !slices.Equal(page.ids, wantFallback) {
					t.Fatalf("WITH_FALLBACK limit %d after %d remote rows: %d rows (err %v), want %d",
						limit, failedAfter, len(page.ids), page.err, len(wantFallback))
				}
				if timer.GetCount(recordlayer.EventScanRemoteFetchEntry) == 0 || timer.GetCount(recordlayer.EventScanIndex) == 0 {
					t.Fatal("WITH_FALLBACK did not run both the mapped read and the fallback scan")
				}
			})
		}
	}
}

func rewriteQuantity(t *testing.T, s *recordlayer.FDBRecordStore, id int64) {
	t.Helper()
	if _, err := s.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id)), Quantity: proto.Int32(2)}); err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexFetchMethod_ImmediateFailures is RemoteFetchTest's
// indexPrefetchSimpleIndexFallbackTest (:493) and testOrphanPolicyError (:519)
// at plan level: SNAPSHOT fails USE_REMOTE_FETCH with Java's
// UnsupportedOperationException and falls back under WITH_FALLBACK; an orphan
// entry fails both remote methods, WITH_FALLBACK after its fallback scan meets
// the orphan too.
func TestIntegration_IndexFetchMethod_ImmediateFailures(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	seedPricedOrders(t, store, 5)
	snapshot := recordlayer.DefaultExecuteProperties()
	snapshot.IsolationLevel = recordlayer.IsolationLevelSnapshot
	for _, method := range indexFetchMethods {
		inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, timer *recordlayer.StoreTimer) {
			page := executeFetch(t, s, priceScan(method, false), nil, snapshot)
			var unsupported *recordlayer.UnsupportedOperationError
			if method == indexfetch.UseRemoteFetch {
				if !errors.As(page.err, &unsupported) {
					t.Fatalf("SNAPSHOT USE_REMOTE_FETCH: err %v, want UnsupportedOperationError", page.err)
				}
				return
			}
			if page.err != nil || !slices.Equal(page.ids, idRange(0, 4)) {
				t.Fatalf("SNAPSHOT %v: rows %v err %v", method, page.ids, page.err)
			}
			// Java counts the refused mapped read, then falls back to the index scan.
			if method == indexfetch.UseRemoteFetchWithFallback &&
				(timer.GetCount(recordlayer.CountRemoteFetch) != 1 || timer.GetCount(recordlayer.EventScanIndex) == 0) {
				t.Fatalf("SNAPSHOT WITH_FALLBACK: %d mapped reads, %d index scans; want 1 and the fallback scan",
					timer.GetCount(recordlayer.CountRemoteFetch), timer.GetCount(recordlayer.EventScanIndex))
			}
		})
	}

	_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		begin, end := testSubspace(t).Sub(recordlayer.RecordKey, int64(2)).FDBRangeKeys()
		rtx.Transaction().ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for method, want := range map[indexfetch.Method]struct {
		message    string
		badEntries int64
	}{
		indexfetch.ScanAndFetch:               {"record not found from index entry", 1},
		indexfetch.UseRemoteFetch:             {"record not found for prefetched index entry", 1},
		indexfetch.UseRemoteFetchWithFallback: {"record not found from index entry", 2},
	} {
		inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, timer *recordlayer.StoreTimer) {
			page := executeFetch(t, s, priceScan(method, false), nil, recordlayer.DefaultExecuteProperties())
			var storage *recordlayer.RecordCoreStorageError
			if !errors.As(page.err, &storage) || storage.Message != want.message {
				t.Fatalf("%v over an orphan: err %v, want %q", method, page.err, want.message)
			}
			if got := timer.GetCount(recordlayer.CountBadIndexEntry); got != want.badEntries {
				t.Fatalf("%v over an orphan: %d bad entries, want %d", method, got, want.badEntries)
			}
		})
	}
}

// TestIndexFetchMethod_DowngradesBelowAPIVersion71 pins
// RecordQueryIndexPlan.executePlan (:259-271): below API version 7.1 every
// method scans and fetches.
func TestIndexFetchMethod_DowngradesBelowAPIVersion71(t *testing.T) {
	t.Parallel()
	for _, method := range indexFetchMethods {
		for version, want := range map[int]indexfetch.Method{
			0: indexfetch.ScanAndFetch, 630: indexfetch.ScanAndFetch, 700: indexfetch.ScanAndFetch,
			710: method, 730: method,
		} {
			if got := effectiveIndexFetchMethod(method, version); got != want {
				t.Errorf("%v at API version %d runs as %v, want %v", method, version, got, want)
			}
		}
	}
}

// TestIndexFetchMethod_OutsideExecutionIdentity pins what lets a continuation
// cross methods: like Java's continuation planHash, the scan's execution salt
// is the same under every method, fetching and covering alike.
func TestIndexFetchMethod_OutsideExecutionIdentity(t *testing.T) {
	t.Parallel()
	var fetching, covering []string
	for _, method := range indexFetchMethods {
		plan := saltGoldenPlan().WithIndexFetchMethod(method)
		salt, err := indexScanRangeFingerprintSalt(plan, recordlayer.IndexScanByValue)
		if err != nil {
			t.Fatal(err)
		}
		fetching = append(fetching, salt)
		coveringSalt, err := coveringIndexScanRangeFingerprintSalt(
			mustExecutorConstruct(plans.NewRecordQueryCoveringIndexPlan(plan)), recordlayer.IndexScanByValue)
		if err != nil {
			t.Fatal(err)
		}
		covering = append(covering, coveringSalt)
	}
	if fetching[0] != fetching[1] || fetching[0] != fetching[2] || covering[0] != covering[1] || covering[0] != covering[2] {
		t.Fatal("the fetch method changed a scan's execution identity; its continuations would not resume under another method")
	}
}

// TestIntegration_IndexFetchMethod_UnknownMethod pins Java's default arm:
// "Unknown useIndexPrefetch option".
func TestIntegration_IndexFetchMethod_UnknownMethod(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	inUncommittedStore(t, store, func(s *recordlayer.FDBRecordStore, _ *recordlayer.StoreTimer) {
		page := executeFetch(t, s, priceScan(indexfetch.Method(7), false), nil, recordlayer.DefaultExecuteProperties())
		var recordCore *recordlayer.RecordCoreError
		if !errors.As(page.err, &recordCore) || recordCore.Message != "Unknown useIndexPrefetch option" {
			t.Fatalf("unknown method: err %v, want Java's RecordCoreException", page.err)
		}
	})
}
