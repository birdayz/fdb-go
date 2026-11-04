package infra

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// triggerSpec finds event in a workflow's `on:` and returns its spec node, nil
// when the event takes none.
//
// `on` is one of YAML's genuinely awkward keys — it can be a scalar ("on: push"),
// a sequence ("on: [push, pull_request]") or a mapping ("on:\n  push:") — and it
// is also the YAML 1.1 boolean `true`, which some parsers hand back as a bool
// key. All three shapes are handled because a gate that silently matched none of
// them would count zero workflows and pass while asserting nothing.
func triggerSpec(on *yaml.Node, event string) (spec *yaml.Node, ok bool) {
	switch on.Kind {
	case yaml.ScalarNode:
		return nil, on.Value == event
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if c.Value == event {
				return nil, true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if on.Content[i].Value == event {
				return on.Content[i+1], true
			}
		}
	}
	return nil, false
}

// prBaseFilter reports whether `on:` runs on pull_request and names the
// base-branch filter ("branches" or "branches-ignore") that restricts it.
func prBaseFilter(on *yaml.Node) (filter string, ok bool) {
	spec, ok := triggerSpec(on, "pull_request")
	for j := 0; spec != nil && spec.Kind == yaml.MappingNode && j+1 < len(spec.Content); j += 2 {
		if k := spec.Content[j].Value; k == "branches" || k == "branches-ignore" {
			return k, true
		}
	}
	return "", ok
}

