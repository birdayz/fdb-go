package docscheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The unreachable-window gate.
//
// nightly_window_gate_test.go proves the heartbeat WIRING exists: every windowed
// job publishes a heartbeat and the reconciler reads it. That is necessary and it
// is not sufficient — a job can carry a perfectly wired heartbeat and still never
// write one, because its window never admits the hour its runner is handed to it.
// Then the workflow is green, the wiring check is green, and the reconciler
// truthfully reports a net that has never run. That is exactly what happened:
//
//	Measured 2026-08-01 over the artifact API. Of the nine nets, three —
//	fuzz-diff, fuzz-binding, fuzz-engine — had NO nightly-heartbeat artifact of
//	any age, ever. Their heartbeat steps were byte-identical to the two healthy
//	fuzz lanes beside them in the same file. Nothing was wrong with the lanes.
//	The 00:00-10:00 band was wrong: on 2026-07-30 the queue handed nightly-fuzz
//	its single runner at 20:22, 21:12, 23:32, 00:05 and 01:24 UTC, and the three
//	jobs that landed BEFORE midnight were discarded as "daytime" while the two
//	that landed after it ran.
//
// The window is a wall-clock predicate over a value nobody controls: the hour the
// self-hosted queue finally frees the runner. The only honest way to size such a
// predicate is against the values it has actually been given. So this test holds
// the measurement — every job-allocation hour observed over each net's scheduled
// history — and replays it against the band the workflow declares. A band that
// rejects an hour the queue has really produced is a night the net will silently
// not run, and that is a build failure here rather than a discovery six weeks
// later via the reconciler.
//
// This is also why the bands are declared as WINDOW_START/WINDOW_END shell
// variables rather than inlined into the comparison: the predicate has to be
// readable by something other than the runner that executes it.

// windowBandRe pulls the two declared hours out of a gate script.
var (
	windowStartRe = regexp.MustCompile(`(?m)^\s*WINDOW_START=(\d+)\s*$`)
	windowEndRe   = regexp.MustCompile(`(?m)^\s*WINDOW_END=(\d+)\s*$`)
)

// band is a permitted allocation window in whole UTC hours, half-open [start,end).
// start > end means the band wraps midnight; start == end is rejected as
// unreadable rather than guessed at.
type band struct{ start, end int }

func (b band) admits(hour int) bool {
	if b.start < b.end {
		return hour >= b.start && hour < b.end
	}
	return hour >= b.start || hour < b.end
}

func (b band) String() string { return fmt.Sprintf("%02d:00-%02d:00", b.start, b.end) }

// measuredLandings is the observed input to every window gate: the UTC hours at
// which the self-hosted queue has actually allocated a runner to that job, over
// its whole scheduled history.
//
// Sourced 2026-08-01 from the Actions API (`/repos/OWNER/REPO/actions/runs/N/jobs`,
// field started_at, restricted to event=schedule) across 39 nightly-fuzz runs, 47
// nightly-stress, 12 nightly-rowdiff, 109 nightly-coverage and 4 nightly-oracles.
// Where a job has been renamed over its life the hours are the union across names,
// because the YAML job key — which is what this test matches on — did not change.
//
// These are FACTS, not a policy. Widening a band to admit a newly observed hour is
// the correct response to this test going red; deleting the observation is not.
var measuredLandings = map[string][]int{
	"nightly-fuzz.yml/diff-fuzz":      {5, 6, 7, 8, 9, 10, 23},
	"nightly-fuzz.yml/race-detector":  {0, 6, 7, 8, 9, 10},
	"nightly-fuzz.yml/binding-stress": {5, 6, 7, 8, 9, 10, 20, 21},
	"nightly-fuzz.yml/client-fuzz":    {1, 6, 7, 8, 9, 10},
	"nightly-fuzz.yml/engine-fuzz":    {6, 7, 8, 10, 20},
	"nightly-stress.yml/stress":       {0, 6, 7, 8, 9, 10},
	"nightly-rowdiff.yml/rowdiff":     {3, 4, 20},
	"nightly-coverage.yml/coverage":   {4, 5, 6, 7, 8, 22},
	"nightly-oracles.yml/oracles":     {15, 22},

	// The two factory jobs are NEW (they arrived with the generation factory) and
	// have no scheduled history of their own yet. Their hours are POOLED from every
	// other entry above rather than observed on them, and that substitution is
	// legitimate for exactly one reason, stated so nobody has to guess: all eleven
	// window-gated jobs run on `hetzner-fdb`, a SINGLE self-hosted slot. The
	// allocation hour is a property of that one queue — of when it frees up — not of
	// what a job does once it has the runner. Pooling the queue's observed hours is
	// therefore a measurement of the thing that actually decides the landing.
	//
	// The oracles' 15 is deliberately EXCLUDED: that net keeps a distinct afternoon
	// slot, so its hour is evidence about a different band, not about this queue's
	// nightly drain.
	//
	// These entries are weaker evidence than the rest of this map and are marked so.
	// Replace each with the job's OWN started_at hours once it has scheduled runs;
	// until then a pooled prior is what honestly exists, and the alternative — an
	// unmeasured band, or a fabricated one — is what this map exists to forbid.
	"nightly-factory.yml/corpus": {0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 20, 21, 22, 23},
	"nightly-factory.yml/batch":  {0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 20, 21, 22, 23},
}

