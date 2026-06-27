//go:build stress

package stress_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/gen"
	purefdb "fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	protopkg "google.golang.org/protobuf/proto"
)

func TestFDB_RawIngestBench(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	db, err := purefdb.OpenDatabase(ctx0, clusterFilePath, purefdb.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close()

	type config struct {
		workers   int
		batchSize int
	}
	configs := []config{
		{1, 500},
		{1, 2000},
		{4, 500},
		{4, 2000},
		{8, 2000},
		{16, 2000},
		{32, 2000},
	}

	for _, cfg := range configs {
		cfg := cfg
		t.Run(fmt.Sprintf("w%d_b%d", cfg.workers, cfg.batchSize), func(t *testing.T) {
			n := 1_000_000
			prefix := []byte(fmt.Sprintf("bench_w%d_b%d_", cfg.workers, cfg.batchSize))

			start := time.Now()
			var totalWritten atomic.Int64
			chunkSize := (n + cfg.workers - 1) / cfg.workers

			var wg sync.WaitGroup
			var firstErr atomic.Value

			for w := range cfg.workers {
				wStart := w * chunkSize
				wEnd := wStart + chunkSize
				if wEnd > n {
					wEnd = n
				}
				if wStart >= n {
					break
				}
				wg.Add(1)
				go func(from, to int) {
					defer wg.Done()
					for offset := from; offset < to; offset += cfg.batchSize {
						end := offset + cfg.batchSize
						if end > to {
							end = to
						}
						ctx := context.Background()
						tx := db.CreateTransaction()
						for i := offset; i < end; i++ {
							key := append(append([]byte{}, prefix...), tuple.Tuple{int64(i)}.Pack()...)
							val := make([]byte, 8)
							binary.LittleEndian.PutUint64(val, uint64(i*7))
							tx.Set(key, val)
						}
						if commitErr := tx.Commit(ctx); commitErr != nil {
							firstErr.CompareAndSwap(nil, commitErr)
							return
						}
						totalWritten.Add(int64(end - offset))
					}
				}(wStart, wEnd)
			}
			wg.Wait()

			if v := firstErr.Load(); v != nil {
				t.Fatalf("error: %v (wrote %d/%d)", v, totalWritten.Load(), n)
			}

			elapsed := time.Since(start)
			t.Logf("RAW FDB w=%d b=%d: %d rows in %v (%.0f rows/s)",
				cfg.workers, cfg.batchSize, n, elapsed, float64(n)/elapsed.Seconds())
		})
	}
}

func TestFDB_RawReadScaling(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	db, err := purefdb.OpenDatabase(ctx0, clusterFilePath, purefdb.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close()

	prefix := []byte("rawread_")
	{
		ctx := context.Background()
		for offset := 0; offset < 10_000; offset += 1000 {
			tx := db.CreateTransaction()
			for i := offset; i < offset+1000; i++ {
				key := append(append([]byte{}, prefix...), tuple.Tuple{int64(i)}.Pack()...)
				val := make([]byte, 8)
				binary.LittleEndian.PutUint64(val, uint64(i))
				tx.Set(key, val)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("populate: %v", err)
			}
		}
	}

	for _, workers := range []int{1, 4, 8} {
		workers := workers
		t.Run(fmt.Sprintf("w%d", workers), func(t *testing.T) {
			n := 100_000
			batchSize := 2000
			chunkSize := (n + workers - 1) / workers

			start := time.Now()
			var totalRead atomic.Int64
			var wg sync.WaitGroup
			var firstErr atomic.Value

			for w := range workers {
				wStart := w * chunkSize
				wEnd := wStart + chunkSize
				if wEnd > n {
					wEnd = n
				}
				if wStart >= n {
					break
				}
				wg.Add(1)
				go func(from, to int) {
					defer wg.Done()
					for offset := from; offset < to; offset += batchSize {
						end := offset + batchSize
						if end > to {
							end = to
						}
						tx := db.CreateTransaction()
						for i := offset; i < end; i++ {
							key := append(append([]byte{}, prefix...), tuple.Tuple{int64(i % 10_000)}.Pack()...)
							if _, getErr := tx.Get(context.Background(), key); getErr != nil {
								firstErr.CompareAndSwap(nil, getErr)
								return
							}
						}
						tx.Cancel()
						totalRead.Add(int64(end - offset))
					}
				}(wStart, wEnd)
			}
			wg.Wait()

			if v := firstErr.Load(); v != nil {
				t.Fatalf("error: %v", v)
			}

			elapsed := time.Since(start)
			t.Logf("RAW FDB READS w=%d: %d reads in %v (%.0f reads/s)",
				workers, n, elapsed, float64(n)/elapsed.Seconds())
		})
	}
}

func TestFDB_SaveRecordBatchScaling(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	pureDB, err := purefdb.OpenDatabase(ctx0, clusterFilePath, purefdb.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer pureDB.Close()

	fdbDB := recordlayer.NewFDBDatabase(fdb.WrapDatabase(pureDB))

	for _, workers := range []int{1, 4, 8} {
		workers := workers
		t.Run(fmt.Sprintf("w%d", workers), func(t *testing.T) {
			n := 500_000
			batchSize := 2000
			chunkSize := (n + workers - 1) / workers

			// Build metadata
			md := buildBenchMD(t)

			// Create store
			_, err := fdbDB.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
				ss := subspace.FromBytes(tuple.Tuple{fmt.Sprintf("savebatch_w%d", workers)}.Pack())
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rctx).SetSubspace(ss).SetMetaDataProvider(md).
					CreateOrOpen()
				return nil, err
			})
			if err != nil {
				t.Fatalf("create store: %v", err)
			}

			start := time.Now()
			var totalWritten atomic.Int64
			var wg sync.WaitGroup
			var firstErr atomic.Value

			for w := range workers {
				wStart := w * chunkSize
				wEnd := wStart + chunkSize
				if wEnd > n {
					wEnd = n
				}
				if wStart >= n {
					break
				}
				wg.Add(1)
				go func(from, to int) {
					defer wg.Done()
					for offset := from; offset < to; offset += batchSize {
						end := offset + batchSize
						if end > to {
							end = to
						}
						_, runErr := fdbDB.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
							ss := subspace.FromBytes(tuple.Tuple{fmt.Sprintf("savebatch_w%d", workers)}.Pack())
							store, err := recordlayer.NewStoreBuilder().
								SetContext(rctx).SetSubspace(ss).SetMetaDataProvider(md).
								Open()
							if err != nil {
								return nil, err
							}
							msgs := make([]protopkg.Message, end-offset)
							for i := offset; i < end; i++ {
								msgs[i-offset] = &gen.Order{
									OrderId: protopkg.Int64(int64(i)),
									Price:   protopkg.Int32(int32(i * 7)),
								}
							}
							_, err = store.SaveRecordBatch(msgs)
							return nil, err
						})
						if runErr != nil {
							firstErr.CompareAndSwap(nil, runErr)
							return
						}
						totalWritten.Add(int64(end - offset))
					}
				}(wStart, wEnd)
			}
			wg.Wait()

			if v := firstErr.Load(); v != nil {
				t.Fatalf("error: %v", v)
			}
			elapsed := time.Since(start)
			t.Logf("SaveRecordBatch w=%d: %d rows in %v (%.0f rows/s)",
				workers, n, elapsed, float64(n)/elapsed.Seconds())
		})
	}
}

