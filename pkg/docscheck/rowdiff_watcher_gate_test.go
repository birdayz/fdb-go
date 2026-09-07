package docscheck

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

	if err := checkArms(string(out), wantArmLabels); err != nil {
		t.Fatal(err)
	}
}

// THE ARM POPULATION IS PINNED BY IDENTITY. `ALL OK` is also what a suite whose
// arms stopped RUNNING prints, and this file already carries that failure: an arm
// asked `git rev-parse --git-dir`, which a Bazel runfiles tree does not have, so
// the suite reported 53 arms locally and 52 under the runner that gates merges —
// and the arm that vanished was the one added to catch a neighbouring silence.
//
// A floor (`got >= want`) does not close that: add an arm and silently skip it
// and the total never moves. Nor does a COUNT, even an exact one — add one arm
// while another is skipped, or duplicate a label while deleting a different one,
// and the cardinality is identical. That is the same cancellation one level up,
// which is why this compares LABELS. There is no count comparison left to get
// wrong: a run with the full population plus a duplicate is not caught by any
// arithmetic between "labels that ran" and "labels pinned" — those count
// different things, lines against distinct entries — so a repeated label is
// detected as its own state instead.
//
// Three states, and each section prints ONLY when it has members. Rendering all
// three unconditionally reads as thorough and makes the message
// undiscriminating: an assertion on the word MISSING then holds for an
// unexpected arm too, and swapping the two lists passes every test.
//
// The cost is real and is the point: every arm added, renamed or removed reddens
// this once, until the author restates the population here. That is a prompt to
// re-read the arm counts quoted elsewhere in the suite, which have gone stale
// three times — twice inside the very commit that was fixing the previous
// staleness.
//
// Three states, three messages. NONE: the suite did not run, which is not the
// same as passing. MISSING: arms disappeared rather than failed, which reports as
// green. UNEXPECTED: arms were added without restating the population.
const wantArmLabels = `
a daemon outage is not logged as a removal
a deletion cut short leaves no published generation behind
a failed copy publishes no trace directory
a FAILED exit copy leaves no staging directory
a FAILED exit copy says so in the watcher log
a failing copy stretch is logged exactly once
a false precondition fails its case by name
a false precondition runs no mutation
a live container in the dump is evidence (rc=0)
an empty capture says "(no fdb-container-*.log)"
an empty capture says "(no fdb-df-*.txt)"
an empty capture says "(no fdb-logs-* directories)"
an exhausted outage backstop is reported as an outage, not a removal
an inspect with no traces on a failed night is NOT evidence (rc=1)
an inspect WITH traces on a failed night is evidence (rc=0)
an occupied exit destination is REFUSED, not nested into
a refused exit publish is reported as NOT copied
a removed container ends the copier exactly once
a re-selected container gets no second copier (launch ids: 1 )
a re-selected container's exit is logged exactly once
a ROTATED second trace file is captured
a single blip is not reported as the container ending
a tolerated outage skips the copy instead of failing it
a trace file created after the watcher attached is captured
a transient inspect failure does not end periodic capture
c1 was re-selected, so the guard was actually exercised (2 selections)
Capture FDB container forensics: an unpinned .bazelrc fails the tag guard loudly
Capture FDB container forensics: a pinned .bazelrc passes the tag guard
digest: a change in the Java reference tree does not (same)
digest: a container listing registers (moved)
digest: a disk-free capture registers (moved)
digest: a file inside an existing generation directory registers (moved)
digest: a file in the hidden staging directory registers (moved)
digest: a file two levels down registers (moved)
digest: a last-inspect capture registers (moved)
digest: an append inside a generation directory moves it (moved)
digest: an append to an existing log moves it (moved)
digest: an empty generation directory registers (moved)
digest: an equal-length rewrite inside a generation directory moves it (moved)
digest: a new watcher log registers (moved)
digest: a pid file registers (moved)
digest: a retired generation registers (moved)
digest: a same-size pid rewrite moves it (moved)
digest: the forensics report registers (moved)
empty capture + both sweeps green stays green (rc=0)
empty capture + deep sweep failed exits non-zero (rc=1)
empty capture + PAGING sweep failed exits non-zero (rc=1)
empty then present is a reload, not a removal
empty then UNREACHABLE is an outage, not a removal
exactly one published generation survives the prune
no false 'NOT copied at exit' for a container whose trace was captured
recovery on the FIRST sample clears the skip signal
recovery through the RE-ISSUE clears it too
removal is logged
the copy publishes once it succeeds
the df sampler survives an outage and keeps sampling
the dump reads the per-container last inspect
the dump reads the trace directory the watcher wrote
the exit-transition copy alone captures the terminal line
the exit-transition copy leaves no staging directory
the exit transition is logged exactly once
the injected outage was observed by the copier
the last inspect survives removal
the PERIODIC copier leaves no staging directory
the recovery is logged exactly once
the suite leaves its working directory unchanged
the surviving generation still holds its traces
the terminal Severity=40 line is captured after the container stops
two empty answers mean REMOVED
unreachable from the first call is an outage
Watch the FDB container while it is alive: an unpinned .bazelrc fails the tag guard loudly
Watch the FDB container while it is alive: a pinned .bazelrc passes the tag guard
`

// armLabels returns the labels of the arms a run reported passing. Split out from
// the process globals so every branch of the decision below can be driven from a
// unit test rather than only by whatever a green run happens to produce.
func armLabels(out string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "  ok   "); ok {
			got = append(got, rest)
		}
	}
	sort.Strings(got)
	return got
}

