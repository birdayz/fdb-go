// Command fuzzrun runs one fuzzing command and classifies its failure.
//
// It exists to keep the nightly fuzz gate honest. A fuzz run that burns its whole
// -fuzztime budget without finding anything is a PASS, but Go's fuzz coordinator
// intermittently reports that budget expiry as "context deadline exceeded" and exits
// non-zero (see classify's doc comment for the exact stdlib race). That turned the
// nightly red on targets that had found nothing across millions of executions, which
// is precisely the noise that trains people to ignore a red safety net.
//
// fuzzrun retries ONLY a run that exhibits the complete budget-expiry shape, once,
// and fails on everything else. Recognition is a positive whitelist rather than a
// denylist because "context deadline exceeded" is also what a timed-out FDB
// testcontainer start reports; retrying that class would hide real flakes.
//
// Usage:
//
//	fuzzrun -label "FuzzFoo" -- go test -run='^$' -fuzz='^FuzzFoo$' -fuzztime=2m ./pkg/...
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runResult is the outcome of one child process.
type runResult struct {
	output string
	// exitCode is the child's exit status, 0 on success and -1 when the process
	// could not be started or was killed by a signal. Keeping it distinct from the
	// output is what lets classify reject a `timeout`-kill (124) or an OOM (137)
	// outright instead of guessing from text.
	exitCode int
	err      error
}

func (r runResult) ok() bool { return r.exitCode == 0 && r.err == nil }

func main() {
	label := flag.String("label", "", "human-readable name of the target, for log lines")
	deadline := flag.Int64("deadline", 0,
		"unix timestamp after which a confirming retry is not STARTED (0 = no limit); "+
			"bounds the job's wall clock if the race ever goes systematic. Note this "+
			"gates the retry's start, not its end: one begun just under the deadline "+
			"still runs its full budget.")
	flag.Parse()

	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "fuzzrun: no command given (use: fuzzrun -label NAME -- CMD ARGS...)")
		os.Exit(2)
	}
	if *label == "" {
		*label = argv[0]
	}

	os.Exit(gate(*label, os.Stdout, *deadline, func() runResult { return runCommand(argv, os.Stdout) }))
}

// gate applies the retry policy and returns the process exit code. It takes the
// runner as a closure so the policy is testable without spawning fuzz jobs.
func gate(label string, w io.Writer, retryDeadline int64, run func() runResult) int {
	r := run()
	if r.ok() {
		return 0
	}
	if r.err != nil {
		fmt.Fprintf(w, "fuzzrun: %v\n", r.err)
	}

	if classify(r) == verdictRealFailure {
		fmt.Fprintf(w, "::error::fuzz FAIL: %s (exit %d)\n", label, r.exitCode)
		return 1
	}

	// Past the job's wall-clock budget we skip the retry — but we do NOT flip the
	// result to red. The classifier is authoritative: it has already established
	// this run is the stdlib race, and the retry is corroboration, not the basis
	// for that call (under bazel it is a fresh random fuzz run in a fresh sandbox,
	// so it says little about run 1 anyway). Failing here would re-introduce the
	// exact false red this tool removes, and would do it precisely when the race is
	// most frequent.
	if retryDeadline > 0 && time.Now().Unix() >= retryDeadline {
		fmt.Fprintf(w, "::warning::%s: fuzz budget expiry leaked as \"context deadline exceeded\" "+
			"(golang/go#72104); the job's retry deadline has passed, so this is accepted on the "+
			"classification alone without a confirming retry.\n", label)
		return 0
	}

	// Known Go stdlib race: the budget expired cleanly but was reported as an error.
	// Surface it loudly — a silent retry would hide a genuine change in its frequency.
	fmt.Fprintf(w, "::warning::%s: fuzz budget expiry leaked as \"context deadline exceeded\" "+
		"(golang/go#72104); the run consumed its full budget, found nothing, and wrote no "+
		"crasher. Retrying once.\n", label)

	r2 := run()
	if r2.ok() {
		fmt.Fprintf(w, "%s: retry clean — confirmed stdlib deadline race, not a finding.\n", label)
		return 0
	}
	if r2.err != nil {
		fmt.Fprintf(w, "fuzzrun: %v\n", r2.err)
	}
	fmt.Fprintf(w, "::error::fuzz FAIL: %s (failed again on retry, exit %d)\n", label, r2.exitCode)
	return 1
}

// runCommand runs argv, streaming its output to w as it arrives while also capturing
// it for classification. Streaming matters: if the surrounding job hits its
// timeout-minutes budget mid-run, a buffered implementation would leave exactly the
// in-flight target's log missing — the one you need.
func runCommand(argv []string, w io.Writer) runResult {
	var buf bytes.Buffer
	// One sink VALUE assigned to both streams, deliberately. os/exec compares
	// Stdout and Stderr with interfaceEqual and, when they are the same value,
	// wires a single pipe with a single copying goroutine. Building two separate
	// io.MultiWriter(w, &buf) values instead would give two goroutines writing
	// concurrently to buf — a data race. Keep this as one variable.
	sink := io.MultiWriter(w, &buf)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = sink
	cmd.Stderr = sink

	err := cmd.Run()
	out := buf.String()
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	switch {
	case err == nil:
		return runResult{output: out, exitCode: 0}
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// ExitCode is -1 when the process was terminated by a signal, which is
			// exactly the "killed, not a test failure" case classify must reject.
			return runResult{output: out, exitCode: ee.ExitCode()}
		}
		// Could not start at all (missing binary, bad path). Report it: otherwise
		// the failure surfaces as an empty-bodied error annotation.
		return runResult{output: out, exitCode: -1, err: fmt.Errorf("running %q: %w", argv[0], err)}
	}
}
