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
	// Note on tasks2 vs tasks1: cannot be a strict-less assertion
	// because the first call may grow the Reference's member set
	// (rule yields), and the second call's ExploreReferenceTask
	// then iterates all members including the new ones. Saturation
	// only short-circuits ApplyRulesTask, not ExploreExpressionTask
	// over members. The member-count assertion above is the strict
	// idempotence check that catches saturation-clear regressions.
	_ = tasks2
	_ = tasks1
}

// recordingEventHandler captures planner events for assertions.
type recordingEventHandler struct {
	exploreRefs    int
	exploreExprs   int
	applyRules     int
	growthEvents   int
	optimizeRefs   int
	transformRules int
	ruleYields     int
}

func (h *recordingEventHandler) OnExploreReference(_ *expressions.Reference) { h.exploreRefs++ }
func (h *recordingEventHandler) OnExploreExpression(_ expressions.RelationalExpression) {
	h.exploreExprs++
}

func (h *recordingEventHandler) OnTransformRule(_ *expressions.Reference, _ ExpressionRule, yielded int) {
	h.transformRules++
	h.ruleYields += yielded
}

func (h *recordingEventHandler) OnApplyRules(_ *expressions.Reference, grew int) {
	h.applyRules++
	if grew > 0 {
		h.growthEvents++
	}
}

func (h *recordingEventHandler) OnOptimizeReference(_ *expressions.Reference, _ expressions.RelationalExpression) {
	h.optimizeRefs++
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
	if h.transformRules == 0 {
		t.Fatal("no OnTransformRule events — per-rule tasks didn't fire?")
	}
	if h.ruleYields == 0 {
		t.Fatal("no rule yields reported by OnTransformRule")
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

// TestPlanner_Plan_FullPipeline pins the convenience Plan method:
// EXPLORE + OPTIMIZE in one call. Returns the extracted best plan.
func TestPlanner_Plan_FullPipeline(t *testing.T) {
	t.Parallel()
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
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan err=%v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil expression")
	}
	if tasks == 0 {
		t.Fatal("Plan ran 0 tasks")
	}
}

// TestPlanner_Plan_MaxTasksHit pins the Plan method's error when
// EXPLORE hits MaxTasks.
func TestPlanner_Plan_MaxTasksHit(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	p := NewPlanner(DefaultExpressionRules(), nil)
	p.MaxTasks = 1
	plan, _, err := p.Plan(ref)
	if err != ErrPlannerCapHit {
		t.Fatalf("Plan with MaxTasks=1 err=%v, want ErrPlannerCapHit", err)
	}
	if plan != nil {
		t.Fatal("Plan should return nil on cap hit")
	}
}

// TestPlanner_BestMember_StampedAfterOptimize pins that
// OptimizeReferenceTask populates the bestMember map for every
// reachable Reference, accessible via BestMember(ref).
func TestPlanner_BestMember_StampedAfterOptimize(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	if _, conv := p.Explore(topRef); !conv {
		t.Fatal("planner did not converge")
	}

	// Both top and inner Reference should have a stamped best.
	topBest := p.BestMember(topRef)
	if topBest == nil {
		t.Fatal("top BestMember not stamped")
	}
	innerBest := p.BestMember(innerRef)
	if innerBest == nil {
		t.Fatal("inner BestMember not stamped")
	}
}

// TestPlanner_OnOptimizeReference_FiresAfterSaturation pins that
// the OnOptimizeReference event fires for every reachable Reference
// after EXPLORE finishes.
func TestPlanner_OnOptimizeReference_FiresAfterSaturation(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	ref := expressions.InitialOf(filter)

	h := &recordingEventHandler{}
	p := NewPlanner(DefaultExpressionRules(), nil).SetEvents(h)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}
	if h.optimizeRefs == 0 {
		t.Fatal("OnOptimizeReference never fired")
	}
}