// gateScript is one window gate's shell, kept alongside the band it declares so
// the two can be checked against each other.
type gateScript struct {
	key    string
	script string
	band   band
}

// declaredBands is collectGates keyed by job for the landing replay.
func declaredBands(t *testing.T, root string) map[string]band {
	t.Helper()
	bands := map[string]band{}
	for _, g := range collectGates(t, root) {
		bands[g.key] = g.band
	}
	return bands
}

// collectGates reads the WINDOW_START/WINDOW_END pair, and the surrounding shell,
// out of every window-gated job. It keys on "<file>/<job-key>" so a job rename in
// the display `name:` cannot silently orphan a measurement.
func collectGates(t *testing.T, root string) []gateScript {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var gates []gateScript
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") || e.Name() == reconcileWorkflow {
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
			for _, step := range job.Steps {
				// Same identification as the wiring gate: a step that can write
				// ok=false is a window gate whatever it is called.
				if !strings.Contains(step.Run, "ok=false") {
					continue
				}
				key := e.Name() + "/" + jobName
				ms, me := windowStartRe.FindStringSubmatch(step.Run), windowEndRe.FindStringSubmatch(step.Run)
				if ms == nil || me == nil {
					t.Errorf("%s: the window gate does not declare WINDOW_START and WINDOW_END as their own lines.\n"+
						"The permitted band has to be readable by something other than the runner executing it — otherwise nothing can check that the band admits the hours the queue actually produces, which is how three nets ran zero times.\n"+
						"Declare the two hours as shell variables and compare against them.", key)
					continue
				}
				start, _ := strconv.Atoi(ms[1])
				end, _ := strconv.Atoi(me[1])
				if start > 23 || end > 24 {
					t.Errorf("%s: window %d..%d is not a pair of UTC hours", key, start, end)
					continue
				}
				if start == end {
					t.Errorf("%s: WINDOW_START == WINDOW_END == %d — that is either an empty band or a whole day, and the gate must not have to guess which", key, start)
					continue
				}
				gates = append(gates, gateScript{key: key, script: step.Run, band: band{start: start, end: end}})
			}
		}
	}
	sort.Slice(gates, func(i, j int) bool { return gates[i].key < gates[j].key })
	return gates
}

func TestNightlyWindowAdmitsMeasuredLandings(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	bands := declaredBands(t, root)

	// NO-OP GUARD, the same discipline the reconciler and the wiring gate carry: a
	// parse that silently found nothing would pass this test having replayed no
	// measurement at all. The count is the nine window-gated nightly jobs.
	const knownWindowedJobs = 11
	if len(bands) < knownWindowedJobs {
		t.Fatalf("parsed only %d window bands, expected at least %d — the workflow parse is broken and every assertion below would pass vacuously; parsed: %v",
			len(bands), knownWindowedJobs, bands)
	}

	// Every gate must have a measurement. An unmeasured band is an unfalsifiable
	// one: it can be arbitrarily narrow and nothing here would notice.
	var keys []string
	for k := range bands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		hours, ok := measuredLandings[key]
		if !ok {
			t.Errorf("%s declares window %s but has no entry in measuredLandings.\n"+
				"A band nothing replays against is unfalsifiable — it can be narrowed to nothing and this test stays green.\n"+
				"Measure it: list the job's scheduled runs and take started_at from /repos/OWNER/REPO/actions/runs/<id>/jobs, then add the observed UTC hours here.", key, bands[key])
			continue
		}
		if len(hours) == 0 {
			t.Errorf("%s has an EMPTY measuredLandings entry — an empty replay proves nothing", key)
			continue
		}
		var rejected []int
		for _, h := range hours {
			if !bands[key].admits(h) {
				rejected = append(rejected, h)
			}
		}
		if len(rejected) > 0 {
			t.Errorf("%s declares window %s, which REJECTS allocation hour(s) %v that the queue has actually produced for it.\n"+
				"Every one of those is a night this net exits SUCCESS having run nothing: the gate sets ok=false, every real step is if:-skipped, and no heartbeat is written.\n"+
				"Observed hours: %v.\n"+
				"Widen the band to admit them — WINDOW_START > WINDOW_END wraps midnight, which is what an evening landing needs. Do not delete the observation.",
				key, bands[key], rejected, hours)
		}
	}

	// Reverse direction: a measurement whose job is gone would sit here asserting
	// nothing while reading like coverage.
	for key := range measuredLandings {
		if _, ok := bands[key]; !ok {
			t.Errorf("measuredLandings has an entry for %q but no window-gated job by that name exists.\n"+
				"Either the job was renamed (move the measurement to the new key) or it lost its window gate (drop the row).", key)
		}
	}
}

