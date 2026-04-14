package billing_test

import (
	"testing"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
)

func TestFlatPricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	amount, desc, err := billing.CalculateCharge(0, &storev1.PricingModel{
		Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(9900)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(9900)))
	g.Expect(desc).To(Equal("flat fee"))

	// Flat with quantity > 0 (quantity is irrelevant for flat)
	amount, _, err = billing.CalculateCharge(999, &storev1.PricingModel{
		Model: &storev1.PricingModel_Flat{Flat: &storev1.FlatPricing{AmountCents: proto.Int64(100)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(100)))
}

func TestPerUnitPricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Normal
	amount, _, err := billing.CalculateCharge(100, &storev1.PricingModel{
		Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(500)))

	// Zero quantity
	amount, _, err = billing.CalculateCharge(0, &storev1.PricingModel{
		Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(5)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(0)))

	// Single unit
	amount, _, err = billing.CalculateCharge(1, &storev1.PricingModel{
		Model: &storev1.PricingModel_PerUnit{PerUnit: &storev1.PerUnitPricing{UnitPriceCents: proto.Int64(1)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1)))
}

func TestTieredPricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	tiers := []*storev1.Tier{
		{UpTo: proto.Int64(100), PriceCents: proto.Int64(10)},
		{UpTo: proto.Int64(1000), PriceCents: proto.Int64(5)},
		{UpTo: proto.Int64(0), PriceCents: proto.Int64(2)}, // infinity
	}
	pricing := &storev1.PricingModel{
		Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{Tiers: tiers}},
	}

	// Within first tier
	amount, _, err := billing.CalculateCharge(50, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(500))) // 50 * 10

	// Exactly at first tier boundary
	amount, _, err = billing.CalculateCharge(100, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1000))) // 100 * 10

	// Across two tiers: 100*10 + 150*5 = 1750
	amount, _, err = billing.CalculateCharge(250, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1750)))

	// Across all three tiers: 100*10 + 900*5 + 500*2 = 1000 + 4500 + 1000 = 6500
	amount, _, err = billing.CalculateCharge(1500, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(6500)))

	// Zero quantity
	amount, _, err = billing.CalculateCharge(0, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(0)))

	// Single tier (no infinity)
	amount, _, err = billing.CalculateCharge(50, &storev1.PricingModel{
		Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{
			Tiers: []*storev1.Tier{{UpTo: proto.Int64(100), PriceCents: proto.Int64(10)}},
		}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(500)))

	// Empty tiers
	_, _, err = billing.CalculateCharge(50, &storev1.PricingModel{
		Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{}},
	})
	g.Expect(err).To(HaveOccurred())
}

func TestVolumePricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	tiers := []*storev1.Tier{
		{UpTo: proto.Int64(100), PriceCents: proto.Int64(10)},
		{UpTo: proto.Int64(1000), PriceCents: proto.Int64(5)},
		{UpTo: proto.Int64(0), PriceCents: proto.Int64(2)},
	}
	pricing := &storev1.PricingModel{
		Model: &storev1.PricingModel_Volume{Volume: &storev1.VolumePricing{Tiers: tiers}},
	}

	// Within first tier: all at 10c
	amount, _, err := billing.CalculateCharge(50, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(500)))

	// Exactly at boundary: still first tier
	amount, _, err = billing.CalculateCharge(100, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1000)))

	// Falls in second tier: ALL units at 5c
	amount, _, err = billing.CalculateCharge(250, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1250))) // 250 * 5

	// Falls in infinity tier: ALL at 2c
	amount, _, err = billing.CalculateCharge(1500, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(3000))) // 1500 * 2

	// Zero
	amount, _, err = billing.CalculateCharge(0, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(0))) // 0 * 10

	// Empty tiers
	_, _, err = billing.CalculateCharge(50, &storev1.PricingModel{
		Model: &storev1.PricingModel_Volume{Volume: &storev1.VolumePricing{}},
	})
	g.Expect(err).To(HaveOccurred())
}

func TestPackagePricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	pricing := &storev1.PricingModel{
		Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{
			PackageSize: proto.Int64(1000), PackagePriceCents: proto.Int64(1000),
		}},
	}

	// Exact divisor
	amount, _, err := billing.CalculateCharge(3000, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(3000))) // 3 packages

	// Partial package: 2501 → 3 packages
	amount, _, err = billing.CalculateCharge(2501, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(3000)))

	// Single unit: 1 → 1 package
	amount, _, err = billing.CalculateCharge(1, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(1000)))

	// Zero
	amount, _, err = billing.CalculateCharge(0, pricing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(0)))

	// Invalid package size
	_, _, err = billing.CalculateCharge(100, &storev1.PricingModel{
		Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{
			PackageSize: proto.Int64(0),
		}},
	})
	g.Expect(err).To(HaveOccurred())
}

func TestBpsPricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// 25 bps on $1000 (100000 cents) = $2.50 (250 cents)
	amount, _, err := billing.CalculateCharge(100000, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(25)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(250)))

	// 100 bps (1%) on $50 (5000 cents) = $0.50 (50 cents)
	amount, _, err = billing.CalculateCharge(5000, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(100)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(50)))

	// Zero transaction
	amount, _, err = billing.CalculateCharge(0, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(25)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(0)))

	// Small amount: rounding down (integer division)
	// 1 bps on 99 cents = 99*1/10000 = 0 (truncated)
	amount, _, err = billing.CalculateCharge(99, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(1)}},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(amount).To(Equal(int64(0)))
}

