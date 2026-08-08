package main

import (
	"strings"
	"testing"
)

// THE DEFECT, in one test.
//
// PR #637 (bot/frl-pin-bump) merged into master with zero concluded checks. Its
// workflow runs existed and were held at `action_required`; held runs never
// surface as check runs, so `gh pr checks` printed "no checks reported" and the
// merge state read UNSTABLE rather than blocked. Every signal a human or a bot
// would consult said "nothing failed", and the thing they inferred — "so it is
// safe" — does not follow from it.
//
// This is the negative control for the whole tool: an input that MUST fail the
// gate. If Classify ever reports this shape as satisfied, the gate has become
// the bug it was written to catch, and no amount of green elsewhere means
// anything.
func TestZeroChecksIsAViolationNotAPass(t *testing.T) {
	t.Parallel()

	expected := []string{"Build, Lint & Test", "Race detector", "Vulnerability scan"}

	verdicts := Classify(expected, nil) // exactly what #637 reported
	bad := Unsatisfied(verdicts)

	if len(bad) != len(expected) {
		t.Fatalf("a pull request reporting ZERO checks must leave all %d expected checks unsatisfied, got %d.\n"+
			"This is the #637 shape: 'no checks reported' must never classify as 'nothing failed'.",
			len(expected), len(bad))
	}
	for _, v := range verdicts {
		if v.State != StateAbsent {
			t.Errorf("expected check %q with no observation must be ABSENT, got %s", v.Name, v.State)
		}
		if !strings.Contains(v.Why, "action_required") {
			t.Errorf("the ABSENT reason for %q must name the approval-hold mechanism so the reader is not sent "+
				"down the GITHUB_TOKEN anti-recursion misdiagnosis; got %q", v.Name, v.Why)
		}
	}
}

// The three states GitHub collapses into one must stay distinct, and only one
// of them may permit a merge.
func TestClassifyThreeStates(t *testing.T) {
	t.Parallel()

	expected := []string{"passed", "failed", "held", "never-created", "running", "skipped"}
	observed := []ObservedCheck{
		{Name: "passed", Status: "completed", Conclusion: "success"},
		{Name: "failed", Status: "completed", Conclusion: "failure"},
		{Name: "held", Status: "completed", Conclusion: "action_required"},
		{Name: "running", Status: "in_progress"},
		{Name: "skipped", Status: "completed", Conclusion: "skipped"},
		// "never-created" is deliberately absent from the observations.
	}

	want := map[string]CheckState{
		"passed":        StatePassed,
		"failed":        StateFailed,
		"held":          StateAbsent, // held ran NOTHING — not a pass, not a failure
		"never-created": StateAbsent,
		"running":       StatePending,
		"skipped":       StateAbsent, // GitHub calls this neutral; a required job that did not run verified nothing
	}

	for _, v := range Classify(expected, observed) {
		if got := v.State; got != want[v.Name] {
			t.Errorf("check %q: got %s, want %s (%s)", v.Name, got, want[v.Name], v.Why)
		}
	}

	// Only PASSED may satisfy the gate. In particular ABSENT must not, which is
	// the inversion under test.
	bad := Unsatisfied(Classify(expected, observed))
	if len(bad) != 5 {
		t.Fatalf("exactly one of the six states (PASSED) may satisfy the gate; got %d unsatisfied, want 5", len(bad))
	}
}

// The zero value of CheckState must be the UNSAFE one. A future field added to
// ObservedCheck, or an unrecognised GitHub conclusion, then degrades to
// "blocked pending explanation" rather than to "fine".
func TestUnclassifiableDegradesToAbsentNotPassed(t *testing.T) {
	t.Parallel()

	var zero CheckState
	if zero != StateAbsent {
		t.Fatalf("the zero CheckState must be StateAbsent so unclassified input fails closed; got %s", zero)
	}
	v := Classify([]string{"x"}, []ObservedCheck{{Name: "x", Status: "completed", Conclusion: "some_future_conclusion"}})
	if v[0].State == StatePassed {
		t.Errorf("an unrecognised conclusion must never classify as PASSED; got %s", v[0].State)
	}
}

