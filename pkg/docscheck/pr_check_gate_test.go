package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// cmd/verify-pr-checks is the alarm that makes "this pull request's required
// checks never ran" a failure instead of a green merge button. An alarm nobody
// invokes is not an alarm, and the way this one dies is not deletion — it is a
// workflow tidy-up that drops one `run:` line while every other gate stays
// green.
//
// It is also a gate with a specific way of being WIRED wrong, distinct from
// being removed: invoke it from a workflow that itself runs as a pull-request
// check, and it can no longer observe the state it exists for. Held runs hold
// the gate too, so it reports nothing at exactly the moment it has something to
// report — a gate that can only pass when it is the thing running. It has to be
// driven from a trigger the approval policy does not touch (`schedule` /
// `workflow_dispatch` on the default branch).
//
// Both failure modes are checked here, and both are driven from explicit
// workflow-shaped inputs below rather than only from whatever `.github/workflows`
// happens to hold today.
const prCheckGateCmd = "cmd/verify-pr-checks"

// gateWiring classifies how (and whether) the pull-request check gate is
// invoked across a set of workflow sources.
type gateWiring struct {
	// safe names workflows that invoke the gate from a trigger the PR approval
	// policy cannot hold.
	safe []string
	// selfReferential names workflows that invoke the gate but run ONLY as a
	// pull-request check — the shape that cannot observe its own subject.
	selfReferential []string
}

type gateWorkflow struct {
	On   yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func scanGateWiring(t *testing.T, workflows map[string]string) gateWiring {
	t.Helper()
	var w gateWiring
	for file, src := range workflows {
		var wf gateWorkflow
		if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if !invokesGate(wf) {
			continue
		}
		if hasTrigger(&wf.On, "schedule") || hasTrigger(&wf.On, "workflow_dispatch") {
			w.safe = append(w.safe, file)
			continue
		}
		w.selfReferential = append(w.selfReferential, file)
	}
	sort.Strings(w.safe)
	sort.Strings(w.selfReferential)
	return w
}

func invokesGate(wf gateWorkflow) bool {
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for _, line := range strings.Split(step.Run, "\n") {
				line = strings.TrimSpace(line)
				// A command named only inside a SHELL COMMENT does not run. This
				// is the realistic drift — someone comments the invocation out
				// and leaves the text as a note — and it must not count as
				// wiring.
				if strings.HasPrefix(line, "#") {
					continue
				}
				if strings.Contains(line, prCheckGateCmd) {
					return true
				}
			}
		}
	}
	return false
}

func hasTrigger(on *yaml.Node, want string) bool {
	if on == nil || on.Kind == 0 {
		return false
	}
	switch on.Kind {
	case yaml.ScalarNode:
		return strings.TrimSpace(on.Value) == want
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if strings.TrimSpace(c.Value) == want {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if strings.TrimSpace(on.Content[i].Value) == want {
				return true
			}
		}
	}
	return false
}

func readWorkflows(t *testing.T) map[string]string {
	t.Helper()
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(raw)
	}
	if len(out) == 0 {
		t.Fatalf("no workflow files under %s — this gate would pass having read nothing", dir)
	}
	return out
}

// THE CORPUS READING: the repository as it stands must have the gate wired, from
// a trigger that can actually see held pull requests.
func TestPRCheckGateIsWired(t *testing.T) {
	t.Parallel()

	w := scanGateWiring(t, readWorkflows(t))
	if len(w.safe) == 0 {
		t.Errorf("no workflow invokes %s from a schedule/workflow_dispatch trigger.\n"+
			"Without it, a pull request whose required checks never ran presents as mergeable: "+
			"`gh pr checks` prints \"no checks reported\" and the merge state reads UNSTABLE, not blocked. "+
			"That is how PR #637 merged with all six required checks ABSENT.\n"+
			"self-referential invocations found (these cannot observe the state — a held run holds the gate too): %v",
			prCheckGateCmd, w.selfReferential)
	}
	for _, file := range w.selfReferential {
		t.Errorf("%s invokes %s but runs ONLY as a pull-request check.\n"+
			"The approval hold that strands a pull request's checks strands this gate identically, so it "+
			"reports nothing exactly when it has something to report. Drive it from `schedule` instead.",
			file, prCheckGateCmd)
	}
}

