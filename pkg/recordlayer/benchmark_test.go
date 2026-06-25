package recordlayer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var benchDBOnce sync.Once

// ensureBenchDB initializes sharedDB if Ginkgo's SynchronizedBeforeSuite hasn't
// run (e.g., when running benchmarks standalone). Starts its own FDB testcontainer.
func ensureBenchDB(b *testing.B) {
	b.Helper()
	benchDBOnce.Do(func() {
		if sharedDB != nil {
			return
		}
		ctx := context.Background()
		container, err := foundationdbtc.Run(ctx, "",
			foundationdbtc.WithAPIVersion(730),
		)
		if err != nil {
			b.Fatalf("failed to start FDB container: %v", err)
		}
		clusterFile, err := container.ClusterFile(ctx)
		if err != nil {
			b.Fatalf("failed to get cluster file: %v", err)
		}
		tmpFile, err := os.CreateTemp("", "fdb_bench_*.txt")
		if err != nil {
			b.Fatalf("failed to create temp file: %v", err)
		}
		tmpFile.WriteString(clusterFile)
		tmpFile.Close()
		fdb.MustAPIVersion(730)
		dbConn, err := fdb.OpenDatabase(tmpFile.Name())
		if err != nil {
			b.Fatalf("failed to open FDB: %v", err)
		}
		sharedDB = NewFDBDatabase(dbConn)
	})
	if sharedDB == nil {
		b.Fatal("sharedDB initialization failed")
	}
}

// benchSubspace returns a unique subspace for the given benchmark, ensuring
// isolation across concurrent benchmarks. Uses b.Name() as the key prefix.
// This is separate from specSubspace() which relies on Ginkgo's CurrentSpecReport.
func benchSubspace(b *testing.B) subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{b.Name()}.Pack())
}

// benchMetaData builds metadata with Order/Customer/TypedRecord primary keys.
func benchMetaData(b *testing.B) *RecordMetaData {
	b.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("failed to build metadata: %v", err)
	}
	return md
}

// benchMetaDataWithValueIndex builds metadata with a VALUE index on price.
func benchMetaDataWithValueIndex(b *testing.B) *RecordMetaData {
	b.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewIndex("price_idx", Field("price")))
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("failed to build metadata: %v", err)
	}
	return md
}

// benchMetaDataWithCountKey builds metadata with ungrouped record counting enabled.
func benchMetaDataWithCountKey(b *testing.B) *RecordMetaData {
	b.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.SetRecordCountKey(EmptyKey())
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("failed to build metadata: %v", err)
	}
	return md
}

// benchMetaDataSplit builds metadata with split long records enabled.
func benchMetaDataSplit(b *testing.B) *RecordMetaData {
	b.Helper()
	builder := NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto).
		SetSplitLongRecords(true)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("failed to build metadata: %v", err)
	}
	return md
}

// benchOrder creates a simple Order record for benchmarking.
func benchOrder(id int64, price int32) *gen.Order {
	return &gen.Order{
		OrderId: proto.Int64(id),
		Price:   proto.Int32(price),
		Flower: &gen.Flower{
			Type:  proto.String("Rose"),
			Color: gen.Color_RED.Enum(),
		},
	}
}

