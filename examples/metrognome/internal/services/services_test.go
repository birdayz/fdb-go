package services_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/gomega"

	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/services"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var testServer *httptest.Server

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

	tmpFile, err := os.CreateTemp("", "svc_test_*.txt")
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
	recordDB := rl.NewFDBDatabase(fdbDB)

	db, err := storage.NewDB(recordDB)
	if err != nil {
		panic("failed to init storage: " + err.Error())
	}

	meterEngine := meter.NewEngine(recordDB, subspace.Sub("svc_test_meters"))
	billingEngine := billing.NewEngine(recordDB, db.MetaData(), db.Subspace())

	mux := http.NewServeMux()
	register := func(path string, handler http.Handler) { mux.Handle(path, handler) }

	register(metrognomev1connect.NewCustomerServiceHandler(services.NewCustomerService(db.Customers())))
	register(metrognomev1connect.NewMeterServiceHandler(services.NewMeterService(db.Meters(), meterEngine)))
	register(metrognomev1connect.NewPlanServiceHandler(services.NewPlanService(db.Plans(), db.Charges())))
	register(metrognomev1connect.NewContractServiceHandler(services.NewContractService(db.Contracts())))
	register(metrognomev1connect.NewEventServiceHandler(services.NewEventService(db.Events(), meterEngine)))
	register(metrognomev1connect.NewInvoiceServiceHandler(services.NewInvoiceService(db.Invoices(), billingEngine)))
	register(metrognomev1connect.NewCreditServiceHandler(services.NewCreditService(db.Credits())))
	register(metrognomev1connect.NewAlertServiceHandler(services.NewAlertService(db.Alerts())))

	testServer = httptest.NewServer(mux)

	code := m.Run()

	testServer.Close()
	_ = container.Terminate(context.Background())
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

func TestE2EBillingFlow(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)
	contractClient := metrognomev1connect.NewContractServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)
	invoiceClient := metrognomev1connect.NewInvoiceServiceClient(http.DefaultClient, testServer.URL)

	// 1. Create meter
	meterResp, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug:            "svc_api_calls",
		Name:            "API Calls",
		AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meterResp.Msg.GetMeter().GetSlug()).To(Equal("svc_api_calls"))

	// 2. Create customer
	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{
		Name:       "Billing Flow Corp",
		ExternalId: "ext-bf-1",
	}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	// 3. Create plan + charge
	planResp, err := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{
		Name: "API Plan",
	}))
	g.Expect(err).NotTo(HaveOccurred())
	planID := planResp.Msg.GetPlan().GetId()

	_, err = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId:    planID,
		MeterSlug: "svc_api_calls",
		Pricing: &metrognomev1.PricingModel{
			Model: &metrognomev1.PricingModel_PerUnit{
				PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: 5},
			},
		},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// 4. Create contract
	periodStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contractResp, err := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId:    custID,
		PlanId:        planID,
		StartAt:       periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	contractID := contractResp.Msg.GetContract().GetId()

	// 5. Ingest 200 events
	ts := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	events := make([]*metrognomev1.Event, 200)
	for i := range events {
		events[i] = &metrognomev1.Event{
			CustomerId:     custID,
			EventType:      "svc_api_calls",
			TimestampMs:    ts + int64(i*100),
			Value:          1,
			IdempotencyKey: fmt.Sprintf("svc-flow-%d", i),
		}
	}

	ingestResp, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ingestResp.Msg.GetAccepted()).To(Equal(int32(200)))

	// 5b. Dedup check
	reingestResp, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events[:5]}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(reingestResp.Msg.GetDuplicates()).To(Equal(int32(5)))

	// 6. Query usage
	usageResp, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: custID, MeterSlug: "svc_api_calls", StartMs: periodStart, EndMs: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(usageResp.Msg.GetTotalValue()).To(Equal(int64(200)))

	// 7. Generate invoice: 200 × $0.05 = $10.00
	invoiceResp, err := invoiceClient.GenerateInvoice(ctx, connect.NewRequest(&metrognomev1.GenerateInvoiceRequest{
		ContractId: contractID, PeriodStart: periodStart, PeriodEnd: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invoiceResp.Msg.GetInvoice().GetTotalCents()).To(Equal(int64(1000)))
	g.Expect(invoiceResp.Msg.GetInvoice().GetLineItems()[0].GetQuantity()).To(Equal(int64(200)))
}

func TestE2EWithGroupBy(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)
	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)

	// Meter with group-by
	_, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug:              "svc_llm_tokens",
		Name:              "LLM Tokens",
		AggregationType:   metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
		GroupByProperties: []string{"region", "model"},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "GroupBy Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	ts := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	events := []*metrognomev1.Event{
		{
			CustomerId: custID, EventType: "svc_llm_tokens", TimestampMs: ts, Value: 500,
			IdempotencyKey: "svc-gb-1", PropertiesJson: `{"region":"us-east-1","model":"gpt-4"}`,
		},
		{
			CustomerId: custID, EventType: "svc_llm_tokens", TimestampMs: ts + 1, Value: 300,
			IdempotencyKey: "svc-gb-2", PropertiesJson: `{"region":"us-east-1","model":"gpt-4"}`,
		},
		{
			CustomerId: custID, EventType: "svc_llm_tokens", TimestampMs: ts + 2, Value: 1000,
			IdempotencyKey: "svc-gb-3", PropertiesJson: `{"region":"eu-west-1","model":"claude-4"}`,
		},
	}

	ingestResp, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ingestResp.Msg.GetAccepted()).To(Equal(int32(3)))

	// Total usage: 500 + 300 + 1000 = 1800
	usageResp, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: custID, MeterSlug: "svc_llm_tokens", StartMs: ts - 3600000, EndMs: ts + 3600000,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(usageResp.Msg.GetTotalValue()).To(Equal(int64(1800)))
}

