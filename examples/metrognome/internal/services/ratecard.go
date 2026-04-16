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

type RateCardService struct {
	metrognomev1connect.UnimplementedRateCardServiceHandler
	store     *storage.RateCardStore
	rateStore *storage.RateStore
}

func NewRateCardService(store *storage.RateCardStore, rateStore *storage.RateStore) *RateCardService {
	return &RateCardService{store: store, rateStore: rateStore}
}

func (s *RateCardService) CreateRateCard(ctx context.Context, req *connect.Request[metrognomev1.CreateRateCardRequest]) (*connect.Response[metrognomev1.CreateRateCardResponse], error) {
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}

	id := newID("rc")
	record := &storev1.RateCard{
		Id:          proto.String(id),
		Name:        proto.String(req.Msg.GetName()),
		Description: proto.String(req.Msg.GetDescription()),
		Aliases:     req.Msg.GetAliases(),
		CreatedAt:   proto.Int64(time.Now().UnixMilli()),
	}

	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create rate card: %w", err))
	}

	return connect.NewResponse(&metrognomev1.CreateRateCardResponse{
		RateCard: rateCardToAPI(record),
	}), nil
}

func (s *RateCardService) GetRateCard(ctx context.Context, req *connect.Request[metrognomev1.GetRateCardRequest]) (*connect.Response[metrognomev1.GetRateCardResponse], error) {
	record, err := s.store.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("rate card", err)
	}
	return connect.NewResponse(&metrognomev1.GetRateCardResponse{
		RateCard: rateCardToAPI(record),
	}), nil
}

func (s *RateCardService) ListRateCards(ctx context.Context, req *connect.Request[metrognomev1.ListRateCardsRequest]) (*connect.Response[metrognomev1.ListRateCardsResponse], error) {
	items, cont, err := s.store.List(ctx, int(req.Msg.GetPageSize()), req.Msg.GetContinuation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list rate cards: %w", err))
	}
	cards := make([]*metrognomev1.RateCard, len(items))
	for i, item := range items {
		cards[i] = rateCardToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListRateCardsResponse{
		RateCards:    cards,
		Continuation: cont,
	}), nil
}

func (s *RateCardService) AddRate(ctx context.Context, req *connect.Request[metrognomev1.AddRateRequest]) (*connect.Response[metrognomev1.AddRateResponse], error) {
	if req.Msg.GetRateCardId() == "" || req.Msg.GetProductId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("rate_card_id and product_id are required"))
	}

	id := newID("rate")

	var tiers []*storev1.Tier
	for _, t := range req.Msg.GetTiers() {
		tiers = append(tiers, &storev1.Tier{
			UpTo:       proto.Int64(t.GetUpTo()),
			PriceCents: proto.Int64(t.GetPriceCents()),
		})
	}

	record := &storev1.Rate{
		Id:                     proto.String(id),
		RateCardId:             proto.String(req.Msg.GetRateCardId()),
		ProductId:              proto.String(req.Msg.GetProductId()),
		RateType:               storev1.RateType(req.Msg.GetRateType()).Enum(),
		PriceCents:             proto.Int64(req.Msg.GetPriceCents()),
		Tiers:                  tiers,
		BillingFrequencyMonths: proto.Int64(req.Msg.GetBillingFrequencyMonths()),
		StartingAt:             proto.Int64(req.Msg.GetStartingAt()),
		EndingBefore:           proto.Int64(req.Msg.GetEndingBefore()),
		Entitled:               proto.Bool(req.Msg.GetEntitled()),
		CreatedAt:              proto.Int64(time.Now().UnixMilli()),
	}

	if err := s.rateStore.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("add rate: %w", err))
	}

	return connect.NewResponse(&metrognomev1.AddRateResponse{
		Rate: rateToAPI(record),
	}), nil
}

func (s *RateCardService) ListRates(ctx context.Context, req *connect.Request[metrognomev1.ListRatesRequest]) (*connect.Response[metrognomev1.ListRatesResponse], error) {
	items, err := s.rateStore.ListByRateCard(ctx, req.Msg.GetRateCardId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list rates: %w", err))
	}
	rates := make([]*metrognomev1.Rate, len(items))
	for i, item := range items {
		rates[i] = rateToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListRatesResponse{
		Rates: rates,
	}), nil
}

func rateCardToAPI(r *storev1.RateCard) *metrognomev1.RateCard {
	return &metrognomev1.RateCard{
		Id:          r.GetId(),
		Name:        r.GetName(),
		Description: r.GetDescription(),
		Aliases:     r.GetAliases(),
		CreatedAt:   r.GetCreatedAt(),
	}
}

func rateToAPI(r *storev1.Rate) *metrognomev1.Rate {
	var tiers []*metrognomev1.RateTier
	for _, t := range r.GetTiers() {
		tiers = append(tiers, &metrognomev1.RateTier{
			UpTo:       t.GetUpTo(),
			PriceCents: t.GetPriceCents(),
		})
	}
	return &metrognomev1.Rate{
		Id:                     r.GetId(),
		RateCardId:             r.GetRateCardId(),
		ProductId:              r.GetProductId(),
		RateType:               metrognomev1.RateType(r.GetRateType()),
		PriceCents:             r.GetPriceCents(),
		Tiers:                  tiers,
		BillingFrequencyMonths: r.GetBillingFrequencyMonths(),
		StartingAt:             r.GetStartingAt(),
		EndingBefore:           r.GetEndingBefore(),
		Entitled:               r.GetEntitled(),
		CreatedAt:              r.GetCreatedAt(),
	}
}
