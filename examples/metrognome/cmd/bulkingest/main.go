// Command bulkingest writes millions of usage events directly to FDB using InsertBatch.
// Runs on the same machine as FDB for maximum throughput. No HTTP overhead, no dedup checks.
// Usage: FDB_CLUSTER_FILE=/etc/foundationdb/fdb.cluster ./bulkingest -n 5000000 -workers 20
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func main() {
	total := flag.Int("n", 5_000_000, "Total events")
	workers := flag.Int("workers", 20, "Parallel goroutines")
	batch := flag.Int("batch", 200, "Events per FDB transaction")
	customer := flag.String("customer", "cust-anthropic", "Customer ID")
	flag.Parse()

	clusterFile := os.Getenv("FDB_CLUSTER_FILE")
	fdb.MustAPIVersion(720)
	var fdbDB fdb.Database
	var err error
	if clusterFile != "" {
		fdbDB, err = fdb.OpenDatabase(clusterFile)
		if err != nil {
			log.Fatalf("open FDB: %v", err)
		}
	} else {
		fdbDB = fdb.MustOpenDefault()
	}

	recordDB := rl.NewFDBDatabase(fdbDB)
	db, err := storage.NewDB(recordDB)
	if err != nil {
		log.Fatalf("init storage: %v", err)
	}

	perWorker := *total / *workers
	batchesPerWorker := perWorker / *batch

	log.Printf("Bulk ingesting %d events for %s", *total, *customer)
	log.Printf("  %d workers × %d batches × %d events/batch", *workers, batchesPerWorker, *batch)

	var accepted atomic.Int64
	var errors atomic.Int64
	start := time.Now()

	// Progress reporter
	go func() {
		for {
			time.Sleep(5 * time.Second)
			a := accepted.Load()
			e := errors.Load()
			elapsed := time.Since(start).Seconds()
			rate := float64(a) / elapsed
			pct := float64(a) / float64(*total) * 100
			log.Printf("  %.1f%% | %d/%d events | %d errors | %.0f events/sec",
				pct, a, *total, e, rate)
		}
	}()

	// Meter distribution
	type meterProfile struct {
		slug   string
		weight int
		minVal int64
		maxVal int64
	}
	meters := []meterProfile{
		{"api_calls", 55, 1, 1},
		{"llm_tokens", 35, 100, 50000},
		{"storage_gb", 5, 1, 50},
		{"bandwidth_gb", 5, 1, 100},
	}
	cumWeights := make([]int, len(meters))
	cum := 0
	for i, m := range meters {
		cum += m.weight
		cumWeights[i] = cum
	}

	baseTs := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(workerID*1337), uint64(workerID+42)))
			ctx := context.Background()

			for b := 0; b < batchesPerWorker; b++ {
				events := make([]*storev1.UsageEvent, *batch)
				for i := range events {
					globalIdx := int64(workerID)*int64(perWorker) + int64(b)*int64(*batch) + int64(i)

					// Pick meter
					roll := rng.IntN(100)
					mi := 0
					for mi < len(cumWeights)-1 && roll >= cumWeights[mi] {
						mi++
					}
					m := meters[mi]

					val := m.minVal
					if m.maxVal > m.minVal {
						val = m.minVal + rng.Int64N(m.maxVal-m.minVal)
					}

					// Spread across 30 days
					day := rng.IntN(30)
					hour := rng.IntN(24)
					minute := rng.IntN(60)
					second := rng.IntN(60)
					ts := baseTs + int64(day)*86400000 + int64(hour)*3600000 + int64(minute)*60000 + int64(second)*1000

					events[i] = &storev1.UsageEvent{
						Id:              proto.String(fmt.Sprintf("bi-%d", globalIdx)),
						CustomerId:      proto.String(*customer),
						MeterSlug:       proto.String(m.slug),
						EventType:       proto.String(m.slug),
						TimestampMs:     proto.Int64(ts),
						Value:           proto.Int64(val),
						IdempotencyKey:  proto.String(fmt.Sprintf("bi-%d", globalIdx)),
						TimestampBucket: proto.Int64(billing.BucketHour(ts)),
						IngestedAt:      proto.Int64(time.Now().UnixMilli()),
					}
				}

				n, err := db.Events().BulkInsert(ctx, events)
				if err != nil {
					errors.Add(1)
					if errors.Load() <= 10 {
						log.Printf("worker %d batch %d: %v", workerID, b, err)
					}
					continue
				}
				accepted.Add(int64(n))
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)
	log.Printf("Done in %v", elapsed)
	log.Printf("  Accepted: %d, Errors: %d", accepted.Load(), errors.Load())
	log.Printf("  Throughput: %.0f events/sec", float64(accepted.Load())/elapsed.Seconds())
}
