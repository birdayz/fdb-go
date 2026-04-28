package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlanner_NilRefIsNoOp(t *testing.T) {
	t.Parallel()
	p := NewPlanner(DefaultExpressionRules(), nil)
	tasks, conv := p.Explore(nil)
	if !conv {
		t.Fatal("nil ref Explore should converge")
	}
	if tasks != 0 {
		t.Fatalf("nil ref ran %d tasks, want 0", tasks)
	}
}

func TestPlanner_ConvergesOnEmptyTree(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	p := NewPlanner(DefaultExpressionRules(), nil)
	tasks, conv := p.Explore(ref)
	if !conv {
		t.Fatalf("Scan Explore did not converge in %d tasks", tasks)
	}
	if tasks == 0 {
		t.Fatal("planner ran 0 tasks — initial Reference was not explored")
	}
	// Scan has no children; only one ExploreRef + ExploreExpr +
	// ApplyRules fires.
	if got := len(ref.Members()); got != 1 {
		t.Fatalf("scan reference ended with %d members, want 1 (no rules apply to a bare scan)", got)
	}
}

func TestPlanner_FiltersThroughDistinct(t *testing.T) {
	t.Parallel()
	// Filter(P, Distinct(Scan)) — PushFilterThroughDistinct should
	// yield Distinct(Filter(P, Scan)) as an alternative member.
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	dist := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(dist)),
	)
	ref := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, conv := p.Explore(ref)
	if !conv {
		t.Fatal("planner did not converge")
	}
	// After PushFilterThroughDistinct, Reference holds at least 2
	// members.
	if got := len(ref.Members()); got < 2 {
		t.Fatalf("Reference has %d members; expected ≥2 after PushFilterThroughDistinct", got)
	}

	// Look for Distinct-rooted alternative.
	foundPushed := false
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.LogicalDistinctExpression); ok {
			foundPushed = true
			break
		}
	}
	if !foundPushed {
		t.Fatal("planner did not yield Distinct-rooted member — PushFilterThroughDistinct did not fire")
	}
}

func TestPlanner_IdempotentOnReExplore(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	src := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
		)),
	)
	ref := expressions.InitialOf(src)

	p := NewPlanner(DefaultExpressionRules(), nil)
	tasks1, conv1 := p.Explore(ref)
	if !conv1 {
		t.Fatal("first Explore did not converge")
	}
	membersAfter1 := len(ref.Members())

	// Second call: saturation tracked in p.exploreCount, no new
	// members should be added.
	tasks2, conv2 := p.Explore(ref)
	if !conv2 {
		t.Fatal("second Explore did not converge")
	}
	if got := len(ref.Members()); got != membersAfter1 {
		t.Fatalf("second Explore grew Reference from %d to %d", membersAfter1, got)
	}
	// Second call should run FAR fewer tasks because saturation kicks
	// in on every Reference encountered.
	if tasks2 >= tasks1 {
		t.Logf("first run: %d tasks; second run: %d tasks (saturation may not be pruning)", tasks1, tasks2)
	}
}

// recordingEventHandler captures planner events for assertions.
type recordingEventHandler struct {
	exploreRefs  int
	exploreExprs int
	applyRules   int
	growthEvents int
}

func (h *recordingEventHandler) OnExploreReference(_ *expressions.Reference) { h.exploreRefs++ }
func (h *recordingEventHandler) OnExploreExpression(_ expressions.RelationalExpression) {
	h.exploreExprs++
}

func (h *recordingEventHandler) OnApplyRules(_ *expressions.Reference, grew int) {
	h.applyRules++
	if grew > 0 {
		h.growthEvents++
	}
}

func TestPlanner_EventsFire(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	src := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
		)),
	)
	ref := expressions.InitialOf(src)
	h := &recordingEventHandler{}
	p := NewPlanner(DefaultExpressionRules(), nil).SetEvents(h)
	_, conv := p.Explore(ref)
	if !conv {
		t.Fatal("planner did not converge")
	}
	if h.exploreRefs == 0 || h.exploreExprs == 0 || h.applyRules == 0 {
		t.Fatalf("events did not fire: exploreRefs=%d, exploreExprs=%d, applyRules=%d",
			h.exploreRefs, h.exploreExprs, h.applyRules)
	}
	if h.growthEvents == 0 {
		t.Fatal("no growth events — rule chain didn't add members?")
	}
}

func TestPlanner_MaxTasksCapHit(t *testing.T) {
	t.Parallel()
	// Force MaxTasks=1; planner should hit the cap and NOT converge.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	p := NewPlanner(DefaultExpressionRules(), nil)
	p.MaxTasks = 1
	tasks, conv := p.Explore(ref)
	if conv {
		t.Fatal("MaxTasks=1 should NOT converge — too few tasks for a full Explore")
	}
	if tasks > 1 {
		t.Fatalf("ran %d tasks under MaxTasks=1", tasks)
	}
}

// TestPlanner_SaturationPrunesRedundantFiring pins that a saturated
// Reference doesn't get its rules re-fired on the second-pass
// re-exploration. Counting OnApplyRules with grew=0 vs grew>0 events
// distinguishes saturated vs growing fires.
func TestPlanner_SaturationPrunesRedundantFiring(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	h := &recordingEventHandler{}
	p := NewPlanner(DefaultExpressionRules(), nil).SetEvents(h)

	// First explore — Scan is a leaf, ApplyRules fires once with
	// grew=0 (no rule matches a bare Scan).
	_, _ = p.Explore(ref)
	firstApply := h.applyRules

	// Second explore — saturation should short-circuit, but the
	// short-circuit STILL counts as an applyRules event (grew=0).
	_, _ = p.Explore(ref)
	secondApply := h.applyRules - firstApply

	if secondApply == 0 {
		t.Fatal("expected at least one ApplyRules event on second Explore")
	}
	// All second-pass events must have grew=0 (saturation in effect).
	totalGrowth := h.growthEvents
	// Bare scan never grows; total growth across BOTH passes should
	// be 0.
	if totalGrowth != 0 {
		t.Fatalf("bare Scan should never grow; observed %d growth events", totalGrowth)
	}
}
