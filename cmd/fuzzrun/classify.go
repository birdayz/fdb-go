package main

import (
	"regexp"
	"strings"
)

// verdict classifies the outcome of a failed `go test -fuzz` / `bazelisk test`
// fuzzing run.
type verdict int

const (
	// verdictRealFailure — the run found something, or failed for a reason we do
	// not positively recognise. Fail the job. This is the zero value on purpose:
	// anything unrecognised must fail, never retry.
	verdictRealFailure verdict = iota
	// verdictDeadlineRace — the run consumed its whole -fuzztime budget without
	// finding anything, and the only reported failure is the budget expiry itself.
	// Retryable; see classify.
	verdictDeadlineRace
)

func (v verdict) String() string {
	switch v {
	case verdictDeadlineRace:
		return "deadline-race"
	case verdictRealFailure:
		return "real-failure"
	}
	return "unknown"
}

// Exit codes we are willing to consider benign. Anything else — 124 (`timeout`
// killed it), 137 (OOM), 2/8 (bazel command/build error), -1 (could not spawn) —
// is a real failure regardless of what the output says.
const (
	exitGoTestFailure    = 1 // `go test`: a test failed
	exitBazelTestsFailed = 3 // `bazelisk test`: tests ran and failed (build errors are 1/2/8)
)

// hardFailureMarkers never appear in a benign budget-expiry run. Defence in depth
// behind the structural check below — each is a failure mode that reaches the same
// "no crasher file" shape:
//
//   - a seed-corpus entry failing during warmup is reported via
//     stop(errors.New(crasherMsg)), NOT a crashError, so testing/fuzz.go never
//     prints "Failing input written to" ($GOROOT/src/internal/fuzz/fuzz.go:245-250).
//   - a crasher that WAS persisted (the normal finding path).
//   - the diff-oracle harness's own t.Errorf messages.
var hardFailureMarkers = []string{
	"failure while testing seed corpus entry:",
	"Failing input written to",
	"To re-run:",
	"panic:",
	"DATA RACE",
	"STRUCTURAL MISMATCH",
	"oracle error",
}

var (
	// A fuzz progress line. Its presence proves the coordinator actually ran and
	// burned budget, which is what distinguishes budget expiry from a process that
	// died before fuzzing began (e.g. a TestMain container start timing out — which
	// in this repo reports the string "context deadline exceeded" too).
	fuzzProgressRE = regexp.MustCompile(`(?m)^fuzz: elapsed: `)
	// The failure header for a fuzz target, e.g. "--- FAIL: FuzzFoo (120.08s)".
	fuzzFailRE = regexp.MustCompile(`^--- FAIL: Fuzz\w+ \([0-9.]+s\)$`)
	anyFailRE  = regexp.MustCompile(`(?m)^\s*--- FAIL: `)
)

// classify decides whether a non-zero fuzz run is a genuine finding or the known
// Go stdlib race that leaks "context deadline exceeded" when the -fuzztime budget
// expires.
//
// The race lives in $GOROOT/src/internal/fuzz/fuzz.go (Go 1.26.5):
//
//	if opts.Timeout > 0 { ctx, cancel = context.WithTimeout(ctx, opts.Timeout) }
//	fuzzCtx, cancelWorkers := context.WithCancel(ctx)
//	doneC := ctx.Done()
//	stop := func(err error) {
//	        if err == fuzzCtx.Err() || isInterruptError(err) { err = nil } // <-- here
//	        ...
//	}
//	case <-doneC:
//	        stop(ctx.Err())
//
// When the -fuzztime deadline fires, context's cancelCtx.cancel closes the parent's
// done channel BEFORE propagating cancellation to its children. The coordinator
// goroutine can wake on <-doneC and evaluate fuzzCtx.Err() inside that window, while
// the child is still uncancelled and reports nil. The suppression check
// `err == fuzzCtx.Err()` then compares DeadlineExceeded against nil, does not
// suppress, and the normal end of the fuzzing budget is reported as a test failure.
//
// It is target-agnostic and reproduces on a fuzz target with no dependencies at all.
// Observed in the nightly on FuzzGetValueReply (2026-07-14) and FuzzEndpoint
// (2026-07-11) — different targets, byte-identical signature.
//
// Recognition is a POSITIVE WHITELIST, not "deadline text minus a denylist". That
// distinction is load-bearing: "context deadline exceeded" is a first-class error
// string in this codebase (every package that hosts fuzz targets also starts an FDB
// testcontainer under a context.WithTimeout), so treating the bare string as benign
// would silently retry real container/setup flakes into green. A run is benign only
// when it exhibits the complete shape:
//
//  1. the process exited with a test-failure status (not a kill/timeout/build error);
//  2. it emitted fuzz progress, proving the budget was actually consumed;
//  3. it reported exactly ONE failure, and that failure is a Fuzz target; and
//  4. that failure's ONLY detail line is "context deadline exceeded".
//
// Anything else — including a bare "context deadline exceeded" with no fuzzing —
// is a real failure.
func classify(r runResult) verdict {
	if r.exitCode != exitGoTestFailure && r.exitCode != exitBazelTestsFailed {
		return verdictRealFailure
	}
	out := r.output
	for _, m := range hardFailureMarkers {
		if strings.Contains(out, m) {
			return verdictRealFailure
		}
	}
	if !fuzzProgressRE.MatchString(out) {
		return verdictRealFailure
	}
	// Exactly one reported failure, and it must be the fuzz target itself.
	if len(anyFailRE.FindAllString(out, -1)) != 1 {
		return verdictRealFailure
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !fuzzFailRE.MatchString(strings.TrimSpace(line)) {
			continue
		}
		return classifyFailureDetail(lines[i+1:])
	}
	return verdictRealFailure
}

// classifyFailureDetail inspects the indented detail block that follows a
// "--- FAIL: FuzzXxx" header. The budget-expiry race produces exactly one detail
// line. Any additional detail is a real finding.
func classifyFailureDetail(rest []string) verdict {
	var details []string
	for _, line := range rest {
		if line == "" || (line[0] != ' ' && line[0] != '\t') {
			break // end of the indented detail block
		}
		details = append(details, strings.TrimSpace(line))
	}
	if len(details) == 1 && details[0] == "context deadline exceeded" {
		return verdictDeadlineRace
	}
	return verdictRealFailure
}
