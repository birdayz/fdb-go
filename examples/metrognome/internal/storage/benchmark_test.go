package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
)

func BenchmarkEventIngest1(b *testing.B) {
	benchmarkEventIngest(b, 1)
}

func BenchmarkEventIngest10(b *testing.B) {
	benchmarkEventIngest(b, 10)
}

func BenchmarkEventIngest100(b *testing.B) {
	benchmarkEventIngest(b, 100)
}

func benchmarkEventIngest(b *testing.B, batchSize int) {
	ctx := context.Background()
	ts := time.Now().UnixMilli()
	bucket := billing.BucketHour(ts)
	// Use a run-unique prefix to avoid idempotency key collisions across iterations
	runID := time.Now().UnixNano()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events := make([]*storev1.UsageEvent, batchSize)
		for j := range events {
			events[j] = &storev1.UsageEvent{
				Id:              proto.String(fmt.Sprintf("b%d-%d-%d", runID, i, j)),
				CustomerId:      proto.String("bench-cust"),
				MeterSlug:       proto.String("bench_meter"),
				TimestampMs:     proto.Int64(ts),
				Value:           proto.Int64(1),
				IdempotencyKey:  proto.String(fmt.Sprintf("b%d-%d-%d", runID, i, j)),
				TimestampBucket: proto.Int64(bucket),
				IngestedAt:      proto.Int64(ts),
			}
		}
		result, err := testDB.Events().Ingest(ctx, events)
		if err != nil {
			b.Fatal(err)
		}
		if result.Accepted != int32(batchSize) {
			b.Fatalf("expected %d accepted, got %d", batchSize, result.Accepted)
		}
	}
	b.ReportMetric(float64(batchSize), "events/op")
}

func BenchmarkUsageQuery(b *testing.B) {
	ctx := context.Background()
	ts := time.Now().UnixMilli()
	bucket := billing.BucketHour(ts)

	// Seed with 100 events
	events := make([]*storev1.UsageEvent, 100)
	for i := range events {
		events[i] = &storev1.UsageEvent{
			Id:              proto.String(fmt.Sprintf("bench-q-%d", i)),
			CustomerId:      proto.String("bench-query-cust"),
			MeterSlug:       proto.String("bench_query_meter"),
			TimestampMs:     proto.Int64(ts),
			Value:           proto.Int64(int64(i + 1)),
			IdempotencyKey:  proto.String(fmt.Sprintf("bench-q-idem-%d", i)),
			TimestampBucket: proto.Int64(bucket),
			IngestedAt:      proto.Int64(ts),
		}
	}
	if _, err := testDB.Events().Ingest(ctx, events); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		total, err := testDB.Events().GetUsage(ctx, "bench-query-cust", "bench_query_meter", bucket, bucket)
		if err != nil {
			b.Fatal(err)
		}
		if total <= 0 {
			b.Fatal("expected positive usage")
		}
	}
}

func BenchmarkInvoiceGeneration(b *testing.B) {
	ctx := context.Background()
	ts := time.Date(2028, 1, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	bucket := billing.BucketHour(ts)

	// Setup: customer + plan + charge + contract + events
	customer := &storev1.Customer{
		Id: proto.String("bench-inv-cust"), Name: proto.String("Bench Corp"),
		CreatedAt: proto.Int64(ts),
	}
	_ = testDB.Customers().Create(ctx, customer)

	plan := &storev1.Plan{
		Id: proto.String("bench-inv-plan"), Name: proto.String("Bench Plan"),
		CreatedAt: proto.Int64(ts),
	}
	_ = testDB.Plans().Create(ctx, plan)

	charge := &storev1.Charge{
		Id: proto.String("bench-inv-charge"), PlanId: proto.String("bench-inv-plan"),
		MeterSlug: proto.String("bench_inv_meter"),
		Pricing: &storev1.PricingModel{
			Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)},
			},
		},
		CreatedAt: proto.Int64(ts),
	}
	_ = testDB.Charges().Create(ctx, charge)

	periodStart := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contract := &storev1.Contract{
		Id: proto.String("bench-inv-ctr"), CustomerId: proto.String("bench-inv-cust"),
		PlanId: proto.String("bench-inv-plan"), StartAt: proto.Int64(periodStart),
		BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
		Active:        proto.Bool(true), CreatedAt: proto.Int64(ts),
	}
	_ = testDB.Contracts().Create(ctx, contract)

	// Seed events
	events := make([]*storev1.UsageEvent, 100)
	for i := range events {
		events[i] = &storev1.UsageEvent{
			Id:              proto.String(fmt.Sprintf("bench-inv-evt-%d", i)),
			CustomerId:      proto.String("bench-inv-cust"),
			MeterSlug:       proto.String("bench_inv_meter"),
			TimestampMs:     proto.Int64(ts),
			Value:           proto.Int64(1),
			IdempotencyKey:  proto.String(fmt.Sprintf("bench-inv-idem-%d", i)),
			TimestampBucket: proto.Int64(bucket),
			IngestedAt:      proto.Int64(ts),
		}
	}
	if _, err := testDB.Events().Ingest(ctx, events); err != nil {
		b.Fatal(err)
	}

	engine := billing.NewEngine(testDB.FDB(), testDB.MetaData(), testDB.Subspace())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Generate with unique period each iteration to avoid duplicate invoice IDs
		ps := periodStart + int64(i)*86400000
		pe := ps + 86400000
		_, err := engine.GenerateInvoice(ctx, "bench-inv-ctr", ps, pe)
		if err != nil {
			b.Fatal(err)
		}
	}
}
