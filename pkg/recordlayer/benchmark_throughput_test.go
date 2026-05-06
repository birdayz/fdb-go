package recordlayer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// Throughput benchmarks for SaveRecord. These measure the hot path that
// dominates usage-based billing ingest: N records per transaction, with
// indexes (VALUE, SUM, COUNT), at various concurrency levels.
//
// The goal is to identify and optimize the bottlenecks:
//   - loadWithSplit: blocking FDB read per record to check existence (11% CPU)
//   - key expression evaluation: PK + index key extraction (17% CPU)
//   - proto marshal: serializing the record (included in saveWithSplit)
//   - GC pressure: allocations per record
//
// Run: bazelisk run //pkg/recordlayer:recordlayer_test -- \
//        -test.bench=BenchmarkThroughput -test.benchtime=10s -test.run='^$'

// --- Metadata builders for throughput benchmarks ---

func throughputMetaDataPlain(b *testing.B) *RecordMetaData {
	b.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		b.Fatal(err)
	}
	return md
}

func throughputMetaDataWithAggregateIndexes(b *testing.B) *RecordMetaData {
	b.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	// VALUE index on price (like event_by_customer_meter_time)
	builder.AddIndex("Order", NewIndex("order_price_idx", Field("price")))
	// SUM index (like usage_sum)
	builder.AddIndex("Order", NewSumIndex("order_sum_price",
		GroupBy(Field("price"), Field("order_id"))))
	// COUNT index (like usage_count)
	builder.AddIndex("Order", NewCountIndex("order_count",
		GroupBy(EmptyKey(), Field("order_id"))))
	// Record count
	builder.SetRecordCountKey(EmptyKey())
	md, err := builder.Build()
	if err != nil {
		b.Fatal(err)
	}
	return md
}

// --- Batch size benchmarks (single goroutine) ---
// Measures: how does batch size affect per-event cost?

func BenchmarkThroughputBatch1(b *testing.B) {
	benchThroughputBatch(b, 1, false)
}

func BenchmarkThroughputBatch10(b *testing.B) {
	benchThroughputBatch(b, 10, false)
}

func BenchmarkThroughputBatch50(b *testing.B) {
	benchThroughputBatch(b, 50, false)
}

func BenchmarkThroughputBatch100(b *testing.B) {
	benchThroughputBatch(b, 100, false)
}

// With aggregate indexes (SUM + COUNT + VALUE + record count)
func BenchmarkThroughputBatch50WithIndexes(b *testing.B) {
	benchThroughputBatch(b, 50, true)
}

func BenchmarkThroughputBatch100WithIndexes(b *testing.B) {
	benchThroughputBatch(b, 100, true)
}

func benchThroughputBatch(b *testing.B, batchSize int, withIndexes bool) {
	ensureBenchDB(b)

	var md *RecordMetaData
	if withIndexes {
		md = throughputMetaDataWithAggregateIndexes(b)
	} else {
		md = throughputMetaDataPlain(b)
	}
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create store
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := int64(i * batchSize)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			for j := 0; j < batchSize; j++ {
				id := base + int64(j)
				order := &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(int32(id % 100)),
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batchSize), "records/op")
}

// --- Concurrent throughput benchmarks ---
// Measures: how does concurrency affect total throughput?
// This is the key benchmark for the loadWithSplit bottleneck.

func BenchmarkThroughputConcurrent1(b *testing.B) {
	benchThroughputConcurrent(b, 1, 50, false)
}

func BenchmarkThroughputConcurrent8(b *testing.B) {
	benchThroughputConcurrent(b, 8, 50, false)
}

func BenchmarkThroughputConcurrent32(b *testing.B) {
	benchThroughputConcurrent(b, 32, 50, false)
}

func BenchmarkThroughputConcurrent64(b *testing.B) {
	benchThroughputConcurrent(b, 64, 50, false)
}

func BenchmarkThroughputConcurrent128(b *testing.B) {
	benchThroughputConcurrent(b, 128, 50, false)
}

// With indexes
func BenchmarkThroughputConcurrent32WithIndexes(b *testing.B) {
	benchThroughputConcurrent(b, 32, 50, true)
}

func BenchmarkThroughputConcurrent64WithIndexes(b *testing.B) {
	benchThroughputConcurrent(b, 64, 50, true)
}

func BenchmarkThroughputConcurrent128WithIndexes(b *testing.B) {
	benchThroughputConcurrent(b, 128, 50, true)
}