// BenchmarkSaveRecord measures the cost of saving a single Order record in a
// fresh store, including transaction commit.
func BenchmarkSaveRecord(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(benchOrder(int64(i), 100))
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkSaveRecordBuild measures SaveRecord with Build() instead of Open().
// Build() skips the store state read — state is lazy-loaded on first index
// operation. This matches Java's build() + preloadRecordStoreStateAsync().
func BenchmarkSaveRecordBuild(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				SetAssumeAllIndexesReadable(true).
				Build()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(benchOrder(int64(i), 100))
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkLoadRecord measures the cost of loading a previously saved Order by
// primary key, including transaction overhead.
func BenchmarkLoadRecord(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store and save a record to load.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return store.SaveRecord(benchOrder(1, 100))
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	pk := tuple.Tuple{int64(1)}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			return store.LoadRecord(pk)
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkLoadRecordBuild measures LoadRecord with Build() instead of Open().
// Build() skips the store state read — no lazy load needed for reads.
func BenchmarkLoadRecordBuild(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store and save a record to load.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return store.SaveRecord(benchOrder(1, 100))
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	pk := tuple.Tuple{int64(1)}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				SetAssumeAllIndexesReadable(true).
				Build()
			if err != nil {
				return nil, err
			}
			return store.LoadRecord(pk)
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkScanRecords measures scanning 100 records with a forward scan,
// including cursor iteration and deserialization.
func BenchmarkScanRecords(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-populate 100 records.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 100; i++ {
			if _, err := store.SaveRecord(benchOrder(i, int32(i*10))); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			if len(records) != 100 {
				return nil, fmt.Errorf("expected 100 records, got %d", len(records))
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkSaveRecordWithIndex measures saving an Order with a VALUE index on
// the price field.
func BenchmarkSaveRecordWithIndex(b *testing.B) {
	ensureBenchDB(b)

	priceIndex := NewIndex("bench_price", Field("price"))
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", priceIndex)
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("metadata: %v", err)
	}

	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store.
	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(benchOrder(int64(i), int32(i*10)))
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkScanIndex measures scanning all entries from a VALUE index.
func BenchmarkScanIndex(b *testing.B) {
	ensureBenchDB(b)

	priceIndex := NewIndex("bench_price_scan", Field("price"))
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", priceIndex)
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("metadata: %v", err)
	}

	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-populate 100 records with the index.
	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 100; i++ {
			if _, err := store.SaveRecord(benchOrder(i, int32(i*10))); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			if len(entries) != 100 {
				return nil, fmt.Errorf("expected 100 index entries, got %d", len(entries))
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkSaveRecordWithMultipleIndexes measures saving an Order with VALUE +
// COUNT + SUM indexes — the most expensive common write path.
func BenchmarkSaveRecordWithMultipleIndexes(b *testing.B) {
	ensureBenchDB(b)

	priceIndex := NewIndex("bench_multi_price", Field("price"))
	countIndex := NewCountIndex("bench_multi_count", GroupAll(Field("price")))
	sumIndex := NewSumIndex("bench_multi_sum", GroupAll(Field("price")))

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", priceIndex)
	builder.AddIndex("Order", countIndex)
	builder.AddIndex("Order", sumIndex)
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("metadata: %v", err)
	}

	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store.
	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(benchOrder(int64(i), int32(i%100)))
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkGetRecordCount measures reading the atomic record count.
func BenchmarkGetRecordCount(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaDataWithCountKey(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create store and save some records so the count is non-zero.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 50; i++ {
			if _, err := store.SaveRecord(benchOrder(i, 100)); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			if count != 50 {
				return nil, fmt.Errorf("expected count 50, got %d", count)
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkSaveLargeRecord measures saving a ~50KB record (below the 100KB split
// threshold), stressing serialization and FDB value write.
func BenchmarkSaveLargeRecord(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	// Build a 50KB payload once and reuse it.
	padding := strings.Repeat("X", 50_000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			order := &gen.Order{
				OrderId: proto.Int64(int64(i)),
				Price:   proto.Int32(42),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
			}
			return store.SaveRecord(order)
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkSaveSplitRecord measures saving a ~250KB record that triggers the
// split record path (3 x 100KB chunks). Includes serialize, split into 3
// chunks, and commit.
func BenchmarkSaveSplitRecord(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaDataSplit(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-create the store.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	// Build a 250KB payload once and reuse it.
	padding := strings.Repeat("X", 250_000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			order := &gen.Order{
				OrderId: proto.Int64(int64(i)),
				Price:   proto.Int32(42),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
			}
			return store.SaveRecord(order)
		})
		if err != nil {
			b.Fatalf("split-save %d: %v", i, err)
		}
	}
}

// BenchmarkStoreOpen measures the cost of opening an existing store in a new
// transaction. This is the hot path for every request in a typical application.
func BenchmarkStoreOpen(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		})
		if err != nil {
			b.Fatalf("open %d: %v", i, err)
		}
	}
}

// BenchmarkStoreOpenCached measures store open with state caching enabled.
func BenchmarkStoreOpenCached(b *testing.B) {
	ensureBenchDB(b)

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("metadata: %v", err)
	}

	ss := benchSubspace(b)
	ctx := context.Background()

	cache := NewMetaDataVersionStampStoreStateCache()
	sharedDB.SetStoreStateCache(cache)
	defer sharedDB.SetStoreStateCache(PassThroughStoreStateCache())

	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SetStateCacheability(true)
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		})
		if err != nil {
			b.Fatalf("open-cached %d: %v", i, err)
		}
	}
}

// BenchmarkDeleteRecord measures the cost of deleting a single record by PK.
func BenchmarkDeleteRecord(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(0); i < int64(b.N)+100; i++ {
			if _, err := store.SaveRecord(benchOrder(i, int32(i))); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			return store.DeleteRecord(tuple.Tuple{int64(i)})
		})
		if err != nil {
			b.Fatalf("delete %d: %v", i, err)
		}
	}
}

// BenchmarkSaveRecordWithCountAndIndex measures saving with both record counting
// and a VALUE index — the common production configuration.
func BenchmarkSaveRecordWithCountAndIndex(b *testing.B) {
	ensureBenchDB(b)

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.SetRecordCountKey(EmptyKey())
	builder.AddIndex("Order", NewIndex("Order$price", Field("price")))
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("metadata: %v", err)
	}

	ss := benchSubspace(b)
	ctx := context.Background()

	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(benchOrder(int64(i), int32(i%1000)))
		})
		if err != nil {
			b.Fatalf("save %d: %v", i, err)
		}
	}
}

// BenchmarkSaveRecordBatch measures saving 10 records in a single transaction.
// This is a common real-world pattern: batch inserts amortize tx overhead.
func BenchmarkSaveRecordBatch(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaDataWithValueIndex(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		_, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := int64(i * 10)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			for j := int64(0); j < 10; j++ {
				if _, err := store.SaveRecord(benchOrder(base+j, int32((base+j)%100))); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("batch %d: %v", i, err)
		}
	}
}

// BenchmarkScanWithContinuation measures paged scanning with continuation tokens.
// Scans 100 records in pages of 10, resuming from continuation each time.
func BenchmarkScanWithContinuation(b *testing.B) {
	ensureBenchDB(b)

	md := benchMetaData(b)
	ss := benchSubspace(b)
	ctx := context.Background()

	// Pre-populate with 100 records.
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 100; i++ {
			if _, err := store.SaveRecord(benchOrder(i, int32(i%50))); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var continuation []byte
		totalScanned := 0
		for {
			result, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor := store.ScanRecords(continuation, ScanProperties{
					ExecuteProperties: ExecuteProperties{
						ReturnedRowLimit: 10,
					},
					CursorStreamingMode: StreamingModeWantAll,
				})
				records, cont, err := AsListWithContinuation(ctx, cursor)
				if err != nil {
					return nil, err
				}
				return []any{len(records), cont}, nil
			})
			if err != nil {
				b.Fatalf("scan page: %v", err)
			}
			page := result.([]any)
			pageCount := page[0].(int)
			pageCont := page[1].([]byte)
			totalScanned += pageCount
			continuation = pageCont
			if len(continuation) == 0 || pageCount == 0 {
				break
			}
		}
		if totalScanned != 100 {
			b.Fatalf("expected 100 records, got %d", totalScanned)
		}
	}
}
