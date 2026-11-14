package main

import (
	"strings"
	"testing"
)

// test2json writes a fixture's ^V as the literal 0x16 framing byte testing emits.
func test2json(s string) string { return strings.ReplaceAll(s, "^V", "\x16") }

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

// bazelDeadlineRace is the same race as it surfaces through `bazelisk test`, which
// is how 124 of the 142 nightly (target, func) pairs run. Bazel frames the captured
// test log with its own lines and exits 3, not 1.
//
// This is the plain -test.v shape; bazelFramedDeadlineRace is what GO_TEST_WRAP_TESTV produces.
const bazelDeadlineRace = `==================== Test output for //pkg/fdbgo/client:client_test:
=== RUN   FuzzRYWCache
fuzz: elapsed: 1m27s, execs: 2210544 (25000/sec), new interesting: 0 (total: 41)
fuzz: elapsed: 1m30s, execs: 2284412 (0/sec), new interesting: 0 (total: 41)
--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
=== NAME  
FAIL
================================================================================
FAILED: //pkg/fdbgo/client:client_test (Exit 1) (see /home/x/.cache/bazel/.../test.log)
//pkg/fdbgo/client:client_test                                           FAILED in 95.0s

Executed 1 out of 1 test: 1 fails locally.
`

// bazelFramedDeadlineRace is bazelDeadlineRace under .bazelrc's GO_TEST_WRAP_TESTV=1:
// testing's -test.v=test2json frames each status line with ^V (testing.go markFraming).
var bazelFramedDeadlineRace = test2json(`==================== Test output for //pkg/fdbgo/client:client_test:
^V=== RUN   FuzzRYWCache
fuzz: elapsed: 1m27s, execs: 2210544 (25000/sec), new interesting: 0 (total: 41)
fuzz: elapsed: 1m30s, execs: 2284412 (0/sec), new interesting: 0 (total: 41)
^V--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
` + "^V=== NAME  \n" + `^VFAIL
================================================================================
FAILED: //pkg/fdbgo/client:client_test (Exit 1) (see /home/x/.cache/bazel/.../test.log)
//pkg/fdbgo/client:client_test                                           FAILED in 95.0s

Executed 1 out of 1 test: 1 fails locally.
`)

func TestClassify_DeadlineRaceIsRetryable(t *testing.T) {
	t.Parallel()
	cases := map[string]runResult{
		"FuzzGetValueReply 2026-07-14": {output: nightlyGetValueReply, exitCode: 1},
		"FuzzEndpoint 2026-07-11":      {output: nightlyEndpoint, exitCode: 1},
		"bazel framing, exit 3":        {output: bazelDeadlineRace, exitCode: 3},
		"test2json framing, exit 3":    {output: bazelFramedDeadlineRace, exitCode: 3},
		"test2json framing, go test exit 1": {output: test2json(`fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
^V--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
^VFAIL
exit status 1
FAIL	fdb.dev/cmd/fdb-diff-oracle	120.091s
`), exitCode: 1},
		// Every framed status form chattyPrinter writes, including an indented nested result.
		"test2json framing of other tests' status lines": {output: test2json(`^V=== RUN   TestSetup
^V=== PAUSE TestSetup
^V=== CONT  TestSetup
^V    --- PASS: TestSetup/sub (0.00s)
^V--- PASS: TestSetup (0.00s)
^V=== RUN   TestOptional
^V--- SKIP: TestOptional (0.00s)
^V=== RUN   FuzzRYWCache
fuzz: elapsed: 1m30s, execs: 2284412 (0/sec), new interesting: 0 (total: 41)
^V--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
^VFAIL
`), exitCode: 3},
		"test2json framing with CRLF line ends": {output: test2json("fuzz: elapsed: 1m30s, execs: 2284412 (0/sec), new interesting: 0 (total: 41)\r\n" +
			"^V--- FAIL: FuzzRYWCache (90.02s)\r\n    context deadline exceeded\r\n^VFAIL\r\n"), exitCode: 3},
	}
	for name, r := range cases {
		if got := classify(r); got != verdictDeadlineRace {
			t.Errorf("%s: classify = %v, want %v", name, got, verdictDeadlineRace)
		}
	}
}

