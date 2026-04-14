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

type CustomerService struct {
	metrognomev1connect.UnimplementedCustomerServiceHandler
	store *storage.CustomerStore
}

func NewCustomerService(store *storage.CustomerStore) *CustomerService {
	return &CustomerService{store: store}
}

func (s *CustomerService) CreateCustomer(ctx context.Context, req *connect.Request[metrognomev1.CreateCustomerRequest]) (*connect.Response[metrognomev1.CreateCustomerResponse], error) {
	if err := validateCreateCustomer(req.Msg); err != nil {
		return nil, err
	}
	id := newID("cust")
	now := time.Now().UnixMilli()

	record := &storev1.Customer{
		Id:         proto.String(id),
		Name:       proto.String(req.Msg.GetName()),
		ExternalId: proto.String(req.Msg.GetExternalId()),
		CreatedAt:  proto.Int64(now),
	}

	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create customer: %w", err))
	}

	return connect.NewResponse(&metrognomev1.CreateCustomerResponse{
		Customer: customerToAPI(record),
	}), nil
}

func (s *CustomerService) GetCustomer(ctx context.Context, req *connect.Request[metrognomev1.GetCustomerRequest]) (*connect.Response[metrognomev1.GetCustomerResponse], error) {
	record, err := s.store.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("customer", err)
	}
	return connect.NewResponse(&metrognomev1.GetCustomerResponse{
		Customer: customerToAPI(record),
	}), nil
}

func (s *CustomerService) ListCustomers(ctx context.Context, req *connect.Request[metrognomev1.ListCustomersRequest]) (*connect.Response[metrognomev1.ListCustomersResponse], error) {
	items, cont, err := s.store.List(ctx, int(req.Msg.GetPageSize()), req.Msg.GetContinuation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	customers := make([]*metrognomev1.Customer, len(items))
	for i, item := range items {
		customers[i] = customerToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListCustomersResponse{
		Customers:    customers,
		Continuation: cont,
	}), nil
}

func customerToAPI(s *storev1.Customer) *metrognomev1.Customer {
	return &metrognomev1.Customer{
		Id:         s.GetId(),
		Name:       s.GetName(),
		ExternalId: s.GetExternalId(),
		CreatedAt:  s.GetCreatedAt(),
	}
}
