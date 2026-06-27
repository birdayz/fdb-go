package chaos

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
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

// TestConcurrentSave200 fires 200 concurrent SaveRecord goroutines within a
// single FDB transaction, waits for all to complete, then verifies every record
// is loadable, the count is correct, and all indexes are consistent.
//
// Verification runs in two phases:
//   - Phase 1 (same transaction): LoadRecord, GetRecordCount, ScanRecords
//   - Phase 2 (after commit): VerifySnapshot checks all indexes including VERSION
//     (VERSION index uses SET_VERSIONSTAMPED_KEY, only visible after commit)
func TestConcurrentSave200(t *testing.T) {
	t.Parallel()

	md := buildConcurrentKitchenSinkMetadata()
	db := recordlayer.NewFDBDatabase(testRealDB)
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	ctx := context.Background()

	const N = 200

	// Transaction 1: 200 concurrent saves + in-transaction verification.
	_, _, err := db.RunWithVersionstamp(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Fire 200 goroutines, each saving a unique record.
		var wg sync.WaitGroup
		errs := make(chan error, N)

		for i := 0; i < N; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				_, err := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(id)),
					Price:    proto.Int32(int32(id * 3)),
					Quantity: proto.Int32(int32(id%10 + 1)),
				})
				if err != nil {
					errs <- fmt.Errorf("save pk=%d: %w", id, err)
				}
			}(i)
		}

		wg.Wait()
		close(errs)
		for err := range errs {
			return nil, err
		}

		// --- Phase 1: in-transaction verification ---

		// 1. Every record is loadable.
		for i := 0; i < N; i++ {
			rec, err := store.LoadRecord(tuple.Tuple{int64(i)})
			if err != nil {
				return nil, fmt.Errorf("load pk=%d: %w", i, err)
			}
			if rec == nil {
				return nil, fmt.Errorf("load pk=%d: record not found", i)
			}
		}

		// 2. Record count matches.
		count, err := store.GetRecordCount()
		if err != nil {
			return nil, fmt.Errorf("get record count: %w", err)
		}
		if count != N {
			return nil, fmt.Errorf("record count: got %d, want %d", count, N)
		}

		// 3. Full scan returns all records.
		scanCount := 0
		cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, fmt.Errorf("scan error: %w", err)
			}
			if !result.HasNext() {
				break
			}
			_ = result.GetValue()
			scanCount++
		}
		_ = cursor.Close()
		if scanCount != N {
			return nil, fmt.Errorf("scan count: got %d, want %d", scanCount, N)
		}

		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// --- Phase 2: post-commit verification (separate transaction) ---
	// VERSION index entries use SET_VERSIONSTAMPED_KEY, only visible after commit.
	// VerifySnapshot checks all indexes: VALUE, COUNT, SUM, RANK, VERSION, covering.
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			Open()
		if err != nil {
			return nil, err
		}

		violations := VerifySnapshot(store, md)
		if len(violations) > 0 {
			msg := fmt.Sprintf("%d index violations after 200 concurrent saves:\n", len(violations))
			for _, v := range violations {
				msg += fmt.Sprintf("  - %s\n", v)
			}
			return nil, errors.New(msg)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("200 concurrent saves: all records loaded, count correct, all 6 index types verified")
}