// ghExprRe strips the `${{ ... }}` expressions Actions substitutes before bash
// ever sees the script. Only github.event_name matters to the gate's control
// flow, and it is pinned to `schedule` below — the manual-dispatch path exits
// early by design and is not what silently skips nights.
var ghExprRe = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// TestNightlyWindowGateShellMatchesDeclaredBand runs each gate's REAL shell for
// all 24 hours and checks the ok= it writes against the band it declares.
//
// Without this, the landing replay above only ever checks a Go reimplementation of
// the window predicate against a table — and the thing that decides whether a
// safety net runs tonight is the bash. The two must agree or the declaration is
// decoration. The wrap branch is the specific reason this matters: `[ "$HOUR" -ge
// "$WINDOW_START" ] || [ "$HOUR" -lt "$WINDOW_END" ]` is one keystroke from `&&`,
// which yields a band that admits nothing and reads exactly the same.
func TestNightlyWindowGateShellMatchesDeclaredBand(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	gates := collectGates(t, root)
	const knownWindowedJobs = 11
	if len(gates) < knownWindowedJobs {
		t.Fatalf("collected only %d window gates, expected at least %d — the parse is broken and this test would prove nothing", len(gates), knownWindowedJobs)
	}

	for _, g := range gates {
		t.Run(strings.ReplaceAll(g.key, "/", "_"), func(t *testing.T) {
			t.Parallel()
			for hour := 0; hour < 24; hour++ {
				got := runGate(t, ghExprRe.ReplaceAllString(g.script, "schedule"), hour)
				want := g.band.admits(hour)
				if got != want {
					t.Errorf("%s: at allocation hour %02d:00 the gate shell writes ok=%v but its declared band %s says %v.\n"+
						"The shell is what decides whether the net runs; the declaration is what everything else reads. A disagreement means one of them is lying, and the reconciler will only find out after the net has been silent for days.",
						g.key, hour, got, g.band, want)
				}
			}
		})
	}
}

// runGate executes one gate script the way Actions does (bash -e, $GITHUB_OUTPUT
// and $GITHUB_ENV as append-only files), with `date` stubbed so the allocation
// hour is the input rather than whenever the suite happens to run. It returns the
// ok= the gate wrote.
func runGate(t *testing.T, script string, hour int) bool {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "output")
	envPath := filepath.Join(dir, "env")
	for _, p := range []string{outPath, envPath} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", p, err)
		}
	}

	// The stub answers the two forms the gates use: the allocation hour, and the
	// epoch seconds the fuzz budget model anchors to.
	preamble := fmt.Sprintf(`date() {
  for a in "$@"; do
    case "$a" in
      +%%-H) echo %d; return 0;;
      +%%s) echo 1700000000; return 0;;
    esac
  done
  echo 1700000000
}
`, hour)

	cmd := exec.Command("bash", "-e", "-c", preamble+script)
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+outPath, "GITHUB_ENV="+envPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gate script failed at hour %02d: %v\n%s", hour, err, out)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading gate output: %v", err)
	}
	var ok string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "ok="); found {
			ok = v
		}
	}
	switch ok {
	case "true":
		return true
	case "false":
		return false
	default:
		t.Fatalf("gate wrote no usable ok= at hour %02d; $GITHUB_OUTPUT was:\n%s\n"+
			"A gate that writes neither leaves every downstream `if:` false — the whole job skips and exits success, which is the failure this file exists to make impossible.", hour, raw)
		return false
	}
}
