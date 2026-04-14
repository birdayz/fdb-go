package billing

import (
	"time"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

// CurrentPeriod returns the billing period (start, end) that contains the given timestamp,
// based on the contract's billing period type and start date.
func CurrentPeriod(contract *storev1.Contract, asOf int64) (periodStart, periodEnd int64) {
	start := time.UnixMilli(contract.GetStartAt()).UTC()
	now := time.UnixMilli(asOf).UTC()

	switch contract.GetBillingPeriod() {
	case storev1.BillingPeriod_BILLING_PERIOD_MONTHLY:
		// Find the most recent period boundary before asOf
		y, m, _ := start.Date()
		// Walk forward from contract start month by month until we pass asOf
		for {
			ps := time.Date(y, m, start.Day(), 0, 0, 0, 0, time.UTC)
			pe := time.Date(y, m+1, start.Day(), 0, 0, 0, 0, time.UTC)
			if pe.After(now) {
				return ps.UnixMilli(), pe.UnixMilli()
			}
			m++
			if m > 12 {
				m = 1
				y++
			}
		}

	case storev1.BillingPeriod_BILLING_PERIOD_QUARTERLY:
		y, m, _ := start.Date()
		for {
			ps := time.Date(y, m, start.Day(), 0, 0, 0, 0, time.UTC)
			pe := time.Date(y, m+3, start.Day(), 0, 0, 0, 0, time.UTC)
			if pe.After(now) {
				return ps.UnixMilli(), pe.UnixMilli()
			}
			m += 3
			if m > 12 {
				m -= 12
				y++
			}
		}

	case storev1.BillingPeriod_BILLING_PERIOD_ANNUAL:
		y, m, _ := start.Date()
		for {
			ps := time.Date(y, m, start.Day(), 0, 0, 0, 0, time.UTC)
			pe := time.Date(y+1, m, start.Day(), 0, 0, 0, 0, time.UTC)
			if pe.After(now) {
				return ps.UnixMilli(), pe.UnixMilli()
			}
			y++
		}

	default:
		// Default to monthly
		y, m, _ := now.Date()
		ps := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		pe := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
		return ps.UnixMilli(), pe.UnixMilli()
	}
}

// PreviousPeriod returns the billing period immediately before the one containing asOf.
func PreviousPeriod(contract *storev1.Contract, asOf int64) (periodStart, periodEnd int64) {
	currentStart, _ := CurrentPeriod(contract, asOf)
	// Get the period containing one millisecond before the current start
	return CurrentPeriod(contract, currentStart-1)
}
