package storage_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var (
	testDB       *storage.DB
	testRecordDB *rl.FDBDatabase
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		panic("failed to start FDB container: " + err.Error())
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		panic("failed to get cluster file: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "metrognome_test_*.txt")
	if err != nil {
		panic(err.Error())
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		panic(err.Error())
	}
	tmpFile.Close()

	fdb.MustAPIVersion(720)
	fdbDB, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		panic("failed to open FDB: " + err.Error())
	}
	testRecordDB = rl.NewFDBDatabase(fdbDB)

	testDB, err = storage.NewDB(testRecordDB)
	if err != nil {
		panic("failed to init storage: " + err.Error())
	}

	code := m.Run()

	_ = container.Terminate(context.Background())
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

func TestCustomerCRUD(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	cust := &storev1.Customer{
		Id:         proto.String("test-cust-1"),
		Name:       proto.String("Acme Corp"),
		ExternalId: proto.String("ext-123"),
		CreatedAt:  proto.Int64(time.Now().UnixMilli()),
	}

	err := testDB.Customers().Create(ctx, cust)
	g.Expect(err).NotTo(HaveOccurred())

	loaded, err := testDB.Customers().Get(ctx, "test-cust-1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(loaded.GetName()).To(Equal("Acme Corp"))
	g.Expect(loaded.GetExternalId()).To(Equal("ext-123"))
}

func TestMeterCRUD(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	meter := &storev1.Meter{
		Id:              proto.String("test-meter-1"),
		Slug:            proto.String("api_calls_test"),
		Name:            proto.String("API Calls"),
		AggregationType: storev1.AggregationType_AGGREGATION_TYPE_COUNT.Enum(),
		CreatedAt:       proto.Int64(time.Now().UnixMilli()),
	}

	err := testDB.Meters().Create(ctx, meter)
	g.Expect(err).NotTo(HaveOccurred())

	bySlug, err := testDB.Meters().GetBySlug(ctx, "api_calls_test")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(bySlug.GetName()).To(Equal("API Calls"))
}

func TestEventIngestAndDedup(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ts := time.Now().UnixMilli()
	bucket := billing.BucketHour(ts)

	events := []*storev1.UsageEvent{
		{
			Id: proto.String("evt-dedup-1"), CustomerId: proto.String("cust-dedup"),
			MeterSlug: proto.String("tokens"), TimestampMs: proto.Int64(ts),
			Value: proto.Int64(100), IdempotencyKey: proto.String("idem-1"),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		},
		{
			Id: proto.String("evt-dedup-2"), CustomerId: proto.String("cust-dedup"),
			MeterSlug: proto.String("tokens"), TimestampMs: proto.Int64(ts),
			Value: proto.Int64(200), IdempotencyKey: proto.String("idem-2"),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		},
	}

	result, err := testDB.Events().Ingest(ctx, events)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Accepted).To(Equal(int32(2)))
	g.Expect(result.Duplicates).To(Equal(int32(0)))

	// Reingest same events — should be all duplicates
	events2 := []*storev1.UsageEvent{
		{
			Id: proto.String("evt-dedup-3"), CustomerId: proto.String("cust-dedup"),
			MeterSlug: proto.String("tokens"), TimestampMs: proto.Int64(ts),
			Value: proto.Int64(100), IdempotencyKey: proto.String("idem-1"), // same key!
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		},
	}

	result2, err := testDB.Events().Ingest(ctx, events2)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result2.Accepted).To(Equal(int32(0)))
	g.Expect(result2.Duplicates).To(Equal(int32(1)))
}

