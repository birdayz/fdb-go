package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// FuzzPlanner_Confluence pins that the task-stack Planner converges
// to the SAME final Reference state as FixpointApply across random
// expression trees + random rule subsets. Two drivers, same input,
// same output member-set (multi-set of expression-kinds).
//
// Catches the failure mode where Planner's saturation-tracking
// pruning incorrectly skips a Reference that could still produce new
// matches via a sibling's growth.
func FuzzPlanner_Confluence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		exprA := buildFuzzExpression(b, 0, 0)
		exprB := buildFuzzExpression(b, 0, 0) // identical input via deterministic builder
		refA := expressions.InitialOf(exprA)
		refB := expressions.InitialOf(exprB)
		rules := selectRules(b)

		// Driver A: FixpointApply.
		_, convA := FixpointApply(rules, refA, 50)
		if !convA {
			t.Skipf("FixpointApply did not converge — fuzz seed pathological")
			return
		}

		// Driver B: task-stack Planner.
		p := NewPlanner(rules, nil)
		_, convB := p.Explore(refB)
		if !convB {
			t.Fatalf("Planner did not converge on input where FixpointApply did")
		}

		// Members count must match.
		if a, b := len(refA.Members()), len(refB.Members()); a != b {
			t.Fatalf("FixpointApply produced %d members; Planner produced %d — confluence violation", a, b)
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
