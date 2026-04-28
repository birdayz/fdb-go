package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// FuzzPlanner_WithBatchA_NoPanic pins that the Planner with the
// FULL rule set — DefaultExpressionRules + BatchAExpressionRules —
// terminates and doesn't panic on random expression trees. The
// existing FuzzPlanner_PlanFullPipeline drives a smaller logical-
// only set; this one specifically stresses the implement rules
// (PrimaryScan / ImplementFilter / ImplementSort /
// ImplementDistinct / ImplementTypeFilter / ImplementUnion /
// ImplementIntersection) interacting with the logical-rewrite
// chain.
//
// Why a separate target: the implement rules introduce physical
// wrappers into References which the logical rewrites must
// tolerate. The 5-wrapper-symmetry asymmetries that bit us mid-
// shift would surface here as planner non-termination or panics
// — not as missed-fire bugs in unit tests.
//
// MaxTasks set generously since the combined rule set produces
// more alternatives than the logical-only set; the seed expression
// shapes from buildFuzzExpression typically converge in <2k tasks.
func FuzzPlanner_WithBatchA_NoPanic(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 16))

	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)

		// Combine logical-rewrite + Batch A read-side implement rules.
		rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)

		p := NewPlanner(rules, nil)
		p.MaxTasks = 50_000

		plan, _, err := p.Plan(ref)
		if err != nil && err != ErrPlannerCapHit {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		// Plan succeeded → root must have a BestMember stamp.
		if err == nil && !p.HasBestMember(ref) {
			t.Fatal("Plan succeeded but root Reference has no BestMember stamp — invariant break")
		}
		// Plan succeeded → returned plan must be non-nil (cost
		// extraction always picks SOME member from a non-empty
		// Reference).
		if err == nil && plan == nil {
			t.Fatal("Plan succeeded but plan is nil — invariant break")
		}
	})
}
