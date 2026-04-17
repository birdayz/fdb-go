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
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type InvoiceService struct {
	metrognomev1connect.UnimplementedInvoiceServiceHandler
	invoices  *storage.InvoiceStore
	contracts *storage.ContractStore
	engine    *billing.Engine
}

func NewInvoiceService(invoices *storage.InvoiceStore, contracts *storage.ContractStore, engine *billing.Engine) *InvoiceService {
	return &InvoiceService{invoices: invoices, contracts: contracts, engine: engine}
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

func (s *InvoiceService) UpdateInvoiceStatus(ctx context.Context, req *connect.Request[metrognomev1.UpdateInvoiceStatusRequest]) (*connect.Response[metrognomev1.UpdateInvoiceStatusResponse], error) {
	record, err := s.invoices.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, storageError("invoice", err)
	}

	newStatus := req.Msg.GetStatus()
	oldStatus := record.GetStatus()

	// Validate state transition
	if err := validateStatusTransition(oldStatus, storev1.InvoiceStatus(newStatus)); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	record.Status = storev1.InvoiceStatus(newStatus).Enum()
	if newStatus == metrognomev1.InvoiceStatus_INVOICE_STATUS_ISSUED {
		record.FinalizedAt = proto.Int64(time.Now().UnixMilli())
	}

	if err := s.invoices.Save(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update invoice status: %w", err))
	}

	return connect.NewResponse(&metrognomev1.UpdateInvoiceStatusResponse{
		Invoice: invoiceToAPI(record),
	}), nil
}

// validateStatusTransition enforces the invoice status state machine:
//
//	DRAFT → ISSUED → PAID
//	DRAFT → VOID
//	ISSUED → VOID
func validateStatusTransition(from storev1.InvoiceStatus, to storev1.InvoiceStatus) error {
	switch from {
	case storev1.InvoiceStatus_INVOICE_STATUS_DRAFT:
		if to == storev1.InvoiceStatus_INVOICE_STATUS_ISSUED || to == storev1.InvoiceStatus_INVOICE_STATUS_VOID {
			return nil
		}
	case storev1.InvoiceStatus_INVOICE_STATUS_ISSUED:
		if to == storev1.InvoiceStatus_INVOICE_STATUS_PAID || to == storev1.InvoiceStatus_INVOICE_STATUS_VOID {
			return nil
		}
	case storev1.InvoiceStatus_INVOICE_STATUS_PAID:
		// terminal state
	case storev1.InvoiceStatus_INVOICE_STATUS_VOID:
		// terminal state
	}
	return fmt.Errorf("invalid transition: %s → %s", from, to)
}

func (s *InvoiceService) GenerateAllInvoices(ctx context.Context, req *connect.Request[metrognomev1.GenerateAllInvoicesRequest]) (*connect.Response[metrognomev1.GenerateAllInvoicesResponse], error) {
	asOf := req.Msg.GetAsOf()
	if asOf <= 0 {
		asOf = time.Now().UnixMilli()
	}

	// Find all active contracts
	contracts, err := s.contracts.ListActive(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list active contracts: %w", err))
	}

	var generated, skipped, errors int32

	for _, contract := range contracts {
		// Calculate the previous period (the one that just ended)
		periodStart, periodEnd := billing.PreviousPeriod(contract, asOf)

		// Skip if the period hasn't ended yet (period end is in the future)
		if periodEnd > asOf {
			skipped++
			continue
		}

		// Generate invoice for this period
		_, err := s.engine.GenerateInvoice(ctx, contract.GetId(), periodStart, periodEnd)
		if err != nil {
			errors++
			continue
		}
		generated++
	}

	return connect.NewResponse(&metrognomev1.GenerateAllInvoicesResponse{
		Generated: generated,
		Skipped:   skipped,
		Errors:    errors,
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
		Id:                   s.GetId(),
		CustomerId:           s.GetCustomerId(),
		ContractId:           s.GetContractId(),
		PeriodStart:          s.GetPeriodStart(),
		PeriodEnd:            s.GetPeriodEnd(),
		LineItems:            lineItems,
		SubtotalCents:        s.GetSubtotalCents(),
		CreditsAppliedCents:  s.GetCreditsAppliedCents(),
		TotalCents:           s.GetTotalCents(),
		Status:               metrognomev1.InvoiceStatus(s.GetStatus()),
		CreatedAt:            s.GetCreatedAt(),
		FinalizedAt:          s.GetFinalizedAt(),
		CommittedAmountCents: s.GetCommittedAmountCents(),
		UsageChargesCents:    s.GetUsageChargesCents(),
		OverageCents:         s.GetOverageCents(),
	}
}