func TestUsageAggregation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ts := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	bucket := billing.BucketHour(ts)

	events := []*storev1.UsageEvent{
		{
			Id: proto.String("evt-agg-1"), CustomerId: proto.String("cust-agg"),
			MeterSlug: proto.String("bytes"), TimestampMs: proto.Int64(ts),
			Value: proto.Int64(500), IdempotencyKey: proto.String("agg-1"),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		},
		{
			Id: proto.String("evt-agg-2"), CustomerId: proto.String("cust-agg"),
			MeterSlug: proto.String("bytes"), TimestampMs: proto.Int64(ts + 1000),
			Value: proto.Int64(300), IdempotencyKey: proto.String("agg-2"),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		},
		{
			Id: proto.String("evt-agg-3"), CustomerId: proto.String("cust-agg"),
			MeterSlug: proto.String("bytes"), TimestampMs: proto.Int64(ts + 2000),
			Value: proto.Int64(200), IdempotencyKey: proto.String("agg-3"),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		},
	}

	result, err := testDB.Events().Ingest(ctx, events)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Accepted).To(Equal(int32(3)))

	// SUM should be 500 + 300 + 200 = 1000
	total, err := testDB.Events().GetUsage(ctx, "cust-agg", "bytes", bucket, bucket)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(1000)))

	// COUNT
	count, err := testDB.Events().GetUsageCount(ctx, "cust-agg", "bytes", bucket, bucket)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(count).To(Equal(int64(3)))

	// Buckets
	buckets, err := testDB.Events().GetUsageBuckets(ctx, "cust-agg", "bytes", bucket, bucket)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(buckets).To(HaveKey(bucket))
	g.Expect(buckets[bucket]).To(Equal(int64(1000)))
}

func TestEndToEndInvoiceGeneration(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	// 1. Create customer
	customer := &storev1.Customer{
		Id: proto.String("cust-e2e"), Name: proto.String("E2E Corp"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	}
	g.Expect(testDB.Customers().Create(ctx, customer)).To(Succeed())

	// 2. Create plan with per-unit charge
	plan := &storev1.Plan{
		Id: proto.String("plan-e2e"), Name: proto.String("API Plan"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	}
	g.Expect(testDB.Plans().Create(ctx, plan)).To(Succeed())

	charge := &storev1.Charge{
		Id: proto.String("chrg-e2e"), PlanId: proto.String("plan-e2e"),
		MeterSlug: proto.String("api_calls_e2e"),
		Pricing: &storev1.PricingModel{
			Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(10)}, // $0.10 per call
			},
		},
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	}
	g.Expect(testDB.Charges().Create(ctx, charge)).To(Succeed())

	// 3. Create contract
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contract := &storev1.Contract{
		Id: proto.String("ctr-e2e"), CustomerId: proto.String("cust-e2e"),
		PlanId: proto.String("plan-e2e"), StartAt: proto.Int64(periodStart),
		BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
		Active:        proto.Bool(true), CreatedAt: proto.Int64(time.Now().UnixMilli()),
	}
	g.Expect(testDB.Contracts().Create(ctx, contract)).To(Succeed())

	// 4. Ingest 50 API call events
	ts := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	bucket := billing.BucketHour(ts)
	events := make([]*storev1.UsageEvent, 50)
	for i := range events {
		events[i] = &storev1.UsageEvent{
			Id: proto.String(fmtID("evt-e2e", i)), CustomerId: proto.String("cust-e2e"),
			MeterSlug: proto.String("api_calls_e2e"), TimestampMs: proto.Int64(ts + int64(i*1000)),
			Value: proto.Int64(1), IdempotencyKey: proto.String(fmtID("e2e-idem", i)),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		}
	}

	result, err := testDB.Events().Ingest(ctx, events)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Accepted).To(Equal(int32(50)))

	// 5. Generate invoice
	engine := billing.NewEngine(testDB.FDB(), testDB.MetaData(), testDB.Subspace())
	invoice, err := engine.GenerateInvoice(ctx, "ctr-e2e", periodStart, periodEnd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invoice).NotTo(BeNil())
	g.Expect(invoice.GetCustomerId()).To(Equal("cust-e2e"))
	g.Expect(invoice.GetLineItems()).To(HaveLen(1))
	g.Expect(invoice.GetLineItems()[0].GetQuantity()).To(Equal(int64(50)))
	g.Expect(invoice.GetLineItems()[0].GetAmountCents()).To(Equal(int64(500))) // 50 * 10 cents
	g.Expect(invoice.GetSubtotalCents()).To(Equal(int64(500)))
	g.Expect(invoice.GetTotalCents()).To(Equal(int64(500)))
}

