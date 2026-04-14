package services

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type InvoiceService struct {
	metrognomev1connect.UnimplementedInvoiceServiceHandler
	invoices *storage.InvoiceStore
	engine   *billing.Engine
}

func NewInvoiceService(invoices *storage.InvoiceStore, engine *billing.Engine) *InvoiceService {
	return &InvoiceService{invoices: invoices, engine: engine}
}

func (s *InvoiceService) GetInvoice(ctx context.Context, req *connect.Request[metrognomev1.GetInvoiceRequest]) (*connect.Response[metrognomev1.GetInvoiceResponse], error) {
	record, err := s.invoices.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("invoice", err)
	}
	return connect.NewResponse(&metrognomev1.GetInvoiceResponse{
		Invoice: invoiceToAPI(record),
	}), nil
}

func (s *InvoiceService) ListInvoices(ctx context.Context, req *connect.Request[metrognomev1.ListInvoicesRequest]) (*connect.Response[metrognomev1.ListInvoicesResponse], error) {
	items, cont, err := s.invoices.ListByCustomer(ctx, req.Msg.GetCustomerId(), int(req.Msg.GetPageSize()), req.Msg.GetContinuation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	invoices := make([]*metrognomev1.Invoice, len(items))
	for i, item := range items {
		invoices[i] = invoiceToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListInvoicesResponse{
		Invoices:     invoices,
		Continuation: cont,
	}), nil
}

func (s *InvoiceService) GenerateInvoice(ctx context.Context, req *connect.Request[metrognomev1.GenerateInvoiceRequest]) (*connect.Response[metrognomev1.GenerateInvoiceResponse], error) {
	invoice, err := s.engine.GenerateInvoice(ctx, req.Msg.GetContractId(), req.Msg.GetPeriodStart(), req.Msg.GetPeriodEnd())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate invoice: %w", err))
	}
	return connect.NewResponse(&metrognomev1.GenerateInvoiceResponse{
		Invoice: invoiceToAPI(invoice),
	}), nil
}

func invoiceToAPI(s *storev1.Invoice) *metrognomev1.Invoice {
	lineItems := make([]*metrognomev1.LineItem, len(s.GetLineItems()))
	for i, li := range s.GetLineItems() {
		lineItems[i] = &metrognomev1.LineItem{
			ChargeId:    li.GetChargeId(),
			MeterSlug:   li.GetMeterSlug(),
			Description: li.GetDescription(),
			Quantity:    li.GetQuantity(),
			AmountCents: li.GetAmountCents(),
		}
	}
	return &metrognomev1.Invoice{
		Id:                  s.GetId(),
		CustomerId:          s.GetCustomerId(),
		ContractId:          s.GetContractId(),
		PeriodStart:         s.GetPeriodStart(),
		PeriodEnd:           s.GetPeriodEnd(),
		LineItems:           lineItems,
		SubtotalCents:       s.GetSubtotalCents(),
		CreditsAppliedCents: s.GetCreditsAppliedCents(),
		TotalCents:          s.GetTotalCents(),
		Status:              metrognomev1.InvoiceStatus(s.GetStatus()),
		CreatedAt:           s.GetCreatedAt(),
		FinalizedAt:         s.GetFinalizedAt(),
	}
}
