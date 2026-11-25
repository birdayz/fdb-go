package executor

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestIntegration_OrphanIndexEntry_RaisesStorageError pins finding F19: a query
// that reaches an index entry whose base record is GONE must raise a typed
// RecordCoreStorageError, not silently drop the row.
//
// Java (RecordQueryIndexPlan → FetchIndexRecords.PRIMARY_KEY →
// FDBRecordStoreBase.loadIndexEntryRecord) always fetches with
// IndexOrphanBehavior.ERROR: the ERROR arm throws
// RecordCoreStorageException("record not found from index entry") with
// INDEX_NAME / PRIMARY_KEY / INDEX_KEY. Go must match — an orphaned index entry
// is detectable store corruption, and returning fewer rows silently converts it
// into a wrong answer.
//
// Corruption is not producible from SQL, so it is fabricated white-box: save an
// Order, then ClearRange the record's key range under the store's records
// subspace while leaving the index entry intact. The index-scan+fetch plan then
// finds the entry, tries to load the record, and (post-fix) errors.
//
// Revert-proof: replace the indexFetchCursor orphan arm with `continue` and this
// test flips to 0 rows / nil error (the pre-fix behavior) and fails.
func TestIntegration_OrphanIndexEntry_RaisesStorageError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t) // metadata has a VALUE index "order_price_idx" on Order.price

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	})

	// Fabricate corruption: clear ONLY the record's key range (version + data
	// splits) under the records subspace, leaving the index entry dangling.
	// recordsSubspace = storeSubspace.Sub(RecordKey); the record for pk lives at
	// recordsSubspace.Sub(pk...) and FDBRangeKeys covers every split suffix.
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		recSub := testSubspace(t).Sub(recordlayer.RecordKey, int64(1))
		begin, end := recSub.FDBRangeKeys()
		rtx.Transaction().ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("clear record range: %v", err)
	}

	// Sanity: the index entry must still be present (otherwise the scan would
	// return 0 rows for a benign reason and never reach the orphan path).
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, oerr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if oerr != nil {
			return nil, oerr
		}
		idx := s.GetMetaData().GetIndex("order_price_idx")
		if idx == nil {
			t.Fatal("order_price_idx missing from metadata")
		}
		entries, lerr := recordlayer.AsList(ctx, s.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if lerr != nil {
			return nil, lerr
		}
		if len(entries) != 1 {
			t.Fatalf("index entries after record clear = %d, want 1 (orphan)", len(entries))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("scan index entries: %v", err)
	}

	// Execute SELECT * FROM orders WHERE price = 100 as an index-scan+fetch plan.
	// order_price_idx indexes only price, so a full-record projection is
	// non-covering → the plan fetches through indexFetchCursor → the orphan path.
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		timer := recordlayer.NewStoreTimer()
		rtx.SetTimer(timer)
		s, oerr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if oerr != nil {
			return nil, oerr
		}

		eqRange := predicates.EmptyComparisonRange()
		merged := eqRange.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(100), Typ: values.NotNullLong},
		})
		if !merged.Complete() {
			t.Fatal("build equality range failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{merged.Range},
			[]string{"Order"},
			PositionalTypeForRecordLayout((&demopb.Order{}).ProtoReflect().Descriptor(), false),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, cerr := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if cerr != nil {
			t.Fatalf("ExecutePlan: %v", cerr)
		}
		defer cursor.Close()

		results, drainErr := CollectAll(ctx, cursor)
		// Pre-fix behavior (the bug): drainErr == nil and len(results) == 0 — the
		// orphan is silently dropped. Post-fix: drainErr is a typed storage error.
		if drainErr == nil {
			t.Fatalf("orphaned index entry silently dropped: got %d rows, nil error (want RecordCoreStorageError)", len(results))
		}

		var storageErr *recordlayer.RecordCoreStorageError
		if !errors.As(drainErr, &storageErr) {
			t.Fatalf("error = %v (%T), want *recordlayer.RecordCoreStorageError", drainErr, drainErr)
		}
		if storageErr.Message != "record not found from index entry" {
			t.Errorf("message = %q, want %q", storageErr.Message, "record not found from index entry")
		}
		if storageErr.IndexName != "order_price_idx" {
			t.Errorf("index name = %q, want %q", storageErr.IndexName, "order_price_idx")
		}
		if len(storageErr.PrimaryKey) != 1 || storageErr.PrimaryKey[0] != int64(1) {
			t.Errorf("primary key = %v, want [1]", storageErr.PrimaryKey)
		}
		if len(storageErr.IndexKey) == 0 {
			t.Errorf("index key = %v, want non-empty (indexed value + pk)", storageErr.IndexKey)
		}
		if count := timer.GetCount(recordlayer.CountBadIndexEntry); count != 1 {
			t.Errorf("bad index entries = %d, want 1", count)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_IndexFetchBadEntryCounter(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	for _, api := range []string{"index", "partial-record"} {
		for _, outcome := range []string{"found", "missing", "decode-error", "nil-pk"} {
			t.Run(api+"/"+outcome, func(t *testing.T) {
				timer := recordlayer.NewStoreTimer()
				store.Context().SetTimer(timer)
				pk := tuple.Tuple{int64(1)}
				var record *recordlayer.FDBStoredRecord[proto.Message]
				var loadErr error
				switch outcome {
				case "found":
					record = &recordlayer.FDBStoredRecord[proto.Message]{PrimaryKey: pk, Record: &demopb.Order{OrderId: proto.Int64(1)}}
				case "decode-error":
					loadErr = &recordlayer.RecordDeserializationError{PrimaryKey: pk, Cause: fdb.Error{Code: 1007}}
				case "nil-pk":
					pk = nil
				}
				var result QueryResult
				var err error
				if api == "index" {
					cursor := indexFetchCursor{store: store}
					result, err = cursor.finish(&recordlayer.IndexEntry{Key: tuple.Tuple{int64(100), int64(1)}}, pk, record, loadErr)
				} else {
					cursor := fetchFullRecordCursor{store: store}
					result, err = cursor.finish(QueryResult{PrimaryKey: pk}, record, loadErr)
				}
				if outcome == "found" {
					if err != nil || result.Record != record {
						t.Fatalf("found result = %v, %v", result, err)
					}
				} else if outcome == "decode-error" {
					if !errors.Is(err, loadErr) {
						t.Fatalf("load error = %v, want %v", err, loadErr)
					}
				} else {
					var storageErr *recordlayer.RecordCoreStorageError
					if !errors.As(err, &storageErr) {
						t.Fatalf("missing record/key error = %v, want RecordCoreStorageError", err)
					}
				}
				want := int64(0)
				if outcome == "missing" {
					want = 1
				}
				if count := timer.GetCount(recordlayer.CountBadIndexEntry); count != want {
					t.Fatalf("bad index entries = %d, want %d", count, want)
				}
			})
		}
	}
}

// TestIntegration_OrphanIndexEntry_ScanIndexRecords_RaisesStorageError pins the
// sibling maintenance path: FDBRecordStore.ScanIndexRecords (used by the
// index-from-index rebuild, IndexingByIndex) must also raise on an orphan.
// Java's scanIndexRecords defaults to IndexOrphanBehavior.ERROR (not SKIP), so
// an orphaned source-index entry aborts the scan rather than silently building
// an incomplete target index.
func TestIntegration_OrphanIndexEntry_ScanIndexRecords_RaisesStorageError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		recSub := testSubspace(t).Sub(recordlayer.RecordKey, int64(1))
		begin, end := recSub.FDBRangeKeys()
		rtx.Transaction().ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("clear record range: %v", err)
	}

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, oerr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if oerr != nil {
			return nil, oerr
		}
		_, lerr := recordlayer.AsList(ctx, s.ScanIndexRecords("order_price_idx", recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if lerr == nil {
			t.Fatal("ScanIndexRecords silently skipped orphan: want RecordCoreStorageError")
		}
		var storageErr *recordlayer.RecordCoreStorageError
		if !errors.As(lerr, &storageErr) {
			t.Fatalf("error = %v (%T), want *recordlayer.RecordCoreStorageError", lerr, lerr)
		}
		if storageErr.Message != "record not found from index entry" {
			t.Errorf("message = %q, want %q", storageErr.Message, "record not found from index entry")
		}
		if storageErr.IndexName != "order_price_idx" {
			t.Errorf("index name = %q, want %q", storageErr.IndexName, "order_price_idx")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
