package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// scriptedRunner returns a runner that yields the given outcomes in order and
// records how many times it was called.
func scriptedRunner(outcomes ...runResult) (func() runResult, *int) {
	calls := 0
	return func() runResult {
		i := calls
		calls++
		if i >= len(outcomes) {
			panic("fuzzrun called the command more times than the test scripted")
		}
		return outcomes[i]
	}, &calls
}

func pass() runResult {
	return runResult{output: "fuzz: elapsed: 2m0s, execs: 5601233\nPASS\nok\n", exitCode: 0}
}

func raceFailure() runResult {
	return runResult{output: nightlyGetValueReply, exitCode: 1}
}

func TestGate_PassRunsOnce(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(pass())
	var sb strings.Builder
	code, _ := gate("FuzzFoo", &sb, 0, run)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if *calls != 1 {
		t.Errorf("ran command %d times, want 1", *calls)
	}
}

// The bug this tool exists for: budget expiry leaked as a failure, retry is clean,
// job stays green.
func TestGate_DeadlineRaceRetriesAndPasses(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(raceFailure(), pass())
	var sb strings.Builder
	code, _ := gate("FuzzGetValueReply", &sb, 0, run)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if *calls != 2 {
		t.Errorf("ran command %d times, want 2", *calls)
	}
	if !strings.Contains(sb.String(), "::warning::") {
		t.Error("retry was silent; it must be surfaced so a frequency change is visible")
	}
}

// A real finding must fail on the FIRST run — never burn a second budget, and never
// risk the retry masking it.
func TestGate_RealFindingFailsWithoutRetry(t *testing.T) {
	t.Parallel()
	crasher := runResult{
		output: `fuzz: elapsed: 12s, execs: 40122 (3343/sec), new interesting: 2 (total: 18)
--- FAIL: FuzzGetValueReply (12.01s)
    context deadline exceeded
Failing input written to testdata/fuzz/FuzzGetValueReply/deadbeef
`,
		exitCode: 1,
	}
	run, calls := scriptedRunner(crasher)
	var sb strings.Builder
	code, _ := gate("FuzzGetValueReply", &sb, 0, run)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 1 {
		t.Errorf("ran command %d times, want 1 (a real finding must not be retried)", *calls)
	}
	if !strings.Contains(sb.String(), "::error::") {
		t.Error("real finding did not emit a GitHub error annotation")
	}
}

// The safety net behind the classifier: if the race signature somehow accompanied a
// real bug, the retry still fails and the job still goes red.
func TestGate_DeadlineRaceThatRepeatsStillFails(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(raceFailure(), raceFailure())
	var sb strings.Builder
	code, _ := gate("FuzzGetValueReply", &sb, 0, run)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 2 {
		t.Errorf("ran command %d times, want 2", *calls)
	}
	if !strings.Contains(sb.String(), "failed again on retry") {
		t.Error("second failure was not reported as such")
	}
}

// Past the job's wall-clock budget the retry is skipped — but the run is still
// accepted, because the CLASSIFIER is authoritative and has already identified the
// stdlib race. Going red here would re-introduce the false red this tool exists to
// remove, and would do it exactly when the race is most frequent.
func TestGate_PastDeadlineSkipsRetryButStillPasses(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(raceFailure())
	var sb strings.Builder
	past := time.Now().Add(-time.Minute).Unix()
	code, _ := gate("FuzzGetValueReply", &sb, past, run)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if *calls != 1 {
		t.Errorf("ran command %d times, want 1 (retry deadline had passed)", *calls)
	}
	if !strings.Contains(sb.String(), "::warning::") {
		t.Error("skipping the confirming retry must still be surfaced")
	}
}

// The deadline must NOT rescue a real failure: it only ever skips the confirming
// retry for a run the classifier already cleared.
func TestGate_PastDeadlineStillFailsARealFinding(t *testing.T) {
	t.Parallel()
	crasher := runResult{
		output: `fuzz: elapsed: 1m27s, execs: 2210544 (25000/sec), new interesting: 3 (total: 41)
fuzz: minimizing 37-byte failing input file
--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
`,
		exitCode: 1,
	}
	run, calls := scriptedRunner(crasher)
	var sb strings.Builder
	past := time.Now().Add(-time.Minute).Unix()
	code, _ := gate("FuzzRYWCache", &sb, past, run)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 1 {
		t.Errorf("ran command %d times, want 1", *calls)
	}
	if !strings.Contains(sb.String(), "::error::") {
		t.Error("real finding did not emit an error annotation")
	}
}

func TestGate_RetryAllowedBeforeDeadline(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(raceFailure(), pass())
	var sb strings.Builder
	future := time.Now().Add(time.Hour).Unix()
	code, _ := gate("FuzzGetValueReply", &sb, future, run)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if *calls != 2 {
		t.Errorf("ran command %d times, want 2", *calls)
	}
}

// A command that could not be spawned must fail loudly with a reason, not an
// empty-bodied error annotation.
func TestGate_SpawnFailureIsReported(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(runResult{exitCode: -1, err: errFakeSpawn})
	var sb strings.Builder
	code, _ := gate("FuzzFoo", &sb, 0, run)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 1 {
		t.Errorf("ran command %d times, want 1", *calls)
	}
	if !strings.Contains(sb.String(), errFakeSpawn.Error()) {
		t.Errorf("spawn error not surfaced; got %q", sb.String())
	}
}

var errFakeSpawn = errSpawn("running \"nope\": executable file not found in $PATH")

type errSpawn string

func (e errSpawn) Error() string { return string(e) }

// The `raced` return drives the run-wide tally, so it must be true for exactly the
// cases the classifier cleared as the stdlib race — and false for a real finding,
// which would otherwise inflate the tally and mask a systematic race.
func TestGate_RacedSignal(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		outcomes  []runResult
		wantRaced bool
	}{
		"clean pass":        {[]runResult{pass()}, false},
		"race, retry clean": {[]runResult{raceFailure(), pass()}, true},
		// Counted even though the job goes red: the target DID exhibit the race, which
		// is what -racelog documents. Under a systematic regression this becomes the
		// DOMINANT case, so excluding it would drop the one summary line that explains
		// a wall of per-target failures.
		"race, retry raced again": {[]runResult{raceFailure(), raceFailure()}, true},
		"real finding":            {[]runResult{{output: "boom\n", exitCode: 1}}, false},
	}
	for name, tc := range cases {
		run, _ := scriptedRunner(tc.outcomes...)
		var sb strings.Builder
		_, raced := gate("FuzzFoo", &sb, 0, run)
		if raced != tc.wantRaced {
			t.Errorf("%s: raced = %v, want %v", name, raced, tc.wantRaced)
		}
	}
}

func TestNoteRace_AppendsLabels(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/races"
	noteRace(path, "FuzzA")
	noteRace(path, "FuzzB")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading race log: %v", err)
	}
	if got := string(b); got != "FuzzA\nFuzzB\n" {
		t.Errorf("race log = %q, want %q", got, "FuzzA\nFuzzB\n")
	}
	// An unset -racelog must be a no-op, not a crash.
	noteRace("", "FuzzC")
}