// THE DANGEROUS DIRECTION. Every case below contains "context deadline exceeded"
// and NO crasher marker — the exact shape a denylist-based classifier would retry
// into green. Each must be a real failure.
func TestClassify_DeadlineTextWithoutTheBudgetShapeIsNeverRetried(t *testing.T) {
	t.Parallel()
	cases := map[string]runResult{
		// The one that motivated the rewrite: every package hosting a fuzz target in
		// this repo also starts an FDB testcontainer under a context.WithTimeout
		// (CLAUDE.md mandates it). A slow Docker start dies before fuzzing begins,
		// printing the identical error string with no fuzz output at all.
		// See pkg/fdbgo/client/testmain_test.go TestMain.
		"container start timed out before fuzzing": {
			output:   "start FDB container: context deadline exceeded\n",
			exitCode: 1,
		},
		// A seed-corpus entry failing during warmup is reported via
		// stop(errors.New(crasherMsg)), NOT a crashError, so Go prints no
		// "Failing input written to" line ($GOROOT/src/internal/fuzz/fuzz.go:245).
		"seed corpus entry failed": {
			output: `fuzz: elapsed: 0s, gathering baseline coverage: 3/16 completed
failure while testing seed corpus entry: FuzzGetValueReply/seed#2
--- FAIL: FuzzGetValueReply (0.03s)
    context deadline exceeded
`,
			exitCode: 1,
		},
		// `timeout 150` killed the run. Budget expiry never looks like this.
		"killed by outer timeout": {
			output:   nightlyGetValueReply,
			exitCode: 124,
		},
		"killed by signal (OOM)": {
			output:   nightlyGetValueReply,
			exitCode: -1,
		},
		"bazel build error": {
			output:   "ERROR: compilation failed\ncontext deadline exceeded\n",
			exitCode: 8,
		},
		// Fuzzing ran, but the target reported a second, real problem alongside.
		"extra detail line under the same failure": {
			output: `fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
--- FAIL: FuzzGetValueReply (120.08s)
    fuzz_test.go:1585: GetValueReply: STRUCTURAL MISMATCH
    context deadline exceeded
`,
			exitCode: 1,
		},
		// Two failures reported — the deadline is not the whole story.
		"a second target also failed": {
			output: `fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
--- FAIL: TestSomethingElse (0.01s)
    thing_test.go:12: broke
--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
`,
			exitCode: 1,
		},
		// THE WORST CASE, and the one a shape-only check cannot see. A crasher was
		// found and the deadline fired while it was being minimized. The deferred
		// writer at $GOROOT/src/internal/fuzz/fuzz.go:149-164 persists the crasher
		// but builds a crashError only `if err == nil`; under this race err is
		// DeadlineExceeded, so there is no "Failing input written to", no crasherMsg,
		// and no nested "--- FAIL" — byte-identical to a benign expiry while a real
		// finding sits on disk. Only the "fuzz: minimizing" witness distinguishes it.
		"crasher found, deadline fired during minimization": {
			output: `fuzz: elapsed: 1m27s, execs: 2210544 (25000/sec), new interesting: 3 (total: 41)
fuzz: minimizing 37-byte failing input file
fuzz: elapsed: 1m30s, minimizing
--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
`,
			exitCode: 1,
		},
		// Same finding, but the log has been truncated past the "fuzz: minimizing
		// N-byte" line so only the periodic stats line survives. This is the ONLY
		// case the minimizingStatsRE backstop exists for — without a fixture it
		// reads as redundant and gets deleted by a future cleanup.
		"minimization swallow, only the stats line survives": {
			output: `fuzz: elapsed: 1m30s, minimizing
--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
`,
			exitCode: 1,
		},
		// ...and through bazel's verbose framing at exit 3.
		"minimization swallow through bazel framing": {
			output: `==================== Test output for //pkg/fdbgo/client:client_test:
=== RUN   FuzzRYWCache
fuzz: elapsed: 1m27s, execs: 2210544 (25000/sec), new interesting: 3 (total: 41)
fuzz: minimizing 37-byte failing input file
--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
=== NAME  
FAIL
`,
			exitCode: 3,
		},
		// No fuzz progress at all: the process died before consuming any budget.
		"fuzz failure with no progress output": {
			output: `--- FAIL: FuzzGetValueReply (0.00s)
    context deadline exceeded
`,
			exitCode: 1,
		},
		"could not spawn the command": {
			output:   "",
			exitCode: -1,
		},
	}
	for name, r := range cases {
		if got := classify(r); got != verdictRealFailure {
			t.Errorf("%s: classify = %v, want %v", name, got, verdictRealFailure)
		}
	}
}

