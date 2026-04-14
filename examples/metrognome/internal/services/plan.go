package services

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type PlanService struct {
	metrognomev1connect.UnimplementedPlanServiceHandler
	plans   *storage.PlanStore
	charges *storage.ChargeStore
}

func NewPlanService(plans *storage.PlanStore, charges *storage.ChargeStore) *PlanService {
	return &PlanService{plans: plans, charges: charges}
}

func (s *PlanService) CreatePlan(ctx context.Context, req *connect.Request[metrognomev1.CreatePlanRequest]) (*connect.Response[metrognomev1.CreatePlanResponse], error) {
	id := newID("plan")
	now := time.Now().UnixMilli()
	record := &storev1.Plan{
		Id:          proto.String(id),
		Name:        proto.String(req.Msg.GetName()),
		Description: proto.String(req.Msg.GetDescription()),
		CreatedAt:   proto.Int64(now),
	}
	if err := s.plans.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create plan: %w", err))
	}
	return connect.NewResponse(&metrognomev1.CreatePlanResponse{
		Plan: planToAPI(record),
	}), nil
}

func (s *PlanService) GetPlan(ctx context.Context, req *connect.Request[metrognomev1.GetPlanRequest]) (*connect.Response[metrognomev1.GetPlanResponse], error) {
	record, err := s.plans.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("plan", err)
	}
	return connect.NewResponse(&metrognomev1.GetPlanResponse{
		Plan: planToAPI(record),
	}), nil
}

func (s *PlanService) ListPlans(ctx context.Context, req *connect.Request[metrognomev1.ListPlansRequest]) (*connect.Response[metrognomev1.ListPlansResponse], error) {
	items, cont, err := s.plans.List(ctx, int(req.Msg.GetPageSize()), req.Msg.GetContinuation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	plans := make([]*metrognomev1.Plan, len(items))
	for i, item := range items {
		plans[i] = planToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListPlansResponse{
		Plans:        plans,
		Continuation: cont,
	}), nil
}

func (s *PlanService) AddCharge(ctx context.Context, req *connect.Request[metrognomev1.AddChargeRequest]) (*connect.Response[metrognomev1.AddChargeResponse], error) {
	id := newID("chrg")
	now := time.Now().UnixMilli()
	record := &storev1.Charge{
		Id:        proto.String(id),
		PlanId:    proto.String(req.Msg.GetPlanId()),
		MeterSlug: proto.String(req.Msg.GetMeterSlug()),
		Pricing:   apiPricingToStore(req.Msg.GetPricing()),
		CreatedAt: proto.Int64(now),
	}
	if err := s.charges.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("add charge: %w", err))
	}
	return connect.NewResponse(&metrognomev1.AddChargeResponse{
		Charge: chargeToAPI(record),
	}), nil
}

func (s *PlanService) ListCharges(ctx context.Context, req *connect.Request[metrognomev1.ListChargesRequest]) (*connect.Response[metrognomev1.ListChargesResponse], error) {
	items, err := s.charges.ListByPlan(ctx, req.Msg.GetPlanId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	charges := make([]*metrognomev1.Charge, len(items))
	for i, item := range items {
		charges[i] = chargeToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListChargesResponse{
		Charges: charges,
	}), nil
}

func planToAPI(s *storev1.Plan) *metrognomev1.Plan {
	return &metrognomev1.Plan{
		Id:          s.GetId(),
		Name:        s.GetName(),
		Description: s.GetDescription(),
		CreatedAt:   s.GetCreatedAt(),
	}
}

func chargeToAPI(s *storev1.Charge) *metrognomev1.Charge {
	return &metrognomev1.Charge{
		Id:        s.GetId(),
		PlanId:    s.GetPlanId(),
		MeterSlug: s.GetMeterSlug(),
		Pricing:   storePricingToAPI(s.GetPricing()),
		CreatedAt: s.GetCreatedAt(),
	}
}

func apiPricingToStore(p *metrognomev1.PricingModel) *storev1.PricingModel {
	if p == nil {
		return nil
	}
	switch m := p.Model.(type) {
	case *metrognomev1.PricingModel_Flat:
		return &storev1.PricingModel{Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(m.Flat.GetAmountCents())}}}
	case *metrognomev1.PricingModel_PerUnit:
		return &storev1.PricingModel{Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(m.PerUnit.GetUnitPriceCents())}}}
	case *metrognomev1.PricingModel_Tiered:
		tiers := make([]*storev1.Tier, len(m.Tiered.GetTiers()))
		for i, t := range m.Tiered.GetTiers() {
			tiers[i] = &storev1.Tier{UpTo: proto.Int64(t.GetUpTo()), PriceCents: proto.Int64(t.GetPriceCents())}
		}
		return &storev1.PricingModel{Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{Tiers: tiers}}}
	case *metrognomev1.PricingModel_Volume:
		tiers := make([]*storev1.Tier, len(m.Volume.GetTiers()))
		for i, t := range m.Volume.GetTiers() {
			tiers[i] = &storev1.Tier{UpTo: proto.Int64(t.GetUpTo()), PriceCents: proto.Int64(t.GetPriceCents())}
		}
		return &storev1.PricingModel{Model: &storev1.PricingModel_Volume{Volume: &storev1.VolumePricing{Tiers: tiers}}}
	case *metrognomev1.PricingModel_Package:
		return &storev1.PricingModel{Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{PackageSize: proto.Int64(m.Package.GetPackageSize()), PackagePriceCents: proto.Int64(m.Package.GetPackagePriceCents())}}}
	case *metrognomev1.PricingModel_Bps:
		return &storev1.PricingModel{Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(m.Bps.GetBasisPoints())}}}
	default:
		return nil
	}
}

func storePricingToAPI(p *storev1.PricingModel) *metrognomev1.PricingModel {
	if p == nil {
		return nil
	}
	switch m := p.Model.(type) {
	case *storev1.PricingModel_Flat:
		return &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Flat{Flat: &metrognomev1.FlatPricing{AmountCents: m.Flat.GetAmountCents()}}}
	case *storev1.PricingModel_PerUnit:
		return &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_PerUnit{PerUnit: &metrognomev1.PerUnitPricing{UnitPriceCents: m.PerUnit.GetUnitPriceCents()}}}
	case *storev1.PricingModel_Tiered:
		tiers := make([]*metrognomev1.Tier, len(m.Tiered.GetTiers()))
		for i, t := range m.Tiered.GetTiers() {
			tiers[i] = &metrognomev1.Tier{UpTo: t.GetUpTo(), PriceCents: t.GetPriceCents()}
		}
		return &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Tiered{Tiered: &metrognomev1.TieredPricing{Tiers: tiers}}}
	case *storev1.PricingModel_Volume:
		tiers := make([]*metrognomev1.Tier, len(m.Volume.GetTiers()))
		for i, t := range m.Volume.GetTiers() {
			tiers[i] = &metrognomev1.Tier{UpTo: t.GetUpTo(), PriceCents: t.GetPriceCents()}
		}
		return &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Volume{Volume: &metrognomev1.VolumePricing{Tiers: tiers}}}
	case *storev1.PricingModel_Package:
		return &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Package{Package: &metrognomev1.PackagePricing{PackageSize: m.Package.GetPackageSize(), PackagePriceCents: m.Package.GetPackagePriceCents()}}}
	case *storev1.PricingModel_Bps:
		return &metrognomev1.PricingModel{Model: &metrognomev1.PricingModel_Bps{Bps: &metrognomev1.BpsPricing{BasisPoints: m.Bps.GetBasisPoints()}}}
	default:
		return nil
	}
}
