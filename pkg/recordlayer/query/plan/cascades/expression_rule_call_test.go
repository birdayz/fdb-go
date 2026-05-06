package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// fixtureScan returns a real RelationalExpression we can stuff into a
// Reference for rule-call tests.
func fixtureScan(name string) expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
}

func TestExpressionRuleCall_NilContextNormalised(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	rc := NewExpressionRuleCall(ref, nil, nil)
	if rc.Context == nil {
		t.Fatal("nil PlanContext not normalised to EmptyPlanContext")
	}
	if got := rc.Context.GetPlannerConfiguration(); got.AllowDuplicateProjections {
		t.Fatal("default config not preserved")
	}
}

func TestExpressionRuleCall_YieldInsertsIntoReference(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	rc := NewExpressionRuleCall(ref, matching.NewBindings(), EmptyPlanContext())
	newScan := fixtureScan("U")
	if inserted := rc.Yield(newScan); !inserted {
		t.Fatal("Yield reported duplicate on a structurally-distinct expression")
	}
	if got := ref.Members(); len(got) != 2 {
		t.Fatalf("Reference has %d members, want 2", len(got))
	}
	if rc.Yielded()[0] != newScan {
		t.Fatal("Yielded() didn't record the yielded expression")
	}
}

func TestExpressionRuleCall_YieldDedupes(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	rc := NewExpressionRuleCall(ref, matching.NewBindings(), EmptyPlanContext())
	dup := fixtureScan("T") // same canonical form as the existing member
	if inserted := rc.Yield(dup); inserted {
		t.Fatal("Yield reported success on a structurally-equivalent duplicate — should dedup")
	}
	if got := ref.Members(); len(got) != 1 {
		t.Fatalf("Reference grew despite dedup — has %d members, want 1", len(got))
	}
	// Yielded() still records the call (rule's intent), even though
	// Reference dedup absorbed the result.
	if got := rc.Yielded(); len(got) != 1 {
		t.Fatalf("Yielded() size=%d after dedup, want 1 (records rule's intent)", len(got))
	}
}

func TestExpressionRuleCall_Yield_PanicsOnNil(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	rc := NewExpressionRuleCall(ref, matching.NewBindings(), EmptyPlanContext())
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on Yield(nil)")
		}
		// Sanity: the yielded list should NOT have grown — validate-
		// first ordering means state isn't corrupted on the panic path.
		if got := rc.Yielded(); len(got) != 0 {
			t.Fatalf("Yielded() leaked nil entry: %v", got)
		}
	}()
	rc.Yield(nil)
}

func TestExpressionRuleCall_BindingsAccessible(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	bindings := matching.NewBindings()
	rc := NewExpressionRuleCall(ref, bindings, EmptyPlanContext())
	if rc.Bindings != bindings {
		t.Fatal("Bindings field not set to constructor argument")
	}
}
