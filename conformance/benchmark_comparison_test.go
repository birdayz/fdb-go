package conformance_test

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// benchResult holds timing for one benchmark.
type benchResult struct {
	Name       string
	GoNanos    int64
	JavaNanos  int64
	Iterations int64
}

func (r benchResult) GoAvgUs() float64   { return float64(r.GoNanos) / float64(r.Iterations) / 1000 }
func (r benchResult) JavaAvgUs() float64 { return float64(r.JavaNanos) / float64(r.Iterations) / 1000 }
func (r benchResult) Ratio() float64     { return r.GoAvgUs() / r.JavaAvgUs() }

var _ = Describe("Performance Comparison: Go vs Java", Label("benchmark"), func() {
	var (
		ctx         context.Context
		java        *JavaInvoker
		clusterFile string
		goRecordDB  *recordlayer.FDBDatabase
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()

		cf, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFile = cf

		goRecordDB = recordlayer.NewFDBDatabase(sharedDB)
	})

	// benchOrder creates a simple Order.
	benchOrder := func(id int64, price int32) *gen.Order {
		return &gen.Order{
			OrderId: proto.Int64(id),
			Price:   proto.Int32(price),
			Flower: &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			},
		}
	}

	const warmup = 20 // warmup iterations (discarded) — matches Java's WARMUP

	// runGoSave measures Go save performance.
	runGoSave := func(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
		// Create store.
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			_, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Warmup.
		for w := int64(0); w < warmup; w++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				return store.SaveRecord(benchOrder(-(w + 1), 100))
			})
			Expect(err).NotTo(HaveOccurred())
		}

		start := time.Now()
		for i := int64(0); i < iterations; i++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				return store.SaveRecord(benchOrder(i, 100))
			})
			Expect(err).NotTo(HaveOccurred())
		}
		return time.Since(start).Nanoseconds()
	}

	// runGoLoad measures Go load performance.
	runGoLoad := func(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
		// Setup: save one record to load.
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(benchOrder(1, 100))
		})
		Expect(err).NotTo(HaveOccurred())

		pk := tuple.Tuple{int64(1)}

		// Warmup.
		for w := 0; w < warmup; w++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				return store.LoadRecord(pk)
			})
			Expect(err).NotTo(HaveOccurred())
		}

		start := time.Now()
		for i := int64(0); i < iterations; i++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				return store.LoadRecord(pk)
			})
			Expect(err).NotTo(HaveOccurred())
		}
		return time.Since(start).Nanoseconds()
	}

	// runGoScan measures Go scan performance (100 records).
	runGoScan := func(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
		// Setup: save 100 records.
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for j := int64(1); j <= 100; j++ {
				if _, err := store.SaveRecord(benchOrder(j, int32(j*10))); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Warmup.
		for w := 0; w < warmup; w++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				_, err = recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}

		start := time.Now()
		for i := int64(0); i < iterations; i++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
				if err != nil {
					return nil, err
				}
				if len(records) != 100 {
					return nil, fmt.Errorf("expected 100 records, got %d", len(records))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		return time.Since(start).Nanoseconds()
	}

	// runGoStoreOpen measures Go store open performance.
	runGoStoreOpen := func(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
		// Setup: create store.
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			_, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Warmup.
		for w := 0; w < warmup; w++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}

		start := time.Now()
		for i := int64(0); i < iterations; i++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
		return time.Since(start).Nanoseconds()
	}

	// runGoDelete measures Go delete performance.
	runGoDelete := func(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
		// Setup: pre-populate warmup + measured records.
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for j := int64(-warmup); j < iterations; j++ {
				if _, err := store.SaveRecord(benchOrder(j, 100)); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Warmup.
		for w := int64(0); w < warmup; w++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				_, err = store.DeleteRecord(tuple.Tuple{-warmup + w})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}

		start := time.Now()
		for i := int64(0); i < iterations; i++ {
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				_, err = store.DeleteRecord(tuple.Tuple{i})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
		return time.Since(start).Nanoseconds()
	}

	// invokeJavaBenchmark calls a Java benchmark step and extracts timing.
	invokeJavaBenchmark := func(step string, ss subspace.Subspace, iterations int64) int64 {
		var result struct {
			Iterations int64 `json:"iterations"`
			TotalNanos int64 `json:"totalNanos"`
			AvgNanos   int64 `json:"avgNanos"`
		}
		err := java.InvokeAs(ctx, step, map[string]any{
			"clusterFile": clusterFile,
			"subspace":    BytesToIntArray(ss.Bytes()),
			"iterations":  iterations,
		}, &result)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Iterations).To(Equal(iterations))
		return result.TotalNanos
	}

	// buildBasicMD creates metadata without indexes (matching Java's benchMetaData).
	buildBasicMD := func() *recordlayer.RecordMetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// buildIndexedMD creates metadata with a VALUE index on price (matching Java's benchMetaDataWithIndex).
	buildIndexedMD := func() *recordlayer.RecordMetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", recordlayer.NewIndex("bench_price", recordlayer.Field("price")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// uniqueSS returns a unique subspace for each call.
	uniqueSS := func() subspace.Subspace {
		return subspace.FromBytes(tuple.Tuple{uuid.New().String()}.Pack())
	}

	It("compares Go and Java Record Layer performance", func() {
		const N int64 = 100 // iterations per benchmark (20 warmup + 100 measured on each side)

		var results []benchResult

		// 1. SaveRecord
		By("benchmarking SaveRecord")
		goSS, javaSS := uniqueSS(), uniqueSS()
		md := buildBasicMD()
		goNanos := runGoSave(md, goSS, N)
		javaNanos := invokeJavaBenchmark("benchmarkSaveRecord", javaSS, N)
		results = append(results, benchResult{"SaveRecord", goNanos, javaNanos, N})

		// 2. LoadRecord
		By("benchmarking LoadRecord")
		goSS, javaSS = uniqueSS(), uniqueSS()
		goNanos = runGoLoad(md, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkLoadRecord", javaSS, N)
		results = append(results, benchResult{"LoadRecord", goNanos, javaNanos, N})

		// 3. ScanRecords (100)
		By("benchmarking ScanRecords (100 records)")
		goSS, javaSS = uniqueSS(), uniqueSS()
		goNanos = runGoScan(md, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkScanRecords", javaSS, N)
		results = append(results, benchResult{"ScanRecords(100)", goNanos, javaNanos, N})

		// 4. SaveRecordWithIndex
		By("benchmarking SaveRecordWithIndex")
		goSS, javaSS = uniqueSS(), uniqueSS()
		idxMD := buildIndexedMD()
		goNanos = runGoSaveWithIndex(idxMD, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkSaveRecordWithIndex", javaSS, N)
		results = append(results, benchResult{"SaveRecordWithIndex", goNanos, javaNanos, N})

		// 5. ScanIndex (100)
		By("benchmarking ScanIndex (100 entries)")
		goSS, javaSS = uniqueSS(), uniqueSS()
		goNanos = runGoScanIndex(idxMD, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkScanIndex", javaSS, N)
		results = append(results, benchResult{"ScanIndex(100)", goNanos, javaNanos, N})

		// 6. DeleteRecord
		By("benchmarking DeleteRecord")
		goSS, javaSS = uniqueSS(), uniqueSS()
		goNanos = runGoDelete(md, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkDeleteRecord", javaSS, N)
		results = append(results, benchResult{"DeleteRecord", goNanos, javaNanos, N})

		// 7. StoreOpen
		By("benchmarking StoreOpen")
		goSS, javaSS = uniqueSS(), uniqueSS()
		goNanos = runGoStoreOpen(md, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkStoreOpen", javaSS, N)
		results = append(results, benchResult{"StoreOpen", goNanos, javaNanos, N})

		// 8. SaveRecordBatch (10/tx)
		By("benchmarking SaveRecordBatch (10/tx)")
		goSS, javaSS = uniqueSS(), uniqueSS()
		goNanos = runGoSaveBatch(idxMD, goSS, N)
		javaNanos = invokeJavaBenchmark("benchmarkSaveRecordBatch", javaSS, N)
		results = append(results, benchResult{"SaveBatch(10/tx)", goNanos, javaNanos, N})

		// Print comparison table.
		GinkgoWriter.Println("\n=== Go vs Java Record Layer Performance ===")
		GinkgoWriter.Println(fmt.Sprintf("%-25s %12s %12s %8s", "Operation", "Go (us/op)", "Java (us/op)", "Ratio"))
		GinkgoWriter.Println(fmt.Sprintf("%-25s %12s %12s %8s", "---------", "----------", "-----------", "-----"))
		for _, r := range results {
			marker := ""
			if r.Ratio() < 1.0 {
				marker = " <-- Go wins"
			}
			GinkgoWriter.Println(fmt.Sprintf("%-25s %12.0f %12.0f %7.2fx%s",
				r.Name, r.GoAvgUs(), r.JavaAvgUs(), r.Ratio(), marker))
		}
		GinkgoWriter.Println(fmt.Sprintf("\nIterations per benchmark: %d (+ 20 warmup, discarded)", N))
		GinkgoWriter.Println("Both use same FDB container, same machine, sequential execution")
		GinkgoWriter.Println("Go: pure Go FDB client (no CGo) | Java: FDB C binding (CGo)")
		GinkgoWriter.Println("Ratio < 1.0 = Go faster, > 1.0 = Java faster")
	})

	It("compares bulk insert throughput: Go vs Java", Label("bulk-insert"), func() {
		const totalRecords = 500_000
		const batchSize = 2000
		workers := []int{1, 4, 8}

		type bulkResult struct {
			TotalRecords float64 `json:"totalRecords"`
			TotalNanos   float64 `json:"totalNanos"`
			RowsPerSec   float64 `json:"rowsPerSecond"`
			Workers      float64 `json:"workers"`
			Error        string  `json:"error"`
		}

		invokeBulk := func(step string, ss subspace.Subspace, w int) bulkResult {
			var result bulkResult
			err := java.InvokeAs(ctx, step, map[string]any{
				"clusterFile":  clusterFile,
				"subspace":     BytesToIntArray(ss.Bytes()),
				"totalRecords": totalRecords,
				"batchSize":    batchSize,
				"workers":      w,
			}, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Error).To(BeEmpty(), "Java benchmark error")
			return result
		}

		GinkgoWriter.Println("\n=== Bulk Insert: Java saveRecord (sync) vs saveRecordAsync (pipelined) ===")
		GinkgoWriter.Println(fmt.Sprintf("%-6s %18s %18s", "Workers", "Sync (rows/s)", "Async (rows/s)"))
		GinkgoWriter.Println(fmt.Sprintf("%-6s %18s %18s", "-------", "-------------", "--------------"))

		for _, w := range workers {
			syncSS := uniqueSS()
			syncResult := invokeBulk("benchmarkBulkInsertSync", syncSS, w)

			asyncSS := uniqueSS()
			asyncResult := invokeBulk("benchmarkBulkInsertAsync", asyncSS, w)

			GinkgoWriter.Println(fmt.Sprintf("w%-5d %18.0f %18.0f",
				w, syncResult.RowsPerSec, asyncResult.RowsPerSec))
		}

		GinkgoWriter.Println(fmt.Sprintf("\n%d records, %d per batch, no secondary indexes", totalRecords, batchSize))
		GinkgoWriter.Println("Sync = saveRecord() per row (sequential reads)")
		GinkgoWriter.Println("Async = saveRecordAsync() + allOf().join() (pipelined reads)")
	})
})

// runGoSaveWithIndex measures Go save with VALUE index.
func runGoSaveWithIndex(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
	ctx := context.Background()
	goRecordDB := recordlayer.NewFDBDatabase(sharedDB)

	_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	Expect(err).NotTo(HaveOccurred())

	start := time.Now()
	for i := int64(0); i < iterations; i++ {
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(i),
				Price:   proto.Int32(100),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			})
		})
		Expect(err).NotTo(HaveOccurred())
	}
	return time.Since(start).Nanoseconds()
}