// When one check name reports more than once, the LATEST attempt decides — by
// check-run id, never by input order and never by picking the worst.
//
// The stale-cancelled case is the one that forced this rule and it is measured,
// not hypothetical: ci.yml sets `cancel-in-progress: true`, so every push to an
// open pull request strands a `cancelled` run beside the successful one that
// superseded it. A worst-wins gate reported most merged pull requests in this
// repository as violations, and a gate that fires on the normal flow is one
// people learn to ignore — which costs more than the hole it closes.
func TestLatestAttemptWinsPerName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		observed []ObservedCheck
		want     CheckState
	}{
		{"a stale cancelled run does not mask the newer success", []ObservedCheck{
			{ID: 1, Name: "ci", Status: "completed", Conclusion: "cancelled"},
			{ID: 2, Name: "ci", Status: "completed", Conclusion: "success"},
		}, StatePassed},
		{"a newer failure is not masked by an older success", []ObservedCheck{
			{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"},
			{ID: 2, Name: "ci", Status: "completed", Conclusion: "failure"},
		}, StateFailed},
		{"input order does not decide it — id does", []ObservedCheck{
			{ID: 2, Name: "ci", Status: "completed", Conclusion: "failure"},
			{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"},
		}, StateFailed},
		{"a newer hold is not masked by an older success", []ObservedCheck{
			{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"},
			{ID: 2, Name: "ci", Status: "completed", Conclusion: "action_required"},
		}, StateAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Classify([]string{"ci"}, tc.observed)[0].State
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// ExpectedChecks must recognise all three shapes GitHub accepts for `on:`, and
// must ignore workflows that never run on a pull request.
func TestExpectedChecksDerivation(t *testing.T) {
	t.Parallel()

	got, err := ExpectedChecks(map[string]string{
		"mapping.yml":  "on:\n  pull_request:\n    branches: [master]\njobs:\n  a:\n    name: Mapping Job\n",
		"sequence.yml": "on: [push, pull_request]\njobs:\n  b:\n    name: Sequence Job\n",
		"scalar.yml":   "on: pull_request\njobs:\n  c:\n    name: Scalar Job\n",
		"unnamed.yml":  "on: pull_request\njobs:\n  bare-key-job: {}\n",
		// Must be IGNORED — never runs on a pull request.
		"nightly.yml": "on:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  d:\n    name: Nightly Job\n",
		// Must be IGNORED — a job-level `if` makes absence legitimate.
		"conditional.yml": "on: pull_request\njobs:\n  e:\n    name: Conditional Job\n    if: github.actor != 'nobody'\n",
	})
	if err != nil {
		t.Fatalf("ExpectedChecks: %v", err)
	}

	want := []string{"Mapping Job", "Scalar Job", "Sequence Job", "bare-key-job"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("derived %v, want %v", got, want)
	}
}

// VACUITY GUARD. An expected set that collapses to empty makes every pull
// request trivially compliant — the gate reports green having verified nothing,
// the same fail-open shape it exists to catch. That must be an error, not a
// quiet pass.
//
// This is the second negative control, on the derivation rather than the
// classification: a workflow corpus that is syntactically fine and yields
// nothing to require.
func TestEmptyExpectedSetIsAnError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		workflows map[string]string
	}{
		{"no workflow triggers on pull_request", map[string]string{
			"nightly.yml": "on:\n  schedule:\n    - cron: '0 0 * * *'\njobs:\n  a:\n    name: A\n",
		}},
		{"pull_request workflow has no jobs", map[string]string{
			"empty.yml": "on: pull_request\njobs: {}\n",
		}},
		{"every pull_request job is conditional", map[string]string{
			"cond.yml": "on: pull_request\njobs:\n  a:\n    name: A\n    if: false\n",
		}},
		{"no workflows at all", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExpectedChecks(tc.workflows)
			if err == nil {
				t.Fatalf("an empty expected set must be an ERROR, not a silent pass; got %v", got)
			}
			if !strings.Contains(err.Error(), "no-op") {
				t.Errorf("the vacuity error must say the gate has become a no-op, so the reader knows the "+
					"instrument died rather than that the repository is clean; got %q", err)
			}
		})
	}
}

// The gate reads the workflow corpus, so a malformed file must stop it rather
// than silently shrink the expected set.
func TestMalformedWorkflowIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := ExpectedChecks(map[string]string{
		"broken.yml": "on: pull_request\njobs:\n  a:\n   name: [unclosed\n",
	}); err == nil {
		t.Fatal("a workflow that does not parse must be an error; silently dropping it shrinks the expected set and weakens the gate")
	}
}

// A truncated listing must be an error, not a quietly narrower sweep.
//
// This is the third negative control, and it is here because the gate shipped
// with the bug: a 100-item limit against 149 pull requests merged inside the
// default window, auditing two thirds and printing a clean summary for all of
// them. A gate that silently narrows its own scope is the same fail-open shape
// as a check that never ran.
func TestSaturatedListingIsAnError(t *testing.T) {
	t.Parallel()

	if err := checkListSaturation("merged", 99, 100); err != nil {
		t.Errorf("a listing under the limit is complete and must pass: %v", err)
	}
	for _, got := range []int{100, 101} {
		err := checkListSaturation("merged", got, 100)
		if err == nil {
			t.Fatalf("a listing of %d against a limit of 100 is truncated and must be an ERROR, not a "+
				"silently narrowed sweep", got)
		}
		if !strings.Contains(err.Error(), "TRUNCATED") {
			t.Errorf("the message must say the listing was truncated, so the reader knows the sweep's SCOPE "+
				"shrank rather than that the repository is clean; got %q", err)
		}
	}
}
