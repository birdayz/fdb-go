package meter_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
)

// TestScaleDynamicMeterCorrectness is the critical correctness test for the
// dynamic meter engine. It verifies that user-defined group-by fields work
// at scale without losing data.
//
// Setup:
//   - 3 meters with different group-by configurations
//   - 5 customers
//   - 10,000+ events spread across groups
//   - Known expected values computed independently
//   - Exact match verification (zero tolerance)
func TestScaleDynamicMeterCorrectness(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	engine := meter.NewEngine(testRecordDB, subspace.Sub("scale_test"))

	// --- Meter 1: No group-by (simple counter) ---
	g.Expect(engine.Register(&storev1.Meter{
		Id:              proto.String("scale-m1"),
		Slug:            proto.String("scale_simple"),
		AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
	})).To(Succeed())

	// --- Meter 2: Single group-by (region) ---
	g.Expect(engine.Register(&storev1.Meter{
		Id:                proto.String("scale-m2"),
		Slug:              proto.String("scale_region"),
		AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		GroupByProperties: []string{"region"},
	})).To(Succeed())

	// --- Meter 3: Two group-by fields (region + model) ---
	g.Expect(engine.Register(&storev1.Meter{
		Id:                proto.String("scale-m3"),
		Slug:              proto.String("scale_region_model"),
		AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		GroupByProperties: []string{"region", "model"},
	})).To(Succeed())

	customers := []string{"cust-A", "cust-B", "cust-C", "cust-D", "cust-E"}
	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-south-1"}
	models := []string{"gpt-4", "claude-4", "llama-3", "gemini-2"}
	bucket := int64(1800000000000) // fixed bucket for all events

	// --- Track expected values ---
	// Meter 1: simple[customer] = sum
	simpleExpected := make(map[string]int64)
	// Meter 2: region[customer][region] = sum
	regionExpected := make(map[string]map[string]int64)
	// Meter 3: regionModel[customer][region:model] = sum
	regionModelExpected := make(map[string]map[string]int64)

	for _, c := range customers {
		simpleExpected[c] = 0
		regionExpected[c] = make(map[string]int64)
		regionModelExpected[c] = make(map[string]int64)
	}

	// --- Ingest 10,000 events with deterministic values ---
	totalEvents := 10000
	var totalIngested atomic.Int64
	var ingestErrors atomic.Int64

	t.Logf("ingesting %d events across %d customers, %d regions, %d models...",
		totalEvents, len(customers), len(regions), len(models))

	start := time.Now()

	for i := 0; i < totalEvents; i++ {
		custIdx := i % len(customers)
		regionIdx := (i / len(customers)) % len(regions)
		modelIdx := (i / (len(customers) * len(regions))) % len(models)

		cust := customers[custIdx]
		region := regions[regionIdx]
		model := models[modelIdx]
		value := int64(i%100 + 1) // 1-100

		// Track expected values
		simpleExpected[cust] += value
		regionExpected[cust][region] += value
		regionModelExpected[cust][region+":"+model] += value

		// Meter 1: simple
		err := engine.IngestEvent(ctx, "scale_simple", cust, bucket, value, nil)
		if err != nil {
			ingestErrors.Add(1)
			continue
		}

		// Meter 2: region
		err = engine.IngestEvent(ctx, "scale_region", cust, bucket, value,
			map[string]string{"region": region})
		if err != nil {
			ingestErrors.Add(1)
			continue
		}

		// Meter 3: region + model
		err = engine.IngestEvent(ctx, "scale_region_model", cust, bucket, value,
			map[string]string{"region": region, "model": model})
		if err != nil {
			ingestErrors.Add(1)
			continue
		}

		totalIngested.Add(1)
	}

	elapsed := time.Since(start)
	t.Logf("ingested %d events in %v (%.0f events/sec), %d errors",
		totalIngested.Load(), elapsed, float64(totalIngested.Load())/elapsed.Seconds(), ingestErrors.Load())
	g.Expect(ingestErrors.Load()).To(Equal(int64(0)), "zero ingest errors")
	g.Expect(totalIngested.Load()).To(Equal(int64(totalEvents)))

	// --- Verify Meter 1: Simple (no group-by) ---
	t.Log("verifying meter 1 (simple, no group-by)...")
	for _, cust := range customers {
		actual, err := engine.GetUsage(ctx, "scale_simple", cust, bucket, bucket, nil)
		g.Expect(err).NotTo(HaveOccurred(), "GetUsage for %s", cust)
		g.Expect(actual).To(Equal(simpleExpected[cust]),
			"meter simple, customer %s: expected %d, got %d", cust, simpleExpected[cust], actual)
	}
	t.Log("  meter 1 PASSED")

	// --- Verify Meter 2: Region group-by ---
	t.Log("verifying meter 2 (group-by region)...")
	for _, cust := range customers {
		// Total across all regions
		totalActual, err := engine.GetUsage(ctx, "scale_region", cust, bucket, bucket, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(totalActual).To(Equal(simpleExpected[cust]),
			"meter region total, customer %s", cust)

		// Per-region
		for _, region := range regions {
			actual, err := engine.GetUsage(ctx, "scale_region", cust, bucket, bucket,
				map[string]string{"region": region})
			g.Expect(err).NotTo(HaveOccurred())
			expected := regionExpected[cust][region]
			g.Expect(actual).To(Equal(expected),
				"meter region, customer %s, region %s: expected %d, got %d",
				cust, region, expected, actual)
		}
	}
	t.Log("  meter 2 PASSED")

	// --- Verify Meter 3: Region + Model group-by ---
	t.Log("verifying meter 3 (group-by region+model)...")
	for _, cust := range customers {
		// Total
		totalActual, err := engine.GetUsage(ctx, "scale_region_model", cust, bucket, bucket, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(totalActual).To(Equal(simpleExpected[cust]),
			"meter region+model total, customer %s", cust)

		// Per-group via GetUsageGroups
		groups, err := engine.GetUsageGroups(ctx, "scale_region_model", cust, bucket, bucket)
		g.Expect(err).NotTo(HaveOccurred())

		// Sum all groups — must equal total
		var groupSum int64
		for _, grp := range groups {
			groupSum += grp.Value
		}
		g.Expect(groupSum).To(Equal(simpleExpected[cust]),
			"meter region+model group sum, customer %s: expected %d, got %d",
			cust, simpleExpected[cust], groupSum)

		// Verify each group individually
		for _, grp := range groups {
			key := grp.GroupValues["region"] + ":" + grp.GroupValues["model"]
			expected := regionModelExpected[cust][key]
			g.Expect(grp.Value).To(Equal(expected),
				"meter region+model, customer %s, group %s: expected %d, got %d",
				cust, key, expected, grp.Value)
		}
	}
	t.Log("  meter 3 PASSED")

	t.Logf("ALL VERIFICATIONS PASSED: %d events, %d customers, %d groups, zero data loss",
		totalEvents, len(customers), len(regions)*len(models))
}

// TestScaleConcurrentIngest verifies correctness under concurrent writes.
// Multiple goroutines ingest events simultaneously — the SUM must still be exact.
func TestScaleConcurrentIngest(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	engine := meter.NewEngine(testRecordDB, subspace.Sub("concurrent_test"))

	g.Expect(engine.Register(&storev1.Meter{
		Id:                proto.String("conc-m1"),
		Slug:              proto.String("concurrent_meter"),
		AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		GroupByProperties: []string{"worker"},
	})).To(Succeed())

	bucket := int64(1900000000000)
	workers := 8
	eventsPerWorker := 500
	valuePerEvent := int64(1)
	expectedTotal := int64(workers * eventsPerWorker) // each event has value 1

	var wg sync.WaitGroup
	var errors atomic.Int64

	t.Logf("concurrent ingest: %d workers × %d events = %d total", workers, eventsPerWorker, workers*eventsPerWorker)
	start := time.Now()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			workerName := fmt.Sprintf("worker-%d", workerID)
			for i := 0; i < eventsPerWorker; i++ {
				err := engine.IngestEvent(ctx, "concurrent_meter", "conc-cust",
					bucket, valuePerEvent, map[string]string{"worker": workerName})
				if err != nil {
					errors.Add(1)
				}
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("concurrent ingest done in %v (%.0f events/sec), %d errors",
		elapsed, float64(workers*eventsPerWorker)/elapsed.Seconds(), errors.Load())
	g.Expect(errors.Load()).To(Equal(int64(0)))

	// Verify total
	total, err := engine.GetUsage(ctx, "concurrent_meter", "conc-cust", bucket, bucket, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(expectedTotal),
		"concurrent total: expected %d, got %d (lost %d)", expectedTotal, total, expectedTotal-total)

	// Verify per-worker
	groups, err := engine.GetUsageGroups(ctx, "concurrent_meter", "conc-cust", bucket, bucket)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(groups).To(HaveLen(workers))

	var groupSum int64
	for _, grp := range groups {
		g.Expect(grp.Value).To(Equal(int64(eventsPerWorker)),
			"worker %s: expected %d, got %d", grp.GroupValues["worker"], eventsPerWorker, grp.Value)
		groupSum += grp.Value
	}
	g.Expect(groupSum).To(Equal(expectedTotal))

	t.Logf("CONCURRENT TEST PASSED: %d events, %d workers, zero data loss", workers*eventsPerWorker, workers)
}
