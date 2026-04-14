package services_test

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	. "github.com/onsi/gomega"

	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
)

func TestValidateCreateCustomer(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewCustomerServiceClient(http.DefaultClient, testServer.URL)

	// Empty name
	_, err := client.CreateCustomer(ctx, connect.NewRequest(&metrognomev1.CreateCustomerRequest{Name: ""}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
}

func TestValidateCreateMeter(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewMeterServiceClient(http.DefaultClient, testServer.URL)

	// Empty slug
	_, err := client.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Name: "Test", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Invalid slug (uppercase)
	_, err = client.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "INVALID", Name: "Test", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Invalid slug (starts with number)
	_, err = client.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "1bad", Name: "Test", AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_SUM,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Missing aggregation type
	_, err = client.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "valid_slug", Name: "Test",
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Valid slug
	_, err = client.CreateMeter(ctx, connect.NewRequest(&metrognomev1.CreateMeterRequest{
		Slug: "valid_test_meter", Name: "Valid",
		AggregationType: metrognomev1.AggregationType_AGGREGATION_TYPE_COUNT,
	}))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateAddCharge(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewPlanServiceClient(http.DefaultClient, testServer.URL)

	// Missing plan_id
	_, err := client.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		MeterSlug: "test",
		Pricing:   &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Flat{Flat: &metrognomev1.FlatPricing{AmountCents: 100}}},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Missing pricing
	_, err = client.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: "plan-1", MeterSlug: "test",
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Negative flat amount
	_, err = client.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: "plan-1", MeterSlug: "test",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Flat{
			Flat: &metrognomev1.FlatPricing{AmountCents: -100},
		}},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Zero package size
	_, err = client.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: "plan-1", MeterSlug: "test",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Package{
			Package: &metrognomev1.PackagePricing{PackageSize: 0, PackagePriceCents: 100},
		}},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Empty tiered
	_, err = client.AddCharge(ctx, connect.NewRequest(&metrognomev1.AddChargeRequest{
		PlanId: "plan-1", MeterSlug: "test",
		Pricing: &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Tiered{
			Tiered: &metrognomev1.TieredPricing{},
		}},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
}

func TestValidateCreateContract(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewContractServiceClient(http.DefaultClient, testServer.URL)

	// Missing customer_id
	_, err := client.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		PlanId: "plan-1", StartAt: 1000, BillingPeriod: metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Missing billing_period
	_, err = client.CreateContract(ctx, connect.NewRequest(&metrognomev1.CreateContractRequest{
		CustomerId: "cust-1", PlanId: "plan-1", StartAt: 1000,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
}

func TestValidateIngestEvents(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewEventServiceClient(http.DefaultClient, testServer.URL)

	// Empty events
	_, err := client.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Missing idempotency_key
	_, err = client.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{
		Events: []*metrognomev1.Event{{CustomerId: "cust-1", EventType: "test"}},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Missing customer_id
	_, err = client.IngestEvents(ctx, connect.NewRequest(&metrognomev1.IngestEventsRequest{
		Events: []*metrognomev1.Event{{EventType: "test", IdempotencyKey: "key-1"}},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
}

func TestValidateGrantCredit(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewCreditServiceClient(http.DefaultClient, testServer.URL)

	// Missing customer_id
	_, err := client.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		AmountCents: 100,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Zero amount
	_, err = client.GrantCredit(ctx, connect.NewRequest(&metrognomev1.GrantCreditRequest{
		CustomerId: "cust-1", AmountCents: 0,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
}

func TestValidateCreateAlert(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
	client := metrognomev1connect.NewAlertServiceClient(http.DefaultClient, testServer.URL)

	// Missing meter_slug
	_, err := client.CreateAlert(ctx, connect.NewRequest(&metrognomev1.CreateAlertRequest{
		CustomerId: "cust-1", Threshold: 100,
		AlertType: metrognomev1.AlertType_ALERT_TYPE_USAGE,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

	// Zero threshold
	_, err = client.CreateAlert(ctx, connect.NewRequest(&metrognomev1.CreateAlertRequest{
		CustomerId: "cust-1", MeterSlug: "test", Threshold: 0,
		AlertType: metrognomev1.AlertType_ALERT_TYPE_USAGE,
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
}