func checkArms(out, want string) error {
	got := armLabels(out)
	if len(got) == 0 {
		return fmt.Errorf("rowdiff_watcher_suite.sh reported NO passing arms — the suite did " +
			"not run, which is not the same as passing")
	}
	expected := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(want), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			expected[l] = true
		}
	}
	var unexpected, duplicated []string
	seen := map[string]int{}
	for _, l := range got {
		seen[l]++
		switch {
		case seen[l] == 2:
			duplicated = append(duplicated, l)
		case seen[l] == 1 && !expected[l]:
			unexpected = append(unexpected, l)
		}
	}
	var missing []string
	for l := range expected {
		if seen[l] == 0 {
			missing = append(missing, l)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 && len(unexpected) == 0 && len(duplicated) == 0 {
		return nil
	}
	// Each section is emitted ONLY when it has members. Rendering all three
	// unconditionally reads as thorough and makes the message undiscriminating:
	// an assertion on the word "MISSING" then holds for an unexpected arm too, so
	// swapping the two lists passes every test. The section headings are the
	// values under test, not decoration.
	b := &strings.Builder{}
	fmt.Fprintf(b, "rowdiff_watcher_suite.sh arm population differs: %d arms ran, %d pinned.",
		len(got), len(expected))
	if len(missing) > 0 {
		fmt.Fprintf(b, "\nMISSING — pinned but did not run. Arms disappeared rather than failed, "+
			"which reports as green. If they were RETIRED deliberately, remove them from "+
			"wantArmLabels in the same commit and say why: %v", missing)
	}
	if len(unexpected) > 0 {
		fmt.Fprintf(b, "\nUNEXPECTED — ran but is not pinned. Add to wantArmLabels and re-check "+
			"every comment in the suite that quotes an arm count: %v", unexpected)
	}
	if len(duplicated) > 0 {
		fmt.Fprintf(b, "\nDUPLICATED — the same label ran more than once, so one arm's result is "+
			"standing in for another's. Give each case a distinct name: %v", duplicated)
	}
	return errors.New(b.String())
}

// The census above decides from suite OUTPUT, so a full run exercises only the
// branch that run happens to take — which is the equal branch on every green
// day, leaving the two that matter untested until the day they fire. That is the
// shape this repository has been caught by three times: an arm whose first real
// firing is read as a finding rather than as an untested branch. Each branch is
// driven here from explicit state instead.
func TestRowdiffWatcherArmCensus(t *testing.T) {
	t.Parallel()

	out := func(labels ...string) string {
		var b strings.Builder
		for _, l := range labels {
			b.WriteString("  ok   " + l + "\n")
		}
		return b.String()
	}
	pinned := "a\nb\nc\n"

	// `want` and `notWant` are both asserted, and the second is what makes the
	// first mean anything: every section heading used to be rendered whether or
	// not it had members, so an assertion on "MISSING" also held for an
	// unexpected arm and swapping the two lists passed every case here.
	for _, tc := range []struct {
		name    string
		out     string
		want    []string
		notWant []string
	}{
		// The empty-set reading, and the reason this gate exists: a suite that
		// produced nothing must not be indistinguishable from one that passed.
		{"no output at all", "", []string{"reported NO passing arms"}, []string{"MISSING"}},
		{"output but no arms", "\nALL OK\n", []string{"reported NO passing arms"}, nil},
		{"an arm disappeared", out("a", "b"), []string{"MISSING", "[c]"}, []string{"UNEXPECTED", "DUPLICATED"}},
		{"an arm was added", out("a", "b", "c", "d"), []string{"UNEXPECTED", "[d]"}, []string{"MISSING", "DUPLICATED"}},
		// The case a COUNT cannot see: one arm skipped, another added, cardinality
		// unchanged. Both sections must name the right label, in the right role.
		{
			"a same-count substitution", out("a", "b", "d"),
			[]string{"MISSING", "[c]", "UNEXPECTED", "[d]"},
			[]string{"DUPLICATED"},
		},
		{
			"a duplicate masking a deletion", out("a", "b", "b"),
			[]string{"MISSING", "[c]", "DUPLICATED", "[b]"},
			[]string{"UNEXPECTED"},
		},
		// A duplicate with NOTHING missing: the population is complete and one
		// label still ran twice, so a copy-pasted case reusing a name is caught
		// on its own, not only as a side effect of some other arm vanishing.
		{
			"a duplicate alongside the full population", out("a", "a", "b", "c"),
			[]string{"DUPLICATED", "[a]"},
			[]string{"MISSING", "UNEXPECTED"},
		},
		{"exactly the pinned population", out("a", "b", "c"), nil, nil},
		{"order does not matter", out("c", "a", "b"), nil, nil},
		// `ok` in prose is not an arm. The scan keys on the suite's own two-space
		// prefix and three-space gap, so a line merely containing "ok" cannot
		// inflate the census into passing.
		{"prose mentioning ok is not an arm", out("a", "b", "c") + "looks ok to me\n  ok but not an arm\n", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkArms(tc.out, pinned)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("checkArms(%v) = %v, want nil", armLabels(tc.out), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkArms(%v) = nil, want an error containing %v", armLabels(tc.out), tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("checkArms error = %q, want it to contain %q", err, w)
				}
			}
			for _, n := range tc.notWant {
				if strings.Contains(err.Error(), n) {
					t.Errorf("checkArms error = %q, want it NOT to contain %q", err, n)
				}
			}
		})
	}
}
