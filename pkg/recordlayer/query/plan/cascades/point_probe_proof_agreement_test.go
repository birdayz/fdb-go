package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type pointProbeIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
	unique      bool
}

func (d pointProbeIndexDef) IndexName() string                { return d.name }
func (d pointProbeIndexDef) IndexColumnNames() []string       { return d.columns }
func (d pointProbeIndexDef) IndexRecordTypes() []string       { return d.recordTypes }
func (d pointProbeIndexDef) IndexIsUnique() bool              { return d.unique }
func (d pointProbeIndexDef) IndexPrimaryKeyColumns() []string { return nil }
func (d pointProbeIndexDef) IndexCreatesDuplicates() bool     { return false }

// FOUR independent proofs concluded "unique index + all equalities = exactly one
// row": computeCardinalities, plans.isProvablePointProbe, indexProvableMaxCard
// and scanLikeCost. A zero-valued float equality breaks all four, because the
// executor widens it across both signed zeros and a UNIQUE index legitimately
// holds both.
//
// Fixing one left the same plan carrying CONTRADICTORY cardinality — the
// property said unknown while the cost model still ranked joins on a false
// one-row bound. They now share properties.AnyEqualityWidensBeyondOneKey, and
// this test holds them to the same answer so a fifth copy, or a divergence
// between the existing four, fails here rather than silently skewing plans.
// The SCAN half of this test is not symmetry for its own sake. Building only
// INDEX shapes here is precisely why a live disagreement survived: the index
// proofs consult AnyEqualityWidensBeyondOneKey directly, while the scan proofs
// route through pkFullyEqualityBound, whose widening guard sat AFTER its
// stamped-primary-key early return. Two of that helper's three callers proved
// at-most-one for a stamped-PK scan with a terminal zero-float equality, and no
// shape in this file could express it. The gap was dimensional, not volumetric.
func TestPointProbeProofsAgreeOnWideningEquality(t *testing.T) {
	t.Parallel()
	physicalType := func(lit any) values.Type {
		if _, integer := lit.(int64); integer {
			return values.NullableLong
		}
		return values.NullableDouble
	}

	mkIndex := func(lit any) *plans.RecordQueryIndexPlan {
		keyType := physicalType(lit)
		layout := values.NewRecordType("PointProbeProofIndex", false, []values.Field{
			{Name: "V", FieldType: keyType, Ordinal: 0},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: pointProbeLiteral(lit),
		})
		return mustPointProbeConstruct(plans.NewRecordQueryIndexPlan("IDX",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"T"}, layout, false,
		)).WithKeyComponentTypes([]values.Type{keyType}).
			WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
	}

	// A scan with its primary key STAMPED on the plan — the shape that took
	// pkFullyEqualityBound's early-return path, the one the guard used to miss.
	mkScan := func(lit any) *plans.RecordQueryScanPlan {
		keyType := physicalType(lit)
		layout := values.NewRecordType("PointProbeProofScan", false, []values.Field{
			{Name: "V", FieldType: keyType, Ordinal: 0},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: pointProbeLiteral(lit),
		})
		root := mustPointProbeConstruct(values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("point_probe_scan"), layout))
		key := mustPointProbeConstruct(values.ResolveFieldOrdinals(root, []int{0}))
		return mustPointProbeConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, layout, false)).
			WithPrimaryKey([]values.Value{key}).
			WithScanComparisons([]*predicates.ComparisonRange{res.Range}).
			WithKeyComponentTypes([]values.Type{keyType})
	}

	cases := []struct {
		name       string
		lit        any
		wantMax    int64
		wantFanout bool
	}{
		{"nonzero float pins one key", float64(5), 1, false},
		{"positive zero widens to two keys", float64(0), 2, true},
		{"negative zero widens to two keys", math.Copysign(0, -1), 2, true},
		{"integer equality is unaffected", int64(0), 1, false},
	}

	t.Run("index", func(t *testing.T) {
		t.Parallel()
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				p := mkIndex(tc.lit)
				comps := p.GetScanComparisons()

				// Proof 1: the property layer.
				propMax := computeCardinalities(nil, p).GetMaxCardinality()
				// Proof 2: the cost model's plan-local bound.
				idxMax, gotIdx := indexProvableMaxCard(p)
				// Proof 3: the shared widening predicate the others consult.
				widens := properties.AnyEqualityWidensBeyondOneKey(comps, p.GetKeyComponentTypes())

				if propMax.IsUnknown() || propMax.Value() != tc.wantMax {
					t.Errorf("computeCardinalities max = %+v, want %d", propMax, tc.wantMax)
				}
				if !gotIdx || int64(idxMax) != tc.wantMax {
					t.Errorf("indexProvableMaxCard = (%v,%v), want (%d,true)", idxMax, gotIdx, tc.wantMax)
				}
				if widens != tc.wantFanout {
					t.Errorf("AnyEqualityWidensBeyondOneKey = %v, want %v", widens, tc.wantFanout)
				}
				if propMax.IsUnknown() || !gotIdx || int64(idxMax) != propMax.Value() {
					t.Fatalf("proofs DISAGREE for %s: property max=%+v, cost model says (%v,%v) — "+
						"the same plan cannot have two cardinalities; join ranking would use the "+
						"false one", tc.name, propMax, idxMax, gotIdx)
				}
			})
		}
	})

	t.Run("scan/stampedPrimaryKey", func(t *testing.T) {
		t.Parallel()
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				p := mkScan(tc.lit)
				comps := p.GetScanComparisons()

				// Proof 1: the property layer (routes through the plan's own
				// ProvenCardinalities, hence through plans.isProvablePointProbe).
				propMax := computeCardinalities(nil, p).GetMaxCardinality()
				// Proof 2: the logical-memo walk's data-access bound.
				logicalMax, gotLogical := scanProvableMaxCard(p)
				// Proof 3: the CONCRETE walk's data-access bound. Same helper,
				// different caller — and the caller that made the old gap
				// reachable from the join-ordering comparison.
				concreteMax, gotConcrete := scanPlanProvableMaxCard(p, nil)
				// Proof 4: the shared widening predicate.
				widens := properties.AnyEqualityWidensBeyondOneKey(comps, p.GetKeyComponentTypes())

				if propMax.IsUnknown() || propMax.Value() != tc.wantMax {
					t.Errorf("computeCardinalities max = %+v, want %d", propMax, tc.wantMax)
				}
				if !gotLogical || int64(logicalMax) != tc.wantMax {
					t.Errorf("scanProvableMaxCard = (%v,%v), want (%d,true)", logicalMax, gotLogical, tc.wantMax)
				}
				if !gotConcrete || int64(concreteMax) != tc.wantMax {
					t.Errorf("scanPlanProvableMaxCard = (%v,%v), want (%d,true)", concreteMax, gotConcrete, tc.wantMax)
				}
				if widens != tc.wantFanout {
					t.Errorf("AnyEqualityWidensBeyondOneKey = %v, want %v", widens, tc.wantFanout)
				}
				if propMax.IsUnknown() || !gotLogical || !gotConcrete ||
					int64(logicalMax) != propMax.Value() || int64(concreteMax) != propMax.Value() {
					t.Fatalf("proofs DISAGREE for scan %s: property=%+v logical-walk=(%v,%v) concrete-walk=(%v,%v).\n"+
						"pkFullyEqualityBound's widening guard must sit ABOVE its stamped-primary-key "+
						"early return, or a stamped-PK scan is proven at-most-one by the cost walks "+
						"while the property layer correctly declines — one plan, two cardinalities, "+
						"and under RFC-195's clamp the false max=1 CAPS the honest estimate.",
						tc.name, propMax, logicalMax, gotLogical, concreteMax, gotConcrete)
				}
			})
		}
	})
}