func TestE2ECreditFlow(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	creditClient := metrognomev1connect.NewCreditServiceClient(http.DefaultClient, testServer.URL)

	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Credit Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	_, err = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: custID, AmountCents: 5000, Priority: 1,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	_, err = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: custID, AmountCents: 3000, Priority: 2,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	balResp, err := creditClient.GetCreditBalance(ctx, connect.NewRequest(&metrognomev1.GetCreditBalanceRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(balResp.Msg.GetTotalRemainingCents()).To(Equal(int64(8000)))
	g.Expect(balResp.Msg.GetCredits()).To(HaveLen(2))
}

func TestE2EContractLifecycle(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	contractClient := metrognomev1connect.NewContractServiceClient(http.DefaultClient, testServer.URL)
	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)

	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Contract Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	planResp, err := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Lifecycle Plan"}))
	g.Expect(err).NotTo(HaveOccurred())
	planID := planResp.Msg.GetPlan().GetId()

	// Create contract
	now := time.Now().UnixMilli()
	createResp, err := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: custID, PlanId: planID, StartAt: now,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	contractID := createResp.Msg.GetContract().GetId()
	g.Expect(createResp.Msg.GetContract().GetActive()).To(BeTrue())

	// Get contract
	getResp, err := contractClient.GetContract(ctx, connect.NewRequest(&metrognomev1.GetContractRequest{Id: contractID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getResp.Msg.GetContract().GetCustomerId()).To(Equal(custID))

	// List contracts by customer
	listResp, err := contractClient.ListContracts(ctx, connect.NewRequest(&metrognomev1.ListContractsRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(listResp.Msg.GetContracts()).To(HaveLen(1))

	// End contract
	endResp, err := contractClient.EndContract(ctx, connect.NewRequest(&metrognomev1.EndContractRequest{Id: contractID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(endResp.Msg.GetContract().GetActive()).To(BeFalse())
	g.Expect(endResp.Msg.GetContract().GetEndAt()).To(BeNumerically(">", 0))

	// Verify ended
	getResp2, err := contractClient.GetContract(ctx, connect.NewRequest(&metrognomev1.GetContractRequest{Id: contractID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getResp2.Msg.GetContract().GetActive()).To(BeFalse())
}

func TestE2EAlertCRUD(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	alertClient := metrognomev1connect.NewAlertServiceClient(http.DefaultClient, testServer.URL)

	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Alert Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	// Create alert
	createResp, err := alertClient.CreateAlert(ctx, connect.NewRequest(&metrognomev1.CreateAlertRequest{
		CustomerId: custID, MeterSlug: "api_calls", Threshold: 10000,
		AlertType: metrognomev1.AlertType_ALERT_TYPE_USAGE,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(createResp.Msg.GetAlert().GetThreshold()).To(Equal(int64(10000)))
	g.Expect(createResp.Msg.GetAlert().GetTriggered()).To(BeFalse())

	// Create second alert
	_, err = alertClient.CreateAlert(ctx, connect.NewRequest(&metrognomev1.CreateAlertRequest{
		CustomerId: custID, MeterSlug: "api_calls", Threshold: 50000,
		AlertType: metrognomev1.AlertType_ALERT_TYPE_SPEND,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// List alerts
	listResp, err := alertClient.ListAlerts(ctx, connect.NewRequest(&metrognomev1.ListAlertsRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(listResp.Msg.GetAlerts()).To(HaveLen(2))
}

// TestE2EMultiChargeInvoice tests invoice generation with multiple charges on one plan.
func TestE2EMultiChargeInvoice(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)
	contractClient := metrognomev1connect.NewContractServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)
	invoiceClient := metrognomev1connect.NewInvoiceServiceClient(http.DefaultClient, testServer.URL)

	// Create meters
	_, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "mc_api_calls", Name: "API Calls", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	_, err = meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "mc_storage_gb", Name: "Storage GB", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Create customer
	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Multi Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	// Create plan with 3 charges: flat + per-unit API + tiered storage
	planResp, err := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Enterprise"}))
	g.Expect(err).NotTo(HaveOccurred())
	planID := planResp.Msg.GetPlan().GetId()

	// Charge 1: $99 flat platform fee
	_, err = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: planID, MeterSlug: "mc_api_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Flat{
			Flat: &metrognomev1.FlatPricing{AmountCents: 9900},
		}},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Charge 2: $0.01 per API call
	_, err = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: planID, MeterSlug: "mc_api_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{
			PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: 1},
		}},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Charge 3: Tiered storage — first 10GB @ $0.10/GB, next 90 @ $0.05, rest @ $0.02
	_, err = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: planID, MeterSlug: "mc_storage_gb",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Tiered{
			Tiered: &metrognomev1.TieredPricing{Tiers: []*metrognomev1.Tier{
				{UpTo: 10, PriceCents: 10},
				{UpTo: 100, PriceCents: 5},
				{UpTo: 0, PriceCents: 2},
			}},
		}},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Create contract
	periodStart := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contractResp, err := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: custID, PlanId: planID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	contractID := contractResp.Msg.GetContract().GetId()

	// Ingest API calls: 500 events with value 1 each
	ts := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	apiEvents := make([]*metrognomev1.Event, 500)
	for i := range apiEvents {
		apiEvents[i] = &metrognomev1.Event{
			CustomerId: custID, EventType: "mc_api_calls", TimestampMs: ts + int64(i),
			Value: 1, IdempotencyKey: fmt.Sprintf("mc-api-%d", i),
		}
	}
	ingestResp, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: apiEvents}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ingestResp.Msg.GetAccepted()).To(Equal(int32(500)))

	// Ingest storage: 50 GB (single event with value 50)
	_, err = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{
		Events: []*metrognomev1.Event{{
			CustomerId: custID, EventType: "mc_storage_gb", TimestampMs: ts,
			Value: 50, IdempotencyKey: "mc-storage-1",
		}},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Generate invoice
	// Expected:
	//   Charge 1 (flat): $99.00 = 9900 cents
	//   Charge 2 (per-unit API): 500 * 1 cent = 500 cents
	//   Charge 3 (tiered storage): 10*10 + 40*5 = 100 + 200 = 300 cents
	//   Total: 9900 + 500 + 300 = 10700 cents = $107.00
	invoiceResp, err := invoiceClient.GenerateInvoice(ctx, connect.NewRequest(&metrognomev1.GenerateInvoiceRequest{
		ContractId: contractID, PeriodStart: periodStart, PeriodEnd: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	invoice := invoiceResp.Msg.GetInvoice()
	g.Expect(invoice.GetLineItems()).To(HaveLen(3))
	g.Expect(invoice.GetTotalCents()).To(Equal(int64(10700)))
}

func TestE2ECustomerNotFound(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)

	_, err := customerClient.GetCustomer(ctx, connect.NewRequest(&metrognomev1.GetCustomerRequest{Id: "nonexistent"}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
}
