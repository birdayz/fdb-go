// Command ingest sends bulk usage events to a Metrognome instance via ConnectRPC.
// Usage: bazelisk run //examples/metrognome/cmd/ingest -- -url http://host:8080 -key mgn_... -n 5000000 -workers 20
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
)

type authTransport struct {
	key  string
	base http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.key)
	return t.base.RoundTrip(req)
}

func main() {
	url := flag.String("url", "http://localhost:8080", "Metrognome API URL")
	key := flag.String("key", "", "API key (mgn_...)")
	total := flag.Int("n", 5_000_000, "Total events to ingest")
	workers := flag.Int("workers", 20, "Parallel workers")
	batch := flag.Int("batch", 500, "Events per batch")
	customer := flag.String("customer", "cust-anthropic", "Customer ID")
	flag.Parse()

	if *key == "" {
		log.Fatal("-key is required")
	}

	httpClient := &http.Client{
		Transport: &authTransport{key: *key, base: http.DefaultTransport},
	}
	client := metrognomev1connect.NewEventServiceClient(httpClient, *url)

	totalBatches := *total / *batch
	perWorker := totalBatches / *workers

	log.Printf("Ingesting %d events for %s", *total, *customer)
	log.Printf("  %d batches of %d, %d workers (%d batches/worker)", totalBatches, *batch, *workers, perWorker)

	var accepted, duplicates atomic.Int64
	var completed atomic.Int64
	start := time.Now()

	// Progress reporter
	go func() {
		for {
			time.Sleep(5 * time.Second)
			done := completed.Load()
			a := accepted.Load()
			elapsed := time.Since(start).Seconds()
			rate := float64(a) / elapsed
			pct := float64(done) / float64(totalBatches) * 100
			log.Printf("  %.1f%% | %d/%d batches | %d accepted | %.0f events/sec",
				pct, done, totalBatches, a, rate)
		}
	}()

	// Meter distribution: realistic Anthropic usage
	type meterProfile struct {
		slug   string
		weight int // out of 100
		minVal int64
		maxVal int64
	}
	meters := []meterProfile{
		{"api_calls", 55, 1, 1},        // 55% — each event = 1 API call
		{"llm_tokens", 35, 100, 50000}, // 35% — token counts per request
		{"storage_gb", 5, 1, 50},       // 5%  — storage deltas
		{"bandwidth_gb", 5, 1, 100},    // 5%  — bandwidth
	}
	// Build cumulative weights for fast lookup
	cumWeights := make([]int, len(meters))
	cum := 0
	for i, m := range meters {
		cum += m.weight
		cumWeights[i] = cum
	}

	// Base timestamp: April 1, 2026
	baseTs := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(workerID*1000), uint64(workerID)))
			startBatch := workerID * perWorker
			endBatch := startBatch + perWorker

			events := make([]*metrognomev1.Event, *batch)
			for b := startBatch; b < endBatch; b++ {
				for i := range events {
					idx := int64(b**batch + i)

					// Pick meter based on weight distribution
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

					// Spread across 15 days, 24 hours
					day := rng.IntN(15)
					hour := rng.IntN(24)
					minute := rng.IntN(60)
					ts := baseTs + int64(day)*86400000 + int64(hour)*3600000 + int64(minute)*60000

					events[i] = &metrognomev1.Event{
						CustomerId:     *customer,
						EventType:      m.slug,
						Value:          val,
						TimestampMs:    ts,
						IdempotencyKey: fmt.Sprintf("bulk-%d-%d", workerID, idx),
					}
				}

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				resp, err := client.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{
					Events: events,
				}))
				cancel()

				if err != nil {
					log.Printf("worker %d batch %d error: %v", workerID, b-startBatch, err)
					continue
				}
				accepted.Add(int64(resp.Msg.Accepted))
				duplicates.Add(int64(resp.Msg.Duplicates))
				completed.Add(1)
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)
	log.Printf("Done in %v", elapsed)
	log.Printf("  Accepted: %d, Duplicates: %d", accepted.Load(), duplicates.Load())
	log.Printf("  Throughput: %.0f events/sec", float64(accepted.Load())/elapsed.Seconds())
}