func TestInvoiceWithCredits(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	// Setup
	g.Expect(testDB.Customers().Create(ctx, &storev1.Customer{
		Id: proto.String("cust-credit"), Name: proto.String("Credit Corp"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	g.Expect(testDB.Plans().Create(ctx, &storev1.Plan{
		Id: proto.String("plan-credit"), Name: proto.String("Credit Plan"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	g.Expect(testDB.Charges().Create(ctx, &storev1.Charge{
		Id: proto.String("chrg-credit"), PlanId: proto.String("plan-credit"),
		MeterSlug: proto.String("api_calls_credit"),
		Pricing: &storev1.PricingModel{
			Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(100)},
			},
		},
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	periodStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	g.Expect(testDB.Contracts().Create(ctx, &storev1.Contract{
		Id: proto.String("ctr-credit"), CustomerId: proto.String("cust-credit"),
		PlanId: proto.String("plan-credit"), StartAt: proto.Int64(periodStart),
		BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
		Active:        proto.Bool(true), CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	// Grant $3.00 credit
	g.Expect(testDB.Credits().Create(ctx, &storev1.Credit{
		Id: proto.String("cred-1"), CustomerId: proto.String("cust-credit"),
		AmountCents: proto.Int64(300), RemainingCents: proto.Int64(300),
		Priority: proto.Int32(1), CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	// Ingest 10 events → 10 * 100 cents = $10.00 subtotal
	ts := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	bucket := billing.BucketHour(ts)
	events := make([]*storev1.UsageEvent, 10)
	for i := range events {
		events[i] = &storev1.UsageEvent{
			Id: proto.String(fmtID("evt-credit", i)), CustomerId: proto.String("cust-credit"),
			MeterSlug: proto.String("api_calls_credit"), TimestampMs: proto.Int64(ts),
			Value: proto.Int64(1), IdempotencyKey: proto.String(fmtID("credit-idem", i)),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		}
	}
	result, err := testDB.Events().Ingest(ctx, events)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Accepted).To(Equal(int32(10)))

	// Generate invoice: $10.00 - $3.00 credit = $7.00
	engine := billing.NewEngine(testDB.FDB(), testDB.MetaData(), testDB.Subspace())
	invoice, err := engine.GenerateInvoice(ctx, "ctr-credit", periodStart, periodEnd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invoice.GetSubtotalCents()).To(Equal(int64(1000)))
	g.Expect(invoice.GetCreditsAppliedCents()).To(Equal(int64(300)))
	g.Expect(invoice.GetTotalCents()).To(Equal(int64(700)))

	// Credit should be depleted
	balance, _, err := testDB.Credits().GetBalance(ctx, "cust-credit")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(balance).To(Equal(int64(0)))
}

func TestPricingModels(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Per-unit
	amount, _, err := billing.CalculateCharge(100, &storev1.PricingModel{
		Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(500)))

	// Flat
	amount, _, err = billing.CalculateCharge(0, &storev1.PricingModel{
		Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(9900)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(9900)))

	// Tiered: first 100 @ 10c, next 900 @ 5c, rest @ 2c
	// For 250 units: 100*10 + 150*5 = 1000 + 750 = 1750
	amount, _, err = billing.CalculateCharge(250, &storev1.PricingModel{
		Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{
			Tiers: []*storev1.Tier{
				{UpTo: proto.Int64(100), PriceCents: proto.Int64(10)},
				{UpTo: proto.Int64(1000), PriceCents: proto.Int64(5)},
				{UpTo: proto.Int64(0), PriceCents: proto.Int64(2)},
			},
		}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1750)))

	// Volume: 250 units falls in 101-1000 tier → all at 5c → 250*5 = 1250
	amount, _, err = billing.CalculateCharge(250, &storev1.PricingModel{
		Model: &storev1.PricingModel_Volume{Volume: &storev1.VolumePricing{
			Tiers: []*storev1.Tier{
				{UpTo: proto.Int64(100), PriceCents: proto.Int64(10)},
				{UpTo: proto.Int64(1000), PriceCents: proto.Int64(5)},
			},
		}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1250)))

	// Package: 2500 units / 1000 per package = 3 packages * 1000c = 3000c
	amount, _, err = billing.CalculateCharge(2500, &storev1.PricingModel{
		Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{
			PackageSize: proto.Int64(1000), PackagePriceCents: proto.Int64(1000),
		}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(3000)))

	// BPS: 25 bps on 100000c ($1000) → 100000*25/10000 = 250c ($2.50)
	amount, _, err = billing.CalculateCharge(100000, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(25)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(250)))
}

func TestZeroUsageInvoice(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	// Setup customer + plan + contract with no events
	g.Expect(testDB.Customers().Create(ctx, &storev1.Customer{
		Id: proto.String("cust-zero"), Name: proto.String("Zero Corp"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())
	g.Expect(testDB.Plans().Create(ctx, &storev1.Plan{
		Id: proto.String("plan-zero"), Name: proto.String("Zero Plan"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())
	g.Expect(testDB.Charges().Create(ctx, &storev1.Charge{
		Id: proto.String("chrg-zero"), PlanId: proto.String("plan-zero"),
		MeterSlug: proto.String("api_calls_zero"),
		Pricing: &storev1.PricingModel{
			Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(10)},
			},
		},
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	periodStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	g.Expect(testDB.Contracts().Create(ctx, &storev1.Contract{
		Id: proto.String("ctr-zero"), CustomerId: proto.String("cust-zero"),
		PlanId: proto.String("plan-zero"), StartAt: proto.Int64(periodStart),
		BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
		Active:        proto.Bool(true), CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	// Generate invoice with zero usage
	engine := billing.NewEngine(testDB.FDB(), testDB.MetaData(), testDB.Subspace())
	invoice, err := engine.GenerateInvoice(ctx, "ctr-zero", periodStart, periodEnd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invoice.GetTotalCents()).To(Equal(int64(0)))
	g.Expect(invoice.GetLineItems()).To(HaveLen(1))
	g.Expect(invoice.GetLineItems()[0].GetQuantity()).To(Equal(int64(0)))
}

func TestTieredInvoice(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	// Setup with tiered pricing
	g.Expect(testDB.Customers().Create(ctx, &storev1.Customer{
		Id: proto.String("cust-tiered"), Name: proto.String("Tiered Corp"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())
	g.Expect(testDB.Plans().Create(ctx, &storev1.Plan{
		Id: proto.String("plan-tiered"), Name: proto.String("Tiered Plan"),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())
	g.Expect(testDB.Charges().Create(ctx, &storev1.Charge{
		Id: proto.String("chrg-tiered"), PlanId: proto.String("plan-tiered"),
		MeterSlug: proto.String("api_calls_tiered"),
		Pricing: &storev1.PricingModel{
			Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{
				Tiers: []*storev1.Tier{
					{UpTo: proto.Int64(10), PriceCents: proto.Int64(100)}, // first 10 @ $1.00
					{UpTo: proto.Int64(100), PriceCents: proto.Int64(50)}, // next 90 @ $0.50
					{UpTo: proto.Int64(0), PriceCents: proto.Int64(10)},   // rest @ $0.10
				},
			}},
		},
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	periodStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	g.Expect(testDB.Contracts().Create(ctx, &storev1.Contract{
		Id: proto.String("ctr-tiered"), CustomerId: proto.String("cust-tiered"),
		PlanId: proto.String("plan-tiered"), StartAt: proto.Int64(periodStart),
		BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
		Active:        proto.Bool(true), CreatedAt: proto.Int64(time.Now().UnixMilli()),
	})).To(Succeed())

	// Ingest 150 events (each value=1): 10*100 + 90*50 + 50*10 = 1000 + 4500 + 500 = 6000
	ts := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	bucket := billing.BucketHour(ts)
	events := make([]*storev1.UsageEvent, 150)
	for i := range events {
		events[i] = &storev1.UsageEvent{
			Id: proto.String(fmtID("evt-tiered", i)), CustomerId: proto.String("cust-tiered"),
			MeterSlug: proto.String("api_calls_tiered"), TimestampMs: proto.Int64(ts),
			Value: proto.Int64(1), IdempotencyKey: proto.String(fmtID("tiered-idem", i)),
			TimestampBucket: proto.Int64(bucket), IngestedAt: proto.Int64(ts),
		}
	}
	result, err := testDB.Events().Ingest(ctx, events)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Accepted).To(Equal(int32(150)))

	engine := billing.NewEngine(testDB.FDB(), testDB.MetaData(), testDB.Subspace())
	invoice, err := engine.GenerateInvoice(ctx, "ctr-tiered", periodStart, periodEnd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invoice.GetLineItems()[0].GetQuantity()).To(Equal(int64(150)))
	g.Expect(invoice.GetLineItems()[0].GetAmountCents()).To(Equal(int64(6000)))
}

func fmtID(prefix string, i int) string {
	return fmt.Sprintf("%s-%d", prefix, i)
}
