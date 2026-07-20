package main

import (
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
	if code := gate("FuzzFoo", &sb, 0, run); code != 0 {
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
	if code := gate("FuzzGetValueReply", &sb, 0, run); code != 0 {
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
	if code := gate("FuzzGetValueReply", &sb, 0, run); code != 1 {
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
	if code := gate("FuzzGetValueReply", &sb, 0, run); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 2 {
		t.Errorf("ran command %d times, want 2", *calls)
	}
	if !strings.Contains(sb.String(), "failed again on retry") {
		t.Error("second failure was not reported as such")
	}
}

// A retry must not be started past the job's wall-clock budget: doubling every
// target would otherwise blow timeout-minutes and kill the job mid-run.
func TestGate_RetryRefusedPastDeadline(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(raceFailure())
	var sb strings.Builder
	past := time.Now().Add(-time.Minute).Unix()
	if code := gate("FuzzGetValueReply", &sb, past, run); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 1 {
		t.Errorf("ran command %d times, want 1 (retry deadline had passed)", *calls)
	}
	if !strings.Contains(sb.String(), "retry deadline has passed") {
		t.Error("refusal was not explained in the log")
	}
}

func TestGate_RetryAllowedBeforeDeadline(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(raceFailure(), pass())
	var sb strings.Builder
	future := time.Now().Add(time.Hour).Unix()
	if code := gate("FuzzGetValueReply", &sb, future, run); code != 0 {
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
	if code := gate("FuzzFoo", &sb, 0, run); code != 1 {
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
