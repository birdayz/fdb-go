// Command verify-pr-checks answers one question about a pull request that
// GitHub's own merge state answers wrongly: did the checks this repository
// requires actually RUN and PASS?
//
// The distinction is not pedantic. A pull request whose workflow runs are held
// at `action_required` — awaiting maintainer approval, which this repository's
// `all_external_contributors` policy imposes on every PR opened by
// github-actions[bot] — reports `mergeStateStatus: UNSTABLE` and
// `gh pr checks` prints "no checks reported on the '<branch>' branch". Held
// runs are created, then never surface as check runs. Neither signal is an
// error, so a merge decision made from either reads "nothing failed" out of
// "nothing ran".
//
// That inversion has already reached master: PR #637 (bot/frl-pin-bump) merged
// with zero concluded checks, and on 2026-08-05 the CI run for that same branch
// concluded FAILURE once it was allowed to run. The bot's PRs are not
// low-risk-by-construction; they were just never asked.
//
// So the classification here is deliberately three-valued where GitHub is
// two-valued. PASSED and FAILED are the states everyone already models. ABSENT
// is the state that has been reading as safe, and it is the whole point of this
// tool: a check that is expected and simply is not there. A gate that folds
// ABSENT into "not failed" is the bug, not the fix.
package main

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// CheckState is the verdict for ONE expected check on one pull request.
type CheckState int

const (
	// StateAbsent: the expected check produced no concluded result. It was
	// never created, is held for approval, or resolved to `skipped` /
	// `action_required`. This is the state that has been silently reading as
	// green, so it is the zero value: anything this tool fails to classify
	// positively is unsafe by default, never safe by default.
	StateAbsent CheckState = iota
	// StatePending: the check exists and is running. Honest in-flight state —
	// not a pass, but not evidence of a hole either.
	StatePending
	// StateFailed: the check ran and concluded negatively.
	StateFailed
	// StatePassed: the check ran and concluded positively. The ONLY state that
	// permits a merge.
	StatePassed
)

func (s CheckState) String() string {
	switch s {
	case StatePassed:
		return "PASSED"
	case StateFailed:
		return "FAILED"
	case StatePending:
		return "PENDING"
	default:
		return "ABSENT"
	}
}

// ObservedCheck is one check run or commit status as GitHub reports it for a
// pull request's head commit.
type ObservedCheck struct {
	Name string
	// Status is GitHub's lifecycle field: "queued", "in_progress", "completed".
	Status string
	// Conclusion is meaningful only once Status == "completed".
	Conclusion string
	// ID is GitHub's monotonically increasing check-run id, used to pick the
	// LATEST attempt when one name reports more than once. See Classify for why
	// latest — and not worst — is the correct rule.
	ID int64
}

// Verdict pairs an EXPECTED check name with what was actually observed.
type Verdict struct {
	Name  string
	State CheckState
	// Why carries the raw GitHub fields, so a failure message names the
	// mechanism rather than just the outcome. "ABSENT" alone sends the reader
	// to the same misdiagnosis this tool exists to prevent.
	Why string
}

// Classify matches an expected check-name set against what a pull request
// actually reports.
//
// The direction of the match is load-bearing and is where the naive version of
// this gate goes wrong. Iterating the OBSERVED checks and requiring that none
// failed passes vacuously on a PR with zero checks — precisely the state that
// merged #637. So iteration is over the EXPECTED set, and a name with no
// observation is a finding rather than an absence of findings.
func Classify(expected []string, observed []ObservedCheck) []Verdict {
	byName := make(map[string]ObservedCheck, len(observed))
	for _, o := range observed {
		// Keep the LATEST attempt per name, by GitHub's check-run id.
		//
		// The tempting rule is "keep the worst", on the reasoning that a green
		// re-run should not paper over a red leg. Measured against this
		// repository it is wrong, and expensively so: ci.yml sets
		// `cancel-in-progress: true`, so every push to an open pull request
		// leaves a `cancelled` run behind next to the successful one that
		// replaced it. Worst-wins flags those as violations, which on a live
		// sweep meant most merged pull requests — and a gate that cries wolf on
		// the normal flow is a gate everyone learns to ignore, which costs more
		// than the hole it closes.
		//
		// Latest-wins is also GitHub's own rule for required checks, so this
		// tool and branch protection cannot disagree about the same commit. The
		// property that actually matters here is untouched by the choice:
		// ABSENT never becomes PASSED under either rule.
		prev, seen := byName[o.Name]
		if !seen || o.ID >= prev.ID {
			byName[o.Name] = o
		}
	}

	verdicts := make([]Verdict, 0, len(expected))
	for _, name := range expected {
		o, ok := byName[name]
		if !ok {
			verdicts = append(verdicts, Verdict{
				Name:  name,
				State: StateAbsent,
				Why:   "no check run reported for this name (never created, or held at action_required awaiting approval)",
			})
			continue
		}
		verdicts = append(verdicts, Verdict{
			Name:  name,
			State: classifyOne(o),
			Why:   fmt.Sprintf("status=%q conclusion=%q", o.Status, o.Conclusion),
		})
	}
	return verdicts
}

