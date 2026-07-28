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
func TestPointProbeProofsAgreeOnWideningEquality(t *testing.T) {
	t.Parallel()

	mk := func(lit any) *plans.RecordQueryIndexPlan {
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: values.LiteralValue(lit),
		})
		return plans.NewRecordQueryIndexPlan("IDX",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"T"}, values.UnknownType, false,
		).WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
	}

	for _, tc := range []struct {
		name       string
		lit        any
		wantOneRow bool
	}{
		{"nonzero float pins one key", float64(5), true},
		{"positive zero widens to two keys", float64(0), false},
		{"negative zero widens to two keys", math.Copysign(0, -1), false},
		{"integer equality is unaffected", int64(0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mk(tc.lit)
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
			// The point: all proofs must agree with EACH OTHER, whatever the
			// answer. A disagreement means one plan has two cardinalities.
			if gotProp != gotIdx {
				t.Fatalf("proofs DISAGREE for %s: property says one-row=%v, cost model says %v — "+
					"the same plan cannot have two cardinalities; join ranking would use the "+
					"false one", tc.name, gotProp, gotIdx)
			}
		})
	}
}
