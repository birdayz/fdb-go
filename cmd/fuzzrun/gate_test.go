package main

import (
	"strings"
	"testing"
)

// scriptedRunner returns a runner that yields the given outcomes in order and
// records how many times it was called.
func scriptedRunner(outcomes ...struct {
	out string
	ok  bool
},
) (func() (string, bool), *int) {
	calls := 0
	return func() (string, bool) {
		i := calls
		calls++
		if i >= len(outcomes) {
			panic("fuzzrun called the command more times than the test scripted")
		}
		return outcomes[i].out, outcomes[i].ok
	}, &calls
}

type outcome = struct {
	out string
	ok  bool
}

func TestGate_PassRunsOnce(t *testing.T) {
	t.Parallel()
	run, calls := scriptedRunner(outcome{"PASS\nok\n", true})
	var sb strings.Builder
	if code := gate("FuzzFoo", &sb, run); code != 0 {
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
	run, calls := scriptedRunner(
		outcome{nightlyGetValueReply, false},
		outcome{"fuzz: elapsed: 2m0s, execs: 5601233\nPASS\nok\n", true},
	)
	var sb strings.Builder
	if code := gate("FuzzGetValueReply", &sb, run); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if *calls != 2 {
		t.Errorf("ran command %d times, want 2", *calls)
	}
	if !strings.Contains(sb.String(), "::warning::") {
		t.Error("retry was silent; it must be surfaced so a frequency change is visible")
	}
}

// A real finding must fail on the FIRST run — never burn a second 2-minute budget,
// and never risk the retry masking it.
func TestGate_RealFindingFailsWithoutRetry(t *testing.T) {
	t.Parallel()
	crasher := "--- FAIL: FuzzGetValueReply\nFailing input written to testdata/fuzz/FuzzGetValueReply/deadbeef\n"
	run, calls := scriptedRunner(outcome{crasher, false})
	var sb strings.Builder
	if code := gate("FuzzGetValueReply", &sb, run); code != 1 {
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
	run, calls := scriptedRunner(
		outcome{nightlyGetValueReply, false},
		outcome{nightlyGetValueReply, false},
	)
	var sb strings.Builder
	if code := gate("FuzzGetValueReply", &sb, run); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if *calls != 2 {
		t.Errorf("ran command %d times, want 2", *calls)
	}
	if !strings.Contains(sb.String(), "failed again on retry") {
		t.Error("second failure was not reported as such")
	}
}