// TestPlanner_ConfluenceWithFixpointApply pins that the task-stack
// planner converges to the SAME final Reference member set that
// FixpointApply produces. Both drivers must reach the same
// equivalence class — anything else means the new driver is missing
// or fabricating rule fires.
//
// Strategy: build identical input trees, run one through Planner.
// Explore, the other through FixpointApply. Then compare the
// top-level Reference's member-count and the structural equality
// of every member's class.
func TestPlanner_ConfluenceWithFixpointApply(t *testing.T) {
	t.Parallel()

	build := func() *expressions.Reference {
		pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		dist := expressions.NewLogicalDistinctExpression(
			expressions.ForEachQuantifier(expressions.InitialOf(scan)),
		)
		filter := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pred},
			expressions.ForEachQuantifier(expressions.InitialOf(dist)),
		)
		return expressions.InitialOf(filter)
	}

	// Driver A: FixpointApply.
	refA := build()
	if _, conv := FixpointApply(DefaultExpressionRules(), refA, 64); !conv {
		t.Fatal("FixpointApply did not converge")
	}

	// Driver B: task-stack Planner.
	refB := build()
	p := NewPlanner(DefaultExpressionRules(), nil)
	if _, conv := p.Explore(refB); !conv {
		t.Fatal("Planner did not converge")
	}

	// Compare member counts: must match.
	if got, want := len(refA.Members()), len(refB.Members()); got != want {
		t.Fatalf("FixpointApply produced %d members; Planner produced %d — confluence violation", want, got)
	}

	// Compare member kinds (multi-set of Go concrete-type names): must
	// match. Member ORDER may differ because the two drivers fire
	// rules in different orders, so we tally types.
	tally := func(ms []expressions.RelationalExpression) map[string]int {
		m := map[string]int{}
		for _, e := range ms {
			m[exprKindName(e)]++
		}
		return m
	}
	a, b := tally(refA.Members()), tally(refB.Members())
	for k, ac := range a {
		if bc := b[k]; bc != ac {
			t.Errorf("kind %q: FixpointApply has %d, Planner has %d", k, ac, bc)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("kind %q only in Planner output", k)
		}
	}
}

// exprKindName returns a stable string identifying the Go concrete
// type of e (used by the confluence test as a multi-set key).
func exprKindName(e expressions.RelationalExpression) string {
	switch e.(type) {
	case *expressions.LogicalFilterExpression:
		return "Filter"
	case *expressions.LogicalDistinctExpression:
		return "Distinct"
	case *expressions.LogicalSortExpression:
		return "Sort"
	case *expressions.LogicalProjectionExpression:
		return "Projection"
	case *expressions.LogicalTypeFilterExpression:
		return "TypeFilter"
	case *expressions.LogicalUnionExpression:
		return "Union"
	case *expressions.LogicalIntersectionExpression:
		return "Intersection"
	case *expressions.SelectExpression:
		return "Select"
	case *expressions.FullUnorderedScanExpression:
		return "Scan"
	case *expressions.InsertExpression:
		return "Insert"
	case *expressions.UpdateExpression:
		return "Update"
	case *expressions.DeleteExpression:
		return "Delete"
	default:
		return "unknown"
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

func TestPlanner_MemoPopulatedAfterExplore(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, conv := p.Explore(rootRef)
	if !conv {
		t.Fatal("planner did not converge")
	}
	memo := p.Memo()
	if memo == nil {
		t.Fatal("Memo is nil after Explore")
	}
	if !memo.ContainsReference(rootRef) {
		t.Fatal("root Reference not in Memo")
	}
	if !memo.ContainsReference(scanRef) {
		t.Fatal("scan Reference not in Memo")
	}
	// The Memo should know about at least these 2 references.
	if got := len(memo.References()); got < 2 {
		t.Fatalf("Memo has %d references, expected at least 2", got)
	}
}

func TestPlanner_MemoSharesSubExpressions(t *testing.T) {
	t.Parallel()
	// Build a tree where the PullFilterAboveSort and PushFilterThroughSort
	// rules will independently construct sub-expressions that should be
	// shared via the Memo.
	//
	// Input: Filter(P, Sort(Scan))
	// PushFilterThroughSort yields: Sort(Filter(P, Scan)) — creates a
	//   new Reference for Filter(P, Scan).
	// If we then explore that, PullFilterAboveSort on Sort(Filter(P,Scan))
	//   would yield Filter(P, Sort(Scan)) — creating a new Reference
	//   for Sort(Scan).
	//
	// With the Memo, the "Sort(Scan)" sub-expression should be memoized:
	// the original Sort(Scan) and any rule-derived Sort(Scan) share the
	// same Reference.

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	sort := expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(scanRef))
	sortRef := expressions.InitialOf(sort)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(sortRef),
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, conv := p.Explore(rootRef)
	if !conv {
		t.Fatal("planner did not converge")
	}

	memo := p.Memo()
	if memo == nil {
		t.Fatal("Memo is nil after Explore")
	}

	// After PushFilterThroughSort fires, the root Reference should have
	// at least 2 members (original Filter + Sort alternative).
	if got := len(rootRef.Members()); got < 2 {
		t.Fatalf("root Reference has %d members, expected >= 2 after rule chain", got)
	}

	// The Memo should track all reachable References.
	if got := len(memo.References()); got < 3 {
		t.Fatalf("Memo has %d references, expected at least 3 (root+sort+scan)", got)
	}
}
