package testkit

// A whole-corpus census gate that fails must LOOK like a failing test.
//
// THE DEFECT THIS FIXES. These gates are asserted in TestMain, after m.Run(),
// and they used to report by writing prose to stderr and bumping the exit code.
// `go test` renders that as:
//
//	PASS
//	CENSUS FAIL: denominator 586, want 582.
//	FAIL	fdb.dev/pkg/relational/sqldriver	271.639s
//
// There is no `--- FAIL:` line anywhere in it. Every tool and every habit that
// locates a failure keys on that marker — `grep '--- FAIL'`, a CI log scraper, a
// human scrolling — and all of them come back empty on a package that genuinely
// failed. This is the green-from-an-empty-set family in its most dangerous
// direction: the absence of a marker reads as the absence of a failure, so the
// only surviving evidence is a summary line naming the package and a duration.
// It is exactly how one such failure's text was lost, and it would have lost the
// next one.
//
// WHY THE GATES STAY IN TestMain rather than becoming ordinary test functions,
// which is the fix that looks cleaner and does not work. Each gate asserts over
// the population of the WHOLE corpus, so it can only run once every test has
// finished — and no test function can occupy that position. Go releases parallel
// tests only after the sequential ones are done (MEASURED: a sequential test
// polling a counter incremented by parallel siblings observed 0 overlaps), so a
// non-parallel "last" gate test actually runs FIRST, before any parallel test
// body; and a parallel one has no ordering guarantee against its parallel
// siblings. `m.Run()` returning is the only point at which the population is
// complete. So the gates stay where they are and learn to speak go test's
// output format instead.

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// censusGateSuiteName is the identity failing gates report under. It is a
// test-shaped name on purpose: it is what appears after `--- FAIL:`, so it is
// what a reader greps for and what a log scraper attributes the failure to.
const CensusGateSuiteName = "TestWholeCorpusCensusGates"

// censusGate is one whole-corpus assertion: a name, and a check that writes its
// reasoning to the writer and reports whether it FAILED.
type CensusGate struct {
	Name string
	Run  func(io.Writer) bool
}

// runCensusGates runs every gate and renders the outcome in go test's own output
// format, returning whether any failed.
//
// Passing gates still have their prose emitted verbatim — several report
// something useful when they pass (that a -test.run filter narrowed the corpus,
// or the population they reached), and swallowing that would trade one silent
// failure mode for another.
//
// Failing gates are rendered as a parent failure with one subtest per gate,
// matching what `go test` prints for a table test:
//
//	--- FAIL: TestWholeCorpusCensusGates (0.00s)
//	    --- FAIL: TestWholeCorpusCensusGates/exampleGate (0.00s)
//	        <the gate's own reasoning, indented>
//
// The parent line sits at column 0 so `grep -c '^--- FAIL'` finds it, which is
// the specific query that used to return 0 on a failing run.
func RunCensusGates(w io.Writer, verbose bool, gates []CensusGate) bool {
	type failure struct {
		name   string
		detail string
	}
	var failures []failure

	for _, g := range gates {
		var buf bytes.Buffer
		failed := g.Run(&buf)
		if !failed {
			// Verbatim, so a passing gate's report reads exactly as it did.
			if buf.Len() > 0 {
				_, _ = w.Write(buf.Bytes())
			}
			continue
		}
		failures = append(failures, failure{name: g.Name, detail: buf.String()})
	}

	if len(failures) == 0 {
		return false
	}

	// `=== RUN` only under -test.v, mirroring go test: in non-verbose mode it
	// prints the `--- FAIL:` lines and not the RUN lines.
	if verbose {
		fmt.Fprintf(w, "=== RUN   %s\n", CensusGateSuiteName)
		for _, f := range failures {
			fmt.Fprintf(w, "=== RUN   %s/%s\n", CensusGateSuiteName, f.name)
		}
	}
	fmt.Fprintf(w, "--- FAIL: %s (0.00s)\n", CensusGateSuiteName)
	for _, f := range failures {
		fmt.Fprintf(w, "    --- FAIL: %s/%s (0.00s)\n", CensusGateSuiteName, f.name)
		fmt.Fprint(w, indentCensusDetail(f.detail, "        "))
	}
	return true
}

// indentCensusDetail indents every non-empty line of a gate's report, the way
// go test indents a failure message under its `--- FAIL:` line. Blank lines stay
// blank rather than becoming trailing whitespace.
func indentCensusDetail(detail, prefix string) string {
	if detail == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