func TestFDB_SaveRecordPerRowScaling(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	pureDB, err := purefdb.OpenDatabase(ctx0, clusterFilePath, purefdb.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer pureDB.Close()

	fdbDB := recordlayer.NewFDBDatabase(fdb.WrapDatabase(pureDB))
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	for _, workers := range []int{1, 4, 8} {
		workers := workers
		t.Run(fmt.Sprintf("w%d", workers), func(t *testing.T) {
			n := 100_000
			batchSize := 2000
			chunkSize := (n + workers - 1) / workers
			md := buildBenchMD(t)

			_, err := fdbDB.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
				ss := subspace.FromBytes(tuple.Tuple{fmt.Sprintf("saverow_w%d", workers)}.Pack())
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rctx).SetSubspace(ss).SetMetaDataProvider(md).
					CreateOrOpen()
				return nil, err
			})
			if err != nil {
				t.Fatalf("create store: %v", err)
			}

			start := time.Now()
			var totalWritten atomic.Int64
			var openNanos, saveNanos, commitNanos atomic.Int64
			var retryCount atomic.Int64
			var wg sync.WaitGroup
			var firstErr atomic.Value

			for w := range workers {
				wStart := w * chunkSize
				wEnd := wStart + chunkSize
				if wEnd > n {
					wEnd = n
				}
				if wStart >= n {
					break
				}
				wg.Add(1)
				go func(from, to int) {
					defer wg.Done()
					for offset := from; offset < to; offset += batchSize {
						end := offset + batchSize
						if end > to {
							end = to
						}
						t0 := time.Now()
						_, runErr := fdbDB.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
							retryCount.Add(1)
							ss := subspace.FromBytes(tuple.Tuple{fmt.Sprintf("saverow_w%d", workers)}.Pack())
							store, err := recordlayer.NewStoreBuilder().
								SetContext(rctx).SetSubspace(ss).SetMetaDataProvider(md).
								SetSkipPossiblyRebuild(true).
								Open()
							if err != nil {
								return nil, err
							}
							openNanos.Add(time.Since(t0).Nanoseconds())
							t1 := time.Now()
							for i := offset; i < end; i++ {
								msg := &gen.Order{
									OrderId: protopkg.Int64(int64(i)),
									Price:   protopkg.Int32(int32(i * 7)),
								}
								if _, err := store.SaveRecord(msg); err != nil {
									return nil, err
								}
							}
							saveNanos.Add(time.Since(t1).Nanoseconds())
							return nil, nil
						})
						commitNanos.Add(time.Since(t0).Nanoseconds())
						if runErr != nil {
							firstErr.CompareAndSwap(nil, runErr)
							return
						}
						totalWritten.Add(int64(end - offset))
					}
				}(wStart, wEnd)
			}
			wg.Wait()

			if v := firstErr.Load(); v != nil {
				t.Fatalf("error: %v", v)
			}
			elapsed := time.Since(start)
			expectedBatches := int64((n + batchSize - 1) / batchSize)
			t.Logf("SaveRecord per-row w=%d: %d rows in %v (%.0f rows/s) open=%.0fms save=%.0fms total=%.0fms runs=%d expected=%d retries=%d",
				workers, n, elapsed, float64(n)/elapsed.Seconds(),
				float64(openNanos.Load())/1e6, float64(saveNanos.Load())/1e6, float64(commitNanos.Load())/1e6,
				retryCount.Load(), expectedBatches, retryCount.Load()-expectedBatches)
		})
	}
}

