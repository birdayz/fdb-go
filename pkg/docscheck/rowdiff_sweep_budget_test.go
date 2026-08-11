package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The rowdiff sweep-budget ordering gate.
//
// nightly-rowdiff.yml runs its sweeps under TWO bounds, and which one fires
// first decides whether a wedged night produces a diagnosis or a corpse:
//
//   - ROWDIFF_BUDGET is read inside the test. When it fires the seed loop
//     breaks, the ROWDIFF_TALLY line prints, the vacuity floor gets to speak,
//     and the log says how many seeds were actually walked.
//   - --test_timeout is Bazel's. When it fires the Go alarm SIGQUITs the
//     process mid-loop, and every one of those instruments — all of which run
//     after the loop — is lost.
//
// MEASURED, runs 31234733111 / 31290367277 / 31350088492 (2026-08-08, -09, -10)
// and three nights before them: the un-paged sweep hit `--test_timeout=17400`
// on six consecutive nights, `TIMEOUT in 17405.4s` each time, and not one of
// those logs contains a ROWDIFF_TALLY line. The tally had been built precisely
// so a reader could tell "the cluster died" from "the engine returned wrong
// rows"; it could not run, because the only bound in play was the one that
// kills.
//
// So the invariant is an ORDERING, not a pair of magnitudes: every rowdiff
// sweep step must declare ROWDIFF_BUDGET, and that budget must sit far enough
// below the step's own --test_timeout that the reporting path always wins the
// race. A budget quietly raised past its timeout, or dropped altogether,
// restores the silent-corpse behaviour with no other visible symptom — which is
// exactly the class of regression a build failure is cheaper than.
//
// budgetTimeoutMargin is how much clear air the backstop must leave the budget.
// It is not cosmetic: the seed in flight when the budget expires still has to
// finish, and on a dead cluster that costs a full context deadline (~60s
// measured) before the loop can check the bound and report.
const budgetTimeoutMargin = 15 * time.Minute

// rowdiffWorkflow is the workflow this gate reads. Named as a constant so a
// rename of the file surfaces here as a missing-population failure rather than
// as a silently empty scan.
const rowdiffWorkflow = "nightly-rowdiff.yml"

var (
	reRowdiffSweep = regexp.MustCompile(`--test\.run=TestFDB_RowDiff_\w+`)
	reTestTimeout  = regexp.MustCompile(`--test_timeout=(\d+)`)
	reRowdiffBudge = regexp.MustCompile(`ROWDIFF_BUDGET=(\S+)`)
	reRowdiffFloor = regexp.MustCompile(`ROWDIFF_MIN_SEEDS=(\d+)`)
)

