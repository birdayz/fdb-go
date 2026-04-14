// Command seed populates a Metrognome instance with demo data for development.
// Run: bazelisk run //examples/metrognome/cmd/seed
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	clusterFile := os.Getenv("FDB_CLUSTER_FILE")
	fdb.MustAPIVersion(720)
	var fdbDB fdb.Database
	var err error
	if clusterFile != "" {
		fdbDB, err = fdb.OpenDatabase(clusterFile)
		if err != nil {
			slog.Error("failed to open FDB", "error", err)
			os.Exit(1)
		}
	} else {
		fdbDB = fdb.MustOpenDefault()
	}

	recordDB := rl.NewFDBDatabase(fdbDB)
	db, err := storage.NewDB(recordDB)
	if err != nil {
		slog.Error("failed to init storage", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	now := time.Now().UnixMilli()

	// --- Meters ---
	meters := []*storev1.Meter{
		{
			Id: proto.String("mtr-api"), Slug: proto.String("api_calls"), Name: proto.String("API Calls"),
			AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("mtr-tokens"), Slug: proto.String("llm_tokens"), Name: proto.String("LLM Tokens"),
			AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			GroupByProperties: []string{"model", "region"}, CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("mtr-storage"), Slug: proto.String("storage_gb"), Name: proto.String("Storage (GB)"),
			AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("mtr-bandwidth"), Slug: proto.String("bandwidth_gb"), Name: proto.String("Bandwidth (GB)"),
			AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(), CreatedAt: proto.Int64(now),
		},
	}
	for _, m := range meters {
		if err := db.Meters().Create(ctx, m); err != nil {
			slog.Warn("meter exists", "slug", m.GetSlug())
		} else {
			slog.Info("created meter", "slug", m.GetSlug())
		}
	}

	// --- Customers ---
	customers := []*storev1.Customer{
		{Id: proto.String("cust-acme"), Name: proto.String("Acme Corp"), ExternalId: proto.String("acme"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-globex"), Name: proto.String("Globex Corporation"), ExternalId: proto.String("globex"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-initech"), Name: proto.String("Initech"), ExternalId: proto.String("initech"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-umbrella"), Name: proto.String("Umbrella Corporation"), ExternalId: proto.String("umbrella"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-wayne"), Name: proto.String("Wayne Enterprises"), ExternalId: proto.String("wayne"), CreatedAt: proto.Int64(now)},
	}
	for _, c := range customers {
		if err := db.Customers().Create(ctx, c); err != nil {
			slog.Warn("customer exists", "name", c.GetName())
		} else {
			slog.Info("created customer", "name", c.GetName())
		}
	}

	// --- Plans ---
	plans := []*storev1.Plan{
		{Id: proto.String("plan-starter"), Name: proto.String("Starter"), Description: proto.String("For small teams"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("plan-pro"), Name: proto.String("Professional"), Description: proto.String("For growing businesses"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("plan-enterprise"), Name: proto.String("Enterprise"), Description: proto.String("For large organizations"), CreatedAt: proto.Int64(now)},
	}
	for _, p := range plans {
		if err := db.Plans().Create(ctx, p); err != nil {
			slog.Warn("plan exists", "name", p.GetName())
		} else {
			slog.Info("created plan", "name", p.GetName())
		}
	}

	// --- Charges ---
	charges := []*storev1.Charge{
		// Starter: $29 flat + $0.001 per API call
		{
			Id: proto.String("chrg-s-flat"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(2900)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-s-api"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(1)}}},
			CreatedAt: proto.Int64(now),
		},
		// Pro: $99 flat + tiered API calls + $0.05/GB storage
		{
			Id: proto.String("chrg-p-flat"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(9900)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-p-api"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("api_calls"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{
				Tiers: []*storev1.Tier{
					{UpTo: proto.Int64(10000), PriceCents: proto.Int64(1)},
					{UpTo: proto.Int64(100000), PriceCents: proto.Int64(0)}, // free tier 2
					{UpTo: proto.Int64(0), PriceCents: proto.Int64(0)},      // unlimited
				},
			}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-p-storage"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("storage_gb"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)}}},
			CreatedAt: proto.Int64(now),
		},
		// Enterprise: $499 flat + 25bps on API calls + package storage
		{
			Id: proto.String("chrg-e-flat"), PlanId: proto.String("plan-enterprise"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(49900)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-e-storage"), PlanId: proto.String("plan-enterprise"), MeterSlug: proto.String("storage_gb"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{
				PackageSize: proto.Int64(100), PackagePriceCents: proto.Int64(500),
			}}},
			CreatedAt: proto.Int64(now),
		},
	}
	for _, c := range charges {
		if err := db.Charges().Create(ctx, c); err != nil {
			slog.Warn("charge exists", "id", c.GetId())
		} else {
			slog.Info("created charge", "id", c.GetId(), "plan", c.GetPlanId(), "meter", c.GetMeterSlug())
		}
	}

	// --- Contracts ---
	monthStart := time.Date(2026, time.Now().Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contracts := []*storev1.Contract{
		{
			Id: proto.String("ctr-acme"), CustomerId: proto.String("cust-acme"), PlanId: proto.String("plan-starter"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-globex"), CustomerId: proto.String("cust-globex"), PlanId: proto.String("plan-pro"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-initech"), CustomerId: proto.String("cust-initech"), PlanId: proto.String("plan-enterprise"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
	}
	for _, c := range contracts {
		if err := db.Contracts().Create(ctx, c); err != nil {
			slog.Warn("contract exists", "id", c.GetId())
		} else {
			slog.Info("created contract", "id", c.GetId(), "customer", c.GetCustomerId(), "plan", c.GetPlanId())
		}
	}

	// --- Credits ---
	credits := []*storev1.Credit{
		{
			Id: proto.String("cred-acme"), CustomerId: proto.String("cust-acme"),
			AmountCents: proto.Int64(5000), RemainingCents: proto.Int64(5000),
			Priority: proto.Int32(1), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("cred-globex"), CustomerId: proto.String("cust-globex"),
			AmountCents: proto.Int64(25000), RemainingCents: proto.Int64(25000),
			Priority: proto.Int32(1), CreatedAt: proto.Int64(now),
		},
	}
	for _, c := range credits {
		if err := db.Credits().Create(ctx, c); err != nil {
			slog.Warn("credit exists", "id", c.GetId())
		} else {
			slog.Info("created credit", "customer", c.GetCustomerId(), "amount", c.GetAmountCents())
		}
	}

	// --- Usage Events ---
	slog.Info("generating usage events...")
	rng := rand.New(rand.NewPCG(42, 0))
	eventsCreated := 0

	for _, cust := range customers[:3] { // only first 3 have contracts
		custID := cust.GetId()
		for day := 0; day < 30; day++ {
			dayStart := time.Date(2026, time.Now().Month(), day+1, 0, 0, 0, 0, time.UTC)
			eventsPerDay := 10 + rng.IntN(40)
			batch := make([]*storev1.UsageEvent, 0, eventsPerDay)

			for j := 0; j < eventsPerDay; j++ {
				hour := rng.IntN(24)
				ts := dayStart.Add(time.Duration(hour) * time.Hour).UnixMilli()
				batch = append(batch, &storev1.UsageEvent{
					Id:              proto.String(fmt.Sprintf("seed-%s-%d-%d", custID, day, j)),
					CustomerId:      proto.String(custID),
					MeterSlug:       proto.String("api_calls"),
					TimestampMs:     proto.Int64(ts),
					Value:           proto.Int64(int64(1 + rng.IntN(10))),
					IdempotencyKey:  proto.String(fmt.Sprintf("seed-%s-%d-%d", custID, day, j)),
					TimestampBucket: proto.Int64(billing.BucketHour(ts)),
					IngestedAt:      proto.Int64(now),
				})
			}

			result, err := db.Events().Ingest(ctx, batch)
			if err != nil {
				slog.Error("ingest failed", "customer", custID, "day", day, "error", err)
				continue
			}
			eventsCreated += int(result.Accepted)
		}
	}

	slog.Info("seed complete",
		"customers", len(customers),
		"meters", len(meters),
		"plans", len(plans),
		"charges", len(charges),
		"contracts", len(contracts),
		"credits", len(credits),
		"events", eventsCreated,
	)
}
