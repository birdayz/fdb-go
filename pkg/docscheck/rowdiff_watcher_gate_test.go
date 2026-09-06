package docscheck

import (
	"os"
	"os/exec"
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
// of the workspace. Getting its condition wrong has a blast radius, and every
// attempt so far got it wrong in a different way:
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
//   - `outcome == 'success'`: closes those and opens the opposite one. The start
//     step `setsid`s the watcher and only THEN can fail its pid handshake or be
//     cancelled, so requiring success skips cleanup for a process that is
//     demonstrably running.
//
// The predicate they were all approximating is "the start step may have launched
// something", which is that step's outcome being anything but SKIPPED — the only
// outcome meaning no line of it ran. This test pins that, because the failure
// mode has no visible symptom in the run that causes it: the damage lands on a
// DIFFERENT job, on a different night, on the same box.
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

	// Classify on markers UNIQUE to each step, not on a first-match-wins switch.
	// The start step's body also contains `fdb-watch.pid` and `kill` — it writes
	// the pid file and its heredoc signals its own group on the way out — so a
	// looser stop-step marker made the ordering of the cases load-bearing and
	// undocumented. `kill -TERM -"$pid"` appears once in the file.
	var startID, stopIf, stopRun string
	var startN, stopN int
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "cat > fdb-watch.sh") {
				startN++
				startID = step.ID
			}
			if strings.Contains(step.Run, `kill -TERM -"$pid"`) {
				stopN++
				stopIf = step.If
				stopRun = step.Run
			}
		}
	}

	// The population guard, and it is EXACTLY one of each rather than at least
	// one: a rename leaves this test passing over an empty set, and a second
	// match means the markers stopped identifying what they name. Either way
	// every verdict below would be about the wrong thing.
	if startN != 1 || stopN != 1 {
		t.Fatalf("nightly-rowdiff.yml has %d watcher start steps (marker: `cat > fdb-watch.sh`) "+
			"and %d stop steps (marker: `kill -TERM -\"$pid\"`), want exactly 1 of each — this "+
			"gate is measuring nothing or measuring the wrong step, so either restore the "+
			"markers or delete this test deliberately", startN, stopN)
	}
	if startID == "" {
		t.Fatal("the watcher start step has no `id:`, so the stop step cannot gate on its outcome " +
			"and must be gating on something weaker")
	}
	// `!= 'skipped'`, NOT `== 'success'`. `skipped` is the only outcome meaning
	// no line of the start step ran; every other one — success, failure,
	// cancelled — is reachable with the watcher already detached by `setsid`,
	// because the launch precedes the pid handshake that can fail.
	want := "steps." + startID + ".outcome != 'skipped'"
	if !strings.Contains(stopIf, want) {
		t.Fatalf("the watcher stop step's condition is %q, which does not gate on %q.\n"+
			"Three narrower conditions have each left a hole: `always()` alone signals last "+
			"night's pid on a closed-window night; `always() && window.ok` still signals it when "+
			"Checkout fails with the window open, because a plain `if:` implies success() and "+
			"skips the start step while this one runs; and `== 'success'` skips cleanup for a "+
			"watcher that was launched and then failed its pid handshake or was cancelled.",
			stopIf, want)
	}
	// The ownership check. Signalling a process GROUP as root off a number read
	// from a file wants a second opinion, and this one is cheap: ask whether that
	// specific pid is a watcher before killing its group.
	if !strings.Contains(stopRun, "/proc/$pid/cmdline") {
		t.Fatal("the watcher stop step no longer reads /proc/$pid/cmdline before signalling. " +
			"It kills a process GROUP as root off a pid read from a file; without the check a " +
			"recycled or non-numeric pid is signalled blind")
	}
	if !strings.Contains(stopIf, "always()") {
		t.Fatalf("the watcher stop step's condition is %q and does not include always(): a "+
			"cancelled or failed job is exactly when a background poller is left behind, so the "+
			"stop step has to run there too", stopIf)
	}
}

// TestRowdiffWatcherBehaviour runs the shell suite beside this file, which
// extracts the watcher out of the workflow and drives it against a stubbed
// docker. The gate above pins how the STOP step is conditioned; this pins what
// the watcher actually captures, which is the part that was measured wrong
// twice — a 30-second sampler that could not run after the container stopped,
// then a `tail -F` that captured nothing when its glob matched nothing at exec
// time and never followed a rotated file.
//
// Those measurements were taken by hand in a scratch directory and would have
// evaporated with it. A measurement that cannot be re-run is not evidence.
func TestRowdiffWatcherBehaviour(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "pkg", "docscheck", "rowdiff_watcher_suite.sh"))
	out, err := cmd.CombinedOutput()
	t.Logf("rowdiff_watcher_suite.sh output:\n%s", out)
	if err != nil {
		t.Fatalf("rowdiff_watcher_suite.sh failed: %v", err)
	}
	if !strings.Contains(string(out), "ALL OK") {
		t.Fatal("rowdiff_watcher_suite.sh did not report ALL OK — a suite that exits 0 without " +
			"reporting is the empty-set green this repository keeps finding")
	}
}
