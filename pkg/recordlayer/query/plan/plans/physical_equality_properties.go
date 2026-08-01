package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func physicalEqualityShape(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	allowTerminalWidening bool,
) properties.PhysicalEqualityShape {
	return properties.PhysicalEqualityShapeForComparisons(
		comps, physicalTypes, allowTerminalWidening,
	)
}

// provenFullEqualityMultiplicity returns the exact physical-key multiplicity
// of a full equality bind when the shared shape proves one. Dynamic float and
// genuinely Unknown operands return known=false; known signed zero returns two
// (or its checked Cartesian product), never the old false one-row proof.
func provenFullEqualityMultiplicity(
	comps []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	keyColumnCount int,
) (multiplicity int64, known bool) {
	if !properties.EqualityBoundsCoverKey(comps, keyColumnCount) {
		return 0, false
	}
	shape := physicalEqualityShape(comps, physicalTypes, true)
	if shape.UnsupportedKnownNaN || shape.ProvenRowMultiplicity.IsUnknown() {
		return 0, false
	}
	return shape.ProvenRowMultiplicity.Value(), true
}

// addPhysicalRangeSeekCost attaches fixed plural-range setup to a leaf's
// existing row cost with finite saturation. A statically empty equality still
// pays one setup unit even though it returns no row.
func addPhysicalRangeSeekCost(
	base properties.Cost,
	shape properties.PhysicalEqualityShape,
) properties.Cost {
	if shape.UnsupportedKnownNaN {
		return properties.Cost{
			Cardinality: properties.MaxFiniteHeuristic,
			CPU:         properties.MaxFiniteHeuristic,
		}
	}
	base.Cardinality = properties.SaturatingHeuristicAdd(base.Cardinality, 0)
	base.CPU = properties.SaturatingHeuristicAdd(base.CPU, 0)
	if shape.Unsatisfiable {
		base.Cardinality = 0
		base.CPU = properties.PhysicalRangeSeekCost
		return base
	}
	seekCount := shape.SuccessfulSeekUpperBound
	if seekCount <= 1 {
		return base
	}
	extra := properties.SaturatingHeuristicMultiply(
		float64(seekCount-1), properties.PhysicalRangeSeekCost,
	)
	base.CPU = properties.SaturatingHeuristicAdd(base.CPU, extra)
	return base
}

// uniqueEqualityCost prices a proven full primary/UNIQUE equality bind as a
// small physical set. The first seek uses FetchCPU, additional returned keys
// pay their marginal scan rate, and additional disjoint ranges pay explicit
// setup through addPhysicalRangeSeekCost.
func uniqueEqualityCost(
	multiplicity int64,
	shape properties.PhysicalEqualityShape,
) properties.Cost {
	if multiplicity == 0 {
		return addPhysicalRangeSeekCost(properties.Cost{}, shape)
	}
	cardinality := float64(multiplicity)
	cpu := properties.FetchCPU
	if multiplicity > 1 {
		cpu = properties.SaturatingHeuristicAdd(
			cpu,
			properties.SaturatingHeuristicMultiply(float64(multiplicity-1), properties.ScanCPU),
		)
	}
	return addPhysicalRangeSeekCost(properties.Cost{Cardinality: cardinality, CPU: cpu}, shape)
}
