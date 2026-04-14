// Command loadtest hammers FDB with usage events via the dynamic meter engine.
// Uses multiple FDB client connections for parallel network I/O.
// Run: bazelisk run //examples/metrognome/cmd/loadtest -- -events 50000000 -clients 24
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func main() {
	totalEvents := flag.Int("events", 1_000_000, "total events")
	batchSize := flag.Int("batch", 50, "events per FDB transaction")
	workersPerClient := flag.Int("workers", 16, "worker goroutines per FDB client connection")
	numClients := flag.Int("clients", 0, "FDB client connections (0 = NumCPU)")
	meterSlug := flag.String("meter", "load_api_calls", "meter slug")
	doVerify := flag.Bool("verify", true, "verify totals after ingest")
	flag.Parse()

	if *numClients == 0 {
		*numClients = runtime.NumCPU()
	}
	totalWorkers := *numClients * *workersPerClient

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// pprof server
	go func() {
		slog.Info("pprof on :6060")
		http.ListenAndServe(":6060", nil)
	}()

	clusterFile := os.Getenv("FDB_CLUSTER_FILE")
	fdb.MustAPIVersion(720)

	// Open N separate FDB connections — each gets its own network goroutine.
	// All engines share the same meter registration (proto type registered once).
	slog.Info("opening FDB connections...", "count", *numClients)
	engines := make([]*meter.Engine, *numClients)

	// Register the meter on the first engine only (proto types are global singletons)
	{
		var db fdb.Database
		var err error
		if clusterFile != "" {
			db, err = fdb.OpenDatabase(clusterFile)
		} else {
			db = fdb.MustOpenDefault()
		}
		if err != nil {
			slog.Error("open fdb", "error", err)
			os.Exit(1)
		}
		recordDB := rl.NewFDBDatabase(db)
		eng := meter.NewEngine(recordDB, subspace.Sub("loadtest"))
		err = eng.Register(&storev1.Meter{
			Id:                proto.String("load-m1"),
			Slug:              proto.String(*meterSlug),
			AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			GroupByProperties: []string{"region", "model"},
		})
		if err != nil {
			slog.Error("register meter", "error", err)
			os.Exit(1)
		}
		engines[0] = eng
	}

	// Open remaining connections and register meter (idempotent — same slug skips)
	for i := 1; i < *numClients; i++ {
		var db fdb.Database
		var err error
		if clusterFile != "" {
			db, err = fdb.OpenDatabase(clusterFile)
		} else {
			db = fdb.MustOpenDefault()
		}
		if err != nil {
			slog.Error("open fdb", "client", i, "error", err)
			os.Exit(1)
		}
		recordDB := rl.NewFDBDatabase(db)
		eng := meter.NewEngine(recordDB, subspace.Sub("loadtest"))
		// Register is idempotent — will skip since slug already registered
		_ = eng.Register(&storev1.Meter{
			Id:                proto.String("load-m1"),
			Slug:              proto.String(*meterSlug),
			AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			GroupByProperties: []string{"region", "model"},
		})
		engines[i] = eng
	}

	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-south-1"}
	models := []string{"gpt-4", "claude-4", "llama-3", "gemini-2"}
	customers := []string{"lc-1", "lc-2", "lc-3", "lc-4", "lc-5"}

	slog.Info("load test",
		"events", *totalEvents,
		"batch", *batchSize,
		"clients", *numClients,
		"workers_per_client", *workersPerClient,
		"total_workers", totalWorkers,
		"cpus", runtime.NumCPU(),
	)

	type workItem struct {
		startIdx int
		count    int
	}
	work := make(chan workItem, totalWorkers*4)

	var ingested atomic.Int64
	var txErrors atomic.Int64
	var wg sync.WaitGroup

	customerExpected := make([]atomic.Int64, len(customers))

	ctx := context.Background()
	start := time.Now()
	bucket := billing.BucketHour(time.Now().UnixMilli())

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

	// Start workers — each worker is pinned to one FDB client connection
	for c := 0; c < *numClients; c++ {
		eng := engines[c]
		for w := 0; w < *workersPerClient; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range work {
					batch := make([]meter.BatchEvent, item.count)
					for i := range batch {
						idx := item.startIdx + i
						custIdx := idx % len(customers)
						regionIdx := (idx / len(customers)) % len(regions)
						modelIdx := (idx / (len(customers) * len(regions))) % len(models)

						batch[i] = meter.BatchEvent{
							CustomerID:      customers[custIdx],
							TimestampBucket: bucket,
							Value:           1,
							GroupValues:     map[string]string{"region": regions[regionIdx], "model": models[modelIdx]},
						}
						customerExpected[custIdx].Add(1)
					}

					if err := eng.IngestBatch(ctx, *meterSlug, batch); err != nil {
						txErrors.Add(1)
						for i := range batch {
							idx := item.startIdx + i
							custIdx := idx % len(customers)
							customerExpected[custIdx].Add(-1)
						}
						continue
					}
					ingested.Add(int64(item.count))
				}
			}()
		}
	}

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
	// Use the first engine for verification (all write to same FDB subspace)
	var verifyFail int
	for i, cust := range customers {
		exp := customerExpected[i].Load()
		actual, err := engines[0].GetUsage(ctx, *meterSlug, cust, 0, int64(1<<62-1), nil)
		if err != nil {
			slog.Error("verify error", "customer", cust, "error", err)
			verifyFail++
			continue
		}
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
