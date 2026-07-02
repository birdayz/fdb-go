package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// exploreRewriting drives the production unified task stack through the
// REWRITING phase only — the same ExploreGroupTask / ExploreExprTask /
// TransformExprTask / OptimizeGroupTask machinery Plan() uses, without
// chaining into PLANNING (which advances the planner stage and replaces
// each Reference's exploratory members with the pruned final winner).
// Rule tests use it to assert on the full set of explored alternatives.
//
// Returns (tasksRun, converged); converged=false means MaxTasks was hit
// (a non-termination signal, same contract as Plan's ErrPlannerCapHit).
func exploreRewriting(p *Planner, rootRef *expressions.Reference) (int, bool) {
	if rootRef == nil {
		return 0, true
	}
	if p.memo == nil {
		p.memo = NewMemo(rootRef)
	}
	if p.constraintMap == nil {
		p.constraintMap = NewConstraintMap()
	}
	if p.dataAccessConsumed == nil {
		p.dataAccessConsumed = make(map[*expressions.Reference]int)
	}
	// Mirror InitiatePlannerPhaseTask{PhaseRewriting} minus the chain to
	// PLANNING: OptimizeGroup deepest (fires last), ExploreGroup on top.
	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return p.tasksRun, false
		}
		p.pop().Run(p)
		p.tasksRun++
	}
	return p.tasksRun, true
}

func TestPlanner_NilRefIsNoOp(t *testing.T) {
	t.Parallel()
	p := NewPlanner(DefaultExpressionRules(), nil)
	plan, tasks, err := p.Plan(nil)
	if err != nil {
		t.Fatalf("Plan(nil) err=%v, want nil", err)
	}
	if plan != nil {
		t.Fatalf("Plan(nil) returned %T, want nil", plan)
	}
	if tasks != 0 {
		t.Fatalf("Plan(nil) ran %d tasks, want 0", tasks)
	}
}

func TestPlanner_ConvergesOnEmptyTree(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	p := NewPlanner(DefaultExpressionRules(), nil)
	tasks, conv := exploreRewriting(p, ref)
	if !conv {
		t.Fatalf("Scan exploration did not converge in %d tasks", tasks)
	}
	if tasks == 0 {
		t.Fatal("planner ran 0 tasks — initial Reference was not explored")
	}
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
	if _, conv := exploreRewriting(p, ref); !conv {
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

// TestPlanner_IdempotentOnReExplore pins that re-driving the unified
// exploration on an already-converged Reference makes no progress:
// Reference.CommitExploration marks the group done, so a second pass
// adds no members.
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
	if _, conv := exploreRewriting(p, ref); !conv {
		t.Fatal("first exploration did not converge")
	}
	membersAfter1 := len(ref.Members())

	if _, conv := exploreRewriting(p, ref); !conv {
		t.Fatal("second exploration did not converge")
	}
	if got := len(ref.Members()); got != membersAfter1 {
		t.Fatalf("second exploration grew Reference from %d to %d", membersAfter1, got)
	}
}

// TestPlanner_Plan_FullPipeline pins the production Plan entry point:
// REWRITING + PLANNING in one call. Returns the extracted best plan.
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
// the task stack hits MaxTasks.
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

// TestPlanner_BestMember_StampedAfterPlan pins that Plan's OPTIMIZE
// phase stamps a NoProperties winner on the root Reference,
// accessible via BestMember(ref). A winner is stamped only when the
// group holds a physical final member (Java's OptimizeGroup picks
// from final expressions), so the production planning/implementation
// rules are registered. (Child References are optimized only as
// inputs of physical parents — Java CascadesPlanner's OptimizeInputs
// gating — so no per-child winner is asserted here.)
func TestPlanner_BestMember_StampedAfterPlan(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !p.HasBestMember(topRef) {
		t.Fatal("top Reference has no BestMember stamp after Plan")
	}
	if p.BestMember(topRef) == nil {
		t.Fatal("top BestMember not stamped")
	}
}

// TestPlanner_MemoPopulatedAfterExploration pins that the Memo is
// built lazily on the first drive and indexes every reachable
// Reference.
func TestPlanner_MemoPopulatedAfterExploration(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	if _, conv := exploreRewriting(p, rootRef); !conv {
		t.Fatal("planner did not converge")
	}
	memo := p.Memo()
	if memo == nil {
		t.Fatal("Memo is nil after exploration")
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

// TestPlanner_GenerateDataAccess_InsertsScans verifies that the data
// access phase runs between AdjustMatches and PLANNING and converts
// PartialMatches into scanPlanExpressions inserted into the Reference.
func TestPlanner_GenerateDataAccess_InsertsScans(t *testing.T) {
	t.Parallel()

	// Build a simple scan expression with a Reference.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	// Create a test PartialMatch with a known plan.
	plan := &testPlan{name: "data_access_scan"}
	pm := makeDataAccessTestPartialMatch("idx_test", 2, plan)

	// Register the PartialMatch on the Reference for its candidate.
	AddPartialMatchForCandidate(ref, pm.GetMatchCandidate(), pm)

	// Before Plan, the Reference should have only the original scan member.
	if got := len(ref.Members()); got != 1 {
		t.Fatalf("before Plan: expected 1 member, got %d", got)
	}

	// Run the full pipeline. The data access phase should insert a
	// scanPlanExpression into the Reference.
	p := NewPlanner(DefaultExpressionRules(), nil)
	_, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// After Plan, AllMembers should have at least 2 (original + data access scan).
	members := ref.AllMembers()
	if len(members) < 2 {
		t.Fatalf("after Plan: expected >= 2 members (original + data access scan), got %d", len(members))
	}

	// Find the scanPlanExpression among all members.
	var found *scanPlanExpression
	for _, m := range members {
		if spe, ok := m.(*scanPlanExpression); ok {
			found = spe
			break
		}
	}
	if found == nil {
		t.Fatal("data access phase did not insert a scanPlanExpression into the Reference")
	}
	if found.plan != plan {
		t.Fatalf("scanPlanExpression wraps wrong plan: got %v, want %v", found.plan, plan)
	}
}

// TestPlanner_GenerateDataAccess_NoMatchesIsNoOp verifies that the
// data access phase is a no-op when no PartialMatches exist.
func TestPlanner_GenerateDataAccess_NoMatchesIsNoOp(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// No PartialMatches registered — data access phase should be a
	// no-op; only the original scan member should be present.
	if got := len(ref.Members()); got != 1 {
		t.Fatalf("expected 1 member (no data access generation), got %d", got)
	}
}

// TestPlanner_GenerateDataAccess_BottomUp verifies that the data
// access phase processes child References before parent References
// (bottom-up order).
func TestPlanner_GenerateDataAccess_BottomUp(t *testing.T) {
	t.Parallel()

	// Build Filter(Scan) — two References.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "x", Typ: values.TypeBool})
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	// Register a PartialMatch on the inner (scan) Reference only.
	childPlan := &testPlan{name: "child_scan"}
	pm := makeDataAccessTestPartialMatch("child_idx", 1, childPlan)
	AddPartialMatchForCandidate(scanRef, pm.GetMatchCandidate(), pm)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// The inner Reference should have a scanPlanExpression (data access
	// phase processed it bottom-up, before the parent).
	var found bool
	for _, m := range scanRef.AllMembers() {
		if _, ok := m.(*scanPlanExpression); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("data access phase did not insert scanPlanExpression into inner (scan) Reference")
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
	if _, conv := exploreRewriting(p, rootRef); !conv {
		t.Fatal("planner did not converge")
	}

	memo := p.Memo()
	if memo == nil {
		t.Fatal("Memo is nil after exploration")
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
