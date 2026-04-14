// Command loadtest-raw writes usage events directly to FDB using raw atomic
// mutations — no Record Layer, no proto marshal, no index maintenance overhead.
// Pure FDB throughput test for the aggregation pattern.
//
// Layout:
//
//	Record:    [ss, "r", customer, region, model, bucket, eventID] → value (little-endian int64)
//	SUM index: [ss, "s", customer, region, model, bucket] → atomic ADD
//	COUNT:     [ss, "c", customer, region, model, bucket] → atomic ADD
//
// Run: bazelisk run //examples/metrognome/cmd/loadtest-raw -- -events 50000000
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

func main() {
	totalEvents := flag.Int("events", 50_000_000, "total events")
	batchSize := flag.Int("batch", 500, "events per FDB transaction")
	numWorkers := flag.Int("workers", 0, "concurrent workers (0 = 8x CPUs)")
	doVerify := flag.Bool("verify", true, "verify totals after ingest")
	flag.Parse()

	if *numWorkers == 0 {
		*numWorkers = runtime.NumCPU() * 8
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	clusterFile := os.Getenv("FDB_CLUSTER_FILE")
	fdb.MustAPIVersion(720)
	var fdbDB fdb.Database
	var err error
	if clusterFile != "" {
		fdbDB, err = fdb.OpenDatabase(clusterFile)
	} else {
		fdbDB = fdb.MustOpenDefault()
	}
	if err != nil {
		slog.Error("open fdb", "error", err)
		os.Exit(1)
	}

	ss := subspace.Sub("loadraw")
	sumSS := ss.Sub("s")
	countSS := ss.Sub("c")

	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-south-1"}
	models := []string{"gpt-4", "claude-4", "llama-3", "gemini-2"}
	customers := []string{"lc-1", "lc-2", "lc-3", "lc-4", "lc-5"}
	bucket := int64(1800000000000)

	valueLE := make([]byte, 8)
	binary.LittleEndian.PutUint64(valueLE, 1)
	oneLE := valueLE

	slog.Info("raw FDB load test",
		"events", *totalEvents,
		"batch", *batchSize,
		"workers", *numWorkers,
		"cpus", runtime.NumCPU(),
	)

	type workItem struct {
		startIdx int
		count    int
	}
	work := make(chan workItem, *numWorkers*4)

	var ingested atomic.Int64
	var txErrors atomic.Int64
	var wg sync.WaitGroup

	// Track expected per-customer totals
	customerExpected := make([]atomic.Int64, len(customers))

	start := time.Now()

	// Progress
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				n := ingested.Load()
				elapsed := time.Since(start).Seconds()
				slog.Info("progress",
					"events", n,
					"pct", fmt.Sprintf("%.1f%%", float64(n)/float64(*totalEvents)*100),
					"rate", fmt.Sprintf("%.0f/s", float64(n)/elapsed),
					"errors", txErrors.Load(),
				)
			}
		}
	}()

	// Workers
	var idGen atomic.Uint64
	for w := 0; w < *numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				_, err := fdbDB.Transact(func(tx fdb.Transaction) (any, error) {
					for i := 0; i < item.count; i++ {
						idx := item.startIdx + i
						custIdx := idx % len(customers)
						regionIdx := (idx / len(customers)) % len(regions)
						modelIdx := (idx / (len(customers) * len(regions))) % len(models)

						cust := customers[custIdx]
						region := regions[regionIdx]
						model := models[modelIdx]

						// Atomic ADD to SUM key
						sumKey := sumSS.Pack(tuple.Tuple{cust, region, model, bucket})
						tx.Add(fdb.Key(sumKey), oneLE)

						// Atomic ADD to COUNT key
						countKey := countSS.Pack(tuple.Tuple{cust, region, model, bucket})
						tx.Add(fdb.Key(countKey), oneLE)

						// Write event record (optional — skip for pure agg test)
						evtID := strconv.FormatUint(idGen.Add(1), 36)
						_ = evtID // skip record write for max throughput

						customerExpected[custIdx].Add(1)
					}
					return nil, nil
				})
				if err != nil {
					txErrors.Add(1)
					// Roll back expected counts
					for i := 0; i < item.count; i++ {
						idx := item.startIdx + i
						custIdx := idx % len(customers)
						customerExpected[custIdx].Add(-1)
					}
				} else {
					ingested.Add(int64(item.count))
				}
			}
		}()
	}

	// Feed work
	for i := 0; i < *totalEvents; i += *batchSize {
		count := *batchSize
		if i+count > *totalEvents {
			count = *totalEvents - i
		}
		work <- workItem{startIdx: i, count: count}
	}
	close(work)
	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	rate := float64(ingested.Load()) / elapsed.Seconds()
	slog.Info("INGEST COMPLETE",
		"events", ingested.Load(),
		"errors", txErrors.Load(),
		"elapsed", elapsed.Round(time.Millisecond),
		"rate", fmt.Sprintf("%.0f events/sec", rate),
	)

	if !*doVerify {
		return
	}

	slog.Info("verifying...")
	var verifyFail int
	for i, cust := range customers {
		exp := customerExpected[i].Load()

		// Read SUM across all regions/models for this customer
		rangeStart := sumSS.Pack(tuple.Tuple{cust})
		rangeEnd := sumSS.Pack(tuple.Tuple{cust + "\x00"}) // strinc

		result, err := fdbDB.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
			kvs := rtx.GetRange(fdb.KeyRange{Begin: fdb.Key(rangeStart), End: fdb.Key(rangeEnd)}, fdb.RangeOptions{}).GetSliceOrPanic()
			var total int64
			for _, kv := range kvs {
				if len(kv.Value) == 8 {
					total += int64(binary.LittleEndian.Uint64(kv.Value))
				}
			}
			return total, nil
		})
		if err != nil {
			slog.Error("verify error", "customer", cust, "error", err)
			verifyFail++
			continue
		}
		actual := result.(int64)
		if actual != exp {
			slog.Error("DATA LOSS", "customer", cust, "expected", exp, "actual", actual, "lost", exp-actual)
			verifyFail++
		} else {
			slog.Info("OK", "customer", cust, "events", actual)
		}
	}

	if verifyFail == 0 {
		slog.Info("VERIFICATION PASSED — ZERO DATA LOSS", "events", ingested.Load())
	} else {
		slog.Error("VERIFICATION FAILED", "errors", verifyFail)
		os.Exit(1)
	}
}
