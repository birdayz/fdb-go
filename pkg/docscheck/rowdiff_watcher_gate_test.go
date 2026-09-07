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
// which is why this compares LABELS. The count comparison it replaced was not
// blind — `len(got) == len(expected)` compares lines against distinct entries,
// so a full population of N labels plus one duplicate is N+1 lines against N
// distinct entries, and it did return an error. What it could not do is SAY
// anything: both lists came back empty and the message named no label, so the
// one state it detected was the one state it could not describe. And nothing
// drove it, so that was never visible. A repeated label is its own state now,
// with its own message and its own case —
// the gain is diagnosis and coverage, not detection, and the first draft of this
// comment claimed detection because "the arithmetic is gone" felt like it needed
// a stronger justification than it did.
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
// FOUR states, four messages, each naming what it means and what to do. NONE: the
// suite did not run, which is not the same as passing. MISSING: arms disappeared
// rather than failed, which reports as green. UNEXPECTED: arms ran that are not
// pinned. DUPLICATED: one label ran twice, so an arm's result is standing in for
// another's. A count sees this only sometimes: a duplicate ALONGSIDE the full
// population is 4 lines against 3 distinct entries and any arithmetic catches it,
// while a duplicate REPLACING a missing arm is 3 against 3 and none can. That
// second shape is the one a count is blind to, and the sentence here said "a
// count cannot see at all" — reinstating, one line below correcting it, the
// overclaim this whole section is about.
// This enumeration was "three states, three messages" for a round after the
// fourth was added, which is the same class of defect as everything it guards:
// a description of the code that the code has outgrown.
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
the live probe check goes through the guarded decision
the PERIODIC copier leaves no staging directory
the precondition probe has something to compare
the probe guard rejects an empty reading of a file that exists
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

	// Assertions bind a label to its SECTION, and that binding is the whole point.
	// Matching substrings against the WHOLE message cannot distinguish a report
	// from its inverse: for a same-count substitution the message contains
	// MISSING, [c], UNEXPECTED and [d], and so does the message that files each
	// label under the other heading. Every case that populates two sections was
	// therefore green against a payload swap — the same undiscriminating-verdict
	// defect one level up, in the fix for it.
	section := func(msg, name string) (string, bool) {
		for _, line := range strings.Split(msg, "\n") {
			if strings.HasPrefix(line, name+" — ") {
				return line, true
			}
		}
		return "", false
	}

	for _, tc := range []struct {
		name string
		out  string
		// want maps a section heading to the labels that section must list.
		want map[string]string
		// absent names sections that must not appear at all.
		absent []string
		// plain is asserted against the whole message, for the states that have
		// no sections.
		plain string
		// header pins the two counts the message opens with. They are the evidence
		// for every claim made about this gate — the argument that an exact count
		// still misses a substitution is entirely these two numbers — and nothing
		// asserted them: swapping the two arguments, or deleting the header
		// outright, left all ten cases green.
		//
		// Deleting the header reddens all five erroring cases, and so does reporting
		// ten too many arms — the shape a substring match let through, since
		// "2 arms ran, 3 pinned" is a substring of "12 arms ran, 3 pinned". The whole
		// first line is compared, not a fragment of it. SWAPPING the two
		// counts reddens three, not five, and the two it cannot reach are the ones
		// where the counts are EQUAL — 3 ran against 3 pinned, where a swap is the
		// identity. That is a property of the fixtures, not a hole to be closed by
		// assertion, and it is written down so the three is not read as a shortfall.
		header string
	}{
		// The empty-set reading, and the reason this gate exists: a suite that
		// produced nothing must not be indistinguishable from one that passed.
		{name: "no output at all", out: "", plain: "reported NO passing arms", absent: []string{"MISSING", "UNEXPECTED", "DUPLICATED"}},
		{name: "output but no arms", out: "\nALL OK\n", plain: "reported NO passing arms"},
		{name: "an arm disappeared", out: out("a", "b"), want: map[string]string{"MISSING": "[c]"}, absent: []string{"UNEXPECTED", "DUPLICATED"}, header: "2 arms ran, 3 pinned"},
		{name: "an arm was added", out: out("a", "b", "c", "d"), want: map[string]string{"UNEXPECTED": "[d]"}, absent: []string{"MISSING", "DUPLICATED"}, header: "4 arms ran, 3 pinned"},
		// The case a COUNT cannot see, and the case a whole-message match cannot
		// see either: one arm skipped, another added, both sections populated, so
		// only a per-section assertion rejects the inverted report.
		{
			name: "a same-count substitution", out: out("a", "b", "d"),
			want: map[string]string{"MISSING": "[c]", "UNEXPECTED": "[d]"}, absent: []string{"DUPLICATED"},
			header: "3 arms ran, 3 pinned",
		},
		{
			name: "a duplicate masking a deletion", out: out("a", "b", "b"),
			want: map[string]string{"MISSING": "[c]", "DUPLICATED": "[b]"}, absent: []string{"UNEXPECTED"},
			header: "3 arms ran, 3 pinned",
		},
		// A duplicate with NOTHING missing: the population is complete and one
		// label still ran twice, so a copy-pasted case reusing a name is caught on
		// its own rather than as a side effect of some other arm vanishing.
		{
			name: "a duplicate alongside the full population", out: out("a", "a", "b", "c"),
			want: map[string]string{"DUPLICATED": "[a]"}, absent: []string{"MISSING", "UNEXPECTED"},
			header: "4 arms ran, 3 pinned",
		},
		{name: "exactly the pinned population", out: out("a", "b", "c")},
		{name: "order does not matter", out: out("c", "a", "b")},
		// `ok` in prose is not an arm. The scan keys on the suite's own two-space
		// prefix and three-space gap, so a line merely containing "ok" cannot
		// inflate the census into passing.
		{name: "prose mentioning ok is not an arm", out: out("a", "b", "c") + "looks ok to me\n  ok but not an arm\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkArms(tc.out, pinned)
			if len(tc.want) == 0 && tc.plain == "" {
				if err != nil {
					t.Fatalf("checkArms(%v) = %v, want nil", armLabels(tc.out), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkArms(%v) = nil, want an error", armLabels(tc.out))
			}
			msg := err.Error()
			// The WHOLE first line, not a substring of it. `Contains` on "2 arms ran,
			// 3 pinned" also matches "12 arms ran, 3 pinned", so a regression adding ten
			// to the reported count satisfies every one of these assertions — a count
			// pinned by a substring is not pinned.
			if tc.header != "" {
				want := "rowdiff_watcher_suite.sh arm population differs: " + tc.header + "."
				if got, _, _ := strings.Cut(msg, "\n"); got != want {
					t.Errorf("checkArms header = %q, want %q", got, want)
				}
			}
			if tc.plain != "" && !strings.Contains(msg, tc.plain) {
				t.Errorf("checkArms error = %q, want it to contain %q", msg, tc.plain)
			}
			for name, labels := range tc.want {
				line, ok := section(msg, name)
				if !ok {
					t.Errorf("checkArms error = %q, want a %s section", msg, name)
					continue
				}
				if !strings.Contains(line, labels) {
					t.Errorf("%s section = %q, want it to list %s", name, line, labels)
				}
			}
			for _, name := range tc.absent {
				if line, ok := section(msg, name); ok {
					t.Errorf("checkArms error has an unwanted %s section: %q", name, line)
				}
			}
		})
	}
}
