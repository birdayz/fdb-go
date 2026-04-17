package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

// Engine generates invoices by evaluating metered usage against plan charges.
type Engine struct {
	fdb      *rl.FDBDatabase
	metadata *rl.RecordMetaData
	ss       subspace.Subspace
}

// NewEngine creates a billing engine.
func NewEngine(fdb *rl.FDBDatabase, metadata *rl.RecordMetaData, ss subspace.Subspace) *Engine {
	return &Engine{fdb: fdb, metadata: metadata, ss: ss}
}

// GenerateInvoice creates an invoice for a contract's billing period.
// All work happens in a single FDB transaction: read usage aggregates,
// compute charges, apply credits, write invoice.
func (e *Engine) GenerateInvoice(ctx context.Context, contractID string, periodStart, periodEnd int64) (*storev1.Invoice, error) {
	result, err := e.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(e.metadata).
			SetSubspace(e.ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// 1. Load the contract
		contract, err := loadContract(store, e.metadata, contractID)
		if err != nil {
			return nil, fmt.Errorf("load contract: %w", err)
		}

		// 2. Load the plan's charges
		charges, err := loadChargesByPlan(ctx, store, contract.GetPlanId())
		if err != nil {
			return nil, fmt.Errorf("load charges: %w", err)
		}

		// 3. For each charge, evaluate usage and compute the line item
		var lineItems []*storev1.LineItem
		var subtotal int64

		startBucket := bucketHour(periodStart)
		endBucket := bucketHour(periodEnd)

		for _, charge := range charges {
			usage, err := getUsageForCharge(ctx, store, contract.GetCustomerId(), charge, startBucket, endBucket)
			if err != nil {
				return nil, fmt.Errorf("get usage for charge %s: %w", charge.GetId(), err)
			}

			amount, desc, err := CalculateCharge(usage, charge.GetPricing())
			if err != nil {
				return nil, fmt.Errorf("calculate charge %s: %w", charge.GetId(), err)
			}

			lineItems = append(lineItems, &storev1.LineItem{
				ChargeId:    charge.Id,
				MeterSlug:   charge.MeterSlug,
				Description: proto.String(desc),
				Quantity:    proto.Int64(usage),
				AmountCents: proto.Int64(amount),
			})
			subtotal += amount
		}

		// 4. Apply prepaid commit logic.
		// If the contract has a committed amount (minimum spend), the customer pays
		// max(usage_charges, committed_amount). Overage = usage - commit (if any).
		usageCharges := subtotal
		committedAmount := contract.GetCommittedAmountCents()
		var overageCents int64

		if committedAmount > 0 && usageCharges > committedAmount {
			// Usage exceeds commit — charge overage with multiplier
			overageCents = usageCharges - committedAmount
			multiplier := contract.GetOverageMultiplierBps()
			if multiplier == 0 {
				multiplier = 10000 // default 1x
			}
			// Overage line item at the multiplier rate
			overageAmount := (overageCents * multiplier) / 10000
			lineItems = append(lineItems, &storev1.LineItem{
				Description: proto.String(fmt.Sprintf("overage (%d bps multiplier)", multiplier)),
				Quantity:    proto.Int64(overageCents),
				AmountCents: proto.Int64(overageAmount),
			})
			subtotal = committedAmount + overageAmount
		} else if committedAmount > 0 && usageCharges < committedAmount {
			// Usage under commit — charge the committed minimum
			subtotal = committedAmount
		}

		// 5. Apply credits
		creditsApplied, err := applyCredits(ctx, store, e.metadata, contract.GetCustomerId(), subtotal)
		if err != nil {
			return nil, fmt.Errorf("apply credits: %w", err)
		}

		total := subtotal - creditsApplied
		if total < 0 {
			total = 0
		}

		// 6. Create the invoice
		now := time.Now().UnixMilli()
		invoiceID := fmt.Sprintf("inv_%s_%d", contractID, periodStart)
		invoice := &storev1.Invoice{
			Id:                   proto.String(invoiceID),
			CustomerId:           contract.CustomerId,
			ContractId:           proto.String(contractID),
			PeriodStart:          proto.Int64(periodStart),
			PeriodEnd:            proto.Int64(periodEnd),
			LineItems:            lineItems,
			SubtotalCents:        proto.Int64(subtotal),
			CreditsAppliedCents:  proto.Int64(creditsApplied),
			TotalCents:           proto.Int64(total),
			Status:               storev1.InvoiceStatus_INVOICE_STATUS_DRAFT.Enum(),
			CreatedAt:            proto.Int64(now),
			CommittedAmountCents: proto.Int64(committedAmount),
			UsageChargesCents:    proto.Int64(usageCharges),
			OverageCents:         proto.Int64(overageCents),
		}

		if _, err := store.SaveRecord(invoice); err != nil {
			return nil, fmt.Errorf("save invoice: %w", err)
		}

		return invoice, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.Invoice), nil
}

