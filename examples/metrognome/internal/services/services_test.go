package services_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
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
	register(metrognomev1connect.NewEventServiceHandler(services.NewEventService(db.Events(), db.Alerts(), meterEngine)))
	register(metrognomev1connect.NewInvoiceServiceHandler(services.NewInvoiceService(db.Invoices(), db.Contracts(), billingEngine)))
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

	// Query usage broken down by group values
	groupResp, err := eventClient.GetUsageGroups(ctx, connect.NewRequest(&metrognomev1.GetUsageGroupsRequest{
		CustomerId: custID, MeterSlug: "svc_llm_tokens",
		StartMs: ts - 3600000, EndMs: ts + 3600000,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(groupResp.Msg.GetTotalValue()).To(Equal(int64(1800)))
	g.Expect(groupResp.Msg.GetGroups()).To(HaveLen(2)) // us-east-1/gpt-4 and eu-west-1/claude-4

	// Verify group values
	groupMap := make(map[string]int64) // "region:model" → value
	for _, g := range groupResp.Msg.GetGroups() {
		key := g.GetGroupValues()["region"] + ":" + g.GetGroupValues()["model"]
		groupMap[key] = g.GetValue()
	}
	g.Expect(groupMap["us-east-1:gpt-4"]).To(Equal(int64(800)))     // 500 + 300
	g.Expect(groupMap["eu-west-1:claude-4"]).To(Equal(int64(1000))) // 1000
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

// TestE2EAlertTriggering tests that alerts are automatically triggered when usage exceeds threshold.
func TestE2EAlertTriggering(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	alertClient := metrognomev1connect.NewAlertServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)

	// Create meter
	_, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "alert_test_calls", Name: "Alert Test Calls",
		AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Create customer
	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Alert Trigger Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	// Create alert: trigger at 50 usage
	createResp, err := alertClient.CreateAlert(ctx, connect.NewRequest(&metrognomev1.CreateAlertRequest{
		CustomerId: custID, MeterSlug: "alert_test_calls", Threshold: 50,
		AlertType: metrognomev1.AlertType_ALERT_TYPE_USAGE,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(createResp.Msg.GetAlert().GetTriggered()).To(BeFalse())

	// Ingest 30 events (below threshold) — alert should NOT trigger
	ts := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	events30 := make([]*metrognomev1.Event, 30)
	for i := range events30 {
		events30[i] = &metrognomev1.Event{
			CustomerId: custID, EventType: "alert_test_calls", TimestampMs: ts,
			Value: 1, IdempotencyKey: fmt.Sprintf("alert-30-%d", i),
		}
	}
	_, err = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events30}))
	g.Expect(err).NotTo(HaveOccurred())

	// Check: alert not triggered
	listResp, err := alertClient.ListAlerts(ctx, connect.NewRequest(&metrognomev1.ListAlertsRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(listResp.Msg.GetAlerts()).To(HaveLen(1))
	g.Expect(listResp.Msg.GetAlerts()[0].GetTriggered()).To(BeFalse())

	// Ingest 30 more events (total = 60, above threshold) — alert SHOULD trigger
	events30more := make([]*metrognomev1.Event, 30)
	for i := range events30more {
		events30more[i] = &metrognomev1.Event{
			CustomerId: custID, EventType: "alert_test_calls", TimestampMs: ts,
			Value: 1, IdempotencyKey: fmt.Sprintf("alert-60-%d", i),
		}
	}
	_, err = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events30more}))
	g.Expect(err).NotTo(HaveOccurred())

	// Check: alert triggered
	listResp2, err := alertClient.ListAlerts(ctx, connect.NewRequest(&metrognomev1.ListAlertsRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(listResp2.Msg.GetAlerts()[0].GetTriggered()).To(BeTrue())
}

// TestE2EInvoiceStatusTransitions tests the invoice status state machine.
func TestE2EInvoiceStatusTransitions(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)
	contractClient := metrognomev1connect.NewContractServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)
	invoiceClient := metrognomev1connect.NewInvoiceServiceClient(http.DefaultClient, testServer.URL)

	// Setup: meter + customer + plan + charge + contract + events + invoice
	_, _ = meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "ist_calls", Name: "IST Calls", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	custResp, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "IST Corp"}))
	custID := custResp.Msg.GetCustomer().GetId()
	planResp, _ := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "IST Plan"}))
	planID := planResp.Msg.GetPlan().GetId()
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: planID, MeterSlug: "ist_calls",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{
			PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: 1},
		}},
	}))
	periodStart := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contractResp, _ := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: custID, PlanId: planID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	contractID := contractResp.Msg.GetContract().GetId()
	ts := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	_, _ = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{
		Events: []*metrognomev1.Event{{
			CustomerId: custID, EventType: "ist_calls", TimestampMs: ts,
			Value: 10, IdempotencyKey: "ist-1",
		}},
	}))

	invResp, err := invoiceClient.GenerateInvoice(ctx, connect.NewRequest(&metrognomev1.GenerateInvoiceRequest{
		ContractId: contractID, PeriodStart: periodStart, PeriodEnd: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	invoiceID := invResp.Msg.GetInvoice().GetId()
	g.Expect(invResp.Msg.GetInvoice().GetStatus()).To(Equal(metrognomev1.InvoiceStatus_INVOICE_STATUS_DRAFT))

	// DRAFT → ISSUED (valid)
	updateResp, err := invoiceClient.UpdateInvoiceStatus(ctx, connect.NewRequest(&metrognomev1.UpdateInvoiceStatusRequest{
		Id: invoiceID, Status: metrognomev1.InvoiceStatus_INVOICE_STATUS_ISSUED,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updateResp.Msg.GetInvoice().GetStatus()).To(Equal(metrognomev1.InvoiceStatus_INVOICE_STATUS_ISSUED))
	g.Expect(updateResp.Msg.GetInvoice().GetFinalizedAt()).To(BeNumerically(">", 0))

	// ISSUED → PAID (valid)
	updateResp, err = invoiceClient.UpdateInvoiceStatus(ctx, connect.NewRequest(&metrognomev1.UpdateInvoiceStatusRequest{
		Id: invoiceID, Status: metrognomev1.InvoiceStatus_INVOICE_STATUS_PAID,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updateResp.Msg.GetInvoice().GetStatus()).To(Equal(metrognomev1.InvoiceStatus_INVOICE_STATUS_PAID))

	// PAID → ISSUED (invalid — terminal state)
	_, err = invoiceClient.UpdateInvoiceStatus(ctx, connect.NewRequest(&metrognomev1.UpdateInvoiceStatusRequest{
		Id: invoiceID, Status: metrognomev1.InvoiceStatus_INVOICE_STATUS_ISSUED,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Test VOID transition: create another invoice and void it from DRAFT
	_, _ = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{
		Events: []*metrognomev1.Event{{
			CustomerId: custID, EventType: "ist_calls", TimestampMs: ts,
			Value: 5, IdempotencyKey: "ist-2",
		}},
	}))
	// Use a different period to avoid duplicate invoice ID
	periodStart2 := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd2 := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	invResp2, err := invoiceClient.GenerateInvoice(ctx, connect.NewRequest(&metrognomev1.GenerateInvoiceRequest{
		ContractId: contractID, PeriodStart: periodStart2, PeriodEnd: periodEnd2,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	invoiceID2 := invResp2.Msg.GetInvoice().GetId()

	// DRAFT → VOID (valid)
	updateResp, err = invoiceClient.UpdateInvoiceStatus(ctx, connect.NewRequest(&metrognomev1.UpdateInvoiceStatusRequest{
		Id: invoiceID2, Status: metrognomev1.InvoiceStatus_INVOICE_STATUS_VOID,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updateResp.Msg.GetInvoice().GetStatus()).To(Equal(metrognomev1.InvoiceStatus_INVOICE_STATUS_VOID))
}

// TestE2EAlertWebhook tests that webhook is delivered when alert triggers.
func TestE2EAlertWebhook(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	// Set up a webhook receiver
	var mu sync.Mutex
	var webhookReceived bool
	var webhookPayload map[string]any
	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewDecoder(r.Body).Decode(&webhookPayload)
		webhookReceived = true
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookServer.Close()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	alertClient := metrognomev1connect.NewAlertServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)

	_, _ = meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "webhook_test", Name: "Webhook Test",
		AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))

	custResp, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Webhook Corp"}))
	custID := custResp.Msg.GetCustomer().GetId()

	// Create alert with webhook URL, threshold 10
	_, err := alertClient.CreateAlert(ctx, connect.NewRequest(&metrognomev1.CreateAlertRequest{
		CustomerId: custID, MeterSlug: "webhook_test", Threshold: 10,
		AlertType:  metrognomev1.AlertType_ALERT_TYPE_USAGE,
		WebhookUrl: webhookServer.URL,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	// Ingest 20 events (above threshold)
	ts := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	events := make([]*metrognomev1.Event, 20)
	for i := range events {
		events[i] = &metrognomev1.Event{
			CustomerId: custID, EventType: "webhook_test", TimestampMs: ts,
			Value: 1, IdempotencyKey: fmt.Sprintf("wh-%d", i),
		}
	}
	_, err = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events}))
	g.Expect(err).NotTo(HaveOccurred())

	// Wait for webhook delivery (async)
	g.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return webhookReceived
	}, 15*time.Second, 100*time.Millisecond).Should(BeTrue())

	// Verify webhook payload
	mu.Lock()
	defer mu.Unlock()
	g.Expect(webhookPayload["alert_type"]).To(Equal("usage"))
	g.Expect(webhookPayload["customer_id"]).To(Equal(custID))
	g.Expect(webhookPayload["meter_slug"]).To(Equal("webhook_test"))
}

// TestE2EExpiredCreditsNotApplied verifies expired credits are skipped during invoicing.
func TestE2EExpiredCreditsNotApplied(t *testing.T) {
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

	_, _ = meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "exp_credit_test", Name: "Exp Credit", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	custResp, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Expired Credit Corp"}))
	custID := custResp.Msg.GetCustomer().GetId()
	planResp, _ := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Exp Plan"}))
	planID := planResp.Msg.GetPlan().GetId()
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: planID, MeterSlug: "exp_credit_test",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{
			PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: 100},
		}},
	}))

	periodStart := time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2028, 4, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contractResp, _ := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: custID, PlanId: planID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	contractID := contractResp.Msg.GetContract().GetId()

	// Grant an EXPIRED credit ($50.00, expired yesterday)
	_, _ = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId:  custID,
		AmountCents: 5000,
		Priority:    1,
		ExpiresAt:   time.Now().Add(-24 * time.Hour).UnixMilli(), // expired
	}))

	// Ingest 10 events → $10.00
	ts := time.Date(2028, 3, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	events := make([]*metrognomev1.Event, 10)
	for i := range events {
		events[i] = &metrognomev1.Event{
			CustomerId: custID, EventType: "exp_credit_test", TimestampMs: ts,
			Value: 1, IdempotencyKey: fmt.Sprintf("exp-%d", i),
		}
	}
	_, _ = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events}))

	// Generate invoice — expired credit should NOT be applied
	invResp, err := invoiceClient.GenerateInvoice(ctx, connect.NewRequest(&metrognomev1.GenerateInvoiceRequest{
		ContractId: contractID, PeriodStart: periodStart, PeriodEnd: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invResp.Msg.GetInvoice().GetSubtotalCents()).To(Equal(int64(1000)))
	g.Expect(invResp.Msg.GetInvoice().GetCreditsAppliedCents()).To(Equal(int64(0))) // expired, not applied
	g.Expect(invResp.Msg.GetInvoice().GetTotalCents()).To(Equal(int64(1000)))
}

// TestE2EMultipleCreditPriority verifies credits are applied in priority order.
func TestE2EMultipleCreditPriority(t *testing.T) {
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

	_, _ = meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "multi_credit_test", Name: "Multi Credit", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	custResp, _ := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Multi Credit Corp"}))
	custID := custResp.Msg.GetCustomer().GetId()
	planResp, _ := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Multi Plan"}))
	planID := planResp.Msg.GetPlan().GetId()
	_, _ = planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: planID, MeterSlug: "multi_credit_test",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{
			PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: 100},
		}},
	}))

	periodStart := time.Date(2028, 5, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	periodEnd := time.Date(2028, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	contractResp, _ := contractClient.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: custID, PlanId: planID, StartAt: periodStart,
		BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	contractID := contractResp.Msg.GetContract().GetId()

	// Grant 3 credits with different priorities
	// Priority 1: $3.00 (applied first)
	_, _ = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: custID, AmountCents: 300, Priority: 1,
	}))
	// Priority 2: $5.00 (applied second)
	_, _ = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: custID, AmountCents: 500, Priority: 2,
	}))
	// Priority 3: $10.00 (applied third, partially)
	_, _ = creditClient.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: custID, AmountCents: 1000, Priority: 3,
	}))

	// Ingest 10 events → $10.00 subtotal
	ts := time.Date(2028, 5, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	events := make([]*metrognomev1.Event, 10)
	for i := range events {
		events[i] = &metrognomev1.Event{
			CustomerId: custID, EventType: "multi_credit_test", TimestampMs: ts,
			Value: 1, IdempotencyKey: fmt.Sprintf("mc-%d", i),
		}
	}
	_, _ = eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events}))

	// Generate invoice: $10.00 - $3.00 - $5.00 - $2.00 (partial from priority 3) = $0.00
	invResp, err := invoiceClient.GenerateInvoice(ctx, connect.NewRequest(&metrognomev1.GenerateInvoiceRequest{
		ContractId: contractID, PeriodStart: periodStart, PeriodEnd: periodEnd,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invResp.Msg.GetInvoice().GetSubtotalCents()).To(Equal(int64(1000)))
	g.Expect(invResp.Msg.GetInvoice().GetCreditsAppliedCents()).To(Equal(int64(1000))) // all 3 credits cover $10
	g.Expect(invResp.Msg.GetInvoice().GetTotalCents()).To(Equal(int64(0)))

	// Verify remaining credit balances
	balResp, err := creditClient.GetCreditBalance(ctx, connect.NewRequest(&metrognomev1.GetCreditBalanceRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(balResp.Msg.GetTotalRemainingCents()).To(Equal(int64(800))) // priority 3 has $8.00 left
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

// TestE2ECustomerPagination tests listing customers with continuation-based pagination.
func TestE2ECustomerPagination(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)

	// Create 5 customers with a unique prefix to avoid collisions with other tests
	for i := 0; i < 5; i++ {
		_, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{
			Name:       fmt.Sprintf("Pagination Corp %d", i),
			ExternalId: fmt.Sprintf("page-%d", i),
		}))
		g.Expect(err).NotTo(HaveOccurred())
	}

	// List all (no page size) — should get at least 5
	resp, err := customerClient.ListCustomers(ctx, connect.NewRequest(&metrognomev1.ListCustomersRequest{}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(len(resp.Msg.GetCustomers())).To(BeNumerically(">=", 5))

	// List with page size 2 — should get exactly 2 + continuation
	resp, err = customerClient.ListCustomers(ctx, connect.NewRequest(&metrognomev1.ListCustomersRequest{PageSize: 2}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resp.Msg.GetCustomers()).To(HaveLen(2))
	g.Expect(resp.Msg.GetContinuation()).NotTo(BeEmpty())

	// Follow continuation — should get more
	resp2, err := customerClient.ListCustomers(ctx, connect.NewRequest(&metrognomev1.ListCustomersRequest{
		PageSize: 2, Continuation: resp.Msg.GetContinuation(),
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resp2.Msg.GetCustomers()).To(HaveLen(2))

	// Ensure pages don't overlap
	page1IDs := map[string]bool{}
	for _, c := range resp.Msg.GetCustomers() {
		page1IDs[c.GetId()] = true
	}
	for _, c := range resp2.Msg.GetCustomers() {
		g.Expect(page1IDs).NotTo(HaveKey(c.GetId()))
	}
}

// TestE2EMeterAndPlanListing tests ListMeters and ListPlans RPCs.
func TestE2EMeterAndPlanListing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)

	// Create meters
	for i := 0; i < 3; i++ {
		_, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
			Slug: fmt.Sprintf("list_meter_%d", i), Name: fmt.Sprintf("List Meter %d", i),
			AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
		}))
		g.Expect(err).NotTo(HaveOccurred())
	}

	// List meters
	meterResp, err := meterClient.ListMeters(ctx, connect.NewRequest(&metrognomev1.ListMetersRequest{}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(len(meterResp.Msg.GetMeters())).To(BeNumerically(">=", 3))

	// Get meter by slug
	getResp, err := meterClient.GetMeter(ctx, connect.NewRequest(&metrognomev1.GetMeterRequest{Slug: "list_meter_0"}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getResp.Msg.GetMeter().GetName()).To(Equal("List Meter 0"))

	// Create plans
	for i := 0; i < 3; i++ {
		_, err := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{
			Name: fmt.Sprintf("List Plan %d", i),
		}))
		g.Expect(err).NotTo(HaveOccurred())
	}

	// List plans
	planResp, err := planClient.ListPlans(ctx, connect.NewRequest(&metrognomev1.ListPlansRequest{}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(len(planResp.Msg.GetPlans())).To(BeNumerically(">=", 3))
}

// TestE2EWindowedUsage tests GetUsage with HOUR and DAY window sizes.
func TestE2EWindowedUsage(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)
	meterClient := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)
	eventClient := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)

	// Setup
	_, err := meterClient.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "windowed_calls", Name: "Windowed Calls",
		AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).NotTo(HaveOccurred())

	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Windowed Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	// Ingest events across 3 different hours on the same day
	baseTime := time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC) // 10:00 UTC
	hour1 := baseTime.UnixMilli()
	hour2 := baseTime.Add(1 * time.Hour).UnixMilli()
	hour3 := baseTime.Add(2 * time.Hour).UnixMilli()

	events := []*metrognomev1.Event{
		// Hour 1: 3 events, value 10 each = 30
		{CustomerId: custID, EventType: "windowed_calls", TimestampMs: hour1, Value: 10, IdempotencyKey: "win-h1-1"},
		{CustomerId: custID, EventType: "windowed_calls", TimestampMs: hour1 + 1000, Value: 10, IdempotencyKey: "win-h1-2"},
		{CustomerId: custID, EventType: "windowed_calls", TimestampMs: hour1 + 2000, Value: 10, IdempotencyKey: "win-h1-3"},
		// Hour 2: 2 events, value 20 each = 40
		{CustomerId: custID, EventType: "windowed_calls", TimestampMs: hour2, Value: 20, IdempotencyKey: "win-h2-1"},
		{CustomerId: custID, EventType: "windowed_calls", TimestampMs: hour2 + 1000, Value: 20, IdempotencyKey: "win-h2-2"},
		// Hour 3: 1 event, value 50 = 50
		{CustomerId: custID, EventType: "windowed_calls", TimestampMs: hour3, Value: 50, IdempotencyKey: "win-h3-1"},
	}

	ingestResp, err := eventClient.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{Events: events}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ingestResp.Msg.GetAccepted()).To(Equal(int32(6)))

	// Query total (no window)
	usageResp, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: custID, MeterSlug: "windowed_calls",
		StartMs: hour1, EndMs: hour3 + 3600000,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(usageResp.Msg.GetTotalValue()).To(Equal(int64(120))) // 30+40+50

	// Query with hourly windows — should get 3 buckets
	hourlyResp, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: custID, MeterSlug: "windowed_calls",
		StartMs: hour1, EndMs: hour3 + 3600000,
		WindowSize: metrognomev1.WindowSize_WINDOW_SIZE_HOUR,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hourlyResp.Msg.GetTotalValue()).To(Equal(int64(120)))
	g.Expect(hourlyResp.Msg.GetBuckets()).To(HaveLen(3))

	// Verify bucket values
	bucketValues := make(map[int64]int64)
	for _, b := range hourlyResp.Msg.GetBuckets() {
		bucketValues[b.GetStartMs()] = b.GetValue()
	}
	g.Expect(bucketValues[hour1]).To(Equal(int64(30)))
	g.Expect(bucketValues[hour2]).To(Equal(int64(40)))
	g.Expect(bucketValues[hour3]).To(Equal(int64(50)))

	// Query with DAY window — all same day, should get 1 bucket with total 120
	dayResp, err := eventClient.GetUsage(ctx, connect.NewRequest(&metrognomev1.GetUsageRequest{
		CustomerId: custID, MeterSlug: "windowed_calls",
		StartMs: hour1, EndMs: hour3 + 3600000,
		WindowSize: metrognomev1.WindowSize_WINDOW_SIZE_DAY,
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(dayResp.Msg.GetTotalValue()).To(Equal(int64(120)))
	g.Expect(dayResp.Msg.GetBuckets()).To(HaveLen(1))
	g.Expect(dayResp.Msg.GetBuckets()[0].GetValue()).To(Equal(int64(120)))
}

// TestE2EListCharges tests listing charges for a plan.
func TestE2EListCharges(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	planClient := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)

	planResp, err := planClient.CreatePlan(ctx, connect.NewRequest(&metrognomev1.CreatePlanRequest{Name: "Charges Plan"}))
	g.Expect(err).NotTo(HaveOccurred())
	planID := planResp.Msg.GetPlan().GetId()

	// Add 3 charges
	for i := 0; i < 3; i++ {
		_, err := planClient.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
			PlanId: planID, MeterSlug: fmt.Sprintf("charges_meter_%d", i),
			Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{
				PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: int64(i + 1)},
			}},
		}))
		g.Expect(err).NotTo(HaveOccurred())
	}

	// List charges
	listResp, err := planClient.ListCharges(ctx, connect.NewRequest(&metrognomev1.ListChargesRequest{PlanId: planID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(listResp.Msg.GetCharges()).To(HaveLen(3))
}

// TestE2EInvoiceListing tests listing invoices for a customer.
func TestE2EInvoiceListing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	invoiceClient := metrognomev1connect.NewInvoiceServiceClient(http.DefaultClient, testServer.URL)
	customerClient := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)

	custResp, err := customerClient.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: "Invoice List Corp"}))
	g.Expect(err).NotTo(HaveOccurred())
	custID := custResp.Msg.GetCustomer().GetId()

	// List invoices (should be empty for new customer)
	listResp, err := invoiceClient.ListInvoices(ctx, connect.NewRequest(&metrognomev1.ListInvoicesRequest{CustomerId: custID}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(listResp.Msg.GetInvoices()).To(BeEmpty())
}
