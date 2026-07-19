package main

import "testing"

// nightlyGetValueReply is the tail of the real 2026-07-14 nightly failure
// (run 29311279420, "Differential Serialization Fuzzer" job), verbatim. The run
// executed 5,650,945 inputs, found nothing new, wrote no crasher — and was still
// reported as a failure.
const nightlyGetValueReply = `fuzz: elapsed: 1m57s, execs: 5507929 (46817/sec), new interesting: 0 (total: 16)
fuzz: elapsed: 2m0s, execs: 5650945 (47679/sec), new interesting: 0 (total: 16)
fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
FAIL
exit status 1
FAIL	fdb.dev/cmd/fdb-diff-oracle	120.091s
`

// nightlyEndpoint is the real 2026-07-11 nightly failure (run 29142824495) — a
// DIFFERENT fuzz target, byte-identical signature. This pair is what proves the
// failure is target-agnostic rather than a GetValueReply wire divergence.
const nightlyEndpoint = `fuzz: elapsed: 2m0s, execs: 5194411 (0/sec), new interesting: 0 (total: 12)
--- FAIL: FuzzEndpoint (120.09s)
    context deadline exceeded
FAIL
exit status 1
FAIL	fdb.dev/cmd/fdb-diff-oracle	120.093s
`

func TestClassify_NightlyDeadlineRaceIsRetryable(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"FuzzGetValueReply 2026-07-14": nightlyGetValueReply,
		"FuzzEndpoint 2026-07-11":      nightlyEndpoint,
	} {
		if got := classify(out); got != verdictDeadlineRace {
			t.Errorf("%s: classify = %v, want %v", name, got, verdictDeadlineRace)
		}
	}
}

// A real finding must NEVER be classified as retryable. Each case below pairs the
// deadline text with a genuine failure signal, because the dangerous direction is a
// crasher that happens to coincide with budget expiry.
func TestClassify_RealFindingsAreNeverRetried(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"crasher written": `--- FAIL: FuzzGetValueReply (12.01s)
    fuzzing process hung or terminated unexpectedly: exit status 2
Failing input written to testdata/fuzz/FuzzGetValueReply/a1b2c3
To re-run:
go test -run=FuzzGetValueReply/a1b2c3
context deadline exceeded
`,
		"structural mismatch": `--- FAIL: FuzzGetValueReply (3.00s)
    fuzz_test.go:1585: GetValueReply: STRUCTURAL MISMATCH
          Go:  {Penalty:0 HasValue:true Value:[1]}
          C++: {Penalty:0 HasValue:true Value:[]}
context deadline exceeded
`,
		"oracle died": `--- FAIL: FuzzGetValueReply (0.40s)
    fuzz_test.go:569: oracle error: read response length: EOF
context deadline exceeded
`,
		"panic": `panic: runtime error: index out of range [8] with length 4
context deadline exceeded
`,
		"data race": `WARNING: DATA RACE
Write at 0x00c000123456 by goroutine 12:
context deadline exceeded
`,
		"unrelated failure without deadline text": `--- FAIL: FuzzGetValueReply (0.01s)
    fuzz_test.go:41: oracle not available
FAIL
`,
	}
	for name, out := range cases {
		if got := classify(out); got != verdictRealFailure {
			t.Errorf("%s: classify = %v, want %v", name, got, verdictRealFailure)
		}
	}
}

// The signature must be matched on its own terms, not by target name — the whole
// point is that any target can hit it.
func TestClassify_IsTargetAgnostic(t *testing.T) {
	t.Parallel()
	out := `--- FAIL: FuzzSomeTargetThatDoesNotExistYet (90.02s)
    context deadline exceeded
FAIL
`
	if got := classify(out); got != verdictDeadlineRace {
		t.Errorf("classify = %v, want %v", got, verdictDeadlineRace)
	}
}