func TestUniqueIndexProofsAgreeOnNullableNullAndComparisonGaps(t *testing.T) {
	t.Parallel()
	rangeOf := func(comparison predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatalf("failed to build %s range", comparison.Type.Symbol())
		}
		return merged.Range
	}
	isNull := rangeOf(predicates.Comparison{Type: predicates.ComparisonIsNull})
	equality := rangeOf(predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7)))
	ctx := NewPlanContextFromIndexDefs([]IndexDef{pointProbeIndexDef{
		name: "U", columns: []string{"V"}, recordTypes: []string{"T"}, unique: true,
	}})
	proofLayout := values.NewRecordType("UniqueIndexProof", false, []values.Field{
		{Name: "V", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
	})
	for _, test := range []struct {
		name      string
		comps     []*predicates.ComparisonRange
		types     []values.Type
		wantKnown bool
		wantMax   int64
	}{
		{name: "nullable IS NULL", comps: []*predicates.ComparisonRange{isNull}, types: []values.Type{values.NullableLong}},
		{name: "non-leading equality gap", comps: []*predicates.ComparisonRange{predicates.EmptyComparisonRange(), equality}, types: []values.Type{values.NotNullLong, values.NotNullLong}},
		{name: "ordinary non-null equality", comps: []*predicates.ComparisonRange{equality}, types: []values.Type{values.NullableLong}, wantKnown: true, wantMax: 1},
		{name: "IS NULL on NOT NULL key is empty", comps: []*predicates.ComparisonRange{isNull}, types: []values.Type{values.NotNullLong}, wantKnown: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := mustPointProbeConstruct(plans.NewRecordQueryIndexPlan(
				"U", test.comps, []string{"T"}, proofLayout, false,
			)).WithKeyComponentTypes(test.types).
				WithIndexMetadata([]string{"V"}, []string{"ID"}, true)

			propertyMax := computeCardinalities(nil, plan).GetMaxCardinality()
			logicalMax, logicalKnown := indexProvableMaxCard(plan)
			concreteMax, concreteKnown := indexPlanProvableMaxCard(plan, []string{"V"}, true)
			if (!propertyMax.IsUnknown()) != test.wantKnown || logicalKnown != test.wantKnown || concreteKnown != test.wantKnown {
				t.Fatalf("proof agreement = propertyKnown:%v logicalKnown:%v concreteKnown:%v, want %v", !propertyMax.IsUnknown(), logicalKnown, concreteKnown, test.wantKnown)
			}
			if test.wantKnown {
				if propertyMax.Value() != test.wantMax || int64(logicalMax) != test.wantMax || int64(concreteMax) != test.wantMax {
					t.Fatalf("proof maxima = property:%d logical:%v concrete:%v, want %d", propertyMax.Value(), logicalMax, concreteMax, test.wantMax)
				}
			}
			cost := concretePlanCost(plan, properties.FixedStatistics{Cardinality: 1000}, ctx)
			if test.wantKnown {
				if cost.Cardinality != float64(test.wantMax) {
					t.Fatalf("concrete cost cardinality = %v, want %d", cost.Cardinality, test.wantMax)
				}
			} else if cost.Cardinality <= 1 {
				t.Fatalf("unproven unique access was priced as a point probe: %+v", cost)
			}
		})
	}
}
