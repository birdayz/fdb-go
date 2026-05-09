// Package properties — Cardinality and Cardinalities types.
//
// Ports Java's CardinalitiesProperty.Cardinality and
// CardinalitiesProperty.Cardinalities 1:1. A Cardinality is a single
// bound (known int64 or unknown); Cardinalities is a min/max pair.
//
// The merge helpers (IntersectCardinalities, UnionCardinalities,
// WeakenCardinalities) match Java's visitor-private methods exactly,
// including their unknown-handling semantics.
//
// This file also retains the old EstimateCardinality helpers that wrap
// the Cost-walk. The new Cardinalities type is a SEPARATE property
// computed on physical plans (see plan_properties.go); the old
// EstimateCardinality is a heuristic on logical expressions. Both
// coexist — consumers pick whichever is appropriate.
package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// ---------------------------------------------------------------------------
// Cardinality — a single bound (known int64 or unknown)
// ---------------------------------------------------------------------------

// Cardinality represents a single cardinality bound — either a known
// non-negative int64 or unknown. Matches Java's
// CardinalitiesProperty.Cardinality.
type Cardinality struct {
	value int64
	known bool
}

// OfCardinality returns a known Cardinality with the given value.
// Panics if v < 0, matching Java's Preconditions.checkArgument.
func OfCardinality(v int64) Cardinality {
	if v < 0 {
		panic("cardinality must be non-negative")
	}
	return Cardinality{value: v, known: true}
}

// UnknownCardinality returns an unknown Cardinality.
func UnknownCardinality() Cardinality {
	return Cardinality{}
}

// IsUnknown reports whether this cardinality is unknown.
func (c Cardinality) IsUnknown() bool {
	return !c.known
}

// Value returns the cardinality value. Panics if unknown, matching
// Java's OptionalLong.getAsLong() semantics.
func (c Cardinality) Value() int64 {
	if !c.known {
		panic("cardinality is unknown")
	}
	return c.value
}

// Times multiplies two cardinalities. If either is unknown, the
// result is unknown. Matches Java's Cardinality.times().
func (c Cardinality) Times(other Cardinality) Cardinality {
	if c.IsUnknown() || other.IsUnknown() {
		return UnknownCardinality()
	}
	return OfCardinality(c.value * other.value)
}

// Floor returns max(value, minimum). If unknown, returns the receiver
// unchanged. If the current value >= minimum, returns the receiver
// unchanged. Matches Java's Cardinality.floor().
func (c Cardinality) Floor(minimum int64) Cardinality {
	if !c.known || c.value >= minimum {
		return c
	}
	return OfCardinality(minimum)
}

// Equal reports whether two cardinalities are equal.
func (c Cardinality) Equal(other Cardinality) bool {
	if c.known != other.known {
		return false
	}
	if !c.known {
		return true // both unknown
	}
	return c.value == other.value
}

// ---------------------------------------------------------------------------
// Cardinalities — min/max pair
// ---------------------------------------------------------------------------

// Cardinalities captures both minimum and maximum cardinality bounds
// of an expression. Matches Java's CardinalitiesProperty.Cardinalities.
type Cardinalities struct {
	Min Cardinality
	Max Cardinality
}

// GetMinCardinality returns the minimum cardinality bound.
func (c Cardinalities) GetMinCardinality() Cardinality { return c.Min }

// GetMaxCardinality returns the maximum cardinality bound.
func (c Cardinalities) GetMaxCardinality() Cardinality { return c.Max }

// Times multiplies both bounds with the other Cardinalities.
func (c Cardinalities) Times(other Cardinalities) Cardinalities {
	return Cardinalities{
		Min: c.Min.Times(other.Min),
		Max: c.Max.Times(other.Max),
	}
}

// Floor returns new Cardinalities with both min and max floored at the
// given minimum. If neither bound changes, returns the receiver.
// Matches Java's Cardinalities.floor().
func (c Cardinalities) Floor(minimum int64) Cardinalities {
	newMin := c.Min.Floor(minimum)
	newMax := c.Max.Floor(minimum)
	if newMin.Equal(c.Min) && newMax.Equal(c.Max) {
		return c
	}
	return Cardinalities{Min: newMin, Max: newMax}
}

// Equal reports whether two Cardinalities are equal.
func (c Cardinalities) Equal(other Cardinalities) bool {
	return c.Min.Equal(other.Min) && c.Max.Equal(other.Max)
}

// ---------------------------------------------------------------------------
// Factory functions — match Java's static factory methods
// ---------------------------------------------------------------------------

// UnknownCardinalities returns Cardinalities where both min and max
// are unknown.
func UnknownCardinalities() Cardinalities {
	return Cardinalities{Min: UnknownCardinality(), Max: UnknownCardinality()}
}

// ExactlyOne returns Cardinalities{min=1, max=1}.
func ExactlyOne() Cardinalities {
	return Cardinalities{Min: OfCardinality(1), Max: OfCardinality(1)}
}

// AtMostOne returns Cardinalities{min=0, max=1}.
func AtMostOne() Cardinalities {
	return Cardinalities{Min: OfCardinality(0), Max: OfCardinality(1)}
}

// UnknownMaxCardinality returns Cardinalities{min=0, max=unknown}.
func UnknownMaxCardinality() Cardinalities {
	return Cardinalities{Min: OfCardinality(0), Max: UnknownCardinality()}
}