func TestRowdiffSweepBudgetBoundsTheTimeout(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	path := filepath.Join(root, ".github", "workflows", rowdiffWorkflow)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v — under Bazel this workflow must be a `data` dep of the docscheck target", path, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parsing %s: %v", rowdiffWorkflow, err)
	}

	checked := 0
	for jobName, job := range wf.Jobs {
		for _, step := range job.Steps {
			// A sweep step is one that runs a rowdiff test target. Identified by
			// the test filter it passes rather than by step name, so renaming a
			// step cannot drop it out of the gate's population.
			if !reRowdiffSweep.MatchString(step.Run) {
				continue
			}
			checked++
			label := jobName + "/" + step.Name

			tm := reTestTimeout.FindStringSubmatch(step.Run)
			if tm == nil {
				t.Errorf("%s: rowdiff sweep step declares no --test_timeout; the backstop must be explicit, "+
					"not inherited from a .bazelrc default nobody reads next to this step", label)
				continue
			}
			secs, err := strconv.Atoi(tm[1])
			if err != nil {
				t.Errorf("%s: --test_timeout=%q is not an integer", label, tm[1])
				continue
			}
			timeout := time.Duration(secs) * time.Second

			bm := reRowdiffBudge.FindStringSubmatch(step.Run)
			if bm == nil {
				t.Errorf("%s: rowdiff sweep step declares no ROWDIFF_BUDGET. Without it the only bound is "+
					"--test_timeout, whose SIGQUIT kills the process before ROWDIFF_TALLY, the vacuity "+
					"floor and the walked-seed count can print — measured on six consecutive nights ending "+
					"2026-08-10, every one of which timed out having reported no tally at all.", label)
				continue
			}
			budget, err := time.ParseDuration(bm[1])
			if err != nil || budget <= 0 {
				t.Errorf("%s: ROWDIFF_BUDGET=%q is not a positive Go duration", label, bm[1])
				continue
			}

			// The floor is what makes a NON-fatal budget safe. Budget exhaustion
			// is deliberately not an error — the clock is meant to size the night
			// — so the only thing separating "a shorter night" from "per-seed cost
			// regressed tenfold and this has been green on 200 seeds for a month"
			// is ROWDIFF_MIN_SEEDS. Drop it and the sweep still runs, still passes,
			// and stops meaning anything, with no other visible symptom. That is
			// the same silent-degradation shape as the missing budget above, one
			// level down, so it is gated the same way.
			if fm := reRowdiffFloor.FindStringSubmatch(step.Run); fm == nil {
				t.Errorf("%s: declares ROWDIFF_BUDGET but no ROWDIFF_MIN_SEEDS. A budget that ends the "+
					"sweep without a floor underneath it converts a throughput collapse into a quiet "+
					"green — the sweep reports a normal truncated night no matter how little it "+
					"measured.", label)
			} else if floor, ferr := strconv.ParseUint(fm[1], 10, 64); ferr != nil || floor == 0 {
				t.Errorf("%s: ROWDIFF_MIN_SEEDS=%q must be a positive integer; 0 disables the floor and "+
					"declaring it as 0 here is indistinguishable from having no guard at all", label, fm[1])
			}

			if budget+budgetTimeoutMargin > timeout {
				t.Errorf("%s: ROWDIFF_BUDGET=%s leaves only %s before --test_timeout=%s, less than the %s "+
					"margin. The budget must fire FIRST and by a clear margin — the seed in flight when it "+
					"expires still has to finish, and on a dead cluster that costs a full ~60s context "+
					"deadline before the loop can check the bound and report. Too little air here and the "+
					"Bazel timeout wins the race, which is the silent-corpse behaviour this gate exists to "+
					"prevent: raise the timeout or lower the budget.",
					label, budget, timeout-budget, timeout, budgetTimeoutMargin)
			}
		}
	}

	// The population guard. A regex that stops matching — a renamed test, a
	// reworked step — would otherwise leave this gate reporting green over an
	// empty scan, which is the failure mode it is built to catch elsewhere.
	// Both sweeps (un-paged and paging) must be present.
	if checked != 2 {
		t.Fatalf("scanned %s and found %d rowdiff sweep steps, want 2 (the un-paged sweep and the paging "+
			"sweep). A green from an empty or partial scan is not a green: fix the matcher, or update this "+
			"count deliberately if a sweep was genuinely added or removed.", rowdiffWorkflow, checked)
	}
}

// TestRowdiffSweepBudgetGateArms drives the gate's own comparison, which the
// corpus reading above only ever exercises in its passing direction.
//
// The ordering rule is the whole point of the gate, so the arm that must fail
// needs driving explicitly — otherwise the first time anyone gets the ordering
// wrong is also the first time this comparison has ever run.
func TestRowdiffSweepBudgetGateArms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		budget, timeout time.Duration
		wantViolation   bool
	}{
		{"the shipped ordering: 3h30m budget under a 4h backstop", 3*time.Hour + 30*time.Minute, 4 * time.Hour, false},
		{"the paging ordering: 70m budget under a 90m backstop", 70 * time.Minute, 90 * time.Minute, false},
		{"budget above the timeout — the timeout wins and the tally is lost", 5 * time.Hour, 4 * time.Hour, true},
		{"budget equal to the timeout leaves no air at all", 4 * time.Hour, 4 * time.Hour, true},
		{"budget inside the margin — technically first, but not reliably", 3*time.Hour + 50*time.Minute, 4 * time.Hour, true},
		{"exactly the margin is enough", 3*time.Hour + 45*time.Minute, 4 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.budget+budgetTimeoutMargin > tc.timeout
			if got != tc.wantViolation {
				t.Fatalf("budget=%s timeout=%s: violation=%v, want %v", tc.budget, tc.timeout, got, tc.wantViolation)
			}
		})
	}
}