func TestNilPricing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	_, _, err := billing.CalculateCharge(100, nil)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("nil pricing"))
}

func TestTieredExactBoundaries(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	tiers := []*storev1.Tier{
		{UpTo: proto.Int64(10), PriceCents: proto.Int64(100)},
		{UpTo: proto.Int64(20), PriceCents: proto.Int64(50)},
		{UpTo: proto.Int64(0), PriceCents: proto.Int64(10)},
	}
	pricing := &storev1.PricingModel{
		Model: &storev1.PricingModel_Tiered{Tiered: &storev1.TieredPricing{Tiers: tiers}},
	}

	// Exactly at first boundary
	amount, _, _ := billing.CalculateCharge(10, pricing)
	g.Expect(amount).To(Equal(int64(1000))) // 10 * 100

	// One over first boundary: 10*100 + 1*50 = 1050
	amount, _, _ = billing.CalculateCharge(11, pricing)
	g.Expect(amount).To(Equal(int64(1050)))

	// Exactly at second boundary: 10*100 + 10*50 = 1500
	amount, _, _ = billing.CalculateCharge(20, pricing)
	g.Expect(amount).To(Equal(int64(1500)))

	// One over second boundary: 10*100 + 10*50 + 1*10 = 1510
	amount, _, _ = billing.CalculateCharge(21, pricing)
	g.Expect(amount).To(Equal(int64(1510)))

	// Very large number: 10*100 + 10*50 + 999980*10 = 1000 + 500 + 9999800 = 10001300
	amount, _, _ = billing.CalculateCharge(1000000, pricing)
	g.Expect(amount).To(Equal(int64(10001300)))
}

func TestVolumeBoundaryEdge(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	tiers := []*storev1.Tier{
		{UpTo: proto.Int64(100), PriceCents: proto.Int64(10)},
		{UpTo: proto.Int64(0), PriceCents: proto.Int64(5)},
	}
	pricing := &storev1.PricingModel{
		Model: &storev1.PricingModel_Volume{Volume: &storev1.VolumePricing{Tiers: tiers}},
	}

	// At boundary: all at first tier
	amount, _, _ := billing.CalculateCharge(100, pricing)
	g.Expect(amount).To(Equal(int64(1000))) // 100 * 10

	// One over: all at second tier
	amount, _, _ = billing.CalculateCharge(101, pricing)
	g.Expect(amount).To(Equal(int64(505))) // 101 * 5

	// Volume is CHEAPER than tiered at high volumes — this is the expected behavior
	// 100 units: volume = 1000, tiered would be 1000 (same at boundary)
	// 101 units: volume = 505, tiered would be 1000 + 5 = 1005
}

func TestPackageEdgeCases(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	pricing := &storev1.PricingModel{
		Model: &storev1.PricingModel_Package{Package: &storev1.PackagePricing{
			PackageSize: proto.Int64(100), PackagePriceCents: proto.Int64(500),
		}},
	}

	// Exact multiple
	amount, _, _ := billing.CalculateCharge(300, pricing)
	g.Expect(amount).To(Equal(int64(1500))) // 3 packages

	// One unit: still one full package
	amount, _, _ = billing.CalculateCharge(1, pricing)
	g.Expect(amount).To(Equal(int64(500)))

	// 99 units: one package
	amount, _, _ = billing.CalculateCharge(99, pricing)
	g.Expect(amount).To(Equal(int64(500)))

	// 101 units: two packages
	amount, _, _ = billing.CalculateCharge(101, pricing)
	g.Expect(amount).To(Equal(int64(1000)))
}

func TestBpsPrecision(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// 1 bps = 0.01% = 0.0001
	// On $100.00 (10000 cents): 10000 * 1 / 10000 = 1 cent
	amount, _, _ := billing.CalculateCharge(10000, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(1)}},
	})
	g.Expect(amount).To(Equal(int64(1)))

	// 10000 bps = 100% — full amount returned
	amount, _, _ = billing.CalculateCharge(5000, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(10000)}},
	})
	g.Expect(amount).To(Equal(int64(5000)))

	// 5000 bps = 50%
	amount, _, _ = billing.CalculateCharge(1000, &storev1.PricingModel{
		Model: &storev1.PricingModel_Bps{Bps: &storev1.BpsPricing{BasisPoints: proto.Int64(5000)}},
	})
	g.Expect(amount).To(Equal(int64(500)))
}

func TestBucketHour(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Exact hour boundary
	g.Expect(billing.BucketHour(1718402400000)).To(Equal(int64(1718402400000)))

	// Mid-hour
	g.Expect(billing.BucketHour(1718402400000 + 1800000)).To(Equal(int64(1718402400000)))

	// Just before next hour
	g.Expect(billing.BucketHour(1718402400000 + 3599999)).To(Equal(int64(1718402400000)))

	// Exactly next hour
	g.Expect(billing.BucketHour(1718402400000 + 3600000)).To(Equal(int64(1718402400000 + 3600000)))

	// Zero
	g.Expect(billing.BucketHour(0)).To(Equal(int64(0)))
}
