package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Pins the out-of-band member-growth bridge: under the epoch convergence, member growth alone no longer
// drives exploration, so an insertion into a reference whose exploration
// ALREADY BEGAN — a rule's raw insert into an existing child group, or a
// cross-group Absorb folding members explored under the loser's identity
// — must re-arm the epoch AND schedule a task, or the new member is
// silently never explored and AdvancePlannerStage discards it.

func convergedLeafRef(t testing.TB) *expressions.Reference {
	t.Helper()
	ref := expressions.InitialOf(mustFullUnorderedScan(t, []string{"leaf"}, values.NotNullLong))
	ref.StartExploration()
	ref.CommitExploration()
	if ref.NeedsExploration() {
		panic("fixture: ref must be converged")
	}
	return ref
}

// TestMemoInsertReExploring_SchedulesConvergedRef: the InsertReExploring
// path (DecorrelateValuesRule's extras) re-arms and schedules.
func TestMemoInsertReExploring_SchedulesConvergedRef(t *testing.T) {
	t.Parallel()
	ref := convergedLeafRef(t)
	m := NewMemo(ref)

	var scheduled []*expressions.Reference
	m.SetReExploreScheduler(func(r *expressions.Reference) { scheduled = append(scheduled, r) })

	extraInner := expressions.InitialOf(mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong))
	extra, extraErr := expressions.NewLogicalTypeFilterExpression(
		[]string{"T"}, expressions.ForEachQuantifier(extraInner))
	extra = mustConstruct(t, extra, extraErr)
	if !m.InsertReExploring(ref, extra) {
		t.Fatal("insert must add the new member")
	}
	if len(scheduled) != 1 || scheduled[0] != ref.Canonical() {
		t.Fatalf("a converged ref gaining a member must be scheduled, got %v", scheduled)
	}
	if !ref.NeedsExploration() {
		t.Fatal("the insert must re-arm the epoch (NeedsExploration)")
	}

	// A NEVER-explored ref needs nothing: its first round explores every
	// member when a parent's child-walk reaches it.
	fresh := expressions.InitialOf(mustFullUnorderedScan(t, []string{"fresh"}, values.NotNullLong))
	scheduled = nil
	freshInner := expressions.InitialOf(mustFullUnorderedScan(t, []string{"U"}, values.NotNullLong))
	freshExtra, freshExtraErr := expressions.NewLogicalTypeFilterExpression(
		[]string{"U"}, expressions.ForEachQuantifier(freshInner))
	m.InsertReExploring(fresh, mustConstruct(t, freshExtra, freshExtraErr))
	if len(scheduled) != 0 {
		t.Fatalf("a never-explored ref must not be scheduled, got %v", scheduled)
	}
}

// TestMemoMerge_SchedulesSurvivor: Absorb re-arms the survivor (folded
// members were explored under the LOSER's identity) — without a task the
// re-arm is inert: a converged survivor's trailing ExploreGroupTask has
// already run, and a mid-round one commits its stale goal and returns.
func TestMemoMerge_SchedulesSurvivor(t *testing.T) {
	t.Parallel()
	// Two equivalent single-quantifier selects over distinct child refs
	// whose expressions are structurally equal — integrateOne merges them.
	// Equivalence requires the SAME child reference: two structurally
	// equal selects over one child group land in two parent refs that
	// integrateOne merges.
	child := expressions.InitialOf(mustFullUnorderedScan(t, []string{"child"}, values.NotNullLong))
	alias := values.NamedCorrelationIdentifier("Q")
	qovA, qovAErr := values.NewQuantifiedObjectValue(alias, values.NotNullLong)
	qovA = mustConstruct(t, qovA, qovAErr)
	qovB, qovBErr := values.NewQuantifiedObjectValue(alias, values.NotNullLong)
	qovB = mustConstruct(t, qovB, qovBErr)
	selA, selAErr := expressions.NewSelectExpression(qovA,
		[]expressions.Quantifier{expressions.NamedForEachQuantifier(alias, child)}, nil)
	selA = mustConstruct(t, selA, selAErr)
	selB, selBErr := expressions.NewSelectExpression(qovB,
		[]expressions.Quantifier{expressions.NamedForEachQuantifier(alias, child)}, nil)
	selB = mustConstruct(t, selB, selBErr)

	refA := expressions.InitialOf(selA)
	refA.StartExploration()
	refA.CommitExploration()
	refB := expressions.InitialOf(selB)

	m := NewMemo(refA)
	var scheduled []*expressions.Reference
	m.SetReExploreScheduler(func(r *expressions.Reference) { scheduled = append(scheduled, r) })
	m.Integrate(refA, selA)
	scheduled = nil

	// Integrating refB's equivalent expression merges the two groups.
	m.Integrate(refB, selB)
	if refB.Canonical() == refB && refA.Canonical() == refA {
		// No-skips: a fixture that stops exercising the merge is a broken
		// pin, not an optional one.
		t.Fatal("fixture did not merge — integrateOne's equivalence lookup changed; rebuild the fixture")
	}
	if len(scheduled) == 0 {
		t.Fatal("the merge survivor must be scheduled for the re-round its Absorb re-arm requires")
	}
	survivor := refA.Canonical()
	if scheduled[len(scheduled)-1] != survivor {
		t.Fatalf("scheduled %v, want the survivor %v", scheduled, survivor)
	}
}
