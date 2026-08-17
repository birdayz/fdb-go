package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// fixtureScan returns a real RelationalExpression we can stuff into a
// Reference for rule-call tests.
func fixtureScan(name string) expressions.RelationalExpression {
	scan, err := expressions.NewFullUnorderedScanExpression([]string{name}, values.NotNullLong)
	if err != nil {
		panic("fixtureScan invariant: " + err.Error())
	}
	return scan
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

func TestExpressionRuleCall_YieldStagesWithoutMutatingReference(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	rc := NewExpressionRuleCall(ref, matching.NewBindings(), EmptyPlanContext())
	newScan := fixtureScan("U")
	rc.Yield(newScan)
	if got := ref.Members(); len(got) != 1 {
		t.Fatalf("Reference changed before the driver committed the rule: has %d members, want 1", len(got))
	}
	if rc.Yielded()[0] != newScan {
		t.Fatal("Yielded() didn't record the yielded expression")
	}
}

func TestExpressionRuleCall_YieldDefersDeduplicationToCommit(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(fixtureScan("T"))
	rc := NewExpressionRuleCall(ref, matching.NewBindings(), EmptyPlanContext())
	dup := fixtureScan("T") // same canonical form as the existing member
	rc.Yield(dup)
	if got := ref.Members(); len(got) != 1 {
		t.Fatalf("Reference changed before commit — has %d members, want 1", len(got))
	}
	// Yielded records intent; the driver decides whether commit-time
	// deduplication absorbs it after the rule has completed successfully.
	if got := rc.Yielded(); len(got) != 1 {
		t.Fatalf("Yielded() size=%d, want 1 (records rule intent)", len(got))
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
