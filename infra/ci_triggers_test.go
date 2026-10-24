package infra

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// prBaseFilter reports how a workflow's `on:` runs on pull requests: ok is false
// when it does not run on pull_request at all, and filter names the base-branch
// filter ("branches" or "branches-ignore") when one restricts it.
func prBaseFilter(on *yaml.Node) (filter string, ok bool) {
	switch on.Kind {
	case yaml.ScalarNode:
		return "", on.Value == "pull_request"
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if c.Value == "pull_request" {
				return "", true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if on.Content[i].Value != "pull_request" {
				continue
			}
			spec := on.Content[i+1]
			for j := 0; spec.Kind == yaml.MappingNode && j+1 < len(spec.Content); j += 2 {
				if k := spec.Content[j].Value; k == "branches" || k == "branches-ignore" {
					return k, true
				}
			}
			return "", true
		}
	}
	return "", false
}

// jobsWithoutTimeout lists the jobs of a workflow that set no timeout-minutes,
// sorted. GitHub's default is 360 minutes, six hours of a self-hosted runner.
func jobsWithoutTimeout(workflow []byte) ([]string, error) {
	var wf struct {
		Jobs map[string]struct {
			TimeoutMinutes yaml.Node `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &wf); err != nil {
		return nil, err
	}
	var missing []string
	for name, job := range wf.Jobs {
		if job.TimeoutMinutes.Kind == 0 || job.TimeoutMinutes.Tag == "!!null" || job.TimeoutMinutes.Value == "" {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing, nil
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
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	filter, ok := prBaseFilter(&wf.On)
	if !ok {
		t.Fatal("ci.yml does not run on pull_request")
	}
	if filter != "" {
		t.Fatalf("ci.yml's pull_request trigger has a %s filter; stacked PRs into a non-master base would get no CI", filter)
	}
}

func TestJobsWithoutTimeout(t *testing.T) {
	t.Parallel()
	got, err := jobsWithoutTimeout([]byte(`
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
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("jobsWithoutTimeout = %v, want %v", got, want)
	}
	if _, err := jobsWithoutTimeout([]byte("jobs: [")); err == nil {
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
	missing, err := jobsWithoutTimeout(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Fatalf("ci.yml jobs without timeout-minutes: %v", missing)
	}
}
