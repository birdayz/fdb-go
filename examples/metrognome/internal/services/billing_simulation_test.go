package services_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/gomega"

	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
)

// TestMonthlyBillingSimulation simulates a complete billing month:
// - 3 customers on different plans (Starter, Pro, Enterprise)
// - Usage events spread across 30 days
// - Credits for some customers
// - Batch invoice generation at month end
// - Verify all invoice totals
func TestMonthlyBillingSimulation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)
	contractClient := metrognomev1connect.NewContractServiceClient(http.DefaultClient, testServer.URL)
	creditClient := metrognomev1connect.NewCreditServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)
	invoiceClient := metrognomev1connect.NewInvoiceServiceClient(http.DefaultClient, testServer.URL)

	// ── Setup meters ──
	_, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "sim_api_calls", Name: "Sim API Calls",
		AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// ── Setup plans ──

	// Starter: $29 flat + $0.01/call
	starterResp, _ := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Sim Starter"}))
	starterPlanID := starterResp.Msg.GetPlan().GetId()
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: starterPlanID, MeterSlug: "sim_api_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Flat{
			Flat: &metrognomev1.FlatPricing{AmountCents: 2900},
		}},
	}))
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: starterPlanID, MeterSlug: "sim_api_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{
			PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: 1},
		}},
	}))

	// Pro: $99 flat + tiered API
	proResp, _ := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Sim Pro"}))
	proPlanID := proResp.Msg.GetPlan().GetId()
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: proPlanID, MeterSlug: "sim_api_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Flat{
			Flat: &metrognomev1.FlatPricing{AmountCents: 9900},
		}},
	}))
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: proPlanID, MeterSlug: "sim_api_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Tiered{
			Tiered: &metrognomev1.TieredPricing{Tiers: []*metrognomev1.Tier{
				{UpTo: 100, PriceCents: 2},  // first 100 @ 2c
				{UpTo: 1000, PriceCents: 1}, // next 900 @ 1c
				{UpTo: 0, PriceCents: 0},    // free above 1000
			}},
		}},
	}))

	// ── Setup customers + contracts ──

	// Customer 1: Starter plan, 500 API calls, no credits
	cust1, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Sim Acme"}))
	cust1ID := cust1.Msg.GetCustomer().GetId()

	// Customer 2: Pro plan, 2000 API calls, $50 credit
	cust2, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Sim Globex"}))
	cust2ID := cust2.Msg.GetCustomer().GetId()

	// Customer 3: Starter plan, 0 API calls (inactive month)
	cust3, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Sim Dormant"}))
	cust3ID := cust3.Msg.GetCustomer().GetId()

	periodStart := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	_, _ = contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: cust1ID, PlanId: starterPlanID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	ctr2, _ := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: cust2ID, PlanId: proPlanID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	_ = ctr2
	_, _ = contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: cust3ID, PlanId: starterPlanID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))

	// ── Grant credit to customer 2 ──
	_, err = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: cust2ID, AmountCents: 5000, Priority: 1, // $50
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// ── Ingest usage events ──

	// Customer 1: 500 API calls (value=1 each)
	ts := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	events1 := make([]*metrognomev1.Event, 500)
	for i := range events1 {
		events1[i] = &metrognomev1.Event{
			CustomerId: cust1ID, EventType: "sim_api_calls", TimestampMs: ts + int64(i),
			Value: 1, IdempotencyKey: fmt.Sprintf("sim1-%d", i),
		}
	}
	resp1, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events1}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resp1.Msg.GetAccepted()).To(Equal(int32(500)))

	// Customer 2: 2000 API calls
	events2 := make([]*metrognomev1.Event, 200) // 200 events × value 10 = 2000 total
	for i := range events2 {
		events2[i] = &metrognomev1.Event{
			CustomerId: cust2ID, EventType: "sim_api_calls", TimestampMs: ts + int64(i),
			Value: 10, IdempotencyKey: fmt.Sprintf("sim2-%d", i),
		}
	}
	resp2, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events2}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resp2.Msg.GetAccepted()).To(Equal(int32(200)))

	// Customer 3: no events (dormant)

	// ── Verify usage ──

	usage1, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: cust1ID, MeterSlug: "sim_api_calls", StartMs: periodStart, EndMs: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(usage1.Msg.GetTotalValue()).To(Equal(int64(500)))

	usage2, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: cust2ID, MeterSlug: "sim_api_calls", StartMs: periodStart, EndMs: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(usage2.Msg.GetTotalValue()).To(Equal(int64(2000)))

	// ── Generate batch invoices ──
	// Use GenerateAllInvoices as of Feb 1 (after January period ended)
	batchResp, err := invoiceClient.GenerateAllInvoices(ctx, connect.NewRequest(&metrognomev1.GenerateAllInvoicesRequest{
		AsOf: periodEnd, // Feb 1
	}))
	g.Expect(err).NotTo(HaveOccurred())
	// At least our 3 contracts should generate invoices (other tests may have active contracts too)
	g.Expect(batchResp.Msg.GetGenerated()).To(BeNumerically(">=", int32(3)))

	// ── Verify invoices ──

	// Customer 1 (Starter, 500 calls):
	//   Flat: $29.00 = 2900 cents
	//   Per-unit: 500 × 1 cent = 500 cents
	//   Total: 3400 cents = $34.00
	inv1, err := invoiceClient.ListInvoices(ctx, connect.NewRequest(&metrognomev1.ListInvoicesRequest{CustomerId: cust1ID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inv1.Msg.GetInvoices()).To(HaveLen(1))
	g.Expect(inv1.Msg.GetInvoices()[0].GetSubtotalCents()).To(Equal(int64(3400)))
	g.Expect(inv1.Msg.GetInvoices()[0].GetTotalCents()).To(Equal(int64(3400))) // no credits

	// Customer 2 (Pro, 2000 calls, $50 credit):
	//   Flat: $99.00 = 9900 cents
	//   Tiered: 100×2 + 900×1 + 1000×0 = 200 + 900 = 1100 cents
	//   Subtotal: 9900 + 1100 = 11000 cents = $110.00
	//   Credit: -5000 cents = -$50.00
	//   Total: 6000 cents = $60.00
	inv2, err := invoiceClient.ListInvoices(ctx, connect.NewRequest(&metrognomev1.ListInvoicesRequest{CustomerId: cust2ID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inv2.Msg.GetInvoices()).To(HaveLen(1))
	g.Expect(inv2.Msg.GetInvoices()[0].GetSubtotalCents()).To(Equal(int64(11000)))
	g.Expect(inv2.Msg.GetInvoices()[0].GetCreditsAppliedCents()).To(Equal(int64(5000)))
	g.Expect(inv2.Msg.GetInvoices()[0].GetTotalCents()).To(Equal(int64(6000)))

	// Customer 3 (Starter, 0 calls):
	//   Flat: $29.00 = 2900 cents
	//   Per-unit: 0 × 1 cent = 0 cents
	//   Total: 2900 cents = $29.00
	inv3, err := invoiceClient.ListInvoices(ctx, connect.NewRequest(&metrognomev1.ListInvoicesRequest{CustomerId: cust3ID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inv3.Msg.GetInvoices()).To(HaveLen(1))
	g.Expect(inv3.Msg.GetInvoices()[0].GetSubtotalCents()).To(Equal(int64(2900)))
	g.Expect(inv3.Msg.GetInvoices()[0].GetTotalCents()).To(Equal(int64(2900)))

	// ── Verify credit depleted ──
	bal, err := creditClient.GetCreditBalance(ctx, connect.NewRequest(&metrognomev1.GetCreditBalanceRequest{CustomerId: cust2ID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(bal.Msg.GetTotalRemainingCents()).To(Equal(int64(0)))
}
