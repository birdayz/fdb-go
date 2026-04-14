// Package billing implements pricing calculation and invoice generation.
package billing

import (
	"fmt"
	"math"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

// CalculateCharge computes the amount in cents for a given usage quantity
// and pricing model. Returns the amount and a human-readable description.
func CalculateCharge(quantity int64, pricing *storev1.PricingModel) (amountCents int64, description string, err error) {
	if pricing == nil {
		return 0, "", fmt.Errorf("nil pricing model")
	}

	switch m := pricing.Model.(type) {
	case *storev1.PricingModel_Flat:
		return calculateFlat(m.Flat)
	case *storev1.PricingModel_PerUnit:
		return calculatePerUnit(quantity, m.PerUnit)
	case *storev1.PricingModel_Tiered:
		return calculateTiered(quantity, m.Tiered)
	case *storev1.PricingModel_Volume:
		return calculateVolume(quantity, m.Volume)
	case *storev1.PricingModel_Package:
		return calculatePackage(quantity, m.Package)
	case *storev1.PricingModel_Bps:
		return calculateBPS(quantity, m.Bps)
	default:
		return 0, "", fmt.Errorf("unknown pricing model type: %T", pricing.Model)
	}
}

func calculateFlat(p *storev1.FlatPricing) (int64, string, error) {
	return p.GetAmountCents(), "flat fee", nil
}

func calculatePerUnit(quantity int64, p *storev1.PerUnitPricing) (int64, string, error) {
	amount := quantity * p.GetUnitPriceCents()
	desc := fmt.Sprintf("%d units @ %s/unit", quantity, formatCents(p.GetUnitPriceCents()))
	return amount, desc, nil
}

// calculateTiered: each tier is priced independently.
// Example: tiers [{up_to:100, price:10}, {up_to:1000, price:5}, {up_to:0, price:2}]
// For 250 units: 100*10 + 150*5 = 1000 + 750 = 1750
func calculateTiered(quantity int64, p *storev1.TieredPricing) (int64, string, error) {
	if len(p.GetTiers()) == 0 {
		return 0, "", fmt.Errorf("tiered pricing has no tiers")
	}

	var total int64
	remaining := quantity
	prevBound := int64(0)

	for _, tier := range p.GetTiers() {
		if remaining <= 0 {
			break
		}
		upperBound := tier.GetUpTo()
		if upperBound == 0 {
			upperBound = math.MaxInt64 // infinity
		}
		tierSize := upperBound - prevBound
		consumed := min(remaining, tierSize)
		total += consumed * tier.GetPriceCents()
		remaining -= consumed
		prevBound = upperBound
	}

	desc := fmt.Sprintf("%d units (tiered)", quantity)
	return total, desc, nil
}

// calculateVolume: ALL units priced at the rate of the tier they fall into.
// Example: tiers [{up_to:100, price:10}, {up_to:1000, price:5}]
// For 250 units: 250*5 = 1250 (all at second tier rate)
func calculateVolume(quantity int64, p *storev1.VolumePricing) (int64, string, error) {
	if len(p.GetTiers()) == 0 {
		return 0, "", fmt.Errorf("volume pricing has no tiers")
	}

	for _, tier := range p.GetTiers() {
		upperBound := tier.GetUpTo()
		if upperBound == 0 || quantity <= upperBound {
			amount := quantity * tier.GetPriceCents()
			desc := fmt.Sprintf("%d units @ %s/unit (volume)", quantity, formatCents(tier.GetPriceCents()))
			return amount, desc, nil
		}
	}

	// Should not reach here if last tier has up_to=0
	lastTier := p.GetTiers()[len(p.GetTiers())-1]
	amount := quantity * lastTier.GetPriceCents()
	desc := fmt.Sprintf("%d units @ %s/unit (volume)", quantity, formatCents(lastTier.GetPriceCents()))
	return amount, desc, nil
}

// calculatePackage: prepaid blocks of usage. Partial block = full price.
// Example: 1000 units/package @ $10/package. For 2500 units: 3 packages * $10 = $30
func calculatePackage(quantity int64, p *storev1.PackagePricing) (int64, string, error) {
	if p.GetPackageSize() <= 0 {
		return 0, "", fmt.Errorf("package size must be > 0")
	}
	packages := (quantity + p.GetPackageSize() - 1) / p.GetPackageSize() // ceiling division
	amount := packages * p.GetPackagePriceCents()
	desc := fmt.Sprintf("%d packages of %d (quantity: %d)", packages, p.GetPackageSize(), quantity)
	return amount, desc, nil
}

// calculateBPS: basis points on the usage value (which represents transaction amount in cents).
// Example: 25 bps on $1000 transaction = $1000 * 0.0025 = $2.50 = 250 cents
func calculateBPS(transactionAmountCents int64, p *storev1.BpsPricing) (int64, string, error) {
	// BPS = basis points. 1 bps = 0.01% = 0.0001
	// amount = value * bps / 10000
	amount := (transactionAmountCents * p.GetBasisPoints()) / 10000
	desc := fmt.Sprintf("%s @ %d bps", formatCents(transactionAmountCents), p.GetBasisPoints())
	return amount, desc, nil
}

func formatCents(cents int64) string {
	if cents%100 == 0 {
		return fmt.Sprintf("$%d", cents/100)
	}
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
