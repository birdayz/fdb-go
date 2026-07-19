package main

import "strings"

// verdict classifies the outcome of a failed `go test -fuzz` / `bazelisk test`
// fuzzing run from its combined output.
type verdict int

const (
	// verdictRealFailure — the run found something. Fail the job.
	verdictRealFailure verdict = iota
	// verdictDeadlineRace — the run completed its whole -fuzztime budget without
	// finding anything, but Go's fuzz coordinator leaked the budget-expiry
	// context error as a test failure. Retryable; see the comment on classify.
	verdictDeadlineRace
)

func (v verdict) String() string {
	if v == verdictDeadlineRace {
		return "deadline-race"
	}
	return "real-failure"
}

// crasherMarkers are the strings Go's testing package prints when a fuzz target
// actually fails on an input. Any one of them means a real, reproducible finding
// whose input was persisted to the corpus — never retry those.
//
// See $GOROOT/src/testing/fuzz.go (fuzzCrashError branch of F.Fuzz).
var crasherMarkers = []string{
	"Failing input written to",
	"To re-run:",
}

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
// It is target-agnostic and reproduces on a fuzz target with no dependencies at all
// (see TestClassify_TrivialTargetRegression for the captured output). Observed in the
// nightly on FuzzGetValueReply (2026-07-14) and FuzzEndpoint (2026-07-11) — different
// targets, byte-identical signature.
//
// Distinguishing it is safe because the two cases are disjoint on the wire:
// a real finding always persists its input and prints a crasher marker, while the
// race prints only the bare context error and writes nothing. The caller retries a
// deadline-race verdict once, so even a misclassification cannot swallow a finding:
// a persisted crasher is replayed as a seed on the retry and fails deterministically.
func classify(output string) verdict {
	if !strings.Contains(output, "context deadline exceeded") {
		return verdictRealFailure
	}
	for _, m := range crasherMarkers {
		if strings.Contains(output, m) {
			return verdictRealFailure
		}
	}
	// A structural divergence reported via t.Errorf (rather than a panic) still
	// fails the run; those messages must never be retried away either.
	for _, m := range []string{"STRUCTURAL MISMATCH", "oracle error", "panic:", "DATA RACE"} {
		if strings.Contains(output, m) {
			return verdictRealFailure
		}
	}
	return verdictDeadlineRace
}
