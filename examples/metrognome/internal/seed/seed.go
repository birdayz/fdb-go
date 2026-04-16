// Package seed populates a tenant with demo billing data.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

// Tenant seeds a tenant DB with demo meters, customers, plans, events, and invoices.
// Called on first login to make the UI immediately useful.
func Tenant(ctx context.Context, db *storage.DB, displayName string) error {
	now := time.Now().UnixMilli()
	rng := rand.New(rand.NewPCG(uint64(now), 0))

	// --- Meters ---
	meters := []*storev1.Meter{
		{
			Id: proto.String("mtr-api"), Slug: proto.String("api_calls"), Name: proto.String("API Calls"),
			AggregationType: storev1.AggregationType_AGGREGATION_TYPE_COUNT.Enum(), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("mtr-tokens"), Slug: proto.String("llm_tokens"), Name: proto.String("LLM Tokens"),
			AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			ValueProperty:   proto.String("tokens"), GroupByProperties: []string{"model"}, CreatedAt: proto.Int64(now),
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
		_ = db.Meters().Create(ctx, m)
	}

	// --- Customers ---
	customers := []*storev1.Customer{
		{Id: proto.String("cust-acme"), Name: proto.String("Acme Corp"), ExternalId: proto.String("acme"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-globex"), Name: proto.String("Globex Inc"), ExternalId: proto.String("globex"), CreatedAt: proto.Int64(now)},
		{Id: proto.String("cust-initech"), Name: proto.String("Initech"), ExternalId: proto.String("initech"), CreatedAt: proto.Int64(now)},
	}
	for _, c := range customers {
		_ = db.Customers().Create(ctx, c)
	}

	// --- Plans ---
	plans := []*storev1.Plan{
		{
			Id: proto.String("plan-starter"), Name: proto.String("Starter"),
			Description: proto.String("For small teams — 10K API calls included, then $0.001/call"), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("plan-pro"), Name: proto.String("Pro"),
			Description: proto.String("For scaling companies — tiered API + per-unit tokens + storage"), CreatedAt: proto.Int64(now),
		},
	}
	for _, p := range plans {
		_ = db.Plans().Create(ctx, p)
	}

	// --- Charges ---
	charges := []*storev1.Charge{
		// Starter: $49/mo flat + $0.001/API call
		{
			Id: proto.String("chrg-s-flat"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("api_calls"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Flat{
				Flat: &storev1.FlatPricing{AmountCents: proto.Int64(4900)},
			}}, CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-s-api"), PlanId: proto.String("plan-starter"), MeterSlug: proto.String("api_calls"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(1)},
			}}, CreatedAt: proto.Int64(now),
		},
		// Pro: $99/mo flat + tiered API + $0.01/token + $0.05/GB storage
		{
			Id: proto.String("chrg-p-flat"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("api_calls"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Flat{
				Flat: &storev1.FlatPricing{AmountCents: proto.Int64(9900)},
			}}, CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-p-api"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("api_calls"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_Tiered{
				Tiered: &storev1.TieredPricing{Tiers: []*storev1.Tier{
					{UpTo: proto.Int64(10000), PriceCents: proto.Int64(0)},
					{UpTo: proto.Int64(100000), PriceCents: proto.Int64(1)},
					{UpTo: proto.Int64(0), PriceCents: proto.Int64(0)},
				}},
			}}, CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-p-tokens"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("llm_tokens"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(1)},
			}}, CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("chrg-p-storage"), PlanId: proto.String("plan-pro"), MeterSlug: proto.String("storage_gb"),
			Pricing: &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{
				PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)},
			}}, CreatedAt: proto.Int64(now),
		},
	}
	for _, c := range charges {
		_ = db.Charges().Create(ctx, c)
	}

	// --- Contracts ---
	monthStart := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contracts := []*storev1.Contract{
		{
			Id: proto.String("ctr-acme"), CustomerId: proto.String("cust-acme"), PlanId: proto.String("plan-pro"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-globex"), CustomerId: proto.String("cust-globex"), PlanId: proto.String("plan-pro"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
		{
			Id: proto.String("ctr-initech"), CustomerId: proto.String("cust-initech"), PlanId: proto.String("plan-starter"),
			StartAt: proto.Int64(monthStart), BillingPeriod: storev1.BillingPeriod_BILLING_PERIOD_MONTHLY.Enum(),
			Active: proto.Bool(true), CreatedAt: proto.Int64(now),
		},
	}
	for _, c := range contracts {
		_ = db.Contracts().Create(ctx, c)
	}

	// --- Credits ---
	_ = db.Credits().Create(ctx, &storev1.Credit{
		Id: proto.String("cred-acme"), CustomerId: proto.String("cust-acme"),
		AmountCents: proto.Int64(50000), RemainingCents: proto.Int64(50000),
		Priority: proto.Int32(1), CreatedAt: proto.Int64(now),
	})

	// --- Alerts ---
	_ = db.Alerts().Create(ctx, &storev1.Alert{
		Id: proto.String("alrt-acme-api"), CustomerId: proto.String("cust-acme"),
		MeterSlug: proto.String("api_calls"), Threshold: proto.Int64(50000),
		AlertType: storev1.AlertType_ALERT_TYPE_USAGE.Enum(), Triggered: proto.Bool(false),
		CreatedAt: proto.Int64(now),
	})

	// --- Events (2 weeks of realistic usage) ---
	type profile struct {
		customerID string
		slug       string
		perDay     int
		minVal     int
		maxVal     int
	}
	profiles := []profile{
		// Acme: heavy user (Pro plan)
		{"cust-acme", "api_calls", 150, 1, 5},
		{"cust-acme", "llm_tokens", 80, 100, 30000},
		{"cust-acme", "storage_gb", 8, 1, 10},
		// Globex: moderate user (Pro plan)
		{"cust-globex", "api_calls", 60, 1, 3},
		{"cust-globex", "llm_tokens", 40, 50, 15000},
		{"cust-globex", "storage_gb", 5, 1, 5},
		// Initech: light user (Starter plan)
		{"cust-initech", "api_calls", 20, 1, 2},
		{"cust-initech", "bandwidth_gb", 10, 1, 5},
	}

	eventsCreated := 0
	for _, p := range profiles {
		for day := 0; day < 14; day++ {
			dayStart := time.Date(time.Now().Year(), time.Now().Month(), day+1, 0, 0, 0, 0, time.UTC)
			if dayStart.After(time.Now()) {
				break
			}
			n := p.perDay + rng.IntN(p.perDay/3+1) - p.perDay/6
			if n < 1 {
				n = 1
			}
			batch := make([]*storev1.UsageEvent, 0, n)
			for j := 0; j < n; j++ {
				ts := dayStart.Add(time.Duration(rng.IntN(24)) * time.Hour).UnixMilli()
				val := int64(p.minVal + rng.IntN(p.maxVal-p.minVal+1))
				idKey := fmt.Sprintf("seed-%s-%s-%d-%d", p.customerID, p.slug, day, j)
				batch = append(batch, &storev1.UsageEvent{
					CustomerId:      proto.String(p.customerID),
					EventType:       proto.String(p.slug),
					MeterSlug:       proto.String(p.slug),
					TimestampMs:     proto.Int64(ts),
					Value:           proto.Int64(val),
					IdempotencyKey:  proto.String(idKey),
					TimestampBucket: proto.Int64(billing.BucketHour(ts)),
					IngestedAt:      proto.Int64(now),
				})
			}
			result, err := db.Events().Ingest(ctx, batch)
			if err != nil {
				slog.Warn("seed ingest failed", "error", err)
				continue
			}
			eventsCreated += int(result.Accepted)
		}
	}

	// --- Invoices via billing engine ---
	billingEngine := billing.NewEngine(db.FDB(), db.MetaData(), db.Subspace())
	periodStart := monthStart
	periodEnd := time.Now().UnixMilli()
	invoicesGenerated := 0
	for _, c := range contracts {
		inv, err := billingEngine.GenerateInvoice(ctx, c.GetId(), periodStart, periodEnd)
		if err != nil {
			slog.Warn("seed invoice failed", "contract", c.GetId(), "error", err)
			continue
		}
		_ = inv
		invoicesGenerated++
	}

	slog.Info("tenant seeded",
		"display_name", displayName,
		"customers", len(customers),
		"meters", len(meters),
		"plans", len(plans),
		"contracts", len(contracts),
		"events", eventsCreated,
		"invoices", invoicesGenerated,
	)
	return nil
}
