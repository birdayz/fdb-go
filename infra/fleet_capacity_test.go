package infra

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE SELF-HOSTED FAN-OUT OF ONE PUSH MUST FIT THE PROVISIONED POOL.
//
// This gate exists because of a red `libfdb_c differential` on master that cost
// a full investigation and turned out to be nothing we own — and the one thing
// that WOULD have made it ours is the thing nothing was watching.
//
// WHAT ACTUALLY HAPPENED, so the next person can classify it in a minute rather
// than an evening. On the 2026-08-06 17:26:44Z master push the differential job
// was cancelled after 9m13s having executed ZERO steps, annotated "The job was
// not acquired by Runner of type self-hosted even after multiple attempts". The
// pool journals show at least one runner (gh-runner-drain-2) idle and listening
// from 17:26:45Z continuously past the 17:35:57Z cancellation, having taken no
// job at all. So a healthy, correctly-labelled, IDLE runner was available for
// the entire window in which GitHub reported it could not find one. That is a
// dispatch failure upstream of us — it coincided with a declared GitHub Actions
// incident — and no timeout, capacity or scaler change could have prevented it.
//
// THE SIGNATURE THAT SEPARATES IT FROM A REAL FAILURE, worth knowing by heart:
// `steps: []` on the job plus that annotation means NOTHING RAN. A lane in that
// state is not red about what it tests; it never tested anything. Check the job
// step count before reading a red safety net as a regression.
//
// WHAT THIS GATE THEREFORE DOES NOT DO: it cannot prevent that. What it prevents
// is the failure that would look identical and WOULD be ours — oversubscribing
// the pool so a job waits past GitHub's fixed ~10-minute unacquired-job window
// and is cancelled with the same zero-step signature. Today one push fans out
// exactly as many self-hosted jobs as there are slots, and that 1:1 is a
// coincidence of counts that nothing was holding. Add a fifth CI job and the
// margin is gone silently.
//
// The window is GitHub's and is not configurable: nightly-libfdbc.yml sets
// timeout-minutes 40 and an inner `go test -timeout 30m`, so the ~10 minutes has
// no relationship to any budget in this repo. Slots are the only lever we own.

// runnerLabel is the self-hosted pool's label. A job on any other label (or on a
// GitHub-hosted runner) does not consume a slot and is not counted.
const runnerLabel = "hetzner-fdb-vm"

// dedicatedRunners is the grandfathered box that exists outside var.runner_count
// (hcloud_server "runner" / gh-runner-fdb). The pool resource is counted from
// the variable; this one is a separate resource and has to be added by hand.
const dedicatedRunners = 1

// workflowFile is the subset of the schema this gate reads.
type workflowFile struct {
	On   yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		RunsOn yaml.Node `yaml:"runs-on"`
	} `yaml:"jobs"`
}

// hasPushTrigger reports whether the workflow runs on `push`.
//
// `on` is one of YAML's genuinely awkward keys — it can be a scalar ("on: push"),
// a sequence ("on: [push, pull_request]") or a mapping ("on:\n  push:") — and it
// is also the YAML 1.1 boolean `true`, which some parsers hand back as a bool
// key. All three shapes are handled because a gate that silently matched none of
// them would count zero workflows and pass while asserting nothing.
func hasPushTrigger(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value == "push"
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Value == "push" {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "push" {
				return true
			}
		}
	}
	return false
}

// usesLabel reports whether a runs-on value names the pool label, in either the
// scalar or the list form.
func usesLabel(n *yaml.Node, label string) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value == label
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Value == label {
				return true
			}
		}
	}
	return false
}

// provisionedSlots reads var.runner_count's default out of main.tf and adds the
// dedicated box.
//
// Read from the Terraform source rather than hard-coded so that raising capacity
// and raising this gate's budget are THE SAME EDIT. A hard-coded number here
// would let the two drift, and a capacity gate that disagrees with the capacity
// is worse than none.
func provisionedSlots(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("main.tf")
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	re := regexp.MustCompile(`(?s)variable\s+"runner_count"\s*\{.*?default\s*=\s*(\d+)`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatal("main.tf: could not find variable \"runner_count\" default. This gate reads " +
			"capacity from the Terraform source on purpose — if the variable was renamed or " +
			"restructured, update this reader rather than hard-coding a number here, or the " +
			"budget and the fleet drift apart silently")
	}
	var n int
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n + dedicatedRunners
}

func TestPushFanOutFitsTheRunnerPool(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob workflows: %v (found %d)", err, len(paths))
	}

	perWorkflow := map[string]int{}
	total := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		if !hasPushTrigger(&wf.On) {
			continue
		}
		n := 0
		for _, job := range wf.Jobs {
			if usesLabel(&job.RunsOn, runnerLabel) {
				n++
			}
		}
		if n > 0 {
			perWorkflow[filepath.Base(p)] = n
			total += n
		}
	}

	slots := provisionedSlots(t)

	// A gate that matched no workflow at all would pass vacuously, and this one
	// navigates the `on:`/`runs-on:` shape zoo to find its population — exactly
	// where a silent miss would hide.
	if total == 0 {
		t.Fatalf("counted ZERO push-triggered %s jobs across %d workflow file(s). The "+
			"parse found no population, so the budget below is asserting nothing — check "+
			"hasPushTrigger and usesLabel against the current workflow syntax before "+
			"believing this number", runnerLabel, len(paths))
	}

	names := make([]string, 0, len(perWorkflow))
	for k := range perWorkflow {
		names = append(names, k)
	}
	sort.Strings(names)

	if total > slots {
		var detail string
		for _, k := range names {
			detail += "\n    " + k + ": " + itoa(perWorkflow[k])
		}
		t.Fatalf("one push fans out %d self-hosted %s job(s) but the fleet provisions %d "+
			"slot(s) (var.runner_count + %d dedicated).%s\n"+
			"  OVERSUBSCRIPTION IS NOT A QUEUEING INCONVENIENCE HERE. A job that waits for a\n"+
			"  runner past GitHub's fixed ~10-minute unacquired window is CANCELLED, and it\n"+
			"  is cancelled having executed zero steps — so the lane reports RED while having\n"+
			"  tested nothing. On the wire-differential lane that is a wire-format safety net\n"+
			"  going red for reasons unrelated to wire format.\n"+
			"  The window is GitHub's and is not configurable: nightly-libfdbc.yml already\n"+
			"  allows timeout-minutes 40. Slots are the only lever this repo owns.\n"+
			"  FIX BY RAISING CAPACITY (var.runner_count in main.tf, then apply), not by\n"+
			"  raising a timeout and not by deleting this check.",
			total, runnerLabel, slots, dedicatedRunners, detail)
	}
}

// itoa avoids pulling strconv in for one call in a failure path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