// Framed output gets the same scrutiny as plain output, and ^V anywhere but before a
// status line at a line start is unrecognised, so it is a real failure.
func TestClassify_Test2jsonFramingFailsClosed(t *testing.T) {
	t.Parallel()
	benign := strings.Split(bazelFramedDeadlineRace, "\n=====")[0] // drop bazel's closing banner
	if got := classify(runResult{output: benign, exitCode: 3}); got != verdictDeadlineRace {
		t.Fatalf("control: classify = %v, want %v", got, verdictDeadlineRace)
	}
	cases := map[string]string{
		"framed second failure": test2json(`fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
^V--- FAIL: TestSomethingElse (0.01s)
    thing_test.go:12: broke
^V--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
^VFAIL
`),
		"framed repeated fuzz failure": test2json(`fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
^V--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
^V--- FAIL: FuzzGetValueReply (120.08s)
    context deadline exceeded
`),
		"framed crasher found during minimization": test2json(`^V=== RUN   FuzzRYWCache
fuzz: elapsed: 1m27s, execs: 2210544 (25000/sec), new interesting: 3 (total: 41)
fuzz: minimizing 37-byte failing input file
^V--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
` + "^V=== NAME  \n" + `^VFAIL
`),
		"framed minimization stats line only": test2json(`fuzz: elapsed: 1m30s, minimizing
^V--- FAIL: FuzzRYWCache (90.02s)
    context deadline exceeded
`),
		"framed failure with no fuzz progress": test2json(`^V=== RUN   FuzzGetValueReply
^V--- FAIL: FuzzGetValueReply (0.00s)
    context deadline exceeded
^VFAIL
`),
		"framed extra detail line": test2json(`fuzz: elapsed: 2m0s, execs: 5650945 (0/sec), new interesting: 0 (total: 16)
^V--- FAIL: FuzzGetValueReply (120.08s)
    fuzz_test.go:1585: GetValueReply: STRUCTURAL MISMATCH
    context deadline exceeded
`),
		// These would read as benign if the ^V were dropped wherever it appears.
		"framed detail line":    strings.Replace(benign, "    context", "\x16    context", 1),
		"doubly framed header":  strings.Replace(benign, "\x16--- FAIL", "\x16\x16--- FAIL", 1),
		"framing inside a line": benign + "\nfuzz: elapsed: 1m31s, execs: 1 (0/sec)\x16--- FAIL: TestOther (0.01s)\n    other_test.go:9: broke",
		"escaped framing byte":  benign + "\n\x1b\x16--- FAIL: TestOther (0.01s)\n    other_test.go:9: broke",
	}
	for name, out := range cases {
		if got := classify(runResult{output: out, exitCode: 3}); got != verdictRealFailure {
			t.Errorf("%s: classify = %v, want %v", name, got, verdictRealFailure)
		}
	}
}

// A persisted crasher must never be retried, even though it can co-occur with the
// deadline text.
func TestClassify_RealFindingsAreNeverRetried(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"crasher written": `fuzz: elapsed: 12s, execs: 40122 (3343/sec), new interesting: 2 (total: 18)
--- FAIL: FuzzGetValueReply (12.01s)
    context deadline exceeded
Failing input written to testdata/fuzz/FuzzGetValueReply/a1b2c3
To re-run:
go test -run=FuzzGetValueReply/a1b2c3
`,
		"structural mismatch": `fuzz: elapsed: 3s, execs: 9001 (3000/sec), new interesting: 0 (total: 16)
--- FAIL: FuzzGetValueReply (3.00s)
    fuzz_test.go:1585: GetValueReply: STRUCTURAL MISMATCH
context deadline exceeded
`,
		"oracle died": `fuzz: elapsed: 0s, execs: 12 (12/sec), new interesting: 0 (total: 16)
--- FAIL: FuzzGetValueReply (0.40s)
    fuzz_test.go:569: oracle error: read response length: EOF
context deadline exceeded
`,
		"panic": `fuzz: elapsed: 1s, execs: 900 (900/sec), new interesting: 0 (total: 16)
panic: runtime error: index out of range [8] with length 4
context deadline exceeded
`,
		"data race": `fuzz: elapsed: 1s, execs: 900 (900/sec), new interesting: 0 (total: 16)
WARNING: DATA RACE
Write at 0x00c000123456 by goroutine 12:
context deadline exceeded
`,
		"unrelated failure without deadline text": `fuzz: elapsed: 1s, execs: 900 (900/sec), new interesting: 0 (total: 16)
--- FAIL: FuzzGetValueReply (0.01s)
    fuzz_test.go:41: oracle not available
FAIL
`,
	}
	for name, out := range cases {
		if got := classify(runResult{output: out, exitCode: 1}); got != verdictRealFailure {
			t.Errorf("%s: classify = %v, want %v", name, got, verdictRealFailure)
		}
	}
}

// The signature must be matched on its own terms, not by target name — the whole
// point is that any target can hit it.
func TestClassify_IsTargetAgnostic(t *testing.T) {
	t.Parallel()
	out := `fuzz: elapsed: 1m30s, execs: 2284412 (0/sec), new interesting: 0 (total: 3)
--- FAIL: FuzzSomeTargetThatDoesNotExistYet (90.02s)
    context deadline exceeded
FAIL
`
	if got := classify(runResult{output: out, exitCode: 1}); got != verdictDeadlineRace {
		t.Errorf("classify = %v, want %v", got, verdictDeadlineRace)
	}
}

func TestVerdictString(t *testing.T) {
	t.Parallel()
	if verdictRealFailure.String() != "real-failure" {
		t.Errorf("verdictRealFailure.String() = %q", verdictRealFailure.String())
	}
	if verdictDeadlineRace.String() != "deadline-race" {
		t.Errorf("verdictDeadlineRace.String() = %q", verdictDeadlineRace.String())
	}
}

// The minimization backstop must survive a CRLF-translated log: anchoring the regex
// is what made this reachable, so it gets pinned.
func TestClassify_MinimizationBackstopToleratesCRLF(t *testing.T) {
	t.Parallel()
	out := "fuzz: elapsed: 1m30s, minimizing\r\n" +
		"--- FAIL: FuzzRYWCache (90.02s)\r\n" +
		"    context deadline exceeded\r\n"
	if got := classify(runResult{output: out, exitCode: 1}); got != verdictRealFailure {
		t.Errorf("classify = %v, want %v", got, verdictRealFailure)
	}
}
