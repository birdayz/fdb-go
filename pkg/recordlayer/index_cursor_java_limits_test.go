package recordlayer

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Java KeyValueCursorBase checks CursorLimitManager before the iterator and
// returns a last-key row-limit token even at the exact end of the index.
func TestFDB_IndexCursorJavaLimitBoundary(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).SetStoreRecordVersions(true)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewIndex("price", Field("price")))
	builder.AddIndex("Order", NewCountIndex("count", GroupAll(Field("price"))))
	builder.AddIndex("Order", NewMinEverTupleIndex("min_ever_tuple", GroupBy(Field("order_id"), Field("price"))))
	builder.AddIndex("Order", NewMaxEverTupleIndex("max_ever_tuple", GroupBy(Field("order_id"), Field("price"))))
	builder.AddIndex("Order", NewMaxEverVersionIndex("max_ever_version", GroupBy(VersionKey(), Field("price"))))
	bitmap := NewBitmapValueIndex("bitmap", GroupBy(Field("order_id"), Field("price")))
	bitmap.Options[IndexOptionBitmapValueEntrySize] = "4"
	builder.AddIndex("Order", bitmap)
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	open := func() *FDBRecordStore {
		store, err := NewStoreBuilder().SetContext(loadParityContext(t, db)).SetSubspace(root).SetMetaDataProvider(metadata).CreateOrOpen()
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	seed := open()
	if _, err := seed.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}); err != nil {
		t.Fatal(err)
	}
	assertLoadParityCommit(t, seed.context, false)
	scanIndex := func(store *FDBRecordStore, indexName string, continuation []byte, props ScanProperties) RecordCursor[*IndexEntry] {
		if indexName == "price" {
			return MapCursor(store.ScanIndexRecords("price", TupleRangeAll, continuation, props),
				func(rec *FDBIndexedRecord) *IndexEntry { return rec.IndexEntry })
		}
		return store.ScanIndexByType(metadata.GetIndex(indexName), IndexScanByGroup, TupleRangeAll, continuation, props)
	}
	for _, indexName := range []string{"price", "count", "min_ever_tuple", "max_ever_tuple", "max_ever_version", "bitmap"} {
		for _, scanLimit := range []int{0, 1} {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/scanned=%d/reverse=%v", indexName, scanLimit, reverse), func(t *testing.T) {
					store := open()
					state := NewScanLimiterState()
					execute := DefaultExecuteProperties().WithReturnedRowLimit(1)
					execute.ScannedRecordsLimit = scanLimit
					execute.ScanState = state
					props := NewScanProperties(execute).WithReverse(reverse)
					cursor := scanIndex(store, indexName, nil, props)
					first, err := cursor.OnNext(context.Background())
					if err != nil || !first.HasNext() {
						t.Fatalf("first result: %+v %v", first, err)
					}
					last, err := cursor.OnNext(context.Background())
					want := ReturnLimitReached
					if scanLimit > 0 {
						want = ScanLimitReached
					}
					if err != nil || last.HasNext() || last.GetNoNextReason() != want || last.GetContinuation().IsEnd() {
						t.Fatalf("exact Java index limit boundary: reason=%v end=%v err=%v want=%v with last-key token", last.GetNoNextReason(), last.GetContinuation().IsEnd(), err, want)
					}
					if state.RecordsScanned() != 2 {
						t.Fatalf("scan attempts=%d want value plus terminal attempt", state.RecordsScanned())
					}
					again, err := cursor.OnNext(context.Background())
					if err != nil || again.GetNoNextReason() != want || state.RecordsScanned() != 2 {
						t.Fatalf("terminal replay changed result/accounting: %+v %v attempts=%d", again, err, state.RecordsScanned())
					}
					if scanLimit == 0 {
						token, err := last.GetContinuation().ToBytes()
						if err != nil || len(token) == 0 {
							t.Fatalf("continuation bytes=%x err=%v", token, err)
						}
						resumeState := NewScanLimiterState()
						resumeExec := DefaultExecuteProperties().WithReturnedRowLimit(1)
						resumeExec.ScanState = resumeState
						resumed := scanIndex(store, indexName, token, NewScanProperties(resumeExec).WithReverse(reverse))
						end, err := resumed.OnNext(context.Background())
						if err != nil || end.HasNext() || end.GetNoNextReason() != SourceExhausted || !end.GetContinuation().IsEnd() || resumeState.RecordsScanned() != 1 {
							t.Fatalf("resumed page after exact boundary: next=%v reason=%v end=%v scanned=%d err=%v",
								end.HasNext(), end.GetNoNextReason(), end.GetContinuation().IsEnd(), resumeState.RecordsScanned(), err)
						}
					}
				})
			}
		}
		for _, limit := range []int{-1, -2, -100} {
			t.Run(fmt.Sprintf("%s/nonpositive_limit=%d", indexName, limit), func(t *testing.T) {
				store := open()
				state := NewScanLimiterState()
				execute := DefaultExecuteProperties().WithReturnedRowLimit(limit)
				execute.ScanState = state
				rows, err := AsList(context.Background(), scanIndex(store, indexName, nil, NewScanProperties(execute)))
				if err != nil || len(rows) != 1 || state.RecordsScanned() != 2 {
					t.Fatalf("nonpositive limit%d is unlimited: rows=%d attempts=%d error=%v", limit, len(rows), state.RecordsScanned(), err)
				}
			})
		}
	}
}
