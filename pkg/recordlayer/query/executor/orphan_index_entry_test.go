package executor

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
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

	insertOrders(t, store, &gen.Order{
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
		s, oerr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if oerr != nil {
			return nil, oerr
		}

		eqRange := predicates.EmptyComparisonRange()
		merged := eqRange.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.LiteralValue(int64(100)),
		})
		if !merged.Ok {
			t.Fatal("build equality range failed")
		}

		indexPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{merged.Range},
			[]string{"Order"},
			nil,
			false,
		)

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
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_OrphanIndexEntry_ScanIndexRecords_RaisesStorageError pins the
// sibling maintenance path: FDBRecordStore.ScanIndexRecords (used by the
// index-from-index rebuild, IndexingByIndex) must also raise on an orphan.
// Java's scanIndexRecords defaults to IndexOrphanBehavior.ERROR (not SKIP), so
// an orphaned source-index entry aborts the scan rather than silently building
// an incomplete target index.
//
// Revert-proof: restore the `continue` in indexRecordCursor.OnNext and this
// test's drain returns (rows=0, nil error) and fails.
func TestIntegration_OrphanIndexEntry_ScanIndexRecords_RaisesStorageError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
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