func benchThroughputConcurrent(b *testing.B, concurrency, batchSize int, withIndexes bool) {
	ensureBenchDB(b)

	var md *RecordMetaData
	if withIndexes {
		md = throughputMetaDataWithAggregateIndexes(b)
	} else {
		md = throughputMetaDataPlain(b)
	}
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create store
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	var idGen atomic.Int64
	totalRecords := int64(b.N) * int64(batchSize)

	// Run b.N batches distributed across `concurrency` goroutines
	var wg sync.WaitGroup
	batchesPerWorker := b.N / concurrency
	if batchesPerWorker < 1 {
		batchesPerWorker = 1
	}
	remainingBatches := b.N

	for w := 0; w < concurrency && remainingBatches > 0; w++ {
		count := batchesPerWorker
		if count > remainingBatches {
			count = remainingBatches
		}
		remainingBatches -= count
		wg.Add(1)
		go func(count int) {
			defer wg.Done()
			for i := 0; i < count; i++ {
				base := idGen.Add(int64(batchSize))
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return nil, err
					}
					for j := 0; j < batchSize; j++ {
						id := base + int64(j)
						order := &gen.Order{
							OrderId: proto.Int64(id),
							Price:   proto.Int32(int32(id % 100)),
						}
						if _, err := store.SaveRecord(order); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				if err != nil {
					b.Error(err)
					return
				}
			}
		}(count)
	}

	wg.Wait()
	_ = totalRecords
	b.ReportMetric(float64(batchSize), "records/op")
	b.ReportMetric(float64(concurrency), "goroutines")
}

// --- Throughput with unique keys guaranteed (insert-only, no updates) ---
// This is the metrognome hot path: every record has a unique PK,
// loadWithSplit always returns nil. Measures the overhead of the
// existence check on pure inserts.

func BenchmarkThroughputInsertOnly50(b *testing.B) {
	benchThroughputInsertOnly(b, 50)
}

func BenchmarkThroughputInsertOnly100(b *testing.B) {
	benchThroughputInsertOnly(b, 100)
}

// --- Pipelined batch vs sequential comparison ---
// This is the key optimization: SaveRecordBatch pipelines existence checks.

func BenchmarkThroughputSequential50(b *testing.B) {
	benchThroughputSequentialVsBatch(b, 50, false)
}

func BenchmarkThroughputPipelined50(b *testing.B) {
	benchThroughputSequentialVsBatch(b, 50, true)
}

func BenchmarkThroughputSequential50WithIndexes(b *testing.B) {
	benchThroughputSequentialVsBatchWithIndexes(b, 50, false)
}

func BenchmarkThroughputPipelined50WithIndexes(b *testing.B) {
	benchThroughputSequentialVsBatchWithIndexes(b, 50, true)
}

func BenchmarkThroughputSequential100(b *testing.B) {
	benchThroughputSequentialVsBatch(b, 100, false)
}

func BenchmarkThroughputPipelined100(b *testing.B) {
	benchThroughputSequentialVsBatch(b, 100, true)
}

func benchThroughputSequentialVsBatch(b *testing.B, batchSize int, pipelined bool) {
	ensureBenchDB(b)
	md := throughputMetaDataPlain(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	var idGen atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := idGen.Add(int64(batchSize))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			records := make([]proto.Message, batchSize)
			for j := 0; j < batchSize; j++ {
				records[j] = benchOrder(base+int64(j), int32((base+int64(j))%100))
			}
			if pipelined {
				_, err = store.SaveRecordBatch(records)
			} else {
				for _, r := range records {
					if _, err = store.SaveRecord(r); err != nil {
						return nil, err
					}
				}
			}
			return nil, err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batchSize), "records/op")
}

func benchThroughputSequentialVsBatchWithIndexes(b *testing.B, batchSize int, pipelined bool) {
	ensureBenchDB(b)
	md := throughputMetaDataWithAggregateIndexes(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	var idGen atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := idGen.Add(int64(batchSize))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			records := make([]proto.Message, batchSize)
			for j := 0; j < batchSize; j++ {
				records[j] = benchOrder(base+int64(j), int32((base+int64(j))%100))
			}
			if pipelined {
				_, err = store.SaveRecordBatch(records)
			} else {
				for _, r := range records {
					if _, err = store.SaveRecord(r); err != nil {
						return nil, err
					}
				}
			}
			return nil, err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batchSize), "records/op")
}

func benchThroughputInsertOnly(b *testing.B, batchSize int) {
	ensureBenchDB(b)

	md := throughputMetaDataWithAggregateIndexes(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create store
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	// Use a unique ID space so every record is a fresh insert
	var idGen atomic.Int64

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := idGen.Add(int64(batchSize))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			for j := 0; j < batchSize; j++ {
				id := base + int64(j)
				order := &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(int32(id % 100)),
					Flower: &gen.Flower{
						Type:  proto.String(fmt.Sprintf("flower-%d", id)),
						Color: gen.Color_RED.Enum(),
					},
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batchSize), "records/op")
}

// --- InsertOnly batch (skips existence check round trip) ---