// runGoScanIndex measures Go index scan (100 entries).
func runGoScanIndex(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
	ctx := context.Background()
	goRecordDB := recordlayer.NewFDBDatabase(sharedDB)

	// Setup: save 100 indexed records.
	_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for j := int64(1); j <= 100; j++ {
			if _, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(j),
				Price:   proto.Int32(int32(j * 10)),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())

	start := time.Now()
	for i := int64(0); i < iterations; i++ {
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			entries, err := recordlayer.AsList(ctx, store.ScanIndex(store.GetRecordMetaData().GetIndex("bench_price"), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
			if err != nil {
				return nil, err
			}
			if len(entries) != 100 {
				return nil, fmt.Errorf("expected 100 index entries, got %d", len(entries))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	return time.Since(start).Nanoseconds()
}

// runGoSaveBatch measures Go batch save (10 records per tx with VALUE index).
func runGoSaveBatch(md *recordlayer.RecordMetaData, ss subspace.Subspace, iterations int64) int64 {
	ctx := context.Background()
	goRecordDB := recordlayer.NewFDBDatabase(sharedDB)

	_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	Expect(err).NotTo(HaveOccurred())

	start := time.Now()
	for i := int64(0); i < iterations; i++ {
		batch := i
		_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
				Open()
			if err != nil {
				return nil, err
			}
			for j := 0; j < 10; j++ {
				if _, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(batch*10 + int64(j)),
					Price:   proto.Int32(int32(100 + j)),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	return time.Since(start).Nanoseconds()
}
