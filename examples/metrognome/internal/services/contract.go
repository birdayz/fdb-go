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

type ContractService struct {
	metrognomev1connect.UnimplementedContractServiceHandler
	store *storage.ContractStore
}

func NewContractService(store *storage.ContractStore) *ContractService {
	return &ContractService{store: store}
}

func (s *ContractService) CreateContract(ctx context.Context, req *connect.Request[metrognomev1.CreateContractRequest]) (*connect.Response[metrognomev1.CreateContractResponse], error) {
	id := newID("ctr")
	now := time.Now().UnixMilli()
	record := &storev1.Contract{
		Id:            proto.String(id),
		CustomerId:    proto.String(req.Msg.GetCustomerId()),
		PlanId:        proto.String(req.Msg.GetPlanId()),
		StartAt:       proto.Int64(req.Msg.GetStartAt()),
		EndAt:         proto.Int64(req.Msg.GetEndAt()),
		BillingPeriod: convertBillingPeriod(req.Msg.GetBillingPeriod()).Enum(),
		CreatedAt:     proto.Int64(now),
		Active:        proto.Bool(true),
	}
	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create contract: %w", err))
	}
	return connect.NewResponse(&metrognomev1.CreateContractResponse{
		Contract: contractToAPI(record),
	}), nil
}

func (s *ContractService) GetContract(ctx context.Context, req *connect.Request[metrognomev1.GetContractRequest]) (*connect.Response[metrognomev1.GetContractResponse], error) {
	record, err := s.store.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("contract", err)
	}
	return connect.NewResponse(&metrognomev1.GetContractResponse{
		Contract: contractToAPI(record),
	}), nil
}

func (s *ContractService) ListContracts(ctx context.Context, req *connect.Request[metrognomev1.ListContractsRequest]) (*connect.Response[metrognomev1.ListContractsResponse], error) {
	items, err := s.store.ListByCustomer(ctx, req.Msg.GetCustomerId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	contracts := make([]*metrognomev1.Contract, len(items))
	for i, item := range items {
		contracts[i] = contractToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListContractsResponse{
		Contracts: contracts,
	}), nil
}

func (s *ContractService) EndContract(ctx context.Context, req *connect.Request[metrognomev1.EndContractRequest]) (*connect.Response[metrognomev1.EndContractResponse], error) {
	record, err := s.store.End(ctx, req.Msg.GetId(), req.Msg.GetEndAt())
	if err != nil {
		return nil, storageError("contract", err)
	}
	return connect.NewResponse(&metrognomev1.EndContractResponse{
		Contract: contractToAPI(record),
	}), nil
}

func contractToAPI(s *storev1.Contract) *metrognomev1.Contract {
	return &metrognomev1.Contract{
		Id:            s.GetId(),
		CustomerId:    s.GetCustomerId(),
		PlanId:        s.GetPlanId(),
		StartAt:       s.GetStartAt(),
		EndAt:         s.GetEndAt(),
		BillingPeriod: metrognomev1.BillingPeriod(s.GetBillingPeriod()),
		CreatedAt:     s.GetCreatedAt(),
		Active:        s.GetActive(),
	}
}

func convertBillingPeriod(p metrognomev1.BillingPeriod) storev1.BillingPeriod {
	switch p {
	case metrognomev1.BillingPeriod_BILLING_PERIOD_MONTHLY:
		return storev1.BillingPeriod_BILLING_PERIOD_MONTHLY
	case metrognomev1.BillingPeriod_BILLING_PERIOD_QUARTERLY:
		return storev1.BillingPeriod_BILLING_PERIOD_QUARTERLY
	case metrognomev1.BillingPeriod_BILLING_PERIOD_ANNUAL:
		return storev1.BillingPeriod_BILLING_PERIOD_ANNUAL
	default:
		return storev1.BillingPeriod_BILLING_PERIOD_MONTHLY
	}
}