func TestFDB_SaveRecordConcurrentVsBatch(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	pureDB, err := purefdb.OpenDatabase(ctx0, clusterFilePath, purefdb.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer pureDB.Close()

	fdbDB := recordlayer.NewFDBDatabase(fdb.WrapDatabase(pureDB))
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	md := buildBenchMD(t)

	batchSize := 2000
	totalRows := 20_000

	// Create store
	_, err = fdbDB.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
		ss := subspace.FromBytes(tuple.Tuple{"concurrent_vs_batch"}.Pack())
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rctx).SetSubspace(ss).SetMetaDataProvider(md).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	type approach struct {
		name string
		fn   func(store *recordlayer.FDBRecordStore, offset, end int) error
	}

	approaches := []approach{
		{
			name: "sequential",
			fn: func(store *recordlayer.FDBRecordStore, offset, end int) error {
				for i := offset; i < end; i++ {
					msg := &gen.Order{OrderId: protopkg.Int64(int64(i)), Price: protopkg.Int32(int32(i * 7))}
					if _, err := store.SaveRecord(msg); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name: "batch",
			fn: func(store *recordlayer.FDBRecordStore, offset, end int) error {
				msgs := make([]protopkg.Message, end-offset)
				for i := offset; i < end; i++ {
					msgs[i-offset] = &gen.Order{OrderId: protopkg.Int64(int64(i)), Price: protopkg.Int32(int32(i * 7))}
				}
				_, err := store.SaveRecordBatch(msgs)
				return err
			},
		},
	}

	for _, maxInFlight := range []int{8, 32, 128, 512} {
		n := maxInFlight
		approaches = append(approaches, approach{
			name: fmt.Sprintf("concurrent_%d", n),
			fn: func(store *recordlayer.FDBRecordStore, offset, end int) error {
				sem := make(chan struct{}, n)
				var wg sync.WaitGroup
				var firstErr atomic.Value
				for i := offset; i < end; i++ {
					if v := firstErr.Load(); v != nil {
						break
					}
					sem <- struct{}{}
					wg.Add(1)
					go func(id int) {
						defer func() { <-sem; wg.Done() }()
						msg := &gen.Order{OrderId: protopkg.Int64(int64(id)), Price: protopkg.Int32(int32(id * 7))}
						if _, err := store.SaveRecord(msg); err != nil {
							firstErr.CompareAndSwap(nil, err)
						}
					}(i)
				}
				wg.Wait()
				if v := firstErr.Load(); v != nil {
					return v.(error)
				}
				return nil
			},
		})
	}

	for _, a := range approaches {
		a := a
		t.Run(a.name, func(t *testing.T) {
			start := time.Now()
			for offset := 0; offset < totalRows; offset += batchSize {
				end := offset + batchSize
				if end > totalRows {
					end = totalRows
				}
				_, runErr := fdbDB.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
					ss := subspace.FromBytes(tuple.Tuple{"concurrent_vs_batch"}.Pack())
					store, err := recordlayer.NewStoreBuilder().
						SetContext(rctx).SetSubspace(ss).SetMetaDataProvider(md).
						SetSkipPossiblyRebuild(true).
						Open()
					if err != nil {
						return nil, err
					}
					return nil, a.fn(store, offset, end)
				})
				if runErr != nil {
					t.Fatalf("batch at offset %d: %v", offset, runErr)
				}
			}
			elapsed := time.Since(start)
			t.Logf("%s: %d rows in %v (%.0f rows/s)", a.name, totalRows, elapsed, float64(totalRows)/elapsed.Seconds())
		})
	}
}

func buildBenchMD(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}
