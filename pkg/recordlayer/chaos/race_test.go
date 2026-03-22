package chaos

import (
	"context"
	"sync"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestIntraTransactionRace exercises multiple goroutines sharing one
// FDBRecordStore within a single FDB transaction. This is the scenario
// RFC 008 addresses. Run with -race to detect data races:
//
//	bazelisk test //pkg/recordlayer/chaos:chaos_test --test_arg="-test.run=TestIntraTransactionRace" --test_arg="-race" --test_output=streamed
func TestIntraTransactionRace(t *testing.T) {
	t.Parallel()

	md := buildConcurrentKitchenSinkMetadata()
	db := recordlayer.NewFDBDatabase(testRealDB)
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	ctx := context.Background()

	// Pre-create store.
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
	})
	if err != nil {
		t.Fatalf("failed to initialize store: %v", err)
	}

	// Run N iterations. Each iteration opens one store in one transaction
	// and hammers it from multiple goroutines concurrently.
	for iter := 0; iter < 5; iter++ {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(sub).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			var wg sync.WaitGroup
			errs := make(chan error, 100)

			// 5 writer goroutines — concurrent SaveRecord on the same store.
			for w := 0; w < 5; w++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for i := 0; i < 10; i++ {
						pk := int64(id*100 + i)
						_, err := store.SaveRecord(&gen.Order{
							OrderId:  proto.Int64(pk),
							Price:    proto.Int32(int32(pk * 7)),
							Quantity: proto.Int32(int32(i + 1)),
						})
						if err != nil {
							errs <- err
							return
						}
					}
				}(w)
			}

			// 3 reader goroutines — concurrent LoadRecord on the same store.
			for r := 0; r < 3; r++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for i := 0; i < 10; i++ {
						pk := int64(id*100 + i)
						_, _ = store.LoadRecord(tuple.Tuple{pk})
					}
				}(r)
			}

			// 2 scanner goroutines — concurrent ScanRecords.
			for s := 0; s < 2; s++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
					defer func() { _ = cursor.Close() }()
					for {
						result, err := cursor.OnNext(ctx)
						if err != nil {
							break
						}
						if !result.HasNext() {
							break
						}
						_ = result.GetValue()
					}
				}()
			}

			// 1 deleter goroutine.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					pk := int64(i)
					_, _ = store.DeleteRecord(tuple.Tuple{pk})
				}
			}()

			// 1 goroutine doing GetRecordCount.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					_, _ = store.GetRecordCount()
				}
			}()

			// 1 goroutine reading index state.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					_ = store.GetFormatVersion()
					_ = store.GetUserVersion()
					_ = store.GetMetaDataVersion()
					_ = store.IsStateCacheable()
					_ = store.GetRecordStoreState()
				}
			}()

			wg.Wait()
			close(errs)

			for err := range errs {
				return nil, err
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("iteration %d failed: %v", iter, err)
		}
	}

	t.Log("intra-transaction race test passed")
}