// jobsWithoutTimeout lists, sorted, the jobs without timeout-minutes (GitHub's
// default is six hours of a runner) and counts the jobs it parsed.
func jobsWithoutTimeout(workflow []byte) (missing []string, jobs int, err error) {
	var wf struct {
		Jobs map[string]struct {
			TimeoutMinutes yaml.Node `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &wf); err != nil {
		return nil, 0, err
	}
	for name, job := range wf.Jobs {
		if job.TimeoutMinutes.Kind == 0 || job.TimeoutMinutes.Tag == "!!null" || job.TimeoutMinutes.Value == "" {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing, len(wf.Jobs), nil
}

func parseOn(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
		t.Fatal(err)
	}
	return &wf.On
}

func TestPRBaseFilter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, on, filter string
		ok               bool
	}{
		{"scalar", "on: pull_request\n", "", true},
		{"scalar other event", "on: push\n", "", false},
		{"sequence", "on: [push, pull_request]\n", "", true},
		{"sequence without", "on: [push]\n", "", false},
		{"mapping, null spec", "on:\n  pull_request:\n", "", true},
		{"mapping, types only", "on:\n  pull_request:\n    types: [opened]\n", "", true},
		{"mapping, branches", "on:\n  pull_request:\n    branches: [master]\n", "branches", true},
		{"mapping, branches-ignore", "on:\n  pull_request:\n    branches-ignore: [x]\n", "branches-ignore", true},
		{"mapping without", "on:\n  push:\n    branches: [master]\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			filter, ok := prBaseFilter(parseOn(t, tc.on))
			if filter != tc.filter || ok != tc.ok {
				t.Fatalf("prBaseFilter = (%q, %t), want (%q, %t)", filter, ok, tc.filter, tc.ok)
			}
		})
	}
}

// A stacked PR (gh stack, mandated by AGENTS.md) targets the branch below it, so
// a base-branch filter on ci.yml's pull_request trigger leaves it without CI.
func TestCIRunsOnEveryPullRequestBase(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	filter, ok := prBaseFilter(parseOn(t, string(data)))
	if !ok {
		t.Fatal("ci.yml does not run on pull_request")
	}
	if filter != "" {
		t.Fatalf("ci.yml's pull_request trigger has a %s filter; stacked PRs into a non-master base would get no CI", filter)
	}
}

func TestJobsWithoutTimeout(t *testing.T) {
	t.Parallel()
	got, jobs, err := jobsWithoutTimeout([]byte(`
jobs:
  b:
    runs-on: x
  a:
    runs-on: x
    timeout-minutes:
  c:
    timeout-minutes: 30
  d:
    timeout-minutes: ${{ inputs.minutes }}
`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b"}; !slices.Equal(got, want) || jobs != 4 {
		t.Fatalf("jobsWithoutTimeout = %v of %d jobs, want %v of 4", got, jobs, want)
	}
	if got, jobs, err := jobsWithoutTimeout([]byte("name: x\n")); err != nil || got != nil || jobs != 0 {
		t.Fatalf("jobsWithoutTimeout(no jobs) = (%v, %d, %v), want (nil, 0, nil)", got, jobs, err)
	}
	if _, _, err := jobsWithoutTimeout([]byte("jobs: [")); err == nil {
		t.Fatal("jobsWithoutTimeout accepted malformed YAML")
	}
}

// Every ci.yml job runs on the two-slot self-hosted pool, where GitHub's default
// six-hour job timeout would hold a slot for a whole working day.
func TestCIJobsHaveTimeouts(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	missing, jobs, err := jobsWithoutTimeout(data)
	if err != nil {
		t.Fatal(err)
	}
	if jobs == 0 {
		t.Fatal("parsed no jobs from ci.yml: the check would pass over nothing")
	}
	if len(missing) > 0 {
		t.Fatalf("ci.yml jobs without timeout-minutes: %v", missing)
	}
}

// bazelrcForwardsCI reports whether a .bazelrc passes CI into tests under the
// ci config (or under every config).
func bazelrcForwardsCI(bazelrc string) bool {
	for _, line := range strings.Split(bazelrc, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[0] == "test" || f[0] == "test:ci") &&
			(f[1] == "--test_env=CI" || (strings.HasPrefix(f[1], "--test_env=CI=") && len(f[1]) > len("--test_env=CI="))) {
			return true
		}
	}
	return false
}

func TestBazelrcForwardsCI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rc   string
		want bool
	}{
		{"test:ci --test_env=CI\n", true},
		{"test --test_env=CI\n", true},
		{"test:ci --test_env=CI=true\n", true},
		{"test:ci --test_env=CI=\n", false},
		{"# test:ci --test_env=CI\n", false},
		{"test:ci --test_env=CIX\n", false},
		{"build:ci --test_env=CI\n", false},
		{"test --test_env=HOME\n", false},
		{"", false},
	} {
		if got := bazelrcForwardsCI(tc.rc); got != tc.want {
			t.Errorf("bazelrcForwardsCI(%q) = %t, want %t", tc.rc, got, tc.want)
		}
	}
}

// foundationdb.SkipWithoutDocker never skips with CI set, and Bazel gives tests
// only the --test_env variables, so the CI lanes must forward it.
func TestCIForwardsCIToTests(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../.bazelrc")
	if err != nil {
		t.Fatal(err)
	}
	if !bazelrcForwardsCI(string(data)) {
		t.Fatal(".bazelrc does not pass CI to tests under --config=ci; SkipWithoutDocker would skip in CI")
	}
}

const fullDispatchCondition = "${{ github.event_name == 'workflow_dispatch' && inputs.full }}"

// Only this known dispatch-only predicate can reduce the push fan-out count.
// Unknown expressions remain counted rather than guessed to be false on push.
func dispatchOnlyJob(condition string) bool {
	return strings.TrimSpace(condition) == fullDispatchCondition
}

type evidenceStep struct {
	ID   string            `yaml:"id"`
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type evidenceWorkflow struct {
	On          yaml.Node `yaml:"on"`
	Concurrency struct {
		Group string `yaml:"group"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If          string            `yaml:"if"`
		Needs       []string          `yaml:"needs"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []evidenceStep    `yaml:"steps"`
	} `yaml:"jobs"`
}

func readEvidenceWorkflow(t *testing.T) evidenceWorkflow {
	t.Helper()
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf evidenceWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func fullLaneStep(t *testing.T, id string) evidenceStep {
	t.Helper()
	for _, step := range readEvidenceWorkflow(t).Jobs["full"].Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("full job has no step %q", id)
	return evidenceStep{}
}

func TestFullLaneIsOptInAndPinned(t *testing.T) {
	t.Parallel()
	wf := readEvidenceWorkflow(t)
	spec, ok := triggerSpec(&wf.On, "workflow_dispatch")
	if !ok || spec == nil {
		t.Fatal("CI has no dispatch input specification")
	}
	var dispatch struct {
		Inputs map[string]struct {
			Type    string    `yaml:"type"`
			Default yaml.Node `yaml:"default"`
		} `yaml:"inputs"`
	}
	if err := spec.Decode(&dispatch); err != nil {
		t.Fatal(err)
	}
	input, ok := dispatch.Inputs["full"]
	if !ok || input.Type != "boolean" || input.Default.Tag != "!!bool" || input.Default.Value != "false" {
		t.Fatalf("full input must be boolean and default false: %+v", input)
	}
	job, ok := wf.Jobs["full"]
	if !ok || job.If != fullDispatchCondition {
		t.Fatalf("full job must require explicit dispatch opt-in, got %q", job.If)
	}
	wantNeeds := []string{"ci", "conformance-java", "wire-oracle", "fdb-go-install", "govulncheck", "libfdbc-differential", "race"}
	if !slices.Equal(job.Needs, wantNeeds) {
		t.Fatalf("full job dependencies = %v, want %v", job.Needs, wantNeeds)
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatalf("full job must have read-only permissions: %v", job.Permissions)
	}
	if !strings.Contains(wf.Concurrency.Group, "github.event_name == 'workflow_dispatch' && inputs.full && format('-full-{0}', github.run_id)") {
		t.Fatal("a branch push could cancel a full dispatch in the shared workflow concurrency group")
	}
	checkout := false
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout = step.With["ref"] == "${{ github.sha }}" && step.With["persist-credentials"] == "false"
		}
		if strings.Contains(step.Run, "merge-current-master") {
			t.Fatal("full dispatch must not replace its immutable SHA with a synthesized merge")
		}
		for key := range step.Env {
			if key == "TMPDIR" || key == "FDB_LANE_LOCK" {
				t.Fatalf("full dispatch must not create a different lane lock through %s", key)
			}
		}
	}
	if !checkout {
		t.Fatal("full dispatch must check out github.sha without persisted credentials")
	}
	prepare := fullLaneStep(t, "prepare").Run
	if !strings.Contains(prepare, `test "$(git rev-parse HEAD)" = "$GITHUB_SHA"`) || !strings.Contains(prepare, `mktemp -d "$RUNNER_TEMP/fdb-full-ci.XXXXXX"`) {
		t.Fatal("full lane must verify its SHA and prepare evidence outside the worktree")
	}
	upload := false
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && step.With["name"] == "full-lane-evidence" {
			upload = strings.Contains(step.If, "always()") && step.With["if-no-files-found"] == "error" && step.With["path"] == "${{ steps.prepare.outputs.evidence }}"
		}
	}
	if !upload {
		t.Fatal("full lane evidence must survive failed tests and reject empty uploads")
	}
}

func TestDispatchOnlyFanOutCondition(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		condition string
		want      bool
	}{
		{fullDispatchCondition, true},
		{"  " + fullDispatchCondition + "  ", true},
		{"", false},
		{"${{ inputs.full }}", false},
		{"${{ github.event_name == 'push' }}", false},
		{"${{ github.event_name == 'workflow_dispatch' || inputs.full }}", false},
		{"unknown expression", false},
	} {
		if got := dispatchOnlyJob(tc.condition); got != tc.want {
			t.Errorf("dispatchOnlyJob(%q) = %t, want %t", tc.condition, got, tc.want)
		}
	}
}

func TestCIReportsIdentifyTheirTestedInputsAndScope(t *testing.T) {
	t.Parallel()
	wf := readEvidenceWorkflow(t)
	for jobID, lane := range map[string]string{"ci": "suite", "conformance-java": "conformance-java", "full": "full-default-budgets"} {
		found := false
		for _, step := range wf.Jobs[jobID].Steps {
			if !strings.Contains(step.Run, "--lane "+lane+" ") {
				continue
			}
			found = true
			sha := `--sha "$(git rev-parse HEAD)"`
			if jobID == "full" {
				sha = `--sha "$GITHUB_SHA"`
			}
			for _, want := range []string{sha, `--run-url "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"`, "--exclusions ", ".jsonl"} {
				if !strings.Contains(step.Run, want) {
					t.Errorf("%s report lacks %q", jobID, want)
				}
			}
		}
		if !found {
			t.Errorf("no attributed report for %s", jobID)
		}
	}
}
