// Command fuzzrun runs one fuzzing command and classifies its failure.
//
// It exists to keep the nightly fuzz gate honest. A fuzz run that burns its whole
// -fuzztime budget without finding anything is a PASS, but Go's fuzz coordinator
// intermittently reports that budget expiry as "context deadline exceeded" and exits
// non-zero (see classify's doc comment for the exact stdlib race). That turned the
// nightly red on targets that had found nothing across millions of executions, which
// is precisely the noise that trains people to ignore a red safety net.
//
// fuzzrun retries ONLY that signature, once, and fails on everything else. A real
// finding is persisted to the corpus by the first run, so even if the classifier were
// wrong the retry replays it as a seed and fails deterministically.
//
// Usage:
//
//	fuzzrun -label "FuzzFoo" -- go test -run='^$' -fuzz='^FuzzFoo$' -fuzztime=2m ./pkg/...
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// tailLines is set from -tail: on a SUCCESSFUL run only the last N lines are
// echoed, keeping the nightly log readable. Failures always print in full.
var tailLines int

func main() {
	label := flag.String("label", "", "human-readable name of the target, for log lines")
	flag.IntVar(&tailLines, "tail", 0, "on success, echo only the last N lines (0 = all)")
	flag.Parse()

	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "fuzzrun: no command given (use: fuzzrun -label NAME -- CMD ARGS...)")
		os.Exit(2)
	}
	if *label == "" {
		*label = argv[0]
	}

	os.Exit(gate(*label, os.Stdout, func() (string, bool) { return exec_(argv) }))
}

// tail returns the last n lines of s, or all of s when n <= 0.
func tail(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

// gate applies the retry policy and returns the process exit code. It takes the
// runner as a closure so the policy is testable without spawning fuzz jobs.
func gate(label string, w io.Writer, run func() (string, bool)) int {
	out, ok := run()
	if ok {
		io.WriteString(w, tail(out, tailLines))
		return 0
	}
	io.WriteString(w, out)

	if classify(out) == verdictRealFailure {
		fmt.Fprintf(w, "::error::fuzz FAIL: %s\n", label)
		return 1
	}

	// Known Go stdlib race: the budget expired cleanly but was reported as an error.
	// Surface it loudly — a silent retry would hide a genuine change in its frequency.
	fmt.Fprintf(w, "::warning::%s: fuzz budget expiry leaked as %q (golang/go#72104); "+
		"no crasher was written. Retrying once.\n", label, "context deadline exceeded")

	out2, ok2 := run()
	if ok2 {
		io.WriteString(w, tail(out2, tailLines))
		fmt.Fprintf(w, "%s: retry clean — confirmed stdlib deadline race, not a finding.\n", label)
		return 0
	}
	io.WriteString(w, out2)
	fmt.Fprintf(w, "::error::fuzz FAIL: %s (failed again on retry)\n", label)
	return 1
}

// exec_ runs argv, capturing combined output, and reports whether it exited zero.
func exec_(argv []string) (string, bool) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	b, err := cmd.CombinedOutput()
	out := string(b)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out, err == nil
}