func loadContract(store *rl.FDBRecordStore, md *rl.RecordMetaData, id string) (*storev1.Contract, error) {
	rtk := int64(md.GetRecordType("Contract").RecordTypeIndex)
	rec, err := store.LoadRecord(tuple.Tuple{rtk, id})
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("contract %s not found", id)
	}
	return rec.Record.(*storev1.Contract), nil
}

func loadChargesByPlan(ctx context.Context, store *rl.FDBRecordStore, planID string) ([]*storev1.Charge, error) {
	cursor := store.ScanIndexRecords("charge_by_plan",
		rl.TupleRangeAllOf(tuple.Tuple{planID}), nil, rl.ForwardScan())
	entries, err := rl.AsList(ctx, cursor)
	if err != nil {
		return nil, err
	}
	charges := make([]*storev1.Charge, len(entries))
	for i, e := range entries {
		charges[i] = e.Record.Record.(*storev1.Charge)
	}
	return charges, nil
}

func getUsageForCharge(ctx context.Context, store *rl.FDBRecordStore, customerID string, charge *storev1.Charge, startBucket, endBucket int64) (int64, error) {
	meterSlug := charge.GetMeterSlug()

	// Check pricing type — flat charges don't need usage lookup
	if charge.GetPricing() != nil {
		if _, ok := charge.GetPricing().Model.(*storev1.PricingModel_Flat); ok {
			return 1, nil // flat = quantity 1
		}
	}

	// For COUNT-based meters, use count aggregate. For SUM-based, use sum.
	// We default to SUM since the usage_sum index covers all events.
	result, err := store.EvaluateAggregateFunction(ctx,
		[]string{"UsageEvent"},
		rl.NewSumAggregateFunction(
			rl.GroupBy(rl.Field("value"), rl.Field("customer_id"), rl.Field("meter_slug"), rl.Field("timestamp_bucket"))),
		rl.TupleRangeBetweenInclusive(
			tuple.Tuple{customerID, meterSlug, startBucket},
			tuple.Tuple{customerID, meterSlug, endBucket}),
		rl.IsolationLevelSnapshot)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, nil
	}
	return result[0].(int64), nil
}

// applyCredits draws down credits against the subtotal, saving updated credit records.
// Credits are applied in order of priority (ascending), then expiry (ascending).
func applyCredits(ctx context.Context, store *rl.FDBRecordStore, md *rl.RecordMetaData, customerID string, subtotal int64) (int64, error) {
	if subtotal <= 0 {
		return 0, nil
	}

	cursor := store.ScanIndexRecords("credit_by_customer",
		rl.TupleRangeAllOf(tuple.Tuple{customerID}), nil, rl.ForwardScan())
	entries, err := rl.AsList(ctx, cursor)
	if err != nil {
		return 0, err
	}

	now := time.Now().UnixMilli()
	var totalApplied int64
	remaining := subtotal

	for _, e := range entries {
		if remaining <= 0 {
			break
		}
		credit := e.Record.Record.(*storev1.Credit)

		// Skip expired credits
		if credit.GetExpiresAt() > 0 && credit.GetExpiresAt() < now {
			continue
		}
		// Skip depleted credits
		if credit.GetRemainingCents() <= 0 {
			continue
		}

		// Draw down
		draw := min(remaining, credit.GetRemainingCents())
		credit.RemainingCents = proto.Int64(credit.GetRemainingCents() - draw)
		if _, err := store.SaveRecord(credit); err != nil {
			return 0, fmt.Errorf("update credit %s: %w", credit.GetId(), err)
		}
		totalApplied += draw
		remaining -= draw
	}

	return totalApplied, nil
}

// bucketHour returns the hourly bucket for a timestamp (truncated to hour boundary).
func bucketHour(timestampMs int64) int64 {
	const hourMs = 3600 * 1000
	return (timestampMs / hourMs) * hourMs
}

// BucketHour is the exported version for use by other packages.
func BucketHour(timestampMs int64) int64 {
	return bucketHour(timestampMs)
}
