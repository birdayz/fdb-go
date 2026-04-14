package services

import (
	"fmt"
	"regexp"

	"connectrpc.com/connect"

	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
)

var slugRegex = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)

func validateCreateCustomer(req *metrognomev1.CreateCustomerRequest) *connect.Error {
	if req.GetName() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	return nil
}

func validateCreateMeter(req *metrognomev1.CreateMeterRequest) *connect.Error {
	if req.GetSlug() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("slug is required"))
	}
	if !slugRegex.MatchString(req.GetSlug()) {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("slug must be lowercase alphanumeric with underscores, 2-63 chars, starting with a letter"))
	}
	if req.GetName() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	if req.GetAggregationType() == metrognomev1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("aggregation_type is required"))
	}
	return nil
}

func validateCreatePlan(req *metrognomev1.CreatePlanRequest) *connect.Error {
	if req.GetName() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	return nil
}

func validateAddCharge(req *metrognomev1.AddChargeRequest) *connect.Error {
	if req.GetPlanId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("plan_id is required"))
	}
	if req.GetMeterSlug() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("meter_slug is required"))
	}
	if req.GetPricing() == nil || req.GetPricing().GetModel() == nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pricing model is required"))
	}
	return validatePricingModel(req.GetPricing())
}

func validatePricingModel(p *metrognomev1.PricingModel) *connect.Error {
	switch m := p.GetModel().(type) {
	case *metrognomev1.PricingModel_Flat:
		if m.Flat.GetAmountCents() < 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("flat amount_cents must be >= 0"))
		}
	case *metrognomev1.PricingModel_PerUnit:
		if m.PerUnit.GetUnitPriceCents() < 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("per_unit price must be >= 0"))
		}
	case *metrognomev1.PricingModel_Tiered:
		if len(m.Tiered.GetTiers()) == 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("tiered pricing must have at least one tier"))
		}
	case *metrognomev1.PricingModel_Volume:
		if len(m.Volume.GetTiers()) == 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("volume pricing must have at least one tier"))
		}
	case *metrognomev1.PricingModel_Package:
		if m.Package.GetPackageSize() <= 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("package_size must be > 0"))
		}
		if m.Package.GetPackagePriceCents() < 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("package_price_cents must be >= 0"))
		}
	case *metrognomev1.PricingModel_Bps:
		if m.Bps.GetBasisPoints() < 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("basis_points must be >= 0"))
		}
	}
	return nil
}

func validateCreateContract(req *metrognomev1.CreateContractRequest) *connect.Error {
	if req.GetCustomerId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("customer_id is required"))
	}
	if req.GetPlanId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("plan_id is required"))
	}
	if req.GetStartAt() <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("start_at is required"))
	}
	if req.GetBillingPeriod() == metrognomev1.BillingPeriod_BILLING_PERIOD_UNSPECIFIED {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("billing_period is required"))
	}
	return nil
}

func validateIngestEvents(req *metrognomev1.IngestEventsRequest) *connect.Error {
	if len(req.GetEvents()) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("at least one event is required"))
	}
	for i, evt := range req.GetEvents() {
		if evt.GetCustomerId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("event[%d]: customer_id is required", i))
		}
		if evt.GetIdempotencyKey() == "" {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("event[%d]: idempotency_key is required", i))
		}
		if evt.GetEventType() == "" {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("event[%d]: event_type is required", i))
		}
	}
	return nil
}

func validateGrantCredit(req *metrognomev1.GrantCreditRequest) *connect.Error {
	if req.GetCustomerId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("customer_id is required"))
	}
	if req.GetAmountCents() <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("amount_cents must be > 0"))
	}
	return nil
}

func validateCreateAlert(req *metrognomev1.CreateAlertRequest) *connect.Error {
	if req.GetCustomerId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("customer_id is required"))
	}
	if req.GetMeterSlug() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("meter_slug is required"))
	}
	if req.GetThreshold() <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("threshold must be > 0"))
	}
	if req.GetAlertType() == metrognomev1.AlertType_ALERT_TYPE_UNSPECIFIED {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("alert_type is required"))
	}
	return nil
}