func BenchmarkThroughputInsertOnlyBatch50Plain(b *testing.B) {
	benchThroughputInsertOnlyBatch(b, 50, false)
}

func BenchmarkThroughputInsertOnlyBatch50WithIndexes(b *testing.B) {
	benchThroughputInsertOnlyBatch(b, 50, true)
}

func BenchmarkThroughputInsertOnlyBatch100WithIndexes(b *testing.B) {
	benchThroughputInsertOnlyBatch(b, 100, true)
}

func BenchmarkThroughputInsertBatch50WithIndexes(b *testing.B) {
	benchThroughputInsertBatch(b, 50)
}

func BenchmarkThroughputInsertBatch100WithIndexes(b *testing.B) {
	benchThroughputInsertBatch(b, 100)
}

func BenchmarkThroughputInsertBatch200WithIndexes(b *testing.B) {
	benchThroughputInsertBatch(b, 200)
}

func BenchmarkThroughputInsertBatch500WithIndexes(b *testing.B) {
	benchThroughputInsertBatch(b, 500)
}

func benchThroughputInsertOnlyBatch(b *testing.B, batchSize int, withIndexes bool) {
	ensureBenchDB(b)
	var md *RecordMetaData
	if withIndexes {
		md = throughputMetaDataWithAggregateIndexes(b)
	} else {
		md = throughputMetaDataPlain(b)
	}
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	var idGen atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := idGen.Add(int64(batchSize))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			records := make([]proto.Message, batchSize)
			for j := 0; j < batchSize; j++ {
				records[j] = benchOrder(base+int64(j), int32((base+int64(j))%100))
			}
			_, err = store.SaveRecordBatchInsertOnly(records)
			return nil, err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batchSize), "records/op")
}

func benchThroughputInsertBatch(b *testing.B, batchSize int) {
	ensureBenchDB(b)
	md := throughputMetaDataWithAggregateIndexes(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		// Raise tx timeout to 30 s — under container load a high-
		// batch-size CreateOrOpen can exceed FDB's 5 s default while
		// scanning the (by now non-trivial) subspace for the store
		// header.
		if err := rtx.tx.Options().SetTimeout(30_000); err != nil {
			return nil, err
		}
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	var idGen atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := idGen.Add(int64(batchSize))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			if err := rtx.tx.Options().SetTimeout(30_000); err != nil {
				return nil, err
			}
			// Use Build() instead of Open() — skips store state FDB reads.
			// Safe for InsertBatch: storeHeader=nil → no lock check,
			// indexStates=nil → all indexes READABLE.
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Build()
			if err != nil {
				return nil, err
			}
			records := make([]proto.Message, batchSize)
			for j := 0; j < batchSize; j++ {
				records[j] = benchOrder(base+int64(j), int32((base+int64(j))%100))
			}
			return nil, store.InsertBatch(records)
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batchSize), "records/op")
}

func BenchmarkThroughputInsertBatchConcurrent8(b *testing.B) {
	benchThroughputInsertBatchConcurrent(b, 50, 8)
}

func BenchmarkThroughputInsertBatchConcurrent32(b *testing.B) {
	benchThroughputInsertBatchConcurrent(b, 50, 32)
}

func BenchmarkThroughputInsertBatchConcurrent128(b *testing.B) {
	benchThroughputInsertBatchConcurrent(b, 50, 128)
}

func benchThroughputInsertBatchConcurrent(b *testing.B, batchSize, goroutines int) {
	ensureBenchDB(b)
	md := throughputMetaDataWithAggregateIndexes(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		if err := rtx.tx.Options().SetTimeout(30_000); err != nil {
			return nil, err
		}
		_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatal(err)
	}

	var idGen atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func() {
				defer wg.Done()
				base := idGen.Add(int64(batchSize))
				_, runErr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					// Raise the per-tx timeout from FDB's 5 s default to
					// 30 s so the 128-goroutine variant doesn't flake under
					// container load. A single batch insert shouldn't take
					// anywhere near 30 s; the longer ceiling just absorbs
					// queueing delay when many concurrent txs contend.
					if err := rtx.tx.Options().SetTimeout(30_000); err != nil {
						return nil, err
					}
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return nil, err
					}
					records := make([]proto.Message, batchSize)
					for j := 0; j < batchSize; j++ {
						records[j] = benchOrder(base+int64(j), int32((base+int64(j))%100))
					}
					return nil, store.InsertBatch(records)
				})
				if runErr != nil {
					b.Error(runErr)
				}
			}()
		}
		wg.Wait()
	}
	b.ReportMetric(float64(goroutines), "goroutines")
	b.ReportMetric(float64(batchSize*goroutines), "records/op")
}

func BenchmarkThroughputInsertBatchConcurrent16(b *testing.B) {
	benchThroughputInsertBatchConcurrent(b, 50, 16)
}