// ---------------------------------------------------------------------------
// Merge helpers — match Java's visitor methods exactly
// ---------------------------------------------------------------------------

// IntersectCardinalities merges cardinalities for an intersection
// operation. For min: if both sides are known, set to 0 (intersection
// can be empty); if either is unknown, result is unknown. For max:
// take the min of known maxes (intersection bounded by smallest
// input); unknown propagates.
// Matches Java's CardinalitiesVisitor.intersectCardinalities().
func IntersectCardinalities(items []Cardinalities) Cardinalities {
	minCard := UnknownCardinality()
	maxCard := UnknownCardinality()

	for _, c := range items {
		// min cardinality
		if minCard.IsUnknown() {
			minCard = c.GetMinCardinality()
		} else {
			if !c.GetMinCardinality().IsUnknown() {
				// Both known — intersection min is 0 (could be empty).
				minCard = OfCardinality(0)
			} else {
				minCard = UnknownCardinality()
			}
		}

		// max cardinality
		if maxCard.IsUnknown() {
			maxCard = c.GetMaxCardinality()
		} else {
			if !c.GetMaxCardinality().IsUnknown() {
				v := maxCard.Value()
				ov := c.GetMaxCardinality().Value()
				if ov < v {
					v = ov
				}
				maxCard = OfCardinality(v)
			} else {
				maxCard = UnknownCardinality()
			}
		}
	}

	return Cardinalities{Min: minCard, Max: maxCard}
}

// UnionCardinalities merges cardinalities for a union operation.
// Min and max are sums of known components; unknown propagates.
// Matches Java's CardinalitiesVisitor.unionCardinalities().
func UnionCardinalities(items []Cardinalities) Cardinalities {
	if len(items) == 0 {
		return UnknownMaxCardinality()
	}

	minCard := items[0].GetMinCardinality()
	maxCard := items[0].GetMaxCardinality()

	for _, c := range items[1:] {
		if !minCard.IsUnknown() {
			curMin := c.GetMinCardinality()
			if curMin.IsUnknown() {
				minCard = UnknownCardinality()
			} else {
				minCard = OfCardinality(minCard.Value() + curMin.Value())
			}
		}

		if !maxCard.IsUnknown() {
			curMax := c.GetMaxCardinality()
			if curMax.IsUnknown() {
				maxCard = UnknownCardinality()
			} else {
				maxCard = OfCardinality(maxCard.Value() + curMax.Value())
			}
		}
	}

	return Cardinalities{Min: minCard, Max: maxCard}
}

// WeakenCardinalities merges cardinalities by taking the least
// constraining bounds. For min: take the lower of known mins (or
// unknown if either is unknown). For max: take the higher of known
// maxes (or unknown if either is unknown).
// Matches Java's CardinalitiesVisitor.weakenCardinalities().
func WeakenCardinalities(items []Cardinalities) Cardinalities {
	if len(items) == 0 {
		return UnknownMaxCardinality()
	}

	minCard := items[0].GetMinCardinality()
	maxCard := items[0].GetMaxCardinality()

	for _, c := range items[1:] {
		if !minCard.IsUnknown() {
			curMin := c.GetMinCardinality()
			if curMin.IsUnknown() || minCard.Value() > curMin.Value() {
				minCard = curMin
			}
		}

		if !maxCard.IsUnknown() {
			curMax := c.GetMaxCardinality()
			if curMax.IsUnknown() || maxCard.Value() < curMax.Value() {
				maxCard = curMax
			}
		}
	}

	return Cardinalities{Min: minCard, Max: maxCard}
}

// ---------------------------------------------------------------------------
// Legacy EstimateCardinality helpers — wrap Cost-walk for logical trees
// ---------------------------------------------------------------------------

// EstimateCardinality returns just the cardinality (row count
// estimate) of an expression — wraps the Cost-walk and projects out
// the CPU axis. Useful for cardinality-aware rules / matchers that
// don't need to drag the CPU calculation through.
//
// O(N) over the expression sub-tree (same complexity as
// EstimateCost). Sub-Reference recursion picks the first member,
// matching EstimateCost's policy.
func EstimateCardinality(e expressions.RelationalExpression) float64 {
	return EstimateCost(e).Cardinality
}

// EstimateCardinalityWith uses a custom StatisticsProvider for
// per-record-type cardinality (e.g. via Catalog lookups). Wraps
// EstimateCostWith.
func EstimateCardinalityWith(e expressions.RelationalExpression, stats StatisticsProvider) float64 {
	return EstimateCostWith(e, stats).Cardinality
}

// BestRefCardinality returns the cardinality of the cheapest
// member in a Reference — wraps BestRefCost.
func BestRefCardinality(ref *expressions.Reference) float64 {
	return BestRefCost(ref).Cardinality
}

// CardinalityLess is a Reference.GetBest-compatible comparator that
// orders members by cardinality alone (ignoring CPU). Useful when
// the planner wants to pick the smallest-output member regardless
// of compute cost — e.g. picking the join-build side for a hash
// join.
//
// Ties are broken by member identity (first-appearance wins) — same
// stability contract as CostLess.
func CardinalityLess(a, b expressions.RelationalExpression) bool {
	return EstimateCardinality(a) < EstimateCardinality(b)
}
