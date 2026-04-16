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
			AggregationType: storev1.AggregationType_AGGREGATION_TYPE_COUNT.Enum(), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("mtr-tokens"), Slug: proto.String("llm_tokens"), Name: proto.String("LLM Tokens"),
			AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			ValueProperty:     proto.String("tokens"),
			GroupByProperties: []string{"model"}, CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("mtr-storage"), Slug: proto.String("storage_gb"), Name: proto.String("Storage (GB-hours)"),
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
		{Id: proto.String("cust-anthropic"), Name: proto.String("Anthropic"), ExternalId: proto.String("anthropic"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-openai"), Name: proto.String("OpenAI"), ExternalId: proto.String("openai"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-stripe"), Name: proto.String("Stripe"), ExternalId: proto.String("stripe"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-vercel"), Name: proto.String("Vercel"), ExternalId: proto.String("vercel"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-supabase"), Name: proto.String("Supabase"), ExternalId: proto.String("supabase"), CreatedAt: proto.Int64(now)},
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
		{Id: proto.String("plan-starter"), Name: proto.String("Starter"), Description: proto.String("For startups and small teams — 100K API calls/mo included"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("plan-growth"), Name: proto.String("Growth"), Description: proto.String("For scaling companies — tiered pricing, priority support"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("plan-enterprise"), Name: proto.String("Enterprise"), Description: proto.String("Custom pricing, volume discounts, dedicated support"), CreatedAt: proto.Int64(now)},
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
		// Starter: $49/mo flat + $0.001/API call + $0.01/1K tokens
		{
			Id: proto.String("chrg-s-flat"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(4900)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-s-api"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(1)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-s-tokens"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("llm_tokens"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(1)}}},
			CreatedAt: proto.Int64(now),
		},
		// Growth: $199/mo flat + tiered API calls + $0.05/GB storage
		{
			Id: proto.String("chrg-g-flat"), PlanId: proto.String("plan-growth"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(19900)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-g-api"), PlanId: proto.String("plan-growth"), MeterSlug: proto.String("api_calls"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{
				Tiers: []*storev1.Tier{
					{UpTo: proto.Int64(100000), PriceCents: proto.Int64(0)},  // first 100K free
					{UpTo: proto.Int64(1000000), PriceCents: proto.Int64(1)}, // $0.01/call up to 1M
					{UpTo: proto.Int64(0), PriceCents: proto.Int64(0)},       // unlimited above
				},
			}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-g-storage"), PlanId: proto.String("plan-growth"), MeterSlug: proto.String("storage_gb"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)}}},
			CreatedAt: proto.Int64(now),
		},
		// Enterprise: $999/mo flat + 25bps on token usage + package storage (100GB blocks)
		{
			Id: proto.String("chrg-e-flat"), PlanId: proto.String("plan-enterprise"), MeterSlug: proto.String("api_calls"),
			Pricing:   &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(99900)}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-e-tokens"), PlanId: proto.String("plan-enterprise"), MeterSlug: proto.String("llm_tokens"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{
				BasisPoints: proto.Int64(25), // 0.25%
			}}},
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-e-storage"), PlanId: proto.String("plan-enterprise"), MeterSlug: proto.String("storage_gb"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{
				PackageSize: proto.Int64(100), PackagePriceCents: proto.Int64(999),
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
			Id: proto.String("ctr-anthropic"), CustomerId: proto.String("cust-anthropic"), PlanId: proto.String("plan-enterprise"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-openai"), CustomerId: proto.String("cust-openai"), PlanId: proto.String("plan-growth"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-stripe"), CustomerId: proto.String("cust-stripe"), PlanId: proto.String("plan-growth"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-vercel"), CustomerId: proto.String("cust-vercel"), PlanId: proto.String("plan-starter"),
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
			Id: proto.String("cred-anthropic"), CustomerId: proto.String("cust-anthropic"),
			AmountCents: proto.Int64(100000), RemainingCents: proto.Int64(100000), // $1000 credit
			Priority: proto.Int32(1), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("cred-openai"), CustomerId: proto.String("cust-openai"),
			AmountCents: proto.Int64(50000), RemainingCents: proto.Int64(50000), // $500 credit
			Priority: proto.Int32(1), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("cred-vercel"), CustomerId: proto.String("cust-vercel"),
			AmountCents: proto.Int64(10000), RemainingCents: proto.Int64(10000), // $100 credit
			Priority: proto.Int32(1), CreatedAt: proto.Int64(now),
		},
	}
	for _, c := range credits {
		if err := db.Credits().Create(ctx, c); err != nil {
			slog.Warn("credit exists", "id", c.GetId())
		} else {
			slog.Info("created credit", "customer", c.GetCustomerId(), "amount_cents", c.GetAmountCents())
		}
	}

	// --- Alerts ---
	alerts := []*storev1.Alert{
		{
			Id: proto.String("alrt-anthropic-api"), CustomerId: proto.String("cust-anthropic"),
			MeterSlug: proto.String("api_calls"), Threshold: proto.Int64(1000000),
			AlertType: storev1.AlertType_ALERT_TYPE_USAGE.Enum(), Triggered: proto.Bool(false),
			CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("alrt-openai-tokens"), CustomerId: proto.String("cust-openai"),
			MeterSlug: proto.String("llm_tokens"), Threshold: proto.Int64(5000000),
			AlertType: storev1.AlertType_ALERT_TYPE_USAGE.Enum(), Triggered: proto.Bool(false),
			CreatedAt: proto.Int64(now),
		},
	}
	for _, a := range alerts {
		if err := db.Alerts().Create(ctx, a); err != nil {
			slog.Warn("alert exists", "id", a.GetId())
		} else {
			slog.Info("created alert", "customer", a.GetCustomerId(), "meter", a.GetMeterSlug())
		}
	}

	// --- Usage Events ---
	slog.Info("generating usage events...")
	rng := rand.New(rand.NewPCG(42, 0))
	eventsCreated := 0

	// Usage profiles per customer (events per day, value range)
	type usageProfile struct {
		customerID   string
		slug         string
		eventsPerDay int
		minValue     int
		maxValue     int
	}

	profiles := []usageProfile{
		// Anthropic: heavy API + token user (enterprise)
		{"cust-anthropic", "api_calls", 200, 1, 5},
		{"cust-anthropic", "llm_tokens", 150, 100, 50000},
		{"cust-anthropic", "storage_gb", 10, 1, 10},
		// OpenAI: moderate usage (growth)
		{"cust-openai", "api_calls", 80, 1, 3},
		{"cust-openai", "llm_tokens", 60, 50, 20000},
		// Stripe: API-heavy (growth)
		{"cust-stripe", "api_calls", 120, 1, 10},
		{"cust-stripe", "bandwidth_gb", 20, 1, 5},
		// Vercel: light usage (starter)
		{"cust-vercel", "api_calls", 15, 1, 2},
		{"cust-vercel", "storage_gb", 5, 1, 3},
	}

	for _, prof := range profiles {
		for day := 0; day < 14; day++ {
			dayStart := time.Date(2026, time.Now().Month(), day+1, 0, 0, 0, 0, time.UTC)
			if dayStart.After(time.Now()) {
				break
			}
			n := prof.eventsPerDay + rng.IntN(prof.eventsPerDay/3+1) - prof.eventsPerDay/6
			if n < 1 {
				n = 1
			}
			batch := make([]*storev1.UsageEvent, 0, n)

			for j := 0; j < n; j++ {
				hour := rng.IntN(24)
				ts := dayStart.Add(time.Duration(hour) * time.Hour).UnixMilli()
				val := int64(prof.minValue + rng.IntN(prof.maxValue-prof.minValue+1))
				idKey := fmt.Sprintf("seed-%s-%s-%d-%d", prof.customerID, prof.slug, day, j)
				batch = append(batch, &storev1.UsageEvent{
					CustomerId:      proto.String(prof.customerID),
					MeterSlug:       proto.String(prof.slug),
					TimestampMs:     proto.Int64(ts),
					Value:           proto.Int64(val),
					IdempotencyKey:  proto.String(idKey),
					TimestampBucket: proto.Int64(billing.BucketHour(ts)),
					IngestedAt:      proto.Int64(now),
				})
			}

			result, err := db.Events().Ingest(ctx, batch)
			if err != nil {
				slog.Error("ingest failed", "customer", prof.customerID, "slug", prof.slug, "day", day, "error", err)
				continue
			}
			eventsCreated += int(result.Accepted)
		}
	}

	// --- Generate Invoices ---
	// Run the billing engine for each active contract — creates invoices
	// with line items computed from atomic SUM indexes + credit drawdown.
	// All in a single FDB transaction per invoice.
	slog.Info("generating invoices...")
	billingEngine := billing.NewEngine(recordDB, db.MetaData(), db.Subspace())
	invoicesGenerated := 0

	// Generate invoices for the first 2 weeks of the month (where we have events)
	periodStart := time.Date(2026, time.Now().Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, time.Now().Month(), 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	for _, c := range contracts {
		inv, err := billingEngine.GenerateInvoice(ctx, c.GetId(), periodStart, periodEnd)
		if err != nil {
			slog.Warn("invoice generation failed", "contract", c.GetId(), "error", err)
			continue
		}
		slog.Info("generated invoice",
			"contract", c.GetId(),
			"customer", c.GetCustomerId(),
			"subtotal_cents", inv.GetSubtotalCents(),
			"credits_applied_cents", inv.GetCreditsAppliedCents(),
			"total_cents", inv.GetTotalCents(),
			"line_items", len(inv.GetLineItems()),
		)
		invoicesGenerated++
	}

	slog.Info("seed complete",
		"customers", len(customers),
		"meters", len(meters),
		"plans", len(plans),
		"charges", len(charges),
		"contracts", len(contracts),
		"credits", len(credits),
		"alerts", len(alerts),
		"events", eventsCreated,
		"invoices", invoicesGenerated,
	)
}
