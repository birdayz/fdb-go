package recordlayer

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/gen/demopb"
	"google.golang.org/protobuf/proto"
)

var remoteFetchBenchmarkRows []*FDBIndexedRecord

// One operation is a fresh transaction, store open, and a full 1,000-row scan.
// Run with -test.benchtime=20x -test.benchmem for fixed-population allocations.
// Unquiet ns/op is informational, not a timing comparison.
func BenchmarkIndexRemoteFetchScanAllocations(b *testing.B) {
	ensureBenchDB(b)
	const rows = 1000
	metadata := benchMetaDataWithValueIndex(b)
	ss := benchSubspace(b)
	ctx := context.Background()
	_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().SetContext(rc).SetSubspace(ss).SetMetaDataProvider(metadata).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for id := range rows {
			if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(int64(id)), Price: proto.Int32(25)}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		b.Fatalf("seed 1000 records: %v", err)
	}
	for _, method := range []IndexFetchMethod{IndexFetchScanAndFetch, IndexFetchUseRemoteFetch} {
		b.Run(method.String(), func(b *testing.B) {
			scan := func(timer *StoreTimer) error {
				tx, err := sharedDB.CreateWritableTransaction()
				if err != nil {
					return err
				}
				rc := sharedDB.NewRecordContext(tx)
				defer rc.Cancel()
				rc.SetTimer(timer)
				store, err := NewStoreBuilder().SetContext(rc).SetSubspace(ss).SetMetaDataProvider(metadata).Open()
				if err != nil {
					return err
				}
				index := metadata.GetIndex("price_idx")
				cursor := store.ScanIndexRecordsWithFetchMethod(index, method, IndexScanByValue, TupleRangeAll,
					store.GetCommonPrimaryKeyLength(index), nil, IndexOrphanError, NewScanProperties(DefaultExecuteProperties()))
				defer cursor.Close()
				result := make([]*FDBIndexedRecord, 0, rows)
				for {
					next, err := cursor.OnNext(ctx)
					if err != nil {
						return err
					}
					if !next.HasNext() {
						if next.GetNoNextReason() != SourceExhausted {
							return fmt.Errorf("unexpected terminal: %v", next.GetNoNextReason())
						}
						break
					}
					record := next.GetValue()
					order := record.Record.Record.(*demopb.Order)
					if order.GetOrderId() != int64(len(result)) || order.GetPrice() != 25 {
						return fmt.Errorf("row %d: got order %v", len(result), order)
					}
					result = append(result, record)
				}
				if len(result) != rows {
					return fmt.Errorf("scan returned %d rows, want %d", len(result), rows)
				}
				remoteFetchBenchmarkRows = result
				return nil
			}
			// Positive controls prove the methods traverse different fetch paths;
			// timers are disabled in the measured samples.
			timer := NewStoreTimer()
			if err := scan(timer); err != nil {
				b.Fatal(err)
			}
			if got := timer.GetCount(CountRemoteFetch); (method == IndexFetchUseRemoteFetch) != (got > 0) {
				b.Fatalf("%s remote fetch calls = %d", method, got)
			}
			b.Logf("population: rows/op=%d iterations=%d method=%s timer=disabled", rows, b.N, method)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := scan(nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
