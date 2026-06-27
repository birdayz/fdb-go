package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// FuzzPlanner_Determinism pins that the task-stack Planner produces
// the SAME final Reference state on identical inputs. Two fresh
// Planner instances, same expression tree, same rules, same order →
// same member count.
//
// Catches non-determinism bugs (map iteration order leaking into
// results, pointer-address-dependent behavior, etc.).
//
// Note: the Planner is NOT rule-order-independent — the Memo's
// cross-Reference sharing makes earlier rules' yields visible to
// later rules within the same round (Memo state grows during rule
// execution). This matches Java's Cascades (fixed rule registration
// order, deterministic result). The invariant tested here is: given
// the same inputs and same rule order, the result is identical.
func FuzzPlanner_Determinism(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		exprA := buildFuzzExpression(b, 0, 0)
		exprB := buildFuzzExpression(b, 0, 0)
		refA := expressions.InitialOf(exprA)
		refB := expressions.InitialOf(exprB)
		rules := selectRules(b)

		// Driver A: fresh Planner.
		pA := NewPlanner(rules, nil)
		_, convA := pA.Explore(refA)
		if !convA {
			t.Skip("Planner A did not converge")
			return
		}

		// Driver B: fresh Planner, same rules same order.
		pB := NewPlanner(rules, nil)
		_, convB := pB.Explore(refB)
		if !convB {
			t.Fatalf("Planner B did not converge on input where Planner A did")
		}

		// Member counts must be identical (determinism).
		if a, b := len(refA.Members()), len(refB.Members()); a != b {
			t.Fatalf("run A produced %d members; run B produced %d — non-determinism", a, b)
		}
	})
}

// FuzzPlanner_Idempotence pins that Explore is idempotent: a second
// call on the same Reference doesn't grow the member set.
func FuzzPlanner_Idempotence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil)

		_, convA := p.Explore(ref)
		if !convA {
			t.Skip("first Explore did not converge")
			return
		}
		size1 := len(ref.Members())

		_, convB := p.Explore(ref)
		if !convB {
			t.Fatal("second Explore did not converge")
		}
		if got := len(ref.Members()); got != size1 {
			t.Fatalf("second Explore grew Reference from %d to %d (non-idempotent)", size1, got)
		}
	})
}

// FuzzPlanner_PlanFullPipeline pins that the full Plan() entry
// point (EXPLORE + OPTIMIZE) doesn't panic on random inputs and
// returns either a valid plan or ErrPlannerCapHit. Catches
// pathological inputs that break the OPTIMIZE phase or the
// extract recursion.
func FuzzPlanner_PlanFullPipeline(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		p.MaxTasks = 100_000

		plan, _, err := p.Plan(ref)
		if err != nil && err != ErrPlannerCapHit {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		if err == nil && plan == nil {
			t.Fatal("Plan returned nil plan + nil error — invariant break")
		}
		// Plan called BestMember internally; verify it's populated
		// for the root.
		if err == nil && !p.HasBestMember(ref) {
			t.Fatal("Plan succeeded but root Reference has no BestMember stamp")
		}
	})
}

// FuzzPlanner_MemoConsistency pins that after Explore, the Memo's
// internal index is consistent: every Reference in the Memo has all
// its members' child References also in the Memo.
func FuzzPlanner_MemoConsistency(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{3, 7, 2, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil)
		p.MaxTasks = 100_000

		_, conv := p.Explore(ref)
		if !conv {
			t.Skip("did not converge")
			return
		}
		memo := p.Memo()
		if memo == nil {
			t.Fatal("Memo is nil after Explore")
		}
		// Verify every Reference in the Memo has its children also indexed.
		for mRef := range memo.References() {
			for _, member := range mRef.Members() {
				for _, q := range member.GetQuantifiers() {
					child := q.GetRangesOver()
					if child == nil {
						continue
					}
					if !memo.ContainsReference(child) {
						t.Fatalf("Memo inconsistency: child Reference of %T not in Memo", member)
					}
				}
			}
		}
	})
}

// FuzzPlanner_InitialMemberPreserved pins that the original input
// expression is never removed from the Reference — rules can ADD
// alternatives but not REPLACE the input.
func FuzzPlanner_InitialMemberPreserved(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		initial := ref.Get()
		rules := selectRules(b)
		p := NewPlanner(rules, nil)

		_, conv := p.Explore(ref)
		if !conv {
			t.Skip()
			return
		}
		members := ref.Members()
		if len(members) == 0 || members[0] != initial {
			t.Fatalf("initial member not preserved at index 0 (members[0]=%T, initial=%T)", members[0], initial)
		}
	})
}
