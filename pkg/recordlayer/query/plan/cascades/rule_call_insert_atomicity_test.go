package cascades

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// insertThenMaybeFailRule reproduces DecorrelateValuesRule's shape: memoize a
// child, add a further member to a reference it did NOT just create, then
// either succeed or fail. `live` is the reference the extra member goes into —
// the test supplies one that is already reachable, because the leak only
// matters when the group is not an orphan.
type insertThenMaybeFailRule struct {
	matcher matching.BindingMatcher
	live    *expressions.Reference
	extra   expressions.RelationalExpression
	yield   expressions.RelationalExpression
	fail    bool

	// stagedAtBodyEnd records what the call was holding when the body returned,
	// so the test can prove the insert was STAGED rather than infer it from the
	// memo being unchanged — which is also what a rule that never inserted at
	// all would look like.
	stagedAtBodyEnd int
}

func (r *insertThenMaybeFailRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *insertThenMaybeFailRule) OnMatch(call *ExpressionRuleCall) {
	call.MemoizeExpression(fixtureScan("insert-atomicity-memoized-child"))
	call.InsertReExploring(r.live, r.extra)
	r.stagedAtBodyEnd = call.StagedInsertCount()
	if r.fail {
		call.Fail(errRuleProbe)
		return
	}
	call.Yield(r.yield)
}

// TestRuleFailureLeavesNoMemberInALiveGroup pins the half of rule-call
// atomicity that Yield staging did not cover.
//
// Yield was staged; InsertReExploring was not. That asymmetry looks harmless
// until you read what DecorrelateValuesRule does with it: MemoizeExpression
// there "can resolve to an EXISTING (already explored) reference" — its own
// comment — so the reference the extra members go into may be reachable from
// the root. A rule that adds a member there and then Fails has published a
// member it went on to reject, which is precisely the effect the staged
// protocol exists to prevent, escaping through the one call that did not use it.
//
// WHY THIS IS A REGRESSION TEST AND NOT A CRASH REPORT: the failure is not
// reachable as a wrong plan today. Fail sets capErr, the run loop returns
// `nil, tasksRun, capErr`, and every production PlanWithContext call site builds
// a fresh planner immediately before calling it. The leak is therefore in a
// memo nobody reads. What makes it worth closing anyway is that the
// unreachability rests on planner.Run's `if p.memo == nil` — a planner reused
// for a second run carries the first run's memo, and nothing announces that.
// So this test pins the EFFECT rather than the consequence: after a failed
// rule, the live group has exactly the members it started with.
//
// The success arm is not decoration. Staging that never commits would satisfy
// the failure arm perfectly while silently dropping every decorrelated
// alternative DecorrelateValuesRule produces, which is a worse bug than the one
// being fixed and would not show up as a crash — only as plans quietly missing
// a member.
func TestRuleFailureLeavesNoMemberInALiveGroup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		fail        bool
		wantMembers int
	}{
		{name: "rule fails: the staged insert is discarded", fail: true, wantMembers: 1},
		{name: "rule succeeds: the staged insert is published", fail: false, wantMembers: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := expressions.InitialOf(fixtureScan("insert-atomicity-root"))
			live := expressions.InitialOf(fixtureScan("insert-atomicity-live-group"))
			startMembers := len(live.AllMembers())
			if startMembers != 1 {
				t.Fatalf("fixture live group has %d members, want 1 — the counts below are stated "+
					"against a one-member group", startMembers)
			}

			rule := &insertThenMaybeFailRule{
				matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("insert-atomicity"),
				live:    live,
				extra:   fixtureScan("insert-atomicity-extra-member"),
				yield:   fixtureScan("insert-atomicity-yield"),
				fail:    tc.fail,
			}
			planner := NewPlanner([]ExpressionRule{rule}, nil)
			_, tasks, err := planner.PlanWithContext(context.Background(), root)

			if tasks == 0 {
				t.Fatal("PlanWithContext ran no tasks; the fixture rule never fired and every " +
					"assertion below would be vacuous")
			}
			if rule.stagedAtBodyEnd != 1 {
				t.Errorf("the rule body ended holding %d staged inserts, want 1 — InsertReExploring "+
					"applied its effect immediately instead of staging it, and the member-count "+
					"assertion below would then be measuring the wrong mechanism", rule.stagedAtBodyEnd)
			}

			if tc.fail {
				if !errors.Is(err, errRuleProbe) {
					t.Fatalf("PlanWithContext error = %v, want %v", err, errRuleProbe)
				}
			}

			if got := len(live.AllMembers()); got != tc.wantMembers {
				t.Fatalf("live group has %d members, want %d — a failed rule must leave the group "+
					"exactly as it found it, and a successful one must actually publish what it "+
					"staged (dropping it would lose every decorrelated alternative, silently)",
					got, tc.wantMembers)
			}
		})
	}
}
