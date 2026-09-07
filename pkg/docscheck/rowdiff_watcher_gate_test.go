package docscheck

import (
	"fmt"
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

	if err := checkArms(string(out), wantArms); err != nil {
		t.Fatal(err)
	}
}

// THE ARM POPULATION IS PINNED, NOT FLOORED. `ALL OK` is also what a suite whose
// arms stopped RUNNING prints, and this file already carries that failure: an arm
// asked `git rev-parse --git-dir`, which a Bazel runfiles tree does not have, so
// the suite reported 53 arms locally and 52 under the runner that gates merges —
// and the arm that vanished was the one added to catch a neighbouring silence.
//
// A floor (`got >= want`) does not close that. Add an arm and silently skip it
// and the total is unchanged, which is the 53-vs-52 case exactly; add one while
// another is deleted and the two cancel. Only equality forces the population to
// be RESTATED whenever it moves, which is the same discipline this repository
// applies to every measurement recorded as prose — write the population into the
// claim, so it cannot go stale without something failing.
//
// The cost is real and is the point: every addition reddens this once, until the
// author bumps the pin. That is a prompt to re-read the arm counts quoted in
// three places in the suite, which have gone stale three times — twice inside the
// very commit that added arms to fix the previous staleness.
//
// So both directions are alarms, and they mean different things. FEWER: arms
// disappeared rather than failed, which reports as green. MORE: arms were added
// without updating the pin, and three comments in the suite quote arm counts as
// measurements that are now stale. Neither is a reason to relax this to a floor.
const wantArms = 69

// armCount counts the arms a run reported. Split out from the process globals so
// every branch of the decision below can be driven from a unit test rather than
// only by whatever the corpus happens to produce.
func armCount(out string) int { return strings.Count(out, "\n  ok   ") }

func checkArms(out string, want int) error {
	switch got := armCount(out); {
	case got == 0:
		return fmt.Errorf("rowdiff_watcher_suite.sh reported NO passing arms (want %d) — "+
			"the suite did not run, which is not the same as passing", want)
	case got < want:
		return fmt.Errorf("rowdiff_watcher_suite.sh reported %d passing arms, want exactly %d — "+
			"arms disappeared rather than failed, which reports as green. If arms were "+
			"RETIRED deliberately, lower wantArms in the same commit and say why; do not "+
			"delete this check, or the silence it watches for comes back unwatched", got, want)
	case got > want:
		return fmt.Errorf("rowdiff_watcher_suite.sh reported %d passing arms, want exactly %d — "+
			"arms were added without updating wantArms; bump it and re-check every comment "+
			"in the suite that quotes an arm count as a measurement", got, want)
	}
	return nil
}

// The census above decides from suite OUTPUT, so a full run exercises only the
// branch that run happens to take — which is the equal branch on every green
// day, leaving the two that matter untested until the day they fire. That is the
// shape this repository has been caught by three times: an arm whose first real
// firing is read as a finding rather than as an untested branch. Each branch is
// driven here from explicit state instead.
func TestRowdiffWatcherArmCensus(t *testing.T) {
	t.Parallel()

	arm := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("\n  ok   some arm\n")
		}
		return b.String()
	}

	for _, tc := range []struct {
		name    string
		out     string
		want    int
		wantErr string
	}{
		// The empty-set reading, and the reason this gate exists: a suite that
		// produced nothing must not be indistinguishable from one that passed.
		{"no output at all", "", 3, "reported NO passing arms"},
		{"output but no arms", "\nALL OK\n", 3, "reported NO passing arms"},
		{"an arm disappeared", arm(2), 3, "arms disappeared rather than failed"},
		{"an arm was added", arm(4), 3, "without updating wantArms"},
		{"exactly the pinned population", arm(3), 3, ""},
		// `ok` in prose is not an arm. The count keys on the suite's own two-space
		// prefix and three-space gap, so a line merely containing "ok" cannot
		// inflate the census into passing.
		{"prose mentioning ok is not an arm", arm(3) + "\nlooks ok to me\n  ok but not an arm\n", 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkArms(tc.out, tc.want)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("checkArms(%d arms, want %d) = %v, want nil", armCount(tc.out), tc.want, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("checkArms(%d arms, want %d) = nil, want error containing %q",
					armCount(tc.out), tc.want, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("checkArms error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
