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

// prBaseFilter reports how a workflow's `on:` runs on pull requests: ok is false
// when it does not run on pull_request at all, and filter names the base-branch
// filter ("branches" or "branches-ignore") when one restricts it.
func prBaseFilter(on *yaml.Node) (filter string, ok bool) {
	spec, ok := triggerSpec(on, "pull_request")
	for j := 0; spec != nil && spec.Kind == yaml.MappingNode && j+1 < len(spec.Content); j += 2 {
		if k := spec.Content[j].Value; k == "branches" || k == "branches-ignore" {
			return k, true
		}
	}
	return "", ok
}

// jobsWithoutTimeout lists the jobs of a workflow that set no timeout-minutes,
// sorted, and counts the jobs it parsed. GitHub's default is 360 minutes, six
// hours of a self-hosted runner.
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
			(f[1] == "--test_env=CI" || strings.HasPrefix(f[1], "--test_env=CI=")) {
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