// EVERY ARM, from explicit workflow-shaped inputs — including the ones the
// current corpus does not exercise. A gate driven only by today's corpus passes
// vacuously the moment the corpus changes, and its rare arms ship untested.
//
// The three failing cases are the negative controls: synthetic inputs that MUST
// fail the gate, proving it can fail at all.
func TestPRCheckGateWiringArms(t *testing.T) {
	t.Parallel()

	const gateStep = "go run ./" + prCheckGateCmd + " -merged-days 14"

	scheduled := func(run string) string {
		return fmt.Sprintf("on:\n  schedule:\n    - cron: '11 9 * * *'\njobs:\n  a:\n    steps:\n      - run: |\n          %s\n", run)
	}
	prTriggered := func(run string) string {
		return fmt.Sprintf("on:\n  pull_request:\n    branches: [master]\njobs:\n  a:\n    steps:\n      - run: |\n          %s\n", run)
	}

	for _, tc := range []struct {
		name                 string
		workflows            map[string]string
		wantSafe, wantUnsafe int
	}{
		{
			name:      "wired from a scheduled workflow — the shape that works",
			workflows: map[string]string{"nightly-reconcile.yml": scheduled(gateStep)},
			wantSafe:  1,
		},
		{
			name:      "wired from workflow_dispatch only — also unholdable, also fine",
			workflows: map[string]string{"manual.yml": "on:\n  workflow_dispatch:\njobs:\n  a:\n    steps:\n      - run: go run ./" + prCheckGateCmd + "\n"},
			wantSafe:  1,
		},
		{
			name:      "on: as a bare sequence still resolves the trigger",
			workflows: map[string]string{"seq.yml": "on: [schedule, push]\njobs:\n  a:\n    steps:\n      - run: go run ./" + prCheckGateCmd + "\n"},
			wantSafe:  1,
		},
		{
			// NEGATIVE CONTROL 1: the gate is gone entirely.
			name:      "nothing invokes the gate",
			workflows: map[string]string{"ci.yml": scheduled("bazelisk test //...")},
		},
		{
			// NEGATIVE CONTROL 2: the self-referential trap. Wired, but from a
			// trigger the approval hold suppresses — so it can only run when
			// there is nothing to find.
			name:       "wired ONLY as a pull-request check",
			workflows:  map[string]string{"ci.yml": prTriggered(gateStep)},
			wantUnsafe: 1,
		},
		{
			// NEGATIVE CONTROL 3: commented out and left behind as a note. The
			// realistic drift — every other gate stays green and nothing runs.
			name:      "invocation survives only inside a shell comment",
			workflows: map[string]string{"nightly-reconcile.yml": scheduled("# go run ./" + prCheckGateCmd)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := scanGateWiring(t, tc.workflows)
			if len(w.safe) != tc.wantSafe {
				t.Errorf("safe invocations: got %v (%d), want %d", w.safe, len(w.safe), tc.wantSafe)
			}
			if len(w.selfReferential) != tc.wantUnsafe {
				t.Errorf("self-referential invocations: got %v (%d), want %d", w.selfReferential, len(w.selfReferential), tc.wantUnsafe)
			}
		})
	}
}

// The gate derives what it requires from the pull_request-triggered workflows,
// so if that population ever collapses the gate silently requires nothing.
//
// This guard watches for COLLAPSE, and the direction is worth stating because it
// can invert: today a zero here means the derivation broke. If this repository
// ever genuinely stops running checks on pull requests, zero becomes the steady
// state and the alarm to keep is the opposite one — that a required check came
// back unnoticed. Reconcile this floor with that change rather than deleting it.
func TestPullRequestTriggeredWorkflowsExist(t *testing.T) {
	t.Parallel()

	var withPR []string
	for file, src := range readWorkflows(t) {
		var wf gateWorkflow
		if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if hasTrigger(&wf.On, "pull_request") {
			withPR = append(withPR, file)
		}
	}
	if len(withPR) == 0 {
		t.Fatal("no workflow triggers on pull_request — cmd/verify-pr-checks would derive an EMPTY " +
			"expected set and every pull request would be trivially compliant. If this is intentional, " +
			"the guard to keep is the inverted one (alarm when a required check reappears), not none.")
	}
	sort.Strings(withPR)
	t.Logf("pull_request-triggered workflows feeding the expected-check set: %v", withPR)
}
