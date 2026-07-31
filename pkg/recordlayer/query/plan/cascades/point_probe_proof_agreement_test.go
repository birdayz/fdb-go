package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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

	mkIndex := func(lit any) *plans.RecordQueryIndexPlan {
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: values.LiteralValue(lit),
		})
		return plans.NewRecordQueryIndexPlan("IDX",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"T"}, values.UnknownType, false,
		).WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
	}

	// A scan with its primary key STAMPED on the plan — the shape that took
	// pkFullyEqualityBound's early-return path, the one the guard used to miss.
	mkScan := func(lit any) *plans.RecordQueryScanPlan {
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: values.LiteralValue(lit),
		})
		return plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithPrimaryKey([]values.Value{&values.FieldValue{Field: "V", Typ: values.NullableDouble}}).
			WithScanComparisons([]*predicates.ComparisonRange{res.Range})
	}

	cases := []struct {
		name       string
		lit        any
		wantOneRow bool
	}{
		{"nonzero float pins one key", float64(5), true},
		{"positive zero widens to two keys", float64(0), false},
		{"negative zero widens to two keys", math.Copysign(0, -1), false},
		{"integer equality is unaffected", int64(0), true},
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
				gotProp := !computeCardinalities(nil, p).GetMaxCardinality().IsUnknown()
				// Proof 2: the cost model's plan-local bound.
				_, gotIdx := indexProvableMaxCard(p)
				// Proof 3: the shared widening predicate the others consult.
				widens := properties.AnyEqualityWidensBeyondOneKey(comps)

				if gotProp != tc.wantOneRow {
					t.Errorf("computeCardinalities proves one row = %v, want %v", gotProp, tc.wantOneRow)
				}
				if gotIdx != tc.wantOneRow {
					t.Errorf("indexProvableMaxCard = %v, want %v", gotIdx, tc.wantOneRow)
				}
				if widens == tc.wantOneRow {
					t.Errorf("AnyEqualityWidensBeyondOneKey = %v, want %v", widens, !tc.wantOneRow)
				}
				if gotProp != gotIdx {
					t.Fatalf("proofs DISAGREE for %s: property says one-row=%v, cost model says %v — "+
						"the same plan cannot have two cardinalities; join ranking would use the "+
						"false one", tc.name, gotProp, gotIdx)
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
				gotProp := !computeCardinalities(nil, p).GetMaxCardinality().IsUnknown()
				// Proof 2: the logical-memo walk's data-access bound.
				_, gotLogical := scanProvableMaxCard(p)
				// Proof 3: the CONCRETE walk's data-access bound. Same helper,
				// different caller — and the caller that made the old gap
				// reachable from the join-ordering comparison.
				_, gotConcrete := scanPlanProvableMaxCard(p, nil)
				// Proof 4: the shared widening predicate.
				widens := properties.AnyEqualityWidensBeyondOneKey(comps)

				if gotProp != tc.wantOneRow {
					t.Errorf("computeCardinalities proves one row = %v, want %v", gotProp, tc.wantOneRow)
				}
				if gotLogical != tc.wantOneRow {
					t.Errorf("scanProvableMaxCard = %v, want %v", gotLogical, tc.wantOneRow)
				}
				if gotConcrete != tc.wantOneRow {
					t.Errorf("scanPlanProvableMaxCard = %v, want %v", gotConcrete, tc.wantOneRow)
				}
				if widens == tc.wantOneRow {
					t.Errorf("AnyEqualityWidensBeyondOneKey = %v, want %v", widens, !tc.wantOneRow)
				}
				if gotProp != gotLogical || gotProp != gotConcrete {
					t.Fatalf("proofs DISAGREE for scan %s: property=%v logical-walk=%v concrete-walk=%v.\n"+
						"pkFullyEqualityBound's widening guard must sit ABOVE its stamped-primary-key "+
						"early return, or a stamped-PK scan is proven at-most-one by the cost walks "+
						"while the property layer correctly declines — one plan, two cardinalities, "+
						"and under RFC-195's clamp the false max=1 CAPS the honest estimate.",
						tc.name, gotProp, gotLogical, gotConcrete)
				}
			})
		}
	})
}
