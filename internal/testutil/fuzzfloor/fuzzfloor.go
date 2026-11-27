// Package fuzzfloor decides whether a fuzz target's seed-corpus coverage floor
// should be enforced on this run.
//
// A semantics differential counts what it actually compared and fails if the
// count collapses, because every assertion in one sits behind a `continue` and
// a target that stops comparing reports the same green as one that agrees. That
// floor is a statement about the SEED CORPUS — a fixed, deterministic
// population — and it cannot be enforced while that target is being fuzzed:
// under `go test -fuzz` the coordinator hands every input, seeds included, to
// worker SUBPROCESSES, so the counters stay at zero in the process that runs
// f.Cleanup. Measured: an 8s -fuzz run at 31797 execs reported "produced 0
// usable trees from 6 seeds" on a completely healthy run.
//
// It lives in its own package because the obvious spelling of that check is
// wrong in a way that is invisible per-package. Asking only whether -test.fuzz
// is NON-EMPTY disables the floors of every target in the binary, including the
// ones the coordinator did run in-process with their seeds — so fuzzing ONE
// target silently switches off the coverage floors of all its neighbours.
// Demonstrated: with a deliberately broken floor in
// FuzzSimplifyPredicate_PreservesSemantics, plain `go test` fails while
// `go test -fuzz 'FuzzNormalForm_PreservesSemantics$'` PASSES, its log showing
// `=== RUN FuzzSimplifyPredicate/seed#0..4` — the seeds ran, the counters
// reached 60, and the floor was silenced.
//
// One definition, imported by every package with such a floor, so that fix
// cannot land in one copy and miss another.
package fuzzfloor

import (
	"flag"
	"regexp"
	"strings"
)

// SuppressedFor reports whether `go test -fuzz` selected THIS target for active
// fuzzing, in which case its seed-corpus floor describes a population that does
// not exist in this process and must not be enforced.
//
// The match mirrors how the testing package selects a fuzz target: the
// -test.fuzz value is an UNANCHORED regexp, and only its first '/'-separated
// element applies to a top-level target name.
//
// It fails CLOSED. No flag, an empty pattern, or a pattern that does not compile
// all return false, so the floor is enforced. An uncompilable pattern makes
// `go test` exit before any target runs, so that arm is unreachable in practice;
// it is written this way so the unreachable case cannot silence a gate.
func SuppressedFor(targetName string) bool {
	f := flag.Lookup("test.fuzz")
	if f == nil {
		return false
	}
	pattern := f.Value.String()
	if pattern == "" {
		return false
	}
	if i := strings.Index(pattern, "/"); i >= 0 {
		pattern = pattern[:i]
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(targetName)
}
