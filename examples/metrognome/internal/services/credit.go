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

type CreditService struct {
	metrognomev1connect.UnimplementedCreditServiceHandler
	store *storage.CreditStore
}

func NewCreditService(store *storage.CreditStore) *CreditService {
	return &CreditService{store: store}
}

func (s *CreditService) GrantCredit(ctx context.Context, req *connect.Request[metrognomev1.GrantCreditRequest]) (*connect.Response[metrognomev1.GrantCreditResponse], error) {
	if err := validateGrantCredit(req.Msg); err != nil {
		return nil, err
	}
	id := newID("cred")
	now := time.Now().UnixMilli()
	record := &storev1.Credit{
		Id:             proto.String(id),
		CustomerId:     proto.String(req.Msg.GetCustomerId()),
		AmountCents:    proto.Int64(req.Msg.GetAmountCents()),
		RemainingCents: proto.Int64(req.Msg.GetAmountCents()),
		ExpiresAt:      proto.Int64(req.Msg.GetExpiresAt()),
		Priority:       proto.Int32(req.Msg.GetPriority()),
		CreatedAt:      proto.Int64(now),
	}
	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("grant credit: %w", err))
	}
	return connect.NewResponse(&metrognomev1.GrantCreditResponse{
		Credit: creditToAPI(record),
	}), nil
}

func (s *CreditService) ListCredits(ctx context.Context, req *connect.Request[metrognomev1.ListCreditsRequest]) (*connect.Response[metrognomev1.ListCreditsResponse], error) {
	items, err := s.store.ListByCustomer(ctx, req.Msg.GetCustomerId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	credits := make([]*metrognomev1.Credit, len(items))
	for i, item := range items {
		credits[i] = creditToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListCreditsResponse{
		Credits: credits,
	}), nil
}

func (s *CreditService) GetCreditBalance(ctx context.Context, req *connect.Request[metrognomev1.GetCreditBalanceRequest]) (*connect.Response[metrognomev1.GetCreditBalanceResponse], error) {
	total, credits, err := s.store.GetBalance(ctx, req.Msg.GetCustomerId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	apiCredits := make([]*metrognomev1.Credit, len(credits))
	for i, c := range credits {
		apiCredits[i] = creditToAPI(c)
	}
	return connect.NewResponse(&metrognomev1.GetCreditBalanceResponse{
		TotalRemainingCents: total,
		Credits:             apiCredits,
	}), nil
}

func creditToAPI(s *storev1.Credit) *metrognomev1.Credit {
	return &metrognomev1.Credit{
		Id:             s.GetId(),
		CustomerId:     s.GetCustomerId(),
		AmountCents:    s.GetAmountCents(),
		RemainingCents: s.GetRemainingCents(),
		ExpiresAt:      s.GetExpiresAt(),
		Priority:       s.GetPriority(),
		CreatedAt:      s.GetCreatedAt(),
	}
}
