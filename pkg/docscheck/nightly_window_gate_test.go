package docscheck

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The fake-green nightly gate.
//
// Every heavy nightly runs on `hetzner-fdb`, a single self-hosted slot, and opens
// with a "Check nightly window" step. When the runner is allocated outside the
// window that step sets ok=false, every later step is `if:`-skipped, and the job
// exits SUCCESS. A green check then means one of two indistinguishable things:
// "the net ran and found nothing" or "the net did not run at all".
//
// It meant the second, for a long time. Measured 2026-07-29 over the scheduled
// runs on this repo: Nightly Stress was green on 12 consecutive nights with
// `Run stress tests` skipped (the last genuine run, 07-17, had FAILED); Nightly
// RowDiff on 10 consecutive nights with the sweep skipped; Nightly Oracles has
// exactly one scheduled run in its history and it skipped.
//
// The mechanism that makes a skipped night visible is a per-job heartbeat
// artifact, written only when the real work actually executes, reconciled daily by
// .github/workflows/nightly-reconcile.yml. That mechanism is only worth anything
// if it cannot silently lose a net — a window gate added without a heartbeat, or a
// heartbeat the reconciler does not read, restores the original silence with no
// visible symptom. This test is what makes that a build failure.
//
// It is deliberately a THREE-WAY equality (windowed jobs <-> heartbeats <->
// reconciler rows) rather than a one-directional check: each direction is a
// distinct way to go quiet.

const (
	windowStepName    = "Check nightly window"
	heartbeatPrefix   = "nightly-heartbeat-"
	reconcileWorkflow = "nightly-reconcile.yml"
)

// workflow is the slice of GitHub Actions schema this gate reads.
type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
			With struct {
				Name string `yaml:"name"`
			} `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// windowedJob names one job that can silently skip all its work.
type windowedJob struct {
	file, job, heartbeat string
}

// scanWorkflows returns every window-gated job, plus every heartbeat artifact
// name produced anywhere. The two are collected separately on purpose: a
// heartbeat published from a job with no window gate is also a defect (it would
// keep the reconciler green while nothing gates the work), and only separate
// collection can see it.
func scanWorkflows(t *testing.T, root string) (windowed []windowedJob, allHeartbeats map[string]string) {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	allHeartbeats = map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		if e.Name() == reconcileWorkflow {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var wf workflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for jobName, job := range wf.Jobs {
			var hasWindow bool
			var heartbeat string
			for _, step := range job.Steps {
				// The gate is identified by its OBSERVABLE effect — a step that can
				// write ok=false — not by its name alone. Renaming the step must not
				// be a way out of this check.
				if step.Name == windowStepName || strings.Contains(step.Run, "ok=false") {
					hasWindow = true
				}
				if strings.HasPrefix(step.With.Name, heartbeatPrefix) {
					heartbeat = step.With.Name
					allHeartbeats[step.With.Name] = e.Name() + ":" + jobName
				}
			}
			if hasWindow {
				windowed = append(windowed, windowedJob{file: e.Name(), job: jobName, heartbeat: heartbeat})
			}
		}
	}
	sort.Slice(windowed, func(i, j int) bool {
		if windowed[i].file != windowed[j].file {
			return windowed[i].file < windowed[j].file
		}
		return windowed[i].job < windowed[j].job
	})
	return windowed, allHeartbeats
}

// reconciledNets extracts the artifact names the reconciliation job actually
// queries, by reading the `net|artifact|max-age` table out of its run script.
// Reading the real script (rather than a duplicated list here) is the point: a
// row deleted from the workflow must fail this test.
func reconciledNets(t *testing.T, root string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", reconcileWorkflow))
	if err != nil {
		t.Fatalf("reading %s: %v — the reconciliation job is what converts a skipped nightly window into a failure; without it every windowed nightly is fake-green again", reconcileWorkflow, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parsing %s: %v", reconcileWorkflow, err)
	}
	nets := map[string]string{}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for _, line := range strings.Split(step.Run, "\n") {
				line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "NETS='"))
				line = strings.TrimSuffix(line, "'")
				fields := strings.Split(line, "|")
				if len(fields) != 3 || !strings.HasPrefix(fields[1], heartbeatPrefix) {
					continue
				}
				nets[fields[1]] = fields[0] + " (max " + fields[2] + "d)"
			}
		}
	}
	return nets
}

func TestNightlyWindowGatesAreReconciled(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	windowed, allHeartbeats := scanWorkflows(t, root)
	nets := reconciledNets(t, root)

	// NO-OP GUARD. If the parse silently found nothing, every assertion below
	// passes vacuously — a green gate guarding nothing, which is the exact
	// disease this file treats. The count is a floor, not an equality: adding a
	// nightly must not fail here, only failing to reconcile one must.
	const knownWindowedJobs = 9
	if len(windowed) < knownWindowedJobs {
		t.Fatalf("found only %d window-gated nightly jobs, expected at least %d — the workflow parse is broken and every check below would pass vacuously; found: %v",
			len(windowed), knownWindowedJobs, windowed)
	}

	for _, w := range windowed {
		if w.heartbeat == "" {
			t.Errorf("%s job %q has a nightly-window gate but publishes no %s* artifact.\n"+
				"When its runner lands outside the window every real step is skipped and the job still exits SUCCESS — indistinguishable from a clean run.\n"+
				"Add the 'Record genuine-run heartbeat' + 'Upload genuine-run heartbeat' steps (see nightly-stress.yml) and a row in %s.",
				w.file, w.job, heartbeatPrefix, reconcileWorkflow)
			continue
		}
		if _, ok := nets[w.heartbeat]; !ok {
			t.Errorf("%s job %q publishes %q but %s never reads it — the heartbeat is written and never checked, so this net can go silent for any number of nights without a single red build.\n"+
				"Add a `net|%s|<max-age-days>` row to its NETS list.",
				w.file, w.job, w.heartbeat, reconcileWorkflow, w.heartbeat)
		}
	}

	// Reverse direction: a reconciler row whose producer is gone would fail every
	// night forever with "never ran" and train everyone to ignore the job.
	for art, limit := range nets {
		if _, ok := allHeartbeats[art]; !ok {
			t.Errorf("%s reconciles %q — %s — but no workflow publishes that artifact.\n"+
				"It will report 'never ran' every single day. Either restore the producing steps or drop the row.",
				reconcileWorkflow, art, limit)
		}
	}

	// A heartbeat from an UNGATED job would keep the reconciler green while
	// nothing protects the work from being skipped some other way.
	gated := map[string]bool{}
	for _, w := range windowed {
		if w.heartbeat != "" {
			gated[w.heartbeat] = true
		}
	}
	for art, where := range allHeartbeats {
		if !gated[art] {
			t.Errorf("%s publishes %q but has no nightly-window gate — a heartbeat is the proof that a GATED job ran; from an ungated job it asserts nothing.", where, art)
		}
	}
}
