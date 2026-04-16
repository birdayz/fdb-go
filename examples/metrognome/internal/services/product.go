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

type ProductService struct {
	metrognomev1connect.UnimplementedProductServiceHandler
	store *storage.ProductStore
}

func NewProductService(store *storage.ProductStore) *ProductService {
	return &ProductService{store: store}
}

func (s *ProductService) CreateProduct(ctx context.Context, req *connect.Request[metrognomev1.CreateProductRequest]) (*connect.Response[metrognomev1.CreateProductResponse], error) {
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}

	id := newID("prod")
	record := &storev1.Product{
		Id:               proto.String(id),
		Name:             proto.String(req.Msg.GetName()),
		Type:             storev1.ProductType(req.Msg.GetType()).Enum(),
		BillableMetricId: proto.String(req.Msg.GetBillableMetricId()),
		Tags:             req.Msg.GetTags(),
		Description:      proto.String(req.Msg.GetDescription()),
		CreatedAt:        proto.Int64(time.Now().UnixMilli()),
	}

	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create product: %w", err))
	}

	return connect.NewResponse(&metrognomev1.CreateProductResponse{
		Product: productToAPI(record),
	}), nil
}

func (s *ProductService) GetProduct(ctx context.Context, req *connect.Request[metrognomev1.GetProductRequest]) (*connect.Response[metrognomev1.GetProductResponse], error) {
	record, err := s.store.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("product", err)
	}
	return connect.NewResponse(&metrognomev1.GetProductResponse{
		Product: productToAPI(record),
	}), nil
}

func (s *ProductService) ListProducts(ctx context.Context, req *connect.Request[metrognomev1.ListProductsRequest]) (*connect.Response[metrognomev1.ListProductsResponse], error) {
	items, cont, err := s.store.List(ctx, int(req.Msg.GetPageSize()), req.Msg.GetContinuation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list products: %w", err))
	}
	products := make([]*metrognomev1.Product, len(items))
	for i, item := range items {
		products[i] = productToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListProductsResponse{
		Products:     products,
		Continuation: cont,
	}), nil
}

func productToAPI(r *storev1.Product) *metrognomev1.Product {
	return &metrognomev1.Product{
		Id:               r.GetId(),
		Name:             r.GetName(),
		Type:             metrognomev1.ProductType(r.GetType()),
		BillableMetricId: r.GetBillableMetricId(),
		Tags:             r.GetTags(),
		CreatedAt:        r.GetCreatedAt(),
		Description:      r.GetDescription(),
	}
}