func classifyOne(o ObservedCheck) CheckState {
	if o.Status != "completed" {
		return StatePending
	}
	switch o.Conclusion {
	case "success":
		return StatePassed
	case "skipped", "action_required", "stale", "":
		// Deliberately ABSENT, not PASSED. GitHub treats `skipped` as a
		// non-blocking neutral, which is right for an optional job and exactly
		// wrong for a required one: the job did not run, so it verified
		// nothing. `action_required` is the approval hold itself.
		return StateAbsent
	default:
		// failure, cancelled, timed_out, neutral, action_required's siblings —
		// anything that concluded without succeeding.
		return StateFailed
	}
}

// Unsatisfied returns the verdicts that must block a merge: everything that is
// not a clean PASSED. Pending is included — an in-flight check is not a green
// one, and the caller decides whether to wait or to alarm.
func Unsatisfied(verdicts []Verdict) []Verdict {
	var out []Verdict
	for _, v := range verdicts {
		if v.State != StatePassed {
			out = append(out, v)
		}
	}
	return out
}

// workflow is the slice of GitHub workflow syntax this gate reads.
type workflow struct {
	Name string `yaml:"name"`
	// On is deliberately yaml.Node: `on:` is a YAML 1.1 boolean-ish key that
	// round-trips as `true` through some parsers, and its value is a string, a
	// sequence, or a mapping depending on how the workflow was written. All
	// three shapes have to answer "does this trigger on pull_request?".
	On   yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		Name string `yaml:"name"`
		If   string `yaml:"if"`
	} `yaml:"jobs"`
}

// ExpectedChecks derives the check names a pull request MUST report, from the
// workflow definitions themselves.
//
// Deriving rather than hardcoding is what keeps the gate honest as the CI
// surface changes: a new pull_request-triggered job is required the moment it
// exists, with nobody remembering to add it to a list. The inputs are keyed by
// file name purely so failures can name the file.
func ExpectedChecks(workflows map[string]string) ([]string, error) {
	var names []string
	for file, src := range workflows {
		var wf workflow
		if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		if !triggersOnPullRequest(&wf.On) {
			continue
		}
		for key, job := range wf.Jobs {
			if job.If != "" {
				// A job-level `if` makes the check conditionally absent, so
				// requiring it would red the gate on a legitimate skip. None of
				// this repository's pull_request jobs carry one today; this
				// exists so that adding one degrades the gate loudly-by-omission
				// rather than turning it into a false alarm nobody trusts.
				continue
			}
			name := job.Name
			if name == "" {
				// GitHub falls back to the job KEY as the check name.
				name = key
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		// VACUITY GUARD. An empty expected set makes every PR trivially
		// compliant — the instrument reports green having verified nothing,
		// which is the same failure shape as the bug it was built to catch.
		return nil, fmt.Errorf("derived an EMPTY expected-check set from %d workflow file(s): "+
			"either no workflow triggers on pull_request, or their `on:`/`jobs:` shape changed and this "+
			"gate is now a no-op", len(workflows))
	}
	sort.Strings(names)
	return names, nil
}

// triggersOnPullRequest reports whether a workflow's `on:` names pull_request,
// across the three shapes GitHub accepts.
func triggersOnPullRequest(on *yaml.Node) bool {
	if on == nil || on.Kind == 0 {
		return false
	}
	switch on.Kind {
	case yaml.ScalarNode: // on: pull_request
		return strings.TrimSpace(on.Value) == "pull_request"
	case yaml.SequenceNode: // on: [push, pull_request]
		for _, c := range on.Content {
			if strings.TrimSpace(c.Value) == "pull_request" {
				return true
			}
		}
	case yaml.MappingNode: // on: { pull_request: {...} }
		for i := 0; i+1 < len(on.Content); i += 2 {
			if strings.TrimSpace(on.Content[i].Value) == "pull_request" {
				return true
			}
		}
	}
	return false
}
