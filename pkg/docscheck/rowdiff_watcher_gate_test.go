package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The rowdiff watcher's stop gate.
//
// nightly-rowdiff.yml starts a background process that outlives its own step —
// a poller that watches the FDB container while it is alive, because by the
// time any later step runs the container has been removed. That is useful and
// it is also a loaded gun: these runners are PERSISTENT boxes with durable
// registrations, so a survivor spins for the life of the host and then attaches
// `docker logs -f` to the next night's container.
//
// The stop step therefore signals a process GROUP, as root, from a pid read out
// of the workspace. Getting its condition wrong has a blast radius, and three
// successive attempts got it wrong in three different ways:
//
//   - `always()` alone: on a closed-window night `Checkout` is skipped, so the
//     workspace still holds LAST night's pid file and nothing has cleared it.
//   - `always() && window.ok`: closes that one. A plain `if:` is implicitly
//     `success() && …`, so on a Checkout FAILURE with the window open the start
//     step is skipped while the stop step still runs — and `git clean` never ran
//     either, so the stale file is right there.
//   - an `rm` in the START step: gated identically, so it misses exactly those
//     paths, and redundant on the path it does run because checkout's default
//     `clean: true` has already removed the untracked file.
//
// The predicate all three were approximating is "stop what THIS JOB started",
// which is the start step's own outcome. This test pins that, because the
// failure mode has no visible symptom in the run that causes it: the damage
// lands on a DIFFERENT job, on a different night, on the same box.
func TestRowdiffWatcherStopIsGatedOnTheStartStep(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), ".github", "workflows", "nightly-rowdiff.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				ID   string `yaml:"id"`
				If   string `yaml:"if"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var startID, stopIf string
	var sawStart, sawStop bool
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			switch {
			case strings.Contains(step.Run, "cat > fdb-watch.sh"):
				sawStart = true
				startID = step.ID
			case strings.Contains(step.Run, "fdb-watch.pid") && strings.Contains(step.Run, "kill"):
				sawStop = true
				stopIf = step.If
			}
		}
	}

	// The population guard. Both steps must be FOUND before any verdict about
	// them means anything: a rename would otherwise leave this test passing
	// over an empty set, which is the failure it exists to prevent wearing a
	// different hat.
	if !sawStart || !sawStop {
		t.Fatalf("nightly-rowdiff.yml no longer has both a watcher start step (found=%v) and a "+
			"stop step that kills by pid (found=%v) — this gate is measuring nothing, so either "+
			"restore them or delete this test deliberately", sawStart, sawStop)
	}
	if startID == "" {
		t.Fatal("the watcher start step has no `id:`, so the stop step cannot gate on its outcome " +
			"and must be gating on something weaker")
	}
	want := "steps." + startID + ".outcome == 'success'"
	if !strings.Contains(stopIf, want) {
		t.Fatalf("the watcher stop step's condition is %q, which does not gate on %q.\n"+
			"It must stop only what this job started. `always()` alone signals last night's pid "+
			"on a closed-window night; `always() && window.ok` still signals it when Checkout "+
			"fails with the window open, because a plain `if:` implies success() and skips the "+
			"start step while this one runs.", stopIf, want)
	}
	if !strings.Contains(stopIf, "always()") {
		t.Fatalf("the watcher stop step's condition is %q and does not include always(): a "+
			"cancelled or failed job is exactly when a background poller is left behind, so the "+
			"stop step has to run there too", stopIf)
	}
}
